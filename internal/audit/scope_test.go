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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigAndSingleStaticCluster(t *testing.T) {
	dir := t.TempDir()
	emptyPath := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(emptyPath, []byte(`{"atespace":"agents","project":"p1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(emptyPath); err == nil || !strings.Contains(err.Error(), "both empty") {
		t.Fatalf("LoadConfig(empty) = %v", err)
	}
	scopePath := filepath.Join(dir, "scope.json")
	if err := os.WriteFile(scopePath, []byte(`{"atespace":"agents","project":"p1","scope":[{"project":"p1"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(scopePath)
	if err != nil || len(cfg.Scope) != 1 || cfg.Scope[0].Project != "p1" {
		t.Fatalf("LoadConfig(scope) = %+v, %v", cfg, err)
	}
	if cfg.SingleStaticCluster() {
		t.Error("project scope should not be SingleStaticCluster")
	}
	if !(&Config{Clusters: []ClusterTarget{{Name: "c1", Path: "projects/p1/locations/us-central1/clusters/c1"}}}).SingleStaticCluster() {
		t.Error("single static cluster should be SingleStaticCluster")
	}
	if (&Config{Clusters: []ClusterTarget{{Name: "c1"}, {Name: "c2"}}}).SingleStaticCluster() {
		t.Error("two static clusters should not be SingleStaticCluster")
	}
	if !(&Config{Scope: []ScopeTarget{{Path: "projects/p1/locations/us-central1/clusters/c1"}}}).SingleStaticCluster() {
		t.Error("single path scope should be SingleStaticCluster")
	}
	if (&Config{Scope: []ScopeTarget{{Fleet: "host-proj"}}}).SingleStaticCluster() {
		t.Error("fleet scope should not be SingleStaticCluster")
	}
}

func TestRunLiveScopeProjectFleetAndPath(t *testing.T) {
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer live-tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/projects/proj-a/locations/-/clusters":
			_, _ = w.Write([]byte(`{"clusters":[
				{"name":"c-alpha","location":"us-central1"},
				{"name":"c-shared","location":"us-east1"}
			]}`))
		case "/v1/projects/proj-b/locations/europe-west1-c/clusters":
			_, _ = w.Write([]byte(`{"clusters":[{"name":"c-eu","location":""}]}`))
		case "/v1/projects/fleet-host/locations/-/memberships":
			if r.URL.Query().Get("pageToken") == "" {
				_, _ = w.Write([]byte(`{"resources":[
					{"endpoint":{"gkeCluster":{"resourceLink":"//container.googleapis.com/projects/proj-a/locations/us-east1/clusters/c-shared"}}},
					{"endpoint":{"gkeCluster":{"resourceLink":"//container.googleapis.com/projects/proj-c/zones/asia-east1-a/clusters/c-fleet"}}},
					{"endpoint":{"gkeCluster":{"resourceLink":"//container.googleapis.com/projects/proj-c/locations/asia-east1/clusters/gone","clusterMissing":true}}},
					{"endpoint":{}}
				],"nextPageToken":"page-2"}`))
				return
			}
			_, _ = w.Write([]byte(`{"resources":[
				{"endpoint":{"gkeCluster":{"resourceLink":"//container.googleapis.com/projects/proj-d/locations/us-west1/clusters/c-page2"}}},
				{"endpoint":{"gkeCluster":{"resourceLink":"//not-container/projects/x/locations/y/clusters/z"}}}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gcp.Close()

	cfg := &Config{
		Atespace: "agents",
		Project:  "proj-a",
		Scope: []ScopeTarget{
			{Project: "proj-a"},
			{Project: "proj-b", Location: "europe-west1-c"},
			{Fleet: "fleet-host"},
			{Path: "projects/proj-e/locations/us-east4/clusters/c-explicit"},
			// Duplicate explicit path overlaps with proj-a; must be deduplicated.
			{Path: "projects/proj-a/locations/us-central1/clusters/c-alpha"},
		},
	}

	var (
		lookoutContexts []string
		leewayClusters  string
	)
	execFn := func(_ context.Context, bin string, args []string, _ []string) ([]byte, error) {
		if bin == "lookout" {
			for _, a := range args {
				if ctxName, ok := strings.CutPrefix(a, "--context="); ok {
					lookoutContexts = append(lookoutContexts, ctxName)
				}
			}
			return []byte(`{"scanned":5,"findings":0,"elapsed":"1ms"}` + "\n"), nil
		}
		for _, a := range args {
			if cls, ok := strings.CutPrefix(a, "--clusters="); ok {
				leewayClusters = cls
			}
		}
		var clusterJSON []string
		for c := range strings.SplitSeq(leewayClusters, ",") {
			clusterJSON = append(clusterJSON, fmt.Sprintf(`{"cluster":%q,"status":"ok"}`, c))
		}
		return []byte(fmt.Sprintf(`{"schema_version":"leeway.report/v1alpha1","generated_at":"2026-11-28T05:17:00Z","clusters":[%s],"summary":{"clusters_total":%d,"clusters_succeeded":%d,"clusters_failed":0}}`,
			strings.Join(clusterJSON, ","), len(clusterJSON), len(clusterJSON))), nil
	}

	ledgerPath := filepath.Join(t.TempDir(), "ledger.json")
	summary, err := Run(context.Background(), cfg, RunOptions{
		LedgerPath:  ledgerPath,
		Exec:        execFn,
		GoogleToken: func(context.Context) (string, error) { return "live-tok", nil },
		GCPBaseURL:  gcp.URL,
		Now:         func() time.Time { return time.Date(2026, 11, 28, 5, 17, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("Run live scope: %v", err)
	}
	if summary.ClustersExpected != 6 || summary.ClustersSucceeded != 6 {
		t.Fatalf("summary = %+v, want 6/6", summary)
	}
	wantLeeway := strings.Join([]string{
		"proj-a/us-central1/c-alpha",
		"proj-a/us-east1/c-shared",
		"proj-b/europe-west1-c/c-eu",
		"proj-c/asia-east1-a/c-fleet",
		"proj-d/us-west1/c-page2",
		"proj-e/us-east4/c-explicit",
	}, ",")
	if leewayClusters != wantLeeway {
		t.Errorf("leeway --clusters=%q\nwant=%q", leewayClusters, wantLeeway)
	}
	wantLookout := []string{
		"gke_proj-a_us-central1_c-alpha",
		"gke_proj-a_us-east1_c-shared",
		"gke_proj-b_europe-west1-c_c-eu",
		"gke_proj-c_asia-east1-a_c-fleet",
		"gke_proj-d_us-west1_c-page2",
		"gke_proj-e_us-east4_c-explicit",
	}
	if !slices.Equal(lookoutContexts, wantLookout) {
		t.Errorf("lookout contexts = %q\nwant = %q", lookoutContexts, wantLookout)
	}
}

func TestRunLiveScopePartialAndTotalFailures(t *testing.T) {
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/projects/proj-ok/locations/-/clusters":
			_, _ = w.Write([]byte(`{"clusters":[{"name":"c1","location":"us-central1"}]}`))
		case "/v1/projects/proj-empty/locations/-/clusters":
			_, _ = w.Write([]byte(`{}`))
		case "/v1/projects/proj-denied/locations/-/clusters":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"denied on proj-denied\nmore detail"}}`))
		case "/v1/projects/fleet-denied/locations/-/memberships":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":403,"status":"PERMISSION_DENIED"}}`))
		case "/v1/projects/fleet-loop/locations/-/memberships":
			_, _ = w.Write([]byte(`{"resources":[],"nextPageToken":"forever"}`))
		case "/v1/projects/proj-badjson/locations/-/clusters":
			_, _ = w.Write([]byte(`{bad`))
		case "/v1/projects/fleet-badjson/locations/-/memberships":
			_, _ = w.Write([]byte(`{bad`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gcp.Close()

	// Seed a ledger entry for a cluster in proj-denied and prove a failed scope
	// never stale-closes it while proj-ok still succeeds.
	ledgerPath := filepath.Join(t.TempDir(), "ledger.json")
	seed := &Ledger{
		SchemaVersion: LedgerSchemaVersion,
		Entries: map[string]*LedgerEntry{
			"proj-denied/us-central1/c9/default/Deployment/web/NoPDB": {
				SubjectKey: "proj-denied/us-central1/c9/default/Deployment/web/NoPDB",
				Cluster:    "proj-denied/us-central1/c9",
				Engine:     EngineLookout,
				State:      StateActive,
			},
		},
	}
	if err := SaveLedger(ledgerPath, seed); err != nil {
		t.Fatal(err)
	}

	execOK := func(_ context.Context, bin string, _ []string, _ []string) ([]byte, error) {
		if bin == "lookout" {
			return []byte(`{"scanned":2,"findings":0,"elapsed":"1ms"}` + "\n"), nil
		}
		return []byte(`{"schema_version":"leeway.report/v1alpha1","generated_at":"2026-11-28T05:17:00Z","clusters":[{"cluster":"proj-ok/us-central1/c1","status":"ok"}],"summary":{"clusters_total":1,"clusters_succeeded":1,"clusters_failed":0}}`), nil
	}

	cfg := &Config{
		Atespace: "agents",
		Project:  "proj-ok",
		Scope: []ScopeTarget{
			{Project: "proj-ok"},
			{Project: "proj-denied"},
			{Fleet: "fleet-denied"},
			{Path: "bad/path"},
			{},
		},
	}
	summary, err := Run(context.Background(), cfg, RunOptions{
		LedgerPath:  ledgerPath,
		Exec:        execOK,
		GoogleToken: func(context.Context) (string, error) { return "tok", nil },
		GCPBaseURL:  gcp.URL,
	})
	if err == nil || summary.ClustersExpected != 1 || summary.ClustersSucceeded != 1 {
		t.Fatalf("partial scope failure: summary=%+v err=%v", summary, err)
	}
	if !strings.Contains(summary.Errors["scope:project:proj-denied"], "PERMISSION_DENIED") ||
		!strings.Contains(summary.Errors["scope:fleet:fleet-denied"], "PERMISSION_DENIED") ||
		summary.Errors["scope:path:bad/path"] == "" ||
		summary.Errors["scope:empty"] == "" {
		t.Errorf("summary.Errors = %+v", summary.Errors)
	}
	reloaded, err := LoadLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Entries["proj-denied/us-central1/c9/default/Deployment/web/NoPDB"].State != StateActive {
		t.Error("failed scope stale-closed ledger entry for unreachable project")
	}

	// Empty project (0 clusters, 0 errors): succeeds without running lookout/leeway.
	calledExec := false
	emptySum, err := Run(context.Background(), &Config{
		Atespace: "agents",
		Project:  "proj-empty",
		Scope:    []ScopeTarget{{Project: "proj-empty"}},
	}, RunOptions{
		LedgerPath:  filepath.Join(t.TempDir(), "empty-ledger.json"),
		Exec:        func(context.Context, string, []string, []string) ([]byte, error) { calledExec = true; return nil, nil },
		GoogleToken: func(context.Context) (string, error) { return "tok", nil },
		GCPBaseURL:  gcp.URL,
	})
	if err != nil || emptySum.ClustersExpected != 0 || calledExec {
		t.Fatalf("empty project: summary=%+v err=%v calledExec=%v", emptySum, err, calledExec)
	}

	// Missing GoogleToken for live project/fleet scope fails cleanly without calling exec.
	noTokSum, err := Run(context.Background(), &Config{
		Scope: []ScopeTarget{{Project: "proj-ok"}, {Fleet: "fleet-host"}},
	}, RunOptions{
		LedgerPath: filepath.Join(t.TempDir(), "notok-ledger.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "google token provider is required") || noTokSum.ClustersExpected != 0 {
		t.Fatalf("missing token: summary=%+v err=%v", noTokSum, err)
	}

	// Token provider returning error or empty token, malformed JSON, and infinite pagination.
	for _, tc := range []struct {
		name  string
		scope []ScopeTarget
		tok   TokenProvider
		want  string
	}{
		{"token error", []ScopeTarget{{Project: "proj-ok"}}, func(context.Context) (string, error) { return "", errors.New("boom") }, "boom"},
		{"empty token", []ScopeTarget{{Project: "proj-ok"}}, func(context.Context) (string, error) { return "   ", nil }, "empty token"},
		{"project bad json", []ScopeTarget{{Project: "proj-badjson"}}, func(context.Context) (string, error) { return "tok", nil }, "parse container"},
		{"fleet bad json", []ScopeTarget{{Fleet: "fleet-badjson"}}, func(context.Context) (string, error) { return "tok", nil }, "parse gkehub"},
		{"fleet page cap", []ScopeTarget{{Fleet: "fleet-loop"}}, func(context.Context) (string, error) { return "tok", nil }, "exceeded 100 pages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Run(context.Background(), &Config{Scope: tc.scope}, RunOptions{
				LedgerPath:  filepath.Join(t.TempDir(), "l.json"),
				GoogleToken: tc.tok,
				GCPBaseURL:  gcp.URL,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}

	// googleErrorSuffix edge cases.
	if got := googleErrorSuffix([]byte(`{"error":{"message":"` + strings.Repeat("a", 250) + `"}}`)); !strings.Contains(got, "...") {
		t.Errorf("long message suffix = %q", got)
	}
	if got := googleErrorSuffix([]byte(`{"error":{}}`)); got != "" {
		t.Errorf("empty error suffix = %q", got)
	}
	if got := googleErrorSuffix([]byte(`plain text`)); got != "" {
		t.Errorf("plain suffix = %q", got)
	}
}

func TestLiveScopePartialDiscoveryNumericFleetAndZeroTargets(t *testing.T) {
	var clusterGets int
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/projects/proj-partial/locations/-/clusters":
			_, _ = w.Write([]byte(`{"clusters":[{"name":"c1","location":"us-central1"}],"missingZones":["us-east1-b","us-east1-b"]}`))
		case "/v1/projects/proj-ok/locations/-/clusters":
			_, _ = w.Write([]byte(`{"clusters":[{"name":"c1","location":"us-central1"}]}`))
		case "/v1/projects/fleet-numeric/locations/-/memberships":
			_, _ = w.Write([]byte(`{
				"resources":[
					{"endpoint":{"gkeCluster":{"resourceLink":"//container.googleapis.com/projects/1067056737933/locations/us-central1/clusters/c1"}}},
					{"endpoint":{"gkeCluster":{"resourceLink":"//container.googleapis.com/projects/1067056737933/zones/us-central1-a/clusters/c2"}}},
					{"endpoint":{"gkeCluster":{"resourceLink":"//container.googleapis.com/projects/888888888/locations/us-west1/clusters/wi"}}},
					{"endpoint":{"gkeCluster":{"resourceLink":"//container.googleapis.com/projects/777777777/locations/us-south1/clusters/plain"}}},
					{"endpoint":{"gkeCluster":{"resourceLink":"//container.googleapis.com/projects/999999999/locations/us-east1/clusters/denied"}}}
				],
				"unreachable":["locations/europe-west1"]
			}`))
		case "/v1/projects/1067056737933/locations/us-central1/clusters/c1":
			clusterGets++
			_, _ = w.Write([]byte(`{"selfLink":"https://container.googleapis.com/v1/projects/proj-ok/locations/us-central1/clusters/c1"}`))
		case "/v1/projects/888888888/locations/us-west1/clusters/wi":
			// Fleet Workload Identity carries the fleet host project in workloadPool;
			// must NOT rewrite member project 888888888 to fleet-numeric.
			_, _ = w.Write([]byte(`{"selfLink":"https://container.googleapis.com/v1/projects/888888888/locations/us-west1/clusters/wi","workloadIdentityConfig":{"workloadPool":"fleet-numeric.svc.id.goog"}}`))
		case "/v1/projects/777777777/locations/us-south1/clusters/plain":
			_, _ = w.Write([]byte(`{"selfLink":"https://container.googleapis.com/v1/projects/777777777/locations/us-south1/clusters/plain"}`))
		case "/v1/projects/999999999/locations/us-east1/clusters/denied":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"status":"PERMISSION_DENIED","message":"denied"}}`))
		case "/v1/projects/proj-empty/locations/-/clusters":
			_, _ = w.Write([]byte(`{"clusters":[]}`))
		case "/v1/projects/proj-redirect/locations/-/clusters":
			http.Redirect(w, r, "http://127.0.0.1:1/steal", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer gcp.Close()

	// 1. missingZones + numeric fleet project resolution + caching + dedup + unreachable.
	cfg := &Config{
		Scope: []ScopeTarget{
			{Project: "proj-partial"},
			{Project: "proj-ok"},
			{Fleet: "fleet-numeric"},
		},
	}
	targets, errs := resolveAuditClusters(context.Background(), cfg, RunOptions{
		GoogleToken: func(context.Context) (string, error) { return "tok", nil },
		GCPBaseURL:  gcp.URL,
	}, func(string, ...any) {})
	wantPaths := []string{
		"projects/777777777/locations/us-south1/clusters/plain",
		"projects/888888888/locations/us-west1/clusters/wi",
		"projects/proj-ok/locations/us-central1-a/clusters/c2",
		"projects/proj-ok/locations/us-central1/clusters/c1",
		"projects/proj-partial/locations/us-central1/clusters/c1",
	}
	var gotPaths []string
	for _, tgt := range targets {
		gotPaths = append(gotPaths, tgt.Path)
	}
	if !slices.Equal(gotPaths, wantPaths) {
		t.Errorf("resolved paths = %v, want %v", gotPaths, wantPaths)
	}
	if clusterGets != 1 {
		t.Errorf("clusters.get calls for cached project number = %d, want 1", clusterGets)
	}
	for _, wantKey := range []string{
		"scope:project:proj-partial:missing_zones",
		"scope:fleet:fleet-numeric:unreachable",
		"scope:fleet:fleet-numeric:member:999999999/us-east1/denied",
	} {
		if errs[wantKey] == "" {
			t.Errorf("missing expected partial/member error %q in %+v", wantKey, errs)
		}
	}
	if len(errs) != 3 {
		t.Errorf("unexpected extra errs: %+v", errs)
	}

	// 2. Zero targets counts only StateActive entries (not StateRetired) and updates UpdatedAt.
	ledgerPath := filepath.Join(t.TempDir(), "zero-ledger.json")
	now := time.Date(2026, 11, 28, 16, 0, 0, 0, time.UTC)
	retAt := now.Add(-24 * time.Hour)
	seed := &Ledger{
		SchemaVersion: LedgerSchemaVersion,
		Entries: map[string]*LedgerEntry{
			"active-1":  {SubjectKey: "active-1", Cluster: "proj-ok/us-central1/c1", Engine: EngineLookout, State: StateActive},
			"retired-1": {SubjectKey: "retired-1", Cluster: "proj-ok/us-central1/c1", Engine: EngineLookout, State: StateRetired, RetiredAt: &retAt},
		},
	}
	if err := SaveLedger(ledgerPath, seed); err != nil {
		t.Fatal(err)
	}
	sum, err := Run(context.Background(), &Config{Scope: []ScopeTarget{{Project: "proj-empty"}}}, RunOptions{
		LedgerPath:  ledgerPath,
		Now:         func() time.Time { return now },
		GoogleToken: func(context.Context) (string, error) { return "tok", nil },
		GCPBaseURL:  gcp.URL,
	})
	if err != nil {
		t.Fatalf("zero target run: %v", err)
	}
	if sum.ActiveFindings != 1 {
		t.Errorf("ActiveFindings = %d, want 1 (must ignore retired entries)", sum.ActiveFindings)
	}
	reloaded, err := LoadLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.UpdatedAt.Equal(now) {
		t.Errorf("ledger.UpdatedAt = %v, want %v", reloaded.UpdatedAt, now)
	}

	// 3. Caller-supplied HTTPClient with nil CheckRedirect still refuses redirects.
	_, redirErrs := resolveAuditClusters(context.Background(), &Config{Scope: []ScopeTarget{{Project: "proj-redirect"}}}, RunOptions{
		HTTPClient:  gcp.Client(),
		GoogleToken: func(context.Context) (string, error) { return "tok", nil },
		GCPBaseURL:  gcp.URL,
	}, func(string, ...any) {})
	if !strings.Contains(redirErrs["scope:project:proj-redirect"], "HTTP 302") {
		t.Errorf("redirect errs = %+v, want HTTP 302 refusal", redirErrs)
	}

	// 4. Helper edge cases.
	if _, ok := parseContainerSelfLinkProject("https://example.com/no-projects"); ok {
		t.Error("accepted selfLink without /projects/")
	}
	if _, ok := parseContainerSelfLinkProject("https://container.googleapis.com/v1/projects/p/bad"); ok {
		t.Error("accepted short selfLink")
	}
	if isAllDigits("") {
		t.Error("empty string is not all digits")
	}
	customCalled := false
	customClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		customCalled = true
		return http.ErrUseLastResponse
	}}
	_, _ = resolveAuditClusters(context.Background(), &Config{Scope: []ScopeTarget{{Project: "proj-redirect"}}}, RunOptions{
		HTTPClient:  customClient,
		GoogleToken: func(context.Context) (string, error) { return "tok", nil },
		GCPBaseURL:  gcp.URL,
	}, func(string, ...any) {})
	if !customCalled {
		t.Error("explicit custom CheckRedirect was overwritten")
	}
}
