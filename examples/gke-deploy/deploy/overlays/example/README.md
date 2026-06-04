# Example overlay — kustomize entry point

Copy this directory to `overlays/<your-deployment-name>/` and edit
two things; everything else has working defaults.

```bash
# From the recipe root
cp -r examples/gke-deploy/deploy/overlays/example \
      examples/gke-deploy/deploy/overlays/my-prod
```

## Required edits

### 1. `config/config.json` — your GCP project + region

Find the `model.vertex` block:

```json
"vertex": {
  "project": "REPLACE_WITH_YOUR_PROJECT_ID",
  "location": "REPLACE_WITH_YOUR_REGION"
}
```

Replace both placeholders. Region must be a Vertex AI–supported region (e.g. `us-central1`, `us-east5`, `europe-west4`, `asia-southeast1` — see the [Vertex AI locations table](https://docs.cloud.google.com/vertex-ai/docs/general/locations) for the full list per model). Pick one close to your GKE cluster to minimize latency and cross-region cost.

### 2. `patch-deployment.yaml` — matching env vars

Find the `env` block:

```yaml
env:
  - name: GOOGLE_CLOUD_PROJECT
    value: "REPLACE_WITH_YOUR_PROJECT_ID"
  - name: GOOGLE_CLOUD_LOCATION
    value: "REPLACE_WITH_YOUR_REGION"
```

Same values as `config/config.json`. The agent reads both (config wins on conflict; keep them in sync to avoid confusion).

## Optional edits (commented out in `kustomization.yaml`)

| Knob | When to flip |
|---|---|
| `namePrefix:` | Running multiple core-agent deployments side-by-side in the same cluster. **NB: IAM principal binding must match the prefixed KSA name.** |
| `namespace:` | Deploying to a namespace other than `agent-system`. **NB: IAM principal binding's `ns/...` segment must match.** |
| `images:` newTag | Pinning to a specific image version (or :main-`<sha>` for dev builds) |
| `images:` newName | Switching to the `-slim` variant for headless-only deployments |
| ImagePullSecret patch | If GHCR package is still private (operators add `ghcr-pull` Secret + patch SA) |
| Resource overrides | Heavier workloads (autonomous mode, dense MCP tools) |

## Apply

```bash
# Sanity-check that kustomize resolves cleanly
kubectl kustomize examples/gke-deploy/deploy/overlays/my-prod

# Apply
kubectl apply -k examples/gke-deploy/deploy/overlays/my-prod
```

## Verify

```bash
kubectl get pods -n agent-system -l app=core-agent
# expect: 1/1 Running

kubectl logs -n agent-system -l app=core-agent --tail=20
# look for: clean startup + first Vertex AI call succeeds (no auth errors)

kubectl get svc -n agent-system core-agent
# expect: EXTERNAL-IP shows a 10.x.x.x address (internal LB)
```

## Customize further

This overlay is a starting point. Real operators typically extend
it with:

- An additional `patches:` entry to add labels / annotations for
  their monitoring stack
- A sidecar container for log shipping
- HorizontalPodAutoscaler / VerticalPodAutoscaler (note: HPA needs
  multi-session support, task #12)
- NetworkPolicy restricting which pods can reach the attach LB
- A separate `Deployment.spec.template.spec.tolerations` /
  `nodeSelector` to pin to specific node pools

All of those are pure-kustomize additions — no edits to the
recipe's base.

## Don't commit your overlay to the core-agent repo

If you're tracking a copy of this recipe in `core-agent`'s repo
for development, treat `overlays/my-prod/` (or whatever name you
picked) as out-of-tree. The PR-friendly pattern:

```bash
# In your own infra repo
git init my-deployments
cd my-deployments
mkdir -p core-agent/overlays
cp -r /path/to/core-agent/examples/gke-deploy/deploy/overlays/example \
      core-agent/overlays/my-prod
# Reference the base via GitHub URL in core-agent/overlays/my-prod/kustomization.yaml:
#   resources:
#     - github.com/go-steer/core-agent/examples/gke-deploy/deploy/base?ref=v2.3.1
```

That way your customizations live in your repo, the base is
pinned to a specific core-agent release tag, and `git pull` on
either side is conflict-free.
