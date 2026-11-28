// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package deploy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/go-steer/shipmind/internal/agent"
	"github.com/go-steer/shipmind/internal/archive"
	"github.com/go-steer/shipmind/internal/cluster"
	"github.com/go-steer/shipmind/internal/env"
	"github.com/go-steer/shipmind/internal/overlay"
)

// A canary run (#127) exercises a candidate agent image in a running
// environment without changing it. Canary renders the environment's chat
// task and Workspace from the candidate checkout with that image, under
// names of their own, and hands them to a one-off Job that runs a chat
// scenario against them (scenario.RunChat) and deletes them. Nothing the
// environment deploys is touched: not switchboard's task template, not
// the Workspace its threads mount, not the nightly CronJobs.
const (
	// E2ENamespace is where the nightly scenarios run (deploy/e2e/substrate),
	// and so where a canary's Job and Secret go: the Job borrows the
	// nightly's pod (its service account, its router identity).
	E2ENamespace = "shipmind-e2e"
	// CanaryLabel marks a canary's Job and Secret with the run's name.
	CanaryLabel = "shipmind.dev/canary"
	// e2eManifest holds the nightly CronJobs a canary's Job is cut from.
	e2eManifest = "deploy/e2e/substrate/e2e.yaml"
	// canaryTTL is how long a finished canary Job, and with it its
	// Secret, stays for reading before Kubernetes removes it.
	canaryTTL = 24 * time.Hour
	// canaryDeadline bounds the Job: a cold start, an answer, cleanup.
	canaryDeadline = 30 * time.Minute
)

var (
	canaryImage    = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)
	canaryScenario = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

// CanaryOptions say what a canary runs.
type CanaryOptions struct {
	// Image is the candidate agent image (images/agent), by digest.
	Image string
	// RunnerImage is the e2e-runner image, by digest: one built from a
	// commit whose runner knows chat scenarios and carries Scenario.
	RunnerImage string
	// Scenario names a chat scenario in the runner image (/scenarios/<name>).
	Scenario string
	// Cluster names one of the environment's agent.clusters to ask
	// about; empty takes the first.
	Cluster string
	// Archive, when set, is the gs://bucket/prefix target passed to the
	// canary runner's -archive flag. Empty inherits the -archive target
	// from the environment overlay's nightly scenario CronJob, if any.
	Archive string
	// DryRun renders and returns without applying.
	DryRun bool
	// Now defaults to the real clock.
	Now func() time.Time
}

// CanaryRun is what Canary rendered and, unless DryRun, applied.
type CanaryRun struct {
	// Name names the Job, its Secret, the ax task and the ax Workspace.
	Name string
	// Cluster is the path of the cluster the run asks about.
	Cluster string
	// Archive is the effective gs://bucket/prefix archive target rendered
	// into the Job, or empty if archiving is disabled.
	Archive string
	// Job is the Job's manifest. The Secret's is not kept: it holds the
	// task, which carries the agent's attach token.
	Job []byte
}

// Canary launches a canary run of o.Image in environment e, with the
// templates of the checkout at root.
func Canary(ctx context.Context, root string, e *env.Env, c cluster.Cluster, o CanaryOptions) (*CanaryRun, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	path, err := o.check(e)
	if err != nil {
		return nil, err
	}
	// A dry run reads nothing from the cluster: it renders with throwaway
	// tokens, which is enough to prove the templates and the Job.
	var t agent.Tokens
	if o.DryRun {
		if t, err = agent.NewTokens(); err != nil {
			return nil, err
		}
	} else {
		var found bool
		if t, _, found, err = tokens(ctx, c); err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New("the cluster holds no agent tokens: a canary runs in a deployed environment")
		}
	}
	archiveTarget := o.Archive
	if archiveTarget == "" {
		archiveTarget = overlayArchive(e.Dir, root)
	}
	run := &CanaryRun{
		Name:    "canary-" + o.Scenario + "-" + o.Now().UTC().Format("0102-150405"),
		Cluster: path,
		Archive: archiveTarget,
	}

	// The environment as it would be with the candidate image, its
	// Workspace under the run's name so the live one is left alone.
	candidate := *e
	candidate.Agent.ChatImage = o.Image
	candidate.Agent.Workspace = run.Name
	workspace, err := agent.Workspace(root, &candidate)
	if err != nil {
		return nil, err
	}
	task, err := agent.ChatTask(root, &candidate, t)
	if err != nil {
		return nil, err
	}
	if task, err = named(task, run.Name); err != nil {
		return nil, fmt.Errorf("the chat task: %w", err)
	}
	if run.Job, err = canaryJob(root, run, e.Agent.Atespace, o); err != nil {
		return nil, err
	}
	if o.DryRun {
		return run, nil
	}
	secret, err := canarySecret(run.Name, map[string][]byte{"task.yaml": task, "workspace.yaml": workspace})
	if err != nil {
		return nil, err
	}
	if err := c.ApplySecret(ctx, secret, false); err != nil {
		return nil, fmt.Errorf("secret %s/%s (is the environment's nightly e2e component deployed?): %w", E2ENamespace, run.Name, err)
	}
	if _, err := c.Apply(ctx, run.Job, false); err != nil {
		// Nothing will ever own the Secret now; it holds the attach
		// token, so it goes at once rather than waiting for a hand.
		// By identity: a delete's error can quote what it was given.
		if _, derr := c.Delete(ctx, fmt.Appendf(nil, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\n", run.Name, E2ENamespace)); derr != nil {
			return nil, fmt.Errorf("job %s/%s: %w; and its secret of the same name was not removed, delete it by hand: %w", E2ENamespace, run.Name, err, derr)
		}
		return nil, fmt.Errorf("job %s/%s: %w", E2ENamespace, run.Name, err)
	}
	// The Secret goes when the Job does. Failing to tie them is worth
	// saying, not worth failing a run that has started.
	if err := ownSecret(ctx, c, run.Name); err != nil {
		return run, fmt.Errorf("the run started, but its secret %s/%s is not owned by the job and will outlive it; delete it by hand: %w", E2ENamespace, run.Name, err)
	}
	return run, nil
}

// check validates the options against the environment and returns the
// path of the cluster to ask about.
func (o CanaryOptions) check(e *env.Env) (string, error) {
	switch {
	case e.Agent.ChatImage == "":
		return "", errors.New("the environment sets no agent.chatImage: a canary runs the chat task")
	case !canaryImage.MatchString(o.Image):
		return "", fmt.Errorf("image %q: want the candidate agent image pinned by digest", o.Image)
	case !canaryImage.MatchString(o.RunnerImage):
		return "", fmt.Errorf("runner image %q: want an e2e-runner image pinned by digest", o.RunnerImage)
	case !canaryScenario.MatchString(o.Scenario) || len(o.Scenario) > 40:
		return "", fmt.Errorf("scenario %q: want a scenario's name", o.Scenario)
	case len(e.Agent.Clusters) == 0:
		return "", errors.New("the environment lists no agent.clusters to ask about")
	}
	if o.Archive != "" {
		if _, err := archive.Parse(o.Archive); err != nil {
			return "", err
		}
	}
	if o.Cluster == "" {
		return e.Agent.Clusters[0].Path, nil
	}
	var names []string
	for _, cl := range e.Agent.Clusters {
		if cl.Name == o.Cluster {
			return cl.Path, nil
		}
		names = append(names, cl.Name)
	}
	return "", fmt.Errorf("cluster %q is not in the environment's agent.clusters (%s)", o.Cluster, strings.Join(names, ", "))
}

// named sets an ax manifest's metadata.name: a task template has none,
// since lookout and switchboard name the tasks they stamp from it.
func named(manifest []byte, name string) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(manifest, &doc); err != nil {
		return nil, err
	}
	meta, ok := doc["metadata"].(map[string]any)
	if !ok {
		return nil, errors.New("no metadata")
	}
	meta["name"] = name
	return yaml.Marshal(doc)
}

// canaryJob cuts the run's Job from the first nightly CronJob in the
// checkout: the same pod (service account, router identity, security
// context), with the runner image, the chat run's arguments and the
// run's Secret mounted at /canary.
func canaryJob(root string, run *CanaryRun, atespace string, o CanaryOptions) ([]byte, error) {
	spec, err := nightlyJobSpec(filepath.Join(root, e2eManifest))
	if err != nil {
		return nil, err
	}
	fail := func(what string) ([]byte, error) {
		return nil, fmt.Errorf("%s: the nightly CronJob has no %s", e2eManifest, what)
	}
	template, _ := spec["template"].(map[string]any)
	pod, _ := template["spec"].(map[string]any)
	containers, _ := pod["containers"].([]any)
	if len(containers) != 1 {
		return fail("single container")
	}
	container, _ := containers[0].(map[string]any)
	args, _ := container["args"].([]any)
	if len(args) < 4 || args[0] != "scenario" || args[1] != "run" {
		return fail("`scenario run` arguments")
	}
	// The nightly's own flags (in-cluster, the router's mTLS files, the
	// judge) carry over; its atespace placeholder and its scenario don't.
	var runArgs []any
	for i := 0; i < len(args)-1; i++ {
		s, _ := args[i].(string)
		if strings.HasPrefix(s, "-atespace=") {
			runArgs = append(runArgs, "-atespace="+atespace)
			continue
		}
		if run.Archive != "" {
			if strings.HasPrefix(s, "-archive=") || strings.HasPrefix(s, "--archive=") {
				continue
			}
			if (s == "-archive" || s == "--archive") && i+1 < len(args)-1 {
				i++
				continue
			}
		}
		runArgs = append(runArgs, args[i])
	}
	if run.Archive != "" {
		runArgs = append(runArgs, "-archive="+run.Archive)
	}
	container["args"] = append(runArgs,
		"-task=/canary/task.yaml", "-workspace=/canary/workspace.yaml", "-cluster="+run.Cluster, "/scenarios/"+o.Scenario)
	container["image"] = o.RunnerImage
	mounts, _ := container["volumeMounts"].([]any)
	container["volumeMounts"] = append(mounts, map[string]any{"name": "canary", "mountPath": "/canary", "readOnly": true})
	volumes, _ := pod["volumes"].([]any)
	pod["volumes"] = append(volumes, map[string]any{"name": "canary", "secret": map[string]any{"secretName": run.Name}})

	labels := map[string]any{CanaryLabel: run.Name}
	meta, _ := template["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		template["metadata"] = meta
	}
	podLabels, _ := meta["labels"].(map[string]any)
	if podLabels == nil {
		podLabels = map[string]any{}
		meta["labels"] = podLabels
	}
	podLabels[CanaryLabel] = run.Name
	spec["backoffLimit"] = 0
	spec["activeDeadlineSeconds"] = int(canaryDeadline.Seconds())
	spec["ttlSecondsAfterFinished"] = int(canaryTTL.Seconds())
	return yaml.Marshal(map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]any{"name": run.Name, "namespace": E2ENamespace, "labels": labels},
		"spec":       spec,
	})
}

// nightlyJobSpec returns the job spec of the first CronJob in the
// manifest at path.
func nightlyJobSpec(path string) (map[string]any, error) {
	b, err := os.ReadFile(path) //nolint:gosec // a file of the operator's own checkout
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("%s: no CronJob", path)
			}
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if doc["kind"] != "CronJob" {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		jobTemplate, _ := spec["jobTemplate"].(map[string]any)
		jobSpec, ok := jobTemplate["spec"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: the CronJob has no jobTemplate.spec", path)
		}
		return jobSpec, nil
	}
}

// canarySecret renders the run's Secret, labelled like its Job.
func canarySecret(name string, data map[string][]byte) ([]byte, error) {
	encoded := map[string]any{}
	for k, v := range data {
		encoded[k] = base64.StdEncoding.EncodeToString(v)
	}
	return yaml.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": name, "namespace": E2ENamespace, "labels": map[string]any{CanaryLabel: name}},
		"type":       "Opaque",
		"data":       encoded,
	})
}

// ownSecret makes the run's Job the owner of its Secret, so the Job's
// TTL removes both.
func ownSecret(ctx context.Context, c cluster.Cluster, name string) error {
	raw, found, err := c.Get(ctx, "job", E2ENamespace, name)
	if err != nil {
		return err
	}
	var job struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	if !found || json.Unmarshal(raw, &job) != nil || job.Metadata.UID == "" {
		return fmt.Errorf("job %s/%s: its UID was not read", E2ENamespace, name)
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"ownerReferences": []map[string]any{{
		"apiVersion": "batch/v1", "kind": "Job", "name": name, "uid": job.Metadata.UID,
	}}}})
	if err != nil {
		return err
	}
	return c.Patch(ctx, "secret", E2ENamespace, name, canaryPatchType, patch, false)
}

// canaryPatchType is a patch type cluster.Kube.Patch takes; a strategic
// merge keys ownerReferences by uid.
const canaryPatchType = "strategic"

// overlayArchive reads the -archive target from the environment overlay's
// nightly scenario CronJob when envDir holds a kustomization that renders
// one. Missing or unrenderable overlays return "" so a candidate checkout
// that changed an unrelated profile manifest still launches a canary.
func overlayArchive(envDir, root string) string {
	if envDir == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(envDir, "kustomization.yaml")); err != nil {
		return ""
	}
	res, err := overlay.Render(envDir, root, overlay.Options{AnyRef: true})
	if err != nil {
		return ""
	}
	for _, o := range res.Objects {
		if o.Kind() != "CronJob" || o.Namespace() != E2ENamespace || o.GetLabels()["shipmind.dev/nightly"] != "scenario" {
			continue
		}
		raw, err := o.String()
		if err != nil {
			continue
		}
		if target := cronJobArchiveArg([]byte(raw)); target != "" {
			if _, err := archive.Parse(target); err == nil {
				return target
			}
		}
	}
	return ""
}

func cronJobArchiveArg(raw []byte) string {
	var cj struct {
		Spec struct {
			JobTemplate struct {
				Spec struct {
					Template struct {
						Spec struct {
							Containers []struct {
								Args []string `yaml:"args"`
							} `yaml:"containers"`
						} `yaml:"spec"`
					} `yaml:"template"`
				} `yaml:"spec"`
			} `yaml:"jobTemplate"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &cj); err != nil || len(cj.Spec.JobTemplate.Spec.Template.Spec.Containers) == 0 {
		return ""
	}
	args := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Args
	for i := 0; i < len(args); i++ {
		if v, ok := strings.CutPrefix(args[i], "-archive="); ok {
			return v
		}
		if v, ok := strings.CutPrefix(args[i], "--archive="); ok {
			return v
		}
		if (args[i] == "-archive" || args[i] == "--archive") && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
