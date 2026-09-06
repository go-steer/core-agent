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

# Make a GCP project ready for this recipe: enable the APIs it calls and
# grant the Workload Identity bindings its two Kubernetes ServiceAccounts
# need. Idempotent — it reads first and only changes what is missing.
#
#     ./scripts/grant-iam.sh              # enable and grant what is missing
#     ./scripts/grant-iam.sh --check      # report only, change nothing
#
# WHY THIS EXISTS. Workload Identity principals are per-NAMESPACE. Deploy
# the recipe into a namespace you have used before and it inherits that
# namespace's bindings; deploy it into a fresh one — which is the normal
# thing to do, to avoid colliding with an older demo — and it starts with
# none. Nothing in the deploy fails. Both Deployments go Ready, the
# watcher raises an incident, a session opens, and the failure lands
# inside a turn, minutes later, as a 403 on something else's behalf.
#
# THE ROLES. Six, and every one of them is silent at deploy time. The list
# is the same one `examples/gke-troubleshoot-agent/scripts/setup-wif.sh`
# has carried since that recipe shipped; this script exists because it
# reads the recipe's own coordinates, reports before it writes, and can be
# called with --check from set-up-demo.sh.
#
#   roles/aiplatform.user       core-agent-daemon. Without it the agent
#                               cannot answer at all — the first model
#                               call 403s with "(or it may not exist)"
#                               appended, which sends you off checking
#                               model availability. Wrong hypothesis: the
#                               model is fine and the namespace is new.
#                               Cost a live drill run on 2026-09-06.
#
#   roles/mcp.toolUser          core-agent-daemon. Carries
#                               mcp.googleapis.com/tools.call, which the
#                               `gke` MCP surface checks on EVERY tool
#                               call. The subtlest failure of the six:
#                               the agent talks to the model perfectly
#                               well and every cluster read comes back
#                               403, so it produces a fluent, confident,
#                               entirely ungrounded answer built from the
#                               alert text alone. Observed 2026-09-06: 7
#                               of 12 tool calls denied; the drill's G1
#                               (grounded) was the only box that caught
#                               it, and it cost the second run.
#
#   roles/container.viewer      core-agent-daemon. mcp.toolUser buys the
#                               right to call a tool; this buys the right
#                               to see the answer. The recipe points at
#                               the read-only endpoint
#                               (container.googleapis.com/mcp/read-only),
#                               so viewer covers every call it can make.
#                               Re-point mcp.json at the full-access
#                               endpoint and this needs to be
#                               roles/container.admin.
#
#   roles/iam.serviceAccountUser  core-agent-daemon, and the odd one out:
#                               bound on the NODE SERVICE ACCOUNT, not on
#                               the project. GKE MCP's server-side chain
#                               impersonates the node SA, and without this
#                               you get a 403 with no hint that
#                               impersonation is what failed.
#
#   roles/cloudtrace.user       BOTH accounts, because the tracing
#                               overlays instrument both Deployments.
#                               Without it spans are exported and rejected
#                               server-side, so a healthy-looking deploy
#                               produces an empty trace list. The watcher
#                               is the one that gets forgotten: it holds
#                               no other GCP role, so there is no existing
#                               binding to amend and nothing else breaks
#                               to tip you off, and a trace missing only
#                               the inject span still reads as complete.
#
#   roles/monitoring.metricWriter  core-agent-daemon. The metrics half of
#                               the same overlay — Cloud Trace and Cloud
#                               Monitoring are separate services and
#                               cloudtrace.user does not cover metrics.
#                               Traces work, metrics silently do not.
#
# WHAT GETS SKIPPED. Only the telemetry half, and only on OTEL=0 — which
# set-up-demo.sh passes once it has resolved which overlay it is actually
# using. That drops the two cloudtrace pairs, monitoring.metricWriter,
# and their two APIs.
#
# MODEL_FLAVOR does NOT change the list, which is the opposite of what it
# looks like it should do. The anthropic flavor here is
# ANTHROPIC-ON-VERTEX: core-agent's `anthropic-vertex` provider
# authenticates with ADC through google.FindDefaultCredentials and calls
# aiplatform.googleapis.com — same host, same token, same
# roles/aiplatform.user. prereqs.sh says so at the MODEL_FLAVOR switch,
# and records that it was verified from the daemon KSA itself. An earlier
# version of this script skipped the Vertex role on the anthropic path;
# it would have produced a namespace where the whole recipe 403s on its
# first model call and --check reports everything in place.
#
# Everything else is unconditional: the `gke` MCP surface is what this
# recipe IS, on both flavors, and an agent that cannot read the cluster
# is not a GKE agent.
set -euo pipefail

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )

# Arguments before coordinates, so `--help` and a typo both answer without
# a configured project. Sourcing prereqs.sh first would make reading the
# usage require the very setup the usage explains.
CHECK_ONLY=0
case "${1:-}" in
    --check)    CHECK_ONLY=1 ;;
    "")         ;;
    -h|--help)  sed -n '16,21p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)          echo "✗ unknown argument '$1' (expected --check)" >&2; exit 2 ;;
esac

source "${SCRIPT_DIR}/prereqs.sh"
require_coordinates || exit 1

# Unlike set-up-demo.sh's best-effort check, granting IS this script's
# job, so an unreadable project number is fatal. An empty one builds a
# principal with an empty project segment: it matches no existing binding
# (so everything looks missing) and add-iam-policy-binding would create a
# junk member that grants nothing and has to be cleaned up by hand.
if [[ -z "${PROJECT_NUMBER}" ]]; then
    echo "✗ could not resolve the project number for ${PROJECT_ID}." >&2
    echo "  gcloud projects describe ${PROJECT_ID} --format='value(projectNumber)'" >&2
    echo "  Fix that first — this script will not guess it." >&2
    exit 1
fi

ksa_principal() {
    echo "principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/${DEMO_NS}/sa/$1"
}

# Somewhere to put gcloud's stderr, so a failed grant reports the actual
# reason instead of a bare exit code. Created up front: the redirect below
# runs under `set -e`, so a missing directory would abort the script with
# no message at all.
mkdir -p "${RIG_STATE_DIR}"
ERR_LOG="${RIG_STATE_DIR}/grant-iam.err"

OTEL="${OTEL:-1}"

# The node service account, read from the cluster rather than assumed.
# The Compute Engine default is the common case and what every other
# recipe hardcodes, but a cluster built with a custom node SA would then
# get a binding on an account nothing runs as — which grants nothing and
# looks granted. `describe` reports the literal string "default" for the
# Compute Engine default SA, so that answer still needs expanding.
if [[ -z "${NODE_SA:-}" ]]; then
    NODE_SA=$(gcloud container clusters describe "${CLUSTER_NAME}" \
        --region "${REGION}" --project "${PROJECT_ID}" \
        --format='value(nodeConfig.serviceAccount)' 2>/dev/null || true)
fi
if [[ -z "${NODE_SA}" || "${NODE_SA}" == "default" ]]; then
    NODE_SA="${PROJECT_NUMBER}-compute@developer.gserviceaccount.com"
fi

# The APIs. A disabled API fails exactly like a missing role — a 403 from
# somewhere the operator was not looking — so they belong in the same
# pass, even though enabling one is a different verb from granting one.
apis=(
    container.googleapis.com        # GKE, and the GKE MCP surface with it
    iamcredentials.googleapis.com   # the Workload Identity token exchange
    aiplatform.googleapis.com       # Vertex — both flavors, see above
)
if [[ "${OTEL}" != "0" ]]; then
    apis+=(cloudtrace.googleapis.com monitoring.googleapis.com)
fi

# Project-scoped (role, ksa) pairs.
grants=()
grants+=("roles/aiplatform.user core-agent-daemon")
grants+=("roles/mcp.toolUser core-agent-daemon")
grants+=("roles/container.viewer core-agent-daemon")
if [[ "${OTEL}" != "0" ]]; then
    grants+=("roles/cloudtrace.user core-agent-daemon")
    grants+=("roles/cloudtrace.user lookout-watch")
    grants+=("roles/monitoring.metricWriter core-agent-daemon")
fi

echo "→ project ${PROJECT_ID} (${PROJECT_NUMBER}), namespace ${DEMO_NS}, flavor ${MODEL_FLAVOR}"
echo "  node SA ${NODE_SA}"
if [[ "${MODEL_FLAVOR}" != "gemini" ]]; then
    echo "  (${MODEL_FLAVOR} is Anthropic-on-Vertex — same aiplatform role, nothing skipped)"
fi
if [[ "${OTEL}" == "0" ]]; then
    echo "  (OTEL=0 — cloudtrace/monitoring roles and APIs skipped)"
fi

# mcp.toolUser and container.viewer are unconditional, so the list can no
# longer be empty — but an empty one would mean this script silently did
# nothing, which is the failure mode it exists to prevent. Assert.
if [[ ${#grants[@]} -eq 0 ]]; then
    echo "✗ internal error: no grants selected. mcp.toolUser is unconditional, so this cannot happen." >&2
    exit 3
fi

missing=0
changed=0
failed=0

# --- APIs -------------------------------------------------------------
echo
echo "  APIs:"
# One list call, not one per API: unlike the IAM policy below, this state
# does not change as we go — nothing in the loop enables a second API.
#
# A failed read is NOT an empty result. Swallowing the difference would
# report all five APIs disabled to an operator who merely lacks
# serviceusage.services.list, which is a confident wrong answer of exactly
# the kind this script exists to stop producing.
api_read_ok=1
if ! enabled=$(gcloud services list --enabled --project "${PROJECT_ID}" \
        --format='value(config.name)' 2>"${ERR_LOG}"); then
    api_read_ok=0
    echo "    ✗ could not list enabled APIs on ${PROJECT_ID}:"
    sed 's/^/      /' "${ERR_LOG}" >&2 || true
    echo "      needs serviceusage.services.list — API state UNKNOWN, not disabled."
    failed=$(( failed + 1 ))
fi

for api in "${apis[@]}"; do
    if (( ! api_read_ok )); then
        echo "    ${api}: UNKNOWN"
        continue
    fi
    if grep -qxF "${api}" <<<"${enabled}"; then
        echo "    ${api}: enabled ✓"
        continue
    fi

    missing=$(( missing + 1 ))
    if (( CHECK_ONLY )); then
        echo "    ${api}: DISABLED"
        continue
    fi

    echo "    ${api}: disabled — enabling"
    if gcloud services enable "${api}" --project "${PROJECT_ID}" \
        >/dev/null 2>"${ERR_LOG}"; then
        echo "      enabled ✓"
        changed=$(( changed + 1 ))
    else
        echo "    ✗ could not enable ${api}:"
        sed 's/^/      /' "${ERR_LOG}" >&2 || true
        failed=$(( failed + 1 ))
    fi
done

# --- Project-scoped roles ---------------------------------------------
echo
echo "  Project roles:"
for pair in "${grants[@]}"; do
    read -r role ksa <<<"${pair}"
    principal=$(ksa_principal "${ksa}")

    # One read per role rather than one policy dump reused across the
    # loop: the policy changes underneath us as we grant, and a stale
    # snapshot would re-grant something we just added.
    members=$(gcloud projects get-iam-policy "${PROJECT_ID}" \
        --flatten="bindings[].members" \
        --filter="bindings.role=${role}" \
        --format='value(bindings.members)' 2>/dev/null || true)

    if grep -qxF "${principal}" <<<"${members}"; then
        echo "    ${role} on ${ksa}: granted ✓"
        continue
    fi

    missing=$(( missing + 1 ))
    if (( CHECK_ONLY )); then
        echo "    ${role} on ${ksa}: MISSING"
        continue
    fi

    echo "    ${role} on ${ksa}: missing — granting"
    if gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
        --role="${role}" \
        --member="${principal}" \
        --condition=None \
        --format='value(etag)' >/dev/null 2>"${ERR_LOG}"; then
        echo "      granted ✓"
        changed=$(( changed + 1 ))
    else
        echo "    ✗ could not grant ${role} to ${ksa}:"
        sed 's/^/      /' "${ERR_LOG}" >&2 || true
        failed=$(( failed + 1 ))
    fi
done

# --- The one binding that is not on the project -----------------------
# roles/iam.serviceAccountUser lives on the node SA resource. Granting it
# at project scope would also read as granted by a project-policy check
# and would NOT fix the 403, because GKE MCP's impersonation check looks
# at the SA's own policy.
echo
echo "  Node SA (${NODE_SA}):"
sa_principal=$(ksa_principal "core-agent-daemon")
sa_read_ok=1
if ! sa_members=$(gcloud iam service-accounts get-iam-policy "${NODE_SA}" \
        --project "${PROJECT_ID}" \
        --flatten="bindings[].members" \
        --filter="bindings.role=roles/iam.serviceAccountUser" \
        --format='value(bindings.members)' 2>"${ERR_LOG}"); then
    sa_read_ok=0
fi

if (( ! sa_read_ok )); then
    # Same reasoning as the API list: an unreadable policy is not an empty
    # one. Reporting MISSING here would send an operator to grant a binding
    # that may already exist, on an account that may not.
    echo "    ✗ could not read the IAM policy of ${NODE_SA}:"
    sed 's/^/      /' "${ERR_LOG}" >&2 || true
    echo "      roles/iam.serviceAccountUser is UNKNOWN, not missing. Either the"
    echo "      account does not exist (check NODE_SA) or you lack"
    echo "      iam.serviceAccounts.getIamPolicy on it."
    failed=$(( failed + 1 ))
elif grep -qxF "${sa_principal}" <<<"${sa_members}"; then
    echo "    roles/iam.serviceAccountUser on core-agent-daemon: granted ✓"
elif (( CHECK_ONLY )); then
    missing=$(( missing + 1 ))
    echo "    roles/iam.serviceAccountUser on core-agent-daemon: MISSING"
else
    missing=$(( missing + 1 ))
    echo "    roles/iam.serviceAccountUser on core-agent-daemon: missing — granting"
    if gcloud iam service-accounts add-iam-policy-binding "${NODE_SA}" \
        --project "${PROJECT_ID}" \
        --role="roles/iam.serviceAccountUser" \
        --member="${sa_principal}" \
        --format='value(etag)' >/dev/null 2>"${ERR_LOG}"; then
        echo "      granted ✓"
        changed=$(( changed + 1 ))
    else
        echo "    ✗ could not grant roles/iam.serviceAccountUser on ${NODE_SA}:"
        sed 's/^/      /' "${ERR_LOG}" >&2 || true
        failed=$(( failed + 1 ))
    fi
fi

echo
if (( CHECK_ONLY )); then
    # `failed` counts unreadable state, not failed writes — --check writes
    # nothing. It has to be part of the verdict: a run that could not read
    # two of the answers and then printed "all in place" would be the
    # confident wrong answer this script exists to stop producing.
    if (( missing || failed )); then
        # Two plain `if`s rather than `(( n )) && echo`: a false arithmetic
        # test is a failing command, and under `set -e` the first zero
        # counter would abort the script before the second message.
        if (( missing )); then
            echo "✗ ${missing} item(s) missing. Run without --check to fix them."
        fi
        if (( failed )); then
            echo "✗ ${failed} item(s) could not be read — see above. Nothing is proven about them."
        fi
        exit 1
    fi
    echo "✓ all required APIs and bindings are in place."
    exit 0
fi

if (( failed )); then
    echo "✗ ${failed} step(s) failed — you likely lack"
    echo "  resourcemanager.projects.setIamPolicy (roles), serviceusage.services.enable"
    echo "  (APIs), or iam.serviceAccounts.setIamPolicy (the node SA binding) on"
    echo "  ${PROJECT_ID}. Ask a project admin to run this script."
    exit 1
fi

if (( changed )); then
    # Not instant, and the failure it causes on a too-early retry looks
    # exactly like the failure it just fixed.
    echo "✓ ${changed} change(s) applied. No restart needed, but IAM and API enablement"
    echo "  take up to a minute to propagate — if the next turn still 403s, wait and"
    echo "  retry before concluding anything."
else
    echo "✓ nothing to do — every API and binding was already in place."
fi
