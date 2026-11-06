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

# Smoke: stateless memory CLI flags and CORE_AGENT_MEMORY_* env vars
# wire pkg/memory on inmemory:// with zero network calls or API keys (#948).

set -euo pipefail
source "$(dirname "$0")/_common.sh"
build_core_agent --force
pristine_config

log_step "14-memory-stateless-cli: CLI flags wire memory router and preload"
output=$(
    "${CORE_AGENT}" -c "${SMOKE_CONFIG}" \
        --provider=echo \
        --memory-ref="inmemory://smoke-14" \
        --memory-namespace="prod-smoke" \
        --memory-agent="smoke-bot:rw" \
        --memory-user="smoke-operator" \
        --memory-shared="fleet:rw" \
        --memory-shared="runbooks:ro" \
        --memory-preload="turn_start" \
        --memory-consolidate="session_end" \
        -p "verify stateless memory flags" 2>&1
)
echo "${output}"
assert_contains "memory: ref=inmemory://smoke-14 namespace=prod-smoke preload=turn_start consolidate=session_end" "${output}"
pass "CLI flags wired memory router on inmemory://"

log_step "14-memory-stateless-cli: CORE_AGENT_MEMORY_* env vars wire memory router"
env_output=$(
    CORE_AGENT_MEMORY_REF="inmemory://smoke-14-env" \
    CORE_AGENT_MEMORY_NAMESPACE="env-smoke" \
    CORE_AGENT_MEMORY_AGENT="env-bot:ro" \
    CORE_AGENT_MEMORY_SHARED="fleet:ro" \
    CORE_AGENT_MEMORY_PRELOAD="session_start" \
    "${CORE_AGENT}" -c "${SMOKE_CONFIG}" \
        --provider=echo \
        -p "verify env memory overrides" 2>&1
)
echo "${env_output}"
assert_contains "memory: ref=inmemory://smoke-14-env namespace=env-smoke preload=session_start consolidate=off" "${env_output}"
pass "CORE_AGENT_MEMORY_* env vars wired memory router on inmemory://"
