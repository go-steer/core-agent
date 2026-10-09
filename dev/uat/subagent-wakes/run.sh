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

# UAT driver for the subagent roster's live report and scheduled wakes
# (#1283): the running-tasks bar in core-agent's embedded TUI, and in
# core-agent-tui attached to a --no-repl daemon. See README.md.
#
# Commands:
#   ./run.sh local             # embedded TUI: core-agent, in this terminal
#   ./run.sh headless          # remote TUI: --no-repl daemon in the background,
#                              #   core-agent-tui in this terminal; quitting
#                              #   the TUI stops the daemon
#   ./run.sh poke "TEXT"       # (another terminal) set status.txt's first line
#   ./run.sh agents            # (another terminal) GET /agents from the daemon
#   ./run.sh build             # rebuild both binaries into /tmp
#   ./run.sh clean             # stop a leftover daemon, remove /tmp state
#
# Needs a real model: the mock providers can't call spawn_agent. Put your
# settings in .env next to this script (copy env.example; it is
# git-ignored), or export them. All state lives under
# /tmp/core-agent-uat-wakes/, never $HOME.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"

die() { echo "uat: $*" >&2; exit 1; }
log() { echo "uat: $*" >&2; }

# load_env_file exports each KEY=VALUE in ${HERE}/.env that is not
# already set, so a variable exported in the shell wins over the file.
# Parsed rather than sourced: the file holds settings, not commands, and
# sourcing it would overwrite whatever the shell already exported.
load_env_file() {
    local f="${HERE}/.env" line key val n=0
    [[ -f "${f}" ]] || return 0
    while IFS= read -r line || [[ -n "${line}" ]]; do
        n=$((n + 1))
        [[ "${line}" =~ ^[[:space:]]*(#|$) ]] && continue
        line="${line#export }"
        [[ "${line}" =~ ^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*)=(.*)$ ]] \
            || die "${f}:${n}: want KEY=VALUE, got: ${line}"
        key="${BASH_REMATCH[1]}"
        val="${BASH_REMATCH[2]}"
        val="${val%\"}" val="${val#\"}" val="${val%\'}" val="${val#\'}"
        [[ -n "${!key:-}" ]] || export "${key}=${val}"
    done <"${f}"
}
load_env_file

UAT_ROOT="/tmp/core-agent-uat-wakes"
BIN="${UAT_ROOT}/bin/core-agent"
TUI_BIN="${UAT_ROOT}/bin/core-agent-tui"
PORT="${PORT:-7791}"
MODEL_PROVIDER="${MODEL_PROVIDER:-vertex}"
MODEL_NAME="${MODEL_NAME:-gemini-3.7-flash}"
WAKE_SECS="${WAKE_SECS:-40}"
TOKEN_FILE="${UAT_ROOT}/attach-token"
PID_FILE="${UAT_ROOT}/daemon.pid"

require() {
    for c in "$@"; do
        command -v "$c" >/dev/null 2>&1 || die "$c not installed"
    done
}

# require_model_env fails before anything starts when the chosen
# provider's project or key variable is missing.
require_model_env() {
    local where="export it, or set it in ${HERE}/.env (see env.example)"
    case "${MODEL_PROVIDER}" in
        anthropic-vertex)
            [[ -n "${ANTHROPIC_VERTEX_PROJECT_ID:-}" ]] \
                || die "MODEL_PROVIDER=anthropic-vertex needs ANTHROPIC_VERTEX_PROJECT_ID (and usually CLOUD_ML_REGION): ${where}" ;;
        vertex)
            [[ -n "${GOOGLE_CLOUD_PROJECT:-}" ]] \
                || die "MODEL_PROVIDER=vertex needs GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_LOCATION: ${where}" ;;
        anthropic)
            [[ -n "${ANTHROPIC_API_KEY:-}" ]] || die "MODEL_PROVIDER=anthropic needs ANTHROPIC_API_KEY: ${where}" ;;
        gemini)
            [[ -n "${GEMINI_API_KEY:-}${GOOGLE_API_KEY:-}" ]] || die "MODEL_PROVIDER=gemini needs GEMINI_API_KEY: ${where}" ;;
    esac
}

build() {
    require go
    mkdir -p "${UAT_ROOT}/bin"
    log "building core-agent and core-agent-tui from ${REPO_ROOT}"
    (cd "${REPO_ROOT}" && go build -o "${BIN}" ./cmd/core-agent && go build -o "${TUI_BIN}" ./cmd/core-agent-tui)
}

ensure_built() {
    [[ -x "${BIN}" && -x "${TUI_BIN}" ]] || build
}

# setup_workdir MODE renders the fixtures into a fresh working directory
# for one path. Each path gets its own, so its watcher, session DB and
# status.txt never mix with the other's.
setup_workdir() {
    local dir="${UAT_ROOT}/$1"
    rm -rf "${dir}"
    mkdir -p "${dir}/.agents"
    sed -e "s|@MODEL_PROVIDER@|${MODEL_PROVIDER}|" -e "s|@MODEL_NAME@|${MODEL_NAME}|" \
        "${HERE}/fixtures/config.json.tmpl" >"${dir}/.agents/config.json"
    sed -e "s|@WAKE_SECS@|${WAKE_SECS}|" "${HERE}/fixtures/cluster-watch.md" >"${dir}/cluster-watch.md"
    cp "${HERE}/fixtures/AGENTS.md" "${dir}/AGENTS.md"
    echo "all nodes Ready" >"${dir}/status.txt"
}

ensure_token() {
    if [[ ! -s "${TOKEN_FILE}" ]]; then
        mkdir -p "${UAT_ROOT}"
        (umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"${TOKEN_FILE}")
    fi
}

daemon_auth() { echo "Authorization: Bearer $(cat "${TOKEN_FILE}")"; }

daemon_up() {
    curl -fsS -o /dev/null -H "$(daemon_auth)" "http://127.0.0.1:${PORT}/sessions" 2>/dev/null
}

daemon_sid() {
    local sid
    sid="$(curl -fsS -H "$(daemon_auth)" "http://127.0.0.1:${PORT}/sessions" | jq -r '.sessions[0].sessionID // empty')"
    [[ -n "${sid}" ]] || die "the daemon reports no session"
    echo "${sid}"
}

stop_daemon() {
    [[ -f "${PID_FILE}" ]] || return 0
    local pid
    pid="$(cat "${PID_FILE}")"
    if kill -0 "${pid}" 2>/dev/null; then
        kill "${pid}" 2>/dev/null || true
        for _ in $(seq 1 20); do
            kill -0 "${pid}" 2>/dev/null || break
            sleep 0.25
        done
    fi
    rm -f "${PID_FILE}"
}

cmd_local() {
    require_model_env
    ensure_built
    setup_workdir local
    local dir="${UAT_ROOT}/local"
    log "starting core-agent's embedded TUI (Ctrl-D, or Ctrl-C twice, to quit). Log: ${dir}/core-agent.log"
    cd "${dir}"
    exec "${BIN}" -c "${dir}/.agents/config.json" --session-db --session-db-path "${dir}/sessions.db" --log-file "${dir}/core-agent.log"
}

cmd_headless() {
    require curl jq
    require_model_env
    ensure_built
    ensure_token
    stop_daemon
    setup_workdir headless
    local dir="${UAT_ROOT}/headless"
    log "starting the --no-repl daemon on 127.0.0.1:${PORT}. Log: ${dir}/core-agent.log"
    (cd "${dir}" && exec "${BIN}" -c "${dir}/.agents/config.json" --no-repl --attach-listen "127.0.0.1:${PORT}" \
        --attach-token-file "${TOKEN_FILE}" --session-db-path "${dir}/sessions.db" --log-file "${dir}/core-agent.log") \
        </dev/null >"${dir}/daemon.out" 2>&1 &
    echo $! >"${PID_FILE}"
    trap stop_daemon EXIT
    for _ in $(seq 1 60); do
        daemon_up && break
        kill -0 "$(cat "${PID_FILE}")" 2>/dev/null || die "the daemon exited at startup:
$(tail -5 "${dir}/daemon.out")"
        sleep 0.5
    done
    daemon_up || die "the daemon never answered; see ${dir}/daemon.out"
    # Attach to the daemon's own session directly. The bare URL opens a
    # picker whose cursor starts on "+ New session". Assigned on its own
    # line so a failed lookup stops the script (set -e ignores a failing
    # command substitution inside another command's arguments).
    local sid
    sid="$(daemon_sid)"
    log "attaching core-agent-tui (Ctrl-D, or Ctrl-C twice, to quit; that also stops the daemon)"
    cd "${dir}"
    "${TUI_BIN}" --token-file "${TOKEN_FILE}" "http://127.0.0.1:${PORT}/sessions/${sid}"
}

cmd_poke() {
    [[ $# -ge 1 ]] || die "usage: run.sh poke \"TEXT\""
    local hit=0
    for mode in local headless; do
        if [[ -d "${UAT_ROOT}/${mode}" ]]; then
            printf '%s\n' "$1" >"${UAT_ROOT}/${mode}/status.txt"
            log "${mode}: status.txt is now: $1"
            hit=1
        fi
    done
    [[ ${hit} -eq 1 ]] || die "no workdir yet; run local or headless first"
}

cmd_agents() {
    require curl jq
    [[ -s "${TOKEN_FILE}" ]] && daemon_up || die "no headless daemon running; start one with ./run.sh headless"
    local sid
    sid="$(daemon_sid)"
    curl -fsS -H "$(daemon_auth)" "http://127.0.0.1:${PORT}/sessions/${sid}/agents" | jq .
}

cmd_clean() {
    stop_daemon
    rm -rf "${UAT_ROOT}"
    log "removed ${UAT_ROOT}"
}

case "${1:-}" in
    build) build ;;
    local) cmd_local ;;
    headless) cmd_headless ;;
    poke) shift; cmd_poke "$@" ;;
    agents) cmd_agents ;;
    clean) cmd_clean ;;
    *) sed -n '16,33p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
