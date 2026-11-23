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

# Print the agent image's docker build arguments, read from the pins in
# versions.yaml, so the Dockerfile never carries a second copy of them.
#
#   images/agent/build-args.sh            --build-arg K=V ... (for docker build)
#   images/agent/build-args.sh --lines    K=V, one per line (for build-push-action)
#
# Uses yq (preinstalled on GitHub's runners), or the mikefarah/yq image.
set -euo pipefail
cd "$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"

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
  if [[ -z "$v" || "$v" == null ]]; then
    echo "build-args.sh: versions.yaml has no $field for component $name" >&2
    exit 1
  fi
  printf '%s' "$v"
}

VERSION_RE='^([0-9a-f]{40}|v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?)$'

AX_RUNNER_COMMIT=${AX_RUNNER_COMMIT_OVERRIDE:-$(pin ax-runner version)}
CORE_AGENT_COMMIT=$(pin core-agent version)
LOOKOUT_VERSION=$(pin k8s-lookout version)
LEEWAY_VERSION=$(pin leeway version)

[[ "$CORE_AGENT_COMMIT" =~ ^[0-9a-f]{40}$ ]] || { echo "build-args.sh: core-agent version $CORE_AGENT_COMMIT: want a full SHA (name the release in its note)" >&2; exit 1; }
[[ "$AX_RUNNER_COMMIT" =~ ^[0-9a-f]{40}$ ]] || { echo "build-args.sh: ax-runner commit $AX_RUNNER_COMMIT: want a full SHA" >&2; exit 1; }
[[ "$LOOKOUT_VERSION" =~ $VERSION_RE ]] || { echo "build-args.sh: k8s-lookout version $LOOKOUT_VERSION: want a release tag (vX.Y.Z) or full SHA" >&2; exit 1; }
[[ "$LEEWAY_VERSION" =~ $VERSION_RE ]] || { echo "build-args.sh: leeway version $LEEWAY_VERSION: want a release tag (vX.Y.Z) or full SHA" >&2; exit 1; }

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
  "CORE_AGENT_REPO=https://$(pin core-agent source).git"
  "CORE_AGENT_COMMIT=$CORE_AGENT_COMMIT"
  "AX_RUNNER_REPO=https://$(pin ax-runner source).git"
  "AX_RUNNER_COMMIT=$AX_RUNNER_COMMIT"
  "LOOKOUT_REPO=https://$(pin k8s-lookout source).git"
  "LOOKOUT_VERSION=$LOOKOUT_VERSION"
  "LOOKOUT_COMMIT=$LOOKOUT_COMMIT"
  "LEEWAY_REPO=https://$(pin leeway source).git"
  "LEEWAY_VERSION=$LEEWAY_VERSION"
  "LEEWAY_COMMIT=$LEEWAY_COMMIT"
)
for a in "${args[@]}"; do
  case "${1:-}" in
    --lines) printf '%s\n' "$a" ;;
    *) printf -- '--build-arg %s ' "$a" ;;
  esac
done
[[ "${1:-}" == --lines ]] || echo
