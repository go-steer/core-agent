# Cleanup execution plan (2026-07-25 code-review sweep)

Captured 2026-07-25 after a full-codebase review. Tracks how the four
`Cleanup: *` milestones get executed in parallel by multiple agents
without a rebase pileup on the hot files. Prune once the milestones
close.

## The constraint that shapes everything

The bottleneck is **file contention**, not agent capacity. The serious
findings cluster in a few hot files — `pkg/agent/agent.go`,
`pkg/agent/autonomous.go`, `pkg/permissions/gate.go`,
`pkg/models/anthropic/convert.go`, `cmd/core-agent/main.go`. Spawning one
agent per issue would produce constant rebase conflicts there. So work is
organized by **conflict domain** (one lane = one owner = one worktree),
lanes run in parallel, and PRs **stack within a lane**.

## Automode suitability

- **Auto (~28):** clear fix + writable regression test, disjoint files.
  Safe for an unattended agent with the guardrails below.
- **Assisted (~12):** agent writes the code, but a human makes one
  policy/behavior decision first.
- **Human-led (~4):** architectural / API-shape. Interactive,
  design-doc-first. Do NOT automate: `pkg/compose` extraction, the
  `pkg/agent` god-package split, and the two default-behavior security
  calls (default `read_only` allow-bundle, symlink-scope policy).
- **Blocked (1):** k8s-dep drop waits on the k8s-lookout watcher move.

## Lanes

Each lane is one worktree/owner. Lanes A–H run concurrently (disjoint
file sets); PRs stack within a lane.

| Lane | Scope (files) | Milestone(s) |
|---|---|---|
| A — eventlog | `pkg/eventlog/*` | Correctness |
| B — anthropic | `pkg/models/anthropic/{convert,stream}.go` | Correctness |
| C — cost/usage | `pkg/usage`, `internal/pricing`, `modeltier`, `taskclass` | Correctness |
| D — agent loop | `pkg/agent/{agent,autonomous,compactor,subagent,background_spawn,watchdog,cost_ceiling,resume}.go` | Correctness |
| E — gate/tools security | `pkg/permissions/*`, `pkg/tools/{file,fetch}.go`, `pkg/hooks` | Security |
| F — attach | `pkg/attach/*` | Security + Structure |
| G — config/env | `pkg/config`, `pkg/agentenv`, CLI drift in `cmd/` | Structure + Hygiene |
| H — docs/deps | `docs/`, `go.mod` | Hygiene |
| I — tests | `*_test.go` | Hygiene |

**Cross-lane hazard:** the autonomous usage double-count fix edits
`autonomous.go`, so it belongs to **Lane D**, not the cost lane, even
though it is conceptually a cost bug.

## Wave sequencing

1. **Wave 1 (parallel):** all Auto issues across lanes A–H.
2. **Wave 2 (after a policy huddle):** the Assisted issues — decisions
   batched to the maintainer, then lane agents implement.
3. **Wave 3 (human-led, last):** `pkg/compose` extraction and the
   god-package split, as design-doc-first PRs. Last on purpose so they
   rebase once over the settled tree instead of forcing in-flight PRs to
   chase a moving layout.

## Guardrails (every automode agent)

- One worktree per lane; feature branch `fix/…`. Stack downstream PRs on
  the parent branch; **retarget to `main` before merging the parent**.
- **Regression test required** — a fix without a test is not done.
- Run `dev/ci/presubmits/*` before every push. DCO sign-off
  (`git commit -s`). No `Co-Authored-By`.
- One `CHANGELOG.md` `[Unreleased]` bullet per user-visible change; run
  `dev/tools/docs-lint`.
- Strict scope: fix only the assigned issue; discoveries become new
  issues, not scope creep.
- Rebase on `main` after each sibling merges; wait for the four required
  checks green; then admin-squash-merge.
- Agents do **not** self-merge default-behavior security changes — those
  route through maintainer sign-off (Wave 2).
