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

# Create self-development soak seed issues in the private mirror (#1213,
# docs/selfdev-soak-design.md decisions 5, 6, 12 and 20).
#
#   seed.sh --only A7 --dry-run        # print what it would create
#   seed.sh --only A7                  # create seed 0, the A7 issue
#   seed.sh --only M1,M2 | --all
#
# The seed list is the soak's answer key, so it is not in this
# repository. This script reads it from the operator's machine
# (--seeds, default ~/.gke-drill/selfdev-soak/seeds.md) and refuses a
# file inside any git work tree, symlinks resolved: a copy in a checkout
# is one commit away from the worker's tree (decision 20).
#
# Each selected seed becomes one issue, created with the operator's own
# `gh` login, which must be the maintainer's (decision 6: only issues the
# maintainer authored, labeled and assigned are ever forwarded):
#
#   - title: the seed text's first sentence;
#   - body:  "Upstream issue: <url>" first (the dispatcher takes the
#            first upstream link, decision 12), then the seed's full
#            text;
#   - labels: soak:queue and soak:expect-<class>;
#   - assignee: the maintainer.
#
# The expected class goes into the label ONLY, never the title or body,
# and the seed list's other columns (file:line evidence, why a person
# must decide, what is missing) are never read into an issue: the
# dispatcher injects title and body, and the worker must not be able to
# grade-shop from them. A bold "**Grade ...**" note in a seed's text is
# addressed to the grader; it is dropped with everything after it in the
# cell. A seed whose title or body would still name a class, or say how
# it is graded, is refused.
#
# The upstream issue is the one the seed's evidence column starts with
# (`#1234 ...`), else the one its text starts with (`#884: ...`), else
# --default-upstream-issue (1213, the soak's tracking issue). --dry-run
# shows which, so check it before creating anything.
#
# Seed list format: markdown. A `## ` heading names the class
# (`mergeable`, `should-escalate`, `should-stop`; the "Seed 0 (A7)"
# section is mergeable: A7 closes on a green PR). Under it, a table
# whose first column is the seed id and second is its text. `\|` is a
# literal pipe inside a cell.
set -euo pipefail

SEEDS="${HOME}/.gke-drill/selfdev-soak/seeds.md"
REPO="mastersingh24/core-agent-selfdev"
MAINTAINER="mastersingh24"
UPSTREAM_REPO="go-steer/core-agent"
DEFAULT_UPSTREAM_ISSUE="1213"
ONLY=""
ALL=0
DRY_RUN=0

die() {
  echo "seed.sh: $*" >&2
  exit 2
}

usage() {
  cat <<'EOF'
Usage: dev/uat/selfdev-soak/seed.sh (--only ID[,ID...] | --all) [options]

  --only IDS                 comma-separated seed ids (e.g. A7, or M1,E2)
  --all                      every seed in the list
  --dry-run                  print the issues; create nothing, call no gh
  --seeds PATH               the private seed list
                             (default ~/.gke-drill/selfdev-soak/seeds.md);
                             refused inside any git work tree
  --repo OWNER/NAME          the mirror (default mastersingh24/core-agent-selfdev)
  --maintainer LOGIN         author, labeler and assignee; the gh login must
                             be this (default mastersingh24)
  --upstream-repo OWNER/NAME (default go-steer/core-agent)
  --default-upstream-issue N for a seed that names none (default 1213)
  -h, --help                 this text
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --only)                     [[ $# -ge 2 ]] || die "--only needs a value"; ONLY="$2"; shift 2 ;;
    --only=*)                   ONLY="${1#--only=}"; shift ;;
    --all)                      ALL=1; shift ;;
    --dry-run)                  DRY_RUN=1; shift ;;
    --seeds)                    [[ $# -ge 2 ]] || die "--seeds needs a value"; SEEDS="$2"; shift 2 ;;
    --seeds=*)                  SEEDS="${1#--seeds=}"; shift ;;
    --repo)                     [[ $# -ge 2 ]] || die "--repo needs a value"; REPO="$2"; shift 2 ;;
    --repo=*)                   REPO="${1#--repo=}"; shift ;;
    --maintainer)               [[ $# -ge 2 ]] || die "--maintainer needs a value"; MAINTAINER="$2"; shift 2 ;;
    --maintainer=*)             MAINTAINER="${1#--maintainer=}"; shift ;;
    --upstream-repo)            [[ $# -ge 2 ]] || die "--upstream-repo needs a value"; UPSTREAM_REPO="$2"; shift 2 ;;
    --upstream-repo=*)          UPSTREAM_REPO="${1#--upstream-repo=}"; shift ;;
    --default-upstream-issue)   [[ $# -ge 2 ]] || die "--default-upstream-issue needs a value"; DEFAULT_UPSTREAM_ISSUE="$2"; shift 2 ;;
    --default-upstream-issue=*) DEFAULT_UPSTREAM_ISSUE="${1#--default-upstream-issue=}"; shift ;;
    -h|--help)                  usage; exit 0 ;;
    *)                          usage >&2; die "unknown argument: $1" ;;
  esac
done

if [[ -z "${ONLY}" && ${ALL} -eq 0 ]]; then
  usage >&2
  die "choose seeds with --only ID[,ID...] or --all; there is no default"
fi
[[ -z "${ONLY}" || ${ALL} -eq 0 ]] || die "--only and --all are mutually exclusive"
[[ "${DEFAULT_UPSTREAM_ISSUE}" =~ ^[0-9]+$ ]] || die "--default-upstream-issue must be an issue number"
[[ "${REPO}" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die "--repo ${REPO}: want owner/name"
[[ "${UPSTREAM_REPO}" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die "--upstream-repo ${UPSTREAM_REPO}: want owner/name"
[[ "${MAINTAINER}" =~ ^[A-Za-z0-9-]+$ ]] || die "--maintainer ${MAINTAINER}: not a GitHub login"

# ---- Decision 20: the answer key never sits in a git work tree ----

# in_work_tree DIR: true when DIR or any ancestor holds a .git entry (a
# directory for a clone, a file for a linked worktree or submodule).
# A walk, not `git rev-parse`: git stops at filesystem boundaries, and
# refuses (exit non-zero) a repository another user owns, and either
# would read as "not in a work tree", which is the unsafe answer.
in_work_tree() {
  local d="$1"
  while :; do
    if [[ -e "${d}/.git" || -L "${d}/.git" ]]; then
      return 0
    fi
    case "${d}" in
      */.git|*/.git/*) return 0 ;;
    esac
    [[ "${d}" == "/" ]] && return 1
    d="$(dirname "${d}")"
  done
}

[[ -e "${SEEDS}" ]] || die "no seed list at ${SEEDS} (pass --seeds)"
[[ -f "${SEEDS}" ]] || die "${SEEDS} is not a regular file"
seeds_dir="$(cd -- "$(dirname -- "${SEEDS}")" && pwd -P)" || die "cannot resolve ${SEEDS}"
seeds_real="$(readlink -f -- "${SEEDS}")" || die "cannot resolve ${SEEDS}"
for d in "${seeds_dir}" "$(dirname -- "${seeds_real}")"; do
  # The walk catches every clone and linked worktree. git itself also
  # answers when it can, which covers a work tree named only by
  # GIT_DIR/GIT_WORK_TREE in this environment; its silence proves
  # nothing, so only a "true" is used.
  if in_work_tree "${d}" || [[ "$(git -C "${d}" rev-parse --is-inside-work-tree 2>/dev/null || true)" == "true" ]]; then
    die "refusing ${SEEDS}: it is inside a git work tree (${d}). The seed list is the soak's answer key and must live outside every repository (design decision 20); move it, e.g. to ~/.gke-drill/selfdev-soak/seeds.md"
  fi
done
# A hard link has other names, which this check cannot see; one of them
# could be in a checkout.
links="$(stat -c %h -- "${seeds_real}" 2>/dev/null || stat -f %l -- "${seeds_real}")"
[[ "${links}" == "1" ]] || die "refusing ${SEEDS}: it has ${links} hard links, and another name for it could sit in a git work tree; keep one copy, outside every repository"

# ---- Parse the seed list ----

declare -a IDS=()
declare -A CLASS=() TEXT=() EVIDENCE=()

trim() {
  local s="$1"
  s="${s#"${s%%[![:space:]]*}"}"
  s="${s%"${s##*[![:space:]]}"}"
  printf '%s' "${s}"
}

class=""
while IFS= read -r line || [[ -n "${line}" ]]; do
  # Any heading starts a new section. Only a `## ` heading naming a class
  # (or seed 0) opens one; every other heading, `### Notes` included,
  # closes it, so a table outside a class section never becomes seeds.
  if [[ "${line}" =~ ^#+[[:space:]] ]]; then
    if [[ ! "${line}" =~ ^##[[:space:]] ]]; then
      class=""
      continue
    fi
    case "${line}" in
      *"Seed 0"*)         class="mergeable" ;;
      *should-escalate*)  class="should-escalate" ;;
      *should-stop*)      class="should-stop" ;;
      *mergeable*)        class="mergeable" ;;
      *)                  class="" ;;
    esac
    continue
  fi
  [[ -n "${class}" && "${line}" == "|"* ]] || continue
  # \| is a literal pipe inside a cell: park it on a byte no cell holds.
  row="${line//\\|/$'\x1f'}"
  row="${row#|}"
  IFS='|' read -r -a cells <<<"${row}"
  [[ ${#cells[@]} -ge 2 ]] || continue
  id="$(trim "${cells[0]}")"
  text="$(trim "${cells[1]//$'\x1f'/|}")"
  evidence=""
  if [[ ${#cells[@]} -ge 3 ]]; then
    evidence="$(trim "${cells[2]//$'\x1f'/|}")"
  fi
  # The header row and the |---| separator.
  [[ "${id,,}" == "id" || "${id}" =~ ^:?-+:?$ || -z "${id}" ]] && continue
  [[ "${id}" =~ ^[A-Za-z0-9]+$ ]] || die "${SEEDS}: seed id ${id} is not alphanumeric"
  [[ -z "${CLASS[${id}]+set}" ]] || die "${SEEDS}: seed ${id} appears twice"
  IDS+=("${id}")
  CLASS["${id}"]="${class}"
  TEXT["${id}"]="${text}"
  EVIDENCE["${id}"]="${evidence}"
done < "${SEEDS}"

[[ ${#IDS[@]} -gt 0 ]] || die "${SEEDS} holds no seeds"

declare -a SELECTED=()
if [[ ${ALL} -eq 1 ]]; then
  SELECTED=("${IDS[@]}")
else
  IFS=',' read -r -a wanted <<<"${ONLY}"
  for w in "${wanted[@]}"; do
    w="$(trim "${w}")"
    [[ -n "${w}" ]] || continue
    [[ -n "${CLASS[${w}]+set}" ]] || die "no seed ${w} in ${SEEDS}"
    SELECTED+=("${w}")
  done
  [[ ${#SELECTED[@]} -gt 0 ]] || die "--only named no seeds"
fi

# ---- Build each issue ----

declare -A TITLE=() BODY=()
# Words that would tell the worker its expected outcome, or how it is
# graded. A seed whose issue text still contains one is refused, not
# filed: rewording the seed is cheap, and an answer key in a task is
# not detectable afterwards.
class_word_re='(mergeable|should[- ]escalate|should[- ]stop|soak:expect|expect-|(^|[^[:alnum:]])grad(e|ed|er|ers|ing)([^[:alnum:]]|$))'

for id in "${SELECTED[@]}"; do
  text="${TEXT[${id}]}"
  # A bold "**Grade ...**" note is for the grader, not the worker. Drop
  # everything from the marker to the end of the cell: grader notes trail
  # the task text, and no sentence boundary can be trusted inside one
  # (a dotted filename or an "e.g." would end it early and leak the rest).
  # Losing task text after a note fails safe; leaking the note doesn't.
  if [[ "${text}" == *'**Grade'* ]]; then
    text="$(trim "${text%%\*\*Grade*}")"
  fi
  plain="${text//\*\*/}"
  title="${plain%%. *}"
  title="$(trim "${title%.}")"
  if [[ ${#title} -gt 200 ]]; then
    title="${title:0:197}..."
  fi

  upstream=""
  if [[ "${EVIDENCE[${id}]}" =~ ^#([0-9]+) ]]; then
    upstream="${BASH_REMATCH[1]}"
  elif [[ "${plain}" =~ ^#([0-9]+) ]]; then
    upstream="${BASH_REMATCH[1]}"
  else
    upstream="${DEFAULT_UPSTREAM_ISSUE}"
    echo "seed.sh: seed ${id} names no upstream issue at the start of its evidence or text; linking #${upstream}" >&2
  fi
  url="https://github.com/${UPSTREAM_REPO}/issues/${upstream}"
  body="Upstream issue: ${url}"$'\n\n'"${text}"$'\n'

  shopt -s nocasematch
  if [[ "${title}" =~ ${class_word_re} || "${body}" =~ ${class_word_re} ]]; then
    shopt -u nocasematch
    die "seed ${id}: its title or body names an expected class or grading (\"$(trim "${BASH_REMATCH[1]}")\"); those go in a label only, never the issue text (decision 20). Reword the seed"
  fi
  shopt -u nocasematch
  [[ -n "${title}" ]] || die "seed ${id} has no text"
  TITLE["${id}"]="${title}"
  BODY["${id}"]="${body}"
done

if [[ ${DRY_RUN} -eq 1 ]]; then
  echo "seed.sh: --dry-run: would create ${#SELECTED[@]} issue(s) in ${REPO} as ${MAINTAINER}; nothing was created"
  for id in "${SELECTED[@]}"; do
    echo "---"
    echo "seed: ${id}"
    echo "labels: soak:queue, soak:expect-${CLASS[${id}]}"
    echo "assignee: ${MAINTAINER}"
    echo "title: ${TITLE[${id}]}"
    echo "body:"
    printf '%s' "${BODY[${id}]}" | sed 's/^/    /'
  done
  exit 0
fi

# ---- Create, under the maintainer's own gh login ----

command -v gh >/dev/null 2>&1 || die "gh is not on PATH"

login="$(gh api user --jq .login)" || die "gh api user failed; run gh auth login as ${MAINTAINER}"
if [[ "${login,,}" != "${MAINTAINER,,}" ]]; then
  die "gh is logged in as ${login}, not ${MAINTAINER}. The dispatcher forwards only issues the maintainer authored, labeled and assigned (decision 6), so these would never be picked up"
fi

labels="$(gh label list --repo "${REPO}" --limit 500 --json name --jq '.[].name')" || die "gh label list --repo ${REPO} failed"
declare -A NEED=([soak:queue]=1)
for id in "${SELECTED[@]}"; do
  NEED["soak:expect-${CLASS[${id}]}"]=1
done
missing=()
for l in "${!NEED[@]}"; do
  grep -qxF -- "${l}" <<<"${labels}" || missing+=("${l}")
done
if [[ ${#missing[@]} -gt 0 ]]; then
  die "${REPO} lacks label(s): ${missing[*]}. Create them first (README.md, runbook), e.g. gh label create --repo ${REPO} ${missing[0]}"
fi

existing="$(gh issue list --repo "${REPO}" --state all --limit 1000 --json title --jq '.[].title')" || die "gh issue list --repo ${REPO} failed"

BODY_DIR="$(mktemp -d)"
# shellcheck disable=SC2064  # expand now, not at trap time
trap "rm -rf '${BODY_DIR}'" EXIT

for id in "${SELECTED[@]}"; do
  if grep -qxF -- "${TITLE[${id}]}" <<<"${existing}"; then
    echo "seed.sh: seed ${id}: an issue titled \"${TITLE[${id}]}\" already exists in ${REPO}; skipped" >&2
    continue
  fi
  printf '%s' "${BODY[${id}]}" > "${BODY_DIR}/${id}.md"
  url="$(gh issue create --repo "${REPO}" \
    --title "${TITLE[${id}]}" \
    --body-file "${BODY_DIR}/${id}.md" \
    --label soak:queue \
    --label "soak:expect-${CLASS[${id}]}" \
    --assignee "${MAINTAINER}")" || die "seed ${id}: gh issue create failed"
  echo "seed.sh: seed ${id}: ${url}"
  # Two seeds in one run whose titles collide must not both be filed.
  existing+=$'\n'"${TITLE[${id}]}"
done
