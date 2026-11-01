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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/go-steer/shipmind/internal/agent"
	"github.com/go-steer/shipmind/internal/cluster"
	"github.com/go-steer/shipmind/internal/env"
	"github.com/go-steer/shipmind/internal/render"
	"github.com/go-steer/shipmind/internal/secrets"
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
	// Secrets reads the values of shipmind.yaml's `secrets` from Secret
	// Manager; needed only when the environment lists any.
	Secrets secrets.Source
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

	existing, pending, err := splitByNamespace(ctx, p.Objects, c)
	if err != nil {
		return nil, err
	}
	ax, err := axState(ctx, p.Objects, pending, c)
	if err != nil {
		return nil, err
	}
	m, err := existing.YAML()
	if err != nil {
		return nil, err
	}
	ids, err := c.Diff(ctx, []byte(m))
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		add(id, "differs from the render (kubectl diff)")
	}
	for _, ns := range sortedKeys(pending) {
		add("namespace "+ns, fmt.Sprintf("new: the namespace and the %d objects in it don't exist yet", len(pending[ns])-1))
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
	}
	for _, g := range generatedSecrets {
		if !g.wanted(p.Objects) {
			continue
		}
		ok, err := g.present(ctx, c)
		if err != nil {
			return nil, err
		}
		if !ok {
			add(g.what(), "missing (up generates "+g.Why+")")
		}
	}
	managed, err := managedSecrets(ctx, p, c)
	if err != nil {
		return nil, err
	}
	for _, w := range managed {
		add(w.what(), w.Why)
	}
	if found {
		sw, err := switchboardSecret(ctx, p, c, t)
		if err != nil {
			return nil, err
		}
		if sw != nil {
			add(sw.what(), sw.Why)
		}
	}
	for _, sr := range readers(p) {
		stale, err := sr.stale(ctx, c)
		if err != nil {
			return nil, err
		}
		if stale {
			add("deployment "+sr.Namespace+"/"+sr.Deployment, fmt.Sprintf("running since before %s last changed (it reads it only at startup)", sr.What))
		}
	}

	if err := rolloutStatus(ctx, p, c, add); err != nil {
		return nil, err
	}
	if err := substrateStatus(ctx, p, c, add); err != nil {
		return nil, err
	}
	if err := substratePatchStatus(ctx, p, c, add); err != nil {
		return nil, err
	}

	wsWhat := fmt.Sprintf("workspace %s/%s", p.Env.Agent.Atespace, p.Env.Agent.Workspace)
	want, err := agent.Workspace(p.Root, p.Env)
	if err != nil {
		return nil, err
	}
	switch ax {
	case axAbsent:
		add(wsWhat, "missing (ax is not deployed yet)")
		return r, nil
	case axNotReady:
		add(wsWhat, "unknown (ax-server isn't serving: its rollout hasn't finished, or no pod is available)")
		return r, nil
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

// substrateStatus checks Substrate's workloads against the environment:
// every listed workload exists and no other does; every image is
// exactly one of Substrate's own (<images>/<name>:<tag>@<digest pinned
// for name>) or a third-party image versions.yaml pins for Substrate;
// and every rollout has finished.
func substrateStatus(ctx context.Context, p *Plan, c cluster.Cluster, add func(what, detail string)) error {
	sub := p.Env.Substrate
	if sub == nil {
		return nil
	}
	present := map[string]bool{}
	for _, ns := range sub.Namespaces {
		ws, err := c.Workloads(ctx, ns)
		if err != nil {
			return err
		}
		for _, w := range ws {
			present[w.ID] = true
			what := "substrate " + w.ID
			if !slices.Contains(sub.Workloads, w.ID) {
				add(what, "not part of the Substrate install (not in substrate.workloads)")
				continue
			}
			for _, img := range w.Images {
				if !substrateImage(img, sub) && !slices.ContainsFunc(p.SubstrateExtra, func(x string) bool { return normalize(x) == normalize(img) }) {
					add(what, fmt.Sprintf("runs %s, which is neither %s/<name>:%s at its pinned digest nor an image versions.yaml pins for Substrate", img, strings.TrimSuffix(sub.Images, "/"), sub.Tag))
				}
			}
			if !w.Current {
				add(what, "rollout not finished: pods from an older template may still run")
			}
		}
	}
	for _, id := range sub.Workloads {
		if !present[id] {
			add("substrate "+id, "missing")
		}
	}
	return nil
}

// substrateImage reports whether img is exactly one of Substrate's own
// images: <images>/<name>:<tag>@<the digest pinned for name>.
func substrateImage(img string, sub *env.Substrate) bool {
	rest, ok := strings.CutPrefix(img, strings.TrimSuffix(sub.Images, "/")+"/")
	if !ok {
		return false
	}
	nameTag, digest, ok := strings.Cut(rest, "@")
	if !ok {
		return false
	}
	name, tag, ok := strings.Cut(nameTag, ":")
	return ok && tag == sub.Tag && sub.Digests[name] != "" && sub.Digests[name] == digest
}

// normalize spells a Docker Hub reference in full, as the registry
// resolves it: redis:7, docker.io/redis:7 and index.docker.io/redis:7
// are all docker.io/library/redis:7, and bitnami/redis:7 is
// docker.io/bitnami/redis:7.
func normalize(ref string) string {
	if r, ok := strings.CutPrefix(ref, "index.docker.io/"); ok {
		ref = "docker.io/" + r
	}
	first, rest, hasSlash := strings.Cut(ref, "/")
	switch {
	case !hasSlash:
		return "docker.io/library/" + ref
	case first == "docker.io" && !strings.Contains(rest, "/"):
		return "docker.io/library/" + rest
	case !strings.ContainsAny(first, ".:") && first != "localhost":
		return "docker.io/" + ref
	}
	return ref
}

// rolloutStatus reports rendered workloads whose rollout hasn't
// finished. kubectl diff compares templates, and a stuck rollout keeps
// the new template while the old pods run.
func rolloutStatus(ctx context.Context, p *Plan, c cluster.Cluster, add func(what, detail string)) error {
	want := map[string]bool{}
	var namespaces []string
	for _, o := range p.Objects {
		switch o.Kind() {
		case "Deployment", "DaemonSet", "StatefulSet":
			want[o.Kind()+"/"+o.Namespace()+"/"+o.Name()] = true
			if !slices.Contains(namespaces, o.Namespace()) {
				namespaces = append(namespaces, o.Namespace())
			}
		}
	}
	slices.Sort(namespaces)
	for _, ns := range namespaces {
		ws, err := c.Workloads(ctx, ns)
		if err != nil {
			return err
		}
		for _, w := range ws {
			if want[w.ID] && !w.Current {
				add("rollout "+w.ID, "not finished: pods from an older template may still run")
			}
		}
	}
	return nil
}

// splitByNamespace separates the objects kubectl can diff or validate
// from those in namespaces the render creates but the cluster doesn't
// have yet: the server can't dry-run an object into a missing namespace,
// and a dry-run never creates one. A new namespace's own Namespace object
// goes with its contents. An object in a missing namespace the render
// doesn't create stays with the rest, so kubectl fails on it as it
// should: a real apply would too.
func splitByNamespace(ctx context.Context, objs render.Objects, c cluster.Cluster) (existing render.Objects, pending map[string]render.Objects, err error) {
	names, err := c.Namespaces(ctx)
	if err != nil {
		return nil, nil, err
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	created := map[string]bool{}
	for _, o := range objs {
		if o.Kind() == "Namespace" {
			created[o.Name()] = true
		}
	}
	pending = map[string]render.Objects{}
	for _, o := range objs {
		ns := o.Namespace()
		if o.Kind() == "Namespace" {
			ns = o.Name()
		}
		if ns != "" && !have[ns] && created[ns] {
			pending[ns] = append(pending[ns], o)
			continue
		}
		existing = append(existing, o)
	}
	return existing, pending, nil
}

// AxServer is the profile's ax-server Deployment, through which ax
// answers.
const AxServer = "ax-server"

// ax's states as up and status see them.
const (
	axReady    = "ready"
	axAbsent   = "absent"    // not deployed yet: a fresh cluster, or a first up that stopped early
	axNotReady = "not ready" // deployed, but rolling out or with no available pod
)

// axState says whether ax can be asked anything: ax-server exists and
// has rolled out. A render without ax-server reports ready, leaving any
// failure to the ax call itself.
func axState(ctx context.Context, objs render.Objects, pending map[string]render.Objects, c cluster.Cluster) (string, error) {
	for _, o := range objs {
		if o.Kind() != "Deployment" || o.Name() != AxServer {
			continue
		}
		ns := o.Namespace()
		if _, ok := pending[ns]; ok {
			return axAbsent, nil
		}
		ws, err := c.Workloads(ctx, ns)
		if err != nil {
			return "", err
		}
		for _, w := range ws {
			if w.ID == "Deployment/"+ns+"/"+AxServer {
				// Scaled to zero (as a cutover leaves it), it is current
				// with no pod to answer.
				if !w.Current || w.Available == 0 {
					return axNotReady, nil
				}
				return axReady, nil
			}
		}
		return axAbsent, nil
	}
	return axReady, nil
}

func sortedKeys(m map[string]render.Objects) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// generatedSecret is a Secret the render references but no manifest
// carries, because its value is secret: up creates it once, with a
// random value, and never rotates it. Only when the render holds the
// workload that reads it.
type generatedSecret struct {
	Namespace, Name, Key string
	// For is the workload that reads it, as "<Kind>/<name>" in Namespace.
	ForKind, ForName string
	Why              string
}

var generatedSecrets = []generatedSecret{
	// Postgres takes its password on first boot and keeps it in its
	// volume, so a new value would lock ax-server out.
	{Namespace: "ax-system", Name: "ax-postgres", Key: "password", ForKind: "StatefulSet", ForName: "ax-postgres",
		Why: "ax's Postgres password"},
}

func (g generatedSecret) what() string { return "secret " + g.Namespace + "/" + g.Name }

// wanted reports whether the render runs the workload that reads it.
func (g generatedSecret) wanted(objs render.Objects) bool {
	_, ok := objs.Find(g.ForKind, g.Namespace, g.ForName)
	return ok
}

// present reports whether the cluster holds the Secret with its key set.
func (g generatedSecret) present(ctx context.Context, c cluster.Cluster) (bool, error) {
	data, ok, err := c.Secret(ctx, g.Namespace, g.Name)
	if err != nil {
		return false, fmt.Errorf("%s: %w", g.what(), err)
	}
	return ok && len(data[g.Key]) > 0, nil
}

// randomSecret is a new value for a generated Secret: 32 random bytes,
// hex, so it needs no quoting anywhere it lands (a connection string, a
// shell).
func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// startupReader is a Deployment that reads an object only when it
// starts, so a change to the object needs a restart to take effect.
type startupReader struct {
	Kind, Namespace, Name string // the object, kind as kubectl names it
	Deployment            string // in Namespace
	What                  string // the object, for messages
}

// startupReaders are the platform's: lookout reads its task template,
// and Substrate's Kubernetes Secrets credential provider its namespace
// policy (an agent's credential fetches are refused until it has
// restarted on a policy that admits the agent's atespace).
var startupReaders = []startupReader{
	{Kind: "secret", Namespace: Namespace, Name: AgentSecret, Deployment: Lookout, What: "its task template"},
	{Kind: "configmap", Namespace: "ate-system", Name: "k8s-credential-provider-namespace-policy", Deployment: "k8s-credential-provider", What: "its namespace policy"},
}

// stale reports whether a running pod of the reader started before the
// object was last written.
func (sr startupReader) stale(ctx context.Context, c cluster.Cluster) (bool, error) {
	updated, ok, err := c.ObjectUpdated(ctx, sr.Kind, sr.Namespace, sr.Name)
	if err != nil || !ok {
		return false, err
	}
	starts, err := c.PodStarts(ctx, sr.Namespace, sr.Deployment)
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
	objects := p.Objects
	// In a dry run, the agent's Secrets can't be validated either while
	// their namespace is one the run would create.
	secretsPending := false
	if opts.DryRun {
		existing, pending, err := splitByNamespace(ctx, p.Objects, c)
		if err != nil {
			return err
		}
		for _, ns := range sortedKeys(pending) {
			log("namespace %s and the %d objects in it would be created (not validated: a dry run can't create the namespace they need)", ns, len(pending[ns])-1)
		}
		objects = existing
		_, secretsPending = pending[Namespace]
	}
	var namespaces, rest render.Objects
	for _, o := range objects {
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
	patches, err := neededPatches(ctx, p, c, log)
	if err != nil {
		return err
	}
	var generate []generatedSecret
	for _, g := range generatedSecrets {
		if !g.wanted(p.Objects) {
			continue
		}
		ok, err := g.present(ctx, c)
		if err != nil {
			return err
		}
		if !ok {
			generate = append(generate, g)
		}
	}
	secretWrites, err := managedSecrets(ctx, p, c)
	if err != nil {
		return err
	}
	sw, err := switchboardSecret(ctx, p, c, t)
	if err != nil {
		return err
	}
	if sw != nil {
		secretWrites = append(secretWrites, *sw)
	}
	// The Workspace is reconciled last, through ax, which on a fresh
	// cluster only exists once the objects below have rolled out. When ax
	// is already up, ask it now anyway, so a broken ax CLI or connection
	// fails before anything is written.
	_, nsPending, err := splitByNamespace(ctx, p.Objects, c)
	if err != nil {
		return err
	}
	ax, err := axState(ctx, p.Objects, nsPending, c)
	if err != nil {
		return err
	}
	if ax == axReady {
		if _, _, err := c.AxWorkspace(ctx, p.Env.Agent.Atespace, p.Env.Agent.Workspace); err != nil {
			return fmt.Errorf("workspace %s/%s (nothing written yet): %w", p.Env.Agent.Atespace, p.Env.Agent.Workspace, err)
		}
	}

	// Write.
	if len(namespaces) > 0 {
		if _, err := c.Apply(ctx, []byte(nsYAML), opts.DryRun); err != nil {
			return err
		}
		log("applied %d namespaces%s", len(namespaces), dry(opts.DryRun))
	}
	switch {
	case secretsPending:
		log("tokens %s and the task template would be stored in namespace %s, which the run creates (not validated; dry run)", source, Namespace)
	case !strings.HasPrefix(source, "secret "):
		secret := cluster.SecretManifest(Namespace, TokensSecret, map[string][]byte{
			"inject": []byte(t.Inject), "attach": []byte(t.Attach),
		})
		if err := c.ApplySecret(ctx, secret, opts.DryRun); err != nil {
			return fmt.Errorf("secret %s/%s: %w", Namespace, TokensSecret, err)
		}
		log("tokens %s, stored in secret %s/%s%s", source, Namespace, TokensSecret, dry(opts.DryRun))
	}
	if templateChanged && !secretsPending {
		secret := cluster.SecretManifest(Namespace, AgentSecret, map[string][]byte{
			"token": []byte(t.Attach), "task.yaml": task,
		})
		if err := c.ApplySecret(ctx, secret, opts.DryRun); err != nil {
			return fmt.Errorf("secret %s/%s: %w", Namespace, AgentSecret, err)
		}
		log("task template applied to secret %s/%s%s", Namespace, AgentSecret, dry(opts.DryRun))
	} else if !secretsPending {
		log("task template in secret %s/%s unchanged", Namespace, AgentSecret)
	}
	for _, g := range generate {
		if opts.DryRun && nsPending[g.Namespace] != nil {
			log("%s (%s) would be generated in namespace %s, which the run creates (not validated; dry run)", g.what(), g.Why, g.Namespace)
			continue
		}
		v, err := randomSecret()
		if err != nil {
			return err
		}
		if err := c.ApplySecret(ctx, cluster.SecretManifest(g.Namespace, g.Name, map[string][]byte{g.Key: []byte(v)}), opts.DryRun); err != nil {
			return fmt.Errorf("%s: %w", g.what(), err)
		}
		log("%s generated (%s)%s", g.what(), g.Why, dry(opts.DryRun))
	}
	pendingNS := map[string]bool{}
	for ns := range nsPending {
		pendingNS[ns] = true
	}
	if err := writeSecrets(ctx, c, secretWrites, pendingNS, opts.DryRun, log); err != nil {
		return err
	}
	if len(rest) > 0 {
		out, err := c.Apply(ctx, []byte(restYAML), opts.DryRun)
		if err != nil {
			return err
		}
		log("applied %d objects%s\n%s", len(rest), dry(opts.DryRun), strings.TrimRight(out, "\n"))
	}
	if err := applyPatches(ctx, c, patches, opts.DryRun, log); err != nil {
		return err
	}
	if opts.DryRun {
		if templateChanged {
			log("deployment %s/%s would be restarted (dry run)", Namespace, Lookout)
		}
		for _, sr := range readers(p) {
			if sr.Deployment != Lookout {
				log("deployment %s/%s would be restarted if it then runs on an older %s (dry run)", sr.Namespace, sr.Deployment, strings.TrimPrefix(sr.What, "its "))
			}
		}
		if ax != axReady {
			log("workspace %s/%s would be applied once ax is up (ax is %s; dry run)", p.Env.Agent.Atespace, p.Env.Agent.Workspace, ax)
			return nil
		}
		return reconcileWorkspace(ctx, p, c, ws, true, log)
	}
	if templateChanged {
		if _, ok := p.Objects.Find("Deployment", Namespace, Lookout); ok {
			if err := c.RolloutRestart(ctx, Namespace, Lookout); err != nil {
				return err
			}
			log("deployment %s/%s restarted to read the new task template", Namespace, Lookout)
		}
	}
	// Any other startup reader running on an older object restarts. Judged
	// from the cluster's state, not from what this run changed, so an up
	// that stopped between writing an object and restarting its reader is
	// put right by the next one.
	for _, sr := range readers(p) {
		if sr.Deployment == Lookout {
			continue // restarted on templateChanged, above
		}
		stale, err := sr.stale(ctx, c)
		if err != nil {
			return err
		}
		if !stale {
			continue
		}
		if err := c.RolloutRestart(ctx, sr.Namespace, sr.Deployment); err != nil {
			return err
		}
		if err := c.RolloutStatus(ctx, sr.Namespace, sr.Deployment); err != nil {
			return err
		}
		log("deployment %s/%s restarted to read %s", sr.Namespace, sr.Deployment, sr.What)
	}
	for _, o := range p.Objects {
		if o.Kind() == "Deployment" {
			if err := c.RolloutStatus(ctx, o.Namespace(), o.Name()); err != nil {
				return err
			}
			log("deployment %s/%s rolled out", o.Namespace(), o.Name())
		}
	}
	if err := reconcileWorkspace(ctx, p, c, ws, false, log); err != nil {
		return fmt.Errorf("workspace %s/%s (every object is applied and rolled out; re-run up to retry): %w", p.Env.Agent.Atespace, p.Env.Agent.Workspace, err)
	}
	return nil
}

// reconcileWorkspace applies the rendered Workspace through ax unless ax
// already has an identical one.
func reconcileWorkspace(ctx context.Context, p *Plan, c cluster.Cluster, ws []byte, dryRun bool, log func(string, ...any)) error {
	live, found, err := c.AxWorkspace(ctx, p.Env.Agent.Atespace, p.Env.Agent.Workspace)
	if err != nil {
		return err
	}
	if found {
		same, err := sameWorkspace(live, ws)
		if err != nil {
			return err
		}
		if same {
			log("workspace %s/%s unchanged", p.Env.Agent.Atespace, p.Env.Agent.Workspace)
			return nil
		}
	}
	if dryRun {
		log("workspace %s/%s would be applied (dry run)", p.Env.Agent.Atespace, p.Env.Agent.Workspace)
		return nil
	}
	if err := c.AxApply(ctx, p.Env.Agent.Atespace, ws); err != nil {
		return err
	}
	log("workspace %s/%s applied", p.Env.Agent.Atespace, p.Env.Agent.Workspace)
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
