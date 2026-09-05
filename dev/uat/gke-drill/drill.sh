#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# The GKE drill (#970) — run one scenario against a live cluster and
# produce a scorecard.
#
#   ./drill.sh a          bad image tag -> ImagePullBackOff
#   ./drill.sh b          memory limit  -> OOMKilled
#   ./drill.sh c          RBAC-denied ServiceAccount  (the negative case)
#
# Read README.md before the first run. In short: this deploys nothing,
# it breaks a workload in an already-deployed recipe, waits for the
# watcher to raise an incident, captures the transcript, injects one
# follow-up, restores the cluster, and writes a scorecard with two of
# the six boxes filled in and four left for you.
#
# It is deliberately a script and a checklist rather than a test. Do not
# automate the rubric until the drill has run at least three times and
# the rubric has stopped changing (#652).
set -euo pipefail

DRILL_SELF_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
# shellcheck source=lib.sh
source "${DRILL_SELF_DIR}/lib.sh"

usage() {
    cat >&2 <<'EOF'
usage: ./drill.sh <a|b|c>

  a   bad image tag -> ImagePullBackOff
  b   memory limit  -> OOMKilled
  c   RBAC-denied ServiceAccount   (the negative case; scores G2)

Runs one scenario against the already-deployed examples/gke-platform-agent
recipe, captures the transcript, and writes a scorecard. Read README.md
before the first run.

Environment (all optional):
  DRILL_INJECT=manual        drive the G6 follow-up yourself from the TUI
  DRILL_INJECT_AFTER=75      seconds after the incident before injecting it
  DRILL_IDLE_SECS=90         quiet time that counts as "the turn is over"
  DRILL_MAX_SECS=1200        hard cap on one capture
  DRILL_PORT=7779            local port for the hub tunnel
  FORCE=1                    score even with a foreign watcher racing
  WORKLOAD / TARGET_NS / …   inherited from the recipe's scripts/prereqs.sh
EOF
    exit "${1:-1}"
}

case "${1:-}" in
    a|A) SCENARIO_FILE="a-bad-image.sh" ;;
    b|B) SCENARIO_FILE="b-oom.sh" ;;
    c|C) SCENARIO_FILE="c-rbac-denied.sh" ;;
    -h|--help) usage 0 ;;
    *) usage 1 ;;
esac

# How long after the incident session appears to send the G6 follow-up.
# The point is to land it MID-run, while the agent has evidence but has
# not finished; too early and there is nothing to reference, too late
# and it becomes an ordinary second turn.
DRILL_INJECT_AFTER="${DRILL_INJECT_AFTER:-75}"

# DRILL_INJECT=manual leaves G6 to a human at the TUI. Useful on the
# first run of a new scenario, when you want to see what the agent is
# doing before deciding what to ask it.
DRILL_INJECT="${DRILL_INJECT:-auto}"

# ── Run directory ────────────────────────────────────────────────────

RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-${1,,}"
DRILL_RUN_DIR="${DRILL_RUN_ROOT}/${RUN_ID}"
mkdir -p "${DRILL_RUN_DIR}"

# shellcheck source=scenarios/a-bad-image.sh
source "${DRILL_SELF_DIR}/scenarios/${SCENARIO_FILE}"

drill_banner "GKE drill — scenario ${SCENARIO_ID}: ${SCENARIO_NAME}"
drill_log "run dir: ${DRILL_RUN_DIR}"

# ── Cleanup ──────────────────────────────────────────────────────────
#
# Restoring the cluster matters more than finishing the drill: a run
# that dies half-way must not leave a workload broken for whoever picks
# up the cluster next. So restore is a trap, and it is idempotent.
DRILL_RESTORED=""
DRILL_INJECT_PID=""
drill_cleanup() {
    local rc=$?
    trap - EXIT
    if [[ -n "${DRILL_INJECT_PID}" ]] && kill -0 "${DRILL_INJECT_PID}" 2>/dev/null; then
        drill_warn "the scheduled follow-up had not fired yet — cancelling it."
        kill "${DRILL_INJECT_PID}" 2>/dev/null || true
        wait "${DRILL_INJECT_PID}" 2>/dev/null || true
    fi
    drill_stop_port_forward
    if [[ -z "${DRILL_RESTORED}" && -n "${DRILL_BROKEN:-}" ]]; then
        drill_warn "restoring the cluster on the way out (exit ${rc})"
        scenario_restore || drill_warn "restore FAILED — check the cluster by hand."
    fi
    exit "${rc}"
}
trap drill_cleanup EXIT INT TERM

# ── 1. Preflight ─────────────────────────────────────────────────────

drill_banner "1/7  preflight"
drill_preflight
drill_load_token
drill_port_forward

# Pin what actually deployed, so a scorecard is attributable months
# later. A run that cannot say which image produced it is an anecdote.
# Select the container by NAME, not by index: an injected sidecar (a
# proxy, a log shipper, whatever the cluster's admission webhooks add)
# can take slot 0, and a scorecard attributing the run to an istio image
# is worse than one that says "?".
DAEMON_IMAGE=$(kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" \
    get deploy core-agent -o jsonpath='{.spec.template.spec.containers[?(@.name=="core-agent")].image}' 2>/dev/null || true)
if [[ -z "${DAEMON_IMAGE}" ]]; then
    DAEMON_IMAGE=$(kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" \
        get deploy core-agent -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)
    [[ -z "${DAEMON_IMAGE}" ]] || drill_warn \
        "no container named core-agent; attributing the run to containers[0]: ${DAEMON_IMAGE}"
fi
DAEMON_IMAGE="${DAEMON_IMAGE:-?}"
CONTENT_IMAGE_DEPLOYED=$(kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" \
    get deploy core-agent -o jsonpath='{.spec.template.spec.volumes[?(@.name=="content")].image.reference}' 2>/dev/null || true)
if [[ -z "${CONTENT_IMAGE_DEPLOYED}" ]]; then
    # initContainer-copy overlay: the content ref is on the init
    # container instead of on an image volume.
    CONTENT_IMAGE_DEPLOYED=$(kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" \
        get deploy core-agent -o jsonpath='{.spec.template.spec.initContainers[?(@.name=="install-content")].image}' 2>/dev/null || true)
fi
drill_ok "daemon  ${DAEMON_IMAGE}"
drill_ok "content ${CONTENT_IMAGE_DEPLOYED:-<not resolved>}"

SESSIONS_BEFORE=$(drill_session_ids) || drill_die \
    "could not list sessions on the hub. The tunnel is up, so this is almost
  certainly the token: re-run the recipe's ./scripts/gen-tokens.sh, which also
  restarts both Deployments to pick the new one up."
drill_ok "$(printf '%s' "${SESSIONS_BEFORE}" | grep -c . || true) session(s) on the hub before the break"

# ── 2. Break ─────────────────────────────────────────────────────────

drill_banner "2/7  arming scenario ${SCENARIO_ID}"
DRILL_BROKEN=1
scenario_break || drill_die "the scenario failed to arm — nothing to score."

# Baselines AFTER the break settles: the drill's own damage belongs
# inside the baseline, or G4 reports the drill as a mutation.
sleep 10
GENERATION_BEFORE=$(drill_target_generation)
FINGERPRINT_BEFORE=$(drill_target_fingerprint)

# ── 3. Wait for the incident ─────────────────────────────────────────

drill_banner "3/7  waiting for the watcher to raise an incident"
drill_log "up to ${DRILL_SESSION_TIMEOUT}s; follow along with:"
drill_log "  kubectl --context ${KUBE_CONTEXT} -n ${DEMO_NS} logs deploy/lookout-watch -f"

SESSION_ID=$(drill_wait_new_session "${SESSIONS_BEFORE}") || drill_die \
    "no new session appeared within ${DRILL_SESSION_TIMEOUT}s.
  The break landed but the incident never did. Check, in order:
    kubectl --context ${KUBE_CONTEXT} -n ${TARGET_NS} get events --sort-by=.lastTimestamp | tail
    kubectl --context ${KUBE_CONTEXT} -n ${DEMO_NS} logs deploy/lookout-watch --tail=50
    kubectl --context ${KUBE_CONTEXT} -n ${DEMO_NS} logs deploy/core-agent --tail=50"
drill_ok "incident session: ${SESSION_ID}"

# ── 4. Follow-up, scheduled to land mid-run ──────────────────────────

drill_banner "4/7  capturing the turn"
FOLLOWUP_SENT="${SCENARIO_FOLLOWUP}"
if [[ "${DRILL_INJECT}" == "manual" ]]; then
    printf '\n'
    printf '  G6 IS YOURS TO DRIVE. In another terminal, now:\n\n'
    printf '      cd %s\n' "${DRILL_RECIPE_DIR}"
    printf '      ./scripts/attach.sh %s %s\n\n' "${DRILL_APP}" "${SESSION_ID}"
    printf '  Ask a follow-up that can only be answered from evidence already\n'
    printf '  gathered. The suggested one for this scenario:\n\n'
    printf '      %s\n\n' "${SCENARIO_FOLLOWUP}"
    # Block until the human is actually attached. The capture replays
    # from seq 0 so nothing is lost by waiting, but its idle timer
    # starts the moment it does — begin now and a human who takes three
    # minutes to get to the TUI finds the capture already closed, and
    # G6 unscoreable for a reason that has nothing to do with the agent.
    if [[ -t 0 ]]; then
        printf '  Press Enter once you are attached and ready. '
        read -r
    else
        drill_warn "DRILL_INJECT=manual with no tty — starting the capture immediately."
    fi
    printf '  If you ask something other than the text above, correct the\n'
    printf '  followup field in meta.json before scoring, or G6 will read as\n'
    printf '  "the follow-up never landed".\n\n'
else
    drill_log "follow-up will be injected in ${DRILL_INJECT_AFTER}s"
    (
        sleep "${DRILL_INJECT_AFTER}"
        payload=$(jq -nc --arg m "${SCENARIO_FOLLOWUP}" '{message: $m}')
        if hub_post "/sessions/${DRILL_APP}/${SESSION_ID}/inject" "${payload}" \
                > "${DRILL_RUN_DIR}/inject-response.json" 2>&1; then
            printf '✓ follow-up injected\n'
        else
            printf '⚠ follow-up inject FAILED — see %s/inject-response.json\n' "${DRILL_RUN_DIR}" >&2
        fi
    ) &
    # Track it so cleanup can kill it. An injector that outlives the
    # drill wakes a session after the cluster has been restored, which
    # both corrupts a later capture and leaves the agent reasoning about
    # a workload whose failure no longer exists.
    DRILL_INJECT_PID=$!
fi

drill_capture_parent "${SESSION_ID}"

# If the capture ended before the follow-up fired — the stream closed
# early, or the turn was over in under DRILL_INJECT_AFTER seconds —
# cancel it rather than letting it land unwatched. An inject the capture
# cannot see is not G6 evidence, it is just an extra turn nobody read.
if [[ -n "${DRILL_INJECT_PID}" ]] && kill -0 "${DRILL_INJECT_PID}" 2>/dev/null; then
    kill "${DRILL_INJECT_PID}" 2>/dev/null || true
    wait "${DRILL_INJECT_PID}" 2>/dev/null || true
    FOLLOWUP_SENT=""
    drill_warn "the capture closed before the ${DRILL_INJECT_AFTER}s follow-up fired — G6 was not exercised."
    drill_warn "  Lower DRILL_INJECT_AFTER, or raise DRILL_IDLE_SECS, and run it again."
fi
DRILL_INJECT_PID=""

drill_capture_subagents "${SESSION_ID}"

# ── 5. After-state ───────────────────────────────────────────────────

drill_banner "5/7  after-state"
GENERATION_AFTER=$(drill_target_generation)
FINGERPRINT_AFTER=$(drill_target_fingerprint)
drill_ok "generation ${GENERATION_BEFORE} -> ${GENERATION_AFTER}"

# ── 6. Restore ───────────────────────────────────────────────────────

drill_banner "6/7  restoring the cluster"
scenario_restore || drill_warn "restore reported a problem."
DRILL_RESTORED=1
sleep 10
if scenario_verify_restored; then
    drill_ok "cluster is back"
else
    # break-workload.sh restore exits 0 even when `rollout undo` failed
    # (the recipe's DEMO.md says so). Checking the outcome rather than
    # the exit code is the difference between the next drill run being
    # a measurement and being noise.
    drill_warn "THE CLUSTER IS STILL BROKEN. Fix it before the next run:"
    drill_warn "    cd ${DRILL_RECIPE_DIR} && WORKLOAD=${WORKLOAD} ./scripts/break-workload.sh restore"
    drill_warn "    kubectl --context ${KUBE_CONTEXT} -n ${TARGET_NS} rollout history deployment/${WORKLOAD}"
fi

# ── 7. Score ─────────────────────────────────────────────────────────

drill_banner "7/7  scoring"

jq -n \
    --arg run_id "${RUN_ID}" \
    --arg started_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg scenario_id "${SCENARIO_ID}" \
    --arg scenario_name "${SCENARIO_NAME}" \
    --arg negative "${SCENARIO_NEGATIVE}" \
    --arg cluster "${CLUSTER_NAME}" \
    --arg project "${PROJECT_ID}" \
    --arg demo_ns "${DEMO_NS}" \
    --arg target_ns "${TARGET_NS}" \
    --arg workload "${WORKLOAD}" \
    --arg model_flavor "${MODEL_FLAVOR}" \
    --arg daemon_image "${DAEMON_IMAGE}" \
    --arg content_image "${CONTENT_IMAGE_DEPLOYED:-}" \
    --arg session_id "${SESSION_ID}" \
    --arg followup "${FOLLOWUP_SENT}" \
    --arg generation_before "${GENERATION_BEFORE}" \
    --arg generation_after "${GENERATION_AFTER}" \
    --argjson frame_count "$(wc -l < "${DRILL_RUN_DIR}/transcript.jsonl" | tr -d ' ')" \
    --argjson expect_terms "$(printf '%s\n' "${SCENARIO_EXPECT_TERMS[@]}" | jq -R . | jq -s .)" \
    --argjson fingerprint_before "$(printf '%s\n' "${FINGERPRINT_BEFORE}" | grep . | jq -R . | jq -s . || echo '[]')" \
    --argjson fingerprint_after "$(printf '%s\n' "${FINGERPRINT_AFTER}" | grep . | jq -R . | jq -s . || echo '[]')" \
    '$ARGS.named' > "${DRILL_RUN_DIR}/meta.json"

python3 "${DRILL_SELF_DIR}/score.py" --run-dir "${DRILL_RUN_DIR}"

drill_banner "done — scenario ${SCENARIO_ID}"
cat <<EOF
  Evidence:  ${DRILL_RUN_DIR}/evidence.md
  Rubric:    ${DRILL_SELF_DIR}/SCORECARD.md

  Next:
    1. cp ${DRILL_SELF_DIR}/SCORECARD.md \\
         ${DRILL_SELF_DIR}/runs/$(date -u +%Y-%m-%d)-${CLUSTER_NAME}-${SCENARIO_ID,,}.md
    2. Fill it in, reading the evidence sheet and the transcript. G4 and G5
       are already decided there; G1, G2, G3 and G6 are yours.
    3. Commit it with the trailer the focus metric reads:

         git commit --trailer 'live-uat: ${CLUSTER_NAME} <pass|fail>' \\
             dev/uat/gke-drill/runs/

  The run directory is under TMPDIR and will not survive a reboot. Copy
  anything a finding cites.
EOF
