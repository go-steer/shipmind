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

package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/go-steer/shipmind/internal/scenario"
)

const (
	// maxDigestBytes keeps outbound digest posts well within switchboard's
	// 32 KiB outbound thread seed limit (`maxOutboundSeedBytes`) so `@shipmind`
	// replies in Slack inherit the complete digest and remediation YAML.
	maxDigestBytes       = 24 * 1024
	maxRenderedFindings  = 20
	autoPRStartTimeout   = 4 * time.Minute
	autoPRSettleTimeout  = 5 * time.Minute
	autoPRPollInterval   = 2 * time.Second
	autoPRCleanupTimeout = 2 * time.Minute
)

// MessageRef is the conversation/message identifier returned by switchboard's
// `POST /v1/messages` outbound ingress.
type MessageRef struct {
	Conversation string `json:"conversation"`
	ID           string `json:"id"`
}

// PostSwitchboardDigest posts a posture digest (or thread follow-up) to
// switchboard's `:8080` `/v1/messages` outbound ingress. No `session` field is
// sent so switchboard's `NoteOutbound` seeds any subsequent `@shipmind` reply
// in the Slack thread with the digest text (`switchboard` v0.10.0+).
func PostSwitchboardDigest(ctx context.Context, client *http.Client, endpoint, token, conversation, idempotencyKey, text string) (MessageRef, error) {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	payload, err := json.Marshal(struct {
		Conversation string `json:"conversation"`
		Text         string `json:"text"`
	}{
		Conversation: conversation,
		Text:         text,
	})
	if err != nil {
		return MessageRef{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return MessageRef{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := client.Do(req) //nolint:gosec // configured in-cluster switchboard ingress URL
	if err != nil {
		return MessageRef{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return MessageRef{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return MessageRef{}, fmt.Errorf("switchboard ingress %s: status %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var ref MessageRef
	if err := json.Unmarshal(body, &ref); err != nil {
		return MessageRef{}, fmt.Errorf("switchboard ingress response: %w", err)
	}
	return ref, nil
}

// FormatDigest renders a structured Markdown posture digest with deterministic
// remediation YAML blocks suitable for switchboard's Block Kit / thread seeding.
func FormatDigest(clusters []string, delta Delta, prNote string) string {
	var b strings.Builder
	newCnt, escCnt, refCnt := 0, 0, 0
	for _, d := range delta.Due {
		switch d.Reason {
		case DueNew:
			newCnt++
		case DueEscalated:
			escCnt++
		case DueRefresh:
			refCnt++
		}
	}
	fmt.Fprintf(&b, "## Scheduled Posture Digest (%d due: %d new, %d escalated, %d refresh)\n\n",
		len(delta.Due), newCnt, escCnt, refCnt)
	fmt.Fprintf(&b, "- **Clusters swept:** `%s`\n", strings.Join(clusters, "`, `"))
	fmt.Fprintf(&b, "- **Ledger totals:** %d active (%d muted within refresh window, %d held by dwell, %d exempt), %d retired this run\n\n",
		delta.ActiveTotal, delta.MutedRefresh, delta.HeldConsecutive, delta.ExemptActive, len(delta.Retired))

	shown := min(len(delta.Due), maxRenderedFindings)
	for i := range shown {
		formatOneDue(&b, i+1, delta.Due[i])
	}
	if extra := len(delta.Due) - shown; extra > 0 {
		fmt.Fprintf(&b, "\n_…and %d more due posture finding(s) in the persistent ledger._\n", extra)
	}
	if len(delta.Retired) > 0 {
		fmt.Fprintf(&b, "\n### Cleared since previous sweep (%d)\n", len(delta.Retired))
		limit := min(len(delta.Retired), 10)
		for i := range limit {
			r := delta.Retired[i]
			fmt.Fprintf(&b, "- Resolved `%s` (%s)\n", formatSubject(r), r.Kind)
		}
	}
	if strings.TrimSpace(prNote) != "" {
		fmt.Fprintf(&b, "\n### GitOps Pull Requests (`autoPR`)\n%s\n", strings.TrimSpace(prNote))
	}
	b.WriteString("\n_Digest-only posture report (no incident task opened). Reply to this thread mentioning the bot to inspect or open a GitOps pull request with full seeded context._\n")
	return clampUTF8(b.String(), maxDigestBytes)
}

func formatOneDue(b *strings.Builder, idx int, d DueFinding) {
	e := d.Entry
	fmt.Fprintf(b, "### %d. [%s · %s] `%s` on `%s`\n",
		idx, strings.ToUpper(e.Severity), d.Reason, formatSubject(e), e.Cluster)
	fmt.Fprintf(b, "- **Detector:** `%s` (`%s`)", e.Engine, e.Kind)
	if e.Reason != "" {
		fmt.Fprintf(b, " · **Reason:** `%s`", e.Reason)
	}
	if e.Fingerprint != "" && strings.HasPrefix(e.Fingerprint, "sha256:") {
		fmt.Fprintf(b, " · **Fingerprint:** `%s`", e.Fingerprint[:min(len(e.Fingerprint), 23)])
	}
	b.WriteString("\n")
	if e.Message != "" {
		fmt.Fprintf(b, "- **Detail:** %s\n", e.Message)
	}
	if rem := e.Remediation; rem != nil {
		if rem.Kind == "platform_managed" {
			fmt.Fprintf(b, "- **Remediation (`platform_managed`):** %s _(reconciled by platform addon manager; do not open a GitOps manifest patch)_\n", rem.Summary)
		} else if rem.Summary != "" {
			fmt.Fprintf(b, "- **Remediation (`%s`):** %s\n", rem.Kind, rem.Summary)
		}
		if strings.TrimSpace(rem.YAML) != "" && rem.Kind != "platform_managed" {
			fmt.Fprintf(b, "\n```yaml\n%s\n```\n", strings.TrimSpace(rem.YAML))
		}
	}
	b.WriteString("\n")
}

func formatSubject(e *LedgerEntry) string {
	ns := e.Namespace
	if ns == "" {
		ns = ClusterWideNamespace
	}
	kind := e.ObjectKind
	if kind == "" {
		kind = "Cluster"
	}
	name := e.Name
	if name == "" {
		name = e.Kind
	}
	return ns + "/" + kind + "/" + name
}

func clampUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	const tail = "\n…[truncated]\n"
	limit := maxBytes - len(tail)
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + tail
}

// GitOpsActionable filters due findings to workload-scoped declarative changes
// eligible for `propose-change` (`manifest_patch` on leeway, or workload/RBAC/
// netpol static posture on lookout), excluding `platform_managed` addons,
// cluster-capacity/node operational conditions, and `shipmind.dev/scenario` probes.
func GitOpsActionable(due []DueFinding) []DueFinding {
	var out []DueFinding
	for _, d := range due {
		e := d.Entry
		if isScenarioProbe(e) {
			continue
		}
		if e.Engine == EngineLeeway {
			if e.Remediation == nil || e.Remediation.Kind != "manifest_patch" {
				continue
			}
			out = append(out, d)
			continue
		}
		if !strings.HasPrefix(e.Kind, "audit.") || isClusterProviderCheck(e.Kind) || e.Namespace == "" || e.Namespace == "kube-system" {
			continue
		}
		out = append(out, d)
	}
	return out
}

func isScenarioProbe(e *LedgerEntry) bool {
	if e.Namespace == "triage-lab" {
		return true
	}
	return strings.Contains(e.Message, "shipmind.dev/scenario")
}

func isClusterProviderCheck(kind string) bool {
	switch kind {
	case "audit.workload_identity_off", "audit.legacy_metadata", "audit.public_control_plane",
		"audit.nodepool_cannot_grow", "audit.cluster_security_posture", "audit.cluster_network_posture",
		"audit.cluster_observability_off", "audit.cluster_backup_off":
		return true
	default:
		return false
	}
}

// RepoTarget names an allowlisted GitOps repository for AutoPRConfig.
type RepoTarget struct {
	Repository string `json:"repository"`
	BaseBranch string `json:"base_branch"`
}

// AutoPRConfig configures an optional ax audit remediation turn.
type AutoPRConfig struct {
	TaskTemplate []byte
	Repositories []RepoTarget
	PollInterval time.Duration
	Sleep        func(context.Context, time.Duration) error
}

// AutoPRResult records the outcome and token spend of an autoPR task run.
type AutoPRResult struct {
	TaskName  string
	SessionID string
	Answer    string
	PRURLs    []string
	LLMTokens int64
	CostUSD   float64
}

var ghPRRegex = regexp.MustCompile(`https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/pull/[0-9]+`)

// RunAutoPR starts a short-lived ax audit task equipped with `propose-change`
// (`vcs_proposal_list` open-PR dedup), sends the actionable posture brief,
// collects LLM usage and pull request URLs, and deletes the task on exit.
func RunAutoPR(ctx context.Context, tasks scenario.ChatTasks, agent scenario.ChatAgent, cfg AutoPRConfig, actionable []DueFinding, now time.Time) (*AutoPRResult, error) {
	if len(actionable) == 0 {
		return &AutoPRResult{}, nil
	}
	taskName := "posture-autopr-" + now.UTC().Format("0102-150405")
	manifest, err := stampTaskName(cfg.TaskTemplate, taskName)
	if err != nil {
		return nil, fmt.Errorf("stamp audit task: %w", err)
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = autoPRPollInterval
	}
	sleepFn := cfg.Sleep
	if sleepFn == nil {
		sleepFn = sleepContext
	}

	applied := false
	defer func() {
		if !applied {
			return
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), autoPRCleanupTimeout)
		defer cancel()
		_ = tasks.DeleteTask(cctx, taskName)
	}()

	if err := tasks.Apply(ctx, manifest); err != nil {
		return nil, fmt.Errorf("apply audit task %s: %w", taskName, err)
	}
	applied = true

	sid, err := waitSession(ctx, agent, taskName, poll, sleepFn)
	if err != nil {
		return nil, err
	}
	prompt := buildAutoPRPrompt(cfg.Repositories, actionable)
	if err := agent.Inject(ctx, taskName, sid, prompt); err != nil {
		return nil, fmt.Errorf("inject audit brief: %w", err)
	}
	answer, cost, err := waitSettled(ctx, agent, taskName, sid, poll, sleepFn)
	if err != nil {
		return nil, err
	}

	res := &AutoPRResult{
		TaskName:  taskName,
		SessionID: sid,
		Answer:    answer,
		PRURLs:    dedupeStrings(ghPRRegex.FindAllString(answer, -1)),
		CostUSD:   cost,
	}
	if u, err := agent.Usage(ctx, taskName, sid); err == nil && u != nil {
		norm := u.Normalize("core-agent", now)
		res.LLMTokens = norm.InputTokens + norm.OutputTokens + norm.ThinkingTokens
		if norm.CostUSD > 0 {
			res.CostUSD = norm.CostUSD
		}
	}
	return res, nil
}

func stampTaskName(raw []byte, name string) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	meta, ok := doc["metadata"].(map[string]any)
	if !ok {
		return nil, errors.New("audit task template has no metadata")
	}
	meta["name"] = name
	return yaml.Marshal(doc)
}

func waitSession(ctx context.Context, agent scenario.ChatAgent, taskName string, poll time.Duration, sleepFn func(context.Context, time.Duration) error) (string, error) {
	sctx, cancel := context.WithTimeout(ctx, autoPRStartTimeout)
	defer cancel()
	var last error
	for {
		sid, err := agent.CreateSession(sctx, taskName)
		if err == nil {
			return sid, nil
		}
		last = err
		if err := sleepFn(sctx, poll); err != nil {
			return "", fmt.Errorf("audit task %s accepted no session: %w (%v)", taskName, err, last)
		}
	}
}

func waitSettled(ctx context.Context, agent scenario.ChatAgent, taskName, sid string, poll time.Duration, sleepFn func(context.Context, time.Duration) error) (string, float64, error) {
	dctx, cancel := context.WithTimeout(ctx, autoPRSettleTimeout)
	defer cancel()
	idle := 0
	for {
		st, err := agent.State(dctx, taskName, sid)
		if err == nil {
			if tr, terr := agent.Events(dctx, taskName, sid); terr == nil {
				if st == "idle" {
					idle++
				} else {
					idle = 0
				}
				if ans, _ := tr.Answer(); ans != "" && idle >= 2 {
					return ans, tr.Cost, nil
				}
			}
		}
		if err := sleepFn(dctx, poll); err != nil {
			return "", 0, fmt.Errorf("audit task %s did not settle: %w", taskName, err)
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// AutoPRMaxFindings caps how many actionable findings one autoPR turn includes
// in its prompt (and consequently how many entries may receive PR attribution).
const AutoPRMaxFindings = 10

func dedupeStrings(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func buildAutoPRPrompt(repos []RepoTarget, actionable []DueFinding) string {
	var b strings.Builder
	b.WriteString("Scheduled posture sweep detected actionable workload configuration gaps.\n")
	b.WriteString("Delegate to `propose-change` to check open pull requests first (`vcs_proposal_list` with `state: \"open\"` to avoid duplicates), locate managed manifests in the configured GitOps repository, and open a pull request for concrete declarative fixes (skipping any unmanaged workloads).\n\n")
	if len(repos) > 0 {
		b.WriteString("Configured GitOps repositories:\n")
		for _, r := range repos {
			fmt.Fprintf(&b, "- `%s` (base branch: `%s`)\n", r.Repository, r.BaseBranch)
		}
		b.WriteString("\n")
	}
	b.WriteString("Actionable findings:\n")
	limit := min(len(actionable), AutoPRMaxFindings)
	for i := range limit {
		e := actionable[i].Entry
		fmt.Fprintf(&b, "%d. Cluster `%s` — `%s` (`%s`, severity `%s`): %s\n",
			i+1, e.Cluster, formatSubject(e), e.Kind, e.Severity, e.Message)
		if e.Remediation != nil && strings.TrimSpace(e.Remediation.YAML) != "" {
			fmt.Fprintf(&b, "```yaml\n%s\n```\n", strings.TrimSpace(e.Remediation.YAML))
		}
	}
	return b.String()
}
