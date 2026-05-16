#!/usr/bin/env bash
# Smoke: Vertex + GoogleSearch + --session-db.
#
# Verifies the full v1.1.0 surface end-to-end:
#   - GoogleSearch built-in actually fires (returns grounded results)
#   - runner.WriteEvents renders "↪ google_search:" lines to stderr
#   - gemini.GroundingProjection writes "gemini/google_search"-
#     authored rows to the eventlog overlay
#   - v1.0.1's heartbeat-tolerance is still working (test would
#     flake 30-60% of the time without it)
#
# Required env: GOOGLE_CLOUD_PROJECT + ADC

set -euo pipefail
source "$(dirname "$0")/_common.sh"
require_env GOOGLE_CLOUD_PROJECT
build_core_agent
unset GEMINI_API_KEY GOOGLE_API_KEY

db="$(mktemp -t smoke-grounding-XXXXXX.db)"
trap 'rm -f "${db}"' EXIT

log_step "vertex-grounding: GoogleSearch + ↪ display + eventlog projection"
output=$(
    GOOGLE_GENAI_USE_VERTEXAI=true \
    GOOGLE_CLOUD_LOCATION="${GOOGLE_CLOUD_LOCATION:-global}" \
    timeout 90 "${CORE_AGENT}" --provider=vertex --yolo \
        --session-db --session-db-path="${db}" \
        -p "Use Google Search to give me one San Francisco news headline from today." 2>&1
)
echo "${output}"

assert_not_contains "empty response" "${output}"
assert_contains "↪ google_search:" "${output}"
pass "↪ google_search line appeared in stdout"

log_step "verifying eventlog has gemini/google_search rows"
rows=$(go run "$(repo_root)/dev/smoke/cmd/inspect-grounding" "${db}" 2>&1)
echo "${rows}"
assert_contains "gemini/google_search" "${rows}"
pass "gemini/google_search rows present in eventlog ${db}"
