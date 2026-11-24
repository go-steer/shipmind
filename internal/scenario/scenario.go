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

// Package scenario runs an end-to-end scenario against a deployed
// substrate profile: break a workload, let lookout open an incident and
// the agent diagnose it, score the answer, then check the platform's
// lifecycle (the task suspends when idle, and wakes with its session
// still in memory).
//
// A scenario is a directory under e2e/scenarios: a scenario.yaml (what
// to expect, how long to wait) and a workload template applied to break
// something. Every run names its workload uniquely, because lookout keys
// an incident's ax task on the workload: a reused name would land in the
// previous run's session, where the agent rightly answers "still failing,
// as diagnosed" instead of diagnosing.
package scenario

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"

	"go.yaml.in/yaml/v3"
)

// Scenario is a parsed scenario directory.
type Scenario struct {
	// Name is the directory's name.
	Name string `yaml:"-"`
	// Dir is the scenario directory.
	Dir string `yaml:"-"`
	// Namespace is where the workload is broken.
	Namespace string `yaml:"namespace"`
	// Workload is the template file, relative to Dir.
	Workload string `yaml:"workload"`
	Expect   Expect `yaml:"expect"`
	// Drain, when set, replaces the lifecycle's idle suspend with a worker
	// drain (#105): once the answer has settled and the workload is gone,
	// the worker pod hosting the task's actor is deleted, as a node drain
	// does, and the task must suspend for that reason.
	Drain  *Drain `yaml:"drain"`
	Limits Limits `yaml:"limits"`
	// Chat, when set, makes this a chat scenario (#127): instead of
	// breaking a workload and waiting for lookout's incident, the run
	// starts an agent task from a manifest it is given, asks it a
	// question and grades the answer (RunChat). It takes no workload.
	Chat *Chat `yaml:"chat"`

	// cluster is the cluster a chat run asks about (ForCluster).
	cluster string
}

// Chat is a chat scenario's question.
type Chat struct {
	// Ask is the message sent to the agent, a template over the run's
	// Name, Cluster (its full projects/.../clusters/... path) and
	// ClusterName.
	Ask string `yaml:"ask"`
	// Specialists are the subagents whose tool calls count toward the
	// trajectory: core-agent records a specialist's calls apart from the
	// main agent's, and a chat agent delegates its reads.
	Specialists []string `yaml:"specialists"`
}

// ForCluster returns a copy of s whose templates render for the cluster
// at path (projects/<project>/locations/<location>/clusters/<name>).
func (s *Scenario) ForCluster(path string) *Scenario {
	c := *s
	c.cluster = path
	return &c
}

// Drain is what a worker drain must do to a running task.
type Drain struct {
	// Reason is the task's AutoSuspended condition reason a drain must
	// leave (ax's WorkerDraining).
	Reason string `yaml:"reason"`
	// Within bounds the wait from the pod's deletion to the suspend.
	Within time.Duration `yaml:"within"`
}

// Expect is what a correct handling of the scenario looks like.
type Expect struct {
	// Mentions are case-insensitive regular expressions (templates over
	// the run's Name and Namespace) the final answer must all match.
	Mentions []string `yaml:"mentions"`
	// Absent are case-insensitive regular expressions, templates like
	// Mentions, none of which the final answer may match: what an answer
	// says when a tool could not reach or authenticate to the cluster.
	Absent []string `yaml:"absent"`
	// Results are checks on what tools returned, for a chat scenario. A
	// tool can succeed as a call and still report that it reached nothing
	// (leeway returns a report whose clusters carry status "error"), and
	// a model can write a confident answer over that.
	Results []ResultCheck `yaml:"results"`
	// Trajectory is how the agent may get there (#89), checked against
	// the session's tool calls and usage; nil checks nothing.
	Trajectory *Trajectory `yaml:"trajectory"`
	// Diagnosis is the ground truth a judge model grades the answer
	// against (#90): the root cause, the resources involved and the fix,
	// as a template over the run's Name and Namespace. Empty: no judge.
	Diagnosis string `yaml:"diagnosis"`
}

// ResultCheck is what one tool's successful results must not show.
type ResultCheck struct {
	// Tool names the tool, as a trajectory does (with or without its MCP
	// server prefix). It must have returned at least one result.
	Tool string `yaml:"tool"`
	// Absent are case-insensitive regular expressions none of that
	// tool's results may match.
	Absent []string `yaml:"absent"`
}

// CheckResults applies expect.results to a run's successful tool results
// and returns what failed.
func (s *Scenario) CheckResults(results []ToolResult) []string {
	var failures []string
	for _, check := range s.Expect.Results {
		seen := false
		for _, r := range results {
			if !toolMatches(r.Tool, check.Tool) {
				continue
			}
			seen = true
			if r.Method == MethodSummarized {
				failures = append(failures, fmt.Sprintf("%s returned a result the agent summarized with a model before recording it; it can't be checked", r.Tool))
				continue
			}
			for _, pattern := range check.Absent {
				if re, err := regexp.Compile("(?i)" + pattern); err == nil && re.MatchString(r.Output) {
					failures = append(failures, fmt.Sprintf("%s returned a result matching %s", r.Tool, pattern))
				}
			}
		}
		if !seen {
			failures = append(failures, fmt.Sprintf("%s returned no result to check", check.Tool))
		}
	}
	return failures
}

// Trajectory bounds the path to the answer, over the main agent's tool
// calls. core-agent records a specialist's calls apart from the session's,
// so an incident run (Run) checks the main agent's alone, which is where
// a folded profile's calls are, and a chat run (RunChat) adds those of the
// scenario's chat.specialists. Tool names match a call as named or behind
// one MCP server prefix: get_k8s_logs matches gke_get_k8s_logs. The
// bounds are tuned for core-agent's single-agent triage (about 3 tool and
// 4 model calls a run).
type Trajectory struct {
	// MustCall: each entry must be called at least once; "a|b" accepts
	// either.
	MustCall []string `yaml:"must_call"`
	// NeverCall: none of these may be called.
	NeverCall []string `yaml:"never_call"`
	// MaxToolCalls bounds the session's tool calls (0: no bound).
	MaxToolCalls int `yaml:"max_tool_calls"`
	// MaxModelCalls bounds its model calls, from the agent's usage (0:
	// no bound).
	MaxModelCalls int `yaml:"max_model_calls"`
}

// Limits bound each wait.
type Limits struct {
	// Incident: from breaking the workload to lookout's ax task.
	Incident time.Duration `yaml:"incident"`
	// Diagnose: from the task to the agent's answer.
	Diagnose time.Duration `yaml:"diagnose"`
	// Suspend: from removing the workload to the task suspending.
	Suspend time.Duration `yaml:"suspend"`
	// Start: for a chat scenario, from applying the task to its agent
	// accepting a session.
	Start time.Duration `yaml:"start"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Load reads dir/scenario.yaml and checks it.
func Load(dir string) (*Scenario, error) {
	b, err := os.ReadFile(filepath.Join(dir, "scenario.yaml")) //nolint:gosec // the operator's own scenario directory
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var s Scenario
	if err := dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: empty document", dir)
		}
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	s.Dir = dir
	s.Name = filepath.Base(filepath.Clean(dir))
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{dir}, args...)...))
	}
	if !nameRE.MatchString(s.Name) || len(s.Name) > 40 {
		fail("directory name %q: want a DNS label of at most 40 characters (runs append a timestamp)", s.Name)
	}
	if s.Chat != nil {
		s.checkChat(fail)
	} else {
		if len(s.Expect.Results) > 0 {
			fail("expect.results: only a chat scenario checks tool results")
		}
		if len(s.Expect.Absent) > 0 {
			fail("expect.absent: only a chat scenario checks what the answer must not say")
		}
		if !nameRE.MatchString(s.Namespace) {
			fail("namespace %q: want a namespace name", s.Namespace)
		}
		if s.Workload == "" || filepath.IsAbs(s.Workload) || strings.Contains(s.Workload, "..") {
			fail("workload %q: want a file in the scenario directory", s.Workload)
		} else if _, err := os.Stat(filepath.Join(dir, s.Workload)); err != nil {
			fail("workload: %v", err)
		}
	}
	if len(s.Expect.Mentions) == 0 {
		fail("expect.mentions: want at least one")
	}
	for _, m := range s.Expect.Mentions {
		if _, err := s.mention(m, "probe"); err != nil {
			fail("expect.mentions %q: %v", m, err)
		}
	}
	for _, m := range s.Expect.Absent {
		if _, err := s.mention(m, "probe"); err != nil {
			fail("expect.absent %q: %v", m, err)
		}
	}
	if s.Expect.Diagnosis != "" {
		if _, err := s.Diagnosis("probe"); err != nil {
			fail("expect.diagnosis: %v", err)
		}
	}
	if s.Expect.Trajectory != nil {
		if err := s.Expect.Trajectory.check(); err != nil {
			fail("%v", err)
		}
		known, err := loadTools(filepath.Join(filepath.Dir(dir), "tools.yaml"))
		if err != nil {
			fail("%v", err)
		}
		for _, name := range s.Expect.Trajectory.names() {
			if known != nil && !known[name] {
				fail("expect.trajectory: %q isn't a tool in %s", name, filepath.Join(filepath.Dir(dir), "tools.yaml"))
			}
		}
	}
	if s.Drain != nil && (strings.TrimSpace(s.Drain.Reason) == "" || s.Drain.Within <= 0) {
		fail("drain: want a reason and a positive within")
	}
	switch {
	case s.Chat != nil:
		if s.Limits.Start <= 0 || s.Limits.Diagnose <= 0 {
			fail("limits: a chat scenario wants start and diagnose, both positive")
		}
		if s.Limits.Incident != 0 || s.Limits.Suspend != 0 {
			fail("limits: incident and suspend are for an incident scenario")
		}
	case s.Limits.Incident <= 0 || s.Limits.Diagnose <= 0 || s.Limits.Suspend <= 0:
		fail("limits: want incident, diagnose and suspend, all positive")
	case s.Limits.Start != 0:
		fail("limits: start is for a chat scenario")
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &s, nil
}

// checkChat validates a chat scenario's own fields.
func (s *Scenario) checkChat(fail func(format string, args ...any)) {
	for _, check := range s.Expect.Results {
		if strings.TrimSpace(check.Tool) == "" || len(check.Absent) == 0 {
			fail("expect.results: each entry wants a tool and at least one absent pattern")
		}
		for _, pattern := range check.Absent {
			if _, err := regexp.Compile("(?i)" + pattern); err != nil {
				fail("expect.results %s %q: %v", check.Tool, pattern, err)
			}
		}
	}
	if s.Drain != nil || (s.Namespace != "") != (s.Workload != "") {
		fail("chat: a chat scenario breaks no workload: omit namespace, workload and drain (or set both namespace and workload for a seeded posture fixture)")
	}
	if s.Workload != "" {
		if !nameRE.MatchString(s.Namespace) {
			fail("namespace %q: want a namespace name", s.Namespace)
		}
		if filepath.IsAbs(s.Workload) || strings.Contains(s.Workload, "..") {
			fail("workload %q: want a file in the scenario directory", s.Workload)
		} else if _, err := os.Stat(filepath.Join(s.Dir, s.Workload)); err != nil {
			fail("workload: %v", err)
		}
	}
	if strings.TrimSpace(s.Chat.Ask) == "" {
		fail("chat.ask: want the question to send")
	} else if _, err := s.Ask("probe"); err != nil {
		fail("chat.ask: %v", err)
	}
	for _, sp := range s.Chat.Specialists {
		if !nameRE.MatchString(strings.TrimPrefix(sp, "_")) {
			fail("chat.specialists %q: want a specialist's name", sp)
		}
	}
}

// Ask renders a chat scenario's question for a run named name.
func (s *Scenario) Ask(name string) (string, error) {
	return fill(s.Chat.Ask, "ask", s.data(name))
}

// Unwanted returns the expect.absent patterns the answer matches, for a
// run named name.
func (s *Scenario) Unwanted(answer, name string) ([]string, error) {
	var found []string
	for _, m := range s.Expect.Absent {
		re, err := s.mention(m, name)
		if err != nil {
			return nil, err
		}
		if re.MatchString(answer) {
			found = append(found, re.String()[len("(?i)"):])
		}
	}
	return found, nil
}

func (s *Scenario) data(name string) map[string]string {
	cluster := s.cluster
	return map[string]string{
		"Name": name, "Namespace": s.Namespace,
		"Cluster": cluster, "ClusterName": cluster[strings.LastIndexByte(cluster, '/')+1:],
	}
}

func fill(src, name string, data map[string]string) (string, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(src)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Manifest renders the workload for a run named name.
func (s *Scenario) Manifest(name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(s.Dir, s.Workload)) //nolint:gosec // checked in Load
	if err != nil {
		return nil, err
	}
	out, err := fill(string(b), s.Workload, s.data(name))
	return []byte(out), err
}

func (s *Scenario) mention(m, name string) (*regexp.Regexp, error) {
	src, err := fill(m, "mention", s.data(name))
	if err != nil {
		return nil, err
	}
	return regexp.Compile("(?i)" + src)
}

// Score returns the expected mentions the answer lacks, for a run named
// name.
func (s *Scenario) Score(answer, name string) ([]string, error) {
	var missing []string
	for _, m := range s.Expect.Mentions {
		re, err := s.mention(m, name)
		if err != nil {
			return nil, err
		}
		if !re.MatchString(answer) {
			missing = append(missing, re.String()[len("(?i)"):])
		}
	}
	return missing, nil
}

// Diagnosis renders the ground-truth diagnosis for a run named name, or
// "" when the scenario has none.
func (s *Scenario) Diagnosis(name string) (string, error) {
	if s.Expect.Diagnosis == "" {
		return "", nil
	}
	return fill(s.Expect.Diagnosis, "diagnosis", s.data(name))
}

// RunName names a run's workload: the scenario's name and the time, to
// the second, so runs never share an incident.
func RunName(scenario string, t time.Time) string {
	return scenario + "-" + t.UTC().Format("0102-150405")
}

// check validates a trajectory as loaded.
func (t *Trajectory) check() error {
	for _, list := range [][]string{t.MustCall, t.NeverCall} {
		for _, entry := range list {
			for _, name := range strings.Split(entry, "|") {
				if strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t") {
					return fmt.Errorf("expect.trajectory: tool name %q: want names without spaces, alternatives as a|b", entry)
				}
			}
		}
	}
	if t.MaxToolCalls < 0 || t.MaxModelCalls < 0 {
		return fmt.Errorf("expect.trajectory: max_tool_calls and max_model_calls must not be negative")
	}
	return nil
}

// toolMatches reports whether a call (as the agent named it) is tool
// name, as is or behind one server prefix ("gke_"): "resource" doesn't
// match gke_get_k8s_resource.
func toolMatches(call, name string) bool {
	server, ok := strings.CutSuffix(call, "_"+name)
	return call == name || (ok && server != "" && !strings.Contains(server, "_"))
}

// names lists every tool name the trajectory mentions.
func (t *Trajectory) names() []string {
	var names []string
	for _, entry := range t.MustCall {
		names = append(names, strings.Split(entry, "|")...)
	}
	return append(names, t.NeverCall...)
}

// loadTools reads the scenarios' tool catalog, or nil when there is none.
func loadTools(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Tools []string `yaml:"tools"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil || len(doc.Tools) == 0 {
		return nil, fmt.Errorf("%s: want a list of tools (%v)", path, err)
	}
	known := map[string]bool{}
	for _, t := range doc.Tools {
		known[t] = true
	}
	return known, nil
}

// Check grades a session's tool calls, in order, and its model-call
// count (modelCalls < 0: unknown) against t, returning one line per
// failure. It is what a run and a re-grade of an archived run both use.
func (t *Trajectory) Check(calls []string, modelCalls int) (failures, notes []string) {
	called := func(name string) bool {
		for _, c := range calls {
			if toolMatches(c, name) {
				return true
			}
		}
		return false
	}
	for _, entry := range t.MustCall {
		alts := strings.Split(entry, "|")
		if !slices.ContainsFunc(alts, called) {
			failures = append(failures, fmt.Sprintf("never called %s", strings.Join(alts, " or ")))
		}
	}
	for _, name := range t.NeverCall {
		for _, c := range calls {
			if toolMatches(c, name) {
				failures = append(failures, fmt.Sprintf("called %s, which it must not", c))
				break
			}
		}
	}
	if t.MaxToolCalls > 0 && len(calls) > t.MaxToolCalls {
		failures = append(failures, fmt.Sprintf("%d tool calls, more than %d", len(calls), t.MaxToolCalls))
	}
	switch {
	case t.MaxModelCalls == 0:
	case modelCalls < 0:
		notes = append(notes, "max_model_calls not checked: the agent's usage wasn't read")
	case modelCalls > t.MaxModelCalls:
		failures = append(failures, fmt.Sprintf("%d model calls, more than %d", modelCalls, t.MaxModelCalls))
	}
	return failures, notes
}
