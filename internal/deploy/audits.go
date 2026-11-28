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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-steer/shipmind/internal/agent"
	"github.com/go-steer/shipmind/internal/bom"
	"github.com/go-steer/shipmind/internal/cluster"
	"github.com/go-steer/shipmind/internal/env"
	"github.com/go-steer/shipmind/internal/render"
)

// Where the scheduled posture sweep's CronJob, PVC, and Secret live (#127).
const (
	AuditsNamespace = render.AuditNamespace
	AuditsCronJob   = render.AuditCronJob
	AuditsSecret    = "posture-audit"
	auditsBaseDir   = "deploy/audits/substrate"
	switchboardURL  = "http://switchboard.switchboard.svc.cluster.local:8080/v1/messages"
)

type auditScopeConfig struct {
	Project  string `json:"project,omitempty"`
	Location string `json:"location,omitempty"`
	Path     string `json:"path,omitempty"`
	Fleet    string `json:"fleet,omitempty"`
}

type auditRepoConfig struct {
	Repository string `json:"repository"`
	BaseBranch string `json:"base_branch"`
}

type auditRuntimeConfig struct {
	Atespace              string             `json:"atespace"`
	Project               string             `json:"project"`
	Scope                 []auditScopeConfig `json:"scope"`
	Schedule              string             `json:"schedule"`
	RefreshWindow         string             `json:"refresh_window"`
	MinConsecutive        int                `json:"min_consecutive"`
	DomainUnavailableKeys string             `json:"domain_unavailable_keys"`
	ExemptionsFile        string             `json:"exemptions_file,omitempty"`
	LookoutVersion        string             `json:"lookout_version,omitempty"`
	AlertChannel          string             `json:"alert_channel,omitempty"`
	SwitchboardURL        string             `json:"switchboard_url,omitempty"`
	AutoPR                bool               `json:"auto_pr,omitempty"`
	Repositories          []auditRepoConfig  `json:"repositories,omitempty"`
}

// Prepare renders and configures scheduled posture audit objects on p when
// p.Env.Audits is set (or when the overlay already includes
// deploy/audits/substrate). It is idempotent.
func Prepare(p *Plan) error {
	if p == nil || p.Env == nil {
		return nil
	}
	_, present := p.Objects.Find("CronJob", AuditsNamespace, AuditsCronJob)
	if p.Env.Audits == nil && !present {
		return nil
	}
	if !present {
		base, err := render.Build(filepath.Join(p.Root, auditsBaseDir))
		if err != nil {
			return fmt.Errorf("render %s: %w", auditsBaseDir, err)
		}
		for _, o := range base {
			if _, exists := p.Objects.Find(o.Kind(), o.Namespace(), o.Name()); !exists {
				p.Objects = append(p.Objects, o)
			}
		}
	}
	var suspend bool
	var runnerImg string
	if p.Env.Audits != nil {
		suspend = p.Env.Audits.Suspend
		runnerImg = p.Env.Audits.RunnerImage
	}
	return render.ConfigureAuditCronJob(p.Objects, p.Env.Audits.CronSchedule(), suspend, runnerImg)
}

func auditSecret(ctx context.Context, p *Plan, c cluster.Cluster, t agent.Tokens, haveTokens bool, ingressTok string) (*secretWrite, error) {
	if _, ok := p.Objects.Find("CronJob", AuditsNamespace, AuditsCronJob); !ok {
		return nil, nil
	}
	e := p.Env
	aud := e.Audits
	effScope := e.EffectiveAuditScope()
	scope := make([]auditScopeConfig, 0, len(effScope))
	for _, s := range effScope {
		scope = append(scope, auditScopeConfig{
			Project:  s.Project,
			Location: s.Location,
			Path:     s.Path,
			Fleet:    s.Fleet,
		})
	}
	cfg := auditRuntimeConfig{
		Atespace:              e.Agent.Atespace,
		Project:               e.Agent.Project,
		Scope:                 scope,
		Schedule:              aud.CronSchedule(),
		RefreshWindow:         aud.RefreshString(),
		MinConsecutive:        aud.MinConsecutiveRuns(),
		DomainUnavailableKeys: aud.DomainKeys(),
		LookoutVersion:        lookoutPinVersion(p.Root),
	}
	if e.Slack != nil && e.Slack.AlertChannel != "" {
		cfg.AlertChannel = e.Slack.AlertChannel
		cfg.SwitchboardURL = switchboardURL
	}
	want := map[string][]byte{}
	if aud != nil && aud.Exemptions != "" {
		exBytes, err := readAuditExemptions(e.Dir, aud.Exemptions)
		if err != nil {
			return nil, err
		}
		cfg.ExemptionsFile = "/etc/shipmind-audits/exemptions.yaml"
		want["exemptions.yaml"] = exBytes
	}
	if aud != nil && aud.AutoPR && e.Agent.VCS != nil {
		cfg.AutoPR = true
		for _, r := range e.Agent.VCS.Repositories {
			cfg.Repositories = append(cfg.Repositories, auditRepoConfig{
				Repository: r.Repository,
				BaseBranch: r.Base(),
			})
		}
		if haveTokens {
			task, err := agent.AuditTask(p.Root, e, t)
			if err != nil {
				return nil, err
			}
			want["task.yaml"] = task
			want["token"] = []byte(t.Attach)
		} else {
			want["task.yaml"] = []byte("\x00")
			want["token"] = []byte("\x00")
		}
	}
	cfgJSON, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	want["config.json"] = append(cfgJSON, '\n')
	if ingressTok != "" {
		want["ingress-token"] = []byte(ingressTok)
	}
	live, found, err := c.Secret(ctx, AuditsNamespace, AuditsSecret)
	if err != nil {
		return nil, fmt.Errorf("secret %s/%s: %w", AuditsNamespace, AuditsSecret, err)
	}
	d := diffData(live, found, want)
	if d == "" {
		return nil, nil
	}
	return &secretWrite{
		Namespace: AuditsNamespace,
		Name:      AuditsSecret,
		Data:      want,
		Why:       d + " against the render",
	}, nil
}

func readAuditExemptions(dir, rel string) ([]byte, error) {
	if dir == "" {
		return nil, fmt.Errorf("audits.exemptions %q: environment directory is unset", rel)
	}
	p := filepath.Join(dir, filepath.FromSlash(rel))
	info, err := os.Lstat(p)
	if err != nil {
		return nil, fmt.Errorf("audits.exemptions %q: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("audits.exemptions %q: not a regular file", rel)
	}
	return os.ReadFile(p) //nolint:gosec // validated relative regular file inside operator's overlay directory
}

func lookoutPinVersion(root string) string {
	f, err := bom.Load(filepath.Join(root, "versions.yaml"))
	if err != nil {
		return ""
	}
	if c, ok := f.Component("k8s-lookout"); ok {
		return c.Version
	}
	return ""
}

var _ = (*env.Audits)(nil)
