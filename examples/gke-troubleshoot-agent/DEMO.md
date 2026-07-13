# v2.6 demo runbook — semi-autonomous Kubernetes triage on GKE

Step-by-step runbook for demonstrating the v2.6 k8s triage agent on a real GKE cluster. Structured so a human (or an agent) can execute it top-to-bottom with explicit commands, expected outputs, wait times, and recovery paths.

**Audience**: whoever's driving the demo — first-time or hundredth-time. Every command is copy-paste-executable; every checkpoint has a specific string to grep for; every wait has a duration.

**Prereq skill**: comfortable with `kubectl`, `gcloud`, terminal multiplexing (`tmux` or split panes).

**Time budget**: 30 min one-time setup + 5 min pre-flight + 15 min live demo + 3 min teardown.

---

## Table of contents

1. [Prerequisites](#prerequisites) — checkable one-liners
2. [One-time setup](#one-time-setup) — cluster + secrets + deploy
3. [Pre-flight rehearsal](#pre-flight-rehearsal) — 6-step sanity check before going live
4. [Live demo runbook](#live-demo-runbook) — 6 scenes with commands
5. [Post-demo teardown](#post-demo-teardown) — clean up
6. [Troubleshooting](#troubleshooting) — recovery from common failures
7. [Agent-driven mode](#agent-driven-mode) — notes for an LLM executing this runbook

---

## Prerequisites

Copy-paste each block; every check should print a specific expected substring. Any FAIL means fix that item before proceeding.

### Environment

```bash
# Set once at the top; every subsequent block reads these.
export PROJECT_ID="your-project-id-here"      # <-- EDIT
export CLUSTER_NAME="demo-cluster"            # <-- EDIT
export REGION="us-central1"                   # <-- EDIT
export DEMO_NS="agent-triage"                 # Namespace core-agent runs in
export TARGET_NS="default"                    # Namespace we'll break during demo

# Convenience:
export PROJECT_NUMBER=$(gcloud projects describe "${PROJECT_ID}" --format='value(projectNumber)')
```

### Local tools

```bash
gcloud --version          # Expect: "Google Cloud SDK <version>"
kubectl version --client  # Expect: "Client Version: v1.29+"
kustomize version         # Expect: "v5+" (optional; kubectl kustomize works too)
jq --version              # Expect: "jq-1.6+"
```

### Cloud project

```bash
# Vertex AI enabled (agent's model provider)
gcloud services list --enabled --project="${PROJECT_ID}" --filter="name:aiplatform.googleapis.com" \
    | grep -q "aiplatform.googleapis.com" \
    && echo "✓ Vertex AI enabled" \
    || (echo "✗ Vertex AI NOT enabled; run: gcloud services enable aiplatform.googleapis.com --project=${PROJECT_ID}"; false)

# Container API + Kubernetes Engine MCP prereqs
gcloud services list --enabled --project="${PROJECT_ID}" --filter="name:container.googleapis.com" \
    | grep -q "container.googleapis.com" \
    && echo "✓ Container API enabled" \
    || (echo "✗ Container API NOT enabled; run: gcloud services enable container.googleapis.com --project=${PROJECT_ID}"; false)

# IAM Credentials API — required for the WIF token-exchange path.
# Missing this gives "permission denied" at first runtime call with
# no hint about which API is missing.
gcloud services list --enabled --project="${PROJECT_ID}" --filter="name:iamcredentials.googleapis.com" \
    | grep -q "iamcredentials.googleapis.com" \
    && echo "✓ IAM Credentials API enabled" \
    || (echo "✗ IAM Credentials API NOT enabled; run: gcloud services enable iamcredentials.googleapis.com --project=${PROJECT_ID}"; false)
```

(All three APIs are also enabled automatically by `scripts/setup-wif.sh` in the setup phase below; this pre-flight check catches configuration drift.)

### Cluster

```bash
# Cluster exists and reachable
gcloud container clusters describe "${CLUSTER_NAME}" --region="${REGION}" --project="${PROJECT_ID}" \
    --format='value(status)' 2>&1 | grep -q "RUNNING" \
    && echo "✓ Cluster ${CLUSTER_NAME} RUNNING" \
    || (echo "✗ Cluster not RUNNING; check cluster state"; false)

# Workload Identity Federation for GKE enabled
gcloud container clusters describe "${CLUSTER_NAME}" --region="${REGION}" --project="${PROJECT_ID}" \
    --format='value(workloadIdentityConfig.workloadPool)' 2>&1 | grep -q "svc.id.goog" \
    && echo "✓ WIF for GKE enabled" \
    || (echo "✗ WIF for GKE NOT enabled; enable via cluster update"; false)

# kubectl context pointed at this cluster
gcloud container clusters get-credentials "${CLUSTER_NAME}" --region="${REGION}" --project="${PROJECT_ID}"
kubectl config current-context | grep -q "${CLUSTER_NAME}" \
    && echo "✓ kubectl context set" \
    || (echo "✗ kubectl context mismatch"; false)
```

### Container images

```bash
# v2.6.0 images published on GHCR (should exist since we tagged v2.6.0)
for img in core-agent core-agent-slim core-agent-tui k8s-event-watcher; do
  crane digest "ghcr.io/go-steer/${img}:2.6.0" >/dev/null 2>&1 \
      && echo "✓ ghcr.io/go-steer/${img}:2.6.0 exists" \
      || echo "✗ ghcr.io/go-steer/${img}:2.6.0 NOT found — check ε.4 release ran"
done
```

(If `crane` isn't installed, skip this — the deploy will fail loudly if an image is missing.)

### Local `core-agent-tui` binary

Three ways to get it — pick one:

```bash
# Option 1 (recommended): build from source at the v2.6.0 tag.
# `go install @v2.6.0` doesn't work today — the module path
# doesn't yet carry the /v2 suffix required by Go SIVE for
# major version ≥ 2. Tracked as a separate issue.
git clone https://github.com/go-steer/core-agent.git /tmp/core-agent-src
cd /tmp/core-agent-src && git checkout v2.6.0
go install ./cmd/core-agent-tui
cd - >/dev/null

# Option 2: install from main (latest development; may include
# post-v2.6.0 changes)
go install github.com/go-steer/core-agent/cmd/core-agent-tui@main

# Option 3: pull the published container image and extract the binary
docker pull ghcr.io/go-steer/core-agent-tui:2.6.0
CID=$(docker create ghcr.io/go-steer/core-agent-tui:2.6.0)
docker cp "${CID}:/usr/local/bin/binary" "${GOPATH:-$HOME/go}/bin/core-agent-tui"
docker rm "${CID}"
chmod +x "${GOPATH:-$HOME/go}/bin/core-agent-tui"

# Verify (any of the three)
which core-agent-tui \
    && echo "✓ core-agent-tui on PATH" \
    || (echo "✗ TUI not on PATH; ensure ${GOPATH:-$HOME/go}/bin is in \$PATH"; false)
core-agent-tui --version | grep -q "v2.6\|main-" \
    && echo "✓ TUI version looks right" \
    || echo "warning: version string unexpected (may still work)"
```

---

## One-time setup

Execute once per cluster. If re-running (fresh cluster after teardown), redo everything.

> **Ordering matters.** The daemon Deployment consumes state that isn't part of the kustomize output — a Secret (`core-agent-users`) mounted at `/etc/core-agent/users.json`, and a ConfigMap (`core-agent-gcp-env`) providing `GOOGLE_CLOUD_PROJECT` + `GOOGLE_CLOUD_LOCATION` via `envFrom`. Both are created out-of-band in step 3. Deploy the workloads before either exists and the pod either hangs on `FailedMount` or crashes at Vertex init. Steps below create the namespace first, bind IAM, create the out-of-band Secret + ConfigMap, THEN apply the workloads. Rehearse in that order.

### 1. Stage scratch deploy tree + create the namespace

Copy the example overlay to a scratch dir outside the repo (patched with your cluster name), create just the namespace so downstream steps have somewhere to put the Secrets. Full workloads land in step 4 after the Secrets exist.

```bash
# All throwaway state for this demo lives under DEMO_DIR. Rehearse,
# tear down, re-rehearse — a single `rm -rf "${DEMO_DIR}"` cleans up
# everything (scratch deploy tree, tokens, users.json).
export DEMO_DIR="/tmp/core-agent-demo"
export DEMO_DEPLOY_DIR="${DEMO_DIR}/deploy"
export DEMO_OVERLAY_DIR="${DEMO_DEPLOY_DIR}/overlays/example"

# Scratch deploy tree lives outside the repo — nothing here is tracked.
# We copy the whole deploy/ tree (base + overlays) so the overlay's
# relative `resources: [../../base]` reference still resolves. Kustomize
# rejects absolute paths as resources for security reasons, so keeping
# the relative layout intact is simpler than working around the restriction.
mkdir -p "${DEMO_DIR}"
rm -rf "${DEMO_DEPLOY_DIR}"
cp -r examples/gke-troubleshoot-agent/deploy "${DEMO_DEPLOY_DIR}"

# Substitute the placeholder cluster name (kustomize itself is
# non-templating — the placeholder lives in the checked-in patch
# file and we rewrite it in the scratch copy at apply time).
sed -i "s/prod-us-central1/${CLUSTER_NAME}/" \
    "${DEMO_OVERLAY_DIR}/patch-watcher-cluster-name.yaml"

# Confirm the substitution took (guards against a future rename of
# the placeholder string in the source overlay).
grep -q -- "--cluster-name=${CLUSTER_NAME}" \
    "${DEMO_OVERLAY_DIR}/patch-watcher-cluster-name.yaml" \
    && echo "✓ cluster name patched" \
    || (echo "✗ placeholder 'prod-us-central1' not found in patch file — check the source overlay"; false)

# Create only the namespace here. Full `apply -k` (which creates the
# Deployments that mount the Secrets) waits until step 4, AFTER
# step 3 has created the Secrets.
kubectl create namespace "${DEMO_NS}" --dry-run=client -o yaml \
    | kubectl apply -f -
kubectl get ns "${DEMO_NS}" && echo "✓ namespace created"
```

### 2. Enable APIs + bind GCP IAM to the daemon's KSA

Run the recipe's WIF setup script — enables `container.googleapis.com`, `aiplatform.googleapis.com`, `iamcredentials.googleapis.com`, and binds all four IAM roles the daemon needs (`aiplatform.user`, `mcp.toolUser`, `container.admin`, `iam.serviceAccountUser` on the node SA).

```bash
# Uses PROJECT_ID from env; DEMO_NS matches the recipe's default namespace.
NAMESPACE="${DEMO_NS}" ./examples/gke-troubleshoot-agent/scripts/setup-wif.sh

# Audit-first alternative — prints all seven gcloud commands without
# executing them (safe for reviewing before running against a real project):
#   PROJECT_ID="${PROJECT_ID}" NAMESPACE="${DEMO_NS}" DRY_RUN=true \
#       ./examples/gke-troubleshoot-agent/scripts/setup-wif.sh
```

**IAM propagation takes ~2 min after the bindings are applied.** If you rush to deploy the daemon before propagation completes, its first Vertex or GKE MCP call may return "permission denied"; wait 2 min then `kubectl rollout restart` recovers.

**Standard clusters only** — also verify node-pool metadata mode and, in your kustomize overlay, uncomment the `nodeSelector: iam.gke.io/gke-metadata-server-enabled: "true"` block in `50-deployment-daemon.yaml`. Autopilot skips both. See the recipe README setup step 3 for the verification commands.

### 3. Generate tokens + create Secrets

```bash
# Generate three tokens (rehearsal / demo tokens; NOT production)
SRE_TOKEN=$(openssl rand -hex 32)
BOB_TOKEN=$(openssl rand -hex 32)
WATCHER_TOKEN=$(openssl rand -hex 32)

# Stash them under DEMO_DIR (chmod 0600!). This replaces the older
# ~/.core-agent/demo-tokens.env convention — throwaway state stays
# under /tmp so tearing down the demo is one `rm -rf "${DEMO_DIR}"`.
mkdir -p "${DEMO_DIR}"
cat > "${DEMO_DIR}/demo-tokens.env" <<EOF
export SRE_TOKEN="${SRE_TOKEN}"
export BOB_TOKEN="${BOB_TOKEN}"
export WATCHER_TOKEN="${WATCHER_TOKEN}"
EOF
chmod 0600 "${DEMO_DIR}/demo-tokens.env"

# users.json for the daemon
cat > "${DEMO_DIR}/users.json" <<EOF
{
  "version": 1,
  "users": [
    { "identity": "sre-oncall@example.com", "token": "${SRE_TOKEN}" },
    { "identity": "bob@example.com",        "token": "${BOB_TOKEN}"  },
    { "identity": "sa:k8s-event-watcher",   "token": "${WATCHER_TOKEN}" }
  ]
}
EOF
chmod 0600 "${DEMO_DIR}/users.json"

# Create the Secrets in the (already-created) namespace
kubectl -n "${DEMO_NS}" create secret generic core-agent-users \
    --from-file=users.json="${DEMO_DIR}/users.json"

kubectl -n "${DEMO_NS}" create secret generic k8s-event-watcher-token \
    --from-literal=token="${WATCHER_TOKEN}"

# ConfigMap the daemon consumes via envFrom for Vertex init —
# GOOGLE_CLOUD_PROJECT + GOOGLE_CLOUD_LOCATION are per-cluster values,
# so they're created out-of-band here rather than baked into the
# kustomize output. Without this ConfigMap the daemon exits at Vertex
# init with "project and location are required."
kubectl -n "${DEMO_NS}" create configmap core-agent-gcp-env \
    --from-literal=GOOGLE_CLOUD_PROJECT="${PROJECT_ID}" \
    --from-literal=GOOGLE_CLOUD_LOCATION="${REGION}"

# users.json is checked into the cluster Secret now — the local copy
# with plaintext tokens no longer needs to sit on disk. demo-tokens.env
# stays under DEMO_DIR so `source` in later steps still works.
rm "${DEMO_DIR}/users.json"

echo "✓ Secrets + core-agent-gcp-env ConfigMap created; tokens stashed at ${DEMO_DIR}/demo-tokens.env"
```

### 4. Deploy the workloads

Now that the Secrets + `core-agent-gcp-env` ConfigMap exist, apply the full recipe overlay. Kustomize creates the SAs, RBAC, PVC, the generated `core-agent-agents` ConfigMap, Service, and both Deployments. The daemon pod schedules with all its mounts + env vars already present and comes up clean.

```bash
# Applies everything except the namespace (created in step 1) — SAs,
# ClusterRole/ClusterRoleBinding, PVC, ConfigMaps (config.json + the
# .agents/ tree incl. mcp.json), Service, and both Deployments
# (core-agent daemon + k8s-event-watcher).
kubectl apply -k "${DEMO_OVERLAY_DIR}"

# Sanity-check what actually landed. All expected names must appear
# in the demo namespace (NOT in `default`) or the daemon pod will
# hang on FailedMount / crash on Vertex init.
kubectl -n "${DEMO_NS}" get cm core-agent-agents core-agent-gcp-env \
    && echo "✓ ConfigMaps present"
kubectl -n "${DEMO_NS}" get secret core-agent-users k8s-event-watcher-token \
    && echo "✓ Secrets present"
```

### 5. Wait for pods to be Ready

```bash
kubectl -n "${DEMO_NS}" rollout status deployment/core-agent --timeout=180s
kubectl -n "${DEMO_NS}" rollout status deployment/k8s-event-watcher --timeout=180s

# Sanity check: both pods Running + Ready
kubectl -n "${DEMO_NS}" get pods

# Expected:
# NAME                                READY   STATUS    RESTARTS   AGE
# core-agent-<hash>                   1/1     Running   0          Xs
# k8s-event-watcher-<hash>            1/1     Running   0          Xs
```

If ANY pod is not `1/1 Running`, jump to [Troubleshooting](#troubleshooting) before continuing.

### 6. Verify daemon accepts your token

```bash
source "${DEMO_DIR:-/tmp/core-agent-demo}/demo-tokens.env"

# Port-forward the daemon in one terminal (keep this open through the demo)
kubectl -n "${DEMO_NS}" port-forward svc/core-agent 7777:7777 &
PORTFWD_PID=$!
sleep 3

# Auth check — expect HTTP 200 + empty session list
curl -sS -H "Authorization: Bearer ${SRE_TOKEN}" http://127.0.0.1:7777/sessions \
    | jq -r '.sessions | length' \
    | grep -q "^0$" \
    && echo "✓ auth works; session list empty" \
    || (echo "✗ auth failed OR sessions already exist"; false)

# Leave port-forward running for the demo
echo "port-forward running as PID ${PORTFWD_PID}; keep it alive"
```

Setup complete. You can shut down the cluster between prep and demo day; only need to rerun steps 4-5 after re-starting.

---

## Pre-flight rehearsal

Execute 15 min before you go live. Verifies the demo will work TODAY on THIS cluster.

### 1. Port-forward alive

```bash
# In a dedicated terminal that stays open
source "${DEMO_DIR:-/tmp/core-agent-demo}/demo-tokens.env"
kubectl -n "${DEMO_NS}" port-forward svc/core-agent 7777:7777
```

Leave this running.

### 2. Sanity-check auth from a second terminal

```bash
source "${DEMO_DIR:-/tmp/core-agent-demo}/demo-tokens.env"
curl -sS -H "Authorization: Bearer ${SRE_TOKEN}" http://127.0.0.1:7777/sessions | jq -r '.sessions | length'
# Expect: 0 (or small number if you rehearsed already; ideally 0 for a clean demo)
```

If non-zero, clean up: kill lingering sessions from prior rehearsals.

```bash
# Nuke the eventlog for a clean start (aggressive; do only during rehearsal)
kubectl -n "${DEMO_NS}" scale deployment/core-agent --replicas=0
kubectl -n "${DEMO_NS}" delete pvc core-agent-session-db
kubectl apply -k "${DEMO_OVERLAY_DIR:-/tmp/core-agent-demo/deploy/overlays/example}"   # recreates PVC
kubectl -n "${DEMO_NS}" scale deployment/core-agent --replicas=1
kubectl -n "${DEMO_NS}" rollout status deployment/core-agent
```

### 3. Quick TUI attach test

```bash
core-agent-tui http://127.0.0.1:7777 --token SRE_TOKEN
```

You should see:
- Empty session picker
- No error messages
- `q` to quit

If the TUI hangs or errors, check `kubectl -n "${DEMO_NS}" logs deployment/core-agent --tail=50`.

### 4. Verify k8s-event-watcher is watching

```bash
kubectl -n "${DEMO_NS}" logs deployment/k8s-event-watcher --tail=20
# Expect: "starting on cluster \"<name>\" → daemon http://core-agent..."
# Should NOT show connection errors to the daemon
```

### 5. Verify the GKE MCP server loaded

```bash
kubectl -n "${DEMO_NS}" logs deployment/core-agent --tail=200 | grep -i "mcp"
# Expect: at least one line indicating the "gke" MCP server started
# successfully (typical shape: "mcp: server \"gke\" ready" or "started").
# Should NOT see "mcp server \"gke\" failed" or "no mcp.json found".

# If missing, verify mcp.json is mounted:
#   kubectl -n "${DEMO_NS}" exec deployment/core-agent -- ls /etc/core-agent/agents
#   Expect: AGENTS.md, mcp.json, skills/
# If mcp.json isn't there, re-run `kubectl apply -k <overlay>` — the
# ConfigMap generator likely didn't pick it up.
```

### 6. Verify Vertex AI auth succeeded

```bash
kubectl -n "${DEMO_NS}" logs deployment/core-agent --tail=500 | grep -iE "vertex|aiplatform|gemini"
# Expect: model-init line with success shape (e.g., "model gemini-2.5-flash ready"
# or first-turn line without a permission error).
# Should NOT see: "permission denied", "PERMISSION_DENIED",
# "Request had insufficient authentication scopes".

# Common failures:
#   - "permission denied" → IAM binding hasn't propagated yet (wait 2 min);
#      OR roles/aiplatform.user isn't bound (rerun `./scripts/setup-wif.sh`)
#   - "no credentials found" → WIF isn't wired at cluster level (check
#      `gcloud container clusters describe ... --format='value(workloadIdentityConfig.workloadPool)'`)
#   - "iamcredentials API not enabled" → run `gcloud services enable iamcredentials.googleapis.com`
```

Rehearsal complete. Ready to go live.

---

## Live demo runbook

Total wall-clock: ~15 min. Each scene has a duration, setup commands, execution commands, expected outputs, and talking points.

### Scene 1 — Setup + orientation (2 min)

**Terminal layout**: three panes visible to audience.
- Pane A: TUI attached as `sre-oncall@example.com` (SRE_TOKEN)
- Pane B: kubectl scratch pane
- Pane C: `kubectl -n "${DEMO_NS}" logs deployment/k8s-event-watcher -f` (live watcher log)

```bash
# Pane B — verify starting state
kubectl -n "${DEMO_NS}" get pods
kubectl get ns
```

**Say**: "This is a live GKE cluster. Two pods in the `agent-triage` namespace: `core-agent` is the LLM-driven agent daemon; `k8s-event-watcher` is the sidecar that turns Kubernetes Events into agent injects. My TUI is attached over port-forward with an SRE oncall bearer token. Session list is empty — nothing's wrong yet."

```bash
# Pane A — show TUI session list (empty)
# (already attached)
```

### Scene 2 — Trigger a real failure (1 min)

**Setup**: prepare the "known good" webapp in Pane B.

```bash
# Deploy a working nginx first
kubectl -n "${TARGET_NS}" create deployment demo-webapp --image=nginx:1.25 --replicas=1
kubectl -n "${TARGET_NS}" rollout status deployment/demo-webapp --timeout=60s
kubectl -n "${TARGET_NS}" get pods -l app=demo-webapp
# Expect: pod Running 1/1
```

**Execute the break**: (this is the "boom" moment for the audience)

```bash
# Break it — point at an image tag that doesn't exist
kubectl -n "${TARGET_NS}" set image deployment/demo-webapp \
    nginx=nginx:this-tag-does-not-exist-v99
```

**Say**: "That deploy just pointed at a nonexistent image tag. In a real environment this happens all the time — bad CI, typo in a manifest, image mirror out of sync. In ~30 seconds kubelet will emit an `ImagePullBackOff` event. My sidecar is watching that event stream."

**Watch in Pane C**: within ~30s, watcher log shows the inject firing.

```
k8s-event-watcher: fire ImagePullBackOff (namespace=default, name=demo-webapp-...)
```

**Watch in Pane A** (TUI): new session appears in the picker.

### Scene 3 — Agent auto-triages (4-5 min)

Click into the new session (arrow keys + Enter in TUI). Watch turns stream in real time.

**What the audience sees**:

1. Agent invokes `k8s-triage` skill via `load_skill`
2. Router body says: "load `references/ImagePullBackOff.md`"
3. Agent calls `load_skill_resource` with `resource_path: references/ImagePullBackOff.md`
4. Agent runs diagnose steps via GKE MCP:
   - `kubectl describe pod` → sees "Failed to pull image ... manifest unknown"
   - Extracts current image reference: `nginx:this-tag-does-not-exist-v99`
   - Classifies: "wrong tag (typo)"
5. Agent writes a plan artifact via `record_plan`:
   ```
   Diagnosis: Wrong image tag (nginx:this-tag-does-not-exist-v99).
   Fix: kubectl rollout undo deployment/demo-webapp -n default
   Verify: within 90s, new pod pulls the prior image and reaches Ready.
   Rollback: kubectl set image ... nginx=<current-broken-image> if fix worsens state.
   ```
6. Agent applies the fix via GKE MCP (`kubectl rollout undo` equivalent).
7. Agent waits ~90s.
8. Agent re-diagnoses: pod Running 1/1, no new ImagePullBackOff events.
9. Agent posts `INCIDENT SUMMARY` block:
   ```
   INCIDENT SUMMARY
   ================
   Status: RESOLVED
   Incident: default/demo-webapp-... (uid ...)
   Reason: ImagePullBackOff
   Cluster: <your cluster>
   Root cause: Wrong image tag
   Actions taken:
     1. record_plan("rollback to prior nginx image") → recorded
     2. rollout_undo deployment/demo-webapp -n default → applied
   Final state: pod Running 1/1; no new events for 90s.
   ```

**Say while it runs** (~4 min of streamed turns): "The agent is following a written reference — one of 12 that ship in v2.6, one per common k8s failure mode. Each reference has a fixed structure: diagnose steps, common-fixes table, when-to-escalate. Plan-first means before any mutating action, the agent records a written plan we can audit. That's happening in the eventlog you can query directly."

**Verification** (Pane B after the agent finishes):

```bash
kubectl -n "${TARGET_NS}" get pods -l app=demo-webapp
# Expect: Running 1/1
kubectl -n "${TARGET_NS}" get deployment demo-webapp -o jsonpath='{.spec.template.spec.containers[0].image}'
# Expect: nginx:1.25 (the prior good image)
```

### Scene 4 — Multi-user + ACL (2 min)

**Setup**: second TUI in Pane D, attached as `bob@example.com`.

```bash
# In a new terminal/pane
source "${DEMO_DIR:-/tmp/core-agent-demo}/demo-tokens.env"
core-agent-tui http://127.0.0.1:7777 --token BOB_TOKEN
```

Bob's session list is **empty** — he can't see Alice's incidents.

**Say**: "Same daemon, same running agent, different bearer token. Bob is a different SRE. He doesn't see the incident I just handled — it belongs to my identity. If we had per-team routing configured, Bob would only see incidents scoped to his team's namespaces. Substrate-level isolation."

**Optional demo**: fire a second incident in a namespace Bob owns. (Skip if time-constrained.)

### Scene 5 — Session resume across restart (2-3 min)

**Setup**: fire a second incident that takes long enough to demonstrate resume.

```bash
# In Pane B — inject a CrashLoopBackOff (agent will take longer since fix requires investigation)
kubectl -n "${TARGET_NS}" run demo-crash \
    --image=busybox:1.36 \
    --restart=Always \
    --command -- sh -c 'echo starting; sleep 5; echo crashing on purpose; exit 1'
```

Wait ~45s for the agent to start investigating (new session appears in Pane A TUI; agent is mid-diagnose).

**Execute the restart**:

```bash
# In Pane B — kill the daemon pod mid-investigation
kubectl -n "${DEMO_NS}" delete pod -l app.kubernetes.io/name=core-agent
```

**Say while pod recreates (~30s)**: "I just deleted the core-agent pod mid-triage. In v2.4 that would have lost the session; the operator would have to start over. v2.5 added session resume — sessions survive daemon restart because their ACL rows persist in SQLite, and the resumer transparently reconstructs them on next Lookup."

Watch Pane B:

```bash
kubectl -n "${DEMO_NS}" get pods -l app.kubernetes.io/name=core-agent -w
# New pod comes up Ready in ~15-30s; Ctrl-C when it's Ready
```

**Reconnect the TUI** (Pane A):

```bash
# In Pane A — the port-forward may need to be restarted
# Kill the prior port-forward, restart:
pkill -f "port-forward svc/core-agent" || true
kubectl -n "${DEMO_NS}" port-forward svc/core-agent 7777:7777 &
sleep 3

# Reattach TUI (same session ID)
core-agent-tui http://127.0.0.1:7777 --token SRE_TOKEN
```

**Verify resume**: the CrashLoopBackOff session should reappear (Status: idle → active after click-in). Conversation history intact from before the restart.

**Say**: "Same session ID, same conversation, same ACL. Kubelet may have taken 15 seconds to recreate the pod but the agent's state — the diagnosis it had made, the plan it was about to record — all came back."

Cleanup:

```bash
kubectl -n "${TARGET_NS}" delete pod demo-crash
```

### Scene 6 — The honest roadmap (2 min)

Say (no commands): "What you saw is v2.6, released a few days ago. The parts that make v2.7 fill out the picture:

- **Turnkey escalation to Slack / PagerDuty / webhook.** Today the agent writes an INCIDENT SUMMARY block to the eventlog; you'd wire a Cloud Logging sink or Kafka consumer to fan out to Slack. v2.7 adds a native `alert` tool with pre-registered targets — no shell, no external MCP required.

- **Proactive scheduled operations.** v2.6 is reactive — it wakes on k8s events. v2.7 adds a cron-driven sibling: nightly compliance sweeps, hourly blueprint drift detection, weekly cost audits. Same architectural pattern, different signal source.

- **OAuth-authenticated MCP servers.** Slack's official MCP requires OAuth 2.0. v2.7 adds the client-side plumbing to consume it — plus every other RFC 8414-compliant MCP as they ship (Notion, GitHub, Linear).

- **LLM-authored diagnostic tools via kode-gopher.** For diagnostics we don't have a purpose-built sensor for, the agent writes Go on the fly and executes in a sandbox. Combined with 5-8 pre-built sensors we ship for hot paths.

All four are designed and tracked; implementation is ~4-5 weeks. That's the release that pushes this from 'reactive first-responder' to 'always-working platform agent.'"

---

## Post-demo teardown

```bash
# Kill port-forward
pkill -f "port-forward svc/core-agent" || true

# Delete the demo workload (leaves the agent + sidecar running for the next rehearsal)
kubectl -n "${TARGET_NS}" delete deployment demo-webapp --ignore-not-found
kubectl -n "${TARGET_NS}" delete pod demo-crash --ignore-not-found

# Optional: wipe the eventlog for a clean state
kubectl -n "${DEMO_NS}" scale deployment/core-agent --replicas=0
kubectl -n "${DEMO_NS}" delete pvc core-agent-session-db
kubectl apply -k "${DEMO_OVERLAY_DIR:-/tmp/core-agent-demo/deploy/overlays/example}"
kubectl -n "${DEMO_NS}" scale deployment/core-agent --replicas=1
```

Wipe the scratch demo dir (safe — it holds only the working copy of `deploy/` plus the rehearsal tokens):

```bash
rm -rf "${DEMO_DIR:-/tmp/core-agent-demo}"
```

Full cluster teardown (only if the demo cluster is single-purpose):

```bash
gcloud container clusters delete "${CLUSTER_NAME}" --region="${REGION}" --project="${PROJECT_ID}" --quiet
```

---

## Troubleshooting

### `core-agent` pod stuck in `ContainerCreating`

Usually a mount failure. Check:

```bash
kubectl -n "${DEMO_NS}" describe pod -l app.kubernetes.io/name=core-agent | grep -A 5 Events
```

Common causes:
- **Secret not created**: re-run setup step 3.
- **PVC pending**: default StorageClass missing or unbindable. Check `kubectl get pvc -n "${DEMO_NS}"` and `kubectl get sc`.

### `core-agent` pod crashing with "config not found"

The ConfigMap didn't materialize. Re-run:

```bash
kubectl apply -k "${DEMO_OVERLAY_DIR:-/tmp/core-agent-demo/deploy/overlays/example}"
kubectl -n "${DEMO_NS}" get configmap core-agent-agents
```

### Daemon logs "Vertex AI: permission denied"

IAM binding didn't propagate (can take ~2 min after `./scripts/setup-wif.sh` runs). Wait 5 min, then:

```bash
kubectl -n "${DEMO_NS}" rollout restart deployment/core-agent
```

If it's still failing after propagation, the bindings themselves may be missing or wrong. Reapply idempotently:

```bash
NAMESPACE="${DEMO_NS}" ./examples/gke-troubleshoot-agent/scripts/setup-wif.sh
```

If it's still failing after that, check `roles/aiplatform.user` is actually on the KSA principal:

```bash
gcloud projects get-iam-policy "${PROJECT_ID}" \
    --flatten='bindings[].members' \
    --filter="bindings.role=roles/aiplatform.user AND bindings.members ~ ${DEMO_NS}/sa/core-agent-daemon" \
    --format='value(bindings.role)'
# Expect: roles/aiplatform.user
```

### `k8s-event-watcher` logs "connection refused" to daemon

Daemon isn't up yet OR its Service isn't routing. Check:

```bash
kubectl -n "${DEMO_NS}" get svc core-agent
kubectl -n "${DEMO_NS}" get endpoints core-agent
# Expect: endpoints backed by 1 pod IP
```

If empty endpoints, the daemon isn't Ready — check its own logs.

### TUI says "401 unauthorized"

Token mismatch. Verify:

```bash
source "${DEMO_DIR:-/tmp/core-agent-demo}/demo-tokens.env"
echo "$SRE_TOKEN" | head -c 20   # first 20 chars of your token
kubectl -n "${DEMO_NS}" get secret core-agent-users -o jsonpath='{.data.users\.json}' \
    | base64 -d \
    | jq -r '.users[] | select(.identity=="sre-oncall@example.com") | .token' \
    | head -c 20
# The two should match
```

If they differ, the Secret was created with old tokens. Rerun setup step 3.

### Agent doesn't fire on the injected failure

Two possibilities:
1. **Sidecar didn't see the event**. Check: `kubectl -n "${DEMO_NS}" logs deployment/k8s-event-watcher --tail=50`. Look for `fire ImagePullBackOff` or similar.
2. **Sidecar saw + injected but daemon rejected**. Check daemon logs: `kubectl -n "${DEMO_NS}" logs deployment/core-agent --tail=100 | grep -i inject`.

If neither log shows the event, the failure hasn't emitted the expected `reason`. Check what reason kubelet actually used:

```bash
kubectl -n "${TARGET_NS}" get events --sort-by='.lastTimestamp' --field-selector involvedObject.name=demo-webapp | tail -5
```

If reason is unexpected, adjust the demo scenario.

### Agent takes forever / doesn't finish

The `gemini-2.5-flash` model may hit rate limits under repeated demos. Symptoms: turns visible but stalling. Recover:

```bash
# Check Vertex quotas in the Cloud Console under IAM & Admin → Quotas & System Limits
# Filter for "aiplatform.googleapis.com" → "Generate content requests per minute"
```

If rate-limited, wait 60s + retry the same session.

---

## Agent-driven mode

If an agent (LLM + tools) is executing this runbook rather than a human:

1. **Every fenced code block is executable**. Run them via a bash tool; capture stdout + stderr.
2. **Every step has a checkpoint** — an `Expect:` line naming what output confirms success. Grep the tool output for the expected substring; fail loudly if absent.
3. **Wait times are explicit** — when a step says "wait ~30s", `sleep 30` and re-check.
4. **Decision branches** are explicit "if X, then Y" phrasings. Match against the tool output.
5. **Recovery paths** live under `## Troubleshooting`. When a step fails, don't proceed — look up the failure mode there and execute the recovery block.

Recommended agent workflow:

```
For each section in order:
  For each code block:
    Execute via `bash` tool
    Check stdout/stderr against the block's checkpoint
    If fail:
      Search Troubleshooting section for symptom
      If matching recovery block exists: execute it, retry the failed step
      Otherwise: STOP; escalate to human with the failure context
  Only proceed to next section after all steps in current section pass
```

Constraints for the agent:
- **Don't skip the pre-flight rehearsal**. It catches most failures before they'd embarrass a live demo.
- **Don't run the teardown before the demo**. Only after.
- **When triggering the failure scenarios (Scenes 2 + 5), pause between the trigger and the verification** to give the agent time to react. `sleep 30` after the trigger; then check the TUI's session picker via `curl -s -H "Authorization: Bearer ${SRE_TOKEN}" http://127.0.0.1:7777/sessions | jq`.
- **If the demo agent (running in-cluster) fails to auto-triage, that's not a runbook failure** — it's the demo failing. The runbook's job is to set up the conditions; the daemon's job is to react. Log both cases distinctly.

This runbook itself is stable across v2.6 patch releases (`v2.6.x`). Version bumps that change the recipe or the triage skill shape may require updates — check `git log examples/gke-troubleshoot-agent/DEMO.md` before executing against a newer core-agent tag.
