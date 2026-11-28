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

// Package agent renders the SRE agent's ax objects for an environment:
// the task template lookout (and switchboard) stamp a task from, the
// Workspace every task mounts, and switchboard's configuration. The templates live in
// agents/sre-triage/ax/ in a shipmind checkout.
package agent

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/go-steer/shipmind/internal/env"
)

// Dir holds the templates, relative to a shipmind checkout.
const Dir = "agents/sre-triage/ax"

// Tokens holds the bearer token pair stored in Secret
// lookout-ax/agent-tokens. Attach is mounted into task-core-agent.yaml.tmpl
// as CORE_AGENT_ATTACH_TOKEN (the session API lookout and switchboard
// speak); Inject is retained in the Secret schema for zero-drift
// compatibility with existing clusters (docs/console-design.md#CON-3).
type Tokens struct {
	Inject string
	Attach string
}

var tokenRE = regexp.MustCompile(`^[0-9a-f]{32,128}$`)

// Check reports whether both tokens are present and well formed.
func (t Tokens) Check() error {
	if !tokenRE.MatchString(t.Inject) {
		return fmt.Errorf("inject token: want 32 to 128 lowercase hex characters")
	}
	if err := t.CheckAttach(); err != nil {
		return err
	}
	if t.Inject == t.Attach {
		return fmt.Errorf("inject and attach tokens are the same")
	}
	return nil
}

// CheckAttach checks the attach token alone: all a core-agent task
// template carries.
func (t Tokens) CheckAttach() error {
	if !tokenRE.MatchString(t.Attach) {
		return fmt.Errorf("attach token: want 32 to 128 lowercase hex characters")
	}
	return nil
}

// NewTokens returns two fresh random tokens.
func NewTokens() (Tokens, error) {
	gen := func() (string, error) {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		return hex.EncodeToString(b), nil
	}
	inject, err := gen()
	if err != nil {
		return Tokens{}, err
	}
	attach, err := gen()
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{Inject: inject, Attach: attach}, nil
}

// The recipes the agent image carries that a task runs: one incident per
// task for lookout (gke-triage-f), one watchboard task per cluster for
// lookout's warning digests (gke-watchboard), and a conversation per task
// for switchboard (gke-chat), all authored as native flat core-agent
// recipes in images/agent.
const (
	IncidentBundle   = "gke-triage-f"
	ChatBundle       = "gke-chat"
	WatchboardBundle = "gke-watchboard"

	// WatchboardIdleSuspendAfter bounds how long lookout's watchboard task
	// stays awake between batched digest flushes (lookout flushes every
	// 60s by default), freeing its Substrate worker slot quickly instead of
	// holding a slot for agent.idleSuspendAfter (default 10m). Strictly
	// greater than gke-watchboard's 90s max_wallclock_seconds (30s
	// headroom) so ax never suspends an unwatched turn mid-flight.
	WatchboardIdleSuspendAfter = "2m"
)

// Task renders the core-agent ax task template lookout stamps an incident
// task from, for e with tokens t (using agent.incidentImage). When
// agent.vcs.incidentAutoPR or slack.alertChannel is set, it points -c at
// the Workspace's incident-config.json and adds the matching egress rules.
func Task(root string, e *env.Env, t Tokens) ([]byte, error) {
	if e.Agent.IncidentImage == "" {
		return nil, fmt.Errorf("incident task: agent.incidentImage is required")
	}
	autoPR := e.Agent.VCS != nil && e.Agent.VCS.IncidentAutoPR
	alert := e.Slack != nil && e.Slack.AlertChannel != ""
	cfgPath, vcsSecret := "", ""
	if autoPR || alert {
		cfgPath = "/workspace/repo/incident-config.json"
	}
	if autoPR {
		vcsSecret = e.Agent.VCS.Secret
	}
	return coreAgentTask(root, e, t, IncidentBundle, e.Agent.IncidentImage, cfgPath, vcsSecret, alert, e.Agent.IdleSuspend())
}

// WatchboardTask renders the core-agent ax task template lookout stamps
// its per-cluster watchboard task (lookout-wb-<cluster>-<hash8>) from,
// running the dedicated read-only gke-watchboard recipe on
// agent.incidentImage (#133). When slack.alertChannel is set, it points -c
// at the Workspace's watchboard-config.json and enables switchboard
// outbound ingress. Watchboard tasks never carry VCS credentials, file
// tools, or propose-change even when agent.vcs.incidentAutoPR is true, and
// use WatchboardIdleSuspendAfter (2m) to free their worker slot quickly
// between batched flushes.
func WatchboardTask(root string, e *env.Env, t Tokens) ([]byte, error) {
	if e.Agent.IncidentImage == "" {
		return nil, fmt.Errorf("watchboard task: agent.incidentImage is required")
	}
	alert := e.Slack != nil && e.Slack.AlertChannel != ""
	cfgPath := ""
	if alert {
		cfgPath = "/workspace/repo/watchboard-config.json"
	}
	return coreAgentTask(root, e, t, WatchboardBundle, e.Agent.IncidentImage, cfgPath, "", alert, WatchboardIdleSuspendAfter)
}

// ChatTask renders the core-agent task template switchboard starts a
// thread's task from (using the native gke-chat recipe in images/agent).
// Requires agent.chatImage. When agent.vcs is set, it points -c at the
// Workspace's chat-config.json and adds GitHub egress rules so the single
// general-purpose chat agent can also delegate GitOps pull requests to
// propose-change.
func ChatTask(root string, e *env.Env, t Tokens) ([]byte, error) {
	if e.Agent.ChatImage == "" {
		return nil, fmt.Errorf("chat task: agent.chatImage is required")
	}
	cfgPath, vcsSecret := "", ""
	if e.Agent.VCS != nil {
		cfgPath = "/workspace/repo/chat-config.json"
		vcsSecret = e.Agent.VCS.Secret
	}
	return coreAgentTask(root, e, t, ChatBundle, e.Agent.ChatImage, cfgPath, vcsSecret, false, e.Agent.IdleSuspend())
}

// AuditTask renders the ax task template the scheduled posture runner
// stamps a short-lived remediation task from when e.Audits.AutoPR is set.
func AuditTask(root string, e *env.Env, t Tokens) ([]byte, error) {
	if e.Audits == nil || !e.Audits.AutoPR || e.Agent.VCS == nil {
		return nil, nil
	}
	image := e.Agent.ChatImage
	if image == "" {
		image = e.Agent.IncidentImage
	}
	if image == "" {
		return nil, fmt.Errorf("audits.autoPR: needs agent.chatImage or agent.incidentImage")
	}
	return coreAgentTask(root, e, t, ChatBundle, image, "/workspace/repo/audit-config.json", e.Agent.VCS.Secret, false, e.Agent.IdleSuspend())
}

// coreAgentTask renders core-agent's task template for bundle from image.
func coreAgentTask(root string, e *env.Env, t Tokens, bundle, image, cfgPath, vcsSecret string, switchboardIngress bool, idleSuspend string) ([]byte, error) {
	if err := t.Check(); err != nil {
		return nil, err
	}
	return render(root, "task-core-agent.yaml.tmpl", map[string]any{
		"Bundle":             bundle,
		"Atespace":           e.Agent.Atespace,
		"Image":              image,
		"Project":            e.Agent.Project,
		"Workspace":          e.Agent.Workspace,
		"AttachToken":        t.Attach,
		"IdleSuspend":        idleSuspend,
		"ConfigPath":         cfgPath,
		"GoogleHosts":        googleEgressHosts(e.Agent.Clusters),
		"PostureScope":       postureScope(bundle, e.Agent.Project, e.Agent.Clusters),
		"EnableFileTools":    vcsSecret != "",
		"VCSSecret":          vcsSecret,
		"SwitchboardIngress": switchboardIngress,
	})
}

func googleEgressHosts(clusters []env.Cluster) string {
	hosts := []string{"aiplatform.googleapis.com", "container.googleapis.com"}
	var wildcards []string
	for _, c := range clusters {
		parts := strings.Split(c.Path, "/")
		if len(parts) == 6 && parts[0] == "projects" && parts[2] == "locations" && parts[3] != "" {
			w := fmt.Sprintf("%q", "*."+parts[3]+".gke.goog")
			if !slices.Contains(wildcards, w) {
				wildcards = append(wildcards, w)
			}
		}
	}
	slices.Sort(wildcards)
	hosts = append(hosts, wildcards...)
	return strings.Join(hosts, ", ")
}

// postureScope is the project list leeway mcp sweeps when a report names
// no cluster, for a chat task whose clusters span more than the agent's
// own project: that project first, then the others, comma-separated. It
// defaults to the agent's project alone, so an environment whose clusters
// all live there gets "" and no setting.
func postureScope(bundle, project string, clusters []env.Cluster) string {
	if bundle != ChatBundle {
		return ""
	}
	var others []string
	for _, c := range clusters {
		parts := strings.Split(c.Path, "/")
		if len(parts) != 6 || parts[0] != "projects" || parts[1] == "" || parts[1] == project {
			continue
		}
		if !slices.Contains(others, parts[1]) {
			others = append(others, parts[1])
		}
	}
	if len(others) == 0 {
		return ""
	}
	slices.Sort(others)
	return strings.Join(append([]string{project}, others...), ",")
}

const proposeChangeInstructions = `@include builtin:propose-change

## Deduplication and fast-exit guards

- If the caller's brief already names the target repository and base branch, skip ` + "`vcs_capabilities`" + ` and start directly with ` + "`vcs_proposal_list`" + `.
- Before cloning or editing, call ` + "`vcs_proposal_list`" + ` (with ` + "`state: \"open\"`" + `) on the target repository. If an open pull request already addresses the same workload and root cause, do not push a duplicate branch or open a second pull request — return the existing pull request number and URL immediately.
- After ` + "`vcs_clone`" + `, use ` + "`grep`" + ` to locate the failing workload or resource in the cloned repository. If the workload or resource does not exist in the repository (for example, an ephemeral test deployment or unmanaged workload), stop immediately without editing or committing and report that the workload is not managed in the configured GitOps repository.
- Never fabricate secret values, API keys, passwords, tokens, or external service URLs/connection strings. If fixing the issue requires an unknown value that is not derivable from the repository or diagnosis, stop without committing and report what operator-supplied value is needed.
`

var proposeChangeTools = []string{
	"read_file",
	"edit_file",
	"write_file",
	"grep",
	"vcs_capabilities",
	"vcs_clone",
	"vcs_status",
	"vcs_diff",
	"vcs_log",
	"vcs_commit",
	"vcs_publish",
	"vcs_propose",
	"vcs_proposal_view",
	"vcs_proposal_list",
}

func loadBaseConfig(root, bundle string) (map[string]any, error) {
	p := filepath.Join(root, "agents/sre-triage", bundle, ".agents", "config.json")
	base, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(base, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return doc, nil
}

func buildVCSBlock(vcs *env.VCS) map[string]any {
	type repo struct {
		Forge      string `json:"forge"`
		Repository string `json:"repository"`
		Role       string `json:"role"`
		BaseBranch string `json:"base_branch"`
	}
	role := "gitops"
	if len(vcs.Repositories) > 1 {
		role = "managed"
	}
	repos := make([]repo, 0, len(vcs.Repositories))
	for _, r := range vcs.Repositories {
		repos = append(repos, repo{
			Forge:      "github",
			Repository: r.Repository,
			Role:       role,
			BaseBranch: r.Base(),
		})
	}
	return map[string]any{
		"workspace_dir": "/workspace/repos",
		"forges": []map[string]any{{ //nolint:gosec // token_env names the environment variable, not a credential
			"name":      "github",
			"provider":  "github",
			"token_env": "CORE_AGENT_GITHUB_TOKEN",
			"commit_identity": map[string]string{
				"name":  "shipmind[bot]",
				"email": "shipmind[bot]@users.noreply.github.com",
			},
		}},
		"repositories": repos,
	}
}

func proposeChangeSpec() map[string]any {
	return map[string]any{
		"name":         "propose-change",
		"description":  "Clones a configured GitOps repository, edits Kubernetes manifests or configuration files, pushes a branch, and opens a pull request.",
		"instructions": proposeChangeInstructions,
		"mode":         "chat",
		"capability":   "change_executor",
		"tools":        proposeChangeTools,
		"mcp":          []string{},
		"skills":       []string{},
		"budgets": map[string]any{
			"max_turns":             15,
			"max_wallclock_seconds": 180,
		},
	}
}

func appendPermissionsAllow(doc map[string]any, patterns ...string) {
	perms, _ := doc["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
		doc["permissions"] = perms
	}
	var allow []string
	switch raw := perms["allow"].(type) {
	case []any:
		for _, v := range raw {
			if s, ok := v.(string); ok {
				allow = append(allow, s)
			}
		}
	case []string:
		allow = append(allow, raw...)
	}
	for _, p := range patterns {
		if !slices.Contains(allow, p) {
			allow = append(allow, p)
		}
	}
	perms["allow"] = allow
}

// ChatConfig renders the core-agent config JSON written to the Workspace's
// chat-config.json when e.Agent.ChatImage and e.Agent.VCS are configured:
// agents/sre-triage/gke-chat/.agents/config.json plus the environment's
// vcs block and propose-change subagent. Local sandbox file edits and
// vcs_commit are allowlisted; vcs_publish and vcs_propose remain in ask
// mode by default so switchboard prompts in Slack before any branch push
// or pull request creation, unless e.Agent.VCS.ChatAutoApprove opts into
// allowlisting them.
func ChatConfig(root string, e *env.Env) ([]byte, error) {
	if e.Agent.ChatImage == "" || e.Agent.VCS == nil {
		return nil, nil
	}
	doc, err := loadBaseConfig(root, ChatBundle)
	if err != nil {
		return nil, err
	}
	doc["vcs"] = buildVCSBlock(e.Agent.VCS)
	doc["subagents"] = []any{proposeChangeSpec()}
	appendPermissionsAllow(doc,
		"spawn_agent:propose-change",
		"edit_file:/workspace/repos/*",
		"write_file:/workspace/repos/*",
		"vcs_commit:*",
	)
	if e.Agent.VCS.ChatAutoApprove {
		appendPermissionsAllow(doc,
			"vcs_publish:*",
			"vcs_propose:*",
		)
	}
	return json.MarshalIndent(doc, "", "  ")
}

func switchboardAlertBlock(channel, description string) map[string]any {
	return map[string]any{
		"rate_limit_per_target": "5/min",
		"targets": []map[string]any{{
			"name":         "slack",
			"kind":         "webhook",
			"template":     "switchboard",
			"conversation": channel,
			"url":          "http://switchboard.switchboard.svc.cluster.local:8080/v1/messages",
			"auth": map[string]string{ //nolint:gosec // bearer_env names the environment variable, not a credential
				"bearer_env": "CORE_AGENT_SWITCHBOARD_TOKEN",
			},
			"description": description,
		}},
	}
}

// IncidentConfig renders the core-agent config JSON written to the
// Workspace's incident-config.json when agent.vcs.incidentAutoPR or
// slack.alertChannel is set: agents/sre-triage/gke-triage-f/.agents/config.json
// plus optional vcs / propose-change and/or switchboard alert target, with
// headless permissions allowlisted for those declared actions.
func IncidentConfig(root string, e *env.Env) ([]byte, error) {
	autoPR := e.Agent.VCS != nil && e.Agent.VCS.IncidentAutoPR
	alert := e.Slack != nil && e.Slack.AlertChannel != ""
	if !autoPR && !alert {
		return nil, nil
	}
	doc, err := loadBaseConfig(root, IncidentBundle)
	if err != nil {
		return nil, err
	}
	agentCfg, _ := doc["agent"].(map[string]any)
	if agentCfg == nil {
		agentCfg = map[string]any{}
		doc["agent"] = agentCfg
	}
	if autoPR {
		agentCfg["max_steps"] = 12
		agentCfg["max_wallclock_seconds"] = 240
		doc["vcs"] = buildVCSBlock(e.Agent.VCS)
		doc["subagents"] = []any{proposeChangeSpec()}
		appendPermissionsAllow(doc,
			"spawn_agent:propose-change",
			"edit_file:/workspace/repos/*",
			"write_file:/workspace/repos/*",
			"vcs_commit:*",
			"vcs_publish:*",
			"vcs_propose:*",
		)
	} else {
		agentCfg["max_steps"] = 8
	}
	if alert {
		doc["alerts"] = switchboardAlertBlock(e.Slack.AlertChannel, "Posts incident diagnoses and pull request links to the on-call Slack channel through switchboard.")
		appendPermissionsAllow(doc, "alert:slack")
	}
	return json.MarshalIndent(doc, "", "  ")
}

// WatchboardConfig renders the core-agent config JSON written to the
// Workspace's watchboard-config.json when slack.alertChannel is set:
// agents/sre-triage/gke-watchboard/.agents/config.json plus the
// switchboard Slack alert target and alert:slack allowlist. Watchboard
// tasks never receive vcs, propose-change, or elevated step budgets even
// when agent.vcs.incidentAutoPR is true (#133).
func WatchboardConfig(root string, e *env.Env) ([]byte, error) {
	if e.Slack == nil || e.Slack.AlertChannel == "" {
		return nil, nil
	}
	doc, err := loadBaseConfig(root, WatchboardBundle)
	if err != nil {
		return nil, err
	}
	doc["alerts"] = switchboardAlertBlock(e.Slack.AlertChannel, "Posts actionable watchboard warning digest summaries to the on-call Slack channel through switchboard.")
	appendPermissionsAllow(doc, "alert:slack")
	return json.MarshalIndent(doc, "", "  ")
}

func ensureSubmap(doc map[string]any, key string) map[string]any {
	m, _ := doc[key].(map[string]any)
	if m == nil {
		m = map[string]any{}
		doc[key] = m
	}
	return m
}

// AuditConfig renders the core-agent config JSON written to the Workspace's
// audit-config.json when e.Audits.AutoPR is enabled: headless permissions
// allowlisted for propose-change (with vcs_proposal_list open-PR dedup).
func AuditConfig(root string, e *env.Env) ([]byte, error) {
	if e.Audits == nil || !e.Audits.AutoPR || e.Agent.VCS == nil || (e.Agent.ChatImage == "" && e.Agent.IncidentImage == "") {
		return nil, nil
	}
	doc, err := loadBaseConfig(root, ChatBundle)
	if err != nil {
		return nil, err
	}
	agentCfg := ensureSubmap(doc, "agent")
	agentCfg["max_steps"] = 15
	agentCfg["max_wallclock_seconds"] = 240
	ensureSubmap(doc, "permissions")["approval_timeout"] = "30s"
	doc["vcs"] = buildVCSBlock(e.Agent.VCS)
	doc["subagents"] = []any{proposeChangeSpec()}
	appendPermissionsAllow(doc,
		"spawn_agent:propose-change",
		"edit_file:/workspace/repos/*",
		"write_file:/workspace/repos/*",
		"vcs_commit:*",
		"vcs_publish:*",
		"vcs_propose:*",
	)
	return json.MarshalIndent(doc, "", "  ")
}

func alertChannel(e *env.Env) string {
	if e.Slack == nil {
		return ""
	}
	return e.Slack.AlertChannel
}

// SwitchboardConfig renders switchboard's configuration for e: its Slack
// gateway fronting the single general-purpose agent ("triage") as an ax
// agent, a task per thread, started from the task template at
// /etc/switchboard/triage.yaml, plus outbound HTTP ingress on :8080 when
// slack.alertChannel is set.
func SwitchboardConfig(root string, e *env.Env) ([]byte, error) {
	b, err := render(root, "switchboard.json.tmpl", map[string]any{
		"Atespace":     e.Agent.Atespace,
		"AlertChannel": alertChannel(e),
	})
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("%s: not JSON once rendered", filepath.Join(root, Dir, "switchboard.json.tmpl"))
	}
	return b, nil
}

// AgentsMD renders the Workspace's AGENTS.md for e.
func AgentsMD(root string, e *env.Env) ([]byte, error) {
	var repos []env.Repository
	autoPR := false
	if e.Agent.VCS != nil {
		repos = e.Agent.VCS.Repositories
		autoPR = e.Agent.VCS.IncidentAutoPR
	}
	return render(root, "AGENTS.md.tmpl", map[string]any{
		"Clusters":       e.Agent.Clusters,
		"Repositories":   repos,
		"IncidentAutoPR": autoPR,
		"AlertChannel":   alertChannel(e),
	})
}

// Workspace renders the ax Workspace for e.
func Workspace(root string, e *env.Env) ([]byte, error) {
	md, err := AgentsMD(root, e)
	if err != nil {
		return nil, err
	}
	chatCfg, err := ChatConfig(root, e)
	if err != nil {
		return nil, err
	}
	incCfg, err := IncidentConfig(root, e)
	if err != nil {
		return nil, err
	}
	wbCfg, err := WatchboardConfig(root, e)
	if err != nil {
		return nil, err
	}
	audCfg, err := AuditConfig(root, e)
	if err != nil {
		return nil, err
	}
	return render(root, "workspace.yaml.tmpl", map[string]string{
		"Atespace":         e.Agent.Atespace,
		"Workspace":        e.Agent.Workspace,
		"AgentsMD":         string(md),
		"ChatConfig":       string(chatCfg),
		"IncidentConfig":   string(incCfg),
		"WatchboardConfig": string(wbCfg),
		"AuditConfig":      string(audCfg),
	})
}

func render(root, name string, data any) ([]byte, error) {
	p := filepath.Join(root, Dir, name)
	src, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	t, err := template.New(name).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return b.Bytes(), nil
}
