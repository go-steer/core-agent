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

# Shared plumbing for the GKE drill (#970). `source` this from drill.sh
# and the scenario scripts; on its own it runs nothing against a
# cluster and asserts nothing.
#
# The drill deliberately owns no coordinates of its own. Everything
# about WHICH cluster, WHICH namespaces and WHICH identities comes from
# the recipe's own scripts/prereqs.sh, so a drill run cannot silently
# score a different deployment than the one the operator set up. The
# only thing configured here is what the drill itself needs: where to
# put artifacts, how long to wait, and which local port to tunnel on.

set -euo pipefail

DRILL_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
REPO_ROOT=$( cd -- "${DRILL_DIR}/../../.." &> /dev/null && pwd )
DRILL_RECIPE_DIR="${DRILL_RECIPE_DIR:-${REPO_ROOT}/examples/gke-platform-agent}"

[[ -f "${DRILL_RECIPE_DIR}/scripts/prereqs.sh" ]] || {
    echo "✗ recipe not found at ${DRILL_RECIPE_DIR}" >&2
    echo "  The drill scores examples/gke-platform-agent. Set DRILL_RECIPE_DIR to" >&2
    echo "  point it elsewhere, but note the scenarios assume that recipe's rig." >&2
    exit 1
}

# Tool check BEFORE sourcing prereqs.sh, not with the rest of preflight
# below. prereqs.sh does `export PROJECT_ID="${PROJECT_ID:-$(gcloud …)}"`,
# and under `set -e` an export whose command substitution fails aborts
# the shell — so a missing gcloud kills the drill during the source with
# no message at all. The message is the point.
for _t in gcloud kubectl curl jq python3; do
    command -v "${_t}" >/dev/null 2>&1 || {
        echo "✗ ${_t} is not on PATH — the drill needs gcloud, kubectl, curl, jq and python3." >&2
        exit 1
    }
done
unset _t

# prereqs.sh computes RECIPE_ROOT from SCRIPT_DIR, and honours an
# already-set SCRIPT_DIR ("${SCRIPT_DIR:-...}"). Ours points at the
# drill, so hand it the recipe's instead — otherwise RECIPE_ROOT
# resolves to dev/uat and every path built from it is wrong, quietly.
SCRIPT_DIR="${DRILL_RECIPE_DIR}/scripts"
# shellcheck source=/dev/null
source "${DRILL_RECIPE_DIR}/scripts/prereqs.sh"
unset SCRIPT_DIR

# The workload the scenarios break. prereqs.sh does not set it —
# break-workload.sh defaults it itself — so the drill has to, or the
# scenario scripts read an unset variable under `set -u`. Same default,
# and passed explicitly to break-workload.sh so the two cannot diverge.
export WORKLOAD="${WORKLOAD:-emailservice}"

# ── Drill knobs ──────────────────────────────────────────────────────

# The app name sessions register under on the hub. NOT the recipe's
# agent.display_name — following that into a URL gets a 404 (the same
# trap scripts/attach.sh documents).
DRILL_APP="${DRILL_APP:-core-agent}"

# 7779, not attach.sh's 7778. G6 requires a human attached with the TUI
# *while the drill runs*, so the two tunnels must not collide.
DRILL_PORT="${DRILL_PORT:-7779}"
DRILL_BASE_URL="http://127.0.0.1:${DRILL_PORT}"

# How long to wait for the watcher to turn the broken workload into an
# incident session. lookout batches events; a minute is normal, three
# is the outer edge of normal on a cold watcher.
DRILL_SESSION_TIMEOUT="${DRILL_SESSION_TIMEOUT:-300}"

# The turn is "over" when the event stream has been quiet this long.
# There is no reliable terminal frame to wait for: turn-complete is a
# typed frame, and typed frames are never replayed, so a stream that
# joined late would hang forever waiting for one. Quiescence is the
# only signal that works from any join point.
DRILL_IDLE_SECS="${DRILL_IDLE_SECS:-90}"

# Hard cap on one scenario's capture, whatever the stream is doing.
DRILL_MAX_SECS="${DRILL_MAX_SECS:-1200}"

# Artifacts. Under TMPDIR by convention — a drill run captures a live
# transcript and a live bearer token's worth of context, and neither
# belongs in $HOME or in the checkout. The SCORECARD is the only thing
# that gets copied into the repo, by hand, by the operator.
DRILL_RUN_ROOT="${DRILL_RUN_ROOT:-${TMPDIR:-/tmp}/gke-drill}"

# ── Output helpers ───────────────────────────────────────────────────

drill_log()  { printf '→ %s\n' "$*"; }
drill_ok()   { printf '✓ %s\n' "$*"; }
drill_warn() { printf '⚠ %s\n' "$*" >&2; }
drill_die()  { printf '✗ %s\n' "$*" >&2; exit 1; }

drill_banner() {
    printf '\n'
    printf '════════════════════════════════════════════════════════════════\n'
    printf '  %s\n' "$*"
    printf '════════════════════════════════════════════════════════════════\n'
}

# ── Preflight ────────────────────────────────────────────────────────

# --fail-with-body is curl 7.76+ (2021). The drill uses it rather than
# -f because a 4xx from the hub carries the reason in the body, and
# "curl: (22)" with the reason discarded is how a five-minute auth
# mistake becomes a half-hour one.
drill_require_curl_version() {
    local ver
    ver=$(curl --version | head -1 | awk '{print $2}')
    curl --help all 2>/dev/null | grep -q -- '--fail-with-body' \
        || drill_die "curl ${ver} has no --fail-with-body (needs 7.76+)."
}

# The hub token, written by the recipe's gen-tokens.sh into
# RIG_STATE_DIR (outside the checkout, on purpose).
#
# It is loaded into a curl CONFIG FILE rather than passed as -H, because
# a header on the command line is visible in `ps` to every user on the
# box for the whole life of the request. The config file is 0600 and
# lives beside the token it quotes.
DRILL_CURL_CFG=""
drill_load_token() {
    local envfile="${RIG_STATE_DIR}/demo-tokens.env"
    [[ -f "${envfile}" ]] || drill_die \
        "${envfile} not found — run the recipe's ./scripts/gen-tokens.sh first."
    # shellcheck source=/dev/null
    source "${envfile}"
    [[ -n "${PLATFORM_TOKEN:-}" ]] || drill_die "PLATFORM_TOKEN is empty in ${envfile}"

    DRILL_CURL_CFG="${RIG_STATE_DIR}/drill-curl.cfg"
    ( umask 077; printf 'header = "Authorization: Bearer %s"\n' "${PLATFORM_TOKEN}" \
        > "${DRILL_CURL_CFG}" )
}

# Refuse to score a deployment that is not actually up. Every one of
# these has been a real wasted cluster hour at some point: a daemon
# that never went Ready, a watcher that CrashLooped on a bad pin, a
# second recipe's watcher stealing the incident.
drill_preflight() {
    drill_require_curl_version
    require_coordinates || exit 1

    local k="kubectl --context ${KUBE_CONTEXT}"

    ${k} get ns "${DEMO_NS}" >/dev/null 2>&1 \
        || drill_die "namespace ${DEMO_NS} not found — deploy the recipe first (./scripts/set-up-demo.sh)."
    ${k} get ns "${TARGET_NS}" >/dev/null 2>&1 \
        || drill_die "target namespace ${TARGET_NS} not found — nothing to break."

    local ready
    ready=$(${k} -n "${DEMO_NS}" get deploy core-agent -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
    [[ "${ready:-0}" -ge 1 ]] || drill_die "deploy/core-agent in ${DEMO_NS} has no ready replica."
    ready=$(${k} -n "${DEMO_NS}" get deploy lookout-watch -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
    [[ "${ready:-0}" -ge 1 ]] || drill_die \
        "deploy/lookout-watch in ${DEMO_NS} has no ready replica — no watcher, no incident, nothing to score."

    # A foreign watcher does not break the drill, it CORRUPTS it: both
    # watchers see the same cluster-wide event and whichever injects
    # first owns the incident, so the transcript you score may belong
    # to a different daemon running different content.
    if ! warn_foreign_watchers; then
        [[ "${FORCE:-}" == "1" ]] || drill_die \
            "a foreign watcher will race for this incident — quiesce it, or set FORCE=1 to score anyway."
        drill_warn "FORCE=1 — proceeding with a foreign watcher active. Note it on the scorecard."
    fi
}

# ── Port-forward ─────────────────────────────────────────────────────
#
# Same shape as the recipe's attach.sh, and for the same reason: a
# kubectl port-forward whose API-server stream has dropped keeps the
# port bound without serving it, so a bare TCP probe cannot tell our
# tunnel from a corpse. Refuse a taken port; treat our kubectl exiting
# as fatal.

DRILL_PF_PID=""
drill_port_forward() {
    local log="${DRILL_RUN_DIR}/port-forward.log"

    if (exec 3<>"/dev/tcp/127.0.0.1/${DRILL_PORT}") 2>/dev/null; then
        drill_die "127.0.0.1:${DRILL_PORT} is already in use — not starting a second tunnel.
  If it is a stale forward:  pkill -f 'port-forward svc/core-agent ${DRILL_PORT}:7777'
  Or run on another port:    DRILL_PORT=7879 $0 $*"
    fi

    drill_log "port-forwarding svc/core-agent ${DRILL_PORT}:7777 (ns ${DEMO_NS})"
    kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" \
        port-forward svc/core-agent "${DRILL_PORT}:7777" >"${log}" 2>&1 &
    DRILL_PF_PID=$!

    local i
    for i in $(seq 1 20); do
        if ! kill -0 "${DRILL_PF_PID}" 2>/dev/null; then
            sed 's/^/    /' "${log}" >&2
            drill_die "kubectl port-forward exited before the tunnel came up."
        fi
        if (exec 3<>"/dev/tcp/127.0.0.1/${DRILL_PORT}") 2>/dev/null; then
            drill_ok "tunnel up on ${DRILL_BASE_URL}"
            return 0
        fi
        sleep 0.5
    done
    sed 's/^/    /' "${log}" >&2
    drill_die "port-forward did not become ready within 10s."
}

drill_stop_port_forward() {
    [[ -n "${DRILL_PF_PID}" ]] || return 0
    kill "${DRILL_PF_PID}" 2>/dev/null || true
    wait "${DRILL_PF_PID}" 2>/dev/null || true
    DRILL_PF_PID=""
}

# ── Hub API ──────────────────────────────────────────────────────────

hub_get() {
    curl -sS --fail-with-body -K "${DRILL_CURL_CFG}" "${DRILL_BASE_URL}$1"
}

# POSTs carry Content-Type: application/json and no Origin — the attach
# server's browser write guard 415s anything else.
hub_post() {
    curl -sS --fail-with-body -K "${DRILL_CURL_CFG}" \
        -H 'Content-Type: application/json' \
        -X POST --data "$2" "${DRILL_BASE_URL}$1"
}

# Session ids visible to the operator, newline-separated.
drill_session_ids() {
    hub_get "/sessions" | jq -r '.sessions[]?.sessionID' | sort
}

# Block until a session id appears that was not in $1 (a sorted list
# captured before the break). Prints the new id.
#
# If more than one appears, take the newest by last_touched_at and say
# so: a cluster that produced two incidents at once is scoreable, but
# the operator needs to know the transcript is one of several.
drill_wait_new_session() {
    local before="$1" deadline=$(( SECONDS + DRILL_SESSION_TIMEOUT ))
    local new count
    while (( SECONDS < deadline )); do
        new=$(comm -13 <(printf '%s\n' "${before}") <(drill_session_ids) || true)
        count=$(printf '%s' "${new}" | grep -c . || true)
        if (( count >= 1 )); then
            if (( count > 1 )); then
                drill_warn "${count} new sessions appeared; scoring the most recently touched. Note it on the scorecard."
            fi
            hub_get "/sessions" \
                | jq -r --argjson ids "$(printf '%s\n' "${new}" | jq -R . | jq -s .)" \
                    '[.sessions[] | select(.sessionID as $s | $ids | index($s))]
                     | sort_by(.last_touched_at) | last | .sessionID'
            return 0
        fi
        sleep 5
    done
    return 1
}

# ── Transcript capture ───────────────────────────────────────────────

# Stream the parent session's SSE from seq 0 into $DRILL_RUN_DIR, and
# stop when the stream has been quiet for DRILL_IDLE_SECS (or when
# DRILL_MAX_SECS is up).
#
# Writes two files:
#   events.sse        raw, exactly what the server sent
#   transcript.jsonl  one {"sse":<event name>,"data":<payload>} per frame
#
# The raw copy is kept deliberately. Everything downstream reads the
# JSONL, so if a scoring bug is suspected after the cluster is gone,
# the unparsed bytes are still there to re-derive it from.
drill_capture_parent() {
    local sid="$1"
    local raw="${DRILL_RUN_DIR}/events.sse"
    local jsonl="${DRILL_RUN_DIR}/transcript.jsonl"

    drill_log "streaming /sessions/${DRILL_APP}/${sid}/events?since=0"
    : > "${raw}"
    # curl's own diagnostics go to a separate file: events.sse is meant
    # to be exactly the bytes the server sent, and a scoring dispute is
    # not the moment to discover a transport error was interleaved into
    # the evidence.
    curl -sS -N --no-buffer -K "${DRILL_CURL_CFG}" \
        "${DRILL_BASE_URL}/sessions/${DRILL_APP}/${sid}/events?since=0" \
        >>"${raw}" 2>"${DRILL_RUN_DIR}/events.stderr" &
    local curl_pid=$!

    local started=${SECONDS} last_size=-1 quiet_since=${SECONDS} size
    while true; do
        sleep 5
        size=$(wc -c < "${raw}")
        if [[ "${size}" != "${last_size}" ]]; then
            last_size="${size}"
            quiet_since=${SECONDS}
        elif (( SECONDS - quiet_since >= DRILL_IDLE_SECS )) && (( size > 0 )); then
            drill_ok "stream quiet for ${DRILL_IDLE_SECS}s — turn looks finished"
            break
        fi
        if (( SECONDS - started >= DRILL_MAX_SECS )); then
            drill_warn "hit DRILL_MAX_SECS=${DRILL_MAX_SECS} with the stream still active — capture is TRUNCATED. Say so on the scorecard."
            break
        fi
        if ! kill -0 "${curl_pid}" 2>/dev/null; then
            drill_warn "the event stream closed on its own (server hung up or the tunnel dropped)."
            break
        fi
    done
    kill "${curl_pid}" 2>/dev/null || true
    wait "${curl_pid}" 2>/dev/null || true

    python3 "${DRILL_DIR}/sse2jsonl.py" < "${raw}" > "${jsonl}"
    drill_ok "$(wc -l < "${jsonl}" | tr -d ' ') frames -> ${jsonl}"
}

# Subagent turns are NOT on the parent stream — the attach server says
# so in as many words ("the parent-scoped /events SSE stream leave[s]
# unanswerable"). For this recipe that is where almost all the evidence
# is: the `cluster` subagent owns the gke MCP, so a drill that scored
# only the parent would see a diagnosis with no tool calls behind it
# and mark G1 failed on every run.
drill_capture_subagents() {
    local sid="$1"
    local out="${DRILL_RUN_DIR}/subagents.json"
    local names name

    # /subagents is the CONFIGURED roster; /agents is live instances.
    # Prefer the roster: by the time we pull, the instance is finished
    # and may already have been reaped from the in-memory list.
    names=$(hub_get "/sessions/${DRILL_APP}/${sid}/subagents" 2>/dev/null \
        | jq -r '.subagents[]?.name' || true)
    if [[ -z "${names}" ]]; then
        names=$(hub_get "/sessions/${DRILL_APP}/${sid}/agents" 2>/dev/null \
            | jq -r '.agents[]?.name' || true)
    fi
    if [[ -z "${names}" ]]; then
        drill_warn "no subagents resolved for ${sid} — scoring the parent stream alone."
        echo '{}' > "${out}"
        return 0
    fi

    echo '{}' > "${out}"
    while IFS= read -r name; do
        [[ -n "${name}" ]] || continue
        local body since=0 page merged='[]' truncated
        merged='[]'
        while true; do
            page=$(hub_get "/sessions/${DRILL_APP}/${sid}/agents/${name}/events?since=${since}&limit=500" 2>/dev/null || true)
            [[ -n "${page}" ]] || break
            # A name that resolves to nothing is a 404 with a body
            # listing what WOULD have resolved. Keep it: an empty
            # subagent capture and a misspelled name look identical
            # otherwise, which is exactly the #694 confusion.
            if [[ "$(jq -r 'has("error")' <<<"${page}")" == "true" ]]; then
                drill_warn "subagent '${name}': $(jq -r '.error' <<<"${page}")"
                break
            fi
            merged=$(jq -c --argjson m "${merged}" '$m + (.events // [])' <<<"${page}")
            truncated=$(jq -r '.truncated // false' <<<"${page}")
            [[ "${truncated}" == "true" ]] || break
            since=$(jq -r '.next_since' <<<"${page}")
        done
        body=$(jq -c --arg n "${name}" --argjson e "${merged}" '. + {($n): $e}' "${out}")
        printf '%s\n' "${body}" > "${out}"
        drill_ok "subagent ${name}: $(jq -r --arg n "${name}" '.[$n] | length' "${out}") frames"
    done <<< "${names}"
}

# ── G4 evidence: did anything actually change in the cluster? ────────
#
# The transcript can only show the tool calls the agent MADE. G4 is the
# stronger claim that no mutation LANDED, and the cluster is the only
# witness to that.
drill_target_generation() {
    kubectl --context "${KUBE_CONTEXT}" -n "${TARGET_NS}" \
        get deploy "${WORKLOAD}" -o jsonpath='{.metadata.generation}' 2>/dev/null || echo "?"
}

# Everything in TARGET_NS a remediation would plausibly touch, as a
# fingerprint that is stable while a workload is crash-looping.
#
# The two halves use different fields on purpose. A Deployment's
# resourceVersion moves on every STATUS write, and a scenario that
# leaves a pod backing off rewrites status every few seconds — so a
# resourceVersion fingerprint would report a mutation on every run and
# G4 would be noise. .metadata.generation moves only on a SPEC write,
# which is exactly the event G4 is about. The other kinds have no
# controller writing their status, so resourceVersion is precise there
# and catches an edit generation would not see.
drill_target_fingerprint() {
    local k="kubectl --context ${KUBE_CONTEXT} -n ${TARGET_NS}"
    # The trailing `|| true` is load-bearing under `set -e -o pipefail`:
    # `grep .` exits 1 on empty input, so a namespace holding none of
    # these kinds — or one transient kubectl failure — would take the
    # whole drill down at the point where a workload is already broken.
    # An empty fingerprint is a legitimate reading; a dead drill is not.
    {
        ${k} get deploy,statefulset,daemonset \
            -o jsonpath='{range .items[*]}{.kind}/{.metadata.name}:gen{.metadata.generation}{"\n"}{end}' 2>/dev/null
        ${k} get sa,role,rolebinding,cm,secret \
            -o jsonpath='{range .items[*]}{.kind}/{.metadata.name}:rv{.metadata.resourceVersion}{"\n"}{end}' 2>/dev/null
    } | grep . | sort || true
}
