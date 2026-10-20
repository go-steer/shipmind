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

// Package deploy compares an environment with its cluster (Status) and
// brings the cluster to it (Up, Down).
//
// An environment is more than its kustomize render. The agent's task
// template carries the agent's tokens, so it lives in a Secret rendered
// at deploy time from the tokens already in the cluster; and the agent's
// Workspace is an ax object, applied with the ax CLI. Status covers all
// three, so "no drift" means the whole environment matches.
package deploy

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/go-steer/shipmind/internal/agent"
	"github.com/go-steer/shipmind/internal/cluster"
	"github.com/go-steer/shipmind/internal/env"
	"github.com/go-steer/shipmind/internal/render"
)

// Where the agent's Secrets live. The task template Secret's name and
// keys are what the lookout Deployment in the substrate profile mounts.
const (
	Namespace    = "lookout-ax"
	TokensSecret = "agent-tokens"     // keys: inject, attach
	AgentSecret  = "lookout-ax-agent" // keys: token (attach), task.yaml
	// Lookout reads the task template once, at startup, so a changed
	// template needs a restart of this Deployment.
	Lookout = "lookout-ax"
)

// Plan is everything Status and Up need, rendered once.
type Plan struct {
	Env *env.Env
	// Root is the shipmind checkout the templates come from.
	Root    string
	Objects render.Objects
	Ref     string
	// SubstrateExtra are the third-party images versions.yaml pins for
	// Substrate (its database, its proxy), which run beside its own.
	SubstrateExtra []string
}

// Change is one difference between the environment and the cluster.
type Change struct {
	// What names the object ("apps.v1.Deployment.ax-system.ax-redis",
	// "secret lookout-ax/lookout-ax-agent", "workspace cred-test/gke-demo").
	What string
	// Detail says what differs.
	Detail string
	// Expected is the environment's reason for expecting the change, or
	// "" when the change is drift.
	Expected string
}

// Report is Status's result.
type Report struct {
	Rendered int
	Changes  []Change
	// Tokens says where the agent's tokens come from.
	Tokens string
}

// Drift returns the changes the environment does not expect.
func (r *Report) Drift() []Change {
	var d []Change
	for _, c := range r.Changes {
		if c.Expected == "" {
			d = append(d, c)
		}
	}
	return d
}

func (p *Plan) manifests() ([]byte, error) {
	y, err := p.Objects.YAML()
	return []byte(y), err
}

// tokens finds the agent's tokens: the tokens Secret, else (adopting a
// deployment made before shipmind) the ones in the live task template.
// source says which; found is false when the cluster has none yet. When
// both exist and disagree, it fails: picking one would cut lookout off
// from the tasks that carry the other.
func tokens(ctx context.Context, c cluster.Cluster) (t agent.Tokens, source string, found bool, err error) {
	stored, inStore, err := c.Secret(ctx, Namespace, TokensSecret)
	if err != nil {
		return t, "", false, err
	}
	live, inTemplate, err := templateTokens(ctx, c)
	if err != nil && !inStore {
		return t, "", false, err
	}
	if inStore {
		t = agent.Tokens{Inject: string(stored["inject"]), Attach: string(stored["attach"])}
		if err := t.Check(); err != nil {
			return t, "", false, fmt.Errorf("secret %s/%s: %w", Namespace, TokensSecret, err)
		}
		if inTemplate && live != t {
			return t, "", false, fmt.Errorf("the tokens in secret %s/%s and in the live task template (secret %s/%s) disagree; existing tasks carry one pair, so resolve it by hand before deploying",
				Namespace, TokensSecret, Namespace, AgentSecret)
		}
		return t, "secret " + Namespace + "/" + TokensSecret, true, nil
	}
	if !inTemplate {
		return t, "", false, nil
	}
	return live, "adopted from the live task template (secret " + Namespace + "/" + AgentSecret + ")", true, nil
}

// templateTokens reads the tokens out of the live task template, if
// there is one.
func templateTokens(ctx context.Context, c cluster.Cluster) (t agent.Tokens, found bool, err error) {
	data, ok, err := c.Secret(ctx, Namespace, AgentSecret)
	if err != nil || !ok {
		return t, false, err
	}
	var task struct {
		Spec struct {
			Env []struct{ Name, Value string } `yaml:"env"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data["task.yaml"], &task); err != nil {
		return t, false, fmt.Errorf("secret %s/%s task.yaml: %w", Namespace, AgentSecret, err)
	}
	for _, e := range task.Spec.Env {
		switch e.Name {
		case "MAST_INJECT_TOKEN":
			t.Inject = e.Value
		case "MAST_ATTACH_TOKEN":
			t.Attach = e.Value
		}
	}
	if err := t.Check(); err != nil {
		return t, false, fmt.Errorf("tokens in secret %s/%s: %w", Namespace, AgentSecret, err)
	}
	return t, true, nil
}

// Status compares the environment with the cluster, read only.
func Status(ctx context.Context, p *Plan, c cluster.Cluster) (*Report, error) {
	r := &Report{Rendered: len(p.Objects)}
	add := func(what, detail string) {
		reason, _ := p.Env.Expected(what)
		r.Changes = append(r.Changes, Change{What: what, Detail: detail, Expected: reason})
	}

	m, err := p.manifests()
	if err != nil {
		return nil, err
	}
	ids, err := c.Diff(ctx, m)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		add(id, "differs from the render (kubectl diff)")
	}

	t, source, found, err := tokens(ctx, c)
	if err != nil {
		return nil, err
	}
	agentWhat := "secret " + Namespace + "/" + AgentSecret
	if !found {
		r.Tokens = "none in the cluster yet (up generates them)"
		add(agentWhat, "missing")
	} else {
		r.Tokens = source
		want, err := agent.Task(p.Root, p.Env, t)
		if err != nil {
			return nil, err
		}
		live, _, err := c.Secret(ctx, Namespace, AgentSecret)
		if err != nil {
			return nil, err
		}
		if d := compareAgentSecret(live, want, t); d != "" {
			add(agentWhat, d)
		}
		stale, err := lookoutStale(ctx, c)
		if err != nil {
			return nil, err
		}
		if stale {
			add("deployment "+Namespace+"/"+Lookout, "running since before its task template last changed (lookout reads it only at startup)")
		}
	}

	if err := substrateStatus(ctx, p, c, add); err != nil {
		return nil, err
	}

	wsWhat := fmt.Sprintf("workspace %s/%s", p.Env.Agent.Atespace, p.Env.Agent.Workspace)
	want, err := agent.Workspace(p.Root, p.Env)
	if err != nil {
		return nil, err
	}
	live, ok, err := c.AxWorkspace(ctx, p.Env.Agent.Atespace, p.Env.Agent.Workspace)
	if err != nil {
		return nil, err
	}
	switch {
	case !ok:
		add(wsWhat, "missing")
	default:
		same, err := sameWorkspace(live, want)
		if err != nil {
			return nil, err
		}
		if !same {
			add(wsWhat, "files differ from the render")
		}
	}
	return r, nil
}

// substrateStatus checks Substrate's workloads: every image is either
// Substrate's own, from the environment's repository at its tag, or a
// third-party image versions.yaml pins for Substrate.
func substrateStatus(ctx context.Context, p *Plan, c cluster.Cluster, add func(what, detail string)) error {
	sub := p.Env.Substrate
	if sub == nil {
		return nil
	}
	prefix := strings.TrimSuffix(sub.Images, "/") + "/"
	for _, ns := range sub.Namespaces {
		images, err := c.WorkloadImages(ctx, ns)
		if err != nil {
			return err
		}
		if len(images) == 0 {
			add("substrate "+ns, "no workloads (is Substrate installed?)")
			continue
		}
		ids := make([]string, 0, len(images))
		for id := range images {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			for _, img := range images[id] {
				if ownImage(img, prefix, sub.Tag) || slices.ContainsFunc(p.SubstrateExtra, func(x string) bool { return normalize(x) == normalize(img) }) {
					continue
				}
				add("substrate "+id, fmt.Sprintf("runs %s, not %s*:%s or an image versions.yaml pins for Substrate", img, prefix, sub.Tag))
			}
		}
	}
	return nil
}

// normalize spells a Docker Hub reference in full, as the registry
// resolves it: redis:7 is docker.io/library/redis:7, and bitnami/redis:7
// is docker.io/bitnami/redis:7.
func normalize(ref string) string {
	first, _, hasSlash := strings.Cut(ref, "/")
	switch {
	case !hasSlash:
		return "docker.io/library/" + ref
	case !strings.ContainsAny(first, ".:") && first != "localhost":
		return "docker.io/" + ref
	}
	return ref
}

// ownImage reports whether img is <prefix><name>:<tag>, optionally with
// a digest.
func ownImage(img, prefix, tag string) bool {
	rest, ok := strings.CutPrefix(img, prefix)
	if !ok {
		return false
	}
	rest, _, _ = strings.Cut(rest, "@")
	name, t, ok := strings.Cut(rest, ":")
	return ok && name != "" && !strings.Contains(name, ":") && t == tag
}

// lookoutStale reports whether a running lookout pod started before the
// task template Secret was last written.
func lookoutStale(ctx context.Context, c cluster.Cluster) (bool, error) {
	updated, ok, err := c.SecretUpdated(ctx, Namespace, AgentSecret)
	if err != nil || !ok {
		return false, err
	}
	starts, err := c.PodStarts(ctx, Namespace, Lookout)
	if err != nil {
		return false, err
	}
	for _, s := range starts {
		if s.Before(updated) {
			return true, nil
		}
	}
	return false, nil
}

// compareAgentSecret describes how the live agent Secret differs from
// the rendered task template and tokens, or returns "".
func compareAgentSecret(live map[string][]byte, wantTask []byte, t agent.Tokens) string {
	if live == nil {
		return "missing"
	}
	var diffs []string
	if string(live["token"]) != t.Attach {
		diffs = append(diffs, "token is not the attach token")
	}
	same, err := sameYAML(live["task.yaml"], wantTask)
	switch {
	case err != nil:
		diffs = append(diffs, "task.yaml: "+err.Error())
	case !same:
		diffs = append(diffs, "task.yaml differs from the render")
	}
	return strings.Join(diffs, "; ")
}

// sameYAML compares two YAML documents by value, ignoring comments and
// formatting.
func sameYAML(a, b []byte) (bool, error) {
	var x, y any
	if err := yaml.Unmarshal(a, &x); err != nil {
		return false, err
	}
	if err := yaml.Unmarshal(b, &y); err != nil {
		return false, err
	}
	return reflect.DeepEqual(x, y), nil
}

// sameWorkspace compares a live Workspace (as `ax get` prints it, with
// server-set metadata) with a rendered one: name, atespace and spec.
func sameWorkspace(live, want []byte) (bool, error) {
	type ws struct {
		Metadata struct {
			Name     string `yaml:"name"`
			Atespace string `yaml:"atespace"`
		} `yaml:"metadata"`
		Spec any `yaml:"spec"`
	}
	var l, w ws
	if err := yaml.Unmarshal(live, &l); err != nil {
		return false, fmt.Errorf("live workspace: %w", err)
	}
	if err := yaml.Unmarshal(want, &w); err != nil {
		return false, fmt.Errorf("rendered workspace: %w", err)
	}
	return reflect.DeepEqual(l, w), nil
}

// UpOptions control Up.
type UpOptions struct {
	// DryRun validates every Kubernetes write server-side and writes
	// nothing, including the tokens and the Workspace.
	DryRun bool
	// Log receives one line per step.
	Log func(format string, args ...any)
}

// Up brings the cluster to the environment. It computes everything
// before it writes anything; then applies the Namespaces, the agent's
// tokens (adopted or generated once, never rotated here) and task
// template Secret, then every other object, then the Workspace;
// restarts lookout if its task template changed (lookout reads it only
// at startup); and waits for every rendered Deployment to roll out.
func Up(ctx context.Context, p *Plan, c cluster.Cluster, opts UpOptions) error {
	log := opts.Log
	if log == nil {
		log = func(string, ...any) {}
	}

	// Compute.
	var namespaces, rest render.Objects
	for _, o := range p.Objects {
		if o.Kind() == "Namespace" {
			namespaces = append(namespaces, o)
		} else {
			rest = append(rest, o)
		}
	}
	nsYAML, err := namespaces.YAML()
	if err != nil {
		return err
	}
	restYAML, err := rest.YAML()
	if err != nil {
		return err
	}
	t, source, found, err := tokens(ctx, c)
	if err != nil {
		return err
	}
	if !found {
		if t, err = agent.NewTokens(); err != nil {
			return err
		}
		source = "generated"
	}
	task, err := agent.Task(p.Root, p.Env, t)
	if err != nil {
		return err
	}
	ws, err := agent.Workspace(p.Root, p.Env)
	if err != nil {
		return err
	}
	liveAgent, _, err := c.Secret(ctx, Namespace, AgentSecret)
	if err != nil {
		return err
	}
	templateChanged := compareAgentSecret(liveAgent, task, t) != ""
	liveWS, wsFound, err := c.AxWorkspace(ctx, p.Env.Agent.Atespace, p.Env.Agent.Workspace)
	if err != nil {
		return err
	}
	wsSame := false
	if wsFound {
		if wsSame, err = sameWorkspace(liveWS, ws); err != nil {
			return err
		}
	}

	// Write.
	if len(namespaces) > 0 {
		if _, err := c.Apply(ctx, []byte(nsYAML), opts.DryRun); err != nil {
			return err
		}
		log("applied %d namespaces%s", len(namespaces), dry(opts.DryRun))
	}
	if !strings.HasPrefix(source, "secret ") {
		secret := cluster.SecretManifest(Namespace, TokensSecret, map[string][]byte{
			"inject": []byte(t.Inject), "attach": []byte(t.Attach),
		})
		if err := c.ApplySecret(ctx, secret, opts.DryRun); err != nil {
			return fmt.Errorf("secret %s/%s: %w", Namespace, TokensSecret, err)
		}
		log("tokens %s, stored in secret %s/%s%s", source, Namespace, TokensSecret, dry(opts.DryRun))
	}
	if templateChanged {
		secret := cluster.SecretManifest(Namespace, AgentSecret, map[string][]byte{
			"token": []byte(t.Attach), "task.yaml": task,
		})
		if err := c.ApplySecret(ctx, secret, opts.DryRun); err != nil {
			return fmt.Errorf("secret %s/%s: %w", Namespace, AgentSecret, err)
		}
		log("task template applied to secret %s/%s%s", Namespace, AgentSecret, dry(opts.DryRun))
	} else {
		log("task template in secret %s/%s unchanged", Namespace, AgentSecret)
	}
	out, err := c.Apply(ctx, []byte(restYAML), opts.DryRun)
	if err != nil {
		return err
	}
	log("applied %d objects%s\n%s", len(rest), dry(opts.DryRun), strings.TrimRight(out, "\n"))
	switch {
	case wsSame:
		log("workspace %s/%s unchanged", p.Env.Agent.Atespace, p.Env.Agent.Workspace)
	case opts.DryRun:
		log("workspace %s/%s would be applied (dry run)", p.Env.Agent.Atespace, p.Env.Agent.Workspace)
	default:
		if err := c.AxApply(ctx, p.Env.Agent.Atespace, ws); err != nil {
			return err
		}
		log("workspace %s/%s applied", p.Env.Agent.Atespace, p.Env.Agent.Workspace)
	}

	if opts.DryRun {
		if templateChanged {
			log("deployment %s/%s would be restarted (dry run)", Namespace, Lookout)
		}
		return nil
	}
	if templateChanged {
		if _, ok := p.Objects.Find("Deployment", Namespace, Lookout); ok {
			if err := c.RolloutRestart(ctx, Namespace, Lookout); err != nil {
				return err
			}
			log("deployment %s/%s restarted to read the new task template", Namespace, Lookout)
		}
	}
	for _, o := range p.Objects {
		if o.Kind() == "Deployment" {
			if err := c.RolloutStatus(ctx, o.Namespace(), o.Name()); err != nil {
				return err
			}
			log("deployment %s/%s rolled out", o.Namespace(), o.Name())
		}
	}
	return nil
}

func dry(d bool) string {
	if d {
		return " (dry run)"
	}
	return ""
}

// kept are the kinds Down leaves alone unless asked: they hold data or
// other workloads.
var kept = []string{"Namespace", "PersistentVolumeClaim"}

// Down deletes the rendered objects, except Namespaces and
// PersistentVolumeClaims unless all is set. It never deletes the
// namespace holding the agent's tokens (deleting it would delete them,
// and every existing task carries them), and does not touch the agent's
// Secrets, the ax Workspace or tasks, or Substrate.
func Down(ctx context.Context, p *Plan, c cluster.Cluster, all bool) (string, error) {
	var doomed render.Objects
	for _, o := range p.Objects {
		if o.Kind() == "Namespace" && o.Name() == Namespace {
			continue
		}
		if all || !slices.Contains(kept, o.Kind()) {
			doomed = append(doomed, o)
		}
	}
	y, err := doomed.YAML()
	if err != nil {
		return "", err
	}
	return c.Delete(ctx, []byte(y))
}
