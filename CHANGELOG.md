# Changelog

All notable changes to `core-agent` are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Stability promise

The public API of `core-agent` is the exported surface of these packages:

- `agent`, `eventlog`, `tools`, `permissions`, `config`, `models` (+ `models/anthropic`, `models/gemini`, `models/mock`), `recording`, `runner`, `session`, `usage`, `instruction`, `mcp`, `skills`, `telemetry`

Pre-1.0, breaking changes are possible at any minor version (`v0.X`). When we make one, the change is called out in this file under **Changed** or **Removed**, and non-trivial removals get a one-version deprecation period when feasible. Patch versions (`v0.X.Y`) are bug fixes only.

The `extras/` adapters (`extras/scion-agent/`, `extras/ax-agent/`) and the `internal/` packages they ship with track `core-agent`'s minor version but do not promise their own stability — adapters target moving runtimes (Scion, AX) and follow whatever those upstream projects do.

---

## [Unreleased]

### Fixed

- **`models/gemini`** — set `Config.ToolConfig.IncludeServerSideToolInvocations = true` whenever the `builtinsLLM` wrapper injects server-side built-ins (`google_search` / `url_context` / `code_execution`) alongside any function-calling tools. Without this flag, Gemini 3+ rejects the combined request with `Please enable tool_config.include_server_side_tool_invocations to use Built-in tools with Function calling`, blocking `--provider=gemini` for any consumer using `tools.Default()`. Surfaced by the v1.0.0 smoke pass (`docs/v1-acceptance.md`). Fix in `models/gemini/builtins.go`.

### Documentation

- **Gemini 3.0+ requirement** added to `docs/site/content/docs/providers.md` and `docs/site/content/docs/configuration.md`. When combining `core-agent`'s default tool suite with the Gemini provider's built-in tools (both default-on), the Gemini API requires a 3.0-or-later model — Gemini 2.5 rejects the combination outright. Workarounds for consumers who must use Gemini 2.5: `--no-builtin-tools` (drops the function-calling suite) or library-level `gemini.WithGoogleSearch(false)` + `gemini.WithURLContext(false)` (drops the server-side built-ins). Default model already pins `gemini-3.1-pro-preview`, so consumers who don't override never hit this.

---

## [0.1.0] — 2026-05-16

First tagged release. Three milestones of work landed on `main` before this tag; the release is the consolidation rather than a discrete piece of work.

### Added

#### Core library (M1 + M2)
- **`agent` package** — wraps ADK's `llmagent` + `runner` with the `Option` pattern: `WithAppName`, `WithName`, `WithDescription`, `WithInstruction`, `WithStreaming`, `WithSession`, `WithTools`, `WithToolsets`, `WithSystemInstructionPrefix`. `Agent.Run(ctx, prompt)` streams ADK events for one turn.
- **`models` package** — `Provider` interface + registry. Backends: `gemini` (api.google.com + Vertex), `anthropic` (api.anthropic.com + `anthropic-vertex`), `mock` (echo + scripted).
- **`config` package** — `.agents/config.json` schema + discovery + atomic persist. Per-tool output caps via `ToolOutput.PerTool`.
- **`permissions` package** — ask / allow / yolo gate, pattern grammar, path scope, non-overridable bash denylist, `Prompter` interface.
- **`tools` package** — eight default-on built-ins (`read_file`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash`, `todo`) + `GateToolset` wrapper bridging the gate to ADK toolsets + `Truncate` helper.
- **`mcp` package** — `mcp.json` schema, stdio + Streamable HTTP transports, env-var interpolation, namespacing.
- **`skills` package** — Claude-compatible `SKILL.md` discovery → ADK `skilltoolset`.
- **`instruction` package** — `AGENTS.md` / `CLAUDE.md` / `GEMINI.md` fallback loader; user-global memory at `~/.core-agent/AGENTS.md`.
- **`telemetry` package** — opt-in OpenTelemetry exporter setup (console / OTLP / none).
- **`usage` package** — per-turn token tracker + cost helpers + a built-in pricing table for Gemini.
- **`session` package** — JSON transcript persistence at `.agents/sessions/<timestamp>.json` for one-shot runs.
- **`runner` package** — `Headless` (one-shot) + `REPL` (multi-turn) drivers + `WriteEvents` event-streaming helper for library callers.
- **`recording` package** — `recording.NewRecorder(m, w)` LLM-wire recorder; pairs with `mock.NewScripted` for credential-free replay.
- **`cmd/core-agent`** — bundled CLI: `--provider`, `-m`, `-p`, `--no-builtin-tools`, `--disable-tools`, `--script`, `--script-strict`, `--record-to`, `--color`, `--ask`, `--session-db`, `--session-db-path`.

#### M3 — Autonomy + durable sessions + subagents
- **`tools.NewLifecycleTool`** — generic state-emission primitive the model uses to signal "thinking" / "blocked" / "done" / custom labels. Consumer-supplied handler decides where the events go.
- **`tools.NewAskUserTool`** + three built-in `Prompter`s (`StdinPrompter`, `RefusePrompter`, `StaticPrompter`) for in-turn human consultation. CLI flag `--ask=stdin|auto|off`.
- **`agent.RunAutonomous`** — multi-turn driver for unattended runs. Budgets via `WithMaxTurns / WithMaxTokens / WithMaxCost / WithMaxWallclock / WithPerTurnTimeout`. Retry policy via `WithRetryPolicy` (`AbortRun` / `RetryTurn` / `SkipTurn`). Permissions deadlock guard via `WithPermissionsGate`. Returns structured `RunResult{Reason, Turns, Tokens, Cost, Duration, FinalText, DoneDetail}`.
- **`agent.WithSessionService` + `agent.WithEventLog`** — durable session backend. `eventlog.Open(ctx, dialector)` returns a `*Handle` bundling a `session.Service` (wraps ADK's GORM-backed `database.SessionService`) and a `Stream` with monotonic seq numbers. Multi-driver via SQLite (pure-Go through `glebarez/sqlite`, no CGO) / MySQL / Postgres.
- **`eventlog.Stream`** — `Append` / `Since(fromSeq, opts...)` / `Watch(fromSeq, opts...)` / `Close`. Query options: `ForSession`, `WithSessionTree`, `WithBranchPrefix`, `WithAuthor`, `WithAuthorSuffix`, `WithLimit`. WAL mode default for SQLite. Polling `Watch` at 200ms (`WithWatchInterval` to override).
- **`eventlog.SessionLock`** — exclusive lease on `(app, user, session)` via `Handle.AcquireLock`. 5s heartbeat, 30s staleness window, automatic theft on stale leases. Acquired by `ResumeAutonomous`; concurrent attempts return `ErrSessionLocked` with the holder identifier.
- **`agent.ResumeAutonomous`** — crash-resume for autonomous runs. Per-turn checkpoint events (`Author="<binary>/autonomous"`) land in the durable log; resume reads the latest checkpoint and continues from the next turn. Cross-binary resume via `WithAuthorSuffix("/autonomous")`. Terminal-state short-circuit only on `Completed` so budget-exhausted runs can be resumed with a higher cap.
- **`agent.WithSubagents([]*Agent)`** + **`agent.NewSubagentTool`** — in-process delegation. Each subagent becomes a callable tool the parent's model invokes by name. Subagent runs in a derived session row (`<parent>:sub:<branch>`) with `Branch="<parent>.<sub>"` for branch-scoped audit queries. Depth cap of 2 (configurable) enforced via `context.Context` value.
- **CLI flags** — `--ask=stdin|auto|off`, `--session-db`, `--session-db-path` (default `~/.<binary>/sessions.db` derived from `os.Executable()`).
- **Two adapters in `extras/`** — `extras/scion-agent/` packages core-agent for Scion's container runtime (lifecycle status, `--input`, `sciontool_status` tool, `--session-db` parity). `extras/ax-agent/` packages it as an AX (Agent eXecutor) gRPC remote agent (lives on the `axplore` branch since `github.com/google/ax` is currently private; same `--session-db` parity).

### Documentation

- New Hugo site pages: [Autonomous runs](https://go-steer.github.io/core-agent/docs/autonomous/), [Sessions and event log](https://go-steer.github.io/core-agent/docs/sessions/). Plus comprehensive [Library API](https://go-steer.github.io/core-agent/docs/library-api/) covering subagents, autonomous, durable sessions, prompters, MCP, skills, telemetry, and transcripts.
- `docs/DESIGN.md` — design notes covering goals/non-goals, package layout, provider interface, multi-turn handling, built-in tools, subagent tool, durable sessions, autonomous runs, recording vs eventlog, adapters.
- Plan + decisions docs: `docs/autonomous-plan.md`, `docs/eventlog-plan.md`, `docs/eventlog-decisions.md`, `docs/subagents-plan.md`, `docs/tools-plan.md`, `docs/m3-followups-plan.md`, `docs/m3-followups-decisions.md`. Plan docs preserved as historical context with status headers; decisions docs are the canonical "what shipped + why" record.

### Examples

- `examples/basic/` — minimal one-turn agent
- `examples/with-tools/` — agent with the built-in tool suite
- `examples/streaming/` — `runner.WriteEvents` for chat-style output
- `examples/replay/` — `mock.NewScripted` against a recorded transcript
- `examples/autonomous/` — `RunAutonomous` end-to-end with scripted mock provider
- `examples/autonomous-resume/` — Phase 1 + Phase 2 demonstrating crash-resume against SQLite
- `examples/with-subagent/` — parent + research subagent demonstrating branch-scoped audit log

### Known gaps (not in this release; tracked for v0.2 candidates)

- **Subagent cost rollup** into the parent's `usage.Tracker` — subagent runs track usage internally; surfacing it back to the parent is a follow-up.
- **Postgres / MySQL integration tests** — multi-driver claim is verified for SQLite only. Library callers can swap dialectors today; CI doesn't yet test Postgres.
- **Real-LLM end-to-end smoke** — examples use scripted mocks; no automated smoke against actual Gemini / Anthropic.
- **Glob `**` recursive shorthand** — explicitly out of scope (stdlib-only constraint). Workaround: explicit walk root.
- **Bubble Tea TUI** + slash-command framework beyond `/exit` `/quit` — consumer concerns, not library.
- **Anthropic feature coverage** — extended/adaptive thinking, structured outputs, server-side tools (web_search, code_execution), vision.
- **Amazon Bedrock + Claude Platform on AWS** — additional Anthropic backends.
- **Auto-detection for `anthropic-vertex`** from generic GCP env vars — explicit-only today.
- **Mid-run pause/resume** for `RunAutonomous` — across-turn crash-resume shipped; mid-turn is a different design.
- **Native push for `Stream.Watch`** (Postgres `LISTEN/NOTIFY`, SQLite `update_hook`) — polling at 200ms today.

[0.1.0]: https://github.com/go-steer/core-agent/releases/tag/v0.1.0
