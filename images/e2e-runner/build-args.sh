#!/usr/bin/env bash
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

# Print the e2e runner's docker build arguments: Go versions from go.mod
# files, the ax pin from versions.yaml, kubectl's version from the line
# below. The Dockerfile has no defaults, so they can't drift.
#
#   images/e2e-runner/build-args.sh            --build-arg K=V ... (for docker build)
#   images/e2e-runner/build-args.sh --lines    K=V, one per line (for build-push-action)
set -euo pipefail
cd "$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"

# kubectl within one minor version of the clusters it drives.
KUBECTL_VERSION=v1.36.5

# yq is preinstalled on GitHub's runners; elsewhere, its image.
YQ_IMAGE=mikefarah/yq:4.54.1
yq_() {
  if command -v yq >/dev/null 2>&1; then
    yq "$@" versions.yaml
  else
    docker run --rm -i "$YQ_IMAGE" "$@" <versions.yaml
  fi
}

pin() {
  local v
  v=$(yq_ ".components[] | select(.name == \"ax\") | .$1")
  [[ -n "$v" && "$v" != null ]] || { echo "build-args.sh: versions.yaml has no ax $1" >&2; exit 1; }
  printf '%s' "$v"
}
AX_COMMIT=$(pin version)
AX_REPO="https://$(pin source).git"
# ax's own Go version, from its go.mod at the pin.
AX_GO_VERSION=$(curl -fsSL "https://raw.githubusercontent.com/$(pin source | sed 's|github.com/||')/$AX_COMMIT/go.mod" | awk '$1 == "go" {print $2; exit}')
[[ -n "$AX_GO_VERSION" ]] || { echo "build-args.sh: no go directive in ax's go.mod at $AX_COMMIT" >&2; exit 1; }
GO_VERSION=$(awk '$1 == "go" {print $2; exit}' go.mod)

args=(
  "GO_VERSION=$GO_VERSION"
  "AX_GO_VERSION=$AX_GO_VERSION"
  "AX_REPO=$AX_REPO"
  "AX_COMMIT=$AX_COMMIT"
  "KUBECTL_VERSION=$KUBECTL_VERSION"
)
for a in "${args[@]}"; do
  case "${1:-}" in
    --lines) printf '%s\n' "$a" ;;
    *) printf -- '--build-arg %s ' "$a" ;;
  esac
done
[[ "${1:-}" == --lines ]] || echo
