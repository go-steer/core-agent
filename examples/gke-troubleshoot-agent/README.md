# GKE troubleshooting agent recipe

Propose-only Kubernetes triage agent for GKE. Runs `core-agent` as
a long-lived daemon in your cluster, watches Kubernetes Events via a
sidecar (k8s-lookout's `lookout watch`, deployed under the
`k8s-event-watcher` name), and drives per-incident investigations
using structured triage skills backed by the GKE MCP server.

**The agent diagnoses, verifies, proposes, and escalates. It never
mutates your cluster** — and that isn't a persona instruction, it's
the configuration: the only MCP server wired in is the *read-only*
GKE endpoint, `bash`/`write_file`/`edit_file`/`delete_file`/`fetch_url`
are in `tools.disable`, and the KSA is bound to `roles/container.viewer`.
There is no mutating verb anywhere in the daemon's tool catalog for a
persona to talk itself into using. Remediation lands in the incident
summary as a proposal, and a human applies it.

This recipe layers on top of `../gke-deploy/` — the multi-session
substrate and session-resume features that ship in v2.4 + v2.5. Read
that recipe first if you haven't already; the concepts (WIF for GKE
direct binding, kustomize base + overlays, cosign image verification)
apply here too.

> **Running a live demo?** See [`DEMO.md`](DEMO.md) — a step-by-step
> runbook (prerequisites + setup + 6-scene walkthrough + teardown +
> troubleshooting) structured so a human or an agent can execute it
> top-to-bottom with explicit commands and checkpoints.

## What you get

1. A `core-agent` Deployment (multi-session enabled, plan-first on,
   session-resume-enabled) exposed as an in-cluster Service.
2. A `k8s-event-watcher` Deployment (sidecar; runs alongside the
   daemon in the same cluster) watching Events via client-go
   informer. It runs `ghcr.io/go-steer/lookout:v0.11.0` — the
   watcher's source lives in
   [go-steer/k8s-lookout](https://github.com/go-steer/k8s-lookout),
   and its image is a drop-in swap for the retired
   `ghcr.io/go-steer/k8s-event-watcher` image (same flags, same
   RBAC, same Deployment shape).
3. The `k8s-triage` skill — a router that loads reason-specific
   references (CrashLoopBackOff, ImagePullBackOff, OOMKilled,
   FailedMount, FailedScheduling, BackOff, Unhealthy,
   NetworkNotReady, NodeNotReady, Evicted) and drives the
   diagnose → verify → propose → escalate loop.
4. Full RBAC + IAM guidance (least-privilege ClusterRole for the
   watcher; documented GCP IAM roles for the daemon).
5. GKE MCP server wired into `mcp.json` at
   `container.googleapis.com/mcp/read-only` — the read-only endpoint,
   so the mutating verbs are never exposed to the model in the first
   place. Auth is `google_oauth` using the daemon's KSA with the IAM
   bindings from setup step 4.
6. Escalation via the native `alert` tool: one `oncall` target reading
   its webhook URL from `ONCALL_WEBHOOK_URL`, rate-limited to 10/min.
7. Plan-first enforcement (`require_plan_artifact: true`). Every `gke`
   MCP call — including read-only ones — is denied until the agent has
   called `record_plan`, so no cluster introspection happens before a
   written plan exists. Scope caveat: the gate flag is **per session and
   sticky**, so it binds the first incident of a session; subsequent
   injects into the same session are unblocked and the per-incident plan
   is convention (AGENTS.md + the skill's Step 0), not enforcement.
   Artifacts land in the `plans` emptyDir at
   `/etc/core-agent/.agents/plans/plan-<seq>.md` — ephemeral, read them
   with `kubectl exec` or off the eventlog, not off the PVC.

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
                                     │  │ k8s-triage   │  │    GKE MCP
                                     │  │   skill      │──┼──► /mcp/read-only
                                     │  │  (router)    │  │    (diagnose only)
                                     │  └──────────────┘  │
                                     └────────────────────┘
                                               │
                             ┌─────────────────┴────────────────┐
                             ▼                                  ▼
                   ┌────────────────────┐          ┌────────────────────┐
                   │  eventlog (SQLite) │          │  alert tool        │
                   │  INCIDENT SUMMARY  │          │  target: oncall    │
                   │  + proposed fix    │          │  → ONCALL_WEBHOOK  │
                   │  (audit trail)     │          │    _URL (10/min)   │
                   └────────────────────┘          └────────────────────┘
                                                              │
                                                              ▼
                                                    a human applies
                                                    the proposed fix
```

Every incident → one session → one audit trail. When the sidecar
fires an inject, the daemon creates a per-incident session (via
`POST /sessions` with `X-Asserted-Caller: sre-oncall@example.com`),
the agent calls `record_plan` (mandatory — plan-first gates every MCP
call), invokes the `k8s-triage` skill, and the skill loads the
reason-specific reference and executes it.

Each reference ends the same way: a **convergence check** and a
**remediation proposal**. The convergence check is a real
`wait_and_verify` call against a `gke_*` read tool — that observation
is the *only* thing that can produce `Status: RESOLVED`. If the
failure hasn't cleared, the agent writes `UNRESOLVED` with a concrete
proposal a human can apply, and fires `alert(target: "oncall", ...)`.
Budget exhaustion escalates the same way.

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

Then the escalation webhook. `config.json` declares one alert target,
`oncall`, whose URL comes from `ONCALL_WEBHOOK_URL`; the daemon reads
it from an optional `core-agent-alerts` Secret:

```bash
kubectl -n agent-triage create secret generic core-agent-alerts \
    --from-literal=ONCALL_WEBHOOK_URL='https://hooks.example.com/services/XXX'
```

The Secret is `optional: true` in the Deployment, so the pod still
boots without it — but then the agent has **no escalation path at
all**. A target whose `url_env` is unset can never deliver (a process's
environment is fixed at exec time), so the daemon drops it at startup
and, since `oncall` is the only target, registers no `alert` tool.
Watch for this at boot:

```
core-agent: alerts: target "oncall" is not deliverable (url_env "ONCALL_WEBHOOK_URL" is unset or empty); dropped from the alert tool
core-agent: alerts: no deliverable targets; the alert tool is NOT registered — the agent has no escalation path
```

That is deliberate: the alternative is an agent that believes it paged
someone. Without the tool, the triage skill falls back to
`Escalation: not sent (no alert target configured)` in the `INCIDENT
SUMMARY`, which is a hand-off a human can act on. If you genuinely
don't want escalation, delete the `alerts` block from `config.json` —
same outcome, stated on purpose.

The target uses the `generic` template: a JSON POST any webhook
receiver can consume. `generic` is the only template the alert tool
currently implements; provider-shaped payloads (Slack, PagerDuty
Events v2) are rejected at config load.

### 3. Verify cluster + node-pool WIF (Standard clusters only)

**Autopilot clusters**: WIF is on by default and every node pool uses
the GKE metadata server automatically. Skip this step.

**Standard clusters**: verify the cluster-level `workload-pool` is set
AND every node pool that will host the daemon has
`--workload-metadata=GKE_METADATA`:

```bash
# Cluster-level WIF (workloadPool should be "<PROJECT_ID>.svc.id.goog")
gcloud container clusters describe "${CLUSTER_NAME}" \
    --location="${REGION}" \
    --project="${PROJECT_ID}" \
    --format='value(workloadIdentityConfig.workloadPool)'

# Per-node-pool metadata (should be GKE_METADATA, not GCE_METADATA)
gcloud container node-pools list --cluster="${CLUSTER_NAME}" \
    --location="${REGION}" --project="${PROJECT_ID}" --format='value(name)' \
| while read pool; do
    mode=$(gcloud container node-pools describe "${pool}" \
        --cluster="${CLUSTER_NAME}" --location="${REGION}" \
        --project="${PROJECT_ID}" --format='value(config.workloadMetadataConfig.mode)')
    echo "pool=${pool} mode=${mode:-<unset>}"
  done
```

Remediation for a node pool showing `<unset>` or `GCE_METADATA`:

```bash
gcloud container node-pools update "${POOL_NAME}" \
    --cluster="${CLUSTER_NAME}" \
    --location="${REGION}" \
    --project="${PROJECT_ID}" \
    --workload-metadata=GKE_METADATA
```

Also, Standard-cluster pods need to be pinned onto WIF-enabled nodes
via a `nodeSelector`. See `deploy/base/50-deployment-daemon.yaml` for
the commented-out block; uncomment it in your overlay if you're on a
Standard cluster. (Autopilot rejects that selector — leave it commented
for Autopilot.)

### 4. Enable APIs + bind IAM roles for the daemon's KSA

`scripts/setup-wif.sh` automates both. It enables the GCP APIs the
recipe needs and binds the IAM roles that let the daemon:

- Call Gemini via Vertex AI (`roles/aiplatform.user`)
- Call GKE MCP tools (`roles/mcp.toolUser`)
- **Read** GKE clusters + workloads via the MCP (`roles/container.viewer`)
- Impersonate the node service account, which the GKE MCP's server-side
  chain requires (`roles/iam.serviceAccountUser` on the node SA)

`container.viewer` — not `container.admin` — is the least-privilege
grant that matches the read-only MCP endpoint this recipe wires. It is
the outermost of the three layers that make "propose-only" true:
IAM can't authorize a mutation, the read-only endpoint doesn't expose
one, and `tools.disable` removes the local escape hatches. If you
re-point `config/mcp.json` at the full-access `/mcp` endpoint, you must
upgrade this binding to `roles/container.admin` — and you're back to
trusting the persona.

```bash
# Simplest — reads PROJECT_ID from your active gcloud config, uses recipe defaults.
./scripts/setup-wif.sh

# Or explicit:
PROJECT_ID=your-project-id \
NAMESPACE=agent-triage \
KSA_NAME=core-agent-daemon \
    ./scripts/setup-wif.sh

# Audit-first: print the gcloud commands without executing them.
DRY_RUN=true ./scripts/setup-wif.sh
```

**Missing any one of the four roles gives a 403 at runtime with no
clear indication of which is missing** — that's why the script binds
all four together. `mcp.toolUser` alone doesn't work without
`container.viewer`; either project role alone doesn't work without the
`iam.serviceAccountUser`-on-node-SA binding.

<details>
<summary><b>What the script does (inline gcloud commands)</b></summary>

For operators who want to run the bindings manually or audit exactly
what gets applied:

```bash
PROJECT_ID=your-project-id
PROJECT_NUMBER=$(gcloud projects describe "${PROJECT_ID}" --format='value(projectNumber)')
NAMESPACE=agent-triage
KSA_NAME=core-agent-daemon
NODE_SA="${PROJECT_NUMBER}-compute@developer.gserviceaccount.com"
KSA_PRINCIPAL="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/${NAMESPACE}/sa/${KSA_NAME}"

# APIs
gcloud services enable container.googleapis.com aiplatform.googleapis.com iamcredentials.googleapis.com \
    --project="${PROJECT_ID}"

# Project-scoped role bindings
for role in roles/aiplatform.user roles/mcp.toolUser roles/container.viewer; do
  gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
      --member="${KSA_PRINCIPAL}" \
      --role="${role}" \
      --condition=None
done

# iam.serviceAccountUser on the NODE SA (not the project) — required by
# GKE MCP's server-side impersonation chain
gcloud iam service-accounts add-iam-policy-binding "${NODE_SA}" \
    --member="${KSA_PRINCIPAL}" \
    --role="roles/iam.serviceAccountUser"
```

`iamcredentials.googleapis.com` is specifically the API that powers
the WIF token-exchange call the pod's metadata server makes; without
it, ADC returns "permission denied" with no useful hint at first
runtime call.

</details>

**Renaming the KSA?** If your overlay uses `namePrefix:` or `namespace:`
to change the KSA's name or namespace, the `principal://...` member
string must use the matching name + namespace. Update the script's
env vars accordingly:
`NAMESPACE=<new-ns> KSA_NAME=<new-ksa> ./scripts/setup-wif.sh`.
Mismatched bindings look fine to gcloud but the pod's runtime token
exchange returns "permission denied" with no clear hint about what
mismatched.

**Multi-project inspection**: if the daemon needs to introspect
clusters in projects OTHER than the deployment's home project, re-run
the script against each target project (KSA principal stays the same;
only the project receiving the binding changes):

```bash
PROJECT_ID=other-project ./scripts/setup-wif.sh
```

### 5. Apply

```bash
cd examples/gke-troubleshoot-agent/deploy/overlays/prod
kubectl apply -k .
kubectl -n agent-triage rollout status deployment core-agent
kubectl -n agent-triage rollout status deployment k8s-event-watcher
```

### 6. Attach a TUI

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
2. The agent calling `record_plan` first — before any MCP call, which
   plan-first would otherwise deny.
3. The agent invoking the `k8s-triage` skill, and the router calling
   `load_skill_resource` for `references/CrashLoopBackOff.md`.
4. Read-only diagnosis: `gke_get_k8s_resource`, `gke_get_k8s_logs`
   (previous container), `gke_list_k8s_events` — exit code 1, no
   stack trace, restart count climbing.
5. A `wait_and_verify` convergence check that does *not* pass (this
   pod never recovers), followed by an `INCIDENT SUMMARY` with
   `Status: UNRESOLVED`, a proposed fix, and an `alert` call to the
   `oncall` target.

Note step 5: `sh -c 'exit 1'` cannot self-heal, so a correct run here
is `UNRESOLVED` + a proposal. If you ever see `RESOLVED` for this pod,
that's a confabulation bug worth filing — `RESOLVED` is only legitimate
when a `wait_and_verify` call actually observed the failure clear.

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

## Escalation

Because the agent can't apply fixes, escalation isn't a fallback path
— it's the normal ending for any incident that doesn't self-heal.
It happens on two channels at once.

**1. The `alert` tool (push).** `config.json` declares:

```json
"alerts": {
  "rate_limit_per_target": "10/min",
  "targets": [
    { "name": "oncall", "url_env": "ONCALL_WEBHOOK_URL", "template": "generic",
      "description": "Page the on-call SRE. ..." }
  ]
}
```

The router calls `alert(target: "oncall", level: "critical", summary:
..., details: {...})` for every `UNRESOLVED` or `ESCALATED` incident.
The `generic` template POSTs a JSON body — point `ONCALL_WEBHOOK_URL`
at whatever ingests it (a Slack/Discord incoming webhook, an internal
receiver, a Cloud Function that fans out to PagerDuty). The
provider-specific templates named in the alert-tool design are *not*
implemented; `slack`, `discord`, and `pagerduty_events_v2` are
rejected at config load. Rate limiting is per target, so an event
storm can't turn into a page storm.

**2. The eventlog (pull / audit).** Every incident also closes with a
structured `INCIDENT SUMMARY` block:

```
INCIDENT SUMMARY
================
Status: RESOLVED | UNRESOLVED | ESCALATED
Incident: {namespace}/{name} ({uid})
Reason: {reason}
Cluster: {cluster}
Root cause: <one line>
Evidence: <the tool calls that support it>
Proposal: <the change a human should apply, or "none">
Escalation: <alert sent to oncall | not needed>
Final state: <one line>
```

`Status: RESOLVED` is reserved for the case where a `wait_and_verify`
call observed the failure clear on its own — the agent took no action,
so a resolution it didn't verify is a resolution it made up.

Consume the eventlog via any of:

- **Cloud Logging sink** (GKE default: kubelet forwards pod stderr
  to Cloud Logging). Filter for `jsonPayload.message =~ "INCIDENT
  SUMMARY"` and route to Pub/Sub → Cloud Function → your tracker.
- **`stern` or `kubectl logs -f`** during active triage development.
- **Direct SQL** against the eventlog SQLite file on the PVC (via
  `kubectl exec` into the daemon pod).

Slack's official MCP consumption (Streamable HTTP + OAuth 2.0) is
designed at [`docs/mcp-oauth-design.md`](../../docs/mcp-oauth-design.md)
and tracked at [#190](https://github.com/go-steer/core-agent/issues/190);
wiring it here would add a second MCP server, which this recipe
deliberately doesn't do.

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

This recipe is the event-driven-triage baseline: the watcher runs
with the classic Events-only configuration this recipe has always
used. The [k8s-lookout](https://github.com/go-steer/k8s-lookout)
project it ships from has a much larger capability surface —
additional signal sources via `--sources=` (object-state, rollout,
saturation, degradation, expiry, capacity, token-burn), storm
correlation (`--storm=`), an on-disk occurrence store (`--store=`),
and a `-gke` image flavor with cloud-provider sources compiled in.
See that repo's README and deploy manifests to opt in.

## Related

- `../gke-deploy/` — the underlying long-lived-daemon recipe.
- `../multi-session-bearer/` — multi-session substrate reference.
- `docs/k8s-event-agent-design.md` — v2.6 design doc.
- `docs/session-resume-design.md` — v2.5 session-resume design.
- `docs/multi-session-design.md` — v2.4 substrate design.
