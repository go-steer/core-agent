#!/usr/bin/env bash
#
# Cut a pre-release (dev / rc / alpha / beta) tag.
#
# Mechanical carve: rename `## [Unreleased]` → `## [X.Y.Z-pre] — YYYY-MM-DD`
# in CHANGELOG.md, insert a fresh empty `## [Unreleased]` above it as the
# next landing pad, and print the git commands to commit + tag + push.
#
# Precondition: `[Unreleased]` must already have real content (agents add
# a bullet to it on every merged PR, per AGENTS.md). If it's empty or just
# the "No unreleased changes" boilerplate, we bail early — the dev tag
# needs a narrative + PR list, and this script is intentionally not the
# thing that invents them.
#
# Usage: cut-dev-tag.sh vX.Y.Z-<pre>
#   e.g.: cut-dev-tag.sh v2.7.0-dev.4
#
# This script edits CHANGELOG.md in place and shows the diff, but does NOT
# commit, tag, or push. Follow the printed steps to finish the cut.

set -euo pipefail

TAG="${1:?TAG required (e.g. v2.7.0-dev.4)}"
CHANGELOG="${CHANGELOG:-CHANGELOG.md}"

# Pre-release tags only. Stable tags flow through docs/release-process.md.
if [[ ! "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-[A-Za-z0-9.]+$ ]]; then
  echo "error: expected vX.Y.Z-<pre> (e.g. v2.7.0-dev.4); got: ${TAG}" >&2
  echo "for stable releases see docs/release-process.md" >&2
  exit 1
fi
VERSION="${TAG#v}"

if [[ ! -f "$CHANGELOG" ]]; then
  echo "error: ${CHANGELOG} not found (run from repo root)" >&2
  exit 1
fi

# Refuse to overwrite an existing tag.
if git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null; then
  echo "error: tag ${TAG} already exists" >&2
  exit 1
fi

# Preflight: run the release-time guards that .github/workflows/release.yml
# runs, BEFORE we carve the CHANGELOG. Motivation (2026-07-18 retag): the
# v2.7.0-dev.4 tag got cut on a day when internal/pricing/builtin.go was
# 3 days stale relative to LiteLLM's current catalog. The pricing-freshness
# guard in release.yml rejected the build, forcing a retag. Running the
# same guard here catches the drift locally in ~10s instead of a 4-minute
# CI round-trip that ends in a broken tag.
#
# Guards mirrored from release.yml — keep in sync when new guards land
# there. Each one prints its own remediation on failure.
echo "── Preflight: release guards ────────────────────────────"

# Guard 1: pricing catalog freshness. Same command release.yml runs.
# Skips if the regen tool isn't present (pre-#259 checkouts) so
# retroactive dev-tag operators aren't blocked.
if [[ -d "dev/regen-builtin-pricing" ]]; then
  go run ./dev/regen-builtin-pricing >/dev/null
  if ! git diff --quiet internal/pricing/builtin.go; then
    echo "error: internal/pricing/builtin.go is stale vs LiteLLM's current catalog." >&2
    echo "" >&2
    echo "Remediation: commit the diff (or discard if the drift is intentional)," >&2
    echo "then re-run this script." >&2
    echo "" >&2
    echo "Diff:" >&2
    git --no-pager diff internal/pricing/builtin.go >&2
    exit 1
  fi
  echo "  pricing freshness: OK"
fi

# (Add new preflight guards above this line as they get added to
# release.yml. Keep this section short — the tag-cut script is not the
# place for expensive checks.)
echo "── Preflight OK ─────────────────────────────────────────"
echo ""

# Extract the [Unreleased] section content — everything between the
# `## [Unreleased]` header and the next `## [` header, excluding both
# headers themselves.
UNRELEASED_BODY="$(awk '
  /^## \[Unreleased\]/ { in_section=1; next }
  in_section && /^## \[/ { exit }
  in_section
' "$CHANGELOG")"

# Refuse to cut a tag when [Unreleased] has nothing real in it. The
# "No unreleased changes" boilerplate + blank lines is the sentinel.
STRIPPED="$(printf '%s' "$UNRELEASED_BODY" \
  | grep -v '^[[:space:]]*$' \
  | grep -v '^_No unreleased changes' \
  || true)"
if [[ -z "$STRIPPED" ]]; then
  echo "error: [Unreleased] is empty or still the boilerplate stub." >&2
  echo "add a narrative + Changes by Kind entries under [Unreleased] first," >&2
  echo "then re-run. (Every PR that merges should already bump [Unreleased];" >&2
  echo "if this session lands a PR that didn't, backfill before tagging.)" >&2
  exit 1
fi

# Compose the replacement: fresh empty [Unreleased] + dated pre-release
# header carrying the current body.
TODAY="$(git log -1 --format='%cs')"  # commit-date of HEAD, YYYY-MM-DD
NEW_UNRELEASED=$'## [Unreleased]\n\n_No unreleased changes since ['"${VERSION}"$']._\n'

# In-place rewrite. Use a python one-liner for clarity; we already
# depend on python via other release tooling and it beats a fragile
# multi-line sed/awk for the delimited replacement.
python3 - "$CHANGELOG" "$TAG" "$TODAY" <<'PY'
import sys, re
path, tag, today = sys.argv[1:]
version = tag.removeprefix("v")
src = open(path).read()

m = re.search(r"^## \[Unreleased\]\n", src, flags=re.M)
if not m:
    sys.exit("error: no `## [Unreleased]` header in " + path)

replacement = (
    f"## [Unreleased]\n\n"
    f"_No unreleased changes since [{version}]._\n\n"
    f"## [{version}] — {today}\n"
)
out = src[:m.start()] + replacement + src[m.end():]
open(path, "w").write(out)
PY

# Show what changed so the human can eyeball before committing.
echo ""
echo "── diff ─────────────────────────────────────────────────"
git --no-pager diff -- "$CHANGELOG" | head -40
echo "..."
echo "── next steps ───────────────────────────────────────────"
cat <<EOF
git add ${CHANGELOG}
git commit -m "chore(changelog): promote [Unreleased] to [${VERSION}]"
git tag ${TAG}
git push origin main ${TAG}
EOF
