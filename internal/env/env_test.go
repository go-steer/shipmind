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
  incidentImage: registry.example.com/agent@` + digest + `
  clusters:
    home: projects/example-project/locations/us-central1/clusters/demo
    scope:
      - project: example-project
expect:
  - object: apps.v1.Deployment.ns.name
    reason: image moves to a digest
`

func TestParseGood(t *testing.T) {
	e, err := Parse(strings.NewReader(good))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if e.Context != "demo" || e.Agent.Clusters.HomeName() != "demo" || e.Agent.Clusters.HomeRegion() != "us-central1" {
		t.Errorf("env = %+v", e)
	}
	if r, ok := e.Expected("apps.v1.Deployment.ns.name"); !ok || r != "image moves to a digest" {
		t.Errorf("Expected = %q, %v", r, ok)
	}
	for _, id := range []string{
		"secret lookout-ax/lookout-ax-agent",
		"workspace agents/demo-ws",
		"substrate DaemonSet/ate-system/atelet",
		"rollout Deployment/lookout-ax/lookout-ax",
		"scope access",
		"scope project/example-project",
		"scope project/example-project/us-central1-b",
		"scope fleet/fleet-host-proj",
		"rbac.authorization.k8s.io.v1.ClusterRole..lookout-ax-watch",
		"v1.Namespace..demo",
	} {
		doc := good + "  - object: " + id + "\n    reason: test\n"
		if _, err := Parse(strings.NewReader(doc)); err != nil {
			t.Errorf("expect %q rejected: %v", id, err)
		}
	}
	withChat := strings.Replace(good, "  clusters:\n", "  chatImage: registry.example.com/agent-core@"+digest+"\n  clusters:\n", 1)
	if c, err := Parse(strings.NewReader(withChat)); err != nil || c.Agent.ChatImage != "registry.example.com/agent-core@"+digest {
		t.Errorf("chatImage: %+v, %v", c, err)
	}
	if e.Agent.ChatImage != "" {
		t.Errorf("chatImage %q without one set", e.Agent.ChatImage)
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
		{"legacy agent.image", strings.Replace(good, "  clusters:\n", "  image: registry.example.com/agent-mast@"+digest+"\n  clusters:\n", 1), "field image not found"},
		{"incident image", strings.Replace(good, "@"+digest, ":latest", 1), "agent.incidentImage"},
		{"missing incident image", strings.Replace(good, "  incidentImage: registry.example.com/agent@"+digest+"\n", "", 1), "agent.incidentImage"},
		{"idle not a duration", strings.Replace(good, "  clusters:\n", "  idleSuspendAfter: soon\n  clusters:\n", 1), "agent.idleSuspendAfter"},
		{"idle too short", strings.Replace(good, "  clusters:\n", "  idleSuspendAfter: 30s\n  clusters:\n", 1), "agent.idleSuspendAfter"},
		{"idle too long", strings.Replace(good, "  clusters:\n", "  idleSuspendAfter: 30m\n  clusters:\n", 1), "agent.idleSuspendAfter"},
		{"chat image", strings.Replace(good, "  clusters:\n", "  chatImage: registry.example.com/agent:latest\n  clusters:\n", 1), "agent.chatImage"},
		{"legacy list clusters", strings.Replace(good, "  clusters:\n    home: projects/example-project/locations/us-central1/clusters/demo\n    scope:\n      - project: example-project\n", "  clusters:\n    - name: demo\n      path: projects/example-project/locations/us-central1/clusters/demo\n", 1), "cannot unmarshal"},
		{"missing home", strings.Replace(good, "    home: projects/example-project/locations/us-central1/clusters/demo\n", "", 1), "agent.clusters.home"},
		{"bad home location", strings.Replace(good, "locations/us-central1/clusters/demo", "locations/bad/clusters/demo", 1), "agent.clusters.home"},
		{"no scope", strings.Replace(good, "    scope:\n      - project: example-project\n", "    scope: []\n", 1), "at least one scope entry"},
		{"region is zone", strings.Replace(good, "    scope:\n", "    regions: [us-central1-a]\n    scope:\n", 1), "agent.clusters.regions[0]"},
		{"duplicate region", strings.Replace(good, "    scope:\n", "    regions: [us-central1, us-central1]\n    scope:\n", 1), `agent.clusters.regions: "us-central1" listed twice`},
		{"scope empty entry", strings.Replace(good, "      - project: example-project\n", "      - {}\n", 1), "set exactly one of project, path, or fleet"},
		{"scope two kinds", strings.Replace(good, "      - project: example-project\n", "      - project: example-project\n        fleet: fleet-host\n", 1), "set exactly one of project, path, or fleet"},
		{"scope location without project", strings.Replace(good, "      - project: example-project\n", "      - fleet: fleet-host\n        location: us-central1\n", 1), "agent.clusters.scope[0].location: only valid with project"},
		{"scope bad project", strings.Replace(good, "      - project: example-project\n", "      - project: Bad\n", 1), "agent.clusters.scope[0].project"},
		{"scope bad location", strings.Replace(good, "      - project: example-project\n", "      - project: example-project\n        location: \"-\"\n", 1), "agent.clusters.scope[0].location"},
		{"scope bad path", strings.Replace(good, "      - project: example-project\n", "      - path: projects/x/locations/us-central1/clusters/demo\n", 1), "agent.clusters.scope[0].path"},
		{"scope bad fleet", strings.Replace(good, "      - project: example-project\n", "      - fleet: Bad\n", 1), "agent.clusters.scope[0].fleet"},
		{"scope duplicate project", strings.Replace(good, "expect:", "      - project: example-project\nexpect:", 1), "agent.clusters.scope: duplicate entry"},
		{"scope duplicate path", strings.Replace(good, "      - project: example-project\n", "      - path: projects/example-project/locations/us-central1/clusters/demo\n      - path: projects/example-project/locations/us-central1/clusters/demo\n", 1), "agent.clusters.scope: duplicate entry"},
		{"scope duplicate fleet", strings.Replace(good, "      - project: example-project\n", "      - fleet: fleet-host\n      - fleet: fleet-host\n", 1), "agent.clusters.scope: duplicate entry"},
		{"scope unknown label field", strings.Replace(good, "expect:", "        label: env=prod\nexpect:", 1), "field label not found"},
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
		{"slack alertChannel format", withSlack(slackOK + "  alertChannel: '#alerts'\n"), "slack.alertChannel"},
		{"secret name", withSecrets("  - secret: switchboard\n    keys: {a: b}\n"), "secrets[0].secret"},
		{"secret no keys", withSecrets("  - secret: switchboard/slack\n    keys: {}\n"), "want at least one key"},
		{"secret key", withSecrets("  - secret: switchboard/slack\n    keys: {'a b': c}\n"), "key \"a b\""},
		{"secret manager name", withSecrets("  - secret: switchboard/slack\n    keys: {a: 'projects/x/secrets/y'}\n"), "Secret Manager secret name"},
		{"secret project", withSecrets("  - secret: switchboard/slack\n    project: X\n    keys: {a: b}\n"), "secrets[0].project"},
		{"secret restart", withSecrets("  - secret: switchboard/slack\n    keys: {a: b}\n    restart: [Switchboard]\n"), "restart \"Switchboard\""},
		{"secret twice", withSecrets("  - secret: switchboard/slack\n    keys: {a: b}\n  - secret: switchboard/slack\n    keys: {a: b}\n"), "listed twice"},
		{"vcs without chatImage", strings.Replace(good, "expect:", "  vcs:\n    secret: gh-token\n    repositories:\n      - repository: org/repo\nexpect:", 1), "needs agent.chatImage"},
		{"vcs chatAutoApprove without chatImage", strings.Replace(good, "expect:", "  vcs:\n    secret: gh-token\n    incidentAutoPR: true\n    chatAutoApprove: true\n    repositories:\n      - repository: org/repo\nexpect:", 1), "agent.vcs.chatAutoApprove: needs agent.chatImage"},
		{"vcs bad secret", withVCS("    secret: Bad_Secret\n    repositories:\n      - repository: org/repo\n"), "agent.vcs.secret"},
		{"vcs no repos", withVCS("    secret: gh-token\n    repositories: []\n"), "at least one repository"},
		{"vcs bad repo", withVCS("    secret: gh-token\n    repositories:\n      - repository: org\n"), "agent.vcs.repositories[0].repository"},
		{"vcs dot traversal repo", withVCS("    secret: gh-token\n    repositories:\n      - repository: ../repo\n"), "agent.vcs.repositories[0].repository"},
		{"vcs dash repo", withVCS("    secret: gh-token\n    repositories:\n      - repository: -org/repo\n"), "agent.vcs.repositories[0].repository"},
		{"vcs dotgit repo", withVCS("    secret: gh-token\n    repositories:\n      - repository: org/repo.git\n"), "agent.vcs.repositories[0].repository"},
		{"vcs host prefix repo", withVCS("    secret: gh-token\n    repositories:\n      - repository: github.com/repo\n"), "agent.vcs.repositories[0].repository"},
		{"vcs duplicate repo", withVCS("    secret: gh-token\n    repositories:\n      - repository: org/repo\n      - repository: ORG/Repo\n"), "listed twice"},
		{"vcs bad branch", withVCS("    secret: gh-token\n    repositories:\n      - repository: org/repo\n        baseBranch: 'main branch'\n"), "agent.vcs.repositories[0].baseBranch"},
		{"vcs lock branch", withVCS("    secret: gh-token\n    repositories:\n      - repository: org/repo\n        baseBranch: main.lock\n"), "agent.vcs.repositories[0].baseBranch"},
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
	doc := withSlack(slackOK + "  description: Chat with the staging agents\n  alertChannel: C0123456789\n")
	e, err := Parse(strings.NewReader(doc))
	if err != nil || e.Slack == nil || e.Slack.App != "shipmind-stage" || e.Slack.Command != "/shipmind-stage" ||
		e.Slack.Description != "Chat with the staging agents" || e.Slack.AlertChannel != "C0123456789" {
		t.Fatalf("Parse = %+v, %v", e, err)
	}
}

func withSecrets(list string) string {
	return strings.Replace(good, "expect:", "secrets:\n"+list+"expect:", 1)
}

func TestParseSecrets(t *testing.T) {
	e, err := Parse(strings.NewReader(withSecrets("  - secret: switchboard/switchboard-slack\n    keys:\n      app-token: shipmind-stage-slack-app-token\n      bot-token: shipmind-stage-slack-bot-token\n    restart: [switchboard]\n")))
	if err != nil {
		t.Fatal(err)
	}
	m := e.Secrets[0]
	if m.Namespace() != "switchboard" || m.Name() != "switchboard-slack" || m.Keys["bot-token"] != "shipmind-stage-slack-bot-token" || m.Restart[0] != "switchboard" {
		t.Errorf("secret %+v", m)
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

func TestIdleSuspend(t *testing.T) {
	if got := (Agent{}).IdleSuspend(); got != DefaultIdleSuspendAfter {
		t.Errorf("unset: %q", got)
	}
	if got := (Agent{IdleSuspendAfter: "3m"}).IdleSuspend(); got != "3m" {
		t.Errorf("3m: %q", got)
	}
}

func withVCS(vcsBody string) string {
	doc := strings.Replace(good, "  clusters:\n", "  chatImage: registry.example.com/agent-core@"+digest+"\n  clusters:\n", 1)
	return strings.Replace(doc, "expect:", "  vcs:\n"+vcsBody+"expect:", 1)
}

func TestParseVCS(t *testing.T) {
	e, err := Parse(strings.NewReader(withVCS("    secret: gh-token\n    chatAutoApprove: true\n    repositories:\n      - repository: org/gitops\n      - repository: org/apps\n        baseBranch: release\n")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if e.Agent.VCS == nil || e.Agent.VCS.Secret != "gh-token" || !e.Agent.VCS.ChatAutoApprove || len(e.Agent.VCS.Repositories) != 2 {
		t.Fatalf("vcs = %+v", e.Agent.VCS)
	}
	if got := e.Agent.VCS.Repositories[0].Base(); got != "main" {
		t.Errorf("default Base() = %q, want main", got)
	}
	if got := e.Agent.VCS.Repositories[1].Base(); got != "release" {
		t.Errorf("explicit Base() = %q, want release", got)
	}
	incidentOnly := strings.Replace(good, "expect:", "  vcs:\n    secret: gh-token\n    incidentAutoPR: true\n    repositories:\n      - repository: org/gitops\nexpect:", 1)
	e2, err := Parse(strings.NewReader(incidentOnly))
	if err != nil || e2.Agent.VCS == nil || !e2.Agent.VCS.IncidentAutoPR {
		t.Fatalf("incidentAutoPR without chatImage: %+v, %v", e2, err)
	}
	shortIdle := strings.Replace(incidentOnly, "  clusters:\n", "  idleSuspendAfter: 3m\n  clusters:\n", 1)
	if _, err := Parse(strings.NewReader(shortIdle)); err == nil || !strings.Contains(err.Error(), "want at least 5m when agent.vcs.incidentAutoPR is true") {
		t.Errorf("incidentAutoPR with idleSuspendAfter=3m: err = %v", err)
	}
	okIdle := strings.Replace(incidentOnly, "  clusters:\n", "  idleSuspendAfter: 5m\n  clusters:\n", 1)
	if _, err := Parse(strings.NewReader(okIdle)); err != nil {
		t.Errorf("incidentAutoPR with idleSuspendAfter=5m: err = %v", err)
	}
}

func TestParseAudits(t *testing.T) {
	var nilAudits *Audits
	if nilAudits.CronSchedule() != DefaultAuditSchedule || nilAudits.RefreshString() != DefaultAuditRefreshWindow ||
		nilAudits.Refresh().Hours() != 168 || nilAudits.MinConsecutiveRuns() != DefaultAuditMinConsecutive ||
		nilAudits.DomainKeys() != DefaultAuditDomainUnavailableKeys {
		t.Fatal("nil Audits defaults mismatch")
	}

	e, err := Parse(strings.NewReader(strings.Replace(good, "expect:", "audits: {}\n"+slackOK+"  alertChannel: C0123456789\nexpect:", 1)))
	if err != nil || e.Audits == nil {
		t.Fatalf("Parse default audits: %+v, %v", e, err)
	}
	if e.Audits.CronSchedule() != DefaultAuditSchedule || e.Audits.RefreshString() != DefaultAuditRefreshWindow ||
		e.Audits.Refresh().Hours() != 168 || e.Audits.MinConsecutiveRuns() != 1 || e.Audits.DomainKeys() != "" {
		t.Errorf("default audits = %+v", e.Audits)
	}

	fullDoc := strings.Replace(good, "  clusters:\n", "  chatImage: registry.example.com/agent-core@"+digest+"\n  idleSuspendAfter: 6m\n  clusters:\n", 1)
	fullDoc = strings.Replace(fullDoc, "expect:", `  vcs:
    secret: gh-token
    repositories:
      - repository: org/gitops
audits:
  schedule: "*/30 1-5/2 * 1,6 0-5"
  suspend: true
  refreshWindow: 24h
  minConsecutive: 2
  exemptions: audits/exemptions.yaml
  domainUnavailableKeys: topology.kubernetes.io/zone,node-role
  autoPR: true
  runnerImage: registry.example.com/runner@`+digest+`
expect:`, 1)
	full, err := Parse(strings.NewReader(fullDoc))
	if err != nil || full.Audits == nil || !full.Audits.Suspend || !full.Audits.AutoPR {
		t.Fatalf("Parse full audits: %+v, %v", full, err)
	}
	if full.Audits.CronSchedule() != "*/30 1-5/2 * 1,6 0-5" || full.Audits.RefreshString() != "24h" ||
		full.Audits.Refresh().Hours() != 24 || full.Audits.MinConsecutiveRuns() != 2 ||
		full.Audits.DomainKeys() != "topology.kubernetes.io/zone,node-role" {
		t.Errorf("full audits = %+v", full.Audits)
	}

	// Explicit empty domainUnavailableKeys disables domain_unavailable.
	emptyKeys := strings.Replace(good, "expect:", "audits:\n  domainUnavailableKeys: \"\"\nexpect:", 1)
	if ek, err := Parse(strings.NewReader(emptyKeys)); err != nil || ek.Audits.DomainKeys() != "" {
		t.Fatalf("empty domainUnavailableKeys: %+v, %v", ek, err)
	}

	// audits.autoPR with agent.vcs + incidentImage alone (no chatImage) is valid.
	incAutoPR := strings.Replace(good, "expect:", "  vcs:\n    secret: gh-token\n    repositories:\n      - repository: org/gitops\naudits:\n  autoPR: true\nexpect:", 1)
	if _, err := Parse(strings.NewReader(incAutoPR)); err != nil {
		t.Errorf("audits.autoPR with incidentImage: %v", err)
	}

	for _, tc := range []struct {
		name, auditsYAML, extraAgent, want string
	}{
		{"bad cron field count", "  schedule: '0 5 * *'\n", "", "audits.schedule"},
		{"bad cron leading space", "  schedule: ' 0 5 * * *'\n", "", "audits.schedule"},
		{"bad cron syntax", "  schedule: 'abc 5 * * *'\n", "", "audits.schedule"},
		{"bad cron out of range", "  schedule: '60 5 * * *'\n", "", "audits.schedule"},
		{"bad cron reversed range", "  schedule: '10-5 5 * * *'\n", "", "audits.schedule"},
		{"bad cron high range", "  schedule: '5-70 5 * * *'\n", "", "audits.schedule"},
		{"refresh not duration", "  refreshWindow: 7d\n", "", "audits.refreshWindow"},
		{"refresh too short", "  refreshWindow: 30m\n", "", "audits.refreshWindow"},
		{"refresh too long", "  refreshWindow: 9000h\n", "", "audits.refreshWindow"},
		{"minConsecutive zero", "  minConsecutive: 0\n", "", "audits.minConsecutive"},
		{"minConsecutive high", "  minConsecutive: 11\n", "", "audits.minConsecutive"},
		{"exemptions abs", "  exemptions: /etc/exemptions.yaml\n", "", "audits.exemptions"},
		{"exemptions traversal", "  exemptions: ../exemptions.yaml\n", "", "audits.exemptions"},
		{"exemptions uncleaned", "  exemptions: ./exemptions.yaml\n", "", "audits.exemptions"},
		{"exemptions ext", "  exemptions: exemptions.json\n", "", "audits.exemptions"},
		{"domain keys bad label", "  domainUnavailableKeys: 'bad key'\n", "", "audits.domainUnavailableKeys"},
		{"domain keys bad prefix", "  domainUnavailableKeys: 'BAD_PREFIX/zone'\n", "", "audits.domainUnavailableKeys"},
		{"domain keys duplicate", "  domainUnavailableKeys: 'zone,zone'\n", "", "audits.domainUnavailableKeys"},
		{"runner image tag only", "  runnerImage: registry.example.com/runner:latest\n", "", "audits.runnerImage"},
		{"autoPR without vcs", "  autoPR: true\n", "  chatImage: registry.example.com/agent-core@" + digest + "\n", "audits.autoPR: needs agent.vcs"},
		{"autoPR short idle", "  autoPR: true\n", "  chatImage: registry.example.com/agent-core@" + digest + "\n  idleSuspendAfter: 3m\n", "want at least 5m when audits.autoPR is true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := good
			if tc.extraAgent != "" {
				doc = strings.Replace(doc, "  clusters:\n", tc.extraAgent+"  clusters:\n", 1)
			}
			if tc.name == "autoPR short idle" {
				doc = strings.Replace(doc, "expect:", "  vcs:\n    secret: gh-token\n    repositories:\n      - repository: org/gitops\naudits:\n"+tc.auditsYAML+"expect:", 1)
			} else {
				doc = strings.Replace(doc, "expect:", "audits:\n"+tc.auditsYAML+"expect:", 1)
			}
			if _, err := Parse(strings.NewReader(doc)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadAuditExemptionsFile(t *testing.T) {
	dir := t.TempDir()
	doc := strings.Replace(good, "expect:", "audits:\n  exemptions: exemptions.yaml\nexpect:", 1)
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	// Missing exemptions file fails Load.
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "audits.exemptions") {
		t.Fatalf("Load missing exemptions = %v", err)
	}
	// Directory in place of exemptions file fails Load.
	exPath := filepath.Join(dir, "exemptions.yaml")
	if err := os.Mkdir(exPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Load dir exemptions = %v", err)
	}
	if err := os.Remove(exPath); err != nil {
		t.Fatal(err)
	}
	// Regular file succeeds.
	if err := os.WriteFile(exPath, []byte("exemptions: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load valid exemptions = %v", err)
	}
}

func TestClustersScopeAndHelpers(t *testing.T) {
	doc := `context: demo
agent:
  atespace: agents
  project: example-project
  workspace: demo-ws
  incidentImage: registry.example.com/agent@` + digest + `
  clusters:
    home: projects/example-project/locations/us-central1-b/clusters/demo
    regions: [us-central1, europe-west1]
    scope:
      - project: example-project
      - project: team-a-prod
        location: europe-west1-c
      - path: projects/team-b-prod/locations/us-east1/clusters/prod
      - fleet: fleet-host-prod
audits:
  scope:
    - path: projects/example-project/locations/us-central1-b/clusters/demo
`
	e, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse rich scope: %v", err)
	}
	c := e.Agent.Clusters
	if c.HomeName() != "demo" || c.HomeRegion() != "us-central1" {
		t.Errorf("home name/region = %q/%q", c.HomeName(), c.HomeRegion())
	}
	if got := LocationRegion("us-central1"); got != "us-central1" {
		t.Errorf("LocationRegion(us-central1) = %q", got)
	}
	if got := LocationRegion("us-central1-b"); got != "us-central1" {
		t.Errorf("LocationRegion(us-central1-b) = %q", got)
	}
	if (Clusters{}).HomeRegion() != "" || (Clusters{}).HomeName() != "" {
		t.Error("empty Clusters should return empty HomeRegion/HomeName")
	}
	if !c.Scope[0].IsBroad() || c.Scope[1].IsBroad() || c.Scope[2].IsBroad() || !c.Scope[3].IsBroad() {
		t.Errorf("IsBroad flags unexpected for %+v", c.Scope)
	}
	for i, want := range []string{
		"project `example-project` (all locations)",
		"project `team-a-prod` (location `europe-west1-c`)",
		"cluster `projects/team-b-prod/locations/us-east1/clusters/prod`",
		"fleet `fleet-host-prod`",
	} {
		if got := c.Scope[i].Display(); got != want {
			t.Errorf("Scope[%d].Display() = %q, want %q", i, got, want)
		}
	}
	if got := (ClusterScope{}).Display(); got != "" {
		t.Errorf("empty Display() = %q", got)
	}

	// EffectiveAuditScope returns audits.scope override when set, else agent.clusters.scope.
	if got := e.EffectiveAuditScope(); len(got) != 1 || got[0].Path != c.Home {
		t.Errorf("EffectiveAuditScope override = %+v", got)
	}
	e.Audits.Scope = nil
	if got := e.EffectiveAuditScope(); len(got) != 4 {
		t.Errorf("EffectiveAuditScope fallback = %+v", got)
	}

	// Invalid audits.scope fails validation.
	badAuditScope := strings.Replace(good, "expect:", "audits:\n  scope:\n    - project: Bad\nexpect:", 1)
	if _, err := Parse(strings.NewReader(badAuditScope)); err == nil || !strings.Contains(err.Error(), "audits.scope[0].project") {
		t.Errorf("bad audits.scope = %v", err)
	}

	// ResolveCluster: empty -> home, full path -> direct, bare home/path -> resolved, unknown/ambiguous/no-home -> error.
	if got, err := c.ResolveCluster(""); err != nil || got != c.Home {
		t.Errorf("ResolveCluster('') = %q, %v", got, err)
	}
	const fullOther = "projects/fleet-member-1/locations/asia-east1/clusters/svc"
	if got, err := c.ResolveCluster(fullOther); err != nil || got != fullOther {
		t.Errorf("ResolveCluster(full) = %q, %v", got, err)
	}
	if got, err := c.ResolveCluster("demo"); err != nil || got != c.Home {
		t.Errorf("ResolveCluster(demo) = %q, %v", got, err)
	}
	if got, err := c.ResolveCluster("prod"); err != nil || got != "projects/team-b-prod/locations/us-east1/clusters/prod" {
		t.Errorf("ResolveCluster(prod) = %q, %v", got, err)
	}
	if _, err := c.ResolveCluster("missing"); err == nil || !strings.Contains(err.Error(), "pass a full") {
		t.Errorf("ResolveCluster(missing) = %v", err)
	}
	if _, err := (Clusters{}).ResolveCluster(""); err == nil || !strings.Contains(err.Error(), "agent.clusters.home") {
		t.Errorf("ResolveCluster(empty home) = %v", err)
	}
	ambig := Clusters{
		Home: "projects/example-project/locations/us-central1/clusters/prod",
		Scope: []ClusterScope{
			{Path: "projects/other-project/locations/europe-west1/clusters/prod"},
		},
	}
	if _, err := ambig.ResolveCluster("prod"); err == nil || !strings.Contains(err.Error(), "matches multiple paths") {
		t.Errorf("ResolveCluster(ambiguous) = %v", err)
	}

	// GCPRegions must be sorted, unique, and valid region names (no zones).
	if len(GCPRegions) < 40 {
		t.Fatalf("GCPRegions len = %d, want ~43", len(GCPRegions))
	}
	for i, r := range GCPRegions {
		if !regionRE.MatchString(r) {
			t.Errorf("GCPRegions[%d] %q does not match regionRE", i, r)
		}
		if i > 0 && GCPRegions[i-1] >= r {
			t.Errorf("GCPRegions not strictly sorted at %d: %q >= %q", i, GCPRegions[i-1], r)
		}
	}
}
