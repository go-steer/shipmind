# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# The substrate profile's agent image on core-agent (#48): core-agent
# run as an ax task in serve mode.
#
#   ax-task-runner  ax's runner contract, built from the ax fork at the
#                   ax-runner pin in versions.yaml
#   core-agent      built (-tags no_tui) from core-agent at the core-agent
#                   pin, a full commit SHA, checked after the fetch; no_tui
#                   drops the unused terminal UI (~5 MB) and declines MCP
#                   elicitation in headless mode (#144)
#   lookout         built (-tags allproviders) from k8s-lookout at its pin
#                   in versions.yaml; gke-triage-f and gke-watchboard
#                   (8-tool trimmed triage surface) and gke-chat (9-tool
#                   audit + triage surface) run lookout mcp
#                   --clusters-from=auto as a stdio MCP server
#                   (#127, #131, #133, #144)
#   leeway          built from leeway at its pin in versions.yaml; gke-chat
#                   runs leeway mcp --timeout=20s --parallelism=32 as a
#                   stdio MCP server (#127, #144)
#   /opt/core-agent/recipes/<bundle>
#                   gke-triage-f, gke-chat, and gke-watchboard are authored
#                   natively under agents/sre-triage/<bundle>/.agents/ (flat
#                   main agents holding lookout — and leeway on gke-chat —
#                   directly, with zero hosted GKE MCP server;
#                   propose-change is layered at deploy time on gke-triage-f
#                   and gke-chat, while gke-watchboard stays strictly
#                   read-only / alert-only; #131, #133, #144), carrying
#                   pinned k8s-lookout skills from the lookout build stage.
#                   Each recipe is booted once (no network needed) until its
#                   attach listener answers as Substrate's router (Host
#                   check, core-agent#1358), verifying MCP server startup,
#                   loaded AGENTS.md, bundled skills, and exact tool
#                   registration.
#
# The pins arrive as build arguments read from versions.yaml
# (build-args.sh); this file has no defaults for them, so they can't
# drift from the bill of materials. Build from the repository root:
#
#   docker build -f images/agent/Dockerfile $(images/agent/build-args.sh) .

ARG GO_VERSION=1.26.11
ARG AX_GO_VERSION=1.27.1

FROM --platform=$BUILDPLATFORM golang:${AX_GO_VERSION}-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS ax-runner
ARG AX_RUNNER_REPO
ARG AX_RUNNER_COMMIT
ARG TARGETOS
ARG TARGETARCH
RUN test -n "$AX_RUNNER_REPO" && test -n "$AX_RUNNER_COMMIT" && apk add --no-cache git
WORKDIR /src
# The fork keeps upstream's module path (github.com/google/ax), so it is
# built from a checkout rather than with go install.
RUN git init -q . \
 && git fetch -q --depth=1 "$AX_RUNNER_REPO" "$AX_RUNNER_COMMIT" \
 && git checkout -q FETCH_HEAD \
 && test "$(git rev-parse HEAD)" = "$AX_RUNNER_COMMIT"
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/ax-task-runner ./cmd/ax-task-runner

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine@sha256:ae52b5bc6ed045a9ed26ab03dc19327a802a9cc9aa8b27ed57f5c6b60579b0bb AS core-agent
ARG CORE_AGENT_REPO
ARG CORE_AGENT_COMMIT
ARG TARGETOS
ARG TARGETARCH
RUN test -n "$CORE_AGENT_REPO" && test -n "$CORE_AGENT_COMMIT" && apk add --no-cache git
WORKDIR /src
RUN git init -q . \
 && git fetch -q --depth=1 "$CORE_AGENT_REPO" "$CORE_AGENT_COMMIT" \
 && git checkout -q FETCH_HEAD \
 && test "$(git rev-parse HEAD)" = "$CORE_AGENT_COMMIT"
RUN go mod download
# One binary for the image, built for the target, and one for this
# stage, to run the recipe boot smoke checks below. --version reports
# the commit; versions.yaml's note names the release it is.
RUN ldflags="-s -w -X github.com/go-steer/core-agent/v3/internal/version.Version=${CORE_AGENT_COMMIT} -X github.com/go-steer/core-agent/v3/internal/version.Commit=${CORE_AGENT_COMMIT}" \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -tags no_tui -trimpath -ldflags "$ldflags" -o /out/core-agent ./cmd/core-agent \
 && CGO_ENABLED=0 go build -tags no_tui -trimpath -ldflags "$ldflags" -o /usr/local/bin/core-agent ./cmd/core-agent

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine@sha256:ae52b5bc6ed045a9ed26ab03dc19327a802a9cc9aa8b27ed57f5c6b60579b0bb AS lookout
ARG LOOKOUT_REPO
ARG LOOKOUT_VERSION
ARG LOOKOUT_COMMIT
ARG TARGETOS
ARG TARGETARCH
RUN test -n "$LOOKOUT_REPO" && test -n "$LOOKOUT_VERSION" && test -n "$LOOKOUT_COMMIT" && apk add --no-cache git
WORKDIR /src
RUN git init -q . \
 && case "$LOOKOUT_VERSION" in \
      v*) git fetch -q --depth=1 "$LOOKOUT_REPO" "refs/tags/${LOOKOUT_VERSION}:refs/tags/${LOOKOUT_VERSION}" \
       && git checkout -q "refs/tags/${LOOKOUT_VERSION}" ;; \
      *)  git fetch -q --depth=1 "$LOOKOUT_REPO" "$LOOKOUT_VERSION" \
       && git checkout -q FETCH_HEAD ;; \
    esac \
 && sha="$(git rev-parse HEAD)" \
 && test "$sha" = "$LOOKOUT_COMMIT"
RUN go mod download
RUN sha="$(git rev-parse HEAD)" \
 && ldflags="-s -w -X github.com/go-steer/k8s-lookout/internal/version.Version=${LOOKOUT_VERSION} -X github.com/go-steer/k8s-lookout/internal/version.Commit=${sha}" \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -tags allproviders -trimpath -ldflags "$ldflags" -o /out/lookout ./cmd/lookout \
 && CGO_ENABLED=0 go build -tags allproviders -trimpath -ldflags "$ldflags" -o /usr/local/bin/lookout ./cmd/lookout \
 && /usr/local/bin/lookout version | grep -qF "$LOOKOUT_VERSION"

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine@sha256:ae52b5bc6ed045a9ed26ab03dc19327a802a9cc9aa8b27ed57f5c6b60579b0bb AS leeway
ARG LEEWAY_REPO
ARG LEEWAY_VERSION
ARG LEEWAY_COMMIT
ARG TARGETOS
ARG TARGETARCH
RUN test -n "$LEEWAY_REPO" && test -n "$LEEWAY_VERSION" && test -n "$LEEWAY_COMMIT" && apk add --no-cache git
WORKDIR /src
RUN git init -q . \
 && case "$LEEWAY_VERSION" in \
      v*) git fetch -q --depth=1 "$LEEWAY_REPO" "refs/tags/${LEEWAY_VERSION}:refs/tags/${LEEWAY_VERSION}" \
       && git checkout -q "refs/tags/${LEEWAY_VERSION}" ;; \
      *)  git fetch -q --depth=1 "$LEEWAY_REPO" "$LEEWAY_VERSION" \
       && git checkout -q FETCH_HEAD ;; \
    esac \
 && sha="$(git rev-parse HEAD)" \
 && test "$sha" = "$LEEWAY_COMMIT"
RUN go mod download
RUN sha="$(git rev-parse HEAD)" \
 && ldflags="-s -w -X github.com/go-steer/leeway/internal/version.Version=${LEEWAY_VERSION} -X github.com/go-steer/leeway/internal/version.Commit=${sha}" \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "$ldflags" -o /out/leeway ./cmd/leeway \
 && CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o /usr/local/bin/leeway ./cmd/leeway \
 && /usr/local/bin/leeway version | grep -qF "$LEEWAY_VERSION"

FROM --platform=$BUILDPLATFORM alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f AS recipes
RUN apk add --no-cache jq curl
COPY --from=core-agent /usr/local/bin/core-agent /usr/local/bin/core-agent
COPY --from=lookout /usr/local/bin/lookout /usr/local/bin/lookout
COPY --from=lookout /src/skills /lookout-skills
COPY --from=leeway /usr/local/bin/leeway /usr/local/bin/leeway
COPY agents/sre-triage /in
# Assemble both native core-agent recipes (gke-triage-f and gke-chat), copy
# pinned k8s-lookout skills (including playbooks both beside the skills and
# under k8s-triage/references/playbooks so ADK's load_skill_resource can read
# them), and verify exact lookout/leeway tool surfaces offline before booting.
RUN set -e; \
    lookout $(jq -r '.servers.lookout.args | join(" ")' /in/gke-triage-f/.agents/mcp.json) --list-tools > /tmp/lookout-triage-tools; \
    for t in k8s_list_clusters k8s_triage_workload k8s_resource_spec k8s_triage_logs k8s_event_timeline k8s_list_resources k8s_state_edges k8s_triage_delta; do \
      grep -q "^${t} " /tmp/lookout-triage-tools; \
    done; \
    grep -q '^8 tools ' /tmp/lookout-triage-tools; \
    if grep -q '^k8s_scan ' /tmp/lookout-triage-tools; then echo "gke-triage-f must not expose k8s_scan"; exit 1; fi; \
    if grep -q '^k8s_audit_all ' /tmp/lookout-triage-tools; then echo "gke-triage-f must not expose k8s_audit_all"; exit 1; fi; \
    lookout $(jq -r '.servers.lookout.args | join(" ")' /in/gke-chat/.agents/mcp.json) --list-tools > /tmp/lookout-chat-tools; \
    for t in k8s_list_clusters k8s_audit_all k8s_triage_workload k8s_resource_spec k8s_triage_logs k8s_event_timeline k8s_list_resources k8s_state_edges k8s_triage_delta; do \
      grep -q "^${t} " /tmp/lookout-chat-tools; \
    done; \
    grep -q '^9 tools ' /tmp/lookout-chat-tools; \
    if grep -q '^k8s_scan ' /tmp/lookout-chat-tools; then echo "gke-chat must not expose k8s_scan"; exit 1; fi; \
    printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | leeway $(jq -r '.servers.leeway.args | join(" ")' /in/gke-chat/.agents/mcp.json) | grep -q '"name":"report"'; \
    mkdir -p /out/gke-triage-f/.agents/plans /out/gke-triage-f/.agents/skills; \
    cp -a /in/gke-triage-f/.agents/. /out/gke-triage-f/.agents/; \
    cp -a /lookout-skills/k8s-triage /lookout-skills/playbooks /out/gke-triage-f/.agents/skills/; \
    mkdir -p /out/gke-triage-f/.agents/skills/k8s-triage/references/playbooks; \
    cp -a /lookout-skills/playbooks/. /out/gke-triage-f/.agents/skills/k8s-triage/references/playbooks/; \
    test -f /out/gke-triage-f/.agents/AGENTS.md; \
    test -f /out/gke-triage-f/.agents/config.json; \
    test -f /out/gke-triage-f/.agents/skills/k8s-triage/SKILL.md; \
    test -f /out/gke-triage-f/.agents/skills/k8s-triage/references/playbooks/crashloopbackoff.md; \
    jq -e '(.servers | keys | sort) == ["lookout"] and (.servers.lookout.agentic_never // false) == false' /out/gke-triage-f/.agents/mcp.json >/dev/null; \
    test ! -d /out/gke-triage-f/.agents/subagents; \
    test ! -d /out/gke-triage-f/.agents/roots; \
    mkdir -p /out/gke-chat/.agents/plans /out/gke-chat/.agents/skills; \
    cp -a /in/gke-chat/.agents/. /out/gke-chat/.agents/; \
    cp -a /lookout-skills/k8s-triage /lookout-skills/cluster-health /lookout-skills/k8s-audit /lookout-skills/k8s-capacity /lookout-skills/gitops-drift /lookout-skills/playbooks /out/gke-chat/.agents/skills/; \
    mkdir -p /out/gke-chat/.agents/skills/k8s-triage/references/playbooks; \
    cp -a /lookout-skills/playbooks/. /out/gke-chat/.agents/skills/k8s-triage/references/playbooks/; \
    test -f /out/gke-chat/.agents/AGENTS.md; \
    test -f /out/gke-chat/.agents/config.json; \
    test -f /out/gke-chat/.agents/skills/k8s-triage/SKILL.md; \
    test -f /out/gke-chat/.agents/skills/k8s-triage/references/playbooks/crashloopbackoff.md; \
    jq -e '(.servers | keys | sort) == ["leeway", "lookout"] and .servers.leeway.agentic_never == true and (.servers.lookout.agentic_never // false) == false' /out/gke-chat/.agents/mcp.json >/dev/null; \
    test ! -d /out/gke-chat/.agents/subagents; \
    test ! -d /out/gke-chat/.agents/roots
# Dedicated native gke-watchboard recipe (#133): lookout-only stdio MCP
# (8 tools including k8s_list_clusters) plus pinned k8s-triage skill and
# shared playbooks (under k8s-triage/references/playbooks for ADK's
# load_skill_resource) from the lookout build stage.
RUN set -e; \
    lookout $(jq -r '.servers.lookout.args | join(" ")' /in/gke-watchboard/.agents/mcp.json) --list-tools > /tmp/watchboard-lookout-tools; \
    for t in k8s_list_clusters k8s_triage_workload k8s_resource_spec k8s_triage_logs k8s_event_timeline k8s_list_resources k8s_state_edges k8s_triage_delta; do \
      grep -q "^${t} " /tmp/watchboard-lookout-tools; \
    done; \
    grep -q '^8 tools ' /tmp/watchboard-lookout-tools; \
    if grep -q '^k8s_scan ' /tmp/watchboard-lookout-tools; then echo "gke-watchboard must not expose k8s_scan"; exit 1; fi; \
    if grep -q '^k8s_audit_all ' /tmp/watchboard-lookout-tools; then echo "gke-watchboard must not expose k8s_audit_all"; exit 1; fi; \
    mkdir -p /out/gke-watchboard/.agents/plans /out/gke-watchboard/.agents/skills; \
    cp -a /in/gke-watchboard/.agents/. /out/gke-watchboard/.agents/; \
    cp -a /lookout-skills/k8s-triage /lookout-skills/playbooks /out/gke-watchboard/.agents/skills/; \
    mkdir -p /out/gke-watchboard/.agents/skills/k8s-triage/references/playbooks; \
    cp -a /lookout-skills/playbooks/. /out/gke-watchboard/.agents/skills/k8s-triage/references/playbooks/; \
    test -f /out/gke-watchboard/.agents/AGENTS.md; \
    test -f /out/gke-watchboard/.agents/config.json; \
    test -f /out/gke-watchboard/.agents/skills/k8s-triage/SKILL.md; \
    test -f /out/gke-watchboard/.agents/skills/k8s-triage/references/playbooks/hpa-thrash.md; \
    jq -e '(.servers | keys | sort) == ["lookout"] and (.servers.lookout.agentic_never // false) == false' /out/gke-watchboard/.agents/mcp.json >/dev/null; \
    test ! -d /out/gke-watchboard/.agents/subagents; \
    test ! -d /out/gke-watchboard/.agents/roots
# The Host Substrate's router sends (switchboard's and lookout's
# default router URL; the nightly's too).
ARG ROUTER_HOST=atenet-router.ate-system.svc.cluster.local
# The task template's flags that shape what core-agent builds, so the
# check boots what a task boots (internal/agent's test keeps the two in
# step): the provider decides which tools come on by default, and a
# disabled tool another one needs is a startup error.
ARG TASK_FLAGS="--provider=vertex --model=gemini-3.8-flash --checkpoint=operator --disable-tools=bash,read_file,read_many_files,view_file_outline,write_file,edit_file,delete_file,stat,list_dir,glob,grep --agentic-tools=false --no-background-agents --no-pricing-refresh"
ARG VCS_TASK_FLAGS="--provider=vertex --model=gemini-3.8-flash --checkpoint=operator --disable-tools=bash,read_many_files,view_file_outline,delete_file,stat,list_dir,glob --agentic-tools=false --no-background-agents --no-pricing-refresh"
RUN set -e; mkdir -p /tmp/content; \
    boot() { \
      label="$1"; cfg="$2"; extra="$3"; flags="$4"; recipe="$5"; \
      SMOKE_TOKEN=smoke CORE_AGENT_GOOGLE_AUTH=injected CORE_AGENT_GITHUB_TOKEN=injected CORE_AGENT_SWITCHBOARD_TOKEN=injected CORE_AGENT_TRUSTED_DIRS="/tmp,/out/${recipe}/.agents" GOOGLE_CLOUD_PROJECT=smoke GOOGLE_CLOUD_LOCATION=global \
        core-agent -c "$cfg" $extra $flags --no-repl \
          --attach-listen=127.0.0.1:8484 --attach-token=SMOKE_TOKEN \
          --session-db --session-db-path="/tmp/smoke-$label.db" --agents-content-dir=/tmp/content \
          >"/tmp/smoke-$label.log" 2>&1 & pid=$!; \
      code=000; for i in $(seq 1 60); do \
        code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H "Host: $ROUTER_HOST" -H "Authorization: Bearer smoke" http://127.0.0.1:8484/sessions || true); \
        [ "$code" != 000 ] && break; \
        kill -0 "$pid" 2>/dev/null || break; sleep 0.5; \
      done; \
      if [ "$code" = 200 ]; then \
        sid=$(curl -sf -m 10 -X POST -H "Host: $ROUTER_HOST" -H "Authorization: Bearer smoke" -H "Content-Type: application/json" -d '{}' http://127.0.0.1:8484/sessions | jq -r '.sessionID // empty' || true); \
        if [ -z "$sid" ]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; echo "recipe $label: POST /sessions returned no sessionID:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
        if ! curl -sf -m 10 -H "Host: $ROUTER_HOST" -H "Authorization: Bearer smoke" "http://127.0.0.1:8484/sessions/$sid/mcp" > "/tmp/smoke-$label-mcp.json"; then \
          kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; \
          echo "recipe $label: GET /sessions/$sid/mcp failed:"; cat "/tmp/smoke-$label.log"; exit 1; \
        fi; \
        if [ "$recipe" = "gke-triage-f" ]; then \
          if ! jq -e '(.servers | map(.name) | sort) == ["lookout"] and (.servers[] | select(.name=="lookout") | (.tools | map(.name) | sort)) == ["lookout_k8s_event_timeline","lookout_k8s_list_clusters","lookout_k8s_list_resources","lookout_k8s_resource_spec","lookout_k8s_state_edges","lookout_k8s_triage_delta","lookout_k8s_triage_logs","lookout_k8s_triage_workload"]' "/tmp/smoke-$label-mcp.json" >/dev/null; then \
            kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; \
            echo "recipe $label: unexpected main-agent triage MCP tools:"; cat "/tmp/smoke-$label-mcp.json"; cat "/tmp/smoke-$label.log"; exit 1; \
          fi; \
        else \
          if ! jq -e '(.servers | map(.name) | sort) == ["leeway","lookout"] and (.servers[] | select(.name=="lookout") | (.tools | map(.name) | sort)) == ["lookout_k8s_audit_all","lookout_k8s_event_timeline","lookout_k8s_list_clusters","lookout_k8s_list_resources","lookout_k8s_resource_spec","lookout_k8s_state_edges","lookout_k8s_triage_delta","lookout_k8s_triage_logs","lookout_k8s_triage_workload"] and (.servers[] | select(.name=="leeway") | (.tools | map(.name))) == ["leeway_report"]' "/tmp/smoke-$label-mcp.json" >/dev/null; then \
            kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; \
            echo "recipe $label: unexpected main-agent chat MCP tools:"; cat "/tmp/smoke-$label-mcp.json"; cat "/tmp/smoke-$label.log"; exit 1; \
          fi; \
        fi; \
      fi; \
      kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; \
      if [ "$code" = 000 ]; then echo "recipe $label: core-agent did not start:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if [ "$code" != 200 ]; then echo "recipe $label: GET /sessions as the router ($ROUTER_HOST) answered $code, not 200"; exit 1; fi; \
      if grep -q "workspace trust:.*is untrusted" "/tmp/smoke-$label.log"; then echo "recipe $label: workspace trust clamped config:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if grep -q "is not deliverable" "/tmp/smoke-$label.log"; then echo "recipe $label: alert target was dropped:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if grep -Eq "tools allowlist names |has no tool matching " "/tmp/smoke-$label.log"; then echo "recipe $label: unmatched MCP tool allowlist:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if grep -Eq "mcp: [0-9]+ server\(s\), [1-9][0-9]* down" "/tmp/smoke-$label.log"; then echo "recipe $label: subagent MCP server failed to start:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if grep -Eq "mcp: [0-9]+ server\(s\) loaded .*\[[1-9][0-9]* failed" "/tmp/smoke-$label.log"; then echo "recipe $label: main-agent MCP server failed to start:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if grep -q "NOT loaded" "/tmp/smoke-$label.log"; then echo "recipe $label: bundled skill was dropped:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if ! grep -Eq "instruction: loaded [1-9][0-9]* file\(s\): .*/out/${recipe}/\.agents/AGENTS\.md" "/tmp/smoke-$label.log"; then echo "recipe $label: native AGENTS.md was not loaded:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if [ "$recipe" = "gke-triage-f" ] && ! grep -q "skills: 1 loaded — k8s-triage" "/tmp/smoke-$label.log"; then echo "recipe $label: expected 1 bundled skill (k8s-triage):"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if [ "$recipe" = "gke-chat" ] && ! grep -q "skills: 5 loaded — cluster-health, gitops-drift, k8s-audit, k8s-capacity, k8s-triage" "/tmp/smoke-$label.log"; then echo "recipe $label: expected 5 bundled skills:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      echo "recipe $label: core-agent starts and answers the router"; \
    }; \
    for b in gke-triage-f gke-chat; do \
      boot "$b" "/out/$b/.agents/config.json" "" "$TASK_FLAGS" "$b"; \
      jq '.permissions.allow += ["spawn_agent:propose-change","edit_file:/tmp/repos/*","write_file:/tmp/repos/*","vcs_commit:*","alert:slack"] | . + {"vcs":{"workspace_dir":"/tmp/repos","forges":[{"name":"github","provider":"github","token_env":"CORE_AGENT_GITHUB_TOKEN"}],"repositories":[{"forge":"github","repository":"example/repo","role":"gitops","base_branch":"main"}]},"subagents":[{"name":"propose-change","description":"Opens GitOps PRs","instructions":"@include builtin:propose-change\n","mode":"chat","capability":"change_executor","tools":["read_file","edit_file","write_file","grep","vcs_capabilities","vcs_clone","vcs_status","vcs_diff","vcs_log","vcs_commit","vcs_publish","vcs_propose","vcs_proposal_view","vcs_proposal_list"],"mcp":[],"skills":[],"budgets":{"max_turns":15,"max_wallclock_seconds":180}}],"alerts":{"rate_limit_per_target":"5/min","targets":[{"name":"slack","kind":"webhook","template":"switchboard","conversation":"C0123456789","url":"http://switchboard.switchboard.svc.cluster.local:8080/v1/messages","auth":{"bearer_env":"CORE_AGENT_SWITCHBOARD_TOKEN"}}]}}' "/out/$b/.agents/config.json" > "/tmp/aug-$b.json"; \
      boot "$b-augmented" "/tmp/aug-$b.json" "--agents-dir=/out/$b/.agents" "$VCS_TASK_FLAGS" "$b"; \
    done
# Boot-check gke-watchboard (#133): base and alert-only augmented (never
# VCS or propose-change), verifying lookout-only 8-tool MCP registration,
# native AGENTS.md, and k8s-triage skill loading (with playbooks beside it).
RUN set -e; mkdir -p /tmp/content; \
    boot_wb() { \
      label="$1"; cfg="$2"; extra="$3"; \
      SMOKE_TOKEN=smoke CORE_AGENT_GOOGLE_AUTH=injected CORE_AGENT_SWITCHBOARD_TOKEN=injected CORE_AGENT_TRUSTED_DIRS="/tmp,/out/gke-watchboard/.agents" GOOGLE_CLOUD_PROJECT=smoke GOOGLE_CLOUD_LOCATION=global \
        core-agent -c "$cfg" $extra $TASK_FLAGS --no-repl \
          --attach-listen=127.0.0.1:8484 --attach-token=SMOKE_TOKEN \
          --session-db --session-db-path="/tmp/smoke-$label.db" --agents-content-dir=/tmp/content \
          >"/tmp/smoke-$label.log" 2>&1 & pid=$!; \
      code=000; for i in $(seq 1 60); do \
        code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H "Host: $ROUTER_HOST" -H "Authorization: Bearer smoke" http://127.0.0.1:8484/sessions || true); \
        [ "$code" != 000 ] && break; \
        kill -0 "$pid" 2>/dev/null || break; sleep 0.5; \
      done; \
      if [ "$code" = 200 ]; then \
        sid=$(curl -sf -m 10 -X POST -H "Host: $ROUTER_HOST" -H "Authorization: Bearer smoke" -H "Content-Type: application/json" -d '{}' http://127.0.0.1:8484/sessions | jq -r '.sessionID // empty' || true); \
        if [ -z "$sid" ]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; echo "recipe $label: POST /sessions returned no sessionID:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
        if ! curl -sf -m 10 -H "Host: $ROUTER_HOST" -H "Authorization: Bearer smoke" "http://127.0.0.1:8484/sessions/$sid/mcp" > "/tmp/smoke-$label-mcp.json"; then \
          kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; \
          echo "recipe $label: GET /sessions/$sid/mcp failed:"; cat "/tmp/smoke-$label.log"; exit 1; \
        fi; \
        if ! jq -e '(.servers | map(.name)) == ["lookout"] and (.servers[] | select(.name=="lookout") | (.tools | map(.name) | sort)) == ["lookout_k8s_event_timeline","lookout_k8s_list_clusters","lookout_k8s_list_resources","lookout_k8s_resource_spec","lookout_k8s_state_edges","lookout_k8s_triage_delta","lookout_k8s_triage_logs","lookout_k8s_triage_workload"]' "/tmp/smoke-$label-mcp.json" >/dev/null; then \
          kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; \
          echo "recipe $label: unexpected watchboard MCP tools:"; cat "/tmp/smoke-$label-mcp.json"; cat "/tmp/smoke-$label.log"; exit 1; \
        fi; \
      fi; \
      kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; \
      if [ "$code" = 000 ]; then echo "recipe $label: core-agent did not start:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if [ "$code" != 200 ]; then echo "recipe $label: GET /sessions as the router ($ROUTER_HOST) answered $code, not 200"; exit 1; fi; \
      if grep -q "workspace trust:.*is untrusted" "/tmp/smoke-$label.log"; then echo "recipe $label: workspace trust clamped config:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if grep -q "is not deliverable" "/tmp/smoke-$label.log"; then echo "recipe $label: alert target was dropped:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if grep -Eq "tools allowlist names |has no tool matching " "/tmp/smoke-$label.log"; then echo "recipe $label: unmatched MCP tool allowlist:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if grep -Eq "mcp: [0-9]+ server\(s\), [1-9][0-9]* down" "/tmp/smoke-$label.log"; then echo "recipe $label: subagent MCP server failed to start:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if grep -Eq "mcp: [0-9]+ server\(s\) loaded .*\[[1-9][0-9]* failed" "/tmp/smoke-$label.log"; then echo "recipe $label: main-agent MCP server failed to start:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if grep -q "core-agent: skills:.*NOT loaded" "/tmp/smoke-$label.log"; then echo "recipe $label: skill was withheld:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if ! grep -Eq "instruction: loaded [1-9][0-9]* file\(s\): .*/out/gke-watchboard/\.agents/AGENTS\.md" "/tmp/smoke-$label.log"; then echo "recipe $label: native AGENTS.md was not loaded:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if ! grep -q "skills: 1 loaded — k8s-triage" "/tmp/smoke-$label.log"; then echo "recipe $label: k8s-triage skill was not loaded:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if ! grep -q "subagents: 0 configured" "/tmp/smoke-$label.log"; then echo "recipe $label: watchboard must configure zero subagents:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      echo "recipe $label: core-agent starts and answers the router"; \
    }; \
    boot_wb "gke-watchboard" "/out/gke-watchboard/.agents/config.json" ""; \
    jq '.permissions.allow += ["alert:slack"] | . + {"alerts":{"rate_limit_per_target":"5/min","targets":[{"name":"slack","kind":"webhook","template":"switchboard","conversation":"C0123456789","url":"http://switchboard.switchboard.svc.cluster.local:8080/v1/messages","auth":{"bearer_env":"CORE_AGENT_SWITCHBOARD_TOKEN"}}]}}' "/out/gke-watchboard/.agents/config.json" > "/tmp/aug-gke-watchboard.json"; \
    boot_wb "gke-watchboard-alert" "/tmp/aug-gke-watchboard.json" "--agents-dir=/out/gke-watchboard/.agents"

# Distroless nonroot (UID/GID 65532): carries CA certificates, tzdata and
# /etc/passwd, with no shell, package manager, git or curl. ax-task-runner
# execs core-agent directly, core-agent's vcs_* tools use go-git in process,
# and core-agent runs lookout mcp and leeway mcp directly over stdio.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

LABEL org.opencontainers.image.source="https://github.com/go-steer/shipmind" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.description="core-agent as an ax task image, with lookout, leeway, and the shipmind sre-triage agent recipes."

COPY --from=ax-runner /out/ax-task-runner /usr/local/bin/ax-task-runner
COPY --from=core-agent /out/core-agent /usr/local/bin/core-agent
COPY --from=lookout /out/lookout /usr/local/bin/lookout
COPY --from=leeway /out/leeway /usr/local/bin/leeway
COPY --from=recipes /out /opt/core-agent/recipes

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/ax-task-runner"]
