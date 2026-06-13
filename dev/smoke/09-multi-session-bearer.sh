#!/usr/bin/env bash
# Smoke: multi-session daemon end-to-end (#162 α.1 + α.2 + β + γ).
#
# Builds core-agent, starts it with multi_session.enabled against a
# locally-staged users.json table (alice + bob + ops), then exercises
# every isolation invariant the substrate promises:
#
#   - Unauthenticated request → 401
#   - Bad token → 401
#   - Valid token → 200 + Caller resolved
#   - Stranger (alice) can't see the admin-owned legacy session → 404 (not 403)
#   - Admin (ops) sees everything
#   - Audit log carries caller= per event when admin injects
#   - Proxy: bot identity asserts another user via X-Asserted-Caller → 200
#   - Non-proxy caller using X-Asserted-Caller → 401
#
# All temp state defaults under /tmp/multi-session-smoke/ per the
# UAT-files-in-/tmp convention. Override SMOKE_DIR to relocate.
#
# Provider: echo (no API keys needed). The smoketest exercises the
# substrate, not model quality.

set -euo pipefail
source "$(dirname "$0")/_common.sh"

SMOKE_DIR="${SMOKE_DIR:-/tmp/multi-session-smoke}"
PORT="${SMOKE_PORT:-37777}"
BASE="http://127.0.0.1:${PORT}"
USERS_FILE="${SMOKE_DIR}/users.json"
SESSION_DB="${SMOKE_DIR}/session.db"
LOG_FILE="${SMOKE_DIR}/core-agent.log"
WORK_DIR="${SMOKE_DIR}/work"
ALICE_TOKEN="tok_alice_$(openssl rand -hex 16)"
BOB_TOKEN="tok_bob_$(openssl rand -hex 16)"
OPS_TOKEN="tok_ops_$(openssl rand -hex 16)"
BOT_TOKEN="tok_bot_$(openssl rand -hex 16)"

build_core_agent

cleanup() {
    if [[ -n "${DAEMON_PID:-}" ]] && kill -0 "${DAEMON_PID}" 2>/dev/null; then
        kill "${DAEMON_PID}" 2>/dev/null || true
        wait "${DAEMON_PID}" 2>/dev/null || true
    fi
}
trap cleanup EXIT

log_step "stage smoke directory + users.json (mode 0600)"
rm -rf "${SMOKE_DIR}"
mkdir -p "${SMOKE_DIR}" "${WORK_DIR}/.agents"
cat > "${USERS_FILE}" <<EOF
{
  "version": 1,
  "users": [
    { "identity": "alice@example.com", "token": "${ALICE_TOKEN}" },
    { "identity": "bob@example.com",   "token": "${BOB_TOKEN}"   },
    { "identity": "ops@example.com",   "token": "${OPS_TOKEN}"   },
    { "identity": "sa:slack-bot",      "token": "${BOT_TOKEN}"   }
  ]
}
EOF
chmod 0600 "${USERS_FILE}"

cat > "${WORK_DIR}/.agents/config.json" <<EOF
{
  "version": 1,
  "model": { "name": "echo" },
  "permissions": { "mode": "yolo" },
  "attach": {
    "listen": "127.0.0.1:${PORT}",
    "multi_session": {
      "enabled": true,
      "auth": {
        "kind": "bearer_table",
        "table_file": "${USERS_FILE}"
      },
      "admin_identities": ["ops@example.com"],
      "proxy_identities": ["sa:slack-bot"],
      "default_identity": "anon",
      "allow_anonymous": false
    }
  }
}
EOF

log_step "start core-agent daemon (multi_session.enabled, provider=echo)"
(
    cd "${WORK_DIR}"
    "${CORE_AGENT}" --provider=echo --no-repl \
        --session-db --session-db-path="${SESSION_DB}" \
        < /dev/null > "${LOG_FILE}" 2>&1 &
    echo $! > "${SMOKE_DIR}/daemon.pid"
)
DAEMON_PID=$(cat "${SMOKE_DIR}/daemon.pid")

# Wait for the listener to bind. 5s should be plenty.
deadline=$(( $(date +%s) + 5 ))
while ! curl -s -o /dev/null --max-time 1 "${BASE}/sessions" 2>/dev/null; do
    sleep 0.2
    if (( $(date +%s) > deadline )); then
        cat "${LOG_FILE}" >&2
        fail "daemon did not bind ${PORT} within 5s"
    fi
done

# -----------------------------------------------------------------
# Auth boundary tests
# -----------------------------------------------------------------

log_step "no Authorization → 401"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE}/sessions")
if [[ "${code}" != "401" ]]; then
    fail "expected 401 on unauthenticated request, got ${code}"
fi
pass "unauthenticated request rejected (401)"

log_step "invalid bearer token → 401"
code=$(curl -s -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer not-a-real-token" "${BASE}/sessions")
if [[ "${code}" != "401" ]]; then
    fail "expected 401 on invalid token, got ${code}"
fi
pass "invalid token rejected (401)"

log_step "valid alice token → 200"
code=$(curl -s -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer ${ALICE_TOKEN}" "${BASE}/sessions")
if [[ "${code}" != "200" ]]; then
    fail "expected 200 with valid alice token, got ${code}"
fi
pass "valid token accepted (200)"

# -----------------------------------------------------------------
# Session list filtering (the hidden-existence invariant)
# -----------------------------------------------------------------

log_step "alice sees no sessions (the legacy startup session is unowned → admin-only)"
alice_sessions=$(curl -s -H "Authorization: Bearer ${ALICE_TOKEN}" "${BASE}/sessions")
if ! grep -q '"sessions":\[\]' <<<"${alice_sessions}"; then
    fail "alice should see empty sessions list; got: ${alice_sessions}"
fi
pass "alice sees zero sessions (unauthorized hidden)"

log_step "bob also sees no sessions"
bob_sessions=$(curl -s -H "Authorization: Bearer ${BOB_TOKEN}" "${BASE}/sessions")
if ! grep -q '"sessions":\[\]' <<<"${bob_sessions}"; then
    fail "bob should see empty sessions list; got: ${bob_sessions}"
fi
pass "bob sees zero sessions"

log_step "ops (admin) sees every session"
ops_sessions=$(curl -s -H "Authorization: Bearer ${OPS_TOKEN}" "${BASE}/sessions")
if ! grep -q '"sessionID"' <<<"${ops_sessions}"; then
    fail "ops should see the legacy session; got: ${ops_sessions}"
fi
pass "ops sees the legacy session via admin bypass"

# Extract the session ID for later assertions.
SID=$(printf '%s' "${ops_sessions}" \
    | grep -o '"sessionID":"[^"]*"' \
    | head -1 \
    | cut -d'"' -f4)
if [[ -z "${SID}" ]]; then
    fail "could not extract sessionID from ops list output"
fi

# -----------------------------------------------------------------
# Per-session ACL: alice can't see / inject into ops's session
# -----------------------------------------------------------------

log_step "alice tries to GET ops's session events → 404 (NOT 403)"
code=$(curl -s -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer ${ALICE_TOKEN}" \
    "${BASE}/sessions/${SID}/status")
if [[ "${code}" != "404" ]]; then
    fail "expected 404 (hide existence) on alice → ops session; got ${code}"
fi
pass "alice → ops session returns 404 (not 403 — the no-leak invariant)"

log_step "alice tries to inject into ops's session → 404"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H "Authorization: Bearer ${ALICE_TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"message":"alice injecting"}' \
    "${BASE}/sessions/${SID}/inject")
if [[ "${code}" != "404" ]]; then
    fail "expected 404 on alice → ops inject; got ${code}"
fi
pass "alice inject into ops session returns 404"

# -----------------------------------------------------------------
# Admin can inject; audit log carries caller=ops
# -----------------------------------------------------------------

log_step "ops injects into own session"
inject_response=$(curl -s -X POST \
    -H "Authorization: Bearer ${OPS_TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"message":"hello from ops"}' \
    "${BASE}/sessions/${SID}/inject")
if ! grep -q '"injected"' <<<"${inject_response}"; then
    fail "ops inject failed; got: ${inject_response}"
fi
pass "ops inject succeeded"

# Give the agent a moment to drain the inbox and write eventlog rows.
sleep 1.5

log_step "audit log row for the ops inject carries caller=ops in Metadata"
if ! command -v sqlite3 >/dev/null 2>&1; then
    pass "skipping audit-log assertion: sqlite3 not installed"
else
    rows=$(sqlite3 "${SESSION_DB}" \
        "SELECT metadata FROM agent_eventlog WHERE session_id = '${SID}' AND metadata != '' ORDER BY seq;")
    if [[ -z "${rows}" ]]; then
        cat "${LOG_FILE}" >&2
        fail "no eventlog rows with metadata for session ${SID}"
    fi
    if ! grep -q "ops@example.com" <<<"${rows}"; then
        fail "expected caller=ops@example.com in metadata; got:\n${rows}"
    fi
    pass "audit log threads caller=ops through Metadata sidecar"
fi

# -----------------------------------------------------------------
# Proxy / X-Asserted-Caller
# -----------------------------------------------------------------

log_step "proxy: bot asserts alice@ → bot authenticates, effective Caller becomes alice"
# Since alice doesn't own the legacy session, the proxy succeeds at
# the auth layer but the session-level ACL still denies (effective
# caller is alice). 404 is the expected response.
code=$(curl -s -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer ${BOT_TOKEN}" \
    -H "X-Asserted-Caller: alice@example.com" \
    "${BASE}/sessions/${SID}/status")
if [[ "${code}" != "404" ]]; then
    fail "expected 404 (alice not authorized on ops session); got ${code}"
fi
pass "proxy header accepted at auth layer; ACL still denies (404 — the right composition)"

log_step "proxy: bot asserts unknown identity → 401"
code=$(curl -s -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer ${BOT_TOKEN}" \
    -H "X-Asserted-Caller: ghost@example.com" \
    "${BASE}/sessions")
if [[ "${code}" != "401" ]]; then
    fail "expected 401 when bot asserts unknown identity; got ${code}"
fi
pass "proxy assertion of unprovisioned identity → 401"

log_step "non-proxy user (alice) tries to use X-Asserted-Caller → 401"
code=$(curl -s -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer ${ALICE_TOKEN}" \
    -H "X-Asserted-Caller: bob@example.com" \
    "${BASE}/sessions")
if [[ "${code}" != "401" ]]; then
    fail "expected 401 when non-proxy caller asserts identity; got ${code}"
fi
pass "non-proxy caller asserting → 401 (security trail recorded)"

# -----------------------------------------------------------------
# users.json file-mode enforcement (offline test — restart with
# world-readable file should fail to start)
# -----------------------------------------------------------------

# -----------------------------------------------------------------
# POST /sessions — on-demand session creation (the headline use case
# that v2.4 deferred and the spike PR lit up). Each authenticated
# caller can now own their own session, and the cross-session
# isolation invariants finally become observable end-to-end.
# -----------------------------------------------------------------

log_step "alice creates her own session via POST /sessions"
alice_create=$(curl -s -X POST \
    -H "Authorization: Bearer ${ALICE_TOKEN}" \
    "${BASE}/sessions")
ALICE_SID=$(printf '%s' "${alice_create}" | grep -o '"sessionID":"[^"]*"' | head -1 | cut -d'"' -f4)
if [[ -z "${ALICE_SID}" ]]; then
    fail "alice POST /sessions did not return a sessionID; got: ${alice_create}"
fi
pass "alice owns session ${ALICE_SID}"

log_step "bob creates his own session via POST /sessions"
bob_create=$(curl -s -X POST \
    -H "Authorization: Bearer ${BOB_TOKEN}" \
    "${BASE}/sessions")
BOB_SID=$(printf '%s' "${bob_create}" | grep -o '"sessionID":"[^"]*"' | head -1 | cut -d'"' -f4)
if [[ -z "${BOB_SID}" ]]; then
    fail "bob POST /sessions did not return a sessionID; got: ${bob_create}"
fi
if [[ "${BOB_SID}" == "${ALICE_SID}" ]]; then
    fail "alice + bob got the same sessionID ${ALICE_SID}; factory must generate unique IDs"
fi
pass "bob owns session ${BOB_SID} (distinct from alice)"

log_step "alice's GET /sessions shows ONLY alice's session"
alice_list=$(curl -s -H "Authorization: Bearer ${ALICE_TOKEN}" "${BASE}/sessions")
if ! grep -q "${ALICE_SID}" <<<"${alice_list}"; then
    fail "alice can't see her own session; got: ${alice_list}"
fi
if grep -q "${BOB_SID}" <<<"${alice_list}"; then
    fail "alice CAN see bob's session — the no-leak invariant is broken; got: ${alice_list}"
fi
pass "alice sees alice-only session list (bob's session hidden)"

log_step "bob's GET /sessions shows ONLY bob's session"
bob_list=$(curl -s -H "Authorization: Bearer ${BOB_TOKEN}" "${BASE}/sessions")
if ! grep -q "${BOB_SID}" <<<"${bob_list}"; then
    fail "bob can't see his own session; got: ${bob_list}"
fi
if grep -q "${ALICE_SID}" <<<"${bob_list}"; then
    fail "bob CAN see alice's session — the no-leak invariant is broken; got: ${bob_list}"
fi
pass "bob sees bob-only session list (alice's session hidden)"

log_step "alice tries to inject into bob's session → 404"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H "Authorization: Bearer ${ALICE_TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"message":"alice nosing into bob"}' \
    "${BASE}/sessions/${BOB_SID}/inject")
if [[ "${code}" != "404" ]]; then
    fail "alice → bob's session inject returned ${code}; expected 404 (the no-leak invariant)"
fi
pass "alice → bob's session inject blocked (404)"

log_step "ops (admin) sees all three sessions (legacy + alice + bob)"
ops_list_after=$(curl -s -H "Authorization: Bearer ${OPS_TOKEN}" "${BASE}/sessions")
for needed in "${SID}" "${ALICE_SID}" "${BOB_SID}"; do
    if ! grep -q "${needed}" <<<"${ops_list_after}"; then
        fail "ops missing session ${needed}; got: ${ops_list_after}"
    fi
done
pass "ops (admin) sees all three sessions via bypass"

log_step "POST /sessions without auth → 401"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${BASE}/sessions")
if [[ "${code}" != "401" ]]; then
    fail "expected 401 on unauthenticated POST /sessions; got ${code}"
fi
pass "unauthenticated POST /sessions rejected (401)"

log_step "loader rejects world-readable users.json at startup"
chmod 0644 "${USERS_FILE}"
loose_log="${SMOKE_DIR}/loose-mode-startup.log"
if (cd "${WORK_DIR}" && "${CORE_AGENT}" --provider=echo \
        --session-db "${SMOKE_DIR}/loose.db" > "${loose_log}" 2>&1) ; then
    fail "daemon should have refused to start with mode 0644 users.json"
fi
if ! grep -q "must be 0600 or stricter" "${loose_log}"; then
    fail "expected file-mode error message; got:\n$(cat "${loose_log}")"
fi
pass "world-readable users.json rejected at startup (file-mode invariant)"
chmod 0600 "${USERS_FILE}"

printf '\n%sALL CHECKS PASSED%s — multi-session substrate + POST /sessions wired correctly.\n' "${GREEN}${BOLD}" "${RESET}"
