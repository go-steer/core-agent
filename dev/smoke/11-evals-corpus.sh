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

# Behavioural evals: the whole corpus (#652 harness, #966 corpus).
#
# This is the only leg in dev/smoke that asks whether the agent found
# something, rather than whether a request shape was accepted. Each case
# is a JSON file under dev/evals/cases/ against a fixture world rendered
# by a shared kubectl shim, and the grade is read off the shim's own log
# plus the final answer — never off the model's account of what it did.
#
# Every case runs at two tiers, differing by exactly one flag:
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
# The whole corpus runs even when an early case fails, and the leg fails
# at the end with a per-case tally. Stopping at the first failure would
# make the second case's result depend on the first one passing, and the
# runs are expensive enough that nobody would pay twice to find out.
#
# Requires: ANTHROPIC_VERTEX_PROJECT_ID (+ gcloud ADC). Region from
# CLOUD_ML_REGION (default us-east5). Model via EVAL_MODEL. EVAL_CASE
# narrows the run to one case path, for iterating on a new one.

set -uo pipefail
source "$(dirname "$0")/_common.sh"
require_env ANTHROPIC_VERTEX_PROJECT_ID
build_core_agent

MODEL="${EVAL_MODEL:-claude-sonnet-5}"
EVALRUN="${TMPDIR:-/tmp}/core-agent-evalrun"
REPORT_DIR="${TMPDIR:-/tmp}/core-agent-eval-reports"

if [[ -n "${EVAL_CASE:-}" ]]; then
    CASES=("${EVAL_CASE}")
else
    CASES=()
    while IFS= read -r c; do
        CASES+=("${c}")
    done < <(find dev/evals/cases -name '*.json' | sort)
fi

[[ ${#CASES[@]} -gt 0 ]] || fail "no eval cases found under dev/evals/cases"

mkdir -p "${REPORT_DIR}"

log_step "evals: building the runner"
go build -o "${EVALRUN}" ./dev/smoke/cmd/evalrun || fail "could not build evalrun"

failed=()
for case_path in "${CASES[@]}"; do
    case_id="$(basename "${case_path}" .json)"
    report="${REPORT_DIR}/${case_id}.json"

    log_step "evals: ${case_id} (${MODEL}, tools + no-access)"
    set +e
    CLOUD_ML_REGION="${CLOUD_ML_REGION:-us-east5}" \
    "${EVALRUN}" \
        --case "${case_path}" \
        --fixtures dev/evals/fixtures \
        --binary "${CORE_AGENT}" \
        --agent-args "--provider=anthropic-vertex --model=${MODEL}" \
        --json "${report}" \
        --timeout "${EVAL_TIMEOUT:-4m}"
    rc=$?
    set -e

    case "${rc}" in
        0) ;;
        # Indeterminate is named separately wherever it is reported. A
        # failing check says the agent did the wrong thing; an
        # indeterminate one says this leg did not find out what the agent
        # did, and the two send whoever reads the log to different places.
        2) failed+=("${case_id}: INDETERMINATE — a check observed nothing (report: ${report})") ;;
        *) failed+=("${case_id}: FAILED (exit ${rc}; report: ${report})") ;;
    esac
done

if [[ ${#failed[@]} -gt 0 ]]; then
    for line in "${failed[@]}"; do
        echo "  ✗ ${line}"
    done
    fail "${#failed[@]} of ${#CASES[@]} eval case(s) did not pass"
fi

pass "${#CASES[@]} eval case(s) passed, and a model with no access to the cluster passed none of them"
