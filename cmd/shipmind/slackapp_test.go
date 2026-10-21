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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/shipmind/internal/bom"
	"github.com/go-steer/shipmind/internal/manifests"
)

// manifestFetcher serves switchboard's real Slack app manifest, as this
// repository vendors it.
type manifestFetcher struct{ b []byte }

func (f manifestFetcher) Fetch(_ context.Context, _, _ string, paths []string) (map[string][]byte, string, error) {
	files := map[string][]byte{}
	for _, p := range paths {
		files[p] = f.b
	}
	return files, "0123456789abcdef0123456789abcdef01234567", nil
}

func TestSlackApp(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, envDir := deployRoot(t)
	slackApp := func(code int, want string, args ...string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		if got := run(append([]string{"slack-app"}, args...), &out, &errOut); got != code ||
			!strings.Contains(out.String()+errOut.String(), want) {
			t.Fatalf("slack-app %v: exit %d (want %d)\nstdout %s\nstderr %s\nwant %q", args, got, code, out.String(), errOut.String(), want)
		}
		return out.String()
	}

	slackApp(2, "want one environment directory", "-root", root)
	slackApp(2, "flag provided but not defined", "-bogus", envDir)
	slackApp(0, "", "-h")
	slackApp(1, "shipmind.yaml", "-root", root, t.TempDir())
	slackApp(1, "no slack block", "-root", root, envDir)
	envFile := filepath.Join(envDir, "shipmind.yaml")
	if err := os.WriteFile(envFile, append(readFile(t, envFile), []byte("slack:\n  app: demo-stage\n  command: /demo-stage\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	slackApp(1, "versions.yaml", "-root", root, "-any-ref", envDir)
	slackApp(1, "is not a kustomization", "-root", t.TempDir(), "-any-ref", envDir)

	c := bom.Component{Name: "switchboard", Source: "github.com/go-steer/switchboard", Version: "v0.8.0",
		Profiles: []string{"substrate"}, Files: []string{slackManifest}}
	versions := "components:\n  - name: switchboard\n    source: github.com/go-steer/switchboard\n    version: v0.8.0\n    profiles: [substrate]\n"
	if err := os.WriteFile(filepath.Join(root, "versions.yaml"), []byte(versions), 0o600); err != nil {
		t.Fatal(err)
	}
	slackApp(1, "vendors no switchboard deploy/slack/app-manifest.yaml", "-root", root, "-any-ref", envDir)

	if err := os.WriteFile(filepath.Join(root, "versions.yaml"), []byte(versions+"    files: [deploy/slack/app-manifest.yaml]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	real := readFile(t, filepath.Join("../..", manifests.Dir, "switchboard", slackManifest))
	if err := manifests.Vendor(context.Background(), manifestFetcher{real}, root, c); err != nil {
		t.Fatal(err)
	}
	// The checkout is no longer at the pinned commit with a clean tree.
	slackApp(1, "uncommitted changes", "-root", root, envDir)
	out := slackApp(0, "name: demo-stage", "-root", root, "-any-ref", envDir)
	for _, want := range []string{"# Slack app manifest for demo-stage (context demo)", "switchboard v0.8.0's deploy/slack/app-manifest.yaml",
		"display_name: demo-stage", "- command: /demo-stage", "Configure demo-stage for this channel", "socket_mode_enabled: true"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// An edited vendored copy is refused.
	p := filepath.Join(root, manifests.Dir, "switchboard", filepath.FromSlash(slackManifest))
	if err := os.WriteFile(p, append(real, []byte("# edited\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	slackApp(1, "edited after vendoring", "-root", root, "-any-ref", envDir)
}
