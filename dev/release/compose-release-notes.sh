#!/usr/bin/env bash
#
# Compose release notes for a tagged release.
#
# Stable tags (vX.Y.Z): carve the matching `## [X.Y.Z]` section out
# of CHANGELOG.md. Fail loudly if the section is missing.
#
# Pre-release tags (vX.Y.Z-dev.N, -rc.N, -alpha.N, -beta.N): if a
# matching `## [X.Y.Z-…]` section exists in CHANGELOG.md use it as-is;
# otherwise use `## [Unreleased]` as the narrative and auto-append a
# PR list from `git log LAST_STABLE..TAG` grouped by conventional-
# commit type. Never hard-fail on missing section — dev tags are cut
# frequently and shouldn't require a per-tag CHANGELOG edit.
#
# Both paths append the install/verify footer template with @TAG@ /
# @VERSION@ placeholders substituted.
#
# Usage: compose-release-notes.sh TAG NOTES_PATH [CHANGELOG_PATH] [FOOTER_PATH]
#   TAG           — the git tag (e.g. v2.6.0 or v2.7.0-dev.3)
#   NOTES_PATH    — where to write the composed notes markdown
#   CHANGELOG_PATH— path to CHANGELOG.md (default: ./CHANGELOG.md)
#   FOOTER_PATH   — path to install-verify-footer.md.tmpl
#                   (default: ./dev/release/install-verify-footer.md.tmpl)
#
# Env:
#   REPO_URL      — https://github.com/… base for PR links
#                   (default: https://github.com/go-steer/core-agent)
#
# Exits non-zero when: tag format is invalid; the CHANGELOG has no
# matching stable section (pre-release path never fails on that).

set -euo pipefail

TAG="${1:?TAG required (vX.Y.Z or vX.Y.Z-pre)}"
NOTES="${2:?NOTES_PATH required}"
CHANGELOG="${3:-CHANGELOG.md}"
FOOTER="${4:-dev/release/install-verify-footer.md.tmpl}"
REPO_URL="${REPO_URL:-https://github.com/go-steer/core-agent}"

# Reject malformed tags (this also catches e.g. `vfoo.bar.baz`).
if [[ ! "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.]+)?$ ]]; then
  echo "::error::expected vX.Y.Z or vX.Y.Z-<pre>, got: ${TAG}" >&2
  exit 1
fi

VERSION="${TAG#v}"          # e.g. 2.7.0-dev.3
BASE_VERSION="${VERSION%%-*}" # e.g. 2.7.0

IS_PRERELEASE=0
if [[ "$VERSION" == *-* ]]; then
  IS_PRERELEASE=1
fi

# Extract a `## [VERSION]` section from a CHANGELOG-shaped stream on
# stdin. Enters on `## [VERSION]`, stops at the next `## [`.
extract_section() {
  local ver="$1"
  awk -v ver="$ver" '
    $0 ~ "^## \\[" ver "\\]" { in_section=1; print; next }
    in_section && /^## \[/    { exit }
    in_section                { print }
  '
}

# Map a conventional-commit type prefix to a K8s-style category
# heading. Anything unrecognized falls through to "Other".
classify_type() {
  local t="$1"
  case "$t" in
    feat)             echo "Feature" ;;
    fix)              echo "Bug or Regression" ;;
    docs|docs+ci)     echo "Documentation" ;;
    chore|polish|refactor|test|ci|style|perf|build)
                      echo "Other (Cleanup)" ;;
    *)                echo "Other" ;;
  esac
}

# Given a commit subject, print `KIND|BULLET_TEXT|PR_NUMBER`. Empty
# PR_NUMBER when the subject has no trailing `(#NNN)`. Bullet text
# strips the `type(scope):` prefix and the trailing PR ref so the
# markdown link can be appended cleanly.
render_bullet() {
  local subject="$1"
  local prefix rest pr kind
  # Split on the first `: ` — everything before is the prefix, after
  # is the human-readable subject.
  if [[ "$subject" == *": "* ]]; then
    prefix="${subject%%: *}"
    rest="${subject#*: }"
  else
    prefix=""
    rest="$subject"
  fi
  # Prefix looks like `feat`, `feat(scope)`, or `feat(scope+scope2)`.
  # Strip the `(...)` if present.
  local type_only="${prefix%%(*}"
  kind="$(classify_type "$type_only")"
  # Pull the last `(#NNN)` off the tail — some subjects embed
  # intermediate `(#NNN)` refs to related issues, so use the trailing
  # one as the PR number.
  if [[ "$rest" =~ \(#([0-9]+)\)[[:space:]]*$ ]]; then
    pr="${BASH_REMATCH[1]}"
    # Strip the trailing `(#NNN)` and any preceding whitespace.
    rest="$(printf '%s' "$rest" | sed -E 's/[[:space:]]*\(#[0-9]+\)[[:space:]]*$//')"
  else
    pr=""
  fi
  printf '%s|%s|%s\n' "$kind" "$rest" "$pr"
}

# Emit `### Changes by Kind` + per-kind subsections from a
# newline-separated list of commit subjects on stdin.
render_changes_by_kind() {
  local subjects
  subjects="$(cat)"
  if [[ -z "$subjects" ]]; then
    echo "_No commits in this range._"
    return
  fi

  # Build a single tab-separated table: KIND\tBULLET\tPR (one row per
  # commit). Then group by KIND for output.
  local rows=""
  while IFS= read -r subject; do
    [[ -z "$subject" ]] && continue
    rows+="$(render_bullet "$subject")"$'\n'
  done <<<"$subjects"

  echo "### Changes by Kind"
  echo

  # Emit in a fixed order so output is deterministic.
  local kind
  for kind in "Feature" "Bug or Regression" "Documentation" "Other (Cleanup)" "Other"; do
    local matches
    matches="$(printf '%s' "$rows" | awk -F'|' -v k="$kind" '$1 == k')"
    if [[ -n "$matches" ]]; then
      printf '#### %s\n' "$kind"
      while IFS='|' read -r _ bullet pr; do
        [[ -z "$bullet" ]] && continue
        if [[ -n "$pr" ]]; then
          printf -- '- %s ([#%s](%s/pull/%s))\n' "$bullet" "$pr" "$REPO_URL" "$pr"
        else
          printf -- '- %s\n' "$bullet"
        fi
      done <<<"$matches"
      echo
    fi
  done
}

# Find the newest stable tag (no `-pre` suffix) that is an ancestor
# of the given ref. Returns empty if none found.
last_stable_before() {
  local ref="$1"
  git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname --merged "$ref" \
    | grep -v -- '-' \
    | grep -v "^${ref}\$" \
    | head -n1
}

# ────────── stable path ──────────
if [[ "$IS_PRERELEASE" -eq 0 ]]; then
  extract_section "$VERSION" < "$CHANGELOG" > "$NOTES"
  if [[ ! -s "$NOTES" ]]; then
    echo "::error::no CHANGELOG.md section [${VERSION}] found in ${CHANGELOG}" >&2
    exit 1
  fi

# ────────── pre-release path ──────────
else
  # Prefer an explicit `## [X.Y.Z-pre]` section if the operator wrote
  # one; otherwise use `## [Unreleased]` as the narrative and follow
  # it with an auto-generated PR list.
  extract_section "$VERSION" < "$CHANGELOG" > "$NOTES"
  if [[ ! -s "$NOTES" ]]; then
    UNRELEASED="$(extract_section "Unreleased" < "$CHANGELOG" || true)"
    LAST_STABLE="$(last_stable_before "$TAG" || true)"
    {
      printf '## Pre-release [%s]\n\n' "$VERSION"
      if [[ -n "$UNRELEASED" ]]; then
        # Drop the leading `## [Unreleased]` header and the blank
        # line right after it — we've already emitted our own
        # pre-release header above.
        printf '%s\n\n' "$UNRELEASED" | sed -E '1,2d'
      else
        printf 'Pre-release build of %s.\n\n' "$BASE_VERSION"
      fi

      # Auto-generated PR list from git log range.
      if [[ -n "$LAST_STABLE" ]]; then
        printf '## Commits since %s\n\n' "$LAST_STABLE"
        git log --pretty='%s' "$LAST_STABLE..$TAG" | render_changes_by_kind
      else
        printf '## Commits in this pre-release\n\n'
        git log --pretty='%s' "$TAG" | head -n 200 | render_changes_by_kind
      fi
    } > "$NOTES"
  fi
fi

# Append the install/verify footer with placeholders substituted.
if [[ -f "$FOOTER" ]]; then
  sed \
    -e "s|@TAG@|${TAG}|g" \
    -e "s|@VERSION@|${VERSION}|g" \
    "$FOOTER" \
    >> "$NOTES"
fi

echo "wrote release notes for ${TAG} to ${NOTES}" >&2
