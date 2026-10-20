# shipmind design

shipmind puts the go-steer agent platform together: one repository that
pins every component, deploys them as one system and tests them end to
end. The first agent it ships is an SRE agent for Kubernetes, but the
platform underneath is a general agent platform, and the repository is
laid out for that.

## Why a separate repository

Before shipmind, the assembled platform existed in places no component
repository owned:

- The live demo cluster (agent-substrate) was defined by a local research
  directory that was not under version control: the ax-server manifests,
  the lookout ax-sink deployment and task templates, the ax Workspace, the
  agent image Dockerfile, the token refresher, the egress policies and the
  demo scripts.
- The Substrate install lived on a branch of a Substrate fork.
- Cross-repository checks were scattered: core-agent pinned lookout in its
  own CI, and k8s-sre-agent installed lookout and switchboard at pins of
  its own.

Integration bugs had nowhere to be caught. The 2026-10-19 outage is the
example: ax's Redis had no persistence, so a node move erased every ax
task and workspace record. No single component's tests could have seen
it; a test of the assembled system would have.

## What shipmind is, and is not

shipmind is a **distribution**. It owns:

- **The bill of materials** (`versions.yaml`): every component, the exact
  release tag or commit it is built from, and every image by digest.
- **The agents** the platform runs, as data: workload bundles, Workspace
  instructions, schemas.
- **The images** that combine components (the agent image is mast, the
  ax task runner and the agent bundles in one image).
- **Deployment**: each component's manifests, rendered at its pinned
  version and checked in, composed into profiles and environments.
- **End-to-end tests**: scenarios that break something real and check
  what the platform does about it.
- **Platform documentation**: this design, the decision log, upgrade and
  cutover runbooks.

It does **not** own component source code. A fix to mast lands in mast; a
fix to the ax fork lands in the fork; shipmind moves its pin. Each
component keeps its own release cadence, unit tests, kind tests and docs
site. A monorepo was considered and rejected (see
[decisions.md](decisions.md)).

## Components

| Component | Repository | Role in the platform |
|---|---|---|
| mast | go-steer/mast | Autonomous agent harness. Runs workload bundles headless: specialists, budgets, approval policy, durable sessions. |
| core-agent | go-steer/core-agent | Interactive and autonomous agent harness (ADK). The second harness the platform supports. |
| mast-web | go-steer/mast-web | Web UI over mast's attach surface: live sessions, approvals. |
| switchboard | go-steer/switchboard | Chat integration (Slack, Google Chat): digests, approvals, and, with ax, starting agents per thread. |
| k8s-lookout | go-steer/k8s-lookout | Kubernetes event detection and diagnostics. Opens an incident and hands it to an agent. |
| ax | mastersingh24/ax (fork of google/ax) | Task orchestrator: one task per agent, run as an Agent Substrate actor, suspended when idle. |
| Agent Substrate | mastersingh24/substrate (fork) | gVisor sandboxes with snapshot, suspend and wake; per-actor egress policy and credential injection. |
| substrate-scope | go-steer/substrate-scope | 3D view of every agent running or suspended on Substrate. |
| core-models | go-steer/core-models | Shared model-provider layer. A library inside mast and core-agent, not deployed on its own, so it is not pinned here. |

## Profiles

A profile is one way of running agents. Both profiles share lookout, the
agent bundles, switchboard and mast-web; they differ in what an agent is.

### substrate (first)

An agent is an ax task: one Substrate actor per incident or per Chat
thread, suspended when idle and woken by the next request.

```
k8s-lookout ──(ax sink: one task per incident)──▶ ax-server ──▶ Substrate actor
                                                                 │ ax runner
                                                                 │ mast serve (agent bundle)
                                                                 ▼
                  GKE MCP / Vertex ◀── egress gateway (injected Google token)
substrate-scope: watches every actor        switchboard (M3): starts tasks per thread
```

Pieces this profile owns beyond the components: the ax Workspace (its
AGENTS.md maps cluster names to GKE paths and carries the "not every
signal is new work" rule), the task template, per-task egress policies,
the token refresher that keeps a Google access token in the credential
provider's Secret, and the worker ComputeClass (C4 first, N4 fallback,
because a gVisor snapshot only restores on a host with every CPU feature
of the host where the agent first booted).

### standalone (second)

An agent is a Kubernetes Deployment: lookout delivers incidents to mast
(or core-agent) over `POST /inject`, switchboard relays digests and
approvals, mast-web attaches to sessions. No Substrate, so it runs on
kind, which is what makes it the profile for fast PR tests.

## Repository layout

The layout shipmind grows into. Directories appear with the milestone that
fills them; none is created empty.

```
versions.yaml            the bill of materials
agents/
  sre-triage/            gke-triage bundles, Workspace AGENTS.md, schemas
images/
  agent-mast/            mast + ax runner + agent bundles
  token-refresher/
deploy/
  vendor/<component>/    rendered manifests at the pinned version (generated)
  profiles/substrate/
  profiles/standalone/
  envs/kind/
cmd/shipmind/            the CLI
internal/                the CLI's packages (bom today)
e2e/
  scenarios/             break manifest + expected outcome, one directory each
docs/
```

Environment-specific values (project IDs, domains, registry paths, the
agent-substrate cluster's overlay) live in a private companion repository,
`go-steer/shipmind-agent-substrate`, the same split as supercargo and
supercargo-nstech. It holds kustomize overlays that reference this
repository's profiles at a full commit SHA, so moving the overlay to a new
shipmind is a reviewed commit there, as moving a component pin is here. Nothing in
either repository is a secret; keys and tokens stay in Secret Manager and
Kubernetes Secrets.

## The bill of materials

`versions.yaml` lists every component with its source repository, a
release tag (`vX.Y.Z`) or a full commit SHA (never a branch, and never an
abbreviated SHA, which git resolves after a branch of the same name), the
profiles that run it, and its images by digest. `shipmind check` enforces those rules, and the `verify-versions`
presubmit runs it on every PR.

Moving a pin is a pull request:

1. A component release (or a fork's deploy tag) triggers a
   `repository_dispatch` to shipmind.
2. A workflow opens a PR that updates `versions.yaml` and re-renders
   `deploy/vendor/<component>`, so the review shows exactly which
   manifests changed.
3. The PR runs the end-to-end tier for the profiles that use the
   component.

A shipmind release is a tag on a commit whose component set passed the
nightly tier. Releases are versioned on their own (`v0.1.0`, ...), not
after any component.

## Testing

| Tier | When | Where | What it proves |
|---|---|---|---|
| Unit | every PR | CI | shipmind's own Go code: pin rules, rendering, scenario scoring. |
| Wiring | every PR (from M2) | kind, standalone profile | The components connect: a lookout event opens a session, switchboard posts (to a fake Slack), mast-web sees the session. Asserts plumbing, not diagnosis quality. |
| Scenarios | nightly (from M1) | GKE, substrate profile, real model | Each scenario breaks a workload, waits for the platform's answer and scores it: the right diagnosis, the cost, idle suspend afterwards, wake without a restart. |
| Release | before a tag | the nightly result for that commit | The pinned set works together. |

Nothing long-running runs on a workstation; scenarios run in CI or as
Jobs on the cluster. The prompt A/B experiments from October (generalist
against routed specialists) become scenario runs with two agent variants.

## Milestones

### Milestone 1: agent-substrate deploys from shipmind

Done when the demo cluster's platform components are deployed from this
repository (plus its private overlay) and one nightly scenario runs.

1. Create the private overlay repository and move the cluster-specific
   values there.
2. Move the Substrate cluster install (fork branch
   `agent-substrate-cluster-v0.5.0`) into `deploy/`, keeping the fork for
   source patches only.
3. Move the ax-server deployment, including Redis persistence.
4. Move the agent image (`images/agent-mast`) and the gke-triage bundles
   (`agents/sre-triage`), and build the image in CI.
5. Move the lookout ax-sink deployment, the task template and the ax
   Workspace.
6. Move the token refresher, the credential provider policy and the
   egress policies.
7. Move substrate-scope's deployment.
8. Publish the images built from forks and go-steer sources to a public
   registry and pin them by digest in `versions.yaml`.
9. `shipmind up/down/status` for the substrate profile.
10. The first nightly scenario: crash-loop on a missing `DATABASE_URL` →
    correct diagnosis → idle suspend → wake with no restart.
11. Redeploy agent-substrate from shipmind (a scheduled cutover).

### Milestone 2: standalone profile and PR wiring tests

The standalone profile on kind, the wiring tier on every PR, and
core-agent as a second harness. Retire the cross-repository checks this
replaces (core-agent's lookout pin check and gke-troubleshoot recipe e2e)
once shipmind covers them.

### Milestone 3: switchboard starts agents

switchboard's ax backend (target switchboard v0.8): picking an agent in
Chat starts an ax task per thread or a personal agent per owner, woken
through Substrate's router. mast-web joins the substrate profile.

## Open questions

- **Rendering.** Components ship Helm (mast, lookout), kustomize (lookout,
  switchboard) and plain YAML (mast-web, substrate-scope). The working
  assumption is kustomize for composition with Helm charts rendered at
  vendor time, so `deploy/vendor` holds plain YAML; settle it in M1 issue 2.
- **Where scenarios run nightly.** On agent-substrate itself, or on a
  fresh cluster per run. A fresh cluster tests the install too, at the
  cost of N4/C4 stockouts turning into red nightlies.
- **Model credentials in CI.** The nightly tier needs Vertex access; the
  token-refresher pattern (Workload Identity, no keys) is the candidate.
