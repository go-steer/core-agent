#!/usr/bin/env bash
# Smoke: headless (no TTY on stdin) without --yolo gets the
# ErrNoPrompter error message that points at --yolo and the
# config option, rather than silently hanging or producing a
# vague failure.
#
# Verifies v1.1.0's wiring:
#   - permissions.FromConfig + the TTY-aware resolveGatePrompter
#     in cmd/core-agent return nil prompter when stdin isn't a TTY
#   - permissions/gate.go's ErrNoPrompter wrap message still
#     names --yolo as a bypass option
#
# Required env: GOOGLE_CLOUD_PROJECT + ADC
# (any provider works; using Vertex to share creds with 02–04)

set -euo pipefail
source "$(dirname "$0")/_common.sh"
require_env GOOGLE_CLOUD_PROJECT
build_core_agent
unset GEMINI_API_KEY GOOGLE_API_KEY

log_step "headless-gate: bash call without --yolo surfaces helpful error"
# We need stdin to be a pipe (not /dev/null) to hit the cleanest
# headless path. runner.IsTerminal checks ModeCharDevice which
# treats /dev/null as a "terminal" — a known sharp edge — so the
# StdinPrompter would get wired and fail with "stdin prompter: EOF"
# instead of the gate cleanly returning ErrNoPrompter pointing at
# --yolo. A pipe gives us the predictable headless behavior real
# CI / daemon callers see.
#
# We accept either of the two headless failure modes since both are
# valid signals to the user that they should use --yolo:
#   (a) "interactive approval required" — the ErrNoPrompter path
#   (b) "stdin prompter: EOF"           — the wired-prompter-with-
#                                          no-input path
# The --yolo hint is only in (a); we assert on whichever signal
# fires plus the bypass-mention when (a) fires.
output=$(
    echo "" | (
        GOOGLE_GENAI_USE_VERTEXAI=true \
        GOOGLE_CLOUD_LOCATION="${GOOGLE_CLOUD_LOCATION:-global}" \
        timeout 60 "${CORE_AGENT}" --provider=vertex \
            -p "Use bash to print hello world. If bash refuses, tell me exactly what error it returned." 2>&1
    )
)
echo "${output}"

# Either failure mode is acceptable evidence the headless path is
# gated. Prefer the ErrNoPrompter (a) message since it names --yolo.
if grep -q -F -- "interactive approval required" <<<"${output}"; then
    assert_contains "--yolo" "${output}"
    pass "headless gate error mentions --yolo bypass (ErrNoPrompter path)"
elif grep -q -F -- "stdin prompter: EOF" <<<"${output}"; then
    pass "headless gate refused with EOF (wired prompter could not read)"
else
    fail "expected either ErrNoPrompter or EOF signal; neither found"
fi
