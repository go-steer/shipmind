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
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/go-steer/shipmind/internal/agent"
	"github.com/go-steer/shipmind/internal/cluster"
	"github.com/go-steer/shipmind/internal/env"
)

// Where switchboard's agent Secret lives, and what reads it: the
// Deployment in deploy/switchboard/slack mounts config.json and
// triage.yaml, and takes token as the agent's bearer.
const (
	SwitchboardNamespace = "switchboard"
	Switchboard          = "switchboard"
	SwitchboardSecret    = "switchboard-agent" // keys: config.json, triage.yaml, token
)

// secretWrite is a Secret up writes because the cluster's copy is missing
// or differs: its data, and why. Deployments that read it only at startup
// are startup readers (readers), restarted when they run on an older copy.
type secretWrite struct {
	Namespace, Name string
	Data            map[string][]byte
	Why             string
}

func (w secretWrite) what() string { return "secret " + w.Namespace + "/" + w.Name }

// diffData names the keys whose values differ between want and live,
// without a value: "missing", or "differs (keys a, b)". Empty if equal.
func diffData(live map[string][]byte, found bool, want map[string][]byte) string {
	if !found {
		return "missing"
	}
	var keys []string
	for _, k := range slices.Sorted(maps.Keys(want)) {
		if !bytes.Equal(live[k], want[k]) {
			keys = append(keys, k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(live)) {
		if _, ok := want[k]; !ok {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	return "differs (keys " + strings.Join(keys, ", ") + ")"
}

// managedSecrets reads every `secrets` entry from Secret Manager and
// returns the ones the cluster lacks or holds different values for.
func managedSecrets(ctx context.Context, p *Plan, c cluster.Cluster) ([]secretWrite, error) {
	if len(p.Env.Secrets) == 0 {
		return nil, nil
	}
	if p.Secrets == nil {
		return nil, errors.New("shipmind.yaml lists secrets, but nothing reads Secret Manager")
	}
	var out []secretWrite
	for _, m := range p.Env.Secrets {
		want, err := fromSecretManager(ctx, p, m)
		if err != nil {
			return nil, err
		}
		live, found, err := c.Secret(ctx, m.Namespace(), m.Name())
		if err != nil {
			return nil, fmt.Errorf("secret %s: %w", m.Secret, err)
		}
		if d := diffData(live, found, want); d != "" {
			out = append(out, secretWrite{Namespace: m.Namespace(), Name: m.Name(), Data: want,
				Why: d + " against Secret Manager"})
		}
	}
	return out, nil
}

func fromSecretManager(ctx context.Context, p *Plan, m env.ManagedSecret) (map[string][]byte, error) {
	project := m.Project
	if project == "" {
		project = p.Env.Agent.Project
	}
	data := map[string][]byte{}
	for _, k := range slices.Sorted(maps.Keys(m.Keys)) {
		v, err := p.Secrets.Access(ctx, project, m.Keys[k])
		if err != nil {
			return nil, fmt.Errorf("secret %s, key %s: %w", m.Secret, k, err)
		}
		data[k] = v
	}
	return data, nil
}

// switchboardSecret returns switchboard's agent Secret if the render runs
// switchboard and the cluster's copy is missing or differs: its
// configuration and the agent's task template, rendered with the agent's
// tokens like lookout's, and the attach token as its bearer.
func switchboardSecret(ctx context.Context, p *Plan, c cluster.Cluster, t agent.Tokens) (*secretWrite, error) {
	if _, ok := p.Objects.Find("Deployment", SwitchboardNamespace, Switchboard); !ok {
		return nil, nil
	}
	cfg, err := agent.SwitchboardConfig(p.Root, p.Env)
	if err != nil {
		return nil, err
	}
	task, err := agent.Task(p.Root, p.Env, t)
	if err != nil {
		return nil, err
	}
	want := map[string][]byte{"config.json": cfg, "triage.yaml": task, "token": []byte(t.Attach)}
	live, found, err := c.Secret(ctx, SwitchboardNamespace, SwitchboardSecret)
	if err != nil {
		return nil, fmt.Errorf("secret %s/%s: %w", SwitchboardNamespace, SwitchboardSecret, err)
	}
	d := diffData(live, found, want)
	if d == "" {
		return nil, nil
	}
	return &secretWrite{Namespace: SwitchboardNamespace, Name: SwitchboardSecret, Data: want,
		Why: d + " against the render"}, nil
}

// writeSecrets applies each Secret, or in a dry run says it would; a
// Secret in a namespace the run creates can't be validated by a dry run.
func writeSecrets(ctx context.Context, c cluster.Cluster, ws []secretWrite, pending map[string]bool, dryRun bool, log func(string, ...any)) error {
	for _, w := range ws {
		if dryRun && pending[w.Namespace] {
			log("%s would be written (%s), in namespace %s, which the run creates (not validated; dry run)", w.what(), w.Why, w.Namespace)
			continue
		}
		if err := c.ApplySecret(ctx, cluster.SecretManifest(w.Namespace, w.Name, w.Data), dryRun); err != nil {
			return fmt.Errorf("%s: %w", w.what(), err)
		}
		log("%s written (%s)%s", w.what(), w.Why, dry(dryRun))
	}
	return nil
}

// readers returns every startup reader of p: the fixed ones, switchboard
// when the render runs it (its configuration, template and bearer), and
// the Deployments each managed Secret names under restart.
func readers(p *Plan) []startupReader {
	rs := slices.Clone(startupReaders)
	if _, ok := p.Objects.Find("Deployment", SwitchboardNamespace, Switchboard); ok {
		rs = append(rs, startupReader{Kind: "secret", Namespace: SwitchboardNamespace, Name: SwitchboardSecret,
			Deployment: Switchboard, What: "its configuration and agent template"})
	}
	for _, m := range p.Env.Secrets {
		for _, d := range m.Restart {
			rs = append(rs, startupReader{Kind: "secret", Namespace: m.Namespace(), Name: m.Name(),
				Deployment: d, What: "secret " + m.Name()})
		}
	}
	return rs
}
