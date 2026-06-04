# `gke-deploy` — config recipe for deploying core-agent to GKE

A complete, drop-in recipe for running `core-agent` as a long-lived
pod in a GKE cluster, reachable by operators over an **internal**
HTTP LoadBalancer, with Workload Identity Federation for credential-
free auth to Vertex AI + the GKE read-only MCP server, and
registered with Google Cloud's Agent Registry.

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
       │   └── KSA annotated → GSA core-agent@PROJECT.iam.gserviceaccount.com
       │       ├── roles/aiplatform.user      → Vertex AI inference
       │       └── roles/container.viewer     → GKE MCP read-only tools
       │
       ├── ConfigMap mount /opt/data/.agents/
       │   ├── config.json    (model + permissions)
       │   ├── mcp.json       (GKE read-only MCP server)
       │   └── AGENTS.md      (instruction priming)
       │
       ├── PVC mount /opt/data/
       │   ├── sessions.db        (eventlog + session state, durable across restarts)
       │   └── .agents/plans/     (if plan-first is enabled)
       │
       ├── Secret env ATTACH_TOKEN  → --attach-token=ATTACH_TOKEN
       │
       └── annotation apphub.cloud.google.com/functional-type: "AGENT"
                     → registers with Google Cloud Agent Registry on apply
```

## What this recipe does NOT do

| Constraint | Rationale |
|---|---|
| No public exposure | Service is internal-only; operators must attach from inside the VPC. Add an IAP-protected Ingress if external access is needed. |
| No cluster mutation | GSA has `container.viewer` only — the agent can read its own cluster but cannot mutate. Add `container.developer` if you need write capability. |
| No GCP project beyond Vertex + GKE | GSA roles are tight. Add specific roles (e.g. `cloudsql.client`, `monitoring.viewer`) only when a use case calls for them. |
| One replica only | Session DB on `ReadWriteOnce` PVC — multi-replica needs `ReadWriteMany` storage + multi-session daemon (task #12, v2.4). |
| Plan-first OFF by default | Simpler first-run; operator flips `permissions.require_plan_artifact: true` in the ConfigMap to enable. |
| One operator's perspective | Single-session daemon for v2.3. Per-user sessions land in v2.4 (PR #105). |

## Prerequisites

1. **GCP project** with these APIs enabled:

   ```bash
   gcloud services enable \
     container.googleapis.com \
     artifactregistry.googleapis.com \
     aiplatform.googleapis.com \
     cloudresourcemanager.googleapis.com \
     agentregistry.googleapis.com \
     --project=$PROJECT_ID
   ```

2. **GKE cluster with Workload Identity Federation enabled.**

   New cluster (Standard mode; Autopilot has WIF on by default):

   ```bash
   gcloud container clusters create core-agent-host \
     --location=$REGION \
     --release-channel=stable \
     --num-nodes=1 \
     --machine-type=e2-medium \
     --workload-pool=$PROJECT_ID.svc.id.goog
   ```

   Existing cluster — enable WIF:

   ```bash
   gcloud container clusters update <CLUSTER_NAME> \
     --location=$REGION \
     --workload-pool=$PROJECT_ID.svc.id.goog
   ```

3. **`kubectl` credentials** for the cluster:

   ```bash
   gcloud container clusters get-credentials core-agent-host \
     --location=$REGION \
     --project=$PROJECT_ID
   ```

4. **Tooling on your workstation:**
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

### Step 1 — Create the Google Service Account + grant roles

```bash
export PROJECT_ID="<your-project-id>"
export REGION="us-central1"

gcloud iam service-accounts create core-agent \
  --project=$PROJECT_ID \
  --display-name="core-agent on GKE"

gcloud projects add-iam-policy-binding $PROJECT_ID \
  --member="serviceAccount:core-agent@$PROJECT_ID.iam.gserviceaccount.com" \
  --role="roles/aiplatform.user"

gcloud projects add-iam-policy-binding $PROJECT_ID \
  --member="serviceAccount:core-agent@$PROJECT_ID.iam.gserviceaccount.com" \
  --role="roles/container.viewer"
```

### Step 2 — Bind the KSA → GSA via Workload Identity Federation

```bash
gcloud iam service-accounts add-iam-policy-binding \
  core-agent@$PROJECT_ID.iam.gserviceaccount.com \
  --project=$PROJECT_ID \
  --role="roles/iam.workloadIdentityUser" \
  --member="serviceAccount:$PROJECT_ID.svc.id.goog[agent-system/core-agent]"
```

The member string `serviceAccount:$PROJECT_ID.svc.id.goog[NAMESPACE/KSA_NAME]` is the WIF identifier — keep `agent-system` (namespace) and `core-agent` (KSA name) consistent with the YAMLs.

### Step 3 — Substitute your project ID into the manifests

The YAMLs in `deploy/` ship with `PROJECT_ID` and `us-central1` as
placeholders. Replace them with your values:

```bash
cd examples/gke-deploy/deploy
sed -i "s/PROJECT_ID/$PROJECT_ID/g" 10-serviceaccount.yaml 30-configmap.yaml 50-deployment.yaml
# Optional: change region if you're not in us-central1
sed -i "s/us-central1/$REGION/g" 30-configmap.yaml 50-deployment.yaml
```

For a real production workflow, use a kustomize overlay instead of `sed` so your customizations stay separate from the base recipe.

### Step 4 — Create the attach-token Secret

```bash
kubectl create namespace agent-system  # if not yet applied

kubectl create secret generic core-agent \
  --namespace agent-system \
  --from-literal=attach-token="$(openssl rand -hex 32)"
```

(Or use the `20-secret.yaml.example` file as a template — but `kubectl create secret` is preferred since the token never lives in a file you might accidentally commit.)

### Step 5 — Apply

```bash
kubectl apply -k examples/gke-deploy/deploy/
```

This creates the Namespace, KSA (with WIF annotation), ConfigMap, PVC, Deployment, and Service in dependency order. The Deployment's annotation registers the agent with the Agent Registry on apply.

### Step 6 — Verify

```bash
# Pod up
kubectl get pods -n agent-system -l app=core-agent
# expect: 1/1 Running

# Internal LB has an address (may take a minute to provision)
kubectl get svc -n agent-system core-agent
# expect EXTERNAL-IP column shows a 10.x.x.x address

# Logs report successful startup + Vertex AI auth via WIF
kubectl logs -n agent-system -l app=core-agent --tail=50
```

If you see auth errors in the logs, re-check:
- KSA annotation matches the GSA exactly (`kubectl get sa -n agent-system core-agent -o yaml`)
- WIF binding member string uses the right namespace + KSA name
- Cluster has WIF enabled (`gcloud container clusters describe <name> --format='value(workloadIdentityConfig.workloadPool)'` should be non-empty)

## Attach

From inside the VPC (Cloud Workstations, IAP tunnel, VPN):

```bash
export ATTACH_URL="http://$(kubectl get svc -n agent-system core-agent \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}'):7777"
export ATTACH_TOKEN="$(kubectl get secret -n agent-system core-agent \
  -o jsonpath='{.data.attach-token}' | base64 -d)"

core-agent-tui "$ATTACH_URL" --attach-token-env=ATTACH_TOKEN
```

(`--attach-token-env=ATTACH_TOKEN` reads the token from the env var instead of putting it on the command line.)

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
kubectl delete -k examples/gke-deploy/deploy/
kubectl delete pvc -n agent-system core-agent-data  # explicit; kustomize doesn't track it
kubectl delete secret -n agent-system core-agent
kubectl delete namespace agent-system

# Revoke GSA roles + delete GSA (if no longer used)
gcloud projects remove-iam-policy-binding $PROJECT_ID \
  --member="serviceAccount:core-agent@$PROJECT_ID.iam.gserviceaccount.com" \
  --role="roles/aiplatform.user"
gcloud projects remove-iam-policy-binding $PROJECT_ID \
  --member="serviceAccount:core-agent@$PROJECT_ID.iam.gserviceaccount.com" \
  --role="roles/container.viewer"
gcloud iam service-accounts delete \
  core-agent@$PROJECT_ID.iam.gserviceaccount.com --quiet
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
