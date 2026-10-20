# Changelog

All notable changes to shipmind are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and releases
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). A
release is a tested set of component versions (see "Cutting a release" in
[`CONTRIBUTING.md`](./CONTRIBUTING.md)).

## [Unreleased]

### Added

- `shipmind status` and `up -dry-run` handle objects in namespaces that
  don't exist yet: the server can't dry-run into a missing namespace, so
  they are reported as new (status) or listed as to-be-created (dry run)
  instead of failing the whole diff.

- `deploy/e2e/substrate`: the nightly CronJob running the missing-env
  scenario in the cluster from the pinned `e2e-runner` image, with
  least-privilege RBAC. Environments opt in from their overlay. The
  profile invariant tests now cover `deploy/e2e` too.

- `shipmind scenario run` and `e2e/scenarios/missing-env`: the first
  end-to-end scenario (a crash loop on a missing `DATABASE_URL`), scored
  on the agent's answer and checked through idle suspend and wake. The
  `e2e-runner` image (shipmind, the ax CLI, kubectl and the scenarios) is
  built and published by the `images` workflow for running it in the
  cluster.
- The format presubmits check only files git tracks or would track, so
  worktrees under `.claude/` no longer fail them.

- Every image the substrate profile renders is now pinned in
  `versions.yaml` and, apart from the third-party ones (redis, lookout),
  published by shipmind's CI to `ghcr.io/go-steer/shipmind`: ax-server,
  substrate-scope, the token refresher and Substrate's worker image. The
  profiles reference them directly; no placeholders remain, and the
  invariant tests no longer allow any. `versions.yaml` also pins the
  nine Substrate release images its installer uses (CI publishes all
  sixteen). An environment overlay may still point at its own registry.

- `versions.yaml`, the platform's bill of materials, pinned to what the
  agent-substrate demo cluster runs on 2026-10-20: Substrate (fork
  b52296fe), ax (fork 685b160, deploy tag
  `deployed/substrate-v0.5-685b160`), the ax runner (e8bc98d), mast
  (922e549), k8s-lookout (v0.36.0) and substrate-scope (c2e8f50).
- `shipmind check` and `shipmind versions`: check the pins (a release tag
  or a full commit SHA; images by digest; one YAML document) and list
  them. The `verify-versions` presubmit runs the check on every PR.
- `shipmind vendor`: fetch each component's manifests at its pin into
  `deploy/vendor/<component>/`, one kustomize base per manifest, with a
  `VENDOR.yaml` lock. `shipmind check` now also verifies every vendored
  file against its lock, offline.
- The substrate profile's first part, `deploy/profiles/substrate/ax`:
  ax-server and Redis from the ax fork's manifests, Redis pinned by
  digest, the egress trust bundle turned on. Rendered against the live
  agent-substrate cluster, the only difference is Redis's image moving
  from `redis:7-alpine` to the same image by digest.
- `deploy/profiles/substrate/scope`: substrate-scope from its own
  manifests at c2e8f50, its image left as a placeholder for the overlay.
  The architecture page's nginx pin moved out of `versions.yaml`: that
  page is the demo cluster's, not the platform's.
- `deploy/profiles/substrate/credentials`: the token refresher
  (`images/token-refresher`), which keeps a Workload Identity access token
  in a Secret for Substrate's egress gateway to inject, so agents never
  hold a Google credential. `setup/workload-identity.sh` (from
  core-agent's gke-troubleshoot recipe) grants its IAM.
- `deploy/profiles/substrate/lookout`: k8s-lookout v0.36.0 with the ax
  sink, its store on a volume, and lookout's own watcher ClusterRole at
  v0.36.0 (vendored). The live role predates v0.36.0 and lacks its
  `list volumeattachments` rule; applying the profile adds it.
- `deploy/profiles/substrate/workers`: Substrate's WorkerPool for ax
  tasks, its image a placeholder the overlay sets, held by a test to the
  same pinning rule as container images. `shipmind status` also checks
  Substrate, which its own installer applies and kubectl diff never
  sees: against the environment's `substrate` section, every listed
  workload must exist and no other may, every image must be Substrate's
  own at its pinned digest or one of versions.yaml's third-party pins,
  and every rollout must have finished. Rendered workloads with an
  unfinished rollout are drift too (a stuck rollout keeps the new
  template while old pods run). A missing ax binary is an error, no
  longer a missing Workspace.
- `.github/workflows/images.yml`: builds and publishes the token
  refresher, ax-server (the ax fork, with its own `.ko.yaml`),
  substrate-scope (its own Dockerfile) and every Substrate release image
  (the fork's own publisher) to `ghcr.io/go-steer/shipmind`, each built
  from its pin; images from other repositories are tagged with their
  source commit, and a main build skips what already exists.
- `shipmind status | up | down <env-dir>`: an environment is an overlay
  directory with a `shipmind.yaml` (kube context, the agent's ax
  settings, expected changes). `status` compares the whole environment
  with its cluster, read only (Kubernetes objects by `kubectl diff`, the
  agent's task template Secret and ax Workspace by value), and exits 3 on
  drift. `up` applies it, adopting or generating the agent's tokens once
  and never rotating them; `-dry-run` writes nothing. Both require the
  checkout to be at the overlay's pinned commit. Against agent-substrate:
  no drift.
- `agents/sre-triage/ax/`: the ax task template and Workspace templates,
  reproducing what agent-substrate runs.
- Profile invariant tests: every rendered image is pinned in
  `versions.yaml` (or a `ko://` placeholder for the overlay), no remote
  bases, ax's Redis keeps its volume and append-only log, and lookout's
  store stays on its volume.
- `images/agent-mast` and `agents/sre-triage`: the substrate profile's
  agent image, rebuilt from the mast and ax-runner pins with the bundles
  as they run on agent-substrate (workloads byte-identical to
  mast-triage:v13). CI builds it on PRs and publishes
  `ghcr.io/go-steer/shipmind/agent-mast:<commit>` from main.
  `versions.yaml` pins the first build (aa9729e, `sha256:82128f81…`) as
  the `agent-mast` component.
- The design ([`docs/DESIGN.md`](./docs/DESIGN.md)) and the decision log
  ([`docs/decisions.md`](./docs/decisions.md)).
