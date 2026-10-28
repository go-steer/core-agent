# Contributor pitfalls: the long form behind AGENTS.md

[`AGENTS.md`](../AGENTS.md) is loaded into every agent's system prompt, so it
holds the rules and a one-line pointer here. This file holds the full
mechanics, the incident history and the worked examples. Read the section
named in the pointer before doing the thing it guards.

Nothing here is a separate policy. If this file and `AGENTS.md` disagree,
`AGENTS.md` wins and this file has a bug.

---

## Wiring sites in `cmd/core-agent` take getters, not `agentRef` values

`agent.New` fires inside `runner.Run`, several hundred lines below the attach,
broker and tool wiring. So `agentRef` is nil at every one of those sites. On a
`--multi-session` daemon it is nil *forever*, because there is no primary
agent and `POST /sessions` builds each one on demand.

Reading `agentRef.SessionID()` at a wiring site does not produce a blank
field. It produces a **SIGSEGV at startup, before the daemon serves
anything**, and it fires even when the feature that wanted the value is
switched off.

The idiom in that file is `func() *agent.Agent { return agentRef }`. Where an
API can, make the getter the only form.

This shipped once (#647), and only CI's e2e caught it. Every component was
individually correct. The defect was in *when* a value was read, which no unit
test in the affected packages can see.

A getter can still bind the wrong agent. A per-session consumer (a
`POST /sessions` tenant) needs a per-session getter, not the global
`agentRef`; see #1362.

---

## Testing that a shell script *calls* something

**Scan what the shell executes, not the file text.** A regexp over a script's
text cannot tell a call from a mention. A usage block is the natural place to
write `require_x || exit 1`, so a script can ship with the guard present only
as help text, and a text match agrees it is guarded. Anchoring to column zero
does not help, because heredoc bodies are usually unindented.

**What to match against.** Blank the comments, quoted spans and heredoc bodies,
preserving line numbers, and match against that. If you hand-roll that lexer,
**assert its soundness against real bash** rather than arguing it in a doc
comment.

**Which direction is safe depends on what a non-match means for your check.**
Work it out before you pick:
- **The check fails when it cannot find something.** This is the #1105 shape:
  "this script must call the guard". Blanking real code is safe here, because
  the check only gets stricter and fails loudly.
- **The check fails when it does find something**, i.e. a violation scanner.
  The directions invert. Over-blanking hides violations and the gate passes
  silently forever, which is the fatal direction.

State in the source which shape your check is and which way it is allowed to
err.

**This also decides whether you can reuse an existing lexer.** A blanking
scanner is only sound for tokens that survive its blanking.
- `shellCode` in `examples/gke-platform-agent/recipe_test.go` blanks quoted
  spans. So `"${CORE_AGENT}" --provider=gemini -p "hi"` comes back as
  `                --provider=gemini -p     `, with the command word gone.
- Reusing it for a check that hunts a quoted command word yields a gate that
  finds nothing in any script, and every harness in this tree quotes that
  word.
- `recipe_test.go` already knew this. It matches mutating verbs against
  comments-stripped text and *not* against `shellCode()`, because
  `K="kubectl …"` is real signal.

Before reusing a lexer, run your target token through it and read the output.

**Self-test the lexer against bash.**
- Run each self-test case through bash with a stub that prints a sentinel.
- Treat "the scanner kept it but the shell never ran it" as an unconditional
  failure.
- Constructs that broke a plausible-looking scanner:
  - `<<<` needs a *backward* check, or the scan lands on the second `<`.
  - `<<""`
  - `$'…\'…'`
  - `$((1 << 2))`
  - `<<\EOF`

**Mutation suites.** A mutation suite must assert the **expected failure
message**, not merely a non-zero exit. It must also verify that the mutation
actually changed the tree. Three mutations once failed for the wrong reason,
pointing at the wrong file, and only the message assertion could see it.

This was learned over six adversarial rounds on #1105 / PR #1109.

---

## Proving a branch is already in `main` before deleting it

Squash-merging means a merged branch is *not* an ancestor of `main`. So
`git branch -d` refuses it, and `merge-base --is-ancestor` says "unmerged".

Checks, cheapest first:

1. Run `gh pr view N --json headRefOid,mergeCommit`. If the branch tip still
   equals `headRefOid` and the merge commit is an ancestor of `main`, the whole
   branch was inside the merged diff.
2. Subject match plus `git cherry`. This can only ever corroborate.
3. For a commit that is still unmatched, use a **two-dot diff scoped to exactly
   the files that commit touched**.

Both unscoped forms are wrong here:
- A plain two-dot diff is drowned by everything `main` gained since.
- A three-dot diff (`merge-base..branch`) reports what the branch *added*,
  which is true whether or not `main` later took it.

Check for an OPEN PR before deleting a remote branch, because deleting a PR's
head branch closes the PR.

---

## Stacked PRs and the CHANGELOG version-heading duplication

The ordinary stacked-PR procedure is in the
[`stacked-pr-order`](../.agents/skills/stacked-pr-order/SKILL.md) skill. The
hazard that needs the long form is this one.

**What happens.** A base-update on a branch stacked on a release-promotion PR
silently duplicates the CHANGELOG version heading.
- Both sides carry the identical `## [Unreleased]` → `## [X.Y.Z-pre] — DATE`
  rename, and git resolves two identical insertions **without a conflict**.
- The stacked PR's bullet ends up filed under a release that was already
  tagged from a commit that demonstrably lacks it.
- No check catches this. `verify-release-notes` runs against fixtures, not the
  repo's `CHANGELOG.md`, and both it and `verify-docs-lint` pass on the
  duplicated file.

**What to do.** After any base-update on such a branch, read
`git diff <base>...HEAD -- CHANGELOG.md` (the three-dot diff), not the merge's
exit code. A one-line check is `grep -nE '^## \[' CHANGELOG.md | head`.

Observed on #993.

---

## "No checks reported" means the PR is DIRTY

GitHub cannot compute a merge commit for a conflicting PR, so
`pull_request`-triggered workflows never queue at all. Check
`gh pr view <N> --json mergeStateStatus` **first**, before reading workflow
files:

| `mergeStateStatus` | Meaning |
|---|---|
| `DIRTY` | Conflicts. No checks will ever run. Rebase and force-push, and they fire immediately. |
| `BEHIND` | Stale but not conflicting. `--admin` ignores it. |
| `BLOCKED` | Still running. |
| `UNKNOWN` | Recomputing. Re-poll in about 20s. |

Straight after a push, missing checks are just registration lag; wait 30s
before concluding anything.

**Related hazards**
- `main` moves faster than a full CI cycle, so a green PR can flip back to
  `BEHIND` before you look. Don't chase it with repeated rebases.
- **A preceding `git fetch` disarms `--force-with-lease`.** The lease compares
  against the remote-tracking ref that the fetch just updated, so it will
  happily overwrite someone else's push.
- **The docs-only workflow reuses the required check names.** `ci-docs.yml`
  runs no-op jobs under the same names as the required checks, so on a
  Go-touching PR a green check list is not proof that the real `CI` run
  passed. Confirm the `CI` run's conclusion (`gh run list --branch <b>`)
  before an `--admin` merge (#1271).

---

## Adversarial review: why the gate exists, and the pre-fix method

The procedures are in two skills:
- [`adversarial-review-gate`](../.agents/skills/adversarial-review-gate/SKILL.md)
- [`prefix-failure-verification`](../.agents/skills/prefix-failure-verification/SKILL.md)

**Why reverting doesn't work.** Once the fix introduced a new const, type or
signature, the test no longer *compiles* against pre-fix sources, and a compile
error is not evidence about an assertion.

**The method that works** (used on #935, #974 and #1036):
1. Copy each production file aside.
2. Patch the *behaviour* back inside the new code, leaving every new symbol in
   place so the package still builds. Silence "declared and not used" with
   `_ = theSymbol`, and mark each site `// PREFIX BEHAVIOUR: ...`.
3. Run the suite and record the exact failure lines for the PR, verbatim.
4. Restore from the copies.
5. Run `grep -rn "PREFIX BEHAVIOUR" pkg/` to prove zero markers survived. A
   test-cache hit on the re-run is good evidence that the restore was
   byte-identical.

**Predict the failure list before running it.** A pure-function or boundary
test that legitimately passes both ways is fine. Declare it in the PR as
"passes before and after, by design" rather than quietly omitting it.

**What `review-gate` checks.** The required check looks only for the phrase
"adversarial review" in the PR body of a Go-touching PR. Docs-only PRs are
exempt, and so are PRs authored by the automation bots (`go-steer-bot[bot]`,
the GitHub App behind the weekly `lookout-pin-check` job, and
`github-actions[bot]`). It is a reminder that the section exists, not a
judge of its content (#1385).

`dev/claude/settings-review-gate.json` is an optional Claude Code hook that
blocks `gh pr create` at the terminal when the section is missing.

---

## No agent attribution: the history

The rule rules out all of the following, in commits and in the PR title and
body:
- an agent co-author;
- a "Generated with" footer;
- an agent as commit author or committer;
- any trailer that marks work as agent-authored.

**The PR check is the gate.**
`dev/ci/presubmits/verify-no-agent-attribution` scans *every* commit on the
branch, because a squash merge copies each branch commit's co-authors into
`main`. One such commit (#931) is how Claude ended up on this repository's
contributor list.

**There is a backstop, but it can't block.** An `--admin` merge, a direct push
and an edited squash message can all land a commit the PR check never judged.
So `agent-attribution-main.yml` re-scans each push to `main`, runs a daily
sweep of the last three days for pushes that skipped CI, and opens an issue on
a hit. It can only make a hit loud, so don't count on it.

---

## Tool descriptions as prompt text: the incidents

**#905.** `mark_task_done`'s `"one-paragraph completion summary"` shaped the
visible output in the deployment it broke. The culprit was the arg tag, not
the description. A genre name drags a whole rhetorical mode with it:
"summary" implies retrospection, and "status" implies a state machine. The
model honours that mode over the persona, because satisfying the schema is a
precondition for the call succeeding at all.

**#909.** The session that motivated this was a headless daemon with an
operator attached over the TUI, i.e. both interactive and headless at once.
There is no mode bit to branch on. The rule that follows:
- Branch when the branch changes what is TRUE about this build (`whenTool`,
  `gate.HasTool`, `sciontoolOnPath`).
- When it only changes what is TYPICAL, delete the example instead.
- Where a deployment genuinely wants a tool gone, change *registration*. An
  absent tool is unambiguous and testable; a mode-varying string is invisible
  to the recipe author.

**#919.** The list of packages with a `ModelFacingBans` sweep used to be
maintained by hand, and it was wrong within one release. `mark_task_done`, the
tool the whole thread started from, was the one tool nothing checked.
`TestEveryToolRegisteringPackageHasASweep` replaced the list. A sweep also has
to cover its own package's *conditionally* registered tools. `alert` is gated
on a live target, and it sat outside a sweep that believed it was inside one.

---

## "Which doc changes are user-visible": the history

Before this rule, `AGENTS.md` said every doc change gets a CHANGELOG bullet.
Practice almost never did that: 7 of 99 docs-only commits on `main` carried
one. So the rule narrowed the text for internal docs and changed practice for
the published site.

The first self-development run (#1116) exposed the gap. It followed the text,
and its grader had encoded the practice.

---

## Retired pitfalls

These were in `AGENTS.md` and no longer apply to this repository. The
provider adapters moved to `go-steer/core-models` with #1296, so the Anthropic
pitfalls now live there.
- **Anthropic's Vertex SDK panics on missing creds.** `vertex.WithGoogleAuth`
  panics when ADC can't be loaded. The workaround was to load credentials
  explicitly and pass them via `vertex.WithCredentials`.
- **Anthropic separates the system prompt.** It is a top-level
  `System []TextBlockParam` field, which the adapter lifted out of
  `genai.GenerateContentConfig.SystemInstruction`.

They stay here so the blog post that quotes them
(`docs/site/.../blog/working-with-coding-assistants.md`) has a referent.
