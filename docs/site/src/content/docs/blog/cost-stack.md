---
title: The cost stack, in the right order
description: A sequence of five PRs that materially lowered per-session cost on our GKE-troubleshoot recipe. The sequence they landed in was the design, and the work isn't done.
template: doc
tableOfContents: true
sidebar:
  order: 4
---

On 2026-07-13 we ran our GKE-troubleshoot recipe end-to-end against a real cluster and got a real number: about **$0.28 per triage session**. Ten turns, 181k input tokens, `gemini-3.5-flash`. Not catastrophic — at a hundred incidents a day it's about $840 a month — but high enough that if operators ever looked at the bill, they'd notice, and it's the kind of number that only goes up as agents get chattier.

Three days later, on comparable turn shapes, that number was materially smaller — roughly a 5–6× per-session reduction on the same recipe on the same cluster. Not by using a cheaper model. Not by cutting features. By landing five PRs in a specific order:

1. **[#222](https://github.com/go-steer/core-agent/issues/222)** — per-turn `UsageMetadata` on `/sessions/<id>/usage`
2. **[#128](https://github.com/go-steer/core-agent/issues/128)** — [`pkg/digest`](https://github.com/go-steer/core-agent/tree/main/pkg/digest) skeleton: JSON pruner + Store interface
3. **[#130](https://github.com/go-steer/core-agent/issues/130) + [#129](https://github.com/go-steer/core-agent/issues/129)** — structural pruner wired into MCP wrap + `retrieve_raw` safety net
4. **[#221](https://github.com/go-steer/core-agent/issues/221)** — Vertex explicit context caching
5. **[#223](https://github.com/go-steer/core-agent/issues/223)** — LLM subagent second-chance path for prose payloads

The **order was the design**. Any of those PRs could have landed in isolation and produced a measurable improvement on paper. Landed out of sequence, several of them would have been unmeasurable, unsafe, or both. This post is about why.

## Diagnosis first

Before the sequencing decision, the diagnosis: **the demo's cost was dominated by MCP output size, not by prefix-repeat waste.** GKE MCP responses are effectively 100% JSON; a `list_pods` call in a busy namespace returns a payload measured in tens of kilobytes, most of which the model doesn't need. Prefix repetition (system instruction + tool declarations, resent every turn) mattered too, but was the smaller of the two lines on the graph.

That diagnosis pinned the ordering:

- **Cut the biggest line first.** MCP output size, via structural pruning.
- **Cut the second line second.** Prefix repetition, via context caching.
- **Cut the residue third.** Prose-shaped MCP outputs the structural pruner can't reduce, via an LLM second-chance.
- **All three need measurement before they can be evaluated.** So per-turn attribution has to land first.
- **All of them need a safe reversal knob.** So the "cut" always ships with a way to retrieve the original.

That's the whole shape. The five PRs are just the implementation of it.

## Why #222 first

[`#222`](https://github.com/go-steer/core-agent/issues/222) shipped per-turn `UsageMetadata` on the attach protocol's `/sessions/<id>/usage` endpoint. Cache-aware: separates input tokens into `cached` and `uncached`, breaks out output, thoughts, and tool-use, adds cost in USD, all pivotable by model. Before it, `/stats` returned session-level totals only.

Landing it first was not because the digest work needed it as a hard dependency. It was because **without per-turn attribution, none of the subsequent PRs would have been measurable.** A cost cut that shows up as "session total went from $0.28 to $0.19" is invisible if you can't tell which of turns 1 through 10 got smaller and by how much. It might have been a run-to-run variance. It might have been a follow-up question the operator didn't ask this time. You cannot iterate on optimizations you can't attribute.

The general principle, learned the hard way on earlier work: **before you optimize, ship the meter.** The meter has to break out the specific dimension you plan to move. In this case that was cache-hit ratio and per-turn shape — both invisible to the pre-#222 aggregate view.

This is also the argument for landing measurement in a *user-visible* way, not just as a log line. `/sessions/<id>/usage` is queryable from the TUI, from `curl`, from a dashboard. That means the operator can watch the number move as each subsequent PR lands and form an intuition for what's helping. If the meter only reaches a Prometheus counter nobody scrapes yet, it's not measurement — it's future measurement.

## Why #128 second, and why it's a skeleton

[`#128`](https://github.com/go-steer/core-agent/issues/128) shipped [`pkg/digest`](https://github.com/go-steer/core-agent/tree/main/pkg/digest) as a skeleton: a `Process` type that routes payloads through a chain of pruners, a `Store` interface for persisting the pre-pruned original, and one initial pruner — a structural JSON walker that recursively drops fields that look like noise (long opaque strings, arrays past a size threshold, nested objects deeper than a depth cap). The MCP wire path was *not* wired in; nothing in the daemon called it yet.

Two reasons for the skeleton-first approach:

- **Isolation of the interesting decisions.** The pruner's semantics — what counts as noise, what depth to stop at, how to signal truncation to the consumer — are the load-bearing design work. Doing them in a package that runs no production traffic yet means we could write real tests, iterate freely, and land the design without any risk of a cost regression from a botched wire-through.
- **The `Store` interface was the future's problem.** We knew we'd need a way to retrieve pre-pruned payloads (see next section), and we knew the store implementation would evolve — filesystem first, then eventlog-backed, then possibly memory-tiered. Nailing the interface without committing to a first implementation kept the door open.

The skeleton also earned its keep in review: a self-contained package with unit tests is much easier to argue about than a diff spread across five call sites. Reviewers focused on the pruner's semantics rather than on the plumbing.

## Why #130+#129 as a bundle

[`#130`](https://github.com/go-steer/core-agent/issues/130) wired the structural pruner into the MCP wrap path — every MCP tool response now goes through the digest before it's appended to the model's context. [`#129`](https://github.com/go-steer/core-agent/issues/129) shipped `retrieve_raw`, a built-in tool the model can invoke to fetch the pre-pruned payload for any digested response.

These two had to land together. Alone, neither is safe:

- `#130` alone means the model sees a digested payload it can't undo. If the pruner drops a field the model *did* need — a specific pod status, a numeric threshold, a container image tag — the model has no recovery path and the turn fails silently. The operator sees a bad answer and can't tell whether it was the model or the tool.
- `#129` alone means there's a tool for retrieving raw payloads but nothing to retrieve them *from*, because the digest hasn't landed yet.

Bundled, they enforce a specific contract: **every digested payload has a retrieval handle.** The digest carries an ID; `retrieve_raw` takes that ID and returns the pre-pruned payload. The model can be told (via the tool description) to treat the digest as authoritative and only reach for `retrieve_raw` when a field it needed was elided — [we later sharpened that tool description in #300](https://github.com/go-steer/core-agent/pull/300) after Flash reached for it casually and blew a cost regression.

The design principle: **any change that removes information the model was previously seeing must ship with a mechanism for the model to get it back.** Otherwise the optimization is invisible in the good case and catastrophic in the bad case, which is the worst combination.

`#130+#129` was the single biggest cost lever in the stack. Structural pruning on JSON-shaped MCP responses cut MCP payloads by 5–10× on the GKE-troubleshoot recipe. The turn-shape data from `#222` made it possible to confirm this immediately: the very first drive after #130 landed showed the input-token line for MCP-heavy turns dropping to a fraction of its previous shape.

## Why #221 fourth

[`#221`](https://github.com/go-steer/core-agent/issues/221) shipped Vertex explicit context caching. The daemon creates a `CachedContent` resource after turn 1 (containing the stable prefix: system instruction + tool declarations) and stamps it onto every subsequent `GenerateContent` call. The stable prefix bills at cache-read rates rather than full input rates. On a `gemini-3.5-flash` session with a chunky tool declaration set, that's a ~10× discount on the ~15k tokens of prefix per turn.

Placement was fourth because:

- **Prefix caching is orthogonal to MCP output size.** It cuts a different dimension, so its impact stacks multiplicatively with `#130`. Landing it before `#130` would have made the demo cost drop from $0.28 to ~$0.15 — real, but leaving 3× on the table until `#130` also landed. Landing it *after* gets you $0.05 in one demo drive, which is a much more compelling artifact to write down.
- **Vertex explicit caching has a lifecycle to reason about.** The cache resource has a TTL. When the daemon holds a stale handle and the server-side cache expired underneath it, every subsequent turn hard-fails with `NOT_FOUND: cached content metadata`. We caught this in a resumed-session UAT and shipped the retry-uncached-on-404 hotfix in [#299](https://github.com/go-steer/core-agent/pull/299). Landing this after `#130` (which is deterministic and has no lifecycle) meant the "did this break the cost?" investigation had only one moving part.
- **We also needed the pricing infrastructure to score it.** Cache-read rates had to be wired into `/pricing` first ([#264](https://github.com/go-steer/core-agent/pull/264)) so `/usage`'s cost field reflected the discount. Without that, the cache would have shipped and the reported cost would have been unchanged, hiding the actual saving behind stale pricing.

The order made every step attributable. Turn cost dropped by roughly the amount the model of the change predicted, at each step. When it didn't (the [#221 hotfix](https://github.com/go-steer/core-agent/pull/270) — Vertex rejects `Tools` + `SystemInstruction` on cached turns — was the notable one), we could tell immediately because the meter said so.

## Why #223 last

[`#223`](https://github.com/go-steer/core-agent/issues/223) landed the LLM subagent second-chance path for prose payloads. Some MCP responses are structured JSON (great — `#130` handles them); some are prose (Slack messages, GitHub PR bodies, human-authored logs). Structural pruning does nothing useful on prose because there's no structure to walk. `#223` routes those payloads to a small LLM subagent that summarizes them, wrapped in the same digest infrastructure so `retrieve_raw` still works.

Two reasons this was last:

- **It only makes sense after the deterministic path exists.** The LLM call costs money and adds latency. If it ran on everything — including the JSON responses `#130` already handled cheaply and deterministically — it would be a cost regression, not a saving. Sequencing means every payload goes through the cheap structural cut first, and only prose that survives the structural pass hits the LLM. Cheap-then-expensive.
- **It needed the fallback to be opt-in.** Landed as `--mcp-agentic-wrap-llm` and the `agentic_wrap_llm` field on `mcp.json`. Off by default in v1: prose payloads pass through un-summarized, which is the safe (though sometimes expensive) failure mode. Operators who see prose responses inflating their per-turn token count can flip it on; those who don't, aren't paying for an LLM call they don't need.

`#223` landed via [#290](https://github.com/go-steer/core-agent/pull/290), which absorbed a follow-up ([#292](https://github.com/go-steer/core-agent/pull/292)) before merging. That merge closed the cost stack — every subsequent PR was a fix or a follow-up on the shipped surface, not a new cost lever.

## What the numbers actually did

One drive post-stack, on 2026-07-16 — a two-turn GKE-troubleshoot session against `${PROJECT_ID}` / `std-simian-test` — gave us a representative measurement:

- 2 turns · in 80,846 (30,932 cached / 49,914 uncached) · out 612
- **Cost: $0.0850** on that session (uncached-reference $0.1268 → cache saved $0.0418, ~33% reduction on that drive)

The per-turn cost that showed up in the meter for that drive was actually *higher* than the baseline's per-turn cost in raw dollars ($0.042 vs $0.028), because the post-stack drive issued more and larger MCP calls per turn. That's fine — the honest comparison is the **effective input rate**: dropped from around $1.55/M to around $1.05/M on this workload, roughly a third off, attributable to a mix of caching and structural pruning.

Treat these as illustrative rather than definitive. The per-session number tells the operator-facing story (~5–6× reduction on comparable turn shapes for this recipe). The $/M number tells the engineering story (each token is genuinely cheaper on this workload). Real workloads vary; the specific savings depend on MCP output shapes, prefix stability, prose-versus-JSON mix, and how chatty the model gets. What generalizes isn't the specific number — it's that each lever was measurable in isolation, thanks to `#222` in the ground under them, so you can see which one is helping on *your* traffic instead of trusting ours.

**Cost work is not done.** Everything above is one round; the stack we shipped is scaffolding for the next one. Real telemetry from more operators will surface payloads the current pruners don't handle well, prefixes the cache manager wastes, and prose that the LLM fallback either over- or under-summarizes. The gaps get filed as follow-ups. The general expectation: another 2× on top of this over time as we see more real workloads, and no reason to think we're near a floor.

## What we'd do differently

Two things:

1. **Ship [`#222`](https://github.com/go-steer/core-agent/issues/222) even earlier than we did.** In the earlier v2.6 drive we already had aggregate session cost, and it was clear the demo's cost was a lever worth pulling. If we'd shipped per-turn `UsageMetadata` at that moment — before diagnosing anything else — the subsequent diagnosis phase would have been shorter and more precise. The heuristic: **ship the meter as soon as you know you'll be optimizing this axis, not as the first step of the optimization plan.**
2. **Document the sequencing rationale in-tree at plan time, not in retrospect.** The plan document ([`docs/backlog-cost-stack-2026-07-14.md`](https://github.com/go-steer/core-agent/blob/main/docs/backlog-cost-stack-2026-07-14.md)) was written *after* the ordering discussion, not before. It captured the decision, but it captured it once — meaning a future session picking up the plan doc had to trust the outcome without seeing the alternatives that got rejected. A "considered orderings" section, written at plan time, would have been useful when a follow-up PR ([#292](https://github.com/go-steer/core-agent/pull/292)) landed weeks later and the question "why isn't this in the main sequence?" came up in review.

The through-line: **cost optimization is a sequencing problem, not a per-PR problem.** Any of these five PRs could have shipped in isolation and moved the number. Landed as a stack, in the right order, each one made the next one measurable, the next one safe, and the round of measurements we captured on the GKE recipe legible enough to write about — and to build on. The stack is what we ship; the next round of reductions is what we work on next.
