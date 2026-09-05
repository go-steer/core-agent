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

# Break a workload in TARGET_NS to trigger a real cluster event, which the
# lookout-watch turns into an incident inject on the hub. Attach with
# ./scripts/attach.sh to watch the platform agent (and its `cluster`
# subagent) diagnose it.
#
# Modes (MODE=... or first arg):
#   bad-secret  (default) mount a non-existent Secret -> FailedMount
#   bad-image             point at a non-existent image -> ImagePullBackOff
#   oom                   squeeze the memory limit    -> OOMKilled/CrashLoopBackOff
#   unschedulable         demand more CPU than exists -> Pending/FailedScheduling
#   bad-probe             readiness probe on a dead port -> stalled rollout
#   restore               undo any of the above
#
# The first two are the original UAT and are unchanged. The three added on
# 2026-08-17 exist because the watcher runs --sources=auto against the
# enrichment-complete ClusterRole, so eleven sources resolve — and both
# original modes land on k8s-events alone. The new ones reach further:
#
#   oom            object-state (restart count climbing) + k8s-events (BackOff)
#   unschedulable  capacity (pending-pod aging, NotTriggerScaleUp) + k8s-events
#   bad-probe      rollout (new RS stuck while old RS healthy — fires after
#                  --rollout-observe, default 3m) + object-state
#                  (progress_deadline countdown) + k8s-events (Unhealthy)
#
# Note what is NOT reached, so nobody adds a mode expecting it: `saturation`
# needs metrics-server and trends usage against requests, so an OOMKill does
# not touch it; `degradation` explicitly never fires when a ready ratio
# reaches 0, and a pod that goes NotReady and stays there is one transition,
# which can never hit its flip threshold. Both of those belong to object-state
# and k8s-events by design.
#
# Each new mode also lands on a different one of the `cluster` subagent's
# six skills.
#
# ONE AT A TIME. `restore` is `rollout undo`, which goes back exactly one
# revision, so breaking twice without restoring in between rolls you back
# to the *previous* breakage rather than to health.
#
# Target workload: WORKLOAD env (default emailservice), namespace TARGET_NS.
set -euo pipefail

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
source "${SCRIPT_DIR}/prereqs.sh"
require_coordinates || exit 1


MODE="${1:-${MODE:-bad-secret}}"
WORKLOAD="${WORKLOAD:-emailservice}"
K="kubectl --context ${KUBE_CONTEXT} -n ${TARGET_NS}"

$K get deploy "${WORKLOAD}" >/dev/null 2>&1 || {
    echo "✗ deployment ${WORKLOAD} not found in ${TARGET_NS}."
    echo "  Set WORKLOAD=<name> (and TARGET_NS in prereqs.sh) to an existing workload."
    echo "  Deployments in ${TARGET_NS}:"; $K get deploy -o name | sed 's/^/    /'
    exit 1
}

# Race guard (read-only check). 'restore' triggers no incident, so skip it
# there. warn_foreign_watchers only reads + prints; it never mutates. This
# fires most often when a second recipe's watcher is still deployed in
# another namespace — both watch the whole cluster, so both see the event
# you are about to create, and whichever injects first owns the incident.
if [[ "${MODE}" != "restore" ]] && ! warn_foreign_watchers; then
    if [[ "${FORCE:-}" == "1" ]]; then
        echo "  FORCE=1 set — proceeding despite the foreign watcher." >&2
    else
        read -r -p "A foreign watcher will race for this incident. Break anyway? [y/N] " ans
        [[ "${ans}" =~ ^[Yy]$ ]] || { echo "aborted; quiesce the other watcher (or FORCE=1) then retry."; exit 1; }
    fi
fi

case "${MODE}" in
  bad-secret)
    echo "→ breaking ${WORKLOAD} in ${TARGET_NS}: mounting a non-existent Secret (FailedMount)"
    $K patch deployment "${WORKLOAD}" --patch '
spec:
  template:
    spec:
      volumes:
        - name: demo-broken-creds
          secret: { secretName: smtp-credentials-typo }
      containers:
        - name: server
          volumeMounts:
            - { name: demo-broken-creds, mountPath: /etc/demo-creds, readOnly: true }'
    ;;
  bad-image)
    # lookout v0.18.0 added --imagepull-transient-min-count (default 3), which
    # holds back *retryable* pull failures until they repeat. It does not apply
    # here: a missing tag is a terminal `manifest unknown`, so this still fires
    # on the first event. Don't chase it as a regression.
    echo "→ breaking ${WORKLOAD} in ${TARGET_NS}: pinning a non-existent image (ImagePullBackOff)"
    CONTAINER=$($K get deploy "${WORKLOAD}" -o jsonpath='{.spec.template.spec.containers[0].name}')
    $K set image "deployment/${WORKLOAD}" \
        "${CONTAINER}=gcr.io/google-samples/does-not-exist:v0-demo-break"
    ;;
  oom)
    # `add` on an existing member replaces it, so this works whether or not the
    # container already declares resources — and replacing the whole object
    # avoids the limits<requests rejection you get from patching one field.
    echo "→ breaking ${WORKLOAD} in ${TARGET_NS}: 8Mi memory limit (OOMKilled → CrashLoopBackOff)"
    $K patch deployment "${WORKLOAD}" --type=json -p '[
      {"op":"add","path":"/spec/template/spec/containers/0/resources","value":{
        "requests":{"cpu":"10m","memory":"8Mi"},
        "limits":{"cpu":"100m","memory":"8Mi"}}}]'
    ;;
  unschedulable)
    # 200 is chosen to exceed the largest GKE machine type (c3-standard-176),
    # so the cluster autoscaler answers NotTriggerScaleUp — "no node group can
    # satisfy this" — instead of actually provisioning a node. Do NOT lower it
    # to something a node pool could fit: CA would scale up for real, the pod
    # would eventually schedule, and you would be billed for the lesson.
    echo "→ breaking ${WORKLOAD} in ${TARGET_NS}: requesting 200 CPUs (Pending → FailedScheduling)"
    $K patch deployment "${WORKLOAD}" --type=json -p '[
      {"op":"add","path":"/spec/template/spec/containers/0/resources","value":{
        "requests":{"cpu":"200","memory":"64Mi"}}}]'
    ;;
  bad-probe)
    # Readiness against a port nothing listens on. `add` replaces the whole
    # readinessProbe, which matters: the boutique services probe over grpc, and
    # merging a tcpSocket handler alongside it would be a rejected two-handler
    # probe. The livenessProbe is left alone on purpose — we want a pod that
    # stays up and never goes ready, not one that restarts (that is `oom`).
    #
    # progressDeadlineSeconds drops to 60 so objectstate.progress_deadline lands
    # in about a minute instead of ten. It lives on the DEPLOYMENT spec, not the
    # pod template, so `rollout undo` will NOT revert it — `restore` resets it
    # explicitly.
    echo "→ breaking ${WORKLOAD} in ${TARGET_NS}: readiness probe on a dead port (stalled rollout)"
    $K patch deployment "${WORKLOAD}" --type=json -p '[
      {"op":"add","path":"/spec/template/spec/containers/0/readinessProbe","value":{
        "tcpSocket":{"port":39099},
        "initialDelaySeconds":5,"periodSeconds":5,"failureThreshold":3}},
      {"op":"add","path":"/spec/progressDeadlineSeconds","value":60}]'
    ;;
  restore)
    echo "→ restoring ${WORKLOAD} in ${TARGET_NS} (rollback to previous revision)"
    # rollout undo reverts the whole pod template — image, probe, resources,
    # volumeMounts — in one step. What follows cleans up the two pieces undo
    # cannot: an orphaned `volumes` entry, and progressDeadlineSeconds, which
    # lives on the Deployment spec rather than the pod template.
    $K rollout undo "deployment/${WORKLOAD}" || true
    $K patch deployment "${WORKLOAD}" --type=json -p \
      '[{"op":"remove","path":"/spec/template/spec/volumes"}]' 2>/dev/null || true
    # Reset the deadline only if it still holds the value bad-probe set. A
    # different number is the operator's own and is left alone.
    if [[ "$($K get deploy "${WORKLOAD}" -o jsonpath='{.spec.progressDeadlineSeconds}')" == "60" ]]; then
        $K patch deployment "${WORKLOAD}" --type=json -p \
          '[{"op":"add","path":"/spec/progressDeadlineSeconds","value":600}]' || true
    fi
    $K rollout status "deployment/${WORKLOAD}" --timeout=120s || true
    echo "✓ restore attempted; verify with: ${K} get pods"
    echo "  If it is still broken, you likely broke twice without restoring in"
    echo "  between — undo went back one revision, to the earlier breakage. Run"
    echo "  restore again, or: ${K} rollout history deployment/${WORKLOAD}"
    exit 0
    ;;
  *)
    echo "✗ unknown MODE '${MODE}'"
    echo "  want: bad-secret | bad-image | oom | unschedulable | bad-probe | restore"
    exit 1 ;;
esac

echo
echo "✓ ${WORKLOAD} broken. Watch the event land + the incident fire:"
echo "    kubectl --context ${KUBE_CONTEXT} -n ${TARGET_NS} get events --watch"
echo "    kubectl --context ${KUBE_CONTEXT} -n ${DEMO_NS} logs deploy/lookout-watch -f"
echo "  Then attach to the incident session:  ./scripts/attach.sh"
echo "  Undo with:  ./scripts/break-workload.sh restore   (WORKLOAD=${WORKLOAD})"
echo "  Restore before breaking again — undo rewinds exactly one revision."
