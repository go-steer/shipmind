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

package scenario

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// Every scenario in the repository loads, and its workload renders to
// valid YAML.
func TestRepoScenarios(t *testing.T) {
	dirs, err := filepath.Glob("../../e2e/scenarios/*/scenario.yaml")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no scenarios: %v", err)
	}
	for _, d := range dirs {
		s, err := Load(filepath.Dir(d))
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		m, err := s.Manifest(RunName(s.Name, t0))
		if err != nil {
			t.Errorf("%s: %v", s.Name, err)
			continue
		}
		dec := yaml.NewDecoder(strings.NewReader(string(m)))
		for {
			var doc map[string]any
			if err := dec.Decode(&doc); err != nil {
				if err.Error() != "EOF" {
					t.Errorf("%s: workload is not YAML: %v", s.Name, err)
				}
				break
			}
		}
	}
}

func TestScore(t *testing.T) {
	s, err := Load("../../e2e/scenarios/missing-env")
	if err != nil {
		t.Fatal(err)
	}
	name := RunName(s.Name, t0)
	if name != "missing-env-1021-120000" {
		t.Errorf("RunName = %q", name)
	}
	missing, err := s.Score("database_url is unset in ConfigMap "+name+"-config", name)
	if err != nil || len(missing) != 0 {
		t.Errorf("Score(correct) = %v, %v", missing, err)
	}
	missing, err = s.Score("the pod crash-loops", name)
	if err != nil || !slices.Equal(missing, []string{"DATABASE_URL", name + "-config"}) {
		t.Errorf("Score(wrong) = %v, %v", missing, err)
	}
}

func TestLoadErrors(t *testing.T) {
	write := func(t *testing.T, dir, name, body string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	good := "namespace: lab\nworkload: w.yaml\nexpect:\n  mentions: [X]\nlimits:\n  incident: 1m\n  diagnose: 1m\n  suspend: 1m\n"
	for _, tc := range []struct {
		name, dir, doc, want string
	}{
		{"empty", "ok", "", "empty document"},
		{"unknown field", "ok", good + "bogus: 1\n", "bogus"},
		{"bad dir name", "Bad_Name", good, "directory name"},
		{"namespace", "ok", strings.Replace(good, "namespace: lab", "namespace: Lab", 1), "namespace"},
		{"workload path", "ok", strings.Replace(good, "workload: w.yaml", "workload: ../w.yaml", 1), "workload"},
		{"workload missing", "ok", strings.Replace(good, "workload: w.yaml", "workload: nope.yaml", 1), "workload:"},
		{"no mentions", "ok", strings.Replace(good, "mentions: [X]", "mentions: []", 1), "expect.mentions"},
		{"bad regex", "ok", strings.Replace(good, "mentions: [X]", "mentions: ['(']", 1), "expect.mentions"},
		{"bad template", "ok", strings.Replace(good, "mentions: [X]", "mentions: ['{{.Nope}}']", 1), "expect.mentions"},
		{"limits", "ok", strings.Replace(good, "suspend: 1m", "suspend: 0s", 1), "limits"},
		{"trajectory name", "ok", strings.Replace(good, "mentions: [X]", "mentions: [X]\n  trajectory:\n    must_call: ['get pod']", 1), "expect.trajectory"},
		{"trajectory empty alternative", "ok", strings.Replace(good, "mentions: [X]", "mentions: [X]\n  trajectory:\n    never_call: ['a|']", 1), "expect.trajectory"},
		{"trajectory negative", "ok", strings.Replace(good, "mentions: [X]", "mentions: [X]\n  trajectory:\n    max_tool_calls: -1", 1), "expect.trajectory"},
		{"trajectory unknown key", "ok", strings.Replace(good, "mentions: [X]", "mentions: [X]\n  trajectory:\n    must: [x]", 1), "must"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), tc.dir)
			write(t, dir, "scenario.yaml", tc.doc)
			write(t, dir, "w.yaml", "kind: X\n")
			_, err := Load(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("Load of an empty directory succeeded")
	}
	dir := filepath.Join(t.TempDir(), "ok")
	write(t, dir, "scenario.yaml", good)
	write(t, dir, "w.yaml", "name: {{.Nope}}\n")
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Manifest("x"); err == nil {
		t.Error("Manifest with an unknown key succeeded")
	}
	if s.Limits.Diagnose != time.Minute {
		t.Errorf("limits = %+v", s.Limits)
	}
}

// oom-killed: a container that allocates more than its memory limit. The
// answer must name the OOM kill, the 64Mi limit and the field to raise,
// in the shapes an answer actually takes.
func TestOOMKilledScenario(t *testing.T) {
	s, err := Load("../../e2e/scenarios/oom-killed")
	if err != nil {
		t.Fatal(err)
	}
	if s.Namespace != "triage-lab" || len(s.Expect.Mentions) != 3 {
		t.Fatalf("scenario %+v", s)
	}
	// The answer core-agent gave to this workload on staging (2026-11-05).
	good := "Container `reports` is terminated with `exitCode: 137` (`OOMKilled`) because the workload allocates a 200 MiB bytearray at startup while container memory is capped at 64 MiB. Field: `spec.template.spec.containers[name=\"reports\"].resources.limits.memory` Value: `256Mi`"
	if missing, err := s.Score(good, "oom-killed-1105-160452"); err != nil || len(missing) != 0 {
		t.Errorf("a correct answer scored missing %v, %v", missing, err)
	}
	for _, ok := range []string{
		"The container hit the out-of-memory killer: 64 MB is below what it allocates. Raise the memory-limit.",
		"OOM killed (exit 137). There is a limit of 64Mi on memory; set resources.limits:\n  memory: 256Mi.",
	} {
		if missing, err := s.Score(ok, "oom-killed-1105-160452"); err != nil || len(missing) != 0 {
			t.Errorf("%q scored missing %v, %v", ok, missing, err)
		}
	}
	for _, bad := range []string{
		"The pod crash-loops because DATABASE_URL is missing.",
		"OOMKilled: the node ran out of memory; add nodes.",
		"OOMKilled after 64 minutes; raise the memory limit.",
		"OOMKilled at 164Mi; raise the memory limit.",
	} {
		if missing, err := s.Score(bad, "oom-killed-1105-160452"); err != nil || len(missing) == 0 {
			t.Errorf("%q scored correct (%v)", bad, err)
		}
	}
	m, err := s.Manifest("oom-killed-1105-160452")
	if err != nil || !strings.Contains(string(m), "memory: 64Mi") || !strings.Contains(string(m), "shipmind.dev/scenario: oom-killed") {
		t.Errorf("manifests: %v\n%s", err, m)
	}
}

// Every nightly CronJob runs a scenario that exists here, so the runner
// image built from this tree carries it (images/e2e-runner copies
// e2e/scenarios). A CronJob still needs the runner pin moved to an image
// built after its scenario was added.
func TestNightlyScenariosExist(t *testing.T) {
	b, err := os.ReadFile("../../deploy/e2e/substrate/e2e.yaml")
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`(?m)^\s+- /scenarios/([a-z0-9-]+)$`).FindAllStringSubmatch(string(b), -1)
	if len(refs) < 2 {
		t.Fatalf("found %d scenario references in the CronJobs", len(refs))
	}
	entries, err := os.ReadDir("../../e2e/scenarios")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join("../../e2e/scenarios", e.Name(), "scenario.yaml")); e.IsDir() && err == nil {
			have[e.Name()] = true
		}
	}
	for _, r := range refs {
		if !have[r[1]] {
			t.Errorf("a CronJob runs /scenarios/%s, which e2e/scenarios doesn't have", r[1])
		}
	}
}

func TestTrajectoryCheck(t *testing.T) {
	tj := &Trajectory{MustCall: []string{"get_k8s_resource|describe_k8s_resource", "get_k8s_logs"}, NeverCall: []string{"delete_k8s_resource"}, MaxToolCalls: 3, MaxModelCalls: 5}
	for _, tc := range []struct {
		calls      []string
		modelCalls int
		failures   []string
		notes      []string
	}{
		{[]string{"gke_describe_k8s_resource", "gke_get_k8s_logs"}, 4, nil, nil},
		{[]string{"get_k8s_resource", "get_k8s_logs"}, 4, nil, nil}, // no prefix
		{[]string{"gke_get_k8s_logs"}, 4, []string{"never called get_k8s_resource or describe_k8s_resource"}, nil},
		{[]string{"gke_get_k8s_resource", "gke_get_k8s_logs", "gke_delete_k8s_resource"}, 4, []string{"called gke_delete_k8s_resource, which it must not"}, nil},
		{[]string{"gke_get_k8s_resource", "gke_get_k8s_logs", "x", "y"}, 6, []string{"4 tool calls, more than 3", "6 model calls, more than 5"}, nil},
		{[]string{"gke_get_k8s_resource", "gke_get_k8s_logs"}, -1, nil, []string{"max_model_calls not checked: the agent's usage wasn't read"}},
		{[]string{"gke_get_k8s_resourcex", "gke_get_k8s_logs"}, 4, []string{"never called get_k8s_resource or describe_k8s_resource"}, nil}, // not a whole name
		{[]string{"a_b_get_k8s_resource", "gke_get_k8s_logs"}, 4, []string{"never called get_k8s_resource or describe_k8s_resource"}, nil},  // one server prefix only
	} {
		failures, notes := tj.Check(tc.calls, tc.modelCalls)
		if !slices.Equal(failures, tc.failures) || !slices.Equal(notes, tc.notes) {
			t.Errorf("Check(%v, %d) = %q, %q; want %q, %q", tc.calls, tc.modelCalls, failures, notes, tc.failures, tc.notes)
		}
	}
}

// Both scenarios bound the path; their expectations load and accept what
// a healthy run on staging did (core-agent 3.1.0, 2026-11-06: the pod and
// its logs, then the Deployment, in four model calls).
func TestScenarioTrajectories(t *testing.T) {
	healthy := []string{"gke_get_k8s_resource", "gke_get_k8s_logs", "gke_get_k8s_resource"}
	for _, name := range []string{"missing-env", "oom-killed"} {
		s, err := Load("../../e2e/scenarios/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if s.Expect.Trajectory == nil {
			t.Fatalf("%s: no trajectory", name)
		}
		if failures, _ := s.Expect.Trajectory.Check(healthy, 4); len(failures) != 0 {
			t.Errorf("%s: a healthy run fails %q", name, failures)
		}
		if failures, _ := s.Expect.Trajectory.Check(append(healthy, "gke_patch_k8s_resource"), 5); len(failures) == 0 {
			t.Errorf("%s: a write passes", name)
		}
	}
}

func TestToolMatches(t *testing.T) {
	for _, tc := range []struct {
		call, name string
		want       bool
	}{
		{"gke_get_k8s_resource", "get_k8s_resource", true},
		{"get_k8s_resource", "get_k8s_resource", true},
		{"gke_get_k8s_resource", "resource", false},
		{"gke_get_k8s_resource", "k8s_resource", false},
		{"_get_k8s_resource", "get_k8s_resource", false},
		{"gke_delete_k8s_resource", "delete_k8s_resource", true},
	} {
		if got := toolMatches(tc.call, tc.name); got != tc.want {
			t.Errorf("toolMatches(%q, %q) = %v", tc.call, tc.name, got)
		}
	}
}

// The scenarios' tool catalog is the bundles' tool_catalog, so a
// trajectory can't name a tool that doesn't exist; a typo is refused.
func TestToolCatalog(t *testing.T) {
	known, err := loadTools("../../e2e/scenarios/tools.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, bundle := range []string{"gke-triage-f", "gke-chat"} {
		b, err := os.ReadFile("../../agents/sre-triage/" + bundle + "/workload.yaml")
		if err != nil {
			t.Fatal(err)
		}
		var w struct {
			Catalog struct {
				Tools []struct {
					Name string `yaml:"name"`
				} `yaml:"tools"`
			} `yaml:"tool_catalog"`
		}
		if err := yaml.Unmarshal(b, &w); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tl := range w.Catalog.Tools {
			names = append(names, tl.Name)
		}
		if len(names) == 0 || len(names) != len(known) {
			t.Errorf("%s: tool_catalog has %d tools, e2e/scenarios/tools.yaml %d", bundle, len(names), len(known))
		}
		for _, n := range names {
			if !known[n] {
				t.Errorf("%s: tool %q isn't in e2e/scenarios/tools.yaml", bundle, n)
			}
		}
	}

	dir := filepath.Join(t.TempDir(), "typo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "namespace: lab\nworkload: w.yaml\nexpect:\n  mentions: [X]\n  trajectory:\n    never_call: [delete_k8s_resources]\nlimits:\n  incident: 1m\n  diagnose: 1m\n  suspend: 1m\n"
	for name, body := range map[string]string{filepath.Join(dir, "scenario.yaml"): doc, filepath.Join(dir, "w.yaml"): "kind: X\n", filepath.Join(filepath.Dir(dir), "tools.yaml"): "tools: [delete_k8s_resource]\n"} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), `"delete_k8s_resources" isn't a tool`) {
		t.Errorf("a misspelled never_call loaded: %v", err)
	}
}
