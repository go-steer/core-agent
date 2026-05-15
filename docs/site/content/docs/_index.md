---
title: Documentation
toc: false
sidebar:
  open: true
---

Reference documentation for `core-agent`.

| Page | What it covers |
|---|---|
| [Getting started](getting-started/) | Install the CLI or library, pick a provider, run a first turn, lay out a project. |
| [Providers](providers/) | Each model backend — env vars, config, model IDs, gotchas. |
| [Configuration](configuration/) | Full `.agents/config.json` schema reference, field by field. |
| [MCP servers](mcp/) | `mcp.json` schema, stdio + HTTP transports, env-var interpolation, namespacing, gating. |
| [Skills](skills/) | Claude-compatible `SKILL.md` bundles — format, discovery, gating. |
| [Permissions](permissions/) | Modes (ask / allow / yolo), pattern grammar, path scope, the bash denylist. |
| [Library API](library-api/) | Embedding `core-agent` from your own Go code; extension points. |
| [Autonomous runs](autonomous/) | `agent.RunAutonomous` for unattended workers; budgets, termination, crash-resume, the lifecycle tool. |
| [Sessions and event log](sessions/) | Durable sessions + audit log via the `eventlog` package; SQLite/Postgres/MySQL, replay, live tail, session lock. |
| [Scion adapter](scion-adapter/) | Optional packaging for Scion's container runtime. |
