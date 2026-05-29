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

**Evaluating `core-agent` against raw ADK?** → [Why core-agent]({{< relref "why-core-agent.md" >}}) is the long-form pitch with a side-by-side capability comparison.

**Brand new?** → [Getting started]({{< relref "getting-started.md" >}}) walks you from `go install` through `core-agent -p "hello"` against your first provider, plus the `.agents/` project layout.

**Running the bundled binary?** → [Using the CLI]({{< relref "cli/_index.md" >}}) splits into [Interactive (TUI)]({{< relref "cli/interactive/_index.md" >}}) — drive the agent yourself from a terminal — and [Autonomous (headless)]({{< relref "cli/autonomous/_index.md" >}}) — unattended workers with budgets + crash-resume.

**Embedding `core-agent` in your own Go binary?** → [Using the library]({{< relref "library/_index.md" >}}) covers the extension points; [API]({{< relref "library/api.md" >}}) is the exhaustive reference.

**Tuning prompts, skills, and tool descriptions?** → [Agent design]({{< relref "agent-design/_index.md" >}}) is the prescriptive section — what patterns work, what failure modes to watch for, how to get the model to use subagents and `agentic_*` wrappers.

**Configuring a specific surface?** → [Reference]({{< relref "reference/_index.md" >}}) is the cross-cutting index — providers, config, permissions, MCP, skills, sessions, context management, attach mode.

## Reference index

### CLI

- **[Using the CLI]({{< relref "cli/_index.md" >}})** — interactive vs autonomous landing
  - **[Interactive (TUI)]({{< relref "cli/interactive/_index.md" >}})** — quickstart, workflows, slash reference
  - **[Autonomous (headless)]({{< relref "cli/autonomous/_index.md" >}})** — quickstart, operations, multi-agent GKE scenario

### Library

- **[Using the library]({{< relref "library/_index.md" >}})** — quickstart, guide (narrative tour of extension points), API (exhaustive reference)

### Agent design

- **[Agent design]({{< relref "agent-design/_index.md" >}})** — prompt + skill + tool-description patterns for efficient, well-behaved agents

### Reference

- **[Configuration]({{< relref "reference/configuration.md" >}})** — `.agents/config.json` schema
- **[Providers]({{< relref "reference/providers.md" >}})** — Gemini, Vertex, Anthropic, mock
- **[Permissions]({{< relref "reference/permissions.md" >}})** — ask/accept-edits/plan/yolo, patterns, scope
- **[MCP servers]({{< relref "reference/mcp.md" >}})** — declarative third-party tool integration
- **[Skills]({{< relref "reference/skills.md" >}})** — Claude-compatible `SKILL.md` bundles
- **[Context management]({{< relref "reference/context-management.md" >}})** — compaction, checkpoints, agentic tool wrappers
- **[Sessions and event log]({{< relref "reference/sessions.md" >}})** — durable storage, audit log, replay, crash-resume
- **[Attach mode TUI]({{< relref "reference/attach-tui.md" >}})** — `core-agent-tui` remote operator client
- **[Scion adapter]({{< relref "reference/scion-adapter.md" >}})** — embedding under Scion's distributed runtime

## Help and community

- **Source code** → [github.com/go-steer/core-agent](https://github.com/go-steer/core-agent)
- **Issues** → [github.com/go-steer/core-agent/issues](https://github.com/go-steer/core-agent/issues) — bug reports, feature requests
- **Discussions** → [github.com/go-steer/core-agent/discussions](https://github.com/go-steer/core-agent/discussions) — questions, what-are-you-building threads
- **Releases & changelog** → [latest releases](https://github.com/go-steer/core-agent/releases) and [`CHANGELOG.md`](https://github.com/go-steer/core-agent/blob/main/CHANGELOG.md)

## What this site doesn't cover

- **Cogo** — Claude-Code-style TUI built on top of similar primitives. Different project; see [cogo.io](https://github.com/go-steer/cogo) docs.
- **ADK Go** — `core-agent` wraps Google's [Agent Development Kit](https://github.com/google/adk-go). For raw ADK primitives, see the upstream docs.
- **Model APIs** — for what models *can do*, see Google AI Studio / Vertex docs and Anthropic's docs. This site documents how `core-agent` talks to them, not the model surface itself.
