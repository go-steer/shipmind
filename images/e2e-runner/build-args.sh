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
# files, the ax, k8s-lookout and leeway pins from versions.yaml, and
# kubectl's version from the line below. The Dockerfile has no defaults, so
# they can't drift.
#
#   images/e2e-runner/build-args.sh            --build-arg K=V ... (for docker build)
#   images/e2e-runner/build-args.sh --lines    K=V, one per line (for build-push-action)
set -euo pipefail
cd "$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"

# kubectl within one minor version of the clusters it drives.
KUBECTL_VERSION=v1.36.5

# yq is preinstalled on GitHub's runners; elsewhere, its image.
YQ_IMAGE=mikefarah/yq:4.54.1
yq_file() {
  local file=$1
  shift
  if command -v yq >/dev/null 2>&1; then
    yq "$@" "$file"
  else
    docker run --rm -i "$YQ_IMAGE" "$@" <"$file"
  fi
}

pin() {
  local name=$1 field=$2 v
  v=$(yq_file versions.yaml ".components[] | select(.name == \"$name\") | .$field")
  [[ -n "$v" && "$v" != null ]] || { echo "build-args.sh: versions.yaml has no $name $field" >&2; exit 1; }
  printf '%s' "$v"
}

VERSION_RE='^([0-9a-f]{40}|v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?)$'

AX_COMMIT=${AX_COMMIT_OVERRIDE:-$(pin ax version)}
[[ "$AX_COMMIT" =~ ^[0-9a-f]{40}$ ]] || { echo "build-args.sh: ax commit $AX_COMMIT: want a full SHA" >&2; exit 1; }
AX_REPO="https://$(pin ax source).git"
# ax's own Go version, from its go.mod at the pin.
AX_GO_VERSION=$(curl -fsSL "https://raw.githubusercontent.com/$(pin ax source | sed 's|github.com/||')/$AX_COMMIT/go.mod" | awk '$1 == "go" {print $2; exit}')
[[ -n "$AX_GO_VERSION" ]] || { echo "build-args.sh: no go directive in ax's go.mod at $AX_COMMIT" >&2; exit 1; }
GO_VERSION=$(awk '$1 == "go" {print $2; exit}' go.mod)

LOOKOUT_VERSION=$(pin k8s-lookout version)
LEEWAY_VERSION=$(pin leeway version)
[[ "$LOOKOUT_VERSION" =~ $VERSION_RE ]] || { echo "build-args.sh: k8s-lookout version $LOOKOUT_VERSION: want a release tag or full SHA" >&2; exit 1; }
[[ "$LEEWAY_VERSION" =~ $VERSION_RE ]] || { echo "build-args.sh: leeway version $LEEWAY_VERSION: want a release tag or full SHA" >&2; exit 1; }

LOOKOUT_COMMIT=""
if [[ "$LOOKOUT_VERSION" =~ ^[0-9a-f]{40}$ ]]; then
  LOOKOUT_COMMIT=$LOOKOUT_VERSION
elif [[ -f deploy/vendor/k8s-lookout/VENDOR.yaml ]]; then
  v_ver=$(yq_file deploy/vendor/k8s-lookout/VENDOR.yaml '.version')
  v_sha=$(yq_file deploy/vendor/k8s-lookout/VENDOR.yaml '.commit')
  if [[ "$v_ver" == "$LOOKOUT_VERSION" && "$v_sha" =~ ^[0-9a-f]{40}$ ]]; then
    LOOKOUT_COMMIT=$v_sha
  fi
fi
[[ "$LOOKOUT_COMMIT" =~ ^[0-9a-f]{40}$ ]] || { echo "build-args.sh: k8s-lookout $LOOKOUT_VERSION: want matching 40-char commit in deploy/vendor/k8s-lookout/VENDOR.yaml" >&2; exit 1; }

LEEWAY_COMMIT=""
if [[ "$LEEWAY_VERSION" =~ ^[0-9a-f]{40}$ ]]; then
  LEEWAY_COMMIT=$LEEWAY_VERSION
else
  LEEWAY_COMMIT=$(pin leeway note | grep -Eo '\b[0-9a-f]{40}\b' | head -n1 || true)
fi
[[ "$LEEWAY_COMMIT" =~ ^[0-9a-f]{40}$ ]] || { echo "build-args.sh: leeway $LEEWAY_VERSION: want a full SHA in version or its 40-char tag commit in note" >&2; exit 1; }

args=(
  "GO_VERSION=$GO_VERSION"
  "AX_GO_VERSION=$AX_GO_VERSION"
  "AX_REPO=$AX_REPO"
  "AX_COMMIT=$AX_COMMIT"
  "LOOKOUT_REPO=https://$(pin k8s-lookout source).git"
  "LOOKOUT_VERSION=$LOOKOUT_VERSION"
  "LOOKOUT_COMMIT=$LOOKOUT_COMMIT"
  "LEEWAY_REPO=https://$(pin leeway source).git"
  "LEEWAY_VERSION=$LEEWAY_VERSION"
  "LEEWAY_COMMIT=$LEEWAY_COMMIT"
  "KUBECTL_VERSION=$KUBECTL_VERSION"
)
for a in "${args[@]}"; do
  case "${1:-}" in
    --lines) printf '%s\n' "$a" ;;
    *) printf -- '--build-arg %s ' "$a" ;;
  esac
done
[[ "${1:-}" == --lines ]] || echo
