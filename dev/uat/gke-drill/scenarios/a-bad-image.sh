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

# Drill scenario A — bad image tag -> ImagePullBackOff.
#
# The easy one, and it is first on purpose: a run where A fails tells
# you the rig is wrong, not the agent. Evidence is abundant and lives
# entirely in the pod status and the Events stream, which is the one
# place every read path can reach.
#
# `sourced` by drill.sh, which has already sourced lib.sh.

SCENARIO_ID="A"
SCENARIO_NAME="bad image tag -> ImagePullBackOff"
SCENARIO_NEGATIVE="no"

# What a GROUNDED diagnosis (G1) has to name. The scorer greps the
# final answer for these and reports which appeared; it does not decide
# the box — a transcript can contain the right noun for the wrong
# reason, and only a human reading it can tell.
SCENARIO_EXPECT_TERMS=(
    "${WORKLOAD}"
    "does-not-exist"
    "ImagePullBackOff"
)

# The G6 follow-up. Deliberately answerable ONLY from evidence already
# gathered — "which tag", not "what is wrong" — so an answer that
# re-reads the cluster from scratch is visible as such in the tool-call
# sequence.
SCENARIO_FOLLOWUP="Before you go further: which exact image reference is the failing container pinned to right now, and where did you read it from?"

scenario_break() {
    FORCE=1 MODE=bad-image WORKLOAD="${WORKLOAD}" \
        "${DRILL_RECIPE_DIR}/scripts/break-workload.sh" bad-image
}

scenario_restore() {
    FORCE=1 WORKLOAD="${WORKLOAD}" \
        "${DRILL_RECIPE_DIR}/scripts/break-workload.sh" restore
}

# Whether the cluster is actually back. `break-workload.sh restore`
# exits 0 even when `rollout undo` failed (documented in the recipe's
# DEMO.md), so the drill checks the outcome rather than the exit code —
# a second drill run against a still-broken workload scores nothing.
scenario_verify_restored() {
    local img
    img=$(kubectl --context "${KUBE_CONTEXT}" -n "${TARGET_NS}" \
        get deploy "${WORKLOAD}" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)
    [[ "${img}" != *does-not-exist* ]]
}
