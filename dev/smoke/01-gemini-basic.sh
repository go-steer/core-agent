#!/usr/bin/env bash
# Smoke: basic single-turn against the direct Gemini API.
# Catches: provider registration, API-key auth, request/response
# round-trip on the non-Vertex path.
#
# Required env: GEMINI_API_KEY or GOOGLE_API_KEY

set -euo pipefail
source "$(dirname "$0")/_common.sh"
require_one_of GEMINI_API_KEY GOOGLE_API_KEY
build_core_agent

log_step "gemini-basic: single turn against the direct Gemini API"
output=$(
    "${CORE_AGENT}" --provider=gemini --yolo \
        -p "Say hello in exactly one word." 2>&1
)
echo "${output}"

# The usage summary line ("core-agent: 1 turn(s) ...") is the cleanest
# signal that the request completed end-to-end; assert on that rather
# than on the model's word choice (varies per call).
assert_contains "core-agent:" "${output}"
assert_contains "turn(s)" "${output}"
pass "direct Gemini API returned a response"
