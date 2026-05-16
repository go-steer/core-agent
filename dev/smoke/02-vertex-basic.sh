#!/usr/bin/env bash
# Smoke: basic single-turn through Vertex AI.
#
# Catches:
#   - v1.0.0-style IncludeServerSideToolInvocations regression
#     (Vertex rejecting the flag with "is not supported in Gemini
#     Enterprise Agent Platform").
#   - Any other Vertex-rejected request shape introduced by accident.
#   - The "empty response" symptom from v1.0.1's heartbeat-tolerance
#     bug if it regressed.
#
# Required env: GOOGLE_CLOUD_PROJECT + ADC
# (gcloud auth application-default login)

set -euo pipefail
source "$(dirname "$0")/_common.sh"
require_env GOOGLE_CLOUD_PROJECT
build_core_agent

# Clear API-key env vars to keep the Vertex auth path clean. genai's
# precedence logic will fall back to ADC; if it doesn't, the 401 we
# get is a cleaner signal than a mixed-auth confusion.
unset GEMINI_API_KEY GOOGLE_API_KEY

log_step "vertex-basic: single turn against Vertex Gemini"
output=$(
    GOOGLE_GENAI_USE_VERTEXAI=true \
    GOOGLE_CLOUD_LOCATION="${GOOGLE_CLOUD_LOCATION:-global}" \
    "${CORE_AGENT}" --provider=vertex --yolo \
        -p "Say hello in exactly one word." 2>&1
)
echo "${output}"

assert_not_contains "includeServerSideToolInvocations" "${output}"
assert_not_contains "empty response" "${output}"
assert_contains "core-agent:" "${output}"
assert_contains "turn(s)" "${output}"
pass "Vertex returned a response with no rejection / no empty-response error"
