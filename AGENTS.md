# core-agent project memory

When an AGENTS.md-aware agent runs inside this repo, this file is loaded into
its system prompt as the project-level instruction prefix. It holds **rules and
pointers**. The mechanics, incident history and worked examples behind each
rule live in [`docs/contributor-pitfalls.md`](./docs/contributor-pitfalls.md)
and in the skills under [`.agents/skills/`](./.agents/skills/). Read the
pointed-to section before doing the thing it guards. Keep this file short: a
new rule earns a line here and its story goes there.

## What this project is

`core-agent` is a reusable Go agent runtime built on the Google ADK
(`google.golang.org/adk/v2`). It is the bottom layer for any Go project that
needs a multi-turn LLM agent. It provides:
- model providers;
- MCP servers and skills;
- instruction loading;
- permission gating;
- budgets and guardrails;
- telemetry;
- durable sessions;
- remote attach;
- a baseline built-in tool suite (files, shell, search, todo).

It ships an in-process TUI (via `go-steer/core-tui`) as the default TTY surface
and stays runnable headless. Product-specific tools remain a consumer concern.
It was extracted from [`go-steer/cogo`](https://github.com/go-steer/cogo) and
keeps that project's layout conventions.

## Layout

Library packages live under `pkg/`, unexported helpers under `internal/`, and
binaries under `cmd/`.

```
pkg/
  agent/            ADK agent + runner wrapper; autonomous loop; background
                    subagents; compaction, checkpoints, guardrails.
  compose/          Reusable substrate wiring (multi-session factory, MCP
                    digest, builders) lifted out of cmd/core-agent.
  instruction/      AGENTS.md / CLAUDE.md / GEMINI.md loader (AGENTS.d
                    overlay, @include, builtin: personas).
  config/           .agents/config.json schema, discovery, atomic Save.
  permissions/      ask/auto/acceptEdits/plan/allow/yolo gate, bash safety,
                    path scope, control-plane writes, plan-first.
  approver/         Model-backed approver behind permission mode `auto`.
  tools/            Built-in tool suite + GateToolset (permissions → ADK).
  mcp/  skills/     mcp.json server lifecycle; SKILL.md discovery.
  models/           Provider interface + Resolve; gemini/, anthropic/, and
                    profiles/ via go-steer/core-models. New providers and
                    adapter fixes go to core-models, not here.
  pricing/          Re-export of core-models' price catalog (rates are fixed
                    upstream, then the pin is bumped).
  usage/  modeltier/  taskclass/   Cost tracker; model-tier + task routing.
  eventlog/         Durable sessions + audit log (SQLite/Postgres/MySQL).
  transcript/       Transcript files on exit (pkg/session is its deprecated
                    alias).
  attach/  attachadapter/  auth/   HTTP/SSE remote-attach server, the agent
                    bridge onto it, and its authentication.
  childenv/         Keeps daemon credentials out of child processes.
  digest/  watchdog/  hooks/  agentenv/  recording/  telemetry/
  runner/           Headless (one-shot) + REPL drivers.
internal/           Attach client, core-tui remote adapter, web UI, evals,
                    self-recipe tests, image pins, version, testutil.
cmd/core-agent/     Reference CLI (in-process TUI by default).
cmd/core-agent-tui/ Standalone TUI (spawn-and-attach).
examples/           Library examples + recipes (examples/internal/smokeset
                    says which ones CI runs).
extras/scion/       Scion harness integration.
dev/                Build, test, lint, CI, UAT, release tooling; see
                    dev/README.md.
docs/               Internal design docs, assessments, UAT records.
docs/site/          Published Astro Starlight site.
.agents/            The self-development recipe and its skills (#1116).
```

## Build & test

```bash
dev/tools/ci             # full local CI, fast-fail order
dev/tools/build          # go build ./...
dev/tools/test-unit      # go test -race -coverprofile, all packages
dev/tools/lint-go        # golangci-lint (pinned version auto-installs)
dev/tools/fix-go-format  # gofmt + goimports
```

The default test run needs no network and no API keys. Provider-gated tests
skip cleanly without credentials.

## Conventions

- **Design before non-trivial work.** Anything bigger than a small fix gets a
  `docs/<feature>-design.md` with a "Settled decisions (do not relitigate)"
  section and an explicit "Out of scope" list, registered in
  `docs/README.md`. Give it a status line, and update that line when the work
  ships or is abandoned.
- **License headers everywhere.** Every Go, shell, YAML and Python source file
  starts with the full Apache 2.0 boilerplate attributed to Google LLC.
  `goheader` in `dev/tools/lint-go` enforces it for `.go` files. For other
  new files, run `dev/tools/add-license-headers`.
- **The complexity thresholds only go down.** `funlen` and `gocognit` in
  `dev/tools/.golangci.yml` are a ratchet pinned to the worst function in the
  tree. If your function trips it, split the function. Raising the number is
  never the fix, and neither is `//nolint`. The one exemption,
  `cmd/core-agent`'s `run` (#685), is named in the config. Don't grow it:
  reusable substrate goes in `pkg/compose` (see
  `docs/compose-extraction-design.md`), and binary-only glue goes in a named
  helper in `cmd/core-agent`, not inline in `run()`.
- **Commits.** Make them small and self-contained, with Conventional Commits
  subjects (`feat:`, `fix:`, `docs:`, `chore:`, `refactor:`, `test:`, `ci:`,
  `build:`). The body says *why* and what verification was done. Sign off
  with DCO (`git commit -s`); see [`CONTRIBUTING.md`](./CONTRIBUTING.md).
- **No agent attribution of any kind.** That means no Co-Authored-By agent
  trailer, no "Generated with" footer, no agent as author or committer, and no
  marker trailer. This applies to commits, PR titles and PR bodies. The
  required `agent attribution` check scans *every* branch commit, because a
  squash merge copies co-authors into `main`. History:
  [contributor-pitfalls § agent attribution](./docs/contributor-pitfalls.md#no-agent-attribution-the-history).
- **Tests before merging.** Every new package ships with unit tests. A feature
  without a test is not done. A bug fix needs a regression test that **fails
  on the pre-fix code** (see the adversarial review gate below).
- **Errors flow to the user.** Provider, tool and config failures never
  panic. They surface as errors through the agent loop, or as
  `core-agent: ...` lines on stderr.
- **Gate everything that consumes external state.** MCP and skill tool calls
  pass through `permissions.Gate`, so the same permission semantics apply
  everywhere. Consumers wrap their own tools with `tools.GateToolset`.
- **Tool descriptions and `jsonschema:` tags are prompt text, not code
  comments.** They carry system-prompt weight, they outrank the persona when
  the model picks a tool, and a recipe author can neither see nor override
  them. Review them as prose. Three rules (#905, #909):
  - A string argument is a writing prompt. Name what the text must *contain*
    (findings, evidence, the proposed change), never its *genre* ("summary",
    "status").
  - Write one neutral string, not interactive and headless variants. Branch
    only on what is TRUE about this build (`whenTool`, `gate.HasTool`). When
    a variant only changes what is TYPICAL, delete the example instead. To
    remove a tool for a deployment, change its registration.
  - Never state frequency ("use this generously").

  `internal/testutil.ModelFacingBans` sweeps every registered tool's
  description *and* arg schema. Add the next bad phrase there.
  `TestEveryToolRegisteringPackageHasASweep` fails any package that registers
  a tool without a sweep. It cannot tell whether your sweep covers the tools
  you register only conditionally; that part is on you (#919).
  Incidents:
  [contributor-pitfalls § tool descriptions](./docs/contributor-pitfalls.md#tool-descriptions-as-prompt-text-the-incidents).

## Pitfalls & gotchas (real ones we've hit)

- **`t.Setenv` and `t.Parallel()` don't mix.** A test that calls `t.Setenv`
  cannot call `t.Parallel()`.
- **ADK streaming needs `agent.RunConfig{StreamingMode: agent.StreamingModeSSE}`.**
  The default `StreamingModeNone` produces no `Partial` events.
- **ADK tool declarations live on `req.Config.Tools`** (`[]*genai.Tool`).
  `req.Tools` is unused by the providers.
- **Gemini function names must match `[A-Za-z0-9_]{1,64}`.** MCP tools are
  namespaced `<server>_<tool>`, never with a dot
  (`pkg/mcp/namespace.go::sanitizePrefix`).
- **The MCP SDK's `Toolset.Tools(ctx)` needs an `agent.ReadonlyContext`.** A
  stub is in `pkg/mcp/listctx.go`.
- **ADK's `telemetry.New(...)` does not install OTEL globals.** Call
  `providers.SetGlobalOtelProviders()`, as `pkg/telemetry/otel.go` does.
- **Vertex Claude model IDs sometimes need an `@VERSION` suffix** (e.g.
  `claude-opus-4-5@20251101`). Pass that form via `--model` if the bare alias
  fails.
- **In `cmd/core-agent`, anything wired before `runner.Run` takes a getter,
  never a value read off `agentRef`.** `agentRef` is nil at every wiring site,
  and nil forever on `--multi-session`. An eager read is a startup SIGSEGV
  even when the feature is off. Use `func() *agent.Agent { return agentRef }`.
  A per-session consumer needs a per-session getter. Read
  [contributor-pitfalls § getters](./docs/contributor-pitfalls.md#wiring-sites-in-cmdcore-agent-take-getters-not-agentref-values)
  before touching wiring in `main.go`.
- **To test that a shell script *calls* something, scan what the shell
  executes, not the file text.** Assert any hand-rolled lexer against real
  bash, and decide which way it may err before you write it. Read
  [contributor-pitfalls § shell scripts](./docs/contributor-pitfalls.md#testing-that-a-shell-script-calls-something)
  before writing or reusing a script-scanning check.

## How we develop

There is one long-lived branch, `main`. Work happens on short-lived branches
(`feat/…`, `fix/…`, `chore/…`, `docs/…`), each with a PR against `main`.
- **Merging.** A PR merges when branch protection's required checks are green.
  The live list:
  `gh api repos/go-steer/core-agent/branches/main/protection/required_status_checks --jq '.contexts[]'`.
  On a docs-only PR, `ci-docs.yml` emits no-op passes for `test`, `lint`,
  `go mod tidy is clean` and `govulncheck`. The other required checks run
  for real.
- **Run presubmits before every push.** Run everything in
  `dev/ci/presubmits/` except the two `e2e-*` scripts. They are the same
  scripts CI runs, so a green local sweep is the same green run. Glob the
  directory rather than trusting any hand-kept list. Skill:
  [`presubmit-sweep`](./.agents/skills/presubmit-sweep/SKILL.md).
- **A harness script that runs the binary pins its config with `-c`.**
  `config.Find` walks *up* from the cwd, so an unpinned run silently inherits
  the first `.agents/` above it. `--agents-dir` does not substitute. Use
  `pristine_config` / `${SMOKE_CONFIG}` from `dev/smoke/_common.sh`.
  Enforced by `verify-harness-config-pinned` (#1116); see
  [`dev/README.md`](./dev/README.md#the-harness-config-pin-gate).
- **Every `main.go` under `examples/` (outside `internal/` and `testdata/`,
  at any depth) is either run by CI or excluded with a reason.** Add a line
  to `examples/internal/smokeset`, named by the directory's path relative to
  `examples/`. A test there requires every example to be listed, and
  `examples-smoke` runs the runnable ones and fails on a non-zero exit
  (#852, #1417).
- **Bumping the `core-tui` pin is not just a `go.mod` edit.** Every exported
  interface in `github.com/go-steer/core-tui/tui` must be implemented with a
  `var _ coretui.X = (*T)(nil)` guard, or declined with
  `//coretui:declined X` plus a reason. This applies in
  `cmd/core-agent/coretui_guards.go` and in
  `internal/coretuiremote/guards.go`. `verify-coretui-guards` enforces it;
  see [`dev/README.md`](./dev/README.md#the-core-tui-capability-gate).
- **Adversarial review gate before every PR.** Before `gh pr create` on any
  change touching Go code:
  1. Run a skeptic subagent over the staged diff, checking correctness, races
     and API misuse, verified against real dependency source rather than
     memory.
  2. Fix or pin every finding.
  3. Record the outcome in the PR body under `## Adversarial review`.

  For bug fixes, **verify the new regression test fails on the pre-fix
  code** by patching the behaviour back. Do not revert files or check out the
  parent: a compile error is not evidence about an assertion.

  Skills:
  [`adversarial-review-gate`](./.agents/skills/adversarial-review-gate/SKILL.md)
  and
  [`prefix-failure-verification`](./.agents/skills/prefix-failure-verification/SKILL.md).

  The required `review-gate` check only greps the PR body for the phrase
  "adversarial review", so the substance is on you. Evidence that the gate
  pays: across the seven substantive PRs of the #537–#567 shutdown/resume
  train, it caught a P0/P1-class defect in nearly every first draft.
- **Rebase, don't merge.** Keep feature branches rebased on `main`.
  `git push --force-with-lease` on your own branch is normal, but a preceding
  `git fetch` disarms the lease. Never force-push `main`.
- **Stacked PRs.**
  - Retarget downstream PRs to `main` *before* merging the parent, because
    `--delete-branch` closes them.
  - After each parent lands, rebase with `--onto`.
  - After a base-update on a branch stacked on a release-promotion PR, read
    `git diff <base>...HEAD -- CHANGELOG.md`. A duplicated version heading
    merges without conflict.

  Skill: [`stacked-pr-order`](./.agents/skills/stacked-pr-order/SKILL.md).
  The hazard is explained in
  [contributor-pitfalls § stacked PRs](./docs/contributor-pitfalls.md#stacked-prs-and-the-changelog-version-heading-duplication).
- **Admin merge.** `gh pr merge <N> --admin --squash --delete-branch` is the
  maintainer path for the rebase cascade and for release commits. It is not a
  way to skip review on contributor PRs. On a Go PR, confirm that the real
  `CI` run passed: `ci-docs.yml`'s no-op jobs reuse the required check names.
- **"No checks reported" means the PR is DIRTY, not that CI is broken.**
  Check `gh pr view <N> --json mergeStateStatus` first. See
  [contributor-pitfalls § DIRTY](./docs/contributor-pitfalls.md#no-checks-reported-means-the-pr-is-dirty).
- **Squash-merged branches are not ancestors of `main`.** Prove a branch
  merged with the PR's `headRefOid` before deleting it, and check for an open
  PR first. See
  [contributor-pitfalls § branch in main](./docs/contributor-pitfalls.md#proving-a-branch-is-already-in-main-before-deleting-it).
- **UAT lives in two places.** Hermetic, CI-runnable scripts go in
  `dev/smoke/NN-*.sh` (mock providers, no credentials). Manual UAT against
  real backends goes in `dev/uat/<feature>/`. All UAT state goes under `/tmp`,
  never `$HOME`.
- **The published site moves with the code.** A user-visible change updates
  `docs/site/src/content/docs/` in the same PR. Before adding or renaming a
  user-visible surface (tool, provider, image variant, CLI flag, release), run
  `dev/tools/docs-lint`. It hard-fails the drift that has bitten before:
  numeric tool counts, spelled-out image-variant counts, pinned `@vX.Y.Z` in
  install snippets, and prose version pins for the wrong major.
- **`[Unreleased]` grows on every merged PR that ships a user-visible
  change.** Add one bullet to `## [Unreleased]` in `CHANGELOG.md`, under
  `#### Feature`, `#### Bug or Regression`, `#### Documentation`,
  `#### Other (Cleanup)` or `#### Security`, with a trailing
  `([#NNN](url))` link. Breaking changes get a `**BREAKING:**` prefix under
  `#### Changed`. Cite the issue, not the PR. Skill:
  [`changelog-bullet`](./.agents/skills/changelog-bullet/SKILL.md).
- **Which doc changes are user-visible.** Three kinds get a
  `#### Documentation` bullet:
  - the published site under `docs/site/src/content/docs/`;
  - `README.md` and the READMEs under `examples/`;
  - a change to a *rule* in `AGENTS.md`, `CONTRIBUTING.md`,
    `docs/release-process.md` or a skill under `.agents/`.

  Everything else under `docs/` and `dev/` is internal and needs no bullet of
  its own. When an internal doc records a user-visible change, the bullet
  belongs to that change. History:
  [contributor-pitfalls § user-visible docs](./docs/contributor-pitfalls.md#which-doc-changes-are-user-visible-the-history).

## How we release

The full recipe is [`docs/release-process.md`](./docs/release-process.md).
What a PR author needs:
- **SemVer.** A minor release for additive features, a patch for fixes only.
  Breaking changes need a major bump, with a one-version deprecation period
  when feasible. A major bump renames the module path to `/vN` **before any
  `vN` tag**, pre-releases included; a tag cut first can never be
  `go install`ed (#327).
- **Never hand-carve the CHANGELOG.** `dev/release/cut-dev-tag.sh` handles
  dev, rc and pre-release tags. `dev/release/cut-ga-tag.sh` folds every
  pre-release section since the last GA into one cumulative entry, hoists
  BREAKING bullets, and leaves a `<HEADLINE — ...>` placeholder for you to
  replace.
- **What the scripts do.** Both assume `[Unreleased]` is current (backfill
  from `git log` if it isn't), run preflight guards such as pricing freshness,
  edit `CHANGELOG.md`, and print the commands to finish. They do not commit,
  tag or push.
- **After a GA, bump `internal/version.Version` to `v<next-minor>.0-dev`**
  (release-process step 4). Until that lands, `verify-version-fallback`
  fails every PR. It was missed after v3.0.0 (#1351).
