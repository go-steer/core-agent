---
title: Documentation
toc: false
sidebar:
  open: true
---

`core-agent` is a reusable Go base agent built on the [Google Agent Development Kit](https://github.com/google/adk-go). It ships first-class Gemini and Claude (first-party + Vertex) backends, MCP server integration, Claude-style skills, an autonomous-run driver, durable sessions with an audit log, in-process subagents, and a permission gate — all behind a small Option-pattern API designed to be embedded in your own Go program.

{{< cards >}}
  {{< card link="getting-started/" title="Getting Started" subtitle="Install, pick a provider, run a first turn, lay out a project." icon="lightning-bolt" >}}
  {{< card link="providers/" title="Providers" subtitle="Every model backend — env vars, config, model IDs, gotchas." icon="adjustments" >}}
  {{< card link="library-api/" title="Library API" subtitle="Embed core-agent in your own Go binary. Every option, every extension point." icon="code" >}}
  {{< card link="autonomous/" title="Autonomous runs" subtitle="agent.RunAutonomous for unattended workers — budgets, crash-resume, lifecycle." icon="cog" >}}
  {{< card link="sessions/" title="Sessions and event log" subtitle="Durable sessions, audit log, replay (Since), live tail (Watch). SQLite / Postgres / MySQL." icon="database" >}}
  {{< card link="configuration/" title="Configuration" subtitle="Full .agents/config.json schema, field by field." icon="cog" >}}
  {{< card link="permissions/" title="Permissions" subtitle="Ask / allow / yolo modes, pattern grammar, path scope, the bash denylist." icon="lock-closed" >}}
  {{< card link="mcp/" title="MCP servers" subtitle="mcp.json schema, stdio + HTTP transports, env-var interpolation, namespacing." icon="server" >}}
  {{< card link="skills/" title="Skills" subtitle="Claude-compatible SKILL.md bundles — format, discovery, gating." icon="academic-cap" >}}
  {{< card link="scion-adapter/" title="Scion adapter" subtitle="Optional packaging for Scion's container runtime." icon="cube" >}}
{{< /cards >}}

## Quick links

- **Source**: [github.com/go-steer/core-agent](https://github.com/go-steer/core-agent)
- **Releases**: [latest tags + changelog](https://github.com/go-steer/core-agent/releases)
- **Issue tracker**: [report a bug or request a feature](https://github.com/go-steer/core-agent/issues)
- **Install**: `go get github.com/go-steer/core-agent@latest`

## What `core-agent` is — and isn't

It **is** a Go library + a thin reference CLI. You embed it in your own binary, pick which packages to wire up, add your domain-specific tools, and ship.

It **is not** a finished agent product. No Bubble Tea TUI, no rich slash-command framework, no domain-specific tools beyond the generic file/shell/todo/glob/grep set. Those belong in the consumer (see [`cogo`](https://github.com/go-steer/cogo) for a Claude-Code-style TUI built on top of similar primitives).
