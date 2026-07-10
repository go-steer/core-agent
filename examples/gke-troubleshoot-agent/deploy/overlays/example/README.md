# Example overlay

Copy this directory to a new one (e.g. `deploy/overlays/prod/`) and
edit the values below for your environment. Then `kubectl apply -k`
your overlay.

## What to edit

| File | What to change |
|---|---|
| `kustomization.yaml` | Image tag (`newTag`), optional `namePrefix` + `commonLabels`, optional custom `config.json` |
| `patch-watcher-cluster-name.yaml` | Change `--cluster-name=prod-us-central1` to your cluster's real name |

## What to leave alone

- The `resources: [../../base]` line — that's what pulls in every
  base manifest. Change only if your overlay lives at a different
  depth relative to the base.
- The watcher's other args (`--daemon-url`, `--owner`, `--dedup-window`,
  `--metrics-addr`) — the defaults are correct for a single-cluster
  deployment. Only change `--daemon-url` when the watcher lives in a
  different cluster than the daemon (central-daemon fleet topology).

## Deploy

From this overlay directory:

```bash
# 1. Create Secrets (see ../../base/20-secrets-placeholder.md for full commands)
kubectl create ns agent-triage
kubectl -n agent-triage create secret generic core-agent-users \
    --from-file=users.json=/path/to/users.json
kubectl -n agent-triage create secret generic k8s-event-watcher-token \
    --from-literal=token="$(jq -r '.users[]|select(.identity=="sa:k8s-event-watcher")|.token' /path/to/users.json)"

# 2. Bind GCP IAM roles to the daemon's KSA via WIF for GKE direct binding
#    (see ../../README.md §"GCP IAM setup")

# 3. Apply
kubectl apply -k .

# 4. Verify
kubectl -n agent-triage get pods,svc,deploy
```

## Multi-cluster: add a sidecar for another cluster

Deploy this overlay in each cluster you want covered, but override
the watcher's `--daemon-url` to point at the central daemon's
external endpoint (LoadBalancer IP, GCLB Ingress, VPN, etc.):

```yaml
# In your remote-cluster overlay's patch-watcher-cluster-name.yaml:
args:
  - "--daemon-url=https://core-agent.prod.example.com:7777"  # ← external URL
  - "--token-env=WATCHER_TOKEN"
  - "--mode=per-incident"
  - "--owner=sre-oncall@example.com"
  - "--cluster-name=dev-europe-west1"  # ← unique per cluster
  # ... rest unchanged
```

Each cluster's watcher then contributes incidents into the same
central daemon's audit trail, distinguishable by the `cluster`
field on every inject payload.
