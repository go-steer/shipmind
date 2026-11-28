# Decision log

Newest first. Each entry: what was decided, why, and what was rejected.
A decision that changes gets a new entry pointing at the old one; old
entries are not edited.

## 2026-11-28: Live cluster scope replaces the static agent.clusters table (#104)

Supersedes the static `agent.clusters` list in `shipmind.yaml` and the
rendered cluster table in Workspace `AGENTS.md`.

Maintaining a hand-edited cluster list in `shipmind.yaml` broke whenever
clusters were created, moved, or deleted: every new cluster required an
overlay edit and `shipmind up` before chat or scheduled audits could see
it, and baking a static list into `AGENTS.md` encouraged the chat agent
to assume that list was authoritative instead of discovering live
clusters.

1. **Structured `agent.clusters` schema (`home`, optional `regions`,
   required `scope`).** `shipmind.yaml` replaces `agent.clusters: []{name,
   path}` with:
   - `home`: required full GKE resource path
     (`projects/<project>/locations/<location>/clusters/<name>`). Used as
     agent context, incident/watchboard control-plane egress, and the
     default `-cluster` for `shipmind scenario canary`, never as a silent
     chat default.
   - `regions`: optional list of GCP regions (`us-central1`, never zones)
     that narrows `*.<region>.gke.goog` egress when `scope` contains
     broad project or fleet entries. Omitting `regions` on a broad scope
     admits all GCP regions (`env.GCPRegions`); narrow scopes (`path` and
     `project`+`location` only) emit only `home`'s region plus those
     exact scope regions.
   - `scope`: required non-empty list of discovery entries, each setting
     exactly one of `project` (with optional `location` region or zone),
     `path` (explicit GKE cluster resource path), or `fleet` (GKE fleet
     host project). GKE label filtering is deferred until `k8s-lookout`
     and `leeway` support `--cluster-label` natively.
2. **Zone-stripped egress and `gkehub.googleapis.com` for fleets.** GKE
   control-plane DNS endpoints always resolve under `*.<region>.gke.goog`
   even for zonal clusters, and Substrate egress allows only single-label
   wildcards. All region derivations strip trailing zone suffixes
   (`us-central1-b` -> `us-central1`). Incident (`gke-triage-f`) and
   watchboard (`gke-watchboard`) tasks receive only `home`'s single
   `*.<home-region>.gke.goog` wildcard alongside `aiplatform.googleapis.com`
   and `container.googleapis.com`. Chat (`gke-chat`) and audit tasks add
   `gkehub.googleapis.com` whenever their effective scope includes a
   `fleet:` entry (`k8s-lookout` `v0.46.0` `pkg/cloud/gke/fleetscope.go`
   uses only GKE Hub memberships + Container API `clusters.list`).
3. **Live `LOOKOUT_CLUSTERS_FROM` / `LEEWAY_CLUSTERS_FROM` wiring.**
   `gke-chat/.agents/mcp.json` drops `--clusters-from=auto` (which in
   `k8s-lookout` `v0.46.0` overrides `LOOKOUT_CLUSTERS_FROM`), so the
   task template's `LOOKOUT_CLUSTERS_FROM` drives live multi-cluster
   discovery (`project`, `project/location`, `fleet:<host>`, and
   `<project>/<location>` for explicit `path:` entries since `lookout`'s
   `ParseScopes` does not accept single-cluster paths). `gke-triage-f`
   and `gke-watchboard` keep `--clusters-from=auto` because `lookout`
   names the incident cluster on the prompt. `LEEWAY_CLUSTERS_FROM`
   receives `project` and `project/location` entries only, omitting both
   `fleet:` (`leeway` `v0.1.9` rejects `fleet:` in `LEEWAY_CLUSTERS_FROM`)
   and explicit `path:` entries (mapping `path:` to `<project>/<location>`
   sets `configuredFrom` in `leeway` `v0.1.9` and disables its unscoped
   multi-cluster refusal guard, letting an accidental unscoped
   `leeway_report({})` sweep sibling clusters outside the explicit path
   list; explicit full cluster paths work directly in `leeway_report`).
4. **Live-resolved scheduled audits (`#127`) with optional `audits.scope`
   override.** Scheduled posture audits default to `agent.clusters.scope`
   and accept an optional `audits.scope` override when an environment
   wants a narrower nightly sweep than interactive chat. `shipmind audit
   run` resolves `project:` and `fleet:` scopes live against
   `container.googleapis.com` and `gkehub.googleapis.com` at CronJob
   runtime (using the same `ate-credentials/google-token` / metadata
   token provider it uses for cluster auth) into concrete cluster paths
   before running `lookout audit` and `leeway report --clusters=...`,
   recording per-scope discovery failures without stale-closing
   unreachable clusters in the ledger.
5. **Anti-hallucination Workspace rules & deploy-time IAM checks.**
   Workspace `AGENTS.md` drops the static cluster table in favor of
   explicit rules: `home` is context never a chat default; call
   `lookout_k8s_list_clusters` when the cluster is unspecified or an
   object is not where expected; confirm named objects exist before
   diagnosing; never state a cause no tool output showed. `shipmind
   status` probes each unique in-scope project/location and fleet with
   `ate-credentials/google-token` (redirects disabled, token never
   printed) so missing cross-project `roles/container.viewer` (or custom
   viewer+logs) or `roles/gkehub.viewer` grants surface as drift (`scope
   project/...`, `scope fleet/...`, or `scope access`) at deploy time.

Rejected:
- Keeping a fallback `agent.clusters` list syntax alongside the structured
  map: there is only one private overlay (`go-steer/shipmind-deploy`) and
  carrying two YAML shapes complicates `internal/env` and Config Studio
  for zero external compatibility benefit.
- Resolving `agent.clusters.scope` at `shipmind up` time into static
  `AGENTS.md` / `audit` cluster lists: reintroduces the exact staleness
  problem #104 removes.
- Admitting `*.gke.goog` as a two-label wildcard: Substrate's egress
  gateway rejects multi-label wildcards by design.

## 2026-11-28: lookout's watchboard runs its own read-only gke-watchboard recipe and task template (#133)

Extends 2026-11-05 ("Two agent profiles, autonomous and interactive, one
image"), 2026-11-17 ("One general-purpose chat agent plus autonomous incident
PRs and Slack alerts"), 2026-11-25 ("gke-chat is a native flat core-agent
recipe with a trimmed posture surface and -tags no_tui (#144)"), and
2026-11-28 ("Native flat gke-triage-f, lookout-only read path across all
recipes, and pinned k8s-lookout skills (#131, #144)").

`k8s-lookout`'s per-cluster watchboard task (`lookout-wb-<cluster>-<hash8>`)
runs a dedicated native `core-agent` recipe, **`gke-watchboard`**, rendered
as its own task template (`watchboard-task.yaml` in `Secret
lookout-ax/lookout-ax-agent`) and wired into `lookout-ax` via
`--ax-watchboard-task-template=/etc/lookout-ax/watchboard-task.yaml`
(`k8s-lookout` `v0.46.0`, `#847`):

1. **Digest-shaped native recipe (`agents/sre-triage/gke-watchboard/.agents/`).**
   `AGENTS.md` is authored for rolling `"kind":"watchboard.digest"` batches
   of warning-class leading indicators (`degradation`, `objectstate`,
   `autoscaling`, `capacity`, `saturation.forecast`, `expiry`, `ingress`,
   `gateway`, downgraded incidents — now carrying sanitized `message` text
   per entry since `k8s-lookout#846`) and `"kind":"watchboard.rotated"`
   handoffs. Each arriving digest (`sequence`) is evaluated independently,
   matching repeats by target object (`namespace`, `kind_of_object`, `name`)
   and `reason` (never by `fingerprint` alone), exempting
   `shipmind.dev/scenario` probes from Slack alerts without suppressing
   unrelated real warnings in the same or later digests, and summarizing
   actionable warnings concisely.
2. **B1 `lookout`-only stdio MCP plus pinned `k8s-triage` skill.**
   `gke-watchboard/.agents/mcp.json` runs `lookout mcp` directly over stdio
   with the trimmed 8-tool triage surface (`k8s_triage_workload`,
   `k8s_resource_spec`, `k8s_triage_logs`, `k8s_event_timeline`,
   `k8s_list_resources`, `k8s_state_edges`, `k8s_triage_delta`, plus
   auto-added `k8s_list_clusters` under `--clusters-from=auto`, 47,930 bytes
   / ~13.2k tokens of schema, dropping 34.8 KB `k8s_scan`) and **no** hosted
   GKE MCP (`container.googleapis.com/mcp/read-only`). `images/agent` copies
   `k8s-triage` into `/opt/core-agent/recipes/gke-watchboard/.agents/skills/`
   and `playbooks` into `k8s-triage/references/playbooks/` (so ADK's
   `load_skill_resource`, which restricts paths to `references/`, `assets/`,
   or `scripts/`, can read `hpa-thrash.md` and sibling playbooks), and
   `config.json` allowlists both `mcp:lookout_k8s_*` and `skill:*` tools
   under `mode: "ask"` (`approval_timeout: "30s"`). Watchboard tasks receive
   `*.<location>.gke.goog` egress alongside `container.googleapis.com` and
   `aiplatform.googleapis.com` plus `CORE_AGENT_TRUSTED_DIRS`.
3. **Tight budgets, short `2m` idle suspend, and strictly read-only.**
   `gke-watchboard` caps each turn at `max_steps: 6` and
   `max_wallclock_seconds: 90` (`final_report: true`, `max_session_cost_usd:
   5`), and `watchboard-task.yaml` pins `spec.idle.suspendAfter: 2m` (120s —
   strictly above the 90s wall-clock cap with 30s headroom so `ax` never
   suspends an unwatched turn mid-flight, longer than `lookout`'s 60s
   `--watchboard-flush` so bursts stay warm, and far shorter than
   `agent.idleSuspendAfter`'s 10m default or 5m `incidentAutoPR` floor so
   the watchboard frees its Substrate worker slot quickly). When
   `slack.alertChannel` is set, `shipmind up` writes `watchboard-config.json`
   to the ax Workspace layering only the `alert:slack` webhook target;
   watchboard tasks **never** receive `propose-change`, `vcs_*`, file tools,
   or GitHub egress even when `agent.vcs.incidentAutoPR: true`.
4. **Zero-rotation upgrade and live-cluster cutover.** `shipmind status`
   compares `token`, `task.yaml`, and `watchboard-task.yaml` in `Secret
   lookout-ax/lookout-ax-agent` by value (and validates both templates'
   `CORE_AGENT_ATTACH_TOKEN` when present). Upgrading an existing cluster
   adds `watchboard-task.yaml` and restarts `lookout-ax` without touching or
   rotating `Secret lookout-ax/agent-tokens`. Two operational cutover rules
   govern existing clusters (matching 2026-11-03):
   - Move `agent.incidentImage` in `go-steer/shipmind-deploy` to an
     `images/agent` digest built at or after `#133` (carrying
     `/opt/core-agent/recipes/gke-watchboard`) at the same time as bumping
     the overlay's `shipmind` commit pin, so newly stamped watchboard tasks
     find the baked recipe on boot.
   - Because `ax` tasks are immutable and `lookout`'s ax sink reuses the
     deterministic per-cluster task name `lookout-wb-<cluster>-<hash8>`
     across restarts and `--watchboard-rotate` session rotations, delete the
     pre-existing watchboard task (`ax delete task -a <atespace>
     lookout-wb-<cluster>-<hash8>`) after `shipmind up` so `lookout-ax`
     stamps a fresh watchboard actor from `watchboard-task.yaml`.

**Why:** Stamping the shared watchboard from `gke-triage-f`'s per-incident
template conflated two incompatible workloads: a single-object incident
diagnoser (which can also run 240s autonomous `propose-change` PR turns and
holds a worker slot for 5–10m) versus a long-lived warning-digest board
receiving batches of unrelated leading indicators across the cluster. On
`shipmind-stage` on 2026-11-20, that mismatch caused a synthetic scenario
probe on digest #1 to suppress a real unrelated warning later in the same
session, and left the watchboard occupying a Substrate worker slot between
flushes. Splitting the recipe and task template isolates watchboard budgets,
tools, egress, and worker-slot residency cleanly on the same `images/agent`
image.

**Rejected:**
- *Keeping the watchboard on `gke-triage-f`'s incident template* (even with
  prompt workarounds in Workspace `AGENTS.md`, the watchboard still inherits
  incident step/time budgets, `propose-change` and GitHub egress whenever
  `incidentAutoPR: true`, and a 5–10m idle timeout holding a Substrate worker
  slot).
- *Store-only warnings with no watchboard agent* (recording warnings in
  `lookout`'s SQLite store and surfacing them only on daily scheduled
  posture sweeps or on-demand `lookout mcp` calls saves worker slots and
  model tokens, but gives up timely agent triage and Slack notification on
  fast-moving leading indicators such as `degradation`, `capacity`,
  `saturation.forecast`, `expiry`, `ingress`, and `gateway` between daily
  runs).
- *Adding a separate `agent.watchboardImage` field or a second container
  image* (same binaries as `images/agent`; per 2026-11-05, profile
  differences stay configuration on one image, rendered from
  `agent.incidentImage`).

## 2026-11-28: Native flat gke-triage-f, lookout-only read path across all recipes, and pinned k8s-lookout skills (#131, #144)

Amends 2026-11-25 ("gke-chat is a native flat core-agent recipe with a trimmed
posture surface and -tags no_tui (#144)"), 2026-11-24 ("gke-chat runs lookout
and leeway as their own MCP servers, and a canary proves an agent image before
its pin moves (#127)"), and 2026-11-05 ("The incident agent triages;
specialists get briefs").

1. **Native flat `gke-triage-f` recipe; delete `import-workload` and fold
   machinery.** With `mast` and `images/agent-mast` retired (#152),
   `gke-triage-f` is authored directly under
   `agents/sre-triage/gke-triage-f/.agents/` (`AGENTS.md`, `config.json`,
   `mcp.json`). The remaining legacy `mast` bundles
   (`agents/sre-triage/gke-triage-f/{workload.yaml,mcp.json,schemas/,specialists/}`,
   `agents/sre-triage/gke-triage/`, and
   `agents/sre-triage/ax/core-agent-gke-triage-f.*`) and the entire
   `core-agent config import-workload` + awk/jq fold loop in
   `images/agent/Dockerfile` are deleted.
2. **100% `lookout`-only Kubernetes read path everywhere (drop hosted GKE
   MCP).** `https://container.googleapis.com/mcp/read-only` (the `gke` server)
   is removed from both `gke-triage-f` and `gke-chat`. All live Kubernetes
   reads flow through local stdio `lookout mcp --clusters-from=auto
   --max-concurrency-per-cluster=8` inside `images/agent` (same transport
   pattern as `gke-chat` posture audits; multi-cluster ready with Substrate's
   injected Google token). Incident tasks (`gke-triage-f`) now carry
   `CORE_AGENT_TRUSTED_DIRS` (so `core-agent` starts the recipe's stdio
   `lookout` server) and the same `*.<location>.gke.goog` egress hosts as
   `gke-chat`, while retaining `container.googleapis.com` for cluster
   discovery; `LEEWAY_CLUSTERS_FROM` stays chat-only. Shared in-cluster mTLS
   `lookout mcp` and sentinel `--store` / `--at` history reads are deferred.
3. **Trimmed 8-tool triage surface (dropping `k8s_scan`).** `gke-triage-f`
   runs `lookout mcp
   --tools=k8s_triage_workload,k8s_resource_spec,k8s_triage_logs,k8s_event_timeline,k8s_list_resources,k8s_state_edges,k8s_triage_delta
   --clusters-from=auto --max-concurrency-per-cluster=8`, exposing 8 tools
   (including auto-added `k8s_list_clusters`; 47,930 B / ~13.2k tokens at
   lookout's 3.64 B/token) and dropping `k8s_scan` (34.8 KB alone).
   `gke-chat` adds `k8s_audit_all` for 9 `lookout` tools (65,601 B / ~18.0k
   tokens) alongside `leeway`'s `report` (`agentic_never: true`). `core-agent`
   digesting stays enabled on `lookout` because both flat main agents hold
   `retrieve_raw`.
4. **Pinned `k8s-lookout` skills and CLI-to-MCP mapping.** `images/agent`
   copies skills straight from the pinned `lookout` build stage
   (`/src/skills/`): `k8s-triage` + `playbooks` for `gke-triage-f` (1 loaded
   skill), and `k8s-triage`, `cluster-health`, `k8s-audit`, `k8s-capacity`,
   `gitops-drift` + `playbooks` for `gke-chat` (5 loaded skills). Because
   ADK's `load_skill_resource` restricts paths to `references/`, `assets/`, or
   `scripts/` (rejecting `../playbooks/`), `playbooks/*.md` are also copied
   into `k8s-triage/references/playbooks/`, and both recipes allowlist
   `skill:list_skills*`, `skill:load_skill*`, and `skill:load_skill_resource*`
   under `permissions.mode: "ask"`. Both `AGENTS.md` files map `overflow
   section=<s> cmd="lookout ..."` trailers and skill CLI examples to their
   `lookout_k8s_*` MCP tools (noting that `lookout_k8s_triage_logs` rejects
   passing `namespace` alongside `workload`), and `e2e/scenarios/triage-overflow`
   exercises an incident whose enrichment bundle reliably overflows
   `section=logs` under the default 4096-byte cap so the run only passes by
   following the overflow trailer with `lookout_k8s_triage_logs`.

**Why:** Keeping the hosted GKE MCP server alongside `lookout` gave the model
two competing ways to read a pod, spent schema tokens on raw Kubernetes reads
that lacked `lookout`'s secret masking, Drain log distillation, and dependency
edge checks, and left the incident agent unable to follow the `overflow
section=<s> cmd="lookout ..."` trailers `lookout watch` already attaches to
incidents. Running `lookout mcp` over local stdio inside the distroless actor
reuses `gke-chat`'s proven credential-injection and `*.<location>.gke.goog`
egress path with zero new in-cluster mTLS sidecar or credential plane, while
pinning the skills directly from the `lookout` build stage prevents skill
documentation from drifting behind the `lookout` binary.

**Rejected:** Running a shared in-cluster mTLS `lookout mcp` server attached
to the sentinel's SQLite store now (introduces a new network hop, mTLS
identity surface, and multi-cluster store design before the local stdio path
is proven); keeping hosted GKE MCP beside `lookout` (duplicated read paths,
raw unmasked payloads, extra schema tokens); enabling full `--profile=triage`
including `k8s_scan` (adds 34.8 KB / ~9.6k tokens of schema to every model
call for a target-free sweep an incident session already targeted); and
keeping `gke-triage-f` behind `core-agent config import-workload` after `mast`
was retired.

## 2026-11-28: Enable lookout storm correlation (--storm=auto) in the substrate profile (#132)

1. **`--storm=auto` across the `substrate` profile.**
   `deploy/profiles/substrate/lookout/lookout.yaml` switches `--storm=off` to
   `--storm=auto` for all environments (keeping `args[1]`
   `--cluster-name=CLUSTER` and `args[2]` `--namespace=NAMESPACES` at their
   indices for environment overlay patches). At startup `k8s-lookout` probes
   `pods`, `nodes`, and `apps/replicasets` `list+watch` against the vendored
   `ClusterRole/lookout-ax-watch` (`deploy/vendor/k8s-lookout/deploy/12-clusterrole-watcher/`)
   and resolves storm correlation and the live topology graph `on`.
   `TestSubstrateLookoutStormAutoAndGraphRBAC` enforces both `--storm=auto`
   and all six graph grants in CI.
2. **Storm-aware scenario detection without touching shared storm tasks.**
   `internal/scenario` keeps `Transcript.FirstIncident` rejecting `storm` and
   `storm.*` payloads so `sweep` and deferred cleanup never claim or delete a
   shared storm task. `Transcript.StormContaining` parses `k8s-lookout`'s
   `kind=storm` (`representative_incidents`) and `kind=storm.member`
   (`incident`) wire payloads, including multi-bullet core-agent `[Inbox]`
   bundles. During incident lookup (`waitForIncident` / `findIncident`), `scenario.Run`
   inspects new non-`Failed` tasks and already-`Running` pre-existing
   `lookout-*` tasks (never waking `Suspended` tasks, and caching tasks
   classified as non-storm by `sweep` or earlier polls so unrelated
   per-incident tasks are not re-polled over 15s SSE streams or kept awake),
   always prefers a dedicated per-incident task when one exists or is still
   booting, and otherwise fails fast with `Result.Storm` populated (storm
   task, session, kind, and blast-radius ancestor), removes the scenario
   workload, and leaves the shared storm task untouched.

**Why:** Running with `--storm=off` carried two costs on `substrate`
deployments. First, correlated bursts opened one ax task per failing pod
instead of one storm task — a node outage affecting 15 pods saturated a
4-worker pool (`deploy/profiles/substrate/workers/workerpool.yaml`, 12 actor
slots). Second, `--storm=off` turned off `k8s-lookout`'s live topology graph,
pushing incident enrichment onto per-incident scoped-list fallbacks and
disabling `--store` graph history (`--at` queries). The vendored
`ClusterRole/lookout-ax-watch` already grants the required
`pods`/`nodes`/`apps/replicasets` `list+watch` permissions (and the
routing-layer `services`/`endpointslices`/`ingresses`/`networkpolicies`
`list+watch` grants for graph history), so `--storm=auto` resolves `on` with
no RBAC expansion while loudly degrading if a narrower overlay trims grants.
Nightly scenarios deploy one single-replica workload on staggered 30-minute
schedules (`03:17`, `03:47`, `04:17` UTC) — below lookout's 3-incident / 60s
storm threshold — so normal runs still open dedicated per-incident tasks;
when concurrent failures fold a probe into a storm, structured `Result.Storm`
reporting replaces a misleading 10-minute `"no lookout incident task"`
timeout without clobbering the live storm agent.

**Rejected:** Keeping `--storm=off` (leaves the worker pool exposed to burst
saturation and forfeits live topology-graph enrichment and store history);
enabling `--storm=auto` only in production overlays while keeping dev and
stage on `--storm=off` (splits runtime behavior across environments so storm
correlation and topology-graph changes reach production untested); adopting
a shared storm task as `Result.Task` to grade, drain, or delete in
`internal/scenario` (a storm session targets the shared blast-radius ancestor
rather than one workload's ConfigMap or memory limit, accumulates multi-turn
usage beyond single-incident trajectory bounds, will not idle-suspend while
sibling members stay broken, and deleting it would destroy live incident
handling).

## 2026-11-25: gke-chat is a native flat core-agent recipe with a trimmed posture surface and -tags no_tui (#144)

Amends 2026-11-05 ("Two agent profiles, autonomous and interactive, one
image"), 2026-11-17 ("One general-purpose chat agent plus autonomous incident
PRs and Slack alerts"), and 2026-11-24 ("gke-chat runs lookout and leeway as
their own MCP servers, and a canary proves an agent image before its pin
moves (#127)").

1. **Native flat `gke-chat` recipe.** The 2026-11-06 precondition is met: all
   three overlays in `go-steer/shipmind-deploy` (`substrate`,
   `shipmind-stage`, `shipmind-prod`) set both `agent.chatImage` and
   `agent.incidentImage`. `gke-chat` is now authored directly in
   `core-agent`'s native layout under `agents/sre-triage/gke-chat/.agents/`
   (`AGENTS.md`, `config.json`, `mcp.json`) and copied straight into
   `/opt/core-agent/recipes/gke-chat/.agents` by `images/agent`. Its legacy
   `mast` bundle (`workload.yaml`, `specialists/_fallback.specialist.md`,
   `specialists/posture-audit.specialist.md`, `schemas/finding.json`,
   `agents/sre-triage/ax/core-agent-gke-chat.*`) is deleted, and `gke-chat`
   is dropped from `images/agent-mast` (which has carried no `lookout` or
   `leeway` binary since #137). `gke-triage-f` and `gke-triage` keep their
   `_fallback` specialist unchanged because `mast` reserves `FallbackName =
   "_fallback"`.
2. **Trimmed posture surface directly on the main agent.** The `gke-chat`
   main agent holds 9 read-only MCP tools across 3 servers: 6 hosted GKE
   reads (`list_clusters`, `get_cluster`, `get_k8s_resource`,
   `describe_k8s_resource`, `get_k8s_logs`, `list_k8s_events`), 2 `lookout`
   tools (`lookout mcp --tools=k8s_audit_all --clusters-from=auto
   --max-concurrency-per-cluster=8`, advertising only `k8s_list_clusters`
   and `k8s_audit_all`), and 1 `leeway` tool (`leeway mcp --timeout=20s
   --parallelism=32` with `agentic_never: true`, advertising `report`).
   `core-agent` digesting stays enabled on `lookout` because the main agent
   carries `retrieve_raw`; `agentic_never: true` on `leeway` keeps
   deterministic remediation YAML and status fields intact. The trimmed
   posture surface costs 20,946 bytes of schema (~5.7k tokens at lookout's
   3.64 bytes/token) instead of ~109 KB (~30k tokens) for all 14
   `--profile=audit` tools, directly answering 2026-11-05's schema-cost
   objection without needing subagent indirection.
3. **`propose-change` remains the sole subagent.** When `agent.vcs` is set,
   `shipmind up` layers `propose-change` (`mode: chat`,
   `capability: change_executor`, `mcp: []`) onto
   `agents/sre-triage/gke-chat/.agents/config.json` in `chat-config.json`
   (and `audit-config.json` for `audits.autoPR`), keeping repository clones,
   file edits, and pull request creation isolated from cluster reads and
   behind the VCS approval policy.
4. **Build `core-agent` with `-tags no_tui` in `images/agent`.** The default
   build installed the bubble-tea TUI elicitor even under `--no-repl`,
   advertising MCP elicitation capability with no UI loop draining it and
   parking any server elicitation ask until the turn's wall-clock deadline.
   Building with `-tags no_tui` compiles `tui_disabled.go`, where
   `makeMCPElicitor()` returns `nil` (immediately declining elicitation),
   and trims ~5 MB of unused terminal UI dependencies from the distroless
   image.
5. **Canary archiving.** `shipmind scenario canary` inherits the environment
   overlay's `-archive` target (overridable via `-archive`) into the canary
   Job so canary runs write `transcript.sse`, `usage.json`, `result.json`,
   and `meta.json` to Cloud Storage (`docs/DESIGN.md`, `#SM-10`).

**Why:** Canary runs of the imported `posture-audit` specialist timed out
and misbehaved for three structural reasons: ADK injected `posture-audit`'s
router-facing description as its own identity (causing the subagent to call
itself), rooted subagents with `tools: []` lacked `retrieve_raw` for
digested MCP payloads, and the default `core-agent` build hung indefinitely
on headless MCP elicitation. Flattening read-only cluster and posture tools
onto `gke-chat`'s main agent removes the delegation hop, eliminates
`finding.json` / `finish_task` mast baggage, gives `lookout` digests native
access to `retrieve_raw`, and keeps schema overhead bounded via
`--tools=k8s_audit_all` (`k8s_scan` alone was 34,944 B and the 11
single-check tools duplicated `k8s_audit_all`'s `only`, `skip`, and
`min_severity` arguments).

**Rejected:** Exposing all 14 `--profile=audit` tools on the main agent
(~30k tokens of tool schema on every chat model call, reversing 2026-11-05's
cost discipline); keeping `gke-chat` behind `core-agent config
import-workload` with `_fallback` and `posture-audit` specialists (preserves
self-calling subagents, missing `retrieve_raw`, and mast schema overhead);
renaming `_fallback` in `gke-triage-f` or `gke-triage` (`mast` reserves
`FallbackName = "_fallback"`); and blocking these flag-and-recipe fixes on
upstream releases for `k8s-lookout#867`, `core-agent#1604`, and `leeway#41`.

## 2026-11-24: gke-chat runs lookout and leeway as their own MCP servers, and a canary proves an agent image before its pin moves (#127)

`gke-chat`'s `posture-audit` specialist gets lookout's audit and leeway's
report by running the two components' own binaries as stdio MCP servers
inside the agent task: `lookout mcp --profile=audit --clusters-from=auto`
and `leeway mcp`, built from their `versions.yaml` pins into
`images/agent`. shipmind adds only configuration: the bundle's
`mcp.json`, the chat task's trusted directories, `*.<location>.gke.goog`
egress for the environment's clusters, and a project list for leeway
when those clusters span projects.

A first version of this change put a bridge in shipmind
(`internal/posture`, `shipmind mcp`): an MCP server wrapped around
`leeway report`, a fake metadata server for injected Google auth, a
credential-proxy translator, and cluster-name mapping. That was component
source in the distribution, against the first hard rule, and it was
never merged. The capability went upstream instead (go-steer/leeway#37
in `v0.1.8`, go-steer/k8s-lookout#858 in `v0.45.0`), and shipmind moved
its pins.

What the components do not do, shipmind does not paper over. Neither
reads the Workspace's cluster table, so the specialist is told to name
clusters by full path. lookout's cluster list covers the agent's own
project and reaches others on demand by path. There is no whole-call
timeout from shipmind.

Because none of this can be shown to work without a cluster, the change
adds a canary: a chat scenario kind and `shipmind scenario canary`
(DESIGN.md, "Canary runs of a candidate agent image"). It runs a
candidate agent image as one throwaway task in a running environment and
changes nothing the environment deploys.

Rejected: keeping the bridge as a stopgap (it would have had to be
maintained against two components' internals); running the canary from a
workstation (scenarios run in CI or as Jobs); and adding the canary to
the nightly e2e component (it would reach a cluster only by upgrading the
environment, which is what a canary exists to precede).

## 2026-11-22: Placement and ComputeClass rank are scheduled `leeway report` posture, not lookout watch signals

shipmind does not run leeway in informer mode: neither lookout's in-tree
`topology-drift` and `compute-class` watch sources nor `leeway watch`.
Placement skew and ComputeClass rank are checked by `leeway report`, run
on a schedule beside `lookout audit` (#127) and on demand from chat.

- `go-steer/leeway` becomes a pinned component in `versions.yaml`.
- Each profile's `lookout watch` names its `--sources` and leaves out
  `topology-drift` and `compute-class`, ahead of
  go-steer/k8s-lookout#842 removing them from `--sources=auto`.
- No `LeewayPolicy` or `ClusterLeewayPolicy` CRDs are installed on target
  clusters.
- Findings go to Slack as a digest with the remediation attached. They
  never open an incident task and never page. The schedule starts at
  daily; a ledger keyed by finding identity stops a standing finding
  from being reported again inside the refresh window.

The boundary this sets: lookout is the reactive sentinel, the read path
and the static posture audit; leeway owns runtime placement and
ComputeClass rank; shipmind owns when either runs and where the result
goes.

**Why:** skew and rank fallback happen while every pod is Running and
Ready. They are exposure to a later zone or capacity failure, not an
outage, so hearing about them in seconds buys little and an incident
task is the wrong shape. On shipmind-stage on 2026-11-20 a routine
node auto-provisioning scale-down left one zone empty and lookout's
in-tree source raised `leeway.domain_unavailable` into the shared
watchboard task, where the digest carried no message and the agent did
not alert. The in-tree copy has also fallen behind `go-steer/leeway`
(scoring fixes, remediation output, report mode). `leeway report` holds
no informer between runs and spends no model tokens when a cluster is
clean.

**Consequence:** report mode gives up what only a resident watcher can
know. `leeway.baseline_breach` is produced by `leeway watch` only.
`leeway report` has no rollout source, so it cannot rule out
`rollout_bias` and a run taken mid-rollout can report skew that would
have been suppressed. Dwell is bypassed, so the ledger has to absorb
transients, including an empty zone on a cluster that is allowed to
scale a zone to zero. A placement finding clears when the next run no
longer reports it; nothing injects `resolved`.

**Rejected:** keeping the two watch sources in lookout as thin wrappers
over the leeway library (one informer and earlier detection, but posture
findings stay in the incident path and lookout keeps 23 flags and a
second copy of the wiring); running `leeway watch` beside
`lookout watch` (a second cluster-wide Pod informer for a signal nobody
is paged on); installing `LeewayPolicy` CRDs to promote soft-spread
findings to critical (a workload that needs zone spread should declare
`topologySpreadConstraints`, which the scheduler enforces).

## 2026-11-20: The admin console lives in-tree in shipmind (#128)

Clarifies 2026-10-20 ("A distribution, not a monorepo"). Design:
[`console-design.md`](./console-design.md).

`shipmind`'s first-party Admin Console (`shipmind console` CLI subcommand,
`internal/console`, and `images/console`, published to
`ghcr.io/go-steer/shipmind/console` and pinned by digest in
`versions.yaml`) lives in-tree in `go-steer/shipmind` and deploys in
`shipmind-system` across both `standalone` and `substrate` profiles.
`shipmind up` stamps a sanitized, non-secret `ConfigMap`
(`shipmind-system/shipmind-release`) so the in-cluster console knows its
effective `shipmind.yaml`, BOM, and drift baseline without the deployer's
workstation, authenticates operators via `go-steer/purser` (`loopback`,
`bearer`, `oidc`, `proxy-header`, `iap-jwt`), embeds a vendored
`mast-web` release under `/mast-web/` (enforcing `substrate-scope`'s
never-wake-on-read invariant on `substrate`), and routes `shipmind.yaml`
edits through a drift-free three-tier write path (local `up`, browser diff
export, or `go-steer/core-vcs` pull requests against
`console.overlayRepo`). Component-side status and auth additions stay in
their component repositories (`core-agent`, `purser`, `k8s-lookout`,
`switchboard`, `mast-web`) and enter `shipmind` via `versions.yaml` pin
bumps.

**Why:** unlike `k8s-lookout`, `switchboard`, `mast-web`, and
`substrate-scope` — which are standalone components usable without
`shipmind` — the admin console *is* `shipmind`'s distribution control
plane, like `cmd/shipmind`, `internal/deploy`, and `images/e2e-runner`.
Keeping it in `go-steer/shipmind` means `internal/env`, `internal/bom`,
`internal/deploy`, `internal/archive`, `internal/slackapp`, and Config
Studio move in one pull request with zero schema skew whenever
`shipmind.yaml` evolves, and one `shipmind` binary serves both Day-0
workstation bootstrap (`shipmind console <env-dir>`) and distroless
in-cluster serving (`images/console`).

**Rejected:** a separate `go-steer/shipmind-console` repository (forces a
two-repository release and pin bump on every `shipmind.yaml` field and
duplicates `internal/env` validation and `internal/deploy` drift rules);
growing `substrate-scope` or `mast-web` into the platform admin console
(`substrate-scope` is a Substrate-only 3D visualizer with no role on the
`standalone` profile, and `mast-web` is a single-agent session client);
patching cluster Deployments or Secrets directly from the UI (causes
immediate `shipmind status` exit-3 drift and loses changes on the next
`shipmind up`).

## 2026-11-18: Optional zero-HITL pull request creation in Slack chat (`agent.vcs.chatAutoApprove`)

Extends 2026-11-17 ("One general-purpose chat agent plus autonomous incident
PRs and Slack alerts").

`agent.vcs.chatAutoApprove` (boolean, default `false`, requiring
`agent.chatImage`) allowlists `vcs_publish:*` and `vcs_propose:*` in the
Workspace's `chat-config.json` when set to `true`. When `false` (or omitted),
`vcs_publish` and `vcs_propose` stay in `ask` mode so switchboard prompts for
approval in Slack before pushing a branch or opening a pull request.

**Why:** `core-vcs` already enforces the repository allowlist, the `agent/`
topic-branch prefix, and `TARGET_IS_BRANCH` (never pushing to `main`), so the
GitHub pull request itself is the human review gate before anything reaches
production. In environments where an operator explicitly asks `@shipmind-<env>`
in Slack to open a pull request (including on an alert-only incident thread),
pausing twice more on Slack `Allow` buttons (`vcs_publish`, then `vcs_propose`)
adds friction without adding production-branch protection. Keeping `false` as
the default preserves explicit per-action confirmation for environments that
want a chat gate before any remote branch or pull request is written.

**Rejected:** making auto-approve the unconditional behavior with no opt-out
(breaks environments that treat remote repository writes as human-confirmed);
sharing `agent.vcs.incidentAutoPR` for both incident and chat behavior
(conflates whether autonomous incidents open unprompted pull requests with
whether human-initiated chat turns prompt in Slack — in `incident -> Slack alert
-> human reply -> open PR`, an environment wants `incidentAutoPR: false` and
`chatAutoApprove: true`).

## 2026-11-17: One general-purpose chat agent plus autonomous incident PRs and Slack alerts

Supersedes 2026-11-17 ("A separate switchboard 'propose' agent opens GitOps
pull requests").

1. **Single general-purpose Slack agent.** Switchboard keeps one agent
   (`triage`, `default_agent: "triage"`). When `agent.vcs` is set alongside
   `agent.chatImage`, `shipmind up` writes `chat-config.json` into the ax
   Workspace and points `/etc/switchboard/triage.yaml` at it (with
   `--agents-dir=/opt/core-agent/recipes/gke-chat/.agents`), equipping
   `triage` with a synchronous `propose-change` specialist (`@include
   builtin:propose-change`, `mcp: []`, `read_file`/`edit_file`/`write_file`/
   `grep` and pure-Go `vcs_*` tools) alongside `_fallback`. Delegation to
   `propose-change` (`spawn_agent:propose-change`) and local clone/edit/
   `vcs_commit` steps in `/workspace/repos/*` are allowlisted, while
   `vcs_publish` and `vcs_propose` stay in `ask` mode so switchboard prompts in
   Slack before pushing a branch or opening a pull request. (Core-agent's shared
   `builtinTools` slice registers the enabled file and `vcs_*` schemas on the
   root agent as well; `AGENTS.md` directs the root coordinator to delegate all
   repository and file work through `propose-change`.) The separate `propose`
   switchboard agent, `propose.yaml` volume mount and baked `gke-propose` recipe
   are removed; `images/agent` checks both `core-agent-gke-chat.base.json` and
   `core-agent-gke-triage-f.base.json` against the imported recipes at Docker
   build time.
2. **Autonomous incident PRs (`agent.vcs.incidentAutoPR: true`) and Slack
   notifications (`slack.alertChannel`).** When either is set (with
   `agent.incidentImage`), `shipmind up` writes `incident-config.json` into the
   Workspace. `incidentAutoPR` raises the incident budget to `max_steps: 12` and
   `max_wallclock_seconds: 240` (requiring `agent.idleSuspendAfter >= 5m` so `ax`
   does not suspend an unwatched `lookout` turn mid-PR), allowlists
   `spawn_agent:propose-change` plus headless `vcs_publish`/`vcs_propose`, and
   equips `propose-change` with `vcs_proposal_list` open-PR deduplication,
   fast-exit on unmanaged workloads, and a guard refusing unknown secrets or
   connection URLs. Alert-only incidents (`incidentAutoPR: false`,
   `slack.alertChannel` set) keep the `150s` wall-clock cap (`max_steps: 8`) so
   environments with `idleSuspendAfter: 3m` can run them. `slack.alertChannel`
   enables switchboard's outbound HTTP ingress (`ingress_addr: ":8080"`,
   `ingress_allow: [<channel>]`), exposes `Service/switchboard` (`:8080`) with a
   `NetworkPolicy` rule admitting only `ate-system` `atenet-egress`, generates
   `ate-credentials/switchboard-ingress` (`token`) once and mirrors it to
   `switchboard/switchboard-agent` (`ingress-token`), and configures
   core-agent's `alert` tool (`template: "switchboard"`) over a plain-HTTP
   `http://switchboard.switchboard.svc.cluster.local` egress rule (`ax`
   `db1c1de`, keeping `ax.proto` wire-compatible with `k8s-lookout` `v0.43.1`).

**Why:** operators talk to `@shipmind-<env>` in one thread — whether asking a
cluster question, requesting a GitOps PR, or replying on an incident post —
without switching agents via slash commands. Executing GitOps work inside
`propose-change` (`mcp: []`) keeps raw manifest reads and diffs out of the main
conversation and incident context; exempting synthetic `shipmind.dev/scenario`
probe workloads in `AGENTS.md` keeps nightly e2e scenarios within their 10-call
trajectory ceiling and out of Slack alert channels, while `propose-change`'s
`vcs_proposal_list` dedup, fast-exit, and unknown-secret guards bound real
incidents. Using `"http://<host>"` on `EgressRule.hosts` routes
task-to-switchboard webhook calls through Substrate's egress credential injector
without breaking `k8s-lookout`'s strict `protojson.Unmarshal` on `task.yaml`.

**Rejected:** keeping a second `propose` switchboard agent (forces users to
know `/shipmind-<env> agent propose` and breaks replying directly in an
incident alert thread); adding a new `bool http` field to `ax.proto`
(`k8s-lookout` `v0.43.1` rejects unknown fields in `task.yaml` at startup);
posting to Slack via raw Incoming Webhook URLs instead of switchboard's
`/v1/messages` ingress (bypasses switchboard's thread routing and puts a
webhook secret in URL config).

## 2026-11-17: A separate switchboard "propose" agent opens GitOps pull requests

When an environment sets `agent.vcs` (`secret` in `ate-credentials` plus
`repositories`) alongside `agent.chatImage`, `shipmind up` registers a second
switchboard agent (`propose`, `/etc/switchboard/propose/propose.yaml`) beside
`triage`, writes `propose-config.json` into the environment's ax Workspace, and
mounts the optional `propose.yaml` Secret item into switchboard. `images/agent`
builds `/opt/core-agent/recipes/gke-propose` from `gke-chat` with
`@include builtin:propose-change` and checks at build time that
`core-agent-gke-propose.json` matches the imported `gke-chat` config.

`propose` runs on the same distroless nonroot `images/agent` (`debug: false`)
with `read_file`, `edit_file`, `write_file` and `grep` enabled alongside
core-agent's pure-Go `vcs_*` tools; `edit_file`, `write_file`, `vcs_commit`,
`vcs_publish` and `vcs_propose` stay in `ask` mode so switchboard relays each
mutation as an approval prompt in the Slack thread (`standing_approvers:
["nobody"]` disables persistent grant writes against the read-only image
recipe directory, while `CORE_AGENT_TRUSTED_DIRS` trusts the Workspace and
baked recipe in memory). The GitHub token stays
outside the actor in `ate-credentials/<secret>`: Substrate's egress gateway
injects `Bearer ` on `api.github.com` and `Basic `
(`base64("x-access-token:<token>")`) on `github.com`.

**Why:** keeps `triage` (`default_agent`) read-only with file tools disabled
(matching 2026-11-05 "The incident agent triages; specialists get briefs"),
keeps repository names and Secret names in the private overlay rather than
`images/agent`, and needs no `git` binary, shell or sandboxed token.

**Rejected:** enabling file and `vcs_*` tools on `triage` itself (every chat
thread would carry file-mutation and forge schemas and egress to GitHub);
baking repository coordinates into `images/agent` (violates the public-repo
rule); passing the GitHub token in task `env` instead of Substrate egress
injection.

## 2026-11-17: images/agent runs on distroless nonroot

`images/agent` moves its runtime stage from `debian:bookworm-slim` (root,
with `git` and `curl` installed) to
`gcr.io/distroless/static-debian12:nonroot` (`USER 65532:65532`), matching
core-agent's own release image and `images/e2e-runner`.

**Why:** the Debian base carried `git` for repository clones and `curl` for
poking at core-agent over `ax ssh`. `ax-task-runner` execs `core-agent`
directly without `/bin/sh`, shipmind's Workspaces use `spec.files` rather
than `spec.git`, core-agent's `vcs_*` tools use pure-Go `go-git` in process,
and lookout, switchboard and the scenario runner all reach the agent over its
HTTP session API through Substrate's router rather than `ax ssh`. Dropping
the Debian userland removes `/bin/sh`, `apt`, `git`, `curl` and root inside
the actor.

**Rejected:** keeping `debian:bookworm-slim` for interactive `ax ssh`
debugging (session state, tool calls and prompts are already observable over
the attach HTTP API, and any code-execution bug inside the actor would inherit
root plus a shell and `curl`); a separate debug image variant (two images to
build and pin for one binary, rejected in 2026-11-05 "Two agent profiles,
autonomous and interactive, one image").

## 2026-11-07: The scenario judge is Claude Sonnet 5.5, on a fixed rubric

Nightly scenario answers are graded by `claude-sonnet-5-5` on Vertex AI at
high effort, against each scenario's `expect.diagnosis`, with core-agent's
response-quality rubric copied in at ae922c2b (#90). The grade is reported,
not gated, until there is a baseline.

**Why:** a judge from another model family than the agent's (Gemini) can't
favour its own answers. Before choosing, both Claude Sonnet 5.5 and Opus
5.5 graded the 14 archived runs and 10 hand-written answers with known
flaws (`e2e/calibration/`), twice each, with the rule set beforehand: take
Sonnet if it does as well as Opus. Both put all 48 grades in the expected
band and gave the same score both times; Sonnet costs about $0.006 a grade
to Opus's $0.012. The rubric is copied rather than imported so a trend
moves only when the agent does: changing the judge model or the rubric
means re-grading the archive (#91) and a new decision here, not an upgrade.

**Rejected:** Opus 5.5 (no better on this set, at twice the cost; worth
re-running the calibration against when scenarios grow harder, #15);
Gemini Pro (the agent's own family); importing core-agent's `Judge` (it
sends a temperature, which Claude 5.5 models reject, and would move with
core-agent releases).

## 2026-11-06: core-agent is the agent harness; mast is maintenance-only

New agent work goes to core-agent only: specialists (deep dives, issues and
pull requests), scenarios, identity. mast runs what it runs today, prod's
incidents and dev's until core-agent 3.1 GA, and gets fixes but no new
features. When prod moves, `images/agent-mast`, the mast task template and
the `mast` pin are deleted; `agent.chatImage`/`agent.incidentImage` become
the permanent per-profile settings; the bundles in `agents/sre-triage`
can then become native core-agent recipes instead of imported ones.

**Why:** on staging both paths run on core-agent, every scenario run has
passed, and core-agent's single-agent triage costs about half mast's per
incident ($0.016 to $0.040 against $0.039 to $0.073, 2026-11-05/06). The
three core-agent issues found on the way (#1497, #1498, #1500) are fixed
in v3.1.0-rc.2. mast's own plan (go-steer/mast#596, go-steer/core-agent#1411)
keeps it supported only until shipmind has moved.

**Rejected:** keeping both harnesses current (two bundle formats and two
test baselines for one agent); moving prod before its Substrate cutover,
which is best done once, to Substrate 1.0 GA.

## 2026-11-05: The incident agent triages; specialists get briefs

The incident profile's main agent holds the read tools and diagnoses
lookout's signal itself; specialists are for what comes after triage:
deep dives in one domain, and actions with side effects (issues and pull
requests), which stay separate from the readers and behind approval.
Today that means `gke-triage-f`'s one specialist is folded into the main
agent at build time (`core-agent-gke-triage-f.fold-specialist`).

**Why:** core-agent hands a subagent only the request the model writes,
so delegating a raw signal means re-typing it, and output tokens are the
dearest (2.1k for one lookout incident, measured). A handoff after triage
carries the agent's own finding, which is short. mast's coordinator
existed to route among specialists and to keep the one that changed the
cluster apart; with one read-only specialist it was only overhead, and
the capability split returns with the action specialists.

**Rejected:** a brief-only delegation (the specialist would re-fetch, or
lose, what lookout already gathered: event counts and times, the
enrichment bundle); porting mast's built-in coordinator instruction (it
framed every message as an incident). A core-agent way to hand a
subagent the triggering message verbatim is requested upstream.

## 2026-11-05: Two agent profiles, autonomous and interactive, one image

lookout's incidents and switchboard's threads stay one image (core-agent
and the ax runner) with two profiles that differ in configuration on
purpose: their bundles, overlays and the main agent's tools. Incidents
are mast's shape, where the main agent only delegates and the specialist
reads the cluster; chat's main agent also holds `list_clusters` and
`get_cluster`. `agent.chatImage` and `agent.incidentImage` become
permanent per-profile settings when mast goes, rather than being deleted.

**Why:** the binaries are the same; what should differ (who approves,
budgets, sessions, tools) is configuration, and two images would be two
builds and two pins of one binary. Splitting the image waits until the
contents differ (tools needing other binaries, a hardened base).

**Rejected:** separate images now; keeping the importer's layout, where
the main agent holds every MCP tool (go-steer/core-agent#1498): it read
clusters itself instead of delegating, and every call carried the tool
schemas.

## 2026-11-03: lookout's incidents follow, by their own setting

`agent.incidentImage` moves lookout's incidents to core-agent the way
`agent.chatImage` moved switchboard's threads, rendering the same
core-agent template with `gke-triage-f`. Both settings go once every
environment runs core-agent: `agent.image` then names images/agent, and
the mast template and `agent-mast` are deleted.

**Why:** an environment can move each use on its own and back, and the
nightly scenario, which drives an incident end to end, is the check for
this one. lookout needed no change: its ax sink was written against
core-agent's session API. The scenario did: core-agent wraps an injected
message in an `[Inbox]` bundle and can finish an investigation in a
later turn. Moving either way applies to new incident tasks only: ax
tasks are immutable, and lookout reuses an incident's task when it
reopens, so delete old ones to move them.

**Rejected:** switching `agent.image` itself to core-agent (it would
move every environment's incidents and threads together, and prod and
development are still on mast); choosing the harness from the image's
name.

## 2026-11-03: switchboard's threads move to core-agent first, without per-person identity

`images/agent` builds core-agent (#48) with the ax runner, and converts
`agents/sre-triage`'s mast bundles at build time with that core-agent's
`config import-workload`. An environment that sets `agent.chatImage` runs
switchboard's threads on it; lookout's incidents stay on mast and
`agent.image`. The chat bundle's attach token authenticates as
`switchboard` (`token_identity`), which owns every thread's session; it
may assert no one (no `proxy_identities`), and switchboard's
`assert_caller` stays off. Setting or changing `chatImage` changes
switchboard's task template, so switchboard (`replace_drifted`) replaces
each thread's task on its next message and the conversation starts over.

**Why:** converting at build time keeps one source for each bundle while
mast still runs them, and an importer regression fails the image build
instead of the agent. Per-person identity waits because core-agent's
sessions belong to their creator and switchboard adds no contributors:
with `assert_caller` on, only the person who started a thread could talk
in it, and everyone else would get "session not found".

**Rejected:** committing converted copies of the bundles (two sources
that drift until mast is gone); moving lookout in the same step (its
nightly scenarios are the comparison the move needs); turning on
`assert_caller` now; listing `switchboard` in `proxy_identities` ahead
of it (in `token_identity` mode that lets the token assert any name,
which nothing would use yet).

## 2026-11-01: The private repository is go-steer/shipmind-deploy

The private companion repository (2026-10-20, "Public repository,
private environment overlay") is renamed from
`go-steer/shipmind-agent-substrate` to `go-steer/shipmind-deploy`.

**Why:** it was named after its first environment, the agent-substrate
cluster, and now holds all of ours (agent-substrate, shipmind-stage,
shipmind-prod) with their setup scripts and runbooks. GitHub redirects
the old name.

## 2026-10-21: switchboard's ax backend targets v0.9, not v0.8

Milestone 3's switchboard ax backend now targets switchboard v0.9.
switchboard v0.8.0 shipped without it, carrying the admin API and the
gateway's own identity instead. Its design is go-steer/switchboard
`docs/ax-backend.md`, tracked in go-steer/switchboard#168, with the
end-to-end here as #39.

In that design an ax agent is a task template with a scope: a task per
thread, per owner, or one shared. switchboard creates the task, resumes it
and waits for readiness. It then speaks the unchanged session contract
through the atenet router, as lookout's ax sink does.

**Why:** nothing for the ax backend had been started when v0.8.0 was cut,
and shipmind wanted that release for what it does carry. Two things the
backend depends on landed in the ax fork only recently: a task serving its
own API (`spec.http.port`) and the readiness hold.

**Consequence:** the backend scopes switchboard's streams to the turn,
because an open SSE stream keeps a task from idle-suspending. When
Milestone 3 deploys it, this profile needs the router's raised parking
budget kept, a rendered task template for the gateway's agent, and a
NetworkPolicy letting switchboard reach ax-server and the router.

## 2026-10-20: The agent's task template is rendered at deploy time

The ax task template, which carries the agent's tokens, is rendered by
`shipmind up` into a Secret from a template in this repository and the
tokens already in the cluster; it is never committed, to this repository
or the overlay. Tokens are adopted from a pre-shipmind deployment and
never rotated by a deploy. `shipmind status` compares the template and
the ax Workspace by value, so "no drift" covers the whole environment.

**Why:** ax has no secret references for env, so the tokens must be in
the template; tasks are immutable and carry the tokens they were created
with, so rotating them would cut lookout off from every open incident.

**Rejected:** a kustomize secretGenerator from an untracked file (tokens
on a workstation that reboots daily); Secret Manager (a new dependency for
two demo tokens; revisit for a production environment); committing the
template encrypted.

## 2026-10-20: The agent image reproduces what runs, gke-triage-f included

`images/agent-mast` rebuilds the running agent image (mast-triage:v13)
from pins: mast and ax's task runner built from the commits in
`versions.yaml`, and the bundles in `agents/sre-triage` byte for byte as
they run. That includes `gke-triage-f`, the October experiment the task
template uses, and the routed `gke-triage` copy with the stock
`diagnoser` specialist that v13's build left in it. The bundles are kept
out of license-header normalization, which would change them.

The image is not in `versions.yaml` until CI has published it: the entry
then names this repository at the commit that built it, with the image
digest, so `shipmind check`'s rules (a full SHA, a digest) apply to it
like any other component.

**Why:** the cutover moves the cluster to shipmind's build of the image.
If the image also changed the agent's bundle, a difference in the
agent's answers after the cutover couldn't be told apart from a build
difference.

**Rejected:** shipping mast's stock `gke-triage` now. It has absorbed
most of `gke-triage-f`, but it still differs (the read-only endpoint, the
change-executor budgets, the specialist), and that is a behavior change
for scenario runs to decide (#20).

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
