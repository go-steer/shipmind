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
	"strings"
	"testing"
	"time"

	"github.com/go-steer/shipmind/internal/agent"
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
	ops           []string // every write, in order
	secretUpdated time.Time
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

func (f *fakeCluster) Diff(context.Context, []byte) ([]string, error) { return f.diff, f.fail("diff") }

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

func (f *fakeCluster) SecretUpdated(context.Context, string, string) (time.Time, bool, error) {
	return f.secretUpdated, !f.secretUpdated.IsZero(), f.fail("secretupdated")
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
	return f.fail("rollout")
}

func (f *fakeCluster) AxWorkspace(context.Context, string, string) ([]byte, bool, error) {
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
	if strings.Join(c.ops, ", ") != strings.Join(want, ", ") {
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
	if err == nil || !strings.Contains(err.Error(), "disagree") || len(c.ops) != 0 {
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
	if err := Up(context.Background(), p, c, UpOptions{}); err == nil || len(c.ops) != 0 {
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
	for _, op := range []string{"secretupdated", "podstarts"} {
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
	if strings.Join(c.ops, ", ") != "apply Namespace, apply Deployment" {
		t.Errorf("writes %v", c.ops)
	}
}

func TestUpAdoptsTokens(t *testing.T) {
	c := inSync(t)
	delete(c.secrets, Namespace+"/"+TokensSecret)
	if err := Up(context.Background(), plan(t), c, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.ops, ", ") != "apply Namespace, secret agent-tokens, apply Deployment" {
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
