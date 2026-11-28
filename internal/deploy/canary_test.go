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
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/go-steer/shipmind/internal/env"
)

const canaryName = "canary-posture-audit-1124-120000"

var (
	candidateImage = "registry.example.com/agent@sha256:" + strings.Repeat("c", 64)
	runnerImage    = "registry.example.com/e2e-runner@sha256:" + strings.Repeat("d", 64)
)

func canaryEnv() *env.Env {
	e := testEnv()
	e.Agent.ChatImage = "registry.example.com/agent@sha256:" + strings.Repeat("2", 64)
	e.Agent.Clusters = append(e.Agent.Clusters, env.Cluster{Name: "prod", Path: "projects/other-project/locations/europe-west1/clusters/prod"})
	return e
}

func canaryOptions() CanaryOptions {
	return CanaryOptions{
		Image: candidateImage, RunnerImage: runnerImage, Scenario: "posture-audit",
		Now: func() time.Time { return time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC) },
	}
}

func canaryCluster() *fakeCluster {
	return &fakeCluster{
		secrets: map[string]map[string][]byte{Namespace + "/" + TokensSecret: {"inject": []byte(tok.Inject), "attach": []byte(tok.Attach)}},
		objects: map[string]string{"job/" + E2ENamespace + "/" + canaryName: `{"metadata":{"uid":"uid-1"}}`},
	}
}

// job is as much of a Job as the tests read.
type job struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name, Namespace string
		Labels          map[string]string
	} `yaml:"metadata"`
	Spec struct {
		BackoffLimit            int `yaml:"backoffLimit"`
		ActiveDeadlineSeconds   int `yaml:"activeDeadlineSeconds"`
		TTLSecondsAfterFinished int `yaml:"ttlSecondsAfterFinished"`
		Template                struct {
			Metadata struct{ Labels map[string]string } `yaml:"metadata"`
			Spec     struct {
				ServiceAccountName string `yaml:"serviceAccountName"`
				RestartPolicy      string `yaml:"restartPolicy"`
				Containers         []struct {
					Image        string
					Args         []string
					VolumeMounts []struct {
						Name      string `yaml:"name"`
						MountPath string `yaml:"mountPath"`
					} `yaml:"volumeMounts"`
				}
				Volumes []struct{ Name string }
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func TestCanary(t *testing.T) {
	c := canaryCluster()
	e := canaryEnv()
	run, err := Canary(context.Background(), root, e, c, canaryOptions())
	if err != nil {
		t.Fatalf("Canary: %v", err)
	}
	if run.Name != canaryName || run.Cluster != "projects/example-project/locations/us-central1/clusters/demo" {
		t.Fatalf("run %+v", run)
	}
	// The Secret before the Job that mounts it, then the Job owns it.
	// (The fake names a Secret from two-space YAML; this one is checked
	// by its content below.)
	if len(c.ops) != 3 || !strings.HasPrefix(c.ops[0], "secret ") || c.ops[1] != "apply Job" || c.ops[2] != "patch secret/"+canaryName {
		t.Fatalf("ops %v", c.ops)
	}
	if len(c.patches) != 1 || !strings.Contains(c.patches[0], `"uid":"uid-1"`) || !strings.Contains(c.patches[0], `"kind":"Job"`) || !strings.HasPrefix(c.patches[0], "secret/"+E2ENamespace+"/"+canaryName+" strategic ") {
		t.Errorf("patches %v", c.patches)
	}
	// Nothing the environment deploys was written: no ax apply, no other
	// Secret, no restart.
	if len(c.axApplied) != 0 || len(c.rollouts) != 0 {
		t.Errorf("the environment was touched: ax %v, rollouts %v", c.axApplied, c.rollouts)
	}
	// The caller's environment is not changed by rendering the candidate.
	if e.Agent.ChatImage == candidateImage || e.Agent.Workspace != "demo-ws" {
		t.Errorf("Canary changed the environment it was given: %+v", e.Agent)
	}

	var j job
	if err := yaml.Unmarshal(run.Job, &j); err != nil {
		t.Fatal(err)
	}
	spec := j.Spec.Template.Spec
	if j.Kind != "Job" || j.Metadata.Name != canaryName || j.Metadata.Namespace != E2ENamespace || j.Metadata.Labels[CanaryLabel] != canaryName ||
		j.Spec.Template.Metadata.Labels[CanaryLabel] != canaryName || j.Spec.Template.Metadata.Labels["app.kubernetes.io/name"] != "shipmind-e2e" {
		t.Errorf("job metadata %+v, pod labels %v", j.Metadata, j.Spec.Template.Metadata.Labels)
	}
	if j.Spec.BackoffLimit != 0 || j.Spec.ActiveDeadlineSeconds != 1800 || j.Spec.TTLSecondsAfterFinished != 86400 {
		t.Errorf("job spec %+v", j.Spec)
	}
	// The nightly's pod: its service account and its router identity.
	if spec.ServiceAccountName != "shipmind-e2e" || spec.RestartPolicy != "Never" || len(spec.Containers) != 1 {
		t.Fatalf("pod %+v", spec)
	}
	ct := spec.Containers[0]
	want := []string{"scenario", "run", "-in-cluster", "-atespace=agents",
		"-router-cert=/run/podidentity/credential-bundle.pem", "-router-trust=/run/servicedns-ca/trust-bundle.pem", "-judge=claude-sonnet-5-5",
		"-task=/canary/task.yaml", "-workspace=/canary/workspace.yaml",
		"-cluster=projects/example-project/locations/us-central1/clusters/demo", "/scenarios/posture-audit"}
	if ct.Image != runnerImage || !slices.Equal(ct.Args, want) {
		t.Errorf("container image %s\nargs %q\nwant %q", ct.Image, ct.Args, want)
	}
	var mounts, volumes []string
	for _, m := range ct.VolumeMounts {
		mounts = append(mounts, m.Name+"="+m.MountPath)
	}
	for _, v := range spec.Volumes {
		volumes = append(volumes, v.Name)
	}
	if !slices.Equal(mounts, []string{"podidentity=/run/podidentity", "servicedns-ca=/run/servicedns-ca", "canary=/canary"}) || !slices.Equal(volumes, []string{"podidentity", "servicedns-ca", "canary"}) {
		t.Errorf("mounts %v, volumes %v", mounts, volumes)
	}
	if !strings.Contains(string(run.Job), "secretName: "+canaryName) {
		t.Errorf("the job doesn't mount the run's secret:\n%s", run.Job)
	}

	// The Secret holds the candidate's chat task and its own Workspace.
	var secret struct {
		Metadata struct {
			Name, Namespace string
			Labels          map[string]string
		}
		Data map[string]string
	}
	if err := yaml.Unmarshal([]byte(c.applied[0]), &secret); err != nil {
		t.Fatal(err)
	}
	if secret.Metadata.Name != canaryName || secret.Metadata.Namespace != E2ENamespace || secret.Metadata.Labels[CanaryLabel] != canaryName {
		t.Errorf("secret metadata %+v", secret.Metadata)
	}
	decode := func(key string) string {
		b, err := base64.StdEncoding.DecodeString(secret.Data[key])
		if err != nil || len(b) == 0 {
			t.Fatalf("secret key %s: %v", key, err)
		}
		return string(b)
	}
	var task struct {
		Kind     string
		Metadata struct{ Name, Atespace string }
		Spec     struct {
			Image     string
			Command   []string
			Workspace struct{ Name string }
			Env       []struct{ Name, Value string }
			Egress    []struct{ Hosts []string }
		}
	}
	if err := yaml.Unmarshal([]byte(decode("task.yaml")), &task); err != nil {
		t.Fatal(err)
	}
	if task.Kind != "Task" || task.Metadata.Name != canaryName || task.Metadata.Atespace != "agents" || task.Spec.Image != candidateImage || task.Spec.Workspace.Name != canaryName {
		t.Errorf("task %+v", task)
	}
	if !slices.Contains(task.Spec.Command, "/opt/core-agent/recipes/gke-chat/.agents/config.json") {
		t.Errorf("the task doesn't run the chat bundle: %q", task.Spec.Command)
	}
	// The candidate checkout's task shape, not the deployed one: the
	// clusters' control-plane egress and the multi-project report scope.
	if len(task.Spec.Egress) == 0 || !slices.Contains(task.Spec.Egress[0].Hosts, "*.us-central1.gke.goog") {
		t.Errorf("egress %+v", task.Spec.Egress)
	}
	envs := map[string]string{}
	for _, v := range task.Spec.Env {
		envs[v.Name] = v.Value
	}
	if envs["CORE_AGENT_ATTACH_TOKEN"] != tok.Attach || envs["LEEWAY_CLUSTERS_FROM"] != "example-project,other-project" {
		t.Errorf("task env %v", envs)
	}
	ws := decode("workspace.yaml")
	if !strings.Contains(ws, "kind: Workspace") || !strings.Contains(ws, `name: "`+canaryName+`"`) || strings.Contains(ws, "demo-ws") {
		t.Errorf("workspace:\n%.400s", ws)
	}
}

func TestCanaryDryRun(t *testing.T) {
	c := &fakeCluster{failOn: "secret"} // any read of the cluster would fail
	o := canaryOptions()
	o.DryRun, o.Cluster = true, "prod"
	run, err := Canary(context.Background(), root, canaryEnv(), c, o)
	if err != nil {
		t.Fatalf("Canary: %v", err)
	}
	if len(c.ops) != 0 || len(c.applied) != 0 {
		t.Errorf("a dry run wrote: %v", c.ops)
	}
	if run.Cluster != "projects/other-project/locations/europe-west1/clusters/prod" || !strings.Contains(string(run.Job), "-cluster="+run.Cluster) {
		t.Errorf("run %+v", run)
	}
}

func TestCanaryRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*env.Env, *CanaryOptions, *fakeCluster)
		want   string
	}{
		{"no chatImage", func(e *env.Env, _ *CanaryOptions, _ *fakeCluster) { e.Agent.ChatImage = "" }, "sets no agent.chatImage"},
		{"image by tag", func(_ *env.Env, o *CanaryOptions, _ *fakeCluster) { o.Image = "registry.example.com/agent:latest" }, "want the candidate agent image pinned by digest"},
		{"runner by tag", func(_ *env.Env, o *CanaryOptions, _ *fakeCluster) { o.RunnerImage = "e2e-runner:latest" }, "want an e2e-runner image pinned by digest"},
		{"scenario path", func(_ *env.Env, o *CanaryOptions, _ *fakeCluster) { o.Scenario = "../x" }, "want a scenario's name"},
		{"no clusters", func(e *env.Env, _ *CanaryOptions, _ *fakeCluster) { e.Agent.Clusters = nil }, "lists no agent.clusters"},
		{"unknown cluster", func(_ *env.Env, o *CanaryOptions, _ *fakeCluster) { o.Cluster = "staging" }, `cluster "staging" is not in the environment's agent.clusters (demo, prod)`},
		{"bad archive", func(_ *env.Env, o *CanaryOptions, _ *fakeCluster) { o.Archive = "runs/stage" }, "want gs://bucket/prefix"},
		{"no tokens", func(_ *env.Env, _ *CanaryOptions, c *fakeCluster) { c.secrets = nil }, "the cluster holds no agent tokens"},
		{"unreadable tokens", func(_ *env.Env, _ *CanaryOptions, c *fakeCluster) { c.failOn = "secret" }, "secret failed"},
		{"secret refused", func(_ *env.Env, _ *CanaryOptions, c *fakeCluster) { c.failOn = "applysecret" }, "is the environment's nightly e2e component deployed?"},
		{"job refused", func(_ *env.Env, _ *CanaryOptions, c *fakeCluster) { c.failOn = "apply" }, "job shipmind-e2e/" + canaryName + ": apply failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, o, c := canaryEnv(), canaryOptions(), canaryCluster()
			tc.mutate(e, &o, c)
			run, err := Canary(context.Background(), root, e, c, o)
			if run != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("run %+v, err %v, want an error containing %q", run, err, tc.want)
			}
			if slices.Contains(c.ops, "apply Job") {
				t.Errorf("a refused canary started a job: %v", c.ops)
			}
		})
	}
}

// A Job the cluster refuses leaves no Secret behind: nothing would ever
// own it, and it holds the attach token.
func TestCanaryJobRefused(t *testing.T) {
	c := canaryCluster()
	c.failOn = "apply"
	if _, err := Canary(context.Background(), root, canaryEnv(), c, canaryOptions()); err == nil || strings.Contains(err.Error(), "by hand") {
		t.Fatalf("err %v", err)
	}
	// Deleted by identity: the delete is not handed the tokens.
	if want := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: " + canaryName + "\n  namespace: " + E2ENamespace + "\n"; c.deleted != want {
		t.Errorf("the refused run's secret was deleted with:\n%s\nwant:\n%s", c.deleted, want)
	}

	// A Secret that can't be removed either is named for a person.
	stuck := &undeletable{fakeCluster: canaryCluster()}
	stuck.failOn = "apply"
	run, err := Canary(context.Background(), root, canaryEnv(), stuck, canaryOptions())
	if run != nil || err == nil || !strings.Contains(err.Error(), "apply failed") || !strings.Contains(err.Error(), "was not removed, delete it by hand: no delete") {
		t.Errorf("run %+v, err %v", run, err)
	}
}

// undeletable is a cluster that refuses deletes as well as whatever its
// fake is set to fail on.
type undeletable struct{ *fakeCluster }

func (undeletable) Delete(context.Context, []byte) (string, error) {
	return "", errors.New("no delete")
}

// A run whose Secret can't be tied to its Job has still started: it is
// returned, with an error saying what to delete by hand.
func TestCanaryUnownedSecret(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fakeCluster)
	}{
		{"job not read", func(c *fakeCluster) { c.failOn = "get" }},
		{"job without a UID", func(c *fakeCluster) { c.objects = map[string]string{"job/" + E2ENamespace + "/" + canaryName: `{}`} }},
		{"job not found", func(c *fakeCluster) { c.objects = nil }},
		{"patch refused", func(c *fakeCluster) { c.failOn = "patch" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := canaryCluster()
			tc.mutate(c)
			run, err := Canary(context.Background(), root, canaryEnv(), c, canaryOptions())
			if run == nil || run.Name != canaryName || err == nil || !strings.Contains(err.Error(), "will outlive it; delete it by hand") {
				t.Errorf("run %+v, err %v", run, err)
			}
		})
	}
}

// The Job is cut from the checkout's nightly CronJob; a checkout without
// one, or with one of another shape, is refused.
func TestCanaryNightlyShape(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		for _, sub := range []string{"agents", "deploy/e2e/substrate"} {
			if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		// The agent templates come from the real checkout.
		if err := os.Symlink(mustAbs(t, filepath.Join(root, "agents/sre-triage")), filepath.Join(dir, "agents/sre-triage")); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if err := os.WriteFile(filepath.Join(dir, e2eManifest), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	const cron = "kind: CronJob\nspec:\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers:\n"
	for _, tc := range []struct{ name, body, want string }{
		{"no manifest", "", "no such file"},
		{"not yaml", "kind: [", e2eManifest},
		{"no cronjob", "kind: Namespace\n", "no CronJob"},
		{"no job template", "kind: CronJob\nspec: {}\n", "has no jobTemplate.spec"},
		{"two containers", cron + "            - name: a\n            - name: b\n", "has no single container"},
		{"other arguments", cron + "            - name: a\n              args: [serve, x, y, z]\n", "has no `scenario run` arguments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, err := Canary(context.Background(), write(t, tc.body), canaryEnv(), canaryCluster(), canaryOptions())
			if run != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("run %+v, err %v, want an error containing %q", run, err, tc.want)
			}
		})
	}
	// A nightly pod with no labels and no mounts still gets the run's.
	bare := cron + "            - name: a\n              args: [scenario, run, -atespace=X, /scenarios/missing-env]\n"
	o := canaryOptions()
	o.DryRun = true
	run, err := Canary(context.Background(), write(t, bare), canaryEnv(), canaryCluster(), o)
	if err != nil || !strings.Contains(string(run.Job), CanaryLabel+": "+canaryName) || !strings.Contains(string(run.Job), "mountPath: /canary") || !strings.Contains(string(run.Job), "-atespace=agents") {
		t.Errorf("bare nightly: err %v\n%s", err, run.Job)
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestNamed(t *testing.T) {
	out, err := named([]byte("kind: Task\nmetadata:\n  atespace: agents\n"), "run-1")
	if err != nil || !strings.Contains(string(out), "name: run-1") || !strings.Contains(string(out), "atespace: agents") {
		t.Errorf("named = %q, %v", out, err)
	}
	if _, err := named([]byte("kind: Task\n"), "run-1"); err == nil || !strings.Contains(err.Error(), "no metadata") {
		t.Errorf("a manifest with no metadata: %v", err)
	}
	if _, err := named([]byte("kind: ["), "run-1"); err == nil {
		t.Error("a manifest that isn't YAML was named")
	}
}

func TestCanaryArchive(t *testing.T) {
	// 1. Explicit Archive sets -archive=<target> on the canary Job.
	o := canaryOptions()
	o.DryRun = true
	o.Archive = "gs://shipmind-scenario-runs/stage"
	run, err := Canary(context.Background(), root, canaryEnv(), &fakeCluster{failOn: "secret"}, o)
	if err != nil {
		t.Fatalf("Canary explicit archive: %v", err)
	}
	if run.Archive != o.Archive {
		t.Fatalf("run.Archive = %q, want %q", run.Archive, o.Archive)
	}
	var j job
	if err := yaml.Unmarshal(run.Job, &j); err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"scenario", "run", "-in-cluster", "-atespace=agents",
		"-router-cert=/run/podidentity/credential-bundle.pem", "-router-trust=/run/servicedns-ca/trust-bundle.pem", "-judge=claude-sonnet-5-5",
		"-archive=gs://shipmind-scenario-runs/stage",
		"-task=/canary/task.yaml", "-workspace=/canary/workspace.yaml",
		"-cluster=projects/example-project/locations/us-central1/clusters/demo", "/scenarios/posture-audit"}
	if got := j.Spec.Template.Spec.Containers[0].Args; !slices.Equal(got, wantArgs) {
		t.Errorf("args\n got %q\nwant %q", got, wantArgs)
	}

	// 2. Overlay auto-detection inherits the nightly CronJob's -archive patch.
	ovDir := t.TempDir()
	ref := strings.Repeat("a", 40)
	kust := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - https://github.com/go-steer/shipmind//deploy/e2e/substrate?ref=" + ref + "\npatches:\n  - path: e2e.yaml\n    target:\n      kind: CronJob\n      labelSelector: shipmind.dev/nightly=scenario\n"
	patch := "- op: add\n  path: /spec/jobTemplate/spec/template/spec/containers/0/args/4\n  value: -archive=gs://shipmind-scenario-runs/dev\n"
	if err := os.WriteFile(filepath.Join(ovDir, "kustomization.yaml"), []byte(kust), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ovDir, "e2e.yaml"), []byte(patch), 0o600); err != nil {
		t.Fatal(err)
	}
	e := canaryEnv()
	e.Dir = ovDir
	oAuto := canaryOptions()
	oAuto.DryRun = true
	runAuto, err := Canary(context.Background(), root, e, &fakeCluster{failOn: "secret"}, oAuto)
	if err != nil {
		t.Fatalf("Canary overlay archive: %v", err)
	}
	if runAuto.Archive != "gs://shipmind-scenario-runs/dev" || !strings.Contains(string(runAuto.Job), "-archive=gs://shipmind-scenario-runs/dev") {
		t.Errorf("overlay archive = %q\n%s", runAuto.Archive, runAuto.Job)
	}

	// 3. Explicit -archive overrides the overlay's target.
	oOver := canaryOptions()
	oOver.DryRun = true
	oOver.Archive = "gs://shipmind-scenario-runs/custom"
	runOver, err := Canary(context.Background(), root, e, &fakeCluster{failOn: "secret"}, oOver)
	if err != nil {
		t.Fatalf("Canary override archive: %v", err)
	}
	if runOver.Archive != "gs://shipmind-scenario-runs/custom" || !strings.Contains(string(runOver.Job), "-archive=gs://shipmind-scenario-runs/custom") || strings.Contains(string(runOver.Job), "/dev") {
		t.Errorf("override archive = %q\n%s", runOver.Archive, runOver.Job)
	}

	// 4. Unrenderable overlay degrades gracefully to empty archive.
	brokenDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(brokenDir, "kustomization.yaml"), []byte("kind: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := overlayArchive(brokenDir, root); got != "" {
		t.Errorf("broken overlay archive = %q, want empty", got)
	}
	if got := overlayArchive(t.TempDir(), root); got != "" {
		t.Errorf("missing kustomization archive = %q, want empty", got)
	}

	// 5. Replacing an existing -archive arg in the base nightly manifest leaves no duplicates.
	for _, tc := range []struct {
		name string
		args string
	}{
		{"single-token", "[scenario, run, -atespace=X, -archive=gs://old/p, /scenarios/missing-env]"},
		{"double-dash", "[scenario, run, -atespace=X, --archive=gs://old/p, /scenarios/missing-env]"},
		{"two-token", "[scenario, run, -atespace=X, -archive, gs://old/p, /scenarios/missing-env]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, sub := range []string{"agents", "deploy/e2e/substrate"} {
				if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(mustAbs(t, filepath.Join(root, "agents/sre-triage")), filepath.Join(dir, "agents/sre-triage")); err != nil {
				t.Fatal(err)
			}
			body := "kind: CronJob\nspec:\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers:\n            - name: a\n              args: " + tc.args + "\n"
			if err := os.WriteFile(filepath.Join(dir, e2eManifest), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			opts := canaryOptions()
			opts.DryRun = true
			opts.Archive = "gs://new/p"
			gotRun, err := Canary(context.Background(), dir, canaryEnv(), &fakeCluster{failOn: "secret"}, opts)
			if err != nil {
				t.Fatalf("Canary: %v", err)
			}
			if strings.Contains(string(gotRun.Job), "gs://old/p") || !strings.Contains(string(gotRun.Job), "-archive=gs://new/p") {
				t.Errorf("replaced archive:\n%s", gotRun.Job)
			}
		})
	}

	// 6. cronJobArchiveArg parses all flag spellings and rejects invalid YAML.
	for _, tc := range []struct {
		name, body, want string
	}{
		{"eq", "spec:\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers:\n            - args: [-archive=gs://b/p]\n", "gs://b/p"},
		{"double-eq", "spec:\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers:\n            - args: [--archive=gs://b/p]\n", "gs://b/p"},
		{"two-tok", "spec:\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers:\n            - args: [-archive, gs://b/p]\n", "gs://b/p"},
		{"none", "spec:\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers:\n            - args: [scenario, run]\n", ""},
		{"bad-yaml", "spec: [", ""},
	} {
		if got := cronJobArchiveArg([]byte(tc.body)); got != tc.want {
			t.Errorf("cronJobArchiveArg(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
