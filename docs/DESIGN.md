# shipmind design

<!-- design-baseline: SM-1, SM-3, SM-4, SM-5, SM-6 -->

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
- **The images** that combine components (the agent image is core-agent,
  the ax task runner, lookout, leeway and the agent bundles in one image).
- **Deployment**: each component's manifests, vendored at its pinned
  version and checked in, composed into profiles and environments.
- **End-to-end tests**: scenarios that break something real and check
  what the platform does about it.
- **Platform documentation**: this design, the decision log, upgrade and
  cutover runbooks.

It does **not** own component source code. A fix to core-agent lands in
core-agent; a fix to the ax fork lands in the fork; shipmind moves its
pin. Each component keeps its own release cadence, unit tests, kind tests
and docs site. A monorepo was considered and rejected (see
[decisions.md](decisions.md)).

## Components

| Component | Repository | Role in the platform |
|---|---|---|
| core-agent | go-steer/core-agent | Interactive and autonomous agent harness (ADK). Runs both chat threads and incident triage: specialists, budgets, approval policy, durable sessions. |
| mast-web | go-steer/mast-web | Web UI over core-agent's attach surface: live sessions, approvals. |
| switchboard | go-steer/switchboard | Chat integration (Slack, Google Chat): digests, approvals, and, with ax, starting agents per thread. |
| k8s-lookout | go-steer/k8s-lookout | Kubernetes event detection and diagnostics. Opens an incident and hands it to an agent. |
| ax | mastersingh24/ax (fork of google/ax) | Task orchestrator: one task per agent, run as an Agent Substrate actor, suspended when idle. |
| Agent Substrate | agent-substrate/substrate | gVisor sandboxes with snapshot, suspend and wake; per-actor egress policy and credential injection. Upstream releases since v1.0; v0.5 ran from a fork. |
| substrate-scope | go-steer/substrate-scope | 3D view of every agent running or suspended on Substrate. |
| core-models | go-steer/core-models | Shared model-provider layer. A library inside core-agent, not deployed on its own, so it is not pinned here. |

## Profiles

A profile is one way of running agents. Both profiles share lookout, the
agent bundles, switchboard and mast-web; they differ in what an agent is.

### substrate (first) {#SM-1}

An agent is an ax task: one Substrate actor per incident or per Chat
thread, suspended when idle and woken by the next request.

```
k8s-lookout ──(ax sink: one task per incident)──▶ ax-server ──▶ Substrate actor
                                                                 │ ax runner
                                                                 │ core-agent (agent recipe)
                                                                 ▼
GKE APIs (*.gke.goog, container.googleapis.com) / Vertex ◀── egress gateway (injected Google token)
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

### standalone (second) {#SM-2}

An agent is a Kubernetes Deployment: lookout delivers incidents to
core-agent over `POST /inject`, switchboard relays digests and
approvals, mast-web attaches to sessions. No Substrate, so it runs on
kind, which is what makes it the profile for fast PR tests.

## Repository layout

The layout shipmind grows into. Directories appear with the milestone that
fills them; none is created empty.

```
versions.yaml            the bill of materials
agents/
  sre-triage/            native core-agent recipes (gke-triage-f, gke-chat), ax templates
images/
  agent/                 core-agent + ax runner + lookout + leeway + agent recipes (#48)
  console/               the admin console (shipmind console -in-cluster, #128)
  e2e-runner/            shipmind + ax CLI + kubectl + scenarios
  token-refresher/
deploy/
  vendor/<component>/    manifests at the pinned version, one kustomize base each, and other listed files (generated)
  profiles/substrate/    one kustomization per component, patched for the profile
  profiles/standalone/
  envs/kind/
cmd/shipmind/            the CLI
internal/                the CLI's packages (bom, env, deploy, scenario, console, ...)
e2e/
  scenarios/             break manifest + expected outcome, one directory each
docs/
  DESIGN.md              this design
  console-design.md      admin console technical design (#128)
  decisions.md           decision log
```

Environment-specific values (project IDs, domains, registry paths, the
agent-substrate cluster's overlay) live in a private companion repository,
`go-steer/shipmind-deploy`, the same split as supercargo and
supercargo-nstech. It holds kustomize overlays that reference this
repository's profiles at a full commit SHA, so moving the overlay to a new
shipmind is a reviewed commit there, as moving a component pin is here. Nothing in
either repository is a secret; keys and tokens stay in Secret Manager and
Kubernetes Secrets.

## The bill of materials {#SM-3}

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

## Deployment mechanics {#SM-4}

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

**switchboard.** `deploy/switchboard/slack` runs switchboard's Slack
gateway in front of the SRE triage agent as an ax agent: each Slack
thread gets an agent task of its own, started from the same task template
lookout uses, suspended when idle, woken by the next message and deleted
once the thread is dormant, reached through Substrate's router over mTLS.
A thread's task runs the conversation recipe, `gke-chat`, not lookout's
`gke-triage-f`: a thread is one long-lived session, so `gke-chat` caps
work per turn (calls, wall clock) with one cost ceiling per thread,
where `gke-triage-f` caps calls per incident. It also answers questions
rather than closing an incident, and doesn't pause for approval after
every finding; a change to the cluster still parks at the write gate.
An environment opts in by referencing it, as with the nightly scenario.
When the environment also sets `agent.vcs` (with `agent.chatImage`), `up`
layers `chat-config.json` onto `triage` so the single general-purpose
Slack agent delegates GitOps repository edits and pull requests to
`propose-change` (using core-agent's pure-Go `vcs_*` tools and Substrate
egress credential injection from `ate-credentials/<secret>`, with Slack
approval prompts on `vcs_publish` and `vcs_propose` by default, or
allowlisted when `agent.vcs.chatAutoApprove: true`); setting
`agent.vcs.incidentAutoPR: true` equips lookout's autonomous incident
tasks with `propose-change` as well. When
`slack.alertChannel` is set, `up` turns on switchboard's `:8080` outbound
HTTP ingress (generating `ate-credentials/switchboard-ingress` once and
mirroring it into `switchboard/switchboard-agent`) so incident tasks post
their diagnosis and pull request links to Slack through Substrate's egress
gateway. switchboard takes no variables in its configuration, so `up`
renders the configuration and the chat task template into one Secret,
`switchboard/switchboard-agent`, as it does lookout's, and switchboard is
a startup reader of it.

**Secrets from Secret Manager.** Values no one should handle, such as a
Slack app's tokens, stay in Secret Manager. An environment's
`shipmind.yaml` names them (`secrets`: a Kubernetes Secret, each key's
Secret Manager secret, and the Deployments that read it at startup); `up`
copies the latest versions in, and `status` compares them key by key.
Neither prints a value, and a failed read reports the secret's name, not
gcloud's output.

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
Workspace, `agent.incidentImage` and optional `agent.chatImage`, the
cluster table) and the changes `status` should expect.

**The agent's ax objects.** Two pieces of an environment are not
Kubernetes manifests. The ax task template lookout stamps every incident
task from carries the agent's attach bearer token (`CORE_AGENT_ATTACH_TOKEN`)
as plain env (ax has no secret references for env), so it lives in a
Secret rendered at deploy time, never in a repository. The tokens live
in their own Secret (`lookout-ax/agent-tokens`, retaining both `inject`
and `attach` keys untouched on existing clusters), generated once on a
fresh cluster and never rotated by a deploy because every existing task
has the attach token baked in; if `agent-tokens` is missing alongside a
live task template, `status` and `up` refuse rather than rotating it.
ax's Postgres password is the same kind of value: a Secret
(`ax-system/ax-postgres`) that `up` generates once, when the render runs
Postgres, and never rotates, because Postgres keeps the password it
first booted with; `status` reports it if it is missing. The Workspace (`AGENTS.md`: the cluster table, repeat rule and optional
GitOps / Slack alert instructions, plus `chat-config.json` and/or
`incident-config.json` when `agent.vcs` or `slack.alertChannel` is set) is
an ax object, applied with the ax CLI. Both are rendered from templates in
`agents/sre-triage/ax/`.

**status, up, down.** `shipmind status <env-dir>` renders everything and
compares it with the cluster, read only: `kubectl diff` for the
Kubernetes objects, a by-value comparison for the task template Secret
and the Workspace. Each difference is either expected (listed in
`shipmind.yaml` with its reason) or drift; it exits 3 on drift, so "no
drift" is a script-checkable gate. `shipmind up` applies the render,
stores the tokens (generated once, never rotated), applies the task template
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

## Admin console {#SM-9}

`shipmind` ships a first-party Admin Console in-tree (`shipmind console`
CLI subcommand and `images/console`, published to
`ghcr.io/go-steer/shipmind/console` and pinned by digest in
`versions.yaml`), deployed in `shipmind-system` by both the `standalone`
and `substrate` profiles (#128). Full technical specification:
[`console-design.md`](./console-design.md).

Four rules govern the console so external adopters get the same control
plane on Day 0 (`kind`, bare-metal Kubernetes, or GKE) and in multi-user
production:

1. **Both profiles, no GCP lock-in.** Talks to `core-agent`,
   `k8s-lookout`, `switchboard` and a `1Gi` SQLite PVC (`console.db`) for
   posture ledgers, scenario scorecards and action audit logs, with Cloud
   Storage archival (`console.archive`) optional. Under `substrate`, it
   reads `ax` tasks and Substrate actor/worker state without ever waking a
   suspended actor unless an admin explicitly confirms (`409` wake guard),
   and deep-links to `substrate-scope`. Both profiles embed a pinned,
   vendored `mast-web` under `/mast-web/` for live session transcripts,
   chat and permission prompts.
2. **In-cluster release metadata (`shipmind-system/shipmind-release`).**
   `shipmind up` stamps (and `status` checks) a sanitized, non-secret
   `ConfigMap` holding the effective `shipmind.yaml`, active
   `versions.yaml` BOM, `shipmind` commit SHA and rendered object digest
   inventory, so the in-cluster console knows its drift baseline without
   the deployer's workstation or private Git repository.
3. **Portable authentication (`go-steer/purser`).** Defaults to loopback
   (`shipmind console <env-dir>`) and generated bearer-token auth
   (`shipmind-system/shipmind-console-auth`) for zero-setup Day-0 access,
   with pluggable `purser` modes (`oidc`, `proxy-header`, `iap-jwt`)
   forwarding verified operator identity (`X-Asserted-Caller`) on session
   attach, approvals and day-2 actions.
4. **Drift-free three-tier configuration write path.** Declarative
   `shipmind.yaml` edits never patch cluster objects behind `shipmind
   status`: local mode validates and runs `shipmind up -dry-run` / `up`,
   in-cluster without VCS exports a validated diff and `shipmind up`
   command, and in-cluster with `console.overlayRepo` opens a reviewed
   pull/merge request through `go-steer/core-vcs`. Ephemeral day-2 actions
   (test alert, on-demand posture audit, write-gate approval, `lookout`
   ack/severity override, `ax` task wake/delete) run live with caller
   attribution.

It presents five panes: (1) Environment, BOM & Integration Health
(surfacing `lookout` skipped sources and `switchboard` `:8080` delivery
errors directly), (2) Posture & Scheduled Audits (#127), (3) Incidents,
Watchboard & Chat Activity, (4) Scenario & Eval Scorecard (#93), and (5)
Config Studio (`shipmind.yaml`).

## Testing

| Tier | When | Where | What it proves |
|---|---|---|---|
| Unit | every PR | CI | shipmind's own Go code: pin rules, rendering, scenario scoring. |
| Wiring | every PR (from M2) | kind, standalone profile | The components connect: a lookout event opens a session, switchboard posts (to a fake Slack), mast-web sees the session. Asserts plumbing, not diagnosis quality. |
| Scenarios | nightly (from M1) | GKE, substrate profile, real model | Each scenario breaks a workload, waits for the platform's answer and scores it: the right diagnosis, the cost, idle suspend afterwards, wake without a restart. |
| Canary | on demand, before an agent image's pin moves | GKE, a running environment, real model | A candidate agent image answers a chat scenario as a throwaway task, with nothing the environment deploys changed: its tools reach the cluster through the egress gateway. |
| Release | before a tag | the nightly result for that commit | The pinned set works together. |

### How a scenario run is graded {#SM-5}

A scenario (`e2e/scenarios/<name>/scenario.yaml`) states what a correct
handling looks like, and every run is checked against it (epic #93):

- **The answer** (`expect.mentions`): case-insensitive regular expressions
  the final answer must all match.
- **The path** (`expect.trajectory`, #89): tools it must call (`a|b` for
  either), tools it must never call, and bounds on tool calls and model
  calls, checked against the main agent's tool calls for the answer (from
  its event stream) and the agent's usage. Names must be in
  `e2e/scenarios/tools.yaml`, the bundles' tool catalog, so a typo can't
  quietly disable a guard, and match a call as named or behind one MCP
  server prefix. A failed check fails the run like a missing mention, and
  the result lists the calls. core-agent streams a specialist's calls in a
  session of its own, so a profile that delegates needs them merged before
  it gets a trajectory; the incident profile triages in its main agent.
- **The lifecycle**: the incident opens, the task suspends once the workload
  is gone, and wakes with the session in memory. A scenario with a `drain`
  block (#105, `e2e/scenarios/worker-drain`) suspends it a harder way: with
  the workload gone and the task still awake, the runner deletes the worker
  pod hosting its actor (default grace period, as a node drain does), and
  the task must suspend with ax's `AutoSuspended` reason `WorkerDraining`
  within `drain.within`, then wake with its session. A revert after the
  worker is lost (`RevertedAfterWorkerLoss`) keeps the task but not its
  session, so it fails. It passes on ax's condition (reason, and written
  after the deletion), not on the phase at a poll, since lookout's resolved
  signal can wake the task again in between. Deleting a worker pod drains
  every actor on it, as a node drain does, so every other task that was on
  the worker (in the runner's atespace) must come through with its state
  safe: suspended (for the drain, or idle at the same moment), resumed on
  another worker before a poll saw it suspended, or deleted. Any of ax's
  reverts (`RevertedAfterWorkerLoss`, `RevertedAfterFailedSuspend`),
  Failed, or resumed but still on the drained worker fails the run; a task
  idle-suspended at the same moment is watched to the end of the window. The right to list and delete pods in
  `ax-workers` is opt-in per environment (`deploy/e2e/substrate/drain`,
  which also holds its nightly at 04:17 UTC), taken only once that
  environment's ax-server suspends on drain. Dev and stage run the nightly;
  prod keeps it suspended and runs it by hand at each rollout, since it
  suspends whatever real tasks share the worker.
- **The judge** (`expect.diagnosis`, #90): with `-judge MODEL`, a model on
  Vertex AI grades the settled answer against the scenario's ground-truth
  diagnosis with core-agent's response-quality rubric (a 1-5 score; whether
  the diagnosis is correct, specific and actionable; the reasoning), copied
  into `internal/judge` rather than imported so a scenario's scores stay
  comparable over time. The grade is recorded in the result, never a
  failure, until there is a baseline to gate on; a judge that can't be set
  up or a call that fails is recorded in place of the grade (or, without a
  settled answer to grade, only logged), and the run goes on. The judge is
  Claude, a different model family from the agent's Gemini, so no model
  grades its own answers; the project comes from the metadata server on the
  cluster. `shipmind scenario judge` grades stored answers (an archived
  `result.json`, or a hand-written answer) the same way, skipping runs
  without a settled answer as a run does: it is how a judge is calibrated (`e2e/calibration/`) and the start of re-grading (#91).

With `-archive`, the run's record (event stream, usage, result, metadata)
goes to `gs://shipmind-scenario-runs/<env>/<scenario>/<date>/<workload>/`,
`meta.json` last (#88). Next: a re-grade command that applies today's
checks to archived runs and reports per agent version and model (#91). The
trajectory check and the judge are each one function used by a run and by
a re-grade alike.

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
requires its session to still be in the agent's memory with its history, and
deletes the task. The result is one JSON object; the exit status says
pass or fail. It runs from a workstation (a kube context and a
port-forwarded router) or inside the cluster (`-in-cluster`, the
`e2e-runner` image). `deploy/e2e/substrate` runs each scenario nightly
as its own CronJob (labelled `shipmind.dev/nightly: scenario`, so an
overlay patches them all with one target), with just the permissions it
needs (Deployments and ConfigMaps in the scenario's namespace, the
agent's attach token); an environment opts in by referencing it at the
same shipmind commit as its profile. With `-archive`, each run's record
(the session's event stream, usage, result and metadata) is written to
Cloud Storage once the run is decided, so it can be inspected, compared
and re-graded later.

Substrate v1.0's router accepts only mTLS clients whose SPIFFE ID it
allows. With `-router-cert` (a podCertificate projection's credential
bundle, from the podidentity signer) and `-router-trust` (a
clusterTrustBundle projection of the servicedns signer), the runner
dials the router over https, presents the pod's certificate and
verifies the router as `atenet-router.ate-system.svc`, re-reading both
files as the kubelet rotates them (go-steer/purser's PKI client
profile). Without them it speaks plain HTTP, as v0.5's router expects.

### Canary runs of a candidate agent image {#SM-10}

A new agent image is unproven until it has run on a cluster: the image
build boots it with no network, and the nightly scenarios run only what
an environment has already deployed. A canary run closes that gap
without upgrading an environment. It answers one question: does this
image, in the task shape this checkout renders, work behind this
environment's egress gateway and against its clusters?

**Chat scenarios.** A scenario with a `chat:` block breaks nothing.
`shipmind scenario run` is handed an ax Task (and the Workspace it
mounts), applies both, opens a session through Substrate's router, sends
the scenario's question about one named cluster, waits for a settled
answer and deletes both. It grades the answer on what it must and must
not say, on the tools called by the main agent and by the scenario's
specialists together (core-agent records a specialist's calls apart from
the session's), and on those calls' results. A tool that errored and
never succeeded fails the run; one that errored and then succeeded is
noted. A scenario can also name patterns a tool's successful results
must not contain (`expect.results`), for a tool that reports a cluster
it could not reach inside a result that is not an error. The
specialists' turns are archived with the session's.
`e2e/scenarios/posture-audit` asks `gke-chat` for a posture review and
requires lookout's audit and leeway's report to have been called, to
have succeeded, and leeway's report to have reached the cluster (#127).

**Launching one.** `shipmind scenario canary -image <ref> [-archive gs://bucket/prefix] <environment>
<scenario>` renders the environment's chat task and Workspace from the
checkout it is run in, with the candidate image and under a name of
their own, puts them in a Secret, and starts a one-off Job in
`shipmind-e2e` that runs the scenario against them. The Job is cut from
the nightly CronJob in the same checkout, so it has the nightly's service
account and router identity and needs no permissions of its own, and it
inherits the environment overlay's `-archive` target (overridable via
`-archive`) so canary runs archive `transcript.sse`, `usage.json`,
`result.json`, and `meta.json` (plus any specialist event logs). Once
the Job exists it owns the Secret, and both go when the Job's TTL
expires; a Job the cluster refuses takes its Secret with it.

What a canary does not touch is the point: switchboard's task template,
the Workspace its threads mount, lookout's incident template and the
nightly CronJobs are as the environment deployed them. An agent image
reaches an environment's threads only by its `agent.chatImage` moving,
as before; the canary is the check to run first.

It has limits. The run needs the nightly e2e component deployed (its
namespace and service account), an `e2e-runner` image built from a commit
that has the scenario, and the candidate agent image already published
(CI publishes one for every main commit that touches its inputs). The
Job is the base nightly pod, carrying over only the overlay's `-archive`
target rather than arbitrary patches to that CronJob. A run whose pod is
killed before it cleans up leaves its task and Workspace in the
atespace, and nothing sweeps them; the command prints how to delete
them. Result checks read what core-agent recorded, which is the tool's
output as the model saw it: whole when small, pruned when large (long
arrays keep their first and last ten entries), and a result summarized
by a model can't be checked, so it fails the run. And a chat scenario
plants no findings, so it proves the path to the cluster, not the
quality of the answer.

## Milestones

### Milestone 1: agent-substrate deploys from shipmind {#SM-6}

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

### Milestone 2: standalone profile and PR wiring tests {#SM-7}

The standalone profile on kind and the wiring tier on every PR, running
core-agent. Retire the cross-repository checks this replaces
(core-agent's lookout pin check and gke-troubleshoot recipe e2e) once
shipmind covers them.

### Milestone 3: switchboard starts agents {#SM-8}

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
