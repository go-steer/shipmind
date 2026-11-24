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
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-steer/shipmind/internal/scenario"
)

// ClusterTarget names one cluster in an audit sweep.
type ClusterTarget struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Config is the JSON configuration mounted at `/etc/shipmind-audits/config.json`
// inside the scheduled posture CronJob.
type Config struct {
	Atespace              string          `json:"atespace"`
	Project               string          `json:"project"`
	Clusters              []ClusterTarget `json:"clusters"`
	Schedule              string          `json:"schedule"`
	RefreshWindow         string          `json:"refresh_window"`
	MinConsecutive        int             `json:"min_consecutive"`
	DomainUnavailableKeys string          `json:"domain_unavailable_keys"`
	ExemptionsFile        string          `json:"exemptions_file,omitempty"`
	LookoutVersion        string          `json:"lookout_version,omitempty"`
	AlertChannel          string          `json:"alert_channel,omitempty"`
	SwitchboardURL        string          `json:"switchboard_url,omitempty"`
	AutoPR                bool            `json:"auto_pr,omitempty"`
	Repositories          []RepoTarget    `json:"repositories,omitempty"`
}

// LoadConfig reads and validates a JSON Config file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if len(c.Clusters) == 0 {
		return nil, fmt.Errorf("config %s: clusters is empty", path)
	}
	return &c, nil
}

// CommandRunner executes a deterministic audit binary (`lookout` or `leeway`)
// and returns its stdout bytes.
type CommandRunner func(ctx context.Context, bin string, args []string, extraEnv []string) ([]byte, error)

// ExecRunner is the production CommandRunner executing binaries with no shell.
func ExecRunner(ctx context.Context, bin string, args []string, extraEnv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // fixed lookout/leeway binaries and structured args
	cmd.Env = append(os.Environ(), extraEnv...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// RunOptions wires external dependencies for Run (real cluster/binaries in
// production; deterministic fakes in unit tests).
type RunOptions struct {
	LedgerPath      string
	LookoutBin      string
	LeewayBin       string
	Kubeconfig      string
	KubeContext     string
	IngressToken    string
	TaskTemplate    []byte
	Exec            CommandRunner
	GoogleToken     TokenProvider
	GCPBaseURL      string
	SwitchboardHTTP *http.Client
	ChatTasks       scenario.ChatTasks
	ChatAgent       scenario.ChatAgent
	AutoPRPoll      time.Duration
	AutoPRSleep     func(context.Context, time.Duration) error
	Now             func() time.Time
	Log             func(format string, args ...any)
}

// RunSummary is the structured JSON outcome printed to stdout at the end of a
// posture sweep.
type RunSummary struct {
	StartedAt          string            `json:"started_at"`
	FinishedAt         string            `json:"finished_at"`
	ClustersExpected   int               `json:"clusters_expected"`
	ClustersSucceeded  int               `json:"clusters_succeeded"`
	ActiveFindings     int               `json:"active_findings"`
	DueFindings        int               `json:"due_findings"`
	RetiredFindings    int               `json:"retired_findings"`
	MutedRefresh       int               `json:"muted_refresh"`
	HeldConsecutive    int               `json:"held_consecutive"`
	ExemptActive       int               `json:"exempt_active"`
	DigestConversation string            `json:"digest_conversation,omitempty"`
	DigestMessageID    string            `json:"digest_message_id,omitempty"`
	AwokenTasks        int               `json:"awoken_tasks"`
	LLMTokens          int64             `json:"llm_tokens"`
	CostUSD            float64           `json:"cost_usd"`
	PRURLs             []string          `json:"pr_urls,omitempty"`
	Errors             map[string]string `json:"errors,omitempty"`
}

// Run executes one complete scheduled posture sweep across cfg.Clusters.
// When all clusters are clean or all active findings sit inside the refresh
// window (or dwell / exemptions), Run exits with 0 LLM tokens and 0 awoken
// ax actors.
func Run(ctx context.Context, cfg *Config, opts RunOptions) (*RunSummary, error) {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	logFn := opts.Log
	if logFn == nil {
		logFn = func(string, ...any) {}
	}
	execFn := opts.Exec
	if execFn == nil {
		execFn = ExecRunner
	}
	lookoutBin := opts.LookoutBin
	if lookoutBin == "" {
		lookoutBin = "lookout"
	}
	leewayBin := opts.LeewayBin
	if leewayBin == "" {
		leewayBin = "leeway"
	}

	started := nowFn().UTC()
	ledger, err := LoadLedger(opts.LedgerPath)
	if err != nil {
		return nil, err
	}
	refreshDur := 168 * time.Hour
	if cfg.RefreshWindow != "" {
		if d, perr := time.ParseDuration(cfg.RefreshWindow); perr == nil && d > 0 {
			refreshDur = d
		}
	}

	kcPath, extraEnv, closeBroker, err := prepareSweepAuth(cfg, opts)
	if err != nil {
		return nil, err
	}
	defer closeBroker()

	obs, healthy, errs := collectAllEngines(ctx, cfg, lookoutBin, leewayBin, kcPath, opts.KubeContext, extraEnv, execFn, logFn)

	now := nowFn().UTC()
	delta := Evaluate(ledger, obs, healthy, EvalConfig{
		RefreshWindow:  refreshDur,
		MinConsecutive: cfg.MinConsecutive,
	}, now)

	summary := buildInitialSummary(started, cfg, healthy, delta, errs)

	// Zero-idle-LLM-token fast path: clean or all active findings suppressed.
	if len(delta.Due) == 0 {
		summary.FinishedAt = nowFn().UTC().Format(time.RFC3339)
		if err := SaveLedger(opts.LedgerPath, ledger); err != nil {
			return summary, err
		}
		return summary, joinScanErrors(errs)
	}

	prNote, prBySubject, autoErr := maybeRunAutoPR(ctx, cfg, opts, delta.Due, summary, now, logFn)
	if autoErr != nil {
		errs["autopr"] = autoErr.Error()
		summary.Errors = errs
	}

	delivered := true
	if cfg.AlertChannel != "" && cfg.SwitchboardURL != "" {
		digestText := FormatDigest(clusterIDs(cfg.Clusters), delta, prNote)
		idem := fmt.Sprintf("shipmind-audit-%s", started.Format("20060102T150405Z"))
		ref, derr := PostSwitchboardDigest(ctx, opts.SwitchboardHTTP, cfg.SwitchboardURL, opts.IngressToken, cfg.AlertChannel, idem, digestText)
		if derr != nil {
			delivered = false
			errs["switchboard"] = derr.Error()
			summary.Errors = errs
		} else {
			summary.DigestConversation = ref.Conversation
			summary.DigestMessageID = ref.ID
		}
	}

	if delivered {
		MarkReported(delta.Due, now, prBySubject)
	}
	summary.FinishedAt = nowFn().UTC().Format(time.RFC3339)
	if err := SaveLedger(opts.LedgerPath, ledger); err != nil {
		return summary, err
	}
	return summary, joinScanErrors(errs)
}

func prepareSweepAuth(cfg *Config, opts RunOptions) (kcPath string, extraEnv []string, closeFn func(), err error) {
	if opts.Kubeconfig != "" || opts.GoogleToken == nil {
		return opts.Kubeconfig, nil, func() {}, nil
	}
	var gkePaths []string
	for _, cl := range cfg.Clusters {
		if _, _, _, ok := ParseGKEClusterPath(cl.Path); ok {
			gkePaths = append(gkePaths, cl.Path)
		}
	}
	if len(gkePaths) == 0 {
		return "", nil, func() {}, nil
	}
	tmpDir, err := os.MkdirTemp("", "shipmind-audit-auth-*")
	if err != nil {
		return "", nil, nil, err
	}
	broker, err := StartCredentialBroker(BrokerOptions{
		Dir:          tmpDir,
		ClusterPaths: gkePaths,
		GoogleToken:  opts.GoogleToken,
		GCPBaseURL:   opts.GCPBaseURL,
	})
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", nil, nil, err
	}
	cleanup := func() {
		_ = broker.Close()
		_ = os.RemoveAll(tmpDir)
	}
	envVars := []string{
		"CREDENTIAL_PROXY_URL=" + broker.URL,
		"CREDENTIAL_PROXY_TOKEN_FILE=" + broker.TokenFile,
		"LEEWAY_CREDENTIAL_PROXY_MODE=client-go",
	}
	return broker.KubeconfigPath, envVars, cleanup, nil
}

func collectAllEngines(
	ctx context.Context,
	cfg *Config,
	lookoutBin, leewayBin, kcPath, kubeContext string,
	extraEnv []string,
	execFn CommandRunner,
	logFn func(string, ...any),
) ([]Observation, map[ClusterEngine]bool, map[string]string) {
	var allObs []Observation
	healthy := map[ClusterEngine]bool{}
	errs := map[string]string{}

	subcmds := []string{"all"}
	if NeedsExplicitLookoutSubcommands(cfg.LookoutVersion) {
		subcmds = append(subcmds, "computeclass", "ai")
	}

	for _, cl := range cfg.Clusters {
		cid := CanonicalClusterID(cl.Path)
		if cid == "" {
			cid = cl.Name
		}
		cObs, err := runLookoutForCluster(ctx, lookoutBin, subcmds, cid, cl, kcPath, kubeContext, cfg.ExemptionsFile, extraEnv, execFn)
		if err != nil {
			errs["lookout:"+cid] = err.Error()
			logFn("lookout failed on %s: %v", cid, err)
			continue
		}
		healthy[ClusterEngine{Cluster: cid, Engine: EngineLookout}] = true
		allObs = append(allObs, cObs...)
	}

	lwObs, lwHealthy, lwErrs, err := runLeewaySweep(ctx, leewayBin, cfg, kcPath, kubeContext, extraEnv, execFn)
	if err != nil {
		errs["leeway"] = err.Error()
		logFn("leeway report failed: %v", err)
	} else {
		for cid, ok := range lwHealthy {
			if ok {
				healthy[ClusterEngine{Cluster: cid, Engine: EngineLeeway}] = true
			}
		}
		for cid, msg := range lwErrs {
			errs["leeway:"+cid] = msg
		}
		allObs = append(allObs, lwObs...)
	}
	return allObs, healthy, errs
}

func runLookoutForCluster(
	ctx context.Context,
	lookoutBin string,
	subcmds []string,
	cid string,
	cl ClusterTarget,
	kcPath, kubeContext, exemptionsFile string,
	extraEnv []string,
	execFn CommandRunner,
) ([]Observation, error) {
	var out []Observation
	for _, sub := range subcmds {
		args := []string{"audit", sub, "-A", "--format=json"}
		ctxName := kubeContext
		if kcPath != "" {
			args = append(args, "--kubeconfig="+kcPath)
			if ctxName == "" {
				ctxName = ContextName(cl.Path)
			}
		}
		if ctxName != "" {
			args = append(args, "--context="+ctxName)
		}
		if exemptionsFile != "" {
			args = append(args, "--exemptions="+exemptionsFile)
		}
		raw, err := execFn(ctx, lookoutBin, args, extraEnv)
		if err != nil {
			return nil, err
		}
		obs, _, err := ParseLookoutStream(cid, bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("lookout audit %s: %w", sub, err)
		}
		out = append(out, obs...)
	}
	return out, nil
}

func runLeewaySweep(
	ctx context.Context,
	leewayBin string,
	cfg *Config,
	kcPath, kubeContext string,
	extraEnv []string,
	execFn CommandRunner,
) ([]Observation, map[string]bool, map[string]string, error) {
	args := []string{
		"report",
		"--pretty=false",
		"--topology-domain-unavailable-keys=" + cfg.DomainUnavailableKeys,
	}
	var expected, clusterSpecs []string
	for _, cl := range cfg.Clusters {
		cid := CanonicalClusterID(cl.Path)
		expected = append(expected, cid)
		if len(extraEnv) == 0 && kcPath != "" {
			clusterSpecs = append(clusterSpecs, ContextName(cl.Path))
		} else {
			clusterSpecs = append(clusterSpecs, cid)
		}
	}
	if kcPath != "" {
		args = append(args, "--kubeconfig="+kcPath)
	}
	if kubeContext != "" && len(cfg.Clusters) == 1 {
		args = append(args, "--context="+kubeContext, "--cluster-name="+expected[0])
	} else {
		args = append(args, "--clusters="+strings.Join(clusterSpecs, ","))
	}
	raw, err := execFn(ctx, leewayBin, args, extraEnv)
	if err != nil {
		return nil, nil, nil, err
	}
	parsed, err := ParseLeewayReport(raw, expected)
	if err != nil {
		return nil, nil, nil, err
	}
	return parsed.Observations, parsed.Healthy, parsed.ClusterErrors, nil
}

func maybeRunAutoPR(
	ctx context.Context,
	cfg *Config,
	opts RunOptions,
	due []DueFinding,
	summary *RunSummary,
	now time.Time,
	logFn func(string, ...any),
) (string, map[string]string, error) {
	if !cfg.AutoPR {
		return "", nil, nil
	}
	actionable := GitOpsActionable(due)
	if len(actionable) == 0 {
		return "", nil, nil
	}
	if opts.ChatTasks == nil || opts.ChatAgent == nil || len(opts.TaskTemplate) == 0 {
		return "", nil, errors.New("auto_pr is enabled, but ax task runner dependencies or task.yaml are missing")
	}
	res, err := RunAutoPR(ctx, opts.ChatTasks, opts.ChatAgent, AutoPRConfig{
		TaskTemplate: opts.TaskTemplate,
		Repositories: cfg.Repositories,
		PollInterval: opts.AutoPRPoll,
		Sleep:        opts.AutoPRSleep,
	}, actionable, now)
	if err != nil {
		return "", nil, err
	}
	summary.AwokenTasks = 1
	summary.LLMTokens = res.LLMTokens
	summary.CostUSD = res.CostUSD
	summary.PRURLs = res.PRURLs
	logFn("autoPR task %s finished: %d tokens, $%.4f, PRs=%v", res.TaskName, res.LLMTokens, res.CostUSD, res.PRURLs)

	prBySubject := map[string]string{}
	if len(res.PRURLs) > 0 {
		sent := actionable[:min(len(actionable), AutoPRMaxFindings)]
		prRef := strings.Join(res.PRURLs, ", ")
		for _, a := range sent {
			prBySubject[a.Entry.SubjectKey] = prRef
		}
		return "Opened or matched pull request(s): " + prRef, prBySubject, nil
	}
	return clampUTF8(strings.TrimSpace(res.Answer), 600), prBySubject, nil
}

func buildInitialSummary(started time.Time, cfg *Config, healthy map[ClusterEngine]bool, delta Delta, errs map[string]string) *RunSummary {
	succeeded := 0
	for _, cl := range cfg.Clusters {
		cid := CanonicalClusterID(cl.Path)
		if healthy[ClusterEngine{Cluster: cid, Engine: EngineLookout}] && healthy[ClusterEngine{Cluster: cid, Engine: EngineLeeway}] {
			succeeded++
		}
	}
	s := &RunSummary{
		StartedAt:         started.Format(time.RFC3339),
		ClustersExpected:  len(cfg.Clusters),
		ClustersSucceeded: succeeded,
		ActiveFindings:    delta.ActiveTotal,
		DueFindings:       len(delta.Due),
		RetiredFindings:   len(delta.Retired),
		MutedRefresh:      delta.MutedRefresh,
		HeldConsecutive:   delta.HeldConsecutive,
		ExemptActive:      delta.ExemptActive,
	}
	if len(errs) > 0 {
		s.Errors = errs
	}
	return s
}

func clusterIDs(clusters []ClusterTarget) []string {
	out := make([]string, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, CanonicalClusterID(c.Path))
	}
	return out
}

func joinScanErrors(errs map[string]string) error {
	if len(errs) == 0 {
		return nil
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(errs)) {
		parts = append(parts, k+": "+errs[k])
	}
	return fmt.Errorf("posture sweep had %d error(s): %s", len(parts), strings.Join(parts, "; "))
}
