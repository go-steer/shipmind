#!/bin/sh
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

# Keep a Kubernetes Secret holding a fresh Google OAuth access token for this
# pod's Workload Identity, so Agent Substrate's egress gateway can inject it
# (via the Kubernetes Secrets credential provider) into actors' requests to
# Google APIs. Stopgap until Substrate's actor-JWT token exchange (#1661).
#
# Env: SECRET_NAME (default google-token), SECRET_KEY (default token),
#      REFRESH_SECONDS (default 1800; tokens last about an hour).
set -u
SA_DIR=/var/run/secrets/kubernetes.io/serviceaccount
NS=$(cat "$SA_DIR/namespace")
SECRET=${SECRET_NAME:-google-token}
KEY=${SECRET_KEY:-token}
INTERVAL=${REFRESH_SECONDS:-1800}
META=http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default
API=https://kubernetes.default.svc

refresh() {
  tok=$(curl -sf -H 'Metadata-Flavor: Google' "$META/token" | jq -r .access_token) || return 1
  [ -n "$tok" ] && [ "$tok" != null ] || return 1
  b64=$(printf %s "$tok" | base64 | tr -d '\n')
  body=$(printf '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"%s","namespace":"%s"},"type":"Opaque","data":{"%s":"%s"}}' \
    "$SECRET" "$NS" "$KEY" "$b64")
  printf %s "$body" | curl -sf -X PATCH --cacert "$SA_DIR/ca.crt" \
    -H "Authorization: Bearer $(cat "$SA_DIR/token")" \
    -H 'Content-Type: application/apply-patch+yaml' \
    --data-binary @- \
    "$API/api/v1/namespaces/$NS/secrets/$SECRET?fieldManager=token-refresher&force=true" >/dev/null
}

identity=$(curl -sf -H 'Metadata-Flavor: Google' "$META/email" || echo unknown)
echo "token-refresher: identity=$identity secret=$NS/$SECRET key=$KEY every ${INTERVAL}s"
while true; do
  if refresh; then
    echo "$(date -u +%FT%TZ) refreshed $NS/$SECRET"
    sleep "$INTERVAL"
  else
    echo "$(date -u +%FT%TZ) refresh failed; retrying in 60s"
    sleep 60
  fi
done
