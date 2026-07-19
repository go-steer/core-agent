---
title: Reference
weight: 6
---

Cross-cutting reference material that both CLI users and library consumers need. Organized by topic rather than by audience — these pages describe `core-agent`'s configurable surfaces in the depth needed for non-trivial use.

## Configuration + identity

- **[Configuration]({{< relref "/docs/reference/configuration.md" >}})** — every field of `.agents/config.json`, every CLI flag that doesn't have a config-file equivalent.
- **[Providers]({{< relref "/docs/reference/providers.md" >}})** — Gemini, Vertex, Anthropic, Anthropic-via-Vertex, mock; env vars, model IDs, gotchas per backend.
- **[Permissions]({{< relref "/docs/reference/permissions.md" >}})** — gate modes (ask/accept-edits/plan/yolo), allow/deny patterns, path scope, persistence.

## Capabilities

- **[Built-in tools]({{< relref "/docs/reference/tools.md" >}})** — the model-facing tool catalog: file, search, shell, network, planning, plus the optional lifecycle tools (`mark_task_done`, `ask_user`, `schedule_next_turn`).
- **[MCP servers]({{< relref "/docs/reference/mcp.md" >}})** — declarative third-party tool integration via MCP.
- **[Skills]({{< relref "/docs/reference/skills.md" >}})** — Claude-compatible `SKILL.md` bundles.
- **[Context management]({{< relref "/docs/reference/context-management.md" >}})** — compaction, task-boundary checkpoints, agentic tool wrappers.

## Runtime

- **[Sessions and event log]({{< relref "/docs/reference/sessions.md" >}})** — durable session storage, audit log, replay, live tail, crash-resume.
- **[Attach mode TUI]({{< relref "/docs/reference/attach-tui.md" >}})** — `core-agent-tui` remote operator client.
- **[Multi-session daemon]({{< relref "/docs/reference/multi-session.md" >}})** — one daemon serving multiple concurrent sessions with per-caller auth, ACL, audit threading, and instruction overlays.
- **[Scion adapter]({{< relref "/docs/reference/scion-adapter.md" >}})** — embedding `core-agent` under the Scion distributed-runtime layer.
- **[Hooks]({{< relref "/docs/reference/hooks.md" >}})** — config-driven shell-command dispatch on tool/model/turn boundaries.
