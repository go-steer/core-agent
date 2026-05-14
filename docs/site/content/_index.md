---
title: core-agent
toc: false
---

# core-agent

A reusable Go base agent built on the [Google Agent Development Kit](https://pkg.go.dev/google.golang.org/adk).

`core-agent` is the bottom layer for any project that needs a multi-turn LLM agent in Go. It ships the wiring — model providers, MCP servers, skills, instruction loading, permission gating, telemetry, transcript persistence — so consuming projects can focus on their own tools and product logic.

It deliberately is **not** a finished agent. There are no built-in bash / file / grep tools, no UI, no slash commands beyond `/exit`. What you get is the substrate.

[Get started →](docs/getting-started/) &nbsp; [View on GitHub →](https://github.com/go-steer/core-agent)

---

## What it gives you

- **Multiple model providers** — Gemini API, Vertex AI (Gemini), Anthropic / Claude (first-party + Vertex AI). Auto-detected from environment.
- **AGENTS.md instruction loading** — system prompt prefix from `AGENTS.md` (with `CLAUDE.md` / `GEMINI.md` fallbacks).
- **MCP servers** — declarative `.agents/mcp.json`; stdio and Streamable HTTP; namespaced and gated.
- **Claude-compatible skills** — `SKILL.md` bundles in `.agents/skills/<name>/`.
- **Permission gate** — ask / allow / yolo modes; built-in bash denylist; per-tool allowlists; path scope.
- **Telemetry** — opt-in OpenTelemetry export (console / OTLP); off by default.
- **Headless CLI** — one-shot via `-p`; multi-turn REPL by default.

## When to use it

Use `core-agent` when you're building **a Go agent** and you want a choice of model backends, the standard `AGENTS.md` / MCP / skills / permissions infrastructure without writing it yourself, and clean extension points for your own tools.

If you want a polished interactive coding agent out of the box (like `cogo`), this is the wrong layer — it's the foundation those things would be built on, not a replacement for them.

## Status

Early. APIs may change. Track milestones in the [README's milestone log](https://github.com/go-steer/core-agent#milestones).
