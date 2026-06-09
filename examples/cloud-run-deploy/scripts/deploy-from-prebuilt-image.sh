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
#
# Path A deploy script — pulls the published core-agent image from
# GHCR, retags + pushes to your project's Artifact Registry, then
# deploys to Cloud Run with config sourced from Secret Manager.
#
# This is the production-shaped variant: image and config are
# decoupled, IAM is explicit, no Dockerfile to maintain. For the
# 5-minute quickstart (single `gcloud run deploy --source .`), see
# the README and skip this script.
#
# Required env vars (set before running):
#   PROJECT_ID    — your GCP project ID
#   REGION        — Cloud Run region (e.g. us-central1)
#
# Optional env vars (defaults shown):
#   SERVICE_NAME       — Cloud Run service name (default: core-agent)
#   IMAGE_TAG          — published core-agent tag (default: 2.3.1)
#   AR_REPO            — Artifact Registry repository (default: core-agent)
#   AGENTS_DIR         — local .agents/ bundle to upload (default: ../.agents)
#   RUNNER_SA          — service account name (default: core-agent-runner)
#   SMALL_MODEL        — agentic-tools small model (default: gemini-2.5-flash)

set -euo pipefail

: "${PROJECT_ID:?must set PROJECT_ID}"
: "${REGION:?must set REGION (e.g. us-central1)}"

SERVICE_NAME="${SERVICE_NAME:-core-agent}"
# Default: track the :main rolling tag until v2.4.0 ships. The
# :2.3.1 tag predates several features this recipe relies on:
#   - agent-card endpoint at /.well-known/agent-card.json (cef0d1b)
#   - X-Attach-Token side-channel header for gateway-fronted auth
#     (#141) — required so the operator's TUI can use
#     --auth=google-id-token without fighting Cloud Run for the
#     Authorization header.
# Sweep back to a pinned semver (e.g. 2.4.0) for production
# deploys once a release cuts including the above. IMAGE_REF
# takes precedence; IMAGE_TAG is the fallback and is also what
# gets used as the destination tag in AR.
IMAGE_REF="${IMAGE_REF:-ghcr.io/go-steer/core-agent:main}"
IMAGE_TAG="${IMAGE_TAG:-main}"
AR_REPO="${AR_REPO:-core-agent}"
AGENTS_DIR="${AGENTS_DIR:-$(cd "$(dirname "$0")/.." && pwd)/.agents}"
RUNNER_SA="${RUNNER_SA:-core-agent-runner}"
SMALL_MODEL="${SMALL_MODEL:-gemini-2.5-flash}"

SRC_IMAGE="$IMAGE_REF"
DST_IMAGE="${REGION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO}/core-agent:${IMAGE_TAG}"
RUNNER_EMAIL="${RUNNER_SA}@${PROJECT_ID}.iam.gserviceaccount.com"

echo "==> project=$PROJECT_ID region=$REGION service=$SERVICE_NAME image=$DST_IMAGE"

# 1. Enable APIs (idempotent)
echo "==> enabling APIs"
gcloud services enable \
  run.googleapis.com \
  aiplatform.googleapis.com \
  artifactregistry.googleapis.com \
  cloudbuild.googleapis.com \
  secretmanager.googleapis.com \
  --project="$PROJECT_ID"

# 2. Create Artifact Registry repo (idempotent)
echo "==> ensuring Artifact Registry repo $AR_REPO in $REGION"
if ! gcloud artifacts repositories describe "$AR_REPO" \
       --location="$REGION" --project="$PROJECT_ID" >/dev/null 2>&1; then
  gcloud artifacts repositories create "$AR_REPO" \
    --repository-format=docker \
    --location="$REGION" \
    --description="core-agent images mirrored from ghcr.io" \
    --project="$PROJECT_ID"
fi

# 3. Mirror the image. Pull from GHCR, retag, push to AR.
# Using `docker` here because `gcloud artifacts docker tags add`
# requires the source image to already be in another AR repo.
echo "==> mirroring $SRC_IMAGE -> $DST_IMAGE"
gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet
docker pull "$SRC_IMAGE"
docker tag "$SRC_IMAGE" "$DST_IMAGE"
docker push "$DST_IMAGE"

# 4. Create the runtime service account (idempotent)
echo "==> ensuring service account $RUNNER_EMAIL"
if ! gcloud iam service-accounts describe "$RUNNER_EMAIL" \
       --project="$PROJECT_ID" >/dev/null 2>&1; then
  gcloud iam service-accounts create "$RUNNER_SA" \
    --display-name="core-agent Cloud Run runtime" \
    --project="$PROJECT_ID"
fi

# Vertex AI inference is the only outbound IAM the default recipe
# needs. Grant other roles (BigQuery, GCS, etc.) explicitly per
# use case.
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:${RUNNER_EMAIL}" \
  --role=roles/aiplatform.user \
  --condition=None \
  --quiet >/dev/null

# 5. Stage the .agents/ bundle in Secret Manager. config.json and
# AGENTS.md become mounted files at /etc/core-agent/.agents/*.
# Updating config = `gcloud secrets versions add` + a redeploy
# (Cloud Run picks up the new version via the `:latest` alias).
echo "==> staging $AGENTS_DIR in Secret Manager"
for f in config.json AGENTS.md; do
  secret_name="core-agent-$(echo "$f" | tr '.' '-' | tr '[:upper:]' '[:lower:]')"
  if ! gcloud secrets describe "$secret_name" --project="$PROJECT_ID" >/dev/null 2>&1; then
    gcloud secrets create "$secret_name" \
      --replication-policy=automatic \
      --project="$PROJECT_ID" >/dev/null
  fi
  gcloud secrets versions add "$secret_name" \
    --data-file="$AGENTS_DIR/$f" \
    --project="$PROJECT_ID" >/dev/null
  gcloud secrets add-iam-policy-binding "$secret_name" \
    --member="serviceAccount:${RUNNER_EMAIL}" \
    --role=roles/secretmanager.secretAccessor \
    --project="$PROJECT_ID" --quiet >/dev/null
done

# 6. Mint the ATTACH_TOKEN secret if it doesn't exist. Idempotent:
# preserves the existing token on re-runs so operators don't have
# to refresh their cached value.
#
# `tr -d '\n'` is load-bearing: openssl rand emits a trailing
# newline, gcloud stores the bytes verbatim, Cloud Run mounts the
# env var verbatim. Operators reading the secret locally via
# `$(gcloud secrets versions access)` get a value stripped of one
# trailing newline by command substitution — so their local
# ATTACH_TOKEN ends up one byte shorter than the container's, and
# core-agent's constant-time bearer compare fails by one byte.
# Strip the newline at creation time and everyone's value matches.
echo "==> ensuring ATTACH_TOKEN secret"
if ! gcloud secrets describe core-agent-attach-token --project="$PROJECT_ID" >/dev/null 2>&1; then
  openssl rand -hex 32 | tr -d '\n' | gcloud secrets create core-agent-attach-token \
    --data-file=- \
    --replication-policy=automatic \
    --project="$PROJECT_ID"
fi
gcloud secrets add-iam-policy-binding core-agent-attach-token \
  --member="serviceAccount:${RUNNER_EMAIL}" \
  --role=roles/secretmanager.secretAccessor \
  --project="$PROJECT_ID" --quiet >/dev/null

# 7. Deploy. Key flags:
#   --no-allow-unauthenticated  : Cloud Run IAM gate
#   --port=7777                  : core-agent's fixed listener
#   --min-instances=1            : keep warm so sessions.db survives
#   --max-instances=3            : allow scaling under transient bursts
#                                  (TUI's polling fan-out can spike briefly)
#   --no-cpu-throttling          : agent does background work
#   --concurrency=80             : SSE on /events holds one slot for its
#                                  lifetime; the TUI's parallel polls of
#                                  /status, /usage, etc. need headroom
#                                  (concurrency=1 starves them and 429s)
#   --timeout=3600               : allow long-running SSE attaches
#
# VERTEX_LOCATION defaults to 'global' because the default model
# (gemini-3.1-pro-preview-customtools, etc.) is served from the
# global endpoint. For regional models, set VERTEX_LOCATION to the
# region — e.g. VERTEX_LOCATION=us-central1 for gemini-2.5-pro.
# GOOGLE_CLOUD_LOCATION is the Vertex API location and is INDEPENDENT
# of the Cloud Run service's --region (where this container runs).
VERTEX_LOCATION="${VERTEX_LOCATION:-global}"
echo "==> deploying $SERVICE_NAME to Cloud Run (vertex location=$VERTEX_LOCATION)"
gcloud run deploy "$SERVICE_NAME" \
  --image="$DST_IMAGE" \
  --region="$REGION" \
  --project="$PROJECT_ID" \
  --service-account="$RUNNER_EMAIL" \
  --no-allow-unauthenticated \
  --port=7777 \
  --min-instances=1 \
  --max-instances=3 \
  --cpu=2 \
  --memory=2Gi \
  --no-cpu-throttling \
  --concurrency=80 \
  --timeout=3600 \
  --set-env-vars="GOOGLE_CLOUD_PROJECT=${PROJECT_ID},GOOGLE_CLOUD_LOCATION=${VERTEX_LOCATION},GOOGLE_GENAI_USE_VERTEXAI=1,AGENTIC_SMALL_MODEL=${SMALL_MODEL}" \
  --set-secrets="ATTACH_TOKEN=core-agent-attach-token:latest,/etc/core-agent/.agents/config.json=core-agent-config-json:latest,/etc/core-agent/.agents/AGENTS.md=core-agent-agents-md:latest" \
  --args="--no-repl,--attach-listen=:7777,--attach-token=ATTACH_TOKEN,--session-db,--session-db-path=/tmp/sessions.db,--agentic-tools,-c,/etc/core-agent/.agents/config.json"

# 8. Print the URL + how to attach.
URL="$(gcloud run services describe "$SERVICE_NAME" \
        --region="$REGION" --project="$PROJECT_ID" \
        --format='value(status.url)')"

cat <<EOF

==> Deployed.
    Service URL: $URL
    Auth: --no-allow-unauthenticated (caller needs roles/run.invoker)

To attach (operator running locally):

  gcloud run services add-iam-policy-binding $SERVICE_NAME \\
    --region=$REGION --project=$PROJECT_ID \\
    --member="user:\$(gcloud config get-value account)" \\
    --role=roles/run.invoker

  # Recommended: --auth=google-id-token mints the ID token in the
  # TUI itself (no proxy hop, no per-request token-mint quota).
  # Requires ADC: gcloud auth application-default login
  # Requires TUI binary >= v2.4.0 (or built from main today):
  #   go install github.com/go-steer/core-agent/cmd/core-agent-tui@main
  export ATTACH_TOKEN="\$(gcloud secrets versions access latest \\
    --secret=core-agent-attach-token --project=$PROJECT_ID)"
  SERVICE_URL="\$(gcloud run services describe $SERVICE_NAME \\
    --region=$REGION --project=$PROJECT_ID --format='value(status.url)')"
  core-agent-tui --auth=google-id-token --token=ATTACH_TOKEN "\$SERVICE_URL"

  # Legacy / fallback (works with any TUI version):
  #   gcloud run services proxy $SERVICE_NAME --region=$REGION --project=$PROJECT_ID &
  #   core-agent-tui --token=ATTACH_TOKEN http://localhost:8080
EOF
