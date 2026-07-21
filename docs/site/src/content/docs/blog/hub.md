---
title: What we built and what we learned
description: A tour of what core-agent is, what shipped in v2.7, and the arcs the rest of this series digs into.
template: doc
tableOfContents: true
sidebar:
  order: 1
---

This is a blog about building a coding-assistant harness with a coding assistant. The recursion is the point.

We — one developer, one coding assistant (Claude Code, mostly) — spent May, June, and July of 2026 turning core-agent from a fresh commit into v2.7.0 GA: a Go-native agent runtime that shipped 357 commits and roughly 50k lines of Go, with a distributed multi-daemon fleet, an SSE-based attach protocol, an OpenTelemetry + Prometheus metrics pipeline that reaches Cloud Trace end-to-end, and a cost-accounting stack that survives real production workloads. The posts here are about how we did that, and about the parts we got wrong on the way.

The two months on paper is misleading — we weren't coding every day. There were two coding bursts (mid-May through mid-June, then July 9 through the v2.7 push to GA on July 20), bracketing roughly three weeks in late June and early July where the commit graph is nearly flat. That gap was the point: we were running the product against real Kubernetes clusters, real MCP servers, and real triage scenarios instead of writing more code. Most of what shipped in v2.7 — the digest stack that surfaced from cost regressions on the demo drive, the OTel wiring that broke silently against Cloud Trace before we fixed it, the Vertex cache reference-404 that only appeared on resumed sessions — was designed against evidence gathered during that gap, not against a spec. The UAT stretch was where the loop's "smoke test" step earned its keep, at a scale where you can't fake it with unit tests.

The same pair built an earlier Go-native terminal coding agent in the weeks before core-agent, and wrote up the loop we settled into working with an assistant on that project. This is the next chapter. That earlier project was three weeks, ~100 commits, and essentially no UAT gap. core-agent runs the same loop for longer and with the UAT stretch treated as a first-class phase, not an afterthought. What changes when you do that is most of what this series is about.

## What core-agent is

core-agent is a reusable Go-based agent, built on the [Google Agent Development Kit for Go](https://github.com/google/adk-go). You embed it as a library in your own binary, or you run the reference `core-agent` CLI directly. Either way you pick the model providers, the MCP servers, the built-in tools, and the skills you need, drop an `AGENTS.md` in your repo, and ship.

Two modes matter:

- **Interactive TUI**, either local or attached over SSE. `core-agent-tui` is a separate binary that speaks the [attach protocol](/core-agent/reference/attach-http/) and can connect to any core-agent daemon on the network — including several at once, via the multi-daemon session switcher.
- **Autonomous**, running as a long-lived daemon that consumes events (Kubernetes events via `k8s-event-watcher`, MCP alert deliveries, scheduled cron triggers) and drives triage or remediation without a human at the keyboard.

Both modes ship from the same binary, share the same session store, and expose the same set of tools, MCPs, and skills. That symmetry is one of the load-bearing design choices; it shows up in most of the arcs below.

## What shipped in v2.7

v2.7.0 GA landed on 2026-07-20, cut from five incremental `v2.7.0-dev.N` tags. The headline pieces:

- **`go install`-able.** Module path is now `github.com/go-steer/core-agent/v2`, per Go's Semantic Import Versioning. Every v2.x tag before v2.7.0 was silently broken for `go install`; the rename closes [#206](https://github.com/go-steer/core-agent/issues/206) and takes container images from "the way you install this" to "the way you install this if you want a pinned image."
- **End-to-end distributed tracing.** W3C `traceparent` propagates across the daemon, [`k8s-event-watcher`](https://github.com/go-steer/core-agent/tree/main/cmd/k8s-event-watcher), and every MCP tool call. The span waterfall (`mcp.tool_call → digest.process → subagent.llm_call`) is verified against [GKE Managed OpenTelemetry](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/managed-otel-gke) landing in Cloud Trace. A `deploy/components/otel/` kustomize component makes the wiring a one-line overlay.
- **Cost-attribution + digest stack.** [`pkg/digest`](https://github.com/go-steer/core-agent/tree/main/pkg/digest) prunes MCP tool responses structurally, with an LLM-fallback second-chance path for prose that structural pruning can't reduce. Vertex explicit context caching bills the stable prefix at cache-read rates. Per-turn `UsageMetadata` with cache attribution rides the attach protocol out to the remote TUI's footer.
- **Fleet ergonomics.** `/switch` + `/attach` + peer-hub unify the operation of many daemons in one TUI. `.agents/env.yaml` manifests replace sed-substituted placeholders in bundles. Multi-session cost isolation lands (per-session `usage.Tracker`, rebuilt from eventlog on resume) so `/stats` and per-session cost ceilings stop returning the union across every session on the daemon.
- **The 2.7 minor also carried a lot of correctness work** — Vertex cache reference retries, Gemini bare-STOP handling, digest expansion of nested JSON, k8s event dedup by `LastTimestamp` — most of it surfaced by field usage between the dev tags rather than by tests.

The full changelog for v2.7 is [in the repo](https://github.com/go-steer/core-agent/blob/main/CHANGELOG.md#270--2026-07-20). This post won't try to reproduce it.

## The arcs

Every arc below is a lesson we didn't have when the project started, that we do now, and that would change how we'd start over. Each becomes its own post in the series.

### 1. Working with a coding assistant, one project later

The write-up from our earlier project ended with a post on the plan → implement → smoke → refine → memorize loop. That loop still works. What changes when you run it for two months, in bursts, with a real UAT gap in the middle, is *around* the loop, not inside it: parallel worktrees to run three or four agents at once, `AGENTS.md` + memory + skills as a durable discipline instead of clever prompting, presubmits as the load-bearing feedback loop, and — the payoff — a section on where the assistant led us the wrong way for a month before we noticed.

→ [Read the flagship post](/core-agent/blog/working-with-coding-assistants/)

### 2. The attach protocol

The choice to ship attach as **SSE + JSON**, rather than gRPC, WebSockets, or a custom framed protocol, turned out to matter far more than the design docs suggested. Capabilities negotiation across binaries that ship on different cadences, how `/whoami` became load-bearing for operator sanity, and why "just curl the endpoint" is a first-class debugging tool rather than a demo.

→ [The attach protocol — why SSE, why capabilities, why /whoami](/core-agent/blog/attach-protocol/)

### 3. The cost stack, in the right order

Five PRs cut per-session cost on our GKE-troubleshoot recipe from $0.28 to ~$0.05: [#222](https://github.com/go-steer/core-agent/issues/222) (usage metadata over the wire) → [#128](https://github.com/go-steer/core-agent/issues/128) (structural digest) → [#130](https://github.com/go-steer/core-agent/issues/130)+[#129](https://github.com/go-steer/core-agent/issues/129) (MCP wrap + `retrieve_raw` reversal) → [#221](https://github.com/go-steer/core-agent/issues/221) (Vertex context caching) → [#223](https://github.com/go-steer/core-agent/issues/223) (agentic wrap with LLM fallback). Getting the order wrong wouldn't have broken anything; it would have made each step un-measurable.

→ [The cost stack, in the right order](/core-agent/blog/cost-stack/)

### 4. The embedded-TUI thesis flip

Deep dive on the arc teased in the flagship. We argued for a month that the terminal UI belonged in a separate binary attached over SSE, and treated "embedded TUI" as a bad idea worth documenting so we'd stop being asked. Then we flipped. What UAT surfaced, why the assistant and I reinforced each other's wrong conclusion for weeks, and why "one TUI, two transports" was hiding in plain sight the whole time.

→ [The embedded-TUI thesis flip](/core-agent/blog/embedded-tui-flip/)

### 5. Distributed runtime + fleet observability

Multi-daemon changes what "the agent" means — process, peer, session, or turn — and every dashboard question filters at a different level. What breaks when there are three daemons (session identity, cost attribution, event dedup, tracing sampling), and how the OTel + Prometheus pipeline — one MeterProvider, two readers — turned out to be the thing that made the fleet legible.

→ [Distributed runtime and fleet observability](/core-agent/blog/distributed-runtime/)

## The one non-technical thing worth stating up front

Building an agent runtime, with an agent, teaches you a specific thing: **the harness is the product.** Every affordance the agent needed to be productive on core-agent — durable memory that survives a compaction, an `AGENTS.md` it actually reads on startup, presubmits it can trust, skills it can invoke rather than reimplement — is an affordance a *user's* agent needs on *their* codebase. The recursion is not just cute: the thing we were building was a place for an agent to live, and the thing building it was an agent that needed the same place. When we made core-agent easier to work in as its own contributor, we made it better as a product. When we didn't, the agent told us.

That's the through-line for the rest of the series.
