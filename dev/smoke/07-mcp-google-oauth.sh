#!/usr/bin/env bash
# Smoke: connect to the Google-hosted GKE MCP server
# (container.googleapis.com/mcp) using OAuth access tokens minted from
# Application Default Credentials.
#
# Catches: mcp.AuthSpec parsing, google.FindDefaultCredentials wiring,
# googleAuthTransport injection, fail-fast pre-fetch, and a full
# round-trip through a real GKE MCP tool call.
#
# Required env:
#   GEMINI_API_KEY or GOOGLE_API_KEY     — the LLM provider for the agent
#   MCP_GOOGLE_OAUTH_SMOKE_PROJECT       — a GCP project the caller can
#                                          list GKE clusters in (the
#                                          project need not have any
#                                          clusters; the listing call
#                                          succeeding is the assertion)
#
# Required setup:
#   `gcloud auth application-default login` (or equivalent ADC source)
#   Caller has roles/mcp.toolUser and roles/container.clusterViewer
#   on $MCP_GOOGLE_OAUTH_SMOKE_PROJECT.

set -euo pipefail
source "$(dirname "$0")/_common.sh"

require_one_of GEMINI_API_KEY GOOGLE_API_KEY
require_env MCP_GOOGLE_OAUTH_SMOKE_PROJECT

# Verify ADC is available; skip cleanly if not. Keeps the smoke safe
# to run on machines without gcloud configured.
if ! gcloud auth application-default print-access-token >/dev/null 2>&1; then
    skip "Application Default Credentials not available (run \`gcloud auth application-default login\`)"
fi

build_core_agent

# Temporary .agents/ pointing at the Google-hosted GKE MCP server with
# OAuth auth. Cleaned up on exit.
workdir=$(mktemp -d)
trap 'rm -rf "${workdir}"' EXIT
mkdir -p "${workdir}/.agents"
cat >"${workdir}/.agents/mcp.json" <<'JSON'
{
  "version": 1,
  "servers": {
    "gke": {
      "transport": "http",
      "url": "https://container.googleapis.com/mcp",
      "auth": {
        "google_oauth": {
          "scopes": ["https://www.googleapis.com/auth/container.read-only"]
        }
      }
    }
  }
}
JSON

log_step "mcp-google-oauth: GKE MCP server via ADC OAuth access token"
output=$(
    cd "${workdir}" && "${CORE_AGENT}" --provider=gemini --yolo \
        -p "Use the GKE MCP server to list clusters in project ${MCP_GOOGLE_OAUTH_SMOKE_PROJECT}. Reply with only the cluster names (comma-separated), or the word NONE if there are no clusters." 2>&1
)
echo "${output}"

# Auth-path misconfigurations would surface as one of these. None of
# them should appear in a healthy run.
assert_not_contains "load Google default credentials" "${output}"
assert_not_contains "initial Google OAuth token fetch" "${output}"
assert_not_contains "401" "${output}"
assert_not_contains "PERMISSION_DENIED" "${output}"

# Run completed end-to-end (usage summary line is the cleanest "done"
# signal — same shape as 01-gemini-basic.sh).
assert_contains "core-agent:" "${output}"
assert_contains "turn(s)" "${output}"

pass "GKE MCP server reachable via OAuth ADC; tool call round-trip succeeded"
