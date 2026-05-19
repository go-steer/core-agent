---
title: Why core-agent
weight: 3
---

If you're building an agent in Go, your starting point is the [Google Agent Development Kit](https://pkg.go.dev/google.golang.org/adk) (ADK). ADK gives you a model interface, a tool calling loop, a session abstraction, and some streaming primitives. That's roughly 40% of what a production agent needs. `core-agent` is the other 60% — the parts every team writes the second they take an ADK demo from "responds to my prompt" to "I'd let a real user touch this."

This page makes the case directly: what `core-agent` provides on top of ADK, what you'd otherwise build yourself, and when raw ADK (or something else) is actually the right call.

## The pitch in one paragraph

`core-agent` is an opinionated substrate. It wraps ADK with the parts that aren't about model intelligence but about being a real piece of software: a permission system humans trust, multiple model backends behind one config, a durable event log that survives crashes and powers replay, a way to load instructions and skills from your project tree, MCP server lifecycle management, autonomous-run budgets and termination signals, mid-turn interrupts, in-process and remote subagents, server-side built-in observability, and a headless CLI that's useful in five minutes. Every one of those is something you'd write before shipping. Pulling them in as a library is a one-day exercise; building them yourself is two engineer-months.

## What you get out of the box

The pattern across these features is the same: ADK gives you the primitive (a `model.LLM`, a `tool.Tool`, a `session.Service`), and `core-agent` gives you the production wrapping that turns the primitive into a working surface. A few examples:

**Multi-provider model selection.** ADK ships Gemini + Apigee adapters. `core-agent` adds Anthropic (first-party and Vertex-served) as a real `model.LLM` implementation, plus mock providers (`echo`, `scripted`) for credential-free testing, plus a `models.Resolve(cfg)` resolver that auto-detects which provider to use from your environment. One config field — `model.provider` — picks the backend; everything else is wired identically. The Anthropic adapter alone is ~1,500 lines you don't have to write.

**Permission gate that humans can live with.** Three modes (`ask`, `allow`, `yolo`), pattern-based allow/deny lists, path-scope enforcement on file tools, a non-overridable bash denylist that catches `rm -rf /`-class mistakes, and a pluggable `Prompter` interface so the same gate works in a TTY, a headless CI run (with `--ask=auto`), a TUI, or a web frontend. Each tool call routes through the gate before it executes; rejections come back as model-visible errors so the agent can recover, not as silent failures. Writing this yourself takes two weeks and you'll get the bash-denylist escapes wrong.

**Instruction loading from the project tree.** `core-agent` looks for `AGENTS.md` (with `CLAUDE.md` / `GEMINI.md` as fallbacks) in both `~/.core-agent/` and the project directory, and prepends them as a system instruction. This is the convention nine out of ten coding agents now use; it means a user can drop one file into a repo and customize how the agent works for that project without writing any code.

**MCP servers, declaratively.** Drop a `.agents/mcp.json` describing your MCP servers; `core-agent` handles lifecycle (start, stop, restart on crash), supports both stdio and Streamable HTTP transports, namespaces tools (`<server>_<tool>` so two servers can both expose `search`), and routes every MCP tool call through the same permission gate. ADK has an MCP toolset; the lifecycle and config-driven wiring around it is what you'd build.

**Claude-compatible skills.** Drop a `SKILL.md` bundle into `.agents/skills/<name>/`; the agent invokes it on demand. Format is the one Anthropic published, so skills written for Claude Code work without modification.

**Durable session + audit log with crash-resume.** `eventlog.Open(...)` returns a SQLite/Postgres/MySQL-backed `session.Service` plus a `Stream` with monotonic sequence numbers, `Since(seq)` replay, and `Watch(seq)` live-tail. A session lock makes concurrent resume safe (two processes can't both think they own a session). Crash-resume just works: restart, re-open the same session, the agent picks up where it left off. ADK's `session.InMemoryService` loses everything on process exit.

**Autonomous-run driver.** `agent.RunAutonomous` for unattended workers (batch jobs, CI tasks, scheduled scripts) with budget caps on turns, tokens, cost, and wallclock. Built-in `lifecycle` tool the model uses to declare it's done; `--ask=auto` so prompts like "ask before X" get a clean refusal in headless contexts instead of blocking forever.

**Mid-turn interrupt, programmatic and interactive.** `StartAutonomous(...)` returns a handle with `Pause()` / `Resume()` / `Stop()` / `Inject(message)`. The bundled REPL gets Claude Code-style ESC-cancels-turn and double-Ctrl+C-exits gestures from the same machinery. You can yank a runaway agent without losing context.

**Subagents, in-process and remote.** `WithSubagents([]*Agent)` registers each agent as a callable tool the parent's model can invoke synchronously. For dynamic spawning the parent decides at runtime, `BackgroundAgentManager` + the `spawn_agent` tool runs subagents in-process; the `RemoteAgentSpawner` interface lets you delegate to an entirely separate runtime (Scion adapter ships in `extras/scion-remote-agent/`). Subagent events stream into the parent's audit log under a branch label so the audit trail stays unified.

**Server-side built-in observability.** When Gemini's `GoogleSearch` or `URLContext` tools fire on the server side, `core-agent` surfaces them in the chat-style output (`↪ google_search: query: ...`) and as queryable eventlog rows (`Author="gemini/google_search"`). Same `↪` namespace reserved for Anthropic's server-side tools when they land in the SDK.

**Telemetry, opt-in.** OpenTelemetry export to console or OTLP. Off by default so a fresh invocation makes zero outbound calls beyond the model. Per-turn token + cost tracking via the `usage` package with a built-in price table you can override per model.

**A CLI that's useful immediately.** `core-agent -p "hello"` is the one-shot path; bare `core-agent` is a REPL with conversation history preserved across turns. Both honor the same config, the same permissions, the same provider auto-detection. Most users never need to write Go code at all.

## Side-by-side

What ships with each substrate, scored from empty (○) through partial (◐) to provided (●):

| Capability                                          | Raw ADK Go | `core-agent` |
|-----------------------------------------------------|:---:|:---:|
| Gemini provider (API + Vertex)                      | ● | ● |
| Anthropic provider (first-party + Vertex)           | ○ | ● |
| Mock providers (echo / scripted) for testing        | ○ | ● |
| Provider auto-detection from env                    | ○ | ● |
| Built-in tool suite (read/write/edit/grep/bash/…)   | ○ | ● |
| Permission gate (ask/allow/yolo + denylist)         | ○ | ● |
| Path-scope enforcement on file tools                | ○ | ● |
| Pluggable `Prompter` for approvals                  | ○ | ● |
| AGENTS.md / CLAUDE.md / GEMINI.md loading           | ○ | ● |
| MCP server lifecycle from config                    | ◐ | ● |
| Claude-compatible `SKILL.md` skills                 | ○ | ● |
| Durable session (SQLite/Postgres/MySQL)             | ○ | ● |
| Event log (`Since(seq)` / `Watch(seq)` / replay)    | ○ | ● |
| Crash-resume via session lock                       | ○ | ● |
| Autonomous-run driver with budgets                  | ○ | ● |
| Mid-turn interrupt + inject                         | ○ | ● |
| In-process subagents                                | ◐ | ● |
| Remote-subagent seam (`RemoteAgentSpawner`)         | ○ | ● |
| Server-side built-in projection (Gemini search)     | ○ | ● |
| Per-turn token + cost tracking                      | ○ | ● |
| OpenTelemetry export                                | ○ | ● |
| Headless CLI + REPL                                 | ○ | ● |
| Parallel tool-call dispatch                         | ● | ● |
| Multi-turn conversation                             | ● | ● |
| Tool-calling loop                                   | ● | ● |
| Streaming events                                    | ● | ● |

A ◐ on raw ADK means "the primitive is there but the production wiring is your job" — ADK has an `MCPToolset` type, for example, but it doesn't manage the server's lifecycle, restart it on crash, or read it from declarative config. A ◐ on `core-agent` would mean we've started but it's incomplete; there aren't any in the table above today.

## When you should not use core-agent

`core-agent` is opinionated. Three cases where the opinions don't match what you want:

1. **You're shipping a TUI as your primary interface.** `core-agent`'s public API is library + headless CLI; there's no Bubble Tea / lipgloss surface. If you want one, look at [cogo](https://github.com/go-steer/cogo) — it's the same team's TUI built on similar primitives, and the eventual plan is to back it with `core-agent` directly.
2. **You want the absolute thinnest possible binary.** `core-agent` pulls Anthropic's SDK, the eventlog persistence layer (`database/sql` + drivers, transitively), and a few other deps for the features above. If your agent uses one provider and one tool and you're allergic to dependencies, raw ADK gives you a smaller binary. Most teams don't actually care about this; if you do, the gap is real.
3. **You need an orchestrator, not an agent.** `core-agent` runs *one* agent (with subagents). If you're building LangGraph-style multi-agent workflows where the graph topology is the main abstraction, you'll find `core-agent`'s shape constraining. ADK has more flexible workflow agents (`SequentialAgent`, `ParallelAgent`, `LoopAgent`) you'd build on directly.

For everything else — chat assistants, coding agents, autonomous workers, batch processors, internal tools, anything that's "agent talks to model, calls tools, persists state, surfaces in a UI" — starting from `core-agent` saves you the two engineer-months and lets you focus on the things that actually differentiate your product.

## Where to go next

- **[Getting started](../getting-started/)** — install and run your first agent in five minutes.
- **[User guide](../user-guide/)** — configure providers, give your agent a personality via `AGENTS.md`, wire up skills and MCP servers.
- **[Library guide](../library-guide/)** — embed `core-agent` in your own Go binary; the extension points (`Prompter`, `RemoteAgentSpawner`, custom tools and providers, session services) walked through with worked examples.
- **[Library API](../library-api/)** — the exhaustive reference for every exported type and option.
