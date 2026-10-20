# Changelog

All notable changes to shipmind are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and releases
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). A
release is a tested set of component versions (see "Cutting a release" in
[`CONTRIBUTING.md`](./CONTRIBUTING.md)).

## [Unreleased]

### Added

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
- Profile invariant tests: every rendered image is pinned in
  `versions.yaml` (or a `ko://` placeholder for the overlay), no remote
  bases, and ax's Redis keeps its volume and append-only log.
- The design ([`docs/DESIGN.md`](./docs/DESIGN.md)) and the decision log
  ([`docs/decisions.md`](./docs/decisions.md)).
