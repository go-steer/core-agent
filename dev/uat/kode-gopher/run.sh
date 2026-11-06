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

# UAT helper for examples/kode-gopher (#1391) against a local kind cluster.
# All state is written under /tmp/core-agent-uat-kode-gopher (never $HOME).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
RECIPE_DIR="${REPO_ROOT}/examples/kode-gopher"
UAT_ROOT="${UAT_ROOT:-/tmp/core-agent-uat-kode-gopher}"
BIN="${UAT_ROOT}/bin/core-agent"
KUBECONFIG_PATH="${UAT_ROOT}/kubeconfig"
KIND_CLUSTER="${KIND_CLUSTER:-kode-gopher-uat}"

die() { echo "kode-gopher-uat: $*" >&2; exit 1; }
log() { echo "kode-gopher-uat: $*" >&2; }

ensure_built() {
    mkdir -p "${UAT_ROOT}/bin" "${UAT_ROOT}/work"
    cp -R "${RECIPE_DIR}/.agents" "${UAT_ROOT}/work/"
    if [[ ! -x "${BIN}" ]] || [[ "${REPO_ROOT}/cmd/core-agent/main.go" -nt "${BIN}" ]]; then
        log "building ${BIN}"
        (cd "${REPO_ROOT}" && go build -o "${BIN}" ./cmd/core-agent)
    fi
}

cmd_dry_run() {
    ensure_built
    log "running offline recipe + recipecheck verification"
    (cd "${REPO_ROOT}" && go test ./examples/kode-gopher/... && go test ./examples/internal/recipecheck -run TestEverySkillNamedToolIsReachable/kode-gopher)
    log "verifying pinned recipe config via echo one-shot (without live kode-gopher binary)"
    mkdir -p "${UAT_ROOT}/home"
    rm -f "${UAT_ROOT}/work/.agents/mcp.json"
    HOME="${UAT_ROOT}/home" "${BIN}" -c "${UAT_ROOT}/work/.agents/config.json" --no-pricing-refresh --provider=echo -p "ok" >/dev/null
    log "dry-run OK"
}

cmd_up() {
    command -v kind >/dev/null 2>&1 || die "kind not found on PATH"
    command -v kubectl >/dev/null 2>&1 || die "kubectl not found on PATH"
    mkdir -p "${UAT_ROOT}"
    if ! kind get clusters | grep -qx "${KIND_CLUSTER}"; then
        kind create cluster --name "${KIND_CLUSTER}" --kubeconfig "${KUBECONFIG_PATH}"
    else
        kind export kubeconfig --name "${KIND_CLUSTER}" --kubeconfig "${KUBECONFIG_PATH}"
    fi
    KUBECONFIG="${KUBECONFIG_PATH}" kubectl create namespace demo --dry-run=client -o yaml | KUBECONFIG="${KUBECONFIG_PATH}" kubectl apply -f -
    KUBECONFIG="${KUBECONFIG_PATH}" kubectl -n demo create deployment web --image=nginx:latest --dry-run=client -o yaml | KUBECONFIG="${KUBECONFIG_PATH}" kubectl apply -f -
    log "kind cluster ${KIND_CLUSTER} ready (kubeconfig at ${KUBECONFIG_PATH})"
}

cmd_smoke() {
    command -v kode-gopher >/dev/null 2>&1 || die "kode-gopher not found on PATH (go install github.com/gke-demos/kode-gopher/cmd/kode-gopher@latest)"
    ensure_built
    export KUBECONFIG="${KUBECONFIG_PATH}"
    cd "${UAT_ROOT}/work"
    "${BIN}" -c "${UAT_ROOT}/work/.agents/config.json" \
        -p "Use kode_gopher_lookup_package_docs to inspect k8s.io/api/apps/v1.DeploymentList, then summarize its items field."
}

cmd_down() {
    if command -v kind >/dev/null 2>&1 && kind get clusters | grep -qx "${KIND_CLUSTER}"; then
        kind delete cluster --name "${KIND_CLUSTER}"
    fi
    rm -rf "${UAT_ROOT}"
    log "cleaned up ${KIND_CLUSTER} and ${UAT_ROOT}"
}

case "${1:-dry-run}" in
    dry-run|--dry-run) cmd_dry_run ;;
    up)                cmd_up ;;
    smoke)             cmd_smoke ;;
    down)              cmd_down ;;
    *)                 die "usage: $0 {dry-run|up|smoke|down}" ;;
esac
