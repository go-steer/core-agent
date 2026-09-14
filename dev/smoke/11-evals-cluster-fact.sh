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

# Behavioural eval: cluster-fact verification (#652).
#
# This is the only leg in dev/smoke that asks whether the agent found
# something, rather than whether a request shape was accepted. The
# cluster is a fixture — four namespaces rendered by a kubectl shim
# from one JSON file — with exactly one Deployment referencing a tag
# the registry does not have. The prompt names neither the namespace
# nor the workload, and the grade is read off the shim's own log plus
# the final answer, never off the model's account of what it did.
#
# Two tiers run, differing by exactly one flag:
#
#   tools      the real run
#   no-access  the same case with --no-builtin-tools
#
# The second is the grader's self-test rather than a measurement of the
# agent: every objective check must score zero with no tool access, or
# the check is measuring the model's vocabulary and will keep measuring
# vocabulary however good the agent gets. It runs here beside the real
# tier, every time, rather than being audited once when the case was
# written — a check only drifts into vocabulary-measuring later.
#
# Exit 2 from evalrun (indeterminate — some check observed nothing) is
# a failure here, not a pass. An unanswered question is not a green.
#
# Requires: ANTHROPIC_VERTEX_PROJECT_ID (+ gcloud ADC). Region from
# CLOUD_ML_REGION (default us-east5). Model via EVAL_MODEL.

set -uo pipefail
source "$(dirname "$0")/_common.sh"
require_env ANTHROPIC_VERTEX_PROJECT_ID
build_core_agent

MODEL="${EVAL_MODEL:-claude-sonnet-5}"
CASE="${EVAL_CASE:-dev/evals/cases/cluster-fact-image-pull.json}"
EVALRUN="${TMPDIR:-/tmp}/core-agent-evalrun"
REPORT="${TMPDIR:-/tmp}/core-agent-eval-report.json"

log_step "evals: building the runner"
go build -o "${EVALRUN}" ./dev/smoke/cmd/evalrun || fail "could not build evalrun"

log_step "evals: ${CASE} (${MODEL}, tools + no-access)"
set +e
CLOUD_ML_REGION="${CLOUD_ML_REGION:-us-east5}" \
"${EVALRUN}" \
    --case "${CASE}" \
    --fixtures dev/evals/fixtures \
    --binary "${CORE_AGENT}" \
    --agent-args "--provider=anthropic-vertex --model=${MODEL}" \
    --json "${REPORT}" \
    --timeout "${EVAL_TIMEOUT:-4m}"
rc=$?
set -e

case "${rc}" in
    0) ;;
    2) fail "the eval is INDETERMINATE — a check observed nothing, so this run measured nothing (report: ${REPORT})" ;;
    *) fail "the eval FAILED (exit ${rc}; report: ${REPORT})" ;;
esac

pass "the agent found the planted defect, and a model with no access to the cluster did not"
