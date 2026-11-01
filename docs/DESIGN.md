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
- **Deployment**: each component's manifests, vendored at its pinned
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
| Agent Substrate | agent-substrate/substrate | gVisor sandboxes with snapshot, suspend and wake; per-actor egress policy and credential injection. Upstream releases since v1.0; v0.5 ran from a fork. |
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
provider's Secret, the WorkerPool (sized so each worker holds three
agents at their limits, which Substrate books against it), and the
worker ComputeClass, which may mix CPU generations: Substrate restores a
memory snapshot only on a worker with the snapshot's CPU features, and
otherwise restores files only.

Substrate itself is installed by its own installer (`ate-setup`), per
cluster, not rendered. Two settings the profile needs have no installer
option, so `shipmind up` patches them in after the install and `status`
checks them, so an installer run that drops them shows as drift:
atelet runs only on worker nodes (`nodeSelector
ate.dev/worker-node=true`), and the router holds a request for a
resuming actor up to 60s (`--parked-request-budget=60s`; the default
5s is shorter than a cold resume). Since v1.0, the router takes only
mTLS clients whose SPIFFE ID the install allows: ax-server, lookout,
substrate-scope and the nightly scenario each present their pod's
podidentity certificate and verify the router against the servicedns
trust bundle, both projected volumes.

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
  vendor/<component>/    manifests at the pinned version, one kustomize base each, and other listed files (generated)
  profiles/substrate/    one kustomization per component, patched for the profile
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
`go-steer/shipmind-deploy`, the same split as supercargo and
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
2. A workflow opens a PR that updates `versions.yaml` and re-vendors
   `deploy/vendor/<component>` (`shipmind vendor`), so the review shows
   exactly which manifests changed.
3. The PR runs the end-to-end tier for the profiles that use the
   component.

A shipmind release is a tag on a commit whose component set passed the
nightly tier. Releases are versioned on their own (`v0.1.0`, ...), not
after any component.

## Deployment mechanics

**Vendoring.** A component that ships Kubernetes manifests lists them
under `manifests` in `versions.yaml`. `shipmind vendor` reads each one
from the component's repository at its pin and stores it as its own
kustomize base: `deploy/ax-server.yaml` in the ax fork becomes
`deploy/vendor/ax/deploy/ax-server/`, holding the file and a generated
`kustomization.yaml`. One base per manifest lets a profile take, patch or
leave out each manifest separately; two manifests that declare the same
Namespace (as ax's do) could not share a base. `VENDOR.yaml` records the
source, the version, the commit it resolved to and each file's SHA-256.
`shipmind check` verifies it offline: a pin moved without a re-vendor, or
a vendored file edited without its lock, fails. CI also re-vendors from
the network and fails on any difference, which catches what a lock in
the same tree can't: a file edited together with its lock entry, or an
upstream tag that moved.

A component's other files, ones a deployment needs that aren't
Kubernetes objects, are listed under `files` and copied as they are to
the same path under `deploy/vendor/<component>/`, with no kustomization,
locked and checked the same way. Today that is switchboard's Slack app
manifest.

**Slack apps.** Each environment gets its own Slack app, so staging and
production can share a workspace. switchboard's app manifest carries
the scopes its code needs, so it is vendored with switchboard and moves
with the pin. An environment's `shipmind.yaml` only names its app
(`slack: {app, command, description}`), and `shipmind slack-app
<env-dir>` prints switchboard's manifest under those names, from a
checkout at the environment's pin. After a switchboard bump, rerun it
for each environment and paste the result into each app's App Manifest
page; Slack shows the scope changes, and the app is reinstalled.
Creating the app and storing its tokens: [`slack.md`](./slack.md).

**Profiles.** `deploy/profiles/<profile>/<component>/` is a kustomization
over the vendored bases with the profile's patches: images pinned by
digest from `versions.yaml`, settings every deployment of the profile
needs. Tests render every profile in-process and hold them to
invariants: every image is pinned in `versions.yaml`, no remote bases, and
component-specific rules such as ax's Postgres keeping its volume.

**Environments.** An environment is an overlay directory: a
`kustomization.yaml` that references a profile at a full shipmind commit
SHA and adds its own values, and a `shipmind.yaml` with what kustomize
can't carry: the kube context (every command passes it explicitly, never
the current context), the agent's ax settings (atespace, project,
Workspace, agent image, the cluster table) and the changes `status`
should expect.

**The agent's ax objects.** Two pieces of an environment are not
Kubernetes manifests. The ax task template lookout stamps every incident
task from carries the agent's two bearer tokens as plain env (ax has no
secret references for env), so it lives in a Secret rendered at deploy
time, never in a repository. The tokens live in their own Secret
(`lookout-ax/agent-tokens`); a deployment made before shipmind has them
only in its task template, and `up` adopts them from there. Tokens are
never rotated by a deploy: every existing task has them baked in. ax's
Postgres password is the same kind of value: a Secret
(`ax-system/ax-postgres`) that `up` generates once, when the render runs
Postgres, and never rotates, because Postgres keeps the password it
first booted with; `status` reports it if it is missing. The
Workspace (AGENTS.md: the cluster table and the repeat rule) is an ax
object, applied with the ax CLI. Both are rendered from templates in
`agents/sre-triage/ax/`.

**status, up, down.** `shipmind status <env-dir>` renders everything and
compares it with the cluster, read only: `kubectl diff` for the
Kubernetes objects, a by-value comparison for the task template Secret
and the Workspace. Each difference is either expected (listed in
`shipmind.yaml` with its reason) or drift; it exits 3 on drift, so "no
drift" is a script-checkable gate. `shipmind up` applies the render,
stores the tokens (adopted or generated once), applies the task template
Secret and the Workspace, and waits for every Deployment to roll out;
`-dry-run` validates every write server-side and writes nothing. Both run
from a shipmind checkout at exactly the commit the overlay pins, with no
uncommitted changes, so what is deployed is what the pin says (`status
-any-ref` relaxes that to try unmerged changes). `shipmind down` deletes
the rendered objects, keeping Namespaces and volumes unless `-all`.

**A limit of client-side apply.** `up` applies client-side, as the
cluster's objects were first created, and `status` diffs the same way.
A field set by hand that no manifest mentions (a `kubectl patch` adding
`suspend: true`, say) survives an `up` and doesn't show as drift. Fields
whose hand-set values matter are stated in the manifests, even at their
defaults, so they are managed.

**Substrate is the exception.** Agent Substrate installs through its own
Go installer (`ate-setup`), which applies manifests from its checkout with
environment-driven settings, waits for rollouts and wires the credential
provider; it has no render mode. shipmind runs that installer at the
pinned fork commit with pre-built images, and keeps the cluster-specific
inputs (installer settings, the worker ComputeClass, the WorkerPool) in
the profile and the overlay. Source-level manifest patches stay in the
fork.

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

**Scenarios.** A scenario is a directory under `e2e/scenarios/`: a
`scenario.yaml` (the namespace, what a correct answer must mention, how
long each step may take) and a workload template. `shipmind scenario
run` applies the workload under a name unique to the run (lookout keys
an incident's ax task on the workload, so a reused name would land in an
earlier run's session, where the agent rightly answers "still failing"),
waits for lookout's new ax task, reads the agent's answer to the first
incident message through Substrate's router and scores it, removes the
workload, waits for the task to suspend, wakes it through the router and
requires its session to still be in mast's memory with its history, and
deletes the task. The result is one JSON object; the exit status says
pass or fail. It runs from a workstation (a kube context and a
port-forwarded router) or inside the cluster (`-in-cluster`, the
`e2e-runner` image). `deploy/e2e/substrate` runs it nightly as a
CronJob with just the permissions it needs (Deployments and ConfigMaps
in the scenario's namespace, the agent's attach token); an environment
opts in by referencing it at the same shipmind commit as its profile.

Substrate v1.0's router accepts only mTLS clients whose SPIFFE ID it
allows. With `-router-cert` (a podCertificate projection's credential
bundle, from the podidentity signer) and `-router-trust` (a
clusterTrustBundle projection of the servicedns signer), the runner
dials the router over https, presents the pod's certificate and
verifies the router as `atenet-router.ate-system.svc`, re-reading both
files as the kubelet rotates them (go-steer/purser's PKI client
profile). Without them it speaks plain HTTP, as v0.5's router expects.

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

switchboard's ax backend (target switchboard v0.9; designed in
go-steer/switchboard `docs/ax-backend.md`, tracked in
go-steer/switchboard#168 and #39 here): picking an agent in Chat starts an
ax task per thread or a personal agent per owner, woken through
Substrate's router. mast-web joins the substrate profile.

## Open questions

- **Where scenarios run nightly.** On agent-substrate itself, or on a
  fresh cluster per run. A fresh cluster tests the install too, at the
  cost of N4/C4 stockouts turning into red nightlies.
- **Model credentials in CI.** The nightly tier needs Vertex access; the
  token-refresher pattern (Workload Identity, no keys) is the candidate.
