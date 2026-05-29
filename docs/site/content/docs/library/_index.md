---
title: Using the library
weight: 4
---

Embedding `core-agent` in your own Go binary. Use cases include: custom coding assistants with domain tools, HTTP-served agents, web-app prompt UX, alternative LLM backends, integrations into existing orchestration frameworks.

> **Prefer to have an agent walk you through this?** The [`library-embedding` skill]({{< relref "/docs/skills-library/library-embedding.md" >}}) covers the same material in workflow form. Install once, then say "help me embed core-agent in my service" and the agent walks the 5-step runbook with you.

## In this section

- **[Guide]({{< relref "/docs/library/guide.md" >}})** — extension points + worked examples (custom prompter, custom tools, custom provider, HTTP-served agent).
- **[API]({{< relref "/docs/library/api.md" >}})** — full reference: every option function, every public type, every default.

## Common references

- [Configuration]({{< relref "/docs/reference/configuration.md" >}}) — `.agents/config.json` schema (consumed by both CLI + library callers)
- [Providers]({{< relref "/docs/reference/providers.md" >}}) — each backend's env vars, model IDs, gotchas
- [Sessions and event log]({{< relref "/docs/reference/sessions.md" >}}) — durable session storage via `eventlog`
- [Context management]({{< relref "/docs/reference/context-management.md" >}}) — `WithCompactor` / `WithCheckpointer` / `tools/agentic` for long-lived agents
