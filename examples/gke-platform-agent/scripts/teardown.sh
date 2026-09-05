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
# Does NOT touch TARGET_NS — run ./scripts/break-workload.sh restore for
# that.
#
# Does NOT touch another recipe's namespace or RBAC: every cluster-scoped
# name below is suffixed with THIS deployment's namespace, so deployments
# that share a cluster tear down independently.
set -euo pipefail

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
source "${SCRIPT_DIR}/prereqs.sh"
require_coordinates || exit 1


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

rm -f "${RIG_STATE_DIR}/demo-tokens.env" "${RIG_STATE_DIR}/users.json"
echo "→ removed local token stash (${RIG_STATE_DIR})"

if [[ "${1:-}" == "--images" ]]; then
    for tag in "${CONTENT_TAG}" "${CONTENT_TAG}-copy"; do
        echo "→ deleting ${CONTENT_IMAGE}:${tag}"
        gcloud artifacts docker images delete "${CONTENT_IMAGE}:${tag}" \
            --project="${PROJECT_ID}" --delete-tags --quiet || true
    done
fi

echo "✓ teardown complete."
echo "  Content images kept in ${AR_REPO} (pass --images to delete them)."
echo "  Broke a workload? restore it with: ./scripts/break-workload.sh restore"
