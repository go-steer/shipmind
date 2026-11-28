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

package deploy

import (
	"bytes"
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

	"github.com/go-steer/shipmind/internal/cluster"
	"github.com/go-steer/shipmind/internal/env"
)

// GoogleTokenSecret is the Secret in CredentialsNamespace where
// token-refresher writes the agent's current Google OAuth access token.
const (
	GoogleTokenSecret      = "google-token"
	tokenRefresherName     = "token-refresher"
	scopeProbeTimeout      = 10 * time.Second
	maxScopeProbeErrorBody = 64 << 10
)

var errScopeTokenRejected = errors.New("agent Google token rejected")

// ScopeChecker verifies whether the agent's Google identity token can list
// GKE clusters in each in-scope project/location and memberships in each
// in-scope GKE fleet (#104). Implementations must never include the token
// in error messages.
type ScopeChecker interface {
	CheckProject(ctx context.Context, token, project, location string) error
	CheckFleet(ctx context.Context, token, hostProject string) error
}

// HTTPScopeChecker implements ScopeChecker directly against Google's
// Container and GKE Hub REST APIs with redirect following disabled so the
// agent's bearer token can never be forwarded off-host.
type HTTPScopeChecker struct {
	Client           *http.Client
	ContainerBaseURL string
	GKEHubBaseURL    string
}

func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func (h HTTPScopeChecker) httpClient() *http.Client {
	if h.Client != nil {
		if h.Client.CheckRedirect != nil {
			return h.Client
		}
		cp := *h.Client
		cp.CheckRedirect = noRedirect
		return &cp
	}
	return &http.Client{
		Timeout:       scopeProbeTimeout,
		CheckRedirect: noRedirect,
	}
}

// CheckProject probes container.googleapis.com clusters.list on project[/location].
func (h HTTPScopeChecker) CheckProject(ctx context.Context, token, project, location string) error {
	base := strings.TrimRight(h.ContainerBaseURL, "/")
	if base == "" {
		base = "https://container.googleapis.com"
	}
	loc := location
	if loc == "" {
		loc = "-"
	}
	endpoint := fmt.Sprintf("%s/v1/projects/%s/locations/%s/clusters", base, url.PathEscape(project), url.PathEscape(loc))
	return h.probe(ctx, endpoint, token, "container.googleapis.com",
		fmt.Sprintf("Kubernetes Engine API is disabled on project %s", project),
		fmt.Sprintf("agent Google identity cannot list GKE clusters in project %s (grant roles/container.viewer or equivalent)", project),
	)
}

// CheckFleet probes gkehub.googleapis.com memberships.list on hostProject.
func (h HTTPScopeChecker) CheckFleet(ctx context.Context, token, hostProject string) error {
	base := strings.TrimRight(h.GKEHubBaseURL, "/")
	if base == "" {
		base = "https://gkehub.googleapis.com"
	}
	endpoint := fmt.Sprintf("%s/v1/projects/%s/locations/-/memberships?pageSize=1", base, url.PathEscape(hostProject))
	return h.probe(ctx, endpoint, token, "gkehub.googleapis.com",
		fmt.Sprintf("GKE Hub API is disabled on fleet host project %s", hostProject),
		fmt.Sprintf("agent Google identity cannot list fleet memberships in %s (grant roles/gkehub.viewer)", hostProject),
	)
}

func (h HTTPScopeChecker) probe(ctx context.Context, endpoint, token, apiHost, disabledMsg, deniedMsg string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil) //nolint:gosec // fixed Google API host or test server URL
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.httpClient().Do(req) //nolint:gosec // fixed Google API host or test server URL
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", apiHost, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxScopeProbeErrorBody))
	if err != nil {
		return fmt.Errorf("reading response from %s: %w", apiHost, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("%w (HTTP 401; check Deployment %s/%s)", errScopeTokenRejected, CredentialsNamespace, tokenRefresherName)
	case http.StatusForbidden, http.StatusNotFound:
		if isGoogleAPIDisabled(body) {
			return fmt.Errorf("%s (HTTP %d)", disabledMsg, resp.StatusCode)
		}
		return fmt.Errorf("%s (HTTP %d)", deniedMsg, resp.StatusCode)
	default:
		return fmt.Errorf("unexpected HTTP %d from %s", resp.StatusCode, apiHost)
	}
}

func isGoogleAPIDisabled(body []byte) bool {
	var parsed struct {
		Error *struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) != nil || parsed.Error == nil {
		return false
	}
	for _, item := range parsed.Error.Errors {
		if item.Reason == "accessNotConfigured" || item.Reason == "SERVICE_DISABLED" {
			return true
		}
	}
	for _, item := range parsed.Error.Details {
		if item.Reason == "SERVICE_DISABLED" {
			return true
		}
	}
	return strings.Contains(parsed.Error.Message, "has not been used") || strings.Contains(parsed.Error.Message, "is disabled")
}

type scopeProbeTarget struct {
	what     string
	fleet    string
	project  string
	location string
}

func collectScopeProbeTargets(e *env.Env) []scopeProbeTarget {
	scopes := slices.Clone(e.Agent.Clusters.Scope)
	if e.Audits != nil && len(e.Audits.Scope) > 0 {
		scopes = append(scopes, e.Audits.Scope...)
	}
	allLocProjects := map[string]bool{}
	for _, s := range scopes {
		if s.Project != "" && s.Location == "" {
			allLocProjects[s.Project] = true
		}
	}
	seen := map[string]bool{}
	var targets []scopeProbeTarget
	addTarget := func(t scopeProbeTarget) {
		if !seen[t.what] {
			seen[t.what] = true
			targets = append(targets, t)
		}
	}
	for _, s := range scopes {
		switch {
		case s.Project != "" && s.Location == "":
			addTarget(scopeProbeTarget{what: "scope project/" + s.Project, project: s.Project})
		case s.Project != "" && s.Location != "":
			if !allLocProjects[s.Project] {
				addTarget(scopeProbeTarget{what: "scope project/" + s.Project + "/" + s.Location, project: s.Project, location: s.Location})
			}
		case s.Path != "":
			if p, l, _, ok := env.ParseClusterPath(s.Path); ok && !allLocProjects[p] {
				addTarget(scopeProbeTarget{what: "scope project/" + p + "/" + l, project: p, location: l})
			}
		case s.Fleet != "":
			addTarget(scopeProbeTarget{what: "scope fleet/" + s.Fleet, fleet: s.Fleet})
		}
	}
	return targets
}

// scopeAccessStatus probes every unique in-scope project/location and fleet
// using the agent's refreshed Google access token from Secret
// ate-credentials/google-token, reporting missing IAM grants as Status
// findings so they surface at deploy time rather than mid-investigation (#104).
func scopeAccessStatus(ctx context.Context, p *Plan, c cluster.Cluster, add func(what, detail string)) error {
	if p.ScopeAccess == nil {
		return nil
	}
	targets := collectScopeProbeTargets(p.Env)
	if len(targets) == 0 {
		return nil
	}
	data, found, err := c.Secret(ctx, CredentialsNamespace, GoogleTokenSecret)
	if err != nil {
		return fmt.Errorf("secret %s/%s: %w", CredentialsNamespace, GoogleTokenSecret, err)
	}
	tok := string(bytes.TrimSpace(data["token"]))
	if !found || tok == "" {
		if _, hasRefresher := p.Objects.Find("Deployment", CredentialsNamespace, tokenRefresherName); hasRefresher {
			if !found {
				add("scope access", fmt.Sprintf("unverified: secret %s/%s is missing (%s has not written a token yet)", CredentialsNamespace, GoogleTokenSecret, tokenRefresherName))
			} else {
				add("scope access", fmt.Sprintf("unverified: secret %s/%s has an empty token key (%s has not written a token yet)", CredentialsNamespace, GoogleTokenSecret, tokenRefresherName))
			}
		}
		return nil
	}
	for _, t := range targets {
		var perr error
		if t.fleet != "" {
			perr = p.ScopeAccess.CheckFleet(ctx, tok, t.fleet)
		} else {
			perr = p.ScopeAccess.CheckProject(ctx, tok, t.project, t.location)
		}
		if perr != nil {
			if errors.Is(perr, errScopeTokenRejected) {
				add("scope access", perr.Error())
				return nil
			}
			add(t.what, perr.Error())
		}
	}
	return nil
}
