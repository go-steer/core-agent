# Self-development: core-agent works on core-agent

Status: design, unfiled. Proposed as a new box under
[#1042](https://github.com/go-steer/core-agent/issues/1042) ("proven
autonomy") — it is the same claim as the GKE drill, made against a
target we own completely and can grade exactly.

## The question

Every line of this repository was written by an agent, and none of it was
written by *this* agent. The substrate ships a built-in file suite, `bash`,
`grep`/`glob`, `todo`, `record_plan`, plan-first enforcement, Claude-format
skills, declarative subagents, hooks, compaction, checkpoints,
`autonomous.Run` with turn/token/cost/wallclock budgets, `Resume`,
`watchdog: enforce`, a per-turn cost ceiling, an approval gate, and an
Anthropic provider that can run the same frontier model the incumbent
harness runs. On paper there is nothing left to build.

So the question is not "can it?" — it is "why hasn't it?", and the answer is
mundane and checkable: **there is no `.agents/` directory in this
repository.** Every recipe we have written lives under `examples/` and
points at somebody else's domain. The substrate has never been pointed at
itself.

`docs/north-star.md` already names "Coding CLI (daily driver replacement)"
as a target and claims slash-parity is largely shipped. This document is the
smaller, falsifiable version of that ambition: not "replace the daily
driver", but "land a real PR on this repo, unattended, under budget, with
the review gate satisfied".

## What already exists vs. what is missing

Audited 2026-09-17 against the tree at `187d77e`.

| Capability the workflow needs | Status |
|---|---|
| File read/write/edit, `glob`, `grep`, `bash` | shipped (`pkg/tools`) |
| Plan-then-execute | shipped (`record_plan` + `permissions.plan_mode: required`) |
| Skeptic reviewer as a subagent | shipped (declarative subagents, `subagents[].root`) |
| Long-horizon survivability | shipped (compaction at ~85%, `mark_task_done`) |
| Unattended budgets + kill switches | shipped (`autonomous.Run`, `watchdog: enforce`, `max_turn_cost_usd`) |
| Operator steering mid-run | shipped (attach API, `/interrupt`, `/pause`, operator hold) |
| The project's operating manual as a prompt | shipped — `AGENTS.md`, 345 lines, loaded natively |
| **A recipe pointing any of it at this repo** | **missing** |
| **A sandbox for `bash`** | **missing** (`pkg/tools/bash.go` has a denylist + path scope, no sandbox) |
| **A worktree isolation primitive** | **missing** (`worktree` appears nowhere in `pkg/` or `cmd/`) |
| Local pre-push review-gate enforcement | Claude-Code-specific (`dev/claude/settings-review-gate.json`); `pkg/hooks` can host the port |

The head start is larger than the gap. `AGENTS.md` is already the prompt —
presubmit list, adversarial review gate, stacked-PR retarget order, CHANGELOG
discipline, the complexity ratchet, the `t.Setenv`/`t.Parallel` trap. It was
written for an agent to read at prompt time and core-agent loads `AGENTS.md`
as its project instruction prefix without configuration.

## Settled decisions (do not relitigate)

### D1 — The recipe is committed at `/.agents/`, not kept out of the repo

`.gitignore` already reserves the space:

```
# Per-user .agents/ state.
/.agents/sessions/
/.agents/logs/
```

Two lines that only make sense if a root `/.agents/` was always expected.

This is deliberately the opposite of the treatment `.claude/` gets three
lines above, which is ignored wholesale as "AI assistant working state —
never committed to this repo... assistant harness config is personal
tooling." For core-agent the self-development recipe is not personal
tooling. It is a product artifact under test, it must change in the same PR
as the thing it drives (add a script to `dev/ci/presubmits/` and the skill
that runs the sweep is now wrong), and it is the highest-fidelity example we
can ship — a recipe whose domain every reader already has checked out.

The counter-argument is that a committed root config is an opinion imposed on
every contributor. It is answered by D3: the live agent runs with an explicit
`--agents-dir`, so the committed tree is a *template* that nothing picks up
by accident.

### D2 — Five artifact classes, five homes

| Class | Path | Committed |
|---|---|---|
| This design doc | `docs/self-development-design.md` + bullet in `docs/README.md` | yes |
| The recipe | `/.agents/{config.json,skills/,mcp.json}` + subagent roots at `/.agents/reviewer/` | yes |
| Runtime state | `/.agents/{sessions,logs,plans}/`, eventlog DB | no — gitignored |
| The drill driver | `dev/uat/self-dev/{run.sh,README.md,scenarios/}` | yes |
| A run's scratch + the checkout it edits | `/tmp/core-agent-selfdev/<run-id>/` | n/a |
| Work products | branches → PRs against `go-steer/core-agent` | via PR |

`record_plan` writes to `.agents/plans/plan-N.md` (`pkg/config/config.go:642`)
and the eventlog needs a home; neither is in `.gitignore` today. Both get
added with the recipe, in the same PR, or the first run dirties the tree.

The recipe's own regression test **cannot** sit beside it the way
`examples/gke-platform-agent/recipe_test.go` does: the Go tool ignores
directories whose names begin with `.`, so a `.agents/recipe_test.go` is
invisible to `go test ./...`. It lives at `internal/selfrecipe/` instead and
loads `../../.agents/config.json` by relative path. Worth stating loudly —
it is the kind of thing that gets written, passes locally because someone ran
`go test ./.agents/`, and silently never runs in CI.

### D3 — Committing `/.agents/` silently re-parents every harness script

`config.Find` (`pkg/config/discovery.go:43`) walks *up* from the start
directory and returns on the **first** `.agents/` it hits. Today, from the
repo root, it finds nothing in-tree. The moment `/.agents/` is committed,
every `core-agent` invocation anywhere under this checkout that does not pin
a config inherits the self-development recipe's model, permissions, budgets
and subagents.

Measured: **zero of the 14 `dev/smoke/*.sh` scripts pass `--agents-dir` or
`-c`.** They run on pristine defaults today and would not tomorrow. Recipes
under `examples/*` that carry their own `.agents/` are safe, because Find
stops at the first hit — but that safety is incidental, not designed.

Mitigations, all three required:

1. Every `dev/smoke/*.sh` and every presubmit that shells out to the binary
   pins `--agents-dir` explicitly. A bad `--agents-dir` is fatal (exit 2) and
   never falls back, so a pin that rots fails loudly.
2. A presubmit — `dev/ci/presubmits/verify-agents-dir-pinned` — scans what
   the harness scripts *execute* for an unpinned `core-agent` invocation.
   Scan the executed command, not the file text.
3. The recipe's `display_name` is `core-agent-selfdev`, so the startup
   summary makes an accidental inheritance visible on line one.

There is a small upside hiding here. Walking up from a checkout under
`/home/<user>/projects/` reaches `~/.agents/` today, which means a
contributor with a portable user scope is *already* running this repo's
tooling under a config they forgot about. A committed root `/.agents/`
shadows that.

### D4 — Tiers 0 and 1 operate on a clone under `/tmp`, not on the real checkout

The house rule is that UAT state lives under `/tmp`, never `$HOME`. An
in-repo `/.agents/sessions/` is not `$HOME` and does not violate it — but the
stronger form is available for free here, and it buys isolation as well as
compliance: `git clone` the repo to `/tmp/core-agent-selfdev/<run-id>/repo`,
let the agent work there, push the branch to the real remote. A runaway run
damages a throwaway directory. Only at T2 does the agent touch a worktree in
the developer's actual checkout, and by then it is running under plan-first
plus an approval gate.

This also sidesteps the shared-stash hazard: a session confined to a fresh
clone has no stash stack to collide with.

### D5 — CI is the ground truth; the agent never merges

`--admin` merge is the maintainer path. The agent's terminal state is "PR
open, CI green, adversarial review section written". A human merges. This is
not a permanent rule — it is the rule until the ladder below is climbed with
evidence at each rung.

## The ladder

Each tier differs from the one below it by as few flags as possible, so a
failure localizes.

- **T0 — docs-only PR on a `/tmp` clone.** Proves: recipe loads, `AGENTS.md`
  reaches the prompt, gate behaves, `dev/ci/presubmits/verify-docs-lint`
  runs, `gh pr create` works with a real token, CHANGELOG bullet lands in the
  right subsection. No Go code, so the review-gate CI check exempts it and
  the blast radius is a paragraph.
- **T1 — bug fix with a regression test.** Adds the hard part: the test must
  be shown to **fail on the pre-fix code**. That verification is itself a
  graded step, not a claim in the PR body — patch the behaviour back with
  `// PREFIX BEHAVIOUR` markers rather than reverting files, and predict the
  failure list before running it.
- **T2 — a feature, attended.** `plan_mode: required` plus `approval_notify`.
  The operator approves the plan and the mutating calls; the agent executes.
  First tier that touches a worktree in the real checkout.
- **T3 — unattended, overnight.** `autonomous.Run` under
  `watchdog: enforce`, `max_turn_cost_usd`, `max_session_cost_usd`,
  wallclock. Reuses the shape of the A1 soak harness. Terminal state is still
  "PR open, CI green".

Grading is the part that makes this worth doing rather than just fun. A
self-development run produces a graded, ground-truthable outcome for free —
CI is a witness that cannot be talked into a pass, in exactly the way the
kubectl-shim witness grades the cluster-fact evals rather than the
transcript. Every T1 run is a candidate case for the #966 corpus, drawn from
our own incidents, which is already the stated policy for that corpus.

## Recipe shape

Sketch, following `examples/gke-platform-agent/.agents/config.json`:

```jsonc
{
  "version": 1,
  "model": { "provider": "anthropic", "name": "claude-opus-5" },
  "permissions": {
    "mode": "ask",              // T0/T1; "yolo" only inside the /tmp clone
    "plan_mode": "required"
  },
  "agent": {
    "display_name": "core-agent-selfdev",
    "max_steps": 300,
    "max_turn_cost_usd": 2.0,
    "max_session_cost_usd": 25.0
  },
  "safety": { "watchdog": "enforce" },
  "subagents": [
    {
      "name": "reviewer",
      "description": "Adversarial reviewer for a staged diff. ...",
      "root": "reviewer",
      "model": { "provider": "anthropic", "name": "claude-opus-5" },
      "tools": ["read_file", "read_many_files", "grep", "glob", "bash"]
    }
  ]
}
```

**The frontier-tier model is a requirement, not a default** — see the spike
evidence below. `claude-opus-4-5` scored 1/6 and 2/6 on the same graded task
that `claude-opus-5` scored 5/6 on. Dropping the recipe to a cheaper tier to
save budget does not degrade the output gracefully; it produces confident,
well-structured artifacts that are wrong in the direction a gate cannot see.

Skills under `/.agents/skills/`, one per ritual that `AGENTS.md` currently
describes in prose: the presubmit sweep, the adversarial review gate, the
pre-fix-failure verification, the stacked-PR retarget order, the CHANGELOG
bullet. These are not new policy — they are the existing conventions made
executable, and a divergence between a skill and `AGENTS.md` is a bug in the
skill.

One constraint carries over from the eval corpus work and applies here:
skills survive `--no-builtin-tools`, so a skill is never a safe place to put
a fact the run is supposed to discover.

## Spike evidence (2026-09-17)

Four runs of one real task, taken from this plan's own P2: write
`dev/ci/presubmits/verify-agents-dir-pinned`, the check D3 requires. Each ran
against a throwaway clone under `/tmp`, was told not to touch the smoke
scripts, and was graded afterwards by a fixed six-mutation rubric — each
mutation a single fixture dropped into `dev/smoke/`, scored on whether the
presubmit names it, never on exit code (the tree's own scripts are unpinned,
so every run exits non-zero regardless).

| arm | model | harness | `AGENTS.md` | score |
|-----|-------|---------|-------------|-------|
| B | `claude-opus-4-5` | core-agent | pre-migration | 1/6 |
| C | `claude-opus-4-5` | core-agent | migrated (439 lines) | 2/6 |
| D | `claude-opus-5` | core-agent | migrated | 5/6 |
| E | `claude-opus-5` | Claude Code | migrated | 5/6 |

**The harness is not the variable.** D and E scored identically and missed the
identical case (an invocation nested inside `bash -c`, the one mutation graded
as out-of-scope bonus). core-agent driving a frontier model produced work
indistinguishable from the tool that has built this repo to date. That is the
feasibility question answered.

**Prose in `AGENTS.md` is load-bearing, but only above a model floor.** The
B→C delta measures the memory migration alone and bought exactly one
mutation. The C→D delta is the model. An earlier reading of B/C as "written
conventions do not transfer" was an artifact of an unflagged model confound
and does not survive D.

**The convention text caused the one fatal design.** D's first attempt (killed
by an unrelated timeout) reused the existing bash-verified `shellCode` lexer
from `examples/gke-platform-agent/recipe_test.go`, reasoning correctly from
the `AGENTS.md` bullet that told it to scan execution rather than text and to
prove the scanner against real bash. That reuse would have produced a gate
that finds nothing in any script, because `shellCode` blanks quoted spans and
every harness here writes `"${CORE_AGENT}"`. The bullet has since been
corrected: a blanking scanner's safe direction depends on whether a non-match
means pass or fail, and reuse is only sound for tokens that survive the
blanking. The lesson generalises past this task — *the most principled-looking
option was the fatal one, and only running the target token through the lexer
showed it.*

**Where the two arms disagree is the interesting part, and it is not an
error.** They agree exactly on `dev/smoke`: 11 unpinned invocations in 9
scripts, same lines. The whole delta is `dev/uat/attach/run.sh`, where the
binary is assembled into a string and dispatched later:

```bash
local cmd="cd ${agent_dir} && … '${BIN}' --provider=${MODEL_PROVIDER} …"
tmux_new_window "${name}" "${cmd}"
```

D flags those two sites; E cannot see them and says so in its source, because
after lexing, the whole assignment is one word. Both calls are defensible and
they are the same question this doc's corrected scanner convention asks — *is
quoted assignment content data or code?* `recipe_test.go` already answers it
one way for a different check (`K="kubectl …"` is treated as real signal), and
E answers it the other way to avoid blinding itself on `"${CORE_AGENT}"`.

The exposure here happens to be nil — `UAT_ROOT=/tmp/core-agent-uat`, so the
`cd` lands outside the checkout and `config.Find` walks up past the root
`.agents/` entirely. But that immunity is a property of one variable's value,
not of the script's intent, and it is the same accident that protects
`dev/smoke/11-evals-corpus.sh` (whose runner execs from an `os.MkdirTemp`
world). P2 should flag both anyway and let the pin make the immunity
deliberate. Neither spike artifact is adopted as-is; the check is written by
hand with this disagreement as a known test case.

**Open blocker: both core-agent runs died mid-task** with
`anthropic: stream: context canceled` — the second in the background with no
external timeout and no budget or watchdog trip anywhere in the log. The
Claude Code arm ran to completion. Output quality is not the risk; staying
alive for the length of a task is. T3 is unattended `autonomous.Run` by
definition, so this is gating for that rung specifically, not for T0–T2.

Method note, since it applies to the next round: n=1 per arm, and D's two runs
agreed on the principle while disagreeing on the structure (shared package vs.
self-contained checker), so run-to-run variance is real and these scores carry
no error bars. The rubric is ours, written after seeing part of D's
trajectory; it tests false positives well and false negatives poorly.

## Risks

**The reviewer shares a substrate with the reviewed.** A systematic blind
spot reviews itself and passes. This is the real recursion hazard — not
self-modification, which is a non-issue because Go builds a *new* binary and
the running process is never the one being edited. Mitigation is the existing
defence in depth: CI as an independent witness, presubmits, a human merge,
and — for anything touching `pkg/tools`, `pkg/permissions`, or `pkg/watchdog`
— the incumbent harness as the reviewer rather than core-agent itself.

**The recipe rots against `AGENTS.md`.** The skills restate conventions that
live in prose. When the prose changes, the skill is silently stale. A
docs-lint rule, or better, one skill that *reads* `AGENTS.md` rather than
paraphrasing it.

**An unattended run burns budget on a loop.** Precedent exists: a
`list_agents` loop once survived stop and interrupt. `watchdog: enforce` plus
the per-turn ceiling plus the 3-in-a-row streak escalation is the answer, and
all three are shipped.

## Out of scope

- **Sandboxing `bash`.** Named as a real gap above; it is its own design and
  its own PR. T0/T1 buy safety with a `/tmp` clone instead, which is why the
  ladder starts there.
- **A worktree primitive in `pkg/`.** T2 scripts it in the drill driver. If
  parallel self-development ever becomes the point, revisit — not before.
- **Merging its own PRs.** D5.
- **Running self-development inside GitHub Actions.** Every tier runs from
  `dev/uat/self-dev/` on a developer machine under the maintainer's `gh`
  auth; "unattended" (T3) means `autonomous.Run` with no operator watching,
  not a scheduled workflow. Worth recording the trap for whoever revisits
  this: `review-gate.yml` exempts exactly two author logins,
  `github-actions[bot]` and `go-steer-bot[bot]`, and a CI-hosted run would
  have to author as one of them — so it would be exempt from the very gate
  the exercise exists to prove it can satisfy. Moving into Actions means
  first narrowing that allowlist to the two weekly jobs that need it.
- **IDE integration / LSP sidecar.** `docs/north-star.md` gap 1, unrelated.
- **Any weight updates or RL.** As with shared memory: "self-improvement" here
  is curation and tooling, nothing else.
- **Replacing the incumbent harness.** The claim is "can land a PR", not "is
  the daily driver".

## Open questions

1. **How do we tell an agent-authored PR from a human one?** The agent runs
   locally under the maintainer's `gh` auth, so its PRs are authored by the
   maintainer and are indistinguishable from hand-written ones. That is fine
   for merge policy and wrong for grading — the ladder's evidence depends on
   knowing which PRs came from a run. Cheapest option is a trailer or label
   the driver stamps; the run ID is already in the scratch path.
2. **Does T0 need its own `dev/smoke/NN-*.sh`?** The hermetic scripts run on
   mock providers; a self-development run needs a real model and a real
   remote, which points at `dev/uat/`. But a scripted-provider T0 that
   exercises the wiring without a token would be cheap CI coverage.
3. **Where does a self-development run's transcript get archived?** The
   transcript export/archival design (#887 set) is v3.0 and "transcript"
   there means three artifacts. If self-development runs are eval cases, the
   corpus needs a stable pointer to them.

## Implementation phases

- **P1** — this doc + `docs/README.md` registration. (docs-only)
- **P2** — D3 mitigations *first*: pin `--agents-dir` in the 14 smoke scripts
  and the presubmits, add `verify-agents-dir-pinned`. Lands before any
  `/.agents/` exists, so it can be verified against the current tree and
  reviewed on its own merits.
- **P3** — the recipe: `/.agents/`, the `reviewer` subagent, the skills, the
  `.gitignore` additions, `internal/selfrecipe/` test.
- **P4** — `dev/uat/self-dev/` driver + T0 executed, with the resulting PR
  linked from the UAT record.
- **P5** — T1, with the pre-fix-failure verification as a scored step.
- T2/T3 gated on T1's evidence.

P2 before P3 is the load-bearing ordering. Everything else can reorder.
