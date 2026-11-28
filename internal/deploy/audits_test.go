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
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/go-steer/shipmind/internal/cluster"
	"github.com/go-steer/shipmind/internal/env"
	"github.com/go-steer/shipmind/internal/render"
)

func syncFakeClusterWrites(t *testing.T, fc *fakeCluster) {
	t.Helper()
	if fc.secrets == nil {
		fc.secrets = map[string]map[string][]byte{}
	}
	for _, raw := range fc.applied {
		var s struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
			Data map[string]string `yaml:"data"`
		}
		if err := yaml.Unmarshal([]byte(raw), &s); err != nil || s.Kind != "Secret" {
			continue
		}
		decoded := map[string][]byte{}
		for k, v := range s.Data {
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				t.Fatalf("decode secret %s/%s key %s: %v", s.Metadata.Namespace, s.Metadata.Name, k, err)
			}
			decoded[k] = b
		}
		fc.secrets[s.Metadata.Namespace+"/"+s.Metadata.Name] = decoded
	}
	if len(fc.axApplied) > 0 {
		fc.workspace = []byte(fc.axApplied[len(fc.axApplied)-1])
	}
}

func TestAuditsReconcileStatusUpDown(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	exFile := filepath.Join(dir, "exemptions.yaml")
	if err := os.WriteFile(exFile, []byte("exemptions:\n  - reason: accepted\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := plan(t)
	swObjs, err := render.Build("testdata/switchboard")
	if err != nil {
		t.Fatal(err)
	}
	p.Objects = append(p.Objects, swObjs...)
	p.Env.Dir = dir
	p.Env.Agent.ChatImage = "registry.example.com/agent-core@sha256:" + strings.Repeat("1", 64)
	p.Env.Agent.IncidentImage = p.Env.Agent.ChatImage
	p.Env.Agent.VCS = &env.VCS{
		Secret:       "gh-token",
		Repositories: []env.Repository{{Repository: "org/gitops"}},
	}
	p.Env.Slack = &env.Slack{
		App:          "shipmind-stage",
		Command:      "/shipmind-stage",
		AlertChannel: "C0123456789",
	}
	minCons := 2
	domKeys := "topology.kubernetes.io/zone"
	p.Env.Audits = &env.Audits{
		Schedule:              "0 6 * * *",
		RefreshWindow:         "72h",
		MinConsecutive:        &minCons,
		Exemptions:            "exemptions.yaml",
		DomainUnavailableKeys: &domKeys,
		AutoPR:                true,
	}

	fc := &fakeCluster{
		missingNS: []string{"shipmind-audits"},
		secrets:   map[string]map[string][]byte{},
	}

	// Fresh cluster Status: reports namespace shipmind-audits + missing posture-audit Secret.
	rep, err := Status(ctx, p, fc)
	if err != nil {
		t.Fatalf("Status fresh: %v", err)
	}
	hasNS, hasAudSecret := false, false
	for _, ch := range rep.Changes {
		if ch.What == "namespace shipmind-audits" {
			hasNS = true
		}
		if ch.What == "secret shipmind-audits/posture-audit" {
			hasAudSecret = true
		}
	}
	if !hasNS || !hasAudSecret {
		t.Fatalf("Status changes = %+v, want namespace shipmind-audits and secret shipmind-audits/posture-audit", rep.Changes)
	}

	// DryRun on fresh cluster skips server validation of pending shipmind-audits Secret.
	if err := Up(ctx, p, fc, UpOptions{DryRun: true}); err != nil {
		t.Fatalf("Up DryRun fresh: %v", err)
	}

	// Real Up creates namespace, tokens, switchboard ingress token, posture-audit Secret, CronJob, PVC, Workspace.
	fc.missingNS = nil
	if err := Up(ctx, p, fc, UpOptions{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	syncFakeClusterWrites(t, fc)
	audSec := fc.secrets["shipmind-audits/posture-audit"]
	if audSec == nil {
		t.Fatal("secret shipmind-audits/posture-audit was not written")
	}
	for _, key := range []string{"config.json", "exemptions.yaml", "ingress-token", "task.yaml", "token"} {
		if len(audSec[key]) == 0 {
			t.Errorf("secret shipmind-audits/posture-audit missing key %q", key)
		}
	}
	var cfg auditRuntimeConfig
	if err := json.Unmarshal(audSec["config.json"], &cfg); err != nil {
		t.Fatalf("unmarshal config.json: %v", err)
	}
	if cfg.Schedule != "0 6 * * *" || cfg.RefreshWindow != "72h" || cfg.MinConsecutive != 2 ||
		cfg.DomainUnavailableKeys != "topology.kubernetes.io/zone" || cfg.AlertChannel != "C0123456789" ||
		!cfg.AutoPR || cfg.LookoutVersion == "" || len(cfg.Repositories) != 1 ||
		len(cfg.Scope) != 1 || cfg.Scope[0].Path != "projects/example-project/locations/us-central1/clusters/demo" {
		t.Errorf("auditRuntimeConfig = %+v", cfg)
	}

	// audits.scope override narrows the audit runtime config without changing
	// agent.clusters.scope, and surfaces immediately as drift.
	p.Env.Audits.Scope = []env.ClusterScope{{Project: "prod-proj", Location: "us-east1"}, {Fleet: "fleet-host"}}
	repOver, err := Status(ctx, p, fc)
	if err != nil {
		t.Fatalf("Status(audits.scope override): %v", err)
	}
	if drift := repOver.Drift(); len(drift) != 1 || drift[0].What != "secret shipmind-audits/posture-audit" || !strings.Contains(drift[0].Detail, "config.json") {
		t.Fatalf("drift after audits.scope override = %+v", drift)
	}
	if err := Up(ctx, p, fc, UpOptions{}); err != nil {
		t.Fatalf("Up(audits.scope override): %v", err)
	}
	syncFakeClusterWrites(t, fc)
	var cfgOver auditRuntimeConfig
	if err := json.Unmarshal(fc.secrets["shipmind-audits/posture-audit"]["config.json"], &cfgOver); err != nil {
		t.Fatalf("unmarshal override config.json: %v", err)
	}
	if len(cfgOver.Scope) != 2 || cfgOver.Scope[0].Project != "prod-proj" || cfgOver.Scope[0].Location != "us-east1" || cfgOver.Scope[1].Fleet != "fleet-host" {
		t.Errorf("override auditRuntimeConfig.Scope = %+v", cfgOver.Scope)
	}
	p.Env.Audits.Scope = nil
	if err := Up(ctx, p, fc, UpOptions{}); err != nil {
		t.Fatalf("Up(restore default audit scope): %v", err)
	}
	syncFakeClusterWrites(t, fc)

	// Status right after Up reports zero drift!
	fc.workloads = map[string][]cluster.Workload{
		"ns":          {{ID: "Deployment/ns/redis", Current: true, Available: 1}},
		"switchboard": {{ID: "Deployment/switchboard/switchboard", Current: true, Available: 1}},
	}
	rep2, err := Status(ctx, p, fc)
	if err != nil {
		t.Fatalf("Status post-Up: %v", err)
	}
	if drift := rep2.Drift(); len(drift) != 0 {
		t.Fatalf("post-Up drift = %+v, want none", drift)
	}

	// Editing exemptions.yaml surfaces immediately as drift on secret shipmind-audits/posture-audit.
	if err := os.WriteFile(exFile, []byte("exemptions:\n  - reason: updated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep3, err := Status(ctx, p, fc)
	if err != nil {
		t.Fatalf("Status after exemptions edit: %v", err)
	}
	if drift := rep3.Drift(); len(drift) != 1 || drift[0].What != "secret shipmind-audits/posture-audit" || !strings.Contains(drift[0].Detail, "exemptions.yaml") {
		t.Fatalf("drift after exemptions edit = %+v", drift)
	}

	// Down preserves Namespace/shipmind-audits and PersistentVolumeClaim/posture-ledger unless all=true.
	if _, err := Down(ctx, p, fc, false); err != nil {
		t.Fatalf("Down(all=false): %v", err)
	}
	if strings.Contains(fc.deleted, "kind: PersistentVolumeClaim") || strings.Contains(fc.deleted, "kind: Namespace") {
		t.Errorf("Down(all=false) deleted kept audit resources:\n%s", fc.deleted)
	}
	if !strings.Contains(fc.deleted, "kind: CronJob") {
		t.Errorf("Down(all=false) did not delete CronJob:\n%s", fc.deleted)
	}
}
