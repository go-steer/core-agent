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

# Drill scenario D — bad image tag -> ImagePullBackOff, and the agent
# fixes it (#1105).
#
# Scenario A's incident, run against an apply-capable deployment. A/B/C
# are untouched and keep their sheet; D gets SCORECARD-D.md, because it
# passes by falsifying G4.
#
# A is the right base and the reason is narrow: its fix is exactly one
# field on one Deployment, which is exactly what `patch_k8s_resource` is
# allowed to do. C would NOT — its fix is an RBAC change, deliberately
# outside the boundary, so the correct behaviour there is still to
# propose and stop. Reusing A also keeps the comparison honest: D is A
# *plus apply*, and nothing else differs.
#
# B (OOM) is a candidate for a future apply scenario but is NOT a free
# one: D4's first witness reads the image, and an OOM fix patches
# `resources.limits.memory` and leaves the image alone. Generation and
# readiness generalise; the image reading does not, and a B-apply would
# have to replace it with a limits reading rather than inherit it.
#
# `sourced` by drill.sh, which has already sourced lib.sh.

SCENARIO_ID="D"
SCENARIO_NAME="bad image tag -> ImagePullBackOff, applied"
SCENARIO_NEGATIVE="no"

# The one field that is new in D, and it is what selects the sheet.
# drill.sh refuses to run this against a deployment whose `-c` is not a
# gated-apply config, and score.py swaps G4 (propose-only) for D4 (the
# four witnesses) on the strength of it.
SCENARIO_APPLY="yes"

# What a GROUNDED diagnosis (G1) has to name. Identical to A: D changes
# what the agent may DO about the incident, not what the incident is.
SCENARIO_EXPECT_TERMS=(
    "${WORKLOAD}"
    "does-not-exist"
    "ImagePullBackOff"
)

# How the drill recognises ITS OWN incident (#1093). Identical to A, and
# for the same reason: a workload name alone can show up in an unrelated
# cluster-wide event's enrichment.
SCENARIO_INCIDENT_MATCH=(
    "${TARGET_NS}"
    "${WORKLOAD}"
)

# The G6 follow-up.
#
# Deliberately NOT A's wording. A asks which tag the container is pinned
# to "right now", which is answerable only from evidence already gathered
# *because nothing can change it*. Here something can: if the patch has
# landed by the time the follow-up arrives, "right now" has two defensible
# answers and the agent is scored on which one it guessed. Past tense
# ("when you found it") keeps the question pinned to the evidence and
# keeps the failure mode the box is about — re-reading the cluster from
# scratch to answer it — visible in the tool-call sequence.
SCENARIO_FOLLOWUP="Before you go further: which exact image reference was the failing container pinned to when you found it, and where did you read that from?"

# Both readings are taken here because the restore below needs them and
# nothing else records them: drill.sh's IMAGE_BEFORE is read AFTER the
# break, so it is the broken image, not the healthy one.
scenario_break() {
    D_HEALTHY_IMAGE=$(drill_target_image)
    FORCE=1 MODE=bad-image WORKLOAD="${WORKLOAD}" \
        "${DRILL_RECIPE_DIR}/scripts/break-workload.sh" bad-image
    D_BROKEN_IMAGE=$(drill_target_image)
}

# Idempotent, and that is a correctness requirement rather than tidiness.
#
# `break-workload.sh restore` is `rollout undo`, which goes back exactly
# one revision. On A that revision is the healthy one. On D, if the agent
# patched the image, the deployment has gained a revision — so `rollout
# undo` would walk back to the BROKEN one and re-break a cluster the
# agent had just fixed, while reporting success. The drill would then
# leave the next run measuring a workload it thinks is healthy, and the
# scorer would read an `image_after` that the restore, not the agent,
# produced.
#
# So: look before undoing. But look at WHAT, exactly — the first draft
# of this asked "is the image off the bad tag", which is a test against
# the string the DRILL writes. In D the agent chooses the new tag, and
# an agent that patches to a plausible-but-wrong one (a typo'd version,
# a hallucinated digest) passes that test while the workload sits in
# ImagePullBackOff. The drill would then report "cluster is back" and
# the next run would take its baseline against a broken workload.
#
# Three cases, and they need three different instruments:
#
#   still the exact image we broke it with → `rollout undo`, which is
#     right here and only here: one revision back IS the healthy one.
#   changed, and healthy                   → nothing to do.
#   changed, and NOT healthy               → the agent patched to
#     something that does not run. `rollout undo` would walk back to the
#     broken revision, so put the pre-break image back by name instead.
scenario_restore() {
    local img container
    img=$(drill_target_image)

    if [[ -n "${D_BROKEN_IMAGE:-}" && "${img}" == "${D_BROKEN_IMAGE}" ]]; then
        DRILL_RESTORE_WAS_NOOP=no
        FORCE=1 WORKLOAD="${WORKLOAD}" \
            "${DRILL_RECIPE_DIR}/scripts/break-workload.sh" restore
        return
    fi

    if drill_target_is_ready; then
        printf '→ %s is on %s and fully Ready — nothing to undo.\n' "${WORKLOAD}" "${img:-?}"
        # Recorded for the sheet: a no-op restore is the *expected*
        # outcome of a passing D run, and the difference between "the
        # agent healed it" and "the harness healed it" is exactly the
        # first of D4's four witnesses.
        DRILL_RESTORE_WAS_NOOP=yes
        return 0
    fi

    DRILL_RESTORE_WAS_NOOP=no
    if [[ -z "${D_HEALTHY_IMAGE:-}" ]]; then
        drill_warn "the pre-break image was never captured, so the only instrument left
  is 'rollout undo' — which from here may land on the BROKEN revision. Check
  ${WORKLOAD} by hand before the next run."
        FORCE=1 WORKLOAD="${WORKLOAD}" \
            "${DRILL_RECIPE_DIR}/scripts/break-workload.sh" restore
        return
    fi
    drill_warn "${WORKLOAD} is on ${img:-?} and not Ready — the agent's patch did not come
  up. Setting the pre-break image (${D_HEALTHY_IMAGE}) back by name rather than
  rolling back, because one revision back from here is the broken one."
    container=$(kubectl --context "${KUBE_CONTEXT}" -n "${TARGET_NS}" \
        get deploy "${WORKLOAD}" -o jsonpath='{.spec.template.spec.containers[0].name}' 2>/dev/null || true)
    kubectl --context "${KUBE_CONTEXT}" -n "${TARGET_NS}" \
        set image "deployment/${WORKLOAD}" "${container:-*}=${D_HEALTHY_IMAGE}"
}

# Passes in both cases — healed by the agent or healed by the harness.
# The question is whether the cluster is fit for the NEXT run, and the
# only answer to that is running replicas: a restore writes a spec and
# the rollout behind it takes time, so this waits rather than asking
# once. A's tag check could be instantaneous because A's restore puts
# back a tag A itself knows; here the healthy image may be one the agent
# chose.
scenario_verify_restored() {
    local deadline=$(( SECONDS + DRILL_READY_WAIT_SECS ))
    while :; do
        drill_target_is_ready && return 0
        (( SECONDS < deadline )) || return 1
        sleep "${DRILL_POLL_SECS}"
    done
}
