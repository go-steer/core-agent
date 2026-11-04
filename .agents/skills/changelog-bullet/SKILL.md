---
name: changelog-bullet
description: Write the CHANGELOG entry for a core-agent PR. Use whenever a change is user-visible — a feature, bug fix, published-doc or contributor-rule change, breaking change or cleanup. Covers which subsection, citing the issue rather than the PR, what a bullet has to contain, and the stacked-release-branch hazard that silently files a bullet under a shipped version.
---

# The CHANGELOG bullet

Every merged PR with a user-visible change adds one bullet under
`## [Unreleased]` in `CHANGELOG.md`, **as part of the PR itself**. Both
release scripts assume `[Unreleased]` is current; if it is stale at tag time
someone has to backfill it from `git log`, which produces a worse entry than
the person who made the change would have.

## Does a doc change need one?

Yes for three kinds: the published site under `docs/site/src/content/docs/`;
`README.md` and the READMEs under `examples/`; and a change to a *rule* in
`AGENTS.md`, `CONTRIBUTING.md`, `docs/release-process.md` or a skill under
`.agents/`. All three go under `#### Documentation`.

No for everything else under `docs/` and `dev/`. Design docs, assessments,
UAT write-ups and friction logs are internal. When an internal doc records a
user-visible change, the bullet belongs to that change, not to the doc.

This is `AGENTS.md`'s "Which doc changes are user-visible" rule, and if the
two ever disagree, `AGENTS.md` wins. Don't infer the rule from
`CHANGELOG.md`'s history. Before the rule, docs-only PRs almost never
carried a bullet, even though the text said they should.

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

## What a bullet has to contain (<= 300 characters)

Each `[Unreleased]` bullet is a single line capped at **300 characters**
(enforced by `dev/tools/verify-release-notes`), ending with a
`([#NNN](https://github.com/go-steer/core-agent/issues/NNN))` link. The
long-form rationale, rejected alternatives, and incident history belong in
the PR body or `docs/`, so `CHANGELOG.md` and the GitHub Release body stay
readable and well under GitHub's 125 KB release-body limit (#1387).

- **Lead with what changed in the user's terms**, bolded, naming the affected
  package or surface.
- **State the concrete effect or fix** in one concise sentence.
- **End with the issue link** `([#NNN](url))`.

## The hazard worth memorizing

**A base-update on a branch stacked on a release-promotion PR silently
duplicates the CHANGELOG version heading.** Both sides carry the identical
`## [Unreleased]` → `## [X.Y.Z] — DATE` rename, git resolves two identical
insertions **without a conflict**, and your bullet ends up filed under a
release that was tagged from a commit demonstrably lacking it.

No check catches the duplicate version heading automatically:
`verify-release-notes` checks `[Unreleased]` bullets on `CHANGELOG.md`, not
duplicate version headings, and `verify-docs-lint` passes on the duplicated
file.

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
