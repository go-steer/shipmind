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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/shipmind/internal/scenario"
)

// scenarioFake answers like a platform that never opens an incident.
type scenarioFake struct {
	token  string
	secret error
}

func (f scenarioFake) Apply(context.Context, []byte, bool) (string, error) { return "", nil }
func (f scenarioFake) Delete(context.Context, []byte) (string, error)      { return "", nil }
func (f scenarioFake) Secret(context.Context, string, string) (map[string][]byte, bool, error) {
	if f.secret != nil {
		return nil, false, f.secret
	}
	return map[string][]byte{"token": []byte(f.token)}, f.token != "", nil
}
func (f scenarioFake) Tasks(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (f scenarioFake) DeleteTask(context.Context, string) error { return nil }

func TestScenarioCommand(t *testing.T) {
	oldK, oldT, oldA := newScenarioKube, newScenarioTasks, newScenarioAgent
	t.Cleanup(func() { newScenarioKube, newScenarioTasks, newScenarioAgent = oldK, oldT, oldA })
	fake := scenarioFake{token: "tok"}
	var gotRouter *scenario.Router
	newScenarioKube = func(string) scenarioKube { return fake }
	newScenarioTasks = func(scenario.Ax) scenario.Tasks { return fake }
	newScenarioAgent = func(r *scenario.Router) scenario.Agent { gotRouter = r; return r }

	// A scenario with near-zero limits, so a run times out at once.
	dir := filepath.Join(t.TempDir(), "quick")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"scenario.yaml": "namespace: lab\nworkload: w.yaml\nexpect:\n  mentions: [X]\nlimits:\n  incident: 1ns\n  diagnose: 1ns\n  suspend: 1ns\n",
		"w.yaml":        "kind: ConfigMap\nmetadata:\n  name: {{.Name}}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	jsonOut := filepath.Join(t.TempDir(), "r.json")

	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{"usage", []string{"scenario"}, 2, "", "usage: shipmind scenario run"},
		{"no flags", []string{"scenario", "run", dir}, 2, "", "want -atespace"},
		{"both modes", []string{"scenario", "run", "-atespace", "a", "-context", "c", "-in-cluster", dir}, 2, "", "exactly one of"},
		{"no router", []string{"scenario", "run", "-atespace", "a", "-context", "c", dir}, 2, "", "-router is required"},
		{"bad flag", []string{"scenario", "run", "-bogus"}, 2, "", "flag provided but not defined"},
		{"help", []string{"scenario", "run", "-h"}, 0, "", "-atespace"},
		{"bad scenario", []string{"scenario", "run", "-atespace", "a", "-context", "c", "-router", "http://r", t.TempDir()}, 1, "", "scenario.yaml"},
		{"not in cluster", []string{"scenario", "run", "-atespace", "a", "-in-cluster", dir}, 1, "", "not running in a cluster"},
		{"times out", []string{"scenario", "run", "-atespace", "a", "-context", "c", "-router", "http://r", "-json", jsonOut, dir}, 1,
			`"pass": false`, "scenario quick: broke lab/quick-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "not in cluster" {
				t.Setenv("KUBERNETES_SERVICE_HOST", "")
			}
			var out, errOut bytes.Buffer
			if code := run(tc.args, &out, &errOut); code != tc.code {
				t.Errorf("exit %d, want %d\nstdout %s\nstderr %s", code, tc.code, out.String(), errOut.String())
			}
			if !strings.Contains(out.String(), tc.stdout) || !strings.Contains(errOut.String(), tc.stderr) {
				t.Errorf("stdout %q (want %q)\nstderr %q (want %q)", out.String(), tc.stdout, errOut.String(), tc.stderr)
			}
		})
	}
	if gotRouter == nil || gotRouter.Token != "tok" || gotRouter.Base != "http://r" || gotRouter.Atespace != "a" {
		t.Errorf("router = %+v", gotRouter)
	}
	var r scenario.Result
	b, err := os.ReadFile(jsonOut)
	if err != nil || json.Unmarshal(b, &r) != nil || r.Pass || r.Scenario != "quick" {
		t.Errorf("json result %q, %v", b, err)
	}

	// In the cluster: the service account's kubeconfig, and the router and
	// ax-server by their service names.
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	t.Setenv("KUBECONFIG", "")
	var axSeen scenario.Ax
	newScenarioTasks = func(a scenario.Ax) scenario.Tasks { axSeen = a; return fake }
	var out0, err0 bytes.Buffer
	if code := run([]string{"scenario", "run", "-atespace", "a", "-in-cluster", dir}, &out0, &err0); code != 1 {
		t.Errorf("in-cluster: exit %d, stderr %s", code, err0.String())
	}
	if gotRouter.Base != inClusterRouter || axSeen.Server != "ax-server.ax-system.svc.cluster.local:8080" || !strings.HasSuffix(os.Getenv("KUBECONFIG"), "shipmind-in-cluster.kubeconfig") {
		t.Errorf("in-cluster: router %q, ax %+v, KUBECONFIG %q", gotRouter.Base, axSeen, os.Getenv("KUBECONFIG"))
	}
	// An unwritable -json path is an error.
	err0.Reset()
	if code := run([]string{"scenario", "run", "-atespace", "a", "-context", "c", "-router", "http://r", "-json", t.TempDir(), dir}, &out0, &err0); code != 1 || !strings.Contains(err0.String(), "is a directory") {
		t.Errorf("bad -json: exit %d, stderr %q", code, err0.String())
	}

	fake.token = ""
	newScenarioKube = func(string) scenarioKube { return fake }
	var out, errOut bytes.Buffer
	if code := run([]string{"scenario", "run", "-atespace", "a", "-context", "c", "-router", "http://r", dir}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "attach token") {
		t.Errorf("no token: exit %d, stderr %q", code, errOut.String())
	}
	fake.secret = errors.New("forbidden")
	newScenarioKube = func(string) scenarioKube { return fake }
	errOut.Reset()
	if code := run([]string{"scenario", "run", "-atespace", "a", "-context", "c", "-router", "http://r", dir}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "forbidden") {
		t.Errorf("secret error: exit %d, stderr %q", code, errOut.String())
	}
}
