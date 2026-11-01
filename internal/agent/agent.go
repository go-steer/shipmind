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

// Package agent renders the SRE agent's ax objects for an environment:
// the task template lookout (and switchboard) stamp a task from, the
// Workspace every task mounts, and switchboard's configuration. The templates live in
// agents/sre-triage/ax/ in a shipmind checkout.
package agent

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"text/template"

	"github.com/go-steer/shipmind/internal/env"
)

// Dir holds the templates, relative to a shipmind checkout.
const Dir = "agents/sre-triage/ax"

// Tokens are the agent's two bearer tokens: inject (POST /inject, how
// signals arrive) and attach (the session API lookout and switchboard
// speak).
type Tokens struct {
	Inject string
	Attach string
}

var tokenRE = regexp.MustCompile(`^[0-9a-f]{32,128}$`)

// Check reports whether both tokens are present and well formed.
func (t Tokens) Check() error {
	if !tokenRE.MatchString(t.Inject) {
		return fmt.Errorf("inject token: want 32 to 128 lowercase hex characters")
	}
	if !tokenRE.MatchString(t.Attach) {
		return fmt.Errorf("attach token: want 32 to 128 lowercase hex characters")
	}
	if t.Inject == t.Attach {
		return fmt.Errorf("inject and attach tokens are the same")
	}
	return nil
}

// NewTokens returns two fresh random tokens.
func NewTokens() (Tokens, error) {
	gen := func() (string, error) {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		return hex.EncodeToString(b), nil
	}
	inject, err := gen()
	if err != nil {
		return Tokens{}, err
	}
	attach, err := gen()
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{Inject: inject, Attach: attach}, nil
}

// The bundles the agent image carries (images/agent-mast) that a task
// runs: one incident per task for lookout, a conversation per task for
// switchboard. Budgets differ most: mast meters a session for its life,
// and a conversation is one long session.
const (
	IncidentBundle = "gke-triage-f"
	ChatBundle     = "gke-chat"
)

// Task renders the ax task template lookout stamps an incident task
// from, for e with tokens t.
func Task(root string, e *env.Env, t Tokens) ([]byte, error) {
	return task(root, e, t, IncidentBundle)
}

// ChatTask renders the task template switchboard starts a thread's task
// from: the same agent, image and tokens, with the conversation bundle.
func ChatTask(root string, e *env.Env, t Tokens) ([]byte, error) {
	return task(root, e, t, ChatBundle)
}

func task(root string, e *env.Env, t Tokens, bundle string) ([]byte, error) {
	if err := t.Check(); err != nil {
		return nil, err
	}
	return render(root, "task.yaml.tmpl", map[string]string{
		"Bundle":      bundle,
		"Atespace":    e.Agent.Atespace,
		"Image":       e.Agent.Image,
		"Project":     e.Agent.Project,
		"Workspace":   e.Agent.Workspace,
		"InjectToken": t.Inject,
		"AttachToken": t.Attach,
	})
}

// SwitchboardConfig renders switchboard's configuration for e: its Slack
// gateway fronting the agent as an ax agent, a task per thread, started
// from the task template at /etc/switchboard/triage.yaml. switchboard
// takes no variables in its configuration, so the environment's values
// are rendered in, as for the task template.
func SwitchboardConfig(root string, e *env.Env) ([]byte, error) {
	b, err := render(root, "switchboard.json.tmpl", map[string]string{"Atespace": e.Agent.Atespace})
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("%s: not JSON once rendered", filepath.Join(root, Dir, "switchboard.json.tmpl"))
	}
	return b, nil
}

// AgentsMD renders the Workspace's AGENTS.md for e.
func AgentsMD(root string, e *env.Env) ([]byte, error) {
	return render(root, "AGENTS.md.tmpl", map[string]any{"Clusters": e.Agent.Clusters})
}

// Workspace renders the ax Workspace for e.
func Workspace(root string, e *env.Env) ([]byte, error) {
	md, err := AgentsMD(root, e)
	if err != nil {
		return nil, err
	}
	return render(root, "workspace.yaml.tmpl", map[string]string{
		"Atespace":  e.Agent.Atespace,
		"Workspace": e.Agent.Workspace,
		"AgentsMD":  string(md),
	})
}

func render(root, name string, data any) ([]byte, error) {
	p := filepath.Join(root, Dir, name)
	src, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	t, err := template.New(name).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return b.Bytes(), nil
}
