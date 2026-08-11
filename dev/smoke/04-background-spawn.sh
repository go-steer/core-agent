#!/usr/bin/env bash
# Smoke: dynamic background subagents end-to-end.
#
# Verifies the background-subagent surface (spawn_agent + stop_agent):
#   - spawn_agent is registered + callable by the model
#   - two subagents spawn under their own branches and run to completion
#   - spawn_agent { wait: true } returns each subagent's final output
#     INLINE within the same turn — the synchronous delegation path
#
# This is a one-shot `-p` run, which has exactly one turn: the push
# (`[Background reports]`) path needs a *subsequent* turn to drain, so a
# poll-free one-shot uses wait:true to get results back. The async push
# path is covered by pkg/agent/background unit tests + the
# examples/background-monitor demo (multi-turn drain).
#
# Required env: GOOGLE_CLOUD_PROJECT + ADC

set -euo pipefail
source "$(dirname "$0")/_common.sh"
require_env GOOGLE_CLOUD_PROJECT
build_core_agent
unset GEMINI_API_KEY GOOGLE_API_KEY

log_step "background-spawn: parent spawns two subagents with wait:true, both complete inline"
output=$(
    GOOGLE_GENAI_USE_VERTEXAI=true \
    GOOGLE_CLOUD_LOCATION="${GOOGLE_CLOUD_LOCATION:-global}" \
    timeout 180 "${CORE_AGENT}" --provider=vertex --yolo -p "
You're an orchestrator. Use spawn_agent with wait: true to launch two
background subagents, one at a time (each call blocks until that
subagent finishes and returns its output inline):
  1. name 'count-up', empty tools list, goal: count from 1 to 3, then
     reply with exactly this text and nothing else: count-up done: 3
  2. name 'count-down', empty tools list, goal: count from 5 to 3, then
     reply with exactly this text and nothing else: count-down done: 3
Because you passed wait: true you receive each subagent's final output
inline as the tool result — do not try to poll for them. After both
return, tell me exactly what each subagent replied.
" 2>&1
)
echo "${output}"

# Loose assertions — model wording varies; we want proof that:
# (1) spawn_agent was actually invoked, (2) both subagents were named
# as requested, (3) each subagent's final output came back inline via
# wait:true and reached the parent's answer.
assert_contains "spawn_agent" "${output}"
assert_contains "count-up" "${output}"
assert_contains "count-down" "${output}"
# The subagents' deterministic final replies must surface inline.
assert_contains "count-up done: 3" "${output}"
assert_contains "count-down done: 3" "${output}"
pass "both subagents spawned and returned their final output inline via wait:true"
