# T1 — bug fix with a regression test, on a throwaway clone

You are working in a fresh clone of the `core-agent` repository. Do this
task end to end and stop when the pull request is open.

## The bug

Issue #1002: `spawn_agent` discards an acked `return_result` when the
subagent then errors, so the parent re-does the work. Read the issue
first with `gh issue view 1002`. It has the ground truth from a live run,
the cause, and a proposed fix.

In short: a subagent that has called `return_result` and been acked has
banked a real result. If the run then fails (a provider 429, say),
`completionResult` in `pkg/agent/background/tools.go` returns the error
text as the output and never reads the banked result. The parent is told
the text is incidental, so it throws the findings away and repeats the
whole delegation.

Fix it so the parent receives the banked result, learns that the run
failed after returning it, receives the run's error text, and gets
guidance that fits that case. Its
current guidance was written for a subagent that failed with nothing
banked. That behaviour is correct for that case and must not change.
The issue's proposed fix is a starting point, not a specification.

## What done looks like

- **A regression test** in `pkg/agent/background` drives a handle with
  both a returned result and a run error, and asserts that the result
  survives. Add it as a new `Test…` function, not as extra cases inside
  an existing one.
- **The test fails on the pre-fix code, and you have proved it.** Use
  the `prefix-failure-verification` skill. Predict the failure list in
  writing before you run anything. Save the complete `go test` output of
  the pre-fix run, verbatim, to `.agents/logs/prefix-failure.txt` in the
  clone. That path is gitignored and must stay uncommitted. Save plain
  `go test` output (`-v` is fine, `-json` is not). Put the prediction and
  the exact failure lines in the PR body. Afterwards, no
  `PREFIX BEHAVIOUR` marker may survive anywhere, committed or not.
  The rig checks this independently. After you finish, it runs your new
  test functions against the pre-fix production code. If they don't
  compile there, it reads your saved output instead. A test that passes
  on the buggy code fails the run. The rig also runs its own test for
  this bug, before and after your change.
- Any text the model reads (guidance strings, tool descriptions,
  `jsonschema:` tags) follows `AGENTS.md`'s rules for model-facing text.
  The package's sweep covers tool descriptions and argument schemas but
  not guidance strings, so review any guidance you write against those
  rules by hand. Extending the sweep in `internal/testutil` is out of
  scope for this task.
- If the change alters what `spawn_agent` returns, update the published
  site page that documents that result, in the same PR.
- A `CHANGELOG.md` bullet under the right subsection of
  `## [Unreleased]`, citing issue #1002. Cite the issue, not the PR.

## Constraints

- Stay in scope. Change files under `pkg/agent/`, the published site
  under `docs/`, and `CHANGELOG.md`. Nothing else: no scripts, no CI
  workflows, no `go.mod`, and not the recipe under `.agents/`.
- Run the presubmit sweep before you push. Use the `presubmit-sweep`
  skill; this change is Go, so it is the full sweep, not the docs subset.
- Run the adversarial review gate before you open the PR. Use the
  `adversarial-review-gate` skill and the `reviewer` subagent. Record the
  outcome in the PR body under `## Adversarial review`. CI's required
  `review-gate` check fails a Go PR without that section.
- Work on a branch named `fix/selfdev-<RUN_ID>`. Every `<RUN_ID>` in
  this file has already been replaced with the actual run id before you
  received it, so use the literal value you can see.
- Every commit must be DCO signed off (`git commit -s`).
- Open the pull request with `gh pr create`. Do **not** merge it. Your
  terminal state is "PR open".
- No agent attribution anywhere: no `Co-Authored-By` naming an agent, no
  "Generated with" footer, no line that marks the work as agent-authored,
  in any commit message, the PR title or the PR body. CI's required
  `agent attribution` check fails the PR otherwise.
- Don't put a CI-skip token in any commit message. GitHub then runs no
  workflow on the PR at all, and it can never go green.
