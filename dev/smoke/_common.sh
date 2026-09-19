#!/usr/bin/env bash
# Shared helpers for dev/smoke/ scripts. Source from each script.
#
# Exit code convention:
#   0  — passed
#   1  — failed (assertion mismatch, process error, etc.)
#   77 — skipped (required env vars missing); autotools convention
#
# run-all.sh aggregates these.

set -u
set -o pipefail

# ANSI styling when stdout is a TTY.
if [[ -t 1 ]]; then
    GREEN=$'\033[32m'
    RED=$'\033[31m'
    YELLOW=$'\033[33m'
    BOLD=$'\033[1m'
    RESET=$'\033[0m'
else
    GREEN=""; RED=""; YELLOW=""; BOLD=""; RESET=""
fi

# Default path the smoke scripts build/use core-agent at. Override
# with CORE_AGENT=/path/to/binary if you've already built one.
CORE_AGENT="${CORE_AGENT:-/tmp/core-agent}"

# Every smoke invocation pins its config with -c. Without a pin,
# config.Find walks UP from the process cwd and takes the first
# .agents/ it meets — so a .agents/ at the repo root, committed or just
# left behind by a local run, silently supplies the model, the
# permission mode and the budgets for a run that reports itself as
# passing. #1116/D3.
#
# --agents-dir does not substitute: loadConfig() reads config.json by
# walking up from cwd BEFORE resolveAgentsDir() applies that flag, so it
# moves the skills and MCP servers and leaves the settings behind.
# cmd/core-agent warns about exactly that split (splitTreeWarning).
#
# SMOKE_CONFIG is the pristine one: an empty config, so a run gets
# compiled-in defaults and nothing else. Scripts that need settings
# write their own .agents/config.json and pin to that instead.
#
# The pin moves the CONTENT ROOT too — agentsDir becomes Dir(-c) and
# projectRoot its parent — so a smoke run no longer picks up the
# checkout's own AGENTS.md. That is the point, not a side effect: run
# unpinned from the checkout, these scripts were loading the repo's
# contributor guide into the model's system prompt, and on a machine
# with a root .agents/AGENTS.md they failed outright with "failed to
# inject session state into instruction".
#
# What the pin does NOT close: instruction discovery still reaches
# ~/.core-agent/AGENTS.md and ~/.agents/AGENTS.md. A developer with a
# personal AGENTS.md there is still steering these runs.
#
# Deliberately NOT overridable from the environment. pristine_config
# truncates whatever this names, so honouring SMOKE_CONFIG=~/.agents/
# config.json would destroy a real config on the way to running a test.
# Point CORE_AGENT at a different binary if you want to vary a run;
# varying the config is what the -c pin exists to prevent.
SMOKE_CONFIG="${TMPDIR:-/tmp}/core-agent-smoke/pristine/.agents/config.json"

# pristine_config — (re)create the empty config SMOKE_CONFIG points at.
# Call before the first invocation in a script.
#
# The whole .agents/ goes, not just the file: pinning -c also makes this
# directory the content root, so plans, persisted permission grants and
# artifacts land here and would otherwise carry from one smoke script
# into the next. "Pristine" has to mean the tree, not one file in it.
pristine_config() {
    local dir
    dir="$(dirname "${SMOKE_CONFIG}")"
    case "${dir}" in
        */core-agent-smoke/pristine/.agents) rm -rf "${dir}" ;;
        *) fail "pristine_config: refusing to clear unexpected dir ${dir}" ;;
    esac
    mkdir -p "${dir}"
    printf '{"version": 1}\n' > "${SMOKE_CONFIG}"
}

# log_step <message> — step header (bold).
log_step() {
    printf '%s== %s ==%s\n' "${BOLD}" "$*" "${RESET}"
}

# pass <message> — green PASS line.
pass() {
    printf '%sPASS%s: %s\n' "${GREEN}" "${RESET}" "$*"
}

# fail <message> — red FAIL line + exit 1.
fail() {
    printf '%sFAIL%s: %s\n' "${RED}" "${RESET}" "$*" >&2
    exit 1
}

# skip <message> — yellow SKIP line + exit 77 (autotools "skipped").
skip() {
    printf '%sSKIP%s: %s\n' "${YELLOW}" "${RESET}" "$*"
    exit 77
}

# require_env VAR [VAR ...] — skip if any are unset/empty. Lists all
# missing vars together so a partial-creds setup gets one clear
# message rather than re-running just to discover the next gap.
require_env() {
    local missing=()
    for var in "$@"; do
        if [[ -z "${!var:-}" ]]; then
            missing+=("$var")
        fi
    done
    if (( ${#missing[@]} > 0 )); then
        skip "missing env vars: ${missing[*]}"
    fi
}

# require_one_of VAR [VAR ...] — skip if all are unset/empty.
require_one_of() {
    local var
    for var in "$@"; do
        if [[ -n "${!var:-}" ]]; then
            return 0
        fi
    done
    skip "needs at least one of: $*"
}

# build_core_agent [--force] — build core-agent into $CORE_AGENT
# unless it already exists. Idempotent.
build_core_agent() {
    local repo_root
    repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
    if [[ ! -x "${CORE_AGENT}" || "${1:-}" == "--force" ]]; then
        log_step "building core-agent → ${CORE_AGENT}"
        (cd "${repo_root}" && go build -o "${CORE_AGENT}" ./cmd/core-agent)
    fi
}

# assert_contains <expected_substring> <actual_text> — fail if the
# expected substring is not in actual_text. Uses fixed-string match
# (grep -F) so the expected value can contain regex meta-characters.
assert_contains() {
    local expected="$1" actual="$2"
    if ! grep -q -F -- "${expected}" <<<"${actual}"; then
        {
            printf 'expected to contain: %q\n' "${expected}"
            printf 'actual output (last 40 lines):\n'
            tail -n 40 <<<"${actual}"
        } >&2
        fail "missing expected substring: ${expected}"
    fi
}

# assert_not_contains <unexpected_substring> <actual_text> — fail if
# the unexpected substring IS in actual_text.
assert_not_contains() {
    local unexpected="$1" actual="$2"
    if grep -q -F -- "${unexpected}" <<<"${actual}"; then
        {
            printf 'expected NOT to contain: %q\n' "${unexpected}"
            printf 'actual output (last 40 lines):\n'
            tail -n 40 <<<"${actual}"
        } >&2
        fail "found unexpected substring: ${unexpected}"
    fi
}

# repo_root — print the absolute path of the repo root.
repo_root() {
    cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd
}
