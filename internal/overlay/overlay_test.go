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

package overlay

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// checkout makes a git repository with one profile and returns its path
// and HEAD.
func checkout(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "deploy/profiles/p/kustomization.yaml"), "resources:\n  - cm.yaml\n")
	write(t, filepath.Join(root, "deploy/profiles/p/cm.yaml"),
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: base\n  namespace: demo\ndata:\n  k: base\n")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-q", "-m", "init")
	return root, git("rev-parse", "HEAD")
}

// env writes an overlay that pins profile p at ref and patches it.
func env(t *testing.T, ref string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "kustomization.yaml"), `resources:
  - https://github.com/go-steer/shipmind//deploy/profiles/p?ref=`+ref+`
  - extra
patches:
  - path: patch.yaml
`)
	write(t, filepath.Join(dir, "patch.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: base\n  namespace: demo\ndata:\n  k: patched\n")
	write(t, filepath.Join(dir, "extra/kustomization.yaml"), "resources:\n  - cm.yaml\n")
	write(t, filepath.Join(dir, "extra/cm.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: extra\n  namespace: demo\n")
	write(t, filepath.Join(dir, ".git/HEAD"), "ignored\n")
	return dir
}

func TestRender(t *testing.T) {
	root, head := checkout(t)
	res, err := Render(env(t, head), root, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if res.Ref != head || len(res.Objects) != 2 {
		t.Fatalf("Render = %s, %d objects", res.Ref, len(res.Objects))
	}
	base, ok := res.Objects.Find("ConfigMap", "demo", "base")
	if !ok {
		t.Fatal("no base ConfigMap")
	}
	if s, _ := base.String(); !strings.Contains(s, "k: patched") {
		t.Errorf("overlay patch not applied:\n%s", s)
	}
}

func TestRenderRefChecks(t *testing.T) {
	root, head := checkout(t)
	other := strings.Repeat("1", 40)
	if _, err := Render(env(t, other), root, Options{}); err == nil || !strings.Contains(err.Error(), "pins shipmind "+other) {
		t.Errorf("wrong ref: err = %v", err)
	}
	if _, err := Render(env(t, other), root, Options{AnyRef: true}); err != nil {
		t.Errorf("wrong ref with AnyRef: %v", err)
	}
	write(t, filepath.Join(root, "dirty.txt"), "x")
	if _, err := Render(env(t, head), root, Options{}); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("dirty tree: err = %v", err)
	}
	if _, err := Render(env(t, head), t.TempDir(), Options{}); err == nil {
		t.Error("Render against a non-checkout succeeded")
	}
}

func TestRenderErrors(t *testing.T) {
	root, head := checkout(t)
	pin := "https://github.com/go-steer/shipmind//deploy/profiles/p?ref=" + head
	for _, tc := range []struct {
		name, kustomization, want string
		extra                     map[string]string
	}{
		{name: "no shipmind", kustomization: "resources:\n  - extra\n", want: "references no shipmind profile"},
		{name: "branch ref", kustomization: "resources:\n  - https://github.com/go-steer/shipmind//deploy/profiles/p?ref=main\n", want: "neither a shipmind pin"},
		{name: "two refs", kustomization: "resources:\n  - " + pin + "\n  - https://github.com/go-steer/shipmind//deploy/profiles/p?ref=" + strings.Repeat("2", 40) + "\n", want: "pins shipmind at both"},
		{name: "missing profile", kustomization: "resources:\n  - https://github.com/go-steer/shipmind//deploy/profiles/nope?ref=" + head + "\n", want: "is not a kustomization"},
		{name: "not yaml", kustomization: "resources: [\n", want: "kustomization.yaml"},
		{name: "bad patch", kustomization: "resources:\n  - " + pin + "\npatches:\n  - path: missing.yaml\n", want: "render"},
		{name: "remote component", kustomization: "resources:\n  - " + pin + "\ncomponents:\n  - https://github.com/go-steer/shipmind//deploy/profiles/p?ref=" + strings.Repeat("3", 40) + "\n", want: "pins shipmind at both"},
		{name: "scp-style remote", kustomization: "resources:\n  - " + pin + "\n  - git@github.com:go-steer/shipmind.git//deploy/profiles/p?ref=" + head + "\n", want: "neither a shipmind pin"},
		{name: "other host form", kustomization: "resources:\n  - " + pin + "\n  - https://github.com/Go-Steer/shipmind//deploy/profiles/p?ref=" + head + "\n", want: "neither a shipmind pin"},
		{name: "host-qualified", kustomization: "resources:\n  - " + pin + "\n  - github.com/kubernetes-sigs/kustomize//examples/helloWorld?ref=v5.0.0\n", want: "neither a shipmind pin"},
		{name: "remote base", kustomization: "resources:\n  - " + pin + "\nbases:\n  - https://example.com/x\n", want: "bases entry"},
		{name: "escaping pin", kustomization: "resources:\n  - https://github.com/go-steer/shipmind//deploy/../../etc?ref=" + head + "\n", want: "leaves the shipmind repository"},
		{name: "outside the overlay", kustomization: "resources:\n  - " + pin + "\n  - ../shared\n", want: "neither a shipmind pin"},
		{name: "helm", kustomization: "resources:\n  - " + pin + "\nhelmCharts:\n  - name: x\n", want: "helmCharts"},
		{name: "not a list", kustomization: "resources: x\n", want: "resources is not a list"},
		{name: "nested remote", kustomization: "resources:\n  - " + pin + "\n  - extra\n",
			extra: map[string]string{"extra/kustomization.yaml": "resources:\n  - https://example.com/remote.yaml\n"}, want: "extra/kustomization.yaml"},
		{name: "other name", kustomization: "resources:\n  - " + pin + "\n",
			extra: map[string]string{"sub/kustomization.yml": "resources: []\n"}, want: "name kustomizations kustomization.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "kustomization.yaml"), tc.kustomization)
			write(t, filepath.Join(dir, "extra/kustomization.yaml"), "resources: []\n")
			for name, body := range tc.extra {
				write(t, filepath.Join(dir, name), body)
			}
			_, err := Render(dir, root, Options{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := Render(t.TempDir(), root, Options{}); err == nil {
		t.Error("Render of a directory with no kustomization succeeded")
	}
}

// A shipmind pin in a nested kustomization is swapped too.
func TestRenderNestedPin(t *testing.T) {
	root, head := checkout(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "kustomization.yaml"), "resources:\n  - nested\n")
	write(t, filepath.Join(dir, "nested/kustomization.yaml"), "resources:\n  - https://github.com/go-steer/shipmind//deploy/profiles/p?ref="+head+"\n")
	res, err := Render(dir, root, Options{})
	if err != nil || res.Ref != head || len(res.Objects) != 1 {
		t.Fatalf("Render = %+v, %v", res, err)
	}
}

// Untracked files count as uncommitted even when git is told to hide
// them.
func TestRenderDirtyUntrackedHidden(t *testing.T) {
	root, head := checkout(t)
	cmd := exec.Command("git", "-C", root, "config", "status.showUntrackedFiles", "no")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	write(t, filepath.Join(root, "stray.yaml"), "x")
	if _, err := Render(env(t, head), root, Options{}); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("err = %v", err)
	}
}

func TestRenderRefusesSymlinks(t *testing.T) {
	root, head := checkout(t)
	dir := env(t, head)
	if err := os.Symlink("/etc/hostname", filepath.Join(dir, "link.yaml")); err != nil {
		t.Skip(err)
	}
	if _, err := Render(dir, root, Options{}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("err = %v, want a refused symlink", err)
	}
}
