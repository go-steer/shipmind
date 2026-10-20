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

package cluster

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fake writes a stand-in for kubectl and ax into dir. It logs its
// arguments and stdin, and answers from files in dir:
// diff.out/diff.exit, secret.json, workspace.yaml.
const fakeScript = `#!/bin/sh
d=$(dirname "$0")
echo "$*" >> "$d/args.log"
cat > "$d/stdin.last"
case "$*" in
  *" fail "*) echo boom >&2; exit 2 ;;
  *" diff "*) [ -f "$d/diff.out" ] && cat "$d/diff.out"; exit $(cat "$d/diff.exit" 2>/dev/null || echo 0) ;;
  *" get secret "*"managedFields"*) [ -f "$d/managed.txt" ] && cat "$d/managed.txt"; exit 0 ;;
  *" get secret "*) [ -f "$d/secret.json" ] && cat "$d/secret.json"; exit 0 ;;
  *" get deployments,daemonsets,statefulsets "*) [ -f "$d/workloads.json" ] && cat "$d/workloads.json"; exit 0 ;;
  *" get deployment "*) [ -f "$d/selector.json" ] && cat "$d/selector.json"; exit 0 ;;
  *" get pods "*) echo "$*" > "$d/pods.args"; [ -f "$d/starts.txt" ] && cat "$d/starts.txt"; exit 0 ;;
  *" apply --server-side "*) if [ -f "$d/ssa.fail" ]; then echo "error when applying patch: data: c2VjcmV0" >&2; exit 1; fi; echo applied ;;
  *" get workspace "*) if [ -f "$d/workspace.yaml" ]; then cat "$d/workspace.yaml"; else echo 'Error: getting workspace: rpc error: code = NotFound desc = workspace "ws" not found' >&2; exit 1; fi ;;
  *) echo ok ;;
esac
`

func fake(t *testing.T) (Kube, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake")
	if err := os.WriteFile(bin, []byte(fakeScript), 0o700); err != nil { //nolint:gosec // test helper must be executable
		t.Fatal(err)
	}
	return Kube{Context: "demo", Kubectl: bin, Ax: bin}, dir
}

func put(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func lastArgs(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "args.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

const diffOut = `diff -u -N /tmp/LIVE-1/apps.v1.Deployment.ax-system.ax-redis /tmp/MERGED-2/apps.v1.Deployment.ax-system.ax-redis
--- /tmp/LIVE-1/apps.v1.Deployment.ax-system.ax-redis
+++ /tmp/MERGED-2/apps.v1.Deployment.ax-system.ax-redis
-        image: redis:7-alpine
diff -u -N /tmp/LIVE-1/rbac.authorization.k8s.io.v1.ClusterRole..lookout-ax-watch /tmp/MERGED-2/rbac.authorization.k8s.io.v1.ClusterRole..lookout-ax-watch
`

func TestDiff(t *testing.T) {
	k, dir := fake(t)
	ctx := context.Background()
	ids, err := k.Diff(ctx, []byte("kind: X\n"))
	if err != nil || ids != nil {
		t.Errorf("no diff: %v, %v", ids, err)
	}
	if got := lastArgs(t, dir); got != "--context demo diff -f -" {
		t.Errorf("args %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "stdin.last")); string(b) != "kind: X\n" {
		t.Errorf("stdin %q", b)
	}
	put(t, filepath.Join(dir, "diff.out"), diffOut)
	put(t, filepath.Join(dir, "diff.exit"), "1")
	ids, err = k.Diff(ctx, nil)
	want := []string{"apps.v1.Deployment.ax-system.ax-redis", "rbac.authorization.k8s.io.v1.ClusterRole..lookout-ax-watch"}
	if err != nil || !slices.Equal(ids, want) {
		t.Errorf("diff: %v, %v", ids, err)
	}
	put(t, filepath.Join(dir, "diff.exit"), "2")
	if _, err := k.Diff(ctx, nil); err == nil {
		t.Error("diff exit 2 succeeded")
	}
	// Differences in a format ParseDiff can't read are an error, never
	// "no changes" (an external diff program changes the format).
	put(t, filepath.Join(dir, "diff.exit"), "1")
	put(t, filepath.Join(dir, "diff.out"), "Files /tmp/LIVE-1/x and /tmp/MERGED-2/x differ\n")
	if _, err := k.Diff(ctx, nil); err == nil || !strings.Contains(err.Error(), "could not read") {
		t.Errorf("unreadable diff: err = %v", err)
	}
}

func TestDiffIgnoresExternalDiff(t *testing.T) {
	k, dir := fake(t)
	script := "#!/bin/sh\necho \"KUBECTL_EXTERNAL_DIFF=[$KUBECTL_EXTERNAL_DIFF]\" > \"$(dirname \"$0\")/env.txt\"\n"
	if err := os.WriteFile(k.Kubectl, []byte(script), 0o700); err != nil { //nolint:gosec // test helper must be executable
		t.Fatal(err)
	}
	t.Setenv("KUBECTL_EXTERNAL_DIFF", "colordiff -N -u")
	if _, err := k.Diff(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "env.txt")); string(b) != "KUBECTL_EXTERNAL_DIFF=[]\n" {
		t.Errorf("kubectl saw %q", b)
	}
}

func TestApplySecret(t *testing.T) {
	k, dir := fake(t)
	ctx := context.Background()
	if err := k.ApplySecret(ctx, []byte("kind: Secret\n"), true); err != nil {
		t.Fatal(err)
	}
	if got := lastArgs(t, dir); got != "--context demo apply --server-side --field-manager=shipmind --force-conflicts -f - --dry-run=server" {
		t.Errorf("args %q", got)
	}
	put(t, filepath.Join(dir, "ssa.fail"), "")
	err := k.ApplySecret(ctx, []byte("kind: Secret\n"), false)
	if err == nil || strings.Contains(err.Error(), "c2VjcmV0") || !strings.Contains(err.Error(), "withheld") {
		t.Errorf("err = %v, want a failure that withholds kubectl's output", err)
	}
}

func TestSecretUpdatedAndPodStarts(t *testing.T) {
	k, dir := fake(t)
	ctx := context.Background()
	if _, found, err := k.SecretUpdated(ctx, "ns", "s"); err != nil || found {
		t.Errorf("no secret: found=%v, %v", found, err)
	}
	put(t, filepath.Join(dir, "managed.txt"), "2026-10-19T09:00:00Z\n2026-10-20T10:00:00Z\n2026-10-18T00:00:00Z\n")
	ts, found, err := k.SecretUpdated(ctx, "ns", "s")
	if err != nil || !found || ts.Format("2006-01-02T15") != "2026-10-20T10" {
		t.Errorf("SecretUpdated = %v, %v, %v", ts, found, err)
	}
	put(t, filepath.Join(dir, "managed.txt"), "yesterday\n")
	if _, _, err := k.SecretUpdated(ctx, "ns", "s"); err == nil {
		t.Error("bad time accepted")
	}

	if starts, err := k.PodStarts(ctx, "ns", "web"); err != nil || starts != nil {
		t.Errorf("no deployment: %v, %v", starts, err)
	}
	put(t, filepath.Join(dir, "selector.json"), `{"app":"web","tier":"x"}`)
	put(t, filepath.Join(dir, "starts.txt"), "2026-10-20T11:00:00Z\n")
	starts, err := k.PodStarts(ctx, "ns", "web")
	if err != nil || len(starts) != 1 {
		t.Errorf("PodStarts = %v, %v", starts, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "pods.args")); !strings.Contains(string(b), "-l app=web,tier=x") {
		t.Errorf("pods query %q", b)
	}
	put(t, filepath.Join(dir, "starts.txt"), "soon\n")
	if _, err := k.PodStarts(ctx, "ns", "web"); err == nil {
		t.Error("bad start time accepted")
	}
	put(t, filepath.Join(dir, "selector.json"), `{`)
	if _, err := k.PodStarts(ctx, "ns", "web"); err == nil {
		t.Error("bad selector accepted")
	}
	if err := k.RolloutRestart(ctx, "ns", "web"); err != nil {
		t.Fatal(err)
	}
	if got := lastArgs(t, dir); got != "--context demo rollout restart -n ns deployment/web" {
		t.Errorf("restart args %q", got)
	}
}

func TestApplyDeleteRollout(t *testing.T) {
	k, dir := fake(t)
	ctx := context.Background()
	if out, err := k.Apply(ctx, []byte("x"), false); err != nil || out != "ok\n" {
		t.Errorf("Apply: %q, %v", out, err)
	}
	if got := lastArgs(t, dir); got != "--context demo apply -f -" {
		t.Errorf("args %q", got)
	}
	if _, err := k.Apply(ctx, []byte("x"), true); err != nil {
		t.Fatal(err)
	}
	if got := lastArgs(t, dir); got != "--context demo apply -f - --dry-run=server" {
		t.Errorf("dry-run args %q", got)
	}
	if _, err := k.Delete(ctx, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got := lastArgs(t, dir); got != "--context demo delete --ignore-not-found -f -" {
		t.Errorf("delete args %q", got)
	}
	if err := k.RolloutStatus(ctx, "ns", "web"); err != nil {
		t.Fatal(err)
	}
	if got := lastArgs(t, dir); got != "--context demo rollout status -n ns deployment/web --timeout=300s" {
		t.Errorf("rollout args %q", got)
	}
}

func TestSecret(t *testing.T) {
	k, dir := fake(t)
	ctx := context.Background()
	if _, found, err := k.Secret(ctx, "ns", "s"); err != nil || found {
		t.Errorf("missing secret: found=%v, %v", found, err)
	}
	put(t, filepath.Join(dir, "secret.json"), `{"data":{"token":"YWJj"}}`)
	data, found, err := k.Secret(ctx, "ns", "s")
	if err != nil || !found || string(data["token"]) != "abc" {
		t.Errorf("secret: %v, %v, %v", data, found, err)
	}
	put(t, filepath.Join(dir, "secret.json"), `{"data":{"token":"!!"}}`)
	if _, _, err := k.Secret(ctx, "ns", "s"); err == nil {
		t.Error("bad base64 accepted")
	}
	put(t, filepath.Join(dir, "secret.json"), `{`)
	if _, _, err := k.Secret(ctx, "ns", "s"); err == nil {
		t.Error("bad JSON accepted")
	}
}

func TestAx(t *testing.T) {
	k, dir := fake(t)
	ctx := context.Background()
	if _, found, err := k.AxWorkspace(ctx, "agents", "ws"); err != nil || found {
		t.Errorf("missing workspace: found=%v, %v", found, err)
	}
	if got := lastArgs(t, dir); got != "--context demo -a agents get workspace ws" {
		t.Errorf("args %q", got)
	}
	put(t, filepath.Join(dir, "workspace.yaml"), "kind: Workspace\n")
	if b, found, err := k.AxWorkspace(ctx, "agents", "ws"); err != nil || !found || string(b) != "kind: Workspace\n" {
		t.Errorf("workspace: %q, %v, %v", b, found, err)
	}
	if err := k.AxApply(ctx, "agents", []byte("kind: Workspace\n")); err != nil {
		t.Fatal(err)
	}
	if got := lastArgs(t, dir); got != "--context demo -a agents apply -f -" {
		t.Errorf("apply args %q", got)
	}
	if _, _, err := k.AxWorkspace(ctx, "fail", "ws"); err == nil {
		t.Error("ax failure not reported")
	}
	// A missing ax binary is an error, not a missing Workspace, even
	// though its message says "not found".
	k.Ax = filepath.Join(t.TempDir(), "no-such-ax")
	if _, found, err := k.AxWorkspace(ctx, "agents", "ws"); err == nil || found {
		t.Errorf("missing binary: found=%v, err=%v; want an error", found, err)
	}
}

func TestNoContext(t *testing.T) {
	k, _ := fake(t)
	k.Context = ""
	if _, err := k.Apply(context.Background(), nil, false); err == nil || !strings.Contains(err.Error(), "never uses the current context") {
		t.Errorf("err = %v", err)
	}
}

func TestDefaults(t *testing.T) {
	var k Kube
	if k.kubectl() != "kubectl" {
		t.Errorf("kubectl() = %q", k.kubectl())
	}
	t.Setenv("SHIPMIND_AX", "/opt/ax")
	if k.ax() != "/opt/ax" {
		t.Errorf("ax() = %q", k.ax())
	}
	t.Setenv("SHIPMIND_AX", "")
	if k.ax() != "ax" {
		t.Errorf("ax() = %q", k.ax())
	}
	k.Kubectl = "/nonexistent/kubectl"
	k.Context = "demo"
	if _, err := k.Apply(context.Background(), nil, false); err == nil {
		t.Error("missing binary not reported")
	}
}

func TestSecretManifest(t *testing.T) {
	got := string(SecretManifest("ns", "s", map[string][]byte{"b": []byte("2"), "a": []byte("1")}))
	want := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\n  namespace: ns\ntype: Opaque\ndata:\n  a: MQ==\n  b: Mg==\n"
	if got != want {
		t.Errorf("SecretManifest =\n%s\nwant\n%s", got, want)
	}
}

func TestWorkloads(t *testing.T) {
	k, dir := fake(t)
	put(t, filepath.Join(dir, "workloads.json"), `{"items":[
	  {"kind":"DaemonSet","metadata":{"name":"atelet","namespace":"ate-system","generation":3},
	   "spec":{"template":{"spec":{"initContainers":[{"image":"init:1"}],"containers":[{"image":"atelet:1"},{"image":"plugin:1"}]}}},
	   "status":{"observedGeneration":3,"desiredNumberScheduled":2,"updatedNumberScheduled":2}},
	  {"kind":"Deployment","metadata":{"name":"api","namespace":"ate-system","generation":5},
	   "spec":{"replicas":2,"template":{"spec":{"containers":[{"image":"api:2"}]}}},
	   "status":{"observedGeneration":5,"replicas":3,"updatedReplicas":2}},
	  {"kind":"Deployment","metadata":{"name":"one","namespace":"ate-system","generation":1},
	   "spec":{"template":{"spec":{"containers":[{"image":"one:1"}]}}},
	   "status":{"observedGeneration":1,"replicas":1,"updatedReplicas":1}},
	  {"kind":"StatefulSet","metadata":{"name":"pg","namespace":"ate-system","generation":2},
	   "spec":{"replicas":1,"template":{"spec":{"containers":[{"image":"pg:1"}]}}},
	   "status":{"observedGeneration":1,"updatedReplicas":1,"currentRevision":"a","updateRevision":"a"}}]}`)
	ws, err := k.Workloads(context.Background(), "ate-system")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Workload{}
	for _, w := range ws {
		got[w.ID] = w
	}
	if w := got["DaemonSet/ate-system/atelet"]; !w.Current || !slices.Equal(w.Images, []string{"init:1", "atelet:1", "plugin:1"}) {
		t.Errorf("atelet = %+v", w)
	}
	// An old pod still running: replicas 3 against 2 wanted.
	if w := got["Deployment/ate-system/api"]; w.Current {
		t.Errorf("api with an old pod left = %+v, want not current", w)
	}
	// replicas defaults to 1.
	if w := got["Deployment/ate-system/one"]; !w.Current {
		t.Errorf("one = %+v, want current", w)
	}
	// The controller hasn't seen generation 2.
	if w := got["StatefulSet/ate-system/pg"]; w.Current {
		t.Errorf("pg = %+v, want not current", w)
	}
	put(t, filepath.Join(dir, "workloads.json"), "{")
	if _, err := k.Workloads(context.Background(), "ate-system"); err == nil {
		t.Error("bad JSON accepted")
	}
}

func TestInClusterKubeconfig(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := InClusterKubeconfig(t.TempDir()); err == nil {
		t.Error("InClusterKubeconfig outside a cluster succeeded")
	}
	for host, want := range map[string]string{"10.0.0.1": "https://10.0.0.1:443", "fd00::1": "https://[fd00::1]:443"} {
		t.Setenv("KUBERNETES_SERVICE_HOST", host)
		t.Setenv("KUBERNETES_SERVICE_PORT", "443")
		p, err := InClusterKubeconfig(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		if !strings.Contains(string(b), "server: "+want) || !strings.Contains(string(b), "current-context: "+InClusterContext) ||
			!strings.Contains(string(b), "tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token") {
			t.Errorf("kubeconfig for %s:\n%s", host, b)
		}
	}
}

func TestDeleteLabeled(t *testing.T) {
	k, dir := fake(t)
	if _, err := k.DeleteLabeled(context.Background(), "lab", "deployments,configmaps", "app=x"); err != nil {
		t.Fatal(err)
	}
	if got := lastArgs(t, dir); got != "--context demo delete deployments,configmaps -n lab -l app=x --ignore-not-found" {
		t.Errorf("args %q", got)
	}
}
