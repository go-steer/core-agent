---
name: stacked-pr-order
description: Land a stack of dependent PRs in core-agent without closing the downstream ones. Use when a branch is based on another feature branch rather than main, or when merging a parent whose children are still open. Covers the retarget-before-merge order, rebasing after each parent lands, recovery, and reading merge state correctly.
---

# Stacked PR order

When `feat/B` depends on `feat/A`, base PR B on branch A. Three things go
wrong, in this order of likelihood.

## 1. Retarget downstream PRs to `main` BEFORE merging the parent

`gh pr merge A --delete-branch` **closes any PR whose base was branch A.**
Deleting a head branch closes the PRs pointed at it, and that is not
reversible by re-merging.

```bash
gh pr edit B --base main     # first
gh pr merge A --admin --squash --delete-branch   # then
```

**Recovery if you forget:** push the parent SHA back to re-create the branch,
`gh pr reopen B`, `gh pr edit B --base main`.

Before deleting any remote branch, check for an open PR pointed at it.

## 2. Rebase the downstream onto new main after each parent lands

```bash
git rebase --onto origin/main <old-parent-sha>
```

The `--onto` form is the point: the parent landed as a *squash*, so its
commits are not ancestors of main and a plain rebase replays them on top of
their own squashed copy. You get conflicts against your own change.

`git push --force-with-lease` on your own branch is normal here. **A
preceding `git fetch` disarms it** — it compares against the remote-tracking
ref the fetch just updated, so it will happily overwrite someone else's push.
Use the explicit form: `--force-with-lease=<ref>:<sha>`.

## 3. The CHANGELOG duplication

A base-update on a branch stacked on a release-promotion PR duplicates the
version heading with no conflict. See the `changelog-bullet` skill — it is
the same hazard and the check is `git diff <base>...HEAD -- CHANGELOG.md`.

## Reading merge state

**"No checks reported" means the PR is DIRTY, not that CI is broken.** GitHub
cannot compute a merge commit for a conflicting PR, so `pull_request`
workflows never queue at all.

```bash
gh pr view <N> --json mergeStateStatus
```

| Status | Means |
|---|---|
| `DIRTY` | conflicts; no checks will ever run. Rebase and force-push, they fire immediately |
| `BEHIND` | stale but not conflicting; `--admin` ignores it |
| `BLOCKED` | still running |
| `CLEAN` | ready |
| `UNKNOWN` | recomputing; re-poll in ~20s |

Straight after a push it is just registration lag — wait 30s before
concluding anything. `main` moves faster than a full CI cycle, so a green PR
can flip back to `BEHIND` before you look; do not chase it with repeated
rebases.

## Proving a branch is already in main before deleting it

Squash-merging means a merged branch is **not** an ancestor of main, so
`git branch -d` refuses it and `merge-base --is-ancestor` says "unmerged".
Cheapest first:

1. `gh pr view N --json headRefOid,mergeCommit` — if the branch tip still
   equals `headRefOid` and the merge commit is an ancestor of main, the whole
   branch was inside the merged diff.
2. Subject match + `git cherry`, which can only ever corroborate.
3. For a still-unmatched commit, a **two-dot diff scoped to exactly the files
   that commit touched**. Both unscoped forms are wrong: plain two-dot is
   drowned by everything main gained since, and three-dot
   (`merge-base..branch`) reports what the branch *added*, which is true
   whether or not main later took it.

## Merging

`gh pr merge <N> --admin --squash --delete-branch` is the maintainer path for
the rebase-then-merge cascade. **Never merge unverified CI** — confirm
`mergeStateStatus: CLEAN` and zero failed checks first. `--admin` is not a
way to skip review on a contributor PR.

If `--delete-branch` fails with *"'main' is already used by worktree"*, the
**merge still succeeded** — only the local checkout step failed. Verify with
`gh pr view <N> --json state` and delete the branch with
`git push origin --delete <branch>`.
