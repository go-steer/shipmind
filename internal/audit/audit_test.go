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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/shipmind/internal/scenario"
)

func TestIdentityAndNormalization(t *testing.T) {
	for in, want := range map[string]string{
		"payment-backend-7d9f8b6c4-x9k2l": "payment-backend",
		"payment-backend-7d9f8b6c4":       "payment-backend",
		"payment-backend":                 "payment-backend",
		"db-0":                            "db-0",
		"backup-28472940":                 "backup-28472940",
		"x9k2l":                           "x9k2l",
		"":                                "",
	} {
		if got := NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}

	if got := CanonicalClusterID("projects/p1/locations/us-central1/clusters/c1"); got != "p1/us-central1/c1" {
		t.Errorf("CanonicalClusterID(path) = %q", got)
	}
	if got := CanonicalClusterID("gke:p1/us-central1/c1"); got != "p1/us-central1/c1" {
		t.Errorf("CanonicalClusterID(gke:) = %q", got)
	}
	if got := CanonicalClusterID("kind-local"); got != "kind-local" {
		t.Errorf("CanonicalClusterID(local) = %q", got)
	}

	if got := LookoutSubjectKey("p1/us-central1/c1", "shop", "Pod", "api-7d9f8b6c4-x9k2l", "ErrImagePull", "audit.images"); got != "p1/us-central1/c1/shop/Pod/api/ImagePullBackOff" {
		t.Errorf("LookoutSubjectKey = %q", got)
	}
	if got := LookoutSubjectKey("p1/us-central1/c1", "", "Cluster", "", "", "audit.workload_identity_off"); got != "p1/us-central1/c1//Cluster//audit.workload_identity_off" {
		t.Errorf("LookoutSubjectKey fallback = %q", got)
	}

	// Leeway topology keys (stability-policy.md §2.2).
	if got := LeewayTopologyKey("p1/us-central1/c1", LeewayFinding{
		Kind:        "leeway.contract_violated",
		Subject:     LeewaySubject{Kind: "Deployment", Namespace: "shop", Name: "batch-worker"},
		TopologyKey: "topology.kubernetes.io/zone",
	}); got != "p1/us-central1/c1/shop/Deployment/batch-worker/topology.kubernetes.io/zone/leeway.contract_violated" {
		t.Errorf("LeewayTopologyKey = %q", got)
	}
	if got := LeewayTopologyKey("p1/us-east1/c1", LeewayFinding{
		Kind:        "leeway.domain_unavailable",
		Subject:     LeewaySubject{Kind: "Domain", Name: "us-east1-c"},
		TopologyKey: "topology.kubernetes.io/zone",
	}); got != "p1/us-east1/c1/_cluster/Domain/us-east1-c/topology.kubernetes.io/zone/leeway.domain_unavailable" {
		t.Errorf("LeewayTopologyKey domain = %q", got)
	}
	if got := LeewayTopologyKey("c1", LeewayFinding{}); got != "c1/_cluster/Cluster/unknown/unknown/unknown" {
		t.Errorf("LeewayTopologyKey empty = %q", got)
	}

	// Leeway rank keys (stability-policy.md §2.3, including focus == 0).
	zero, neg := 0, -1
	if got := LeewayRankKey("p1/us-central1/c1", LeewayRankFinding{
		Kind:       "leeway.rank_degraded",
		Subject:    LeewaySubject{Kind: "PreferenceAxis", Name: "gold"},
		ObjectKind: "ComputeClass",
		Rule:       "last-rank",
	}); got != "p1/us-central1/c1/_cluster/ComputeClass/gold/rule:last-rank/leeway.rank_degraded" {
		t.Errorf("LeewayRankKey axis-wide = %q", got)
	}
	if got := LeewayRankKey("p1/us-central1/c1", LeewayRankFinding{
		Kind:    "leeway.rank_tier_unused",
		Subject: LeewaySubject{Name: "gold"},
		Rule:    "tier-unused",
		Focus:   &zero,
	}); got != "p1/us-central1/c1/_cluster/ComputeClass/gold/rule:tier-unused/0/leeway.rank_tier_unused" {
		t.Errorf("LeewayRankKey focus=0 = %q", got)
	}
	if got := LeewayRankKey("p1/us-central1/c1", LeewayRankFinding{
		Kind:    "leeway.rank_wedged",
		Subject: LeewaySubject{Name: "gold"},
		Rule:    "wedged",
		Focus:   &neg,
	}); got != "p1/us-central1/c1/_cluster/ComputeClass/gold/rule:wedged/leeway.rank_wedged" {
		t.Errorf("LeewayRankKey focus=-1 = %q", got)
	}
	if got := LeewayRankKey("c1", LeewayRankFinding{}); got != "c1/_cluster/ComputeClass/unknown/compute-class/unknown" {
		t.Errorf("LeewayRankKey empty = %q", got)
	}

	// #845 version check.
	if !NeedsExplicitLookoutSubcommands("v0.44.0") || !NeedsExplicitLookoutSubcommands("v0.44.1-gke") {
		t.Error("v0.44.x must run explicit computeclass + ai subcommands")
	}
	if NeedsExplicitLookoutSubcommands("v0.45.0") || NeedsExplicitLookoutSubcommands("v1.0.0") || NeedsExplicitLookoutSubcommands("bc4909baa94081fa51b345277ed628c260652e7c") {
		t.Error("v0.45.0+ or commit pin must not duplicate computeclass + ai")
	}
}

func TestParseLookoutStreamContract(t *testing.T) {
	stream := strings.Join([]string{
		`{"kind":"audit.no_pdb","severity":"warning","namespace":"shop","kind_of_object":"Deployment","name":"checkout","reason":"NoPodDisruptionBudget","message":"Deployment has 3 replicas and no PDB","fingerprint":"sha256:1111111111111111111111111111111111111111111111111111111111111111","replicas":"3"}`,
		`{"kind":"audit.privileged","severity":"critical","namespace":"legacy","kind_of_object":"DaemonSet","name":"agent","reason":"PrivilegedContainer","message":"privileged=true","exempt_reason":"host agent","exempt_expires":"2027-01-01T00:00:00Z"}`,
		`{"kind":"scan.check_skipped","severity":"info","reason":"Skipped","message":"skipped"}`,
		`{"scanned":42,"findings":3,"elapsed":"120ms","exempt":1}`,
	}, "\n") + "\n"

	obs, sum, err := ParseLookoutStream("projects/p1/locations/us-central1/clusters/c1", strings.NewReader(stream))
	if err != nil {
		t.Fatalf("ParseLookoutStream: %v", err)
	}
	if sum.Scanned != 42 || sum.Findings != 3 || !sum.HasEx || sum.Exempt != 1 || len(obs) != 2 {
		t.Fatalf("sum=%+v len(obs)=%d", sum, len(obs))
	}
	if obs[0].Details["replicas"] != "3" || obs[0].Fingerprint == "" {
		t.Errorf("obs[0] = %+v", obs[0])
	}
	if obs[1].Fingerprint != "" || obs[1].ExemptReason != "host agent" {
		t.Errorf("obs[1] = %+v", obs[1])
	}

	// Refusal cases: missing summary, finding after summary, count mismatch, stage failure, clusters_failed.
	for name, bad := range map[string]string{
		"truncated":      `{"kind":"audit.no_pdb","severity":"warning","reason":"NoPDB"}`,
		"after summary":  "{\"scanned\":1,\"findings\":1,\"elapsed\":\"1ms\"}\n{\"kind\":\"audit.no_pdb\"}\n",
		"count mismatch": "{\"kind\":\"audit.no_pdb\"}\n{\"scanned\":1,\"findings\":2,\"elapsed\":\"1ms\"}\n",
		"check_failed":   "{\"kind\":\"scan.check_failed\",\"message\":\"forbidden\"}\n{\"scanned\":1,\"findings\":1,\"elapsed\":\"1ms\"}\n",
		"clusters_fail":  "{\"scanned\":1,\"findings\":0,\"elapsed\":\"1ms\",\"clusters_failed\":1}\n",
		"not summary":    "{\"foo\":\"bar\"}\n",
		"bad json":       "{not-json}\n",
	} {
		if _, _, err := ParseLookoutStream("p1/us-central1/c1", strings.NewReader(bad)); err == nil {
			t.Errorf("%s: ParseLookoutStream succeeded on invalid input", name)
		}
	}
}

func TestParseLeewayReportContract(t *testing.T) {
	raw := `{
  "schema_version": "leeway.report/v1alpha1",
  "generated_at": "2026-11-24T05:17:00Z",
  "clusters": [
    {
      "cluster": "c1",
      "source": "gke:p1/us-central1/c1",
      "status": "ok",
      "topology_findings": [
        {
          "kind": "leeway.placement_drift",
          "subject": {"kind": "Deployment", "namespace": "shop", "name": "frontend"},
          "topologyKey": "topology.kubernetes.io/zone",
          "tier": "B",
          "severity": "warning",
          "suspectedCause": "missing_spread_constraint",
          "untestedCauses": ["rollout_bias"],
          "remediation": {
            "kind": "manifest_patch",
            "summary": "Add zone topologySpreadConstraints",
            "yaml": "spec:\n  template:\n    spec:\n      topologySpreadConstraints:\n        - maxSkew: 1"
          }
        }
      ],
      "rank_findings": [
        {
          "kind": "leeway.rank_degraded",
          "subject": {"kind": "PreferenceAxis", "name": "gold"},
          "objectKind": "ComputeClass",
          "tier": "B",
          "severity": "warning",
          "rule": "last-rank",
          "reason": "60% of pods on fallback tier"
        }
      ]
    },
    {
      "cluster": "c2",
      "source": "gke:p1/us-east1/c2",
      "status": "error",
      "error": "dial timeout"
    }
  ],
  "summary": {"clusters_total": 2, "clusters_succeeded": 1, "clusters_failed": 1}
}`
	parsed, err := ParseLeewayReport([]byte(raw), []string{
		"projects/p1/locations/us-central1/clusters/c1",
		"projects/p1/locations/us-east1/clusters/c2",
		"projects/p1/locations/europe-west1/clusters/c3",
	})
	if err != nil {
		t.Fatalf("ParseLeewayReport: %v", err)
	}
	if !parsed.Healthy["p1/us-central1/c1"] || parsed.Healthy["p1/us-east1/c2"] {
		t.Errorf("Healthy = %+v", parsed.Healthy)
	}
	if parsed.ClusterErrors["p1/us-east1/c2"] == "" || parsed.ClusterErrors["p1/europe-west1/c3"] == "" {
		t.Errorf("ClusterErrors = %+v", parsed.ClusterErrors)
	}
	if len(parsed.Observations) != 2 {
		t.Fatalf("len(Observations) = %d, want 2", len(parsed.Observations))
	}

	if _, err := ParseLeewayReport([]byte(`{"schema_version":"leeway.report/v2"}`), nil); err == nil {
		t.Error("accepted unsupported schema_version")
	}
}

func TestLedgerDwellRefreshEscalationExemptionAndRetirement(t *testing.T) {
	dir := t.TempDir()
	ledgerFile := filepath.Join(dir, "sub", "ledger.json")
	l, err := LoadLedger(ledgerFile)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 24, 5, 17, 0, 0, time.UTC)
	healthy := map[ClusterEngine]bool{
		{Cluster: "p1/us-central1/c1", Engine: EngineLookout}: true,
		{Cluster: "p1/us-central1/c1", Engine: EngineLeeway}:  true,
	}
	cfg := EvalConfig{RefreshWindow: 168 * time.Hour, MinConsecutive: 2}

	skew := Observation{
		SubjectKey:  "p1/us-central1/c1/shop/Deployment/web/topology.kubernetes.io/zone/leeway.placement_drift",
		Cluster:     "p1/us-central1/c1",
		Engine:      EngineLeeway,
		Kind:        "leeway.placement_drift",
		Severity:    "warning",
		Namespace:   "shop",
		ObjectKind:  "Deployment",
		Name:        "web",
		Message:     "zonal skew",
		Remediation: &Remediation{Kind: "manifest_patch", Summary: "add spread", YAML: "maxSkew: 1"},
	}
	exemptObs := Observation{
		SubjectKey:    "p1/us-central1/c1/legacy/DaemonSet/agent/Privileged",
		Cluster:       "p1/us-central1/c1",
		Engine:        EngineLookout,
		Kind:          "audit.privileged",
		Severity:      "critical",
		Namespace:     "legacy",
		ObjectKind:    "DaemonSet",
		Name:          "agent",
		Reason:        "Privileged",
		ExemptReason:  "reviewed host agent",
		ExemptExpires: "2026-12-01T00:00:00Z",
	}

	// Day 1: minConsecutive=2 holds skew on run 1; active exemption suppresses exemptObs.
	d1 := Evaluate(l, []Observation{skew, exemptObs}, healthy, cfg, t0)
	if len(d1.Due) != 0 || d1.HeldConsecutive != 1 || d1.ExemptActive != 1 {
		t.Fatalf("day 1 delta = %+v", d1)
	}

	// Day 2: skew appears a 2nd consecutive time -> reported as "new"!
	d2 := Evaluate(l, []Observation{skew, exemptObs}, healthy, cfg, t0.Add(24*time.Hour))
	if len(d2.Due) != 1 || d2.Due[0].Reason != DueNew {
		t.Fatalf("day 2 delta = %+v", d2)
	}
	MarkReported(d2.Due, t0.Add(24*time.Hour), map[string]string{skew.SubjectKey: "https://github.com/org/gitops/pull/42"})

	// Day 4: still within 168h refresh window -> muted (0 due).
	d4 := Evaluate(l, []Observation{skew, exemptObs}, healthy, cfg, t0.Add(4*24*time.Hour))
	if len(d4.Due) != 0 || d4.MutedRefresh != 1 {
		t.Fatalf("day 4 delta = %+v", d4)
	}

	// Day 5: severity escalates warning -> critical inside refresh window -> immediately due as "escalated"!
	escalated := skew
	escalated.Severity = "critical"
	d5 := Evaluate(l, []Observation{escalated, exemptObs}, healthy, cfg, t0.Add(5*24*time.Hour))
	if len(d5.Due) != 1 || d5.Due[0].Reason != DueEscalated {
		t.Fatalf("day 5 escalation delta = %+v", d5)
	}
	MarkReported(d5.Due, t0.Add(5*24*time.Hour), nil)

	// Day 13 (>168h since Day 5) + exemption expired after 2026-12-01 -> both due ("refresh" + "new")!
	d13 := Evaluate(l, []Observation{escalated, exemptObs}, healthy, cfg, t0.Add(13*24*time.Hour))
	if len(d13.Due) != 2 {
		t.Fatalf("day 13 delta = %+v, want both refresh and lapsed-exemption due", d13)
	}
	MarkReported(d13.Due, t0.Add(13*24*time.Hour), nil)

	// Save & reload round-trip.
	if err := SaveLedger(ledgerFile, l); err != nil {
		t.Fatalf("SaveLedger: %v", err)
	}
	reloaded, err := LoadLedger(ledgerFile)
	if err != nil || len(reloaded.Entries) != 2 {
		t.Fatalf("LoadLedger: %+v, %v", reloaded, err)
	}

	// Day 14: leeway fails on cluster -> leeway entry is retained, NOT retired; lookout succeeds clean -> lookout entry retires!
	partialHealthy := map[ClusterEngine]bool{
		{Cluster: "p1/us-central1/c1", Engine: EngineLookout}: true,
		{Cluster: "p1/us-central1/c1", Engine: EngineLeeway}:  false,
	}
	d14 := Evaluate(reloaded, nil, partialHealthy, cfg, t0.Add(14*24*time.Hour))
	if len(d14.Retired) != 1 || d14.Retired[0].SubjectKey != exemptObs.SubjectKey {
		t.Fatalf("day 14 retired = %+v", d14.Retired)
	}
	if reloaded.Entries[skew.SubjectKey].State != StateActive {
		t.Fatal("failed leeway pass retired active leeway entry")
	}

	// Day 50 (>30d after retirement): retired entry pruned from ledger.
	Evaluate(reloaded, []Observation{escalated}, healthy, cfg, t0.Add(50*24*time.Hour))
	if _, still := reloaded.Entries[exemptObs.SubjectKey]; still {
		t.Error("retired entry older than retention window was not pruned")
	}
}

func TestCredentialBrokerLoopback(t *testing.T) {
	k8sSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer real-google-token" {
			http.Error(w, "bad k8s auth", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"kind":"PodList","path":"`+r.URL.Path+`"}`)
	}))
	defer k8sSrv.Close()

	gcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer real-google-token" {
			http.Error(w, "bad gcp auth", http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/clusters/c1") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"controlPlaneEndpointsConfig": map[string]any{
					"dnsEndpointConfig": map[string]any{"endpoint": k8sSrv.URL},
				},
			})
			return
		}
		_, _ = io.WriteString(w, `{"gcp_ok":true}`)
	}))
	defer gcpSrv.Close()

	dir := t.TempDir()
	b, err := StartCredentialBroker(BrokerOptions{
		Dir:          dir,
		ClusterPaths: []string{"projects/p1/locations/us-central1/clusters/c1"},
		GoogleToken:  func(context.Context) (string, error) { return "real-google-token", nil },
		GCPBaseURL:   gcpSrv.URL,
	})
	if err != nil {
		t.Fatalf("StartCredentialBroker: %v", err)
	}
	defer func() { _ = b.Close() }()

	tokBytes, err := os.ReadFile(b.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	proxyTok := string(tokBytes)

	// 1. Unauthenticated request rejected with 401.
	resp, err := http.Get(b.URL + "/v1/gcp/container.googleapis.com/v1/projects/p1")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauth status = %d, want 401", resp.StatusCode)
	}

	doAuth := func(path string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, b.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+proxyTok)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		body, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(body)
	}

	// 2. Non-Google host refused with 403.
	if code, _ := doAuth("/v1/gcp/evil.example.com/v1/x"); code != http.StatusForbidden {
		t.Errorf("non-google host status = %d, want 403", code)
	}

	// 3. Allowed Google API forwarded with real Google token.
	if code, body := doAuth("/v1/gcp/container.googleapis.com/v1/projects/p1/locations/-"); code != http.StatusOK || !strings.Contains(body, "gcp_ok") {
		t.Errorf("gcp proxy = %d %s", code, body)
	}

	// 4. Out-of-scope cluster refused with 403.
	if code, _ := doAuth("/v1/k8s/p1/us-central1/other/api/v1/pods"); code != http.StatusForbidden {
		t.Errorf("out-of-scope k8s status = %d, want 403", code)
	}

	// 5. In-scope cluster resolves dnsEndpoint via clusters.get and proxies Kubernetes API.
	if code, body := doAuth("/v1/k8s/p1/us-central1/c1/api/v1/pods"); code != http.StatusOK || !strings.Contains(body, `"PodList"`) {
		t.Errorf("k8s proxy = %d %s", code, body)
	}

	// Also verify IP+CA fallback in parseClusterEndpoint.
	caPEM := generateTestCertPEM(t)
	ep, err := parseClusterEndpoint([]byte(`{"endpoint":"10.0.0.1","masterAuth":{"clusterCaCertificate":"`+base64.StdEncoding.EncodeToString(caPEM)+`"}}`), http.DefaultClient)
	if err != nil || ep.baseURL != "https://10.0.0.1" {
		t.Errorf("parseClusterEndpoint IP+CA = %+v, %v", ep, err)
	}
}

func generateTestCertPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

type fakeAuditActor struct {
	applied  []string
	deleted  []string
	injected string
	failOpen bool
}

func (f *fakeAuditActor) Apply(_ context.Context, m []byte) error {
	f.applied = append(f.applied, string(m))
	return nil
}
func (f *fakeAuditActor) DeleteTask(_ context.Context, name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}
func (f *fakeAuditActor) DeleteWorkspace(context.Context, string) error { return nil }
func (f *fakeAuditActor) Sessions(context.Context, string) ([]scenario.Session, error) {
	return []scenario.Session{{ID: "s-1", Status: "active"}}, nil
}
func (f *fakeAuditActor) CreateSession(context.Context, string) (string, error) {
	if f.failOpen {
		return "", errors.New("router 503")
	}
	return "s-1", nil
}
func (f *fakeAuditActor) Inject(_ context.Context, _, _, msg string) error {
	f.injected = msg
	return nil
}
func (f *fakeAuditActor) State(context.Context, string, string) (string, error) {
	return "idle", nil
}
func (f *fakeAuditActor) Events(context.Context, string, string) (*scenario.Transcript, error) {
	return &scenario.Transcript{
		Server: "core-agent/test",
		Cost:   0.025,
		Events: []scenario.Event{
			{Seq: 1, Author: "user", Texts: []string{f.injected}},
			{Seq: 2, Author: "agent", Role: "model", Texts: []string{"Opened GitOps PR https://github.com/org/gitops/pull/99"}},
		},
	}, nil
}
func (f *fakeAuditActor) Usage(context.Context, string, string) (*scenario.AgentUsage, error) {
	var u scenario.AgentUsage
	_ = json.Unmarshal([]byte(`{"overall":{"input_tokens":1200,"output_tokens":150,"turns":2,"cost_usd":0.025}}`), &u)
	return &u, nil
}
func (f *fakeAuditActor) SubagentEvents(context.Context, string, string, string) (*scenario.Transcript, bool, error) {
	return &scenario.Transcript{}, true, nil
}

func TestRunEndToEndZeroIdleTokensSwitchboardAndAutoPR(t *testing.T) {
	ctx := context.Background()
	var swPosts []string
	var swHeaders []string
	failSwitchboard := false
	swSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failSwitchboard {
			http.Error(w, `{"error":"slack down"}`, http.StatusBadGateway)
			return
		}
		swHeaders = append(swHeaders, r.Header.Get("Authorization")+"|"+r.Header.Get("Idempotency-Key"))
		body, _ := io.ReadAll(r.Body)
		swPosts = append(swPosts, string(body))
		_ = json.NewEncoder(w).Encode(MessageRef{Conversation: "C0123456789:1700000000.0001", ID: "1700000000.0001"})
	}))
	defer swSrv.Close()

	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.json")
	now := time.Date(2026, 11, 24, 5, 17, 0, 0, time.UTC)

	cfg := &Config{
		Atespace:              "agents",
		Project:               "p1",
		Clusters:              []ClusterTarget{{Name: "c1", Path: "projects/p1/locations/us-central1/clusters/c1"}},
		Schedule:              "17 5 * * *",
		RefreshWindow:         "168h",
		MinConsecutive:        1,
		DomainUnavailableKeys: "",
		LookoutVersion:        "v0.44.0", // exercises #845 explicit computeclass + ai fallback too!
		AlertChannel:          "C0123456789",
		SwitchboardURL:        swSrv.URL + "/v1/messages",
		AutoPR:                true,
		Repositories:          []RepoTarget{{Repository: "org/gitops", BaseBranch: "main"}},
	}

	var invoked []string
	cleanSweep := true
	execFake := func(_ context.Context, bin string, args []string, _ []string) ([]byte, error) {
		invoked = append(invoked, bin+" "+strings.Join(args, " "))
		if bin == "lookout" {
			if cleanSweep || args[1] != "all" {
				return []byte(`{"scanned":10,"findings":0,"elapsed":"10ms"}` + "\n"), nil
			}
			return []byte(
				`{"kind":"audit.no_pdb","severity":"warning","namespace":"shop","kind_of_object":"Deployment","name":"orders","reason":"NoPodDisruptionBudget","message":"3 replicas without PDB","fingerprint":"sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"}` + "\n" +
					`{"scanned":10,"findings":1,"elapsed":"15ms"}` + "\n",
			), nil
		}
		if cleanSweep {
			return []byte(`{"schema_version":"leeway.report/v1alpha1","generated_at":"2026-11-24T05:17:00Z","clusters":[{"cluster":"c1","source":"gke:p1/us-central1/c1","status":"ok"}],"summary":{"clusters_total":1,"clusters_succeeded":1,"clusters_failed":0}}`), nil
		}
		return []byte(`{"schema_version":"leeway.report/v1alpha1","generated_at":"2026-11-24T05:17:00Z","clusters":[{"cluster":"c1","source":"gke:p1/us-central1/c1","status":"ok","topology_findings":[{"kind":"leeway.placement_drift","subject":{"kind":"Deployment","namespace":"shop","name":"orders"},"topologyKey":"topology.kubernetes.io/zone","tier":"B","severity":"warning","suspectedCause":"missing_spread_constraint","remediation":{"kind":"manifest_patch","summary":"Add spread constraint","yaml":"topologySpreadConstraints:\n  - maxSkew: 1"}},{"kind":"leeway.placement_drift","subject":{"kind":"Deployment","namespace":"kube-system","name":"konnectivity"},"topologyKey":"topology.kubernetes.io/zone","tier":"B","severity":"info","remediation":{"kind":"platform_managed","summary":"GKE addon"}}]}],"summary":{"clusters_total":1,"clusters_succeeded":1,"clusters_failed":0}}`), nil
	}

	actor := &fakeAuditActor{}
	opts := RunOptions{
		LedgerPath:   ledgerPath,
		Kubeconfig:   "/tmp/fake-kc",
		IngressToken: "ing-secret",
		TaskTemplate: []byte("apiVersion: ax.io/v1alpha1\nkind: Task\nmetadata:\n  atespace: agents\nspec: {}\n"),
		Exec:         execFake,
		ChatTasks:    actor,
		ChatAgent:    actor,
		AutoPRPoll:   time.Millisecond,
		AutoPRSleep:  func(context.Context, time.Duration) error { return nil },
		Now:          func() time.Time { return now },
	}

	// Pass 1: Clean cluster -> 0 LLM tokens, 0 awoken actors, 0 switchboard posts, and v0.44.0 ran all+computeclass+ai!
	s1, err := Run(ctx, cfg, opts)
	if err != nil {
		t.Fatalf("Pass 1 Run: %v", err)
	}
	if s1.LLMTokens != 0 || s1.AwokenTasks != 0 || len(actor.applied) != 0 || len(swPosts) != 0 {
		t.Fatalf("Pass 1 clean summary = %+v, applied=%d, posts=%d", s1, len(actor.applied), len(swPosts))
	}
	if len(invoked) != 4 { // lookout all, computeclass, ai + leeway report
		t.Fatalf("v0.44.0 invoked = %v, want 4 commands", invoked)
	}

	// Upgrade lookout pin to v0.45.0 -> only lookout audit all + leeway report (2 commands).
	cfg.LookoutVersion = "v0.45.0"
	invoked = nil
	cleanSweep = false

	// Pass 2a: Simulate transient switchboard 502 -> findings must NOT be marked reported!
	failSwitchboard = true
	now = now.Add(24 * time.Hour)
	_, err = Run(ctx, cfg, opts)
	if err == nil || !strings.Contains(err.Error(), "switchboard") {
		t.Fatalf("Pass 2a expected switchboard error, got %v", err)
	}
	if len(invoked) != 2 {
		t.Fatalf("v0.45.0 invoked = %v, want 2 commands", invoked)
	}

	// Pass 2b: Switchboard recovers on next run -> findings are still due, digest posts with remediation.yaml & PR link, task cleaned up!
	failSwitchboard = false
	actor.applied, actor.deleted = nil, nil
	now = now.Add(time.Hour)
	s2, err := Run(ctx, cfg, opts)
	if err != nil {
		t.Fatalf("Pass 2b Run: %v", err)
	}
	if s2.DueFindings != 3 || s2.AwokenTasks != 1 || s2.LLMTokens != 1350 || !slices.Equal(s2.PRURLs, []string{"https://github.com/org/gitops/pull/99"}) {
		t.Fatalf("Pass 2b summary = %+v", s2)
	}
	if len(actor.applied) != 1 || len(actor.deleted) != 1 {
		t.Errorf("autoPR lifecycle applied=%v deleted=%v", actor.applied, actor.deleted)
	}
	if len(swPosts) != 1 || !strings.Contains(swPosts[0], "topologySpreadConstraints") || !strings.Contains(swPosts[0], "platform_managed") || !strings.Contains(swPosts[0], "pull/99") {
		t.Errorf("switchboard digest post = %v", swPosts)
	}
	if !strings.HasPrefix(swHeaders[0], "Bearer ing-secret|shipmind-audit-") {
		t.Errorf("switchboard headers = %v", swHeaders)
	}

	// Pass 3: Next day with same active findings -> inside 168h refresh window -> 0 LLM tokens, 0 awoken actors, 0 new Slack posts!
	actor.applied = nil
	swPosts = nil
	now = now.Add(24 * time.Hour)
	s3, err := Run(ctx, cfg, opts)
	if err != nil {
		t.Fatalf("Pass 3 Run: %v", err)
	}
	if s3.DueFindings != 0 || s3.MutedRefresh != 3 || s3.LLMTokens != 0 || s3.AwokenTasks != 0 || len(actor.applied) != 0 || len(swPosts) != 0 {
		t.Fatalf("Pass 3 muted summary = %+v", s3)
	}
}

func TestConfigExecBrokerSweepAndDigestClamping(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// LoadConfig validation + happy path.
	if _, err := LoadConfig(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("LoadConfig missing file succeeded")
	}
	badCfg := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(badCfg, []byte(`{"atespace":"a"}`), 0o600)
	if _, err := LoadConfig(badCfg); err == nil {
		t.Error("LoadConfig empty clusters succeeded")
	}
	goodCfg := filepath.Join(dir, "good.json")
	_ = os.WriteFile(goodCfg, []byte(`{"atespace":"agents","project":"p1","clusters":[{"name":"c1","path":"projects/p1/locations/us-central1/clusters/c1"}]}`), 0o600)
	cfg, err := LoadConfig(goodCfg)
	if err != nil || cfg.Atespace != "agents" {
		t.Fatalf("LoadConfig good = %+v, %v", cfg, err)
	}

	// ExecRunner happy + failure path with /bin/echo and /bin/false.
	out, err := ExecRunner(ctx, "echo", []string{"hello-audit"}, []string{"AUDIT_TEST_VAR=1"})
	if err != nil || !strings.Contains(string(out), "hello-audit") {
		t.Errorf("ExecRunner echo = %q, %v", string(out), err)
	}
	if _, err := ExecRunner(ctx, "false", nil, nil); err == nil {
		t.Error("ExecRunner false succeeded")
	}

	// Run with GoogleToken (exercises prepareSweepAuth + broker env injection).
	var gotEnv []string
	_, err = Run(ctx, cfg, RunOptions{
		LedgerPath:  filepath.Join(dir, "ledger.json"),
		GoogleToken: func(context.Context) (string, error) { return "tok", nil },
		Exec: func(_ context.Context, bin string, _ []string, extraEnv []string) ([]byte, error) {
			gotEnv = extraEnv
			if bin == "lookout" {
				return []byte(`{"scanned":1,"findings":0,"elapsed":"1ms"}` + "\n"), nil
			}
			return []byte(`{"schema_version":"leeway.report/v1alpha1","generated_at":"2026-11-24T05:17:00Z","clusters":[{"cluster":"p1/us-central1/c1","status":"ok"}],"summary":{"clusters_total":1,"clusters_succeeded":1,"clusters_failed":0}}`), nil
		},
	})
	if err != nil || len(gotEnv) != 3 {
		t.Fatalf("Run with GoogleToken broker = %v, env=%v", err, gotEnv)
	}

	// Actionability filter: scenario probe namespace and cluster provider checks are excluded from GitOps PRs.
	due := []DueFinding{
		{Reason: DueNew, Entry: &LedgerEntry{SubjectKey: "s1", Namespace: "triage-lab", Kind: "audit.no_pdb"}},
		{Reason: DueNew, Entry: &LedgerEntry{SubjectKey: "s2", Namespace: "", Kind: "audit.public_control_plane"}},
		{Reason: DueNew, Entry: &LedgerEntry{SubjectKey: "s3", Namespace: "shop", Name: "orders", ObjectKind: "Deployment", Kind: "audit.no_pdb", Severity: "warning", Remediation: &Remediation{Kind: "manifest_patch", YAML: strings.Repeat("# line\n", 5000)}}},
	}
	act := GitOpsActionable(due)
	if len(act) != 1 || act[0].Entry.SubjectKey != "s3" {
		t.Fatalf("GitOpsActionable = %+v", act)
	}

	// Digest formatting clamps oversized payloads below switchboard's 32 KiB NoteOutbound ceiling.
	digest := FormatDigest([]string{"p1/us-central1/c1"}, Delta{Due: due, Retired: []*LedgerEntry{{SubjectKey: "r1", Kind: "audit.no_limits"}}}, "- https://github.com/org/repo/pull/1")
	if len(digest) > maxDigestBytes || !strings.Contains(digest, "truncated") {
		t.Errorf("digest len = %d (max %d), truncated missing", len(digest), maxDigestBytes)
	}

	// RunAutoPR failOpen error + canceled sleepContext.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := sleepContext(cctx, time.Second); err == nil {
		t.Error("sleepContext on canceled ctx succeeded")
	}
	failActor := &fakeAuditActor{failOpen: true}
	_, err = RunAutoPR(ctx, failActor, failActor, AutoPRConfig{
		TaskTemplate: []byte("kind: Task\nmetadata:\n  atespace: agents\n"),
		PollInterval: time.Millisecond,
		Sleep:        func(context.Context, time.Duration) error { return context.DeadlineExceeded },
	}, act, time.Now().UTC())
	if err == nil || len(failActor.deleted) != 1 {
		t.Errorf("RunAutoPR failOpen = %v, deleted=%v", err, failActor.deleted)
	}
}

func TestAdversarialRegressions(t *testing.T) {
	ctx := context.Background()

	// 1. cloud.unavailable and read.unavailable mark lookout degraded so open findings are NOT stale-closed.
	for _, kind := range []string{"cloud.unavailable", "read.unavailable"} {
		stream := `{"kind":"` + kind + `","severity":"warning","namespace":"shop","reason":"Unavailable","message":"denied"}` + "\n" +
			`{"scanned":1,"findings":1,"elapsed":"5ms"}` + "\n"
		if _, _, err := ParseLookoutStream("p1/us-central1/c1", strings.NewReader(stream)); err == nil || !strings.Contains(err.Error(), kind) {
			t.Errorf("ParseLookoutStream(%s) = %v, want degraded error", kind, err)
		}
	}
	if got := GitOpsActionable([]DueFinding{{Reason: DueNew, Entry: &LedgerEntry{Engine: EngineLookout, Kind: "read.unavailable", Namespace: "shop"}}}); len(got) != 0 {
		t.Errorf("GitOpsActionable admitted non-audit kind: %+v", got)
	}

	// 2. CanonicalClusterID handles gke_ contexts and kubeconfig:gke_ prefixes.
	if got := CanonicalClusterID("kubeconfig:gke_p1_us-central1_c1"); got != "p1/us-central1/c1" {
		t.Errorf("CanonicalClusterID(kubeconfig:gke_...) = %q", got)
	}

	// 3. Multi-cluster leeway --clusters uses 3-part GKE triples with broker, kubeconfig contexts with -kubeconfig, and honors -context without -kubeconfig.
	twoCfg := &Config{
		Atespace: "agents",
		Project:  "p1",
		Clusters: []ClusterTarget{
			{Name: "c1", Path: "projects/p1/locations/us-central1/clusters/c1"},
			{Name: "c2", Path: "projects/p2/locations/us-east1/clusters/c2"},
		},
	}
	var gotCmds []string
	captureExec := func(_ context.Context, bin string, args []string, _ []string) ([]byte, error) {
		gotCmds = append(gotCmds, bin+" "+strings.Join(args, " "))
		if bin == "lookout" {
			return []byte(`{"scanned":1,"findings":0,"elapsed":"1ms"}` + "\n"), nil
		}
		return []byte(`{"schema_version":"leeway.report/v1alpha1","generated_at":"2026-11-24T05:17:00Z","clusters":[{"cluster":"p1/us-central1/c1","status":"ok"},{"cluster":"p2/us-east1/c2","status":"ok"}],"summary":{"clusters_total":2,"clusters_succeeded":2,"clusters_failed":0}}`), nil
	}
	// Broker mode -> 3-part GKE triples.
	if _, err := Run(ctx, twoCfg, RunOptions{
		LedgerPath:  filepath.Join(t.TempDir(), "l1.json"),
		GoogleToken: func(context.Context) (string, error) { return "tok", nil },
		Exec:        captureExec,
	}); err != nil {
		t.Fatalf("broker two-cluster Run: %v", err)
	}
	if !slices.ContainsFunc(gotCmds, func(s string) bool {
		return strings.HasPrefix(s, "leeway report") && strings.Contains(s, "--clusters=p1/us-central1/c1,p2/us-east1/c2")
	}) {
		t.Errorf("broker leeway args = %v, want 3-part GKE triples", gotCmds)
	}

	// Workstation -kubeconfig multi-cluster -> passes --kubeconfig and gke_ contexts.
	gotCmds = nil
	if _, err := Run(ctx, twoCfg, RunOptions{
		LedgerPath: filepath.Join(t.TempDir(), "l2.json"),
		Kubeconfig: "/tmp/kc",
		Exec:       captureExec,
	}); err != nil {
		t.Fatalf("kubeconfig two-cluster Run: %v", err)
	}
	if !slices.ContainsFunc(gotCmds, func(s string) bool {
		return strings.HasPrefix(s, "leeway report") && strings.Contains(s, "--kubeconfig=/tmp/kc") && strings.Contains(s, "--clusters=gke_p1_us-central1_c1,gke_p2_us-east1_c2")
	}) {
		t.Errorf("kubeconfig leeway args = %v", gotCmds)
	}

	// Single-cluster -context without -kubeconfig -> passes --context to both lookout and leeway.
	gotCmds = nil
	oneCfg := &Config{Atespace: "agents", Project: "p1", Clusters: twoCfg.Clusters[:1]}
	if _, err := Run(ctx, oneCfg, RunOptions{
		LedgerPath:  filepath.Join(t.TempDir(), "l3.json"),
		KubeContext: "my-ctx",
		Exec:        captureExec,
	}); err != nil {
		t.Fatalf("context-only Run: %v", err)
	}
	if len(gotCmds) != 2 || !strings.Contains(gotCmds[0], "--context=my-ctx") || !strings.Contains(gotCmds[1], "--context=my-ctx") {
		t.Errorf("context-only cmds = %v, want --context=my-ctx on both", gotCmds)
	}

	// 4. AutoPR PR attribution caps at AutoPRMaxFindings (10) and records multiple PR URLs.
	var manyDue []DueFinding
	for i := range 12 {
		manyDue = append(manyDue, DueFinding{
			Reason: DueNew,
			Entry: &LedgerEntry{
				SubjectKey: fmt.Sprintf("sub-%02d", i),
				Engine:     EngineLookout,
				Kind:       "audit.no_pdb",
				Namespace:  "shop",
				Name:       fmt.Sprintf("app-%02d", i),
				Severity:   "warning",
			},
		})
	}
	multiActor := &fakeMultiPRActor{}
	sum := &RunSummary{}
	prNote, prBySub, err := maybeRunAutoPR(ctx, &Config{AutoPR: true}, RunOptions{
		TaskTemplate: []byte("kind: Task\nmetadata:\n  atespace: agents\n"),
		ChatTasks:    multiActor,
		ChatAgent:    multiActor,
		AutoPRPoll:   time.Millisecond,
		AutoPRSleep:  func(context.Context, time.Duration) error { return nil },
	}, manyDue, sum, time.Now().UTC(), func(string, ...any) {})
	if err != nil {
		t.Fatalf("maybeRunAutoPR: %v", err)
	}
	if len(prBySub) != AutoPRMaxFindings || prBySub["sub-10"] != "" || prBySub["sub-11"] != "" {
		t.Errorf("prBySub attributed past AutoPRMaxFindings: len=%d", len(prBySub))
	}
	if !strings.Contains(prNote, "pull/1") || !strings.Contains(prNote, "pull/2") || !strings.Contains(prBySub["sub-00"], "pull/2") {
		t.Errorf("multi-PR attribution = note=%q sub0=%q", prNote, prBySub["sub-00"])
	}
}

type fakeMultiPRActor struct{ fakeAuditActor }

func (f *fakeMultiPRActor) Events(context.Context, string, string) (*scenario.Transcript, error) {
	return &scenario.Transcript{
		Server: "core-agent/test",
		Events: []scenario.Event{
			{Seq: 1, Author: "user", Texts: []string{"brief"}},
			{Seq: 2, Author: "agent", Role: "model", Texts: []string{"Opened https://github.com/org/repo-a/pull/1 and https://github.com/org/repo-b/pull/2 (dup https://github.com/org/repo-a/pull/1)"}},
		},
	}, nil
}
