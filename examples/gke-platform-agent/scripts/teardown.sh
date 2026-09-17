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

# Tear down the gke-platform-agent deployment. Deletes the namespace
# (Secrets + PVC + workloads), the cluster-scoped + kube-system watcher
# RBAC that a namespace delete would otherwise orphan, and the local token
# stash. Leaves the pushed content image in Artifact Registry unless you
# pass --images (registry pulls cost nothing to keep; delete for a clean
# slate).
#
# Does NOT touch the project-level WIF/IAM bindings — those are one-time
# and path-based (ns/sa name), so they stay valid across a delete+recreate.
#
# Does NOT touch TARGET_NS's WORKLOADS — run ./scripts/break-workload.sh
# restore for that. It does delete the one thing this recipe can own
# inside TARGET_NS: the gated-apply Role/RoleBinding, if the gated-apply
# component was composed into the overlay. That RBAC grants the daemon
# deployment-patch rights and lives outside DEMO_NS, so a namespace delete
# does not reach it — leaving it behind would mean a cluster the operator
# believes is clean still has a standing write grant.
#
# Does NOT touch another recipe's namespace or RBAC: every cluster-scoped
# name below is suffixed with THIS deployment's namespace, so deployments
# that share a cluster tear down independently.
set -euo pipefail

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
source "${SCRIPT_DIR}/prereqs.sh"

# The local credential stash goes first, before the coordinate checks and
# before anything that talks to the cluster. Every kubectl call below can
# fail under `set -e` — the gated-apply one especially, since TARGET_NS is
# by construction a namespace this recipe does not own, so an operator
# without RBAC there aborts the teardown. Live bearer tokens left on disk
# is the worst thing to leave behind, and it is the one step that cannot
# fail.
#
# ABOVE THE GUARDS, not merely above the kubectl calls. A guard is a new
# way to exit 1, and the operator most likely to trip require_coordinates
# or require_demo_ns_matches_base is the one with a stale coordinate
# exported from an earlier experiment — which is to say, exactly the
# person running teardown. Sending them off to fix their environment while
# their tokens stay on disk inverts the priority this comment states.
# RIG_STATE_DIR is the one path that makes this safe to hoist: prereqs.sh
# defaults it from TMPDIR alone, independently of every coordinate.
rm -f "${RIG_STATE_DIR}/demo-tokens.env" "${RIG_STATE_DIR}/users.json"
echo "→ removed local token stash (${RIG_STATE_DIR})"

require_coordinates || exit 1
# This one DELETES by name, so an overridden DEMO_NS tears down a
# namespace that was never created and leaves the real one standing.
require_demo_ns_matches_base || exit 1

# The Instrumentation CR the tracing overlays add is namespaced, so it
# goes with the namespace — nothing extra to clean up on the OTel path.
# Managed OpenTelemetry itself is a cluster-level setting that
# set-up-demo.sh never turns on, so teardown leaves it alone too.

echo "→ deleting namespace ${DEMO_NS} (daemon, watcher, Secrets, PVC)"
kubectl --context "${KUBE_CONTEXT}" delete namespace "${DEMO_NS}" --ignore-not-found --wait=false

# Cluster-scoped + kube-system RBAC survive a namespace delete, so remove
# them explicitly for a truly clean slate. The watcher ClusterRole grants
# cluster-wide read (enrichment); its binding grants it to the watcher SA;
# the capacity Role/RoleBinding live in kube-system (the
# cluster-autoscaler-status ConfigMap). Names are namespace-suffixed, so
# this only touches THIS demo's objects. Harmless if already gone.
echo "→ deleting cluster-scoped + kube-system watcher RBAC"
kubectl --context "${KUBE_CONTEXT}" delete clusterrole,clusterrolebinding \
    "lookout-watch-${DEMO_NS}" --ignore-not-found
kubectl --context "${KUBE_CONTEXT}" -n kube-system delete role,rolebinding \
    "lookout-watch-capacity-${DEMO_NS}" --ignore-not-found

# The gated-apply grant (deploy/components/gated-apply) lives in TARGET_NS,
# which nothing above deletes: DEMO_NS goes with the namespace delete, the
# watcher's objects are cluster-scoped or in kube-system, but this one sits
# in the namespace the agent was pointed AT. Left behind it is a standing
# deployment-patch grant on a cluster that looks torn down. Harmless if the
# component was never composed.
#
# NOTE the comma type-list. `delete role X rolebinding Y` parses X,
# "rolebinding" and Y as three ROLES, and --ignore-not-found then silences
# the two that do not exist — so it deletes the Role, leaves the
# RoleBinding, and still exits 0.
if [[ "${1:-}" == "--images" ]]; then
    for tag in "${CONTENT_TAG}" "${CONTENT_TAG}-copy"; do
        echo "→ deleting ${CONTENT_IMAGE}:${tag}"
        gcloud artifacts docker images delete "${CONTENT_IMAGE}:${tag}" \
            --project="${PROJECT_ID}" --delete-tags --quiet || true
    done
fi

# Last, for the same reason the token stash went first: this is the one
# call whose namespace the recipe does not own, so it is the one most
# likely to 403 and abort the script. Everything that can be cleaned up
# without permission in TARGET_NS has been by now.
#
# The name is READ from the component, not reconstructed from DEMO_NS. A
# reconstructed name is a guess about what set-up-demo.sh wrote, and on the
# `kubectl apply -k` path set-up-demo.sh wrote nothing at all — the guess
# would then delete a name that never existed and exit 0, which is the whole
# failure this step exists to prevent.
GATED_APPLY_NAME=$(sed -nE 's/^  name: (gated-apply-.*)$/\1/p' \
    "${DEMO_DEPLOY_DIR}/components/gated-apply/role.yaml" | head -1)
if [[ -z "${GATED_APPLY_NAME}" ]]; then
    echo "✗ could not read the gated-apply Role name from deploy/components/gated-apply/role.yaml" >&2
    echo "  Delete it by hand if the component was ever applied:" >&2
    echo "    kubectl -n ${TARGET_NS} delete role,rolebinding <name>" >&2
    exit 1
fi
echo "→ deleting gated-apply RBAC (${GATED_APPLY_NAME}) in ${TARGET_NS} (if present)"
kubectl --context "${KUBE_CONTEXT}" -n "${TARGET_NS}" delete role,rolebinding \
    "${GATED_APPLY_NAME}" --ignore-not-found

echo "✓ teardown complete."
echo "  Content images kept in ${AR_REPO} (pass --images to delete them)."
echo "  Broke a workload? restore it with: ./scripts/break-workload.sh restore"
