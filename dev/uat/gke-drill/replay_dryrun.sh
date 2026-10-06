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

# Run replay_sessions.sh end to end with no cluster, then grade its
# output with a2_count.py.
#
#     ./replay_dryrun.sh
#
# Same fakes as dryrun.sh (testdata/fakebin): a kubectl whose
# port-forward really binds, and a curl that answers the hub from
# fixtures and refuses anything it does not recognise.
#
# The hub here holds the 2026-10-06 run-2 shape (#1258): ONE incident
# that opened TWO sessions, each carrying its own cut and turn error as
# durable rows, plus a session from before the window and an idle one.
# What it proves: the window selects exactly the sessions it should, an
# idle session is not read (a read would resume it) unless asked, every
# replay lands in its own file with a manifest, the helper only reads,
# and a2_count over the replays grades A2 met where a capture of one
# session would have failed it.

set -uo pipefail

SELF_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
cd "${SELF_DIR}"

FAKEBIN="${SELF_DIR}/testdata/fakebin"
PASS=0
FAIL=0

ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; PASS=$((PASS + 1)); }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$*"; FAIL=$((FAIL + 1)); }
head_() { printf '\n\033[1m%s\033[0m\n' "$*"; }
eq() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }
grep_()  { if [[ -f "$2" ]] && grep -Eq -- "$3" "$2"; then ok "$1"; else bad "$1 (no /$3/ in ${2##*/})"; fi; }
ungrep() { if [[ -f "$2" ]] && grep -Eq -- "$3" "$2"; then bad "$1 (unexpected /$3/ in ${2##*/})"; else ok "$1"; fi; }
have()   { if [[ -s "$2" ]]; then ok "$1"; else bad "$1 (${2##*/} missing or empty)"; fi; }
absent() { if [[ -e "$2" ]]; then bad "$1 (${2##*/} exists)"; else ok "$1"; fi; }

WORKDIR=""
cleanup() {
    pkill -f 'socket.socket' 2>/dev/null || true
    if [[ "${DRYRUN_KEEP:-}" == "1" ]]; then
        printf '\n  artifacts kept in %s\n' "${WORKDIR}"
    else
        [[ -n "${WORKDIR}" && -d "${WORKDIR}" ]] && rm -rf "${WORKDIR}"
    fi
    return 0
}
trap cleanup EXIT INT TERM
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/gke-drill-replay-dryrun.XXXXXX")"

export PATH="${FAKEBIN}:${PATH}"
# If the fakes ever failed to shadow the real tools, this would talk to
# whatever cluster the operator is pointed at.
export KUBECONFIG=/dev/null
export PROJECT_ID=fixture-project CLUSTER_NAME=fixture-cluster KUBE_CONTEXT=fixture-context
export REGION=us-central1 MODEL_FLAVOR=gemini DEMO_NS=drill-demo TARGET_NS=drill-target
export WORKLOAD=emailservice DRILL_PORT=7861 DRILL_REPLAY_SECS=1

# One session's replay: a per-turn cut and the turn error it caused, as
# the durable rows a #1258 daemon writes, plus the incident prompt.
replay_fixture() {
    local sid="$1"
    python3 - "${sid}" <<'PY'
import json, sys
sid = sys.argv[1]
frames = [
    {"seq": 1, "event": {"ID": f"{sid}-u", "Author": "user",
                         "Content": {"role": "user", "parts": [{"text": "[Inbox] emailservice ImagePullBackOff"}]}}},
    {"seq": 2, "event": {"ID": f"{sid}-trip", "Author": "agent/guardrail-turn-trip", "InvocationID": "guardrail-turn-trip",
                         "CustomMetadata": {"source": "agent", "guardrail": "cost_ceiling",
                                            "reason": "per-turn cost ceiling exceeded", "halted_turn": True}}},
    {"seq": 3, "event": {"ID": f"{sid}-err", "Author": "agent/turn-error", "InvocationID": "turn-error",
                         "CustomMetadata": {"source": "agent", "kind": "canceled", "code": "CANCELED",
                                            "message": "turn canceled", "retryable": False, "cut_by": "cost_ceiling"}}},
]
print('event: capabilities\ndata: {"protocol_version":"1.19.0"}\n')
for f in frames:
    print("event: agent\ndata: " + json.dumps(f) + "\n")
PY
}

# run_case <name> <args…> — sets OUT_DIR, RC, LOG, CALLS.
CASE_N=0
run_case() {
    local name="$1"; shift
    local dir="${WORKDIR}/$((++CASE_N))-${name}"
    mkdir -p "${dir}/fake" "${dir}/state"
    export DRILL_FAKE_DIR="${dir}/fake" RIG_STATE_DIR="${dir}/state" DRILL_RUN_ROOT="${dir}/runs"
    printf 'export PLATFORM_TOKEN=dryrun-fake-token\n' > "${RIG_STATE_DIR}/demo-tokens.env"
    : > "${DRILL_FAKE_DIR}/calls.log"
    # The fake hub lists sessions-after.json once the cluster is "broken".
    touch "${DRILL_FAKE_DIR}/broken"
    jq -nc '{sessions: [
        {sessionID: "sess-old",    last_touched_at: "2026-10-06T16:00:00Z",           status: "active"},
        {sessionID: "sess-inc-1",  last_touched_at: "2026-10-06T17:36:29.123456789Z", status: "active"},
        {sessionID: "sess-inc-2",  last_touched_at: "2026-10-06T19:37:02+02:00",      status: "active"},
        {sessionID: "sess-idle",   last_touched_at: "2026-10-06T17:40:00Z",           status: "idle"},
        {sessionID: "sess-late",   last_touched_at: "2026-10-06T23:00:00Z",           status: "active"}
    ]}' > "${DRILL_FAKE_DIR}/sessions-after.json"
    local sid
    for sid in sess-old sess-inc-1 sess-inc-2 sess-idle sess-late; do
        replay_fixture "${sid}" > "${DRILL_FAKE_DIR}/events-${sid}.sse"
    done
    OUT_DIR="${dir}/out"
    LOG="${dir}/replay.out"
    set +e
    ./replay_sessions.sh --out "${OUT_DIR}" "$@" > "${LOG}" 2>&1
    RC=$?
    set -e
    CALLS="${DRILL_FAKE_DIR}/calls.log"
}

set -e

head_ "1. a window selects the incident's sessions — both of them"
run_case window --since 2026-10-06T17:30:00Z --until 2026-10-06T18:00:00Z
eq "exit 0" "${RC}" "0"
have "the first incident session is replayed" "${OUT_DIR}/replay-sess-inc-1.sse"
have "the second one too (nanosecond timestamp and a +02:00 offset both parse)" "${OUT_DIR}/replay-sess-inc-2.sse"
absent "a session last touched before --since is not read" "${OUT_DIR}/replay-sess-old.sse"
absent "a session touched after --until is not read" "${OUT_DIR}/replay-sess-late.sse"
absent "an idle session is not read: the read would resume it" "${OUT_DIR}/replay-sess-idle.sse"
grep_ "the manifest says why the idle one was skipped" "${OUT_DIR}/sessions.tsv" $'^sess-idle\t.*skip: idle'
grep_ "the manifest counts each replay's agent frames" "${OUT_DIR}/sessions.tsv" $'^sess-inc-1\t.*\t3 agent frames$'
grep_ "every replay reads from seq 0" "${CALLS}" 'sessions/core-agent/sess-inc-2/events\?since=0'
ungrep "it only reads: no inject, interrupt or POST" "${CALLS}" 'inject|interrupt|-X POST'

# The point of all of it: grade A2 on every session the incident opened.
A2LOG="${WORKDIR}/daemon.log"
{
    for s in sess-inc-1 sess-inc-2; do
        printf '2026-10-06T17:36:30Z agent: [session %s] cost_ceiling guardrail cut the turn in flight — the cancellation error that follows is this cut, not a provider failure: per-turn\n' "${s}"
        printf '2026-10-06T17:36:31Z core-agent: session %s turn: context canceled\n' "${s}"
    done
} > "${A2LOG}"
set +e
python3 a2_count.py --log "${A2LOG}" --events "${OUT_DIR}"/replay-*.sse > "${WORKDIR}/a2.out" 2>&1
A2RC=$?
python3 a2_count.py --log "${A2LOG}" --events "${OUT_DIR}/replay-sess-inc-1.sse" > "${WORKDIR}/a2-one.out" 2>&1
A2ONE=$?
set -e
grep_ "a2_count over both replays: guardrail trip 2 vs 2" "${WORKDIR}/a2.out" '\| guardrail trip \| 2 \| 2 \| 0 \| \*\*PASS\*\*'
grep_ "…and turn error 2 vs 2" "${WORKDIR}/a2.out" '\| turn error \| 2 \| 2 \| 0 \| \*\*PASS\*\*'
eq "a2_count exits 1 only on a FAIL: here 2 (provider retry unexercised), not 1" "${A2RC}" "2"
eq "graded on ONE of the two sessions — what the drill captured on 2026-10-06 — A2 fails" "${A2ONE}" "1"

head_ "2. --include-idle reads the idle session too"
run_case idle --since 2026-10-06T17:30:00Z --until 2026-10-06T18:00:00Z --include-idle
eq "exit 0" "${RC}" "0"
have "the idle session is replayed when asked" "${OUT_DIR}/replay-sess-idle.sse"

head_ "3. --all takes every active session"
run_case all --all
eq "exit 0" "${RC}" "0"
eq "four active sessions replayed" "$(find "${OUT_DIR}" -name 'replay-*.sse' | wc -l | tr -d ' ')" "4"

head_ "4. a window nothing falls in is an error, not an empty pass"
run_case empty --since 2027-01-01T00:00:00Z
eq "exit 1" "${RC}" "1"
grep_ "it says nothing matched" "${LOG}" 'no session on the hub matched the window'

head_ "5. a --since that does not parse is reported as that, not as an empty window"
run_case badts --since yesterday
eq "exit 1" "${RC}" "1"
grep_ "it names the selection failure" "${LOG}" 'could not select sessions'
ungrep "…and does not claim nothing matched" "${LOG}" 'no session on the hub matched'

head_ "6. a short Go fraction (trailing zeros trimmed) parses too"
run_case shortfrac --since 2026-10-06T17:36:29.12Z --until 2026-10-06T17:36:29.2Z
eq "exit 0" "${RC}" "0"
have "the session touched at .123456789 falls inside [.12, .2]" "${OUT_DIR}/replay-sess-inc-1.sse"

head_ "7. it refuses ambiguous arguments before touching anything"
run_case noargs
eq "no --since and no --all: exit 1" "${RC}" "1"
grep_ "…with the usage" "${LOG}" 'give exactly one of --since'
ungrep "…and no tunnel was opened" "${CALLS}" 'port-forward'
run_case both --all --since 2026-10-06T17:30:00Z
eq "--all with --since: exit 1" "${RC}" "1"

printf '\n%d passed, %d failed\n' "${PASS}" "${FAIL}"
(( FAIL == 0 ))
