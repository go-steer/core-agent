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

# Generate demo bearer tokens, write users.json, and create the two
# Secrets the daemon + watcher need. Rehearsal tokens only — NOT prod.
#
# Identities MUST match .agents/config.hub.json:
#   platform-oncall@example.com  admin_identity   (operator attach + watcher --owner)
#   sa:lookout-watch             proxy_identity   (watcher POSTs incidents as this)
set -euo pipefail

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
source "${SCRIPT_DIR}/prereqs.sh"
require_coordinates || exit 1


PLATFORM_TOKEN=$(openssl rand -hex 32)
WATCHER_TOKEN=$(openssl rand -hex 32)

# 0700 before anything lands in it, so the two 0600 files below are never
# briefly reachable through a world-readable parent.
mkdir -p "${RIG_STATE_DIR}"
chmod 0700 "${RIG_STATE_DIR}"

# Stash tokens (0600) so later scripts can `source` them. Throwaway.
cat > "${RIG_STATE_DIR}/demo-tokens.env" <<EOF
export PLATFORM_TOKEN="${PLATFORM_TOKEN}"
export WATCHER_TOKEN="${WATCHER_TOKEN}"
EOF
chmod 0600 "${RIG_STATE_DIR}/demo-tokens.env"

# users.json bearer table for the daemon (mounted via the initContainer).
cat > "${RIG_STATE_DIR}/users.json" <<EOF
{
  "version": 1,
  "users": [
    { "identity": "${ADMIN_IDENTITY}",   "token": "${PLATFORM_TOKEN}" },
    { "identity": "${WATCHER_IDENTITY}",  "token": "${WATCHER_TOKEN}"  }
  ]
}
EOF
chmod 0600 "${RIG_STATE_DIR}/users.json"

# Namespace must exist before the Secrets land in it. (Idempotent.)
kubectl --context "${KUBE_CONTEXT}" create namespace "${DEMO_NS}" \
    --dry-run=client -o yaml | kubectl --context "${KUBE_CONTEXT}" apply -f -

# core-agent-users: the whole users.json. lookout-watch-token: just the
# watcher's token, injected into the watcher pod as WATCHER_TOKEN.
kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" delete secret \
    core-agent-users lookout-watch-token --ignore-not-found
kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" create secret generic core-agent-users \
    --from-file=users.json="${RIG_STATE_DIR}/users.json"
kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" create secret generic lookout-watch-token \
    --from-literal=token="${WATCHER_TOKEN}"

# Plaintext tokens now live in the cluster Secret; drop the local copy.
rm -f "${RIG_STATE_DIR}/users.json"

# ── Restart whatever is already consuming these Secrets ──────────────
# BOTH pods read their credential exactly once, at pod start: the daemon's
# initContainer stages users.json into an emptyDir, and the watcher
# resolves WATCHER_TOKEN from a secretKeyRef into its env. Neither picks
# up a rewritten Secret while running.
#
# That matters because re-running this script rotates both tokens, and
# `set-up-demo.sh` only restarts a Deployment whose POD SPEC changed. A
# content-image bump changes the daemon's spec (so it restarts and loads
# the new table) but leaves the watcher's spec untouched — so the watcher
# keeps POSTing its old token and every inject fails:
#
#   injector: POST inject: status 401: unauthorized: no valid credential
#
# (That exact string is the daemon's ErrUnauthenticated branch — an
# unknown BEARER TOKEN. A proxy-identity problem reads
# "asserted-caller header rejected" instead, so the two are easy to tell
# apart in the watcher log.)
#
# Restart both here, where the invalidation happens. No-ops on a first
# run, when neither Deployment exists yet.
for dep in core-agent lookout-watch; do
    if kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" get "deploy/${dep}" >/dev/null 2>&1; then
        echo "→ restarting deploy/${dep} to pick up the rotated credential"
        kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" rollout restart "deploy/${dep}"
    fi
done

echo "✓ Secrets created in ${DEMO_NS}; tokens stashed at ${RIG_STATE_DIR}/demo-tokens.env"
echo "  attach as ${ADMIN_IDENTITY} with \$PLATFORM_TOKEN (see attach.sh)"
