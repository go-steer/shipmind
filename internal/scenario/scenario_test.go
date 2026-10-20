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
