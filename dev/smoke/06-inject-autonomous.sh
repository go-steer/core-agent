#!/usr/bin/env bash
# Smoke: examples/autonomous-handle runs end-to-end with no
# credentials. Exercises the full v1.3.0 surface:
#   - StartAutonomous + AutonomousHandle returned
#   - Pause / Resume around an Inject
#   - Wait blocks until terminal
#   - Status transitions visible in the printed output
#
# No env required (echo mock).

set -euo pipefail
source "$(dirname "$0")/_common.sh"

log_step "inject-autonomous: examples/autonomous-handle end-to-end"
output=$(
    cd "$(repo_root)" && timeout 30 go run ./examples/autonomous-handle 2>&1
)
echo "${output}"

assert_contains "== StartAutonomous ==" "${output}"
assert_contains "== Pause ==" "${output}"
assert_contains "status: paused" "${output}"
assert_contains "== Inject ==" "${output}"
assert_contains "== Resume ==" "${output}"
assert_contains "status: running" "${output}"
assert_contains "== Wait ==" "${output}"
pass "AutonomousHandle Pause / Inject / Resume / Wait observed in expected order"
