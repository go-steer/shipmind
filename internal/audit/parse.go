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
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strconv"
	"strings"
)

// LeewayReportVersion is the frozen wire schema version from go-steer/leeway#30.
const LeewayReportVersion = "leeway.report/v1alpha1"

// Remediation holds structured remediation metadata from leeway report.
type Remediation struct {
	Kind    string `json:"kind"`
	Summary string `json:"summary,omitempty"`
	Command string `json:"command,omitempty"`
	YAML    string `json:"yaml,omitempty"`
}

// Observation is one normalized posture finding from lookout audit or leeway report.
type Observation struct {
	SubjectKey     string            `json:"subject_key"`
	Cluster        string            `json:"cluster"`
	Engine         string            `json:"engine"`
	Fingerprint    string            `json:"fingerprint,omitempty"`
	Kind           string            `json:"kind"`
	Severity       string            `json:"severity"`
	Namespace      string            `json:"namespace,omitempty"`
	ObjectKind     string            `json:"object_kind,omitempty"`
	Name           string            `json:"name,omitempty"`
	Reason         string            `json:"reason,omitempty"`
	SuspectedCause string            `json:"suspected_cause,omitempty"`
	Message        string            `json:"message,omitempty"`
	Details        map[string]string `json:"details,omitempty"`
	Remediation    *Remediation      `json:"remediation,omitempty"`
	ExemptReason   string            `json:"exempt_reason,omitempty"`
	ExemptExpires  string            `json:"exempt_expires,omitempty"`
}

// LookoutSummary records the terminating summary line of a lookout audit pass.
type LookoutSummary struct {
	Scanned  int
	Findings int
	Exempt   int
	HasEx    bool
	Elapsed  string
}

var lookoutEnvelopeKeys = map[string]bool{
	"kind": true, "severity": true, "namespace": true, "kind_of_object": true,
	"name": true, "reason": true, "message": true, "fingerprint": true,
	"exempt_reason": true, "exempt_expires": true, "cluster": true,
}

// ParseLookoutStream parses a `lookout audit ... --format=json` stream per
// k8s-lookout's `docs/audit-ingestion-contract.md`. A missing summary line,
// record count mismatch, or stage failure (`scan.check_failed` /
// `scan.cluster_failed`) returns an error so callers never stale-close open
// findings on a truncated or degraded scan.
func ParseLookoutStream(cluster string, r io.Reader) ([]Observation, LookoutSummary, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	canonicalCluster := CanonicalClusterID(cluster)
	var (
		out         []Observation
		rawCount    int
		sawSummary  bool
		summary     LookoutSummary
		stageErrors []string
		lineNo      int
	)
	for sc.Scan() {
		lineNo++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		fields, err := parseJSONLine(text)
		if err != nil {
			return nil, summary, fmt.Errorf("lookout line %d: %w", lineNo, err)
		}
		kind, hasKind := fields["kind"]
		if !hasKind {
			sum, err := parseLookoutSummary(fields)
			if err != nil {
				return nil, summary, fmt.Errorf("lookout line %d: %w", lineNo, err)
			}
			sawSummary = true
			summary = sum
			continue
		}
		if sawSummary {
			return nil, summary, fmt.Errorf("lookout line %d: finding appeared after summary line", lineNo)
		}
		rawCount++
		switch kind {
		case "scan.cluster_failed", "scan.check_failed", "cloud.unavailable", "read.unavailable":
			stageErrors = append(stageErrors, fmt.Sprintf("%s: %s", kind, fields["message"]))
			continue
		case "scan.check_skipped":
			continue
		}
		out = append(out, lookoutObservation(canonicalCluster, fields))
	}
	if err := sc.Err(); err != nil {
		return nil, summary, fmt.Errorf("read lookout stream: %w", err)
	}
	if !sawSummary {
		return nil, summary, errors.New("lookout stream ended without a terminating summary line")
	}
	if rawCount != summary.Findings {
		return nil, summary, fmt.Errorf("lookout summary declared findings=%d, stream contained %d record(s)", summary.Findings, rawCount)
	}
	if len(stageErrors) > 0 {
		return nil, summary, fmt.Errorf("lookout incomplete scan: %s", strings.Join(stageErrors, "; "))
	}
	return out, summary, nil
}

func parseJSONLine(text string) (map[string]string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("malformed JSON record: %w", err)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			out[k] = s
			continue
		}
		out[k] = strings.TrimSpace(string(v))
	}
	return out, nil
}

func parseLookoutSummary(fields map[string]string) (LookoutSummary, error) {
	scannedStr, okS := fields["scanned"]
	findingsStr, okF := fields["findings"]
	if !okS || !okF {
		return LookoutSummary{}, errors.New("record has no kind and is not a summary line")
	}
	scanned, err1 := strconv.Atoi(scannedStr)
	findings, err2 := strconv.Atoi(findingsStr)
	if err1 != nil || err2 != nil || scanned < 0 || findings < 0 {
		return LookoutSummary{}, fmt.Errorf("invalid summary counts scanned=%q findings=%q", scannedStr, findingsStr)
	}
	if cfStr, ok := fields["clusters_failed"]; ok {
		if cf, err := strconv.Atoi(cfStr); err != nil || cf > 0 {
			return LookoutSummary{}, fmt.Errorf("summary reports clusters_failed=%s", cfStr)
		}
	}
	sum := LookoutSummary{
		Scanned:  scanned,
		Findings: findings,
		Elapsed:  fields["elapsed"],
	}
	if exStr, ok := fields["exempt"]; ok {
		ex, err := strconv.Atoi(exStr)
		if err != nil || ex < 0 {
			return LookoutSummary{}, fmt.Errorf("invalid exempt count %q", exStr)
		}
		sum.Exempt = ex
		sum.HasEx = true
	}
	return sum, nil
}

func lookoutObservation(cluster string, fields map[string]string) Observation {
	var details map[string]string
	for k, v := range fields {
		if lookoutEnvelopeKeys[k] || v == "" {
			continue
		}
		if details == nil {
			details = map[string]string{}
		}
		details[k] = v
	}
	ns := fields["namespace"]
	objKind := fields["kind_of_object"]
	name := fields["name"]
	reason := fields["reason"]
	kind := fields["kind"]
	return Observation{
		SubjectKey:    LookoutSubjectKey(cluster, ns, objKind, name, reason, kind),
		Cluster:       cluster,
		Engine:        EngineLookout,
		Fingerprint:   fields["fingerprint"],
		Kind:          kind,
		Severity:      normalizeSeverity(fields["severity"]),
		Namespace:     ns,
		ObjectKind:    objKind,
		Name:          name,
		Reason:        reason,
		Message:       fields["message"],
		Details:       details,
		ExemptReason:  fields["exempt_reason"],
		ExemptExpires: fields["exempt_expires"],
	}
}

// LeewayReportWire is the frozen leeway.report/v1alpha1 JSON document.
type LeewayReportWire struct {
	SchemaVersion string                `json:"schema_version"`
	GeneratedAt   string                `json:"generated_at"`
	Clusters      []LeewayClusterReport `json:"clusters"`
}

// LeewayClusterReport is one cluster's slice of a leeway report.
type LeewayClusterReport struct {
	Cluster          string              `json:"cluster"`
	Source           string              `json:"source,omitempty"`
	Status           string              `json:"status"`
	Error            string              `json:"error,omitempty"`
	TopologyFindings []LeewayFinding     `json:"topology_findings,omitempty"`
	RankFindings     []LeewayRankFinding `json:"rank_findings,omitempty"`
}

// LeewaySubject identifies a Kubernetes or ComputeClass subject.
type LeewaySubject struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// LeewayFinding is a topology-drift finding in leeway.report/v1alpha1.
type LeewayFinding struct {
	Kind           string        `json:"kind"`
	Subject        LeewaySubject `json:"subject"`
	TopologyKey    string        `json:"topologyKey"`
	Tier           string        `json:"tier"`
	Severity       string        `json:"severity"`
	SuspectedCause string        `json:"suspectedCause,omitempty"`
	UntestedCauses []string      `json:"untestedCauses,omitempty"`
	Remediation    *Remediation  `json:"remediation,omitempty"`
}

// LeewayRankFinding is a ComputeClass priority-rank finding in leeway.report/v1alpha1.
type LeewayRankFinding struct {
	Kind       string        `json:"kind"`
	Subject    LeewaySubject `json:"subject"`
	ObjectKind string        `json:"objectKind"`
	Tier       string        `json:"tier"`
	Severity   string        `json:"severity"`
	Rule       string        `json:"rule"`
	Focus      *int          `json:"focus,omitempty"`
	Reason     string        `json:"reason"`
}

// LeewayParsed holds normalized observations and per-cluster health from leeway report.
type LeewayParsed struct {
	Observations  []Observation
	Healthy       map[string]bool
	ClusterErrors map[string]string
}

// ParseLeewayReport parses a leeway.report/v1alpha1 JSON payload and verifies
// expectedClusters coverage so unreachable clusters retain existing ledger state.
func ParseLeewayReport(raw []byte, expectedClusters []string) (*LeewayParsed, error) {
	var wire LeewayReportWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("malformed leeway report JSON: %w", err)
	}
	if wire.SchemaVersion != LeewayReportVersion {
		return nil, fmt.Errorf("unsupported leeway report schema_version %q (want %q)", wire.SchemaVersion, LeewayReportVersion)
	}
	res := &LeewayParsed{
		Healthy:       map[string]bool{},
		ClusterErrors: map[string]string{},
	}
	seen := map[string]bool{}
	for _, cr := range wire.Clusters {
		clusterID := CanonicalClusterID(cr.Source)
		if clusterID == "" || !strings.Contains(cr.Source, "gke:") {
			clusterID = CanonicalClusterID(cr.Cluster)
		}
		seen[clusterID] = true
		if cr.Status != "ok" {
			errMsg := cr.Error
			if errMsg == "" {
				errMsg = "status " + cr.Status
			}
			res.ClusterErrors[clusterID] = errMsg
			continue
		}
		res.Healthy[clusterID] = true
		for _, tf := range cr.TopologyFindings {
			res.Observations = append(res.Observations, topologyObservation(clusterID, tf))
		}
		for _, rf := range cr.RankFindings {
			res.Observations = append(res.Observations, rankObservation(clusterID, rf))
		}
	}
	for _, exp := range expectedClusters {
		cid := CanonicalClusterID(exp)
		if !seen[cid] {
			res.ClusterErrors[cid] = "cluster missing from leeway report"
		}
	}
	return res, nil
}

func topologyObservation(cluster string, tf LeewayFinding) Observation {
	details := map[string]string{"topology_key": tf.TopologyKey}
	if tf.Tier != "" {
		details["tier"] = tf.Tier
	}
	if len(tf.UntestedCauses) > 0 {
		details["untested_causes"] = strings.Join(tf.UntestedCauses, ",")
	}
	msg := tf.Kind + " on " + tf.TopologyKey
	if tf.Remediation != nil && tf.Remediation.Summary != "" {
		msg = tf.Remediation.Summary
	}
	var rem *Remediation
	if tf.Remediation != nil {
		cp := *tf.Remediation
		rem = &cp
	}
	return Observation{
		SubjectKey:     LeewayTopologyKey(cluster, tf),
		Cluster:        cluster,
		Engine:         EngineLeeway,
		Fingerprint:    tf.Kind,
		Kind:           tf.Kind,
		Severity:       normalizeSeverity(tf.Severity),
		Namespace:      tf.Subject.Namespace,
		ObjectKind:     tf.Subject.Kind,
		Name:           tf.Subject.Name,
		Reason:         tf.SuspectedCause,
		SuspectedCause: tf.SuspectedCause,
		Message:        msg,
		Details:        details,
		Remediation:    rem,
	}
}

func rankObservation(cluster string, rf LeewayRankFinding) Observation {
	details := map[string]string{}
	if rf.Tier != "" {
		details["tier"] = rf.Tier
	}
	if rf.Rule != "" {
		details["rule"] = rf.Rule
	}
	if rf.Focus != nil {
		details["focus"] = strconv.Itoa(*rf.Focus)
	}
	objKind := rf.ObjectKind
	if objKind == "" {
		objKind = "ComputeClass"
	}
	if len(details) == 0 {
		details = nil
	} else {
		details = maps.Clone(details)
	}
	return Observation{
		SubjectKey:  LeewayRankKey(cluster, rf),
		Cluster:     cluster,
		Engine:      EngineLeeway,
		Fingerprint: rf.Kind,
		Kind:        rf.Kind,
		Severity:    normalizeSeverity(rf.Severity),
		ObjectKind:  objKind,
		Name:        rf.Subject.Name,
		Reason:      rf.Rule,
		Message:     rf.Reason,
		Details:     details,
	}
}

func normalizeSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return "critical"
	case "warning", "warn":
		return "warning"
	default:
		return "info"
	}
}
