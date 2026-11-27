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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Default settings for scheduled posture audits (#127).
const (
	DefaultAuditSchedule              = "17 5 * * *"
	DefaultAuditRefreshWindow         = "168h"
	DefaultAuditMinConsecutive        = 1
	DefaultAuditDomainUnavailableKeys = ""
)

// Audits configures scheduled deterministic posture sweeps (`lookout audit`
// and `leeway report`) on the substrate profile (#127).
type Audits struct {
	// Schedule is a standard 5-field UTC cron expression for the posture
	// CronJob; empty defaults to DefaultAuditSchedule ("17 5 * * *", daily).
	Schedule string `yaml:"schedule,omitempty"`
	// Suspend pauses scheduled CronJob runs while keeping the CronJob and
	// its persistent ledger volume managed.
	Suspend bool `yaml:"suspend,omitempty"`
	// RefreshWindow is how long an active reported finding stays muted
	// before re-reporting (1h to 8760h); empty defaults to
	// DefaultAuditRefreshWindow ("168h", 7 days).
	RefreshWindow string `yaml:"refreshWindow,omitempty"`
	// MinConsecutive, when set (1 to 10), requires a finding to appear in
	// at least N consecutive runs before its first report so transient
	// mid-rollout leeway report skew is absorbed. Unset defaults to 1.
	MinConsecutive *int `yaml:"minConsecutive,omitempty"`
	// Exemptions, when set, names a relative .yaml/.yml file inside the
	// environment overlay directory passed as --exemptions to lookout audit.
	Exemptions string `yaml:"exemptions,omitempty"`
	// DomainUnavailableKeys controls leeway report's
	// --topology-domain-unavailable-keys flag (comma-separated Kubernetes
	// topology label keys, or "" to disable leeway.domain_unavailable on
	// clusters that scale zones to zero). Unset defaults to "" (disabled).
	DomainUnavailableKeys *string `yaml:"domainUnavailableKeys,omitempty"`
	// AutoPR, when true (requiring agent.vcs and idleSuspendAfter >= 5m),
	// wakes an ax audit task with propose-change (vcs_proposal_list open-PR
	// deduplication) for actionable findings.
	AutoPR bool `yaml:"autoPR,omitempty"`
	// RunnerImage, when set, overrides the posture CronJob's e2e-runner
	// image (pinned by digest).
	RunnerImage string `yaml:"runnerImage,omitempty"`
}

// CronSchedule returns Schedule, or DefaultAuditSchedule when unset.
func (a *Audits) CronSchedule() string {
	if a == nil || a.Schedule == "" {
		return DefaultAuditSchedule
	}
	return a.Schedule
}

// RefreshString returns RefreshWindow, or DefaultAuditRefreshWindow when unset.
func (a *Audits) RefreshString() string {
	if a == nil || a.RefreshWindow == "" {
		return DefaultAuditRefreshWindow
	}
	return a.RefreshWindow
}

// Refresh returns the parsed RefreshWindow duration (defaulting to 168h).
func (a *Audits) Refresh() time.Duration {
	if a == nil || a.RefreshWindow == "" {
		return 168 * time.Hour
	}
	d, _ := time.ParseDuration(a.RefreshWindow)
	return d
}

// MinConsecutiveRuns returns MinConsecutive, or DefaultAuditMinConsecutive (1) when unset.
func (a *Audits) MinConsecutiveRuns() int {
	if a == nil || a.MinConsecutive == nil {
		return DefaultAuditMinConsecutive
	}
	return *a.MinConsecutive
}

// DomainKeys returns DomainUnavailableKeys, or DefaultAuditDomainUnavailableKeys ("") when unset.
func (a *Audits) DomainKeys() string {
	if a == nil || a.DomainUnavailableKeys == nil {
		return DefaultAuditDomainUnavailableKeys
	}
	return *a.DomainUnavailableKeys
}

var (
	labelNameRE   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$`)
	dnsSubdomain  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	cronStepToken = regexp.MustCompile(`^(\*|[0-9]+(-[0-9]+)?)(/[1-9][0-9]*)?$`)
)

func (e *Env) checkVCS(idleSuspend time.Duration, fail func(string, ...any)) {
	a := e.Agent
	vcs := a.VCS
	if vcs == nil {
		return
	}
	auditOnly := e.Audits != nil && e.Audits.AutoPR
	if vcs.ChatAutoApprove && a.ChatImage == "" {
		fail("agent.vcs.chatAutoApprove: needs agent.chatImage")
	} else if !vcs.IncidentAutoPR && !auditOnly && a.ChatImage == "" {
		fail("agent.vcs: needs agent.chatImage")
	}
	if vcs.IncidentAutoPR && idleSuspend < 5*time.Minute {
		fail("agent.idleSuspendAfter %q: want at least 5m when agent.vcs.incidentAutoPR is true (incident turns run up to 240s)", a.IdleSuspendAfter)
	}
	if !dnsLabel.MatchString(vcs.Secret) {
		fail("agent.vcs.secret %q: want a Secret name in ate-credentials", vcs.Secret)
	}
	if len(vcs.Repositories) == 0 {
		fail("agent.vcs.repositories: want at least one repository")
	}
	seenRepos := map[string]bool{}
	for i, r := range vcs.Repositories {
		if !validGitHubRepo(r.Repository) {
			fail("agent.vcs.repositories[%d].repository %q: want <owner>/<repo> (no host prefix, traversal or .git suffix)", i, r.Repository)
		}
		key := strings.ToLower(r.Repository)
		if seenRepos[key] {
			fail("agent.vcs.repositories: %q listed twice", r.Repository)
		}
		seenRepos[key] = true
		if r.BaseBranch != "" && !validBranch(r.BaseBranch) {
			fail("agent.vcs.repositories[%d].baseBranch %q: want a valid git branch name", i, r.BaseBranch)
		}
	}
}

func (e *Env) checkAudits(idleSuspend time.Duration, fail func(string, ...any)) {
	aud := e.Audits
	if aud == nil {
		return
	}
	if aud.Schedule != "" && !validCronSchedule(aud.Schedule) {
		fail("audits.schedule %q: want a 5-field UTC cron expression (e.g. %q)", aud.Schedule, DefaultAuditSchedule)
	}
	if aud.RefreshWindow != "" {
		d, err := time.ParseDuration(aud.RefreshWindow)
		if err != nil || d < time.Hour || d > 8760*time.Hour {
			fail("audits.refreshWindow %q: want a duration from 1h to 8760h", aud.RefreshWindow)
		}
	}
	if aud.MinConsecutive != nil && (*aud.MinConsecutive < 1 || *aud.MinConsecutive > 10) {
		fail("audits.minConsecutive %d: want an integer from 1 to 10", *aud.MinConsecutive)
	}
	if aud.Exemptions != "" && !validExemptionsPath(aud.Exemptions) {
		fail("audits.exemptions %q: want a clean relative .yaml or .yml file inside the environment directory", aud.Exemptions)
	}
	if aud.DomainUnavailableKeys != nil && *aud.DomainUnavailableKeys != "" && !validDomainKeyList(*aud.DomainUnavailableKeys) {
		fail("audits.domainUnavailableKeys %q: want comma-separated Kubernetes label keys (or \"\" to disable)", *aud.DomainUnavailableKeys)
	}
	if aud.RunnerImage != "" && !digestRef.MatchString(aud.RunnerImage) {
		fail("audits.runnerImage %q: want an image pinned by digest", aud.RunnerImage)
	}
	if aud.AutoPR {
		if e.Agent.VCS == nil {
			fail("audits.autoPR: needs agent.vcs")
		}
		if idleSuspend < 5*time.Minute {
			fail("agent.idleSuspendAfter %q: want at least 5m when audits.autoPR is true (audit PR turns run up to 240s)", e.Agent.IdleSuspendAfter)
		}
	}
}

func checkAuditFiles(dir string, e *Env) error {
	if e.Audits == nil || e.Audits.Exemptions == "" {
		return nil
	}
	p := filepath.Join(dir, filepath.FromSlash(e.Audits.Exemptions))
	info, err := os.Lstat(p) //nolint:gosec // validExemptionsPath forbids absolute paths and .. traversal outside dir
	if err != nil {
		return fmt.Errorf("audits.exemptions %q: %w", e.Audits.Exemptions, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("audits.exemptions %q: not a regular file", e.Audits.Exemptions)
	}
	return nil
}

func validExemptionsPath(p string) bool {
	if strings.TrimSpace(p) != p || strings.ContainsAny(p, "\\\x00\n\r\t ") || strings.HasPrefix(p, "/") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	if clean != p || clean == "." || slices.Contains(strings.Split(clean, "/"), "..") {
		return false
	}
	lower := strings.ToLower(clean)
	return strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml")
}

func validDomainKeyList(raw string) bool {
	seen := map[string]bool{}
	for k := range strings.SplitSeq(raw, ",") {
		if !validLabelKey(k) || seen[k] {
			return false
		}
		seen[k] = true
	}
	return true
}

func validLabelKey(k string) bool {
	prefix, name, hasSlash := strings.Cut(k, "/")
	if !hasSlash {
		return labelNameRE.MatchString(k)
	}
	return len(prefix) <= 253 && dnsSubdomain.MatchString(prefix) && labelNameRE.MatchString(name)
}

func validCronSchedule(expr string) bool {
	if strings.TrimSpace(expr) != expr {
		return false
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false
	}
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	for i, f := range fields {
		if !validCronField(f, bounds[i][0], bounds[i][1]) {
			return false
		}
	}
	return true
}

func validCronField(field string, minVal, maxVal int) bool {
	for part := range strings.SplitSeq(field, ",") {
		if !cronStepToken.MatchString(part) {
			return false
		}
		base, _, _ := strings.Cut(part, "/")
		if base == "*" {
			continue
		}
		loStr, hiStr, isRange := strings.Cut(base, "-")
		lo, _ := strconv.Atoi(loStr)
		if lo < minVal || lo > maxVal {
			return false
		}
		if isRange {
			hi, _ := strconv.Atoi(hiStr)
			if hi < lo || hi > maxVal {
				return false
			}
		}
	}
	return true
}
