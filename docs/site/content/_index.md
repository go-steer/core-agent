---
title: core-agent
---

{{< blocks/cover title="core-agent" image_anchor="top" height="med" >}}

<p class="lead mt-5">
A reusable Go base agent built on the Google Agent Development Kit. Embed it in your binary, pick the providers and tools you need, ship.
</p>

<a class="btn btn-lg btn-primary me-3 mb-4" href="docs/">Read the docs <i class="fa-solid fa-arrow-right ms-2"></i></a>
<a class="btn btn-lg btn-secondary me-3 mb-4" href="https://github.com/go-steer/core-agent">Source on GitHub <i class="fa-brands fa-github ms-2"></i></a>

{{< /blocks/cover >}}

{{% blocks/lead color="primary" %}}

`core-agent` ships first-class **Gemini** and **Claude** (first-party + Vertex) backends, **MCP** server integration, Claude-style **skills**, an **autonomous-run driver** with budgets + crash-resume, **durable sessions** with audit/replay event log, **in-process subagents**, and a **permission gate** — all behind a small Option-pattern API designed to be embedded in your own Go program.

{{% /blocks/lead %}}

{{% blocks/section color="dark" type="row" %}}

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

{{% blocks/section %}}

## Install

```bash
go get github.com/go-steer/core-agent@v0.1.0
```

See [Getting started](docs/getting-started/) for the first turn, or jump to [Library API](docs/library-api/) if you want the full surface.

{{% /blocks/section %}}
