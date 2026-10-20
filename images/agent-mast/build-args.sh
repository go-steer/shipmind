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
#   images/agent-mast/build-args.sh            --build-arg K=V ... (for docker build)
#   images/agent-mast/build-args.sh --lines    K=V, one per line (for build-push-action)
#
# Uses yq (preinstalled on GitHub's runners), or the mikefarah/yq image.
set -euo pipefail
cd "$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"

YQ_IMAGE=mikefarah/yq:4.54.1
yq_() {
  if command -v yq >/dev/null 2>&1; then
    yq "$@" versions.yaml
  else
    docker run --rm -i "$YQ_IMAGE" "$@" <versions.yaml
  fi
}

pin() {
  local name=$1 field=$2 v
  v=$(yq_ ".components[] | select(.name == \"$name\") | .$field")
  if [[ -z "$v" || "$v" == null ]]; then
    echo "build-args.sh: versions.yaml has no $field for component $name" >&2
    exit 1
  fi
  printf '%s' "$v"
}

args=(
  "MAST_REPO=https://$(pin mast source).git"
  "MAST_COMMIT=$(pin mast version)"
  "AX_RUNNER_REPO=https://$(pin ax-runner source).git"
  "AX_RUNNER_COMMIT=$(pin ax-runner version)"
)
for a in "${args[@]}"; do
  case "${1:-}" in
    --lines) printf '%s\n' "$a" ;;
    *) printf -- '--build-arg %s ' "$a" ;;
  esac
done
[[ "${1:-}" == --lines ]] || echo
