# The self-development UAT

Can core-agent, running the recipe this repository commits for itself,
do a real unit of development work on this repository — unsupervised,
end to end, and stop at a pull request a human can judge?

That is [#1116](https://github.com/go-steer/core-agent/issues/1116). The
design is [`docs/self-development-design.md`](../../../docs/self-development-design.md);
the recipe is [`.agents/`](../../../.agents) and shipped in P3
([PR #1143](https://github.com/go-steer/core-agent/pull/1143)). Up to
that point the recipe had unit tests proving its *shape* — that the
config parses, that the skills exist, that the reviewer is configured.
Nothing had ever run it.

This rig runs it.

## The shape of the thing

```
  the real checkout                 the scratch dir
  ───────────────────               ───────────────────────────────
  build the binary  ─────────────▶  bin/core-agent
  fingerprint HEAD                  ┌──────────────────────────┐
  + working tree                    │ repo/  (a fresh clone)   │
         │                          │   .agents/  ← the recipe │
         │                          │   -c pinned at it        │
         │                          │   --yolo, D4-scoped      │
         │                          └──────────────────────────┘
         │                                      │
         │                                      ▼
         │                            a branch, a commit,
         │                            a pushed PR on GitHub
         ▼                                      │
  fingerprint again ◀───── A9 ──────────────────┘
  (it must not have moved)
```

Two properties carry the whole design, and both are assertions rather
than intentions:

- **The agent never touches the real checkout.** It works in a throwaway
  clone under `${SELFDEV_SCRATCH}` (under `/tmp`). A9 fingerprints the
  real checkout before and after and fails if anything moved. The
  fingerprint covers **ignored** paths as well as tracked ones, hashes
  `.agents/` by content, and hashes `.git/refs` and `.git/config` —
  because the single most likely way a run touches the real checkout is
  the walk-up hazard reaching the real `.agents/` and `record_plan`
  writing a plan into it, and every one of those writes is invisible to
  `git status --porcelain` alone. That is D4 enforced, not D4 promised.
- **The agent never merges.** CI is the ground truth and a human is the
  merge decision. The terminal state is "PR open, checks green"; A11 and
  A12 grade exactly that and nothing past it.

## Running it

```bash
# A boot-only rehearsal: no model spend, no PR. Verifies the recipe
# loads from the clone and the binary starts under it.
dev/uat/self-dev/run.sh --dry-run

# The real thing. Spends money and opens a pull request.
dev/uat/self-dev/run.sh --tier t0 --provider anthropic-vertex
```

| flag | env | default | what it does |
|---|---|---|---|
| `--tier` | `SELFDEV_TIER` | `t0` | which task file under `tasks/` to run |
| `--provider` | `SELFDEV_PROVIDER` | recipe's | overrides the parent model's provider |
| `--dry-run` | — | off | boot on `--provider=echo`, assert the recipe loads, stop |
| `--keep` | — | off | keep a clean dry run's scratch dir (every other run keeps it anyway) |
| — | `SELFDEV_SCRATCH` | `${TMPDIR:-/tmp}/core-agent-selfdev` | where the clone and the binary go |
| — | `SELFDEV_REMOTE` | `origin`'s URL | what to clone |
| — | `SELFDEV_BASE` | `main` | the branch to clone and base the PR on |
| — | `SELFDEV_TIMEOUT` | `3600` | wallclock seconds before the run is killed |

Only a clean `--dry-run` cleans up after itself. Any failure, and any
run that actually drove a model, **keeps its scratch dir** — the run
log, the plan artifact and the clone's git history are how you find out
what the agent did, and throwing them away on the one run you need to
read is the wrong default.

## The tiers

From the design doc's ladder. Only T0 has a task file today; each rung
is gated on the one below producing a PR that CI passed and a human
merged.

| tier | the work | why it is the rung it is |
|---|---|---|
| **T0** | a documentation change | no build, no test, no API. If the agent cannot branch, commit, push and open a PR, nothing above matters. |
| **T1** | a bug fix with a regression test | the first rung where the agent must make a test *fail* before making it pass. |
| **T2** | a small feature behind a config field | design surface, not just repair. |
| **T3** | a change spanning the agent loop | the rung where a bad change is expensive. |

## What is graded

Fourteen assertions, written against artifacts the agent does not author —
the clone's git history, the PR as GitHub sees it, the plan file on
disk, and the real checkout's fingerprint. An agent can claim in prose
that it ran the presubmits; it cannot fabricate a green check run.

| # | assertion |
|---|---|
| A1 | the config the run loaded came from the clone, not the real checkout |
| A2 | both `AGENTS.md` files were discovered |
| A3 | all five skills were discovered |
| A4 | the `reviewer` subagent was configured |
| A4b | the reviewer declares **no** model of its own (see below) |
| A5 | a plan artifact exists — `plan_mode: required` was honoured |
| A6 | a real branch exists, committed, and present on the remote |
| A7 | every changed path is under `docs/`, committed or not |
| A8 | **every** commit carries the `Self-Development-Run: <RUN_ID>` trailer |
| A8b | **every** commit is DCO signed off |
| A9 | the real checkout did not move — tracked, ignored, or under `.git/` |
| A10 | no Claude attribution in the commit message or the PR body |
| A11 | the PR is open |
| A12 | every check reached a passing conclusion and the PR is not blocked |

A4b looks like a nitpick and is not. A subagent's own `model` block
survives `--provider`, because `resolveSubagentProvider` shallow-copies
the parent config and overwrites `Model` wholesale — so a provider
pinned there makes the entire recipe exit 2 at startup for every
operator who is not on first-party Anthropic, including the Vertex
setup this repository's own maintainer runs. P3 shipped that bug and
this rig is what caught it. `internal/selfrecipe` guards it statically;
A4b is the same property observed at run time.

**What A12 is and is not.** `ci.yml` skips markdown-only changes and
`ci-docs.yml` emits matching green stubs so the required checks stay
satisfied, so the green a T0 run earns is docs-lint plus a set of stubs
— not the full pipeline. It is a correct pass and a real witness that
the PR is mergeable; it is not evidence that CI exercised agent-authored
code. That only starts being true at T1, where the change is Go.

A8's trailer answers open question 1 on #1116 — how a human later tells
an agent-authored PR from a hand-written one. It is required by the task
file and verified here, which makes it a witness rather than a hope.

## Reading a result

`run.sh` writes `scorecard.txt` next to the log. Every line is `PASS`,
`FAIL <what> — <why>` or `SKIP`, and the script exits non-zero if any
assertion failed. A FAIL names the artifact that disagreed, so the first
thing to do is open it — not re-run.
