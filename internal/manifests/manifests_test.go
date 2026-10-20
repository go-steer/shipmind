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

package manifests

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/shipmind/internal/bom"
)

// gitRepo makes a local repository with two manifests, tags v1.0.0,
// and returns its path and the commit SHA.
func gitRepo(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("config", "uploadpack.allowAnySHA1InWant", "true")
	if err := os.MkdirAll(filepath.Join(dir, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"deploy/a.yaml": "kind: A\n", "deploy/b.yaml": "kind: B\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")
	run("tag", "v1.0.0")
	return dir, run("rev-parse", "HEAD")
}

func comp(version string, manifests ...string) bom.Component {
	return bom.Component{Name: "ax", Source: "github.com/example/ax", Version: version,
		Profiles: []string{"substrate"}, Manifests: manifests}
}

func TestGitVendorAndVerify(t *testing.T) {
	repo, sha := gitRepo(t)
	g := Git{URL: func(string) string { return "file://" + repo }}

	for _, version := range []string{sha, "v1.0.0"} {
		t.Run(version, func(t *testing.T) {
			root := t.TempDir()
			c := comp(version, "deploy/a.yaml", "deploy/b.yaml")
			if err := Vendor(context.Background(), g, root, c); err != nil {
				t.Fatalf("Vendor: %v", err)
			}
			b, err := os.ReadFile(filepath.Join(root, Dir, "ax", "deploy", "a", "a.yaml"))
			if err != nil || string(b) != "kind: A\n" {
				t.Fatalf("a.yaml = %q, %v", b, err)
			}
			if err := Verify(root, c); err != nil {
				t.Fatalf("Verify: %v", err)
			}
			lock, err := os.ReadFile(filepath.Join(root, Dir, "ax", LockFile))
			if err != nil || !strings.Contains(string(lock), "commit: "+sha+"\n") {
				t.Errorf("lock does not record commit %s: %q, %v", sha, lock, err)
			}
			k, err := os.ReadFile(filepath.Join(root, Dir, "ax", "deploy", "b", KustomizationFile))
			if err != nil || !strings.HasSuffix(string(k), "resources:\n  - b.yaml\n") {
				t.Errorf("kustomization.yaml = %q, %v", k, err)
			}
		})
	}
}

func TestGitFetchErrors(t *testing.T) {
	repo, _ := gitRepo(t)
	g := Git{URL: func(string) string { return "file://" + repo }}
	ctx := context.Background()
	if _, _, err := g.Fetch(ctx, "github.com/example/ax", "v9.9.9", []string{"deploy/a.yaml"}); err == nil ||
		!strings.Contains(err.Error(), "github.com/example/ax@v9.9.9") {
		t.Errorf("missing tag: err = %v", err)
	}
	if _, _, err := g.Fetch(ctx, "github.com/example/ax", "v1.0.0", []string{"deploy/nope.yaml"}); err == nil ||
		!strings.Contains(err.Error(), "deploy/nope.yaml") {
		t.Errorf("missing file: err = %v", err)
	}
}

const fakeCommit = "0123456789abcdef0123456789abcdef01234567"

type fakeFetcher struct {
	files map[string][]byte
	err   error
}

func (f fakeFetcher) Fetch(context.Context, string, string, []string) (map[string][]byte, string, error) {
	return f.files, fakeCommit, f.err
}

func vendored(t *testing.T) (string, bom.Component) {
	t.Helper()
	root := t.TempDir()
	c := comp("v1.0.0", "deploy/a.yaml", "b.yaml")
	f := fakeFetcher{files: map[string][]byte{"deploy/a.yaml": []byte("a"), "b.yaml": []byte("b")}}
	if err := Vendor(context.Background(), f, root, c); err != nil {
		t.Fatalf("Vendor: %v", err)
	}
	return root, c
}

func TestVendorErrors(t *testing.T) {
	ctx := context.Background()
	if err := Vendor(ctx, fakeFetcher{}, t.TempDir(), comp("v1.0.0")); err == nil {
		t.Error("Vendor with no manifests succeeded")
	}
	if err := Vendor(ctx, fakeFetcher{err: errors.New("boom")}, t.TempDir(), comp("v1.0.0", "a.yaml")); err == nil ||
		err.Error() != "boom" {
		t.Errorf("fetch error: err = %v", err)
	}
	if err := Vendor(ctx, fakeFetcher{files: map[string][]byte{}}, t.TempDir(), comp("v1.0.0", "a.yaml")); err == nil ||
		!strings.Contains(err.Error(), "returned no a.yaml") {
		t.Errorf("short fetch: err = %v", err)
	}
}

func TestVendorReplacesStaleFiles(t *testing.T) {
	root, c := vendored(t)
	stale := filepath.Join(root, Dir, "ax", "old.yaml")
	if BaseDir("deploy/ax.yml") != "deploy/ax" {
		t.Errorf("BaseDir(deploy/ax.yml) = %q", BaseDir("deploy/ax.yml"))
	}
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := fakeFetcher{files: map[string][]byte{"deploy/a.yaml": []byte("a"), "b.yaml": []byte("b")}}
	if err := Vendor(context.Background(), f, root, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale file survived re-vendor: %v", err)
	}
}

func TestVerify(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, root string, c *bom.Component)
		want   string
	}{
		{"pin moved", func(_ *testing.T, _ string, c *bom.Component) { c.Version = "v1.1.0" },
			"vendored from github.com/example/ax@v1.0.0, but versions.yaml pins github.com/example/ax@v1.1.0"},
		{"manifest added", func(_ *testing.T, _ string, c *bom.Component) { c.Manifests = append(c.Manifests, "c.yaml") },
			"but versions.yaml lists"},
		{"edited", func(t *testing.T, root string, _ *bom.Component) {
			write(t, filepath.Join(root, Dir, "ax", "b", "b.yaml"), "B")
		}, "b/b.yaml was edited after vendoring"},
		{"extra file", func(t *testing.T, root string, _ *bom.Component) {
			write(t, filepath.Join(root, Dir, "ax", "extra.yaml"), "x")
		}, "extra.yaml is not a vendored manifest or its kustomization"},
		{"deleted file", func(t *testing.T, root string, _ *bom.Component) {
			if err := os.Remove(filepath.Join(root, Dir, "ax", "deploy", "a", "a.yaml")); err != nil {
				t.Fatal(err)
			}
		}, "deploy/a/a.yaml is missing"},
		{"no lock", func(t *testing.T, root string, _ *bom.Component) {
			if err := os.Remove(filepath.Join(root, Dir, "ax", LockFile)); err != nil {
				t.Fatal(err)
			}
		}, "not vendored (run `shipmind vendor ax`)"},
		{"bad lock", func(t *testing.T, root string, _ *bom.Component) {
			write(t, filepath.Join(root, Dir, "ax", LockFile), "source: x\nbogus: y\n")
		}, "VENDOR.yaml"},
		{"kustomization edited", func(t *testing.T, root string, _ *bom.Component) {
			write(t, filepath.Join(root, Dir, "ax", "b", KustomizationFile), "resources: []\n")
		}, "b/kustomization.yaml was edited after vendoring"},
		{"kustomization deleted", func(t *testing.T, root string, _ *bom.Component) {
			if err := os.Remove(filepath.Join(root, Dir, "ax", "b", KustomizationFile)); err != nil {
				t.Fatal(err)
			}
		}, "b/kustomization.yaml is missing"},
		{"no commit", func(t *testing.T, root string, _ *bom.Component) {
			p := filepath.Join(root, Dir, "ax", LockFile)
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			write(t, p, strings.Replace(string(b), "commit: "+fakeCommit, "commit: \"\"", 1))
		}, "has no resolved commit"},
		{"colliding layout", func(_ *testing.T, _ string, c *bom.Component) { c.Manifests = []string{"a.yaml", "a/a.yaml.yaml"} },
			"needs directory a/a.yaml"},
		{"manifests dropped", func(_ *testing.T, _ string, c *bom.Component) { c.Manifests = nil },
			"exists but versions.yaml lists no manifests"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, c := vendored(t)
			tc.mutate(t, root, &c)
			err := Verify(root, c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}

	// A component without manifests and without a directory is fine.
	if err := Verify(t.TempDir(), comp("v1.0.0")); err != nil {
		t.Errorf("Verify(no manifests): %v", err)
	}
}

func TestStrays(t *testing.T) {
	root, c := vendored(t)
	f := &bom.File{Components: []bom.Component{c}}
	if err := Strays(root, f); err != nil {
		t.Fatalf("Strays: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, Dir, "gone"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Strays(root, f); err == nil || !strings.Contains(err.Error(), `no component "gone"`) {
		t.Errorf("err = %v, want stray gone", err)
	}
	if err := Strays(t.TempDir(), f); err != nil {
		t.Errorf("Strays(no vendor dir): %v", err)
	}
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLayout(t *testing.T) {
	if err := Layout(comp("v1.0.0", "deploy/a.yaml", "deploy/b.yaml", "a/b.yaml", "a.yaml", "VENDOR.yaml")); err != nil {
		t.Errorf("Layout(fine) = %v", err)
	}
	for _, tc := range []struct {
		manifests []string
		want      string
	}{
		{[]string{"a.yaml", "a/a.yaml.yaml"}, "needs directory a/a.yaml"},
		{[]string{"a/a.yaml.yaml", "a.yaml"}, "a directory manifest"},
		{[]string{"VENDOR.yaml.yaml"}, "needs directory vendor.yaml, which is the lock's file"},
		{[]string{"x.yaml", "x/kustomization.yaml.yaml"}, "x/kustomization.yaml"},
		{[]string{"A.yaml", "a.yaml"}, `manifest "a.yaml" would be vendored to a/a.yaml, which manifest "A.yaml" already uses`},
	} {
		err := Layout(comp("v1.0.0", tc.manifests...))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Layout(%v) = %v, want %q", tc.manifests, err, tc.want)
		}
	}
}

// A vendor that fails partway leaves the previous tree untouched.
func TestVendorFailureKeepsOldTree(t *testing.T) {
	root, c := vendored(t)
	before, err := os.ReadFile(filepath.Join(root, Dir, "ax", LockFile))
	if err != nil {
		t.Fatal(err)
	}
	c.Manifests = []string{"a.yaml", "a/a.yaml.yaml"}
	f := fakeFetcher{files: map[string][]byte{"a.yaml": []byte("a"), "a/a.yaml.yaml": []byte("b")}}
	if err := Vendor(context.Background(), f, root, c); err == nil {
		t.Fatal("Vendor with a colliding layout succeeded")
	}
	after, err := os.ReadFile(filepath.Join(root, Dir, "ax", LockFile))
	if err != nil || string(after) != string(before) {
		t.Errorf("lock changed after a failed vendor: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, Dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("vendor dir has %d entries after a failed vendor, want 1 (no temp leftovers)", len(entries))
	}
}
