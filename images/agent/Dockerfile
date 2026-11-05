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
#   /opt/core-agent/recipes/<bundle>  agents/sre-triage's mast bundles,
#                   converted at build time by that same core-agent
#                   (`core-agent config import-workload`), so the image
#                   carries what the pinned importer makes of the bundle
#                   and the bundles keep one source while mast still runs
#                   them too. A bundle that stops converting fails the
#                   build. agents/sre-triage/ax/core-agent-<bundle>.json,
#                   when present, is merged over the converted config:
#                   the deployment's attach settings, which a mast bundle
#                   has no place for. The MCP servers then go where mast
#                   had them: each specialist gets its own content root
#                   (.agents/roots/<name>) holding the bundle's mcp.json,
#                   which its own mcp_tools filter within, and the main
#                   agent keeps only the tools
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

ARG GO_VERSION=1.26.10
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

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308 AS core-agent
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

FROM --platform=$BUILDPLATFORM alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f AS recipes
RUN apk add --no-cache jq curl
COPY --from=core-agent /usr/local/bin/core-agent /usr/local/bin/core-agent
COPY agents/sre-triage /in
RUN set -e; \
    for b in gke-triage-f gke-chat; do \
      core-agent config import-workload -o "/out/$b" "/in/$b"; \
      over="/in/ax/core-agent-$b.json"; \
      if [ -f "$over" ]; then \
        jq -s '.[0] * .[1]' "/out/$b/.agents/config.json" "$over" > /tmp/config.json; \
        mv /tmp/config.json "/out/$b/.agents/config.json"; \
      fi; \
      mkdir -p "/out/$b/.agents/plans"; \
      fold="/in/ax/core-agent-$b.fold-specialist"; \
      if [ -f "$fold" ]; then \
        n=$(tr -d ' \n' < "$fold"); sa="/out/$b/.agents/subagents/$n.md"; \
        test -f "$sa"; \
        awk '/^## Subagent Roster/ { skip = 1; next } skip && /^## / { skip = 0 } !skip' "/out/$b/AGENTS.md" > /tmp/agents.md; \
        ! grep -q "Subagent Roster" /tmp/agents.md; \
        awk 'f >= 2 { print; next } /^---$/ { f++ }' "$sa" > /tmp/body.md; \
        grep -q '^Return your finding by calling `finish_task`' /tmp/body.md; \
        awk '/^Return your finding by calling `finish_task`/ { exit } { print }' /tmp/body.md > /tmp/diagnosis.md; \
        { cat /tmp/agents.md; printf '\n## Diagnosis\n\n'; cat /tmp/diagnosis.md; cat "/in/ax/core-agent-$b.report.md"; } > "/out/$b/AGENTS.md"; \
        rm "$sa"; \
      fi; \
      mcp="/out/$b/.agents/mcp.json"; \
      for sa in /out/$b/.agents/subagents/*.md; do \
        [ -f "$sa" ] || continue; \
        n=$(basename "$sa" .md); \
        mkdir -p "/out/$b/.agents/roots/$n"; \
        cp "$mcp" "/out/$b/.agents/roots/$n/mcp.json"; \
        awk -v n="$n" 'NR == 1 && $0 == "---" { print; print "root: roots/" n; next } { print }' "$sa" > /tmp/sa.md; \
        head -2 /tmp/sa.md | grep -qx "root: roots/$n"; \
        mv /tmp/sa.md "$sa"; \
      done; \
      main="/in/ax/core-agent-$b.main-tools.json"; \
      if [ -f "$main" ] && [ "$(jq length "$main")" -gt 0 ]; then \
        jq --slurpfile t "$main" '.servers |= map_values(.tools = $t[0])' "$mcp" > /tmp/mcp.json; \
        mv /tmp/mcp.json "$mcp"; \
      else \
        rm "$mcp"; \
      fi; \
    done
# The Host Substrate's router sends (switchboard's and lookout's
# default router URL; the nightly's too).
ARG ROUTER_HOST=atenet-router.ate-system.svc.cluster.local
# The task template's flags that shape what core-agent builds, so the
# check boots what a task boots (internal/agent's test keeps the two in
# step): the provider decides which tools come on by default, and a
# disabled tool another one needs is a startup error.
ARG TASK_FLAGS="--provider=vertex --model=gemini-3.8-flash --checkpoint=operator --disable-tools=bash,read_file,read_many_files,view_file_outline,write_file,edit_file,delete_file,stat,list_dir,glob,grep --agentic-tools=false --no-pricing-refresh"
RUN set -e; mkdir -p /tmp/content; \
    for b in gke-triage-f gke-chat; do \
      SMOKE_TOKEN=smoke CORE_AGENT_GOOGLE_AUTH=injected GOOGLE_CLOUD_PROJECT=smoke GOOGLE_CLOUD_LOCATION=global \
        core-agent -c "/out/$b/.agents/config.json" $TASK_FLAGS --no-repl \
          --attach-listen=127.0.0.1:8484 --attach-token=SMOKE_TOKEN \
          --session-db --session-db-path="/tmp/smoke-$b.db" --agents-content-dir=/tmp/content \
          >"/tmp/smoke-$b.log" 2>&1 & pid=$!; \
      code=000; for i in $(seq 1 60); do \
        code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' -H "Host: $ROUTER_HOST" -H "Authorization: Bearer smoke" http://127.0.0.1:8484/sessions || true); \
        [ "$code" != 000 ] && break; \
        kill -0 "$pid" 2>/dev/null || break; sleep 0.5; \
      done; \
      kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; \
      if [ "$code" = 000 ]; then echo "recipe $b: core-agent did not start:"; cat "/tmp/smoke-$b.log"; exit 1; fi; \
      if [ "$code" != 200 ]; then echo "recipe $b: GET /sessions as the router ($ROUTER_HOST) answered $code, not 200"; exit 1; fi; \
      echo "recipe $b: core-agent starts and answers the router"; \
    done

FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
# git: the agent clones repositories into workspaces. curl: for poking at
# core-agent from `ax ssh`.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git curl \
 && rm -rf /var/lib/apt/lists/*

LABEL org.opencontainers.image.source="https://github.com/go-steer/shipmind" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.description="core-agent as an ax task image, with the shipmind sre-triage agent bundles."

COPY --from=ax-runner /out/ax-task-runner /usr/local/bin/ax-task-runner
COPY --from=core-agent /out/core-agent /usr/local/bin/core-agent
COPY --from=recipes /out /opt/core-agent/recipes

ENTRYPOINT ["/usr/local/bin/ax-task-runner"]
