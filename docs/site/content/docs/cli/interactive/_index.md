---
title: Interactive (TUI)
weight: 1
---

You're at a terminal, driving the agent yourself. `core-agent` (no flags, stdin is a TTY) lands in the bubble-tea TUI. Conversation history is preserved across turns. Slash commands surface session controls.

## In this section

- **[Quickstart]({{< relref "cli/interactive/quickstart.md" >}})** — first 15 minutes: install, point at a provider, drop an `AGENTS.md`, add a skill, set permission posture, check it in.
- **[Workflows]({{< relref "cli/interactive/workflows.md" >}})** — worked examples: Go code-reviewer with MCP web search, read-only documentation writer.
- **[Slash reference]({{< relref "cli/interactive/slash-reference.md" >}})** — full slash command catalog + keybindings.

## Common references

- [Configuration]({{< relref "reference/configuration.md" >}}) — `.agents/config.json` schema
- [Permissions]({{< relref "reference/permissions.md" >}}) — controlling what the agent can do
- [Skills]({{< relref "reference/skills.md" >}}) — reusable named procedures
- [MCP servers]({{< relref "reference/mcp.md" >}}) — declarative third-party tool integration
- [Context management]({{< relref "reference/context-management.md" >}}) — keeping long sessions alive
- [Agent design]({{< relref "agent-design/_index.md" >}}) — prescriptive patterns for prompts, skills, and tool choice
