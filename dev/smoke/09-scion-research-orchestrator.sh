#!/usr/bin/env bash
# Smoke: scion-research-orchestrator binary boots, wires the
# v1.2.0 BackgroundAgentManager + v1.5.0 scionremote.Spawner, and
# successfully drives an in-process subagent end-to-end.
#
# This is the "happy path" verification for examples/scion-research-demo
# WITHOUT requiring a full Scion Hub deployment. When SCION_HUB_ENDPOINT
# is unset, the orchestrator falls back to agent.RefuseRemoteAgentSpawner
# so spawn_remote_agent returns a clean tool-result error — we assert
# the binary boots in that mode and the in-process spawn_agent path
# still works.
#
# The full Scion-Hub-end-to-end smoke is intentionally manual (it
# needs docker + scion-base + a built scion-research-demo image +
# the scion CLI). See examples/scion-research-demo/README.md.
#
# Required env: GOOGLE_CLOUD_PROJECT + ADC

set -euo pipefail
source "$(dirname "$0")/_common.sh"
require_env GOOGLE_CLOUD_PROJECT

# Build the orchestrator binary into /tmp/research-orchestrator so we
# don't pollute the repo. The build runs inside the extras module
# (separate go.mod with the Scion dep).
ORCHESTRATOR_BIN="${RESEARCH_ORCHESTRATOR:-/tmp/research-orchestrator}"
if [[ ! -x "${ORCHESTRATOR_BIN}" || "${1:-}" == "--force" ]]; then
    log_step "building research-orchestrator → ${ORCHESTRATOR_BIN}"
    (
        cd "$(repo_root)/extras/scion-remote-agent" \
        && GOWORK=off go build -o "${ORCHESTRATOR_BIN}" ./cmd/research-orchestrator
    )
fi

unset GEMINI_API_KEY GOOGLE_API_KEY

# Ensure SCION_* is NOT set — we want the refusal fallback for this
# smoke run.
unset SCION_HUB_ENDPOINT SCION_AGENT_TOKEN SCION_PROJECT_ID

log_step "research-orchestrator: spawns in-process subagent, refuses remote spawn cleanly"
output=$(
    GOOGLE_GENAI_USE_VERTEXAI=true \
    GOOGLE_CLOUD_LOCATION="${GOOGLE_CLOUD_LOCATION:-global}" \
    timeout 120 "${ORCHESTRATOR_BIN}" --provider=vertex \
        -m "${GOOGLE_GENAI_MODEL:-gemini-2.0-flash}" \
        --input "
You're the research orchestrator. Do two things in order:

1. Call spawn_agent to launch one in-process subagent named 'echo-1'
   with an empty tools list whose goal is 'reply with the literal string
   GOT_IT then call report_done'.
2. After the subagent reports back, attempt to call spawn_remote_agent
   with name='remote-1', a one-sentence system_prompt, and goal='noop'.
   Expect this call to fail because no Scion environment is configured.
   Report exactly what error message you got back.

Finish with sciontool_status('task_completed', '<one-sentence summary>').
" 2>&1 < /dev/null
)
echo "${output}"

# Bootup banner — confirms the fallback path engaged.
assert_contains "scion env not detected" "${output}"

# In-process spawn worked.
assert_contains "spawn_agent" "${output}"
assert_contains "echo-1" "${output}"

# Remote spawn refusal returned a clean error to the model. The
# refusal message contains "scion environment not configured" (see
# extras/scion-remote-agent/cmd/research-orchestrator/main.go).
assert_contains "spawn_remote_agent" "${output}"
assert_contains "scion environment not configured" "${output}"

pass "orchestrator boots, in-process spawn works, remote spawn refuses cleanly when Scion is absent"
