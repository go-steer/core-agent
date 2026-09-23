# core-agent project memory

When an AGENTS.md-aware agent runs inside this repo, this file is
loaded into the agent's system prompt as the project-level
instruction prefix. Keep it short and load-bearing.

## What this project is

`core-agent` is a reusable Go-based agent built on the Google ADK
(`google.golang.org/adk`). It's the bottom layer for any project that
needs a multi-turn LLM agent in Go — model providers, MCP servers,
skills, instruction loading, permission gating, telemetry, durable
sessions, remote attach, and a baseline built-in tool suite
(files/shell/search/todo). It ships an in-process Bubble Tea TUI as
the default TTY surface (via `go-steer/core-tui`) but stays runnable
headless; product-specific tools and rich slash-command frameworks
remain a consumer concern.

It deliberately mirrors the structure and conventions of
[`go-steer/cogo`](https://github.com/go-steer/cogo), the project it
was extracted from.

## Layout

Library packages live under `pkg/`; unexported helpers under
`internal/`; runnable binaries under `cmd/`.

```
pkg/
  agent/              ADK llmagent + runner wrapper; autonomous loop;
                      background subagents; compaction/checkpoints.
  instruction/        AGENTS.md / CLAUDE.md / GEMINI.md loader
                      (+ AGENTS.d overlay + @include; scoped).
  config/             .agents/config.json schema + discovery + atomic Save.
  permissions/        ask/accept-edits/plan/yolo gate + bash denylist
                      + path scope + plan-first enforcement.
  tools/              Built-in tool suite + GateToolset wrapper
                      (bridges permissions to ADK toolsets).
  mcp/                mcp.json schema + stdio/HTTP server lifecycle.
  skills/             SKILL.md discovery → ADK skilltoolset.
  models/             Provider interface + registry/Resolve;
                      gemini/ (API + Vertex), anthropic/ (native
                      model.LLM adapter, api.anthropic.com + Vertex).
  usage/              Per-turn token + cost tracker.
  modeltier/,         Model-tier + task-class routing tables.
  taskclass/
  eventlog/           Durable sessions + audit log (SQLite/Postgres/MySQL).
  session/            Transcript persistence (.agents/sessions/).
  digest/             Oversized-tool-output summarization.
  attach/             HTTP/SSE remote-attach server.
  agentenv/, auth/,   Env interpolation; attach auth; hooks; watchdog;
  hooks/, watchdog/,  session replay recording.
  recording/
  telemetry/          OpenTelemetry + Prometheus setup.
  runner/             Headless (one-shot) + REPL (multi-turn) drivers.
internal/             pricing catalog, attach client, core-tui remote
                      adapter, web UI, vertex cache, version, testutil.
cmd/core-agent/       Reference CLI binary (default in-process TUI).
cmd/core-agent-tui/   Standalone TUI binary (spawn-and-attach).
                      (k8s-event-watcher moved to go-steer/k8s-lookout.)
examples/             Library use examples.
extras/scion/         Scion harness integration.
dev/                  Build/test/lint tooling — see dev/README.md.
docs/                 Internal design docs (acceptance-mN.md, ...).
docs/site/            Published Astro Starlight site.
```

## Build & test

```bash
dev/tools/ci          # full local CI in fast-fail order
dev/tools/build       # go build ./...
dev/tools/test-unit   # go test -race -coverprofile, all packages
dev/tools/lint-go     # golangci-lint (auto-installs v2.12.1)
dev/tools/fix-go-format  # auto-fix gofmt + goimports
```

Provider-gated tests (e.g. `models/anthropic/vertex_test.go::TestResolve_AnthropicVertex_FromConfig`)
skip cleanly when their creds aren't present. The default test run
needs no network and no API keys.

## Conventions

- **Plan before non-trivial work.** Milestones are designed in plan
  mode; each lands as one or a few focused commits with an
  `acceptance-mN.md` plan written first.
- **License headers everywhere.** The full Apache 2.0 boilerplate
  attributed to Google LLC sits at the top of every Go / shell /
  YAML / Python source file. The `goheader` linter inside
  `dev/tools/lint-go` enforces this on `.go` files; for new shell /
  YAML / Python files, run `dev/tools/add-license-headers` (idempotent;
  also normalizes any older SPDX-style headers to the canonical form).
- **The complexity thresholds only go down.** `dev/tools/lint-go`
  runs `funlen` and `gocognit` as a *ratchet*: each threshold in
  `dev/tools/.golangci.yml` is pinned to the worst function in the tree
  on the day it was set, so it forbids nothing that exists and only
  catches something new that is worse. If your function trips it,
  split the function — raising the number is never the fix, and the
  one exemption (`cmd/core-agent`'s `run`, tracked in #685) is named
  in the config rather than scattered as `//nolint`.
- **Small, self-contained commits with informative bodies.** Subject
  lines follow Conventional Commits (`feat:`, `fix:`, `docs:`,
  `chore:`, `refactor:`, `test:`, `ci:`, `build:`). Bodies explain
  *why* and call out the verification done.
- **No Co-Authored-By trailer.** Maintainer preference — author the
  work under your own name. DCO sign-off (`git commit -s`) is the
  expected practice; see [`CONTRIBUTING.md`](./CONTRIBUTING.md).
- **Tests before merging.** Every new package ships with unit tests.
  A new feature without a test is not done. A new bug fix without a
  regression test makes it easy for the bug to come back.
- **Errors flow to the user.** Provider / tool / config failures
  never panic — they surface as errors returned through the agent
  loop or as `core-agent: ...` lines on stderr.
- **Gate everything that consumes external state.** MCP and skill
  tool calls all pass through `permissions.Gate` so the same
  `ask` / `allow` / `yolo` semantics apply uniformly. Consumers that
  add their own tools should wrap them with `tools.GateToolset`.
- **Tool descriptions and `jsonschema:` tags are prompt text, not
  code comments.** They are system-prompt-weight, they outrank the
  persona at the moment the model picks a tool, and a recipe author
  can neither see nor override them. Review them as prose. Three
  rules, learned the expensive way (#905, #909):
  - **A string-typed argument is a writing prompt.** Name what the
    text must *contain* — findings, evidence, the proposed change —
    never what *genre* of document it is. A genre name drags a whole
    rhetorical mode with it ("summary" implies retrospection,
    "status" implies a state machine), and the model honours that
    mode over the persona, because satisfying the schema is a
    precondition for the call succeeding at all.
    `mark_task_done`'s `"one-paragraph completion summary"` — the arg
    tag, not the description — is what shaped the visible output in
    the deployment it broke.
  - **Write one neutral string; don't write interactive and headless
    variants.** There is no mode bit to branch on: the session that
    motivated #909 was a headless daemon with an operator attached
    over the TUI, i.e. both at once. Branch when the branch changes
    what is TRUE about this build (`whenTool`, `gate.HasTool`,
    `sciontoolOnPath`); when it only changes what is TYPICAL, delete
    the example instead. Where a deployment genuinely wants a tool
    gone, change *registration* — an absent tool is unambiguous and
    testable, a mode-varying string is invisible to the recipe author.
  - **Never state frequency.** "Use this generously" sets a policy the
    persona cannot see or countermand.

  `internal/testutil.ModelFacingBans` is the enforcement: a sweep runs
  over every registered tool's description *and* arg schema in
  `pkg/tools`, `pkg/tools/agentic`, `pkg/tools/alert`, `pkg/tools/peer`,
  `pkg/agent`, `pkg/agent/background` and `pkg/agent/autonomous`. Add a
  phrase there when you find the next one.

  Registering a tool in a package with no sweep fails
  `TestEveryToolRegisteringPackageHasASweep`
  (`internal/testutil/modelfacing_coverage_test.go`) — it walks the tree
  for `functiontool.New(` and requires a matching sweep beside it. The
  list above used to be maintained by hand and was wrong within one
  release (#919): `mark_task_done`, the tool the whole thread started
  from, was the one tool nothing checked. A sweep also has to cover its
  own package's *conditionally* registered tools — `alert` is gated on a
  live target and sat outside a sweep that believed it was inside one.

## Pitfalls & gotchas (real ones we've hit)

- **`t.Setenv` and `t.Parallel()` don't mix** in Go's testing package.
  We hit this writing `models/anthropic/vertex_test.go`; tests that
  call `t.Setenv` cannot also call `t.Parallel()`.
- **ADK streaming requires `agent.RunConfig{StreamingMode: agent.StreamingModeSSE}`.**
  The default `StreamingModeNone` produces no `Partial` events.
- **ADK's `req.Tools` field is unused by the existing Gemini provider** —
  real tool declarations live on `req.Config.Tools` (`[]*genai.Tool`,
  each with `FunctionDeclarations`). The Anthropic adapter follows
  the same convention.
- **Anthropic's Vertex SDK panics on missing creds.**
  `vertex.WithGoogleAuth` calls `panic` when ADC isn't loadable. We
  load credentials explicitly via `google.FindDefaultCredentials` and
  pass them to `vertex.WithCredentials` so we surface a clean error
  instead.
- **Anthropic separates the system prompt** from messages — it's a
  top-level `System []TextBlockParam` field, not a role on the first
  message. The adapter's `convert.go` extracts it from
  `genai.GenerateContentConfig.SystemInstruction` and lifts it.
- **Vertex Claude model IDs sometimes carry `@VERSION` suffixes**
  (e.g. `claude-opus-4-5@20251101`). Bare aliases often work; if not,
  pass the date-suffixed form via `--model`.
- **Gemini function names must match `[A-Za-z0-9_]{1,64}`** — no dots
  in MCP tool namespaces; we use `<server>_<tool>` not
  `<server>.<tool>`. See `pkg/mcp/namespace.go::sanitizePrefix`.
- **The MCP SDK's `Toolset.Tools(ctx)` requires an
  `agent.ReadonlyContext`**, not a regular `context.Context`. There's
  a minimal stub at `pkg/mcp/listctx.go`.
- **ADK's `telemetry.New(...)` returns providers but does NOT install
  them as OTEL globals.** Always call
  `providers.SetGlobalOtelProviders()`. `pkg/telemetry/otel.go` does this.
- **In `cmd/core-agent`, anything wired before `runner.Run` must take
  a getter, never a value read off `agentRef`.** `agent.New` fires
  inside `runner.Run`, several hundred lines below the attach / broker
  / tool wiring, so `agentRef` is nil at every one of those sites — and
  on a `--multi-session` daemon it is nil *forever*, because there is
  no primary agent and `POST /sessions` builds each one on demand.
  Reading `agentRef.SessionID()` at a wiring site is not a blank field,
  it is a **SIGSEGV at startup before the daemon serves anything**, and
  it fires even when the feature that wanted the value is switched off.
  The idiom in that file is `func() *agent.Agent { return agentRef }`.
  Where an API can, make the getter the only form. This shipped once
  (#647) and only CI's e2e caught it: every component was individually
  correct and the defect was in *when* a value was read, which no unit
  test in the affected packages can see.
- **Testing that a shell script CALLS something: scan what the shell
  executes, not the file text.** A regexp over a script's text cannot
  tell a call from a mention — a usage block is the natural place to
  write `require_x || exit 1`, so a script can ship with the guard
  present only as help text and a text match agrees it is guarded.
  Anchoring to column zero does not help; heredoc bodies are usually
  unindented. Blank the comments, quoted spans and heredoc bodies
  (preserving line numbers) and match against that. If you hand-roll
  that lexer, **assert its soundness against real bash** rather than
  arguing it in a doc comment. Which direction is the safe one depends
  on what a *non-match* means for your check, so work it out before
  picking: for a check that fails when it cannot find something (the
  #1105 shape — "this script must call the guard"), blanking real code
  is safe, because the check only gets stricter and fails loudly. For a
  check that fails when it *does* find something — a violation scanner
  — the directions invert: over-blanking hides violations and the gate
  passes silently forever, which is the fatal direction. State in the
  source which shape your check is and which way it is allowed to err.
  **This also decides whether you can reuse an existing lexer.** A
  blanking scanner is only sound for tokens that survive its blanking:
  `shellCode` in `examples/gke-platform-agent/recipe_test.go` blanks
  quoted spans, so `"${CORE_AGENT}" --provider=gemini -p "hi"` comes
  back as `                --provider=gemini -p     ` — the command
  word is gone. Reusing it for a check that hunts a quoted command word
  yields a gate that finds nothing in any script, and every harness in
  this tree quotes that word. `recipe_test.go` already knew: it matches
  mutating verbs against comments-stripped text and *not* against
  `shellCode()`, because `K="kubectl …"` is real signal. Before reusing
  a lexer, run your target token through it and read the output.
  Run each self-test case through bash with a stub
  that prints a sentinel, and treat "scanner kept it but the shell
  never ran it" as an unconditional failure. Constructs that broke a
  plausible-looking scanner: `<<<` (needs a *backward* check or the
  scan lands on the second `<`), `<<""`, `$'…\'…'`, `$((1 << 2))`,
  `<<\EOF`. And a mutation suite must assert the **expected failure
  message**, not merely a non-zero exit, and verify the mutation
  actually changed the tree — three mutations once failed for the wrong
  reason (pointing at the wrong file) and only the message assertion
  could see it. Learned over six adversarial rounds on #1105/PR #1109.

## How we develop

Single long-lived branch: `main`. Work happens on short-lived feature
branches (`feat/...`, `fix/...`, `chore/...`, `docs/...`) → PR
against `main` → merge once CI's four required status checks are
green. Branch protection on `main` requires `test`, `lint`,
`go mod tidy is clean`, and `govulncheck`; docs-only PRs satisfy
these via the companion `ci-docs.yml` workflow without running the
full Go pipeline. Commits are DCO-signed off (`git commit -s`) and
follow Conventional Commits — see [`CONTRIBUTING.md`](./CONTRIBUTING.md)
for the full contributor flow + DCO walkthrough.

Conventions worth knowing at agent prompt time:

- **Run presubmits before every push.** `dev/ci/presubmits/*` are the
  same scripts CI runs. A green local run is the same green run as
  remote CI — skipping them ships preventable red builds. Full sweep:
  `dev/ci/presubmits/{build,lint-go,test-unit,verify-go-format,verify-mod-tidy,vet,verify-vuln,verify-go-toolchain,verify-coretui-guards,verify-harness-config-pinned,examples-smoke}`.
- **A harness script that runs the binary pins its config with `-c`.**
  `config.Find` walks *up* from the process cwd, so an unpinned run
  anywhere under the checkout silently inherits the first `.agents/`
  above it — a different model, a different permission mode, a
  `tools.disable` that removes the tool under test, and no error.
  `--agents-dir` does **not** substitute: the config is loaded by
  discovery before that flag is applied, so it moves the skills and
  sessions and leaves the model and permissions behind. The smoke
  scripts have `pristine_config` / `${SMOKE_CONFIG}` in `_common.sh` for
  this; `dev/ci/presubmits/verify-harness-config-pinned` enforces it and
  `--print` lists every site, including the ones built as strings and
  dispatched through tmux (#1116). See
  [`dev/README.md`](./dev/README.md#the-harness-config-pin-gate).
- **An example under `examples/` is either run by CI or excluded with a
  reason.** `dev/ci/presubmits/examples-smoke` builds and runs every
  program `examples/internal/smokeset` marks runnable and fails on any
  non-zero exit; the companion test there requires every
  `examples/*/main.go` to be in that manifest, so adding an example
  means adding a line. `--print` dumps the disposition table. It exists
  because `go build ./...` proved the examples compiled while
  `parallel-spawn` sat broken for months advertising "Exits 0" (#852).
- **Bumping the `core-tui` pin is not just a `go.mod` edit.** Every
  exported interface in `github.com/go-steer/core-tui/tui` must be
  accounted for by both TUI hosts — implemented and pinned with a
  `var _ coretui.X = (*T)(nil)` guard, or declined with a
  `//coretui:declined X` directive plus the reason, in
  `cmd/core-agent/coretui_guards.go` (local `--tui`) and
  `internal/coretuiremote/guards.go` (attach mode). Nowhere else.
  `dev/ci/presubmits/verify-coretui-guards` enforces it against the
  pinned version and `--print` dumps the interface × adapter matrix;
  see [`dev/README.md`](./dev/README.md#the-core-tui-capability-gate).
  The guards catch a changed method signature at compile time; the gate
  catches the capability that appeared and nobody noticed, which is
  silent otherwise because core-tui feature-detects by type assertion.
- **Adversarial review gate before every PR.** Before `gh pr create`
  on any change touching Go code: run a skeptic subagent over the
  staged diff (correctness, races, API misuse — verified against
  real dependency source, not memory), fix or pin every finding, and
  record the outcome in the PR body under an `## Adversarial review`
  heading. For bug fixes, additionally **verify the new regression
  test FAILS on the pre-fix code** — a test that passes on the buggy
  code is documentation, not a gate; this exact failure shipped in a
  downstream release.

  **Do not do that by reverting the production files or checking out
  the parent commit.** Once the fix introduced a new const, type or
  signature, the test no longer *compiles* against pre-fix sources,
  and a compile error is not evidence about an assertion. The method
  that works (used on #935, #974, #1036): copy each production file
  aside, then patch the *behaviour* back inside the new code, leaving
  every new symbol in place so the package still builds — silence
  "declared and not used" with `_ = theSymbol` and mark each site
  `// PREFIX BEHAVIOUR: ...`. Run the suite, record the exact failure
  lines for the PR verbatim, restore from the copies, then
  `grep -rn "PREFIX BEHAVIOUR" pkg/` to prove zero markers survived (a
  test-cache hit on the re-run is good evidence the restore was
  byte-identical). **Predict the failure list before running it** — a
  pure-function or boundary test that legitimately passes both ways is
  fine, but declare it in the PR as "passes before and after, by
  design" rather than quietly omitting it. Enforced by this convention
  plus the
  `review-gate` **required** CI check (Go-touching PRs fail without
  the section; docs-only PRs and PRs authored by `go-steer-bot[bot]`
  — the weekly `pricing-regen` and `lookout-pin-check` jobs — exempt).
  Optionally, copy `dev/claude/settings-review-gate.json` to your
  local `.claude/settings.json` for a Claude Code hook that blocks
  `gh pr create` at the terminal before CI ever sees it. Evidence it
  pays: the #537–#567 shutdown/resume train shipped seven substantive
  PRs and the gate caught a P0/P1-class defect in nearly every first
  draft.
- **Rebase, don't merge.** Feature branches stay rebased on `main`.
  `git push --force-with-lease` on your own branches is normal;
  never force-push `main`.
- **Stacked PRs.** When `feat/B` depends on `feat/A`, base PR B on
  branch A. Three gotchas worth memorizing:
  - **Retarget downstream PRs to `main` BEFORE merging the parent.**
    `gh pr merge A --delete-branch` closes any PR whose base was
    branch A. Edit base first (`gh pr edit B --base main`), then
    merge A. Recovery if you forget: push the parent SHA back to
    re-create the branch, `gh pr reopen`, `gh pr edit --base main`.
  - **Rebase the downstream onto new main after each parent lands**
    (`git rebase --onto origin/main <old-parent-sha>`) to skip the
    squashed-and-now-on-main commit from the downstream's history.
  - **A base-update on a branch stacked on a release-promotion PR
    silently duplicates the CHANGELOG version heading.** Both sides
    carry the identical `## [Unreleased]` → `## [X.Y.Z-pre] — DATE`
    rename, git resolves two identical insertions **without a
    conflict**, and the stacked PR's bullet ends up filed under a
    release that was already tagged from a commit demonstrably lacking
    it. No check catches this: `verify-release-notes` runs against
    fixtures, not the repo's `CHANGELOG.md`, and both it and
    `verify-docs-lint` pass on the duplicated file. After any
    base-update on such a branch, read
    `git diff <base>...HEAD -- CHANGELOG.md` — the three-dot diff, not
    the merge's exit code. One-line check:
    `grep -nE '^## \[' CHANGELOG.md | head`. Observed on #993.
- **Admin merge protocol.** `gh pr merge <N> --admin --squash --delete-branch`
  is the maintainer path for the rebase-then-merge cascade above
  and for landing release commits. **Not** a way to skip review on
  contributor PRs — that requires actual review.
- **"No checks reported" means the PR is DIRTY, not that CI is
  broken.** GitHub cannot compute a merge commit for a conflicting
  PR, so `pull_request`-triggered workflows never queue at all. Check
  `gh pr view <N> --json mergeStateStatus` **first**, before reading
  workflow files: `DIRTY` = conflicts, no checks will ever run (rebase
  and force-push, they fire immediately); `BEHIND` = stale but not
  conflicting (`--admin` ignores it); `BLOCKED` = still running;
  `UNKNOWN` = recomputing, re-poll in ~20s. Straight after a push it
  is just registration lag — wait 30s before concluding anything.
  Related: `main` moves faster than a full CI cycle, so a green PR can
  flip back to `BEHIND` before you look; don't chase it with repeated
  rebases. And **`--force-with-lease` is disarmed by a preceding
  `git fetch`** — it compares against the remote-tracking ref the
  fetch just updated, so it will happily overwrite someone else's
  push.
- **Proving a branch is already in `main` before deleting it.**
  Squash-merging means a merged branch is *not* an ancestor of main,
  so `git branch -d` refuses it and `merge-base --is-ancestor` says
  "unmerged". Cheapest-first: (1) `gh pr view N --json
  headRefOid,mergeCommit` — if the branch tip still equals
  `headRefOid` and the merge commit is an ancestor of main, the whole
  branch was inside the merged diff; (2) subject match + `git cherry`,
  which can only ever corroborate; (3) for a still-unmatched commit, a
  **two-dot diff scoped to exactly the files that commit touched**.
  Both unscoped forms are wrong here — plain two-dot is drowned by
  everything main gained since, and three-dot (`merge-base..branch`)
  reports what the branch *added*, which is true whether or not main
  later took it. Check for an OPEN PR before deleting a remote branch:
  deleting the head branch closes it.
- **Design docs before non-trivial work.** Anything bigger than a
  small fix gets a `docs/<feature>-design.md` with a "Settled
  decisions (do not relitigate)" section + explicit "Out of scope"
  list. Register the doc in `docs/README.md`'s feature-designs list.
  Settled-decisions framing keeps follow-up reviews from
  re-relitigating the same trade-offs.
- **UAT lives in two places.** `dev/smoke/NN-*.sh` for hermetic
  CI-runnable shell scripts (mock providers, no creds). For real
  manual UAT against real backends, `dev/uat/<feature>/` holds a
  richer driver (typically a `run.sh` + tmux + fixtures + a README
  walking numbered scenarios). All UAT state goes under `/tmp`,
  never `$HOME`.
- **Astro site walks alongside README/DESIGN.** User-visible changes
  update the published site at `docs/site/src/content/docs/` in the
  same PR as the code, not as a follow-up. Before opening a PR that
  adds or renames a user-visible surface (tool, provider, image
  variant, CLI flag, release), run `dev/tools/docs-lint` — it
  hard-fails on the small set of drift patterns that have actually
  bitten us before (numeric tool counts, spelled-out image-variant
  counts, pinned `@vX.Y.Z` in install snippets, wrong-major prose
  version pins).
- **`[Unreleased]` grows on every merged PR.** Any user-visible change
  (new feature, bugfix, breaking change, or one of the doc changes
  listed under "Which doc changes are user-visible" below) adds one
  bullet under the appropriate `#### Feature` / `#### Bug or
  Regression` / `#### Documentation` / `#### Other (Cleanup)` /
  `#### Security` subsection of `## [Unreleased]` in `CHANGELOG.md` as
  part of the PR itself. Breaking changes get a `**BREAKING:**` prefix
  under `#### Changed` so the release scripts can hoist them
  automatically into a `### Breaking Changes` section at tag time.
  Both `dev/release/cut-dev-tag.sh` and `dev/release/cut-ga-tag.sh`
  assume `[Unreleased]` is current — if it's stale at tag time,
  backfill from `git log` before tagging.
- **Which doc changes are user-visible.** Three kinds get a bullet
  under `#### Documentation`: the published site under
  `docs/site/src/content/docs/`; `README.md` and the READMEs under
  `examples/`; and a change to a *rule* in `AGENTS.md`,
  `CONTRIBUTING.md`, `docs/release-process.md` or a skill under
  `.agents/`, because contributors and agents both work from those.
  Everything else under `docs/` and `dev/` is internal and needs no
  bullet of its own. That includes design docs, assessments, UAT
  write-ups and friction logs; `docs/README.md` already calls that
  tree contributor reference. When an internal doc records a
  user-visible change, the bullet belongs to that change, not to the
  doc. Before this rule, `AGENTS.md` said every doc change gets a
  bullet, and practice almost never did that: 7 of 99 docs-only
  commits on main carried one. So this narrows the text for internal
  docs and changes practice for the published site. The first
  self-development run
  ([#1116](https://github.com/go-steer/core-agent/issues/1116)) is
  what exposed the gap. It followed the text, and its grader had
  encoded the practice.

## How we release

SemVer: minor bump (`vX.Y.0`) for additive features, patch (`vX.Y.Z`)
for fixes only. Breaking changes go through a `vX+1.0.0` bump with a
one-version deprecation period when feasible. Full mechanical recipe
in `docs/release-process.md` — this section covers only what an agent
authoring PRs needs to know.

Every merged PR that ships a user-visible change adds one bullet to
`## [Unreleased]` in `CHANGELOG.md`, under the right `#### Feature` /
`#### Bug or Regression` / `#### Documentation` / `#### Other (Cleanup)` /
`#### Security` subsection, with a trailing `([#NNN](url))` link.
Breaking changes get a `**BREAKING:**` prefix under `#### Changed`.
Both tag-cut scripts assume `[Unreleased]` is current — if it's stale
at tag time, backfill from `git log` before tagging.

Two scripts under `dev/release/` do the CHANGELOG carve — **use one,
do NOT hand-carve**:

- **`cut-dev-tag.sh vX.Y.Z-<pre>`** — for dev / rc / pre-release
  tags. Renames `## [Unreleased]` → `## [X.Y.Z-pre] — YYYY-MM-DD`
  and reseeds an empty `## [Unreleased]` above it. Pre-release tags
  also auto-fall-back to `[Unreleased]` + a synthesized PR list if
  their specific section doesn't exist, so most dev tags don't need
  a per-tag CHANGELOG edit at all.
- **`cut-ga-tag.sh vX.Y.Z`** — for GA tags. Folds every pre-release
  section between `## [Unreleased]` and the previous GA into a
  **cumulative** `## [X.Y.Z]` entry (the "since last GA" story an
  operator upgrading from `vX.(Y-1).0` needs, not just what
  accumulated since the last dev tag), hoists `**BREAKING:**`
  bullets to a `### Breaking Changes` section, and deletes the
  folded pre-release sections. Leaves a `<HEADLINE — ...>`
  placeholder for the operator-facing summary; replace it before
  committing. This exists because v2.7.0's first-cut GA notes only
  covered ~5 post-dev.5 bullets and had to be rewritten by hand —
  don't repeat that.

Both scripts run release-time preflight guards (pricing catalog
freshness), edit `CHANGELOG.md` in place, print the git commands to
finish the cut, and do NOT commit / tag / push themselves.

The README doesn't hard-code a "current release" pin any more — the
release-shield badge picks up the latest tag automatically. Nothing to
bump there at release time.
