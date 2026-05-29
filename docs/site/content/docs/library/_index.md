---
title: Using the library
weight: 4
---

Embedding `core-agent` in your own Go binary. Use cases include: custom coding assistants with domain tools, HTTP-served agents, web-app prompt UX, alternative LLM backends, integrations into existing orchestration frameworks.

> **Prefer to have an agent walk you through this?** The [`library-embedding` skill]({{< relref "skills-library/library-embedding.md" >}}) covers the same material in workflow form. Install once, then say "help me embed core-agent in my service" and the agent walks the 5-step runbook with you.

> **The `quickstart` page is landing in Phase 2 of the v2.0 docs redesign.** Until then, [Guide]({{< relref "library/guide.md" >}}) covers the extension points and [API]({{< relref "library/api.md" >}}) is the full reference.

## In this section

- **[Quickstart]({{< relref "library/quickstart.md" >}})** *(coming Phase 2)* — first 15 minutes: import, `agent.New`, first `Run`, minimal working example.
- **[Guide]({{< relref "library/guide.md" >}})** — extension points + worked examples (custom prompter, custom tools, custom provider, HTTP-served agent). *(Was `library-guide.md` pre-v2.)*
- **[API]({{< relref "library/api.md" >}})** — full reference: every option function, every public type, every default. *(Was `library-api.md` pre-v2.)*

## Common references

- [Configuration]({{< relref "reference/configuration.md" >}}) — `.agents/config.json` schema (consumed by both CLI + library callers)
- [Providers]({{< relref "reference/providers.md" >}}) — each backend's env vars, model IDs, gotchas
- [Sessions and event log]({{< relref "reference/sessions.md" >}}) — durable session storage via `eventlog`
- [Context management]({{< relref "reference/context-management.md" >}}) — `WithCompactor` / `WithCheckpointer` / `tools/agentic` for long-lived agents
