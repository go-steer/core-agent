---
name: adversarial-review-gate
description: Run the mandatory skeptic review over a staged diff and record it in the PR body. Use before `gh pr create` on any change touching Go code in core-agent. Covers what to delegate, what the reviewer needs to be told, how to disposition findings, and the `## Adversarial review` section the required CI check looks for.
---

# Adversarial review gate

Before `gh pr create` on any change touching Go code: a skeptic reviews the
staged diff, every finding is fixed or pinned, and the outcome is recorded in
the PR body under an `## Adversarial review` heading.

This is not advisory. The `review-gate` CI check is **required** and a
Go-touching PR fails without the section. (Docs-only PRs and PRs authored by
`go-steer-bot[bot]` are exempt.)

## Run it

Stage first — the reviewer reads `git diff --cached`, so anything unstaged is
invisible to it and will reach the PR unreviewed.

```bash
git add <the files>
git status --short   # confirm nothing you meant to include is still ` M`
```

Then delegate to the `reviewer` subagent. **Tell it three things**, because a
reviewer that has to infer intent grades style instead of correctness:

1. the issue number and what the change is meant to do,
2. the specific things you are least sure about — the race you talked
   yourself out of, the guard you are not certain actually guards,
3. that it should verify against real dependency source rather than memory,
   and mark each finding CONFIRMED or PLAUSIBLE.

Ask it explicitly not to manufacture findings. A clean verdict with a list of
what was checked is a real result.

## Disposition every finding

Every one gets fixed or pinned — pinned meaning you argue in writing why it
is not a defect, or why it is a known limit you are accepting. Silently
dropping a finding is the failure mode this gate exists to prevent.

Do not take the reviewer at face value. It is another model and it is
sometimes wrong, sometimes confidently. Check the claim before you act on
it; if it is wrong, say so in the disposition rather than changing code to
satisfy it.

Findings about **comments and docs the diff makes false** are real findings,
including in files the diff does not touch. Fix those.

## Record it

A table is the readable form: finding, and what you did about it.

```markdown
## Adversarial review

Skeptic subagent over the staged diff. **No correctness defects** — `go
build`, `go vet`, the package suite and `-race` all clean. N findings, all
addressed:

| # | Finding | Disposition |
|---|---------|-------------|
| 1 | … | **Fixed.** … |
| 2 | … | **Pinned as a known limit** — … |
```

Then, if the change was a bug fix, the pre-fix failure evidence (see
`prefix-failure-verification`), and if it added a guard, the mutation
evidence. Both belong in the same PR body.

Close with what the review explicitly cleared. The things most worth doubting
— the race, the nil path, the vacuous-test question — are worth a sentence
each even when the answer was "fine", because that is the part a future
reader cannot reconstruct.

## Evidence it pays

The #537–#567 shutdown/resume train shipped seven substantive PRs and this
gate caught a P0/P1-class defect in nearly every first draft. On #1137 it
caught an allowlist entry scoped package-wide when it was documented as
applying to one file — a hole wide enough to drive the original bug back
through.
