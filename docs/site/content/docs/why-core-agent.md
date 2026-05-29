---
title: Why core-agent
weight: 3
---

If you're building an agent in Go, your starting point is the [Google Agent Development Kit](https://pkg.go.dev/google.golang.org/adk) (ADK). ADK gives you a model interface, a tool-calling loop, a session abstraction, and some streaming primitives. That's roughly 30% of what a production agent needs. `core-agent` is the other 70% — the parts every team writes the second they take an ADK demo from "responds to my prompt" to "I'd let a real user touch this."

This page makes the case directly: what `core-agent` provides on top of ADK, what you'd otherwise build yourself, and when raw ADK (or something else) is actually the right call.

## The pitch in one paragraph

`core-agent` is an opinionated substrate. It wraps ADK with the parts that aren't about model intelligence but about being a real piece of software: a permission system humans trust, multiple model backends behind one config, a durable event log that survives crashes and powers replay, a way to load instructions and skills from your project tree, MCP server lifecycle management, autonomous-run budgets and termination signals, in-process and remote subagents, server-side built-in observability, **an in-process Bubble Tea TUI that lands you straight into a multi-turn chat with operator slash commands**, **context-management mechanisms (compaction, task-boundary checkpoints, agentic tool wrappers) that keep long sessions alive past the model's context wall**, **per-model token + cost tracking with layered pricing**, and a headless CLI that's useful in five minutes. Every one of those is something you'd write before shipping. Pulling them in as a library is a one-day exercise; building them yourself is two engineer-months.

## What you get out of the box

The pattern across these features is the same: ADK gives you the primitive (a `model.LLM`, a `tool.Tool`, a `session.Service`), and `core-agent` gives you the production wrapping that turns the primitive into a working surface.

### Provider, tooling, and permission infrastructure

**Multi-provider model selection.** ADK ships Gemini + Apigee adapters. `core-agent` adds Anthropic (first-party and Vertex-served) as a real `model.LLM` implementation, plus mock providers (`echo`, `scripted`) for credential-free testing, plus a `models.Resolve(cfg)` resolver that auto-detects which provider to use from your environment. One config field — `model.provider` — picks the backend; everything else is wired identically. The Anthropic adapter alone is ~1,500 lines you don't have to write.

**13-tool built-in catalog.** File (`read_file`, `read_many_files`, `write_file`, `edit_file`, `delete_file`, `stat`, `list_dir`), search (`glob`, `grep`), data + network (`json_query`, `fetch_url`), shell (`bash`), planning (`todo`) — all gated, all output-capped, all with prescriptive descriptions that tell the model "use INSTEAD OF `bash X`" so it routes through the structured tool rather than the shell. See [Built-in tools]({{< relref "/docs/reference/tools.md" >}}).

**Permission gate that humans can live with.** Three modes (`ask`, `allow`, `yolo`), pattern-based allow/deny lists, path-scope enforcement on file tools, a non-overridable bash denylist that catches `rm -rf /`-class mistakes, and a pluggable `Prompter` interface so the same gate works in a TTY, a headless CI run (with `--ask=auto`), a TUI, or a web frontend. Each tool call routes through the gate before it executes; rejections come back as model-visible errors so the agent can recover, not as silent failures. Writing this yourself takes two weeks and you'll get the bash-denylist escapes wrong.

**Instruction loading from the project tree.** `core-agent` looks for `AGENTS.md` (with `CLAUDE.md` / `GEMINI.md` as fallbacks) in both `~/.core-agent/` and the project directory, and prepends them as a system instruction. This is the convention nine out of ten coding agents now use; it means a user can drop one file into a repo and customize how the agent works for that project without writing any code.

**MCP servers, declaratively.** Drop a `.agents/mcp.json` describing your MCP servers; `core-agent` handles lifecycle (start, stop, restart on crash), supports both stdio and Streamable HTTP transports, namespaces tools (`<server>_<tool>` so two servers can both expose `search`), and routes every MCP tool call through the same permission gate. ADK has an MCP toolset; the lifecycle and config-driven wiring around it is what you'd build.

**Claude-compatible skills, with auto-discovery from two sources.** Drop a `SKILL.md` bundle into `.agents/skills/<name>/` (project-scoped) or `~/.core-agent/skills/` (user-global); the agent merges both at load time and invokes them on demand. Format is the one Anthropic published, so skills written for Claude Code work without modification. Three bundled meta-skills ship in `SKILLS/`: `cli-setup` (walks an operator through configuring `core-agent`), `autonomous-setup` (walks through unattended single-agent or multi-agent setup), `library-embedding` (walks a Go developer through `agent.New` + the seven extension points). The meta use case: an existing `core-agent` can use those skills to help an operator set up another `core-agent`.

### Long-session survivability

This is the v2.0 story — three mechanisms that together let a single session run for hours or days without the operator-painful "input tokens climb until the next turn errors out" pattern.

**Compaction at the context wall (Mechanism A).** Automatic, post-turn, threshold-gated at ~85% of the model's context window. The compactor calls a tool-less summarizer LLM, writes a five-section "teammate handover" summary to the event log as a boundary event, and slices prior history out of future model requests at request-construction time. The audit log is never mutated; the slicing happens in a wrapper around the runner's `session.Service`. Operator-driven via `/compact [focus]`.

**Subtasks + agentic tool wrappers (Mechanism B).** `Agent.RunSubtask(ctx, SubtaskSpec)` is a synchronous, single-purpose, fresh-context LLM call that sees ONLY its own `SystemPrompt` + `UserMessage`. The four bundled `agentic_*` wrappers (`agentic_read_file`, `agentic_fetch_url`, `agentic_grep`, `agentic_research`) route bulk tool output through a subtask on a (typically cheaper) model and return only the digest — raw 5,000-line file reads never enter the parent's context. Cost rolls up to the parent's tracker, so `/stats` reflects the full session spend.

**Task-boundary checkpoints (Mechanism C).** Closes the *other* failure mode compaction alone doesn't catch: "we finished a task and the model is still drowning in the prior task's exploration when the operator asks about something else." A model-facing `mark_task_done(detail)` tool the model self-invokes at natural task boundaries; the runtime writes a six-section completion record and slices the prior task's history. Operator-driven via `/done [note]`.

See [Context management]({{< relref "/docs/reference/context-management.md" >}}) for the full design.

### Operator UX

**In-process Bubble Tea TUI as the default surface.** Bare `core-agent` (no flags, stdin is a TTY) lands in a full multi-turn chat with thinking indicator, queue panel, slash command palette, contextual ESC behavior, three-state light/dark/auto theming, OSC-11 background detection, scrollable history, in-line markdown rendering, and configurable mouse handling. Lifted from [cogo](https://github.com/go-steer/cogo) (~7,100 LoC, Apache-2, same `go-steer` org); see [`embedded-tui-design-v2.md`](https://github.com/go-steer/core-agent/blob/main/docs/embedded-tui-design-v2.md). Fall through to a line-mode REPL with `core-agent --no-tui` or non-TTY stdin. Slim build (`go build -tags no_tui`) excludes the TUI tree entirely for headless K8s pods (~5 MB smaller binary).

**Operator slash commands** built into both the in-process TUI and the remote `core-agent-tui` attach client: `/help`, `/stats`, `/context`, `/compact`, `/done`, `/btw` (side queries that don't pollute the conversation), `/subagent`, `/interrupt`, `/wake`, `/inject`, `/tools`, `/memory`, `/skills`, `/mcp`, `/pricing`, `/permissions`, `/allow`, `/deny`, `/reload`, `/theme`, `/transcript`. See [Slash reference]({{< relref "/docs/cli/interactive/slash-reference.md" >}}).

**Input while streaming, with auto-continue.** Press Enter mid-turn to queue a message via `Agent.Inject`; the queue panel mirrors what's pending; when the current turn completes with a non-empty inbox the TUI auto-starts a follow-up turn with the queued notes framed as a system note. Soft cap on consecutive auto-continues. Closes the operator-painful "I noticed something three turns ago but had to wait for the agent to come up for air" gap.

**Mid-turn interrupt from any surface.** `/interrupt` slash + `Agent.Interrupt()` + attach-mode `POST /sessions/<sid>/interrupt` all cancel the in-flight model turn from any caller. Pre-existing tool runs, MCP calls, and background subagents unwind via the cancelled context. Bound to Esc on empty input.

**Per-model cost visibility.** When a session uses more than one model (typical pattern: parent on Pro/Opus, subtasks on Flash/Haiku via `--agentic-small-model`), both `/context` and `/stats` show a per-model breakdown sorted by descending cost. Surfaces the actual cost-efficiency win of routing subtasks to a cheaper tier.

**Layered pricing with auto-refresh.** Daily fetch from LiteLLM's `model_prices_and_context_window.json` (ETag-aware, network failures non-fatal). Layered lookup chain: `cfg.Model.Pricing[name]` → `.agents/pricing.json` → `~/.core-agent/pricing.json` → compiled-in fallback → longest-prefix match → `$—` (unknown). Hundreds of models covered out of the box. Operator slash commands `/pricing refresh` and `/pricing set <model> <input> <output>` for manual control.

### Runtime + storage

**Durable session + audit log with crash-resume.** `eventlog.Open(...)` returns a SQLite/Postgres/MySQL-backed `session.Service` plus a `Stream` with monotonic sequence numbers, `Since(seq)` replay, and `Watch(seq)` live-tail. A session lock makes concurrent resume safe (two processes can't both think they own a session). Crash-resume just works: restart, re-open the same session, the agent picks up where it left off. ADK's `session.InMemoryService` loses everything on process exit.

**Autonomous-run driver.** `agent.RunAutonomous` for unattended workers (batch jobs, CI tasks, scheduled scripts) with budget caps on turns, tokens, cost, and wallclock. Built-in `lifecycle` tool the model uses to declare it's done; `--ask=auto` so prompts like "ask before X" get a clean refusal in headless contexts instead of blocking forever. `ResumeAutonomous` picks up after a crash from the durable event log.

**Subagents, in-process and remote.** `WithSubagents([]*Agent)` registers each agent as a callable tool the parent's model can invoke synchronously. For dynamic spawning the parent decides at runtime, `BackgroundAgentManager` + the `spawn_agent` tool runs subagents in-process; the `RemoteAgentSpawner` interface lets you delegate to an entirely separate runtime (Scion adapter ships in `extras/scion-remote-agent/`). Subagent events stream into the parent's audit log under a branch label so the audit trail stays unified.

**Server-side built-in observability.** When Gemini's `GoogleSearch` or `URLContext` tools fire on the server side, `core-agent` surfaces them in the chat-style output (`↪ google_search: query: ...`) and as queryable eventlog rows (`Author="gemini/google_search"`). Same `↪` namespace reserved for Anthropic's server-side tools when they land in the SDK.

**Telemetry, opt-in.** OpenTelemetry export to console or OTLP. Off by default so a fresh invocation makes zero outbound calls beyond the model.

**A CLI and TUI that are useful immediately.** `core-agent -p "hello"` is the one-shot headless path; bare `core-agent` lands you in the in-process Bubble Tea TUI with conversation history preserved across turns. Both honor the same config, the same permissions, the same provider auto-detection. Most users never need to write Go code at all.

## Side-by-side

What ships with each substrate, scored from empty (○) through partial (◐) to provided (●):

| Capability                                          | Raw ADK Go | `core-agent` |
|-----------------------------------------------------|:---:|:---:|
| Gemini provider (API + Vertex)                      | ● | ● |
| Anthropic provider (first-party + Vertex)           | ○ | ● |
| Mock providers (echo / scripted) for testing        | ○ | ● |
| Provider auto-detection from env                    | ○ | ● |
| 13-tool built-in catalog (file/search/shell/net/…)  | ○ | ● |
| Permission gate (ask/allow/yolo + denylist)         | ○ | ● |
| Path-scope enforcement on file tools                | ○ | ● |
| Pluggable `Prompter` for approvals                  | ○ | ● |
| AGENTS.md / CLAUDE.md / GEMINI.md loading           | ○ | ● |
| MCP server lifecycle from config                    | ◐ | ● |
| Claude-compatible `SKILL.md` skills                 | ○ | ● |
| Skills auto-discovery (project + user-global)       | ○ | ● |
| Three bundled meta-skills (`SKILLS/`)               | ○ | ● |
| In-process Bubble Tea TUI (default on TTY)          | ○ | ● |
| `--no-tui` REPL fallback + `-tags no_tui` slim build| ○ | ● |
| Remote attach TUI (`core-agent-tui`)                | ○ | ● |
| Operator slash commands (`/stats`, `/btw`, etc.)    | ○ | ● |
| Input-while-streaming + auto-continue from inbox    | ○ | ● |
| Mid-turn interrupt (slash + lib + HTTP)             | ○ | ● |
| Context-window compaction (Mechanism A)             | ○ | ● |
| Subtasks + `agentic_*` tool wrappers (Mechanism B)  | ○ | ● |
| Task-boundary checkpoints (Mechanism C)             | ○ | ● |
| Per-model token + cost breakdown (`/stats`/`/context`)| ○ | ● |
| Layered pricing with daily LiteLLM refresh          | ○ | ● |
| Durable session (SQLite/Postgres/MySQL)             | ○ | ● |
| Event log (`Since(seq)` / `Watch(seq)` / replay)    | ○ | ● |
| Crash-resume via session lock                       | ○ | ● |
| Autonomous-run driver with budgets                  | ○ | ● |
| In-process subagents                                | ◐ | ● |
| Remote-subagent seam (`RemoteAgentSpawner`)         | ○ | ● |
| Server-side built-in projection (Gemini search)     | ○ | ● |
| OpenTelemetry export                                | ○ | ● |
| Headless CLI                                        | ○ | ● |
| Parallel tool-call dispatch                         | ● | ● |
| Multi-turn conversation                             | ● | ● |
| Tool-calling loop                                   | ● | ● |
| Streaming events                                    | ● | ● |

A ◐ on raw ADK means "the primitive is there but the production wiring is your job" — ADK has an `MCPToolset` type, for example, but it doesn't manage the server's lifecycle, restart it on crash, or read it from declarative config. A ◐ on `core-agent` would mean we've started but it's incomplete; there aren't any in the table above today.

## When you should not use core-agent

`core-agent` is opinionated. Three cases where the opinions don't match what you want:

1. **You want a TUI shape that diverges sharply from Bubble Tea + Charm conventions.** The in-process TUI is built on [bubbletea](https://github.com/charmbracelet/bubbletea) / [bubbles](https://github.com/charmbracelet/bubbles) / [lipgloss](https://github.com/charmbracelet/lipgloss) / [glamour](https://github.com/charmbracelet/glamour). The visual language is Claude-Code-shape: chat-style scrollback, status line, slash command palette, queue panel. If you need a fundamentally different aesthetic (vim-modal, full-screen split panes, terminal-graphics protocol, web-canvas-style) you'll be fighting the layout choices the lifted TUI baked in. Easier to write your own runner against the library and skip the TUI tree entirely (`-tags no_tui`).
2. **You want the absolute thinnest possible binary.** `core-agent` pulls Anthropic's SDK, the eventlog persistence layer (`database/sql` + drivers, transitively), and Bubble Tea + friends for the in-process TUI. The slim build (`go build -tags no_tui`) excludes the TUI dependencies (~5 MB savings) and a headless K8s pod can run it cleanly, but raw ADK is still smaller if you use one provider, one tool, no persistence, and no TUI. Most teams don't actually care about this; if you do, the gap is real.
3. **You need an orchestrator, not an agent.** `core-agent` runs *one* agent (with subagents). If you're building LangGraph-style multi-agent workflows where the graph topology is the main abstraction, you'll find `core-agent`'s shape constraining. ADK has more flexible workflow agents (`SequentialAgent`, `ParallelAgent`, `LoopAgent`) you'd build on directly.

For everything else — chat assistants, coding agents, autonomous workers, batch processors, internal tools, anything that's "agent talks to model, calls tools, persists state, survives long sessions, surfaces in a TUI or headless" — starting from `core-agent` saves you the two engineer-months and lets you focus on the things that actually differentiate your product.

## Where to go next

- **[Getting started]({{< relref "/docs/getting-started.md" >}})** — install and run your first agent in five minutes.
- **[Interactive quickstart]({{< relref "/docs/cli/interactive/quickstart.md" >}})** — operator workflow in the in-process TUI; AGENTS.md, skills, MCP, slash commands, in 15 minutes.
- **[Autonomous quickstart]({{< relref "/docs/cli/autonomous/quickstart.md" >}})** — first working unattended agent in 15 minutes.
- **[Using the library]({{< relref "/docs/library/_index.md" >}})** — embed `core-agent` in your own Go binary; extension points walked through with worked examples.
- **[Library API]({{< relref "/docs/library/api.md" >}})** — the exhaustive reference for every exported type and option.
- **[Agent design]({{< relref "/docs/agent-design/_index.md" >}})** — prescriptive patterns: when to use skills vs. `AGENTS.md` rules, how to get the model to use subagents and agentic wrappers efficiently, cost-efficiency tips.
- **[Skills library]({{< relref "/docs/skills-library/_index.md" >}})** — three bundled Claude-Skills bundles + install instructions; an existing agent can walk an operator through setting up another one.
