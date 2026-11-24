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

// Package audit runs scheduled deterministic posture sweeps (`lookout audit`
// and `leeway report`), maintains a persistent deduplication ledger, posts
// digests to switchboard's outbound ingress, and optionally dispatches GitOps
// pull request proposals when `audits.autoPR` is enabled (#127).
package audit

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// ClusterWideNamespace occupies the namespace slot of a leeway report
	// finding key when the subject is cluster-scoped (go-steer/leeway#30).
	ClusterWideNamespace = "_cluster"

	EngineLookout = "lookout"
	EngineLeeway  = "leeway"

	generatedAlphabet    = "bcdfghjklmnpqrstvwxz2456789"
	minGeneratedLen      = 5
	maxGeneratedLen      = 10
	maxGeneratedSuffixes = 2
)

var generatedSet = func() map[rune]struct{} {
	m := make(map[rune]struct{}, len(generatedAlphabet))
	for _, r := range generatedAlphabet {
		m[r] = struct{}{}
	}
	return m
}()

// ParseGKEClusterPath splits "projects/<p>/locations/<l>/clusters/<c>" into
// its three segments.
func ParseGKEClusterPath(path string) (project, location, cluster string, ok bool) {
	parts := strings.Split(strings.TrimSpace(path), "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "locations" || parts[4] != "clusters" {
		return "", "", "", false
	}
	if parts[1] == "" || parts[3] == "" || parts[5] == "" {
		return "", "", "", false
	}
	return parts[1], parts[3], parts[5], true
}

// CanonicalClusterID returns "<project>/<location>/<cluster>" for GKE cluster
// paths or "gke:<project>/<location>/<cluster>" sources, matching
// go-steer/leeway's report.CanonicalCluster (leeway#30).
func CanonicalClusterID(raw string) string {
	s := strings.TrimSpace(raw)
	if p, l, c, ok := ParseGKEClusterPath(s); ok {
		return p + "/" + l + "/" + c
	}
	if rest, ok := strings.CutPrefix(s, "gke:"); ok {
		triple := strings.TrimSpace(rest)
		parts := strings.Split(triple, "/")
		if len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != "" {
			return triple
		}
	}
	if rest, ok := strings.CutPrefix(strings.TrimPrefix(s, "kubeconfig:"), "gke_"); ok {
		parts := strings.Split(strings.TrimSpace(rest), "_")
		if len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != "" {
			return parts[0] + "/" + parts[1] + "/" + parts[2]
		}
	}
	return s
}

// NormalizeName strips up to two Kubernetes-generated pod/ReplicaSet suffixes,
// matching k8s-lookout's findings.NormalizeName (`audit-ingestion-contract.md` §4).
func NormalizeName(name string) string {
	if name == "" {
		return ""
	}
	segs := strings.Split(name, "-")
	stripped := 0
	for stripped < maxGeneratedSuffixes && len(segs) > 1 {
		if !isGeneratedSegment(segs[len(segs)-1]) {
			break
		}
		segs = segs[:len(segs)-1]
		stripped++
	}
	return strings.Join(segs, "-")
}

func isGeneratedSegment(seg string) bool {
	if len(seg) < minGeneratedLen || len(seg) > maxGeneratedLen {
		return false
	}
	for _, r := range seg {
		if _, ok := generatedSet[r]; !ok {
			return false
		}
	}
	return true
}

// CanonicalReason mirrors k8s-lookout's engine.CanonicalReason family collapse;
// posture audit reasons pass through unchanged.
func CanonicalReason(reason string) string {
	switch reason {
	case "ErrImagePull", "InvalidImageName":
		return "ImagePullBackOff"
	default:
		return reason
	}
}

// LookoutSubjectKey derives the instance subject key for a lookout audit
// finding (`<cluster>/<namespace>/<Kind>/<normalized-name>/<reason>`).
func LookoutSubjectKey(cluster, namespace, kindOfObject, name, reason, kind string) string {
	r := CanonicalReason(strings.TrimSpace(reason))
	if r == "" {
		r = strings.TrimSpace(kind)
	}
	return strings.Join([]string{
		strings.TrimSpace(cluster),
		strings.TrimSpace(namespace),
		strings.TrimSpace(kindOfObject),
		NormalizeName(strings.TrimSpace(name)),
		r,
	}, "/")
}

// LeewayTopologyKey returns the canonical 6-part deduplication key documented
// in go-steer/leeway docs/stability-policy.md §2.2 (leeway#30):
//
//	<cluster>/<namespace>/<subject.kind>/<subject.name>/<topologyKey>/<kind>
func LeewayTopologyKey(cluster string, f LeewayFinding) string {
	ns := strings.TrimSpace(f.Subject.Namespace)
	if ns == "" {
		ns = ClusterWideNamespace
	}
	sKind := strings.TrimSpace(f.Subject.Kind)
	if sKind == "" {
		sKind = "Cluster"
	}
	fKind := strings.TrimSpace(f.Kind)
	sName := strings.TrimSpace(f.Subject.Name)
	if sName == "" {
		if fKind != "" {
			sName = fKind
		} else {
			sName = "unknown"
		}
	}
	topoKey := strings.TrimSpace(f.TopologyKey)
	if topoKey == "" {
		topoKey = "unknown"
	}
	if fKind == "" {
		fKind = "unknown"
	}
	return strings.Join([]string{
		strings.TrimSpace(cluster),
		ns,
		sKind,
		sName,
		topoKey,
		fKind,
	}, "/")
}

// LeewayRankKey returns the canonical 6-part deduplication key documented in
// go-steer/leeway docs/stability-policy.md §2.3 (leeway#30):
//
//	<cluster>/_cluster/<objectKind>/<subject.name>/<ruleKey>/<kind>
func LeewayRankKey(cluster string, f LeewayRankFinding) string {
	objKind := strings.TrimSpace(f.ObjectKind)
	if objKind == "" {
		objKind = "ComputeClass"
	}
	name := strings.TrimSpace(f.Subject.Name)
	if name == "" {
		name = "unknown"
	}
	rule := strings.TrimSpace(f.Rule)
	ruleKey := "compute-class"
	if rule != "" {
		if f.Focus != nil && *f.Focus >= 0 {
			ruleKey = fmt.Sprintf("rule:%s/%d", rule, *f.Focus)
		} else {
			ruleKey = "rule:" + rule
		}
	}
	fKind := strings.TrimSpace(f.Kind)
	if fKind == "" {
		fKind = "unknown"
	}
	return strings.Join([]string{
		strings.TrimSpace(cluster),
		ClusterWideNamespace,
		objKind,
		name,
		ruleKey,
		fKind,
	}, "/")
}

// NeedsExplicitLookoutSubcommands reports whether lookoutVersion predates
// k8s-lookout v0.45.0 (#845), where `lookout audit all` skipped
// `audit computeclass` and `audit ai`.
func NeedsExplicitLookoutSubcommands(lookoutVersion string) bool {
	v := strings.TrimSpace(lookoutVersion)
	if !strings.HasPrefix(v, "v") {
		return false
	}
	core, _, _ := strings.Cut(v[1:], "-")
	core, _, _ = strings.Cut(core, "+")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	maj, err1 := strconv.Atoi(parts[0])
	minVer, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return maj == 0 && minVer < 45
}
