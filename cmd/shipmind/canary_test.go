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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/shipmind/internal/cluster"
	"github.com/go-steer/shipmind/internal/scenario"
)

// canaryFake is a deployed cluster: it holds the agent's tokens and
// answers a Job's UID once one has been applied.
type canaryFake struct {
	deployFake
	noTokens, noUID bool
	applied         *[]string
}

func (f canaryFake) Secret(context.Context, string, string) (map[string][]byte, bool, error) {
	if f.noTokens {
		return nil, false, nil
	}
	return map[string][]byte{"inject": []byte(strings.Repeat("a", 32)), "attach": []byte(strings.Repeat("b", 32))}, true, nil
}

func (f canaryFake) Apply(_ context.Context, m []byte, _ bool) (string, error) {
	*f.applied = append(*f.applied, string(m))
	return "applied\n", nil
}

func (f canaryFake) Get(context.Context, string, string, string) ([]byte, bool, error) {
	if f.noUID {
		return nil, false, nil
	}
	return []byte(`{"metadata":{"uid":"uid-1"}}`), true, nil
}

func canaryEnvDir(t *testing.T, chatImage bool) string {
	t.Helper()
	dir := t.TempDir()
	body := `context: demo
agent:
  atespace: agents
  project: example-project
  workspace: demo-ws
  image: registry.example.com/agent@sha256:` + strings.Repeat("0", 64) + `
  clusters:
    - name: demo
      path: projects/example-project/locations/us-central1/clusters/demo
`
	if chatImage {
		body += "  chatImage: registry.example.com/agent-core@sha256:" + strings.Repeat("1", 64) + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "shipmind.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestScenarioCanaryCommand(t *testing.T) {
	old := newCluster
	t.Cleanup(func() { newCluster = old })
	var applied []string
	fake := canaryFake{applied: &applied}
	newCluster = func(string) cluster.Cluster { return fake }

	const root = "../.."
	image := "registry.example.com/agent@sha256:" + strings.Repeat("c", 64)
	runner := "registry.example.com/e2e-runner@sha256:" + strings.Repeat("d", 64)
	envDir, mastDir := canaryEnvDir(t, true), canaryEnvDir(t, false)
	base := []string{"scenario", "canary", "-root", root, "-image", image}

	for _, tc := range []struct {
		name   string
		args   []string
		fake   func(*canaryFake)
		code   int
		stdout string
		stderr string
	}{
		{"no arguments", []string{"scenario", "canary"}, nil, 2, "", "want -image, an environment directory and a scenario name"},
		{"no image", []string{"scenario", "canary", envDir, "posture-audit"}, nil, 2, "", "want -image"},
		{"bad flag", []string{"scenario", "canary", "-bogus"}, nil, 2, "", "flag provided but not defined"},
		{"help", []string{"scenario", "canary", "-h"}, nil, 0, "", "-runner-image"},
		{"no such scenario", append(base[:len(base):len(base)], envDir, "nope"), nil, 1, "", "scenario.yaml"},
		{"a scenario path", append(base[:len(base):len(base)], envDir, "../x"), nil, 2, "", "want a scenario's name"},
		{"an incident scenario", append(base[:len(base):len(base)], envDir, "missing-env"), nil, 2, "", "missing-env is not a chat scenario"},
		{"no environment", append(base[:len(base):len(base)], t.TempDir(), "posture-audit"), nil, 1, "", "shipmind.yaml"},
		{"a mast environment", append(base[:len(base):len(base)], mastDir, "posture-audit"), nil, 1, "", "sets no agent.chatImage"},
		{"no versions.yaml", []string{"scenario", "canary", "-root", t.TempDir(), "-image", image, envDir, "posture-audit"}, nil, 1, "", "scenario.yaml"},
		{"dry run", append(base[:len(base):len(base)], "-runner-image", runner, "-dry-run", envDir, "posture-audit"), nil, 0,
			"kind: Job", "dry run: canary-posture-audit-"},
		{"dry run archive", append(base[:len(base):len(base)], "-runner-image", runner, "-archive", "gs://runs/stage", "-dry-run", envDir, "posture-audit"), nil, 0,
			"-archive=gs://runs/stage", "dry run: canary-posture-audit-"},
		{"bad archive", append(base[:len(base):len(base)], "-runner-image", runner, "-archive", "runs/stage", "-dry-run", envDir, "posture-audit"), nil, 1,
			"", "want gs://bucket/prefix"},
		// With no -runner-image, versions.yaml's e2e-runner pin runs it.
		{"pinned runner", append(base[:len(base):len(base)], "-dry-run", envDir, "posture-audit"), nil, 0,
			"ghcr.io/go-steer/shipmind/e2e-runner:", "nothing applied"},
		{"unknown cluster", append(base[:len(base):len(base)], "-runner-image", runner, "-cluster", "prod", envDir, "posture-audit"), nil, 1, "", `cluster "prod" is not in the environment's agent.clusters`},
		{"no tokens", append(base[:len(base):len(base)], "-runner-image", runner, envDir, "posture-audit"), func(f *canaryFake) { f.noTokens = true }, 1, "", "the cluster holds no agent tokens"},
		{"starts", append(base[:len(base):len(base)], "-runner-image", runner, envDir, "posture-audit"), nil, 0,
			"-a agents delete task canary-posture-audit-", ""},
		{"starts with archive", append(base[:len(base):len(base)], "-runner-image", runner, "-archive", "gs://runs/stage", envDir, "posture-audit"), nil, 0,
			"archive: gs://runs/stage/posture-audit/", ""},
		// Started, but its Secret isn't owned by the Job: say how to
		// follow it and how to remove it, and exit non-zero.
		{"starts unowned", append(base[:len(base):len(base)], "-runner-image", runner, envDir, "posture-audit"), func(f *canaryFake) { f.noUID = true }, 1,
			"delete job,secret -l shipmind.dev/canary=canary-posture-audit-", "will outlive it; delete it by hand"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := canaryFake{applied: &applied}
			if tc.fake != nil {
				tc.fake(&f)
			}
			newCluster = func(string) cluster.Cluster { return f }
			applied = nil
			var out, errOut bytes.Buffer
			if code := run(tc.args, &out, &errOut); code != tc.code {
				t.Errorf("exit %d, want %d\nstdout %s\nstderr %s", code, tc.code, out.String(), errOut.String())
			}
			if !strings.Contains(out.String(), tc.stdout) || !strings.Contains(errOut.String(), tc.stderr) {
				t.Errorf("stdout %q (want %q)\nstderr %q (want %q)", out.String(), tc.stdout, errOut.String(), tc.stderr)
			}
			if strings.HasPrefix(tc.name, "starts") != (len(applied) == 1) {
				t.Errorf("applied %d jobs", len(applied))
			}
		})
	}
}

// A chat scenario runs through `scenario run` with the task it is given;
// an incident scenario refuses one.
func TestScenarioRunChat(t *testing.T) {
	oldK, oldT, oldA := newScenarioKube, newChatTasks, newChatAgent
	t.Cleanup(func() { newScenarioKube, newChatTasks, newChatAgent = oldK, oldT, oldA })
	fake := scenarioFake{token: "tok"}
	newScenarioKube = func(string) scenarioKube { return fake }
	var applied []string
	newChatTasks = func(scenario.Ax) scenario.ChatTasks { return chatTasksFake{applied: &applied} }
	var gotRouter *scenario.Router
	newChatAgent = func(r *scenario.Router) scenario.ChatAgent { gotRouter = r; return r }

	dir := t.TempDir()
	put := func(rel, body string) string {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Near-zero limits: the unreachable router fails the run at once.
	put("quick-chat/scenario.yaml", "chat:\n  ask: review {{.Cluster}}\nexpect:\n  mentions: [X]\nlimits:\n  start: 1ns\n  diagnose: 1ns\n")
	put("incident/scenario.yaml", "namespace: lab\nworkload: w.yaml\nexpect:\n  mentions: [X]\nlimits:\n  incident: 1ns\n  diagnose: 1ns\n  suspend: 1ns\n")
	put("incident/w.yaml", "kind: ConfigMap\nmetadata:\n  name: {{.Name}}\n")
	task := put("task.yaml", "kind: Task\nmetadata:\n  name: canary-1\n")
	ws := put("workspace.yaml", "kind: Workspace\nmetadata:\n  name: canary-1\n")
	chat, incident := filepath.Join(dir, "quick-chat"), filepath.Join(dir, "incident")
	const cl = "projects/example-project/locations/us-central1/clusters/demo"
	conn := []string{"scenario", "run", "-atespace", "a", "-context", "c", "-router", "http://127.0.0.1:1"}

	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{"no task", append(conn[:len(conn):len(conn)], "-cluster", cl, chat), 2, "", "is a chat scenario: it wants -task and -cluster"},
		{"no cluster", append(conn[:len(conn):len(conn)], "-task", task, chat), 2, "", "it wants -task and -cluster"},
		{"missing task file", append(conn[:len(conn):len(conn)], "-task", filepath.Join(dir, "nope.yaml"), "-cluster", cl, chat), 2, "", "nope.yaml"},
		{"missing workspace file", append(conn[:len(conn):len(conn)], "-task", task, "-workspace", filepath.Join(dir, "nope.yaml"), "-cluster", cl, chat), 2, "", "nope.yaml"},
		{"chat flags on an incident scenario", append(conn[:len(conn):len(conn)], "-task", task, incident), 2, "", "are for a chat scenario; incident is not one"},
		{"bad cluster", append(conn[:len(conn):len(conn)], "-task", task, "-cluster", "demo", chat), 1, "", "want projects/<project>/locations/<location>/clusters/<name>"},
		{"runs", append(conn[:len(conn):len(conn)], "-task", task, "-workspace", ws, "-cluster", cl, chat), 1,
			`"cluster": "` + cl + `"`, "scenario quick-chat: applied task canary-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			applied = nil
			var out, errOut bytes.Buffer
			if code := run(tc.args, &out, &errOut); code != tc.code {
				t.Errorf("exit %d, want %d\nstdout %s\nstderr %s", code, tc.code, out.String(), errOut.String())
			}
			if !strings.Contains(out.String(), tc.stdout) || !strings.Contains(errOut.String(), tc.stderr) {
				t.Errorf("stdout %q (want %q)\nstderr %q (want %q)", out.String(), tc.stdout, errOut.String(), tc.stderr)
			}
			if tc.name == "runs" {
				var r scenario.Result
				if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.Pass || r.Task != "canary-1" || !strings.Contains(strings.Join(r.Failures, "\n"), "accepted no session") {
					t.Errorf("result %+v, %v", r, err)
				}
				// Applied, then removed: the Workspace, the task.
				if want := "apply Workspace,apply Task,delete task canary-1,delete workspace canary-1"; strings.Join(applied, ",") != want {
					t.Errorf("ax saw %v, want %s", applied, want)
				}
			}
		})
	}
	if gotRouter == nil || gotRouter.Token != "tok" || gotRouter.Atespace != "a" {
		t.Errorf("router = %+v", gotRouter)
	}
}

type chatTasksFake struct{ applied *[]string }

func (f chatTasksFake) Apply(_ context.Context, m []byte) error {
	kind := "Task"
	if strings.Contains(string(m), "kind: Workspace") {
		kind = "Workspace"
	}
	*f.applied = append(*f.applied, "apply "+kind)
	return nil
}

func (f chatTasksFake) DeleteTask(_ context.Context, name string) error {
	*f.applied = append(*f.applied, "delete task "+name)
	return nil
}

func (f chatTasksFake) DeleteWorkspace(_ context.Context, name string) error {
	*f.applied = append(*f.applied, "delete workspace "+name)
	return nil
}

// The real ax CLI and router are what a chat run uses.
var (
	_ scenario.ChatTasks = scenario.Ax{}
	_ scenario.ChatAgent = (*scenario.Router)(nil)
)
