# GKE troubleshooting agent recipe

Semi-autonomous Kubernetes triage agent for GKE. Runs `core-agent` as
a long-lived daemon in your cluster, watches Kubernetes Events via a
sidecar (`k8s-event-watcher`), and drives per-incident investigations
using structured triage skills backed by the GKE MCP server.

This recipe layers on top of `../gke-deploy/` — the multi-session
substrate and session-resume features that ship in v2.4 + v2.5. Read
that recipe first if you haven't already; the concepts (WIF for GKE
direct binding, kustomize base + overlays, cosign image verification)
apply here too.

## What you get

1. A `core-agent` Deployment (multi-session enabled, plan-first on,
   session-resume-enabled) exposed as an in-cluster Service.
2. A `k8s-event-watcher` Deployment (sidecar; runs alongside the
   daemon in the same cluster) watching Events via client-go
   informer.
3. The `k8s-triage` skill — a router that loads reason-specific
   references (CrashLoopBackOff, ImagePullBackOff, OOMKilled,
   FailedMount, FailedScheduling, BackOff, Unhealthy,
   NetworkNotReady, NodeNotReady, Evicted) and drives the
   diagnose → fix → verify loop.
4. Full RBAC + IAM guidance (least-privilege ClusterRole for the
   watcher; documented GCP IAM roles for the daemon).
5. GKE MCP server wired into `mcp.json` at `container.googleapis.com/mcp`
   (full-access endpoint — the agent needs write for `rollout undo`,
   `set image`, etc. gated by plan-first). Auth is `google_oauth`
   using the daemon's KSA with the IAM bindings from setup step 3.

## The end-to-end flow

```
   ┌──────────────────┐    watch     ┌────────────────────┐
   │  kube-apiserver  │ ◄─────────── │ k8s-event-watcher  │
   │   (Events API)   │              │  (sidecar pod)     │
   └──────────────────┘              └─────────┬──────────┘
                                               │ POST /sessions +
                                               │ POST /sessions/<sid>/inject
                                               ▼
                                     ┌────────────────────┐
                                     │    core-agent      │
                                     │  (daemon pod)      │
                                     │  ┌──────────────┐  │
                                     │  │ k8s-triage   │  │
                                     │  │   skill      │──┼──► GKE MCP
                                     │  │  (router)    │  │    (diagnose + fix)
                                     │  └──────────────┘  │
                                     └────────────────────┘
                                               │
                                               ▼ resolve or escalate
                                     ┌────────────────────┐
                                     │  eventlog (SQLite) │  ← v2.6 escalation
                                     │  INCIDENT SUMMARY  │    path (tail via
                                     │  blocks            │    Cloud Logging /
                                     │                    │    stern for alerts;
                                     │  Native alert tool │    turnkey escalation
                                     │  → v2.7 (#192)     │    lands in v2.7)
                                     └────────────────────┘
```

Every incident → one session → one audit trail. When the sidecar
fires an inject, the daemon creates a per-incident session (via
`POST /sessions` with `X-Asserted-Caller: sre-oncall@example.com`),
the agent invokes the `k8s-triage` skill, the skill loads the
reason-specific reference and executes it. Fix-and-verify is
mandatory. On budget exhaustion the agent escalates.

## Prerequisites

- A GKE cluster with Workload Identity Federation for GKE enabled
  (default on new clusters since 1.21). Verify:
  `gcloud container clusters describe <name> --format='value(workloadIdentityConfig.workloadPool)'`.
- `gcloud`, `kubectl`, `kustomize` (or `kubectl apply -k`) installed
  locally.
- Vertex AI enabled in the same project (`gcloud services enable
  aiplatform.googleapis.com`).
- The GKE MCP server accessible from your cluster (usually is by
  default: `mcp.googleapis.com`).

## Setup

### 1. Copy the example overlay

```bash
cd examples/gke-troubleshoot-agent/deploy/overlays
cp -r example prod
$EDITOR prod/kustomization.yaml            # image tags, prefixes
$EDITOR prod/patch-watcher-cluster-name.yaml  # your cluster name
```

### 2. Create the Secrets

Detailed instructions in `deploy/base/20-secrets-placeholder.md`.
Summary:

```bash
kubectl create ns agent-triage

# users.json — bearer tokens for operators + the sidecar identity
cat > /tmp/users.json <<EOF
{
  "version": 1,
  "users": [
    { "identity": "sre-oncall@example.com", "token": "$(openssl rand -hex 32)" },
    { "identity": "sa:k8s-event-watcher",   "token": "$(openssl rand -hex 32)" }
  ]
}
EOF
chmod 0600 /tmp/users.json

kubectl -n agent-triage create secret generic core-agent-users \
    --from-file=users.json=/tmp/users.json

kubectl -n agent-triage create secret generic k8s-event-watcher-token \
    --from-literal=token="$(jq -r '.users[]|select(.identity=="sa:k8s-event-watcher")|.token' /tmp/users.json)"

# Save sre-oncall's token separately — this is what YOU'll use to
# attach a TUI:
jq -r '.users[]|select(.identity=="sre-oncall@example.com")|.token' /tmp/users.json > ~/.core-agent/sre-oncall.token
chmod 0600 ~/.core-agent/sre-oncall.token

rm /tmp/users.json
```

### 3. Bind GCP IAM roles to the daemon's KSA

The daemon's KSA needs read/write on GKE workloads + Vertex AI +
Cloud Logging + Cloud Monitoring. With WIF for GKE direct binding,
no Google Service Account impersonation needed:

```bash
PROJECT_ID=your-project-id
PROJECT_NUMBER=$(gcloud projects describe "${PROJECT_ID}" --format='value(projectNumber)')
KSA_PRINCIPAL="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/agent-triage/sa/core-agent-daemon"

for role in \
    roles/aiplatform.user \
    roles/container.viewer \
    roles/container.developer \
    roles/logging.viewer \
    roles/monitoring.viewer; do
  gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
      --member="${KSA_PRINCIPAL}" \
      --role="${role}" \
      --condition=None
done
```

### 4. Apply

```bash
cd examples/gke-troubleshoot-agent/deploy/overlays/prod
kubectl apply -k .
kubectl -n agent-triage rollout status deployment core-agent
kubectl -n agent-triage rollout status deployment k8s-event-watcher
```

### 5. Attach a TUI

```bash
# From your laptop, port-forward the daemon (or expose via IAP /
# internal LB / VPN — see §"Attach paths" below):
kubectl -n agent-triage port-forward svc/core-agent 7777:7777 &

# Attach with your oncall token:
export SRE_TOKEN=$(cat ~/.core-agent/sre-oncall.token)
core-agent-tui http://127.0.0.1:7777 --token SRE_TOKEN
```

## Verify it's working

Trigger a synthetic CrashLoopBackOff:

```bash
kubectl create ns triage-test
kubectl -n triage-test run test-crash \
    --image=busybox:latest \
    --restart=Always \
    --command -- sh -c 'exit 1'
```

Within ~30 seconds the pod enters CrashLoopBackOff. The watcher
picks it up, POSTs a session inject, and the agent starts
investigating. In your TUI you should see:

1. A new session appear in the picker (namespace: triage-test,
   pod: test-crash-*, reason: CrashLoopBackOff).
2. The agent invoking `k8s-triage` skill.
3. The router calling `load_skill_resource` for
   `references/CrashLoopBackOff.md`.
4. The agent diagnosing (exit code 1, no stack trace, etc.) and
   proposing a fix via `record_plan`.
5. The agent applying the fix (or, if it decides the failure is
   irrecoverable, posting a structured summary to the eventlog).

Cleanup:

```bash
kubectl delete ns triage-test
```

## Attach paths — how operators reach the daemon

The daemon runs as a ClusterIP Service. Four common ways to reach
it from outside the cluster:

1. **`kubectl port-forward`** (dev / debugging). Simplest.
2. **Internal HTTP LoadBalancer** — expose the Service via a GCLB
   with an internal IP; access from within the VPC or via VPN.
3. **IAP-secured LoadBalancer** — use Identity-Aware Proxy so IAM
   identity gates access. Add IAP annotations to a BackendConfig.
4. **Cloud Workstations** — expose the daemon within a Cloud
   Workstations image; operators code + attach in one browser tab.

See `../gke-deploy/README.md` for the full manifest recipes for
options 2–4.

## Multi-cluster fleet

The base recipe deploys sidecar + daemon in the same cluster.
For a fleet where one central daemon watches N clusters:

1. Deploy the daemon in one "control-plane" cluster only (delete
   `51-deployment-watcher.yaml` from that cluster's overlay, or
   just leave one sidecar there).
2. In each additional cluster, deploy only the sidecar +
   ClusterRoleBinding (skip the daemon Deployment, Service, PVC,
   config ConfigMap). The sidecar's overlay overrides
   `--daemon-url` to point at the central daemon's external
   endpoint (`https://core-agent.prod.example.com:7777` or
   whatever your LB / IAP setup gives you).
3. Each sidecar carries a unique `--cluster-name`; every inject
   payload identifies the source cluster.

Every cluster's incidents surface in the same central daemon's
session list, distinguishable by the `cluster` field. One TUI,
one audit trail, one on-call rotation.

## Escalation in v2.6 (eventlog-based)

Turnkey escalation (Slack/PagerDuty/webhook fire-and-forget) is
**deferred to v2.7**. The distroless image ships with no `bash`
or `curl`, so the naïve "agent shells out to POST a webhook"
pattern doesn't work; a native, config-driven `alert` tool that
fits distroless is designed at
[`docs/alert-tool-design.md`](../../docs/alert-tool-design.md)
and tracked at [#192](https://github.com/go-steer/core-agent/issues/192).
Slack's official MCP consumption (Streamable HTTP + OAuth 2.0)
is designed at
[`docs/mcp-oauth-design.md`](../../docs/mcp-oauth-design.md) and
tracked at [#190](https://github.com/go-steer/core-agent/issues/190).
Both ship in v2.7.

**Meanwhile, in v2.6**, the router closes every incident with a
structured `INCIDENT SUMMARY` block written to the eventlog:

```
INCIDENT SUMMARY
================
Status: RESOLVED | UNRESOLVED | ESCALATED
Incident: {namespace}/{name} ({uid})
Reason: {reason}
Cluster: {cluster}
Root cause: <one line>
Actions taken: 1. ... 2. ...
Final state: <one line>
```

Consume via any of:

- **Cloud Logging sink** (GKE default: kubelet forwards pod stderr
  to Cloud Logging). Filter for `jsonPayload.message =~ "INCIDENT
  SUMMARY"` and route to Pub/Sub → Cloud Function → Slack.
- **`stern` or `kubectl logs -f`** during active triage development.
- **Direct SQL** against the eventlog SQLite file (via
  `kubectl exec` into the daemon pod, or by SSH'ing to the PVC).

Once the alert tool ships in v2.7, the recipe will grow an
`alerts.targets[]` config section and the router will call
`alert()` directly — no eventlog scraping required. Filed as a
follow-up recipe update in the v2.7 milestone.

## Customizing coverage

Add a new triage reference by dropping a Markdown file into your
overlay's `skills/k8s-triage/references/<Reason>.md`. Update your
overlay's `configMapGenerator` to include it, add a matching
`items:` entry in the daemon Deployment's projected volume, and
`kubectl apply -k`. The router falls through to `_fallback.md` for
any reason without a specific reference, so you can add coverage
incrementally.

For failure modes you want the sidecar to WATCH but currently
doesn't: edit the watcher's `--reason` flag to add the reason to
the allow-list.

## Related

- `../gke-deploy/` — the underlying long-lived-daemon recipe.
- `../multi-session-bearer/` — multi-session substrate reference.
- `docs/k8s-event-agent-design.md` — v2.6 design doc.
- `docs/session-resume-design.md` — v2.5 session-resume design.
- `docs/multi-session-design.md` — v2.4 substrate design.
