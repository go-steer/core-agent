---
title: core-agent
---

A reusable Go base agent built on the [Google Agent Development Kit](https://github.com/google/adk-go). Embed it in your binary, pick the providers and tools you need, ship.

[Read the docs →](docs/) &nbsp;&nbsp;&nbsp; [Source on GitHub →](https://github.com/go-steer/core-agent)

---

`core-agent` ships first-class **Gemini** and **Claude** (first-party + Vertex) backends, **MCP** server integration, Claude-style **skills**, an **autonomous-run driver** with budgets + crash-resume, **durable sessions** with audit/replay event log, **in-process subagents**, and a **permission gate** — all behind a small Option-pattern API designed to be embedded in your own Go program.

{{% blocks/section type="row" %}}

{{% blocks/feature icon="fa-solid fa-bolt" title="Autonomous runs" url="docs/autonomous/" %}}
`agent.RunAutonomous` loops the model toward a goal with budgets (turns / tokens / cost / wallclock). `ResumeAutonomous` picks up after a crash from the durable event log.
{{% /blocks/feature %}}

{{% blocks/feature icon="fa-solid fa-database" title="Durable sessions + audit log" url="docs/sessions/" %}}
`eventlog.Open` returns a SQLite/Postgres/MySQL-backed `session.Service` plus a `Stream` with monotonic `seq`, `Since(seq)` replay, and `Watch(seq)` live tail.
{{% /blocks/feature %}}

{{% blocks/feature icon="fa-solid fa-sitemap" title="In-process subagents" url="docs/library-api/#subagents" %}}
`agent.WithSubagents([]*Agent)` registers each as a callable tool. Subagent events stream into the parent's audit log under a branch-scoped path.
{{% /blocks/feature %}}

{{% /blocks/section %}}

## Install

```bash
go get github.com/go-steer/core-agent@v0.1.0
```

See [Getting started](docs/getting-started/) for the first turn, or jump to [Library API](docs/library-api/) if you want the full surface.

## What it is — and isn't

It **is** a Go library + a thin reference CLI. You embed it in your own binary, pick which packages to wire up, add your domain-specific tools, and ship.

It **is not** a finished agent product. No Bubble Tea TUI, no rich slash-command framework, no domain-specific tools beyond the generic file/shell/todo/glob/grep set. Those belong in the consumer (see [`cogo`](https://github.com/go-steer/cogo) for a Claude-Code-style TUI built on top of similar primitives).
