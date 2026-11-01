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

package secrets

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeGcloud(t *testing.T, script string) Gcloud {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gcloud")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o700); err != nil { //nolint:gosec // test helper must be executable
		t.Fatal(err)
	}
	return Gcloud{Bin: p}
}

func TestAccess(t *testing.T) {
	ctx := context.Background()
	g := fakeGcloud(t, `echo "$*" > "$(dirname "$0")/args"; printf 'xoxb-secret'`)
	v, err := g.Access(ctx, "example-project", "shipmind-stage-slack-bot-token")
	if err != nil || string(v) != "xoxb-secret" {
		t.Fatalf("Access = %q, %v", v, err)
	}
	args, _ := os.ReadFile(filepath.Join(filepath.Dir(g.Bin), "args"))
	if strings.TrimSpace(string(args)) != "secrets versions access latest --secret=shipmind-stage-slack-bot-token --project=example-project" {
		t.Errorf("args %q", args)
	}

	// A failure names the secret, not what gcloud printed on stdout.
	g = fakeGcloud(t, `printf 'xoxb-leak'; echo "PERMISSION_DENIED: no access" >&2; echo more >&2; exit 1`)
	if _, err := g.Access(ctx, "example-project", "s"); err == nil || strings.Contains(err.Error(), "xoxb-leak") ||
		!strings.Contains(err.Error(), "PERMISSION_DENIED: no access") || strings.Contains(err.Error(), "more") {
		t.Errorf("failure: %v", err)
	}
	g = fakeGcloud(t, `exit 0`)
	if _, err := g.Access(ctx, "example-project", "s"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty: %v", err)
	}
	if _, err := g.Access(ctx, "X", "s"); err == nil {
		t.Error("bad project accepted")
	}
	if _, err := g.Access(ctx, "example-project", "projects/x/secrets/y"); err == nil {
		t.Error("bad name accepted")
	}
	if _, err := (Gcloud{Bin: filepath.Join(t.TempDir(), "missing")}).Access(ctx, "example-project", "s"); err == nil {
		t.Error("missing binary accepted")
	}
}
