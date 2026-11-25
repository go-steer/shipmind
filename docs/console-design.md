# shipmind admin console design

**Status:** design (2026-11-20) · **Tracks:** [#128](https://github.com/go-steer/shipmind/issues/128) · **Companion to:** [`DESIGN.md`](./DESIGN.md), [`decisions.md`](./decisions.md)

## TL;DR

**Problem:** `shipmind.yaml` is the right declarative artifact for GitOps storage and `shipmind up` / `shipmind status`, and the wrong day-to-day operator surface. Once an environment adds live cluster scopes ([#104](https://github.com/go-steer/shipmind/issues/104)), GitOps VCS repositories and approval policies ([#118](https://github.com/go-steer/shipmind/issues/118), [#121](https://github.com/go-steer/shipmind/issues/121)), Slack alerting, and scheduled `lookout audit` + `leeway report` posture sweeps ([#127](https://github.com/go-steer/shipmind/issues/127)), operators have to stitch together `kubectl logs`, `ax`, Secret Manager, Slack, and GCS by hand. Silent integration faults stay invisible until an incident misses its alert, and external adopters get no turnkey control plane on Day 0.

**Decision:** Build `shipmind`'s first-party Admin Console **in-tree** in `go-steer/shipmind` (`shipmind console` CLI subcommand + `images/console`, published to `ghcr.io/go-steer/shipmind/console` and pinned by digest in `versions.yaml`), deployed by both the `standalone` and `substrate` profiles. Component-owned telemetry and admin endpoints stay in their component repositories (`purser`, `k8s-lookout`, `switchboard`, `mast-web`) and enter `shipmind` via `versions.yaml` pin bumps.

**Four platform invariants:**

1. **Zero GCP lock-in across both profiles (`standalone` `#SM-2` and `substrate` `#SM-1`):** Runs on `kind`, bare-metal Kubernetes, EKS/AKS, or GKE with in-cluster SQLite persistence (`1Gi` PVC), optional GCS scenario archival ([#88](https://github.com/go-steer/shipmind/issues/88)), embedded vendored `mast-web` in both profiles, and strict never-wake-on-read actor inspection + `substrate-scope` deep links on `substrate`.
2. **Self-describing cluster state (`shipmind-system/shipmind-release` ConfigMap):** `shipmind up` stamps a sanitized, non-secret `ConfigMap` carrying the effective `shipmind.yaml`, active profile BOM (`versions.yaml`), `shipmind` commit SHA, and rendered object digest inventory (`< 32 KiB`), giving the in-cluster console a complete drift baseline without access to the deployer's workstation or private Git overlay.
3. **Portable authentication (`go-steer/purser`):** Works on Day 0 over loopback (`shipmind console <env-dir>`) or bearer-token port-forward with zero DNS, Ingress, or cloud IAM setup, and scales to multi-user Ingress deployments through `purser` (`oidc`, `proxy-header`, `iap-jwt`), forwarding verified operator identity (`X-Asserted-Caller`) on session attach, approvals, and day-2 actions.
4. **Drift-free three-tier configuration write path (`shipmind.yaml`):** Declarative settings are never patched onto cluster objects behind `shipmind status`. Edits apply locally via `shipmind up` (Tier 1), export as a validated diff + `shipmind up` command when no VCS is configured in-cluster (Tier 2), or open a reviewed pull/merge request against `console.overlayRepo` via pure-Go `go-steer/core-vcs` (Tier 3), while ephemeral day-2 runtime actions execute live with caller audit records.

---

## Problem and operational evidence

Operating `shipmind` across `agent-substrate`, `shipmind-stage`, and `shipmind-prod` exposed three structural gaps that no single component UI can close:

1. **Silent integration faults hide in pod logs.** On `shipmind-stage` (2026-11-20), two broken integrations ran unnoticed while every pod reported `1/1 Running` and `shipmind status` exited `0`:
   - `switchboard` failed every outbound `:8080` `/v1/messages` alert from autonomous incident tasks because `slack.alertChannel` (`CDWTP8NFL`) returned `404 channel_not_found`.
   - `lookout-ax` ran `--sources=auto` and silently disabled `source capacity` at startup (`skipped: missing get on kube-system/cluster-autoscaler-status`) because `deploy/14-role-watcher-capacity.yaml` was not vendored.
   Neither fault surfaced anywhere short of tailing container stderr.
2. **Fragmented operator surfaces.** `substrate-scope` renders Substrate actors and workers in 3D (and does not exist on `standalone`), while `mast-web` attaches to a single agent's HTTP session endpoint. Neither knows the environment's bill of materials (`versions.yaml`), live configuration drift, Secret sync freshness, `lookout` incident/watchboard queues, `switchboard` chat thread bindings, scheduled posture findings ([#127](https://github.com/go-steer/shipmind/issues/127)), or scenario eval scorecards ([#93](https://github.com/go-steer/shipmind/issues/93)).
3. **External adopters lack a Day-0 control plane.** A team evaluating `shipmind` on `kind` (`standalone` profile) or a non-GCP cluster (`substrate` profile) has no GKE IAP, no GCS bucket, no Google Secret Manager, and often no automated GitOps pipeline wired on Day 0. Requiring cloud-specific auth or external object storage before the UI renders blocks adoption at install time.

### Why in-tree in `go-steer/shipmind`

`shipmind` holds no component source code (`AGENTS.md`), and that rule stays intact: fixes or new status routes in `k8s-lookout`, `switchboard`, `purser`, `mast-web`, or `core-agent` land in those repositories and enter `shipmind` by moving a pin in `versions.yaml`.

The Admin Console itself is different: like `cmd/shipmind`, `internal/deploy`, and `images/e2e-runner`, it is `shipmind`'s distribution control plane and has no use outside `shipmind`. Keeping `internal/console` and `images/console` in `go-steer/shipmind` (recorded in [`decisions.md`](./decisions.md)) guarantees:

- **Zero schema skew:** Whenever `shipmind.yaml` (`internal/env`), `versions.yaml` (`internal/bom`), drift rules (`internal/deploy`), or archive records (`internal/archive`) gain a field, Config Studio and the health/scorecard panes update in the **same PR** and test run.
- **Single binary, two topologies:** One `shipmind` binary powers both local Day-0 workstation operation (`shipmind console <env-dir>`) and distroless in-cluster serving (`shipmind console -in-cluster` in `images/console`).

---

## 1. Execution modes and packaging {#CON-1}

One subcommand (`shipmind console`) serves the same embedded web application and JSON/SSE API across two execution modes:

```
┌──────────────────────────────────────────────────────────────────────────────────┐
│ Browser: 5 Core Panes + Embedded Vendored mast-web (/mast-web/...)               │
└───────────────────────────────────────┬──────────────────────────────────────────┘
                                        │ Same-origin HTTPS / Loopback HTTP
                                        ▼
┌──────────────────────────────────────────────────────────────────────────────────┐
│ shipmind console (purser authn/authz + CSRF + Host rebinding guard)              │
│  • Local mode:      shipmind console [-root .] [-addr 127.0.0.1:8080] <env-dir>  │
│  • In-cluster mode: images/console (-in-cluster, Namespace: shipmind-system)     │
└───┬──────────────┬────────────────┬──────────────────┬───────────────────────┬───┘
    │              │                │                  │                       │
    ▼              ▼                ▼                  ▼                       ▼
Release/Drift   k8s-lookout     switchboard     Agent Attach Proxy      Posture & Evals
• Local: <env>  • /v1/sources   • /v1/status    • standalone:           • console.db
  + Status/Up   • /v1/incidents • /v1/threads     core-agent :7777        (SQLite PVC)
• In-cluster:   • /v1/ack       • :8080 test    • substrate:            • CronJob / Job
  ConfigMap     • /v1/triage      alert           ax-server (:8080) +     runner (#127)
  shipmind-       -status                         ateapi (:443) +       • Optional GCS
  release                                         atenet-router (:443     archive (#88)
                                                  mTLS, never-wake)
```

### CLI surface

```text
shipmind console [-root .] [-addr 127.0.0.1:8080] [-any-ref] <env-dir>
shipmind console -in-cluster [-addr :8080] [-release-dir /etc/shipmind/release] [-store /var/lib/shipmind-console/console.db]
shipmind console token <env-dir>
```

| Mode | How it reads environment state | How it reaches cluster services | Write path for `shipmind.yaml` |
|---|---|---|---|
| **Local workstation** (`shipmind console <env-dir>`) | Loads `<env-dir>/shipmind.yaml` (`internal/env`) + renders `<env-dir>/kustomization.yaml` against `-root` (`internal/overlay`) and `versions.yaml` (`internal/bom`). Always passes `shipmind.yaml`'s explicit `context` — never implicit current kubeconfig context. | Managed SPDY/WebSocket port-forward tunnels via `context` (avoiding `kube-apiserver` `/services/.../proxy`'s 60s request timeout so `30m` SSE streams stay open). On `substrate` v1.0+, where `atenet-router` requires a `podidentity.podcert.ate.dev` X.509-SVID that a workstation lacks, live `mast-web` attach tunnels through the in-cluster `shipmind-console` pod authenticating with `Secret` `shipmind-system/shipmind-console-auth`'s machine bearer token (accepted alongside any Ingress SSO mode, asserting `local:<os-user>`), falling back to `-router http://...` only on plaintext dev stands. | **Tier 1:** Validates in-process, runs `shipmind up -dry-run`, writes `<env-dir>/shipmind.yaml`, and runs `shipmind up`. |
| **In-cluster** (`images/console`, `-in-cluster`) | Reads mounted `ConfigMap` `shipmind-system/shipmind-release` (`/etc/shipmind/release`) and live cluster objects via in-cluster `ServiceAccount` `shipmind-system/shipmind-console`. | Direct ClusterIP DNS (`lookout-ax`, optional `switchboard`, `core-agent` on `standalone`, `ax-server` + `api.ate-system` + `atenet-router` over `purser` PKI mTLS on `substrate`). | **Tier 2** (diff + CLI export) or **Tier 3** (`core-vcs` PR against `console.overlayRepo`). |

`shipmind console token <env-dir>` reads `Secret` `shipmind-system/shipmind-console-auth` using `<env-dir>`'s `context` and prints the Day-0 bearer token to a TTY (never logging it in `up` or `status`), plus the `kubectl -n shipmind-system port-forward svc/shipmind-console 8080:80` command.

### Container image (`images/console`) and profile manifests

- **Base image and build:** `images/console/Dockerfile` compiles `./cmd/shipmind` with `CGO_ENABLED=0` (using pure-Go `modernc.org/sqlite` for SQLite persistence) onto `gcr.io/distroless/static-debian12:nonroot` (`USER 65532:65532`, `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`, `capabilities.drop: ["ALL"]`). Unlike `images/e2e-runner`, `images/console` carries **no** `kubectl`, `ax` binary, `git` binary, `curl`, or shell — Kubernetes API calls use Go HTTP/JSON (`internal/cluster` REST client), `ax` uses generated gRPC stubs, and VCS proposals use pure-Go `core-vcs`.
- **CI publication and BOM pinning:** `.github/workflows/images.yml` builds and pushes `ghcr.io/go-steer/shipmind/console:<shipmind-sha>` on `main` pushes touching `cmd/**`, `internal/**`, or `images/console/**`. Following the `images/agent` and `images/e2e-runner` rule in [`decisions.md`](./decisions.md), `console` enters `versions.yaml` (`profiles: [standalone, substrate]`) once CI publishes its first digest, pinned by 40-character commit SHA and `@sha256:<digest>`.
- **Deployment differences by profile:**

| Resource / capability | `deploy/profiles/standalone/console` (`#SM-2`) | `deploy/profiles/substrate/console` (`#SM-1`) |
|---|---|---|
| Namespace & workload | `shipmind-system`, `Deployment/shipmind-console` (1 replica, `Recreate` strategy for RWO PVC), `Service/shipmind-console` (`ClusterIP` `80 -> 8080`), `PVC/shipmind-console-store` (`1Gi` RWO), and `/tmp` `emptyDir` (`sizeLimit: 64Mi` for ephemeral `core-vcs` working copies under `readOnlyRootFilesystem: true`). | Same base workload, `Service`, `1Gi` RWO PVC, and `64Mi` `/tmp` `emptyDir`; `Namespace/shipmind-system` carries label `ate.dev/api-access=true`. |
| Agent & Substrate credentials | Mounts `Secret` `shipmind-system/shipmind-console-agent` (`console-token`, `fallback-token`, and optional `ingress-token` mirrored when `slack.alertChannel` is set). | Mounts `shipmind-console-agent` plus projected `ate-token` (`audience: api.ate-system.svc`), `podidentity` (`podidentity.podcert.ate.dev/identity` ECDSAP256 bundle), and `servicedns-ca` (`servicedns.podcert.ate.dev/identity` live trust bundle). Registered in `TestSubstrateRouterClientsMountMTLS` (`internal/render/profiles_test.go`). |
| Required Substrate grants | None. | Global `viewer` role in Substrate access policy for `user:system:serviceaccount:shipmind-system:shipmind-console`; `spiffe://cluster.local/ns/shipmind-system/sa/shipmind-console` added to `ate-setup`'s `ATE_INGRESS_ALLOWED_SPIFFE_IDS`. |
| Kubernetes RBAC | Least-privilege `ClusterRole` (`get`/`list` Namespaces, Deployments, DaemonSets, StatefulSets, Pods, CronJobs, Jobs, Services, NetworkPolicies; **zero** `secrets` permissions outside `shipmind-system`) + `Role` in `shipmind-system` (`get` `ConfigMap/shipmind-release`, `create`/`get`/`list`/`watch` audit/scenario `Jobs` from `CronJob` templates). | Same Kubernetes RBAC, plus `Role` in `ax-workers` (`get`/`list` worker Pods for node/pool correlation, no `delete`). |

> **Security boundary on Kubernetes Secrets:** `ServiceAccount` `shipmind-system/shipmind-console` is **never** granted cluster-wide `get`/`list` on `Secret` resources (which in Kubernetes is cluster-admin equivalent, exposing `lookout-ax/agent-tokens`, `ate-credentials/*`, and `ax-postgres`). The console mounts only its own scoped Secrets (`shipmind-console-auth`, `shipmind-console-agent`, and optional `console.overlayRepo.secret`). Secret sync metadata for Pane 1 is carried as non-secret digests inside `shipmind-release` (below).

---

## 2. In-cluster release metadata (`shipmind-release` ConfigMap) {#CON-2}

Today `shipmind.yaml` and the overlay's `kustomization.yaml` pin exist only on the deployer's filesystem. To make an in-cluster console self-contained without mounting Git credentials or requiring GCP access, `shipmind up` writes (and `shipmind status` diffs) a deterministic, non-secret `ConfigMap` `shipmind-system/shipmind-release`.

### `ConfigMap` schema and byte budget

Kubernetes enforces a hard `1 MiB` (`1,048,576` bytes) ceiling per `ConfigMap`. Raw rendered manifests across CRDs and RBAC roles grow quickly, so `shipmind-release` stores canonical configuration plus a compact per-object SHA-256 inventory (`~6–16 KiB` typical, capped at `256 KiB` by `internal/deploy`):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: shipmind-release
  namespace: shipmind-system
  labels:
    app.kubernetes.io/managed-by: shipmind
data:
  shipmind.commit: "367136a..."          # 40-char go-steer/shipmind commit SHA from overlay ref
  shipmind.profile: "substrate"          # "substrate" | "standalone"
  shipmind.yaml: |                       # canonicalized via env.SanitizeForRelease(p.Env)
    context: gke_demo_us-central1_stage
    agent:
      atespace: shipmind-stage
      ...
  versions.yaml: |                       # active profile subset of versions.yaml
    components:
      ...
  rendered-inventory.json: |             # deterministic sorted inventory, zero secret values
    {
      "schema_version": 1,
      "objects": [
        {"apiVersion": "apps/v1", "kind": "Deployment", "namespace": "lookout-ax", "name": "lookout-ax", "spec_sha256": "..."}
      ],
      "workspace": {"atespace": "shipmind-stage", "name": "gke-triage", "spec_sha256": "..."},
      "secrets": [
        {"namespace": "lookout-ax", "name": "lookout-ax-agent", "keys": ["task.yaml", "token"], "source": "generated"},
        {"namespace": "switchboard", "name": "switchboard-slack", "keys": ["app-token", "bot-token"], "source": "secret-manager"}
      ],
      "startup_readers": [
        {"kind": "secret", "namespace": "lookout-ax", "name": "lookout-ax-agent", "deployment": "lookout-ax"}
      ]
    }
```

### Sanitization and drift rules

1. **Pure deterministic render (`shipmind status` & `up -dry-run` safe):** Every key in `shipmind-release` is a pure deterministic function of the overlay render (`p.Ref`, `p.Env`, `versions.yaml`, and rendered object spec hashes) with **zero wall-clock timestamps and zero live cluster UIDs**. `shipmind-release` therefore participates in `shipmind up` (`restYAML`), `shipmind up -dry-run`, offline profile tests (`internal/render/profiles_test.go`), and `shipmind status`'s `kubectl diff` without producing false exit-3 drift on subsequent `status` runs.
2. **Zero secret material in `shipmind-release`:** `shipmind.yaml` already forbids raw credentials (`secrets` names Secret Manager secret IDs, `agent.vcs.secret` names an `ate-credentials` Secret). Even so, `env.SanitizeForRelease(*Env)` round-trips through `Env` with `KnownFields(true)` and runs a secret-shape scanner (`ghp_`, `xox`, `AIza`, `ya29.`, PEM headers, bearer tokens) before serialization so an accidental token pasted into `description` or `expect[].reason` fails `shipmind up` before hitting the API server.
3. **In-cluster health & sync timestamp with zero cross-namespace `Secret` RBAC:** `shipmind up` reconciles all managed/generated Secrets and restarts any stale `startupReaders` before rollout completion (`internal/deploy/deploy.go`). In-cluster `shipmind-console` reads `ConfigMap/shipmind-release`'s own Kubernetes `metadata.managedFields[*].time` (set server-side on `shipmind up`) as the last-reconciled timestamp and checks `Deployment` rollout completion, container `imageID` digests against `versions.yaml`, `substrateStatus`, and `substratePatchStatus` (`atelet` `nodeSelector` and `atenet-router` `--parked-request-budget=60s`) with **zero `Secret` RBAC** outside `shipmind-system`, while local mode (`shipmind console <env-dir>`) additionally runs live `deploy.Status` against Secret Manager on demand.

---

## 3. Authentication, authorization, and caller identity (`purser`) {#CON-3}

Every console listener runs behind [`go-steer/purser`](https://github.com/go-steer/purser) (`purser.Caller`, `httpmw.NewCaller`, `httpmw.NewBrowserWriteGuard`, `httpmw.CheckBind`, and `authz.WithRules`), plus two console-edge browser hardening guards:

- **Strict `Host` allowlist (DNS-rebinding guard, matching `core-agent#1358`):** Day-0 `loopback` and `bearer` modes admit only `localhost`, `127.0.0.1`, `[::1]` (with any port) and `shipmind-console.shipmind-system.svc*` (plus `console.auth.externalURL`'s host when set), answering `421 Misdirected Request` otherwise; `oidc`, `proxy-header`, and `iap-jwt` require `console.auth.externalURL` (`https://...`) and admit only that canonical host (plus cluster-internal CLI tunnel traffic presenting `shipmind-console-auth`'s bearer token).
- **Port-exact `Origin` + `X-Shipmind-CSRF` guard on cookie sessions:** `purser/httpmw.NewBrowserWriteGuard` permits any `localhost:*` port for header-authenticated APIs, which is insufficient once a browser holds an ambient `SameSite=Strict` cookie over `kubectl port-forward` (`http://localhost:<other-port>` is same-site in browsers and would send the cookie). Every cookie-authenticated console request on a mutating method (`POST`, `PUT`, `PATCH`, `DELETE`) must therefore carry **both** an exact `Origin == <scheme>://<Host>` match (port included, no wildcard loopback port exception) **and** a custom `X-Shipmind-CSRF` header matching an HMAC token bound to the session cookie.

### Four `console.auth` modes

| Mode | Where used | Credential verified | Browser session transport | Setup required |
|---|---|---|---|---|
| `loopback` | Default for `shipmind console <env-dir>` | Local socket admission (`httpmw.IsLoopback` enforced; non-loopback `--addr` refused at startup by `httpmw.CheckBind`). Caller resolves to `local:<os-user>` (`Admin: true`). | Same-origin loopback + per-process `X-Shipmind-CSRF` header + `Host` rebinding guard. | **None** (Day 0). |
| `bearer` | Default in-cluster (`images/console`) | 32-byte hex token in `Secret` `shipmind-system/shipmind-console-auth` (generated once by `shipmind up`, never rotated on redeploy), verified via `purser/authn/bearer` (SHA-256 constant-time table). Requires no `console:` block in `shipmind.yaml`. | Operator pastes token once at `/login`; `POST /api/v1/auth/session` sets an `HttpOnly; SameSite=Strict; Path=/` HMAC-signed cookie (`Secure` when HTTPS) paired with `X-Shipmind-CSRF` so `mast-web` `EventSource` SSE streams never pass tokens in query strings. `Authorization: Bearer` header remains accepted on all modes for API calls and `shipmind console <env-dir>` CLI tunnels. | **None** (`kubectl port-forward` works immediately on `kind` and GKE). |
| `oidc` | Multi-user Ingress / Gateway TLS | OpenID Connect ID token verified by `purser/authn/oidc` (`RS*`/`PS*`/`ES*`/`EdDSA` allowlist, exact-key claim lookup, `email_verified` required, 30s JWKS refetch floor). Requires `externalURL: https://...`. | Authorization Code + PKCE callback at `/auth/callback` storing signed `SameSite=Lax` (for IdP redirect) -> `SameSite=Strict` session cookie bounded by `min(id_token.exp, 12h)` + `X-Shipmind-CSRF` + port-exact `Origin == externalURL`, or direct `Authorization: Bearer <id_token>`. | OIDC client ID/secret in K8s `Secret` + `console.auth.oidc` in `shipmind.yaml`. |
| `proxy-header` / `iap-jwt` | Reverse-proxy SSO (`oauth2-proxy`, Envoy `ext_authz`, Teleport) or GKE IAP | `purser/authn/proxyheader` (explicit header name, e.g. `X-Auth-Request-Email`, gated by trusted peer CIDR or mTLS client cert so pod-network peers cannot spoof headers) or `purser/authn/iap` (ES256 JWT verification on `X-Goog-IAP-JWT-Assertion` with mandatory `audience`). Requires `externalURL: https://...`. | Verified per request from proxy header / IAP JWT + port-exact `Origin == externalURL`. | Ingress proxy or GKE BackendConfig IAP + `console.auth` in `shipmind.yaml`. |

### RBAC roles (`purser/authz.Rules`)

`console.auth` maps authenticated `purser.Caller` identities (`email:alice@example.com`, ASCII-folded `domain:example.com`, `id:spiffe://...`, or `claim:<key>=<val>`) onto two roles:

- **`viewer`:** May read all five panes, inspect `lookout` incidents, read `mast-web` session transcripts and tool calls (`httpmw.ReadOnly` on the attach proxy), view posture findings and scenario scorecards, and validate/export `shipmind.yaml` diffs in Config Studio. Cannot wake suspended actors, approve/deny tool gates, send messages into sessions, trigger audits, mute findings, delete tasks, send test alerts, or open `core-vcs` pull requests.
- **`admin`:** All `viewer` permissions plus interactive `mast-web` session injection and permission responses, explicit actor wake (`POST /api/v1/ax/tasks/{atespace}/{task}/wake`), day-2 runtime actions, and Tier 3 `core-vcs` overlay pull request creation. Loopback and Day-0 `bearer` modes grant `admin` to the holder of the local socket or `shipmind-console-auth` token (with audit records stamped `local:<user>` or `token:console-admin`, matching `switchboard` `adminAuthToken`).

### End-to-end `X-Asserted-Caller` contract with `core-agent`

Forwarding `X-Asserted-Caller: alice@example.com` onto `core-agent`'s attach API requires coordinated upstream `core-agent` support (§6) and multi-token table wiring in `shipmind up` — `core-agent`'s current configuration rejects both a second token and cross-owner session reads:

1. **Why today's `core-agent` attach config rejects console assertions:**
   - `agents/sre-triage/ax/task-core-agent.yaml.tmpl` passes `--attach-token=CORE_AGENT_ATTACH_TOKEN`, an outer transport gate in `core-agent` (`pkg/attach/auth.go`) that rejects any second bearer token with `401` before multi-session authentication runs. Meanwhile `agents/sre-triage/gke-chat/.agents/config.json` uses `auth.token_identity: "switchboard"` (mutually exclusive with `auth.table_file`), and `core-agent-gke-triage-f.base.json` leaves `multi_session.enabled` off.
   - Even under `table_file` with `proxy_unprovisioned: true`, `core-agent` (`pkg/auth`) materializes unprovisioned `X-Asserted-Caller` identities (`alice@example.com`) with `Admin: false` and no ACL role on `"switchboard"`- or `"lookout"`-owned sessions, causing `GET /sessions` to return `[]` and `/sessions/{sid}/events` or `/perms/respond` to fail with `404 session not found`.
2. **Upstream `core-agent` companion + `users.json` `table_file` wiring:**
   - **Upstream (`go-steer/core-agent`, §6):** Add `proxy_grants` (or complete `purser/authz.Rules` phase-2 integration on `pkg/attach`) allowing a provisioned proxy identity (`"shipmind-console-admin"`, `"shipmind-console-viewer"`) to assert unprovisioned human identities (`X-Asserted-Caller: alice@example.com`) that inherit daemon-admin (full read/inject/approve) or daemon-viewer (read-only across all sessions) session visibility while recording `alice@example.com` as the session contributor/approver.
   - **`shipmind up` token backfill & `users.json` rendering:** `internal/deploy` idempotently backfills `console-admin` and `console-viewer` tokens into `Secret` `lookout-ax/agent-tokens` when absent (never rotating existing `inject` or `attach` keys, so live tasks keep working) and mirrors them (`console-token`, `viewer-token`, plus `fallback-token` = `t.Attach`) into `shipmind-system/shipmind-console-agent`. `internal/agent` enables `attach.multi_session.enabled: true` on both `gke-chat` and `gke-triage-f`, drops `--attach-token` in favor of a `0644` SHA-256-only `users.json` (`table_file` with `proxy_unprovisioned: true`), and registers `t.Attach` (`"switchboard"` / `"lookout"`), `"shipmind-console-admin"`, and `"shipmind-console-viewer"`.
   - **Pre-cutover `ax` tasks & maintenance-only `mast` fallback:** Pre-existing immutable `ax` tasks started before the `table_file` rollout (which answer `401` to `console-token`) and legacy `images/agent-mast` tasks (which reject `X-Asserted-Caller`) are reached via `fallback-token` (`t.Attach`) without `X-Asserted-Caller` (enforcing `viewer` read-only at the console proxy and recording `alice@example.com` in `console.db`'s `audit_events` table).

---

## 4. Embedded `mast-web`, never-wake-on-read, and `substrate-scope` links {#CON-4}

### Vendoring and embedding `mast-web`

- `mast-web` (`github.com/go-steer/mast-web`) is pinned in `versions.yaml` (`profiles: [standalone, substrate]`) at a release tag or 40-character commit SHA.
- `internal/manifests` (`shipmind vendor` and `shipmind check`) gains an explicit `embedDir` target copying `mast-web`'s static SPA files into `internal/console/web/vendor/mast-web/` with a `VENDOR.yaml` SHA-256 lock, and `dev/tools/verify-vendor-fresh` checks `git diff -- deploy/vendor internal/console/web/vendor/mast-web`. Both `internal/console/web/` (the console's own zero-bundler ES modules) and `internal/console/web/vendor/mast-web/` compile into `shipmind` via Go `embed.FS`.
- Neither `go test ./...` nor `images/console` requires Node.js or `npm` at build time, and `shipmind console <env-dir>` serves the complete UI from a single static binary.

### Same-origin attach proxy

Neither `core-agent` nor `mast` emits CORS headers, and `mast-web` (`web/attach-core/client.js`) always requests `GET /config` at the **origin root**. Following `substrate-scope`'s `mastweb.go` pattern, `shipmind-console` serves `/mast-web/` with `Referrer-Policy: same-origin` and answers root `GET /config` (plus `/mast-web/.../config`, with `Vary: Referer` and `Cache-Control: no-store`) by inspecting the same-origin `Referer` path to return `{"mode": "proxy", "api_prefix": "..."}` (returning `404` when `Referer` is not a `/mast-web/` page):

| Profile | Embedded `mast-web` route | Proxied `api_prefix` | Upstream target |
|---|---|---|---|
| `standalone` (`#SM-2`) | `/mast-web/standalone/?session={sid}` | `/api/v1/agents/standalone/attach` | `http://core-agent.<ns>.svc.cluster.local:7777` with `Authorization: Bearer <console-or-viewer-token>` + `X-Asserted-Caller`. |
| `substrate` (`#SM-1`) | `/mast-web/a/{atespace}/{task}/?session={sid}` | `/api/v1/agents/{atespace}/{task}/attach` | `https://atenet-router.ate-system.svc.cluster.local` over `purser` PKI mTLS (`mtls.NewCertificateFileSource` + `mtls.NewCertPoolFileSource` reloaded every 30s) with `ate-target-actor: {atespace}/{task}`, `Authorization: Bearer <console-or-viewer-token>`, and `X-Asserted-Caller`. |

(`?session={sid}` deep-link auto-selection lands in `go-steer/mast-web`, §6.) Agent bearer tokens never enter browser cookies, DOM storage, or network responses. Proxied SSE streams (`GET .../events`, `GET .../perms/stream`) enforce a `30m` max lifetime (`SSE_MAX_LIFETIME`) with automatic `Last-Event-ID` reconnect and flush per frame without response buffering.

### The never-wake-on-read invariant (`substrate` profile)

On Agent Substrate, **any HTTP request sent through `atenet-router` to a suspended actor wakes its gVisor sandbox**, and any request passing through `ax-task-runner`'s `spec.http.port` listener (including `GET /sessions`) **resets `ax`'s `spec.idle.suspendAfter` timer**. Browsing the Admin Console must never wake an idle or completed agent, nor keep running agents awake in background loops:

1. **Control-plane reads never touch `atenet-router`:** Background collectors and all read APIs for Panes 1–5 call **only** `ax-server` (`ListTasks`, `GetTask` over gRPC on `:8080`) and Substrate's control API (`ateapi.v1` `ListActors`, `GetActor`, `ListWorkers` on `api.ate-system.svc.cluster.local:443`). An AST/symbol test (`internal/console/clients/neverwake_test.go`, modeled on `substrate-scope`'s `nevermutate_test.go`) fails `go test` if read-path packages reference mutating `ateapi` RPCs or dial `atenet-router` outside the guarded attach gate.
2. **Double-checked `RUNNING` gate before router pass-through:** Reading `ax-task-runner` status (`/metadata/v1alpha1/ax/status`, which does not reset `ax`'s idle clock) or listing an agent's sessions (`GET /sessions`, which *does* reset `spec.idle.suspendAfter` and is therefore fetched once on explicit operator drawer selection, never polled) runs **only** if the cached collector snapshot reports the Substrate actor `RUNNING` **and** after a synchronous `ateapi.v1.GetActor` call (`2s` timeout) re-confirms `state == RUNNING` immediately beforehand. (Note a narrow TOCTOU window exists if `ax` suspends the actor in the milliseconds between `GetActor` and the router dial; avoiding background router polls makes that race negligible in normal operation, and operators should not attach to synthetic `shipmind.dev/scenario` tasks while a scenario is testing `limits.suspend`.)
3. **Unconditional `409 Conflict` on suspended actors; wake via `POST` only:** If any attach proxy request (`/api/v1/agents/{atespace}/{task}/attach/...`) targets an actor whose live `GetActor` state is not `RUNNING`, the attach proxy **always** refuses before dialing `atenet-router` with `409 Conflict` (`{"error": "actor_suspended", "actor_state": "SUSPENDED", "ax_reason": "IdleSuspended"}`) — **no `GET` request or header on the attach proxy may ever wake an actor** (preserving CSRF safety on `GET`).
   - In the UI, clicking **Open in mast-web** on a suspended task opens a confirmation modal (*"Task `lookout-8f31...` is IdleSuspended. Waking it claims 1 worker slot (500m CPU / 1Gi) for at least `idleSuspendAfter` (3m). Wake and attach?"*). Confirming sends an `admin`, CSRF-guarded **`POST /api/v1/ax/tasks/{atespace}/{task}/wake`** (logged to `audit_events`), which calls `ax-server` `ResumeTask`, polls readiness, and only then opens `/mast-web/a/{atespace}/{task}/`. If the tab later sits open after the actor suspends again, `mast-web` reconnects hit `409` and stop cleanly instead of waking the actor.

### Deep-linking to `substrate-scope`

Under `substrate`, every task row in Pane 3 renders **View in substrate-scope** when `substrate-scope` is present in the profile (using `console.substrateScopeURL` when configured for Ingress, or local port-forward URL when unset), linking to `<scope-url>/#agent=<encodeURIComponent(atespace + "/" + task)>` (matching `substrate-scope`'s `window.location.hash` selector), while Pane 1's worker pool card links to `<scope-url>/?layout=decks` for the 3D worker/agent deck view.

---

## 5. Drift-free three-tier configuration write path and day-2 actions {#CON-5}

### Why live cluster config patching is prohibited

`shipmind up` applies manifests client-side, renders `lookout-ax/lookout-ax-agent` and `switchboard/switchboard-agent` Secrets from `shipmind.yaml`, and reconciles the `ax` Workspace by value; `shipmind status` compares every rendered object against the cluster and exits `3` on drift. If the console mutated a live `Deployment`, `Secret`, or `Workspace` in-cluster, the environment would immediately fail `shipmind status` and lose the change on the next `shipmind up`.

All declarative configuration changes therefore flow through `shipmind.yaml` in one of three tiers:

| Tier | Condition | Validation | Execution & output |
|---|---|---|---|
| **Tier 1: Local apply** | `shipmind console <env-dir>` | `env.Parse` (`KnownFields(true)` + `check()`) + `overlay.Render` + `deploy.Up(..., DryRun: true)` against `<env-dir>`'s `context`. | Preserves comments on unmodified YAML subtrees via `yaml.Node` AST editing, previews the `shipmind.yaml` diff + dry-run apply log, atomically writes `<env-dir>/shipmind.yaml`, and runs `deploy.Up` (`shipmind up`) on operator confirmation, streaming step logs over SSE. |
| **Tier 2: Validated diff export** | `-in-cluster` with `console.overlayRepo` unset | `env.Parse` (`KnownFields(true)` + `check()`) against `shipmind-release`'s `shipmind.yaml` baseline, including cross-field invariants (e.g., `incidentAutoPR: true` requiring `idleSuspendAfter >= 5m` and `incidentImage` set). | Renders a unified diff, downloadable `shipmind.yaml`, Slack App Manifest preview (`internal/slackapp`) if `slack` changed, and copy-pasteable `shipmind up -dry-run <env-dir> && shipmind up <env-dir>` commands. Zero Git credentials needed. |
| **Tier 3: GitOps proposal (`core-vcs`)** | `-in-cluster` with `console.overlayRepo` configured | Same `env.Parse` validation + secret-free check (`env.SanitizeForRelease`). | Clones `console.overlayRepo` into an isolated per-request `os.MkdirTemp("/tmp", "console-vcs-*")` worktree (`defer os.RemoveAll`) via pure-Go `go-steer/core-vcs` `v1.2.0` (`vcs.Policy{BranchPrefix: "console/", CacheDir: "/var/lib/shipmind-console/vcs-cache"}`; `github` for github.com / GitHub Enterprise Server; `gitlab` for gitlab.com / self-managed GitLab with optional custom CA bundle), checks open proposals (`ProposalList`) to prevent duplicate PRs, updates `<path>/shipmind.yaml` on topic branch `console/<env>-<slug>-<shortsha>` (`TARGET_IS_BRANCH` enforced — never writes `baseBranch`), signs off / attributes the commit (`Requested-By: <caller.Identity>`), opens a pull/merge request (`ProposalCreate`), and returns the PR/MR URL. |

### `shipmind.yaml` `console` configuration block

```yaml
console:
  auth:
    mode: bearer                          # bearer (default in-cluster) | oidc | proxy-header | iap-jwt
    externalURL: ""                       # required (https://...) for oidc/proxy-header/iap-jwt; optional on bearer
    admins:                               # purser/authz.Rules grants (required for oidc/proxy-header/iap-jwt)
      - domain:example.com
    viewers: []                           # optional read-only grants
    oidc:                                 # when mode == oidc
      issuer: https://accounts.google.com
      clientID: 123456-abc.apps.googleusercontent.com
      clientSecretKey: oidc-client-secret # key in Secret shipmind-system/shipmind-console-auth
    proxyHeader:                          # when mode == proxy-header
      header: X-Auth-Request-Email
      trustedCIDRs: [10.0.0.0/8]
    iap:                                  # when mode == iap-jwt
      audience: /projects/12345/global/backendServices/67890
  overlayRepo:                            # optional: enables Tier 3 Config Studio PRs via core-vcs
    forge: github                         # github | gitlab
    baseURL: ""                           # optional: GHE or self-managed GitLab URL
    repository: go-steer/shipmind-deploy
    baseBranch: main
    path: envs/shipmind-stage             # directory inside repository holding shipmind.yaml
    secret: shipmind-console-vcs          # Secret in shipmind-system (separate from agent ate-credentials!)
  archive: gs://shipmind-scenario-runs/stage  # optional: read-only GCS scenario history (#88)
  substrateScopeURL: ""                   # optional external URL for "View in substrate-scope" links
```

> **Credential isolation (`console.overlayRepo.secret` vs `agent.vcs.secret`):** The console's overlay repository token lives in `shipmind-system/<secret>` and is **never** placed in `ate-credentials`. Autonomous incident tasks and chat agents cannot push branches to `shipmind-deploy` unless an environment explicitly lists `shipmind-deploy` in `agent.vcs.repositories`.

### Caller-attributed ephemeral Day-2 runtime actions

Runtime operational actions do not alter declarative manifests and therefore cause zero `shipmind status` drift. Every action requires the `admin` role, passes `httpmw.NewBrowserWriteGuard` + port-exact `Origin` + `X-Shipmind-CSRF` (`Content-Type: application/json`), forwards `X-Asserted-Caller: <caller.Identity>`, and appends an immutable record to `console.db` (`audit_events` table: `id`, `ts`, `caller`, `auth_source`, `action`, `target`, `request_json`, `outcome`, `error`):

1. **Send Test Alert (`POST /api/v1/integrations/switchboard/test-alert`):** Enabled when the environment includes `deploy/switchboard/slack` and sets `slack.alertChannel` (`shipmind up` mirrors `SwitchboardIngressSecret`'s `ingress-token` into `shipmind-system/shipmind-console-agent` and admits `shipmind-system` `shipmind-console` in `deploy/switchboard/slack/networkpolicy.yaml`). Posts `*shipmind console test alert* — triggered by <caller>` to `http://switchboard.switchboard.svc.cluster.local:8080/v1/messages` and returns the synchronous platform status (`200 OK` + message ref, or `403`/`404` error code and reason).
2. **Run Scheduled Audit Now (`POST /api/v1/audits/{schedule}/run`):** Instantiates a Kubernetes `Job` (`<cronjob>-manual-<unix>`) from the scheduled audit `CronJob.spec.jobTemplate` in `shipmind-system` with labels `shipmind.dev/triggered-by=console` and `shipmind.dev/audit-schedule=<schedule>`, without mutating `CronJob.spec`.
3. **Approve or Deny Parked Write Gate (`POST /api/v1/agents/standalone/sessions/{sid}/perms/respond` or `POST /api/v1/agents/{atespace}/{task}/sessions/{sid}/perms/respond`):** Proxies `{"id": "<prompt-id>", "decision": "..."}` (`core-agent` `PromptResponse` wire schema) to `/sessions/{sid}/perms/respond` (served by both `core-agent` and `mast`) with `X-Asserted-Caller: <caller.Identity>`. Permanent `allow-always` standing grants are rejected (`422`) by the console proxy, matching `switchboard`'s `standing_approvers: ["nobody"]` rule on read-only distroless agent images.
4. **Acknowledge or Override `lookout` Incident (`POST /api/v1/lookout/findings/ack`, `POST /api/v1/lookout/triage-status`):** Calls `k8s-lookout`'s watch admin surface to write a bounded `findings ack` window or `pkg/memory` `TriageStatusRecord` (`status: investigating|triaged|actioned|escalated`, optional `severity_override`) into `lookout.db` with `caller` recorded.
5. **Wake or Delete `ax` Task (`POST /api/v1/ax/tasks/{atespace}/{task}/wake`, `DELETE /api/v1/ax/tasks/{atespace}/{task}`):** Calls `ax-server` `ResumeTask` or `DeleteTask` (routing `switchboard`-owned thread tasks through `switchboard`'s admin `DELETE /v1/agents/{agent}/tasks/{task}` so `switchboard`'s conversation state table marks the thread cleanly replaced rather than orphaned).

---

## 6. Upstream companion APIs {#CON-6}

To respect `shipmind`'s hard rule against carrying component source code, five targeted upstream companion additions are tracked in their owning repositories and pulled into `shipmind` via `versions.yaml`:

| Repository | Target release | Upstream surface required by `shipmind console` | Fallback before pin bump |
|---|---|---|---|
| `go-steer/core-agent` | `v3.3.0` | `proxy_grants` (or `purser/authz.Rules` phase-2 integration on `pkg/attach`) so a provisioned proxy identity (`"shipmind-console-admin"`, `"shipmind-console-viewer"`) asserting unprovisioned `X-Asserted-Caller` human emails receives daemon-admin or read-only viewer visibility across `"lookout"`/`"switchboard"`-owned sessions while recording the human on injects and `/perms/respond` approvals. | Console uses `fallback-token` (`t.Attach`) without `X-Asserted-Caller`, enforcing `viewer` read-only at the console proxy and recording operator attribution in `console.db`'s `audit_events` table. |
| `go-steer/mast-web` | `v0.2.0` | Honor `?session={sid}` query parameter on initial load to focus a deep-linked session tab, plus a static transcript-replay fixture hook for archived `transcript.sse` streams (§7.4). | Opens agent's session list in `mast-web`; Pane 4 renders `transcript.sse` events in an inline drawer until `v0.2.0` lands. |
| `go-steer/purser` | `v0.6.0` | `authn/proxyheader` (header extractor gated on peer CIDR or client mTLS, passing `authtest.RunAuthenticatorSuite`) and `authn/iap` (Google IAP ES256 JWT verifier with mandatory `aud` check). | Console Phase C1 ships on `purser` `v0.5.0` with `loopback`, `bearer`, and `oidc`; `proxy-header` and `iap-jwt` enable on the `purser` `v0.6.0` pin bump. |
| `go-steer/k8s-lookout` | `v0.45.0` | Expose `--metrics-addr=:9090` (`/healthz`, `/readyz?verbose`, `/metrics`) plus an authenticated `--admin-addr=:9091` (`purser` bearer/mTLS) on `lookout watch`: `GET /v1/sources` (structured `resolveAutoDefaults` enabled + skipped sources with exact RBAC/CRD skip reasons), `GET /v1/incidents` & `GET /v1/watchboard` (paged reads from `pkg/store` `occurrences`, `sink_sessions`, and `triage_status`, plus held-reminder count), `POST /v1/findings/ack`, and `POST /v1/triage-status`. | `shipmind`'s `lookout` profile adds `Service/lookout-ax` (`:9090`) + `--metrics-addr=:9090` immediately (`lookout` `v0.44.0` already ships `/readyz?verbose` and `lookout_source_denied` / `lookout_active_incidents` / `lookout_inject_errors_total` on `/metrics`). Rich skipped-source reasons and incident table light up with `v0.45.0`. |
| `go-steer/switchboard` | `v0.12.0` | Extend `cmd/switchboard/admin.go` (`--admin-addr=:9091`, already `purser`-gated) with `GET /v1/status` (chat socket/PubSub liveness, configured `ingress_allow` channels, bounded 100-entry ring buffer of recent `:8080` `/v1/messages` outbound outcomes and platform HTTP status codes without message bodies) and `GET /v1/threads` (active/recent conversation -> task/session bindings and usage totals from `--state-dir`). | When an overlay includes `deploy/switchboard/slack` (or `standalone` switchboard), `service.yaml` exposes `:9090` (`/healthz`, `/readyz`, `/metrics`), `networkpolicy.yaml` admits `shipmind-system` `shipmind-console` to `:8080` `POST /v1/messages`, and `shipmind up` mirrors `ingress-token` into `shipmind-console-agent` so **Test Alert** works in Phase C1. Environments without switchboard show `Not deployed` cleanly in Pane 1. |

---

## 7. Core panes

All five panes render in a responsive single-page shell (`internal/console/web/`, plain ES modules, semantic light/dark CSS tokens matching `mast-web`, zero framework build chain) backed by typed JSON endpoints under `/api/v1/*` (`ReadHeaderTimeout: 5s`, `ReadTimeout: 15s`, `MaxBytesReader: 1 MiB` on JSON bodies).

### 7.1 Pane 1: Environment, BOM, and integration health {#CON-7}

**Goal:** Surface every silent integration fault, version pin, and drift condition on one screen in `< 500 ms` with **0 LLM tokens** and **0 actor wakes**.

- **Environment & BOM header:** Displays environment name, `context` / cluster, active profile (`standalone` or `substrate`), `shipmind` commit SHA (`shipmind-release`), and a component BOM table (`versions.yaml` version/commit, source repository link, pinned `@sha256` digest vs live running pod `containerStatuses[].imageID` digest). Any image digest mismatch highlights immediately as `DRIFT`.
- **Drift & startup-reader check:** Summarizes live workload rollout completion, missing/unexpected `substrate.workloads`, Substrate post-install patches (`atelet` `nodeSelector` and `atenet-router` `60s` parked budget), and startup-reader freshness (`lookout-ax`, `switchboard`, `k8s-credential-provider`, `shipmind-console`). In local mode (`shipmind console <env-dir>`), an **Evaluate Full Drift (`shipmind status`)** button runs `deploy.Status` (including live Secret Manager comparison when `secrets` are configured).
- **Integration health cards:**
  - **`k8s-lookout`:** `/readyz?verbose` status, active incidents gauge, sink delivery error counter (`lookout_inject_errors_total`), and the **Signal Sources table** showing every enabled source in green and every skipped/denied source in amber/red with its exact probe diagnostic (e.g., `capacity — skipped: missing get on configmaps/cluster-autoscaler-status in kube-system; apply deploy/14-role-watcher-capacity.yaml`).
  - **`switchboard`:** Inbound adapter state (Slack Socket Mode WebSocket / Google Chat Pub/Sub), `:8080` outbound ingress status, allowlisted `alertChannel` IDs, recent outbound delivery failures (e.g., `CDWTP8NFL -> 404 channel_not_found` highlighted red with timestamp), and the **Send Test Alert** action (`admin`).
  - **Runtime capacity (`substrate` vs `standalone`):** On `substrate`, shows `ax-server` readiness, `WorkerPool` pod count, total vs booked actor slots, and active vs suspended `ax` tasks (read from `ateapi` + `ax-server`, never waking actors) with a **Open 3D Cluster View (`substrate-scope`)** button. On `standalone`, shows `core-agent` daemon `/healthz`, uptime, and active session count.

### 7.2 Pane 2: Posture and scheduled audits {#CON-8}

**Goal:** Give operators a durable, multi-cluster posture ledger across `lookout audit` (static spec & provider posture) and `leeway report` (live runtime placement skew & `ComputeClass` fallback rank) ([#127](https://github.com/go-steer/shipmind/issues/127)).

- **Audit schedule bar:** Lists each declarative schedule under `shipmind.yaml` `audits` (`CronJob` schedule expression, cluster scope, last run time, duration, exit code, LLM token spend — showing `0 tokens` when the deterministic pre-pass found a clean or unchanged cluster — and next scheduled fire time), plus **Run Now** (`admin`).
- **Normative ingestion rules (`k8s-lookout` `audit-ingestion-contract.md`):**
  1. **Two distinct keys stored per finding:** Every ingested `lookout audit` and `leeway report` record stores **both** its instance **subject key** (`<cluster>/<namespace>/<Kind>/<normalized-name>/<reason>`, via `findings.NormalizeName` + canonical reason) as the ledger row primary key for run-over-run delta, stale-close, and per-workload PR targeting, **and** its class **`fingerprint`** (`PostureFingerprint` / `leeway` finding kind, nullable when absent per contract §5) for cross-cluster fleet rollups.
  2. **Terminating summary line required before delta/stale-close:** An audit stream that exits non-zero or lacks `lookout`'s terminating summary object (`scanned`, `findings`, `elapsed` — or returns `"status": "error"` for a cluster in `leeway report`) is recorded as a **failed run** in `audit_runs` and **never** stale-closes previously open findings for that cluster. Fleet runs also assert `clusters_succeeded == clusters_expected` before computing cross-cluster rollups.
  3. **Exemptions annotated, never dropped:** Findings carrying `exempt_reason` and `exempt_expires` (RFC 3339) are ingested into `posture_findings` like any other finding. The UI hides active exemptions behind a **Show Exempt (`N`)** toggle by default and surfaces a warning badge on exemptions expiring within 14 days or lapsed.
- **Remediation preview & GitOps PRs:** Expanding a finding displays structured evidence (`Details` opaque key/value bag, `leeway` Hamilton domain table / `ComputeClass` rank occupancy) and deterministic `Finding.Remediation` YAML (`manifest_patch` with `topologySpreadConstraints`, PDB, probes, or resource limits). Clicking **Open Remediation PR** (`admin`, when `agent.vcs` is configured) dispatches or attaches to an audit remediation session with `propose-change` (`vcs_proposal_list` open-PR deduplication enforced) and links the resulting PR URL back to the ledger row.

### 7.3 Pane 3: Incidents, watchboard, and chat activity {#CON-9}

**Goal:** Unify reactive `lookout` incidents, batched warning watchboards, and interactive `switchboard` threads into one causal timeline without waking idle sandboxes.

- **Unified activity feed:** Joins `k8s-lookout` incident/storm/watchboard records (`lookout-<16hex>`, `lookout-wb-<slug>-<8hex>`), `switchboard` thread bindings (`C...:ts` / `spaces/...` -> `<task>/<session>`), and (under `substrate`) `ax-server` `ListTasks` + `ateapi` `ListActors` state (`Running`, `IdleSuspended`, `CompletedSuspended`, `WorkerDraining`, `RevertedAfterWorkerLoss`, `RevertedAfterFailedSuspend`, `Failed`).
- **Per-activity telemetry:** Shows cluster, target workload or chat channel, trigger reason/severity, current execution badge, turn count, token breakdown (`input`, `cached`, `output`, `thinking`) and USD cost (cached from last active poll, `switchboard` state, or `lookout`/archive records — never polling suspended actors), pending permission gate indicator, and linked GitOps PR pills.
- **1-click actions:**
  - **Open in `mast-web`:** Opens the embedded `/mast-web/...` session drawer/tab (gating suspended `substrate` actors behind the explicit **Wake & Open** modal from §4).
  - **View in `substrate-scope`** (`substrate` only): Deep-links to the actor column in 3D.
  - **Approve / Deny** (`admin`): Inline banner for sessions parked on `perms_stream` (e.g. `vcs_publish` / `vcs_propose` when `chatAutoApprove: false`, or cluster write gates).
  - **Ack / Downgrade** (`admin`): Suppresses noisy incident fingerprints for a bounded window (`findings ack`) or sets `triage-status` `severity_override`.
  - **Wake / Delete Task** (`admin`, `substrate` only): Resumes a suspended task or deletes a wedged/drifted `ax` task.

### 7.4 Pane 4: Scenario and eval scorecard {#CON-10}

**Goal:** Track platform regression health, trajectory compliance ([#89](https://github.com/go-steer/shipmind/issues/89)), cost/latency trends, and `claude-sonnet-5-5` judge grades ([#90](https://github.com/go-steer/shipmind/issues/90)) across both `kind`/in-cluster runs and optional GCS archives ([#88](https://github.com/go-steer/shipmind/issues/88), [#93](https://github.com/go-steer/shipmind/issues/93)).

- **Storage-agnostic ingestion (no GCS required):**
  - **In-cluster native path:** `shipmind scenario run` gains an optional `-report-url http://shipmind-console.shipmind-system.svc.cluster.local/api/v1/internal/scenarios` flag (authenticated via projected `ServiceAccount` token or shared `shipmind-console` ingest token) so nightly CronJobs (`missing-env`, `oom-killed`, `worker-drain`, posture scenarios) and on-demand Jobs push `meta.json`, `result.json`, `usage.json`, and compressed `transcript.sse` straight into `console.db` (`scenario_runs` table) regardless of cloud provider.
  - **Optional GCS archive hydration:** When `console.archive` (`gs://bucket/prefix`) is set, `internal/archive` extends its zero-SDK HTTP JSON client with read-only `List` and `ReadObject` (using Workload Identity metadata token or ADC, syncing completed runs where `meta.json` is present into `console.db` by run key `<scenario>/<date>/<workload>`).
- **Scorecard views:**
  - **Scenario pass/fail grid:** Recent runs per scenario showing overall verdict, `expect.mentions` regex matches, `expect.trajectory` checks (`must_call`, `never_call`, `tool_calls <= max`, `model_calls <= max` validated against `e2e/scenarios/tools.yaml`), and `drain` lifecycle results (`WorkerDraining` condition + neighbour survival, zero `Reverted*`).
  - **Judge & cost trends:** Charts p50/p95 wall-clock seconds, input/cached/output tokens, USD per run, and `claude-sonnet-5-5` rubric score (`1–5`, `correct`, `specific`, `actionable`, plus full judge reasoning) grouped by `agent` image digest / `core-agent` version so an operator can compare model or recipe upgrades before cutting a release.
  - **Transcript replay:** Clicking any run opens its stored `transcript.sse` tool-call and narration timeline in a read-only `mast-web` transcript viewer without touching any live agent.

### 7.5 Pane 5: Config Studio (`shipmind.yaml`) {#CON-11}

**Goal:** Provide a schema-validated editor over `shipmind.yaml` backed by the drift-free Three-Tier Write Path (§5).

- **Dual form + YAML AST editor:** Edits synchronize two-way between structured cards and raw `shipmind.yaml` text, running `internal/env.Parse` on `/api/v1/config/validate` on every change and pinning errors to exact field paths:
  - **Cluster Scope ([#104](https://github.com/go-steer/shipmind/issues/104)):** Edit `agent.clusters` (`home` cluster + live `scope` entries `{project, label}`, or legacy `{name, path}` entries with 1-click migration helper).
  - **Agent Images, Budgets & Lifecycle:** Configure `agent.image`, `agent.chatImage`, `agent.incidentImage`, and `agent.idleSuspendAfter` (`1m`–`20m`, automatically enforcing `>= 5m` whenever `agent.vcs.incidentAutoPR` is enabled).
  - **GitOps VCS & Approval Policy ([#118](https://github.com/go-steer/shipmind/issues/118), [#121](https://github.com/go-steer/shipmind/issues/121)):** Manage `agent.vcs.secret`, allowlisted `<owner>/<repo>` + `baseBranch` entries, `incidentAutoPR` toggle, and `chatAutoApprove` toggle, with clear inline posture callouts explaining which tool calls pause for Slack/console approval vs push `agent/*` branches directly.
  - **Chat & Outbound Alerts:** Edit `slack` (`app`, `command`, `description`, `alertChannel`), with an inline **Preview Slack App Manifest** drawer rendered live by `internal/slackapp`!
  - **Scheduled Posture Audits ([#127](https://github.com/go-steer/shipmind/issues/127)) & Console Settings:** Configure `audits` cron schedules, cluster targets, `--exemptions` paths, `autoPR` policy, and `console` auth / `overlayRepo` settings.
- **Review & commit bar:** Displays the active write tier badge (**Local Apply**, **Export Diff**, or **Open PR on `<overlayRepo>`**), renders the unified `shipmind.yaml` diff, and executes Tier 1, Tier 2, or Tier 3 accordingly.

---

## 8. Package layout, testing tiers, and rollout plan {#CON-12}

### Repository layout and 4-layer acyclic import DAG

New packages register in `cmd/shipmind/architecture_test.go` (`packageLayer`) and `dev/coverage-floors.txt` without widening `maxNonTestFuncsOver150Lines` (`<= 3`) or introducing cycles:

```text
Layer 0 (leaf packages — stdlib + external modules only):
  internal/archive          + List / ReadObject over GCS JSON API
  internal/bom              unchanged
  internal/cluster          + in-cluster REST helper for ConfigMap/Job/Deployment reads
  internal/env              + Console, Audits, SanitizeForRelease, AST-preserving patch helper
  internal/judge            unchanged
  internal/render           unchanged
  internal/secrets          unchanged
  internal/console/store    pure-Go SQLite (modernc.org/sqlite, WAL): audit_events,
                            audit_runs, posture_findings, scenario_runs + 30d/512MiB prune

Layer 1 (domain adapters — import Layer 0 only):
  internal/agent            + users.json table_file & proxy_grants wiring in core-agent configs
  internal/manifests        + mast-web embedDir static asset vendoring & lock verification
  internal/overlay          unchanged
  internal/scenario         + optional -report-url push to console ingest endpoint
  internal/slackapp         unchanged
  internal/console/clients  typed clients for lookout, switchboard, core-agent attach,
                            ax-server gRPC, and ateapi + atenet-router mTLS (neverwake_test.go)

Layer 2 (orchestrators — import Layers 0–1; never import each other):
  internal/deploy           + shipmind-release ConfigMap & console-auth / console-agent Secrets
  internal/console          HTTP server, purser authn/authz/CSRF/Host middleware, embedded web/
                            + mast-web proxy, Three-Tier Config Studio (core-vcs), DeployRunner iface

Layer 3 (CLI binary entrypoint):
  cmd/shipmind              wires `shipmind console` (injecting deploy.Status/Up as DeployRunner
                            in local mode, or running -in-cluster)
```

### `console.db` SQLite schema & resource bounds

- **Engine:** `modernc.org/sqlite` (`CGO_ENABLED=0`) opened with `_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)`.
- **Four bounded tables:**
  1. `audit_events(id INTEGER PRIMARY KEY, ts TEXT, caller TEXT, auth_source TEXT, action TEXT, target TEXT, request_json TEXT, outcome TEXT, error TEXT)` — operator action trail.
  2. `audit_runs(id TEXT PRIMARY KEY, schedule TEXT, cluster TEXT, engine TEXT, started_at TEXT, finished_at TEXT, status TEXT, scanned INT, findings INT, exempt INT, llm_tokens INT, cost_usd REAL, error TEXT)` — scheduled/manual `#127` runs.
  3. `posture_findings(subject_key TEXT PRIMARY KEY, cluster TEXT, fingerprint TEXT, engine TEXT, kind TEXT, severity TEXT, namespace TEXT, kind_of_object TEXT, name TEXT, reason TEXT, message TEXT, details_json TEXT, remediation_yaml TEXT, exempt_reason TEXT, exempt_expires TEXT, pr_url TEXT, first_seen TEXT, last_seen TEXT, state TEXT)` — active/resolved posture ledger.
  4. `scenario_runs(run_key TEXT PRIMARY KEY, scenario TEXT, env TEXT, started_at TEXT, passed INT, agent_image TEXT, model TEXT, duration_ms INT, input_tokens INT, output_tokens INT, cost_usd REAL, judge_score INT, result_json TEXT, usage_json TEXT, meta_json TEXT, transcript_gzip BLOB)` — `#93` scorecard history.
- **Retention & sizing:** Background hourly pruner enforces a 90-day TTL on `audit_events` / `scenario_runs` (30 days on `transcript_gzip` blobs) and a `512 MiB` ceiling (`oldest-first` eviction inside the `1Gi` PVC). `Deployment/shipmind-console` runs at `requests: {cpu: 50m, memory: 128Mi}`, `limits: {cpu: 500m, memory: 512Mi}`.

### Testing strategy

Every check follows `CONTRIBUTING.md`'s rule: **every positive check ships with its negative refusal test**, and `go test ./...` runs offline with no cluster and no credentials:

- **Unit & architecture gates (`go test -race ./...`):**
  - `internal/env`: accepts valid `console` blocks (`bearer` without `externalURL`, `oidc`/`proxy-header`/`iap-jwt` with `https://` `externalURL`, `overlayRepo`) and rejects malformed combinations (`oidc`/`proxy-header`/`iap-jwt` missing `externalURL` or `admins`, `overlayRepo` path traversal, secrets in `SanitizeForRelease`).
  - `internal/deploy`: verifies deterministic `shipmind-release` ConfigMap rendering (`< 256 KiB` size guard, zero drift on repeated `shipmind status` and `up -dry-run`), idempotent `console-admin`/`console-viewer` backfill in `lookout-ax/agent-tokens`, `ingress-token` mirroring to `shipmind-console-agent`, and `TestSubstrateRouterClientsMountMTLS` coverage on `Deployment/shipmind-console`.
  - `internal/console/clients/neverwake_test.go`: AST test forbidding mutating `ateapi` RPCs in read clients + `httptest` router test proving `GET /api/v1/agents/{atespace}/{task}/attach/sessions` against a `SUSPENDED` actor always returns `409 actor_suspended` with **0 requests** reaching the fake `atenet-router`, and that waking occurs strictly via `POST /api/v1/ax/tasks/{atespace}/{task}/wake` (`admin` + CSRF required).
  - `internal/console`: `purser/authtest` conformance tests across `loopback`, `bearer`, `oidc` (`authtest.NewIssuer`), `proxy-header`, and `iap-jwt`; `viewer` vs `admin` `403` refusal matrix on every mutating route; port-exact `Origin` (rejecting cross-port `http://localhost:9999` with ambient cookie), `X-Shipmind-CSRF`, and DNS-rebinding `Host` `421`/`403` refusal tests; root `GET /config` `Referer`-scoped `mast-web` bootstrap tests; `audit-ingestion-contract.md` parser tests proving a truncated `lookout audit` stream without a summary line fails the run and leaves open `posture_findings` untouched; Tier 1/2/3 Config Studio tests against an in-process `go-git` / fake `core-vcs` forge (`BranchPrefix: "console/"`) proving `TARGET_IS_BRANCH` never writes `main`.
- **Wiring tier (`kind` + Playwright smoke):**
  - Deploys `standalone` profile (`core-agent`, `lookout`, `switchboard`, `shipmind-console`) on `kind`, verifies Day-0 `bearer` login over port-forward, checks Pane 1 BOM/health green, injects a synthetic `lookout` event, views and approves a session in embedded `mast-web`, runs a one-shot posture audit into Pane 2, and validates a Config Studio diff in Pane 5.

### Phased delivery plan

Each slice is independently shippable, keeps `main` green, and maps directly to the `{#CON-N}` anchors above for `Design-Item:` tracking:

| Phase | Scope | Design anchors | Dependencies |
|---|---|---|---|
| **C1: Foundation, Release ConfigMap & Pane 1** | Deterministic `shipmind-release` ConfigMap in `up`/`status`; `internal/console` scaffold + `console.db`; `purser` `loopback` + `bearer` + `oidc` auth with `X-Shipmind-CSRF` & port-exact `Origin`; `shipmind console` CLI + `images/console` + profile manifests (`Service/lookout-ax` `:9090`, `switchboard` `:9090` + `ingress-token` mirror); Pane 1 (BOM, drift, K8s/Substrate health, `switchboard` Test Alert). | `#CON-1`, `#CON-2`, `#CON-3`, `#CON-7` | None (`purser` `v0.5.0`, `lookout` `v0.44.0` `/readyz?verbose`, `switchboard` `v0.11.0`). |
| **C2: Upstream Companion Status & Pane 3 + `mast-web`** | Pin `mast-web` (`internal/manifests` + `verify-vendor-fresh` lock) + embed `/mast-web/` & root `GET /config` attach proxy; `core-agent` `users.json` `table_file` + `proxy_grants` (`v3.3.0`); `substrate` never-wake-on-read gate + `#agent=` `substrate-scope` links; bump `k8s-lookout` (`v0.45.0`) and `switchboard` (`v0.12.0`) for rich source skip reasons, incident/thread APIs, and ack/triage-status actions; ship Pane 3. | `#CON-4`, `#CON-6`, `#CON-9` | C1 + `core-agent` `v3.3.0` + `k8s-lookout` `v0.45.0` + `switchboard` `v0.12.0` + `mast-web` `v0.2.0` pins. |
| **C3: Config Studio & Three-Tier Write Path** | Pane 5 structured + YAML AST editor, `internal/slackapp` manifest preview, Tier 1 local `up -dry-run`/`up` stream, Tier 2 diff export, and Tier 3 `core-vcs` overlay PR creation across GitHub/GHE/GitLab. | `#CON-5`, `#CON-11` | C1 (`core-vcs` `v1.2.0` already GA). |
| **C4: Posture & Scheduled Audits Pane** | Pane 2 schedule runner (`Run Now` Job trigger), `lookout audit` + `leeway report` `console.db` ledger with dual `SubjectKey`/`fingerprint` grains, exemption tracking, and **Open Remediation PR**. | `#CON-8` | C1, C2 + scheduled audits ([#127](https://github.com/go-steer/shipmind/issues/127)). |
| **C5: Scenario Scorecard & Enterprise Auth** | Pane 4 `-report-url` ingestion + optional `internal/archive` GCS reader + `transcript.sse` replay; bump `purser` (`v0.6.0`) for `proxy-header` and `iap-jwt`; `kind` + Playwright wiring gate. | `#CON-10`, `#CON-12` | C1 + `purser` `v0.6.0` (for `proxy-header` / `iap-jwt`). |

---

## Alternatives rejected

1. **Separate `go-steer/shipmind-console` repository:** Forces a two-PR + pin-bump cycle every time `shipmind.yaml` (`internal/env`), `versions.yaml` (`internal/bom`), or `internal/deploy` adds a field, and duplicates `shipmind status`'s drift and validation rules across two codebases.
2. **Extending `substrate-scope` into the platform console:** `substrate-scope` is a dedicated three.js 3D visualizer for Agent Substrate clusters (up to 100k actors) usable without `shipmind`, and has no role on `shipmind`'s `standalone` profile (`#SM-2`). Keeping `substrate-scope` focused on 3D cluster topology and deep-linking to it from `shipmind console` preserves clean separation.
3. **Mounting `lookout-ax`'s and `switchboard`'s SQLite PVCs directly into `shipmind-console`:** Both `lookout-store` and `switchboard-state` are `ReadWriteOnce` volumes in separate namespaces (`lookout-ax`, `switchboard`). Cross-pod SQLite mounts fail on multi-node clusters and violate namespace isolation; querying small authenticated HTTP status endpoints on each component keeps ownership clean.
4. **Mutating live cluster Deployments/Secrets directly from Config Studio:** Breaks `shipmind`'s declarative GitOps contract, causes immediate `shipmind status` exit-3 drift, and silently loses operator edits on the next `shipmind up`.
5. **Requiring GCS or GCP IAP for in-cluster operation:** Locks out `kind`, bare-metal Kubernetes, and non-GCP adopters on Day 0. Using a `1Gi` SQLite PVC + `purser` `bearer`/`oidc`/`proxy-header` makes GCP integrations (`gs://` archive, `iap-jwt`, Secret Manager) strictly additive.
