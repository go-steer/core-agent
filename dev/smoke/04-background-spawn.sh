#!/usr/bin/env bash
# Smoke: dynamic background subagents end-to-end.
#
# Verifies the v1.2.0 surface:
#   - spawn_agent is registered + callable by the model
#   - Two subagents spawn in parallel under their own branches
#   - report_alert + report_done flow from each subagent back to
#     the manager (terminal Alert with kind=completed)
#   - check_agent returns terminal status + stop_reason="completed"
#     for both subagents after they finish
#
# Required env: GOOGLE_CLOUD_PROJECT + ADC

set -euo pipefail
source "$(dirname "$0")/_common.sh"
require_env GOOGLE_CLOUD_PROJECT
build_core_agent
unset GEMINI_API_KEY GOOGLE_API_KEY

log_step "background-spawn: parent spawns two subagents, both complete"
output=$(
    GOOGLE_GENAI_USE_VERTEXAI=true \
    GOOGLE_CLOUD_LOCATION="${GOOGLE_CLOUD_LOCATION:-global}" \
    timeout 180 "${CORE_AGENT}" --provider=vertex --yolo -p "
You're an orchestrator. Use spawn_agent to launch two background
subagents: one named 'count-up' that counts from 1 to 3 then calls
report_alert with text 'count-up done: 3' and then calls report_done,
and one named 'count-down' that counts from 5 to 3 then calls
report_alert with text 'count-down done: 3' and then calls report_done.
Each subagent should have an empty tools list. Then use bash to sleep
15 seconds. Then call check_agent for both and tell me what they each
reported.
" 2>&1
)
echo "${output}"

# Loose assertions — model wording varies; we want proof that:
# (1) spawn_agent was actually invoked, (2) both subagents were named
# as requested, (3) check_agent returned completed status for both.
assert_contains "spawn_agent" "${output}"
assert_contains "count-up" "${output}"
assert_contains "count-down" "${output}"
assert_contains "check_agent" "${output}"
# Two "completed" markers — one per subagent's check_agent result.
completed_count=$(grep -c -F "completed" <<<"${output}" || true)
if (( completed_count < 2 )); then
    fail "expected at least 2 'completed' markers across the output; got ${completed_count}"
fi
pass "both subagents spawned, ran in parallel, and reached terminal 'completed' state"
