---
title: Working with a coding assistant, one project later
description: The same loop we settled into on an earlier project, run over two months on core-agent. What we added, what broke, and where the assistant led us the wrong way for a month.
template: doc
tableOfContents: true
sidebar:
  order: 2
---

On an earlier project — a Go-native terminal coding agent the same pair built in the weeks before core-agent — we wrote up the loop we'd settled into working with a coding assistant: plan, implement, smoke test, refine, memorize. That loop is still the loop. It doesn't change when the project gets bigger.

What changes is everything *around* the loop.

This post is about what we changed working on core-agent, a Go-native agent runtime that went from a fresh commit on May 14 to v2.7.0 GA on July 20. Same pair as before — one developer, one coding assistant (Claude Code, mostly). This time it was 357 commits, roughly 50k lines of Go, across as many as nine concurrent worktrees, and — importantly — not on a straight-line cadence. Nothing in the loop itself was new. Everything in the machinery around the loop was, including what happens *between* the coding bursts.

## The setup at a glance

- **Timeline:** 2026-05-14 → 2026-07-20 (~68 calendar days from first commit to GA).
- **Volume:** 357 commits, ~50k lines of Go excluding tests.
- **Cadence:** two coding bursts — mid-May through mid-June (~245 commits over 5 weeks), then July 9 through GA (~110 commits over ~12 days) — bracketing about three weeks of near-flat commit activity in late June and early July while the product was being run against real workloads.
- **Parallelism:** up to 9 concurrent [git worktrees](https://git-scm.com/docs/git-worktree) under `.claude/worktrees/<topic>/`, each running an independent agent on an independent branch.
- **People:** one dev, one coding assistant. No team. When this post says "we," that's the two of us.

The uneven cadence is not incidental to the story — it *is* part of the story. The mid-project stretch where the commit graph goes almost flat is where the product was being tested against real Kubernetes clusters, real MCP servers, and real triage runs. Most of the correctness work in v2.7 (the digest expansion for nested JSON, the Vertex cache reference-404 retry, the OTel double-export bug that only showed up against Cloud Trace) was designed against evidence collected during that stretch. The coding bursts on either side of the gap look productive on a git graph; the middle is where the design decisions came from.

Two months and 357 commits is not "at scale" by any industrial definition. It is roughly 3–5× the volume of the earlier project, in a codebase with substantially more integration surface (multi-provider models, MCP, distributed daemons, an SSE-based attach protocol, OpenTelemetry, a cost stack). That was enough to break several habits that had worked at the earlier project's size and force us to build discipline that hadn't been necessary before.

Four things changed:

## Parallel worktrees

On the earlier project, one agent working on one branch was the whole picture. If you wanted to explore a second design in parallel, you'd checkout a different branch, and the agent would be *there* instead of *here*. That's fine for a few dozen commits.

At core-agent scale it's not. There are always three or four independent things in flight — a design doc getting reviewed, a bug fix that surfaced during smoke testing, a follow-up cleanup on last week's feature, an exploratory spike on next week's. Serializing them through a single working tree either wastes the assistant's throughput (three agents idle while one drives) or forces context-switches that lose the plot.

The pattern we settled into: **one worktree per topic**, all under `.claude/worktrees/<topic-name>/`, each on its own branch. A snapshot from today looks like this:

```text
/home/user/projects/core-agent                                        (main working tree)
.claude/worktrees/astro-docs                                          docs-cleanup
.claude/worktrees/blog                                                worktree-blog
.claude/worktrees/context-window                                      worktree-context-window
.claude/worktrees/easy-issues                                         feat/shared-tap-usage
.claude/worktrees/kube-agent-hermes                                   worktree-kube-agent-hermes
.claude/worktrees/metrics                                             feat/metrics-otel-prometheus
.claude/worktrees/multi-session-fixes                                 fix/multi-session-issues-273-274-275
.claude/worktrees/scion-agent                                         worktree-scion-agent
```

Nine trees, nine branches, nine possible agents. Each tree is fully isolated — its own working copy, its own build cache, its own `go test` runs. The main checkout stays clean and stays on `main`; long-running work happens in a worktree and gets rebased before opening a PR.

Two things about this pattern turned out to matter more than they looked:

- **The naming is the coordination.** `<topic>` in the worktree path is the same string I use in commit messages, in the PR title prefix, in the design-doc filename, and in conversation with the assistant. Grep for `metrics` and you find the branch, the tree, the design doc, and the PR. When four things are in flight at once, this is the difference between coordinating them and losing one.
- **The stash stack is shared.** All worktrees share the same repo, so bare `git stash` / `git stash pop` on one tree can pop another tree's changes. Once you have several sessions running you learn to never use bare stash; the discipline is `git stash push -u -m "<unique-tag>"`, capture the SHA immediately, and `git stash apply <sha>` by hash — never pop. This is a papercut the pattern surfaces that a single working tree hides.

Nine parallel trees is theoretical; in practice we run three or four agents concurrently. But even the *possibility* of parallelism changes how you break work down: bugs become small, self-contained branches you can dispatch and forget rather than serialized items on a queue.

## AGENTS.md, memory, and skills as durable discipline

The earlier post ended on the "memorize" step — convert anything worth keeping into a durable artifact the next session will see. At that project's scale, most of that memory lived in a growing `docs/` folder and in one long `AGENTS.md`.

At core-agent scale, that stopped scaling. Three pieces evolved:

**1. `AGENTS.md` as a load-bearing prefix.** core-agent's [`AGENTS.md`](https://github.com/go-steer/core-agent/blob/main/AGENTS.md) is 233 lines. Every AGENTS.md-aware agent that runs in the repo loads it into its system prompt. What earns a line in there is not "nice to know" — it's "if this isn't in the prompt, the agent will get it wrong." Concrete examples:

- **Pitfalls we've hit,** verbatim, with the fix. `t.Setenv` doesn't compose with `t.Parallel()`. ADK's `req.Tools` field is unused — real declarations live on `req.Config.Tools`. Anthropic's Vertex SDK panics on missing creds unless you pass credentials via `WithCredentials` yourself. These live under `## Pitfalls & gotchas (real ones we've hit)`.
- **The presubmit invocation, spelled out.** `dev/ci/presubmits/{build,lint-go,test-unit,verify-go-format,verify-mod-tidy,vet,verify-vuln}`. Not "run CI locally" — the actual eight scripts, so an agent can copy-paste them.
- **The release scripts,** with a "do NOT hand-carve" imperative. This one is a scar: we did hand-carve GA notes for one release, they were wrong, we wrote `cut-ga-tag.sh` to make sure it couldn't happen again, and then we put a line in `AGENTS.md` telling the assistant to use it.
- **The stacked-PR gotcha.** `gh pr merge A --delete-branch` closes any PR whose base was branch A — so retarget downstream PRs to `main` BEFORE merging the parent. Learned by breaking it.

What's *not* in `AGENTS.md` is anything the agent can derive by reading the code. The rule is: if `find`/`grep`/`Read` would surface it in ten seconds, don't put it in the prompt. The prompt is for things the agent needs *before* it looks.

**2. Memory as institutional knowledge.** Claude Code has a memory system that persists across conversations. We use it for a specific class of thing: **feedback the user has given about how to work in this project.** Not "how core-agent works" (that's derivable) but "how the user wants me to behave when working on it." A sample:

- *Run presubmits before pushing* — the loop above, every time, not just "important" pushes. This one has a scar attached: two red CI runs in a row, both preventable by the local scripts, both cheaper to catch pre-push.
- *UAT files always under `/tmp`, never `$HOME`* — default `--session-db` / `--cache-dir` / `--log-file` to `os.TempDir()/<app>/...`. Throwaway state doesn't belong in the user's home.
- *Respect DESIGN.md deferrals* — don't re-propose surfacing features the design doc explicitly defers without a concrete consumer use case. The deferral is itself a design decision.
- *Embedded TUI position flipped 2026-05-23* — the codebase's TUI decision changed, mid-project; the memory says so and cites the date so I don't advocate the old position from stale context.

The pattern that makes these load-bearing rather than clutter: **each entry has a `Why:` line and a `How to apply:` line.** The *why* is usually a past incident — a broken CI run, a scrubbed home directory, an argument that reached a conclusion. Knowing the *why* lets the assistant judge edge cases. Without it, the rules erode as new situations don't quite fit them.

**3. Skills as reusable subroutines.** Anything the assistant does more than three times gets a skill: a discovered-at-invocation-time subroutine with its own instructions, references, and (sometimes) helper scripts. core-agent has skills for autonomous setup, CLI setup, library embedding, and more. On the Claude Code side, we lean on skills like `simplify`, `security-review`, `deep-research`, and `run` — these aren't project-specific but they compose with the project-specific ones.

The triangle — `AGENTS.md` for what the agent needs before it looks, memory for how to work with this user, skills for reusable procedures — is what "the harness" means when we talk about it. None of these is prompting. Prompting is the tip; the harness is the iceberg.

## Presubmits as the load-bearing feedback loop

On the earlier project, hand-reviewing every diff was tractable. Read the change, glance at the tests, catch the obvious. At core-agent scale, with several agents committing several times an hour, hand-review as the primary check falls apart.

The load-bearing feedback loop had to move earlier. What we landed on: **the local presubmit scripts are the review, and human review is the second pass.** The scripts under [`dev/ci/presubmits/`](https://github.com/go-steer/core-agent/tree/main/dev/ci/presubmits) are literally the same scripts CI runs — build, lint, tests, format check, `go mod tidy` clean, `go vet`, `govulncheck`. The full sweep takes about 30 seconds. If it doesn't pass locally, don't push.

A few properties made this actually work rather than aspirationally work:

- **The scripts auto-install their tools.** No "you need to `go install golangci-lint` first" step; if the tool is missing, the presubmit installs it. Removes the friction that made "I'll skip this one" tempting.
- **`gofmt` isn't enough.** The format presubmit runs `gofmt -s` *and* `goimports`. The number of times a naive `go fmt ./...` looked clean and CI failed on import ordering was non-trivial before we tightened this.
- **License headers are enforced by lint, not by convention.** Every Go/shell/YAML/Python file has the Apache 2.0 boilerplate with Google LLC attribution. The `goheader` linter catches misses. Skills like `add-license-headers` idempotently apply the canonical form to new files, so the agent doesn't have to know the exact header shape.

The "shift review left" cliché is true here in a specific way: with the assistant generating diffs faster than a human can review them, the automatic checks are the review of record, and the human review is for judgment (does this design make sense, is this the right abstraction) rather than mechanics (does it compile, does it lint, is it formatted).

## Confirmations, not just corrections

Some of the most useful memory entries are the ones that record moments the assistant was *right*. Most people writing about coding assistants remember the corrections and forget the confirmations. Confirmations decay faster than corrections because they don't hurt when you skip them — until the next session gets it wrong for a reason the previous session had already resolved.

Examples from core-agent's memory:

- The **cost-stack sequencing** entry records that the five PRs [#222](https://github.com/go-steer/core-agent/issues/222) → [#128](https://github.com/go-steer/core-agent/issues/128) → [#130](https://github.com/go-steer/core-agent/issues/130)+[#129](https://github.com/go-steer/core-agent/issues/129) → [#221](https://github.com/go-steer/core-agent/issues/221) → [#223](https://github.com/go-steer/core-agent/issues/223) landed in that specific order on 2026-07-17, and that the order was the design. If a future session picks up the digest stack and proposes rearranging, this is what makes the resulting "wait, we already decided" moment happen in seconds instead of hours.
- The **release scripts** entry records that `cut-dev-tag.sh` and `cut-ga-tag.sh` exist, that GA notes must be cumulative since the last GA, and that `cut-ga-tag.sh` (shipped 2026-07-20) folds the `dev.N` sections automatically. This is what stops a future session from hand-carving GA notes and getting them wrong the same way we got them wrong once.
- The **`v2.7.0` shipped** entry records that main is now `v2.8.0-dev` and that the module path is `github.com/go-steer/core-agent/v2`. That saves a `git log` and a `head CHANGELOG.md` every time a new session starts on this repo.

The rule of thumb: if a session ends and you'd bet even odds that a future session will re-derive the same conclusion (or re-hit the same wall), that's a memory. If the conclusion is one `grep` away, it's not.

## Where the assistant led us astray

Most posts about working with coding assistants are victory laps. Ours has a section titled "the assistant argued for a wrong position for a month before we noticed."

The specific case: **the embedded TUI.**

For weeks we (assistant and human, in agreement) held the position that the terminal UI belonged in a *separate* binary attached over the SSE-based attach protocol. The reasoning was inherited from the earlier project: that project's binary had an in-process bubble-tea TUI; extracting core-agent for library reuse meant lifting the agent runtime *out* of that TUI; the natural next step seemed to be keeping the TUI as a separate concern rather than re-embedding it.

We wrote a design doc arguing this — [`docs/embedded-tui-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/embedded-tui-design.md) — treating "embedded TUI" as a bad idea worth documenting so we'd stop being asked about it. The assistant and I reinforced each other's conclusion. The doc had a "Settled decisions" section. It was, by convention, done.

Then a v1.8 UAT surfaced three papercuts we hadn't taken seriously enough on paper:

1. Two processes for one mental model. Cold-start lag, doubled memory, split log streams. Operators kept asking "is this remote?".
2. Indirection bugs. SSE timeouts hid model responses; OSC 11 terminal queries leaked into the input box; the spawned agent's stdin EOF was tricky to reason about.
3. The mental overhead of explaining a two-binary story to anyone who wanted to *just* run the agent interactively.

The user flipped the position on 2026-05-23. The new architecture — captured in [`docs/embedded-tui-design-v2.md`](https://github.com/go-steer/core-agent/blob/main/docs/embedded-tui-design-v2.md) — is `core-agent-tui --local`: one TUI codebase, spawn-and-attach *locally* for the single-daemon case, attach-over-network for everything else. One TUI, two transports. The right design was hiding in plain sight the whole time; the assistant and I had ruled it out early and then kept confirming each other's reasoning.

A memory entry from that day is a load-bearing artifact now:

> **Embedded TUI position flipped 2026-05-23** — embedded TUI is now wanted; architecture is `core-agent-tui --local` (single TUI codebase, spawn-and-attach locally). Don't re-propose putting bubble tea in default core-agent binary.

The lesson isn't "the assistant was wrong" — it's that the human-plus-assistant loop has a specific failure mode where two agreeing parties reinforce each other into a worse local optimum than either would have reached alone. The countermeasure is small but real: **use real UAT often, and be willing to write the memory entry that says "we changed our minds and here's why."** The v2 design doc opens with "Reverses the central decision of [embedded-tui-design.md] (v1)" — the reversal is the load-bearing part of the doc, not an embarrassing footnote.

## What we'd do differently

If we started core-agent over tomorrow, three things would change from day one:

1. **Worktrees from day one.** We spent the first two weeks in a single working tree, serializing everything through it, and only adopted the multi-worktree pattern once the queue was visibly hurting us. It's fine at 30 commits and painful at 100.
2. **`AGENTS.md` as a growing prefix, not as a snapshot.** Early on we treated `AGENTS.md` as a one-shot README-alike. It's really a *log* of things the assistant needed to know before it looked. Every real pitfall we hit belongs in it. If you find yourself explaining the same thing to a fresh session twice, that's a line in `AGENTS.md`.
3. **`cut-dev-tag.sh` and `cut-ga-tag.sh` from the first release, not the fourth.** Every step you can move from "human recipe" into "shell script" is a step you can't get wrong at 11pm. GA notes cumulative-since-last-GA is the specific one we got wrong; there are surely others waiting for us in whatever we ship next.

The through-line: **the harness is the product**. We were building an agent runtime with an agent. Every affordance that agent needed to be productive on core-agent — durable memory, an `AGENTS.md` it actually reads, presubmits it can trust, skills it can invoke — is an affordance a user's agent needs on their own codebase. When we made core-agent easier to work in as its own contributor, we made it better as a product. When we didn't, the agent told us, patiently, in the form of a wrong PR or a stale piece of advice. Then we wrote it down.

That's what "one project later" means. Not that the loop is different. That the machinery around the loop is what you keep.
