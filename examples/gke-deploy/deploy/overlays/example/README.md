# Example overlay — kustomize entry point

Copy this directory to `overlays/<your-deployment-name>/` and edit
ONE file; everything else has working defaults.

```bash
# From the recipe root
cp -r examples/gke-deploy/deploy/overlays/example \
      examples/gke-deploy/deploy/overlays/my-prod
```

## Required edit — `patch-deployment.yaml`

Find the `env` block and replace both placeholders:

```yaml
env:
  - name: GOOGLE_CLOUD_PROJECT
    value: "REPLACE_WITH_YOUR_PROJECT_ID"
  - name: GOOGLE_CLOUD_LOCATION
    value: "REPLACE_WITH_YOUR_REGION"
```

Region must be a Vertex AI–supported region (e.g. `us-central1`, `us-east5`, `europe-west4`, `asia-southeast1` — see the [Vertex AI locations table](https://docs.cloud.google.com/vertex-ai/docs/general/locations) for the full list per model). Pick one close to your GKE cluster to minimize latency and cross-region cost.

That's it for the required setup. The base's ConfigMap (`config.json`, `mcp.json`, `AGENTS.md`) is inherited as-is and is fully functional with these env vars. Core-agent's Gemini provider falls back to `GOOGLE_CLOUD_PROJECT` + `GOOGLE_CLOUD_LOCATION` env vars when `model.vertex` is omitted from config, so there's no second place to edit.

## Optional — override the agentic small model

The base wires `--agentic-tools` + `--agentic-small-model=gemini-2.5-flash` so bulk tool reads (file/grep/MCP fetches) route through a cheap Flash subtask rather than burning Pro tokens on raw output. This is real cost savings for an always-on agent doing dense GKE inspection — typically 5-10× cheaper per turn.

To override, uncomment the `AGENTIC_SMALL_MODEL` env block in `patch-deployment.yaml`:

```yaml
- name: AGENTIC_SMALL_MODEL
  value: "gemini-2.5-flash"   # or another small-model ID
```

Set the value to `""` to disable agentic routing entirely (subtasks then run on the parent's Pro model — simpler but more expensive).

## Optional — customize the ConfigMap (advanced)

If you want to change the agent's prompt, permissions, or MCP server set:

1. Copy the base's config files into your overlay:
   ```bash
   mkdir overlays/my-prod/config
   cp examples/gke-deploy/deploy/base/config/*.{json,md} overlays/my-prod/config/
   ```
2. Edit the copies in `overlays/my-prod/config/`.
3. Add a `configMapGenerator` block to `overlays/my-prod/kustomization.yaml`:
   ```yaml
   configMapGenerator:
     - name: core-agent-config
       behavior: replace
       files:
         - config.json=config/config.json
         - mcp.json=config/mcp.json
         - AGENTS.md=config/AGENTS.md
       options:
         disableNameSuffixHash: true
   ```

The overlay's ConfigMap now wins; the base's defaults stay intact for the next operator who doesn't need customization.

## Other optional overrides (commented out in `kustomization.yaml`)

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
