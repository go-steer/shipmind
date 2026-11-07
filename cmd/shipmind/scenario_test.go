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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/shipmind/internal/archive"
	"github.com/go-steer/shipmind/internal/judge"
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
	oldK, oldT, oldA, oldR := newScenarioKube, newScenarioTasks, newScenarioAgent, newScenarioArchiver
	t.Cleanup(func() {
		newScenarioKube, newScenarioTasks, newScenarioAgent, newScenarioArchiver = oldK, oldT, oldA, oldR
	})
	var archivedTo *archive.GCS
	newScenarioArchiver = func(g *archive.GCS) scenario.Archiver { archivedTo = g; return fakeArchiver{} }
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
		{"half mTLS", []string{"scenario", "run", "-atespace", "a", "-context", "c", "-router", "http://r", "-router-cert", "c.pem", dir}, 2, "", "wants both -router-cert and -router-trust"},
		{"bad archive", []string{"scenario", "run", "-atespace", "a", "-context", "c", "-router", "http://r", "-archive", "runs/stage", dir}, 2, "", "want gs://bucket/prefix"},
		{"archives", []string{"scenario", "run", "-atespace", "a", "-context", "c", "-router", "http://r", "-archive", "gs://runs/stage", dir}, 1, "", "scenario quick: archived 2 files under quick/"},
		{"server name without mTLS", []string{"scenario", "run", "-atespace", "a", "-context", "c", "-router", "http://r", "-router-server-name", "x", dir}, 2, "", "-router-server-name is for mTLS"},
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

	// With mTLS: the https service name in the cluster, and the client
	// built from the projected files.
	oldC := newRouterClient
	t.Cleanup(func() { newRouterClient = oldC })
	var gotTLS scenario.RouterTLS
	closed := false
	tlsClient := &http.Client{}
	newRouterClient = func(rt scenario.RouterTLS) (*http.Client, func() error, error) {
		gotTLS = rt
		return tlsClient, func() error { closed = true; return nil }, nil
	}
	err0.Reset()
	if code := run([]string{"scenario", "run", "-atespace", "a", "-in-cluster", "-router-cert", "/pc/bundle.pem", "-router-trust", "/tb/ca.pem", dir}, &out0, &err0); code != 1 {
		t.Errorf("in-cluster mTLS: exit %d, stderr %s", code, err0.String())
	}
	if gotRouter.Base != inClusterRouterTLS || gotRouter.Client != tlsClient || !closed ||
		gotTLS != (scenario.RouterTLS{CredentialBundle: "/pc/bundle.pem", TrustBundle: "/tb/ca.pem"}) {
		t.Errorf("in-cluster mTLS: router %+v, tls %+v, closed %v", gotRouter, gotTLS, closed)
	}
	newRouterClient = func(scenario.RouterTLS) (*http.Client, func() error, error) { return nil, nil, errors.New("no bundle") }
	err0.Reset()
	if code := run([]string{"scenario", "run", "-atespace", "a", "-context", "c", "-router", "https://r", "-router-cert", "c", "-router-trust", "t", "-router-server-name", "n", dir}, &out0, &err0); code != 1 || !strings.Contains(err0.String(), "no bundle") {
		t.Errorf("mTLS client error: exit %d, stderr %q", code, err0.String())
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
	if archivedTo == nil || archivedTo.Bucket != "runs" || archivedTo.Prefix != "stage" {
		t.Errorf("-archive gs://runs/stage built %+v", archivedTo)
	}
}

type fakeArchiver struct{}

func (fakeArchiver) Save(context.Context, string, map[string][]byte) error { return nil }

// stubGrader grades every answer the same, or fails.
type stubGrader struct {
	score int
	err   error
}

func (s stubGrader) Grade(_ context.Context, expected, answer string) (judge.Grade, error) {
	g := judge.Grade{Model: "stub", Rubric: judge.RubricID, Score: s.score, CorrectDiagnosis: s.score >= 4, Reasoning: "graded " + answer[:min(len(answer), 12)]}
	if s.err != nil {
		g.Score, g.Error = 0, s.err.Error()
	}
	return g, s.err
}

func TestScenarioJudgeCommand(t *testing.T) {
	oldJ, oldM := newJudge, metadataProject
	t.Cleanup(func() { newJudge, metadataProject = oldJ, oldM })
	var gotProject string
	grader := stubGrader{score: 2}
	newJudge = func(_ context.Context, model, project, region, effort string) (scenario.Grader, error) {
		gotProject = project
		return grader, nil
	}
	metadataProject = func(context.Context) string { return "" }
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good.json", `{"scenario": "missing-env", "workload": "missing-env-1101-120000", "answer": "DATABASE_URL is missing", "want": "1-2"}`)
	noScenario := write("bad.json", `{"answer": "x"}`)
	unknown := write("unknown.json", `{"scenario": "nope", "workload": "nope-1", "answer": "x"}`)
	scenarios := "../../e2e/scenarios"

	for _, tc := range []struct {
		name           string
		args           []string
		code           int
		stdout, stderr string
	}{
		{"usage", []string{"scenario", "judge"}, 2, "", "usage:"},
		{"no model", []string{"scenario", "judge", good}, 2, "", "usage:"},
		{"no project", []string{"scenario", "judge", "-model", "m", good}, 2, "", "wants its Google Cloud project"},
		{"graded", []string{"scenario", "judge", "-model", "m", "-project", "p", "-scenarios", scenarios, good}, 0, "good.json: 2/5 (want 1-2) correct=false", ""},
		{"json", []string{"scenario", "judge", "-json", "-model", "m", "-project", "p", "-scenarios", scenarios, good}, 0, `"want":"1-2","model":"stub"`, ""},
		{"bad files", []string{"scenario", "judge", "-model", "m", "-project", "p", "-scenarios", scenarios, noScenario, unknown, good}, 1, "3 graded", "want scenario and workload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut strings.Builder
			if code := run(tc.args, &out, &errOut); code != tc.code {
				t.Fatalf("exit %d, want %d\nstdout %s\nstderr %s", code, tc.code, out.String(), errOut.String())
			}
			if !strings.Contains(out.String(), tc.stdout) || !strings.Contains(errOut.String(), tc.stderr) {
				t.Errorf("stdout %q (want %q)\nstderr %q (want %q)", out.String(), tc.stdout, errOut.String(), tc.stderr)
			}
		})
	}
	if gotProject != "p" {
		t.Errorf("project %q", gotProject)
	}

	// The project falls back to the machine's own.
	metadataProject = func(context.Context) string { return "from-metadata" }
	var out, errOut strings.Builder
	if code := run([]string{"scenario", "judge", "-model", "m", "-scenarios", scenarios, good}, &out, &errOut); code != 0 || gotProject != "from-metadata" {
		t.Errorf("exit %d, project %q, stderr %s", code, gotProject, errOut.String())
	}

	// A failed grade is printed and fails the command.
	grader = stubGrader{err: errors.New("403 permission denied")}
	out.Reset()
	if code := run([]string{"scenario", "judge", "-model", "m", "-project", "p", "-scenarios", scenarios, good}, &out, &errOut); code != 1 || !strings.Contains(out.String(), "403 permission denied") {
		t.Errorf("exit %d, stdout %s", code, out.String())
	}
}

// A judge that can't be set up is reported and the run goes ahead.
func TestScenarioRunBrokenJudge(t *testing.T) {
	oldK, oldT, oldA, oldJ, oldM := newScenarioKube, newScenarioTasks, newScenarioAgent, newJudge, metadataProject
	t.Cleanup(func() {
		newScenarioKube, newScenarioTasks, newScenarioAgent, newJudge, metadataProject = oldK, oldT, oldA, oldJ, oldM
	})
	fake := scenarioFake{token: "tok"}
	newScenarioKube = func(string) scenarioKube { return fake }
	newScenarioTasks = func(scenario.Ax) scenario.Tasks { return fake }
	newScenarioAgent = func(r *scenario.Router) scenario.Agent { return r }
	metadataProject = func(context.Context) string { return "" }
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	called := false
	newJudge = func(context.Context, string, string, string, string) (scenario.Grader, error) {
		called = true
		return nil, errors.New("unreachable")
	}
	dir := filepath.Join(t.TempDir(), "quick")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"scenario.yaml": "namespace: lab\nworkload: w.yaml\nexpect:\n  mentions: [X]\n  diagnosis: d\nlimits:\n  incident: 1ns\n  diagnose: 1ns\n  suspend: 1ns\n",
		"w.yaml":        "kind: ConfigMap\nmetadata:\n  name: {{.Name}}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut strings.Builder
	code := run([]string{"scenario", "run", "-atespace", "a", "-context", "c", "-router", "http://r", "-judge", "claude-sonnet-5-5", dir}, &out, &errOut)
	// The run itself times out at once (exit 1), as it would without a
	// judge: the missing project is reported, not fatal.
	if code != 1 || called || !strings.Contains(errOut.String(), "judge: a judge model wants its Google Cloud project") || !strings.Contains(out.String(), `"scenario": "quick"`) {
		t.Errorf("exit %d, newJudge called %v\nstdout %s\nstderr %s", code, called, out.String(), errOut.String())
	}
}

func TestScenarioJudgeEdges(t *testing.T) {
	oldM := metadataProject
	t.Cleanup(func() { metadataProject = oldM })
	metadataProject = func(context.Context) string { return "" }

	// A scenario without a diagnosis can't be graded.
	root := t.TempDir()
	sc := filepath.Join(root, "plain")
	if err := os.MkdirAll(sc, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"scenario.yaml": "namespace: lab\nworkload: w.yaml\nexpect:\n  mentions: [X]\nlimits:\n  incident: 1m\n  diagnose: 1m\n  suspend: 1m\n",
		"w.yaml":        "kind: ConfigMap\n",
		"answer.json":   `{"scenario": "plain", "workload": "plain-1", "answer": "x"}`,
	} {
		if err := os.WriteFile(filepath.Join(sc, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		stderr string
	}{
		{"help", []string{"scenario", "judge", "-h"}, 0, ""},
		{"bad flag", []string{"scenario", "judge", "-bogus"}, 2, "flag provided but not defined"},
		{"missing file", []string{"scenario", "judge", "-model", "m", "-project", "p", filepath.Join(root, "nope.json")}, 1, "no such file"},
		// The real constructor checks the effort before it looks for
		// credentials.
		{"bad effort", []string{"scenario", "judge", "-model", "m", "-project", "p", "-effort", "extreme", filepath.Join(sc, "answer.json")}, 2, "effort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut strings.Builder
			if code := run(tc.args, &out, &errOut); code != tc.code || !strings.Contains(errOut.String(), tc.stderr) {
				t.Errorf("exit %d (want %d), stderr %q (want %q)", code, tc.code, errOut.String(), tc.stderr)
			}
		})
	}

	oldJ := newJudge
	t.Cleanup(func() { newJudge = oldJ })
	newJudge = func(context.Context, string, string, string, string) (scenario.Grader, error) {
		return stubGrader{score: 3}, nil
	}
	var out, errOut strings.Builder
	if code := run([]string{"scenario", "judge", "-model", "m", "-project", "p", "-scenarios", root, filepath.Join(sc, "answer.json")}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "no expect.diagnosis") {
		t.Errorf("exit %d, stderr %s", code, errOut.String())
	}
}
