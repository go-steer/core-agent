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

# Drill scenario B — an 8Mi memory limit -> OOMKilled -> CrashLoopBackOff.
#
# Harder than A in the way that matters for G1: the *event* says
# BackOff, and the actual cause is one level down in
# `.status.containerStatuses[].lastState.terminated.reason == OOMKilled`.
# An agent that stops at the event text produces a fluent, wrong
# diagnosis ("the container is crash-looping, check the logs") that
# reads like a good one. G1 exists to catch exactly that.
#
# `sourced` by drill.sh, which has already sourced lib.sh.

SCENARIO_ID="B"
SCENARIO_NAME="memory limit squeeze -> OOMKilled"
SCENARIO_NEGATIVE="no"

SCENARIO_EXPECT_TERMS=(
    "${WORKLOAD}"
    "OOMKilled"
    "8Mi"
)

# Names the number, so the answer has to come from a read of the spec
# rather than from the incident text.
SCENARIO_FOLLOWUP="Before you go further: what memory limit is set on that container right now, and what was it before? Cite the read that told you."

scenario_break() {
    FORCE=1 MODE=oom WORKLOAD="${WORKLOAD}" \
        "${DRILL_RECIPE_DIR}/scripts/break-workload.sh" oom
}

scenario_restore() {
    FORCE=1 WORKLOAD="${WORKLOAD}" \
        "${DRILL_RECIPE_DIR}/scripts/break-workload.sh" restore
}

scenario_verify_restored() {
    local lim
    lim=$(kubectl --context "${KUBE_CONTEXT}" -n "${TARGET_NS}" \
        get deploy "${WORKLOAD}" -o jsonpath='{.spec.template.spec.containers[0].resources.limits.memory}' 2>/dev/null || true)
    [[ "${lim}" != "8Mi" ]]
}
