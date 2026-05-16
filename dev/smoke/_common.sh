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
