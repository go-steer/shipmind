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

# The end-to-end and posture audit runner: shipmind (from this checkout),
# the ax CLI (from the ax fork at versions.yaml's ax pin), static lookout
# (-tags allproviders) and leeway (from their pins in versions.yaml), and
# kubectl, plus the scenarios, for running `shipmind scenario run -in-cluster`
# and `shipmind audit run -in-cluster` as CronJobs (#127). The build arguments
# come from images/e2e-runner/build-args.sh; none has a default, so a missing
# one fails the build.

ARG GO_VERSION
ARG AX_GO_VERSION

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS shipmind
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /out/shipmind ./cmd/shipmind

FROM --platform=$BUILDPLATFORM golang:${AX_GO_VERSION}-alpine AS ax
ARG AX_REPO
ARG AX_COMMIT
ARG TARGETOS
ARG TARGETARCH
RUN apk add --no-cache git \
 && git init -q /src && cd /src \
 && git fetch -q --depth=1 "$AX_REPO" "$AX_COMMIT" && git checkout -q FETCH_HEAD \
 && test "$(git rev-parse HEAD)" = "$AX_COMMIT" \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /out/ax ./cmd/ax

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS lookout
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
    go build -tags allproviders -trimpath -ldflags "$ldflags" -o /out/lookout ./cmd/lookout

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS leeway
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
    go build -trimpath -ldflags "$ldflags" -o /out/leeway ./cmd/leeway

FROM alpine:3.22 AS kubectl
ARG KUBECTL_VERSION
ARG TARGETARCH
RUN apk add --no-cache curl \
 && curl -fsSLo /kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
 && echo "$(curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256")  /kubectl" | sha256sum -c - \
 && chmod 0755 /kubectl

FROM gcr.io/distroless/static:nonroot
COPY --from=shipmind /out/shipmind /usr/local/bin/shipmind
COPY --from=ax /out/ax /usr/local/bin/ax
COPY --from=lookout /out/lookout /usr/local/bin/lookout
COPY --from=leeway /out/leeway /usr/local/bin/leeway
COPY --from=kubectl /kubectl /usr/local/bin/kubectl
COPY e2e/scenarios /scenarios
LABEL org.opencontainers.image.source="https://github.com/go-steer/shipmind" \
      org.opencontainers.image.licenses="Apache-2.0"
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/shipmind"]
