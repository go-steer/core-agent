# `gke-deploy` — config recipe for deploying core-agent to GKE

A complete, drop-in recipe for running `core-agent` as a long-lived
pod in a GKE cluster, reachable by operators over an **internal**
HTTP LoadBalancer, with **Workload Identity Federation for GKE**
(direct binding — no Google Service Account in the middle) for
credential-free auth to Vertex AI + the GKE read-only MCP server,
and registered with Google Cloud's Agent Registry.

**No Dockerfile in this recipe** — uses the published
`ghcr.io/go-steer/core-agent:2.3.1` image (multi-arch amd64+arm64,
distroless static, Sigstore signed). The recipe is YAML + a
`.agents/` config bundle; deploy time is ~5 minutes after the
prereqs are in place.

## Architecture

```
[Operator workstation in the VPC]
  core-agent-tui http://10.x.x.x:7777 --attach-token=<from-secret>
       │
       │ HTTP/SSE (Authorization: Bearer ...)
       ▼
[Internal LoadBalancer — networking.gke.io/load-balancer-type: "Internal"]
       │  ClusterIP: 10.x.x.x; reachable only from inside the VPC
       ▼
[Service core-agent.agent-system.svc:7777]
       │
       ▼
[Deployment core-agent (1 replica, Recreate strategy)]
       ├── serviceAccountName: core-agent
       │   └── KSA principal (no GSA!) — bound directly via WIF for GKE:
       │       principal://iam.googleapis.com/projects/<NUM>/locations/global/
       │         workloadIdentityPools/<PROJECT>.svc.id.goog/subject/ns/agent-system/sa/core-agent
       │       ├── roles/aiplatform.user          → Vertex AI inference
       │       ├── roles/mcp.toolUser             → invoke MCP server endpoints
       │       └── roles/container.clusterViewer  → GKE read-only state (via MCP)
       │
       ├── ConfigMap mount /opt/data/.agents/
       │   ├── config.json    (model + permissions + agent.description)
       │   ├── mcp.json       (GKE read-only MCP server)
       │   └── AGENTS.md      (instruction priming)
       │
       ├── PVC mount /opt/data/
       │   ├── sessions.db        (eventlog + session state, durable across restarts)
       │   └── .agents/plans/     (if plan-first is enabled)
       │
       ├── CSI mount /var/run/secrets/workload-spiffe-credentials/
       │   └── SPIFFE x509 SVID + trust bundle, auto-rotated
       │       (GKE Managed Workload Identity → future mTLS use)
       │
       ├── Secret env ATTACH_TOKEN  → --attach-token=ATTACH_TOKEN
       │
       ├── Serves /.well-known/agent-card.json on :7777
       │       → A2A AgentCard; opt-in via config.json's agent.description
       │
       └── annotations:
           ├── apphub.cloud.google.com/functional-type: "AGENT"
           │     → registers with Google Cloud Agent Registry on apply
           ├── a2a-protocol.org/agent-card
           │     → tells Registry where to fetch the discovery card
           └── iam.gke.io/identity-type + iam.gke.io/identity
                 → opts pod into GKE Managed WI for SPIFFE cert issuance
```

### Discovery + agent identity

Two opt-in integrations layered on top of the core deploy:

- **A2A AgentCard at `/.well-known/agent-card.json`.** Always
  unauthenticated, served from the attach listener whenever
  `agent.description` is set in `config.json` (this recipe sets it).
  The `a2a-protocol.org/agent-card` annotation on the Deployment
  tells [Google Cloud Agent Registry](https://docs.cloud.google.com/agent-registry/register-agents)
  where to fetch it. Skills auto-derive from any `.agents/skills/`
  bundles; curated extras can be added via `.agents/agent-card.json`.
  See `docs/agent-card-design.md` for the full design.
- **GKE Managed Workload Identity.** The `iam.gke.io/identity*`
  annotations + `podcertificate.gke.io` CSI volume layer SPIFFE
  x509 SVID issuance on top of WIF for GKE. Certs land in
  `/var/run/secrets/workload-spiffe-credentials/` and rotate
  automatically. Useful for mTLS to other agents today; the
  on-ramp to Google Cloud Agent Identity once it's GA. Setup:
  see [GKE Managed Workload Identity docs](https://docs.cloud.google.com/iam/docs/create-managed-workload-identities-gke).
  The trust-domain segment in the `iam.gke.io/identity` annotation
  (`agents.global.org-<NUMBER>.system.id.goog`) is org-specific —
  replace with your agent identity pool's identifier before applying.

## What this recipe does NOT do

| Constraint | Rationale |
|---|---|
| No public exposure | Service is internal-only; operators must attach from inside the VPC. Add an IAP-protected Ingress if external access is needed. |
| No cluster mutation | KSA principal has `container.clusterViewer` only — the agent can read cluster + workload state but cannot mutate. Add `roles/container.developer` (and call the non-read-only MCP endpoint) if you need write capability. |
| No GCP project beyond Vertex + GKE | GSA roles are tight. Add specific roles (e.g. `cloudsql.client`, `monitoring.viewer`) only when a use case calls for them. |
| One replica only | Session DB on `ReadWriteOnce` PVC — multi-replica needs `ReadWriteMany` storage + multi-session daemon (task #12, v2.4). |
| Plan-first OFF by default | Simpler first-run; operator flips `permissions.require_plan_artifact: true` in the ConfigMap to enable. |
| One operator's perspective | Single-session daemon for v2.3. Per-user sessions land in v2.4 (PR #105). |

## Prerequisites

These are one-time-per-project setup steps. Skip individual items if you've already done them for another deployment.

### Shell environment (used in the commands below)

```bash
export PROJECT_ID="<your-project-id>"
export PROJECT_NUMBER="$(gcloud projects describe $PROJECT_ID --format='value(projectNumber)')"
export REGION="us-central1"
```

You need **both** the textual project ID (`my-project-xyz`) AND the numeric project number (`123456789012`). The WIF principal identifier uses them in different positions and getting them swapped is a common cause of bindings that look correct but never authorize.

### 1. Enable required APIs

```bash
gcloud services enable \
  container.googleapis.com \
  iamcredentials.googleapis.com \
  aiplatform.googleapis.com \
  cloudresourcemanager.googleapis.com \
  agentregistry.googleapis.com \
  --project=$PROJECT_ID
```

(`iamcredentials.googleapis.com` is required for the WIF token-exchange path; the others are GKE/inference/registry plumbing.)

### 2. GKE cluster with Workload Identity Federation for GKE enabled

New cluster (Standard mode; Autopilot has WIF on by default):

```bash
gcloud container clusters create core-agent-host \
  --location=$REGION \
  --release-channel=stable \
  --num-nodes=1 \
  --machine-type=e2-medium \
  --workload-pool=$PROJECT_ID.svc.id.goog
```

Existing Standard cluster — enable WIF on the cluster AND make sure node pools use the GKE metadata server:

```bash
gcloud container clusters update <CLUSTER_NAME> \
  --location=$REGION \
  --workload-pool=$PROJECT_ID.svc.id.goog

# For each existing node pool:
gcloud container node-pools update <POOL_NAME> \
  --cluster=<CLUSTER_NAME> \
  --location=$REGION \
  --workload-metadata=GKE_METADATA
```

(Autopilot has both on by default; skip for Autopilot clusters.)

### 3. IAM bindings for the KSA principal

WIF for GKE *direct binding* — no Google Service Account, no `iam.workloadIdentityUser` binding. The KSA principal IS the IAM member.

Three roles for the default recipe:

| Role | Purpose |
|---|---|
| `roles/aiplatform.user` | Vertex AI inference (call Gemini Pro + Flash) |
| `roles/mcp.toolUser` | Invoke the GKE remote MCP server's tool calls at all |
| `roles/container.clusterViewer` | Actually read GKE cluster + workload state through those MCP tools |

The MCP role + the cluster-viewer role are BOTH required for the GKE MCP server to work — `mcp.toolUser` is permission to call any MCP tool, `container.clusterViewer` is permission to do the underlying read against GKE. Missing either gives 403 with no specific hint about which one.

```bash
# WIF for GKE direct-binding member identifier:
#   principal://iam.googleapis.com/projects/<NUMBER>/locations/global/workloadIdentityPools/<ID>.svc.id.goog/subject/ns/<NS>/sa/<KSA>
#
# Note: <NUMBER> = numeric project NUMBER (in the projects/... path);
#       <ID>     = textual project ID (in the workload pool name).
KSA_PRINCIPAL="principal://iam.googleapis.com/projects/$PROJECT_NUMBER/locations/global/workloadIdentityPools/$PROJECT_ID.svc.id.goog/subject/ns/agent-system/sa/core-agent"

for role in roles/aiplatform.user roles/mcp.toolUser roles/container.clusterViewer; do
  gcloud projects add-iam-policy-binding $PROJECT_ID \
    --role="$role" \
    --member="$KSA_PRINCIPAL" \
    --condition=None
done
```

That's it for IAM. The `principal://` member string is the WIF-for-GKE identifier; bind any role to it on any resource using the same `--member=...` flag.

**Multi-project inspection:** if you want the agent to inspect clusters in projects OTHER than the deployment's home project, re-run the loop against each target project — the KSA principal stays the same; only the project receiving the binding changes. The `mcp.toolUser` binding is one-time on the deployment's home project (it gates calling the MCP server at all), but `container.clusterViewer` needs to be granted on every project you want to read.

**Renaming the KSA?** If your overlay uses `namePrefix:` to rename `core-agent` to something else (e.g. `env-prod-core-agent`), or `namespace:` to deploy somewhere other than `agent-system`, the `principal://...` member string above must use the matching name + namespace. Mismatched bindings look fine to gcloud but the pod's runtime token exchange returns "permission denied."

### 4. `kubectl` credentials for the cluster

```bash
gcloud container clusters get-credentials core-agent-host \
  --location=$REGION \
  --project=$PROJECT_ID
```

### 5. Tooling on your workstation

- `gcloud` (recent)
- `kubectl` (matching cluster version)
- `core-agent-tui` for attaching:
  ```bash
  go install github.com/go-steer/core-agent/cmd/core-agent-tui@latest
  ```
  or pull the container image:
  ```bash
  docker pull ghcr.io/go-steer/core-agent-tui:2.3.1
  ```

## Setup

The IAM + cluster work above happens once. The steps below run per-deployment (or per-overlay, if you're managing multiple).

### Step 1 — Create your overlay with project + region values

The `deploy/` tree is split into a clean `base/` (recipe defaults; not
meant for direct apply) and an example overlay you copy + customize:

```bash
cp -r examples/gke-deploy/deploy/overlays/example \
      examples/gke-deploy/deploy/overlays/my-prod
```

Then edit ONE file:

```bash
$EDITOR examples/gke-deploy/deploy/overlays/my-prod/patch-deployment.yaml
#    → set GOOGLE_CLOUD_PROJECT + GOOGLE_CLOUD_LOCATION
```

That's the required setup. The base's `config.json` omits the `model.vertex` block by design — core-agent's Gemini provider falls back to `GOOGLE_CLOUD_PROJECT` / `GOOGLE_CLOUD_LOCATION` env vars at runtime, so the Deployment is the single source of truth for project + region. No JSON to edit, no sync risk.

The overlay's `kustomization.yaml` has commented-out blocks for the other common knobs — `namePrefix` (rename KSA + other resources), `namespace` override, image-tag pin, image pull secret for private GHCR, resource limit overrides. `patch-deployment.yaml` also has a commented-out `AGENTIC_SMALL_MODEL` env var if you want to override the cost-routing default (Flash; ~5-10× cheaper per turn for MCP-heavy work).

See `overlays/example/README.md` for the full overlay workflow including the "advanced: customize the ConfigMap" path.

### Step 2 — Create the attach-token Secret

```bash
kubectl create namespace agent-system  # if not yet applied

kubectl create secret generic core-agent \
  --namespace agent-system \
  --from-literal=attach-token="$(openssl rand -hex 32)"
```

(Or use the `20-secret.yaml.example` file as a template — but `kubectl create secret` is preferred since the token never lives in a file you might accidentally commit.)

### Step 3 — Apply

```bash
# Sanity-check the rendered output first (catches edit mistakes
# without touching the cluster)
kubectl kustomize examples/gke-deploy/deploy/overlays/my-prod

# Then apply for real
kubectl apply -k examples/gke-deploy/deploy/overlays/my-prod
```

This creates the Namespace, KSA, ConfigMap (with your edited contents), PVC, Deployment, and Service in dependency order. The Deployment's annotation registers the agent with the Agent Registry on apply.

### Step 4 — Verify

```bash
# Pod up
kubectl get pods -n agent-system -l app=core-agent
# expect: 1/1 Running

# Internal LB has an address (may take a minute to provision)
kubectl get svc -n agent-system core-agent
# expect EXTERNAL-IP column shows a 10.x.x.x address

# Logs report successful startup + Vertex AI auth via WIF
kubectl logs -n agent-system -l app=core-agent --tail=50

# Discovery card is live (proves agent.description wired through
# and Agent Registry has something to index)
kubectl port-forward -n agent-system svc/core-agent 7777:7777 &
curl -s http://localhost:7777/.well-known/agent-card.json | head -20
# expect: { "protocolVersion": "0.3.0", "name": "...", "description": "Platform agent for managing GKE clusters", ... }
```

If you see auth errors in the logs, re-check:
- IAM binding uses the **principal://** member format, NOT `serviceAccount:...svc.id.goog[...]` (that's the legacy GSA-impersonation format)
- Project NUMBER vs project ID are in the right positions in the `principal://` string (`projects/<NUMBER>/.../workloadIdentityPools/<ID>.svc.id.goog/...`) — swapping them is the most common cause of bindings that authorize-as-nobody
- Namespace + KSA name in the principal string match the YAMLs (`agent-system` / `core-agent`)
- Cluster has WIF enabled (`gcloud container clusters describe <name> --format='value(workloadIdentityConfig.workloadPool)'` should print `<PROJECT_ID>.svc.id.goog`)
- Standard cluster's node pools use the GKE metadata server (`gcloud container node-pools describe <pool> --cluster=<cluster> --location=<region> --format='value(config.workloadMetadataConfig.mode)'` should print `GKE_METADATA`)
- `iamcredentials.googleapis.com` API is enabled on the project

If GKE MCP tool calls return **403 Forbidden** (auth to the daemon works; auth from the agent to the MCP server fails), re-check:
- Both `roles/mcp.toolUser` AND `roles/container.clusterViewer` are bound to the KSA principal. Missing either gives 403 with no specific hint about which one.
- For inspecting clusters in projects OTHER than the deployment's home project, the target project must have its own `container.clusterViewer` binding for the same KSA principal.

## Attach

From inside the VPC (Cloud Workstations, IAP tunnel, VPN):

```bash
export ATTACH_URL="http://$(kubectl get svc -n agent-system core-agent \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}'):7777"
export ATTACH_TOKEN="$(kubectl get secret -n agent-system core-agent \
  -o jsonpath='{.data.attach-token}' | base64 -d)"

core-agent-tui --token=ATTACH_TOKEN "$ATTACH_URL"
```

**Flag-first ordering matters** — Go's `flag` package stops parsing at the first non-flag arg, so any flag placed AFTER `$ATTACH_URL` is silently ignored (treated as a second positional). `--token=ATTACH_TOKEN` MUST come before the URL.

(`--token=ATTACH_TOKEN` is the env var NAME holding the bearer token. The TUI reads `os.Getenv("ATTACH_TOKEN")` at startup — same env-var-name pattern as the daemon's `--attach-token`. Keeps the token off your shell history.)

### Operator attach paths (pick one)

| Path | Setup effort | Best for |
|---|---|---|
| **Cloud Workstations** (recommended) | Provision one workstation in the same VPC; attach from its terminal | Google Cloud customers; no extra networking |
| **IAP tunnel** | `gcloud compute start-iap-tunnel <bastion-vm> 7777 --local-host-port=localhost:7777` then attach to `localhost:7777` | One-off ops access; no persistent workstation |
| **VPN** | Cloud VPN or partner VPN to your VPC; attach from your laptop | Operators who already have VPN setup |
| **`kubectl port-forward`** | `kubectl port-forward -n agent-system svc/core-agent 7777:7777`; attach to `localhost:7777` | Quick debug from a workstation that already has cluster `kubectl` creds |

## Tuning

### Variant — Anthropic Claude on Vertex AI

Edit `30-configmap.yaml`'s `config.json` to swap the model provider:

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

Same GSA + `roles/aiplatform.user` covers both Gemini and Claude — Vertex AI is the gating IAM. Apply: `kubectl apply -f 30-configmap.yaml` then `kubectl rollout restart deployment/core-agent -n agent-system`.

### Variant — Enable plan-first gating

Edit `config.json` permissions block:

```json
"permissions": {
  "mode": "ask",
  "require_plan_artifact": true,
  "allow": [...]
}
```

Then trigger a `/reload` from the TUI (or restart the pod). The agent now denies mutating tool calls until `record_plan` is invoked.

See `examples/plan-first/` for the full plan-first recipe (this variant is one config switch on top of the standard `gke-deploy`).

### Variant — Slim image

If the in-process TUI is dead weight (you only attach via remote `core-agent-tui`), swap the image for the slim variant:

```yaml
image: ghcr.io/go-steer/core-agent-slim:2.3.1
```

~5MB smaller binary; same runtime behavior for the attach API.

### Scale resource limits

The defaults (500m CPU request, 2 CPU limit, 512Mi → 2Gi memory) suit a single operator driving moderate tool use. Bump for:

- **Heavier MCP tools** (e.g. agents that pull large GKE log pages): increase CPU + memory; consider `pd-ssd` for the PVC if session DB writes become a bottleneck
- **Autonomous mode** with long-running goals: bump CPU to handle parallel tool calls under sustained load
- **Multiple concurrent operators** (when v2.4 multi-session lands): scale memory proportional to expected concurrent sessions

### Add MCP servers

Drop additional server definitions into `mcp.json`. Each server's tool set namespaces under `<server>.<tool>`. For Google Cloud MCP servers needing different scopes, the existing GSA may need additional roles — grant explicitly per server.

## Reload config without pod restart

After editing the ConfigMap:

```bash
kubectl edit configmap core-agent-config -n agent-system
# make your edits

# trigger a reload via the TUI (/reload slash command)
# or via the attach API:
curl -X POST "$ATTACH_URL/sessions/default/reload" \
  -H "Authorization: Bearer $ATTACH_TOKEN"
```

The agent re-reads `instruction.Load`, `skills.LoadAll`, and verifies `mcp.json` parses. MCP server processes themselves keep running — for live MCP server restart, you currently need a pod restart (`kubectl rollout restart deployment/core-agent -n agent-system`). Tracked for v2.4.

## Teardown

```bash
kubectl delete -k examples/gke-deploy/deploy/overlays/my-prod
kubectl delete pvc -n agent-system core-agent-data  # explicit; kustomize doesn't track it
kubectl delete secret -n agent-system core-agent
kubectl delete namespace agent-system

# Revoke the direct IAM bindings. No GSA to delete — direct
# binding means there was nothing intermediate to clean up.
export PROJECT_NUMBER="$(gcloud projects describe $PROJECT_ID --format='value(projectNumber)')"
KSA_PRINCIPAL="principal://iam.googleapis.com/projects/$PROJECT_NUMBER/locations/global/workloadIdentityPools/$PROJECT_ID.svc.id.goog/subject/ns/agent-system/sa/core-agent"

for role in roles/aiplatform.user roles/mcp.toolUser roles/container.clusterViewer; do
  gcloud projects remove-iam-policy-binding $PROJECT_ID \
    --role="$role" \
    --member="$KSA_PRINCIPAL" \
    --condition=None
done

# If you also granted container.clusterViewer on additional
# projects for multi-project inspection, revoke those too:
#   for proj in proj-a proj-b proj-c; do
#     gcloud projects remove-iam-policy-binding $proj \
#       --role=roles/container.clusterViewer --member="$KSA_PRINCIPAL" --condition=None
#   done
```

## Compose with the rest of the substrate

- **Plan-first** (`examples/plan-first/`): set `require_plan_artifact: true` in this recipe's `config.json`; gate-level enforcement of "record_plan before any mutating tool."
- **Parallel investigation** (`examples/gke-parallel-triage/`): the GKE MCP server is already wired here; the AGENTS.md priming is what makes the agent fan out. Drop the relevant guidance into a new `.agents/AGENTS.d/00-triage.md` overlay.
- **Multi-file instructions** (v2.3 loader): add `.agents/AGENTS.d/*.md` overlays for role-specialized priming. Files load lexically after the primary AGENTS.md.
- **kube-agents Platform Agent** shape (see `docs/kube-agents-platform-fit.md`): this recipe is the runtime layer; layering the kube-agents `SOUL.md` + governance SOPs gives you a platform-agent deployment.

## Image identity + supply-chain trust

Verify the image you're running:

```bash
docker pull ghcr.io/go-steer/core-agent:2.3.1
docker run --rm ghcr.io/go-steer/core-agent:2.3.1 --version
# expect: core-agent v2.3.1 (commit ..., built ...)

cosign verify ghcr.io/go-steer/core-agent:2.3.1 \
  --certificate-identity-regexp '^https://github.com/go-steer/core-agent' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The image is published from this repo's GitHub Actions workflow + signed via Sigstore keyless. Operators in regulated environments can pin to a digest:

```bash
# Resolve once, pin forever in your manifest
docker buildx imagetools inspect ghcr.io/go-steer/core-agent:2.3.1 | grep Digest
# → image: ghcr.io/go-steer/core-agent@sha256:...
```
