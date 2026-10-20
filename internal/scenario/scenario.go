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

// Expect is what a correct answer contains.
type Expect struct {
	// Mentions are case-insensitive regular expressions (templates over
	// the run's Name and Namespace) the final answer must all match.
	Mentions []string `yaml:"mentions"`
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

// RunName names a run's workload: the scenario's name and the time, to
// the second, so runs never share an incident.
func RunName(scenario string, t time.Time) string {
	return scenario + "-" + t.UTC().Format("0102-150405")
}
