# AGENTS.md — shipmind

Instructions for AI agents working in this repository.

## Start here

[`README.md`](./README.md) for what shipmind is, and
[`docs/DESIGN.md`](./docs/DESIGN.md) for the design: the components, the
two profiles, the bill of materials, the test tiers and the milestones.
Record any decision that changes the design in
[`docs/decisions.md`](./docs/decisions.md) in the same PR, as a new entry.

## What this project is

A **distribution**: it pins the go-steer platform's components
(`versions.yaml`), deploys them together and tests them end to end. It
ships the `shipmind` CLI and, from Milestone 1, the agent image and the
deployment manifests. It holds no component source code.

## Hard rules (violations are bugs)

- **No component source here.** A fix to mast, lookout, switchboard,
  mast-web, core-agent or substrate-scope lands in that repository; a fix
  to ax or Substrate lands in the fork. shipmind moves its pin.
- **Pins don't move on their own.** A version is a release tag (`vX.Y.Z`)
  or a full commit SHA, never a branch or an abbreviated SHA; an image is
  pinned by digest, never by tag alone. `shipmind check` enforces both.
- **Nothing environment-specific in this repository.** Project IDs,
  domains, registry paths and cluster names belong in the private overlay
  (`go-steer/shipmind-agent-substrate`). This repository is public.
- **No secrets anywhere.** Not in this repository and not in the overlay.
  Keys and tokens live in Secret Manager or Kubernetes Secrets; manifests
  refer to them.
- **Generated stays generated.** `deploy/vendor/` is written by
  `shipmind vendor` from pinned component versions; don't edit it by hand
  (`shipmind check` fails if you do). Patch in a profile instead.
- **Long runs don't run on a workstation.** End-to-end scenarios and
  evals run in CI or as Jobs on a cluster.

## Build & test

```bash
dev/tools/ci                # full local presubmit sweep, fast-fail order
dev/ci/presubmits/build     # go build ./...
dev/ci/presubmits/test-unit # go test -race ./...
dev/ci/presubmits/vet       # go vet ./...
dev/ci/presubmits/lint-go   # golangci-lint (auto-installs the pinned version)
dev/ci/presubmits/verify-go-format  # gofmt + goimports (fix: dev/tools/fix-go-format)
dev/ci/presubmits/verify-mod-tidy   # go.mod/go.sum are tidy
dev/ci/presubmits/verify-versions   # versions.yaml pins and deploy/vendor are sound
dev/ci/presubmits/verify-vendor-fresh  # re-vendor from the network, fail on any diff
dev/ci/presubmits/verify-coverage   # per-package floors (dev/coverage-floors.txt)
dev/ci/presubmits/verify-vuln       # govulncheck
dev/ci/presubmits/verify-no-agent-attribution  # no assistant attribution
```

The default test run needs no network, no cluster and no credentials.
`verify-vendor-fresh` and `verify-vuln` need the network.

## Conventions

- Mirror `steward` / `core-agent` conventions (package layout, `dev/`
  tooling); a maintainer of one go-steer repo should recognize the others.
- **License headers everywhere.** The Apache 2.0 boilerplate attributed to
  Google LLC sits atop every Go / shell / YAML source file. Markdown carries
  no header. `goheader` enforces it on `.go`; run
  `dev/tools/add-license-headers` for the rest.
- **Conventional Commits**, small self-contained commits, bodies explaining
  *why* + the verification done. **DCO sign-off** (`git commit -s`).
- **No `Co-Authored-By` trailer and no assistant attribution** — in commits,
  PR titles/bodies, or any committed/published artifact. Author under your
  own name. `verify-no-agent-attribution` and the `agent-attribution`
  workflow enforce it. See [`CONTRIBUTING.md`](./CONTRIBUTING.md).
- **Tests before merging.** Every new package ships unit tests; a bug fix
  ships a regression test. A new package also needs a floor in
  `dev/coverage-floors.txt`.

## How we develop

Single long-lived branch `main`; short-lived `feat/…` `fix/…` `chore/…`
`docs/…` branches → PR → merge once the required CI checks are green.
Rebase, don't merge; `--force-with-lease` on your own branch is fine, never
force-push `main`. Full contributor flow + DCO in
[`CONTRIBUTING.md`](./CONTRIBUTING.md).

- **Run presubmits before every push.** `dev/tools/ci` runs the same scripts
  CI runs (`dev/ci/presubmits/*`).
- **Adversarial-review gate before every PR touching Go code.** Run a skeptic
  subagent over the staged diff (correctness, races, API misuse — verified
  against real dependency source, not memory), fix or pin every finding, and
  record the outcome in the PR body under an `## Adversarial review` heading.
  For bug fixes, also verify the new regression test FAILS on the pre-fix
  code. Enforced by the `review-gate` required CI check (docs-only and bot
  PRs exempt). `dev/claude/settings-review-gate.json` is an opt-in local
  hook.
- **Admin merge protocol.** `gh pr merge <N> --admin --squash` once CI is
  green. Not a way to skip review: the adversarial gate plus green CI are
  the review on this single-maintainer repo.
- **`[Unreleased]` grows on every merged PR.** Any user-visible change adds a
  bullet under `## [Unreleased]` in `CHANGELOG.md`. A pin bump is
  user-visible.
- **Harness settings are not committed.** `.claude/` and a personal
  `CLAUDE.md` are git-ignored; the only checked-in Claude Code config is the
  opt-in sample under `dev/claude/`.

## Current state

**Milestone 1, mostly moved in.** `versions.yaml` pins what the
agent-substrate demo cluster runs. `deploy/profiles/substrate` renders
ax, lookout, the token refresher and substrate-scope; the agent's ax
objects render from `agents/sre-triage/ax`; `shipmind status` against
the cluster's overlay (the private go-steer/shipmind-agent-substrate)
reports no drift. Still to come: Substrate's own install (#4), the agent
image (#6), publishing images (#10), the nightly scenario (#12), and the
cutover (#13). Never run `shipmind up` against agent-substrate outside
the cutover runbook.
