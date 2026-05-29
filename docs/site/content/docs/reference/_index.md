---
title: Reference
weight: 6
---

Cross-cutting reference material that both CLI users and library consumers need. Organized by topic rather than by audience — these pages describe `core-agent`'s configurable surfaces in the depth needed for non-trivial use.

## Configuration + identity

- **[Configuration]({{< relref "reference/configuration.md" >}})** — every field of `.agents/config.json`, every CLI flag that doesn't have a config-file equivalent.
- **[Providers]({{< relref "reference/providers.md" >}})** — Gemini, Vertex, Anthropic, Anthropic-via-Vertex, mock; env vars, model IDs, gotchas per backend.
- **[Permissions]({{< relref "reference/permissions.md" >}})** — gate modes (ask/accept-edits/plan/yolo), allow/deny patterns, path scope, persistence.

## Capabilities

- **[Built-in tools and MCP servers]({{< relref "reference/mcp.md" >}})** — declarative third-party tool integration via MCP.
- **[Skills]({{< relref "reference/skills.md" >}})** — Claude-compatible `SKILL.md` bundles.
- **[Context management]({{< relref "reference/context-management.md" >}})** — compaction, task-boundary checkpoints, agentic tool wrappers.

## Runtime

- **[Sessions and event log]({{< relref "reference/sessions.md" >}})** — durable session storage, audit log, replay, live tail, crash-resume.
- **[Attach mode TUI]({{< relref "reference/attach-tui.md" >}})** — `core-agent-tui` remote operator client.
- **[Scion adapter]({{< relref "reference/scion-adapter.md" >}})** — embedding `core-agent` under the Scion distributed-runtime layer.
