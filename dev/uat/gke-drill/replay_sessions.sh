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

# Save a server-side replay of every session on the hub touched in a
# window, for grading box A2 (#1042) with a2_count.py (#1258).
#
#   ./replay_sessions.sh --since 2026-10-06T17:30:00Z [--until <ts>] [--out <dir>]
#   ./replay_sessions.sh --all [--out <dir>]
#
# Why this exists. The drill captures the ONE session it decided was its
# incident, live, from whenever it attached. Two things are wrong with
# grading A2 on that alone, and the 2026-10-06 fault batch hit both:
#
#   - An incident can open more than one session. Run 2 opened two and
#     the drill captured one, so every failure in the other was
#     log-only by construction, whatever the product did.
#   - A live capture holds what happened while it was attached. Since
#     #1258 every guardrail trip and turn error is a durable eventlog row,
#     so a replay read afterwards (`GET …/events?since=0`) holds all of
#     them; a live capture that joined late does not.
#
# So: list the sessions on the hub, keep those whose last_touched_at is
# inside the window, and write each one's replay to
# <out>/replay-<sid>.sse, plus a sessions.tsv manifest and window.env
# (the window's start and end). Then grade with the window — the script
# prints the exact line:
#
#   ./a2_count.py --log daemon.log --events <out>/replay-*.sse \
#       --since <start> --until <end>
#
# A session touched in the window is a SUPERSET of the sessions opened
# in it (the hub has no created_at), and every replay reads from seq 0,
# so the replays hold history from before the window too. For A2 that
# is the MASKING direction, not the safe one: an extra transcript-side
# entry with no log to answer it covers one of the window's log-only
# failures and turns a FAIL into a PASS. That is why a2_count must be
# given the same window, which drops transcript events and log lines
# outside it on both sides. With --all there is no window to give; the
# script says so, and a grade from it is a read, not a verdict.
#
# Idle sessions are skipped unless --include-idle. Requesting /events on
# an idle session lazily resumes it, and a resume can run auto-continue,
# which starts a model turn — a grading helper must not do that to a
# session it was only asked to read. An incident session is normally
# still active when the batch ends; the manifest lists every skip. The
# status is read once, from the list: a session evicted between that
# read and its replay is resumed by the replay. The window is seconds;
# run this before the daemon's idle eviction would reach the batch's
# sessions.
#
# Each replay is bounded by DRILL_REPLAY_SECS (default 20): /events
# replays and then live-tails, so the stream never ends on its own, and
# curl's --max-time is what stops it. A replay of a long session that
# needs longer than that is reported as possibly truncated.
#
# Reads only. Nothing is injected, interrupted or resumed (see above).
set -euo pipefail

DRILL_SELF_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
# shellcheck source=lib.sh
source "${DRILL_SELF_DIR}/lib.sh"

usage() {
    cat >&2 <<'EOF'
usage: ./replay_sessions.sh (--since <RFC3339> [--until <RFC3339>] | --all)
                            [--out <dir>] [--include-idle]

Saves GET /sessions/<app>/<sid>/events?since=0 for every session on the
hub whose last_touched_at falls in the window, to <dir>/replay-<sid>.sse,
with a sessions.tsv manifest and the window in window.env. Grade with
the line it prints, which passes the same window to a2_count:

  ./a2_count.py --log daemon.log --events <dir>/replay-*.sse --since <start> --until <end>

Environment (all optional):
  DRILL_REPLAY_SECS=20   how long to read each replay before stopping
  DRILL_PORT=7779        local port for the hub tunnel
  DRILL_APP=core-agent   the app, for a session the list names none for
EOF
    exit "${1:-1}"
}

SINCE="" UNTIL="" ALL=0 OUT="" INCLUDE_IDLE=0
while (( $# )); do
    case "$1" in
        --since)        SINCE="${2:?--since needs a timestamp}"; shift ;;
        --until)        UNTIL="${2:?--until needs a timestamp}"; shift ;;
        --all)          ALL=1 ;;
        --out)          OUT="${2:?--out needs a directory}"; shift ;;
        --include-idle) INCLUDE_IDLE=1 ;;
        -h|--help)      usage 0 ;;
        *)              printf 'unknown argument: %s\n' "$1" >&2; usage 1 ;;
    esac
    shift
done
if [[ "${ALL}" == "1" && -n "${SINCE}${UNTIL}" ]] || [[ "${ALL}" == "0" && -z "${SINCE}" ]]; then
    printf 'give exactly one of --since <ts> or --all\n' >&2
    usage 1
fi

DRILL_REPLAY_SECS="${DRILL_REPLAY_SECS:-20}"
REPLAY_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
OUT="${OUT:-${DRILL_RUN_ROOT}/replays-$(date -u +%Y%m%dT%H%M%SZ)}"
mkdir -p "${OUT}"
chmod 700 "${OUT}" 2>/dev/null || true
# lib.sh's port-forward writes its log under DRILL_RUN_DIR.
DRILL_RUN_DIR="${OUT}"

# replay_select <sessions.json> — "<sid>\t<app>\t<last_touched_at>\t<status>\t<verdict>"
# per session, verdict "take" or the reason it was skipped. Python, not
# jq: last_touched_at is Go's RFC3339Nano (up to nine fractional digits,
# trailing zeros trimmed, a zone that may be an offset), and jq's
# fromdateiso8601 parses none of that. The fraction is normalised to six
# digits because datetime.fromisoformat before Python 3.11 takes only
# three or six.
replay_select() {
    python3 - "$1" "${SINCE}" "${UNTIL}" "${INCLUDE_IDLE}" "${DRILL_APP}" <<'PY'
import json, re, sys
from datetime import datetime, timezone

def parse(ts):
    if not ts:
        return None
    ts = ts.strip().replace("Z", "+00:00")
    ts = re.sub(r"\.(\d+)", lambda m: "." + (m.group(1) + "000000")[:6], ts)
    t = datetime.fromisoformat(ts)
    return t if t.tzinfo else t.replace(tzinfo=timezone.utc)

path, since, until, include_idle = sys.argv[1], parse(sys.argv[2]), parse(sys.argv[3]), sys.argv[4] == "1"
default_app = sys.argv[5]
for s in json.load(open(path)).get("sessions") or []:
    sid, touched, status = s.get("sessionID", ""), s.get("last_touched_at", ""), s.get("status", "")
    app = s.get("app") or default_app
    if not sid:
        continue
    t = parse(touched) if touched else None
    if since and (t is None or t < since):
        verdict = "skip: not touched since --since" if t else "skip: no last_touched_at"
    elif until and t is not None and t > until:
        verdict = "skip: touched after --until"
    elif status == "idle" and not include_idle:
        verdict = "skip: idle (a read would resume it; --include-idle to read anyway)"
    else:
        verdict = "take"
    print(f"{sid}\t{app}\t{touched}\t{status}\t{verdict}")
PY
}

cleanup() { drill_stop_port_forward; }
trap cleanup EXIT
# An interrupt ends the run: without the exit, the loop would go on and
# record every remaining replay as a failure against a dead tunnel.
trap 'cleanup; exit 130' INT TERM

drill_require_curl_version
require_coordinates || exit 1
drill_load_token
drill_port_forward

hub_get "/sessions" > "${OUT}/sessions.json"
# Selected into a file first, not straight into the loop's process
# substitution: that would swallow a failure here, and a bad --since
# would then report "no session matched" instead of the parse error.
replay_select "${OUT}/sessions.json" > "${OUT}/selection.tsv" \
    || drill_die "could not select sessions from ${OUT}/sessions.json (is --since/--until RFC 3339?)"
MANIFEST="${OUT}/sessions.tsv"
printf 'session\tlast_touched_at\tstatus\tresult\n' > "${MANIFEST}"

taken=0 skipped=0
while IFS=$'\t' read -r sid app touched status verdict; do
    [[ -n "${sid}" ]] || continue
    if [[ "${verdict}" != "take" ]]; then
        printf '%s\t%s\t%s\t%s\n' "${sid}" "${touched}" "${status}" "${verdict}" >> "${MANIFEST}"
        skipped=$(( skipped + 1 ))
        continue
    fi
    raw="${OUT}/replay-${sid//[^A-Za-z0-9._-]/_}.sse"
    rc=0
    curl -sS -N --no-buffer --max-time "${DRILL_REPLAY_SECS}" -K "${DRILL_CURL_CFG}" \
        "${DRILL_BASE_URL}/sessions/${app}/${sid}/events?since=0" \
        > "${raw}" 2>"${raw%.sse}.stderr" || rc=$?
    # 28 is curl reaching --max-time, which is how every replay ends:
    # the stream live-tails after the replay and never closes itself.
    if [[ "${rc}" != "0" && "${rc}" != "28" ]]; then
        result="FAILED (curl exit ${rc}; see ${raw##*/} .stderr)"
        drill_warn "session ${sid}: replay failed (curl exit ${rc})"
    else
        frames=$(grep -c '^event: agent' "${raw}" || true)
        result="${frames} agent frames"
        if [[ "${frames}" == "0" ]]; then
            result="EMPTY (no agent frames)"
            drill_warn "session ${sid}: replay holds no agent frames"
        fi
        drill_ok "session ${sid}: ${result} -> ${raw##*/}"
    fi
    printf '%s\t%s\t%s\t%s\n' "${sid}" "${touched}" "${status}" "${result}" >> "${MANIFEST}"
    taken=$(( taken + 1 ))
done < "${OUT}/selection.tsv"

drill_log "${taken} replayed, ${skipped} skipped — manifest ${MANIFEST}"
(( taken > 0 )) || drill_die "no session on the hub matched the window; nothing to grade."

# The window a2_count must be given. An open --until ends now: the log a
# grader passes was captured up to about now, and nothing in a replay
# can be newer.
WINDOW_END="${UNTIL:-${REPLAY_STARTED_AT}}"
{
    printf 'WINDOW_START=%q\n' "${SINCE}"
    printf 'WINDOW_END=%q\n' "${WINDOW_END}"
    printf 'WINDOW_END_IS_REPLAY_TIME=%q\n' "$([[ -z "${UNTIL}" ]] && echo yes || echo no)"
} > "${OUT}/window.env"

printf '\nGrade box A2 with (window recorded in %s/window.env):\n' "${OUT}"
if [[ -n "${SINCE}" ]]; then
    printf '  %s/a2_count.py --log <daemon.log> --events %s/replay-*.sse --since %s --until %s\n' \
        "${DRILL_SELF_DIR}" "${OUT}" "${SINCE}" "${WINDOW_END}"
else
    printf '  %s/a2_count.py --log <daemon.log> --events %s/replay-*.sse --since <batch start> --until %s\n' \
        "${DRILL_SELF_DIR}" "${OUT}" "${WINDOW_END}"
    drill_warn "--all records no window start. Replays hold every session's whole history, so grading"
    drill_warn "  without --since lets rows from before the batch mask its log-only failures."
fi
