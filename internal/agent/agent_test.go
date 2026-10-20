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

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml11 "go.yaml.in/yaml/v2"
	"go.yaml.in/yaml/v3"

	"github.com/go-steer/shipmind/internal/env"
)

const root = "../.."

var tok = Tokens{Inject: strings.Repeat("a", 32), Attach: strings.Repeat("b", 32)}

func testEnv() *env.Env {
	return &env.Env{
		Context: "demo",
		Agent: env.Agent{
			Atespace:  "agents",
			Project:   "example-project",
			Workspace: "demo-ws",
			Image:     "registry.example.com/agent@sha256:" + strings.Repeat("0", 64),
			Clusters: []env.Cluster{
				{Name: "demo", Path: "projects/example-project/locations/us-central1/clusters/demo"},
				{Name: "prod", Path: "projects/example-project/locations/europe-west1/clusters/prod"},
			},
		},
	}
}

func TestTask(t *testing.T) {
	b, err := Task(root, testEnv(), tok)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	var task struct {
		Metadata struct{ Atespace string } `yaml:"metadata"`
		Spec     struct {
			Image      string
			Command    []string
			Env        []struct{ Name, Value string }
			Workspaces []struct{ Name, Path string }
			HTTP       struct{ Port int } `yaml:"http"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(b, &task); err != nil {
		t.Fatalf("rendered task is not YAML: %v\n%s", err, b)
	}
	envs := map[string]string{}
	for _, e := range task.Spec.Env {
		envs[e.Name] = e.Value
	}
	switch {
	case task.Metadata.Atespace != "agents":
		t.Errorf("atespace %q", task.Metadata.Atespace)
	case task.Spec.Image != testEnv().Agent.Image:
		t.Errorf("image %q", task.Spec.Image)
	case envs["GOOGLE_CLOUD_PROJECT"] != "example-project":
		t.Errorf("project %q", envs["GOOGLE_CLOUD_PROJECT"])
	case envs["MAST_INJECT_TOKEN"] != tok.Inject || envs["MAST_ATTACH_TOKEN"] != tok.Attach:
		t.Errorf("tokens %v", envs)
	case len(task.Spec.Workspaces) != 1 || task.Spec.Workspaces[0].Name != "demo-ws":
		t.Errorf("workspaces %v", task.Spec.Workspaces)
	case task.Spec.HTTP.Port != 8484:
		t.Errorf("http.port %d", task.Spec.HTTP.Port)
	}

	if _, err := Task(root, testEnv(), Tokens{Inject: "x", Attach: tok.Attach}); err == nil {
		t.Error("Task with a bad token succeeded")
	}
}

func TestAgentsMD(t *testing.T) {
	b, err := AgentsMD(root, testEnv())
	if err != nil {
		t.Fatalf("AgentsMD: %v", err)
	}
	want := "| cluster | full name |\n|---|---|\n" +
		"| demo | projects/example-project/locations/us-central1/clusters/demo |\n" +
		"| prod | projects/example-project/locations/europe-west1/clusters/prod |\n" +
		"\nIf the cluster is neither"
	if !strings.Contains(string(b), want) {
		t.Errorf("AGENTS.md lacks the cluster table:\n%s", b)
	}
	if !strings.HasPrefix(string(b), "# Clusters\n") || !strings.Contains(string(b), "# Repeated and related signals") {
		t.Errorf("AGENTS.md lacks its sections:\n%s", b)
	}
}

func TestWorkspace(t *testing.T) {
	b, err := Workspace(root, testEnv())
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	var ws struct {
		Metadata struct{ Name, Atespace string } `yaml:"metadata"`
		Spec     struct {
			Files []struct{ Path, Content string }
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(b, &ws); err != nil {
		t.Fatalf("rendered workspace is not YAML: %v\n%s", err, b)
	}
	md, err := AgentsMD(root, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if ws.Metadata.Name != "demo-ws" || ws.Metadata.Atespace != "agents" || len(ws.Spec.Files) != 1 ||
		ws.Spec.Files[0].Path != "AGENTS.md" || ws.Spec.Files[0].Content != string(md) {
		t.Errorf("workspace = %+v", ws)
	}
}

func TestTokens(t *testing.T) {
	if err := tok.Check(); err != nil {
		t.Errorf("Check(good) = %v", err)
	}
	for _, bad := range []Tokens{
		{Inject: "short", Attach: tok.Attach},
		{Inject: tok.Inject, Attach: strings.Repeat("B", 32)},
		{Inject: tok.Inject, Attach: tok.Inject},
	} {
		if err := bad.Check(); err == nil {
			t.Errorf("Check(%+v) succeeded", bad)
		}
	}
	a, err := NewTokens()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewTokens()
	if err != nil {
		t.Fatal(err)
	}
	if a.Check() != nil || b.Check() != nil || a == b {
		t.Errorf("NewTokens = %+v, %+v", a, b)
	}
}

func TestTemplateErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := AgentsMD(dir, testEnv()); err == nil {
		t.Error("AgentsMD with no templates succeeded")
	}
	if err := os.MkdirAll(filepath.Join(dir, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"AGENTS.md.tmpl":      "{{ .Clusters",
		"task.yaml.tmpl":      "{{ .Missing }}",
		"workspace.yaml.tmpl": "x",
	} {
		if err := os.WriteFile(filepath.Join(dir, Dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := AgentsMD(dir, testEnv()); err == nil {
		t.Error("AgentsMD with a broken template succeeded")
	}
	if _, err := Workspace(dir, testEnv()); err == nil {
		t.Error("Workspace with a broken AGENTS.md template succeeded")
	}
	if _, err := Task(dir, testEnv(), tok); err == nil || !strings.Contains(err.Error(), "Missing") {
		t.Errorf("Task with a missing key: err = %v", err)
	}
}

// lookout parses the task template as YAML 1.1 (sigs.k8s.io/yaml), where
// a bare on, no or y is a boolean. Every value is quoted, so a valid
// atespace or workspace named "on" stays a string.
func TestTemplatesSurviveYAML11(t *testing.T) {
	e := testEnv()
	e.Agent.Atespace, e.Agent.Workspace = "on", "no"
	task, err := Task(root, e, tok)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := Workspace(root, e)
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"task": task, "workspace": ws} {
		var doc map[string]any
		if err := yaml11.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		md, _ := doc["metadata"].(map[any]any)
		if v, ok := md["atespace"].(string); !ok || v != "on" {
			t.Errorf("%s: atespace = %#v, want the string \"on\"", name, md["atespace"])
		}
	}
}
