# T0 — docs-only change on a throwaway clone

You are working in a fresh clone of the `core-agent` repository. Do this
task end to end and stop when the pull request is open.

## The change

`docs/site/src/content/docs/reference/configuration.md` explains that
`core-agent` walks up from the working directory to find `.agents/`, and
documents `-c` as a way to point at a specific file. What it never says is
that the walk-up is a **hazard**: a `.agents/` directory anywhere *above*
your working directory is picked up silently, with no error and no prompt,
and it brings a different model, different permissions, different budgets
and a different tool surface with it. A script that runs the binary from a
subdirectory of a project that has a `.agents/` at its root is running
under that project's recipe whether it meant to or not.

Add a short section to that page which:

- states the hazard plainly, in terms of what the reader would observe
  (their run silently uses someone else's model and permission mode, and
  nothing says so);
- tells them how to see which config actually loaded — the startup summary
  line that ends in `(via .agents/ discovery)`;
- tells them the fix: pin `-c <path>` when a run must not inherit;
- notes that `--agents-dir` is **not** a substitute, because the config is
  loaded by discovery before that flag is applied, so it moves the skills
  and sessions while leaving the model, permissions and budgets behind.

Put it where a reader hunting this problem will find it, and match the
page's existing voice and formatting. Do not restructure the page.

## Constraints

- **Docs only.** Do not change any `.go` file, any script, or any test.
- Keep it short. A reader should get the hazard, the diagnostic and the
  fix without scrolling.
- Run `dev/ci/presubmits/verify-docs-lint` before you push. If you changed
  nothing outside `docs/`, that plus `verify-release-notes` (if you touched
  `CHANGELOG.md`) is the relevant sweep.
- Work on a branch named `docs/selfdev-<RUN_ID>`. Every `<RUN_ID>` in
  this file has already been replaced with the actual run id before you
  received it, so use the literal value you can see.
- The commit must be DCO signed off (`git commit -s`) and must carry this
  trailer as its own line at the end of the message:

  ```
  Self-Development-Run: <RUN_ID>
  ```

  This is how a human later tells an agent-authored PR from a hand-written
  one. It is not optional.
- Open the pull request with `gh pr create`. Do **not** merge it. Your
  terminal state is "PR open".
- No Claude attribution anywhere — no `Co-Authored-By`, no "Generated
  with" footer, in the commit message or the PR body.
