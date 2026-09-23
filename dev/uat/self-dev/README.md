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
| A7 | every changed path is under `docs/` or is `CHANGELOG.md`, committed or not, and at least one is under `docs/` |
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
satisfied, so four of the checks a T0 run turns green (`test`, `lint`,
`go mod tidy is clean`, `govulncheck`) are no-ops that ran nothing. The
rest are real — `docs-lint`, the Astro site build, `review-gate`, `go
toolchain`, `version fallback` — so this is a correct pass and a real
witness that
the PR is mergeable. What it is not is evidence that CI **compiled or
tested agent-authored code**. That only starts being true at T1, where
the change is Go.

A8's trailer answers open question 1 on #1116 — how a human later tells
an agent-authored PR from a hand-written one. It is required by the task
file and verified here, which makes it a witness rather than a hope.

## The first live run — T0, 2026-09-22

Run `20260922T134553Z-3675866`, `--tier t0 --provider anthropic-vertex`,
on `main` at `83ed64a8`. 23 turns, ~2.1M input tokens, **$3.13**, and
**3m 45s** end to end against a 3600s budget. It produced
**[PR #1146](https://github.com/go-steer/core-agent/pull/1146)** — a
branch, a signed-off commit carrying the run-id trailer, a pushed PR with
green checks, and no merge. **13 of the 14 assertions passed** (17 of the
18 scorecard lines, the other four being preflight and boot).

The answer to #1116's question is therefore yes at T0: unsupervised, from
a task file, with no human in the loop between the prompt and the pull
request.

The one FAIL was **A7**, and it was the scorecard's fault rather than the
run's. The agent wrote a `CHANGELOG.md` bullet alongside its docs change;
A7's allowlist was `^docs/` and failed it. But `.agents/AGENTS.md` routes
every user-visible change through the `changelog-bullet` skill, and the
task file itself mentioned running `verify-release-notes` "if you touched
`CHANGELOG.md`". And `AGENTS.md` listed "doc" among the user-visible
changes, with no qualifier. Read literally, the recipe required the exact
file the grader forbade.

"Read literally" matters, because practice said otherwise: only 7 of the
99 docs-only commits on main had ever carried a bullet. The text required
it, the practice skipped it, and nothing reconciled the two. The agent
followed the text. The grader had quietly encoded the practice. Neither
is wrong on its own evidence, and that's the defect: a task graded
against a rule the recipe and the repo's history disagree about is
testing whether the agent guesses the way the grader did.

The fix was to decide the rule and write it down, in `AGENTS.md`'s "Which
doc changes are user-visible": the published site, `README.md`, the
`examples/` READMEs and contributor-rule changes get a bullet, and
everything else under `docs/` and `dev/` doesn't. For internal docs that
narrows the old text. For the published site it changes practice.

A7 now permits that one path, still *requires* a `docs/` change so a run
that only filed a bullet cannot pass, and refuses every other path git
reports as changed. **An assertion may be harsher than the task it
grades; it may not be in conflict with it** — and a rig whose own
scorecard disagrees with its own recipe will keep reporting the agent's
best behaviour as a failure.

Reading #1146 as a reviewer found one more gap in the task, not in the
work. The agent's CHANGELOG bullet cites #1116, the self-development epic,
because the recipe says to cite the issue and the task named no issue
except the epic. A reader who follows that link six months from now
lands somewhere unrelated to the walk-up hazard. The agent did what it
was told with what it had, so the fix belongs in the task: **every task
from T1 up names the issue it resolves**. A bug fix has one by
definition.

Reviewing the A7 fix turned up two older holes in the same assertion, both
in how the changed-path list was *built* rather than in the allowlist
that reads it. A **rename** reports only its new path — on the range diff
and on `status --porcelain` alike — so `git mv internal/secret.go
docs/secret.go` deleted a Go file and graded as docs-only. And porcelain
**C-quotes any path containing a space**, which the old `awk '{print
$NF}'` then sliced at that space: an untracked `internal evil docs/` full
of Go files came out as the single token `docs/"` and was *accepted*,
while a legitimate `docs/my notes.md` came out as `notes.md"` and was
rejected with a message naming a file that does not exist. Fail-open and
fail-wrong from one line of parsing. Both are gone: `--no-renames -z` on
the diff, and `-z -uall` through a real parser that splits rename entries
into both of their paths. An allowlist is only as good as the list it is
handed.

Two things the run is *not* evidence for. A12's green is docs-lint plus
`ci-docs.yml` stubs (see above), so no agent-authored code was compiled
by CI — that starts at T1. And T0 by construction has no test to make
fail first, so nothing here says the agent can do the pre-fix
verification the repo's review gate demands.

## Reading a result

`run.sh` writes `scorecard.txt` next to the log. Every line is `PASS`,
`FAIL <what> — <why>` or `SKIP`, and the script exits non-zero if any
assertion failed. A FAIL names the artifact that disagreed, so the first
thing to do is open it — not re-run.
