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
	"os"
	"path/filepath"
	"strings"
	"testing"
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
