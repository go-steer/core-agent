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

# Anthropic-via-Vertex smoke: one plain turn + one TOOL-LOOP turn.
#
# The tool loop is the load-bearing half: on thinking-default Claude
# models it exercises the #357 thinking-block round-trip (the second
# request of the loop must replay the assistant's thinking blocks) and
# the #367 tool_use ID pairing. A regression in either fails this
# script with a live API 400, which no mock can reproduce.
#
# Requires: ANTHROPIC_VERTEX_PROJECT_ID (+ gcloud ADC). Region from
# CLOUD_ML_REGION (default us-east5; "global" also works). Model via
# ANTHROPIC_SMOKE_MODEL (default claude-opus-4-7 — the adapter's
# DefaultModel and the most widely enabled in Model Garden). Point it
# at a thinking-default model (claude-sonnet-5 / claude-opus-5) when
# the project has access to exercise the #357 thinking path live.

set -euo pipefail
source "$(dirname "$0")/_common.sh"
require_env ANTHROPIC_VERTEX_PROJECT_ID
build_core_agent

MODEL="${ANTHROPIC_SMOKE_MODEL:-claude-opus-4-7}"

log_step "vertex-anthropic: single turn (${MODEL})"
set +e
output=$(
    CLOUD_ML_REGION="${CLOUD_ML_REGION:-us-east5}" \
    "${CORE_AGENT}" --provider=anthropic-vertex --model="${MODEL}" --yolo \
        -p "Reply with exactly the word: pong" 2>&1
)
rc=$?
set -e
echo "${output}"
[[ ${rc} -eq 0 ]] || fail "core-agent exited ${rc}"
assert_contains "pong" "${output}"

log_step "vertex-anthropic: tool loop (${MODEL}, bash echo round-trip)"
marker="toolloop-$(date +%s)"
set +e
output=$(
    CLOUD_ML_REGION="${CLOUD_ML_REGION:-us-east5}" \
    "${CORE_AGENT}" --provider=anthropic-vertex --model="${MODEL}" --yolo \
        -p "Run the bash command: echo ${marker} — then tell me exactly what it printed." 2>&1
)
rc=$?
set -e
echo "${output}"
[[ ${rc} -eq 0 ]] || fail "core-agent exited ${rc}"
# The marker must appear in the FINAL assistant text (i.e. the model
# completed the tool loop and reported the output), not just in the
# tool-call echo. Two occurrences (tool trace + answer) is the usual
# shape; require at least the answer path by asserting on the text
# after the last tool sigil is impractical in bash, so assert the
# marker appears AND no API-error string does.
assert_contains "${marker}" "${output}"
assert_not_contains "Expected \`thinking\`" "${output}"
assert_not_contains "tool_use ids must be unique" "${output}"
assert_not_contains "API error" "${output}"

pass "vertex-anthropic single turn + tool loop"
