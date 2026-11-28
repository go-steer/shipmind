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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	maxDiscoveryBody  = 4 << 20
	maxFleetPages     = 100
	defaultAPITimeout = 30 * time.Second
)

// ScopeTarget is one live cluster discovery scope mounted in audit
// config.json (#104, #127). Exactly one of Project (+ optional Location),
// Path, or Fleet is populated.
type ScopeTarget struct {
	Project  string `json:"project,omitempty"`
	Location string `json:"location,omitempty"`
	Path     string `json:"path,omitempty"`
	Fleet    string `json:"fleet,omitempty"`
}

func (s ScopeTarget) label() string {
	switch {
	case s.Project != "" && s.Location != "":
		return "project:" + s.Project + "/" + s.Location
	case s.Project != "":
		return "project:" + s.Project
	case s.Path != "":
		return "path:" + s.Path
	case s.Fleet != "":
		return "fleet:" + s.Fleet
	default:
		return "empty"
	}
}

// SingleStaticCluster reports whether c names exactly one concrete cluster
// without live discovery (allowing an explicit single -context outside the
// cluster).
func (c *Config) SingleStaticCluster() bool {
	if len(c.Scope) == 0 {
		return len(c.Clusters) == 1
	}
	return len(c.Clusters) == 0 && len(c.Scope) == 1 && c.Scope[0].Path != ""
}

// resolveAuditClusters resolves cfg.Scope (and any static cfg.Clusters) into
// concrete, deduplicated, deterministically sorted ClusterTargets at sweep
// runtime. Per-scope discovery errors are recorded in errs so healthy scopes
// still run and unreachable clusters' ledger entries are never stale-closed.
func resolveAuditClusters(ctx context.Context, cfg *Config, opts RunOptions, logFn func(string, ...any)) ([]ClusterTarget, map[string]string) {
	if len(cfg.Scope) == 0 {
		return cfg.Clusters, nil
	}
	errs := map[string]string{}
	seen := map[string]bool{}
	var targets []ClusterTarget
	addTarget := func(cl ClusterTarget) {
		cid := CanonicalClusterID(cl.Path)
		if cid == "" {
			cid = cl.Name
		}
		if seen[cid] {
			return
		}
		seen[cid] = true
		targets = append(targets, cl)
	}
	for _, cl := range cfg.Clusters {
		addTarget(cl)
	}

	needsToken := false
	for _, s := range cfg.Scope {
		if s.Project != "" || s.Fleet != "" {
			needsToken = true
			break
		}
	}
	var (
		tok    string
		tokErr error
	)
	if needsToken {
		if opts.GoogleToken == nil {
			tokErr = errors.New("google token provider is required to resolve live project/fleet audit scope")
		} else {
			tok, tokErr = opts.GoogleToken(ctx)
			tok = strings.TrimSpace(tok)
			if tokErr == nil && tok == "" {
				tokErr = errors.New("google token provider returned an empty token")
			}
		}
	}

	hc := auditHTTPClient(opts.HTTPClient)
	containerBase := "https://container.googleapis.com"
	gkehubBase := "https://gkehub.googleapis.com"
	if base := strings.TrimRight(opts.GCPBaseURL, "/"); base != "" {
		containerBase = base
		gkehubBase = base
	}
	projNumCache := map[string]string{}

	for _, s := range cfg.Scope {
		switch {
		case s.Path != "":
			_, _, name, ok := ParseGKEClusterPath(s.Path)
			if !ok {
				errs["scope:"+s.label()] = fmt.Sprintf("invalid GKE cluster path %q", s.Path)
				continue
			}
			addTarget(ClusterTarget{Name: name, Path: s.Path})
		case s.Project != "":
			if tokErr != nil {
				errs["scope:"+s.label()] = tokErr.Error()
				continue
			}
			found, missingZones, err := listProjectClusters(ctx, hc, containerBase, tok, s.Project, s.Location)
			if err != nil {
				errs["scope:"+s.label()] = err.Error()
				logFn("scope %s discovery failed: %v", s.label(), err)
				continue
			}
			if len(missingZones) > 0 {
				msg := fmt.Sprintf("partial discovery: missing zones %s", strings.Join(missingZones, ", "))
				errs["scope:"+s.label()+":missing_zones"] = msg
				logFn("scope %s %s", s.label(), msg)
			}
			for _, cl := range found {
				addTarget(cl)
			}
		case s.Fleet != "":
			if tokErr != nil {
				errs["scope:"+s.label()] = tokErr.Error()
				continue
			}
			found, warnings, err := listFleetClusters(ctx, hc, gkehubBase, containerBase, tok, s.Fleet, projNumCache)
			if err != nil {
				errs["scope:"+s.label()] = err.Error()
				logFn("scope %s discovery failed: %v", s.label(), err)
				continue
			}
			for k, msg := range warnings {
				errs[k] = msg
				logFn("scope %s %s", s.label(), msg)
			}
			for _, cl := range found {
				addTarget(cl)
			}
		default:
			errs["scope:empty"] = "scope entry sets none of project, path, or fleet"
		}
	}

	slices.SortFunc(targets, func(a, b ClusterTarget) int {
		return strings.Compare(CanonicalClusterID(a.Path), CanonicalClusterID(b.Path))
	})
	return targets, errs
}

func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func auditHTTPClient(custom *http.Client) *http.Client {
	if custom != nil {
		if custom.CheckRedirect != nil {
			return custom
		}
		cp := *custom
		cp.CheckRedirect = noRedirect
		return &cp
	}
	return &http.Client{
		Timeout:       defaultAPITimeout,
		CheckRedirect: noRedirect,
	}
}

func listProjectClusters(ctx context.Context, hc *http.Client, baseURL, tok, project, location string) ([]ClusterTarget, []string, error) {
	loc := location
	if loc == "" {
		loc = "-"
	}
	endpoint := fmt.Sprintf("%s/v1/projects/%s/locations/%s/clusters", baseURL, url.PathEscape(project), url.PathEscape(loc))
	body, err := getGoogleJSON(ctx, hc, endpoint, tok)
	if err != nil {
		return nil, nil, err
	}
	var parsed struct {
		Clusters []struct {
			Name     string `json:"name"`
			Location string `json:"location"`
		} `json:"clusters"`
		MissingZones []string `json:"missingZones"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nil, fmt.Errorf("parse container clusters.list response: %w", err)
	}
	out := make([]ClusterTarget, 0, len(parsed.Clusters))
	for _, c := range parsed.Clusters {
		cloc := strings.TrimSpace(c.Location)
		if cloc == "" && location != "" {
			cloc = location
		}
		name := strings.TrimSpace(c.Name)
		if name == "" || cloc == "" {
			continue
		}
		path := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", project, cloc, name)
		out = append(out, ClusterTarget{Name: name, Path: path})
	}
	return out, sortUniqueStrings(parsed.MissingZones), nil
}

func listFleetClusters(ctx context.Context, hc *http.Client, gkehubBase, containerBase, tok, hostProject string, projNumCache map[string]string) ([]ClusterTarget, map[string]string, error) {
	var (
		out         []ClusterTarget
		unreachable []string
		pageToken   string
	)
	warnings := map[string]string{}
	for page := range maxFleetPages {
		endpoint := fmt.Sprintf("%s/v1/projects/%s/locations/-/memberships", gkehubBase, url.PathEscape(hostProject))
		if pageToken != "" {
			endpoint += "?pageToken=" + url.QueryEscape(pageToken)
		}
		body, err := getGoogleJSON(ctx, hc, endpoint, tok)
		if err != nil {
			return nil, nil, err
		}
		var parsed struct {
			Resources []struct {
				Endpoint *struct {
					GkeCluster *struct {
						ResourceLink   string `json:"resourceLink"`
						ClusterMissing bool   `json:"clusterMissing"`
					} `json:"gkeCluster"`
				} `json:"endpoint"`
			} `json:"resources"`
			Unreachable   []string `json:"unreachable"`
			NextPageToken string   `json:"nextPageToken"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, nil, fmt.Errorf("parse gkehub memberships.list response: %w", err)
		}
		unreachable = append(unreachable, parsed.Unreachable...)
		for _, m := range parsed.Resources {
			if m.Endpoint == nil || m.Endpoint.GkeCluster == nil || m.Endpoint.GkeCluster.ClusterMissing {
				continue
			}
			p, l, name, ok := parseHubResourceLink(m.Endpoint.GkeCluster.ResourceLink)
			if !ok {
				continue
			}
			if isAllDigits(p) {
				id, rerr := resolveFleetMemberProjectID(ctx, hc, containerBase, tok, p, l, name, projNumCache)
				if rerr != nil {
					warnings[fmt.Sprintf("scope:fleet:%s:member:%s/%s/%s", hostProject, p, l, name)] = rerr.Error()
					continue
				}
				p = id
			}
			out = append(out, ClusterTarget{
				Name: name,
				Path: fmt.Sprintf("projects/%s/locations/%s/clusters/%s", p, l, name),
			})
		}
		if parsed.NextPageToken == "" {
			if u := sortUniqueStrings(unreachable); len(u) > 0 {
				warnings["scope:fleet:"+hostProject+":unreachable"] = fmt.Sprintf("partial discovery: unreachable fleet locations %s", strings.Join(u, ", "))
			}
			return out, warnings, nil
		}
		if page == maxFleetPages-1 {
			return nil, nil, fmt.Errorf("fleet %s membership listing exceeded %d pages", hostProject, maxFleetPages)
		}
		pageToken = parsed.NextPageToken
	}
	return out, warnings, nil
}

// resolveFleetMemberProjectID translates a numeric GKE Hub resourceLink project
// number into its canonical GCP project ID via Container API clusters.get
// selfLink (always the cluster's own project, never workloadPool which carries
// the fleet host project under Fleet Workload Identity), caching number->resolved
// per sweep so fleet members deduplicate with project/path entries and keep
// stable ledger keys. When clusters.get returns HTTP 200 with a numeric or empty
// selfLink, it falls back to projectNum rather than dropping a reachable cluster.
func resolveFleetMemberProjectID(ctx context.Context, hc *http.Client, containerBase, tok, projectNum, loc, name string, cache map[string]string) (string, error) {
	if id, ok := cache[projectNum]; ok {
		return id, nil
	}
	endpoint := fmt.Sprintf("%s/v1/projects/%s/locations/%s/clusters/%s", containerBase, url.PathEscape(projectNum), url.PathEscape(loc), url.PathEscape(name))
	body, err := getGoogleJSON(ctx, hc, endpoint, tok)
	if err != nil {
		return "", fmt.Errorf("resolve project number %s for cluster %s/%s: %w", projectNum, loc, name, err)
	}
	var parsed struct {
		SelfLink string `json:"selfLink"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse container clusters.get response for project %s: %w", projectNum, err)
	}
	resolved := projectNum
	if id, ok := parseContainerSelfLinkProject(parsed.SelfLink); ok {
		resolved = id
	}
	cache[projectNum] = resolved
	return resolved, nil
}

func parseContainerSelfLinkProject(selfLink string) (string, bool) {
	_, rest, found := strings.Cut(strings.TrimSpace(selfLink), "/projects/")
	if !found {
		return "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 5 || (parts[1] != "locations" && parts[1] != "zones") || parts[3] != "clusters" {
		return "", false
	}
	if parts[0] == "" || isAllDigits(parts[0]) {
		return "", false
	}
	return parts[0], true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func sortUniqueStrings(in []string) []string {
	var out []string
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// parseHubResourceLink splits a GKE Hub membership resourceLink:
// "//container.googleapis.com/projects/<p>/locations/<l>/clusters/<c>" (or zones/<l>).
func parseHubResourceLink(link string) (project, location, name string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(link), "//container.googleapis.com/")
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 6 || parts[0] != "projects" || (parts[2] != "locations" && parts[2] != "zones") || parts[4] != "clusters" {
		return "", "", "", false
	}
	if parts[1] == "" || parts[3] == "" || parts[5] == "" {
		return "", "", "", false
	}
	return parts[1], parts[3], parts[5], true
}

func getGoogleJSON(ctx context.Context, hc *http.Client, endpoint, tok string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil) //nolint:gosec // fixed Google Container/GKE Hub API base or test server URL
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := hc.Do(req) //nolint:gosec // fixed Google Container/GKE Hub API base or test server URL
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d%s", resp.StatusCode, googleErrorSuffix(body))
	}
	return body, nil
}

func googleErrorSuffix(body []byte) string {
	var env struct {
		Error *struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil || env.Error == nil {
		return ""
	}
	msg := strings.TrimSpace(env.Error.Message)
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	switch {
	case env.Error.Status != "" && msg != "":
		return " (" + env.Error.Status + ": " + msg + ")"
	case env.Error.Status != "":
		return " (" + env.Error.Status + ")"
	case msg != "":
		return " (" + msg + ")"
	default:
		return ""
	}
}
