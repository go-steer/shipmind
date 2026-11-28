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
	"regexp"
	"slices"
	"strings"
)

// GCPRegions lists all Google Cloud regions in canonical sorted order. Used
// as the default *.gke.goog egress wildcard set for chat tasks whose
// agent.clusters.scope includes a broad project or fleet entry without an
// explicit agent.clusters.regions allowlist (#104).
var GCPRegions = []string{
	"africa-south1",
	"asia-east1",
	"asia-east2",
	"asia-northeast1",
	"asia-northeast2",
	"asia-northeast3",
	"asia-south1",
	"asia-south2",
	"asia-southeast1",
	"asia-southeast2",
	"asia-southeast3",
	"australia-southeast1",
	"australia-southeast2",
	"europe-central2",
	"europe-north1",
	"europe-north2",
	"europe-southwest1",
	"europe-west1",
	"europe-west10",
	"europe-west12",
	"europe-west2",
	"europe-west3",
	"europe-west4",
	"europe-west6",
	"europe-west8",
	"europe-west9",
	"me-central1",
	"me-central2",
	"me-west1",
	"northamerica-northeast1",
	"northamerica-northeast2",
	"northamerica-south1",
	"southamerica-east1",
	"southamerica-west1",
	"us-central1",
	"us-central2",
	"us-east1",
	"us-east4",
	"us-east5",
	"us-south1",
	"us-west1",
	"us-west2",
	"us-west3",
	"us-west4",
}

// Clusters configures the agent's home cluster context and live cluster
// discovery scope (#104).
type Clusters struct {
	// Home is the full GKE resource path of the cluster where this agent
	// runs ("projects/<project>/locations/<location>/clusters/<name>").
	// Required; used as context, incident/watchboard egress, and canary
	// default, never as a silent chat default.
	Home string `yaml:"home"`
	// Regions optionally narrows the *.<region>.gke.goog egress allowlist
	// when Scope includes broad project or fleet entries. Omitting it on
	// a broad scope admits all GCPRegions. Always combined with Home's
	// region.
	Regions []string `yaml:"regions,omitempty"`
	// Scope is the required non-empty list of live cluster discovery
	// targets (project [+ optional location], explicit cluster path, or
	// GKE fleet host project).
	Scope []ClusterScope `yaml:"scope"`
}

// ClusterScope is one target in agent.clusters.scope or audits.scope.
// Exactly one of Project, Path, or Fleet must be set; Location is valid
// only alongside Project.
type ClusterScope struct {
	Project  string `yaml:"project,omitempty"`
	Location string `yaml:"location,omitempty"`
	Path     string `yaml:"path,omitempty"`
	Fleet    string `yaml:"fleet,omitempty"`
}

var (
	regionRE   = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)
	locationRE = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+(-[a-z])?$`)
)

// ParseClusterPath validates and splits a full GKE resource path
// ("projects/<project>/locations/<location>/clusters/<name>").
func ParseClusterPath(path string) (project, location, name string, ok bool) {
	if !clusterPath.MatchString(path) {
		return "", "", "", false
	}
	parts := strings.Split(path, "/")
	return parts[1], parts[3], parts[5], true
}

// LocationRegion strips a GKE single-letter zone suffix ("us-central1-b" ->
// "us-central1"), leaving regional locations ("us-central1") unchanged. GKE
// control-plane DNS endpoints (*.<region>.gke.goog) always use the region.
func LocationRegion(location string) string {
	if i := strings.LastIndexByte(location, '-'); i > 0 && i == len(location)-2 {
		if c := location[i+1]; c >= 'a' && c <= 'z' {
			return location[:i]
		}
	}
	return location
}

// IsBroad reports whether s can match clusters across multiple regions
// (a bare project without a location, or a GKE fleet host project).
func (s ClusterScope) IsBroad() bool {
	return (s.Project != "" && s.Location == "") || s.Fleet != ""
}

// Display formats s as a concise human-readable scope descriptor.
func (s ClusterScope) Display() string {
	switch {
	case s.Project != "" && s.Location != "":
		return fmt.Sprintf("project `%s` (location `%s`)", s.Project, s.Location)
	case s.Project != "":
		return fmt.Sprintf("project `%s` (all locations)", s.Project)
	case s.Path != "":
		return fmt.Sprintf("cluster `%s`", s.Path)
	case s.Fleet != "":
		return fmt.Sprintf("fleet `%s`", s.Fleet)
	default:
		return ""
	}
}

func (s ClusterScope) dedupKey() string {
	switch {
	case s.Project != "":
		return "project:" + s.Project + "/" + s.Location
	case s.Path != "":
		return "path:" + s.Path
	default:
		return "fleet:" + s.Fleet
	}
}

// HomeName returns the cluster name segment of c.Home, or "" if Home is unset
// or invalid.
func (c Clusters) HomeName() string {
	_, _, name, _ := ParseClusterPath(c.Home)
	return name
}

// HomeRegion returns the GCP region (zone-stripped) of c.Home, or "" if Home
// is unset or invalid.
func (c Clusters) HomeRegion() string {
	_, loc, _, ok := ParseClusterPath(c.Home)
	if !ok {
		return ""
	}
	return LocationRegion(loc)
}

// ResolveCluster resolves a CLI -cluster argument (empty, bare name, or full
// GKE resource path) to a full cluster path. Empty defaults to c.Home; a full
// GKE resource path is accepted directly; a bare name matches c.Home or a
// unique explicit path entry in c.Scope.
func (c Clusters) ResolveCluster(ref string) (string, error) {
	if c.Home == "" {
		return "", fmt.Errorf("the environment sets no agent.clusters.home to ask about")
	}
	if ref == "" {
		return c.Home, nil
	}
	if _, _, _, ok := ParseClusterPath(ref); ok {
		return ref, nil
	}
	var matches []string
	addIfName := func(path string) {
		if _, _, name, ok := ParseClusterPath(path); ok && name == ref && !slices.Contains(matches, path) {
			matches = append(matches, path)
		}
	}
	addIfName(c.Home)
	for _, sc := range c.Scope {
		if sc.Path != "" {
			addIfName(sc.Path)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("cluster %q is not agent.clusters.home or an explicit path in agent.clusters.scope; pass a full projects/<project>/locations/<location>/clusters/<name> path", ref)
	default:
		return "", fmt.Errorf("cluster %q matches multiple paths (%s); pass a full projects/<project>/locations/<location>/clusters/<name> path", ref, strings.Join(matches, ", "))
	}
}

// EffectiveAuditScope returns audits.scope when set, else agent.clusters.scope.
func (e *Env) EffectiveAuditScope() []ClusterScope {
	if e.Audits != nil && len(e.Audits.Scope) > 0 {
		return e.Audits.Scope
	}
	return e.Agent.Clusters.Scope
}

func checkClusters(c Clusters, fail func(string, ...any)) {
	if _, _, _, ok := ParseClusterPath(c.Home); !ok {
		fail("agent.clusters.home %q: want projects/<project>/locations/<location>/clusters/<name>", c.Home)
	}
	seenRegions := map[string]bool{}
	for i, r := range c.Regions {
		if !regionRE.MatchString(r) {
			fail("agent.clusters.regions[%d] %q: want a GCP region (e.g. us-central1), not a zone", i, r)
			continue
		}
		if seenRegions[r] {
			fail("agent.clusters.regions: %q listed twice", r)
		}
		seenRegions[r] = true
	}
	if len(c.Scope) == 0 {
		fail("agent.clusters.scope: want at least one scope entry")
		return
	}
	checkClusterScope("agent.clusters.scope", c.Scope, fail)
}

func checkClusterScope(prefix string, scope []ClusterScope, fail func(string, ...any)) {
	seen := map[string]bool{}
	for i, s := range scope {
		kinds := 0
		if s.Project != "" {
			kinds++
		}
		if s.Path != "" {
			kinds++
		}
		if s.Fleet != "" {
			kinds++
		}
		if kinds != 1 {
			fail("%s[%d]: set exactly one of project, path, or fleet", prefix, i)
			continue
		}
		if s.Location != "" && s.Project == "" {
			fail("%s[%d].location: only valid with project", prefix, i)
		}
		switch {
		case s.Project != "":
			if !projectRE.MatchString(s.Project) {
				fail("%s[%d].project %q: want a Google Cloud project ID", prefix, i, s.Project)
			}
			if s.Location != "" && !locationRE.MatchString(s.Location) {
				fail("%s[%d].location %q: want a GCP region or zone (e.g. us-central1 or us-central1-a)", prefix, i, s.Location)
			}
		case s.Path != "":
			if _, _, _, ok := ParseClusterPath(s.Path); !ok {
				fail("%s[%d].path %q: want projects/<project>/locations/<location>/clusters/<name>", prefix, i, s.Path)
			}
		case s.Fleet != "":
			if !projectRE.MatchString(s.Fleet) {
				fail("%s[%d].fleet %q: want a Google Cloud fleet host project ID", prefix, i, s.Fleet)
			}
		}
		key := s.dedupKey()
		if seen[key] {
			fail("%s: duplicate entry %s", prefix, s.Display())
		}
		seen[key] = true
	}
}
