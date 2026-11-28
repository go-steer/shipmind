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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/shipmind/internal/agent"
	"github.com/go-steer/shipmind/internal/env"
	"github.com/go-steer/shipmind/internal/render"
)

type fakeScopeChecker struct {
	projects map[string]error
	fleets   map[string]error
	calls    []string
}

func (f *fakeScopeChecker) CheckProject(_ context.Context, token, project, location string) error {
	key := project
	if location != "" {
		key += "/" + location
	}
	f.calls = append(f.calls, "project:"+key+"@"+token)
	return f.projects[key]
}

func (f *fakeScopeChecker) CheckFleet(_ context.Context, token, hostProject string) error {
	f.calls = append(f.calls, "fleet:"+hostProject+"@"+token)
	return f.fleets[hostProject]
}

func TestHTTPScopeChecker(t *testing.T) {
	const secretToken = "test-agent-bearer-value-do-not-leak"

	t.Run("container and gkehub ok and errors", func(t *testing.T) {
		var seenAuth []string
		var seenPaths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seenAuth = append(seenAuth, r.Header.Get("Authorization"))
			seenPaths = append(seenPaths, r.URL.RequestURI())
			switch r.URL.Path {
			case "/v1/projects/ok-proj/locations/-/clusters":
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"clusters":[]}`))
			case "/v1/projects/ok-proj/locations/us-central1/clusters":
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"clusters":[]}`))
			case "/v1/projects/ok-fleet/locations/-/memberships":
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"resources":[]}`))
			case "/v1/projects/unauth-proj/locations/-/clusters":
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":401}}`))
			case "/v1/projects/denied-proj/locations/-/clusters":
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Required container.clusters.list permission"}}`))
			case "/v1/projects/disabled-proj/locations/-/clusters":
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Kubernetes Engine API has not been used","errors":[{"reason":"accessNotConfigured"}]}}`))
			case "/v1/projects/disabled-aip193-proj/locations/-/clusters":
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Permission denied","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SERVICE_DISABLED"}]}}`))
			case "/v1/projects/disabled-msg-fleet/locations/-/memberships":
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":403,"message":"GKE Hub API is disabled in project disabled-msg-fleet"}}`))
			case "/v1/projects/notfound-fleet/locations/-/memberships":
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`not json`))
			case "/v1/projects/server-err/locations/-/clusters":
				w.WriteHeader(http.StatusInternalServerError)
			case "/v1/projects/redirect-proj/locations/-/clusters":
				http.Redirect(w, r, "http://127.0.0.1:1/steal-token", http.StatusFound)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		chk := HTTPScopeChecker{
			Client:           srv.Client(),
			ContainerBaseURL: srv.URL,
			GKEHubBaseURL:    srv.URL,
		}
		ctx := context.Background()

		if err := chk.CheckProject(ctx, secretToken, "ok-proj", ""); err != nil {
			t.Fatalf("CheckProject ok: %v", err)
		}
		if err := chk.CheckProject(ctx, secretToken, "ok-proj", "us-central1"); err != nil {
			t.Fatalf("CheckProject location ok: %v", err)
		}
		if err := chk.CheckFleet(ctx, secretToken, "ok-fleet"); err != nil {
			t.Fatalf("CheckFleet ok: %v", err)
		}
		if !slices.Contains(seenPaths, "/v1/projects/ok-fleet/locations/-/memberships?pageSize=1") {
			t.Errorf("fleet paths = %v", seenPaths)
		}
		for _, auth := range seenAuth {
			if auth != "Bearer "+secretToken {
				t.Fatalf("Authorization header = %q", auth)
			}
		}

		for _, tc := range []struct {
			name string
			call func() error
			want string
		}{
			{"401", func() error { return chk.CheckProject(ctx, secretToken, "unauth-proj", "") }, "HTTP 401"},
			{"403 denied", func() error { return chk.CheckProject(ctx, secretToken, "denied-proj", "") }, "grant roles/container.viewer"},
			{"403 api disabled reason", func() error { return chk.CheckProject(ctx, secretToken, "disabled-proj", "") }, "Kubernetes Engine API is disabled"},
			{"403 api disabled AIP-193 details", func() error { return chk.CheckProject(ctx, secretToken, "disabled-aip193-proj", "") }, "Kubernetes Engine API is disabled"},
			{"403 api disabled msg", func() error { return chk.CheckFleet(ctx, secretToken, "disabled-msg-fleet") }, "GKE Hub API is disabled"},
			{"404 fleet denied", func() error { return chk.CheckFleet(ctx, secretToken, "notfound-fleet") }, "grant roles/gkehub.viewer"},
			{"500", func() error { return chk.CheckProject(ctx, secretToken, "server-err", "") }, "unexpected HTTP 500"},
			{"302 redirect refused", func() error { return chk.CheckProject(ctx, secretToken, "redirect-proj", "") }, "unexpected HTTP 302"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				err := tc.call()
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("err = %v, want substring %q", err, tc.want)
				}
				if err != nil && strings.Contains(err.Error(), secretToken) {
					t.Errorf("error leaked bearer token: %v", err)
				}
			})
		}
	})

	t.Run("transport error and default client", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		chk := HTTPScopeChecker{ContainerBaseURL: "http://127.0.0.1:1"}
		err := chk.CheckProject(ctx, secretToken, "p", "")
		if err == nil || !strings.Contains(err.Error(), "cannot reach container.googleapis.com") || strings.Contains(err.Error(), secretToken) {
			t.Errorf("canceled transport err = %v", err)
		}
		if err := (HTTPScopeChecker{ContainerBaseURL: "http://[::1]:bad"}).CheckProject(context.Background(), secretToken, "p", ""); err == nil {
			t.Error("bad base URL succeeded")
		}
	})

	t.Run("custom CheckRedirect preserved", func(t *testing.T) {
		customCalled := false
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			customCalled = true
			return http.ErrUseLastResponse
		}}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/other", http.StatusFound)
		}))
		defer srv.Close()
		chk := HTTPScopeChecker{Client: c, ContainerBaseURL: srv.URL}
		_ = chk.CheckProject(context.Background(), secretToken, "p", "")
		if !customCalled {
			t.Error("explicit custom CheckRedirect was overwritten")
		}
	})
}

func TestStatusScopeAccess(t *testing.T) {
	ctx := context.Background()

	// 1. All in-scope targets pass -> zero drift; deduplicates across agent.clusters.scope
	// and audits.scope, and bare project shadows project+location and path in that project.
	p := plan(t)
	p.Env.Agent.Clusters.Scope = []env.ClusterScope{
		{Project: "proj-a"},
		{Project: "proj-a", Location: "us-central1"},
		{Path: "projects/proj-a/locations/us-east1/clusters/c1"},
		{Project: "proj-b", Location: "europe-west1"},
		{Path: "projects/proj-b/locations/europe-west1/clusters/c2"},
		{Path: "projects/proj-c/locations/us-west1/clusters/c3"},
		{Fleet: "fleet-host"},
	}
	p.Env.Audits = &env.Audits{
		Schedule: "0 6 * * *",
		Scope:    []env.ClusterScope{{Project: "proj-d"}, {Fleet: "fleet-host"}},
	}
	chk := &fakeScopeChecker{}
	p.ScopeAccess = chk
	c := inSync(t)
	c.secrets[CredentialsNamespace+"/"+GoogleTokenSecret] = map[string][]byte{"token": []byte("tok-1\n")}
	// Ignore audit secret drift in this unit check by calling scopeAccessStatus directly
	// and via Status below on a clean plan.
	var got []string
	if err := scopeAccessStatus(ctx, p, c, func(what, detail string) { got = append(got, what+": "+detail) }); err != nil {
		t.Fatalf("scopeAccessStatus ok: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unexpected findings: %v", got)
	}
	wantCalls := []string{
		"project:proj-a@tok-1",
		"project:proj-b/europe-west1@tok-1",
		"project:proj-c/us-west1@tok-1",
		"fleet:fleet-host@tok-1",
		"project:proj-d@tok-1",
	}
	if !slices.Equal(chk.calls, wantCalls) {
		t.Errorf("probe calls = %v, want %v", chk.calls, wantCalls)
	}

	// 2. Denied project + denied fleet surface as Status drift, and expect: can mark one expected.
	p2 := plan(t)
	p2.Env.Agent.Clusters.Scope = []env.ClusterScope{
		{Project: "proj-denied"},
		{Fleet: "fleet-denied"},
	}
	p2.Env.Expect = append(p2.Env.Expect, env.Expected{Object: "scope fleet/fleet-denied", Reason: "grant pending"})
	p2.ScopeAccess = &fakeScopeChecker{
		projects: map[string]error{"proj-denied": errors.New("agent Google identity cannot list GKE clusters in project proj-denied")},
		fleets:   map[string]error{"fleet-denied": errors.New("agent Google identity cannot list fleet memberships in fleet-denied")},
	}
	c2 := inSync(t)
	c2.secrets[CredentialsNamespace+"/"+GoogleTokenSecret] = map[string][]byte{"token": []byte("tok-2")}
	c2.workspace = mustWorkspace(t, p2.Env)
	r, err := Status(ctx, p2, c2)
	if err != nil {
		t.Fatalf("Status with denied scope: %v", err)
	}
	byWhat := map[string]Change{}
	for _, ch := range r.Changes {
		byWhat[ch.What] = ch
	}
	if ch, ok := byWhat["scope project/proj-denied"]; !ok || ch.Expected != "" || !strings.Contains(ch.Detail, "proj-denied") {
		t.Errorf("denied project change = %+v", byWhat)
	}
	if ch, ok := byWhat["scope fleet/fleet-denied"]; !ok || ch.Expected != "grant pending" {
		t.Errorf("expected fleet change = %+v", byWhat)
	}
	if len(r.Drift()) != 1 || r.Drift()[0].What != "scope project/proj-denied" {
		t.Errorf("Drift() = %+v", r.Drift())
	}

	// 3. Missing google-token Secret: silent when token-refresher is not in the render;
	// reported as unverified when token-refresher Deployment is rendered.
	p3 := plan(t)
	p3.ScopeAccess = &fakeScopeChecker{}
	c3 := inSync(t)
	r3, err := Status(ctx, p3, c3)
	if err != nil {
		t.Fatalf("Status without refresher: %v", err)
	}
	if len(r3.Changes) != 0 {
		t.Errorf("changes without refresher = %+v, want none", r3.Changes)
	}
	refDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(refDir, "kustomization.yaml"), []byte("resources: [refresher.yaml]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	refresherYAML := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: token-refresher\n  namespace: ate-credentials\n"
	if err := os.WriteFile(filepath.Join(refDir, "refresher.yaml"), []byte(refresherYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	refObjs, err := render.Build(refDir)
	if err != nil {
		t.Fatal(err)
	}
	p3.Objects = append(p3.Objects, refObjs...)
	var missingFindings []string
	if err := scopeAccessStatus(ctx, p3, c3, func(what, detail string) { missingFindings = append(missingFindings, what+": "+detail) }); err != nil {
		t.Fatalf("scopeAccessStatus with refresher: %v", err)
	}
	if len(missingFindings) != 1 || !strings.Contains(missingFindings[0], "is missing") {
		t.Errorf("missingFindings = %v", missingFindings)
	}
	// Present secret with empty token key says "empty token key", not "is missing".
	c3.secrets[CredentialsNamespace+"/"+GoogleTokenSecret] = map[string][]byte{"token": []byte("  \n")}
	var emptyTokFindings []string
	if err := scopeAccessStatus(ctx, p3, c3, func(what, detail string) { emptyTokFindings = append(emptyTokFindings, what+": "+detail) }); err != nil {
		t.Fatalf("scopeAccessStatus empty token: %v", err)
	}
	if len(emptyTokFindings) != 1 || !strings.Contains(emptyTokFindings[0], "has an empty token key") {
		t.Errorf("emptyTokFindings = %v", emptyTokFindings)
	}

	// 3b. HTTP 401 fails fast with a single "scope access" finding without probing remaining targets.
	p401 := plan(t)
	p401.Env.Agent.Clusters.Scope = []env.ClusterScope{{Project: "proj-one"}, {Project: "proj-two"}, {Fleet: "fleet-host"}}
	chk401 := &fakeScopeChecker{
		projects: map[string]error{"proj-one": errScopeTokenRejected},
	}
	p401.ScopeAccess = chk401
	c401 := inSync(t)
	c401.secrets[CredentialsNamespace+"/"+GoogleTokenSecret] = map[string][]byte{"token": []byte("expired-tok")}
	var f401 []string
	if err := scopeAccessStatus(ctx, p401, c401, func(what, detail string) { f401 = append(f401, what+": "+detail) }); err != nil {
		t.Fatalf("scopeAccessStatus 401: %v", err)
	}
	if len(f401) != 1 || !strings.HasPrefix(f401[0], "scope access: ") || len(chk401.calls) != 1 {
		t.Errorf("401 fail-fast findings=%v calls=%v, want 1 scope access finding and 1 call", f401, chk401.calls)
	}

	// 4. Secret read error propagates from Status.
	c4 := inSync(t)
	c4.failOn = "secret"
	if err := scopeAccessStatus(ctx, p3, c4, func(string, string) {}); err == nil || !strings.Contains(err.Error(), "secret ate-credentials/google-token") {
		t.Errorf("secret error = %v", err)
	}

	// 5. Empty scope is a no-op even with ScopeAccess configured.
	p5 := plan(t)
	p5.Env.Agent.Clusters.Scope = nil
	p5.ScopeAccess = &fakeScopeChecker{}
	if err := scopeAccessStatus(ctx, p5, c4, func(string, string) {}); err != nil {
		t.Errorf("empty scope returned err: %v", err)
	}
}

func mustWorkspace(t *testing.T, e *env.Env) []byte {
	t.Helper()
	ws, err := agent.Workspace(root, e)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}
