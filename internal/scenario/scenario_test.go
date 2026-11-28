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
	"bytes"
	"encoding/json"
	"errors"
	"io"
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
		if s.Chat != nil {
			if ask, err := s.ForCluster("projects/example-project/locations/us-central1/clusters/demo").Ask(RunName(s.Name, t0)); err != nil || strings.Contains(ask, "{{") {
				t.Errorf("%s: question %q: %v", s.Name, ask, err)
			}
			if s.Workload == "" {
				continue
			}
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

// Every repo scenario states its ground truth for the judge (#90), naming
// the run's own resources.
func TestRepoDiagnoses(t *testing.T) {
	for _, name := range []string{"missing-env", "oom-killed", "triage-overflow"} {
		s, err := Load("../../e2e/scenarios/" + name)
		if err != nil {
			t.Fatal(err)
		}
		run := RunName(s.Name, t0)
		d, err := s.Diagnosis(run)
		if err != nil || !strings.Contains(d, "Deployment "+run+" in namespace triage-lab") || strings.Contains(d, "{{") {
			t.Errorf("%s: diagnosis %q, %v", name, d, err)
		}
	}
	s := &Scenario{Namespace: "lab"}
	if d, err := s.Diagnosis("x"); d != "" || err != nil {
		t.Errorf("no diagnosis: %q, %v", d, err)
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
		{"drain without reason", "ok", good + "drain:\n  within: 1m\n", "drain: want a reason"},
		{"drain without within", "ok", good + "drain:\n  reason: WorkerDraining\n", "drain: want a reason"},
		{"diagnosis template", "ok", strings.Replace(good, "mentions: [X]", "mentions: [X]\n  diagnosis: 'ConfigMap {{.Nope}}'", 1), "expect.diagnosis"},
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
	// The worker-drain nightly rides with its opt-in RBAC (#105).
	d, err := os.ReadFile("../../deploy/e2e/substrate/drain/cronjob.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, d...)
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

// Incident scenarios bound the path; their expectations accept both the
// lookout MCP triage tools (#131) and historical archived GKE MCP calls (#91).
func TestScenarioTrajectories(t *testing.T) {
	lookoutHealthy := []string{"lookout_k8s_triage_workload", "lookout_k8s_triage_logs"}
	legacyHealthy := []string{"gke_get_k8s_resource", "gke_get_k8s_logs", "gke_get_k8s_resource"}
	for _, name := range []string{"missing-env", "oom-killed", "worker-drain"} {
		s, err := Load("../../e2e/scenarios/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if s.Expect.Trajectory == nil {
			t.Fatalf("%s: no trajectory", name)
		}
		if failures, _ := s.Expect.Trajectory.Check(lookoutHealthy, 4); len(failures) != 0 {
			t.Errorf("%s: healthy lookout run fails %q", name, failures)
		}
		if failures, _ := s.Expect.Trajectory.Check(legacyHealthy, 4); len(failures) != 0 {
			t.Errorf("%s: archived GKE MCP run fails %q", name, failures)
		}
		if failures, _ := s.Expect.Trajectory.Check(append(lookoutHealthy, "gke_patch_k8s_resource"), 5); len(failures) == 0 {
			t.Errorf("%s: a write passes", name)
		}
	}
}

// triage-overflow (#131): the enrichment bundle drops section=logs under the
// default 4096-byte cap, so the scenario only passes if the agent follows the
// overflow trailer with lookout_k8s_triage_logs and names SETTLEMENT_WEBHOOK_URL.
func TestTriageOverflowScenario(t *testing.T) {
	s, err := Load("../../e2e/scenarios/triage-overflow")
	if err != nil {
		t.Fatal(err)
	}
	run := RunName(s.Name, t0)
	if missing, err := s.Score("SETTLEMENT_WEBHOOK_URL is missing from ConfigMap "+run+"-config.", run); err != nil || len(missing) != 0 {
		t.Errorf("Score(correct) = %v, %v", missing, err)
	}
	if missing, err := s.Score("The settlement container is crash-looping with exit code 1.", run); err != nil || len(missing) != 2 {
		t.Errorf("Score(incomplete) = %v, %v", missing, err)
	}
	m, err := s.Manifest(run)
	if err != nil || !strings.Contains(string(m), "shipmind.dev/scenario: triage-overflow") || !strings.Contains(string(m), "name: "+run+"-config") {
		t.Errorf("manifest: %v\n%s", err, m)
	}
	if failures, _ := s.Expect.Trajectory.Check([]string{"lookout_k8s_triage_logs"}, 3); len(failures) != 0 {
		t.Errorf("following overflow with lookout_k8s_triage_logs failed: %q", failures)
	}
	if failures, _ := s.Expect.Trajectory.Check([]string{"lookout_k8s_resource_spec"}, 3); len(failures) == 0 {
		t.Error("triage-overflow passed without calling k8s_triage_logs")
	}
	if failures, _ := s.Expect.Trajectory.Check([]string{"gke_get_k8s_resource", "gke_get_k8s_logs"}, 3); len(failures) == 0 {
		t.Error("triage-overflow accepted legacy gke tools instead of lookout k8s_triage_logs")
	}
	if failures, _ := s.Expect.Trajectory.Check([]string{"lookout_k8s_triage_logs", "gke_patch_k8s_resource"}, 4); len(failures) == 0 {
		t.Error("triage-overflow allowed a cluster write")
	}
}

func TestToolMatches(t *testing.T) {
	for _, tc := range []struct {
		call, name string
		want       bool
	}{
		{"gke_get_k8s_resource", "get_k8s_resource", true},
		{"lookout_k8s_triage_logs", "k8s_triage_logs", true},
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

// The scenarios' tool catalog holds the union of the tools declared by native
// gke-triage-f and native gke-chat (.agents/config.json), plus the documented
// retired hosted GKE MCP read and write tool names kept for archived-run
// re-grading and never_call safety guards; a typo is refused.
func TestToolCatalog(t *testing.T) {
	known, err := loadTools("../../e2e/scenarios/tools.yaml")
	if err != nil {
		t.Fatal(err)
	}
	inBundles := map[string]bool{}
	recordTools := func(source string, names []string) {
		t.Helper()
		if len(names) == 0 {
			t.Errorf("%s: declares no tools", source)
		}
		seen := map[string]bool{}
		for _, name := range names {
			if seen[name] {
				t.Errorf("%s: tool %q listed twice", source, name)
			}
			seen[name] = true
			inBundles[name] = true
			if !known[name] {
				t.Errorf("%s: tool %q isn't in e2e/scenarios/tools.yaml", source, name)
			}
		}
	}

	for _, bundle := range []string{"gke-triage-f", "gke-chat"} {
		cb, err := os.ReadFile(filepath.Join("../../agents/sre-triage", bundle, ".agents/config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Tools struct {
				Policies []struct {
					Name string `json:"name"`
				} `json:"policies"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(cb, &cfg); err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(cfg.Tools.Policies))
		for _, p := range cfg.Tools.Policies {
			names = append(names, p.Name)
		}
		recordTools(bundle+"/.agents/config.json", names)

		mb, err := os.ReadFile(filepath.Join("../../agents/sre-triage", bundle, ".agents/mcp.json"))
		if err != nil {
			t.Fatal(err)
		}
		var mcpCfg struct {
			Servers map[string]struct {
				Tools []string `json:"tools"`
			} `json:"servers"`
		}
		if err := json.Unmarshal(mb, &mcpCfg); err != nil {
			t.Fatal(err)
		}
		if _, hasGKE := mcpCfg.Servers["gke"]; hasGKE {
			t.Errorf("%s/.agents/mcp.json must not define hosted gke server", bundle)
		}
		for srv, spec := range mcpCfg.Servers {
			for _, tl := range spec.Tools {
				if !known[tl] {
					t.Errorf("%s/.agents/mcp.json server %s tool %q isn't in e2e/scenarios/tools.yaml", bundle, srv, tl)
				}
			}
		}
	}

	retiredGKETools := map[string]bool{
		"triage-classifier":      true,
		"get_k8s_resource":       true,
		"describe_k8s_resource":  true,
		"list_k8s_events":        true,
		"get_k8s_logs":           true,
		"get_k8s_rollout_status": true,
		"list_k8s_api_resources": true,
		"get_k8s_cluster_info":   true,
		"get_k8s_version":        true,
		"check_k8s_auth":         true,
		"list_clusters":          true,
		"get_cluster":            true,
		"list_node_pools":        true,
		"get_node_pool":          true,
		"list_operations":        true,
		"get_operation":          true,
		"patch_k8s_resource":     true,
		"apply_k8s_manifest":     true,
		"delete_k8s_resource":    true,
		"create_cluster":         true,
		"update_cluster":         true,
		"create_node_pool":       true,
		"update_node_pool":       true,
		"cancel_operation":       true,
	}
	for n := range known {
		if !inBundles[n] && !retiredGKETools[n] {
			t.Errorf("e2e/scenarios/tools.yaml lists %q, which no recipe declares and isn't in retiredGKETools", n)
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

	// A misspelled tool in allow_tool_errors is refused against tools.yaml too.
	allowDir := filepath.Join(t.TempDir(), "allow-typo")
	if err := os.MkdirAll(allowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	allowDoc := "chat:\n  ask: hi\nexpect:\n  mentions: [X]\n  allow_tool_errors:\n    - tool: k8s_triage_workloads\n      patterns: ['not found']\nlimits:\n  start: 1m\n  diagnose: 1m\n"
	for name, body := range map[string]string{filepath.Join(allowDir, "scenario.yaml"): allowDoc, filepath.Join(filepath.Dir(allowDir), "tools.yaml"): "tools: [k8s_triage_workload]\n"} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(allowDir); err == nil || !strings.Contains(err.Error(), `"k8s_triage_workloads" isn't a tool`) {
		t.Errorf("a misspelled allow_tool_errors tool loaded: %v", err)
	}
}

// The opt-in drain nightly runs the same e2e-runner build as the base
// CronJobs, so a pin bump can't leave it behind, and it runs the drain
// scenario.
func TestDrainNightlyMatchesBase(t *testing.T) {
	image := regexp.MustCompile(`(?m)^\s+image: (ghcr\.io/go-steer/shipmind/e2e-runner:\S+)$`)
	base, err := os.ReadFile("../../deploy/e2e/substrate/e2e.yaml")
	if err != nil {
		t.Fatal(err)
	}
	drain, err := os.ReadFile("../../deploy/e2e/substrate/drain/cronjob.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b, d := image.FindAllStringSubmatch(string(base), -1), image.FindAllStringSubmatch(string(drain), -1)
	if len(b) == 0 || len(d) != 1 {
		t.Fatalf("found %d base and %d drain e2e-runner images", len(b), len(d))
	}
	for _, m := range b {
		if m[1] != d[0][1] {
			t.Errorf("drain nightly runs %s, the base runs %s", d[0][1], m[1])
		}
	}
	if !strings.Contains(string(drain), "- /scenarios/worker-drain\n") || !strings.Contains(string(drain), "shipmind.dev/nightly: scenario") {
		t.Error("the drain nightly must run /scenarios/worker-drain and carry the nightly label")
	}
	s, err := Load("../../e2e/scenarios/worker-drain")
	if err != nil || s.Drain == nil {
		t.Errorf("worker-drain: %v, drain %+v", err, s)
	}
}

// cross-cluster-lookup (#104, #15): a chat question that names a Deployment
// without naming its cluster. Pass requires listing clusters in scope and
// checking across them, then either locating the workload on its cluster or
// stating plainly that it could not be found — never fabricating a
// credential-policy cause when nothing appears on the home cluster.
func TestCrossClusterLookupScenario(t *testing.T) {
	s, err := Load("../../e2e/scenarios/cross-cluster-lookup")
	if err != nil {
		t.Fatal(err)
	}
	if s.Chat == nil || s.Workload != "" || s.Namespace != "" || s.Drain != nil {
		t.Fatalf("cross-cluster-lookup must be a non-seeded chat scenario: %+v", s)
	}
	const homeCluster = "projects/example-project/locations/us-central1/clusters/stage"
	c := s.ForCluster(homeCluster)
	run := RunName(s.Name, t0)

	ask, err := c.Ask(run)
	if err != nil || !strings.Contains(ask, run) || strings.Contains(ask, "{{") {
		t.Fatalf("Ask = %q, %v", ask, err)
	}
	if strings.Contains(ask, homeCluster) || strings.Contains(ask, "stage") {
		t.Errorf("question must not name or assume the home cluster: %q", ask)
	}
	diag, err := c.Diagnosis(run)
	if err != nil || !strings.Contains(diag, run) || !strings.Contains(diag, homeCluster) || strings.Contains(diag, "{{") {
		t.Fatalf("Diagnosis = %q, %v", diag, err)
	}

	// Passing answers: located on another cluster, or honestly not found across scope.
	for _, okAns := range []string{
		"I listed clusters in scope and found Deployment `" + run + "` in namespace `triage-lab` on cluster `projects/example-project/locations/us-central1/clusters/dev` (not on `stage`). It has 2/2 ready pods.",
		"Checked all clusters in scope (`stage`, `dev`, `prod`) and listed `triage-lab` on each. Deployment `" + run + "` was not found on any cluster in scope. Confirm the cluster or namespace name.",
		"I checked namespace `triage-lab` on `dev` and `prod`, and no pods or Deployment appeared for `" + run + "`.",
		"Deployment `" + run + "` wasn't found in `triage-lab` on `stage` or `dev`.",
		"Cannot locate Deployment `" + run + "` in `triage-lab`; it is absent from `stage` and `dev`.",
		"No TOOL FAILURE occurred and there were no permission errors. Deployment `" + run + "` could not be found in `triage-lab` across `stage` and `dev`.",
	} {
		missing, err := c.Score(okAns, run)
		if err != nil || len(missing) != 0 {
			t.Errorf("passing answer %q missing %v, err %v", okAns, missing, err)
		}
		unwanted, err := c.Unwanted(okAns, run)
		if err != nil || len(unwanted) != 0 {
			t.Errorf("passing answer %q unwanted %v, err %v", okAns, unwanted, err)
		}
	}

	// Failing answers: unrelated object, vague answer with no cluster/absence,
	// TOOL FAILURE marker, and fabricated credential-policy claims.
	for _, tc := range []struct {
		name, ans string
		wantMiss  bool
		wantBad   bool
	}{
		{"wrong object", "Cluster `dev` looks healthy; all deployments in `default` are running.", true, false},
		{"vague without cluster or absence", "Deployment `" + run + "` has a configuration issue; check logs.", true, false},
		{"tool failure", "TOOL FAILURE: lookout_k8s_list_clusters\nCould not reach cluster `" + run + "`.", false, true},
		{"fabricated credential policy", "Deployment `" + run + "` on cluster `stage` failed due to a credential-policy mismatch in `ate-credentials`.", false, true},
		{"fabricated egress denied", "Deployment `" + run + "` on cluster `stage` failed with egress denied from the credential provider.", false, true},
		{"fabricated hyphenated egress-denied", "Deployment `" + run + "` on cluster `stage` hit an egress-denied error.", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			missing, err := c.Score(tc.ans, run)
			if err != nil {
				t.Fatal(err)
			}
			unwanted, err := c.Unwanted(tc.ans, run)
			if err != nil {
				t.Fatal(err)
			}
			if (len(missing) > 0) != tc.wantMiss || (len(unwanted) > 0) != tc.wantBad {
				t.Errorf("missing=%v (want %v), unwanted=%v (want %v)", missing, tc.wantMiss, unwanted, tc.wantBad)
			}
		})
	}

	// Trajectory: requires k8s_list_clusters plus a cluster read, forbids writes, bounds calls.
	if failures, _ := s.Expect.Trajectory.Check([]string{"lookout_k8s_list_clusters", "lookout_k8s_list_resources"}, 4); len(failures) != 0 {
		t.Errorf("list_clusters + list_resources failed: %q", failures)
	}
	if failures, _ := s.Expect.Trajectory.Check([]string{"lookout_k8s_list_clusters", "lookout_k8s_list_resources", "lookout_k8s_triage_workload"}, 5); len(failures) != 0 {
		t.Errorf("list_clusters + list_resources + triage_workload failed: %q", failures)
	}
	if failures, _ := s.Expect.Trajectory.Check([]string{"lookout_k8s_list_resources"}, 3); len(failures) == 0 {
		t.Error("passed without calling k8s_list_clusters")
	}
	if failures, _ := s.Expect.Trajectory.Check([]string{"lookout_k8s_list_clusters"}, 2); len(failures) == 0 {
		t.Error("passed without reading any cluster after listing clusters")
	}
	if failures, _ := s.Expect.Trajectory.Check([]string{"lookout_k8s_list_clusters", "lookout_k8s_list_resources", "gke_patch_k8s_resource"}, 4); len(failures) == 0 {
		t.Error("allowed a cluster write")
	}
	tooMany := make([]string, 31)
	tooMany[0] = "lookout_k8s_list_clusters"
	for i := 1; i < len(tooMany); i++ {
		tooMany[i] = "lookout_k8s_list_resources"
	}
	if failures, _ := s.Expect.Trajectory.Check(tooMany, 4); len(failures) == 0 {
		t.Error("allowed more than max_tool_calls")
	}

	// Expected tool errors: allow only {{.Name}} not-found errors on targeted
	// lookout read tools; real auth, RBAC, cluster-not-found, and wrong-object
	// errors still fail.
	for _, tc := range []struct {
		tool, msg string
		want      bool
	}{
		{"lookout_k8s_triage_workload", "Tool execution failed. Details: workload Deployment/triage-lab/" + run + " not found (14 objects listed)", true},
		{"lookout_k8s_resource_spec", `fetching Deployment triage-lab/` + run + `: deployments.apps "` + run + `" not found`, true},
		{"lookout_k8s_event_timeline", "workload Deployment/triage-lab/" + run + " not found (14 objects listed)", true},
		{"lookout_k8s_state_edges", "workload Deployment/triage-lab/" + run + " not found (14 objects listed)", true},
		{"lookout_k8s_triage_logs", `deployments.apps "` + run + `" not found`, true},
		{"lookout_k8s_triage_workload", "forbidden: cannot list deployments in namespace triage-lab", false},
		{"lookout_k8s_triage_workload", `cluster "dev" not found`, false},
		{"lookout_k8s_triage_workload", "workload Deployment/triage-lab/other-app not found (14 objects listed)", false},
		{"lookout_k8s_list_clusters", "workload Deployment/triage-lab/" + run + " not found", false},
	} {
		if got := c.AllowsToolError(tc.tool, tc.msg, run); got != tc.want {
			t.Errorf("AllowsToolError(%q, %q) = %v, want %v", tc.tool, tc.msg, got, tc.want)
		}
	}

	// Result checks on k8s_list_clusters logfmt output shapes (k8s-lookout v0.46.0).
	listOK := "cluster=demo project=example-project location=us-central1 canonical=example-project/us-central1/demo dns_endpoint=true\nscope=example-project via=gke clusters=2\nscanned=2 findings=0 elapsed=85ms clusters=2\n"
	listFiltered := "scope=example-project via=gke clusters=2\nscanned=2 findings=0 elapsed=2ms clusters=0\n"
	listEmpty := "scope=example-project via=gke clusters=0 skipped=\"1 no dns endpoint\"\nscanned=0 findings=0 elapsed=40ms clusters=0\n"
	if got := s.CheckResults([]ToolResult{{Tool: "lookout_k8s_list_clusters", Output: listOK}}); len(got) != 0 {
		t.Errorf("healthy list_clusters failed: %v", got)
	}
	if got := s.CheckResults([]ToolResult{{Tool: "lookout_k8s_list_clusters", Output: listFiltered}}); len(got) != 0 {
		t.Errorf("filtered list_clusters with non-zero scanned failed: %v", got)
	}
	if got := s.CheckResults([]ToolResult{{Tool: "lookout_k8s_list_clusters", Output: listEmpty}}); len(got) == 0 {
		t.Error("zero-cluster list_clusters result passed")
	}
	if got := s.CheckResults([]ToolResult{{Tool: "lookout_k8s_list_clusters", Output: "2 clusters found", Method: MethodSummarized}}); len(got) == 0 {
		t.Error("llm_fallback summarized list_clusters result passed")
	}
}

// Every calibration fixture in e2e/calibration/ matches the `shipmind scenario
// judge` schema, points at a scenario whose diagnosis renders cleanly, is
// documented in e2e/calibration/README.md, and the 2026-11-13 cross-cluster
// fabricated answers both expect score 1 and trigger cross-cluster-lookup's
// unwanted-pattern check.
func TestCalibrationCases(t *testing.T) {
	files, err := filepath.Glob("../../e2e/calibration/*.json")
	if err != nil || len(files) < 12 {
		t.Fatalf("calibration files = %v, err %v (want at least 12)", files, err)
	}
	readme, err := os.ReadFile("../../e2e/calibration/README.md")
	if err != nil {
		t.Fatal(err)
	}
	wantBandRE := regexp.MustCompile(`^([1-5])(?:-([1-5]))?$`)

	type calCase struct {
		Scenario string `json:"scenario"`
		Workload string `json:"workload"`
		Cluster  string `json:"cluster,omitempty"`
		Answer   string `json:"answer"`
		Want     string `json:"want"`
	}
	crossClusterCases := 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var c calCase
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			t.Fatalf("%s: trailing data after JSON object (%v)", path, err)
		}
		if c.Scenario == "" || c.Workload == "" || strings.TrimSpace(c.Answer) == "" {
			t.Errorf("%s: scenario, workload and answer are required", path)
			continue
		}
		m := wantBandRE.FindStringSubmatch(c.Want)
		if m == nil || (m[2] != "" && m[1] > m[2]) {
			t.Errorf("%s: want %q is not a valid 1-5 score or band", path, c.Want)
		}
		s, err := Load(filepath.Join("../../e2e/scenarios", c.Scenario))
		if err != nil {
			t.Errorf("%s: load scenario %s: %v", path, c.Scenario, err)
			continue
		}
		if s.Chat != nil && !chatCluster.MatchString(c.Cluster) {
			t.Errorf("%s: chat calibration case needs a valid cluster path, got %q", path, c.Cluster)
		}
		diag, err := s.ForCluster(c.Cluster).Diagnosis(c.Workload)
		if err != nil || strings.TrimSpace(diag) == "" || strings.Contains(diag, "{{") {
			t.Errorf("%s: Diagnosis(%q) = %q, %v", path, c.Workload, diag, err)
		}
		caseName := strings.TrimSuffix(filepath.Base(path), ".json")
		if !strings.Contains(string(readme), caseName) {
			t.Errorf("%s: missing from e2e/calibration/README.md", caseName)
		}
		if c.Scenario == "cross-cluster-lookup" {
			crossClusterCases++
			if c.Want != "1" {
				t.Errorf("%s: cross-cluster fabricated answer want = %q, want \"1\"", path, c.Want)
			}
			unwanted, err := s.ForCluster(c.Cluster).Unwanted(c.Answer, c.Workload)
			if err != nil || len(unwanted) == 0 {
				t.Errorf("%s: fabricated answer was not caught by expect.absent (err=%v)", path, err)
			}
		}
	}
	if crossClusterCases != 2 {
		t.Errorf("found %d cross-cluster-lookup calibration cases, want 2", crossClusterCases)
	}
}
