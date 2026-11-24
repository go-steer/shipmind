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
# run as an ax task in serve mode, the harness that replaces mast.
#
#   ax-task-runner  ax's runner contract, built from the ax fork at the
#                   ax-runner pin in versions.yaml
#   core-agent      built from core-agent at the core-agent pin, a full
#                   commit SHA, checked after the fetch
#   lookout         built (-tags allproviders) from k8s-lookout at its pin
#                   in versions.yaml; gke-chat runs lookout mcp
#                   --profile=audit as a stdio MCP server (#127)
#   leeway          built from leeway at its pin in versions.yaml; gke-chat
#                   runs leeway mcp as a stdio MCP server (#127)
#   /opt/core-agent/recipes/<bundle>  agents/sre-triage's mast bundles,
#                   converted at build time by that same core-agent
#                   (`core-agent config import-workload`), so the image
#                   carries what the pinned importer makes of the bundle
#                   and the bundles keep one source while mast still runs
#                   them too. A bundle that stops converting fails the
#                   build. agents/sre-triage/ax/core-agent-<bundle>.json,
#                   when present, is merged over the converted config:
#                   the deployment's attach settings, which a mast bundle
#                   has no place for. The importer gives each specialist
#                   its own content root (.agents/roots/<name>/mcp.json,
#                   core-agent#1498) and the main agent no MCP servers;
#                   the build checks both, then gives the main agent only
#                   the gke tools
#                   agents/sre-triage/ax/core-agent-<bundle>.main-tools.json
#                   names (a bundle with no such file gets none). A bundle
#                   with core-agent-<bundle>.fold-specialist runs the
#                   named specialist as its main agent instead of
#                   delegating to it: its instructions, up to the
#                   finish_task reporting a delegated specialist uses, are
#                   appended to the recipe's AGENTS.md (whose roster
#                   section goes), then core-agent-<bundle>.report.md, and
#                   the specialist is removed, so the agent that receives a
#                   signal is the one that reads the cluster, and nothing
#                   has to be re-typed into a delegation (core-agent hands
#                   a subagent only what the model writes). Each converted recipe is then booted
#                   once (echo provider, no network needed) until its
#                   attach listener answers, so a config core-agent would
#                   refuse at task start fails the build instead. The
#                   check asks as Substrate's router does, under the
#                   router's Host (ax-task-runner's pass-through keeps
#                   it), so a recipe that would answer the router 421
#                   (core-agent#1358's guard, without attach.allowed_hosts)
#                   fails it too.
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
# stage, to run the importer below. --version reports the commit;
# versions.yaml's note names the release it is.
RUN ldflags="-s -w -X github.com/go-steer/core-agent/v3/internal/version.Version=${CORE_AGENT_COMMIT} -X github.com/go-steer/core-agent/v3/internal/version.Commit=${CORE_AGENT_COMMIT}" \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "$ldflags" -o /out/core-agent ./cmd/core-agent \
 && CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o /usr/local/bin/core-agent ./cmd/core-agent

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
COPY --from=leeway /usr/local/bin/leeway /usr/local/bin/leeway
COPY agents/sre-triage /in
# The posture servers, run exactly as gke-chat's mcp.json runs them, with no
# network and no credentials: lookout must offer its cluster-choice tool
# (multi-cluster mode) and the audit tools, leeway its report tool. The
# boot below then has core-agent list both servers' tools for real.
RUN set -e; \
    lookout $(jq -r '.servers.lookout.args | join(" ")' /in/gke-chat/mcp.json) --list-tools > /tmp/lookout-tools; \
    grep -q '^k8s_list_clusters ' /tmp/lookout-tools; \
    grep -q '^k8s_audit_all ' /tmp/lookout-tools; \
    printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | leeway $(jq -r '.servers.leeway.args | join(" ")' /in/gke-chat/mcp.json) | grep -q '"name":"report"'; \
    for b in gke-triage-f gke-chat; do \
      core-agent config import-workload -o "/out/$b" "/in/$b"; \
      over="/in/ax/core-agent-$b.json"; \
      if [ -f "$over" ]; then \
        jq -s '.[0] * .[1]' "/out/$b/.agents/config.json" "$over" > /tmp/config.json; \
        mv /tmp/config.json "/out/$b/.agents/config.json"; \
      fi; \
      mkdir -p "/out/$b/.agents/plans"; \
      mcp="/out/$b/.agents/mcp.json"; \
      for sa in /out/$b/.agents/subagents/*.md; do \
        [ -f "$sa" ] || continue; \
        n=$(basename "$sa" .md); \
        if grep -q '^mcp_tools:\|^mcp:' "$sa" && ! grep -qx 'mode: single_turn' "$sa" && { ! grep -qx "root: roots/$n" "$sa" || [ ! -f "/out/$b/.agents/roots/$n/mcp.json" ]; }; then echo "recipe $b: the importer gave specialist $n, which uses MCP, no content root of its own"; exit 1; fi; \
      done; \
      if [ -e "$mcp" ]; then echo "recipe $b: the importer gave the main agent MCP servers"; exit 1; fi; \
      fold="/in/ax/core-agent-$b.fold-specialist"; \
      if [ -f "$fold" ]; then \
        n=$(tr -d ' \n' < "$fold"); sa="/out/$b/.agents/subagents/$n.md"; \
        test -f "$sa"; \
        awk '/^## Subagent Roster/ { skip = 1; next } skip && /^## / { skip = 0 } !skip' "/out/$b/AGENTS.md" > /tmp/agents.md; \
        awk 'f >= 2 { print; next } /^---$/ { f++ }' "$sa" > /tmp/body.md; \
        grep -q '^Return your finding by calling `finish_task`' /tmp/body.md; \
        awk '/^Return your finding by calling `finish_task`/ { exit } { print }' /tmp/body.md | sed 's/ and leave `proposed_change` empty//' > /tmp/diagnosis.md; \
        { cat /tmp/agents.md; printf '\n## Diagnosis\n\n'; cat /tmp/diagnosis.md; cat "/in/ax/core-agent-$b.report.md"; } > "/out/$b/AGENTS.md"; \
        cp "/in/$b/mcp.json" "$mcp"; \
        rm "$sa"; rm -rf "/out/$b/.agents/roots/$n"; \
        if grep -qF "\`$n\`" "/out/$b/AGENTS.md" || grep -qF finish_task "/out/$b/AGENTS.md" || ! grep -q '^## Diagnosis$' "/out/$b/AGENTS.md"; then \
          echo "recipe $b: folding $n left its roster entry or finish_task, or no Diagnosis section:"; cat "/out/$b/AGENTS.md"; exit 1; \
        fi; \
        if ls /out/$b/.agents/subagents/*.md >/dev/null 2>&1; then echo "recipe $b: subagents remain after folding $n"; exit 1; fi; \
      fi; \
      main="/in/ax/core-agent-$b.main-tools.json"; \
      if [ -f "$main" ] && [ "$(jq length "$main")" -gt 0 ]; then \
        [ -f "$mcp" ] || cp "/in/$b/mcp.json" "$mcp"; \
        jq --slurpfile t "$main" '.servers = {"gke": (.servers.gke | .tools = $t[0])}' "$mcp" > /tmp/mcp.json; \
        mv /tmp/mcp.json "$mcp"; \
        if ! jq -e --slurpfile t "$main" '(.servers | keys) == ["gke"] and .servers.gke.tools == $t[0]' "$mcp" >/dev/null; then echo "recipe $b: main agent's tools aren't $(cat "$main")"; exit 1; fi; \
      else \
        rm -f "$mcp"; \
      fi; \
      jq -S . "/out/$b/.agents/config.json" > "/tmp/got-$b.json"; \
      jq -S . "/in/ax/core-agent-$b.base.json" > "/tmp/want-$b.json"; \
      if ! cmp -s "/tmp/got-$b.json" "/tmp/want-$b.json"; then \
        echo "recipe $b: core-agent-$b.base.json drifted from imported $b:"; \
        diff -u "/tmp/want-$b.json" "/tmp/got-$b.json" || true; \
        exit 1; \
      fi; \
    done; \
    jq -e '(.servers | keys | sort) == ["leeway", "lookout"]' /out/gke-chat/.agents/roots/posture-audit/mcp.json >/dev/null; \
    jq -e '(.servers | keys) == ["gke"]' /out/gke-chat/.agents/roots/_fallback/mcp.json >/dev/null
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
      label="$1"; cfg="$2"; extra="$3"; flags="$4"; \
      SMOKE_TOKEN=smoke CORE_AGENT_GOOGLE_AUTH=injected CORE_AGENT_GITHUB_TOKEN=injected CORE_AGENT_SWITCHBOARD_TOKEN=injected CORE_AGENT_TRUSTED_DIRS="/tmp,/out/${5}/.agents" GOOGLE_CLOUD_PROJECT=smoke GOOGLE_CLOUD_LOCATION=global \
        core-agent -c "$cfg" $extra $flags --no-repl \
          --attach-listen=127.0.0.1:8484 --attach-token=SMOKE_TOKEN \
          --session-db --session-db-path="/tmp/smoke-$label.db" --agents-content-dir=/tmp/content \
          >"/tmp/smoke-$label.log" 2>&1 & pid=$!; \
      code=000; for i in $(seq 1 60); do \
        code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H "Host: $ROUTER_HOST" -H "Authorization: Bearer smoke" http://127.0.0.1:8484/sessions || true); \
        [ "$code" != 000 ] && break; \
        kill -0 "$pid" 2>/dev/null || break; sleep 0.5; \
      done; \
      kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; \
      if [ "$code" = 000 ]; then echo "recipe $label: core-agent did not start:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if [ "$code" != 200 ]; then echo "recipe $label: GET /sessions as the router ($ROUTER_HOST) answered $code, not 200"; exit 1; fi; \
      if grep -q "workspace trust:.*is untrusted" "/tmp/smoke-$label.log"; then echo "recipe $label: workspace trust clamped config:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if grep -q "is not deliverable" "/tmp/smoke-$label.log"; then echo "recipe $label: alert target was dropped:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if grep -Eq "tools allowlist names |has no tool matching " "/tmp/smoke-$label.log"; then echo "recipe $label: unmatched MCP tool allowlist:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      if grep -Eq "mcp: [0-9]+ server\(s\), [1-9][0-9]* down" "/tmp/smoke-$label.log"; then echo "recipe $label: subagent MCP server failed to start:"; cat "/tmp/smoke-$label.log"; exit 1; fi; \
      echo "recipe $label: core-agent starts and answers the router"; \
    }; \
    for b in gke-triage-f gke-chat; do \
      boot "$b" "/out/$b/.agents/config.json" "" "$TASK_FLAGS" "$b"; \
      jq '.permissions.allow += ["spawn_agent:propose-change","edit_file:/tmp/repos/*","write_file:/tmp/repos/*","vcs_commit:*","alert:slack"] | . + {"vcs":{"workspace_dir":"/tmp/repos","forges":[{"name":"github","provider":"github","token_env":"CORE_AGENT_GITHUB_TOKEN"}],"repositories":[{"forge":"github","repository":"example/repo","role":"gitops","base_branch":"main"}]},"subagents":[{"name":"propose-change","description":"Opens GitOps PRs","instructions":"@include builtin:propose-change\n","mode":"chat","capability":"change_executor","tools":["read_file","edit_file","write_file","grep","vcs_capabilities","vcs_clone","vcs_status","vcs_diff","vcs_log","vcs_commit","vcs_publish","vcs_propose","vcs_proposal_view","vcs_proposal_list"],"mcp":[],"budgets":{"max_turns":15,"max_wallclock_seconds":180}}],"alerts":{"rate_limit_per_target":"5/min","targets":[{"name":"slack","kind":"webhook","template":"switchboard","conversation":"C0123456789","url":"http://switchboard.switchboard.svc.cluster.local:8080/v1/messages","auth":{"bearer_env":"CORE_AGENT_SWITCHBOARD_TOKEN"}}]}}' "/out/$b/.agents/config.json" > "/tmp/aug-$b.json"; \
      boot "$b-augmented" "/tmp/aug-$b.json" "--agents-dir=/out/$b/.agents" "$VCS_TASK_FLAGS" "$b"; \
    done

# Distroless nonroot (UID/GID 65532): carries CA certificates, tzdata and
# /etc/passwd, with no shell, package manager, git or curl. ax-task-runner
# execs core-agent directly, core-agent's vcs_* tools use go-git in process,
# and core-agent runs lookout mcp and leeway mcp directly over stdio.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

LABEL org.opencontainers.image.source="https://github.com/go-steer/shipmind" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.description="core-agent as an ax task image, with lookout, leeway, and the shipmind sre-triage agent bundles."

COPY --from=ax-runner /out/ax-task-runner /usr/local/bin/ax-task-runner
COPY --from=core-agent /out/core-agent /usr/local/bin/core-agent
COPY --from=lookout /out/lookout /usr/local/bin/lookout
COPY --from=leeway /out/leeway /usr/local/bin/leeway
COPY --from=recipes /out /opt/core-agent/recipes

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/ax-task-runner"]
