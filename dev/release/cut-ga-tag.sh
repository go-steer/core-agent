#!/usr/bin/env bash
#
# Cut a GA (stable) tag by folding every pre-release section since the
# last GA — plus `## [Unreleased]` — into a cumulative `## [X.Y.Z]`
# entry, then reseeding a fresh empty `## [Unreleased]` above it.
#
# Why the fold: a GA changelog entry should be the "since last GA"
# story an operator upgrading from vX.(Y-1).0 reads, not just what
# accumulated after the last dev tag. This was the v2.7.0 lesson —
# the first-cut GA entry only covered ~5 post-dev.5 bullets, missing
# every feature from dev.1..dev.4, and had to be rewritten by hand.
#
# What the script does:
#
#   1. Preflight guards (mirror release.yml).
#   2. Extract bullets from every section between `## [Unreleased]`
#      and the previous GA — grouped by kind (Feature / Bug / Docs /
#      Cleanup / Changed). Bullets starting with `**BREAKING:**` are
#      hoisted to a top-level `### Breaking Changes` section.
#   3. Compose the new `## [X.Y.Z] — YYYY-MM-DD` section with a
#      `<HEADLINE — replace this paragraph before committing>`
#      placeholder for the operator-facing summary, the hoisted
#      breaking-changes section (if any), and the merged bullets.
#   4. Append a pre-release-history trailer listing the dev tags
#      that were folded (with their original dates).
#   5. Delete the folded pre-release sections.
#   6. Reseed `## [Unreleased]` above the new GA entry.
#   7. Print the git commands to commit + tag + push + do the
#      required v(X.Y+1).0-dev follow-up bump.
#
# Usage: cut-ga-tag.sh vX.Y.Z
#   e.g.: cut-ga-tag.sh v2.8.0
#
# Editorial pass after this script runs:
#   - Replace the `<HEADLINE — ...>` placeholder with a real 3-5
#     sentence operator-facing summary.
#   - Optionally sub-group bullets under theme headers within a kind
#     (e.g. `_Digest + cost attribution stack:_`) — the script leaves
#     them as one flat list per kind; sub-grouping is human editorial
#     that can't be automated meaningfully.
#
# This script edits CHANGELOG.md in place and shows the diff, but does
# NOT commit, tag, or push. Follow the printed steps to finish the cut.

set -euo pipefail

TAG="${1:?TAG required (e.g. v2.8.0)}"
CHANGELOG="${CHANGELOG:-CHANGELOG.md}"

# Stable tags only. Pre-releases flow through cut-dev-tag.sh.
if [[ ! "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "error: expected vX.Y.Z (e.g. v2.8.0); got: ${TAG}" >&2
  echo "for dev / rc / pre-release tags see cut-dev-tag.sh" >&2
  exit 1
fi
VERSION="${TAG#v}"

if [[ ! -f "$CHANGELOG" ]]; then
  echo "error: ${CHANGELOG} not found (run from repo root)" >&2
  exit 1
fi

if git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null; then
  echo "error: tag ${TAG} already exists" >&2
  exit 1
fi

# Preflight: run the release-time guards that .github/workflows/release.yml
# runs, BEFORE we carve the CHANGELOG. See cut-dev-tag.sh for the origin
# story (2026-07-18 v2.7.0-dev.4 retag). Keep these two blocks in sync —
# both scripts should reject the same drift.
echo "── Preflight: release guards ────────────────────────────"

# Guard 1: pricing catalog freshness.
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

# Guard 2: target version is next-minor (or next-major) of the latest
# GA tag. Same logic dev/ci/presubmits/verify-version-fallback uses to
# derive its expected next-target version. This catches "cut v2.9.0
# when latest is v2.7.0" typos before they become bad tags.
LATEST_GA="$(git tag --list 'v*.*.*' --sort=-v:refname \
              | grep -v '-' | head -1 || true)"
if [[ -n "$LATEST_GA" ]]; then
  if [[ ! "$LATEST_GA" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
    echo "error: latest GA tag '${LATEST_GA}' doesn't parse as vX.Y.Z" >&2
    exit 1
  fi
  L_MAJOR="${BASH_REMATCH[1]}"
  L_MINOR="${BASH_REMATCH[2]}"
  NEXT_MINOR="v${L_MAJOR}.$((L_MINOR + 1)).0"
  NEXT_MAJOR="v$((L_MAJOR + 1)).0.0"
  if [[ "$TAG" != "$NEXT_MINOR" && "$TAG" != "$NEXT_MAJOR" ]]; then
    echo "error: ${TAG} isn't the next expected GA after ${LATEST_GA}." >&2
    echo "" >&2
    echo "Expected one of:" >&2
    echo "  ${NEXT_MINOR} (next minor)" >&2
    echo "  ${NEXT_MAJOR} (next major)" >&2
    echo "" >&2
    echo "If skipping a minor is intentional, override the check by" >&2
    echo "cutting the tag manually via docs/release-process.md steps 1-4." >&2
    exit 1
  fi
  echo "  version sequencing: OK (${LATEST_GA} → ${TAG})"
fi

echo "── Preflight OK ─────────────────────────────────────────"
echo ""

TODAY="$(git log -1 --format='%cs')"  # commit-date of HEAD, YYYY-MM-DD

# The heavy lifting — parse the CHANGELOG into sections, identify what
# to fold (everything between [Unreleased] and the previous GA), merge
# bullets by kind, hoist BREAKING bullets, compose the GA entry.
#
# Kept in Python for parsing sanity; bash text-munging on this scale
# would be brittle. `cut-dev-tag.sh` uses the same pattern.
python3 - "$CHANGELOG" "$TAG" "$TODAY" <<'PY'
import re
import sys
from collections import OrderedDict

path, tag, today = sys.argv[1:]
version = tag.removeprefix("v")
src = open(path).read()

# Split into sections at `## [` headers. Keep the header WITH its body.
# The first "section" is the CHANGELOG preamble (before any `## [`).
parts = re.split(r"(?m)(?=^## \[)", src)
if not parts or not parts[0].startswith("# "):
    # No preamble; keep first split as-is.
    pass
preamble, sections = parts[0], parts[1:]

def parse_header(section):
    """Return (label, is_ga, is_unreleased). label is the raw bracketed name."""
    m = re.match(r"## \[([^\]]+)\](?: — (\d{4}-\d{2}-\d{2}))?", section)
    if not m:
        return (None, False, False)
    label = m.group(1)
    if label == "Unreleased":
        return (label, False, True)
    is_ga = bool(re.fullmatch(r"\d+\.\d+\.\d+", label))
    return (label, is_ga, False)

# Find the range to fold: [Unreleased] (index 0) plus every pre-release
# section up to but not including the previous GA. If no previous GA
# exists (fresh repo), fold everything before hitting a non-parseable
# section — the "everything since the beginning" case.
if not sections:
    sys.exit(f"error: {path} has no `## [` sections")

first_label, _, first_is_unreleased = parse_header(sections[0])
if not first_is_unreleased:
    sys.exit(f"error: expected `## [Unreleased]` as first section; found `## [{first_label}]`")

# Refuse if we'd overwrite an existing GA entry with the target version.
for s in sections:
    label, is_ga, _ = parse_header(s)
    if is_ga and label == version:
        sys.exit(f"error: `## [{version}]` already exists in {path}")

fold_end = len(sections)  # inclusive-exclusive upper bound
for i, s in enumerate(sections[1:], start=1):
    _, is_ga, _ = parse_header(s)
    if is_ga:
        fold_end = i
        break

to_fold = sections[:fold_end]  # [Unreleased] + pre-release sections
tail = sections[fold_end:]     # previous GA and everything below

# Collect bullets by kind. Preserve source order within each kind.
# Kinds appear as `#### <Name>` sub-headers inside `### Changes by Kind`.
KIND_ORDER = ["Changed", "Feature", "Bug or Regression", "Other (Cleanup)", "Documentation"]
buckets = OrderedDict((k, []) for k in KIND_ORDER)
extra_kinds = OrderedDict()   # any unexpected kind we should still keep
breaking = []                 # hoisted `**BREAKING:**` bullets (Changed)
explicit_breaking = []        # existing `### Breaking Changes` bodies
folded_meta = []              # [(label, date)] for the trailer

for section in to_fold:
    label, _, is_unreleased = parse_header(section)
    if not is_unreleased:
        # Capture the date if present so the trailer can render it.
        m = re.match(r"## \[[^\]]+\] — (\d{4}-\d{2}-\d{2})", section)
        folded_meta.append((label, m.group(1) if m else None))

    # An existing `### Breaking Changes` block (typical of a hand-written
    # [Unreleased] on a breaking-change PR) — take its bullets verbatim.
    for m in re.finditer(
        r"(?ms)^### Breaking Changes\s*\n(.*?)(?=^### |\Z)", section
    ):
        for bul in re.findall(r"(?m)^- .+(?:\n  .+)*", m.group(1)):
            explicit_breaking.append(bul.rstrip())

    # Bullets grouped under `#### <Kind>` sub-headers.
    for m in re.finditer(
        r"(?ms)^#### ([^\n]+?)\s*\n(.*?)(?=^#### |^### |^## |\Z)", section
    ):
        kind = m.group(1).strip()
        body = m.group(2)
        # Collect top-level bullets (skip italic sub-group headers like
        # `_Digest stack:_` since we can't preserve their grouping across
        # a merge — a human can re-add sub-groupings after the fold).
        for bul in re.findall(r"(?m)^- .+(?:\n  .+)*", body):
            bul = bul.rstrip()
            # Hoist `**BREAKING:**` bullets out of `#### Changed`.
            if kind == "Changed" and bul.startswith("- **BREAKING:**"):
                # Strip the marker; the section header already labels it.
                bul = "- " + bul[len("- **BREAKING:**"):].lstrip()
                breaking.append(bul)
                continue
            if kind in buckets:
                buckets[kind].append(bul)
            else:
                extra_kinds.setdefault(kind, []).append(bul)

# Compose the new [Unreleased] + [X.Y.Z] block.
lines = []
lines.append("## [Unreleased]")
lines.append("")
lines.append(f"_No unreleased changes since [{version}]._")
lines.append("")
lines.append(f"## [{version}] — {today}")
lines.append("")
lines.append(
    "<HEADLINE — replace this paragraph before committing. "
    "Write a 3-5 sentence operator-facing summary of what changed "
    f"between the previous GA and {version}: theme, top features, "
    "notable breaking changes, migration story.>"
)
lines.append("")

all_breaking = explicit_breaking + breaking
if all_breaking:
    lines.append("### Breaking Changes")
    lines.append("")
    lines.extend(all_breaking)
    lines.append("")

lines.append("### Changes by Kind")
lines.append("")
any_bullets = False
for kind in KIND_ORDER:
    bullets = buckets[kind]
    if not bullets:
        continue
    any_bullets = True
    lines.append(f"#### {kind}")
    lines.extend(bullets)
    lines.append("")
for kind, bullets in extra_kinds.items():
    any_bullets = True
    lines.append(f"#### {kind}")
    lines.extend(bullets)
    lines.append("")
if not any_bullets:
    sys.exit(
        "error: found nothing to fold — no bullets in [Unreleased] "
        "or any pre-release section between it and the previous GA. "
        "Backfill [Unreleased] first (per AGENTS.md, every merged PR "
        "should bump it), or use docs/release-process.md manually."
    )

# Pre-release history trailer — list the dev/rc tags we folded, with
# their original dates, so operators can trace the granular timeline
# back to the individual GitHub Release pages (which retain their
# historical notes).
if folded_meta:
    parts_hist = []
    for label, date in folded_meta:
        parts_hist.append(f"`v{label}` ({date})" if date else f"`v{label}`")
    lines.append(
        "_Pre-release history: cut incrementally as "
        + ", ".join(parts_hist)
        + ". The GitHub Release pages for each pre-release tag retain "
        "their historical release notes; this CHANGELOG folds them "
        "into the GA entry per Keep a Changelog convention._"
    )
    lines.append("")

new_block = "\n".join(lines)
# Emit the new block followed by the tail (previous GA + everything below).
out = preamble + new_block + "".join(tail)
open(path, "w").write(out)
PY

echo ""
echo "── diff (head) ──────────────────────────────────────────"
git --no-pager diff --stat -- "$CHANGELOG"
echo ""
git --no-pager diff -- "$CHANGELOG" | head -60
echo "..."
echo ""
echo "── EDITORIAL PASS REQUIRED ──────────────────────────────"
echo "The rewritten CHANGELOG contains a <HEADLINE — replace this ..."
echo "placeholder. Open ${CHANGELOG} and write the operator-facing"
echo "summary before committing. Optionally sub-group bullets by theme"
echo "within each kind (see v2.7.0 for the pattern)."
echo ""

# Derive the next post-release dev fallback so the printed follow-up
# matches what verify-version-fallback expects.
if [[ "$TAG" =~ ^v([0-9]+)\.([0-9]+)\.[0-9]+$ ]]; then
  NEXT_DEV="v${BASH_REMATCH[1]}.$((BASH_REMATCH[2] + 1)).0-dev"
else
  NEXT_DEV="v?.?.?-dev"
fi

echo "── next steps ───────────────────────────────────────────"
cat <<EOF
# 1. Edit ${CHANGELOG}: replace <HEADLINE — ...> with the release summary.
# 2. Commit + tag + push:
git add ${CHANGELOG} internal/version/version.go
# (also bump internal/version/version.go: Version = "${TAG}")
git commit -m "chore(release): promote [Unreleased] to [${VERSION}]"
git tag ${TAG}
git push origin main ${TAG}

# 3. Immediately follow with the next-target-dev bump per
#    docs/release-process.md step 4 (enforced by
#    dev/ci/presubmits/verify-version-fallback):
#      internal/version.Version = "${NEXT_DEV}"
git commit -am "chore: bump Version fallback to ${NEXT_DEV}"
git push origin main
EOF
