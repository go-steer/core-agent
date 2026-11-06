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

# Manual UAT: the session transcript in Vertex AI Agent Engine Sessions
# outlives the local SQLite file. See README.md. All state under /tmp.
#
# Required:
#   CORE_AGENT_TEST_REASONING_ENGINE  projects/P/locations/L/reasoningEngines/N
#   ADC with roles/aiplatform.user on the project (gcloud auth application-default login)
#   A model the daemon can reach (GEMINI_API_KEY, or GOOGLE_CLOUD_PROJECT for Vertex)
set -euo pipefail

ENGINE="${CORE_AGENT_TEST_REASONING_ENGINE:?set CORE_AGENT_TEST_REASONING_ENGINE}"
UAT_ROOT="${UAT_ROOT:-/tmp/core-agent-uat/agentengine}"
REPO_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
BIN="${UAT_ROOT}/bin/core-agent"
CFG="${UAT_ROOT}/.agents/config.json"
DB="${UAT_ROOT}/sessions.db"
LOG="${UAT_ROOT}/core-agent.log"
# The CLI runs its own session as app "core-agent", user "local" unless
# multi-session auth names a caller.
USER_ID="${UAT_USER_ID:-local}"
APP_NAME="${UAT_APP_NAME:-core-agent}"

log() { echo "uat: $*" >&2; }
die() { echo "uat: $*" >&2; exit 1; }

rm -rf "${UAT_ROOT}"
mkdir -p "${UAT_ROOT}/bin" "${UAT_ROOT}/.agents"

log "step 0: live conformance suite against ${ENGINE}"
(cd "${REPO_ROOT}" && CORE_AGENT_TEST_REASONING_ENGINE="${ENGINE}" go test -count=1 -run 'TestAgentEngine_Conformance_Live' ./pkg/statestore/agentengine/) \
  || die "live conformance failed; fix that before reading any further result"

log "step 1: build ${BIN}"
(cd "${REPO_ROOT}" && go build -o "${BIN}" ./cmd/core-agent)

log "step 2: config pinned at ${CFG} (agentengine transcript, local db at ${DB})"
MODEL_JSON="${UAT_MODEL_JSON:-{\"name\":\"gemini-2.5-flash\",\"provider\":\"gemini\"}}"
cat > "${CFG}" <<JSON
{
  "model": ${MODEL_JSON},
  "permissions": {"mode": "allow"},
  "state": {
    "transcript": {
      "backend": "agentengine",
      "agentengine": {"reasoning_engine": "${ENGINE}"}
    }
  }
}
JSON

log "step 3: one headless turn with a tool call; the startup lines name both stores"
# The path scope is rooted at the cwd, so run from UAT_ROOT: the file
# the prompt reads is then in scope and the read is executed, not denied.
(cd "${UAT_ROOT}" && "${BIN}" -c "${CFG}" --session-db-path "${DB}" \
  -p "Reply with exactly: transcript-uat-ok. Then read the file .agents/config.json and tell me its model name." \
  2>"${LOG}") | tee "${UAT_ROOT}/turn.out"
grep -q "config.json" "${LOG}" "${UAT_ROOT}/turn.out" || log "warning: no sign of the read_file call; check ${LOG}"
grep -q "session store: agentengine" "${LOG}" || die "startup log does not name the agentengine store (see ${LOG})"
grep -q "implied by state.transcript.backend=agentengine" "${LOG}" || die "session db was not implied by the transcript backend (see ${LOG})"

read -r APP_NAME USER_ID SID < <(sqlite3 -separator ' ' "${DB}" 'select app_name, user_id, session_id from agent_guardrail_state limit 1;' 2>/dev/null || true)
[[ -n "${SID:-}" ]] || die "could not read the session triple from ${DB} (is sqlite3 installed?)"
log "session: app=${APP_NAME} user=${USER_ID} id=${SID}"

log "step 4: delete the local database — the sandbox is gone"
rm -f "${DB}" "${DB}-wal" "${DB}-shm"

log "step 5: restore the tree from Agent Engine with no local state at all"
(cd "${REPO_ROOT}" && go run ./dev/uat/agentengine-transcript/cmd/restore \
  -engine "${ENGINE}" -app "${APP_NAME}" -user "${USER_ID}" -session "${SID}") | tee "${UAT_ROOT}/restore.out"
grep -q "transcript-uat-ok" "${UAT_ROOT}/restore.out" || die "the model's reply is not in the restored transcript"
grep -q "tokens=" "${UAT_ROOT}/restore.out" || die "UsageMetadata did not survive the round-trip"

log "PASS: transcript for ${SID} outlived ${DB}. Outputs under ${UAT_ROOT}."
log "Record in README.md: events appended per turn (grep -c AppendEvent in a debug run), and whether any event was rejected for size."
