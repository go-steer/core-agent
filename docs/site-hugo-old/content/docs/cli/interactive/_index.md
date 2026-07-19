---
title: Interactive (TUI)
weight: 1
---

You're at a terminal, driving the agent yourself. `core-agent` (no flags, stdin is a TTY) lands in the Bubble Tea TUI. Conversation history is preserved across turns. Slash commands surface session controls.

## In this section

- **[Quickstart]({{< relref "/docs/cli/interactive/quickstart.md" >}})** — first 15 minutes: install, point at a provider, drop an `AGENTS.md`, add a skill, set permission posture, check it in.
- **[Workflows]({{< relref "/docs/cli/interactive/workflows.md" >}})** — worked examples: Go code-reviewer with MCP web search, read-only documentation writer.
- **[Slash reference]({{< relref "/docs/cli/interactive/slash-reference.md" >}})** — full slash command catalog + keybindings.

## Common references

- [Configuration]({{< relref "/docs/reference/configuration.md" >}}) — `.agents/config.json` schema
- [Permissions]({{< relref "/docs/reference/permissions.md" >}}) — controlling what the agent can do
- [Skills]({{< relref "/docs/reference/skills.md" >}}) — reusable named procedures
- [MCP servers]({{< relref "/docs/reference/mcp.md" >}}) — declarative third-party tool integration
- [Context management]({{< relref "/docs/reference/context-management.md" >}}) — keeping long sessions alive
- [Agent design]({{< relref "/docs/agent-design/_index.md" >}}) — prescriptive patterns for prompts, skills, and tool choice
