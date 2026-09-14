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

# The overnight soak (#1042 box A1) — leave the recipe running against a
# live cluster for eight hours, keep feeding it real incidents, and
# record enough to say afterwards whether it survived.
#
#   ./soak.sh                 8 hours, an incident every 30 minutes
#   SOAK_HOURS=1 ./soak.sh    a short rehearsal before committing a night
#
# This is NOT the drill. The drill scores one incident against six
# rubric boxes and needs a human for four of them. The soak scores
# nothing: it produces a timeline, and box A1 is read off that timeline
# by a person. The two things it must not do are invent a verdict and
# require a human while it runs.
#
# What A1 asks for, and what this records for each of them:
#
#   "no wedged session"        — every session's status and its
#                                `halted` flag are sampled on a fixed
#                                tick; a session that reports `working`
#                                across the whole quiet gap between two
#                                incidents is wedged, and the timeline
#                                shows it.
#   "no silently-disabled      — the daemon log is captured with
#    compaction"                 timestamps for the whole run (retries
#                                and compaction notices are invisible in
#                                transcripts — #1039 paid for that
#                                lesson) and every context-reduction row
#                                is pulled out of it at the end; and the
#                                sampler records each session's LAST
#                                TURN's input-token count, which is the
#                                context occupancy, so a compaction that
#                                did fire is a cliff and one that never
#                                fired is a straight line.
#   "no double-drive"          — the sampler records each session's turn
#                                count, so the same incident driven
#                                twice shows as a session whose turns
#                                jump without an incident having been
#                                injected for it.
#   "no unbounded growth"      — container memory, restart count,
#                                session count and each session's peak
#                                context occupancy, on the same tick.
#
# The failure this is built to avoid is the one the 2026-09-13 batch
# hit: thirteen real provider retries that left no trace in any of
# twenty-one transcripts. Everything here that matters is read from the
# daemon log or from the hub's own session list, not from a transcript.

set -euo pipefail

SOAK_SELF_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
# shellcheck source=lib.sh
source "${SOAK_SELF_DIR}/lib.sh"

usage() {
    cat >&2 <<'EOF'
usage: ./soak.sh

Runs the already-deployed examples/gke-platform-agent recipe unattended
against a live cluster, injecting an incident on a fixed cycle, and
writes a timeline to ~/.gke-drill/soak/<stamp>/.

Environment (all optional):
  SOAK_HOURS=8              how long to run
  SOAK_CYCLE_SECS=1800      seconds between incident injections
  SOAK_PROBE_SECS=120       seconds between health samples
  SOAK_HOLD_SECS=600        how long an incident stays broken before restore
  SOAK_SCENARIOS="a b c"    rotation order
  SOAK_PORT=7780            local port for the hub tunnel
  FORCE=1                   proceed with a foreign watcher racing
  WORKLOAD / TARGET_NS / …  inherited from the recipe's scripts/prereqs.sh

Stop it early with Ctrl-C or `kill`: the trap restores the workload and
writes the summary from whatever it has, so a run cut short is still
evidence.
EOF
    exit "${1:-1}"
}
[[ "${1:-}" == "-h" || "${1:-}" == "--help" ]] && usage 0

SOAK_HOURS="${SOAK_HOURS:-8}"
SOAK_CYCLE_SECS="${SOAK_CYCLE_SECS:-1800}"
SOAK_PROBE_SECS="${SOAK_PROBE_SECS:-120}"
SOAK_HOLD_SECS="${SOAK_HOLD_SECS:-600}"
SOAK_SCENARIOS="${SOAK_SCENARIOS:-a b c}"

# Its own port. The drill uses 7779 and attach.sh uses 7778; an
# eight-hour tunnel must not be the reason either of those fails to
# bind halfway through the night.
DRILL_PORT="${SOAK_PORT:-7780}"
DRILL_BASE_URL="http://127.0.0.1:${DRILL_PORT}"

SOAK_RUN_ROOT="${SOAK_RUN_ROOT:-${HOME}/.gke-drill/soak}"
SOAK_STAMP=$(date -u +%Y%m%dT%H%M%SZ)
SOAK_RUN_DIR="${SOAK_RUN_ROOT}/${SOAK_STAMP}-${CLUSTER_NAME}"
# DRILL_RUN_DIR is what lib.sh's forensics helper writes into.
DRILL_RUN_DIR="${SOAK_RUN_DIR}"
( umask 077; mkdir -p "${SOAK_RUN_DIR}" )

SOAK_TIMELINE="${SOAK_RUN_DIR}/timeline.jsonl"
SOAK_DAEMON_LOG="${SOAK_RUN_DIR}/daemon.log"
SOAK_CONSOLE="${SOAK_RUN_DIR}/console.log"
SOAK_SUMMARY="${SOAK_RUN_DIR}/summary.md"
SOAK_LOG_CHILD_PIDFILE="${SOAK_RUN_DIR}/.logs-pid"

# ── Timeline ─────────────────────────────────────────────────────────

# One JSON object per line, always with `at` and `kind`. JSONL rather
# than a table because the sampler's shape and the injector's shape have
# nothing in common, and a run that is read months later should not
# depend on a column order nobody wrote down.
soak_emit() {
    local kind="$1" payload="$2"
    jq -cn --arg at "$(date -Is)" --arg kind "${kind}" --argjson p "${payload}" \
        '{at: $at, kind: $kind} + $p' >> "${SOAK_TIMELINE}"
}

soak_note() {
    printf '[%s] %s\n' "$(date -Is)" "$*" | tee -a "${SOAK_CONSOLE}"
}

# ── Tunnel ───────────────────────────────────────────────────────────
#
# `drill_port_forward` is written for a run that lasts minutes and is
# watched by a person: it starts one tunnel and calls `drill_die` if
# anything goes wrong. Neither half survives contact with eight
# unattended hours. A `kubectl port-forward` whose API-server stream has
# dropped keeps the port BOUND without serving it — lib.sh says so
# itself — so the failure is silent, every hub reading after it is
# empty, and the morning's timeline looks exactly like a daemon that
# stopped doing anything. That is the worst possible failure for this
# harness: it fabricates the very finding A1 is looking for.
#
# So the tunnel is probed with a real request before each sample and
# rebuilt when it stops answering, and the rebuild is recorded in the
# timeline so a gap is attributable rather than mysterious. Two
# consecutive failures, not one, because a single 30s hiccup during a
# pod roll is normal and reconnecting through it would be churn.
SOAK_PF_FAILS=0

soak_restart_tunnel() {
    drill_stop_port_forward || true
    sleep 2
    kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" \
        port-forward svc/core-agent "${DRILL_PORT}:7777" \
        >>"${SOAK_RUN_DIR}/port-forward.log" 2>&1 &
    DRILL_PF_PID=$!
    local i
    for i in $(seq 1 20); do
        kill -0 "${DRILL_PF_PID}" 2>/dev/null || break
        if hub_get "/healthz" >/dev/null 2>&1; then
            soak_note "tunnel restored"
            soak_emit tunnel_up '{}'
            return 0
        fi
        sleep 1
    done
    soak_note "WARNING: tunnel did not come back up; retrying at the next probe"
    soak_emit tunnel_down '{}'
    return 1
}

soak_ensure_tunnel() {
    if hub_get "/healthz" >/dev/null 2>&1; then
        SOAK_PF_FAILS=0
        return 0
    fi
    SOAK_PF_FAILS=$(( SOAK_PF_FAILS + 1 ))
    soak_note "tunnel probe failed (${SOAK_PF_FAILS} in a row)"
    (( SOAK_PF_FAILS >= 2 )) || return 0
    soak_emit tunnel_restart "$(jq -cn --argjson fails "${SOAK_PF_FAILS}" '{fails: $fails}')"
    SOAK_PF_FAILS=0
    soak_restart_tunnel || true
}

# ── Health sampling ──────────────────────────────────────────────────

# The timestamp every sample measures "sessions this run drove"
# against. Set once, at start, because the alternative — sampling all
# of them — does not scale: this cluster's daemon already holds 133
# sessions from earlier drill batches, and following each one into two
# more requests on every tick would be 266 calls a minute against the
# thing under test. The soak must not be the load that breaks it.
SOAK_SINCE=""

# The comparison SOAK_SINCE is used for is lexicographic, which is only
# equivalent to a chronological one while both sides are UTC in the same
# layout. If the hub ever starts emitting a numeric offset instead of a
# trailing Z, string compare would quietly select the wrong sessions and
# every per-session column in the run would be empty or wrong — so fail
# loudly here rather than discover it in the morning.
soak_set_since() {
    local probe
    probe=$(hub_get "/sessions" 2>/dev/null \
        | jq -r '[.sessions[]?.last_touched_at] | map(select(. != null)) | first // ""' 2>/dev/null || true)
    if [[ -n "${probe}" && "${probe}" != *Z ]]; then
        drill_die "hub last_touched_at is '${probe}', not UTC/Z — the soak's session filter assumes Z."
    fi
    SOAK_SINCE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    soak_note "counting sessions touched at or after ${SOAK_SINCE}"
}

# One sample of everything that can move. Deliberately all read-only and
# all from outside the agent: the hub's own endpoints, and kubectl. A
# sampler that asked the agent how it was doing would be asking the
# thing under test to grade itself.
#
# Per-session detail is fetched only for sessions touched since the run
# started. For those, /usage and /guardrails together answer three of
# A1's four questions directly — `turns` rising without an incident is
# a double-drive, `input_tokens` rising monotonically with nothing in
# the daemon log is compaction not firing, and `halted` is a session
# that has stopped rather than degraded. That is a far better growth
# signal than process RSS, which mostly measures the Go heap's
# willingness to return pages.
soak_sample() {
    local k="kubectl --context ${KUBE_CONTEXT} -n ${DEMO_NS}"
    local pod mem_mi cpu restarts ready sessions healthz

    pod=$(${k} get pods -l app.kubernetes.io/name=core-agent -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    restarts=$(${k} get pods -l app.kubernetes.io/name=core-agent \
        -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}' 2>/dev/null || echo "")
    ready=$(${k} get deploy core-agent -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "")

    # `kubectl top`, not /proc. The published image is distroless: it
    # has no shell, so `kubectl exec … sh -c 'grep VmRSS'` fails with
    # "executable file not found" and silently records an empty column
    # for the whole night. metrics-server is standard on GKE and this
    # reads through the same API the HPA does.
    mem_mi=""; cpu=""
    if [[ -n "${pod}" ]]; then
        read -r cpu mem_mi < <(${k} top pod "${pod}" --no-headers 2>/dev/null \
            | awk '{print $2, $3}') || true
    fi

    # The hub's own view. `|| true` on each: an unreachable hub is a
    # sample worth recording as empty, not a reason to end the run.
    healthz=$(hub_get "/healthz" 2>/dev/null || true)
    sessions=$(hub_get "/sessions" 2>/dev/null || true)

    local total=0 active_json='[]'
    if [[ -n "${sessions}" ]]; then
        total=$(printf '%s' "${sessions}" | jq '.sessions | length' 2>/dev/null || echo 0)
        local ids
        ids=$(printf '%s' "${sessions}" | jq -r --arg since "${SOAK_SINCE}" \
            '.sessions[]? | select(.last_touched_at >= $since) | .sessionID' 2>/dev/null || true)
        # `window` is the one that answers A1, and it is NOT
        # overall.input_tokens: that figure is the sum over every turn,
        # so it only ever rises and a compaction is invisible in it. The
        # last per-turn input count IS the context occupancy at that
        # moment, so a successful compaction shows up as a cliff and a
        # disabled one as a straight line to the ceiling. window_max is
        # carried alongside because a cliff is only legible next to the
        # height it fell from.
        local rows=() sid usage guards
        while IFS= read -r sid; do
            [[ -n "${sid}" ]] || continue
            usage=$(hub_get "/sessions/${DRILL_APP}/${sid}/usage" 2>/dev/null || echo '{}')
            guards=$(hub_get "/sessions/${DRILL_APP}/${sid}/guardrails" 2>/dev/null || echo '{}')
            rows+=("$(jq -cn --arg id "${sid}" \
                --argjson s "$(printf '%s' "${sessions}" | jq -c --arg id "${sid}" \
                    '.sessions[] | select(.sessionID == $id)' 2>/dev/null || echo '{}')" \
                --argjson u "$(printf '%s' "${usage}" | jq -c '.' 2>/dev/null || echo '{}')" \
                --argjson g "$(printf '%s' "${guards}" | jq -c '.' 2>/dev/null || echo '{}')" \
                '{id: $id, status: $s.status, touched: $s.last_touched_at,
                  turns: $u.overall.turns, input_tokens: $u.overall.input_tokens,
                  cost_usd: $u.overall.cost_usd,
                  window: ($u.per_turn // [] | last | .input_tokens),
                  window_max: ([$u.per_turn // [] | .[] | .input_tokens] | max),
                  halted: $g.halted, watchdog_tripped: $g.watchdog.tripped,
                  ceiling_tripped: $g.cost_ceiling.tripped}')")
        done <<< "${ids}"
        if (( ${#rows[@]} > 0 )); then
            active_json=$(printf '%s\n' "${rows[@]}" | jq -cs '.')
        fi
    fi

    local healthz_json='null'
    if [[ -n "${healthz}" ]]; then
        healthz_json=$(printf '%s' "${healthz}" | jq -c '.' 2>/dev/null || echo 'null')
    fi

    soak_emit sample "$(jq -cn \
        --arg pod "${pod}" --arg mem "${mem_mi}" --arg cpu "${cpu}" \
        --arg restarts "${restarts}" --arg ready "${ready}" \
        --argjson total "${total:-0}" --argjson active "${active_json}" \
        --argjson healthz "${healthz_json}" \
        '{pod: $pod, mem: $mem, cpu: $cpu, restarts: $restarts, ready: $ready,
          session_count: $total, active_count: ($active | length),
          active: $active, healthz: $healthz}')"
}

# ── Scenario dispatch ────────────────────────────────────────────────

# The soak reuses the drill's scenario scripts rather than growing its
# own injectors. They are the thing that has been run against this
# cluster dozens of times, and a second copy of "how to break
# emailservice" is a second copy to get wrong.
soak_load_scenario() {
    case "$1" in
        a|A) SCENARIO_FILE="a-bad-image.sh" ;;
        b|B) SCENARIO_FILE="b-oom.sh" ;;
        c|C) SCENARIO_FILE="c-rbac-denied.sh" ;;
        *) drill_die "unknown scenario ${1} (want a, b or c)" ;;
    esac
    # shellcheck source=/dev/null
    source "${SOAK_SELF_DIR}/scenarios/${SCENARIO_FILE}"
}

SOAK_BROKEN=""   # scenario id currently broken, "" when the cluster is clean

soak_restore_if_broken() {
    [[ -n "${SOAK_BROKEN}" ]] || return 0
    soak_note "restoring after scenario ${SOAK_BROKEN}"
    scenario_restore >>"${SOAK_CONSOLE}" 2>&1 || \
        soak_note "WARNING: restore for ${SOAK_BROKEN} reported an error; see console.log"
    SOAK_BROKEN=""
    soak_emit restore "$(jq -cn --arg s "${SCENARIO_ID:-?}" '{scenario: $s}')"
}

# ── Teardown ─────────────────────────────────────────────────────────

SOAK_LOG_PID=""
soak_cleanup() {
    local rc=$?
    trap - EXIT INT TERM
    soak_note "cleaning up (exit ${rc})"
    soak_restore_if_broken || true
    # Supervisor first, then the kubectl it spawned: the other order
    # lets the supervisor notice the dead stream and start a fresh
    # `logs -f` that outlives the run.
    if [[ -n "${SOAK_LOG_PID}" ]]; then
        kill "${SOAK_LOG_PID}" 2>/dev/null || true
    fi
    if [[ -s "${SOAK_LOG_CHILD_PIDFILE}" ]]; then
        kill "$(cat "${SOAK_LOG_CHILD_PIDFILE}")" 2>/dev/null || true
        rm -f "${SOAK_LOG_CHILD_PIDFILE}"
    fi
    drill_stop_port_forward || true
    soak_summarize || true
    soak_note "artifacts: ${SOAK_RUN_DIR}"
}
trap soak_cleanup EXIT INT TERM

# ── Summary ──────────────────────────────────────────────────────────

# Deliberately descriptive. It counts things and quotes lines; it does
# not decide whether A1 passed. The run note in runs/ is written by a
# person from this, the same division of labour SCORECARD.md has.
soak_summarize() {
    local started ended samples incidents
    started=$(head -1 "${SOAK_TIMELINE}" 2>/dev/null | jq -r '.at' 2>/dev/null || echo "?")
    ended=$(tail -1 "${SOAK_TIMELINE}" 2>/dev/null | jq -r '.at' 2>/dev/null || echo "?")
    samples=$(grep -c '"kind":"sample"' "${SOAK_TIMELINE}" 2>/dev/null || echo 0)
    incidents=$(grep -c '"kind":"break"' "${SOAK_TIMELINE}" 2>/dev/null || echo 0)

    {
        printf '# Soak run %s\n\n' "${SOAK_STAMP}"
        printf -- '- cluster: `%s` / project `%s`\n' "${CLUSTER_NAME}" "${PROJECT_ID}"
        printf -- '- namespaces: agent `%s`, target `%s`, workload `%s`\n' \
            "${DEMO_NS}" "${TARGET_NS}" "${WORKLOAD}"
        printf -- '- window: %s → %s\n' "${started}" "${ended}"
        printf -- '- requested: %sh, incident every %ss, sample every %ss\n\n' \
            "${SOAK_HOURS}" "${SOAK_CYCLE_SECS}" "${SOAK_PROBE_SECS}"
        printf '%s samples, %s incidents injected.\n\n' "${samples}" "${incidents}"

        printf '## Process\n\n'
        printf '| at | cpu | mem | restarts | ready | sessions | active |\n'
        printf '|---|---|---|---|---|---|---|\n'
        jq -r 'select(.kind=="sample")
               | "| \(.at) | \(.cpu // "?") | \(.mem // "?") | \(.restarts // "?") "
                 + "| \(.ready // "?") | \(.session_count) | \(.active_count) |"' \
            "${SOAK_TIMELINE}" 2>/dev/null || true

        # The per-session table, not the process one, is where A1 is
        # read. input_tokens is the context measure; turns is the
        # double-drive measure; halted is the wedge measure.
        #
        # `// "?"` is deliberately NOT used for the booleans: in jq,
        # `false // "?"` is "?", so every healthy session would have
        # rendered its halted column as unknown — the one column where
        # "false" is the whole point. `tostring` on a null gives the
        # string "null", which is the honest rendering of a missing
        # reading and is distinguishable from both true and false.
        printf '\n## Sessions this run touched\n\n'
        printf '| at | session | status | turns | window | window_max | cum_in | cost_usd | halted | tripped |\n'
        printf '|---|---|---|---|---|---|---|---|---|---|\n'
        jq -r 'select(.kind=="sample") as $s
               | $s.active[]?
               | "| \($s.at) | \(.id) | \(.status // "?") | \(.turns // "?") "
                 + "| \(.window // "?") | \(.window_max // "?") "
                 + "| \(.input_tokens // "?") | \(.cost_usd // "?") "
                 + "| \(.halted | tostring) "
                 + "| \(((.watchdog_tripped == true) or (.ceiling_tripped == true)) | tostring) |"' \
            "${SOAK_TIMELINE}" 2>/dev/null || true

        printf '\n## Context reduction in the daemon log\n\n'
        printf 'Every line is quoted; an empty section means none were written.\n\n```\n'
        grep -E 'context-reduction|compaction|window-unknown|mechanical|turn-cut' \
            "${SOAK_DAEMON_LOG}" 2>/dev/null || true
        printf '```\n'

        # `\b429\b` rather than `429`: session ids are hex UUIDs and one
        # of them contained "429", which quoted a "session created" line
        # into the retry section of the first rehearsal's summary. A
        # section whose job is to be read literally must not cry wolf.
        printf '\n## Provider retries and guardrails in the daemon log\n\n```\n'
        grep -E 'retry|RESOURCE_EXHAUSTED|(^|[^0-9a-f])429([^0-9a-f]|$)|watchdog|cost ceiling|guardrail' \
            "${SOAK_DAEMON_LOG}" 2>/dev/null || true
        printf '```\n'

        printf '\n## Read this before writing the run note\n\n'
        printf 'The soak asserts nothing. For box A1 the four questions are:\n\n'
        printf '1. **Wedged session** — does any session sit in `working`, or `halted`,\n'
        printf '   across a whole quiet gap between two incidents? Sessions table.\n'
        printf '2. **Silently-disabled compaction** — read the `window` column, not\n'
        printf '   `cum_in`. A compaction is a cliff in `window`. A session whose\n'
        printf '   `window` climbs to its threshold in a straight line, with no\n'
        printf '   compaction line and no `context-reduction-*` row above, is the\n'
        printf '   failure. A degraded row is a PASS for A1 and a reason to look at\n'
        printf '   that session. Note a successful compaction logs nothing, so the\n'
        printf '   cliff is the only witness for the healthy path.\n'
        printf '3. **Double-drive** — did `turns` rise for a session between two samples\n'
        printf '   with no incident injected for it in between?\n'
        printf '4. **Unbounded growth** — two columns. `mem` above: trending or\n'
        printf '   oscillating? And `window_max` per session: does any session end the\n'
        printf '   night with a ceiling it never came down from? One number at the end\n'
        printf '   proves nothing; the shape is the evidence.\n'
    } > "${SOAK_SUMMARY}"
    soak_note "summary → ${SOAK_SUMMARY}"
}

# ── Run ──────────────────────────────────────────────────────────────

drill_banner "SOAK — ${SOAK_HOURS}h unattended, cluster ${CLUSTER_NAME}"

drill_preflight
drill_load_token

# `kubectl top` is the only source of the memory column, and the sampler
# treats a failure as an empty cell on purpose — one unlucky scrape must
# not end an eight-hour run. That tolerance is exactly what makes an
# absent metrics-server dangerous: EVERY cell comes back empty, nothing
# says why, and "does anything leak" — one of A1's four questions — goes
# quietly unanswered for the whole night. Same shape as the distroless
# `kubectl exec` defect the sampler comment above describes; it is
# checked once, here, where the answer costs a second instead of a day.
# Two different failures have to be caught here and only one of them is
# an error exit. A missing metrics-server exits non-zero; a label that
# matches no pod exits ZERO and writes "No resources found" — to stderr.
# So stderr is kept OUT of the captured value: folding it in with `2>&1`
# makes the no-pod case look like a healthy reading, which is the same
# false pass this whole check exists to prevent. It is reported, though,
# because the two cases want different fixes.
soak_require_metrics() {
    local probe err
    err=$(mktemp)
    probe=$(kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" top pod \
        -l app.kubernetes.io/name=core-agent --no-headers 2>"${err}") || probe=""
    if [[ -z "${probe}" ]]; then
        local why
        why=$(tr '\n' ' ' <"${err}")
        rm -f "${err}"
        drill_die "kubectl top returned no row for core-agent in ${DEMO_NS} (${why:-no output}) — without it the memory column is empty for the whole run."
    fi
    rm -f "${err}"
    soak_note "metrics-server answering: ${probe}"
}
soak_require_metrics

drill_port_forward

soak_note "run dir ${SOAK_RUN_DIR}"

# The daemon log for the whole run, with timestamps. #1039's finding was
# that retries are invisible in transcripts and visible only here, so
# this is not a nice-to-have: without it, half of what A1 asks about
# cannot be answered afterwards. `--since=1s` so the file starts at the
# run rather than replaying the pod's history.
#
# Supervised rather than a bare `logs -f`, because `logs -f` ENDS when
# the pod it attached to goes away — and a restart is precisely one of
# the four things A1 is watching for. An unsupervised stream would go
# quiet at the exact moment the log became interesting, and the quiet
# would be indistinguishable from a healthy night. The reconnect marker
# is written into the log so a gap is visible rather than seamless.
soak_follow_daemon() {
    while :; do
        kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" logs -f --timestamps \
            --since=1s deploy/core-agent >>"${SOAK_DAEMON_LOG}" 2>&1 &
        # The kubectl pid goes to a file, not to a variable: cleanup
        # runs in the parent shell and cannot see this subshell's
        # locals. The first version relied on `pkill -P <supervisor>`
        # and left a `kubectl logs -f` running after the rehearsal
        # exited — by the time cleanup got there the supervisor was
        # already dead and the child had been reparented, so there was
        # no -P to match.
        echo $! >"${SOAK_LOG_CHILD_PIDFILE}"
        wait $! || true
        printf '%s soak: --- log stream ended, reconnecting ---\n' \
            "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"${SOAK_DAEMON_LOG}"
        sleep 5
    done
}
soak_follow_daemon &
SOAK_LOG_PID=$!
soak_note "daemon log → ${SOAK_DAEMON_LOG} (pid ${SOAK_LOG_PID})"

soak_emit start "$(jq -cn \
    --arg cluster "${CLUSTER_NAME}" --arg project "${PROJECT_ID}" \
    --arg demo_ns "${DEMO_NS}" --arg target_ns "${TARGET_NS}" \
    --arg workload "${WORKLOAD}" --arg hours "${SOAK_HOURS}" \
    --arg image "$(kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" get deploy core-agent \
        -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo '?')" \
    '{cluster: $cluster, project: $project, demo_ns: $demo_ns, target_ns: $target_ns,
      workload: $workload, hours: $hours, image: $image}')"

soak_set_since
soak_sample

# awk, not bc: SOAK_HOURS is allowed to be fractional (0.2 for a
# rehearsal) so plain shell arithmetic will not do, and `bc` is absent
# from a stock Cloud Shell / slim container — which is where this was
# first run, and it silently produced a deadline of zero and a "run"
# that ended three seconds after it started. awk is in POSIX and is
# already a hard dependency of lib.sh's curl version check.
SOAK_WINDOW_SECS=$(awk -v h="${SOAK_HOURS}" 'BEGIN { printf "%d", h * 3600 }')
(( SOAK_WINDOW_SECS > 0 )) || drill_die "SOAK_HOURS=${SOAK_HOURS} resolves to a zero-length window."
SOAK_DEADLINE=$(( SECONDS + SOAK_WINDOW_SECS ))
SOAK_NEXT_BREAK=${SECONDS}
SOAK_NEXT_PROBE=$(( SECONDS + SOAK_PROBE_SECS ))
SOAK_RESTORE_AT=0
read -r -a SOAK_ROTATION <<< "${SOAK_SCENARIOS}"
SOAK_CYCLE=0

while (( SECONDS < SOAK_DEADLINE )); do
    if (( SECONDS >= SOAK_NEXT_PROBE )); then
        soak_ensure_tunnel
        soak_sample
        SOAK_NEXT_PROBE=$(( SECONDS + SOAK_PROBE_SECS ))
    fi

    if [[ -n "${SOAK_BROKEN}" ]] && (( SECONDS >= SOAK_RESTORE_AT )); then
        soak_restore_if_broken
    fi

    if [[ -z "${SOAK_BROKEN}" ]] && (( SECONDS >= SOAK_NEXT_BREAK )); then
        scen="${SOAK_ROTATION[$(( SOAK_CYCLE % ${#SOAK_ROTATION[@]} ))]}"
        SOAK_CYCLE=$(( SOAK_CYCLE + 1 ))
        soak_load_scenario "${scen}"
        soak_note "cycle ${SOAK_CYCLE}: breaking with scenario ${SCENARIO_ID} (${SCENARIO_NAME})"
        # A scenario that fails to arm is a cluster problem, not a
        # reason to abandon seven remaining hours: record it and let the
        # next cycle try a different one.
        if scenario_break >>"${SOAK_CONSOLE}" 2>&1; then
            SOAK_BROKEN="${SCENARIO_ID}"
            SOAK_RESTORE_AT=$(( SECONDS + SOAK_HOLD_SECS ))
            soak_emit break "$(jq -cn --arg s "${SCENARIO_ID}" --arg n "${SCENARIO_NAME}" \
                --argjson cycle "${SOAK_CYCLE}" '{scenario: $s, name: $n, cycle: $cycle}')"
        else
            soak_note "WARNING: scenario ${SCENARIO_ID} failed to break; skipping this cycle"
            soak_emit break_failed "$(jq -cn --arg s "${SCENARIO_ID}" '{scenario: $s}')"
        fi
        SOAK_NEXT_BREAK=$(( SECONDS + SOAK_CYCLE_SECS ))
    fi

    sleep 10
done

soak_note "window elapsed"
soak_sample
soak_emit end '{}'
