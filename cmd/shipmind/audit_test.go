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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/shipmind/internal/audit"
	"github.com/go-steer/shipmind/internal/env"
	"github.com/go-steer/shipmind/internal/scenario"
)

type fakeAuditKube struct {
	scenario.Kube
	secrets map[string]map[string][]byte
}

func (f fakeAuditKube) Secret(_ context.Context, ns, name string) (map[string][]byte, bool, error) {
	d, ok := f.secrets[ns+"/"+name]
	return d, ok, nil
}

func TestAuditCLI(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"audit"}, &out, &errb); code != 2 {
		t.Fatalf("audit no subcommand = %d, want 2", code)
	}
	errb.Reset()
	if code := run([]string{"audit", "run", "-h"}, &out, &errb); code != 0 {
		t.Fatalf("audit run -h = %d, want 0", code)
	}
	if code := run([]string{"audit", "run", "-bogus"}, &out, &errb); code != 2 {
		t.Fatalf("audit run -bogus = %d, want 2", code)
	}
	if code := run([]string{"audit", "run", "-config=x", "-ledger=y"}, &out, &errb); code != 2 {
		t.Fatalf("audit run missing mode = %d, want 2", code)
	}
	if code := run([]string{"audit", "run", "-config=x", "-ledger=y", "-in-cluster", "-router-cert=only"}, &out, &errb); code != 2 {
		t.Fatalf("audit run half mTLS = %d, want 2", code)
	}
	if code := run([]string{"audit", "run", "-config=" + filepath.Join(t.TempDir(), "missing.json"), "-ledger=y", "-in-cluster"}, &out, &errb); code != 1 {
		t.Fatalf("audit run missing config = %d, want 1", code)
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	ledgerPath := filepath.Join(dir, "ledger.json")
	kcPath := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kcPath, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := audit.Config{
		Atespace: "agents",
		Project:  "p1",
		Clusters: []audit.ClusterTarget{{Name: "c1", Path: "projects/p1/locations/us-central1/clusters/c1"}},
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ingress-token"), []byte("tok123\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	origExec := auditExec
	defer func() { auditExec = origExec }()
	auditExec = func(_ context.Context, bin string, _ []string, _ []string) ([]byte, error) {
		if bin == "lookout" {
			return []byte(`{"scanned":5,"findings":0,"elapsed":"5ms"}` + "\n"), nil
		}
		return []byte(`{"schema_version":"leeway.report/v1alpha1","generated_at":"2026-11-24T05:17:00Z","clusters":[{"cluster":"p1/us-central1/c1","status":"ok"}],"summary":{"clusters_total":1,"clusters_succeeded":1,"clusters_failed":0}}`), nil
	}

	out.Reset()
	errb.Reset()
	if code := run([]string{"audit", "run", "-config=" + cfgPath, "-ledger=" + ledgerPath, "-kubeconfig=" + kcPath, "-context=demo"}, &out, &errb); code != 0 {
		t.Fatalf("audit run kubeconfig = %d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), `"llm_tokens": 0`) || !strings.Contains(out.String(), `"clusters_succeeded": 1`) {
		t.Errorf("stdout = %s", out.String())
	}

	// Test cachedSecretTokenProvider (Secret hit + metadata server fallback).
	mdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing header", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"md-tok"}`))
	}))
	defer mdSrv.Close()
	origMD := metadataTokenURL
	metadataTokenURL = mdSrv.URL
	defer func() { metadataTokenURL = origMD }()

	tp := cachedSecretTokenProvider(fakeAuditKube{secrets: map[string]map[string][]byte{
		"ate-credentials/google-token": {"token": []byte("sec-tok\n")},
	}})
	if tok, err := tp(context.Background()); err != nil || tok != "sec-tok" {
		t.Errorf("secret token = %q, %v", tok, err)
	}
	// Cached hit.
	if tok, err := tp(context.Background()); err != nil || tok != "sec-tok" {
		t.Errorf("cached secret token = %q, %v", tok, err)
	}
	tpFallback := cachedSecretTokenProvider(fakeAuditKube{})
	if tok, err := tpFallback(context.Background()); err != nil || tok != "md-tok" {
		t.Errorf("metadata fallback token = %q, %v", tok, err)
	}

	// Test -in-cluster outside a pod (fails InClusterKubeconfig), then with stubbed InClusterKubeconfig + AutoPR + mTLS.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if code := run([]string{"audit", "run", "-config=" + cfgPath, "-ledger=" + ledgerPath, "-in-cluster"}, &out, &errb); code != 1 {
		t.Fatalf("in-cluster outside pod = %d, want 1", code)
	}

	origIC, origAK, origRC := writeInClusterKubeconfig, newAuditKube, newRouterClient
	defer func() {
		writeInClusterKubeconfig, newAuditKube, newRouterClient = origIC, origAK, origRC
	}()
	writeInClusterKubeconfig = func(string) (string, error) { return kcPath, nil }
	newAuditKube = func(string) scenarioKube {
		return fakeAuditKube{secrets: map[string]map[string][]byte{"ate-credentials/google-token": {"token": []byte("sec-tok")}}}
	}
	closedTLS := false
	newRouterClient = func(scenario.RouterTLS) (*http.Client, func() error, error) {
		return http.DefaultClient, func() error { closedTLS = true; return nil }, nil
	}

	// In-cluster without AutoPR.
	out.Reset()
	errb.Reset()
	if code := run([]string{"audit", "run", "-config=" + cfgPath, "-ledger=" + ledgerPath, "-in-cluster"}, &out, &errb); code != 0 {
		t.Fatalf("in-cluster no autoPR = %d, stderr=%s", code, errb.String())
	}

	// In-cluster with AutoPR: missing task.yaml -> 1, missing token -> 1, then success with mTLS.
	cfg.AutoPR = true
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"audit", "run", "-config=" + cfgPath, "-ledger=" + ledgerPath, "-in-cluster"}, &out, &errb); code != 1 {
		t.Fatalf("in-cluster autoPR missing task.yaml = %d, want 1", code)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("kind: Task\nmetadata:\n  atespace: agents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"audit", "run", "-config=" + cfgPath, "-ledger=" + ledgerPath, "-in-cluster"}, &out, &errb); code != 1 {
		t.Fatalf("in-cluster autoPR missing token = %d, want 1", code)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("attach-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHIPMIND_AX", "/usr/local/bin/ax")
	out.Reset()
	errb.Reset()
	if code := run([]string{"audit", "run", "-config=" + cfgPath, "-ledger=" + ledgerPath, "-in-cluster", "-router-cert=c.pem", "-router-trust=t.pem"}, &out, &errb); code != 0 {
		t.Fatalf("in-cluster autoPR mTLS = %d, stderr=%s", code, errb.String())
	}
	if !closedTLS {
		t.Error("expected closeTLS to run on exit")
	}

	// Multi-cluster config with explicit -context must be rejected (exit 2).
	twoCfgPath := filepath.Join(dir, "two.json")
	twoCfg := audit.Config{
		Atespace: "agents",
		Project:  "p1",
		Clusters: []audit.ClusterTarget{
			{Name: "c1", Path: "projects/p1/locations/us-central1/clusters/c1"},
			{Name: "c2", Path: "projects/p1/locations/us-east1/clusters/c2"},
		},
	}
	tb, _ := json.Marshal(twoCfg)
	if err := os.WriteFile(twoCfgPath, tb, 0o600); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	if code := run([]string{"audit", "run", "-config=" + twoCfgPath, "-ledger=" + ledgerPath, "-kubeconfig=" + kcPath, "-context=demo"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "single cluster") {
		t.Fatalf("multi-cluster -context = %d, stderr=%s", code, errb.String())
	}

	// Scan error returns exit code 1 while still printing JSON summary.
	auditExec = func(_ context.Context, _ string, _ []string, _ []string) ([]byte, error) {
		return nil, os.ErrNotExist
	}
	out.Reset()
	errb.Reset()
	if code := run([]string{"audit", "run", "-config=" + cfgPath, "-ledger=" + ledgerPath, "-kubeconfig=" + kcPath, "-context=demo"}, &out, &errb); code != 1 {
		t.Fatalf("audit run scan error = %d, want 1", code)
	}

	if got := gkeContextForCluster("projects/p1/locations/us-central1/clusters/c1"); got != "gke_p1_us-central1_c1" {
		t.Errorf("gkeContextForCluster = %q", got)
	}
	if got := gkeContextForCluster("local"); got != "" {
		t.Errorf("gkeContextForCluster(local) = %q", got)
	}
	envTwo := &env.Env{Agent: env.Agent{Clusters: []env.Cluster{
		{Name: "c1", Path: "projects/p1/locations/us-central1/clusters/c1"},
		{Name: "c2", Path: "projects/p2/locations/us-east1/clusters/c2"},
	}}}
	if got := selectedClusterPath(envTwo, ""); got != envTwo.Agent.Clusters[0].Path {
		t.Errorf("selectedClusterPath default = %q", got)
	}
	if got := selectedClusterPath(envTwo, "c2"); got != envTwo.Agent.Clusters[1].Path {
		t.Errorf("selectedClusterPath c2 = %q", got)
	}
	if got := selectedClusterPath(envTwo, "missing"); got != "" {
		t.Errorf("selectedClusterPath missing = %q", got)
	}
}
