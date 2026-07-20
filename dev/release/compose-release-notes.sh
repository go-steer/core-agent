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

# Find the newest stable tag (no `-pre` suffix) that is an ancestor
# of the given ref. Returns empty if none found.
last_stable_before() {
  local ref="$1"
  git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname --merged "$ref" \
    | grep -v -- '-' \
    | grep -v "^${ref}\$" \
    | head -n1
}

# Path to the git-cliff config that ships alongside this script.
# Located next to compose-release-notes.sh so both stay together.
CLIFF_CONFIG="$(dirname "$0")/cliff.toml"

# Emit the K8s-style grouped PR list for the tag range via git-cliff.
# Used as the fallback body when compose's primary CHANGELOG-section
# extraction finds nothing. Replaces the ~90-line render_changes_by_kind
# / render_bullet / classify_type bash helpers deleted in this PR;
# git-cliff handles group ordering, PR-link injection, and
# subject-cleanup via cliff.toml's template.
#
# Args: START_REF (exclusive) END_REF (inclusive; usually a tag).
render_range_with_cliff() {
  local start_ref="$1" end_ref="$2"
  if ! command -v git-cliff >/dev/null 2>&1; then
    echo "_git-cliff not on \$PATH — skipping auto-generated PR list. "
    echo "See ${CLIFF_CONFIG} for install instructions._"
    return
  fi
  local range="${end_ref}"
  if [[ -n "$start_ref" ]]; then
    range="${start_ref}..${end_ref}"
  fi
  # --tag pins the output header to the target release version even
  # when the tag doesn't yet exist locally (release workflows tag
  # then invoke compose in the same job). Errors surface on stderr;
  # empty output surfaces the "no commits in range" case above.
  git-cliff \
    --config "$CLIFF_CONFIG" \
    --tag "$end_ref" \
    "$range" 2>/dev/null \
    | sed '/^$/N;/^\n$/D'  # collapse consecutive blank lines
}

# Emit a "### Contributors" trailer listing unique commit authors in
# the range. Fires on both the primary (CHANGELOG-driven) and fallback
# paths so every release credits the folks whose PRs landed.
#
# Deliberately independent of git-cliff — a shell one-liner covers
# this and keeps the trailer working even when git-cliff is absent.
render_contributors() {
  local start_ref="$1" end_ref="$2"
  local range="${end_ref}"
  if [[ -n "$start_ref" ]]; then
    range="${start_ref}..${end_ref}"
  fi
  local authors
  authors="$(git log --format='%aN' "$range" 2>/dev/null | sort -u)"
  if [[ -z "$authors" ]]; then
    return
  fi
  echo "### Contributors"
  echo
  while IFS= read -r name; do
    [[ -z "$name" ]] && continue
    printf -- '- %s\n' "$name"
  done <<<"$authors"
  echo
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
  # it with an auto-generated PR list produced by git-cliff.
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

      # Auto-generated PR list from git log range (git-cliff-driven).
      # Section header differs by whether we found a stable ancestor
      # to anchor the range against.
      if [[ -n "$LAST_STABLE" ]]; then
        printf '## Commits since %s\n\n' "$LAST_STABLE"
      else
        printf '## Commits in this pre-release\n\n'
      fi
      render_range_with_cliff "$LAST_STABLE" "$TAG"
    } > "$NOTES"
  fi
fi

# Append contributor trailer to BOTH paths — the CHANGELOG-driven
# primary path doesn't naturally credit anyone, and the git-cliff
# fallback's grouped-PR-list section already gives us the raw
# commit-author info to work with. Position between narrative +
# install footer so it reads as "and thanks to..." before the
# operator-facing install steps.
LAST_STABLE="${LAST_STABLE:-$(last_stable_before "$TAG" || true)}"
{
  echo
  render_contributors "$LAST_STABLE" "$TAG"
} >> "$NOTES"

# Append the install/verify footer with placeholders substituted.
if [[ -f "$FOOTER" ]]; then
  sed \
    -e "s|@TAG@|${TAG}|g" \
    -e "s|@VERSION@|${VERSION}|g" \
    "$FOOTER" \
    >> "$NOTES"
fi

echo "wrote release notes for ${TAG} to ${NOTES}" >&2
