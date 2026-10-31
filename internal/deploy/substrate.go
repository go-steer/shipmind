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
	"slices"
	"strings"

	"github.com/go-steer/shipmind/internal/cluster"
)

// substratePatch is a change the substrate profile needs on top of a
// Substrate install, which its installer has no setting for. Substrate
// isn't rendered (its installer applies it), so up patches the live
// object in place, and status checks it: an installer run that puts the
// object back shows as drift.
type substratePatch struct {
	Kind, Namespace, Name string // Kind as status names workloads
	What                  string // the change, for messages
	Type                  string // "strategic" or "json"
	Patch                 string
	// Applied reports whether the live object (JSON) carries the change.
	// An error means the object holds a conflicting setting that the
	// patch would not fix, which needs a person.
	Applied func(obj []byte) (bool, error)
}

func (sp substratePatch) what() string {
	return "substrate " + sp.Kind + "/" + sp.Namespace + "/" + sp.Name
}

const parkedBudget = "--parked-request-budget=60s"

var substratePatches = []substratePatch{
	{
		// Upstream runs atelet, and its objectstore-plugin sidecar, on
		// every node; only worker nodes run sandboxes. The workers'
		// ComputeClass labels its nodes ate.dev/worker-node=true.
		Kind: "DaemonSet", Namespace: "ate-system", Name: "atelet",
		What:  "atelet only on worker nodes (nodeSelector ate.dev/worker-node=true)",
		Type:  "strategic",
		Patch: `{"spec":{"template":{"spec":{"nodeSelector":{"ate.dev/worker-node":"true"}}}}}`,
		Applied: func(obj []byte) (bool, error) {
			var ds struct {
				Spec struct {
					Template struct {
						Spec struct {
							NodeSelector map[string]string `json:"nodeSelector"`
						} `json:"spec"`
					} `json:"template"`
				} `json:"spec"`
			}
			if err := json.Unmarshal(obj, &ds); err != nil {
				return false, err
			}
			return ds.Spec.Template.Spec.NodeSelector["ate.dev/worker-node"] == "true", nil
		},
	},
	{
		// The router holds a request for an actor that is resuming for
		// up to its parked-request budget, 5s by default: shorter than a
		// cold resume after a long suspend (about 11s seen), so the first
		// request after one failed with a 504. 60s, and a grace period
		// that still fits the router's drain sequence (13s delay, about
		// 15s, a drain timeout derived from the budget, slack).
		Kind: "Deployment", Namespace: "ate-system", Name: "atenet-router",
		What: "router parks requests for a resuming actor up to 60s (" + parkedBudget + ", grace period 120s)",
		Type: "json",
		Patch: `[{"op":"test","path":"/spec/template/spec/containers/0/name","value":"atenet-router"},` +
			`{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"` + parkedBudget + `"},` +
			`{"op":"replace","path":"/spec/template/spec/terminationGracePeriodSeconds","value":120}]`,
		Applied: func(obj []byte) (bool, error) {
			var d struct {
				Spec struct {
					Template struct {
						Spec struct {
							Grace      *int `json:"terminationGracePeriodSeconds"`
							Containers []struct {
								Name string   `json:"name"`
								Args []string `json:"args"`
							} `json:"containers"`
						} `json:"spec"`
					} `json:"template"`
				} `json:"spec"`
			}
			if err := json.Unmarshal(obj, &d); err != nil {
				return false, err
			}
			pod := d.Spec.Template.Spec
			if len(pod.Containers) == 0 || pod.Containers[0].Name != "atenet-router" {
				return false, fmt.Errorf("its first container is not atenet-router, which the patch expects")
			}
			args := pod.Containers[0].Args
			for _, a := range args {
				if strings.HasPrefix(a, "--parked-request-budget=") && a != parkedBudget {
					return false, fmt.Errorf("it already sets %s; shipmind wants %s", a, parkedBudget)
				}
			}
			return slices.Contains(args, parkedBudget) && pod.Grace != nil && *pod.Grace == 120, nil
		},
	},
}

// state reads the object and reports whether it needs the patch; an
// object that doesn't exist needs nothing (status reports it missing
// from the install).
func (sp substratePatch) state(ctx context.Context, c cluster.Cluster) (exists, applied bool, err error) {
	obj, found, err := c.Get(ctx, strings.ToLower(sp.Kind), sp.Namespace, sp.Name)
	if err != nil || !found {
		return found, false, err
	}
	applied, err = sp.Applied(obj)
	if err != nil {
		return true, false, fmt.Errorf("%s: %w", sp.what(), err)
	}
	return true, applied, nil
}

// substratePatchStatus adds a change for every patch the cluster lacks.
func substratePatchStatus(ctx context.Context, p *Plan, c cluster.Cluster, add func(what, detail string)) error {
	if p.Env.Substrate == nil {
		return nil
	}
	for _, sp := range substratePatches {
		exists, applied, err := sp.state(ctx, c)
		if err != nil {
			return err
		}
		if exists && !applied {
			add(sp.what(), "lacks shipmind's patch: "+sp.What+" (up applies it)")
		}
	}
	return nil
}

// neededPatches returns the patches the cluster lacks, for up to apply
// once it starts writing. A JSON patch that adds an argument isn't
// idempotent, so each is applied only when its check says it is
// missing; a conflicting setting fails here, before anything is written.
func neededPatches(ctx context.Context, p *Plan, c cluster.Cluster, log func(string, ...any)) ([]substratePatch, error) {
	if p.Env.Substrate == nil {
		return nil, nil
	}
	var need []substratePatch
	for _, sp := range substratePatches {
		exists, applied, err := sp.state(ctx, c)
		if err != nil {
			return nil, err
		}
		switch {
		case !exists:
			log("%s: not installed, so not patched", sp.what())
		case !applied:
			need = append(need, sp)
		}
	}
	return need, nil
}

// applyPatches applies what neededPatches returned.
func applyPatches(ctx context.Context, c cluster.Cluster, need []substratePatch, dryRun bool, log func(string, ...any)) error {
	for _, sp := range need {
		if err := c.Patch(ctx, strings.ToLower(sp.Kind), sp.Namespace, sp.Name, sp.Type, []byte(sp.Patch), dryRun); err != nil {
			return fmt.Errorf("%s: %w", sp.what(), err)
		}
		log("%s patched: %s%s", sp.what(), sp.What, dry(dryRun))
	}
	return nil
}
