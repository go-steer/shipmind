# Decision log

Newest first. Each entry: what was decided, why, and what was rejected.
A decision that changes gets a new entry pointing at the old one; old
entries are not edited.

## 2026-10-20: Vendor plain manifests, one kustomize base each

Components' manifests are vendored as files at their pinned version, one
kustomize base per manifest, verified offline against a lock
(`VENDOR.yaml`). Profiles and overlays compose them with kustomize.
Substrate is the exception: it installs through its own installer
(`ate-setup`), run at the pinned fork commit.

**Why:** plain files make every pin bump a reviewable manifest diff and
let `kubectl diff -k` and `apply -k` work without plugins or flags. One
base per manifest is forced by kustomize: two manifests declaring the
same Namespace (ax's both do) cannot share a base, and a kustomization
cannot load files outside its directory. `ate-setup` has no render mode
and does more than apply (rollout waits, credential provider wiring), so
re-implementing it as manifests would fork its logic.

**Rejected:** Helm as the composition layer (only some components ship
charts); vendoring rendered Helm output now (no component needs it yet;
revisit when one ships only a chart); one base per component (breaks on
duplicate Namespaces); rendering Substrate's manifests ourselves.

## 2026-10-20: Substrate profile first

Milestone 1 makes the agent-substrate demo cluster deployable from
shipmind; the standalone profile and kind PR tests are Milestone 2.

**Why:** the substrate profile is where the platform runs today and where
its only copy of the configuration is unversioned. Moving it first makes
shipmind the source of truth from its first milestone, with no
throwaway stage.

**Rejected:** standalone first. Cheaper PR tests sooner, but the live
system would stay in an unversioned directory for another milestone.

## 2026-10-20: Public repository, private environment overlay

shipmind is public like the rest of go-steer. Environment-specific values
(project IDs, domains, registry paths, the agent-substrate cluster's
overlay) go in a private companion repository,
`go-steer/shipmind-agent-substrate`.

**Why:** the platform is generic and should read that way; one demo
cluster's coordinates are not part of it. The same split as supercargo
and supercargo-nstech.

**Consequence:** images built from forks currently live in a private
registry, so `versions.yaml` pins those components by source commit until
the images are published (Milestone 1).

## 2026-10-20: The reference repositories stay out

mast-sre-agent (stock mast plus a declarative bundle, the NovaSRE port)
and k8s-sre-agent (mast embedded as a Go library) are references for
building on mast, not parts of the platform. Neither moves into
shipmind. k8s-sre-agent's eval scenarios are copied into `e2e/scenarios`,
then it is archived; both get a README pointer to shipmind.

**Why:** the platform runs mast's gke-triage, the generalist diagnoser.
The October prompt A/B found the generalist beat the routed, procedural
style mast-sre-agent's nine diagnosers use, and k8s-sre-agent is pinned
to mast v0.4.0.

## 2026-10-20: A distribution, not a monorepo

shipmind pins, deploys and tests the components; it holds none of their
source.

**Why:** the components have separate release cadences, CI, docs sites
and, for ax and Substrate, upstreams. Pinning by version gets the
integration benefits (one tested set, one place for e2e) without merging
histories or coupling releases.

**Rejected:** a monorepo of all components; continuing to run e2e from
inside component repositories.

## 2026-10-20: The name is shipmind

A ship mind is the intelligence that runs a ship, which fits an agent
platform, and the name sits with the rest of go-steer's nautical crew
(mast, lookout, purser, steward, supercargo).

**Rejected:** bridge (overloaded in software, and it names a control
plane), shipyard (accurate for a distribution but not agentic),
shipwright (collides with CNCF's shipwright-io/build on Kubernetes), keel
(keel.sh).
