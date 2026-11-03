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
#   core-agent      built from core-agent at the core-agent pin
#   /opt/core-agent/recipes/<bundle>  agents/sre-triage's mast bundles,
#                   converted at build time by that same core-agent
#                   (`core-agent config import-workload`), so the image
#                   carries what the pinned importer makes of the bundle
#                   and the bundles keep one source while mast still runs
#                   them too. A bundle that stops converting fails the
#                   build. agents/sre-triage/ax/core-agent-<bundle>.json,
#                   when present, is merged over the converted config:
#                   the deployment's attach settings, which a mast bundle
#                   has no place for.
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
ARG CORE_AGENT_VERSION
ARG TARGETOS
ARG TARGETARCH
RUN test -n "$CORE_AGENT_REPO" && test -n "$CORE_AGENT_VERSION" && apk add --no-cache git
WORKDIR /src
# The version is a release tag or a full commit SHA (versions.yaml's
# rule); either can be fetched by name.
RUN git init -q . \
 && git fetch -q --depth=1 "$CORE_AGENT_REPO" "$CORE_AGENT_VERSION" \
 && git checkout -q FETCH_HEAD \
 && git rev-parse HEAD > /out-commit
RUN go mod download
# One binary for the image, built for the target, and one for this
# stage, to run the importer below.
RUN commit=$(cat /out-commit) \
 && ldflags="-s -w -X github.com/go-steer/core-agent/v3/internal/version.Version=${CORE_AGENT_VERSION} -X github.com/go-steer/core-agent/v3/internal/version.Commit=${commit}" \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "$ldflags" -o /out/core-agent ./cmd/core-agent \
 && CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o /usr/local/bin/core-agent ./cmd/core-agent

FROM --platform=$BUILDPLATFORM alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f AS recipes
RUN apk add --no-cache jq
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
