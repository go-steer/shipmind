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

// Package secrets reads values from Google Cloud Secret Manager, for
// Kubernetes Secrets an environment keeps there (shipmind.yaml `secrets`).
package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// Source reads a secret's latest version.
type Source interface {
	Access(ctx context.Context, project, name string) ([]byte, error)
}

// Gcloud reads with the gcloud CLI, as whoever it is logged in as.
type Gcloud struct {
	// Bin is the gcloud binary; empty means "gcloud".
	Bin string
}

var (
	projectRE = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	nameRE    = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,255}$`)
)

// Access implements Source. Its errors never carry the command's output,
// which on success is the secret.
func (g Gcloud) Access(ctx context.Context, project, name string) ([]byte, error) {
	if !projectRE.MatchString(project) || !nameRE.MatchString(name) {
		return nil, fmt.Errorf("secret manager: project %q, secret %q: not a project ID and secret name", project, name)
	}
	bin := g.Bin
	if bin == "" {
		bin = "gcloud"
	}
	cmd := exec.CommandContext(ctx, bin, "secrets", "versions", "access", "latest", "--secret="+name, "--project="+project) //nolint:gosec // validated above; no shell
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("secret manager: %s/%s: exit %d: %s", project, name, ee.ExitCode(), firstLine(errb.String()))
		}
		return nil, fmt.Errorf("secret manager: %s/%s: %w", project, name, err)
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("secret manager: %s/%s: empty", project, name)
	}
	return out.Bytes(), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
