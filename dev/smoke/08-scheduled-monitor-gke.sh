#!/usr/bin/env bash
# Smoke: run the scheduled-monitor UAT driver against a real GKE
# cluster, with a small sandbox deployment for the monitor to watch.
#
# What this catches (vs. the existing scheduled-monitor unit/example
# coverage): real Vertex traffic, real kubectl + Workload-Identity
# auth, real BackgroundAgentManager fan-out talking to a real cluster,
# real Scheduler-driven cadence over multiple wake cycles.
#
# Required env:
#   GOOGLE_CLOUD_PROJECT                 — GCP project for Vertex calls + (if --gsa) Workload Identity binding
#   GOOGLE_GENAI_USE_VERTEXAI=true       — pin Vertex over Gemini API
#   GOOGLE_CLOUD_LOCATION                — Vertex region (default us-central1)
#   plus `gcloud auth application-default login`
#
# Required flags:
#   --context <name>                     — kubectl context for the target GKE cluster
#   --namespace <name>                   — namespace to monitor and to host the sandbox deployment
#
# Optional flags:
#   --ksa <name>                         — Kubernetes ServiceAccount name; created if missing.
#                                          Default: $namespace's "default" SA (no creation, no WI binding).
#   --gsa <email>                        — Google ServiceAccount email to bind to --ksa via
#                                          Workload Identity. Requires --ksa.
#   --duration <go-duration>             — how long to let the UAT driver run (default 3m).
#   --no-deploy                          — skip applying the sandbox deployment (use this when
#                                          something is already running in --namespace).
#   --keep                               — don't tear down the sandbox deployment / KSA on exit.
#   --anomaly                            — inject a scale change halfway through to exercise
#                                          the child monitor's alert path.

set -euo pipefail
source "$(dirname "$0")/_common.sh"

# ----- arg parsing -----

KUBE_CONTEXT=""
NAMESPACE=""
KSA=""
GSA=""
DURATION="3m"
DO_DEPLOY=1
DO_CLEANUP=1
DO_ANOMALY=0

while (( $# > 0 )); do
    case "$1" in
        --context)    KUBE_CONTEXT="$2"; shift 2;;
        --namespace)  NAMESPACE="$2"; shift 2;;
        --ksa)        KSA="$2"; shift 2;;
        --gsa)        GSA="$2"; shift 2;;
        --duration)   DURATION="$2"; shift 2;;
        --no-deploy)  DO_DEPLOY=0; shift;;
        --keep)       DO_CLEANUP=0; shift;;
        --anomaly)    DO_ANOMALY=1; shift;;
        -h|--help)
            sed -n '2,/^set -euo/p' "$0" | sed 's/^# //; s/^#//' | head -n -1
            exit 0;;
        *) fail "unknown flag: $1";;
    esac
done

[[ -n "$KUBE_CONTEXT" ]] || fail "--context is required"
[[ -n "$NAMESPACE" ]] || fail "--namespace is required"
[[ -z "$GSA" || -n "$KSA" ]] || fail "--gsa requires --ksa"

# ----- env / tool prerequisites -----

require_env GOOGLE_CLOUD_PROJECT
require_one_of GOOGLE_GENAI_USE_VERTEXAI GEMINI_API_KEY GOOGLE_API_KEY ANTHROPIC_VERTEX_PROJECT_ID

for bin in kubectl gcloud go; do
    command -v "$bin" >/dev/null 2>&1 || skip "missing dependency: $bin"
done

if ! gcloud auth application-default print-access-token >/dev/null 2>&1; then
    skip "Application Default Credentials not available (run \`gcloud auth application-default login\`)"
fi

KCTL=( kubectl --context "$KUBE_CONTEXT" )

# ----- pre-flight cluster checks -----

log_step "pre-flight: cluster reachable"
if ! "${KCTL[@]}" get nodes >/dev/null 2>&1; then
    fail "cannot reach cluster via context '${KUBE_CONTEXT}' — check kubeconfig / VPN / IAM"
fi
node_count=$("${KCTL[@]}" get nodes -o name 2>/dev/null | wc -l | tr -d ' ')
pass "context=${KUBE_CONTEXT}, nodes=${node_count}"

log_step "pre-flight: namespace + RBAC"
if ! "${KCTL[@]}" get namespace "$NAMESPACE" >/dev/null 2>&1; then
    log_step "creating namespace ${NAMESPACE}"
    "${KCTL[@]}" create namespace "$NAMESPACE"
fi
if ! "${KCTL[@]}" -n "$NAMESPACE" auth can-i get pods >/dev/null 2>&1; then
    fail "missing RBAC: kubectl --context=${KUBE_CONTEXT} -n ${NAMESPACE} get pods is forbidden"
fi
pass "namespace=${NAMESPACE} accessible"

# ----- KSA + (optional) Workload Identity binding -----

if [[ -n "$KSA" ]]; then
    log_step "ServiceAccount ${KSA}"
    if ! "${KCTL[@]}" -n "$NAMESPACE" get sa "$KSA" >/dev/null 2>&1; then
        "${KCTL[@]}" -n "$NAMESPACE" create sa "$KSA"
    fi
    if [[ -n "$GSA" ]]; then
        log_step "Workload Identity: bind KSA=${KSA} → GSA=${GSA}"
        "${KCTL[@]}" -n "$NAMESPACE" annotate sa "$KSA" \
            "iam.gke.io/gcp-service-account=${GSA}" --overwrite
        if ! gcloud iam service-accounts add-iam-policy-binding "$GSA" \
                --role=roles/iam.workloadIdentityUser \
                --member="serviceAccount:${GOOGLE_CLOUD_PROJECT}.svc.id.goog[${NAMESPACE}/${KSA}]" \
                --project="$GOOGLE_CLOUD_PROJECT" >/dev/null 2>&1; then
            printf '%sWARNING%s: could not bind GSA via gcloud (need roles/iam.serviceAccountAdmin on %s). KSA annotation set; binding can be done manually.\n' \
                "${YELLOW}" "${RESET}" "${GSA}" >&2
        fi
    fi
    pass "KSA=${KSA} ready"
else
    KSA="default"
fi

# ----- sandbox deployment -----

DEPLOY_NAME="scheduled-monitor-smoke"

if (( DO_DEPLOY )); then
    log_step "applying sandbox deployment ${DEPLOY_NAME}"
    "${KCTL[@]}" -n "$NAMESPACE" apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${DEPLOY_NAME}
  labels: { app: ${DEPLOY_NAME}, smoke: scheduled-monitor }
spec:
  replicas: 2
  selector:
    matchLabels: { app: ${DEPLOY_NAME} }
  template:
    metadata:
      labels: { app: ${DEPLOY_NAME} }
    spec:
      serviceAccountName: ${KSA}
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          resources:
            requests: { cpu: "10m", memory: "16Mi" }
            limits:   { cpu: "50m", memory: "32Mi" }
YAML
    log_step "waiting for ${DEPLOY_NAME} rollout"
    "${KCTL[@]}" -n "$NAMESPACE" rollout status "deployment/${DEPLOY_NAME}" --timeout=120s
    ready=$("${KCTL[@]}" -n "$NAMESPACE" get "deployment/${DEPLOY_NAME}" -o jsonpath='{.status.readyReplicas}')
    want=$("${KCTL[@]}" -n "$NAMESPACE" get "deployment/${DEPLOY_NAME}" -o jsonpath='{.spec.replicas}')
    pass "deployment ready: ${ready}/${want} replicas"
fi

# ----- cleanup hook -----

OUT_FILE=""

cleanup() {
    if (( DO_CLEANUP )) && (( DO_DEPLOY )); then
        log_step "cleanup: deleting deployment ${DEPLOY_NAME}"
        "${KCTL[@]}" -n "$NAMESPACE" delete deployment "$DEPLOY_NAME" --wait=false >/dev/null 2>&1 || true
    fi
    if [[ -n "$OUT_FILE" ]]; then
        rm -f "$OUT_FILE"
    fi
    # KSA / namespace intentionally NOT deleted — the operator may
    # want to reuse them. Pass --keep + scrub manually if not.
}
trap cleanup EXIT

# ----- launch UAT driver -----

repo="$(repo_root)"
sess_db="/tmp/scheduled-monitor-uat-smoke/sessions.db"
sess_id="smoke-$(date -u +%Y%m%d-%H%M%S)"

# Goal pins the kubectl --context + --namespace explicitly so the
# model can't accidentally talk to the operator's default cluster.
goal=$(cat <<EOF
Monitor pods in Kubernetes context "${KUBE_CONTEXT}" namespace "${NAMESPACE}". Spawn ONE background subagent named "smoke-monitor" that:
  - On every wake: run \`kubectl --context=${KUBE_CONTEXT} --namespace=${NAMESPACE} get pods -o wide\` to inspect state.
  - Write its prior-scan baseline to an absolute path under /tmp/ (e.g. /tmp/scheduled-monitor-uat-smoke/smoke-monitor-baseline.json).
  - Call schedule_next_turn(wake_in_sec=30) between scans — fast cadence so the smoke completes in finite time.
  - report_alert on any change to the set of running pods (added, removed, or phase changed).

You (the supervisor) MUST always pass --context=${KUBE_CONTEXT} --namespace=${NAMESPACE} when running any kubectl yourself. Do NOT call kubectl without those flags — the operator's default context may point at a different cluster.
EOF
)

log_step "launching UAT driver for ${DURATION}"
printf 'session-db: %s\nsession-id: %s\n' "$sess_db" "$sess_id"

# Capture driver output for post-run inspection while also tee-ing
# live to the operator so they see the chat stream as it happens.
OUT_FILE=$(mktemp)

set +e
(
    cd "$repo"
    timeout --signal=INT --kill-after=30s "$DURATION" \
        go run ./dev/uat/scheduled-monitor \
            --provider=vertex \
            --max-wallclock="$DURATION" \
            --max-defer=5m \
            --session-db="$sess_db" \
            --session-id="$sess_id" \
            --goal="$goal" \
            </dev/null 2>&1 | tee "$OUT_FILE"
) &
DRIVER_PID=$!

# ----- optional anomaly injection mid-run -----

if (( DO_ANOMALY )); then
    # Parse Go-style duration (e.g. "3m", "90s", "1h") into seconds,
    # then sleep half of that before scaling so the monitor has time
    # to establish its baseline. Falls back to 60s on anything we
    # don't recognize.
    case "$DURATION" in
        *h) total_secs=$(( ${DURATION%h} * 3600 ));;
        *m) total_secs=$(( ${DURATION%m} * 60 ));;
        *s) total_secs=${DURATION%s};;
        *)  total_secs=120;;
    esac
    half_secs=$(( total_secs / 2 ))
    (( half_secs < 30 )) && half_secs=30
    log_step "anomaly: sleeping ${half_secs}s before scaling (baseline establishment)"
    sleep "$half_secs"
    log_step "anomaly: scaling ${DEPLOY_NAME} to 3 replicas"
    "${KCTL[@]}" -n "$NAMESPACE" scale "deployment/${DEPLOY_NAME}" --replicas=3 || true
    sleep 30
    log_step "anomaly: scaling back to 2 replicas"
    "${KCTL[@]}" -n "$NAMESPACE" scale "deployment/${DEPLOY_NAME}" --replicas=2 || true
fi

wait $DRIVER_PID
DRIVER_EXIT=$?
set -e

# ----- post-run assertions -----

log_step "post-run assertions"

# timeout returns 124 on time-cap; we treat that as "ran to completion
# of the smoke window" not as failure.
if (( DRIVER_EXIT != 0 && DRIVER_EXIT != 124 )); then
    fail "UAT driver exited ${DRIVER_EXIT} (expected 0 or 124)"
fi

# At least one schedule_next_turn call should have landed — proves
# the Scheduler primitive is round-tripping through Vertex.
assert_contains "schedule_next_turn" "$(cat "$OUT_FILE")"

# At least one spawn_agent call — proves the supervisor's fan-out works.
assert_contains "spawn_agent" "$(cat "$OUT_FILE")"

# If the anomaly was injected, at least one [alert] line should have
# fired from the child reaching the supervisor's drain.
if (( DO_ANOMALY )); then
    assert_contains "[alert]" "$(cat "$OUT_FILE")"
fi

pass "scheduled-monitor against GKE — all assertions passed"
