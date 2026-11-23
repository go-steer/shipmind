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

package render

// The invariants every profile in deploy/profiles must hold. They run
// against the real profiles, so a change to a manifest, a patch or a
// pin that breaks one fails `go test ./...`.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/kustomize/kyaml/yaml"

	"github.com/go-steer/shipmind/internal/bom"
)

const profilesDir = "../../deploy/profiles"

// e2eDir holds the end-to-end deployments environments opt into
// (deploy/e2e/<profile>), held to the same invariants as profiles.
const e2eDir = "../../deploy/e2e"

// switchboardDir holds switchboard's gateways environments opt into
// (deploy/switchboard/<platform>), held to the same invariants.
const switchboardDir = "../../deploy/switchboard"

// profileParts returns every profile (deploy/profiles/<profile>), every
// part directly under one (deploy/profiles/<profile>/<part>), every
// end-to-end deployment (deploy/e2e/<profile>) and every switchboard
// gateway (deploy/switchboard/<platform>).
func profileParts(t *testing.T) []string {
	t.Helper()
	var dirs []string
	for _, pattern := range []string{
		filepath.Join(profilesDir, "*", "kustomization.yaml"),
		filepath.Join(profilesDir, "*", "*", "kustomization.yaml"),
		filepath.Join(e2eDir, "*", "kustomization.yaml"),
		filepath.Join(e2eDir, "*", "*", "kustomization.yaml"),
		filepath.Join(switchboardDir, "*", "kustomization.yaml"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			dirs = append(dirs, filepath.Dir(m))
		}
	}
	if len(dirs) == 0 {
		t.Fatal("no profiles found")
	}
	return dirs
}

// A profile's root includes every one of its parts, so an overlay that
// references the profile gets all of it.
func TestProfileRootsIncludeEveryPart(t *testing.T) {
	roots, err := filepath.Glob(filepath.Join(profilesDir, "*", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range roots {
		b, err := os.ReadFile(root)
		if err != nil {
			t.Fatal(err)
		}
		var k struct {
			Resources []string `yaml:"resources"`
		}
		if err := yaml.Unmarshal(b, &k); err != nil {
			t.Fatalf("%s: %v", root, err)
		}
		parts, err := filepath.Glob(filepath.Join(filepath.Dir(root), "*", "kustomization.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, part := range parts {
			name := filepath.Base(filepath.Dir(part))
			if !slices.Contains(k.Resources, name) {
				t.Errorf("%s does not include its part %s", root, name)
			}
		}
	}
}

func build(t *testing.T, dir string) Objects {
	t.Helper()
	objs, err := Build(dir)
	if err != nil {
		t.Fatalf("Build(%s): %v", dir, err)
	}
	return objs
}

// Every image is pinned in versions.yaml, exactly: no tag-only images,
// no placeholders, no digest that versions.yaml doesn't know. An
// environment overlay may still point at its own registry.
func TestProfileImagesArePinned(t *testing.T) {
	f, err := bom.Load("../../versions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pinned := map[string]bool{}
	for _, c := range f.Components {
		for _, img := range c.Images {
			pinned[img.Ref] = true
		}
	}
	for _, dir := range profileParts(t) {
		imgs, err := build(t, dir).Images()
		if err != nil {
			t.Fatal(err)
		}
		for _, img := range imgs {
			if pinned[img.Ref] {
				continue
			}
			t.Errorf("%s: %s container %s runs %q, which versions.yaml does not pin", dir, img.Object, img.Container, img.Ref)
		}
	}
}

// ax keeps every task and workspace record in Postgres. Without a
// volume, a Postgres restart erases them all (Redis did, on 2026-10-19).
// Postgres and ax-server both read the password from the Secret `shipmind
// up` generates (internal/deploy's generatedSecrets).
func TestSubstrateAxPostgresPersists(t *testing.T) {
	objs := build(t, filepath.Join(profilesDir, "substrate", "ax"))

	sts, ok := objs.Find("StatefulSet", "ax-system", "ax-postgres")
	if !ok {
		t.Fatal("no StatefulSet ax-system/ax-postgres")
	}
	templates, err := sts.Pipe(yaml.Lookup("spec", "volumeClaimTemplates"))
	if err != nil || templates == nil {
		t.Fatalf("no volume claim templates (%v)", err)
	}
	claims, err := templates.Elements()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range claims {
		name, _ := c.Pipe(yaml.Lookup("metadata", "name"))
		storage, _ := c.Pipe(yaml.Lookup("spec", "resources", "requests", "storage"))
		if yaml.GetValue(name) == "data" && storage != nil && yaml.GetValue(storage) != "" {
			found = true
		}
	}
	if !found {
		t.Error("no volume claim template data with a storage request")
	}
	spec, err := sts.Pipe(yaml.Lookup("spec", "template", "spec"))
	if err != nil || spec == nil {
		t.Fatalf("pod spec: %v", err)
	}
	mount, err := spec.Pipe(yaml.Lookup("containers", "[name=postgres]", "volumeMounts", "[name=data]", "mountPath"))
	if err != nil || mount == nil || yaml.GetValue(mount) != "/var/lib/postgresql/data" {
		t.Errorf("volume data is not mounted at /var/lib/postgresql/data (%v)", err)
	}
	pgdata, err := spec.Pipe(yaml.Lookup("containers", "[name=postgres]", "env", "[name=PGDATA]", "value"))
	if err != nil || pgdata == nil || !strings.HasPrefix(yaml.GetValue(pgdata), "/var/lib/postgresql/data/") {
		t.Errorf("PGDATA is not a directory on the data volume (%v)", err)
	}

	d, ok := objs.Find("Deployment", "ax-system", "ax-server")
	if !ok {
		t.Fatal("no Deployment ax-system/ax-server")
	}
	for _, c := range []struct {
		obj       *yaml.RNode
		container string
		env       string
	}{
		{sts.RNode, "postgres", "POSTGRES_PASSWORD"},
		{d.RNode, "ax-server", "PGPASSWORD"},
	} {
		ref, err := c.obj.Pipe(yaml.Lookup("spec", "template", "spec", "containers", "[name="+c.container+"]", "env", "[name="+c.env+"]", "valueFrom", "secretKeyRef"))
		if err != nil || ref == nil {
			t.Errorf("%s: %s is not from a Secret (%v)", c.container, c.env, err)
			continue
		}
		name, _ := ref.Pipe(yaml.Lookup("name"))
		key, _ := ref.Pipe(yaml.Lookup("key"))
		if yaml.GetValue(name) != "ax-postgres" || yaml.GetValue(key) != "password" {
			t.Errorf("%s: %s reads %s/%s, want ax-postgres/password", c.container, c.env, yaml.GetValue(name), yaml.GetValue(key))
		}
	}
}

// A profile's kustomizations reference only what is in this repository:
// no remote bases or components and no Helm charts, so a render is
// reproducible from a checkout and never touches the network. Every
// kustomization is named kustomization.yaml, so nothing hides from this
// check (or from profileParts) under another name kustomize accepts.
func TestProfilesHaveNoRemoteResources(t *testing.T) {
	for _, root := range []string{profilesDir, e2eDir, switchboardDir} {
		testNoRemote(t, root)
	}
}

func testNoRemote(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		switch d.Name() {
		case "kustomization.yml", "Kustomization":
			t.Errorf("%s: name kustomizations kustomization.yaml", p)
			return nil
		case "kustomization.yaml":
		default:
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var k struct {
			Resources  []string `yaml:"resources"`
			Bases      []string `yaml:"bases"`
			Components []string `yaml:"components"`
			HelmCharts []any    `yaml:"helmCharts"`
		}
		if err := yaml.Unmarshal(b, &k); err != nil {
			t.Errorf("%s: %v", p, err)
			return nil
		}
		if len(k.HelmCharts) > 0 {
			t.Errorf("%s: helmCharts fetch at render time; vendor the rendered chart instead", p)
		}
		for _, r := range slices.Concat(k.Resources, k.Bases, k.Components) {
			if isRemote(filepath.Dir(p), r) {
				t.Errorf("%s: remote reference %q", p, r)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// isRemote reports whether a kustomize reference, relative to dir, names
// something outside the checkout: a URL, an scp-style git address, or a
// host-qualified path such as github.com/org/repo//dir that doesn't
// exist locally.
func isRemote(dir, r string) bool {
	if strings.Contains(r, "://") || strings.HasPrefix(r, "git@") {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, r)); err == nil {
		return false
	}
	first, rest, found := strings.Cut(r, "/")
	return found && rest != "" && strings.Contains(first, ".") && first != "." && first != ".."
}

func TestIsRemote(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "patches.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	for r, want := range map[string]bool{
		"ax": false, "../../../vendor/ax/deploy/redis": false, "./redis": false, "redis.yaml": false, "patches.d": false,
		"https://github.com/a/b//c?ref=v1": true, "github.com/a/b//c?ref=v1": true, "git@github.com:a/b.git": true,
		"ssh://git@github.com/a/b": true,
	} {
		if got := isRemote(dir, r); got != want {
			t.Errorf("isRemote(%q) = %v, want %v", r, got, want)
		}
	}
}

// lookout keeps its incidents in a store; on an emptyDir a rollout made
// it re-open every open incident as new work (fixed on agent-substrate
// on 2026-10-19 by moving the store to a volume).
func TestSubstrateLookoutStorePersists(t *testing.T) {
	objs := build(t, filepath.Join(profilesDir, "substrate", "lookout"))
	if _, ok := objs.Find("PersistentVolumeClaim", "lookout-ax", "lookout-store"); !ok {
		t.Error("no PersistentVolumeClaim lookout-ax/lookout-store")
	}
	d, ok := objs.Find("Deployment", "lookout-ax", "lookout-ax")
	if !ok {
		t.Fatal("no Deployment lookout-ax/lookout-ax")
	}
	spec, err := d.Pipe(yaml.Lookup("spec", "template", "spec"))
	if err != nil || spec == nil {
		t.Fatalf("pod spec: %v", err)
	}
	claim, err := spec.Pipe(yaml.Lookup("volumes", "[name=store]", "persistentVolumeClaim", "claimName"))
	if err != nil || claim == nil || yaml.GetValue(claim) != "lookout-store" {
		t.Errorf("volume store is not the lookout-store claim (%v)", err)
	}
	mount, err := spec.Pipe(yaml.Lookup("containers", "[name=lookout]", "volumeMounts", "[name=store]", "mountPath"))
	if err != nil || mount == nil || yaml.GetValue(mount) != "/var/lib/lookout" {
		t.Errorf("volume store is not mounted at /var/lib/lookout (%v)", err)
	}
	argsNode, err := spec.Pipe(yaml.Lookup("containers", "[name=lookout]", "args"))
	if err != nil || argsNode == nil {
		t.Fatalf("lookout args: %v", err)
	}
	elems, err := argsNode.Elements()
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	for _, e := range elems {
		args = append(args, yaml.GetValue(e))
	}
	// Exactly one --store, on the volume: with Go's flag parsing a second
	// one (say, appended by an overlay) would win.
	var stores []string
	for i, a := range args {
		switch {
		case strings.HasPrefix(a, "--store="), strings.HasPrefix(a, "-store="):
			stores = append(stores, a[strings.Index(a, "=")+1:])
		case a == "--store" || a == "-store":
			if i+1 < len(args) {
				stores = append(stores, args[i+1])
			}
		}
	}
	if len(stores) != 1 || stores[0] != "/var/lib/lookout/lookout.db" {
		t.Errorf("lookout --store values %v, want exactly /var/lib/lookout/lookout.db", stores)
	}
}

const wantLookoutReactiveSources = "k8s-events,object-state,rollout,workload,autoscaling,saturation,degradation,expiry,capacity,ingress,gateway"

// checkSubstrateLookoutReactiveSources verifies #127 §1 on a rendered
// substrate profile (or its lookout part):
//   - args[0..2] stay at --in-cluster, --cluster-name=CLUSTER,
//     --namespace=NAMESPACES so environment overlays' RFC-6902 test+replace
//     patches on /spec/template/spec/containers/0/args/1 and /args/2 keep
//     matching;
//   - --sources is pinned once to the reactive portable set (with capacity,
//     and without topology-drift or compute-class);
//   - kube-system Role + RoleBinding grant ServiceAccount lookout-ax/lookout-ax
//     get on cluster-autoscaler-status and list on configmaps, alongside the
//     cluster-wide lookout-ax-watch ClusterRole + ClusterRoleBinding.
func checkSubstrateLookoutReactiveSources(objs Objects) error {
	d, ok := objs.Find("Deployment", "lookout-ax", "lookout-ax")
	if !ok {
		return fmt.Errorf("no Deployment lookout-ax/lookout-ax")
	}
	argsNode, err := d.Pipe(yaml.Lookup("spec", "template", "spec", "containers", "[name=lookout]", "args"))
	if err != nil || argsNode == nil {
		return fmt.Errorf("lookout args: %v", err)
	}
	elems, err := argsNode.Elements()
	if err != nil {
		return err
	}
	var args []string
	for _, e := range elems {
		args = append(args, yaml.GetValue(e))
	}
	wantPrefix := []string{"--in-cluster", "--cluster-name=CLUSTER", "--namespace=NAMESPACES"}
	if len(args) < len(wantPrefix) || !slices.Equal(args[:len(wantPrefix)], wantPrefix) {
		return fmt.Errorf("lookout args[0:3] = %v, want %v (environment overlays patch args/1 and args/2 by index)", args[:min(len(args), 3)], wantPrefix)
	}
	var sourcesFlags []string
	for i, a := range args {
		switch {
		case strings.HasPrefix(a, "--sources="), strings.HasPrefix(a, "-sources="):
			sourcesFlags = append(sourcesFlags, a[strings.Index(a, "=")+1:])
		case a == "--sources" || a == "-sources":
			if i+1 < len(args) {
				sourcesFlags = append(sourcesFlags, args[i+1])
			} else {
				sourcesFlags = append(sourcesFlags, "")
			}
		}
	}
	if len(sourcesFlags) != 1 {
		return fmt.Errorf("lookout --sources values %v, want exactly 1", sourcesFlags)
	}
	gotSources := strings.Split(sourcesFlags[0], ",")
	for _, forbidden := range []string{"topology-drift", "compute-class", "auto"} {
		if slices.Contains(gotSources, forbidden) {
			return fmt.Errorf("lookout --sources %q includes forbidden source %q", sourcesFlags[0], forbidden)
		}
	}
	if sourcesFlags[0] != wantLookoutReactiveSources {
		return fmt.Errorf("lookout --sources = %q, want %q", sourcesFlags[0], wantLookoutReactiveSources)
	}

	if _, ok := objs.Find("ClusterRole", "", "lookout-ax-watch"); !ok {
		return fmt.Errorf("no ClusterRole lookout-ax-watch")
	}
	crb, ok := objs.Find("ClusterRoleBinding", "", "lookout-ax-watch")
	if !ok {
		return fmt.Errorf("no ClusterRoleBinding lookout-ax-watch")
	}
	if err := checkBinding(crb, "ClusterRole", "lookout-ax-watch", "lookout-ax", "lookout-ax"); err != nil {
		return fmt.Errorf("ClusterRoleBinding lookout-ax-watch: %w", err)
	}

	if _, leaked := objs.Find("Role", "kube-system", "lookout-watch-capacity"); leaked {
		return fmt.Errorf("unrenamed Role kube-system/lookout-watch-capacity leaked into render")
	}
	role, ok := objs.Find("Role", "kube-system", "lookout-ax-watch-capacity")
	if !ok {
		return fmt.Errorf("no Role kube-system/lookout-ax-watch-capacity")
	}
	if err := checkCapacityRoleRules(role); err != nil {
		return fmt.Errorf("Role kube-system/lookout-ax-watch-capacity: %w", err)
	}
	rb, ok := objs.Find("RoleBinding", "kube-system", "lookout-ax-watch-capacity")
	if !ok {
		return fmt.Errorf("no RoleBinding kube-system/lookout-ax-watch-capacity")
	}
	if err := checkBinding(rb, "Role", "lookout-ax-watch-capacity", "lookout-ax", "lookout-ax"); err != nil {
		return fmt.Errorf("RoleBinding kube-system/lookout-ax-watch-capacity: %w", err)
	}
	return nil
}

func checkBinding(o Object, wantRoleKind, wantRoleName, wantSANamespace, wantSAName string) error {
	var b struct {
		RoleRef struct {
			APIGroup string `yaml:"apiGroup"`
			Kind     string `yaml:"kind"`
			Name     string `yaml:"name"`
		} `yaml:"roleRef"`
		Subjects []struct {
			Kind      string `yaml:"kind"`
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"subjects"`
	}
	s, err := o.String()
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal([]byte(s), &b); err != nil {
		return err
	}
	if b.RoleRef.APIGroup != "rbac.authorization.k8s.io" || b.RoleRef.Kind != wantRoleKind || b.RoleRef.Name != wantRoleName {
		return fmt.Errorf("roleRef = %+v, want rbac.authorization.k8s.io/%s/%s", b.RoleRef, wantRoleKind, wantRoleName)
	}
	if len(b.Subjects) != 1 || b.Subjects[0].Kind != "ServiceAccount" || b.Subjects[0].Namespace != wantSANamespace || b.Subjects[0].Name != wantSAName {
		return fmt.Errorf("subjects = %+v, want [ServiceAccount %s/%s]", b.Subjects, wantSANamespace, wantSAName)
	}
	return nil
}

func checkCapacityRoleRules(o Object) error {
	var r struct {
		Rules []struct {
			APIGroups     []string `yaml:"apiGroups"`
			Resources     []string `yaml:"resources"`
			ResourceNames []string `yaml:"resourceNames"`
			Verbs         []string `yaml:"verbs"`
		} `yaml:"rules"`
	}
	s, err := o.String()
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal([]byte(s), &r); err != nil {
		return err
	}
	hasGetStatus, hasList := false, false
	for _, rule := range r.Rules {
		if !slices.Equal(rule.APIGroups, []string{""}) || !slices.Equal(rule.Resources, []string{"configmaps"}) {
			return fmt.Errorf("unexpected rule %+v", rule)
		}
		switch {
		case slices.Equal(rule.Verbs, []string{"get"}) && slices.Equal(rule.ResourceNames, []string{"cluster-autoscaler-status"}):
			hasGetStatus = true
		case slices.Equal(rule.Verbs, []string{"list"}) && len(rule.ResourceNames) == 0:
			hasList = true
		default:
			return fmt.Errorf("unexpected configmaps rule %+v", rule)
		}
	}
	if !hasGetStatus || !hasList {
		return fmt.Errorf("rules %+v, want get on cluster-autoscaler-status + list on configmaps", r.Rules)
	}
	return nil
}

func TestSubstrateLookoutReactiveSourcesAndCapacityRBAC(t *testing.T) {
	for _, dir := range []string{
		filepath.Join(profilesDir, "substrate", "lookout"),
		filepath.Join(profilesDir, "substrate"),
	} {
		if err := checkSubstrateLookoutReactiveSources(build(t, dir)); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
	}

	// Verify the RFC-6902 test+replace overlay contract used by
	// go-steer/shipmind-deploy on args/1 and args/2.
	overlayDir := t.TempDir()
	absSubstrate, err := filepath.Abs(filepath.Join(profilesDir, "substrate"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(absSubstrate, filepath.Join(overlayDir, "substrate")); err != nil {
		t.Fatal(err)
	}
	patch := `- op: test
  path: /spec/template/spec/containers/0/args/1
  value: --cluster-name=CLUSTER
- op: replace
  path: /spec/template/spec/containers/0/args/1
  value: --cluster-name=demo-cluster
- op: test
  path: /spec/template/spec/containers/0/args/2
  value: --namespace=NAMESPACES
- op: replace
  path: /spec/template/spec/containers/0/args/2
  value: --namespace=triage-lab
`
	if err := os.WriteFile(filepath.Join(overlayDir, "lookout-ax.yaml"), []byte(patch), 0o644); err != nil {
		t.Fatal(err)
	}
	kustomization := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - substrate
patches:
  - path: lookout-ax.yaml
    target:
      kind: Deployment
      namespace: lookout-ax
      name: lookout-ax
`
	if err := os.WriteFile(filepath.Join(overlayDir, "kustomization.yaml"), []byte(kustomization), 0o644); err != nil {
		t.Fatal(err)
	}
	overlayObjs := build(t, overlayDir)
	d, ok := overlayObjs.Find("Deployment", "lookout-ax", "lookout-ax")
	if !ok {
		t.Fatal("overlay missing Deployment lookout-ax/lookout-ax")
	}
	argsNode, err := d.Pipe(yaml.Lookup("spec", "template", "spec", "containers", "[name=lookout]", "args"))
	if err != nil || argsNode == nil {
		t.Fatalf("overlay lookout args: %v", err)
	}
	elems, err := argsNode.Elements()
	if err != nil {
		t.Fatal(err)
	}
	var gotArgs []string
	for _, e := range elems {
		gotArgs = append(gotArgs, yaml.GetValue(e))
	}
	if gotArgs[1] != "--cluster-name=demo-cluster" || gotArgs[2] != "--namespace=triage-lab" {
		t.Errorf("overlay args[1:3] = %v, want [--cluster-name=demo-cluster --namespace=triage-lab]", gotArgs[1:3])
	}
	if gotArgs[len(gotArgs)-1] != "--sources="+wantLookoutReactiveSources {
		t.Errorf("overlay last arg = %q, want --sources=%s", gotArgs[len(gotArgs)-1], wantLookoutReactiveSources)
	}

	// Negative cases: every invariant check must fail when violated.
	clone := func() Objects {
		return build(t, filepath.Join(profilesDir, "substrate", "lookout"))
	}
	setArgs := func(objs Objects, args ...string) Objects {
		d, _ := objs.Find("Deployment", "lookout-ax", "lookout-ax")
		c, err := d.Pipe(yaml.Lookup("spec", "template", "spec", "containers", "[name=lookout]"))
		if err != nil {
			t.Fatal(err)
		}
		seq := yaml.NewListRNode(args...)
		if err := c.PipeE(yaml.SetField("args", seq)); err != nil {
			t.Fatal(err)
		}
		return objs
	}
	drop := func(objs Objects, kind, ns, name string) Objects {
		return slices.DeleteFunc(objs, func(o Object) bool {
			return o.Kind() == kind && o.Namespace() == ns && o.Name() == name
		})
	}
	negatives := []struct {
		name string
		objs Objects
		want string
	}{
		{
			name: "shifted args[1..2] placeholders",
			objs: setArgs(clone(), "--in-cluster", "--sources="+wantLookoutReactiveSources, "--cluster-name=CLUSTER", "--namespace=NAMESPACES"),
			want: "args[0:3]",
		},
		{
			name: "missing --sources",
			objs: setArgs(clone(), "--in-cluster", "--cluster-name=CLUSTER", "--namespace=NAMESPACES"),
			want: "--sources values",
		},
		{
			name: "duplicate --sources",
			objs: setArgs(clone(), "--in-cluster", "--cluster-name=CLUSTER", "--namespace=NAMESPACES", "--sources="+wantLookoutReactiveSources, "--sources="+wantLookoutReactiveSources),
			want: "--sources values",
		},
		{
			name: "topology-drift still enabled",
			objs: setArgs(clone(), "--in-cluster", "--cluster-name=CLUSTER", "--namespace=NAMESPACES", "--sources="+wantLookoutReactiveSources+",topology-drift"),
			want: `forbidden source "topology-drift"`,
		},
		{
			name: "compute-class still enabled",
			objs: setArgs(clone(), "--in-cluster", "--cluster-name=CLUSTER", "--namespace=NAMESPACES", "--sources="+wantLookoutReactiveSources+",compute-class"),
			want: `forbidden source "compute-class"`,
		},
		{
			name: "--sources=auto",
			objs: setArgs(clone(), "--in-cluster", "--cluster-name=CLUSTER", "--namespace=NAMESPACES", "--sources=auto"),
			want: `forbidden source "auto"`,
		},
		{
			name: "missing capacity Role",
			objs: drop(clone(), "Role", "kube-system", "lookout-ax-watch-capacity"),
			want: "no Role kube-system/lookout-ax-watch-capacity",
		},
		{
			name: "missing capacity RoleBinding",
			objs: drop(clone(), "RoleBinding", "kube-system", "lookout-ax-watch-capacity"),
			want: "no RoleBinding kube-system/lookout-ax-watch-capacity",
		},
	}
	for _, tc := range negatives {
		err := checkSubstrateLookoutReactiveSources(tc.objs)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want error containing %q", tc.name, err, tc.want)
		}
	}
}

// A WorkerPool's workerImage is not a container field, so
// TestProfileImagesArePinned doesn't see it: hold it to the same rule.
func TestProfileWorkerImagesArePinned(t *testing.T) {
	f, err := bom.Load("../../versions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pinned := map[string]bool{}
	for _, c := range f.Components {
		for _, img := range c.Images {
			pinned[img.Ref] = true
		}
	}
	seen := 0
	for _, dir := range profileParts(t) {
		for _, o := range build(t, dir) {
			if o.Kind() != "WorkerPool" {
				continue
			}
			seen++
			ref, err := o.Pipe(yaml.Lookup("spec", "workerImage"))
			if err != nil || ref == nil {
				t.Errorf("%s: WorkerPool %s has no workerImage (%v)", dir, o.Name(), err)
				continue
			}
			if v := yaml.GetValue(ref); !pinned[v] {
				t.Errorf("%s: WorkerPool %s runs %q, which versions.yaml does not pin", dir, o.Name(), v)
			}
		}
	}
	if seen == 0 {
		t.Error("no WorkerPool in any profile")
	}
}

// Substrate's router takes only mTLS clients (v1.0). Each of its clients
// in the substrate profile, and the nightly scenario, mounts the pod's
// podidentity certificate and the servicedns trust bundle, both as
// projected volumes, and its SPIFFE ID goes in the install's allowlist.
func TestSubstrateRouterClientsMountMTLS(t *testing.T) {
	type client struct{ dir, kind, ns, name string }
	for _, c := range []client{
		{filepath.Join(profilesDir, "substrate"), "Deployment", "ax-system", "ax-server"},
		{filepath.Join(profilesDir, "substrate"), "Deployment", "lookout-ax", "lookout-ax"},
		{filepath.Join(profilesDir, "substrate"), "Deployment", "substrate-scope", "substrate-scope"},
		{filepath.Join(profilesDir, "..", "e2e", "substrate"), "CronJob", "shipmind-e2e", "missing-env"},
		{filepath.Join(profilesDir, "..", "e2e", "substrate"), "CronJob", "shipmind-e2e", "oom-killed"},
		{filepath.Join(switchboardDir, "slack"), "Deployment", "switchboard", "switchboard"},
	} {
		o, ok := build(t, c.dir).Find(c.kind, c.ns, c.name)
		if !ok {
			t.Errorf("no %s %s/%s in %s", c.kind, c.ns, c.name, c.dir)
			continue
		}
		path := []string{"spec", "template", "spec", "volumes"}
		if c.kind == "CronJob" {
			path = []string{"spec", "jobTemplate", "spec", "template", "spec", "volumes"}
		}
		vols, err := o.Pipe(yaml.Lookup(path...))
		if err != nil || vols == nil {
			t.Errorf("%s %s: no volumes (%v)", c.kind, c.name, err)
			continue
		}
		s, err := vols.String()
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"podCertificate:", "signerName: podidentity.podcert.ate.dev/identity", "clusterTrustBundle:", "signerName: servicedns.podcert.ate.dev/identity"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s %s/%s: volumes lack %q", c.kind, c.ns, c.name, want)
			}
		}
	}
}
