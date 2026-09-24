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

# The self-development UAT (#1116, P4) — can core-agent do development
# work on core-agent?
#
# This drives one tier of the #1116 ladder against a throwaway clone and
# grades the result. The grading is the point. A self-development run
# produces a ground-truthable outcome for free, in the way the #652
# cluster-fact evals do: CI is a witness that cannot be talked into a
# pass, and the git state of the REAL checkout is a witness that cannot
# be talked into "I stayed in the sandbox".
#
# Every assertion below is read off an artifact the agent does not
# author. The transcript says what the model believes it did; the clone's
# git log, the PR on GitHub, the plan file on disk and the real
# checkout's `git status` say what happened. Where the two disagree the
# artifact wins. Same rule as the #652 evals and the approval-gate UAT.
#
# D4 is why this clones: tiers 0 and 1 operate on a clone under /tmp,
# never on the real checkout. A runaway run damages a throwaway
# directory, and a session confined to a fresh clone has no stash stack
# to collide with. Assertion A9 is the enforcement — it fails the run if
# the real checkout moved at all.
#
# D5 is why it stops at "PR open": CI is the ground truth and the agent
# never merges.
set -euo pipefail

SELF_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
REPO_ROOT=$( cd -- "${SELF_DIR}/../../.." &> /dev/null && pwd )

# ── Configuration ────────────────────────────────────────────────────

TIER="${SELFDEV_TIER:-t0}"
DRY_RUN=0
KEEP=0
# The provider the PARENT runs on. The recipe pins `anthropic`; an
# operator on Vertex overrides it here. The reviewer subagent declares no
# model precisely so that this one flag moves both — see D3's P4
# correction, which is the bug this script found.
PROVIDER="${SELFDEV_PROVIDER:-}"
# Scratch root. /tmp, never $HOME — the house rule, and D4 wants the
# isolation anyway.
SCRATCH_ROOT="${SELFDEV_SCRATCH:-${TMPDIR:-/tmp}/core-agent-selfdev}"
# Resolved after the preflight, not here: `--help` in a checkout with no
# `origin` must not die on a raw git error.
REMOTE="${SELFDEV_REMOTE:-}"
BASE_REF="${SELFDEV_BASE:-main}"
# Wallclock ceiling for the agent process itself. The recipe's cost
# ceilings bound spend; this bounds time. Both are needed: a run can be
# cheap and stuck. The default is set per tier below, once --tier is read.
TIMEOUT_SECS="${SELFDEV_TIMEOUT:-}"

usage() {
  cat <<'EOF'
Usage: dev/uat/self-dev/run.sh [options]

  --tier t0            which rung of the #1116 ladder to run (default: t0)
  --provider NAME      parent provider override (gemini|vertex|anthropic|
                       anthropic-vertex|echo|scripted). Default: leave the
                       recipe's own, which is first-party anthropic.
  --dry-run            do everything that costs nothing: clone, boot the
                       recipe, run every assertion that does not need a
                       model, a push or a PR. Proves the rig before you
                       spend money on it.
  --keep               do not delete the scratch clone on success
  -h, --help           this

Environment: SELFDEV_TIER, SELFDEV_PROVIDER, SELFDEV_SCRATCH,
SELFDEV_REMOTE, SELFDEV_BASE, SELFDEV_TIMEOUT.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tier) [[ $# -ge 2 ]] || { echo "--tier needs a value" >&2; exit 2; }; TIER="$2"; shift 2 ;;
    --provider) [[ $# -ge 2 ]] || { echo "--provider needs a value" >&2; exit 2; }; PROVIDER="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    --keep) KEEP=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

# TASK_ISSUE is the issue the task resolves. From T1 up every task names
# one, and A14 checks that the CHANGELOG bullet cites it: T0 named only
# the epic, so its bullet cited #1116, the one number it had.
#
# The wallclock default is per tier too. T0 finished in under 4 minutes.
# T1 live run 1 spent 45 minutes and $10 orienting before writing code,
# about $13 an hour, so an hour-long default would kill a T1 run long
# before its $50 cost ceiling could, and the cost ceiling would never be
# the bound. Four hours leaves room for the ceiling to be the one that trips.
case "${TIER}" in
  t0) TASK_FILE="${SELF_DIR}/tasks/t0-docs.md";   TASK_ISSUE="";     TIER_TIMEOUT=3600 ;;
  t1) TASK_FILE="${SELF_DIR}/tasks/t1-bugfix.md"; TASK_ISSUE="1002"; TIER_TIMEOUT=14400 ;;
  *)  echo "tier ${TIER} has no task file yet" >&2; exit 2 ;;
esac
TIMEOUT_SECS="${TIMEOUT_SECS:-${TIER_TIMEOUT}}"
[[ -f "${TASK_FILE}" ]] || { echo "missing task file: ${TASK_FILE}" >&2; exit 2; }

# ── Scorecard ────────────────────────────────────────────────────────

PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0
declare -a RESULTS=()

ok()    { PASS_COUNT=$((PASS_COUNT+1)); RESULTS+=("PASS  $1"); printf '  \033[32mPASS\033[0m  %s\n' "$1"; }
bad()   { FAIL_COUNT=$((FAIL_COUNT+1)); RESULTS+=("FAIL  $1 — $2"); printf '  \033[31mFAIL\033[0m  %s\n        %s\n' "$1" "$2"; }
skip()  { SKIP_COUNT=$((SKIP_COUNT+1)); RESULTS+=("SKIP  $1 — $2"); printf '  \033[33mSKIP\033[0m  %s (%s)\n' "$1" "$2"; }
note()  { printf '        %s\n' "$1"; }
head2() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# assert_contains FILE PATTERN LABEL FAILMSG — grep -qE, reported.
assert_contains() {
  local file="$1" pat="$2" label="$3" failmsg="$4"
  if [[ -f "${file}" ]] && grep -qE -- "${pat}" "${file}"; then
    ok "${label}"
  else
    bad "${label}" "${failmsg}"
  fi
}

# ── Preflight ────────────────────────────────────────────────────────

head2 "Preflight"

command -v git >/dev/null || { echo "git not found" >&2; exit 2; }
command -v gh  >/dev/null || { echo "gh not found" >&2; exit 2; }
# python3 is on the critical path for every PR assertion, and the
# checksum tool is spelled differently on Linux and macOS. Both fail late
# and confusingly if they are missing, so fail early and clearly instead.
command -v python3 >/dev/null || { echo "python3 not found" >&2; exit 2; }
if command -v sha256sum >/dev/null; then SUM=sha256sum
elif command -v shasum  >/dev/null; then SUM=shasum
else echo "neither sha256sum nor shasum found" >&2; exit 2
fi

if [[ -z "${REMOTE}" ]]; then
  REMOTE="$(git -C "${REPO_ROOT}" remote get-url origin 2>/dev/null || true)"
  [[ -n "${REMOTE}" ]] || { echo "no origin remote; set SELFDEV_REMOTE" >&2; exit 2; }
fi

RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$"
RUN_DIR="${SCRATCH_ROOT}/${RUN_ID}"
CLONE="${RUN_DIR}/repo"
CLONE_HEAD=""
PR_URL=""
LOG="${RUN_DIR}/agent.log"
mkdir -p "${RUN_DIR}"

# The scorecard is written from an EXIT trap, not from the bottom of the
# script. Under `set -e` every command substitution is an abort point, so
# a malformed pr.json during the thirty-minute CI wait used to kill the
# run AFTER the money was spent and the PR was open, leaving no record at
# all. The evidence has to survive the failure it is evidence of.
RECORD="${RUN_DIR}/scorecard.txt"
write_scorecard() {
  local rc=$?
  trap - EXIT
  head2 "Scorecard — ${TIER}, run ${RUN_ID}"
  printf '  %d passed, %d failed, %d skipped\n' "${PASS_COUNT}" "${FAIL_COUNT}" "${SKIP_COUNT}"
  if [[ ${rc} -ne 0 && ${FAIL_COUNT} -eq 0 ]]; then
    printf '  \033[31mABORTED\033[0m — the script exited %d before finishing its assertions\n' "${rc}"
  fi
  [[ ${DRY_RUN} -eq 1 ]] && printf '  (dry run — the graded half needs a model, a push and a PR)\n'
  [[ -n "${PR_URL:-}" ]] && printf '  PR: %s\n' "${PR_URL}"
  printf '  log: %s\n' "${LOG}"
  {
    printf 'self-dev UAT — tier=%s run=%s date=%s\n' "${TIER}" "${RUN_ID}" "$(date -u +%FT%TZ)"
    printf 'base=%s@%s provider=%s dry_run=%s\n' \
      "${BASE_REF}" "${CLONE_HEAD:0:8}" "${PROVIDER:-<recipe default>}" "${DRY_RUN}"
    [[ -n "${PR_URL:-}" ]] && printf 'pr=%s\n' "${PR_URL}"
    [[ ${rc} -ne 0 && ${FAIL_COUNT} -eq 0 ]] && printf 'ABORTED: exit %d before the assertions finished\n' "${rc}"
    printf '\n'
    [[ ${#RESULTS[@]} -gt 0 ]] && printf '%s\n' "${RESULTS[@]}"
    printf '\n%d passed, %d failed, %d skipped\n' "${PASS_COUNT}" "${FAIL_COUNT}" "${SKIP_COUNT}"
  } >"${RECORD}"
  # A failed or aborted run always keeps its scratch, and so does any run
  # that drove a model: deleting the evidence of the thing you are trying
  # to diagnose is the one cleanup nobody wants. Only a clean dry run —
  # which produced nothing but a boot log — cleans up after itself, and it
  # says so rather than printing a path about to stop existing.
  if [[ ${rc} -eq 0 && ${FAIL_COUNT} -eq 0 && ${KEEP} -eq 0 && ${DRY_RUN} -eq 1 ]]; then
    rm -rf "${RUN_DIR}"
    printf '  scratch removed (clean dry run; --keep to retain it)\n\n'
  else
    printf '  scorecard: %s\n\n' "${RECORD}"
  fi
  exit $(( rc != 0 ? rc : (FAIL_COUNT > 0 ? 1 : 0) ))
}
trap write_scorecard EXIT

note "run id     ${RUN_ID}"
note "scratch    ${RUN_DIR}"
note "tier       ${TIER}"
note "task       ${TASK_FILE}"

# The binary under test is built from the CURRENT checkout, not from the
# clone and not from $PATH. The point of the exercise is to grade the
# code in front of you; a stale `core-agent` on PATH would grade
# something else and say nothing about it.
BIN="${RUN_DIR}/core-agent"
note "building core-agent from ${REPO_ROOT}"
( cd "${REPO_ROOT}" && go build -o "${BIN}" ./cmd/core-agent )
ok "binary built from the checkout under test"

# The real checkout's state, captured BEFORE anything runs. A9 compares
# against this.
#
# `git status --porcelain` alone is NOT enough, and the gap is not
# academic: it says nothing about ignored paths, and P3 deliberately
# gitignored the entire runtime surface of a self-dev run —
# `/.agents/{sessions,logs,plans}/`, `env.yaml`, `env.json`, `*.db`. So
# the single most likely way this run touches the real checkout is
# config.Find's walk-up reaching the real `.agents/` and `record_plan`
# writing a plan into it, and that is exactly what a porcelain-only
# fingerprint cannot see. A witness blind to the failure it exists to
# catch is worse than none, because its PASS is read as evidence.
#
# So: ignored entries are listed too, `.agents/` is hashed by content,
# and the refs and config under `.git/` are hashed because a stray
# `git branch`/`git remote add` moves neither HEAD nor status.
real_state() {
  git -C "${REPO_ROOT}" rev-parse HEAD
  git -C "${REPO_ROOT}" status --porcelain --ignored=matching
  # Content, not just presence: a rewritten plan file has the same name.
  if [[ -d "${REPO_ROOT}/.agents" ]]; then
    find "${REPO_ROOT}/.agents" -type f -print0 | LC_ALL=C sort -z |
      xargs -0 -r "${SUM}" 2>/dev/null
  fi
  find "${REPO_ROOT}/.git/refs" -type f -print0 2>/dev/null | LC_ALL=C sort -z |
    xargs -0 -r "${SUM}" 2>/dev/null
  "${SUM}" "${REPO_ROOT}/.git/packed-refs" "${REPO_ROOT}/.git/config" 2>/dev/null || true
}
REAL_BEFORE="$(real_state | "${SUM}" | awk '{print $1}')"
note "real checkout fingerprint ${REAL_BEFORE}"

if [[ ${DRY_RUN} -eq 0 ]]; then
  if ! gh auth status >/dev/null 2>&1; then
    echo "gh is not authenticated; the agent cannot open a PR" >&2
    exit 2
  fi
  ok "gh authenticated"
else
  skip "gh authenticated" "dry run"
fi

# ── Clone ────────────────────────────────────────────────────────────

head2 "Clone (D4: /tmp, never the real checkout)"

git clone --quiet --branch "${BASE_REF}" "${REMOTE}" "${CLONE}"
CLONE_HEAD="$(git -C "${CLONE}" rev-parse HEAD)"
FORK="${CLONE_HEAD}"
# The path the agent's own loader will record, symlinks resolved. A2
# compares against this rather than ${CLONE}.
CLONE_REAL="$(cd "${CLONE}" && pwd -P)"
note "cloned ${REMOTE} @ ${BASE_REF} (${CLONE_HEAD:0:8})"

# The agent authors commits as the operator, and nothing in the commit
# marks it as agent work (A8). Set the identity explicitly rather than
# inheriting, so a machine with no global git identity does not fail at
# commit time three hundred steps in.
git -C "${CLONE}" config user.name  "$(git -C "${REPO_ROOT}" config user.name)"
git -C "${CLONE}" config user.email "$(git -C "${REPO_ROOT}" config user.email)"

CFG="${CLONE}/.agents/config.json"
if [[ ! -f "${CFG}" ]]; then
  bad "recipe present in the clone" "${BASE_REF} has no .agents/config.json — P3 may not be merged into ${BASE_REF} yet"
  exit 1
fi
ok "recipe present in the clone"

# ── Boot ─────────────────────────────────────────────────────────────

head2 "Boot"

# The `-c "${CFG}"` pin is written literally on each invocation line below
# rather than carried in this array. Both forms pin identically at run
# time, but dev/harness-config-check reads the line as text and cannot see
# through an array expansion, so a pin hidden in AGENT_ARGS reads as an
# unpinned site. This is the one check that stands between a self-dev run
# and config.Find's walk-up reaching the real checkout's recipe; keeping
# the pin where the scanner can read it is worth the duplication.
AGENT_ARGS=()
# --yolo, deliberately and only here. The COMMITTED recipe is `ask`,
# because it is reachable by config.Find's walk-up from the real checkout
# and must not be the thing that disarms a developer's gate. D4 scopes
# yolo to the throwaway clone, which is exactly where we are. plan_mode
# stays `required` — that gate is not a permission and A5 grades it.
AGENT_ARGS+=( --yolo )
[[ -n "${PROVIDER}" ]] && AGENT_ARGS+=( --provider="${PROVIDER}" )
# The per-turn ceiling is raised to the session ceiling, also only here.
# A `-p` run is ONE turn, so the recipe's max_turn_cost_usd is not a turn
# bound in this rig; it is a second, smaller session cap. Live T1 run 1
# tripped it at 45 minutes, before a line of code, while the $50 session
# cap it sits under was unreachable. The committed value stays for the
# developer at a REPL, where turns are turns. auto_continue can't do this
# job: it resumes a restart-interrupted daemon turn and never starts a
# new one, so it has nothing to drive in a one-shot run. What bounds the
# run is that raised ceiling (at equal values the per-turn check trips
# first, so a $50 stop reports as a per-turn trip), the enforcing
# watchdog and TIMEOUT_SECS.
# An unset or 0 session ceiling (0 means none) leaves the per-turn one in
# place, so the run always has some cost cap.
SESSION_CAP="$(python3 - "${CFG}" <<'PY' 2>/dev/null || true
import json, math, sys
v = (json.load(open(sys.argv[1])).get("agent") or {}).get("max_session_cost_usd")
if isinstance(v, (int, float)) and not isinstance(v, bool) and math.isfinite(v) and v > 0:
    print(v)
PY
)"
if [[ -n "${SESSION_CAP}" ]]; then
  AGENT_ARGS+=( --max-turn-cost-usd="${SESSION_CAP}" )
else
  note "recipe has no positive agent.max_session_cost_usd; its per-turn ceiling stays as the run's only cost cap"
fi

if [[ ${DRY_RUN} -eq 1 ]]; then
  note "dry run: booting on --provider=echo with a trivial prompt"
  BOOT_ARGS=( --yolo --provider=echo -p "reply with exactly: BOOT" )
  set +e
  ( cd "${CLONE}" && "${BIN}" -c "${CFG}" "${BOOT_ARGS[@]}" ) >"${LOG}" 2>&1
  BOOT_RC=$?
  set -e
  if [[ ${BOOT_RC} -eq 0 ]]; then
    ok "recipe boots (exit 0)"
  else
    bad "recipe boots (exit 0)" "exit ${BOOT_RC}; see ${LOG}"
  fi
else
  PROMPT="$(cat "${TASK_FILE}")"
  PROMPT="${PROMPT//<RUN_ID>/${RUN_ID}}"
  AGENT_ARGS+=( -p "${PROMPT}" )
  note "running the agent (timeout ${TIMEOUT_SECS}s) — log: ${LOG}"
  set +e
  ( cd "${CLONE}" && timeout "${TIMEOUT_SECS}" "${BIN}" -c "${CFG}" "${AGENT_ARGS[@]}" ) >"${LOG}" 2>&1
  AGENT_RC=$?
  set -e
  if [[ ${AGENT_RC} -eq 124 ]]; then
    bad "agent finished within ${TIMEOUT_SECS}s" "wallclock timeout; see ${LOG}"
  elif [[ ${AGENT_RC} -ne 0 ]]; then
    bad "agent exited 0" "exit ${AGENT_RC}; see ${LOG}"
  else
    ok "agent exited 0"
  fi
fi

# ── Assertions on the run ────────────────────────────────────────────
#
# A1–A4 grade the recipe actually taking effect. They are cheap and they
# run in dry mode too, which is what makes --dry-run worth having: the
# expensive failure is a live run that spends real money to discover the
# config never loaded.

head2 "A1–A4  the recipe took effect"

if grep -qF "config: source=${CFG}" "${LOG}"; then
  ok "A1 config came from the clone, not the real checkout"
else
  bad "A1 config came from the clone, not the real checkout" \
    "the startup summary does not name ${CFG}; the walk-up may have found another .agents/ first"
fi

# Assert the two paths, NOT the count. A count is wrong in both
# directions: an operator with `~/.agents/AGENTS.md` loads three and fails
# a perfectly good run, and "2" is equally satisfied by the clone's
# `.agents/AGENTS.md` plus a home one with the repo's own root AGENTS.md
# missing — which is precisely the "a run with one of them is not the
# recipe" case this is supposed to catch.
#
# CLONE_REAL, not CLONE: the loader records `filepath.EvalSymlinks`'d
# paths (pkg/instruction/load.go, canonicalPath), and on macOS TMPDIR is
# a symlink into /private/var, so the unresolved path never matches.
# grep -F for the same reason A1 uses it: a scratch path can contain
# regex metacharacters (macOS really does hand out `/var/folders/xy/a+b/`).
if grep -qF "${CLONE_REAL}/.agents/AGENTS.md" "${LOG}" &&
   grep -qF "${CLONE_REAL}/AGENTS.md" "${LOG}"; then
  ok "A2 both AGENTS.md reached the prompt"
else
  bad "A2 both AGENTS.md reached the prompt" \
    "expected the self-dev persona AND the repo's own AGENTS.md, both from the clone; a run with one of them is not the recipe"
fi

# By name, not by count: pinning "5 loaded" means the next PR that adds a
# sixth skill breaks the rig rather than the recipe, and a count says
# nothing about WHICH five loaded.
MISSING_SKILLS=""
for sk in presubmit-sweep adversarial-review-gate prefix-failure-verification \
          changelog-bullet stacked-pr-order; do
  grep -qF -- "${sk}" "${LOG}" || MISSING_SKILLS="${MISSING_SKILLS} ${sk}"
done
if [[ -z "${MISSING_SKILLS}" ]]; then
  ok "A3 the five skills loaded"
else
  bad "A3 the five skills loaded" "the rituals are not in the prompt; missing:${MISSING_SKILLS}"
fi

assert_contains "${LOG}" "subagents: 1 configured — reviewer" \
  "A4 the reviewer subagent is configured" \
  "no reviewer means no adversarial review gate"

# A model block on the reviewer boots fine on first-party Anthropic and
# exits 2 everywhere else, which is how it survived P3. Assert the
# absence directly rather than trusting that this run's provider would
# have noticed.
# Select the reviewer BY NAME and answer structurally. The first draft
# indexed `subagents[0]` and grepped the JSON text for `"model"`, which
# fails open three separate ways — a second subagent added ahead of the
# reviewer, a config shape python did not expect, or python erroring at
# all, each producing empty output and therefore a PASS. An assertion
# guarding a bug that already shipped once silently must fail CLOSED.
A4B="$(python3 - "${CFG}" <<'PY' 2>/dev/null || echo error
import json,sys
try:
    specs = json.load(open(sys.argv[1]))["subagents"]
    hits = [s for s in specs if s.get("name") == "reviewer"]
    if len(hits) != 1: print("shape"); raise SystemExit
    print("pinned" if "model" in hits[0] else "clean")
except SystemExit: raise
except Exception: print("error")
PY
)"
case "${A4B}" in
  clean)  ok "A4b the reviewer declares no model of its own" ;;
  pinned) bad "A4b the reviewer declares no model of its own" \
            "a subagent model block survives --provider and makes the recipe unbootable off first-party Anthropic (D3, P4 correction)" ;;
  shape)  bad "A4b the reviewer declares no model of its own" \
            "no single subagent named 'reviewer' in ${CFG}" ;;
  *)      bad "A4b the reviewer declares no model of its own" \
            "could not read ${CFG}; the check failed closed rather than passing on silence" ;;
esac

head2 "A5  the plan gate did its job"

PLAN_DIR="${CLONE}/.agents/plans"
if [[ ${DRY_RUN} -eq 1 ]]; then
  skip "A5 a plan artifact exists (plan_mode: required honoured)" "dry run makes no changes"
elif compgen -G "${PLAN_DIR}/*.md" >/dev/null; then
  ok "A5 a plan artifact exists (plan_mode: required honoured)"
  note "plan: $(ls -1 "${PLAN_DIR}"/*.md | head -1)"
else
  bad "A5 a plan artifact exists (plan_mode: required honoured)" \
    "no plan artifact in ${PLAN_DIR}; plan_mode=required should have denied every mutating call until record_plan ran"
fi

# ── Assertions on the artifacts ──────────────────────────────────────

head2 "A6–A8  what the agent actually produced"

if [[ ${DRY_RUN} -eq 1 ]]; then
  skip "A6 a branch was committed and pushed" "dry run"
  skip "A7 the change stays in the task's scope" "dry run"
  skip "A8 no commit carries agent attribution" "dry run"
  skip "A8b every commit is DCO signed off" "dry run"
  skip "A10 no agent attribution in the PR title or body" "dry run"
  skip "A11 a pull request is open" "dry run"
  skip "A12 CI is green on the pull request" "dry run"
else
  BRANCH="$(git -C "${CLONE}" rev-parse --abbrev-ref HEAD)"
  note "branch ${BRANCH}"

  # Grade from the commit the branch grew from, not the one the rig
  # cloned. An agent that rebases onto a newer main mid-run would
  # otherwise be charged with every PR that landed meanwhile: their
  # files fail A7, their commits A8, their tests A13, and a rebase with
  # nothing on top would pass A6. The base is
  # fetched from the rig's own REMOTE into a rig-owned ref, not read off
  # refs/remotes/origin: the agent controls both that ref and the
  # clone's idea of where origin is.
  if git -C "${CLONE}" -c core.hooksPath=/dev/null fetch --quiet "${REMOTE}" "+refs/heads/${BASE_REF}:refs/selfdev/base" 2>/dev/null &&
     mb="$(git -C "${CLONE}" merge-base HEAD refs/selfdev/base 2>/dev/null)" &&
     git -C "${CLONE}" merge-base --is-ancestor "${CLONE_HEAD}" "${mb}"; then
    # Only ever forward from the clone: a base that went backwards
    # (a force-pushed main) would widen the graded range, not narrow it.
    FORK="${mb}"
  else
    note "could not resolve ${BASE_REF}'s fork point from ${REMOTE}; grading from the cloned ${CLONE_HEAD:0:8}"
  fi
  [[ "${FORK}" == "${CLONE_HEAD}" ]] || note "branch forked from ${FORK:0:8}, not the cloned ${CLONE_HEAD:0:8}; grading from the fork"

  if [[ "${BRANCH}" == "HEAD" ]]; then
    # `rev-parse --abbrev-ref` prints the literal "HEAD" when detached, and
    # every clone has a refs/remotes/origin/HEAD — so without this arm a
    # detached commit that was never pushed passed the push check below.
    bad "A6 a branch was committed and pushed" "detached HEAD; the agent committed without branching"
  elif [[ "${BRANCH}" == "${BASE_REF}" ]]; then
    bad "A6 a branch was committed and pushed" "still on ${BASE_REF}; the agent never branched"
  elif [[ "$(git -C "${CLONE}" rev-parse HEAD)" == "${FORK}" ]]; then
    bad "A6 a branch was committed and pushed" "HEAD never moved; nothing was committed"
  # Ask the REMOTE, not the remote-tracking ref: refs/remotes lives inside
  # the directory the agent controls, so it is not evidence of a push.
  elif ! git -C "${CLONE}" ls-remote --exit-code origin "refs/heads/${BRANCH}" >/dev/null 2>&1; then
    bad "A6 a branch was committed and pushed" "commit exists but ${BRANCH} is not on the remote"
  else
    ok "A6 a branch was committed and pushed"
  fi


  # Committed AND uncommitted: junk left in the worktree is part of what
  # the agent did, and a range diff alone never sees it.
  #
  # `--no-renames` and `-z` are both load-bearing, and the first draft had
  # neither. A rename reports only its NEW path on both halves, so
  # `git mv internal/secret.go docs/secret.go` deleted a Go file and graded
  # as docs-only — the allowlist below is worth nothing if a one-word git
  # command launders a path into it. And porcelain C-quotes any path with
  # a space, which the old `awk '{print $NF}'` then sliced at the space:
  # `?? "docs/my notes.md"` came out as `notes.md"` (a false FAIL naming a
  # file that does not exist), and `?? "internal evil docs/"` came out as
  # `docs/"`, which the allowlist ACCEPTS. `-z` emits raw paths and splits
  # rename entries into two fields, so both go away at once.
  CHANGED="$(
    git -C "${CLONE}" diff --name-only --no-renames -z "${FORK}"..HEAD |
      tr '\0' '\n'
    git -C "${CLONE}" status --porcelain -z -uall | python3 -c '
import sys
# porcelain -z: "XY PATH\0" per entry, and "XY NEW\0ORIG\0" for R/C.
fields = sys.stdin.buffer.read().split(b"\0")
i = 0
while i < len(fields):
    entry = fields[i]
    i += 1
    if not entry:
        continue
    status, path = entry[:2], entry[3:]
    sys.stdout.buffer.write(path + b"\n")
    if status[:1] in (b"R", b"C") or status[1:2] in (b"R", b"C"):
        if i < len(fields):
            sys.stdout.buffer.write(fields[i] + b"\n")
            i += 1
'
  )"
  CHANGED="$(printf '%s\n' "${CHANGED}" | grep -v '^$' | sort -u || true)"
  note "changed: $(printf '%s' "${CHANGED}" | tr '\n' ' ')"
  # An ALLOWLIST. The first draft denied three patterns and permitted
  # everything else, so an agent that edited `.github/workflows/`, `go.mod`
  # or the very recipe it was running under graded as "docs-only" — and
  # the workflow edit is exactly the one that then makes CI report a
  # conclusion A12 has to be careful about. The task names one directory;
  # grade against that.
  #
  # `CHANGELOG.md` is in the allowlist because the recipe asks for it.
  # AGENTS.md's "Which doc changes are user-visible" rule gives an edit to
  # the published site a bullet, and T0's target is a site page. When the
  # first live run (20260922T134553Z-3675866) happened, AGENTS.md said
  # *every* doc change gets a bullet, while practice almost never did it.
  # The agent followed the text and wrote the bullet. This assertion had
  # encoded the practice, and it failed the run. So the FAIL was a defect
  # in the scorecard, not the run. An assertion may be harsher than the
  # task, never in conflict with it. Still an allowlist: that one path,
  # anchored, and a `docs/` change is REQUIRED rather than merely
  # permitted, so a run that only filed a bullet cannot pass.
  #
  # The allowlist is per-tier and the default arm is a failure, not a pass.
  # A7 was written for T0 and was wrong for every rung above it (T1's whole
  # point is a Go change), so a tier reaching here without its own entry is
  # a rig bug, and the one thing it must not do is grade.
  #
  # Each tier also REQUIRES some paths, as triples: a path the change must
  # include, a pattern that disqualifies a match ('' for none), and the
  # message to fail on. T0 must touch docs/. T1 must change both a Go test
  # and Go production code, so a run that only added a test, or only
  # patched the code, cannot pass.
  #
  # Note what this does NOT check: T0's task also forbids scripts and
  # tests, and a `.go` file under `docs/` would pass. Directory scope is
  # the property being asserted; the extension clause is not.
  A7="A7 the change stays in the task's scope"
  REQUIRE=()
  case "${TIER}" in
    t0)
      ALLOW_RE='^(docs/|CHANGELOG\.md$)'
      ALLOW_DESC='docs/ and CHANGELOG.md'
      REQUIRE=( '^docs/' '' 'nothing under docs/ changed' )
      ;;
    t1)
      # docs/ because the task tells the agent to update the site page
      # that documents spawn_agent's result if the fix changes it.
      ALLOW_RE='^(pkg/agent/|docs/|CHANGELOG\.md$)'
      ALLOW_DESC='pkg/agent/, docs/ and CHANGELOG.md'
      REQUIRE=( '^pkg/agent/.*_test\.go$' '' 'no Go test under pkg/agent/ changed'
                '^pkg/agent/.*\.go$' '_test\.go$' 'no Go production file under pkg/agent/ changed' )
      ;;
    *)
      bad "${A7}" \
        "no allowlist defined for tier ${TIER} — A7 must be re-specified per tier"
      ALLOW_RE=''
      ;;
  esac
  if [[ -n "${ALLOW_RE}" ]]; then
    OFFENDING="$(printf '%s\n' "${CHANGED}" | grep -vE "${ALLOW_RE}" || true)"
    MISSING=""
    for ((i = 0; i < ${#REQUIRE[@]}; i += 3)); do
      HITS="$(printf '%s\n' "${CHANGED}" | grep -E "${REQUIRE[i]}" || true)"
      if [[ -n "${HITS}" && -n "${REQUIRE[i+1]}" ]]; then
        HITS="$(printf '%s\n' "${HITS}" | grep -vE "${REQUIRE[i+1]}" || true)"
      fi
      if [[ -z "${HITS}" ]]; then
        MISSING="${REQUIRE[i+2]}"
        break
      fi
    done
    if [[ -z "${CHANGED}" ]]; then
      bad "${A7}" "no files changed"
    elif [[ -n "${OFFENDING}" ]]; then
      bad "${A7}" \
        "${TIER} permits ${ALLOW_DESC}; touched: $(printf '%s' "${OFFENDING}" | tr '\n' ' ')"
    elif [[ -n "${MISSING}" ]]; then
      bad "${A7}" "${MISSING}"
    else
      ok "${A7}"
    fi
  fi

  # EVERY commit in the range, not just the tip. Three commits with the
  # sign-off only on the last one passed A8b while the DCO check on
  # GitHub failed the PR — a scorecard contradicting CI rather than
  # diagnosing it.
  #
  # A8 runs the same scanner CI's required `agent attribution` check
  # runs, so the two cannot disagree. It used to require a
  # `Self-Development-Run:` trailer on every commit; that trailer is
  # agent attribution under another name, and the scanner now bans it.
  # Which PR a run opened is recorded here, in the run's own scorecard,
  # not in the repository's history.
  N_COMMITS="$(git -C "${CLONE}" rev-list --count "${FORK}"..HEAD)"
  N_DCO="$(git -C "${CLONE}" log "${FORK}"..HEAD \
    --format="%(trailers:key=Signed-off-by,valueonly)" | grep -c . || true)"
  ATTR_OUT="${RUN_DIR}/attribution-commits.txt"
  if [[ "${N_COMMITS}" == "0" ]]; then
    bad "A8 no commit carries agent attribution" "no commits to check"
  elif (cd "${CLONE}" && "${REPO_ROOT}/dev/tools/verify-no-agent-attribution" \
          --range "${FORK}..HEAD") >"${ATTR_OUT}" 2>&1; then
    ok "A8 no commit carries agent attribution"
  else
    bad "A8 no commit carries agent attribution" \
      "$(grep -m1 '^commit ' "${ATTR_OUT}" || echo "see ${ATTR_OUT}")"
  fi

  if [[ "${N_DCO}" == "${N_COMMITS}" && "${N_COMMITS}" != "0" ]]; then
    ok "A8b every commit is DCO signed off"
  else
    bad "A8b every commit is DCO signed off" "${N_DCO}/${N_COMMITS} signed off; the DCO check will fail"
  fi
fi

# ── The regression test ──────────────────────────────────────────────
#
# T1's hard part, graded rather than claimed (#1116 P5). The PR body will
# say the new test failed on the pre-fix code; this checks it against
# FORK, the commit the branch grew from.
#
# A13's first witness is the rig itself. It builds the pre-fix tree from
# the branch tip with only the production .go files put back to FORK.
# Test files, testdata and embedded fixtures all stay at the tip, so a new
# test that reads a new fixture does not fail pre-fix merely because the
# fixture is missing. Then it runs just the new Test functions. A FAIL
# naming one is the evidence. A clean pass is the defect this rung exists
# to catch: a regression test that passes on the buggy code is
# documentation, not a gate.
#
# That witness is blind to the common case. A fix that adds a field,
# const or signature leaves the new test uncompilable against pre-fix
# sources, and a build failure says nothing about an assertion, which is
# why AGENTS.md prescribes patching the behaviour back instead of
# reverting. There the rig falls back to the agent's own saved run of that
# method (.agents/logs/prefix-failure.txt, gitignored). The PASS line
# says which witness it rests on, because a re-run by the rig and a file
# the agent wrote are not equally strong.
#
# A15 is why the weaker witness is acceptable: an oracle the rig owns,
# written against the bug rather than against the agent's test, run at
# FORK and at the tip. It drives a real spawn and reads what the parent
# is handed, so it doesn't care what the fix calls its new field or
# which hop it lands in, only that the banked result and the run error
# both arrive.
#
# Every git and go step here is guarded rather than left to `set -e`, so
# a failure grades the assertion instead of aborting before the
# scorecard. Every worktree gets a fresh directory under RUN_DIR, so
# nothing here deletes a path it computed.

# rig_worktree DIR REV: a detached worktree of the clone with hooks off.
# The agent controls the clone's .git/hooks, so every rig git command
# that can fire one (worktree add, checkout, rm, fetch) sets
# core.hooksPath=/dev/null. go test runs the agent's code regardless;
# this keeps the rig's own bookkeeping out of the agent's hands.
rig_worktree() {
  git -C "${CLONE}" -c core.hooksPath=/dev/null worktree add --quiet --detach "$1" "$2" >/dev/null 2>&1
}
drop_worktree() {
  git -C "${CLONE}" worktree remove --force "$1" >/dev/null 2>&1 || true
  git -C "${CLONE}" worktree prune >/dev/null 2>&1 || true
}
# test_names FILE STATUS: the Test functions go test reported with that
# status (PASS or FAIL), one per line. Plain text only; -v is fine.
test_names() {
  sed -nE "s/^[[:space:]]*--- $2: (Test[A-Za-z0-9_]+)( .*)?\$/\1/p" "$1" 2>/dev/null | sort -u
}
# among LIST NAMES: the words of LIST that appear as lines in NAMES.
among() {
  local t out=""
  for t in $1; do
    if printf '%s\n' "$2" | grep -qxF "${t}"; then out="${out} ${t}"; fi
  done
  printf '%s' "${out# }"
}
has_build_failure() {
  grep -qE '\[build failed\]|\[setup failed\]' "$1" 2>/dev/null
}

if [[ -n "${TASK_ISSUE}" ]]; then
  head2 "A13–A15  the regression test and the issue"
fi

if [[ -z "${TASK_ISSUE}" ]]; then
  : # T0 has no test and names no issue; its scorecard is unchanged.
elif [[ ${DRY_RUN} -eq 1 ]]; then
  skip "A13 the new test fails on the pre-fix code" "dry run"
  skip "A13b the new test passes on the branch" "dry run"
  skip "A13c no PREFIX BEHAVIOUR marker survived" "dry run"
  skip "A14 the CHANGELOG bullet cites #${TASK_ISSUE}" "dry run"
  skip "A15 the rig's own #${TASK_ISSUE} oracle fails before and passes after" "dry run"
else
  CLONE_TIP="$(git -C "${CLONE}" rev-parse HEAD)"
  RIG_TREES="${RUN_DIR}/rig-trees.$$"
  mkdir -p "${RIG_TREES}"

  # Test functions present at the tip and absent at FORK: the ones the
  # agent added. The task asks for a new function for this reason: a case
  # appended to an existing table cannot be told apart from it.
  NEW_TESTS=""
  PKGS=""
  while IFS= read -r f; do
    [[ -n "${f}" ]] || continue
    added="$(comm -23 \
      <(git -C "${CLONE}" show "${CLONE_TIP}:${f}" 2>/dev/null |
          sed -nE 's/^func (Test[A-Za-z0-9_]+)\(.*/\1/p' | sort -u) \
      <(git -C "${CLONE}" show "${FORK}:${f}" 2>/dev/null |
          sed -nE 's/^func (Test[A-Za-z0-9_]+)\(.*/\1/p' | sort -u) || true)"
    if [[ -n "${added}" ]]; then
      NEW_TESTS="${NEW_TESTS} $(printf '%s' "${added}" | tr '\n' ' ')"
      PKGS="${PKGS} ./$(dirname "${f}")"
    fi
  done < <(git -C "${CLONE}" diff --name-only --no-renames --diff-filter=AM \
             "${FORK}..${CLONE_TIP}" -- '*_test.go' 2>/dev/null || true)
  # shellcheck disable=SC2086 # word lists by construction
  NEW_TESTS="$(printf '%s\n' ${NEW_TESTS} | sort -u | tr '\n' ' ')"
  # shellcheck disable=SC2086
  PKGS="$(printf '%s\n' ${PKGS} | sort -u | tr '\n' ' ')"
  NEW_TESTS="${NEW_TESTS% }"
  PKGS="${PKGS% }"

  A13="A13 the new test fails on the pre-fix code"
  A13B="A13b the new test passes on the branch"
  if [[ -z "${NEW_TESTS}" ]]; then
    bad "${A13}" "no new Test function in any committed _test.go; there is nothing to prove"
    skip "${A13B}" "no new test"
  else
    RUN_RE="^($(printf '%s' "${NEW_TESTS}" | tr ' ' '|'))\$"
    note "new tests: ${NEW_TESTS} (in ${PKGS})"

    # The pre-fix tree: the tip, with every production .go file the
    # branch touched put back to its FORK content, or removed from the
    # index and the tree if the branch added it.
    PRE_TREE="${RIG_TREES}/prefix"
    PREFIX_OUT="${RUN_DIR}/prefix-rig.txt"
    : >"${PREFIX_OUT}"
    PREFIX_RC=""
    if rig_worktree "${PRE_TREE}" "${CLONE_TIP}"; then
      REVERT_OK=1
      while IFS= read -r f; do
        [[ -n "${f}" ]] || continue
        if git -C "${CLONE}" cat-file -e "${FORK}:${f}" 2>/dev/null; then
          git -C "${PRE_TREE}" -c core.hooksPath=/dev/null checkout --quiet "${FORK}" -- "${f}" || REVERT_OK=0
        else
          git -C "${PRE_TREE}" -c core.hooksPath=/dev/null rm --quiet --force -- "${f}" || REVERT_OK=0
        fi
      done < <(git -C "${CLONE}" diff --name-only --no-renames "${FORK}..${CLONE_TIP}" \
                 -- '*.go' ':!*_test.go' ':!*/testdata/*' 2>/dev/null || true)
      if [[ ${REVERT_OK} -eq 1 ]]; then
        PREFIX_RC=0
        # shellcheck disable=SC2086
        (cd "${PRE_TREE}" && go test -count=1 -timeout 5m -v -run "${RUN_RE}" ${PKGS}) \
          >"${PREFIX_OUT}" 2>&1 || PREFIX_RC=$?
      fi
      drop_worktree "${PRE_TREE}"
    fi

    # Per-test outcome, so a new test that passes both ways is named
    # rather than hidden behind the one that failed.
    FAILED="$(among "${NEW_TESTS}" "$(test_names "${PREFIX_OUT}" FAIL)")"
    BOTH_WAYS="$(among "${NEW_TESTS}" "$(test_names "${PREFIX_OUT}" PASS)")"

    EVIDENCE="${CLONE}/.agents/logs/prefix-failure.txt"
    if [[ -z "${PREFIX_RC}" ]]; then
      bad "${A13}" "the rig could not build its pre-fix worktree from ${CLONE_TIP:0:8}"
    elif [[ ${PREFIX_RC} -eq 0 ]]; then
      bad "${A13}" "${NEW_TESTS} PASS on the unfixed code from ${FORK:0:8}; see ${PREFIX_OUT}"
    elif [[ -n "${FAILED}" ]]; then
      ok "${A13} (witness: the rig re-ran it on ${FORK:0:8}'s code; failed: ${FAILED})"
    elif has_build_failure "${PREFIX_OUT}"; then
      # The test needs the fix's new symbols. Fall back to the agent's run.
      EV_FAILED="$(among "${NEW_TESTS}" "$(test_names "${EVIDENCE}" FAIL)")"
      if [[ ! -f "${EVIDENCE}" ]]; then
        bad "${A13}" \
          "the new tests don't compile on ${FORK:0:8}'s code and there is no saved PREFIX BEHAVIOUR run at .agents/logs/prefix-failure.txt"
      elif has_build_failure "${EVIDENCE}"; then
        bad "${A13}" \
          "the saved pre-fix run is a build failure, which is not evidence about an assertion; see ${EVIDENCE}"
      elif [[ -n "${EV_FAILED}" ]]; then
        ok "${A13} (witness: the agent's saved PREFIX BEHAVIOUR run, not a rig re-run, because the test needs the fix's new symbols; failed: ${EV_FAILED})"
        BOTH_WAYS="$(among "${NEW_TESTS}" "$(test_names "${EVIDENCE}" PASS)")"
      else
        bad "${A13}" \
          "the saved pre-fix run has no plain-text --- FAIL naming any of ${NEW_TESTS}; see ${EVIDENCE}"
      fi
    else
      bad "${A13}" \
        "go test exited ${PREFIX_RC} on the pre-fix tree with no --- FAIL naming a new test; see ${PREFIX_OUT}"
    fi
    if [[ -n "${BOTH_WAYS}" ]]; then
      note "passed on the pre-fix code too (the PR should declare these): ${BOTH_WAYS}"
    fi

    # The same functions at the committed tip, not in the clone's working
    # tree: the PR is what gets merged. CI runs the whole suite (A12);
    # this pins the tests A13 graded, so a test that fails before AND
    # after cannot pass as a regression test. It wants a `--- PASS` line
    # for every new function, not just exit 0: go test also exits 0 for a
    # skipped test and for one a build tag hides ("no tests to run").
    POST_TREE="${RIG_TREES}/postfix"
    POST_OUT="${RUN_DIR}/postfix-rig.txt"
    if ! rig_worktree "${POST_TREE}" "${CLONE_TIP}"; then
      bad "${A13B}" "the rig could not check out ${CLONE_TIP:0:8}"
    else
      # shellcheck disable=SC2086
      POST_RC=0
      (cd "${POST_TREE}" && go test -count=1 -timeout 5m -v -run "${RUN_RE}" ${PKGS}) \
        >"${POST_OUT}" 2>&1 || POST_RC=$?
      POST_PASSED="$(test_names "${POST_OUT}" PASS)"
      NOT_PASSED=""
      for t in ${NEW_TESTS}; do
        if ! printf '%s\n' "${POST_PASSED}" | grep -qxF "${t}"; then NOT_PASSED="${NOT_PASSED} ${t}"; fi
      done
      NOT_PASSED="${NOT_PASSED# }"
      if [[ ${POST_RC} -ne 0 ]]; then
        bad "${A13B}" "go test exited ${POST_RC}; see ${POST_OUT}"
      elif [[ -n "${NOT_PASSED}" ]]; then
        bad "${A13B}" "no --- PASS for ${NOT_PASSED} (skipped, or never ran); see ${POST_OUT}"
      else
        ok "${A13B}"
      fi
      drop_worktree "${POST_TREE}"
    fi
  fi

  # Committed, tracked-but-uncommitted, or untracked. The skill's own last
  # step is this grep, and a marker left behind is the fix silently
  # patched back out.
  MARKERS="$(
    git -C "${CLONE}" grep -n 'PREFIX BEHAVIOUR' "${CLONE_TIP}" -- '*.go' 2>/dev/null || true
    git -C "${CLONE}" grep -n --untracked 'PREFIX BEHAVIOUR' -- '*.go' 2>/dev/null || true
  )"
  if [[ -z "${MARKERS}" ]]; then
    ok "A13c no PREFIX BEHAVIOUR marker survived"
  else
    bad "A13c no PREFIX BEHAVIOUR marker survived" "$(printf '%s\n' "${MARKERS}" | sed -n 1p)"
  fi

  # Added lines only, in the link form the changelog uses. A bare "#1002"
  # would also match "#10020".
  if git -C "${CLONE}" diff "${FORK}..${CLONE_TIP}" -- CHANGELOG.md 2>/dev/null |
       grep -E '^\+' | grep -qE "/issues/${TASK_ISSUE}\)"; then
    ok "A14 the CHANGELOG bullet cites #${TASK_ISSUE}"
  else
    bad "A14 the CHANGELOG bullet cites #${TASK_ISSUE}" \
      "no added CHANGELOG.md line links issues/${TASK_ISSUE}; the task names it so the bullet can cite it"
  fi

  # A15, the rig's own oracle. It must FAIL at FORK, which proves it still
  # detects the bug, and PASS at the tip. It spawns a real subagent whose
  # model calls return_result and then errors, and reads the spawn_agent
  # result the parent gets. The first version built the failed Handle by
  # hand and called completionResult on it, and live run 1 showed that
  # state is unreachable: the driver drops the acked result on the error
  # path (runOneTurn skips the done drain, Run returns before its
  # doneSignaled check), so completionResult never sees it. A fix confined
  # to completionResult passed that oracle and fixed nothing live. This
  # one was calibrated against both: it fails on main, on a
  # completionResult-only fix and on a driver-only fix, and passes on the
  # fix that does all three hops.
  #
  # It is written against the package's test harness (newTemplateManager,
  # recordingProvider, tmplFactory, attachEchoParent, awaitResult), not anything the agent's test
  # defines, so it compiles on both sides unless the fix reshaped those.
  # That case is its own failure line, not a silent pass: a fix that moves
  # them deserves a human look.
  #
  # It lives here as a heredoc, not as a _test.go file in the tree, where
  # it would fail on main, which still has the bug.
  A15="A15 the rig's own #${TASK_ISSUE} oracle fails before and passes after"
  ORACLE_PKG="pkg/agent/background"
  ORACLE_FILE="zz_selfdev_oracle_${TASK_ISSUE}_test.go"
  ORACLE_SRC="${RUN_DIR}/${ORACLE_FILE}"
  cat >"${ORACLE_SRC}" <<'GO'
package background

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// selfDevOracleLLM calls return_result on its first call and fails every
// call after it: the #1002 shape, where the model call that follows an
// acked return dies (a 429 in the live run).
type selfDevOracleLLM struct{ calls atomic.Int32 }

func (*selfDevOracleLLM) Name() string { return "selfdev-oracle" }

func (l *selfDevOracleLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if l.calls.Add(1) == 1 {
			fc := &genai.FunctionCall{Name: "return_result", Args: map[string]any{"result": "RIG-SENTINEL-RCA"}}
			content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
			yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
			return
		}
		yield(nil, errors.New("RIG-SENTINEL-ERR"))
	}
}

// Drives a real spawn through the autonomous driver and reads what the
// parent is handed, so a fix confined to completionResult cannot pass
// it: on the live path the banked result is dropped before that.
func TestSelfDevOracle1002(t *testing.T) {
	prov := &recordingProvider{llm: &selfDevOracleLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "oracle",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "oracle-model"),
		ModelID:      "oracle-model",
		Mode:         ModeStanding,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 4}), WithSyncWaitTimeout(30*time.Second))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "oracle", RefOverrides{Goal: "triage"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	b, err := json.Marshal(mgr.awaitResult(context.Background(), h))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"RIG-SENTINEL-RCA", "RIG-SENTINEL-ERR"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("spawn_agent result lacks %s: %s", want, b)
		}
	}
}
GO
  # oracle_at REV TREE OUT: prints pass, fail, build or checkout.
  oracle_at() {
    local rc=0
    rig_worktree "$2" "$1" || { echo checkout; return; }
    cp "${ORACLE_SRC}" "$2/${ORACLE_PKG}/${ORACLE_FILE}" || { drop_worktree "$2"; echo checkout; return; }
    (cd "$2" && go test -count=1 -timeout 5m -run '^TestSelfDevOracle1002$' "./${ORACLE_PKG}") >"$3" 2>&1 || rc=$?
    drop_worktree "$2"
    if [[ ${rc} -eq 0 ]]; then echo pass
    elif has_build_failure "$3"; then echo build
    else echo fail
    fi
  }
  ORACLE_PRE="$(oracle_at "${FORK}" "${RIG_TREES}/oracle-pre" "${RUN_DIR}/oracle-prefix.txt")"
  ORACLE_POST="$(oracle_at "${CLONE_TIP}" "${RIG_TREES}/oracle-post" "${RUN_DIR}/oracle-postfix.txt")"
  case "${ORACLE_PRE}/${ORACLE_POST}" in
    fail/pass) ok "${A15}" ;;
    pass/*)    bad "${A15}" "the oracle passes at ${FORK:0:8}, so the base no longer has the bug it was written for; the rig is stale" ;;
    fail/build) bad "${A15}" "the oracle doesn't compile against the fix (awaitResult or the package's spawn test helpers changed shape); a human should look, see ${RUN_DIR}/oracle-postfix.txt" ;;
    fail/fail) bad "${A15}" "the banked result or the run error still doesn't reach the parent; see ${RUN_DIR}/oracle-postfix.txt" ;;
    *)         bad "${A15}" "oracle ${ORACLE_PRE} at ${FORK:0:8}, ${ORACLE_POST} at the tip; see ${RUN_DIR}/oracle-*.txt" ;;
  esac
fi

head2 "A9  D4 — the real checkout is untouched"

REAL_AFTER="$(real_state | "${SUM}" | awk '{print $1}')"
if [[ "${REAL_AFTER}" == "${REAL_BEFORE}" ]]; then
  ok "A9 the real checkout did not move"
else
  bad "A9 the real checkout did not move" \
    "HEAD or working tree changed during the run — D4 says tiers 0 and 1 never touch it. Inspect: git -C ${REPO_ROOT} status"
fi

# ── The pull request ─────────────────────────────────────────────────

if [[ ${DRY_RUN} -eq 0 ]]; then
  head2 "A10–A12  the pull request"

  PR_JSON="${RUN_DIR}/pr.json"
  # The repo is the CLONE's origin, resolved once. The first draft ran
  # `gh repo view` in the script's own cwd and fell back to a hardcoded
  # slug, so running from /tmp or against a fork looked up the PR in the
  # wrong repository — and the A12 loop passed no --repo at all, which
  # then burned the full thirty-minute poll before reporting "pending".
  PR_REPO="$(cd "${CLONE}" && gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null || true)"
  if [[ -z "${PR_REPO}" ]]; then
    bad "A11 a pull request is open" "could not resolve the clone's GitHub repository from its origin"
    PR_REPO=""
  fi
  if [[ -n "${PR_REPO}" ]] && gh pr view --repo "${PR_REPO}" \
       "${BRANCH}" --json number,url,body,title,state,mergeStateStatus,statusCheckRollup \
       >"${PR_JSON}" 2>/dev/null; then
    PR_NUM="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["number"])' "${PR_JSON}")"
    PR_URL="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["url"])' "${PR_JSON}")"
    ok "A11 a pull request is open"
    note "PR #${PR_NUM} — ${PR_URL}"
  elif [[ -n "${PR_REPO}" ]]; then
    bad "A11 a pull request is open" "no PR found for ${BRANCH}; the terminal state of every tier is 'PR open' (D5)"
    PR_NUM=""
    PR_URL=""
  else
    PR_NUM=""
    PR_URL=""
  fi

  # The PR half of the attribution check, with the same scanner as A8.
  # Title as well as body: they are written by the same call and a run
  # that gets one right routinely gets the other wrong.
  if [[ -n "${PR_NUM}" ]]; then
    PR_TEXT="${RUN_DIR}/pr-text.txt"
    ATTR_PR_OUT="${RUN_DIR}/attribution-pr.txt"
    python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print((d.get("title") or "")+"\n"+(d.get("body") or ""))' \
      "${PR_JSON}" >"${PR_TEXT}"
    # An empty range: the commits are A8's, this is the text only.
    if (cd "${CLONE}" && "${REPO_ROOT}/dev/tools/verify-no-agent-attribution" \
          --range HEAD..HEAD --text-file "${PR_TEXT}") >"${ATTR_PR_OUT}" 2>&1; then
      ok "A10 no agent attribution in the PR title or body"
    else
      bad "A10 no agent attribution in the PR title or body" \
        "$(grep -m1 '^PR title/body' "${ATTR_PR_OUT}" || echo "see ${ATTR_PR_OUT}")"
    fi
  else
    # Reporting PASS on a PR body that was never read is the fail-open
    # shape this whole review pass was about.
    skip "A10 no agent attribution in the PR title or body" \
      "no PR could be read to check its title and body"
  fi

  if [[ -n "${PR_NUM}" ]]; then
    note "waiting for CI on #${PR_NUM} (the witness that cannot be talked into a pass)"
    CI_STATE="pending"
    for _ in $(seq 1 60); do
      sleep 30
      gh pr view --repo "${PR_REPO}" "${PR_NUM}" \
        --json mergeStateStatus,statusCheckRollup >"${PR_JSON}" 2>/dev/null || continue
      # An ALLOWLIST of terminal conclusions. The first draft failed a
      # hand-written list and called everything else green, which graded
      # SKIPPED, NEUTRAL, ACTION_REQUIRED, STARTUP_FAILURE and STALE as
      # green — so a PR with an unparseable workflow file reported "CI is
      # green" on a rig whose whole thesis is that CI cannot be talked
      # into a pass. SKIPPED and NEUTRAL are allowed by name because
      # ci.yml's paths-ignore makes them the normal outcome for a
      # docs-only change; nothing else is.
      CI_STATE="$(python3 - "${PR_JSON}" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
rollup=d.get("statusCheckRollup") or []
if not rollup: print("none"); raise SystemExit
PENDING={"","PENDING","IN_PROGRESS","QUEUED","EXPECTED","WAITING","REQUESTED"}
PASSING={"SUCCESS","SKIPPED","NEUTRAL"}
states=[(c.get("conclusion") or c.get("state") or "") for c in rollup]
if any(s in PENDING for s in states): print("pending")
elif all(s in PASSING for s in states): print("green:"+d.get("mergeStateStatus",""))
else: print("failed:"+",".join(sorted({s for s in states if s not in PASSING})))
PY
)"
      [[ "${CI_STATE}" == "pending" || "${CI_STATE}" == "none" ]] || break
    done
    case "${CI_STATE}" in
      # mergeStateStatus is fetched, so read it: "no checks reported" on a
      # PR means DIRTY, not broken CI, and a BLOCKED/BEHIND PR is not a
      # terminal green either.
      green:CLEAN|green:UNSTABLE|green:HAS_HOOKS)
        ok "A12 CI is green on the pull request" ;;
      green:*)
        bad "A12 CI is green on the pull request" \
          "checks passed but the PR is ${CI_STATE#green:} — see ${PR_URL}" ;;
      failed:*)
        bad "A12 CI is green on the pull request" \
          "non-passing conclusions: ${CI_STATE#failed:} — see ${PR_URL}" ;;
      none)
        bad "A12 CI is green on the pull request" \
          "no checks reported at all after 30 minutes — see ${PR_URL}" ;;
      *)
        bad "A12 CI is green on the pull request" "still ${CI_STATE} after 30 minutes — see ${PR_URL}" ;;
    esac
  else
    skip "A12 CI is green on the pull request" "no PR"
  fi
fi

exit $(( FAIL_COUNT > 0 ? 1 : 0 ))
