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
# (#1283): the running-tasks bar in core-agent's in-process TUI, and in
# core-agent-tui attached to a --no-repl daemon. See README.md.
#
# Commands:
#   ./run.sh build             # build core-agent + core-agent-tui into /tmp
#   ./run.sh local             # path 1: core-agent with its in-process TUI
#   ./run.sh headless          # path 2: --no-repl daemon + core-agent-tui
#   ./run.sh poke "TEXT"       # set status.txt's first line (both paths)
#   ./run.sh agents            # GET /agents from the headless daemon (raw JSON)
#   ./run.sh status            # what's running under tmux
#   ./run.sh clean             # kill the tmux session, remove /tmp state
#
# Needs a real model: the mock providers can't call spawn_agent. Defaults
# to MODEL_PROVIDER=vertex MODEL_NAME=gemini-3.7-flash, which read
# GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION from the environment.
# All state lives under /tmp/core-agent-uat-wakes/, never $HOME.

set -euo pipefail

UAT_ROOT="/tmp/core-agent-uat-wakes"
BIN="${UAT_ROOT}/bin/core-agent"
TUI_BIN="${UAT_ROOT}/bin/core-agent-tui"
SESS="core-agent-uat-wakes"
PORT="${PORT:-7791}"
MODEL_PROVIDER="${MODEL_PROVIDER:-vertex}"
MODEL_NAME="${MODEL_NAME:-gemini-3.7-flash}"
WAKE_SECS="${WAKE_SECS:-40}"
TOKEN_FILE="${UAT_ROOT}/attach-token"

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"

die() { echo "uat: $*" >&2; exit 1; }
log() { echo "uat: $*" >&2; }

require() {
    for c in "$@"; do
        command -v "$c" >/dev/null 2>&1 || die "$c not installed"
    done
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

tmux_window() {
    local name="$1" cmd="$2"
    if ! tmux has-session -t "${SESS}" 2>/dev/null; then
        tmux new-session -d -s "${SESS}" -n "${name}"
    else
        tmux kill-window -t "${SESS}:${name}" 2>/dev/null || true
        tmux new-window -t "${SESS}" -n "${name}"
    fi
    tmux send-keys -t "${SESS}:${name}" "${cmd}" C-m
}

cmd_local() {
    require tmux
    ensure_built
    setup_workdir local
    local dir="${UAT_ROOT}/local"
    tmux_window local "cd ${dir} && ${BIN} -c ${dir}/.agents/config.json --session-db --session-db-path ${dir}/sessions.db --log-file ${dir}/core-agent.log"
    log "local TUI is in tmux window ${SESS}:local — tmux attach -t ${SESS}"
}

cmd_headless() {
    require tmux curl jq
    ensure_built
    ensure_token
    setup_workdir headless
    local dir="${UAT_ROOT}/headless"
    tmux_window daemon "cd ${dir} && ${BIN} -c ${dir}/.agents/config.json --no-repl --attach-listen 127.0.0.1:${PORT} --attach-token-file ${TOKEN_FILE} --session-db-path ${dir}/sessions.db --log-file ${dir}/core-agent.log"
    log "waiting for the daemon on 127.0.0.1:${PORT}"
    for _ in $(seq 1 60); do
        if curl -fsS -o /dev/null -H "Authorization: Bearer $(cat "${TOKEN_FILE}")" "http://127.0.0.1:${PORT}/sessions" 2>/dev/null; then
            break
        fi
        sleep 0.5
    done
    curl -fsS -o /dev/null -H "Authorization: Bearer $(cat "${TOKEN_FILE}")" "http://127.0.0.1:${PORT}/sessions" \
        || die "daemon never answered; see ${dir}/core-agent.log or the ${SESS}:daemon window"
    # Attach to the daemon's own session directly. The bare URL opens a
    # picker whose cursor starts on "+ New session".
    tmux_window tui "cd ${dir} && ${TUI_BIN} --token-file ${TOKEN_FILE} http://127.0.0.1:${PORT}/sessions/$(daemon_sid)"
    log "core-agent-tui is in tmux window ${SESS}:tui — tmux attach -t ${SESS}"
}

daemon_auth() { echo "Authorization: Bearer $(cat "${TOKEN_FILE}")"; }

daemon_sid() {
    local sid
    sid="$(curl -fsS -H "$(daemon_auth)" "http://127.0.0.1:${PORT}/sessions" | jq -r '.sessions[0].sessionID // empty')"
    [[ -n "${sid}" ]] || die "the daemon reports no session"
    echo "${sid}"
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
    [[ -s "${TOKEN_FILE}" ]] || die "no headless daemon set up; run ./run.sh headless"
    curl -fsS -H "$(daemon_auth)" "http://127.0.0.1:${PORT}/sessions/$(daemon_sid)/agents" | jq .
}

cmd_status() {
    tmux list-windows -t "${SESS}" 2>/dev/null || echo "no ${SESS} tmux session"
}

cmd_clean() {
    tmux kill-session -t "${SESS}" 2>/dev/null || true
    rm -rf "${UAT_ROOT}"
    log "removed ${UAT_ROOT}"
}

case "${1:-}" in
    build) build ;;
    local) cmd_local ;;
    headless) cmd_headless ;;
    poke) shift; cmd_poke "$@" ;;
    agents) cmd_agents ;;
    status) cmd_status ;;
    clean) cmd_clean ;;
    *) sed -n '16,33p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
