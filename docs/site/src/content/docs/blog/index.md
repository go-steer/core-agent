---
title: Blog
description: What we've learned building core-agent — a Go-native agent runtime built with a coding assistant, in the open.
template: doc
tableOfContents: false
---

This is a blog about building a coding-assistant harness with a coding assistant. The recursion is the point.

There is one developer behind [core-agent](https://github.com/go-steer/core-agent) and one coding assistant (Claude Code, mostly). Between May and July of 2026 we shipped 357 commits, ~50k lines of Go, and v2.7.0 GA — a Go-native agent runtime with a distributed multi-daemon fleet, an SSE-based attach protocol, an OpenTelemetry + Prometheus metrics pipeline, and a cost accounting stack that survives real workloads. The posts here are about how, and about the parts we got wrong on the way.

The cadence wasn't even. Two coding bursts (mid-May through mid-June, and again July 9 through GA on July 20) bracketed roughly three weeks of almost no commits — that middle stretch was where we ran the product against real-world scenarios instead of writing more of it. This is the next chapter in a longer story: the same pair (one developer, one coding assistant) built an earlier Go-native terminal coding agent in the weeks before core-agent, and wrote up the loop we settled into working with an assistant on that earlier project. Where that project was three weeks and ~100 commits with essentially no UAT gap, core-agent is two months on paper, with the UAT gap as a first-class step in the loop rather than a footnote to it.

## Posts

- [**What we built and what we learned (hub)**](/core-agent/blog/hub/) — the tour: what core-agent is, what shipped in v2.7, and the arcs the rest of the series digs into.
- [**1. Working with a coding assistant, one project later**](/core-agent/blog/working-with-coding-assistants/) — the same loop we described on an earlier project, run over two months of bursty coding with a real UAT gap in the middle. Parallel worktrees, memory as institutional knowledge, presubmits as the load-bearing feedback loop, and the case where the assistant led us the wrong way for a month.
- [**2. The attach protocol — why SSE, why capabilities, why /whoami**](/core-agent/blog/attach-protocol/) — the choice to ship attach as SSE + JSON with capability negotiation, and why `/whoami` earned its keep as a first-class endpoint. Protocol design where "the operator doesn't have to install anything to debug it" is the load-bearing property.
- [**3. The cost stack, in the right order**](/core-agent/blog/cost-stack/) — five PRs cut per-session cost on the GKE-troubleshoot recipe from $0.28 to ~$0.05. The sequence they landed in was the design; landed out of order, most of them would have been unmeasurable.
- [**4. The embedded-TUI thesis flip**](/core-agent/blog/embedded-tui-flip/) — deep dive on the arc from the flagship: we argued the wrong architecture for a month, wrote a design doc committing to it, shipped v1.8, then flipped. What UAT surfaced, and why two agreeing parties can reinforce each other into a bad answer.
- [**5. Distributed runtime and fleet observability**](/core-agent/blog/distributed-runtime/) — multi-daemon changes what "the agent" means. What broke at fleet scale, and the OTel + Prometheus pipeline (one MeterProvider, two readers) that made it legible.
