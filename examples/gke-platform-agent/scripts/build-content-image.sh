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

# Build + push the gke-platform-agent CONTENT image to Artifact Registry.
# The recipe content ships as an OCI image volume, not a ConfigMap, so it
# must live in a registry the GKE node SA can pull.
#
# Two tags, one per DELIVERY path, because the two need different bases:
#
#   :${CONTENT_TAG}       FROM scratch            -> image-volume overlay
#   :${CONTENT_TAG}-copy  FROM chainguard/busybox -> initContainer-copy overlay
#
# They carry byte-identical content; only the base differs. That is a
# separate axis from MODEL_FLAVOR, which selects gemini vs anthropic and
# ships INSIDE both tags — see the staging section below.
set -euo pipefail

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
source "${SCRIPT_DIR}/prereqs.sh"
require_coordinates || exit 1


# ── Preflight: everything the content image must carry ───────────────
# The build context is this directory. A missing piece here doesn't fail
# the docker build (COPY of a missing dir does, but a half-populated one
# doesn't) — it fails much later as a daemon that boots with no persona or
# a subagent with no skills. Check up front.
for required in \
    ".agents/config.json" \
    ".agents/config.hub.json" \
    ".agents/mcp.json" \
    ".agents/plans/.gitkeep" \
    "AGENTS.md" \
    "cluster/AGENTS.md" \
    "cluster/mcp.json" \
    "cluster/skills"
do
    if [[ ! -e "${RECIPE_ROOT}/${required}" ]]; then
        echo "✗ missing ${required} under ${RECIPE_ROOT}"
        echo "  The content image is built from this directory; see deploy/content.Dockerfile."
        exit 1
    fi
done
# .agents/plans must be pre-baked for the nested writable emptyDir to have
# a mount point inside the read-only image volume.
DOCKERFILE="${RECIPE_ROOT}/deploy/content.Dockerfile"
command -v jq >/dev/null || { echo "✗ jq not on PATH (needed to render the anthropic config variants)"; exit 1; }

# ── Stage the build context and render the anthropic variants ────────
# The image is MODEL-FLAVOR-AGNOSTIC: it always carries both
# .agents/config.hub.json (gemini, the checked-in file) and
# .agents/config.hub.anthropic.json (rendered here). MODEL_FLAVOR only
# decides which one the daemon's -c points at, so flipping flavors is a
# redeploy, not a rebuild — and both flavors are guaranteed to be the
# same content otherwise.
#
# Rendering rather than checking in a second config is deliberate. The
# two configs differ in exactly four values; everything else (the
# subagent's long routing description, the tool disable-list, the plan
# gate, the alert target, the whole attach/multi_session block) is
# identical, and a hand-maintained copy would drift on the first edit
# that isn't about models. jq keeps one source of truth.
#
# Staged under /tmp, never in the recipe tree: generated files sitting
# next to hand-written ones are how a stale variant gets shipped.
STAGE="$(mktemp -d "${TMPDIR:-/tmp}/gke-platform-agent-content.XXXXXX")"
trap 'rm -rf "${STAGE}"' EXIT
cp -a "${RECIPE_ROOT}/.agents"   "${STAGE}/.agents"
cp -a "${RECIPE_ROOT}/AGENTS.md" "${STAGE}/AGENTS.md"
cp -a "${RECIPE_ROOT}/cluster"   "${STAGE}/cluster"

# LOCAL RUN STATE MUST NOT REACH THE CONTEXT. Two layers, because this
# one fails open and quietly.
#
# Layer 1: carry .dockerignore with the context. Docker reads it from the
# build-context ROOT, and the root is ${STAGE} now, not ${RECIPE_ROOT} —
# so without this copy every rule in it silently stops applying. The
# Dockerfile does a wholesale `COPY .agents/`, and `.agents/sessions/`
# holds a JSON transcript per local run, so what ships is local
# conversation history baked into a cluster artifact.
cp -a "${RECIPE_ROOT}/.dockerignore" "${STAGE}/.dockerignore"

# Layer 2: prune the staged tree too, then assert. .dockerignore filters
# inside docker where we can't see it; pruning makes the exclusion
# checkable on disk, and turns "the rules stopped applying" from an
# invisible leak into a failed build. Keep this list in sync with
# .dockerignore's run-state rules — the assert below is what catches you
# if it drifts.
rm -rf "${STAGE}/.agents/sessions"
rm -f  "${STAGE}/.agents/config.hub.local.json"
find "${STAGE}" \( -name '*.db' -o -name '*.db-shm' -o -name '*.db-wal' \) -type f -delete
find "${STAGE}/.agents/plans" -type f ! -name '.gitkeep' -delete 2>/dev/null

leaked=$(find "${STAGE}" \( -path '*/.agents/sessions/*' -o -name '*.db' \
             -o -name '*.db-shm' -o -name '*.db-wal' \
             -o -name 'config.hub.local.json' \) -type f -print)
leaked+=$(find "${STAGE}/.agents/plans" -type f ! -name '.gitkeep' -print 2>/dev/null)
if [[ -n "${leaked}" ]]; then
    echo "✗ local run state reached the build context:" >&2
    echo "${leaked}" | sed 's|^|    |' >&2
    exit 1
fi

# Derive an anthropic config from a gemini one. Four values move:
# the parent model, the `cluster` specialist's model, and both cost
# ceilings (see prereqs.sh for why the ceilings must be re-scaled).
# Provider becomes "anthropic-vertex" — project/location are NOT set in
# the config, so the provider falls back to GOOGLE_CLOUD_PROJECT /
# GOOGLE_CLOUD_LOCATION from the pod's env ConfigMap, which is what
# makes location=global reach the right endpoint.
render_anthropic() {
    local src="$1" dst="$2"
    jq --arg parent  "${ANTHROPIC_PARENT_MODEL}" \
       --arg spec    "${ANTHROPIC_CLUSTER_MODEL}" \
       --argjson turn    "${ANTHROPIC_MAX_TURN_COST_USD}" \
       --argjson session "${ANTHROPIC_MAX_SESSION_COST_USD}" '
        .model = { provider: "anthropic-vertex", name: $parent }
        | .agent.max_turn_cost_usd    = $turn
        | .agent.max_session_cost_usd = $session
        | .subagents = [ .subagents[]
            | if .name == "cluster"
              then .model = { provider: "anthropic-vertex", name: $spec }
              else . end ]
    ' "$src" > "$dst"
    # A subagent whose model silently failed to rewrite would run the
    # WRONG provider and fail at first delegation, deep into a UAT.
    local got
    got=$(jq -r '[.model.name] + [.subagents[] | select(.name=="cluster") | .model.name] | join(",")' "$dst")
    [[ "${got}" == "${ANTHROPIC_PARENT_MODEL},${ANTHROPIC_CLUSTER_MODEL}" ]] \
        || { echo "✗ render_anthropic produced unexpected models: ${got}"; exit 1; }
}
render_anthropic "${STAGE}/.agents/config.hub.json" "${STAGE}/.agents/config.hub.anthropic.json"
render_anthropic "${STAGE}/.agents/config.json"     "${STAGE}/.agents/config.anthropic.json"

echo "Building content from: ${RECIPE_ROOT}  (staged in ${STAGE})"
echo "  gemini flavor:    $(jq -r '.model.name' "${STAGE}/.agents/config.hub.json") / $(jq -r '.subagents[]|select(.name=="cluster")|.model.name' "${STAGE}/.agents/config.hub.json")"
echo "  anthropic flavor: ${ANTHROPIC_PARENT_MODEL} / ${ANTHROPIC_CLUSTER_MODEL}"
echo "Pushing to:            ${CONTENT_IMAGE}:{${CONTENT_TAG},${CONTENT_TAG}-copy}"

# ── Ensure the Artifact Registry repo exists ─────────────────────────
if ! gcloud artifacts repositories describe "${AR_REPO}" \
        --project="${PROJECT_ID}" --location="${REGION}" >/dev/null 2>&1; then
    echo "→ creating Artifact Registry repo ${AR_REPO} in ${REGION}"
    gcloud artifacts repositories create "${AR_REPO}" \
        --project="${PROJECT_ID}" --location="${REGION}" \
        --repository-format=docker \
        --description="core-agent demo images"
fi

# Let docker push to *-docker.pkg.dev with your gcloud creds.
gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet

# ── Build + push both delivery variants ──────────────────────────────
echo "→ building image-volume variant (FROM scratch) :${CONTENT_TAG}"
docker build -f "${DOCKERFILE}" \
    -t "${CONTENT_IMAGE}:${CONTENT_TAG}" \
    "${STAGE}"
docker push "${CONTENT_IMAGE}:${CONTENT_TAG}"

echo "→ building initContainer-copy variant (FROM chainguard/busybox) :${CONTENT_TAG}-copy"
docker build -f "${DOCKERFILE}" \
    --build-arg BASE=cgr.dev/chainguard/busybox \
    -t "${CONTENT_IMAGE}:${CONTENT_TAG}-copy" \
    "${STAGE}"
docker push "${CONTENT_IMAGE}:${CONTENT_TAG}-copy"

# ── Report the digest (optional: pin by digest for reproducibility) ──
DIGEST=$(gcloud artifacts docker images describe \
    "${CONTENT_IMAGE}:${CONTENT_TAG}" \
    --project="${PROJECT_ID}" --format='value(image_summary.digest)' 2>/dev/null || true)
echo
echo "✓ content images pushed"
echo "    ${CONTENT_IMAGE}:${CONTENT_TAG}"
echo "    ${CONTENT_IMAGE}:${CONTENT_TAG}-copy"
[[ -n "${DIGEST}" ]] && echo "  image-volume digest: ${DIGEST}  (pin this for a reproducible deploy)"
