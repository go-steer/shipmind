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
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	LedgerSchemaVersion = 1
	StateActive         = "active"
	StateRetired        = "retired"

	DueNew       = "new"
	DueEscalated = "escalated"
	DueRefresh   = "refresh"
)

// ClusterEngine scopes health tracking per (cluster, engine) so a transient
// failure in one engine or cluster never retires open findings in another.
type ClusterEngine struct {
	Cluster string
	Engine  string
}

// Ledger is the persistent posture deduplication store written to the CronJob PVC.
type Ledger struct {
	SchemaVersion int                     `json:"schema_version"`
	UpdatedAt     time.Time               `json:"updated_at"`
	Entries       map[string]*LedgerEntry `json:"entries"`
}

// LedgerEntry tracks one instance subject key across sweeps.
type LedgerEntry struct {
	SubjectKey       string            `json:"subject_key"`
	Cluster          string            `json:"cluster"`
	Engine           string            `json:"engine"`
	Fingerprint      string            `json:"fingerprint,omitempty"`
	Kind             string            `json:"kind"`
	Severity         string            `json:"severity"`
	Namespace        string            `json:"namespace,omitempty"`
	ObjectKind       string            `json:"object_kind,omitempty"`
	Name             string            `json:"name,omitempty"`
	Reason           string            `json:"reason,omitempty"`
	SuspectedCause   string            `json:"suspected_cause,omitempty"`
	Message          string            `json:"message,omitempty"`
	Details          map[string]string `json:"details,omitempty"`
	Remediation      *Remediation      `json:"remediation,omitempty"`
	ExemptReason     string            `json:"exempt_reason,omitempty"`
	ExemptExpires    string            `json:"exempt_expires,omitempty"`
	FirstSeenAt      time.Time         `json:"first_seen_at"`
	LastSeenAt       time.Time         `json:"last_seen_at"`
	ConsecutiveRuns  int               `json:"consecutive_runs"`
	LastReportedAt   *time.Time        `json:"last_reported_at,omitempty"`
	ReportedSeverity string            `json:"reported_severity,omitempty"`
	State            string            `json:"state"`
	RetiredAt        *time.Time        `json:"retired_at,omitempty"`
	PRURL            string            `json:"pr_url,omitempty"`
}

// EvalConfig controls ledger deduplication, dwell, and refresh behavior.
type EvalConfig struct {
	RefreshWindow  time.Duration
	MinConsecutive int
}

// DueFinding is an active finding due to be reported in the current sweep.
type DueFinding struct {
	Reason string
	Entry  *LedgerEntry
}

// Delta summarizes the outcome of evaluating one sweep against the ledger.
type Delta struct {
	Due             []DueFinding
	Retired         []*LedgerEntry
	ActiveTotal     int
	ExemptActive    int
	HeldConsecutive int
	MutedRefresh    int
}

// LoadLedger reads path, returning a fresh empty Ledger when path does not
// exist yet. A malformed file returns an error rather than resetting state.
func LoadLedger(path string) (*Ledger, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return &Ledger{SchemaVersion: LedgerSchemaVersion, Entries: map[string]*LedgerEntry{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var l Ledger
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("ledger %s: %w", path, err)
	}
	if l.SchemaVersion != 0 && l.SchemaVersion != LedgerSchemaVersion {
		return nil, fmt.Errorf("ledger %s: unsupported schema_version %d", path, l.SchemaVersion)
	}
	if l.Entries == nil {
		l.Entries = map[string]*LedgerEntry{}
	}
	l.SchemaVersion = LedgerSchemaVersion
	return &l, nil
}

// SaveLedger atomically writes l to path via a synced sibling temp file.
func SaveLedger(path string, l *Ledger) error {
	clean := filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(clean), 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(clean), ".ledger-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, clean); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// Evaluate applies observations to l for healthy (cluster, engine) pairs,
// retiring cleared entries, holding transient findings below MinConsecutive,
// suppressing active exemptions and muted findings within RefreshWindow, and
// returning the sorted DueFinding list without advancing LastReportedAt yet.
func Evaluate(l *Ledger, obs []Observation, healthy map[ClusterEngine]bool, cfg EvalConfig, now time.Time) Delta {
	if l.Entries == nil {
		l.Entries = map[string]*LedgerEntry{}
	}
	now = now.UTC()
	l.UpdatedAt = now
	if cfg.RefreshWindow <= 0 {
		cfg.RefreshWindow = 168 * time.Hour
	}
	if cfg.MinConsecutive <= 0 {
		cfg.MinConsecutive = 1
	}

	seenThisRun := map[string]bool{}
	for _, o := range obs {
		if !healthy[ClusterEngine{Cluster: o.Cluster, Engine: o.Engine}] || seenThisRun[o.SubjectKey] {
			continue
		}
		seenThisRun[o.SubjectKey] = true
		upsertObservation(l, o, now)
	}

	retireWindow := max(2*cfg.RefreshWindow, 30*24*time.Hour)
	var delta Delta
	for key, e := range l.Entries {
		ce := ClusterEngine{Cluster: e.Cluster, Engine: e.Engine}
		if e.State == StateActive && healthy[ce] && !seenThisRun[key] {
			e.State = StateRetired
			t := now
			e.RetiredAt = &t
			e.ConsecutiveRuns = 0
			delta.Retired = append(delta.Retired, e)
		}
		if e.State == StateRetired {
			if e.RetiredAt != nil && now.Sub(*e.RetiredAt) > retireWindow {
				delete(l.Entries, key)
			}
			continue
		}
		delta.ActiveTotal++
		if !healthy[ce] {
			continue
		}
		if isExemptActive(e, now) {
			delta.ExemptActive++
			continue
		}
		if e.ConsecutiveRuns < cfg.MinConsecutive {
			delta.HeldConsecutive++
			continue
		}
		if reason := dueReason(e, cfg.RefreshWindow, now); reason != "" {
			delta.Due = append(delta.Due, DueFinding{Reason: reason, Entry: e})
		} else {
			delta.MutedRefresh++
		}
	}

	slices.SortFunc(delta.Due, func(a, b DueFinding) int {
		if c := cmp.Compare(severityRank(b.Entry.Severity), severityRank(a.Entry.Severity)); c != 0 {
			return c
		}
		return cmp.Compare(a.Entry.SubjectKey, b.Entry.SubjectKey)
	})
	slices.SortFunc(delta.Retired, func(a, b *LedgerEntry) int {
		return cmp.Compare(a.SubjectKey, b.SubjectKey)
	})
	return delta
}

// MarkReported records successful outbound delivery for due findings.
func MarkReported(due []DueFinding, now time.Time, prURLs map[string]string) {
	t := now.UTC()
	for _, d := range due {
		ts := t
		d.Entry.LastReportedAt = &ts
		d.Entry.ReportedSeverity = d.Entry.Severity
		if u := prURLs[d.Entry.SubjectKey]; u != "" {
			d.Entry.PRURL = u
		}
	}
}

func upsertObservation(l *Ledger, o Observation, now time.Time) {
	e, ok := l.Entries[o.SubjectKey]
	if !ok || e.State != StateActive {
		e = &LedgerEntry{
			SubjectKey:      o.SubjectKey,
			Cluster:         o.Cluster,
			Engine:          o.Engine,
			FirstSeenAt:     now,
			ConsecutiveRuns: 1,
			State:           StateActive,
		}
		l.Entries[o.SubjectKey] = e
	} else {
		e.ConsecutiveRuns++
	}
	e.LastSeenAt = now
	e.Fingerprint = o.Fingerprint
	e.Kind = o.Kind
	e.Severity = o.Severity
	e.Namespace = o.Namespace
	e.ObjectKind = o.ObjectKind
	e.Name = o.Name
	e.Reason = o.Reason
	e.SuspectedCause = o.SuspectedCause
	e.Message = o.Message
	e.Details = maps.Clone(o.Details)
	if o.Remediation != nil {
		cp := *o.Remediation
		e.Remediation = &cp
	} else {
		e.Remediation = nil
	}
	e.ExemptReason = o.ExemptReason
	e.ExemptExpires = o.ExemptExpires
}

func isExemptActive(e *LedgerEntry, now time.Time) bool {
	if e.ExemptReason == "" || e.ExemptExpires == "" {
		return false
	}
	exp, err := time.Parse(time.RFC3339, e.ExemptExpires)
	if err != nil {
		return false
	}
	return exp.After(now)
}

func dueReason(e *LedgerEntry, refreshWindow time.Duration, now time.Time) string {
	if e.LastReportedAt == nil {
		return DueNew
	}
	if severityRank(e.Severity) > severityRank(e.ReportedSeverity) {
		return DueEscalated
	}
	if now.Sub(*e.LastReportedAt) >= refreshWindow {
		return DueRefresh
	}
	return ""
}

func severityRank(s string) int {
	switch s {
	case "critical":
		return 3
	case "warning":
		return 2
	default:
		return 1
	}
}
