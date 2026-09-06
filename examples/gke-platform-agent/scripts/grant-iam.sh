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

# Grant the project IAM the recipe's two Kubernetes ServiceAccounts need,
# via Workload Identity. Idempotent: it reads the policy first and only
# calls add-iam-policy-binding for what is actually missing.
#
#     ./scripts/grant-iam.sh              # grant what is missing
#     ./scripts/grant-iam.sh --check      # report only, change nothing
#
# WHY THIS EXISTS. Workload Identity principals are per-NAMESPACE. Deploy
# the recipe into a namespace you have used before and it inherits that
# namespace's bindings; deploy it into a fresh one — which is the normal
# thing to do, to avoid colliding with an older demo — and it starts with
# none. Nothing in the deploy fails. Both Deployments go Ready, the
# watcher raises an incident, a session opens, and the FIRST MODEL CALL
# 403s, inside a turn, minutes later:
#
#     Permission 'aiplatform.endpoints.predict' denied on resource
#     '.../publishers/google/models/gemini-3.7-flash' (or it may not exist)
#
# That was observed live on 2026-09-06 during a GKE drill run and cost the
# run. The trailing "(or it may not exist)" sends you off checking model
# availability, which is the wrong hypothesis: the model was fine and the
# namespace was new.
#
# The two roles fail in different ways and both are silent at deploy time:
#
#   roles/aiplatform.user   core-agent-daemon only. Without it the agent
#                           cannot answer at all. This is the one that
#                           stops everything.
#   roles/cloudtrace.user   BOTH accounts, because the tracing overlays
#                           instrument both Deployments. Without it spans
#                           are exported and rejected server-side, so a
#                           healthy-looking deploy produces an empty trace
#                           list. The watcher is the one that gets
#                           forgotten: it holds no other GCP role, so
#                           there is no existing binding to amend and
#                           nothing else breaks to tip you off, and a
#                           trace missing only the inject span still reads
#                           as a complete trace.
#
# On MODEL_FLAVOR=anthropic the daemon talks to Anthropic, not Vertex, and
# roles/aiplatform.user is not required — it is skipped rather than
# granted, because a role nothing uses is one more thing to explain. The
# same applies to cloudtrace on a deploy without tracing: pass OTEL=0 (as
# set-up-demo.sh does, once it has resolved which overlay it is using) and
# the two cloudtrace pairs are skipped.
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

# The (role, ksa) pairs this recipe needs, built from MODEL_FLAVOR and
# OTEL. Defaults to including cloudtrace, because the standalone caller
# has not resolved an overlay and the cost of a spare binding is lower
# than the cost of an empty trace list.
OTEL="${OTEL:-1}"
grants=()
if [[ "${MODEL_FLAVOR}" == "gemini" ]]; then
    grants+=("roles/aiplatform.user core-agent-daemon")
fi
if [[ "${OTEL}" != "0" ]]; then
    grants+=("roles/cloudtrace.user core-agent-daemon")
    grants+=("roles/cloudtrace.user lookout-watch")
fi

echo "→ project ${PROJECT_ID} (${PROJECT_NUMBER}), namespace ${DEMO_NS}, flavor ${MODEL_FLAVOR}"
if [[ "${MODEL_FLAVOR}" != "gemini" ]]; then
    echo "  (roles/aiplatform.user not required on the ${MODEL_FLAVOR} path — skipped)"
fi
if [[ "${OTEL}" == "0" ]]; then
    echo "  (OTEL=0 — roles/cloudtrace.user not required, skipped)"
fi

# Both roles skipped is not "nothing to do", it is a call that cannot
# tell you anything. Say so rather than exit 0 on an empty loop.
if [[ ${#grants[@]} -eq 0 ]]; then
    echo "✓ nothing to grant: MODEL_FLAVOR=${MODEL_FLAVOR} needs no Vertex access and OTEL=0 needs no tracing role."
    exit 0
fi

missing=0
granted=0
failed=0

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
        granted=$(( granted + 1 ))
    else
        echo "    ✗ could not grant ${role} to ${ksa}:"
        sed 's/^/      /' "${ERR_LOG}" >&2 || true
        failed=$(( failed + 1 ))
    fi
done

echo
if (( CHECK_ONLY )); then
    if (( missing )); then
        echo "✗ ${missing} binding(s) missing. Run without --check to grant them."
        exit 1
    fi
    echo "✓ all required bindings are in place."
    exit 0
fi

if (( failed )); then
    echo "✗ ${failed} grant(s) failed — you likely lack resourcemanager.projects.setIamPolicy"
    echo "  on ${PROJECT_ID}. Ask a project admin to run this script."
    exit 1
fi

if (( granted )); then
    # Not instant, and the failure it causes on a too-early retry looks
    # exactly like the failure it just fixed.
    echo "✓ ${granted} binding(s) granted. No restart needed, but IAM takes up to a"
    echo "  minute to propagate — if the next turn still 403s, wait and retry before"
    echo "  concluding anything."
else
    echo "✓ nothing to do — all required bindings were already in place."
fi
