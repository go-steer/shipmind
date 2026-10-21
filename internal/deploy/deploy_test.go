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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/shipmind/internal/agent"
	"github.com/go-steer/shipmind/internal/cluster"
	"github.com/go-steer/shipmind/internal/env"
	"github.com/go-steer/shipmind/internal/render"
)

const root = "../.."

var tok = agent.Tokens{Inject: strings.Repeat("a", 32), Attach: strings.Repeat("b", 32)}

func testEnv() *env.Env {
	return &env.Env{
		Context: "demo",
		Agent: env.Agent{
			Atespace: "agents", Project: "example-project", Workspace: "demo-ws",
			Image:    "registry.example.com/agent@sha256:" + strings.Repeat("0", 64),
			Clusters: []env.Cluster{{Name: "demo", Path: "projects/example-project/locations/us-central1/clusters/demo"}},
		},
		Expect: []env.Expected{{Object: "apps.v1.Deployment.ns.redis", Reason: "digest pin"}},
	}
}

func plan(t *testing.T) *Plan {
	t.Helper()
	objs, err := render.Build("testdata/app")
	if err != nil {
		t.Fatal(err)
	}
	return &Plan{Env: testEnv(), Root: root, Objects: objs, Ref: strings.Repeat("f", 40)}
}

// fakeCluster records writes and serves Secrets and the Workspace from
// memory.
type fakeCluster struct {
	// missingNS are namespaces the cluster doesn't have yet; every other
	// namespace exists.
	missingNS     []string
	diffed        string
	workloads     map[string][]cluster.Workload
	ops           []string // every write, in order
	secretUpdated time.Time
	// objectTimes are other objects' write times, "kind/ns/name"; a write
	// through Apply advances a policy ConfigMap's when policyChanges.
	objectTimes   map[string]time.Time
	policyChanges bool
	podStarts     []time.Time
	diff          []string
	secrets       map[string]map[string][]byte
	workspace     []byte
	applied       []string
	dryRuns       int
	axApplied     []string
	rollouts      []string
	deleted       string
	failOn        string
}

func (f *fakeCluster) fail(op string) error {
	if f.failOn == op {
		return errors.New(op + " failed")
	}
	return nil
}

func (f *fakeCluster) Diff(_ context.Context, m []byte) ([]string, error) {
	f.diffed = string(m)
	return f.diff, f.fail("diff")
}

func (f *fakeCluster) Namespaces(context.Context) ([]string, error) {
	all := []string{"ns", "lookout-ax", "default", "ax-system"}
	var have []string
	for _, n := range all {
		if !contains(f.missingNS, n) {
			have = append(have, n)
		}
	}
	return have, f.fail("namespaces")
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func (f *fakeCluster) Apply(_ context.Context, m []byte, dryRun bool) (string, error) {
	if err := f.fail("apply"); err != nil {
		return "", err
	}
	if dryRun {
		f.dryRuns++
		return "", nil
	}
	f.applied = append(f.applied, string(m))
	f.ops = append(f.ops, "apply "+firstKind(m))
	if f.policyChanges {
		if f.objectTimes == nil {
			f.objectTimes = map[string]time.Time{}
		}
		key := "configmap/ate-system/k8s-credential-provider-namespace-policy"
		f.objectTimes[key] = f.objectTimes[key].Add(time.Hour)
	}
	return "applied\n", nil
}

func firstKind(m []byte) string {
	for _, line := range strings.Split(string(m), "\n") {
		if strings.HasPrefix(line, "kind: ") {
			return strings.TrimPrefix(line, "kind: ")
		}
	}
	return "?"
}

func (f *fakeCluster) ApplySecret(_ context.Context, m []byte, dryRun bool) error {
	if err := f.fail("applysecret"); err != nil {
		return err
	}
	if dryRun {
		f.dryRuns++
		return nil
	}
	f.applied = append(f.applied, string(m))
	name := "?"
	for _, line := range strings.Split(string(m), "\n") {
		if strings.HasPrefix(line, "  name: ") {
			name = strings.TrimPrefix(line, "  name: ")
		}
	}
	f.ops = append(f.ops, "secret "+name)
	return nil
}

func (f *fakeCluster) ObjectUpdated(_ context.Context, kind, ns, name string) (time.Time, bool, error) {
	if err := f.fail("objectupdated"); err != nil {
		return time.Time{}, false, err
	}
	if kind == "secret" {
		return f.secretUpdated, !f.secretUpdated.IsZero(), nil
	}
	t, ok := f.objectTimes[kind+"/"+ns+"/"+name]
	return t, ok, nil
}

func (f *fakeCluster) PodStarts(context.Context, string, string) ([]time.Time, error) {
	return f.podStarts, f.fail("podstarts")
}

func (f *fakeCluster) RolloutRestart(_ context.Context, ns, name string) error {
	f.ops = append(f.ops, "restart "+ns+"/"+name)
	return f.fail("restart")
}

func (f *fakeCluster) Delete(_ context.Context, m []byte) (string, error) {
	f.deleted = string(m)
	return "deleted\n", f.fail("delete")
}

func (f *fakeCluster) Secret(_ context.Context, ns, name string) (map[string][]byte, bool, error) {
	if err := f.fail("secret"); err != nil {
		return nil, false, err
	}
	d, ok := f.secrets[ns+"/"+name]
	return d, ok, nil
}

func (f *fakeCluster) RolloutStatus(_ context.Context, ns, name string) error {
	f.rollouts = append(f.rollouts, ns+"/"+name)
	f.ops = append(f.ops, "rollout "+ns+"/"+name)
	return f.fail("rollout")
}

func (f *fakeCluster) Workloads(_ context.Context, ns string) ([]cluster.Workload, error) {
	return f.workloads[ns], f.fail("workloads")
}

func (f *fakeCluster) AxWorkspace(context.Context, string, string) ([]byte, bool, error) {
	f.ops = append(f.ops, "ax get")
	return f.workspace, f.workspace != nil, f.fail("axget")
}

func (f *fakeCluster) AxApply(_ context.Context, _ string, m []byte) error {
	f.axApplied = append(f.axApplied, string(m))
	f.ops = append(f.ops, "ax apply")
	return f.fail("axapply")
}

// inSync returns a cluster that matches plan(t) exactly, with tokens in
// the tokens Secret.
func inSync(t *testing.T) *fakeCluster {
	t.Helper()
	task, err := agent.Task(root, testEnv(), tok)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := agent.Workspace(root, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	return &fakeCluster{
		secrets: map[string]map[string][]byte{
			Namespace + "/" + TokensSecret: {"inject": []byte(tok.Inject), "attach": []byte(tok.Attach)},
			Namespace + "/" + AgentSecret:  {"token": []byte(tok.Attach), "task.yaml": task},
		},
		// As ax prints it: server metadata added, YAML reformatted.
		workspace: []byte(strings.Replace(string(ws), "  atespace: agents\n", "  atespace: agents\n  createTime: 2026-10-19T09:43:36Z\n", 1)),
	}
}

func TestStatusInSync(t *testing.T) {
	r, err := Status(context.Background(), plan(t), inSync(t))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(r.Changes) != 0 || r.Rendered != 2 || r.Tokens != "secret lookout-ax/agent-tokens" {
		t.Errorf("report = %+v", r)
	}
}

func TestStatusDrift(t *testing.T) {
	c := inSync(t)
	c.diff = []string{"apps.v1.Deployment.ns.redis", "v1.ConfigMap.ns.other"}
	c.secrets[Namespace+"/"+AgentSecret]["token"] = []byte("stale")
	c.secrets[Namespace+"/"+AgentSecret]["task.yaml"] = []byte("kind: Task\n")
	c.workspace = []byte("metadata:\n  name: demo-ws\n  atespace: agents\nspec:\n  files: []\n")
	r, err := Status(context.Background(), plan(t), c)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	got := map[string]Change{}
	for _, ch := range r.Changes {
		got[ch.What] = ch
	}
	if got["apps.v1.Deployment.ns.redis"].Expected != "digest pin" {
		t.Errorf("redis not expected: %+v", got)
	}
	if d := got["secret lookout-ax/lookout-ax-agent"].Detail; !strings.Contains(d, "token is not the attach token") || !strings.Contains(d, "task.yaml differs") {
		t.Errorf("agent secret detail %q", d)
	}
	if _, ok := got["workspace agents/demo-ws"]; !ok {
		t.Errorf("workspace drift not reported: %+v", r.Changes)
	}
	if n := len(r.Drift()); n != 3 {
		t.Errorf("Drift() = %d changes, want 3: %+v", n, r.Drift())
	}
}

func TestStatusAdoptsAndMissing(t *testing.T) {
	c := inSync(t)
	delete(c.secrets, Namespace+"/"+TokensSecret)
	r, err := Status(context.Background(), plan(t), c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.Tokens, "adopted from the live task template") || len(r.Changes) != 0 {
		t.Errorf("adopt: %+v", r)
	}

	c = &fakeCluster{secrets: map[string]map[string][]byte{}}
	r, err = Status(context.Background(), plan(t), c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.Tokens, "none") || len(r.Drift()) != 2 {
		t.Errorf("empty cluster: %+v", r)
	}
}

func TestStatusErrors(t *testing.T) {
	for _, op := range []string{"diff", "secret", "axget"} {
		c := inSync(t)
		c.failOn = op
		if _, err := Status(context.Background(), plan(t), c); err == nil {
			t.Errorf("Status with %s failing succeeded", op)
		}
	}
	c := inSync(t)
	c.secrets[Namespace+"/"+TokensSecret]["inject"] = []byte("bad")
	if _, err := Status(context.Background(), plan(t), c); err == nil || !strings.Contains(err.Error(), TokensSecret) {
		t.Errorf("bad tokens secret: err = %v", err)
	}
	c = inSync(t)
	delete(c.secrets, Namespace+"/"+TokensSecret)
	c.secrets[Namespace+"/"+AgentSecret]["task.yaml"] = []byte("spec:\n  env: []\n")
	if _, err := Status(context.Background(), plan(t), c); err == nil || !strings.Contains(err.Error(), "tokens in secret lookout-ax/lookout-ax-agent") {
		t.Errorf("unadoptable task: err = %v", err)
	}
	c.secrets[Namespace+"/"+AgentSecret]["task.yaml"] = []byte("spec: [\n")
	if _, err := Status(context.Background(), plan(t), c); err == nil {
		t.Error("unparsable task accepted")
	}
	c = inSync(t)
	c.workspace = []byte("metadata: [\n")
	if _, err := Status(context.Background(), plan(t), c); err == nil {
		t.Error("unparsable workspace accepted")
	}
}

func TestUpFresh(t *testing.T) {
	c := &fakeCluster{secrets: map[string]map[string][]byte{}}
	var log []string
	err := Up(context.Background(), plan(t), c, UpOptions{Log: func(f string, a ...any) { log = append(log, f) }})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	// Namespaces first, then the tokens and the task template (so a pod
	// that starts can read them), then the rest, then the Workspace.
	want := []string{"apply Namespace", "secret agent-tokens", "secret lookout-ax-agent", "apply Deployment", "ax apply"}
	if strings.Join(writes(c.ops), ", ") != strings.Join(want, ", ") {
		t.Errorf("writes in order %v, want %v", c.ops, want)
	}
	if len(c.rollouts) != 1 || c.rollouts[0] != "ns/redis" {
		t.Errorf("rollouts %v", c.rollouts)
	}
	if !strings.Contains(strings.Join(log, "\n"), "tokens %s") {
		t.Errorf("log %v", log)
	}
}

// A changed task template restarts lookout, which reads it only at
// startup.
func TestUpRestartsLookoutOnNewTemplate(t *testing.T) {
	p := plan(t)
	objs, err := render.Build("testdata/lookout")
	if err != nil {
		t.Fatal(err)
	}
	p.Objects = objs
	c := inSync(t)
	c.secrets[Namespace+"/"+AgentSecret]["task.yaml"] = []byte("kind: Task\n")
	if err := Up(context.Background(), p, c, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(c.ops, ", "), "secret lookout-ax-agent") || !strings.Contains(strings.Join(c.ops, ", "), "restart lookout-ax/lookout-ax") {
		t.Errorf("ops %v, want the template applied and lookout restarted", c.ops)
	}

	c = inSync(t)
	if err := Up(context.Background(), p, c, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(c.ops, ", "), "restart") || strings.Contains(strings.Join(c.ops, ", "), "secret ") {
		t.Errorf("ops %v: an unchanged template must not be rewritten or restart lookout", c.ops)
	}
}

// When the tokens Secret and the live task template disagree, Up refuses
// rather than rotate either way.
func TestUpRefusesDisagreeingTokens(t *testing.T) {
	c := inSync(t)
	other := agent.Tokens{Inject: strings.Repeat("c", 32), Attach: strings.Repeat("d", 32)}
	task, err := agent.Task(root, testEnv(), other)
	if err != nil {
		t.Fatal(err)
	}
	c.secrets[Namespace+"/"+AgentSecret]["task.yaml"] = task
	err = Up(context.Background(), plan(t), c, UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "disagree") || len(writes(c.ops)) != 0 {
		t.Errorf("err = %v, writes %v; want a refusal before any write", err, c.ops)
	}
	if _, err := Status(context.Background(), plan(t), c); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Errorf("Status err = %v", err)
	}
}

// Up computes everything before writing: a template that won't render
// leaves the cluster untouched.
func TestUpWritesNothingOnRenderFailure(t *testing.T) {
	c := &fakeCluster{secrets: map[string]map[string][]byte{}}
	p := plan(t)
	p.Root = t.TempDir()
	if err := Up(context.Background(), p, c, UpOptions{}); err == nil || len(writes(c.ops)) != 0 {
		t.Errorf("err = %v, writes %v", err, c.ops)
	}
}

func TestStatusStaleLookout(t *testing.T) {
	c := inSync(t)
	c.secretUpdated = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	c.podStarts = []time.Time{time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)}
	r, err := Status(context.Background(), plan(t), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 1 || r.Changes[0].What != "deployment lookout-ax/lookout-ax" {
		t.Errorf("changes %+v, want a stale lookout", r.Changes)
	}
	c.podStarts = []time.Time{time.Date(2026, 10, 20, 13, 0, 0, 0, time.UTC)}
	if r, err = Status(context.Background(), plan(t), c); err != nil || len(r.Changes) != 0 {
		t.Errorf("fresh lookout: %+v, %v", r, err)
	}
	for _, op := range []string{"objectupdated", "podstarts"} {
		c.failOn = op
		if _, err := Status(context.Background(), plan(t), c); err == nil {
			t.Errorf("Status with %s failing succeeded", op)
		}
	}
}

func TestUpInSyncKeepsTokens(t *testing.T) {
	c := inSync(t)
	if err := Up(context.Background(), plan(t), c, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	// Namespaces and the rest only: existing tokens are never rewritten,
	// an unchanged template isn't either, and an unchanged Workspace is
	// not re-applied.
	if strings.Join(writes(c.ops), ", ") != "apply Namespace, apply Deployment" {
		t.Errorf("writes %v", c.ops)
	}
}

func TestUpAdoptsTokens(t *testing.T) {
	c := inSync(t)
	delete(c.secrets, Namespace+"/"+TokensSecret)
	if err := Up(context.Background(), plan(t), c, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(writes(c.ops), ", ") != "apply Namespace, secret agent-tokens, apply Deployment" {
		t.Fatalf("writes %v, want the adopted tokens stored and nothing else rewritten", c.ops)
	}
	stored := c.applied[1]
	if !strings.Contains(stored, "attach: "+base64.StdEncoding.EncodeToString([]byte(tok.Attach))) {
		t.Errorf("stored tokens are not the adopted ones:\n%s", stored)
	}
}

func TestUpDryRun(t *testing.T) {
	c := &fakeCluster{secrets: map[string]map[string][]byte{}}
	if err := Up(context.Background(), plan(t), c, UpOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(c.applied) != 0 || c.dryRuns != 4 || len(c.axApplied) != 0 || len(c.rollouts) != 0 {
		t.Errorf("dry run wrote: applied %d, dry runs %d, ax %d, rollouts %d", len(c.applied), c.dryRuns, len(c.axApplied), len(c.rollouts))
	}
}

func TestUpErrors(t *testing.T) {
	for _, op := range []string{"apply", "applysecret", "secret", "axget", "axapply", "rollout"} {
		c := &fakeCluster{secrets: map[string]map[string][]byte{}, failOn: op}
		if err := Up(context.Background(), plan(t), c, UpOptions{}); err == nil {
			t.Errorf("Up with %s failing succeeded", op)
		}
	}
}

func TestDown(t *testing.T) {
	c := &fakeCluster{}
	if _, err := Down(context.Background(), plan(t), c, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.deleted, "kind: Namespace") || !strings.Contains(c.deleted, "kind: Deployment") {
		t.Errorf("Down deleted:\n%s", c.deleted)
	}
	if _, err := Down(context.Background(), plan(t), c, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.deleted, "kind: Namespace") {
		t.Errorf("Down(all) kept the namespace:\n%s", c.deleted)
	}
	// The tokens' namespace survives even -all.
	p := plan(t)
	objs, err := render.Build("testdata/lookout")
	if err != nil {
		t.Fatal(err)
	}
	p.Objects = objs
	if _, err := Down(context.Background(), p, c, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.deleted, "name: lookout-ax\n---") || strings.Contains(c.deleted, "kind: Namespace") {
		t.Errorf("Down(all) deleted the tokens' namespace:\n%s", c.deleted)
	}
	c.failOn = "delete"
	if _, err := Down(context.Background(), plan(t), c, false); err == nil {
		t.Error("Down with delete failing succeeded")
	}
}

func TestStatusSubstrate(t *testing.T) {
	d := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	p := plan(t)
	p.Env.Substrate = &env.Substrate{
		Images: "r.example.com/substrate", Tag: "v0.5.0",
		Digests:    map[string]string{"atelet": d("3"), "atenet": d("4"), "podcertcontroller": d("5")},
		Namespaces: []string{"ate-system", "podcert"},
		Workloads:  []string{"DaemonSet/ate-system/atelet", "StatefulSet/ate-system/postgres", "Deployment/ate-system/atenet", "Deployment/podcert/ctl"},
	}
	p.SubstrateExtra = []string{"docker.io/library/postgres:18-alpine@" + d("1"), "docker.io/envoyproxy/envoy:v1@" + d("2")}
	good := func() map[string][]cluster.Workload {
		return map[string][]cluster.Workload{
			"ate-system": {
				{ID: "DaemonSet/ate-system/atelet", Images: []string{"r.example.com/substrate/atelet:v0.5.0@" + d("3")}, Current: true},
				{ID: "StatefulSet/ate-system/postgres", Images: []string{"postgres:18-alpine@" + d("1")}, Current: true},
				{ID: "Deployment/ate-system/atenet", Images: []string{"r.example.com/substrate/atenet:v0.5.0@" + d("4"), "index.docker.io/envoyproxy/envoy:v1@" + d("2")}, Current: true},
			},
			"podcert": {{ID: "Deployment/podcert/ctl", Images: []string{"r.example.com/substrate/podcertcontroller:v0.5.0@" + d("5")}, Current: true}},
		}
	}
	whats := func(c *fakeCluster) []string {
		t.Helper()
		r, err := Status(context.Background(), p, c)
		if err != nil {
			t.Fatal(err)
		}
		var w []string
		for _, ch := range r.Changes {
			w = append(w, ch.What+": "+ch.Detail)
		}
		return w
	}

	c := inSync(t)
	c.workloads = good()
	if w := whats(c); len(w) != 0 {
		t.Fatalf("in sync: %v", w)
	}

	for _, tc := range []struct {
		name   string
		mutate func(map[string][]cluster.Workload)
		want   string
	}{
		{"republished tag", func(w map[string][]cluster.Workload) {
			w["ate-system"][0].Images = []string{"r.example.com/substrate/atelet:v0.5.0@" + d("9")}
		}, "substrate DaemonSet/ate-system/atelet: runs"},
		{"no digest", func(w map[string][]cluster.Workload) {
			w["ate-system"][0].Images = []string{"r.example.com/substrate/atelet:v0.5.0"}
		}, "substrate DaemonSet/ate-system/atelet: runs"},
		{"other tag", func(w map[string][]cluster.Workload) {
			w["ate-system"][0].Images = []string{"r.example.com/substrate/atelet:v0.4.0@" + d("3")}
		}, "substrate DaemonSet/ate-system/atelet: runs"},
		{"unpinned name", func(w map[string][]cluster.Workload) {
			w["ate-system"][0].Images = []string{"r.example.com/substrate/old/atelet:v0.5.0@" + d("3")}
		}, "substrate DaemonSet/ate-system/atelet: runs"},
		{"third party moved", func(w map[string][]cluster.Workload) { w["ate-system"][1].Images = []string{"postgres:17"} },
			"substrate StatefulSet/ate-system/postgres: runs postgres:17"},
		{"workload deleted", func(w map[string][]cluster.Workload) { w["ate-system"] = w["ate-system"][:2] },
			"substrate Deployment/ate-system/atenet: missing"},
		{"stray workload", func(w map[string][]cluster.Workload) {
			w["podcert"] = append(w["podcert"], cluster.Workload{ID: "Deployment/podcert/debug", Current: true})
		}, "substrate Deployment/podcert/debug: not part of the Substrate install"},
		{"stuck rollout", func(w map[string][]cluster.Workload) { w["ate-system"][0].Current = false },
			"substrate DaemonSet/ate-system/atelet: rollout not finished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := inSync(t)
			c.workloads = good()
			tc.mutate(c.workloads)
			w := whats(c)
			if len(w) != 1 || !strings.HasPrefix(w[0], tc.want) {
				t.Errorf("changes %v, want one starting %q", w, tc.want)
			}
		})
	}

	c = inSync(t)
	c.failOn = "workloads"
	if _, err := Status(context.Background(), p, c); err == nil {
		t.Error("Status with workloads failing succeeded")
	}
}

// A rendered workload whose rollout is stuck is drift even though its
// template matches (kubectl diff sees only the template).
func TestStatusRollout(t *testing.T) {
	c := inSync(t)
	c.workloads = map[string][]cluster.Workload{"ns": {{ID: "Deployment/ns/redis", Current: false}, {ID: "Deployment/ns/unrelated", Current: false}}}
	r, err := Status(context.Background(), plan(t), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 1 || r.Changes[0].What != "rollout Deployment/ns/redis" {
		t.Errorf("changes %+v, want only the rendered workload's stuck rollout", r.Changes)
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"redis:7":                     "docker.io/library/redis:7",
		"docker.io/redis:7":           "docker.io/library/redis:7",
		"index.docker.io/redis:7":     "docker.io/library/redis:7",
		"index.docker.io/bitnami/x:1": "docker.io/bitnami/x:1",
		"bitnami/redis:7":             "docker.io/bitnami/redis:7",
		"ghcr.io/a/b:1":               "ghcr.io/a/b:1",
		"localhost/a:1":               "localhost/a:1",
		"registry:5000/a:1":           "registry:5000/a:1",
		"docker.io/library/x:1":       "docker.io/library/x:1",
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// Objects in a namespace that doesn't exist yet can't be diffed (the
// server can't dry-run into a missing namespace): status reports the
// namespace as new and diffs the rest.
func TestStatusNewNamespace(t *testing.T) {
	c := inSync(t)
	c.missingNS = []string{"ns"}
	r, err := Status(context.Background(), plan(t), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 1 || r.Changes[0].What != "namespace ns" || !strings.Contains(r.Changes[0].Detail, "the 1 objects in it") {
		t.Errorf("changes %+v", r.Changes)
	}
	if strings.Contains(c.diffed, "name: ns") || strings.Contains(c.diffed, "name: redis") {
		t.Errorf("diffed objects in a missing namespace:\n%s", c.diffed)
	}
	c.failOn = "namespaces"
	if _, err := Status(context.Background(), plan(t), c); err == nil {
		t.Error("Status with namespaces failing succeeded")
	}
}

// A dry run skips (and says so) what it can't validate; a real up
// applies everything, namespaces first.
func TestUpNewNamespace(t *testing.T) {
	c := inSync(t)
	c.missingNS = []string{"ns"}
	var log []string
	if err := Up(context.Background(), plan(t), c, UpOptions{DryRun: true, Log: func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) }}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(log, "\n"), "namespace ns and the 1 objects in it would be created") {
		t.Errorf("dry-run log %v", log)
	}
	c = inSync(t)
	c.missingNS = []string{"ns"}
	if err := Up(context.Background(), plan(t), c, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(writes(c.ops), ", ") != "apply Namespace, apply Deployment" {
		t.Errorf("real up writes %v", c.ops)
	}
	c = inSync(t)
	c.failOn = "namespaces"
	if err := Up(context.Background(), plan(t), c, UpOptions{DryRun: true}); err == nil {
		t.Error("dry run with namespaces failing succeeded")
	}
}

// An object in a missing namespace that the render doesn't create isn't
// set aside: kubectl diffs it and fails, as a real apply would.
func TestStatusMissingNamespaceNotCreated(t *testing.T) {
	p := plan(t)
	var noNS render.Objects
	for _, o := range p.Objects {
		if o.Kind() != "Namespace" {
			noNS = append(noNS, o)
		}
	}
	p.Objects = noNS
	c := inSync(t)
	c.missingNS = []string{"ns"}
	if _, err := Status(context.Background(), p, c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.diffed, "name: redis") {
		t.Errorf("the object in a namespace nothing creates was set aside instead of diffed:\n%s", c.diffed)
	}
}

// writes filters ops down to the writes, in order.
func writes(ops []string) []string {
	var w []string
	for _, op := range ops {
		if op != "ax get" && !strings.HasPrefix(op, "rollout ") {
			w = append(w, op)
		}
	}
	return w
}

func freshPlan(t *testing.T) *Plan {
	t.Helper()
	p := plan(t)
	objs, err := render.Build("testdata/fresh")
	if err != nil {
		t.Fatal(err)
	}
	p.Objects = objs
	return p
}

// On a fresh cluster ax isn't deployed: status doesn't ask it, and up
// reconciles the Workspace only once ax-server has rolled out.
func TestFreshCluster(t *testing.T) {
	c := &fakeCluster{secrets: map[string]map[string][]byte{}, missingNS: []string{"ax-system"}}
	r, err := Status(context.Background(), freshPlan(t), c)
	if err != nil {
		t.Fatal(err)
	}
	if contains(c.ops, "ax get") {
		t.Error("status asked ax on a cluster without ax")
	}
	var ws Change
	for _, ch := range r.Changes {
		if strings.HasPrefix(ch.What, "workspace ") {
			ws = ch
		}
	}
	if !strings.Contains(ws.Detail, "ax is not deployed yet") {
		t.Errorf("workspace change %+v", ws)
	}

	c = &fakeCluster{secrets: map[string]map[string][]byte{}, missingNS: []string{"ax-system"}}
	var log []string
	if err := Up(context.Background(), freshPlan(t), c, UpOptions{DryRun: true, Log: func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) }}); err != nil {
		t.Fatal(err)
	}
	if contains(c.ops, "ax get") || !strings.Contains(strings.Join(log, "\n"), "would be applied once ax is up") {
		t.Errorf("fresh dry run: ops %v, log %v", c.ops, log)
	}

	c = &fakeCluster{secrets: map[string]map[string][]byte{}, missingNS: []string{"ax-system"}}
	if err := Up(context.Background(), freshPlan(t), c, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	ri, ai := indexOf(c.ops, "rollout ax-system/ax-server"), indexOf(c.ops, "ax get")
	if ri < 0 || ai < 0 || ai < ri || !contains(c.ops, "ax apply") {
		t.Errorf("fresh up: the Workspace must follow ax-server's rollout: %v", c.ops)
	}
}

func indexOf(xs []string, x string) int {
	for i, y := range xs {
		if y == x {
			return i
		}
	}
	return -1
}

// ax's namespace can exist with ax-server missing (a first up that
// stopped early) or still rolling out: status and dry runs don't ask it.
func TestAxNotUpYet(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workloads []cluster.Workload
		want      string
	}{
		{"absent", nil, "missing (ax is not deployed yet)"},
		{"not ready", []cluster.Workload{{ID: "Deployment/ax-system/ax-server", Current: false}}, "unknown (ax-server's rollout hasn't finished)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeCluster{secrets: map[string]map[string][]byte{}, workloads: map[string][]cluster.Workload{"ax-system": tc.workloads}}
			r, err := Status(context.Background(), freshPlan(t), c)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, ch := range r.Changes {
				found = found || (strings.HasPrefix(ch.What, "workspace ") && ch.Detail == tc.want)
			}
			if !found || contains(c.ops, "ax get") {
				t.Errorf("changes %+v, ops %v", r.Changes, c.ops)
			}
			c = &fakeCluster{secrets: map[string]map[string][]byte{}, workloads: map[string][]cluster.Workload{"ax-system": tc.workloads}}
			if err := Up(context.Background(), freshPlan(t), c, UpOptions{DryRun: true}); err != nil || contains(c.ops, "ax get") {
				t.Errorf("dry run: %v, ops %v", err, c.ops)
			}
		})
	}
}

// With ax up, a broken ax fails up before anything is written; a
// Workspace failure after the writes says where things stand.
func TestUpAxFailures(t *testing.T) {
	ready := map[string][]cluster.Workload{"ax-system": {{ID: "Deployment/ax-system/ax-server", Current: true}}}
	c := &fakeCluster{secrets: map[string]map[string][]byte{}, workloads: ready, failOn: "axget"}
	err := Up(context.Background(), freshPlan(t), c, UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "nothing written yet") || len(writes(c.ops)) != 0 {
		t.Errorf("early ax failure: %v, writes %v", err, writes(c.ops))
	}
	c = &fakeCluster{secrets: map[string]map[string][]byte{}, workloads: ready, failOn: "axapply"}
	err = Up(context.Background(), freshPlan(t), c, UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "re-run up to retry") {
		t.Errorf("late workspace failure: %v", err)
	}
	c = &fakeCluster{secrets: map[string]map[string][]byte{}, workloads: ready, failOn: "workloads"}
	if err := Up(context.Background(), freshPlan(t), c, UpOptions{}); err == nil {
		t.Error("up with workloads failing succeeded")
	}
	if _, err := Status(context.Background(), freshPlan(t), c); err == nil {
		t.Error("status with workloads failing succeeded")
	}
}

// A dry run on a cluster without the agent's namespace can't validate
// the agent's Secrets either; it says so rather than failing.
func TestDryRunNewSecretNamespace(t *testing.T) {
	c := &fakeCluster{secrets: map[string]map[string][]byte{}, missingNS: []string{"lookout-ax"}}
	p := plan(t)
	objs, err := render.Build("testdata/lookout")
	if err != nil {
		t.Fatal(err)
	}
	p.Objects = objs
	var log []string
	if err := Up(context.Background(), p, c, UpOptions{DryRun: true, Log: func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) }}); err != nil {
		t.Fatal(err)
	}
	if c.dryRuns != 0 || !strings.Contains(strings.Join(log, "\n"), "which the run creates") {
		t.Errorf("dry runs %d, log %v", c.dryRuns, log)
	}
}

// The credential provider reads its namespace policy only at startup: up
// restarts it when the policy changes, and status flags a provider
// running on an older policy.
func TestCredentialProviderPolicy(t *testing.T) {
	key := "configmap/ate-system/k8s-credential-provider-namespace-policy"
	t0 := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)

	c := inSync(t)
	c.objectTimes = map[string]time.Time{key: t0}
	c.podStarts = []time.Time{t0.Add(-time.Minute)}
	r, err := Status(context.Background(), plan(t), c)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ch := range r.Changes {
		found = found || (ch.What == "deployment ate-system/k8s-credential-provider" && strings.Contains(ch.Detail, "its namespace policy"))
	}
	if !found {
		t.Errorf("stale provider not reported: %+v", r.Changes)
	}

	c = inSync(t)
	c.objectTimes = map[string]time.Time{key: t0}
	c.podStarts = []time.Time{t0.Add(time.Minute)}
	c.policyChanges = true
	if err := Up(context.Background(), plan(t), c, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if !contains(c.ops, "restart ate-system/k8s-credential-provider") {
		t.Errorf("provider not restarted after a policy change: %v", c.ops)
	}

	c = inSync(t)
	c.objectTimes = map[string]time.Time{key: t0}
	c.podStarts = []time.Time{t0.Add(time.Minute)}
	if err := Up(context.Background(), plan(t), c, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if contains(c.ops, "restart ate-system/k8s-credential-provider") {
		t.Errorf("provider restarted without a policy change: %v", c.ops)
	}

	// An earlier up wrote the policy and stopped before restarting: this
	// one restarts the provider though it changes nothing itself.
	c = inSync(t)
	c.objectTimes = map[string]time.Time{key: t0}
	c.podStarts = []time.Time{t0.Add(-time.Minute)}
	if err := Up(context.Background(), plan(t), c, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if !contains(c.ops, "restart ate-system/k8s-credential-provider") {
		t.Errorf("a provider left stale by an interrupted up was not restarted: %v", c.ops)
	}
}
