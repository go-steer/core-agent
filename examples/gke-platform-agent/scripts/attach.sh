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

# Attach to the gke-platform-agent hub with the TUI as the operator
# (platform-oncall). Port-forwards the core-agent Service :7777 to
# localhost, then launches core-agent-tui. Given the bare hub URL the TUI
# runs its built-in session picker (and /switch lets you hop sessions);
# pass a session id to jump straight in.
#
# Sessions on this hub are created by the lookout-watch's incident injects
# (per-incident mode) — break a workload with ./scripts/break-workload.sh
# first, then attach to the session it spawns. Use --new to open a fresh
# operator-owned session instead; that is also how you pose the GENERAL
# prompts this recipe exists to answer well ("how would you rate your
# performance?"), which need no incident at all.
#
# Usage:
#   ./scripts/attach.sh                 # hub picker (choose a session in the TUI)
#   ./scripts/attach.sh <sid>           # jump to <sid> (default app)
#   ./scripts/attach.sh <app> <sid>     # jump to an explicit app + session id
#   ./scripts/attach.sh --new           # create + attach a fresh session
#
# LOCAL_PORT defaults to 7778, not 7777, so this forward does not collide
# with another recipe's already holding the conventional port.
set -euo pipefail

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
source "${SCRIPT_DIR}/prereqs.sh"
require_coordinates || exit 1

# PLATFORM_TOKEN. Written by gen-tokens.sh under RIG_STATE_DIR, which is
# outside the checkout precisely because this is a live bearer token for
# the hub. Guarded rather than sourced blind: under `set -u` a missing
# file aborts with "No such file or directory" and no hint about which
# step was skipped.
if [[ ! -f "${RIG_STATE_DIR}/demo-tokens.env" ]]; then
    echo "✗ ${RIG_STATE_DIR}/demo-tokens.env not found — run ./scripts/gen-tokens.sh first." >&2
    exit 1
fi
source "${RIG_STATE_DIR}/demo-tokens.env"

command -v core-agent-tui >/dev/null || { echo "✗ core-agent-tui not on PATH (go install github.com/go-steer/core-agent/v2/cmd/core-agent-tui@latest)"; exit 1; }

LOCAL_PORT="${LOCAL_PORT:-7778}"
BASE_URL="http://127.0.0.1:${LOCAL_PORT}"
export PLATFORM_TOKEN                 # passed by NAME so it never hits argv/ps

# For the explicit two-arg form the app is "core-agent", NOT the recipe's
# agent.display_name. Sessions on this hub register under the former;
# following display_name into `attach.sh gke-platform-agent <sid>` gets a
# 404. The picker and the one-arg form never need it.

# ── Port-forward the Service in the background; clean up on exit ──────
#
# The readiness probe below is a bare TCP connect, which cannot tell OUR
# tunnel from anyone else's socket on the same port. That distinction
# matters: a `kubectl port-forward` whose API-server stream has dropped
# keeps running and keeps holding the port without serving it. Our
# kubectl then dies on "bind: address already in use", the TCP probe
# succeeds against the corpse, and the TUI hangs until it reports
# `context deadline exceeded` — pointing at the daemon, which is fine.
# Observed 2026-08-19 with a 2h51m-old half-dead forward.
#
# So: refuse to start if the port is taken, and treat our kubectl exiting
# as fatal rather than racing on regardless.
PF_LOG=/tmp/gke-platform-agent-pf.log

if (exec 3<>"/dev/tcp/127.0.0.1/${LOCAL_PORT}") 2>/dev/null; then
    echo "✗ 127.0.0.1:${LOCAL_PORT} is already in use — not starting a second tunnel."
    echo "  Something is holding the port. If it is a stale port-forward, it may"
    echo "  be alive but no longer serving; check and clear it with:"
    echo
    echo "      ss -ltnp | grep ${LOCAL_PORT}"
    echo "      pkill -f 'port-forward svc/core-agent ${LOCAL_PORT}:7777'"
    echo
    echo "  Or attach over a different port:  LOCAL_PORT=7878 $0 $*"
    exit 1
fi

echo "→ port-forwarding svc/core-agent ${LOCAL_PORT}:7777 (ns ${DEMO_NS})"
kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" \
    port-forward svc/core-agent "${LOCAL_PORT}:7777" >"${PF_LOG}" 2>&1 &
PF_PID=$!
trap 'kill "${PF_PID}" 2>/dev/null || true' EXIT

# Wait for OUR tunnel to accept connections, aborting the moment kubectl
# exits — otherwise a bind failure costs 10s of silence and then a
# misleading TUI error.
pf_ready=""
for _ in $(seq 1 20); do
    if ! kill -0 "${PF_PID}" 2>/dev/null; then
        echo "✗ kubectl port-forward exited before the tunnel came up:"
        sed 's/^/    /' "${PF_LOG}"
        exit 1
    fi
    if (exec 3<>"/dev/tcp/127.0.0.1/${LOCAL_PORT}") 2>/dev/null; then
        pf_ready=1
        break
    fi
    sleep 0.5
done
if [[ -z "${pf_ready}" ]]; then
    echo "✗ port-forward did not become ready within 10s. Last output:"
    sed 's/^/    /' "${PF_LOG}"
    exit 1
fi

# Common flags: bearer auth against the users.json table; label the banner.
TUI=(core-agent-tui --token=PLATFORM_TOKEN --auth=bearer --alias="${ADMIN_IDENTITY}")

# Run the TUI in the FOREGROUND, never via exec.
#
# exec replaces this shell, and a replaced shell runs no traps — so the
# EXIT handler above would never fire and the background port-forward
# would be orphaned to init on every single attach. Those orphans are
# what makes the next attach fail: kubectl port-forward can lose its
# API-server stream and stop serving while the process lives on, so the
# port looks taken but answers nothing. Every attach leaked one before
# this change (2026-08-19: a 2h51m-old orphan on 7778).
#
# Staying in the foreground costs one shell process and buys correct
# cleanup on quit, Ctrl-C, and error alike.
run_tui() {
    "${TUI[@]}" "$@"
    local rc=$?
    # Trap fires on the way out and reaps the tunnel.
    exit "${rc}"
}

if [[ "${1:-}" == "--new" ]]; then
    echo "→ opening a fresh session as ${ADMIN_IDENTITY}"
    run_tui --new-session "${BASE_URL}"
fi

if [[ $# -ge 2 ]]; then
    echo "→ attaching to ${1}/${2} as ${ADMIN_IDENTITY}"
    run_tui "${BASE_URL}/sessions/${1}/${2}"
elif [[ $# -eq 1 ]]; then
    echo "→ attaching to ${1} as ${ADMIN_IDENTITY}"
    run_tui "${BASE_URL}/sessions/${1}"
fi

echo "→ opening the hub picker as ${ADMIN_IDENTITY}"
run_tui "${BASE_URL}"
