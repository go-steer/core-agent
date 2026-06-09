# `cloud-run-deploy` — config recipe for deploying core-agent to Cloud Run

A complete, drop-in recipe for running `core-agent` as a long-lived
Cloud Run service, reachable by operators over Cloud Run's
**IAM-gated HTTPS endpoint** with a runtime service account holding
`roles/aiplatform.user` for credential-free auth to Vertex AI.

Two deploy paths, pick what fits:

- **Path B (5-minute quickstart):** `gcloud run deploy --source .` —
  builds a thin image from this directory's Dockerfile, pushes
  through Cloud Build, deploys. One command after prereqs.
- **Path A (production-shaped):** pull the published
  `ghcr.io/go-steer/core-agent` image, mirror to Artifact Registry,
  deploy from the AR image with config sourced from Secret Manager.
  Image and config decoupled, IAM explicit. One script
  (`scripts/deploy-from-prebuilt-image.sh`).

Both paths land at the same place: a private Cloud Run service
listening on port 7777, with the same `.agents/` bundle, the same
runtime service account, and the same operator attach flow. Pick
based on whether you want speed-to-running (B) or
config-image-separation (A).

## Architecture

```
[Operator workstation, ADC-configured (gcloud auth application-default login)]
  core-agent-tui --auth=google-id-token --token=ATTACH_TOKEN https://my-svc-...-uc.a.run.app
       │
       │ TUI mints a Google ID token via ADC (audience = service URL),
       │ stamps Authorization: Bearer <ID-token> + X-Attach-Token: <attach-token>
       ▼
[Cloud Run IAM gate]
       │  validates caller has roles/run.invoker on this service
       │  (Authorization: Bearer <ID-token>) before forwarding to the container
       ▼
[Cloud Run service core-agent (private, --no-allow-unauthenticated)]
       │
       ▼
[Container instance (min=1, max=1, no CPU throttling, concurrency=1)]
       ├── ServiceAccount: core-agent-runner@PROJECT.iam.gserviceaccount.com
       │   └── roles/aiplatform.user → Vertex AI inference
       │       ADC reads the Cloud Run metadata server transparently
       │
       ├── Image: ghcr.io/go-steer/core-agent (path B)
       │     or:  REGION-docker.pkg.dev/PROJECT/core-agent/core-agent (path A)
       │
       ├── /etc/core-agent/.agents/
       │     path A: mounted from Secret Manager
       │     path B: baked into the image by the Dockerfile
       │
       ├── /tmp/sessions.db
       │     ephemeral; survives between requests in a warm
       │     instance, lost on cold start
       │
       ├── Env: GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_LOCATION,
       │        GOOGLE_GENAI_USE_VERTEXAI=1, AGENTIC_SMALL_MODEL
       │
       ├── Secret env ATTACH_TOKEN  → --attach-token=ATTACH_TOKEN
       │     defense-in-depth on top of Cloud Run IAM
       │
       └── Listens on 0.0.0.0:7777 (Cloud Run --port=7777)
             Serves /.well-known/agent-card.json from the same listener.
```

### Why two gates (Cloud Run IAM + ATTACH_TOKEN)

- **Cloud Run IAM** is the perimeter. If the caller doesn't have
  `roles/run.invoker`, the container never sees the request. This
  is the primary auth and you should set it up correctly.
- **ATTACH_TOKEN** is defense-in-depth. It costs nothing to leave
  enabled and protects against accidental IAM mistakes (e.g.
  granting `run.invoker` to a service account that gets compromised
  later). If you really want to drop it, omit `--attach-token` from
  the args; Cloud Run IAM is then your only gate.

## What this recipe does NOT do

| Constraint | Rationale |
|---|---|
| No public exposure | Service is `--no-allow-unauthenticated`. Add a load balancer with IAP if you need public + Google-identity-protected access. |
| No persistent session DB by default | `/tmp/sessions.db` is ephemeral. See "Persistent session DB" below for the Cloud Run Volumes upgrade (Filestore recommended, GCS as a lower-cost alternative). |
| No multi-replica | `--max-instances=1 --concurrency=1` matches core-agent's single-operator-per-instance model (v2.3). Multi-session lands in v2.4. |
| No GCP project beyond Vertex AI | Runtime SA gets `roles/aiplatform.user` only. Grant additional roles (BigQuery, GCS, etc.) explicitly per use case. |
| No Terraform | `gcloud` commands are first-class; the script is shell on purpose so you can read what it does. |
| No Agent Runtime / Agent Engine deployment | Agent Runtime requires Python. See [docs/agent-runtime-go-friction-log.md](../../docs/agent-runtime-go-friction-log.md) for the why. |

## Prerequisites

One-time setup per GCP project.

### Shell environment

```bash
export PROJECT_ID="<your-project-id>"
export REGION="us-central1"
```

### 1. Enable required APIs

```bash
gcloud services enable \
  run.googleapis.com \
  aiplatform.googleapis.com \
  artifactregistry.googleapis.com \
  cloudbuild.googleapis.com \
  secretmanager.googleapis.com \
  --project=$PROJECT_ID
```

(`cloudbuild.googleapis.com` is only needed for Path B's `--source .`
deployment; `artifactregistry.googleapis.com` + `secretmanager.googleapis.com`
are only needed for Path A. Enabling all five is the simpler default
and costs nothing.)

### 2. Runtime service account

Both paths use the same runtime SA. Create once:

```bash
gcloud iam service-accounts create core-agent-runner \
  --display-name="core-agent Cloud Run runtime" \
  --project=$PROJECT_ID

gcloud projects add-iam-policy-binding $PROJECT_ID \
  --member="serviceAccount:core-agent-runner@$PROJECT_ID.iam.gserviceaccount.com" \
  --role=roles/aiplatform.user \
  --condition=None
```

Path A's script does this automatically (idempotent). Path B users
do it manually here.

### 3. Tooling

- `gcloud` (recent — Cloud Run Volumes and the `services proxy` command
  both want a current SDK)
- `docker` (Path A only — for the pull/retag/push of the published image)
- `core-agent-tui` for attaching:
  ```bash
  go install github.com/go-steer/core-agent/cmd/core-agent-tui@latest
  ```

## Path B: quickstart (`gcloud run deploy --source .`)

From this directory:

```bash
cd examples/cloud-run-deploy

gcloud run deploy core-agent \
  --source . \
  --region=$REGION \
  --project=$PROJECT_ID \
  --service-account=core-agent-runner@$PROJECT_ID.iam.gserviceaccount.com \
  --no-allow-unauthenticated \
  --port=7777 \
  --min-instances=1 \
  --max-instances=3 \
  --cpu=2 \
  --memory=2Gi \
  --no-cpu-throttling \
  --concurrency=80 \
  --timeout=3600 \
  --set-env-vars="GOOGLE_CLOUD_PROJECT=$PROJECT_ID,GOOGLE_CLOUD_LOCATION=global,GOOGLE_GENAI_USE_VERTEXAI=1,AGENTIC_SMALL_MODEL=gemini-2.5-flash" \
  --update-secrets="ATTACH_TOKEN=core-agent-attach-token:latest"
```

The first run will prompt you to allow Cloud Build to create an
Artifact Registry repo (`cloud-run-source-deploy`) — accept. Cloud
Build will then build the Dockerfile, push, and deploy.

For the `ATTACH_TOKEN` secret, mint one before the deploy:

```bash
# `tr -d '\n'` is load-bearing: openssl rand emits a trailing
# newline that gcloud stores verbatim and Cloud Run mounts verbatim
# into the container's env var. When you later read the secret via
# `$(gcloud secrets versions access)` command substitution strips
# one trailing newline, so your local ATTACH_TOKEN ends up one byte
# shorter than the container's — and the constant-time bearer
# compare on the server fails by exactly one byte. Strip the
# newline at creation time and both sides match.
openssl rand -hex 32 | tr -d '\n' | gcloud secrets create core-agent-attach-token \
  --data-file=- --replication-policy=automatic --project=$PROJECT_ID

gcloud secrets add-iam-policy-binding core-agent-attach-token \
  --member="serviceAccount:core-agent-runner@$PROJECT_ID.iam.gserviceaccount.com" \
  --role=roles/secretmanager.secretAccessor \
  --project=$PROJECT_ID
```

That's it. The `.agents/` bundle ships baked into the image; the
ENTRYPOINT + CMD in the Dockerfile drive `core-agent` with the right
flags. To customize the agent (model, permissions, identity, etc.),
edit `.agents/config.json` or `.agents/AGENTS.md` and re-run the
deploy — Cloud Build rebuilds the image.

## Path A: production-shaped (one script)

```bash
export PROJECT_ID="<your-project-id>"
export REGION="us-central1"

cd examples/cloud-run-deploy
./scripts/deploy-from-prebuilt-image.sh
```

The script is idempotent and shows what it's doing as it goes. It:

1. Enables APIs
2. Creates the Artifact Registry repo (if missing)
3. Pulls `ghcr.io/go-steer/core-agent:main` (override via `IMAGE_REF=...` for a pinned digest), retags, pushes to AR
4. Creates the runtime service account + grants `roles/aiplatform.user`
5. Uploads `.agents/config.json` and `.agents/AGENTS.md` to Secret
   Manager as `core-agent-config-json` and `core-agent-agents-md`
6. Mints `core-agent-attach-token` (preserves on re-runs)
7. Deploys to Cloud Run with secrets mounted at
   `/etc/core-agent/.agents/`

To update config without rebuilding the image:

```bash
gcloud secrets versions add core-agent-config-json \
  --data-file=.agents/config.json --project=$PROJECT_ID

# Either redeploy to pick up the new version:
gcloud run services update core-agent --region=$REGION --project=$PROJECT_ID

# Or, if the operator is attached, hit /reload over the attach API:
curl -X POST "$ATTACH_URL/sessions/default/reload" \
  -H "Authorization: Bearer $(gcloud auth print-identity-token)" \
  -H "X-Attach-Token: $ATTACH_TOKEN"
```

The `:latest` alias on the secret mount means new revisions pick up
the newest secret version. In-place reload only works if the
mounted file path itself was updated (which Cloud Run does on
revision update, not on secret update alone).

## Verify

```bash
SERVICE_URL=$(gcloud run services describe core-agent \
  --region=$REGION --project=$PROJECT_ID --format='value(status.url)')

# Grant yourself invoke rights (one-time per operator)
gcloud run services add-iam-policy-binding core-agent \
  --region=$REGION --project=$PROJECT_ID \
  --member="user:$(gcloud config get-value account)" \
  --role=roles/run.invoker

# Discovery card with your identity token (proves IAM gate + core-agent
# are both happy)
curl -H "Authorization: Bearer $(gcloud auth print-identity-token)" \
  "$SERVICE_URL/.well-known/agent-card.json" | head -20
# expect: { "protocolVersion": "0.3.0", "name": "...", "description": "core-agent deployed on Google Cloud Run, ...", ... }

# Startup logs (listener bound, config loaded, MCP servers attached
# if any). Vertex AI auth is lazy — core-agent doesn't actually
# mint a Google token until the first model invocation, so an
# auth failure won't show here.
gcloud run services logs read core-agent \
  --region=$REGION --project=$PROJECT_ID --limit=50
```

If the discovery card returns 401 you're hitting Cloud Run IAM
(missing `roles/run.invoker`). If it returns 200 but the agent's
first reply hangs or errors with an ADC / quota / permission
message, you're hitting Vertex IAM — re-check `roles/aiplatform.user`
on the runtime SA and that `GOOGLE_GENAI_USE_VERTEXAI=1` was set.
Vertex AI auth surfaces in logs only at first model call, not at
startup.

## Attach

The recommended path uses `core-agent-tui --auth=google-id-token`,
which mints a Google ID token via Application Default Credentials
(audience = service URL) and talks to the Cloud Run service URL
directly. No proxy hop, no per-request token mint, no 429 cascades.
For end-user ADC, `gcloud auth application-default login` must be
followed up with `--impersonate-service-account=...` (see the
failure-modes table below) — `idtoken.NewTokenSource` only accepts
service-account-shaped credentials.

```bash
# One-time on the operator's workstation (skip on GCE/GKE/Cloud Run/
# Cloud Shell — ADC picks up the runtime's service account):
gcloud auth application-default login

# Fetch the attach token into your shell (for Posture A — see below):
export ATTACH_TOKEN="$(gcloud secrets versions access latest \
  --secret=core-agent-attach-token --project=$PROJECT_ID)"

# Attach. Audience derives from the URL automatically.
SERVICE_URL="$(gcloud run services describe core-agent \
  --region=$REGION --project=$PROJECT_ID --format='value(status.url)')"
core-agent-tui --auth=google-id-token --token=ATTACH_TOKEN "$SERVICE_URL"
```

**Flag-first ordering matters** — Go's `flag` package stops parsing
at the first non-flag arg, so `--auth=...` and `--token=...` MUST
come before the URL.

(`--token=ATTACH_TOKEN` is the env var NAME holding the bearer
token, not the token itself. The TUI reads `os.Getenv("ATTACH_TOKEN")`
at startup — same env-var-name-indirection pattern as the daemon's
`--attach-token`. Keeps the token off your shell history.)

### TUI binary version

`--auth=google-id-token` and `--auth=google-id-token` shipped in PR #143 (post-v2.3.1). Until
v2.4.0 cuts, build the TUI binary from main:

```bash
go install github.com/go-steer/core-agent/cmd/core-agent-tui@main
```

Once v2.4.0 ships, the `@latest` pseudo-version works.

### Posture A vs Posture B

Two daemon postures supported, pick what fits your risk tolerance:

| Posture | Daemon args | Client invocation | When to use |
|---|---|---|---|
| **A — IAM + ATTACH_TOKEN** (this recipe's default) | `--attach-token=ATTACH_TOKEN` | `--auth=google-id-token --token=ATTACH_TOKEN <url>` | Default. Defense in depth against IAM misconfig (accidental `allAuthenticatedUsers` grant, leaked invoker SA, future org-policy changes). |
| **B — IAM only** | drop `--attach-token=ATTACH_TOKEN` from the Cloud Run service config | `--auth=google-id-token <url>` (no `--token=`) | Simpler — one fewer managed secret. Sensible when IAM bindings are tightly scoped to a small group of named principals. |

### Operator attach paths

| Path | Setup | Best for |
|---|---|---|
| **`core-agent-tui --auth=google-id-token`** (recommended) | ADC configured + TUI binary including PR #143 | Production-shaped operator workflow; no proxy process to manage; SSE stays stable |
| **`gcloud run services proxy`** (legacy / convenient) | Nothing — `gcloud` SDK only. Then `core-agent-tui --token=ATTACH_TOKEN http://localhost:8080` | One-shot smoke tests, environments without a recent TUI binary. Adds 50–200ms per request and a token-mint quota; can drop SSE under chatty streams |
| **Direct + `print-identity-token`** | Caller adds `Authorization: Bearer $(gcloud auth print-identity-token --audiences=$SERVICE_URL)` to every request | Scripting / cron; integration tests outside the TUI |
| **Cloud Workstations** | Provision one workstation; attach from its terminal | If you already use Cloud Workstations for everything else |

#### Legacy: `gcloud run services proxy`

Still works and useful when the local TUI binary predates PR #143:

```bash
# Proxy. Leaves a local server at http://localhost:8080
gcloud run services proxy core-agent \
  --region=$REGION --project=$PROJECT_ID &

export ATTACH_TOKEN="$(gcloud secrets versions access latest \
  --secret=core-agent-attach-token --project=$PROJECT_ID)"

core-agent-tui --token=ATTACH_TOKEN http://localhost:8080
```

The proxy transparently injects `Authorization: Bearer <ID-token>`
and forwards the operator's `--token=` as-is. Compatible with any
TUI version. Tradeoffs: per-request latency, token-mint quota
pressure under chatty SSE (see [#135](https://github.com/go-steer/core-agent/issues/135)), one more process to manage.

## Rotating the ATTACH_TOKEN

Two real gotchas live here, both worth knowing before you need to
rotate in a hurry:

```bash
# 1. New secret VERSION. tr -d '\n' is load-bearing (see the deploy
#    section for why); without it the container's value ends up one
#    byte longer than yours and bearer compare fails by one byte.
openssl rand -hex 32 | tr -d '\n' | gcloud secrets versions add \
  core-agent-attach-token --data-file=- --project=$PROJECT_ID

# 2. Force a new Cloud Run revision so :latest resolves to the new
#    secret version. The existing revision is PINNED to whatever
#    version was :latest at its creation time — adding a new secret
#    version does not affect running revisions.
#
#    Cloud Run's `services update` refuses no-op calls, and
#    re-specifying `--update-secrets=...:latest` is treated as a
#    no-op because the configured value didn't change (even though
#    the resolved value would). The documented workaround is a
#    label bump — a trivial change that triggers a new revision:
gcloud run services update core-agent \
  --region=$REGION --project=$PROJECT_ID \
  --update-labels="secret-rotation=$(date +%s)"

# Deterministic alternative: pin to a specific version number
# instead of :latest. Then rotation = bump the version number:
#   gcloud run services update core-agent --region=$REGION \
#     --project=$PROJECT_ID \
#     --update-secrets="ATTACH_TOKEN=core-agent-attach-token:2"

# 3. Re-export your local value and retry the attach.
export ATTACH_TOKEN="$(gcloud secrets versions access latest \
  --secret=core-agent-attach-token --project=$PROJECT_ID)"
```

To sanity-check before retrying the TUI, verify the local and
container values match (the `\ No newline at end of file` marker
in diff output is exactly the bug that bites without `tr -d '\n'`):

```bash
diff <(gcloud secrets versions access latest --secret=core-agent-attach-token --project=$PROJECT_ID) <(echo -n "$ATTACH_TOKEN")
# expect: no output (files identical)
```

## Tuning

### Variant — Anthropic Claude on Vertex AI

Edit `.agents/config.json` (or the Secret Manager version for Path A):

```json
"model": {
  "provider": "anthropic-vertex",
  "name": "claude-opus-4-7",
  "anthropic": {
    "vertex": {
      "project": "YOUR_PROJECT",
      "location": "us-east5"
    }
  }
}
```

Same SA + `roles/aiplatform.user` covers both Gemini and Claude on
Vertex. Redeploy (or `gcloud secrets versions add` + revision
update for Path A).

### Variant — enable plan-first gating

Edit `.agents/config.json` permissions block:

```json
"permissions": {
  "mode": "ask",
  "require_plan_artifact": true,
  "allow": [...]
}
```

See `examples/plan-first/` for the full plan-first recipe — this
variant is one config switch on top of the standard `cloud-run-deploy`.

### Variant — slim image

If you don't need the in-process TUI (you only attach via remote
`core-agent-tui`), swap the base image in the Dockerfile (Path B)
or change `IMAGE_TAG` to a slim variant in Path A's script:

```dockerfile
ARG CORE_AGENT_VERSION=2.3.1
FROM ghcr.io/go-steer/core-agent-slim:${CORE_AGENT_VERSION}
```

~5MB smaller; same attach API.

### Persistent session DB (Cloud Run Volumes)

Default recipe uses `/tmp/sessions.db`, which is lost on cold start.
Cloud Run Volumes support three backends that work for persistence;
pick by write pattern:

| Backend | Best for SQLite session DB? | Setup cost |
|---|---|---|
| **Filestore (NFS)** ✅ recommended | Yes — real POSIX semantics, proper `fsync`, no fuse | VPC connector or Direct VPC egress; ~$0.20/GiB/month minimum 1 TiB instance |
| **Cloud Storage (gcsfuse)** | Workable but slow — SQLite's many small writes + `fsync` round-trip via fuse | Just the bucket; lowest cost |
| **In-memory (tmpfs)** | No — defeats the purpose | Free, but ephemeral |

For very write-heavy workloads (background-agent fan-out, dense
event logging), GKE with a real RWO PVC (`examples/gke-deploy/`)
is still the better fit. Cloud Run + Filestore handles a single
operator's typical session load comfortably.

#### Option A — Filestore (recommended for SQLite)

```bash
# 1. Filestore instance. The BASIC_HDD tier is the cheapest and
# adequate for a SQLite session DB. Minimum size is 1 TiB.
# Pick a zone in your Cloud Run region.
gcloud filestore instances create core-agent-sessions \
  --zone=${REGION}-a \
  --tier=BASIC_HDD \
  --file-share=name=share1,capacity=1TiB \
  --network=name=default

# Grab the instance's IP (Cloud Run Volumes need it explicitly)
FILESTORE_IP=$(gcloud filestore instances describe core-agent-sessions \
  --zone=${REGION}-a --format='value(networks[0].ipAddresses[0])')

# 2. Cloud Run needs VPC egress to reach Filestore. Direct VPC
# egress (no connector required) is the modern path:
gcloud run services update core-agent \
  --region=$REGION --project=$PROJECT_ID \
  --network=default --subnet=default --vpc-egress=private-ranges-only \
  --add-volume="name=data,type=nfs,location=${FILESTORE_IP}:/share1" \
  --add-volume-mount="volume=data,mount-path=/data" \
  --args="--no-repl,--attach-listen=:7777,--attach-token=ATTACH_TOKEN,--session-db,--session-db-path=/data/sessions.db,--agentic-tools,-c,/etc/core-agent/.agents/config.json"
```

The 1 TiB minimum is wasteful for a single agent — share the
instance across multiple agents (point each one at a different
subdirectory under `/share1`) or use the Filestore Zonal tier if
you need smaller capacity.

If your org enforces "no public IPs on Filestore," the Cloud Run
service must use a VPC the Filestore instance is reachable from;
the `--network` + `--subnet` flags above pin the egress
accordingly. Verify Filestore + Cloud Run are in the same region.

#### Option B — Cloud Storage (lower setup, slower writes)

```bash
gcloud storage buckets create gs://${PROJECT_ID}-core-agent-data \
  --location=$REGION --uniform-bucket-level-access

gcloud storage buckets add-iam-policy-binding \
  gs://${PROJECT_ID}-core-agent-data \
  --member="serviceAccount:core-agent-runner@$PROJECT_ID.iam.gserviceaccount.com" \
  --role=roles/storage.objectUser

gcloud run services update core-agent \
  --region=$REGION --project=$PROJECT_ID \
  --add-volume="name=data,type=cloud-storage,bucket=${PROJECT_ID}-core-agent-data" \
  --add-volume-mount="volume=data,mount-path=/data" \
  --args="--no-repl,--attach-listen=:7777,--attach-token=ATTACH_TOKEN,--session-db,--session-db-path=/data/sessions.db,--agentic-tools,-c,/etc/core-agent/.agents/config.json"
```

Cloud Run Volumes for GCS uses gcsfuse. SQLite writes work but
each `fsync` is a round-trip through fuse + GCS — you'll see
visible lag on chatty sessions. Fine for cold-archive durability;
not great as a live session DB. Use Filestore (option A) if
session perf matters.

### Concurrent operators

`--concurrency=1 --max-instances=1` is the v2.3 single-operator
default. To support multiple operators TODAY (without waiting for
v2.4's multi-session work), the simplest pattern is one Cloud Run
service per operator:

```bash
# Per operator:
gcloud run deploy "core-agent-${OPERATOR_NAME}" --source . ...
```

Each gets isolated session DB + permissions. Cheap on Cloud Run.

## Reload config without revision deploy

The attach API exposes `/sessions/{sid}/reload` which re-reads
`instruction.Load` and `skills.LoadAll` without a process restart:

```bash
curl -X POST "$SERVICE_URL/sessions/default/reload" \
  -H "Authorization: Bearer $(gcloud auth print-identity-token --audiences=$SERVICE_URL)" \
  -H "X-Attach-Token: $ATTACH_TOKEN"
```

This works for changes to AGENTS.md, skill bundles, and the
in-process portions of config.json. For changes to model provider
or MCP server definitions, a revision deploy is still needed (live
MCP server restart lands in v2.4).

## Teardown

```bash
gcloud run services delete core-agent \
  --region=$REGION --project=$PROJECT_ID --quiet

# Path A also cleans up the Secret Manager + AR artifacts:
for secret in core-agent-attach-token core-agent-config-json core-agent-agents-md; do
  gcloud secrets delete $secret --project=$PROJECT_ID --quiet 2>/dev/null || true
done

gcloud artifacts repositories delete core-agent \
  --location=$REGION --project=$PROJECT_ID --quiet 2>/dev/null || true

# Revoke runtime SA roles + delete SA
gcloud projects remove-iam-policy-binding $PROJECT_ID \
  --member="serviceAccount:core-agent-runner@$PROJECT_ID.iam.gserviceaccount.com" \
  --role=roles/aiplatform.user --condition=None --quiet

gcloud iam service-accounts delete \
  core-agent-runner@$PROJECT_ID.iam.gserviceaccount.com \
  --project=$PROJECT_ID --quiet
```

If you used Path B's `--source .`, also clean up the Cloud Build-
provisioned AR repo if you don't want it lingering:

```bash
gcloud artifacts repositories delete cloud-run-source-deploy \
  --location=$REGION --project=$PROJECT_ID --quiet 2>/dev/null || true
```

## Compose with the rest of the substrate

- **Plan-first** (`examples/plan-first/`): set `require_plan_artifact: true`
  in this recipe's `config.json`; gate-level enforcement of
  "record_plan before any mutating tool."
- **GKE** (`examples/gke-deploy/`): the same `.agents/` bundle works
  on GKE if you outgrow Cloud Run's per-request timeout or want a
  real PVC for the session DB.
- **Why not Agent Runtime?** Agent Runtime requires Python at every
  layer (managed templates are cloudpickle-based; "BYOC" is Pre-GA
  + Python-only per Google docs). The friction log at
  [docs/agent-runtime-go-friction-log.md](../../docs/agent-runtime-go-friction-log.md)
  captures the full research thread. Cloud Run is the
  Google-blessed Go path today.

## Image identity + supply-chain trust

Verify the image you're running (applies to both paths — Path A
mirrors the published image bit-for-bit; Path B layers on top of it):

```bash
docker pull ghcr.io/go-steer/core-agent:2.3.1
docker run --rm ghcr.io/go-steer/core-agent:2.3.1 --version
# expect: core-agent v2.3.1 (commit ..., built ...)

cosign verify ghcr.io/go-steer/core-agent:2.3.1 \
  --certificate-identity-regexp '^https://github.com/go-steer/core-agent' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

For Path A, pin the AR-mirrored image to a digest for revision-deterministic
deploys:

```bash
docker buildx imagetools inspect \
  ${REGION}-docker.pkg.dev/${PROJECT_ID}/core-agent/core-agent:2.3.1 \
  | grep Digest
# Use the digest as IMAGE_TAG in the script for fully-pinned deploys.
```
