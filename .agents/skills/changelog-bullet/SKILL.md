---
name: changelog-bullet
description: Write the CHANGELOG entry for a core-agent PR. Use whenever a change is user-visible — a feature, bug fix, doc change, breaking change or cleanup. Covers which subsection, citing the issue rather than the PR, what a bullet has to contain, and the stacked-release-branch hazard that silently files a bullet under a shipped version.
---

# The CHANGELOG bullet

Every merged PR with a user-visible change adds one bullet under
`## [Unreleased]` in `CHANGELOG.md`, **as part of the PR itself**. Both
release scripts assume `[Unreleased]` is current; if it is stale at tag time
someone has to backfill it from `git log`, which produces a worse entry than
the person who made the change would have.

## Which subsection

`#### Feature` · `#### Bug or Regression` · `#### Documentation` ·
`#### Other (Cleanup)` · `#### Security`

Breaking changes go under `#### Changed` with a `**BREAKING:**` prefix, so
`cut-ga-tag.sh` can hoist them into a `### Breaking Changes` section
automatically at tag time.

## Cite the issue, not the PR

```markdown
([#1137](https://github.com/go-steer/core-agent/issues/1137))
```

The issue is where the problem is described and where the discussion lives.
The PR number is recoverable from git; the issue number is the one a reader
six months out actually wants.

## What a bullet has to contain

These are long here, deliberately. The bullet is the durable record of *why*,
and it is read by people who will not open the PR.

- **Lead with the defect in the user's terms**, bolded — what was broken and
  who it hurt, not which function changed.
- **The mechanism.** Why it happened, specifically enough that the reader
  could have predicted it.
- **The decision and the alternative you rejected.** This is the part that
  stops the same trade-off being relitigated in six months.
- **What is deliberately *not* changed**, and why. A reader who finds the old
  behaviour somewhere should be able to tell it was left on purpose.
- **Corrections to things previously written down.** If the change overturns
  an earlier decision or makes an earlier entry's claim false, say so in this
  bullet. Do not edit the old entry — it is a record of what was believed
  then.

Numbers and names beat adjectives: "nine sites" rather than "several", the
actual error string rather than "it failed".

## The hazard worth memorizing

**A base-update on a branch stacked on a release-promotion PR silently
duplicates the CHANGELOG version heading.** Both sides carry the identical
`## [Unreleased]` → `## [X.Y.Z] — DATE` rename, git resolves two identical
insertions **without a conflict**, and your bullet ends up filed under a
release that was tagged from a commit demonstrably lacking it.

No check catches this: `verify-release-notes` runs against fixtures, not the
repo's `CHANGELOG.md`, and both it and `verify-docs-lint` pass on the
duplicated file.

After any base-update on such a branch:

```bash
git diff <base>...HEAD -- CHANGELOG.md   # three-dot, not the merge exit code
grep -nE '^## \[' CHANGELOG.md | head
```

Observed on #993.

## Also

Run `dev/ci/presubmits/verify-release-notes` before pushing. If the change
touches a user-visible surface, the Astro site under
`docs/site/src/content/docs/` walks in the **same PR**, not as a follow-up.
