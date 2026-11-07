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
	Limits   Limits `yaml:"limits"`
}

// Expect is what a correct handling of the scenario looks like.
type Expect struct {
	// Mentions are case-insensitive regular expressions (templates over
	// the run's Name and Namespace) the final answer must all match.
	Mentions []string `yaml:"mentions"`
	// Trajectory is how the agent may get there (#89), checked against
	// the session's tool calls and usage; nil checks nothing.
	Trajectory *Trajectory `yaml:"trajectory"`
	// Diagnosis is the ground truth a judge model grades the answer
	// against (#90): the root cause, the resources involved and the fix,
	// as a template over the run's Name and Namespace. Empty: no judge.
	Diagnosis string `yaml:"diagnosis"`
}

// Trajectory bounds the path to the answer, over the main agent's tool
// calls: core-agent streams a specialist's calls in a session of its
// own, so a profile that delegates needs those merged in before it gets
// a trajectory. Tool names match a call as named or behind one MCP
// server prefix: get_k8s_logs matches gke_get_k8s_logs. The bounds are
// tuned for core-agent's single-agent triage (about 3 tool and 4 model
// calls a run).
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
	if !nameRE.MatchString(s.Namespace) {
		fail("namespace %q: want a namespace name", s.Namespace)
	}
	if s.Workload == "" || filepath.IsAbs(s.Workload) || strings.Contains(s.Workload, "..") {
		fail("workload %q: want a file in the scenario directory", s.Workload)
	} else if _, err := os.Stat(filepath.Join(dir, s.Workload)); err != nil {
		fail("workload: %v", err)
	}
	if len(s.Expect.Mentions) == 0 {
		fail("expect.mentions: want at least one")
	}
	for _, m := range s.Expect.Mentions {
		if _, err := s.mention(m, "probe"); err != nil {
			fail("expect.mentions %q: %v", m, err)
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
	if s.Limits.Incident <= 0 || s.Limits.Diagnose <= 0 || s.Limits.Suspend <= 0 {
		fail("limits: want incident, diagnose and suspend, all positive")
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Scenario) data(name string) map[string]string {
	return map[string]string{"Name": name, "Namespace": s.Namespace}
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
