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

// Package cluster is shipmind's only way to touch a cluster: kubectl and
// the ax CLI, each always given an explicit kube context, never the
// current one.
package cluster

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Cluster is what the deploy commands need from a cluster. Kube
// implements it; tests use a fake.
type Cluster interface {
	// Diff compares manifests with the live objects (kubectl diff, read
	// only) and returns the IDs of the objects that would change, as
	// kubectl names them ("apps.v1.Deployment.ns.name").
	Diff(ctx context.Context, manifests []byte) ([]string, error)
	// Apply applies manifests (client-side, as the objects were first
	// created); with dryRun the server validates without persisting.
	// Never pass a Secret: kubectl's errors can echo what it applied.
	Apply(ctx context.Context, manifests []byte, dryRun bool) (string, error)
	// ApplySecret applies one Secret server-side. Its errors never carry
	// kubectl's output, which can include the Secret's data.
	ApplySecret(ctx context.Context, manifest []byte, dryRun bool) error
	// Delete deletes the objects in manifests, ignoring ones not found.
	Delete(ctx context.Context, manifests []byte) (string, error)
	// Secret returns a Secret's data, or found=false if it doesn't exist.
	Secret(ctx context.Context, namespace, name string) (data map[string][]byte, found bool, err error)
	// SecretUpdated returns when a Secret was last written (the latest
	// managedFields time), or found=false.
	SecretUpdated(ctx context.Context, namespace, name string) (t time.Time, found bool, err error)
	// PodStarts returns the start times of a Deployment's running pods.
	PodStarts(ctx context.Context, namespace, deployment string) ([]time.Time, error)
	// RolloutRestart restarts a Deployment's pods.
	RolloutRestart(ctx context.Context, namespace, name string) error
	// RolloutStatus waits for a Deployment to finish rolling out.
	RolloutStatus(ctx context.Context, namespace, name string) error
	// Workloads lists every Deployment, DaemonSet and StatefulSet in a
	// namespace with its images and whether its rollout has finished.
	Workloads(ctx context.Context, namespace string) ([]Workload, error)
	// AxWorkspace returns an ax Workspace as YAML, or found=false.
	AxWorkspace(ctx context.Context, atespace, name string) (yaml []byte, found bool, err error)
	// AxApply applies ax resources in an atespace.
	AxApply(ctx context.Context, atespace string, manifests []byte) error
}

// Kube runs kubectl and ax against one kube context.
type Kube struct {
	Context string
	// Kubectl and Ax are the binaries; empty means "kubectl" and, for ax,
	// $SHIPMIND_AX or "ax".
	Kubectl, Ax string
}

func (k Kube) kubectl() string {
	if k.Kubectl != "" {
		return k.Kubectl
	}
	return "kubectl"
}

func (k Kube) ax() string {
	if k.Ax != "" {
		return k.Ax
	}
	if a := os.Getenv("SHIPMIND_AX"); a != "" {
		return a
	}
	return "ax"
}

// run executes bin with the context flag first, feeding stdin.
func (k Kube) run(ctx context.Context, bin string, stdin []byte, args ...string) (stdout []byte, exit int, err error) {
	if k.Context == "" {
		return nil, -1, errors.New("no kube context: shipmind never uses the current context")
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"--context", k.Context}, args...)...) //nolint:gosec // fixed binaries, arguments, no shell
	// An external diff program changes kubectl diff's output format,
	// which ParseDiff reads; kubectl treats an empty value as unset.
	cmd.Env = append(os.Environ(), "KUBECTL_EXTERNAL_DIFF=")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.Bytes(), ee.ExitCode(), fmt.Errorf("%s %s: exit %d: %s", bin, strings.Join(args, " "), ee.ExitCode(), strings.TrimSpace(errb.String()))
	}
	if err != nil {
		return nil, -1, err
	}
	return out.Bytes(), 0, nil
}

// diffHeader matches kubectl diff's per-object header, whose last path
// element is the object ID.
var diffHeader = regexp.MustCompile(`(?m)^diff -u -N \S+/(\S+) \S+$`)

// Diff implements Cluster.
func (k Kube) Diff(ctx context.Context, manifests []byte) ([]string, error) {
	out, exit, err := k.run(ctx, k.kubectl(), manifests, "diff", "-f", "-")
	switch exit {
	case 0:
		return nil, nil
	case 1: // differences found
		ids := ParseDiff(out)
		if len(ids) == 0 {
			// Differences, but none in the format ParseDiff reads: never
			// report that as "no changes".
			return nil, errors.New("kubectl diff reported differences shipmind could not read")
		}
		return ids, nil
	default:
		return nil, err
	}
}

// ParseDiff extracts the object IDs from kubectl diff output.
func ParseDiff(out []byte) []string {
	var ids []string
	for _, m := range diffHeader.FindAllSubmatch(out, -1) {
		ids = append(ids, string(m[1]))
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// Apply implements Cluster.
func (k Kube) Apply(ctx context.Context, manifests []byte, dryRun bool) (string, error) {
	args := []string{"apply", "-f", "-"}
	if dryRun {
		args = append(args, "--dry-run=server")
	}
	out, _, err := k.run(ctx, k.kubectl(), manifests, args...)
	return string(out), err
}

// ApplySecret implements Cluster.
func (k Kube) ApplySecret(ctx context.Context, manifest []byte, dryRun bool) error {
	args := []string{"apply", "--server-side", "--field-manager=shipmind", "--force-conflicts", "-f", "-"}
	if dryRun {
		args = append(args, "--dry-run=server")
	}
	if _, exit, err := k.run(ctx, k.kubectl(), manifest, args...); err != nil {
		return fmt.Errorf("kubectl apply of a Secret failed (exit %d; output withheld because it can contain the Secret's data)", exit)
	}
	return nil
}

// SecretUpdated implements Cluster.
func (k Kube) SecretUpdated(ctx context.Context, namespace, name string) (time.Time, bool, error) {
	out, _, err := k.run(ctx, k.kubectl(), nil, "get", "secret", "-n", namespace, name, "--ignore-not-found",
		"-o", "jsonpath={range .metadata.managedFields[*]}{.time}{\"\\n\"}{end}")
	if err != nil {
		return time.Time{}, false, err
	}
	return latest(out)
}

func latest(out []byte) (time.Time, bool, error) {
	var t time.Time
	found := false
	for _, line := range strings.Fields(string(out)) {
		ts, err := time.Parse(time.RFC3339, line)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("managedFields time %q: %w", line, err)
		}
		found = true
		if ts.After(t) {
			t = ts
		}
	}
	return t, found, nil
}

// PodStarts implements Cluster.
func (k Kube) PodStarts(ctx context.Context, namespace, deployment string) ([]time.Time, error) {
	out, _, err := k.run(ctx, k.kubectl(), nil, "get", "deployment", "-n", namespace, deployment, "--ignore-not-found",
		"-o", "jsonpath={.spec.selector.matchLabels}")
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return nil, err
	}
	var labels map[string]string
	if err := json.Unmarshal(out, &labels); err != nil {
		return nil, fmt.Errorf("deployment %s/%s selector: %w", namespace, deployment, err)
	}
	var sel []string
	for key, v := range labels {
		sel = append(sel, key+"="+v)
	}
	slices.Sort(sel)
	out, _, err = k.run(ctx, k.kubectl(), nil, "get", "pods", "-n", namespace, "-l", strings.Join(sel, ","),
		"--field-selector=status.phase=Running", "-o", "jsonpath={range .items[*]}{.status.startTime}{\"\\n\"}{end}")
	if err != nil {
		return nil, err
	}
	var starts []time.Time
	for _, line := range strings.Fields(string(out)) {
		ts, err := time.Parse(time.RFC3339, line)
		if err != nil {
			return nil, fmt.Errorf("pod startTime %q: %w", line, err)
		}
		starts = append(starts, ts)
	}
	return starts, nil
}

// Workload is a Deployment, DaemonSet or StatefulSet as it runs.
type Workload struct {
	// ID is "<Kind>/<namespace>/<name>".
	ID string
	// Images are its init and regular containers' images, in order.
	Images []string
	// Current is false while a rollout hasn't finished: the controller
	// hasn't seen the latest spec, or pods from an older template remain.
	// A stuck rollout keeps the new template but runs the old pods, so
	// the template alone can't say what runs.
	Current bool
}

// Workloads implements Cluster.
func (k Kube) Workloads(ctx context.Context, namespace string) ([]Workload, error) {
	out, _, err := k.run(ctx, k.kubectl(), nil, "get", "deployments,daemonsets,statefulsets", "-n", namespace, "-o", "json")
	if err != nil {
		return nil, err
	}
	return parseWorkloads(out)
}

func parseWorkloads(out []byte) ([]Workload, error) {
	type container struct{ Image string }
	var list struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name       string `json:"name"`
				Namespace  string `json:"namespace"`
				Generation int64  `json:"generation"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int64 `json:"replicas"`
				Template struct {
					Spec struct {
						InitContainers []container `json:"initContainers"`
						Containers     []container `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				ObservedGeneration     int64  `json:"observedGeneration"`
				Replicas               int64  `json:"replicas"`
				UpdatedReplicas        int64  `json:"updatedReplicas"`
				DesiredNumberScheduled int64  `json:"desiredNumberScheduled"`
				UpdatedNumberScheduled int64  `json:"updatedNumberScheduled"`
				CurrentRevision        string `json:"currentRevision"`
				UpdateRevision         string `json:"updateRevision"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	var ws []Workload
	for _, it := range list.Items {
		w := Workload{ID: it.Kind + "/" + it.Metadata.Namespace + "/" + it.Metadata.Name}
		for _, c := range it.Spec.Template.Spec.InitContainers {
			w.Images = append(w.Images, c.Image)
		}
		for _, c := range it.Spec.Template.Spec.Containers {
			w.Images = append(w.Images, c.Image)
		}
		want := int64(1)
		if it.Spec.Replicas != nil {
			want = *it.Spec.Replicas
		}
		st := it.Status
		seen := st.ObservedGeneration >= it.Metadata.Generation
		switch it.Kind {
		case "Deployment":
			w.Current = seen && st.UpdatedReplicas == want && st.Replicas == want
		case "DaemonSet":
			w.Current = seen && st.UpdatedNumberScheduled == st.DesiredNumberScheduled
		case "StatefulSet":
			w.Current = seen && st.UpdatedReplicas == want && (st.UpdateRevision == "" || st.CurrentRevision == st.UpdateRevision)
		}
		ws = append(ws, w)
	}
	return ws, nil
}

// RolloutRestart implements Cluster.
func (k Kube) RolloutRestart(ctx context.Context, namespace, name string) error {
	_, _, err := k.run(ctx, k.kubectl(), nil, "rollout", "restart", "-n", namespace, "deployment/"+name)
	return err
}

// Delete implements Cluster.
func (k Kube) Delete(ctx context.Context, manifests []byte) (string, error) {
	out, _, err := k.run(ctx, k.kubectl(), manifests, "delete", "--ignore-not-found", "-f", "-")
	return string(out), err
}

// Secret implements Cluster.
func (k Kube) Secret(ctx context.Context, namespace, name string) (map[string][]byte, bool, error) {
	out, _, err := k.run(ctx, k.kubectl(), nil, "get", "secret", "-n", namespace, name, "-o", "json", "--ignore-not-found")
	if err != nil {
		return nil, false, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, false, nil
	}
	return decodeSecret(out)
}

func decodeSecret(out []byte) (map[string][]byte, bool, error) {
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, false, err
	}
	data := make(map[string][]byte, len(s.Data))
	for key, v := range s.Data {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, false, fmt.Errorf("secret key %s: %w", key, err)
		}
		data[key] = b
	}
	return data, true, nil
}

// RolloutStatus implements Cluster.
func (k Kube) RolloutStatus(ctx context.Context, namespace, name string) error {
	_, _, err := k.run(ctx, k.kubectl(), nil, "rollout", "status", "-n", namespace, "deployment/"+name, "--timeout=300s")
	return err
}

// AxWorkspace implements Cluster.
func (k Kube) AxWorkspace(ctx context.Context, atespace, name string) ([]byte, bool, error) {
	out, exit, err := k.run(ctx, k.ax(), nil, "-a", atespace, "get", "workspace", name)
	if err != nil {
		// Only ax's own answer means "missing": the gRPC NotFound code.
		// A binary that isn't there ("executable file not found") or any
		// other failure is an error, never a missing Workspace.
		if exit > 0 && strings.Contains(err.Error(), "code = NotFound") {
			return nil, false, nil
		}
		return nil, false, err
	}
	return out, true, nil
}

// AxApply implements Cluster.
func (k Kube) AxApply(ctx context.Context, atespace string, manifests []byte) error {
	_, _, err := k.run(ctx, k.ax(), manifests, "-a", atespace, "apply", "-f", "-")
	return err
}

// SecretManifest renders a Secret holding data, for Apply.
func SecretManifest(namespace, name string, data map[string][]byte) []byte {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\ntype: Opaque\ndata:\n", name, namespace)
	for _, key := range keys {
		fmt.Fprintf(&b, "  %s: %s\n", key, base64.StdEncoding.EncodeToString(data[key]))
	}
	return []byte(b.String())
}
