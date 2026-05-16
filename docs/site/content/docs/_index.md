---
title: Documentation
linkTitle: Documentation
weight: 1
menu:
  main:
    weight: 10
---

You're in the `core-agent` reference docs. The site root has the marketing pitch; this section is the reference.

## Start here

**Brand new?** → [Getting started](getting-started/) walks you from `go install` through `core-agent -p "hello"` against your first provider, plus the `.agents/` project layout.

**Embedding `core-agent` in your own Go binary?** → [Library API](library-api/) is the full reference — every `agent.Option`, every extension point, with worked code samples. The [Minimal example](library-api/#minimal-example) is the shortest possible program.

**Picking a model backend?** → [Providers](providers/) covers Gemini API, Vertex Gemini, Anthropic, and Anthropic via Vertex — env vars, model IDs, auto-detection rules, gotchas.

**Building an unattended worker?** → [Autonomous runs](autonomous/) covers `agent.RunAutonomous` (budgets, lifecycle tool, termination), `ResumeAutonomous` (crash-resume), and `WithSubagents` (in-process delegation).

**Need an audit log or crash-resume?** → [Sessions and event log](sessions/) covers `eventlog.Open`, the `Stream` API (`Since`, `Watch`, `WithSessionTree`), and the session lock that makes concurrent resume safe.

## Reference index

### Core API
- **[Library API](library-api/)** — `agent` package, options, tools, prompters, MCP, skills, recording, telemetry. The largest page; use the right-hand TOC.
- **[Autonomous runs](autonomous/)** — `RunAutonomous`, `ResumeAutonomous`, lifecycle tool, ask-user patterns.
- **[Sessions and event log](sessions/)** — `eventlog.Open`, replay, live tail, session lock, crash-resume.

### Configuration & integration
- **[Configuration](configuration/)** — `.agents/config.json` schema, field by field. Permissions, path scope, tool output caps, mock providers, OTEL exporter, runtime-only CLI flags.
- **[Permissions](permissions/)** — ask / allow / yolo modes, pattern grammar, path scope, the bash denylist, prompters, autonomous-run interaction.
- **[MCP servers](mcp/)** — `mcp.json` schema, stdio + Streamable HTTP transports, env-var interpolation, tool namespacing, gating.
- **[Skills](skills/)** — Claude-compatible `SKILL.md` bundles. Format, discovery, allow/deny, MCP composition.

### Adapters
- **[Scion adapter](scion-adapter/)** — `extras/scion-agent/` packaging for Scion's container runtime: lifecycle status, `--input` task delivery, sticky-state tool, `--session-db` flag.

## Help and community

- **Source code** → [github.com/go-steer/core-agent](https://github.com/go-steer/core-agent)
- **Issues** → [github.com/go-steer/core-agent/issues](https://github.com/go-steer/core-agent/issues) — bug reports, feature requests
- **Discussions** → [github.com/go-steer/core-agent/discussions](https://github.com/go-steer/core-agent/discussions) — questions, what-are-you-building threads
- **Releases & changelog** → [latest releases](https://github.com/go-steer/core-agent/releases) and [`CHANGELOG.md`](https://github.com/go-steer/core-agent/blob/main/CHANGELOG.md)

## What this site doesn't cover

- **Cogo** — Claude-Code-style TUI built on top of similar primitives. Different project; see [cogo.io](https://github.com/go-steer/cogo) docs.
- **ADK Go** — `core-agent` wraps Google's [Agent Development Kit](https://github.com/google/adk-go). For raw ADK primitives, see the upstream docs.
- **Model APIs** — for what models *can do*, see Google AI Studio / Vertex docs and Anthropic's docs. This site documents how `core-agent` talks to them, not the model surface itself.
