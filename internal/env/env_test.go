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

package env

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

const good = `context: demo
agent:
  atespace: agents
  project: example-project
  workspace: demo-ws
  image: registry.example.com/agent@` + digest + `
  clusters:
    - name: demo
      path: projects/example-project/locations/us-central1/clusters/demo
expect:
  - object: apps.v1.Deployment.ns.name
    reason: image moves to a digest
`

func TestParseGood(t *testing.T) {
	e, err := Parse(strings.NewReader(good))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if e.Context != "demo" || e.Agent.Clusters[0].Name != "demo" {
		t.Errorf("env = %+v", e)
	}
	if r, ok := e.Expected("apps.v1.Deployment.ns.name"); !ok || r != "image moves to a digest" {
		t.Errorf("Expected = %q, %v", r, ok)
	}
	for _, id := range []string{"secret lookout-ax/lookout-ax-agent", "workspace agents/demo-ws", "substrate DaemonSet/ate-system/atelet", "rollout Deployment/lookout-ax/lookout-ax", "rbac.authorization.k8s.io.v1.ClusterRole..lookout-ax-watch", "v1.Namespace..demo"} {
		doc := good + "  - object: " + id + "\n    reason: test\n"
		if _, err := Parse(strings.NewReader(doc)); err != nil {
			t.Errorf("expect %q rejected: %v", id, err)
		}
	}
	if _, ok := e.Expected("v1.Secret.ns.other"); ok {
		t.Error("Expected(other) = true")
	}
}

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct {
		name, doc, want string
	}{
		{"empty", "", "empty document"},
		{"unknown field", good + "bogus: 1\n", "bogus"},
		{"two documents", good + "---\ncontext: x\n", "more than one document"},
		{"context", strings.Replace(good, "context: demo", "context: ''", 1), "context"},
		{"atespace", strings.Replace(good, "atespace: agents", "atespace: Agents", 1), "agent.atespace"},
		{"project", strings.Replace(good, "project: example-project", "project: x", 1), "agent.project"},
		{"workspace", strings.Replace(good, "workspace: demo-ws", "workspace: demo_ws", 1), "agent.workspace"},
		{"image", strings.Replace(good, "@"+digest, ":latest", 1), "pinned by digest"},
		{"no clusters", strings.Replace(good, "  clusters:\n    - name: demo\n      path: projects/example-project/locations/us-central1/clusters/demo\n", "  clusters: []\n", 1), "at least one cluster"},
		{"cluster path", strings.Replace(good, "clusters/demo\n", "clusters/other\n", 1), "agent.clusters[0].path"},
		{"cluster name", strings.Replace(good, "- name: demo", "- name: Demo", 1), "agent.clusters[0].name"},
		{"duplicate cluster", strings.Replace(good, "expect:",
			"    - name: demo\n      path: projects/example-project/locations/us-central1/clusters/demo\nexpect:", 1),
			`agent.clusters: "demo" listed twice`},
		{"expect object", strings.Replace(good, "object: apps.v1.Deployment.ns.name", "object: apps/v1 Deployment", 1), "expect[0].object"},
		{"expect reason", strings.Replace(good, "reason: image moves to a digest", "reason: ''", 1), "say why"},
		{"expect agent id", strings.Replace(good, "object: apps.v1.Deployment.ns.name", "object: secret lookout-ax", 1), "expect[0].object"},
		{"substrate images", withSubstrate(strings.Replace(subOK, "images: r.example.com/s", "images: r.example.com/s:v1", 1)), "substrate.images"},
		{"substrate tag", withSubstrate(strings.Replace(subOK, "tag: v1", "tag: a/b", 1)), "substrate.tag"},
		{"substrate digests", withSubstrate(strings.Replace(subOK, "sha256:"+strings.Repeat("0", 64), "sha256:abc", 1)), "substrate.digests"},
		{"substrate no digests", withSubstrate(strings.Replace(subOK, "  digests: {atelet: sha256:"+strings.Repeat("0", 64)+"}\n", "", 1)), "substrate.digests"},
		{"substrate namespaces", withSubstrate(strings.Replace(subOK, "  namespaces: [ate-system]\n", "", 1)), "substrate.namespaces"},
		{"substrate namespace", withSubstrate(strings.Replace(subOK, "namespaces: [ate-system]", "namespaces: [ate-system, Ate]", 1)), "substrate.namespaces[1]"},
		{"substrate no workloads", withSubstrate(strings.Replace(subOK, "  workloads: [DaemonSet/ate-system/atelet]\n", "", 1)), "substrate.workloads"},
		{"substrate workload", withSubstrate(strings.Replace(subOK, "DaemonSet/ate-system/atelet", "Job/other/x", 1)), "substrate.workloads[0]"},
		{"slack app", withSlack(strings.Replace(slackOK, "app: shipmind-stage", "app: ''", 1)), "slack.app"},
		{"slack app long", withSlack(strings.Replace(slackOK, "app: shipmind-stage", "app: "+strings.Repeat("x", 36), 1)), "slack.app"},
		{"slack app spaces", withSlack(strings.Replace(slackOK, "app: shipmind-stage", "app: ' x'", 1)), "slack.app"},
		{"slack command", withSlack(strings.Replace(slackOK, "command: /shipmind-stage", "command: shipmind", 1)), "slack.command"},
		{"slack command case", withSlack(strings.Replace(slackOK, "command: /shipmind-stage", "command: /Shipmind", 1)), "slack.command"},
		{"slack description", withSlack(slackOK + "  description: " + strings.Repeat("x", 141) + "\n"), "slack.description"},
		{"duplicate expect", good + "  - object: apps.v1.Deployment.ns.name\n    reason: again\n", `"apps.v1.Deployment.ns.name" listed twice`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

const subOK = "substrate:\n  images: r.example.com/s\n  tag: v1\n  digests: {atelet: sha256:0000000000000000000000000000000000000000000000000000000000000000}\n  namespaces: [ate-system]\n  workloads: [DaemonSet/ate-system/atelet]\n"

func withSubstrate(sub string) string { return strings.Replace(good, "expect:", sub+"expect:", 1) }

func TestParseSubstrate(t *testing.T) {
	for _, doc := range []string{withSubstrate(subOK), withSubstrate(strings.Replace(subOK, "r.example.com/s", "localhost:5000/s", 1))} {
		e, err := Parse(strings.NewReader(doc))
		if err != nil || e.Substrate == nil || e.Substrate.Tag != "v1" {
			t.Fatalf("Parse = %+v, %v", e, err)
		}
	}
}

const slackOK = "slack:\n  app: shipmind-stage\n  command: /shipmind-stage\n"

func withSlack(sl string) string { return strings.Replace(good, "expect:", sl+"expect:", 1) }

func TestParseSlack(t *testing.T) {
	e, err := Parse(strings.NewReader(withSlack(slackOK + "  description: Chat with the staging agents\n")))
	if err != nil || e.Slack == nil || e.Slack.App != "shipmind-stage" || e.Slack.Command != "/shipmind-stage" ||
		e.Slack.Description != "Chat with the staging agents" {
		t.Fatalf("Parse = %+v, %v", e, err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := Load(dir)
	if err != nil || e.Dir != dir {
		t.Fatalf("Load = %+v, %v", e, err)
	}
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("Load(empty dir) succeeded")
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("context: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), FileName) {
		t.Errorf("Load(bad) = %v, want it to name the file", err)
	}
}
