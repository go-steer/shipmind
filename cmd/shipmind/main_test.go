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

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/shipmind/internal/cluster"
	"github.com/go-steer/shipmind/internal/manifests"
)

func write(t *testing.T, doc string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "versions.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const good = `components:
  - name: mast
    source: github.com/go-steer/mast
    version: 922e5491ba10562ddd68285e318c3efddf074d59
    profiles: [substrate]
`

func TestRun(t *testing.T) {
	goodPath := write(t, good)
	badPath := write(t, "components:\n  - {name: a, source: github.com/a/b, version: v1.0.0, profiles: [substrate],\n"+
		"     images: [{name: redis, ref: 'redis:7-alpine'}]}\n")

	for _, tc := range []struct {
		name     string
		args     []string
		code     int
		stdout   string
		stderr   string
		noStdout bool
		noStderr bool
	}{
		{name: "no args", code: 2, stderr: "usage:"},
		{name: "help", args: []string{"help"}, code: 0, stdout: "commands:"},
		{name: "unknown", args: []string{"launch"}, code: 2, stderr: `unknown command "launch"`},
		{name: "bad flag", args: []string{"check", "-x"}, code: 2, stderr: "flag provided but not defined"},
		{name: "flag help", args: []string{"check", "-h"}, code: 0, stderr: "path to versions.yaml"},
		{name: "extra arg", args: []string{"check", "-f", goodPath, "more"}, code: 2, stderr: `unexpected argument "more"`},
		{name: "missing file", args: []string{"check", "-f", filepath.Join(t.TempDir(), "nope.yaml")}, code: 1, stderr: "no such file"},
		{name: "check good", args: []string{"check", "-f", goodPath}, code: 0, stdout: "1 components, all pinned", noStderr: true},
		{name: "check bad", args: []string{"check", "-f", badPath}, code: 1, stderr: "pinned by digest", noStdout: true},
		{name: "versions bad", args: []string{"versions", "-f", badPath}, code: 1, noStdout: true},
		{name: "versions", args: []string{"versions", "-f", goodPath}, code: 0, stdout: "mast       922e5491ba10562ddd68285e318c3efddf074d59  github.com/go-steer/mast  substrate  0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(tc.args, &out, &errOut); code != tc.code {
				t.Errorf("exit %d, want %d (stderr %q)", code, tc.code, errOut.String())
			}
			if !strings.Contains(out.String(), tc.stdout) {
				t.Errorf("stdout %q, want %q", out.String(), tc.stdout)
			}
			if !strings.Contains(errOut.String(), tc.stderr) {
				t.Errorf("stderr %q, want %q", errOut.String(), tc.stderr)
			}
			if tc.noStdout && out.Len() > 0 {
				t.Errorf("unexpected stdout %q", out.String())
			}
			if tc.noStderr && errOut.Len() > 0 {
				t.Errorf("unexpected stderr %q", errOut.String())
			}
		})
	}
}

type fakeFetcher struct{ err error }

func (f fakeFetcher) Fetch(_ context.Context, _, _ string, paths []string) (map[string][]byte, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	files := map[string][]byte{}
	for _, p := range paths {
		files[p] = []byte("kind: " + p + "\n")
	}
	return files, "0123456789abcdef0123456789abcdef01234567", nil
}

func TestVendorAndCheck(t *testing.T) {
	old := fetcher
	t.Cleanup(func() { fetcher = old })
	fetcher = fakeFetcher{}

	doc := good + `  - name: ax
    source: github.com/example/ax
    version: v1.0.0
    profiles: [substrate]
    manifests: [deploy/ax.yaml]
`
	path := write(t, doc)
	root := filepath.Dir(path)
	runOK := func(args ...string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 0 {
			t.Fatalf("run %v: exit %d: %s", args, code, errOut.String())
		}
		return out.String()
	}
	runFail := func(want string, args ...string) {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), want) {
			t.Errorf("run %v: exit %d, stderr %q, want exit 1 and %q", args, code, errOut.String(), want)
		}
	}

	runFail("ax: not vendored", "check", "-f", path)
	if out := runOK("vendor", "-f", path); out != "vendored ax@v1.0.0: 1 files\n" {
		t.Errorf("vendor output %q", out)
	}
	if out := runOK("check", "-f", path); !strings.Contains(out, "2 components, all pinned; 1 vendored, all unedited") {
		t.Errorf("check output %q", out)
	}
	if out := runOK("vendor", "-f", path, "ax"); !strings.Contains(out, "vendored ax") {
		t.Errorf("vendor ax output %q", out)
	}

	edited := filepath.Join(root, manifests.Dir, "ax", "deploy", "ax", "ax.yaml")
	if err := os.WriteFile(edited, []byte("kind: Edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runFail("deploy/ax/ax.yaml was edited after vendoring", "check", "-f", path)

	if err := os.MkdirAll(filepath.Join(root, manifests.Dir, "gone"), 0o755); err != nil {
		t.Fatal(err)
	}
	runFail(`no component "gone"`, "check", "-f", path)

	runFail(`no component "nope"`, "vendor", "-f", path, "nope")
	runFail("no manifests to vendor", "vendor", "-f", path, "mast")
	fetcher = fakeFetcher{err: errors.New("network down")}
	runFail("network down", "vendor", "-f", path, "ax")
}

// deployFake is a cluster with one drifted object and nothing else.
type deployFake struct{ failDiff bool }

func (f deployFake) Diff(context.Context, []byte) ([]string, error) {
	if f.failDiff {
		return nil, errors.New("diff failed")
	}
	return []string{"v1.ConfigMap.demo.base"}, nil
}
func (deployFake) Apply(context.Context, []byte, bool) (string, error) { return "applied\n", nil }
func (deployFake) Delete(context.Context, []byte) (string, error)      { return "deleted\n", nil }
func (deployFake) Secret(context.Context, string, string) (map[string][]byte, bool, error) {
	return nil, false, nil
}
func (deployFake) RolloutStatus(context.Context, string, string) error  { return nil }
func (deployFake) RolloutRestart(context.Context, string, string) error { return nil }
func (deployFake) ApplySecret(context.Context, []byte, bool) error      { return nil }
func (deployFake) SecretUpdated(context.Context, string, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}
func (deployFake) PodStarts(context.Context, string, string) ([]time.Time, error) { return nil, nil }
func (deployFake) WorkloadImages(context.Context, string) (map[string][]string, error) {
	return nil, nil
}
func (deployFake) AxWorkspace(context.Context, string, string) ([]byte, bool, error) {
	return nil, false, nil
}
func (deployFake) AxApply(context.Context, string, []byte) error { return nil }

// deployRoot makes a committed shipmind checkout with one profile and the
// real agent templates, and an environment that pins it.
func deployRoot(t *testing.T) (root, envDir string) {
	t.Helper()
	root = t.TempDir()
	put := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put(filepath.Join(root, "deploy/profiles/p/kustomization.yaml"), "resources:\n  - cm.yaml\n")
	put(filepath.Join(root, "deploy/profiles/p/cm.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: base\n  namespace: demo\n")
	for _, name := range []string{"AGENTS.md.tmpl", "task.yaml.tmpl", "workspace.yaml.tmpl"} {
		b, err := os.ReadFile(filepath.Join("../../agents/sre-triage/ax", name))
		if err != nil {
			t.Fatal(err)
		}
		put(filepath.Join(root, "agents/sre-triage/ax", name), string(b))
	}
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
	head := git("rev-parse", "HEAD")

	envDir = t.TempDir()
	put(filepath.Join(envDir, "kustomization.yaml"), "resources:\n  - https://github.com/go-steer/shipmind//deploy/profiles/p?ref="+head+"\n")
	put(filepath.Join(envDir, "shipmind.yaml"), `context: demo
agent:
  atespace: agents
  project: example-project
  workspace: demo-ws
  image: registry.example.com/agent@sha256:`+strings.Repeat("0", 64)+`
  clusters:
    - name: demo
      path: projects/example-project/locations/us-central1/clusters/demo
`)
	return root, envDir
}

func TestDeployCommands(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	old := newCluster
	t.Cleanup(func() { newCluster = old })
	fake := deployFake{}
	newCluster = func(string) cluster.Cluster { return fake }
	root, envDir := deployRoot(t)

	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{"status drift", []string{"status", "-root", root, envDir}, exitDrift, "DRIFT     v1.ConfigMap.demo.base", ""},
		{"up dry run", []string{"up", "-root", root, "-dry-run", envDir}, 0, "workspace agents/demo-ws would be applied (dry run)", ""},
		{"up", []string{"up", "-root", root, envDir}, 0, "workspace agents/demo-ws applied", ""},
		{"down", []string{"down", "-root", root, "-yes", envDir}, 0, "deleted", ""},
		{"down without yes", []string{"down", "-root", root, envDir}, 2, "", "pass -yes"},
		{"any-ref on up", []string{"up", "-root", root, "-any-ref", envDir}, 2, "", "status only"},
		{"no env dir", []string{"status", "-root", root}, 2, "", "want one environment directory"},
		{"bad flag", []string{"status", "-bogus", envDir}, 2, "", "flag provided but not defined"},
		{"help", []string{"up", "-h"}, 0, "", "-dry-run"},
		{"no env file", []string{"status", "-root", root, t.TempDir()}, 1, "", "shipmind.yaml"},
		{"wrong root", []string{"status", "-root", t.TempDir(), envDir}, 1, "", "is not a kustomization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(tc.args, &out, &errOut); code != tc.code {
				t.Errorf("exit %d, want %d\nstdout %s\nstderr %s", code, tc.code, out.String(), errOut.String())
			}
			if !strings.Contains(out.String(), tc.stdout) || !strings.Contains(errOut.String(), tc.stderr) {
				t.Errorf("stdout %q (want %q)\nstderr %q (want %q)", out.String(), tc.stdout, errOut.String(), tc.stderr)
			}
		})
	}

	// With a substrate section, the deploy commands read Substrate's
	// third-party pins from the checkout's versions.yaml.
	if err := os.WriteFile(filepath.Join(envDir, "shipmind.yaml"), append(readFile(t, filepath.Join(envDir, "shipmind.yaml")),
		[]byte("substrate:\n  images: r.example.com/substrate\n  tag: v1\n  namespaces: [ate-system]\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	var out0, err0 bytes.Buffer
	if code := run([]string{"status", "-root", root, "-any-ref", envDir}, &out0, &err0); code != 1 || !strings.Contains(err0.String(), "versions.yaml") {
		t.Errorf("substrate without versions.yaml: exit %d, stderr %q", code, err0.String())
	}
	if err := os.WriteFile(filepath.Join(root, "versions.yaml"), []byte("components:\n  - name: substrate\n    source: github.com/a/substrate\n    version: v1.0.0\n    profiles: [substrate]\n    images:\n      - name: postgres\n        ref: docker.io/library/postgres:1@sha256:"+strings.Repeat("1", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out0.Reset()
	err0.Reset()
	if code := run([]string{"status", "-root", root, "-any-ref", envDir}, &out0, &err0); code != exitDrift || !strings.Contains(out0.String(), "substrate ate-system: no workloads") {
		t.Errorf("substrate status: exit %d\nstdout %s\nstderr %s", code, out0.String(), err0.String())
	}

	fake.failDiff = true
	newCluster = func(string) cluster.Cluster { return fake }
	var out, errOut bytes.Buffer
	if code := run([]string{"status", "-root", root, "-any-ref", envDir}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "diff failed") {
		t.Errorf("status with a failing cluster: exit %d, stderr %q", code, errOut.String())
	}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
