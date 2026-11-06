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

# Smoke: VCS tool suite (`vcs_*`) configuration, startup summary, and
# end-to-end hermetic GitHub / GitLab / bare-git flow (#1490).
#
# Needs no network and no cloud credentials:
#   1. Boots core-agent with a pinned config declaring `vcs.forges` +
#      `vcs.repositories` and `--provider=echo`, asserting the startup
#      summary reports the configured forge and repository and that the
#      10 `vcs_*` tools register cleanly.
#   2. Runs the hermetic end-to-end flow test in pkg/tools/vcs against an
#      in-process smart-HTTP git remote + httptest GitHub/GitLab APIs
#      (capabilities -> clone -> edit -> diff -> commit -> publish ->
#      propose -> proposal_list/view/comment + token-file refusal).

set -euo pipefail
source "$(dirname "$0")/_common.sh"

SMOKE_DIR="${SMOKE_DIR:-${TMPDIR:-/tmp}/core-agent-vcs-smoke}"
TOKEN_FILE="${SMOKE_DIR}/github-token"
VCS_CONFIG="${SMOKE_DIR}/.agents/config.json"

cleanup() {
    rm -rf "${SMOKE_DIR}"
}
trap cleanup EXIT

rm -rf "${SMOKE_DIR}"
mkdir -p "${SMOKE_DIR}/.agents"
printf 'ghs_smoke_secret_token\n' > "${TOKEN_FILE}"
chmod 0600 "${TOKEN_FILE}"

cat > "${VCS_CONFIG}" <<EOF
{
  "version": 1,
  "model": { "name": "echo" },
  "permissions": { "mode": "yolo" },
  "vcs": {
    "forges": [
      {
        "name": "gh",
        "provider": "github",
        "host": "git.internal.example",
        "token_file": "${TOKEN_FILE}",
        "commit_identity": {
          "name": "platform-agent[bot]",
          "email": "platform-agent@acme.example"
        }
      }
    ],
    "repositories": [
      {
        "forge": "gh",
        "repository": "acme/infra",
        "role": "gitops",
        "base_branch": "main"
      }
    ],
    "workspace_dir": "${SMOKE_DIR}/repos",
    "branch_prefix": "agent/"
  }
}
EOF

build_core_agent --force

log_step "13-vcs-github: verify core-agent boots with vcs config and emits startup summary"
out=$("${CORE_AGENT}" -c "${VCS_CONFIG}" --provider=echo -p "vcs smoke check" 2>&1) || fail "core-agent run failed: ${out}"
assert_contains "vcs: 1 forge(s) (gh=github/git.internal.example), 1 repo(s) (acme/infra@gh[role=gitops,base=main]), branch_prefix=\"agent/\"" "${out}"
pass "core-agent booted with config.vcs and printed startup line"

log_step "13-vcs-github: run hermetic end-to-end GitHub/GitLab/git flow against httptest forge"
go test -v -run 'TestSuite_EndToEndGitOpsWorkflow|TestSuite_GuardsAndCredentialRefusal|TestSuite_ApproverReceivesDiffSummary' ./pkg/tools/vcs/... \
    || fail "hermetic vcs end-to-end flow failed"
pass "hermetic vcs_* end-to-end flow passed"
