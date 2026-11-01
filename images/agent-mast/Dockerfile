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

# The substrate profile's agent image: mast, run as an ax task.
#
#   ax-task-runner  ax's runner contract, built from the ax fork at the
#                   ax-runner pin in versions.yaml
#   mast, ax-mast-entry  mast and its ax entrypoint, built from mast at
#                   the mast pin, the same way mast's
#                   examples/deploy/ax/Dockerfile builds them
#   /opt/mast/workloads  the agent bundles in agents/sre-triage
#
# The pins arrive as build arguments read from versions.yaml
# (build-args.sh); this file has no defaults for them, so they can't
# drift from the bill of materials. Build from the repository root:
#
#   docker build -f images/agent-mast/Dockerfile $(images/agent-mast/build-args.sh) .

ARG GO_VERSION=1.26.6
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

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS mast
ARG MAST_REPO
ARG MAST_COMMIT
ARG TARGETOS
ARG TARGETARCH
RUN test -n "$MAST_REPO" && test -n "$MAST_COMMIT" && apk add --no-cache git
WORKDIR /src
RUN git init -q . \
 && git fetch -q --depth=1 "$MAST_REPO" "$MAST_COMMIT" \
 && git checkout -q FETCH_HEAD \
 && test "$(git rev-parse HEAD)" = "$MAST_COMMIT"
RUN go mod download
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/mast ./cmd/mast \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/ax-mast-entry ./examples/deploy/ax/entrypoint

FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
# git: mast clones repositories into workspaces. curl: for poking at
# mast from `ax ssh`.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git curl \
 && rm -rf /var/lib/apt/lists/*

LABEL org.opencontainers.image.source="https://github.com/go-steer/shipmind" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.description="mast as an ax task image, with the shipmind sre-triage agent bundles."

COPY --from=ax-runner /out/ax-task-runner /usr/local/bin/ax-task-runner
COPY --from=mast /out/mast /out/ax-mast-entry /usr/local/bin/
COPY agents/sre-triage/gke-triage /opt/mast/workloads/gke-triage
COPY agents/sre-triage/gke-triage-f /opt/mast/workloads/gke-triage-f
COPY agents/sre-triage/gke-chat /opt/mast/workloads/gke-chat

ENTRYPOINT ["/usr/local/bin/ax-task-runner"]
