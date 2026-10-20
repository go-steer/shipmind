# Contributing to shipmind

Thanks for contributing. shipmind follows the same contributor flow as
[core-agent](https://github.com/go-steer/core-agent) and
[steward](https://github.com/go-steer/steward) — a maintainer of one repo
should recognize the others.

## Workflow

- Single long-lived branch: `main`. Work on short-lived feature branches
  (`feat/…`, `fix/…`, `chore/…`, `docs/…`) → PR against `main` → merge
  once CI's required checks are green.
- **Rebase, don't merge.** Keep feature branches rebased on `main`;
  `git push --force-with-lease` on your own branch is normal. Never
  force-push `main`.

## Commits

- **Conventional Commits** subject lines: `feat:`, `fix:`, `docs:`,
  `chore:`, `refactor:`, `test:`, `ci:`, `build:`. Bodies explain *why*
  and call out the verification done. A pin bump is
  `chore(versions): <component> <old> -> <new>`.
- **DCO sign-off** on every commit: `git commit -s` (adds a
  `Signed-off-by:` trailer certifying the [Developer Certificate of
  Origin](https://developercertificate.org/)).
- **No `Co-Authored-By` trailer, and no assistant attribution anywhere.**
  Maintainer preference: author the work under your own name. Do not add
  `Co-Authored-By:` lines, "Generated with …" footers, or any
  tool/assistant credit to commits, PR titles/bodies, or other committed
  or published artifacts.

## Before you push

- Run `dev/tools/ci` — the full presubmit sweep, the same scripts CI
  runs (`dev/ci/presubmits/*`). A green local run is the green remote
  run.
- **Adversarial-review gate** on any PR touching Go code — see
  [`AGENTS.md`](./AGENTS.md) "How we develop". Record the outcome under an
  `## Adversarial review` heading in the PR body; the `review-gate`
  required CI check enforces it.

## Moving a pin

`versions.yaml` is the product: it is the set of component versions a
shipmind release promises work together. Change it deliberately.

1. Pin a release tag (`vX.Y.Z`) or a full 40-character commit SHA in the
   component's repository, never a branch or an abbreviated SHA (git
   resolves a branch of the same name first). A fork's deploy tag goes in
   the `note`, with the commit as the version. Pin every image by digest (`<ref>@sha256:<digest>`); a tag in front of
   the digest is welcome as documentation.
2. If the reason for the pin is not obvious (a fork, a commit between
   releases, a held-back version), say so in the component's `note`.
3. Run `dev/tools/verify-versions`, and add a bullet to `CHANGELOG.md`
   naming the component and both versions.

## Cutting a release

A shipmind release is a tag on a commit whose `versions.yaml` passed the
nightly scenario tier (see "Testing" in
[`docs/DESIGN.md`](./docs/DESIGN.md)). Releases are numbered on their own,
not after any component.

1. **Land everything first.** `main` green, the latest nightly green on
   the commit you are releasing, working tree clean.
2. **Close the changelog.** Rename `## [Unreleased]` to
   `## [X.Y.Z] - YYYY-MM-DD`, open a fresh `## [Unreleased]` above it with
   *Nothing yet.*, and add the two link references at the bottom of the
   file.
3. **Merge that as its own `chore/release-vX.Y.Z` PR**, so the tag lands on
   a commit whose changelog already describes it.
4. **Tag the merge commit and push it:**
   ```bash
   git tag -a -s vX.Y.Z -m "vX.Y.Z"   # annotated + signed
   git push origin vX.Y.Z
   ```

Pre-1.0, a release that drops a component or changes how a profile is
deployed is a **minor** bump.

## Tests

Every new package ships with unit tests. A new feature without a test
is not done; a bug fix without a regression test lets the bug come back.

shipmind's code mostly says yes or no: this pin is sound, this manifest
renders, this scenario passed. A test that only asserts the happy path
passes just as well against a check that never runs, so every check needs
its negative case and an assertion that it *failed*.

Unit tests need no network, no cluster and no credentials. End-to-end
scenarios are a separate tier and never run in `go test ./...`.

## Coverage floors

`dev/tools/verify-coverage` holds every package to a floor in
[`dev/coverage-floors.txt`](./dev/coverage-floors.txt), reading the profile
`dev/tools/test-unit` writes. Per package, not one number for the module.

- **A new package needs an entry.** Without one the check fails rather than
  letting the package in unmeasured. The tool prints the line to paste.
- **Lowering a floor is allowed** — in the PR that makes it necessary, with
  a `#` comment saying why, so the reviewer sees it next to the code.
- **Raising one** is never required to merge; the tool nags when a floor has
  drifted far enough below the truth to have stopped protecting anything.

## License

By contributing you agree your contributions are licensed under the
project's [Apache 2.0](./LICENSE) license. Every Go / shell / YAML source
file carries the Apache 2.0 header attributed to Google LLC.
