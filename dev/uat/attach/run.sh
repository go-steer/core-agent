#!/usr/bin/env bash
# UAT helper for attach-mode + peer-registration + attach-config
# (docs/attach-mode-design.md, docs/peer-registration-design.md, PR #13).
#
# Spins up agents in tmux panes so the operator can watch live
# event streams and inject from a separate pane. All state lives
# under /tmp/core-agent-uat/ — never $HOME — per the project's
# UAT-files-in-/tmp convention.
#
# Commands:
#   ./run.sh build               # build core-agent once into /tmp
#   ./run.sh solo                # Session A: single agent + attach
#   ./run.sh hub                 # standalone hub (no peers)
#   ./run.sh peer NAME PORT      # single peer registered to local hub
#   ./run.sh fleet [N]           # Session B: hub + N peers (default 2)
#   ./run.sh config-hub          # Session C: hub from fixtures/ config
#   ./run.sh tail [URL]          # core-agent attach against URL (or hub)
#   ./run.sh ls   [URL]          # core-agent ls against URL (or hub)
#   ./run.sh status              # what's running under tmux
#   ./run.sh clean               # kill tmux session + remove /tmp dir
#
# Requires: tmux, jq, a built Go toolchain.
# Default model: --provider=echo (no creds needed). Override with
# MODEL=vertex / MODEL=anthropic-vertex etc. plus the matching env
# (see the bundled CLI's provider docs).

set -euo pipefail

UAT_ROOT="/tmp/core-agent-uat"
BIN="${UAT_ROOT}/bin/core-agent"
SESS="core-agent-uat"
HUB_PORT="${HUB_PORT:-7777}"
PEER_PORT_BASE="${PEER_PORT_BASE:-7780}"
MODEL_PROVIDER="${MODEL:-echo}"
# Bearer-token secret. Lives in env only — never in config files.
# Matches the PR #13 "token_env" indirection: config holds the
# *name* (ATTACH_TOKEN); the secret is in this var.
ATTACH_TOKEN="${ATTACH_TOKEN:-uat-token-$(whoami 2>/dev/null || echo nobody)}"
export ATTACH_TOKEN

REPO_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"

die()  { echo "uat: $*" >&2; exit 1; }
log()  { echo "uat: $*" >&2; }

require_tmux() {
    command -v tmux >/dev/null 2>&1 \
        || die "tmux not installed; install it and re-run (UAT is interactive — panes matter)"
}

ensure_dirs() {
    mkdir -p "${UAT_ROOT}/bin" "${UAT_ROOT}/db" "${UAT_ROOT}/sock" "${UAT_ROOT}/agents"
}

ensure_built() {
    ensure_dirs
    if [[ ! -x "${BIN}" ]] || [[ "${REPO_ROOT}/cmd/core-agent/main.go" -nt "${BIN}" ]]; then
        log "building ${BIN} from ${REPO_ROOT}"
        (cd "${REPO_ROOT}" && go build -o "${BIN}" ./cmd/core-agent)
    fi
}

# tmux helpers — open SESS if missing, create panes by name.
tmux_ensure_session() {
    if ! tmux has-session -t "${SESS}" 2>/dev/null; then
        tmux new-session -d -s "${SESS}" -n shell
        tmux send-keys -t "${SESS}:shell" "cd ${REPO_ROOT}" C-m
        tmux send-keys -t "${SESS}:shell" "echo 'UAT session up. Use \`./dev/uat/attach/run.sh tail|ls\` from here.'" C-m
    fi
}

tmux_new_window() {
    local name="$1"; shift
    local cmd="$*"
    tmux new-window -t "${SESS}" -n "${name}" "bash -lc '${cmd//\'/\'\\\'\'}; echo; echo \"[${name} exited — press enter to close]\"; read'"
}

# Each agent runs in REPL mode (no -p) so it stays alive for
# attach/inject. echo provider replies trivially; the live-tail
# value is observing the event stream, not the model output.
spawn_agent() {
    local name="$1"; shift
    local extra_flags="$*"
    local agent_dir="${UAT_ROOT}/agents/${name}"
    mkdir -p "${agent_dir}"
    local db="${UAT_ROOT}/db/${name}.db"
    local cmd="cd ${agent_dir} && ATTACH_TOKEN='${ATTACH_TOKEN}' '${BIN}' --provider=${MODEL_PROVIDER} --session-db --session-db-path='${db}' --attach-token=ATTACH_TOKEN ${extra_flags}"
    log "spawn ${name}: ${extra_flags}"
    tmux_new_window "${name}" "${cmd}"
}

cmd_build() {
    ensure_built
    log "binary at ${BIN}"
}

cmd_solo() {
    require_tmux
    ensure_built
    tmux_ensure_session
    spawn_agent "solo" \
        "--attach-listen=:${HUB_PORT}"
    sleep 1
    log "ready. From another shell: ./run.sh ls  /  ./run.sh tail"
    log "attach to tmux:  tmux attach -t ${SESS}"
}

cmd_hub() {
    require_tmux
    ensure_built
    tmux_ensure_session
    spawn_agent "hub" \
        "--attach-listen=:${HUB_PORT} --attach-peer-hub"
    sleep 1
    log "hub up on http://localhost:${HUB_PORT}"
    log "attach to tmux:  tmux attach -t ${SESS}"
}

cmd_peer() {
    require_tmux
    ensure_built
    tmux_ensure_session
    local name="${1:-peer-1}"
    local port="${2:-${PEER_PORT_BASE}}"
    spawn_agent "${name}" \
        "--attach-listen=:${port} --attach-register-to=http://localhost:${HUB_PORT} --attach-register-endpoint=http://localhost:${port} --attach-register-name=${name}"
    sleep 1
    log "peer ${name} on :${port} registered with hub :${HUB_PORT}"
}

cmd_fleet() {
    local n="${1:-2}"
    cmd_hub
    sleep 1
    for ((i=1; i<=n; i++)); do
        cmd_peer "peer-${i}" "$((PEER_PORT_BASE + i - 1))"
    done
    sleep 1
    log "fleet up: hub + ${n} peers. Inspect:  ./run.sh ls"
}

cmd_config_hub() {
    require_tmux
    ensure_built
    tmux_ensure_session
    # Fixture-driven: every flag comes from .agents/config.json.
    # Pre-pod env so register_endpoint / register_name templates resolve.
    local fixtures="${REPO_ROOT}/dev/uat/attach/fixtures"
    local agent_dir="${UAT_ROOT}/agents/config-hub"
    mkdir -p "${agent_dir}/.agents"
    cp "${fixtures}/hub-config.json" "${agent_dir}/.agents/config.json"
    local db="${UAT_ROOT}/db/config-hub.db"
    # Note: we still pass --session-db on CLI because (a) it isn't
    # an attach flag and (b) the session-db PATH belongs in /tmp
    # not in the committed fixture.
    local cmd="cd ${agent_dir} && \
        POD_IP=127.0.0.1 HOSTNAME_OVERRIDE=config-hub-pod ATTACH_TOKEN='${ATTACH_TOKEN}' \
        '${BIN}' --session-db --session-db-path='${db}'"
    tmux_new_window "config-hub" "${cmd}"
    sleep 1
    log "config-hub up — verify with:  ./run.sh ls"
    log "(every --attach-* flag is sourced from fixtures/hub-config.json)"
}

cmd_tail() {
    ensure_built
    local url="${1:-http://localhost:${HUB_PORT}}"
    log "core-agent attach ${url}"
    ATTACH_TOKEN="${ATTACH_TOKEN}" "${BIN}" attach --token=ATTACH_TOKEN "${url}"
}

cmd_ls() {
    ensure_built
    local url="${1:-http://localhost:${HUB_PORT}}"
    ATTACH_TOKEN="${ATTACH_TOKEN}" "${BIN}" ls --token=ATTACH_TOKEN "${url}"
}

cmd_inject() {
    ensure_built
    local url="${1:?usage: inject <url> <message>}"
    shift
    local msg="$*"
    curl -sS -X POST \
        -H "Authorization: Bearer ${ATTACH_TOKEN}" \
        -H 'Content-Type: application/json' \
        -d "{\"message\": $(printf '%s' "${msg}" | jq -Rs .)}" \
        "${url}" \
        && echo
}

cmd_status() {
    if ! tmux has-session -t "${SESS}" 2>/dev/null; then
        echo "no tmux session ${SESS} — nothing running"
        return
    fi
    echo "tmux session ${SESS} windows:"
    tmux list-windows -t "${SESS}" -F '  #{window_index}: #{window_name}'
    echo
    echo "attach to inspect:  tmux attach -t ${SESS}"
}

cmd_clean() {
    if tmux has-session -t "${SESS}" 2>/dev/null; then
        log "killing tmux session ${SESS}"
        tmux kill-session -t "${SESS}"
    fi
    if [[ -d "${UAT_ROOT}" ]]; then
        log "removing ${UAT_ROOT}"
        rm -rf "${UAT_ROOT}"
    fi
}

usage() {
    sed -n '3,28p' "$0"
    exit 2
}

main() {
    local cmd="${1:-}"; shift || true
    case "${cmd}" in
        build)        cmd_build "$@" ;;
        solo)         cmd_solo "$@" ;;
        hub)          cmd_hub "$@" ;;
        peer)         cmd_peer "$@" ;;
        fleet)        cmd_fleet "$@" ;;
        config-hub)   cmd_config_hub "$@" ;;
        tail)         cmd_tail "$@" ;;
        ls)           cmd_ls "$@" ;;
        inject)       cmd_inject "$@" ;;
        status)       cmd_status "$@" ;;
        clean)        cmd_clean "$@" ;;
        ""|-h|--help) usage ;;
        *)            die "unknown command: ${cmd} (try --help)" ;;
    esac
}

main "$@"
