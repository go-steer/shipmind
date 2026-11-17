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
	"text/template"

	"github.com/go-steer/shipmind/internal/env"
)

// Dir holds the templates, relative to a shipmind checkout.
const Dir = "agents/sre-triage/ax"

// Tokens are the agent's two bearer tokens: inject (POST /inject, how
// signals arrive) and attach (the session API lookout and switchboard
// speak).
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

// The bundles the agent images carry (images/agent-mast, images/agent) that a task
// runs: one incident per task for lookout, a conversation per task for
// switchboard. Budgets differ most: mast meters a session for its life,
// and a conversation is one long session.
const (
	IncidentBundle = "gke-triage-f"
	ChatBundle     = "gke-chat"
)

// Task renders the ax task template lookout stamps an incident task
// from, for e with tokens t. With agent.incidentImage set, it is
// core-agent's template and image instead of mast's (#48); when
// agent.vcs.incidentAutoPR or slack.alertChannel is set, it points -c at
// the Workspace's incident-config.json and adds the matching egress rules.
func Task(root string, e *env.Env, t Tokens) ([]byte, error) {
	if e.Agent.IncidentImage != "" {
		autoPR := e.Agent.VCS != nil && e.Agent.VCS.IncidentAutoPR
		alert := e.Slack != nil && e.Slack.AlertChannel != ""
		cfgPath, vcsSecret := "", ""
		if autoPR || alert {
			cfgPath = "/workspace/repo/incident-config.json"
		}
		if autoPR {
			vcsSecret = e.Agent.VCS.Secret
		}
		return coreAgentTask(root, e, t, IncidentBundle, e.Agent.IncidentImage, cfgPath, vcsSecret, alert)
	}
	return task(root, e, t, IncidentBundle)
}

// ChatTask renders the task template switchboard starts a thread's task
// from: the same agent, image and tokens, with the conversation bundle.
// With agent.chatImage set, it is core-agent's template and image
// instead (#48); when agent.vcs is set, it points -c at the Workspace's
// chat-config.json and adds GitHub egress rules so the single general-purpose
// chat agent can also delegate GitOps pull requests to propose-change.
func ChatTask(root string, e *env.Env, t Tokens) ([]byte, error) {
	if e.Agent.ChatImage != "" {
		cfgPath, vcsSecret := "", ""
		if e.Agent.VCS != nil {
			cfgPath = "/workspace/repo/chat-config.json"
			vcsSecret = e.Agent.VCS.Secret
		}
		return coreAgentTask(root, e, t, ChatBundle, e.Agent.ChatImage, cfgPath, vcsSecret, false)
	}
	return task(root, e, t, ChatBundle)
}

// coreAgentTask renders core-agent's task template for bundle from image.
func coreAgentTask(root string, e *env.Env, t Tokens, bundle, image, cfgPath, vcsSecret string, switchboardIngress bool) ([]byte, error) {
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
		"IdleSuspend":        e.Agent.IdleSuspend(),
		"ConfigPath":         cfgPath,
		"EnableFileTools":    vcsSecret != "",
		"VCSSecret":          vcsSecret,
		"SwitchboardIngress": switchboardIngress,
	})
}

func task(root string, e *env.Env, t Tokens, bundle string) ([]byte, error) {
	if err := t.Check(); err != nil {
		return nil, err
	}
	return render(root, "task.yaml.tmpl", map[string]string{
		"Bundle":      bundle,
		"Atespace":    e.Agent.Atespace,
		"Image":       e.Agent.Image,
		"Project":     e.Agent.Project,
		"Workspace":   e.Agent.Workspace,
		"InjectToken": t.Inject,
		"AttachToken": t.Attach,
		"IdleSuspend": e.Agent.IdleSuspend(),
	})
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
	p := filepath.Join(root, Dir, "core-agent-"+bundle+".base.json")
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
// core-agent-gke-chat.base.json (checked at Docker build time against
// imported gke-chat) plus the environment's vcs block and propose-change
// specialist. Local sandbox file edits and vcs_commit are allowlisted,
// while vcs_publish and vcs_propose remain in ask mode so switchboard
// prompts in Slack before any branch push or pull request creation.
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
	return json.MarshalIndent(doc, "", "  ")
}

// IncidentConfig renders the core-agent config JSON written to the
// Workspace's incident-config.json when e.Agent.IncidentImage is set
// alongside agent.vcs.incidentAutoPR or slack.alertChannel:
// core-agent-gke-triage-f.base.json (checked at Docker build time against
// imported gke-triage-f) plus optional vcs / propose-change and/or
// switchboard alert target, with headless permissions allowlisted for
// those declared actions.
func IncidentConfig(root string, e *env.Env) ([]byte, error) {
	autoPR := e.Agent.VCS != nil && e.Agent.VCS.IncidentAutoPR
	alert := e.Slack != nil && e.Slack.AlertChannel != ""
	if e.Agent.IncidentImage == "" || (!autoPR && !alert) {
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
		doc["alerts"] = map[string]any{
			"rate_limit_per_target": "5/min",
			"targets": []map[string]any{{
				"name":         "slack",
				"kind":         "webhook",
				"template":     "switchboard",
				"conversation": e.Slack.AlertChannel,
				"url":          "http://switchboard.switchboard.svc.cluster.local:8080/v1/messages",
				"auth": map[string]string{ //nolint:gosec // bearer_env names the environment variable, not a credential
					"bearer_env": "CORE_AGENT_SWITCHBOARD_TOKEN",
				},
				"description": "Posts incident diagnoses and pull request links to the on-call Slack channel through switchboard.",
			}},
		}
		appendPermissionsAllow(doc, "alert:slack")
	}
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
	return render(root, "workspace.yaml.tmpl", map[string]string{
		"Atespace":       e.Agent.Atespace,
		"Workspace":      e.Agent.Workspace,
		"AgentsMD":       string(md),
		"ChatConfig":     string(chatCfg),
		"IncidentConfig": string(incCfg),
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
