# v2.8.0 demo runbook — propose-only Kubernetes triage on GKE

Step-by-step runbook for demonstrating the v2.8.0 k8s triage agent on a real GKE cluster. Structured so a human (or an agent) can execute it top-to-bottom with explicit commands, expected outputs, wait times, and recovery paths.

**What the agent does**: diagnose → verify → propose → escalate. It reads cluster state through the read-only GKE MCP endpoint, checks whether the failure clears on its own, writes a proposed remediation into the incident summary, and pages on-call. It never mutates the cluster — enforced by configuration (read-only MCP endpoint, `tools.disable`, `roles/container.viewer`), not by prompt. Scene 3 is where that lands with an audience: the human applies the fix the agent proposed.

**Audience**: whoever's driving the demo — first-time or hundredth-time. Every command is copy-paste-executable; every checkpoint has a specific string to grep for; every wait has a duration.

**Prereq skill**: comfortable with `kubectl`, `gcloud`, terminal multiplexing (`tmux` or split panes).

**Time budget**: 30 min one-time setup + 5 min pre-flight + 15 min live demo + 3 min teardown.

---

## Table of contents

1. [Prerequisites](#prerequisites) — checkable one-liners
2. [One-time setup](#one-time-setup) — cluster + secrets + deploy
3. [Pre-flight rehearsal](#pre-flight-rehearsal) — 5-step sanity check before going live
4. [Live demo runbook](#live-demo-runbook) — 7 scenes with commands
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
# v2.8.0 GA images published on GHCR (should exist since we tagged
# v2.8.0). Three images ship from this repo; the watcher ships from
# go-steer/k8s-lookout as ghcr.io/go-steer/lookout (its ENTRYPOINT is
# `lookout watch`, a drop-in swap for the retired k8s-event-watcher
# image). Watcher floor is v0.11.0: earlier releases don't send
# Content-Type on the bodyless POST /sessions, which daemons ≥
# 2.8.0-dev.1 reject with 415 (#383's CSRF guard).
for img in core-agent core-agent-slim core-agent-tui; do
  crane digest "ghcr.io/go-steer/${img}:2.8.0" >/dev/null 2>&1 \
      && echo "✓ ghcr.io/go-steer/${img}:2.8.0 exists" \
      || echo "✗ ghcr.io/go-steer/${img}:2.8.0 NOT found — check the release-images workflow ran"
done
crane digest "ghcr.io/go-steer/lookout:v0.11.0" >/dev/null 2>&1 \
    && echo "✓ ghcr.io/go-steer/lookout:v0.11.0 exists" \
    || echo "✗ ghcr.io/go-steer/lookout:v0.11.0 NOT found — check the k8s-lookout release"
```

(If `crane` isn't installed, skip this — the deploy will fail loudly if an image is missing.)

### Local `core-agent-tui` binary

Three ways to get it — pick one:

```bash
# Option 1 (recommended): go install the v2.8.0 GA tag directly.
# v2.7.0 was the first go-install-able tag (#327's /v2 module-path
# rewrite — Go's SIVE rule requires the /v2 suffix once major ≥ 2).
# Pre-v2.7.0 tags stay uninstallable via `go install`; source
# builds + container images were never affected.
go install github.com/go-steer/core-agent/v2/cmd/core-agent-tui@v2.8.0

# Option 2: install from main (latest development; may include
# post-v2.8.0 changes).
go install github.com/go-steer/core-agent/v2/cmd/core-agent-tui@main

# Option 3: pull the published container image and extract the binary
docker pull ghcr.io/go-steer/core-agent-tui:2.8.0
CID=$(docker create ghcr.io/go-steer/core-agent-tui:2.8.0)
docker cp "${CID}:/usr/local/bin/binary" "${GOPATH:-$HOME/go}/bin/core-agent-tui"
docker rm "${CID}"
chmod +x "${GOPATH:-$HOME/go}/bin/core-agent-tui"

# Verify (any of the three)
which core-agent-tui \
    && echo "✓ core-agent-tui on PATH" \
    || (echo "✗ TUI not on PATH; ensure ${GOPATH:-$HOME/go}/bin is in \$PATH"; false)
core-agent-tui --version | grep -q "v2.8\|main-" \
    && echo "✓ TUI version looks right" \
    || echo "warning: version string unexpected (may still work)"
```

---

## One-time setup

Execute once per cluster. If re-running (fresh cluster after teardown), redo everything.

> **Ordering matters.** The daemon Deployment mounts a Secret (`core-agent-users`) at `/etc/core-agent/users.json` that isn't part of the kustomize output — it's created out-of-band in step 3. Deploy the workloads before the Secret exists and the pod hangs on `FailedMount`. Steps below create the namespace first, bind IAM, create the Secret, THEN apply the workloads. Rehearse in that order.
>
> The other per-cluster value (`GOOGLE_CLOUD_PROJECT` + `GOOGLE_CLOUD_LOCATION` for Vertex) IS in the kustomize output — see step 2's overlay-override note for how to change it for your project.

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

# AGENTS.md's project / cluster / region are ${env:VAR}-interpolated by
# the daemon at boot — resolved via envFrom on the daemon container
# from the core-agent-gcp-env ConfigMap. The base ConfigMap ships with
# go-steer's demo values (see deploy/base/kustomization.yaml); for your
# cluster, sed the literals list so kustomize regenerates it with your
# GCP project + GKE cluster + location. Missing required vars →
# fail-loud daemon exit at boot, per config/env.yaml.
sed -i \
    -e "s|GOOGLE_CLOUD_PROJECT=gke-demos-345619|GOOGLE_CLOUD_PROJECT=${PROJECT_ID}|" \
    -e "s|GCP_PROJECT=gke-demos-345619|GCP_PROJECT=${PROJECT_ID}|" \
    -e "s|GKE_CLUSTER=std-simian-test|GKE_CLUSTER=${CLUSTER_NAME}|" \
    -e "s|GKE_LOCATION=us-central1|GKE_LOCATION=${REGION}|" \
    "${DEMO_DEPLOY_DIR}/base/kustomization.yaml"

# Confirm the substitutions took.
grep -q -- "--cluster-name=${CLUSTER_NAME}" \
    "${DEMO_OVERLAY_DIR}/patch-watcher-cluster-name.yaml" \
    && echo "✓ cluster name patched (watcher)" \
    || (echo "✗ placeholder 'prod-us-central1' not found in patch file — check the source overlay"; false)
grep -q "GCP_PROJECT=${PROJECT_ID}" "${DEMO_DEPLOY_DIR}/base/kustomization.yaml" \
    && echo "✓ core-agent-gcp-env ConfigMap literals patched (GCP_PROJECT / GKE_CLUSTER / GKE_LOCATION)" \
    || (echo "✗ ConfigMap literals not substituted — check the base kustomization.yaml"; false)

# Create only the namespace here. Full `apply -k` (which creates the
# Deployments that mount the Secrets) waits until step 4, AFTER
# step 3 has created the Secrets.
kubectl create namespace "${DEMO_NS}" --dry-run=client -o yaml \
    | kubectl apply -f -
kubectl get ns "${DEMO_NS}" && echo "✓ namespace created"
```

### 2. Enable APIs + bind GCP IAM to the daemon's KSA

Run the recipe's WIF setup script — enables `container.googleapis.com`, `aiplatform.googleapis.com`, `iamcredentials.googleapis.com`, and binds all four IAM roles the daemon needs (`aiplatform.user`, `mcp.toolUser`, `container.viewer`, `iam.serviceAccountUser` on the node SA). Note `container.viewer`, not `container.admin`: this recipe wires the read-only GKE MCP endpoint and the agent is propose-only, so IAM is the outermost layer of that guarantee.

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

# Escalation webhook. config.json declares one alert target, `oncall`,
# whose URL comes from ONCALL_WEBHOOK_URL. The Deployment mounts this
# Secret with `optional: true`, so the pod boots either way — but the
# URL resolves at CALL time, so a missing value only surfaces when the
# agent first tries to escalate. Point it at a throwaway receiver
# (webhook.site, a Slack incoming webhook, whatever) for the demo.
kubectl -n "${DEMO_NS}" create secret generic core-agent-alerts \
    --from-literal=ONCALL_WEBHOOK_URL="${ONCALL_WEBHOOK_URL:-https://webhook.site/replace-me}"

# users.json is checked into the cluster Secret now — the local copy
# with plaintext tokens no longer needs to sit on disk. demo-tokens.env
# stays under DEMO_DIR so `source` in later steps still works.
rm "${DEMO_DIR}/users.json"

echo "✓ Secrets created; tokens stashed at ${DEMO_DIR}/demo-tokens.env"
```

**Vertex project/location** — the base kustomization ships defaults (`GOOGLE_CLOUD_PROJECT=gke-demos-345619` + `GOOGLE_CLOUD_LOCATION=global`) that work for the go-steer demo project. Every other operator overrides via `behavior: merge` in their overlay. If your `${PROJECT_ID}` differs from `gke-demos-345619` OR you need a different region, patch the scratch overlay's `kustomization.yaml` now:

```bash
# Append an override block to the overlay so kustomize merges your
# per-cluster values on top of the base defaults.
cat >> "${DEMO_OVERLAY_DIR}/kustomization.yaml" <<EOF

configMapGenerator:
  - name: core-agent-gcp-env
    behavior: merge
    literals:
      - GOOGLE_CLOUD_PROJECT=${PROJECT_ID}
      - GOOGLE_CLOUD_LOCATION=${REGION}

generatorOptions:
  disableNameSuffixHash: true
EOF

# Verify the override lands in the rendered output.
kubectl kustomize "${DEMO_OVERLAY_DIR}" \
    | grep -A 4 "name: core-agent-gcp-env"
# Expect: GOOGLE_CLOUD_PROJECT=${PROJECT_ID}, GOOGLE_CLOUD_LOCATION=${REGION}
```

### 4. Deploy the workloads

Now that the Secrets exist, apply the full recipe overlay. Kustomize creates the SAs, RBAC, PVC, both generated ConfigMaps (`core-agent-agents` + `core-agent-gcp-env`), Service, and both Deployments. The daemon pod schedules with all its mounts + env vars already present and comes up clean.

```bash
# Applies everything except the namespace (created in step 1) — SAs,
# ClusterRole/ClusterRoleBinding, PVC, both ConfigMaps (agents tree +
# GCP env vars), Service, and both Deployments (core-agent daemon +
# k8s-event-watcher).
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

### 5. Verify the daemon started clean (no MCP / Vertex init errors)

The daemon logs a fixed set of startup lines and stays quiet on success. There is NO "mcp: gke server ready" or "vertex: model init ok" line to grep for — MCP and Vertex init only surface as `core-agent: mcp: <err>` or `core-agent: <err>` on failure. So the pre-flight check is "grep for known-good startup + assert no error tail."

```bash
kubectl -n "${DEMO_NS}" logs deployment/core-agent --tail=50 > /tmp/daemon.log

# Expected startup fingerprint (exact strings, one per line):
grep -q "core-agent: pricing refresh:"        /tmp/daemon.log && echo "✓ pricing refresh"
grep -q "core-agent: agentic subtasks:"       /tmp/daemon.log && echo "✓ agentic subtasks configured"
grep -q "core-agent: watchdog:"               /tmp/daemon.log && echo "✓ watchdog running"
grep -q "core-agent: session db:"             /tmp/daemon.log && echo "✓ session db initialized"
grep -q "core-agent: attach listener on"      /tmp/daemon.log && echo "✓ attach listener up"
grep -q "core-agent: --no-repl: attach-only"  /tmp/daemon.log && echo "✓ --no-repl mode confirmed"

# Absence of these is success — the ONLY signal MCP or Vertex init failed:
!  grep -q "core-agent: mcp:"                 /tmp/daemon.log && echo "✓ no MCP errors"
!  grep -qiE "permission denied|PERMISSION_DENIED|insufficient authentication scopes" /tmp/daemon.log \
    && echo "✓ no Vertex auth errors"

rm /tmp/daemon.log
```

If a Vertex auth error appears, common causes:

- IAM binding hasn't propagated (wait 2 min); OR `roles/aiplatform.user` isn't bound (rerun `./scripts/setup-wif.sh`).
- WIF isn't wired at cluster level — check `gcloud container clusters describe ... --format='value(workloadIdentityConfig.workloadPool)'`.
- `iamcredentials.googleapis.com` not enabled — run `gcloud services enable iamcredentials.googleapis.com`.

If an MCP error appears (`core-agent: mcp: <details>`), the message itself names the failing server + reason (auth-scope missing, config parse error, endpoint unreachable).

**True end-to-end signal**: the ONLY way to be certain MCP + Vertex both work is Scene 2 driving an actual triage — the agent's first tool call exercises Vertex, its second-or-third exercises GKE MCP. If the pre-flight above passes and Scene 2 shows the agent calling `gke-mcp: logs.tail` (or similar), everything's wired. The CI e2e (`dev/tools/e2e-recipe-gke-troubleshoot-agent`, from TODO #211) covers everything short of that on every PR: deploy on kind, plus the full event → lookout inject → daemon turn pipeline on the echo provider — so a Scene 2 no-show narrows straight to Vertex/MCP credentials, not plumbing.

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

**Say**: "This is a live GKE cluster. Two pods in the `agent-triage` namespace: `core-agent` is the LLM-driven agent daemon; `k8s-event-watcher` is the sidecar (k8s-lookout's `lookout watch`) that turns Kubernetes Events into agent injects. My TUI is attached over port-forward with an SRE oncall bearer token. Session list is empty — nothing's wrong yet."

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

**Watch in Pane A** (TUI): within ~30s, a new session appears in the picker. Pane C (the watcher log) prints one line per successful inject ([#212](https://github.com/go-steer/core-agent/issues/212), shipped in lookout): `fire BackOff pod=<ns>/<pod> → sid=<sessionID> (mode=per-incident)`. Alternative confirmations:

```bash
# The kubernetes Event that triggered the inject (source truth)
kubectl -n "${TARGET_NS}" get events --field-selector reason=Failed --sort-by='.lastTimestamp' | tail -3

# Watcher counters (namespace of the DAEMON, not the target workload)
kubectl -n "${DEMO_NS}" port-forward deployment/k8s-event-watcher 9090:9090 &
curl -s http://localhost:9090/metrics | grep -E "k8s_event_watcher_events_(seen|injected)_total"
```

### Scene 3 — Agent auto-triages (4-5 min)

Click into the new session (arrow keys + Enter in TUI). Watch turns stream in real time.

**What the audience sees**:

1. Agent calls `record_plan` **first** — on a fresh session it has to. The
   daemon runs with `require_plan_artifact: true`, and every `gke` MCP call,
   including the read-only ones, is denied until a plan exists on disk. (The
   flag is per-session and sticky: demo this on the session's *first*
   incident, or the gate is already satisfied and the point doesn't land.)
   ```
   Incident: default/demo-webapp-... ImagePullBackOff
   Plan: load the ImagePullBackOff reference, confirm the image ref from
         the pod spec + events, check whether it clears on its own,
         then propose a fix. I cannot apply it.
   ```
2. Agent invokes the `k8s-triage` skill via `load_skill`
3. Router body says: "load `references/ImagePullBackOff.md`"
4. Agent calls `load_skill_resource` with `resource_path: references/ImagePullBackOff.md`
5. Agent runs the reference's **Diagnose (read-only)** steps via GKE MCP:
   - `gke_describe_k8s_resource` (Pod) → "Failed to pull image ... manifest unknown"
   - `gke_get_k8s_resource` → current image ref `nginx:this-tag-does-not-exist-v99`
   - `gke_list_k8s_events` → the pull failures are repeating, not a one-off
   - Classifies: "wrong tag (typo)"
6. Agent runs the reference's **convergence check** — an actual
   `wait_and_verify` against `gke_get_k8s_resource`, polling for the pod to
   reach Running. It times out: a nonexistent tag never self-heals.
7. Agent posts an `INCIDENT SUMMARY` with a proposal, not an action:
   ```
   INCIDENT SUMMARY
   ================
   Status: UNRESOLVED
   Incident: default/demo-webapp-... (uid ...)
   Reason: ImagePullBackOff
   Cluster: <your cluster>
   Root cause: Image tag nginx:this-tag-does-not-exist-v99 does not exist
               in the registry (manifest unknown).
   Evidence: describe → "manifest unknown"; 6 pull attempts in 4m;
             prior revision ran nginx:1.25 successfully.
   Proposal: roll deployment/demo-webapp back to the previous revision
             (nginx:1.25), or push the intended tag to the registry.
   Escalation: alert sent to oncall.
   Final state: 1/1 replicas unavailable; still backing off.
   ```
8. Agent calls `alert(target: "oncall", level: "critical", ...)`. The
   webhook fires; show the receiver if you wired a live one.

**Say while it runs** (~4 min of streamed turns): "The agent is following a written reference — one per common k8s failure mode. Each has a fixed structure: read-only diagnose steps, a convergence check, a remediation-proposal table, when-to-escalate. Two things to notice. First, the plan came *before* the investigation — plan-first here gates every MCP call, not just mutations, so there's an auditable written plan for every incident. Second, it's proposing, not fixing. That's not the prompt being polite: the only MCP server wired in is the read-only GKE endpoint, `bash` and the file-write tools are disabled in config, and the KSA only holds `container.viewer`. There is no mutating verb in its tool catalog to reach for."

**The payoff line**: "Most agent demos show you an agent that says it's safe. This one is safe because it can't be otherwise — and the status says `UNRESOLVED` because nothing resolved it. `RESOLVED` in this recipe requires a `wait_and_verify` call that actually observed recovery. The agent can't claim a fix it didn't make."

**Then a human applies it** (Pane B — this is the point, don't skip it):

```bash
kubectl -n "${TARGET_NS}" rollout undo deployment/demo-webapp
kubectl -n "${TARGET_NS}" rollout status deployment/demo-webapp --timeout=90s

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
# In Pane B — inject a CrashLoopBackOff (a longer turn: the reference's
# convergence check polls before the agent can conclude anything)
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

**Say**: "Same session ID, same conversation, same ACL. Kubelet may have taken 15 seconds to recreate the pod but the agent's state — the diagnosis it had made, the plan it was about to record — all came back. Note what this scene needed: **me**. I reconnected the TUI, clicked back into the session. Session resume brings the state back; a human still drives the next turn. The next scene removes the human."

Cleanup:

```bash
kubectl -n "${TARGET_NS}" delete pod demo-crash
```

### Scene 6 — Auto-continue: the agent finishes its own turn across a restart (2-3 min)

This is the v2.8.0 headline, and the sharp contrast with Scene 5. Session resume (Scene 5) restores a session's state so a **human** can pick it back up. **Auto-continue** goes further: if the daemon dies while the agent is mid-turn — a tool call streaming, a plan half-written — the *next* boot detects the interrupted turn and finishes it **autonomously**. No operator reconnects. No one clicks anything. And as of v2.8.0 it's **on by default** for this deployment shape — no config to set. (Design: [`docs/auto-continue-design.md`](../../docs/auto-continue-design.md); the state-persistence layer it builds on is [`docs/session-resume-design.md`](../../docs/session-resume-design.md).)

**Why it's on with zero config here**: auto-continue defaults ON when the preconditions hold — a durable eventlog (`--session-db`) plus either a multi-session daemon or `--no-repl`. This recipe's daemon is all three (`multi_session.enabled: true`, `--no-repl`, `--session-db` on a PVC), so it inherits the default. An interactive REPL or an in-process library embedding never trips it. To opt out, set `agent.auto_continue.enabled: false`.

**Setup**: fire an incident, then kill the pod *while the agent is mid-turn* — same timing as Scene 5, but this time **do not reconnect a TUI**. The point is that nobody has to.

```bash
# In Pane B — fire an incident that gives the agent a multi-step turn to be interrupted mid-flight
kubectl -n "${TARGET_NS}" run demo-crash \
    --image=busybox:1.36 \
    --restart=Always \
    --command -- sh -c 'echo starting; sleep 5; echo crashing on purpose; exit 1'
```

Wait ~30-45s until Pane A shows the agent actively working the session (a tool call in flight, or a plan being written).

**Execute the restart — and then walk away from the keyboard**:

```bash
# In Pane B — kill the daemon pod mid-turn
kubectl -n "${DEMO_NS}" delete pod -l app.kubernetes.io/name=core-agent
```

**Say while the pod recreates (~30s)**: "In Scene 5 I killed the pod and then *I* came back to resume. This time I'm not touching anything. When the new pod boots it scans the eventlog, sees a turn that was interrupted mid-flight, checks its guardrails — the turn is fresh enough, it hasn't already retried this interruption, it's under the per-session and cost caps, no crash-loop — and then it just... keeps going. Finishes the diagnosis, runs the convergence check, writes the incident summary, pages on-call. The operator was never in the loop."

**Verify the autonomous continuation** (Pane B — no TUI, just the daemon log):

```bash
# Watch the fresh pod pick the turn back up on its own. On boot the
# daemon logs the default-on notice, then — once the boot scan finds
# the mid-flight turn — an "auto-continue queued" line naming how long
# ago the turn was interrupted.
kubectl -n "${DEMO_NS}" logs -f deployment/core-agent | grep -i "auto-continue"
# Expect (default-on notice): "auto_continue on by default (multi-session/--no-repl + durable eventlog) ..."
# Expect (the resume itself):  "session <id>: auto-continue queued (turn interrupted <N>s ago)"
```

```bash
# And confirm the incident actually got worked to completion with no human
kubectl -n "${TARGET_NS}" get pods -l app=demo-crash
# Then check the eventlog for the agent's own post-restart turns / INCIDENT SUMMARY.
```

**Say**: "That's the difference between *resilient* and *unattended*. Session resume means you don't lose work when a pod dies. Auto-continue means the work doesn't even pause — the agent picks up its own turn and drives it home. And because it's precondition-gated, it only ever fires where it's safe: a durable, headless or multi-session daemon. Your laptop REPL is never going to spend tokens behind your back."

Cleanup:

```bash
kubectl -n "${TARGET_NS}" delete pod demo-crash --ignore-not-found
```

### Scene 7 — The honest roadmap (2 min)

Say (no commands): "Three releases got us here. v2.6 was the reactive first-responder — the watcher, per-incident sessions, the triage skill. v2.7 was the plumbing release: `go install`-able, end-to-end OpenTelemetry tracing, and the cost-attribution stack — structural digest on MCP results, Vertex context caching, per-turn cost you can actually attribute. v2.8.0 is the durability release: OTel metrics alongside the traces, the daemon's substrate extracted into reusable libraries (`pkg/compose`, `pkg/pricing`), attach-side per-caller cost rate limiting, and the auto-continue default-on you just watched.

And what's running in front of you is v2.9, because two of the things you saw today only exist there: `wait_and_verify`, the bounded read-only poll that made the convergence check real instead of a `sleep` the daemon can't run, and the native `alert` tool that turned escalation from a note in the eventlog into an actual page. Both of those are less features than corrections — the recipe *said* it did them for two releases before it could. Let me be equally precise about what I'm **not** claiming: the `alert` tool ships one template, `generic`, a JSON POST. Slack-shaped and PagerDuty-shaped payloads are designed and rejected at config load, so today you point the webhook at a receiver that fans out. Cron-driven proactive operations — nightly compliance sweeps, drift detection, cost audits — and LLM-authored diagnostic tools are both designed and neither is shipped. Nothing you saw today is running either one.

Where it goes next: v2.9 is the **one-contract-many-companions** direction — the same daemon contract fronted by purpose-built companion gateways (chat, and beyond), so this agent isn't just a k8s first-responder reachable over a TUI but a substrate you can put any interface in front of. That's the arc: reactive first-responder → always-working platform agent → embeddable substrate."

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
1. **Sidecar didn't see the event**. The watcher is silent on successful inject (fixed by [#212](https://github.com/go-steer/core-agent/issues/212)), so check the metrics endpoint instead: `kubectl -n "${DEMO_NS}" port-forward deployment/k8s-event-watcher 9090:9090 & sleep 2 && curl -s http://localhost:9090/metrics | grep -E "watcher_(events_(seen|injected|deduped)|inject_errors)_total"`. If `seen` incremented but neither `injected` nor `deduped` did, the event was filtered out by the reason allow-list. If `inject_errors` incremented, the daemon POST failed (check the watcher log for the error line). If nothing incremented, the k8s event never landed — check `kubectl -n "${TARGET_NS}" get events` directly.
2. **Sidecar saw + injected but daemon rejected**. Check daemon logs: `kubectl -n "${DEMO_NS}" logs deployment/core-agent --tail=100 | grep -i inject`.

If neither log shows the event, the failure hasn't emitted the expected `reason`. Check what reason kubelet actually used:

```bash
kubectl -n "${TARGET_NS}" get events --sort-by='.lastTimestamp' --field-selector involvedObject.name=demo-webapp | tail -5
```

If reason is unexpected, adjust the demo scenario.

### Agent takes forever / doesn't finish

The `gemini-3.5-flash` model may hit rate limits under repeated demos. Symptoms: turns visible but stalling. Recover:

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

This runbook itself is stable across v2.8.0 patch releases (`v2.8.x`). Version bumps that change the recipe or the triage skill shape may require updates — check `git log examples/gke-troubleshoot-agent/DEMO.md` before executing against a newer core-agent tag.
