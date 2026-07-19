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

# setup-wif.sh — configure GKE Workload Identity Federation direct-binding
# IAM for the core-agent daemon's KSA in the gke-troubleshoot-agent recipe.
#
# What it does (in order):
#
#   1. Enables the four GCP APIs the recipe requires:
#        - container.googleapis.com   (GKE + GKE MCP)
#        - aiplatform.googleapis.com  (Vertex AI / Gemini)
#        - iamcredentials.googleapis.com  (WIF token-exchange path)
#        - cloudtrace.googleapis.com  (OpenTelemetry → Cloud Trace; harmless
#                                       if the OTel overlay is never applied)
#
#   2. Binds five IAM roles the daemon needs:
#        - roles/aiplatform.user               (call Gemini via Vertex API)
#        - roles/mcp.toolUser                  (call GKE MCP tools at all)
#        - roles/container.admin               (read + write cluster/workload state via MCP)
#        - roles/iam.serviceAccountUser        (impersonate node SA — required by
#                                                GKE MCP's server-side chain; missing
#                                                this gives 403 with no clear hint)
#        - roles/cloudtrace.user               (write spans to Cloud Trace when the
#                                                OTel overlay is applied; harmless
#                                                permission grant if never used)
#
# All bindings use WIF-for-GKE direct binding — no Google Service Account
# impersonation for the KSA itself. The `principal://` member string names
# the KSA directly. See:
#   https://docs.cloud.google.com/kubernetes-engine/docs/how-to/workload-identity#authenticating_to
#
# Usage:
#   ./scripts/setup-wif.sh [PROJECT_ID]
#
# Environment overrides:
#   PROJECT_ID     — GCP project ID. Falls back to `gcloud config get-value project`
#                    if not set as arg or env.
#   NAMESPACE      — K8s namespace holding the daemon KSA. Default: agent-triage.
#   KSA_NAME       — Kubernetes ServiceAccount name. Default: core-agent-daemon.
#   NODE_SA        — Node service account (compute-engine default SA unless the
#                    cluster uses a custom node SA). Default:
#                    ${PROJECT_NUMBER}-compute@developer.gserviceaccount.com.
#   DRY_RUN        — Set to "true" to print gcloud commands without executing.
#                    Useful for auditing before running against a real project.
#
# Idempotent: re-runs are safe. Existing bindings are left in place.
#
# Prereqs on the operator: roles/container.admin + roles/iam.serviceAccountAdmin
# on the project (needed to grant the bindings this script creates).

set -euo pipefail

# ANSI styling when stdout is a TTY.
if [[ -t 1 ]]; then
    GREEN=$'\033[0;32m'
    RED=$'\033[0;31m'
    YELLOW=$'\033[1;33m'
    BLUE=$'\033[0;34m'
    RESET=$'\033[0m'
else
    GREEN=""; RED=""; YELLOW=""; BLUE=""; RESET=""
fi

log_info()    { printf '%s[INFO]%s    %s\n' "${BLUE}"   "${RESET}" "$1"; }
log_success() { printf '%s[SUCCESS]%s %s\n' "${GREEN}"  "${RESET}" "$1"; }
log_warn()    { printf '%s[WARN]%s    %s\n' "${YELLOW}" "${RESET}" "$1"; }
log_error()   { printf '%s[ERROR]%s   %s\n' "${RED}"    "${RESET}" "$1" >&2; }

# 1. Prerequisite: gcloud installed.
if ! command -v gcloud >/dev/null 2>&1; then
    log_error "gcloud CLI is not installed. Install the Google Cloud SDK and try again."
    exit 1
fi

# 2. Resolve PROJECT_ID from arg → env → active gcloud config, in that order.
PROJECT_ID="${1:-${PROJECT_ID:-}}"
if [[ -z "${PROJECT_ID}" ]]; then
    log_info "No PROJECT_ID specified; attempting to detect from active gcloud config..."
    PROJECT_ID=$(gcloud config get-value project 2>/dev/null || true)
    if [[ -z "${PROJECT_ID}" ]]; then
        log_error "Could not detect active gcloud project. Pass PROJECT_ID as arg 1 or set PROJECT_ID env var."
        echo "Usage: $0 [PROJECT_ID]" >&2
        exit 1
    fi
fi

# 3. Config with sensible defaults matching the recipe's manifests.
NAMESPACE="${NAMESPACE:-agent-triage}"
KSA_NAME="${KSA_NAME:-core-agent-daemon}"
DRY_RUN="${DRY_RUN:-false}"

log_info "Configuring GKE Workload Identity Federation for the recipe daemon KSA:"
echo "  GCP Project:      ${PROJECT_ID}"
echo "  K8s Namespace:    ${NAMESPACE}"
echo "  K8s SA Name:      ${KSA_NAME}"

# 4. Fetch (or accept override) the project number for the WIF principal URI.
#    Override via `PROJECT_NUMBER=...` when testing the script without a
#    real project to describe (typically alongside DRY_RUN=true).
if [[ -z "${PROJECT_NUMBER:-}" ]]; then
    log_info "Fetching project number for '${PROJECT_ID}'..."
    PROJECT_NUMBER=$(gcloud projects describe "${PROJECT_ID}" --format='value(projectNumber)' 2>/dev/null || true)
    if [[ -z "${PROJECT_NUMBER}" ]]; then
        log_error "Failed to retrieve project number for '${PROJECT_ID}'. Verify the project ID and your gcloud permissions, or set PROJECT_NUMBER explicitly (useful with DRY_RUN=true)."
        exit 1
    fi
else
    log_info "Using PROJECT_NUMBER override: ${PROJECT_NUMBER}"
fi
echo "  Project Number:   ${PROJECT_NUMBER}"

# 5. Node service account default — Compute Engine's default node SA. Override
#    NODE_SA if your cluster uses a custom node service account.
NODE_SA="${NODE_SA:-${PROJECT_NUMBER}-compute@developer.gserviceaccount.com}"
echo "  Node SA:          ${NODE_SA}"

# 6. Construct the WIF direct-binding member string. Note the two-part
#    identifier convention: PROJECT_NUMBER in the pool path, PROJECT_ID in
#    the pool name.
KSA_PRINCIPAL="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/${NAMESPACE}/sa/${KSA_NAME}"
echo "  KSA Principal:"
echo "    ${KSA_PRINCIPAL}"
echo

if [[ "${DRY_RUN}" == "true" ]]; then
    log_warn "=== DRY RUN MODE: no changes will be applied ==="
    echo
fi

# ---- Helpers ----

# Enable a single GCP API. Idempotent: gcloud services enable is a no-op
# when already enabled.
enable_api() {
    local api="$1"
    log_info "Enabling API: ${api}"
    local cmd="gcloud services enable ${api} --project=${PROJECT_ID}"
    if [[ "${DRY_RUN}" == "true" ]]; then
        echo "  [DRY RUN] Would run: ${cmd}"
    else
        eval "${cmd}" >/dev/null
        log_success "API enabled: ${api}"
    fi
}

# Bind a project-scoped IAM role to the KSA principal.
bind_project_role() {
    local role="$1"
    log_info "Binding project role: ${role}"
    local cmd="gcloud projects add-iam-policy-binding ${PROJECT_ID} \
--role=${role} \
--member=${KSA_PRINCIPAL} \
--condition=None \
--quiet"
    if [[ "${DRY_RUN}" == "true" ]]; then
        echo "  [DRY RUN] Would run: ${cmd}"
    else
        eval "${cmd}" >/dev/null
        log_success "Bound: ${role}"
    fi
}

# Bind iam.serviceAccountUser on a specific service account (not project-scoped).
# Required so the daemon KSA can impersonate the node SA through the
# GKE MCP's server-side chain. Missing this gives 403 with no clear hint.
bind_sa_role() {
    local sa="$1"
    local role="roles/iam.serviceAccountUser"
    log_info "Binding ${role} on service account: ${sa}"
    local cmd="gcloud iam service-accounts add-iam-policy-binding ${sa} \
--role=${role} \
--member=${KSA_PRINCIPAL} \
--quiet"
    if [[ "${DRY_RUN}" == "true" ]]; then
        echo "  [DRY RUN] Would run: ${cmd}"
    else
        eval "${cmd}" >/dev/null
        log_success "Bound ${role} on ${sa}"
    fi
}

# ---- Phase 1: enable APIs ----

log_info "=== Phase 1: enabling required GCP APIs ==="
enable_api "container.googleapis.com"
enable_api "aiplatform.googleapis.com"
enable_api "iamcredentials.googleapis.com"
# Cloud Trace API: needed by the optional OTel overlay (deploy/overlays/
# example-otel/). Enabling it here is idempotent and free — no spans are
# emitted unless the overlay is applied, so this is harmless for
# operators who never use OTel.
enable_api "cloudtrace.googleapis.com"
echo

# ---- Phase 2: bind IAM roles ----

log_info "=== Phase 2: binding IAM roles to the KSA principal ==="

# 2a. Vertex AI (Gemini via Vertex API).
bind_project_role "roles/aiplatform.user"

# 2b. Permission to call any GKE MCP tool at all. WITHOUT this: silent 403
#     on the MCP call with no useful error hint about what's wrong.
bind_project_role "roles/mcp.toolUser"

# 2c. GKE cluster + workload administration. The full-access GKE MCP
#     endpoint (`/mcp`) exercises admin-level operations for some fixes
#     (rollout undo, deployment patches, node cordon/drain, etc.).
bind_project_role "roles/container.admin"

# 2d. Impersonate the node service account. Required by the GKE MCP's
#     server-side chain. Bound on the SA resource, not the project.
bind_sa_role "${NODE_SA}"

# 2e. Write spans to Cloud Trace. Load-bearing only when the OTel overlay
#     is applied AND `--managed-otel-scope=COLLECTION_AND_INSTRUMENTATION_COMPONENTS`
#     has been run against the cluster. Otherwise the role is inert — no
#     spans get emitted, so the permission never gets exercised.
bind_project_role "roles/cloudtrace.user"

echo

# ---- Summary ----

if [[ "${DRY_RUN}" == "true" ]]; then
    log_warn "=== DRY RUN complete: no changes were applied ==="
else
    log_success "=== Setup complete: WIF bindings are active ==="
    echo
    echo "The core-agent daemon's KSA can now:"
    echo "  - Call Gemini via the Vertex AI API"
    echo "  - Call the GKE MCP server + its tools"
    echo "  - Administer GKE clusters + workloads"
    echo "  - Impersonate the node SA (required by GKE MCP)"
    echo "  - Write spans to Cloud Trace (used by the OTel overlay only)"
    echo
    echo "Next step: kubectl apply -k examples/gke-troubleshoot-agent/deploy/overlays/<your-overlay>"
    echo
    echo "Bindings applied:"
    echo "  - roles/aiplatform.user        on projects/${PROJECT_ID}"
    echo "  - roles/mcp.toolUser           on projects/${PROJECT_ID}"
    echo "  - roles/container.admin        on projects/${PROJECT_ID}"
    echo "  - roles/cloudtrace.user        on projects/${PROJECT_ID}"
    echo "  - roles/iam.serviceAccountUser on ${NODE_SA}"
    echo "  All bound to member: ${KSA_PRINCIPAL}"
fi
