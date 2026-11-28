# Changelog

All notable changes to shipmind are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and releases
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). A
release is a tested set of component versions (see "Cutting a release" in
[`CONTRIBUTING.md`](./CONTRIBUTING.md)).

## [Unreleased]

### Added

- Added the `e2e/scenarios/triage-overflow` scenario (#131): a crash-looping
  workload whose enrichment bundle fits `spec`, `delta`, and `radius` under
  `lookout`'s default 4096-byte `--enrich-cap` while overflowing `section=logs`
  into an `overflow section=logs cmd="lookout triage logs ..."` trailer; the
  missing `SETTLEMENT_WEBHOOK_URL` variable appears only in container logs, so
  the scenario passes only when the incident agent follows the overflow hint
  with `lookout_k8s_triage_logs`.
- `shipmind scenario canary` now passes `-archive` to the canary Job,
  inheriting the environment overlay's nightly CronJob `-archive` target by
  default (overridable via `-archive gs://bucket/prefix`), so canary runs
  archive `transcript.sse`, `usage.json`, `result.json`, and `meta.json`
  (#144).
- Added scheduled posture audits for the `substrate` profile (#127):
  declarative `audits` configuration in `shipmind.yaml` (`schedule`
  defaulting to daily `17 5 * * *`, `refreshWindow` defaulting to `168h`,
  optional `minConsecutive` dwell threshold, `exemptions`,
  `domainUnavailableKeys` defaulting to disabled for scale-to-zero zones,
  and `autoPR`), `deploy/audits/substrate` CronJob + PVC + RBAC + Secret
  reconciliation across `shipmind status`, `up`, and `down`, static
  `lookout` and `leeway` binaries in `images/e2e-runner`, and `shipmind
  audit run` (`internal/audit`) with a persistent dedup ledger, zero-LLM-token
  clean/muted sweeps, digest-only Slack delivery via `switchboard` `:8080`
  `/v1/messages` (seeding `@shipmind` thread context), optional `autoPR`
  GitOps pull requests via `propose-change` (`vcs_proposal_list` dedup;
  enabling `audits.autoPR: true` on Substrate v1.0+ requires admitting
  `spiffe://cluster.local/ns/shipmind-audits/sa/shipmind-audits` in
  `ATE_INGRESS_ALLOWED_SPIFFE_IDS`),
  and seeded posture scenarios (`e2e/scenarios/posture-workloads` and
  `e2e/scenarios/posture-placement`).
- Built static `lookout` (`-tags allproviders`) and `leeway` binaries from
  their pins into `images/agent`, and equipped `gke-chat` with a read-only
  `posture-audit` specialist that runs `lookout mcp --profile=audit`
  (`k8s_audit_all` plus the individual workload, hardening, RBAC,
  NetworkPolicy, namespace, cluster, upgrade, `ComputeClass` and AI checks)
  and `leeway mcp` (live placement skew, `ComputeClass` priority-rank
  occupancy, and deterministic remediation) directly, with Substrate's
  injected Google auth and `*.<location>.gke.goog` egress for the
  environment's clusters (#127).
- Added canary runs (#127): a chat scenario kind (`chat:` in
  `scenario.yaml`, `e2e/scenarios/posture-audit`) and `shipmind scenario
  canary -image <ref> <environment> <scenario>`, which runs a candidate
  agent image as one throwaway task in a running environment, from a
  one-off Job, and changes nothing the environment deploys. A chat run
  fails when a tool errors and never succeeds, or when a successful
  result matches a pattern the scenario rules out (`expect.results`: a
  report that reached no cluster), so it shows whether the agent's tools
  reach the cluster before the image's pin moves.
- Bumped `k8s-lookout` from `v0.44.0` to `v0.45.0` and `leeway` from
  `v0.1.7` to `v0.1.8`. lookout `v0.45.0` removes its in-tree leeway
  sources, flags, CRDs and signal kinds (the vendored watcher ClusterRole
  loses its LeewayPolicy rule; the substrate profile used none of them) and
  runs `audit computeclass` and `audit ai` inside `audit all`. It also
  debounces `FailedScheduling` alerts, on by default here: an unschedulable
  pod now opens an incident after 5 events over 2 minutes, or up to 15
  minutes after a triggered scale-up, instead of on the first event. Both
  releases can now serve MCP from inside an agent task with injected Google
  auth and no wrapper (`lookout mcp`, `leeway mcp`), which the `gke-chat`
  posture work builds on (#127).
- Pinned `leeway` `v0.1.7` (`ghcr.io/go-steer/leeway:v0.1.7`) in
  `versions.yaml` for the substrate profile, ahead of the scheduled posture
  sweeps. Nothing renders it yet, and no `LeewayPolicy` or
  `ClusterLeewayPolicy` CRDs are vendored or installed on target clusters
  (#127).
- Bumped `switchboard` to `v0.11.0` (live subagent inner-tool progress for
  `_fallback`, `propose-change`, and synchronously awaited `spawn_agent` in
  `status` and `stream` modes across Slack and Google Chat, plus tool-activity
  delivery on agent frames that combine assistant narration text with
  `functionCall` parts).
- `agent.vcs.chatAutoApprove` option in `shipmind.yaml` (default `false`): when
  `true`, allowlists `vcs_publish` and `vcs_propose` in the Workspace's
  `chat-config.json` so the Slack chat agent pushes `agent/*` branches and opens
  pull requests directly without pausing on Slack approval buttons.
- Bumped `switchboard` to `v0.10.0` (unbound outbound-ingress thread context
  seeding for the first inbound reply without Slack/Chat history scopes,
  completing the `incident -> Slack alert -> human reply -> open PR` flow when
  `agent.vcs.incidentAutoPR` is `false`; built on Go 1.26.11 for
  `GO-2026-6859`).
- Unified general-purpose Slack agent and autonomous lookout incident
  remediation flows: `agent.vcs` (`secret` in `ate-credentials` plus
  `repositories`, with Substrate egress credential injection: `Bearer` on
  `api.github.com` and `Basic` on `github.com` so the GitHub token never enters
  the sandbox) equips the single `triage` Slack agent with a `propose-change`
  GitOps specialist (`chat-config.json` in the ax Workspace, pure-Go `vcs_*`
  tools, `read_file`/`edit_file`/`write_file`/`grep`, and Slack approval prompts
  on `vcs_publish` and `vcs_propose`, superseding #116's separate `propose`
  agent); `agent.vcs.incidentAutoPR` equips autonomous `lookout-ax` incidents
  with `propose-change` (`incident-config.json`, open-PR deduplication via
  `vcs_proposal_list`, fast-exit on unmanaged workloads, `shipmind.dev/scenario`
  probe exemption, and unknown-secret guards); and `slack.alertChannel` enables
  switchboard's `:8080` outbound HTTP ingress (`Service` + `NetworkPolicy`
  scoped to `ate-system` `atenet-egress`, `ate-credentials/switchboard-ingress`
  generated once by `shipmind up`) so incident tasks post diagnoses and pull
  request links to Slack via core-agent's `alert` tool (`template:
  "switchboard"`).
- Bumped `ax` and `ax-runner` to `db1c1def07ec0c76262c9102a558e43ffe19da5b`
  (`substrate-v1-drain-db1c1de`): plain-HTTP egress rules (`http://<host>` on
  `EgressRule.hosts`) for in-cluster HTTP gateways like switchboard's outbound
  ingress, while keeping `ax.proto` wire-compatible with `k8s-lookout`
  `v0.43.1`.
- worker-drain counts any of ax's reverts (`RevertedAfterWorkerLoss`,
  `RevertedAfterFailedSuspend`, any later `Reverted*`) as a revert: the
  task survives but loses the work since its last snapshot, so it fails
  the run, for the scenario's own task and for a neighbour alike.
- `agent.idleSuspendAfter` in an environment's `shipmind.yaml`: how long a
  task may go without requests before ax suspends it, rendered into both
  task templates (default 10m, from 1m to 20m, so the scenarios' 25m
  suspend limit stays reachable). Shorter frees worker slots sooner at the
  cost of more suspend/resume cycles; staging uses 3m, after it ran out of
  slots (12 per 4 workers) during scenario runs. It reaches tasks created
  after the change; existing ones keep their timeout.
- The worker-drain scenario runs nightly at 04:17 UTC (#105) in every
  environment that opts into `deploy/e2e/substrate/drain`, which now carries
  its CronJob beside its RBAC (dev and stage; prod keeps it suspended for
  runs by hand). It passed on stage on 2026-11-14 with ax
  d03b3e0: suspended for the drain 11s after its worker pod was deleted, and
  woke with its session.
- A `worker-drain` scenario (#105): missing-env's incident, then the worker
  pod hosting the awake task's actor is deleted, as a node drain does, and
  the task must suspend with reason `WorkerDraining` and wake with its
  session in memory. It fails until ax-server suspends a draining worker's
  actors (mastersingh24/ax#1). A worker shared with other tasks is drained
  anyway, as a real node drain is, and every task that was on it (in the
  runner's atespace) must come through with its state safe (suspended,
  resumed on another worker, or deleted; not reverted, Failed or left on
  the drained worker); the result lists how each fared. The right to list and delete pods in `ax-workers` is opt-in:
  an overlay adds `deploy/e2e/substrate/drain`.
- An answer judge (#90): each scenario states its ground truth
  (`expect.diagnosis`), and `shipmind scenario run -judge MODEL` has a
  Claude model on Vertex AI grade the settled answer against it with
  core-agent's response-quality rubric: a 1-5 score, whether the diagnosis
  is correct, specific and actionable, the reasoning, tokens and cost, in
  `result.json` (`judge`). Reported, never a failure; a judge that can't be
  set up or fails is recorded and the run goes on. On the cluster the
  project comes from the metadata server. `shipmind scenario judge` grades
  stored answers the same way: archived `result.json` files, and the
  hand-written answers with known flaws in `e2e/calibration/`.
- Trajectory checks in `scenario.yaml` (`expect.trajectory`, #89): tools
  the agent must call (`a|b` for either) and must never call, and bounds on
  tool calls and model calls, checked against the session's tool calls and
  usage. A failed check fails the run; the result lists the calls made
  (`trajectory.tool_calls`), up to the answer, even when it never settles.
  Names must be in `e2e/scenarios/tools.yaml`, the bundles' tool catalog.
  Both scenarios require a read of the workload, forbid every write tool,
  and allow at most 10 tool and 10 model calls.
- `shipmind scenario run -archive gs://bucket/prefix` stores each run's
  record once it is decided, failed runs included: the session's full
  event stream (`transcript.sse`: every message and tool call with its
  arguments and result), its usage (`usage.json`), the result
  (`result.json`) and what ran (`meta.json`), under
  `<prefix>/<scenario>/<date>/<workload>/`. It writes over Cloud Storage's
  JSON API with the pod's workload-identity token and never replaces an
  object; a failed save is logged and doesn't fail the run. The record is
  what trajectory checks, an answer judge and re-grading will read (#48).
- A second nightly scenario, `oom-killed` (CronJob at 03:47 UTC): a
  container allocates 200 MiB under a 64Mi memory limit and is
  OOMKilled. The answer must name the OOM kill, the 64Mi limit and the
  memory limit to raise. Both nightly CronJobs carry the label
  `shipmind.dev/nightly: scenario`, so an overlay can patch them with one
  target (the atespace argument).
- The nightly scenario records the session's model usage once the
  answer settles (`GET /sessions/{id}/usage`): model calls, input tokens
  and the cached share, output and thinking tokens, and each call with
  its time and cost, in its JSON result under `usage` and as a log line,
  with the agent that answered. Output excludes thinking on both agents
  (mast counts it inside output; the scenario takes it out), and
  `superseded` marks a run whose totals may include calls for a later
  message. A failed read is logged and doesn't fail the run.
- switchboard v0.8.0 pinned in `versions.yaml` and its base and
  durable-state PVC vendored, ahead of two instances (Slack and Google
  Chat) in a profile.

- `shipmind slack-app <env-dir>` prints an environment's Slack app
  manifest: switchboard's, vendored at its pin, under the names in the
  environment's new `slack` block. `versions.yaml` can now vendor
  components' non-Kubernetes files (`files`), checked like manifests.

- `shipmind scenario run -router-cert -router-trust`: router mTLS for
  Substrate v1.0, which accepts only mTLS clients. The runner presents
  the pod's projected podidentity certificate, verifies the router
  against the projected servicedns trust bundle, and reloads both as
  they rotate (go-steer/purser v0.5.0). Without the flags nothing
  changes, as v0.5 clusters need (#49).

- **Substrate v1.0 and ax on upstream (#49).**
  - Substrate moves to upstream v1.0.0-rc.1, with images from upstream's
    registry by digest; the fork and its republished images are no
    longer needed.
  - ax and its runner move to the fork's port, 12dbe16, on upstream ax:
    state in Postgres instead of Redis, one workspace per task, the
    fork's task fields renumbered.
  - ax-server, the agent image and the e2e-runner are rebuilt for it.
    The images workflow's dispatch inputs build them before the pin
    moves.
  - `shipmind up` generates ax's Postgres password once (Secret
    `ax-system/ax-postgres`), and `status` reports it missing.
  - `up` patches Substrate's atelet to worker nodes and the router's
    parked-request budget to 60s, which the installer can't set, and
    `status` checks both.
  - The task template binds one `workspace` and sets 500m CPU / 1Gi
    limits; the WorkerPool is `ate.dev/v1` on `ateworker-gvisor`, sized
    2 CPU / 4Gi a worker.
  - The nightly scenario reaches the router over mTLS with its pod's
    certificate.
  - k8s-lookout v0.43.0 and substrate-scope v0.1.0 (d52a8aa): the ax
    fork's v1 protos and router mTLS; lookout mounts its pod
    certificate and the trust bundle, scope's own manifest does.

- **switchboard on the substrate profile (#39).** switchboard v0.9.2
  (v0.9.0 asserted the chat user to mast, which refuses it),
  and `deploy/switchboard/slack`: its Slack gateway in front of the SRE
  triage agent as an ax agent, a task per thread, reaching the router
  over mTLS. `up` renders its configuration and the agent's task template
  into `switchboard/switchboard-agent`, and switchboard is a startup
  reader of it.
- `agents/sre-triage/gke-chat`: the agent's conversation bundle, which
  switchboard's thread tasks run. Budgets per turn (50 model calls, 10
  minutes) and one $10 ceiling per thread instead of `gke-triage-f`'s
  12-call specialist cap, which mast counts for a session's whole life
  and so ended a Slack thread after a question or two; no approval
  pause after every finding (changes still park at the write gate); a
  specialist that answers the person and follows through.
- `shipmind.yaml` `secrets`: Kubernetes Secrets whose values live in
  Secret Manager, copied in by `up` and compared by `status`, never
  printed.
- `images/agent`, the agent on core-agent (#48): core-agent
  v3.1.0-dev.3 and the ax runner, with `agents/sre-triage`'s bundles
  converted by core-agent's importer at build time and each booted once
  as a check. `agent.chatImage` in an environment runs switchboard's
  threads on it (`task-core-agent.yaml.tmpl`); lookout's incidents stay
  on mast. Setting or changing it changes switchboard's task template,
  so each Slack thread starts a fresh task, and conversation, on its
  next message.
- `agent.incidentImage`, the same for lookout's incidents: set, each
  incident runs core-agent's template with `gke-triage-f` (#48), whose
  tool calls outside the allow list now fail after 30s instead of
  waiting for an approver no incident has. `up` and `status` read the
  attach token from a core-agent template too, so the check that refuses
  to deploy over tokens the running tasks carry keeps working once the
  template has no inject token. An incident task created before the
  switch keeps the harness it started with: ax tasks are immutable, and
  lookout reuses a task when its incident reopens.
- The nightly scenario reads core-agent: it finds lookout's incident
  inside core-agent's `[Inbox]` bundle, doesn't count a background
  subagent's `[Background reports]` turn as a new message, and doesn't
  settle while a subagent is still running.

### Changed

- Converted `gke-triage-f` to a native flat `core-agent` recipe under
  `agents/sre-triage/gke-triage-f/.agents/`, removed all remaining legacy
  `mast` bundles and `core-agent config import-workload`/fold machinery from
  `images/agent/Dockerfile`, dropped the hosted GKE MCP server
  (`https://container.googleapis.com/mcp/read-only`) across both `gke-triage-f`
  and `gke-chat` in favor of local stdio `lookout mcp` (8-tool trimmed triage
  surface dropping `k8s_scan` on `gke-triage-f`; 9-tool audit + triage surface
  plus `leeway` on `gke-chat`), shipped pinned `k8s-lookout` skills (and
  `k8s-triage/references/playbooks`) in `images/agent` with `skill:*` read
  allowlists and CLI-to-MCP mapping notes, gave incident tasks
  `CORE_AGENT_TRUSTED_DIRS` and `*.<location>.gke.goog` egress, and updated
  incident scenario trajectories and `e2e/scenarios/tools.yaml` (#131, #144).
- Enabled `k8s-lookout` storm correlation and the live topology graph across
  the `substrate` profile by switching `deploy/profiles/substrate/lookout`
  from `--storm=off` to `--storm=auto` (verified against
  `ClusterRole/lookout-ax-watch`'s `pods`, `nodes`, and `apps/replicasets`
  `list+watch` grants by `TestSubstrateLookoutStormAutoAndGraphRBAC`), and
  taught `internal/scenario` incident lookup to detect `kind=storm` /
  `kind=storm.member` folds cleanly into `Result.Storm` (caching non-storm
  tasks to avoid repeated SSE polls or waking suspended tasks) without
  grading, draining, or deleting shared storm tasks (#132).
- Pinned `agent` (`769a4f7@sha256:ec2c35b2`: `core-agent 1b15d237` `-tags no_tui`,
  `k8s-lookout v0.46.0`, `leeway v0.1.9`, `ax-runner db1c1de`, native flat
  `gke-chat`) and `e2e-runner` (`769a4f7@sha256:783ce1da`: Substrate `v1.0.1`
  router `503` slow-resume retry, `posture-audit` / scheduled posture audit
  support) in `versions.yaml` and `deploy/` (#144, #146).
- Bumped Agent Substrate to `v1.0.1` (`20778a6db`) in `versions.yaml` (all 10
  upstream `ghcr.io/agent-substrate/substrate/*` image digests) and
  `deploy/profiles/substrate/workers/workerpool.yaml` (`ateworker-gvisor`),
  and made `internal/scenario.Router` retry `503 Service Unavailable` with
  bounded exponential backoff so the e2e runner and audit auto-PR client join
  in-flight slow actor resumes instead of failing immediately (#146). Substrate
  `v1.0.1` hardens `atenet-egress` against loopback/link-local/metadata SSRF,
  upgrades Go to `1.27.4` (`GO-2026-6859`) and `klauspost/compress` to
  `v1.20.4` (`GO-2026-6865`), returns retryable `503` instead of empty `504`
  on slow resumes, and caches Secrets in the k8s credential provider for
  `--secret-cache-ttl` (default `10s`; `token-refresher` rotates every `1800s`
  on 1h tokens).
- Bumped `k8s-lookout` to `v0.46.0` (`lookout:v0.46.0-gke@sha256:d16db0ce`),
  `leeway` to `v0.1.9` (`leeway:v0.1.9@sha256:d1dbbcb7`), and `core-agent` to
  `1b15d237` (`v3.3.0-dev`), and removed `#142`'s temporary
  `lookout_k8s_audit_all` explicit-scope workaround from
  `gke-chat/.agents/AGENTS.md` (#144). `k8s-lookout` `v0.46.0` defaults
  namespace-scoped audit tools to `all_namespaces: true` over MCP, strips
  inapplicable scope properties, hides unconfigured server-flag tools, queues
  excess single-caller stdio calls per cluster, sorts findings worst-first
  before truncation, and logs `elicit=cluster` (`k8s-lookout#867`). `leeway`
  `v0.1.9` honors `notifications/cancelled`, bounds discovery under
  `--timeout`, guards ambient multi-cluster sweeps, accepts array `clusters`,
  and adds unknown-field suggestions (`leeway#41`). `core-agent` `1b15d237`
  omits MCP elicitation in headless/non-TUI runs (`#1606`), skips digest wrap
  on subagents lacking `retrieve_raw` (`#1604`), keeps router descriptions out
  of child ADK identity prompts (`#1607`), gates the Gemini parallelism quirk
  on enabled tools (`#1608`), and adds `model.thinking_level` (`#1605`).
- Converted `gke-chat` to a native, flat `core-agent` recipe under
  `agents/sre-triage/gke-chat/.agents/` (`AGENTS.md`, `config.json`,
  `mcp.json`), removing its legacy `mast` bundle, `_fallback` and
  `posture-audit` subagents, `finding.json` / `finish_task` plumbing, and
  dropping `gke-chat` from `images/agent-mast` (#144). The main agent now
  holds the 6 GKE read tools, `lookout mcp --tools=k8s_audit_all
  --clusters-from=auto --max-concurrency-per-cluster=8` (`k8s_list_clusters`
  and `k8s_audit_all`, ~5.7k posture schema tokens vs ~30k for
  `--profile=audit`), and `leeway mcp --timeout=20s --parallelism=32`
  (`agentic_never: true`) directly, while `propose-change` (`mcp: []`)
  remains layered at deploy time via `chat-config.json`.
- Built `core-agent` in `images/agent` with `-tags no_tui`, declining
  headless MCP elicitation requests immediately instead of blocking on an
  unattached TUI elicitor and trimming ~5 MB from the distroless image
  (#144).
- Pinned reactive `--sources` on `lookout-ax` in `deploy/profiles/substrate/lookout`
  (`k8s-events,object-state,rollout,workload,autoscaling,saturation,degradation,expiry,capacity,ingress,gateway`,
  excluding `topology-drift` and `compute-class` in favor of scheduled `leeway report`
  posture sweeps, with `args[1]` and `args[2]` preserved for environment overlays)
  and vendored `k8s-lookout` `v0.44.0`'s `deploy/14-role-watcher-capacity.yaml` with
  its `kube-system` `RoleBinding` so the `capacity` source starts with explicit RBAC
  grants (#127).
- Bumped `k8s-lookout` to `v0.44.0` (`lookout:v0.44.0-gke@sha256:21066cab`):
  typed Kubernetes Protobuf negotiation by default (`k8s-lookout#824`),
  paginated/trimmed pod and event streaming plus copy-on-write graph edges for
  200k-pod watch/read scale (`k8s-lookout#825`, `#830`, `#831`), `steward`
  `v0.4.0` (`k8s-lookout#819`), and Go `1.26.11` for `GO-2026-6859`
  (`k8s-lookout#826`).
- `agent` image pinned at `dfac600` (`sha256:187872c2`): `core-agent`
  `v3.2.0-rc.4` (`9908a221`, #123) on `distroless/static-debian12:nonroot` with
  `ax-runner` at `db1c1de`.
- Bumped `core-agent` to `v3.2.0-rc.4` (`9908a221`): `core-vcs` `v1.2.0` with
  shared bare object caches under `<workspace_dir>/.cache` (`core-agent#1581`),
  `core-models` `v0.15.4` (`core-agent#1581`), lexical + symlink containment on
  open-prefix permission rules (`core-agent#1579`), and synchronous subagent
  alert race fixes (`core-agent#1580`). The `agent` image pin moves once CI
  publishes the new build.
- `agent` image pinned at `866bb3e` (`sha256:91482f9c`): unified `gke-chat` and
  `gke-triage-f` bundles (#118, retiring `gke-propose`) on core-agent
  v3.2.0-rc.3 with `ax-runner` at `db1c1de`.

- `agent` image pinned at `4936974` (`sha256:e971b0ed`): core-agent
  v3.2.0-rc.3, the `gke-propose` recipe (#116), and
  `distroless/static-debian12:nonroot` (`65532:65532`, #115) with `ax-runner`
  at `d03b3e0`.

- core-agent v3.2.0-rc.3 (`a0dbed2f`) in `images/agent`: defers
  `VCSEventSink` mid-turn audit writes past in-flight turns so
  database-backed sessions don't fail on ADK's stale-session check
  (go-steer/core-agent#1571).

- `images/agent` runtime base moves from `debian:bookworm-slim` (root, with
  `git` and `curl`) to `gcr.io/distroless/static-debian12:nonroot`
  (`65532:65532`), removing the shell, package manager and root user inside
  the actor now that `core-agent` carries pure-Go `vcs_*` tools.

- core-agent v3.2.0-rc.2 (`fe3b18e7`) and Go 1.26.11 (for GO-2026-6859) in
  `images/agent`: brings the pure-Go `vcs_*` tool suite over core-vcs
  v1.1.0 and `builtin:propose-change` (go-steer/core-agent#1490,
  unregistered unless `config.vcs` is set, so existing bundles are
  unchanged) and core-models v0.15.3 (#1557). The `agent` image pin stays
  until CI publishes the new build.

- ax-server at mastersingh24/ax 9a017f7: an actor whose drain suspend
  failed and then crashed is reverted to its latest snapshot (task
  AutoSuspended `RevertedAfterFailedSuspend`) instead of staying Failed.
  ax-runner and the agent image stay at d03b3e0. e2e-runner 6139d97
  (#112).

- e2e-runner 0ce27ac: worker-drain drains a shared worker and checks its
  other tasks (#109).

- ax-server and ax-runner at mastersingh24/ax d03b3e0 (ax#1): ax-server
  suspends a draining worker's actors synchronously (task AutoSuspended
  `WorkerDraining`) and reverts an actor lost with its worker
  (`RevertedAfterWorkerLoss`) instead of leaving the task Failed; the runner
  keeps the agent alive through a first SIGTERM so the suspend snapshots it.
  The agent image is rebuilt with that runner, and the e2e-runner (c3f78e2)
  carries the worker-drain scenario. Stage first (#105).

- Go 1.26.11 (`go.mod`), for GO-2026-6859 in `net/http`.

- switchboard answers end with a usage footer (model, tokens, cost and
  latency of the turn) and render as Slack Block Kit (headers, lists,
  tables): `show_usage` and `slack_rich_blocks` in the switchboard config.
  The footer shows the turn's spend to everyone in the channel.

- The nightly scenarios run on e2e-runner 5656dea and grade each answer
  with a judge (`-judge=claude-sonnet-5-5`, #90): the grade is in
  `result.json` and the archive. The runner's workload identity needs
  `roles/aiplatform.user` in the cluster's project, as the agents'
  token-refresher has; until then the result records the judge's error
  and the run is unaffected.

- Substrate v1.0.0 (GA) and the ax fork at 7c50347 (ax-server and
  ax-runner), rebased onto upstream google/ax 241bc4d with Substrate
  v1.0.0. The runner's pass-through now sends the agent Host
  127.0.0.1:<port>, so core-agent's `attach.allowed_hosts` router entries
  are no longer needed (kept for now, harmless); its workspace setup no
  longer re-runs after a cold resume. ax.proto is unchanged, so lookout,
  switchboard and substrate-scope stay. Clusters move by reinstall
  (deploy repo runbook), stage first.
- core-agent v3.1.0 (GA) in images/agent: rc.2 plus its changelog and
  core-models v0.15.2.
- core-agent v3.1.0-rc.2 in images/agent. Its importer now gives each
  specialist its own content root and the main agent no MCP servers
  (go-steer/core-agent#1498), which the image build used to arrange
  itself and now checks instead. It enforces `agent.max_steps`
  (go-steer/core-agent#1500, a turn's main-agent model calls), so the
  incident profile caps a turn at 8, about twice a healthy one, where
  `final_report` gets a partial answer before the 150s bound would cut
  the turn off with none.
- The incident profile triages in its main agent (#48): `gke-triage-f`'s
  one specialist runs as the main agent, with its four read tools and its
  diagnosis instructions (a reporting section replaces its `finish_task`),
  so lookout's signal is read where it lands instead of being re-typed
  into a delegation, the costliest step measured (2.1k output tokens).
  Specialists to come (deep dives, issues and pull requests) get briefs
  from the triage. Chat keeps its delegation. An incident turn is
  bounded at 150s (`agent.max_wallclock_seconds`; a healthy one takes
  45 to 75s). Background
  subagents (`spawn_agent`) are off in both profiles: delegation is a
  specialist's own synchronous tool.
- The agent on core-agent has its MCP tools where mast had them (#48):
  each specialist loads the GKE server from its own content root, and
  the main agent keeps only what
  `agents/sre-triage/ax/core-agent-<bundle>.main-tools.json` names:
  none for incidents (mast's coordinator only delegated), the
  cluster-level `list_clusters` and `get_cluster` for chat, so a quick
  question needs no specialist. The file tools (`read_file`,
  `write_file`, `grep` and the rest) are off with `bash`: mast had none,
  and each tool's schema rides in every model call. core-agent's
  importer gives the main agent every server (go-steer/core-agent#1498).
- core-agent v3.1.0-rc.1 in images/agent. Its loopback attach listener
  now answers 421 to a Host it doesn't know (a DNS-rebinding guard,
  go-steer/core-agent#1358). Requests reach it from Substrate's router
  through ax-task-runner's pass-through, which keeps the router's Host,
  so both bundles list the router in `attach.allowed_hosts`. The image's
  boot check now asks under that Host, and fails the build on a 421.
- The agent on core-agent runs without `bash` (`--disable-tools=bash`).
  It reads clusters through GKE's MCP server, mast's bundles never had
  it, and from rc.1 only an admin may approve it on a multi-session
  daemon (go-steer/core-agent#1355), so approving it from Slack would
  be refused.
- `gke-triage-f` and `gke-chat` drop the `change-executor` specialist.
  The agent reads clusters through GKE's read-only MCP endpoint, so it
  had no write tools to run and every proposed change was already left
  empty (go-steer/mast#503); a person applies the fix the finding names.
  core-agent refuses a specialist naming tools its server lacks, so this
  is also what lets the bundles import (go-steer/core-agent#1483, #48).

### Removed

- Retired `mast` and `agent-mast` now that every live environment runs
  `core-agent` (#48): removed `images/agent-mast/`,
  `.github/workflows/image-agent-mast.yml`, `agents/sre-triage/ax/task.yaml.tmpl`,
  the `mast` and `agent-mast` component pins in `versions.yaml`, and the
  `mast` runtime, token-adoption, and scenario normalization branches in
  `internal/agent`, `internal/deploy`, and `internal/scenario`.
  **Breaking `shipmind.yaml` change:** `agent.image` is removed
  (`KnownFields(true)` rejects it), `agent.incidentImage` is now required
  (pinned by digest), and `agent.chatImage` remains the per-profile chat
  image setting; environment overlays in `go-steer/shipmind-deploy` must
  delete `agent.image` when moving past this commit. During staggered
  rollouts, run `shipmind scenario canary` and `shipmind status -any-ref`
  against un-bumped overlays from `<= 386c838` until their `agent.image`
  field is removed.

### Fixed

- `gke-chat`'s `posture-audit` specialist now explicitly instructs passing
  `all_namespaces: true` (or `namespace`) to namespace-scoped `lookout` audit
  tools (`k8s_audit_all`, etc.) and `clusters` (never `cluster`) to
  `leeway_report`, preventing `"no scope"` and unknown-field MCP errors seen
  in canary runs (#127).
- Shared `watchboard.digest` sessions in `agents/sre-triage/ax/AGENTS.md.tmpl`
  now evaluate warning entries and Slack `alert` calls per arriving digest
  `sequence` rather than once for the whole session lifetime: new actionable
  non-`shipmind.dev/scenario` warning entries diagnose and call `alert` once for
  that digest batch even when an earlier digest in the same watchboard session
  was a synthetic `shipmind.dev/scenario` probe or already alerted on an
  unrelated warning, while ordinary incident sessions keep
  `alert`-once-on-initial-diagnosis and repeat ("finding stands") suppression
  (#127).
- `up` and `status` no longer ignore a live task template whose tokens
  are malformed when the tokens Secret exists; they refuse, as for any
  other disagreement, instead of overwriting it.
- k8s-lookout v0.43.1: an unchanged Event re-delivered by a relist no
  longer re-opens a resolved incident, which kept staging's nightly
  incident task awake past its suspend and re-created it after the
  scenario deleted it (#60).

- `shipmind scenario run` no longer fails a correct diagnosis when
  lookout sends an update about the incident (a restart burst) while the
  agent is still investigating: a message before the answer joins the
  agent's turn instead of ending the answer window empty.

- Substrate's credential provider reads its namespace policy only at
  startup, so on a fresh cluster (installer first, policy from `up`
  after) every agent's credential fetch was refused: "egress denied".
  `up` now restarts the provider when the policy changes, and `status`
  flags a provider running on an older policy, as it already did for
  lookout and its task template.

- `shipmind up -dry-run` on a fresh cluster no longer fails validating
  the agent's Secrets in a namespace the run would create; it says they
  would be stored, unvalidated.

- `shipmind up` and `status` work on a fresh cluster: `up` reconciles the
  ax Workspace after the objects (ax-server among them) have rolled out,
  instead of asking ax before anything is deployed, and `status` reports
  the Workspace as missing without asking an ax that doesn't exist yet.

- Go 1.26.10, for GO-2026-6809 (crypto/x509 distrusting compromised
  intermediates), which govulncheck finds reachable; the e2e-runner image
  builds with it too.

- The nightly CronJob states `suspend: false`, so a hand-set suspend is
  drift and the next `up` undoes it; DESIGN.md notes the client-side
  apply limit behind it.

- The nightly runs the e2e-runner build with the sweep fix (1238fb2).

- `shipmind scenario run`'s leftover sweep deleted lookout's watchboard
  task on agent-substrate: it took any lookout task whose session
  mentioned an earlier run's workload, and the watchboard hears about
  every workload. The sweep and the incident search now consider only
  lookout's per-incident tasks (`lookout-<16 hex>`), and only when the
  task's first message parses as a single incident (never a storm, whose
  task is shared) about the run's workload in the scenario's namespace.

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
