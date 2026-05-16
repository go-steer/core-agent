# Changelog

All notable changes to `core-agent` are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Stability promise

The public API of `core-agent` is the exported surface of these packages:

- `agent`, `eventlog`, `tools`, `permissions`, `config`, `models` (+ `models/anthropic`, `models/gemini`, `models/mock`), `recording`, `runner`, `session`, `usage`, `instruction`, `mcp`, `skills`, `telemetry`

Pre-1.0, breaking changes are possible at any minor version (`v0.X`). When we make one, the change is called out in this file under **Changed** or **Removed**, and non-trivial removals get a one-version deprecation period when feasible. Patch versions (`v0.X.Y`) are bug fixes only.

The `extras/` adapters (`extras/scion-agent/`, `extras/ax-agent/`) and the `internal/` packages they ship with track `core-agent`'s minor version but do not promise their own stability — adapters target moving runtimes (Scion, AX) and follow whatever those upstream projects do.

---

## [Unreleased]

---

## [1.3.0] — 2026-05-16

Interrupt machinery — both **programmatic** (for harness embedding
like Scion) and **interactive** (Claude Code-style ESC mid-turn in
the bundled CLI's REPL). Two new public library surfaces, a Scion
adapter rewrite that uses one of them, and a raw-mode terminal
handler that gives the bundled REPL ESC-cancels-turn /
double-Ctrl+C-exits gestures the way every interactive agent CLI on
the market does.

### Added

- **`Agent.Inject(message)` + `Agent.InboxArrived()`** — per-agent queue any caller (harness goroutine, HTTP handler, orchestrator gRPC stream, test fixture) pushes messages onto. The pre-turn drain inside `Agent.Run` prepends queued messages as an `[Inbox]` block above the prompt the model sees, sibling to the v1.2.0 `[Background reports]` block. Drop-oldest backpressure at the soft cap (256) so a stuck consumer can't deadlock the agent. `InboxArrived()` returns a 1-buffer notify channel so harnesses can wait for new input instead of polling.
- **`agent.StartAutonomous(ctx, build, goal, opts...)`** — fire-and-return constructor that runs the autonomous loop in a goroutine and returns an `*AutonomousHandle`. `RunAutonomous` keeps working unchanged (synchronous convenience that wraps `StartAutonomous(...).Wait()`).
- **`AutonomousHandle`** — programmatic control over a running autonomous loop. `Pause()` blocks at the next pre-turn checkpoint (current turn finishes normally); `Resume()` unblocks. `Stop()` cancels via ctx (idempotent; tears down even when paused). `Inject(message)` delegates to the underlying `Agent.Inject` so harnesses can push messages to a running loop. `Status()` reports `Running` / `Paused` / `Stopped` / `Completed` / `Failed`. `Wait()` blocks until terminal and returns the same `RunResult` shape `RunAutonomous` returns. `Done()` exposes the goroutine-exit channel for select-style integration.
- **`agent.WithBeforeTurn(cb)` AutonomousOption** — the hook `AutonomousHandle` uses internally to implement Pause. Library callers can use it directly for rate limits, external approvals, or other gating logic that runs at the per-turn checkpoint cadence.
- **Pause/Resume audit events** — `Pause()` / `Resume()` emit synthetic events to the eventlog (`Author="<binary>/autonomous"`, `CustomMetadata.kind="paused"|"resumed"`) when one is wired. Empty `Content.Role` so ADK's content processor skips them from LLM context. New helper `emitNoteEvent` in `agent/checkpoint.go` for this pattern.
- **`extras/scion-agent` Scion adapter rewrite** — replaces the between-turns stdin scan with a background goroutine that reads stdin and calls `Agent.Inject` for each line. Main loop waits on `Agent.InboxArrived()` and runs a turn with prompt `"continue"`; the pre-turn drain produces the `[Inbox]` block from queued messages. Messages arriving during an in-flight turn no longer block — they queue and land on the next turn. `--input` still seeds the first turn (now via `Inject` before the loop starts).
- **`examples/autonomous-handle/`** — credential-free demo of the handle API. Uses a thin slow-LLM wrapper around the echo mock so the Pause window is visible. Demonstrates `StartAutonomous` → `Pause` → `Inject` → `Resume` → `Wait`.
- **`dev/smoke/06-inject-autonomous.sh`** — smoke wrapping `examples/autonomous-handle`. No credentials required; safe to run anywhere.
- **Mid-turn interrupt in the bundled REPL** — pressing **ESC** during an in-flight turn cancels just that turn's context; the model's LLM call returns `Canceled`, conversation history is preserved (ADK streams events into the session as they arrive, so partial state survives), and the user gets the `> ` prompt back to type a new direction. Pressing **Ctrl+C** does the same; a second Ctrl+C within 1 second exits the REPL cleanly (Claude Code / gemini-cli convention). The bundled CLI's REPL auto-enables this when stdin is a TTY; piped or non-TTY use falls back silently to the legacy single-Ctrl+C-exits behavior. Implementation lives in `runner/interrupt.go` (package-private `turnInterrupter`); uses `golang.org/x/term`'s `MakeRaw` for cross-platform raw mode. Tool calls in flight when the cancel fires are best-effort: `bash` (which uses `exec.CommandContext`) cancels promptly; tools that ignore ctx finish their in-flight work before the loop unwinds. Tested with 11 state-machine unit cases including the double-Ctrl+C window, hint deduplication, and non-TTY fallback.

### New direct dependency

- `golang.org/x/term` — needed for `MakeRaw`/`Restore` to gate the REPL into raw input mode during a turn. Well-maintained Go-team package; was already transitively available through other dependencies.

### Deferred (out of scope for v1.3.0)

- **`AutonomousHandle.Redirect(newGoal)`** — hard interrupt + restart with a new goal while preserving conversation context. Workaround in v1.3.0: `handle.Stop()` then `StartAutonomous(newGoal)` with the same agent (the eventlog carries history; the new run sees it). Promote to a first-class method when a consumer hits the seam.
- **`extras/scion-remote-agent/` reference `RemoteAgentSpawner`** for sibling-container spawning via Scion's Hub HTTP API or CLI shell-out. The v1.2.0 `agent.RemoteAgentSpawner` interface is the seam; the implementation choice (HTTP vs CLI) depends on the deployment model and should be made with Scion-side input.
- **Concurrent task multiplexing per container** — today one Scion container = one logical agent. If Scion ever wants to multiplex (cost optimization), session multiplexing would be needed.
- **Lifecycle status taxonomy enrichment** — `sciontool_status` currently emits four sticky states. A richer taxonomy (progress %, ETA, blocking-on-what) is worth doing but should be designed against what Scion's UI actually wants to display.
- **REPL `/inject` slash command** — interactive UX; library-only for v1.3.0.

---

## [1.2.0] — 2026-05-16

Dynamic in-process background subagents (the parent agent's model spawns them at runtime via a tool call, providing system prompt + goal + tools) plus a consumer-pluggable remote-spawner seam for out-of-process subagents (gRPC, K8s Jobs, Cloud Run, …). Subagent reports flow back to the parent through both a synchronous OnAlert hook (for inline display) and a pre-turn drain that prepends alerts to the parent's next prompt.

Subagent permissions inherit the parent's `*permissions.Gate` wholesale; ask-mode prompts include a `[<subagent-name>]` source attribution so the human approving the call knows which agent is asking; concurrent prompt access is serialized through a mutex so background subagents can't race for `os.Stdin`. Bounded permission subsets with parent-as-arbiter is deferred to v1.3+.

### Added

- **`agent.BackgroundAgentManager`** — per-parent registry that owns spawned subagent lifecycles. Constructor `agent.NewBackgroundAgentManager(opts...)` requires `WithBackgroundProvider(provider, modelID)`; optional knobs cover the permissions gate (`WithBackgroundGate`), the catalog of tools subagents may request (`WithBackgroundCatalog`), depth cap (`WithBackgroundMaxDepth`, default 2), concurrency cap (`WithBackgroundMaxConcurrent`, default 8), default per-subagent budgets (`WithBackgroundDefaultBudgets`, default 50 turns / $1.00 / 10 min), and alert channel buffer (`WithBackgroundAlertBuffer`, default 256). Lifecycle methods: `Spawn / List / Get / Stop / Alerts / OnAlert / PrependPendingAlerts / Close`. Drop-oldest backpressure on the alert channel.
- **`agent.WithBackgroundManager(mgr)` Option** — attaches the manager to the parent. Inside `agent.New`, the manager's `attachParent` is called so subsequent `Spawn` calls can read the parent's session triple and session.Service.
- **`agent.Agent.Run` pre-turn alert drain** — when a manager is wired, pending background alerts are drained (non-blocking) and prepended to the prompt the underlying ADK runner sees, so the parent's model always sees what its subagents reported before deciding what to do next. New helper `Agent.BackgroundManager()` returns the wired manager.
- **`agent.NewSpawnAgentTool` + companions** — the four model-facing tools the parent's model uses: `spawn_agent` (launch), `list_agents` (introspect), `check_agent` (detailed status + final result), `stop_agent` (cancel). `agent.NewBackgroundSpawnTools(mgr)` returns all four for one-line CLI wiring.
- **`agent.RemoteAgentSpawner` interface** — consumer-pluggable seam for out-of-process spawning, mirroring the `tools.Prompter` shape. Implement `Spawn(ctx, spec) (RemoteAgentHandle, error)` against your substrate; core-agent stays transport-agnostic. The handle's `Events()` channel feeds into the same alert pipeline as in-process subagents via `agent.NewSpawnRemoteAgentTool(spawner, mgr)`. `agent.RefuseRemoteAgentSpawner(reason)` is the analog of `tools.RefusePrompter` for headless / unconfigured runs.
- **`permissions.StdinPrompter` source attribution** — new `Source string` field on `permissions.PromptRequest`. When non-empty, `StdinPrompter`'s heading reads `[<source>] <tool> wants to ...` so the human knows which subagent triggered the prompt. The gate populates `Source` from a `permissions.WithSubagentSource(ctx, name)` context value the spawn machinery stamps on every subagent's ctx. `permissions.SubagentSourceFromContext(ctx)` is the public reader.
- **`permissions.Serialize(p Prompter) Prompter`** — wraps any `Prompter` in a mutex so concurrent `AskApproval` calls run one at a time. Necessary when the gate is shared across background subagents that might prompt the same underlying medium (`os.Stdin`) at the same time. `permissions.PrompterFunc` adapter added for one-off prompters.
- **`runner.FormatAlertLine(from, kind, text)`** — exposed formatter so consumers wiring their own alert sinks render lines identically to the bundled CLI's REPL. `runner.AnsiMagenta()` exposes the matching color. REPL auto-installs an `OnAlert` hook that writes `↪ <from> <kind>: <text>` to stderr in magenta when a `BackgroundAgentManager` is wired.
- **`cmd/core-agent --no-background-agents`** — opt-out flag for the bundled CLI. Default is enabled — `spawn_agent` / `list_agents` / `check_agent` / `stop_agent` ship by default and the model can decide when to use them.
- **`examples/background-monitor/`** — credential-free end-to-end demo of the manager API using the echo mock provider. Spawns two stub subagents, exercises the OnAlert hook + pre-turn drain.

### Deferred (out of scope for v1.2.0)

- **Bounded permission subsets + parent-as-arbiter.** v1.2.0 inherits the parent gate wholesale. The richer model (subagent gets a subset, out-of-subset requests bubble up to the parent's model for a decision) is a v1.3+ feature. Per-subagent gate construction + a cross-agent permission-request message type are the design pieces.
- **Persistence across main-agent restarts.** Background subagent state is process-local. Cross-restart resume needs the manager to persist its registry to eventlog and reconstruct on `ResumeAutonomous`.
- **Subagent → subagent communication.** Subagents only `report_alert` to their parent; cross-tree messaging isn't supported.
- **MCP / skill tools in the spawn catalog.** v1.2.0's catalog defaults to the built-in tool suite. Library callers can pass additional tools via `WithBackgroundCatalog`; the CLI doesn't enumerate MCP/skill toolsets into the catalog automatically. Add later if a real consumer hits the gap.
- **Budget pooling across siblings.** Each subagent has its own budget; no global cap. Add a pool-level cap later if runaway spawning becomes a real cost concern.

---

## [1.1.0] — 2026-05-16

Interactive permissions for the bundled CLI, plus first-class visibility into Gemini's server-side built-in tool activity (search-grounding) — both in stdout and the eventlog audit trail.

### Added

- **`permissions.StdinPrompter(in, out)`** — new public `Prompter` implementation that renders permission requests to `out` and reads one of `y` / `s` / `t` / `a` / `n` from `in`, mapping cleanly to the existing `Decision` enum. Reprompts on invalid input, denies on bare enter, surfaces EOF / context cancellation as errors. Replaces the placeholder `nil` the bundled CLI passed for the gate prompter in v1.0.x.
- **`--yolo` flag on `cmd/core-agent`** — equivalent to `permissions.mode = "yolo"` in config: bypasses the gate so every tool call runs without approval. Use for headless / scripted invocations where pre-staging an allowlist is impractical.
- **Interactive permissions in `cmd/core-agent`** — when stdin is a TTY and `--yolo` isn't set, the CLI wires `permissions.StdinPrompter(os.Stdin, os.Stderr)` automatically. Tool calls in `ask` mode now prompt the user instead of erroring out immediately. Non-TTY callers still get the same `ErrNoPrompter` failure, but the error message now points at `--yolo` and the `permissions.mode` config knob.
- **`gemini.GroundingProjection(svc)`** — new public `session.Service` wrapper. For every event carrying `GroundingMetadata`, it appends one synthetic event per `WebSearchQueries` entry and per `GroundingChunks[i].Web` source to the same session. Authored `gemini/google_search`, branch-preserved, deduplicated. Synthetic events have an empty `Content.Role` so ADK's content processor skips them when building the next turn's LLM context — they're audit + display, not conversation history. URI-less sources and empty queries are filtered. `cmd/core-agent` wires the projection automatically when `--session-db` is used with `--provider=gemini` / `vertex`.
- **`↪ google_search:` lines in `runner.WriteEvents`** — search queries and grounded sources now render alongside client-side `→` / `←` tool calls in the chat-style output, using a `↪` sigil and a new magenta color (added `ansiMagenta` to the minimal palette). Deduplicated per `WriteEvents` call so repeated metadata in the stream doesn't double-print. Format mirrors the projection's eventlog rows so stdout and `agent_eventlog` describe the same activity.

### Known limitations

- **`URLContext` evidence is not projected today.** ADK's gemini converter (`internal/llminternal/converters`) only lifts `GroundingMetadata` into `model.LLMResponse`; `URLContextMetadata` is dropped before our `session.Service` wrapper can see it. Surfacing it would require intercepting raw genai responses below ADK; deferred until a consumer needs it.
- **Anthropic server-side tools (`web_search`, `web_fetch`)** aren't projected — those built-ins aren't surfaced in the Anthropic adapter yet (carried forward from v1.0.x `Known gaps`). When they land, the same `↪` namespace under `anthropic/*` is reserved for them.
- **Grounding evidence appears *after* the model's text** in the chat stream rather than during. Grounding metadata only lands on the aggregated response event, not on partial streaming chunks — acceptable trade for keeping the synchronous text flow uninterrupted; flagged so consumers building richer UIs over `WriteEvents` know not to expect interleaving.

---

## [1.0.1] — 2026-05-16

Critical bug fixes for `--provider=vertex`. Two regressions surfaced after v1.0.0 shipped; both are fixed here, and Vertex search-grounding now delivers real results.

### Fixed

- **`models/gemini`** — only set `Config.ToolConfig.IncludeServerSideToolInvocations` when fronting the direct Gemini API (`genai.BackendGeminiAPI`), not when fronting Vertex AI. v1.0.0 set this flag unconditionally to satisfy the direct Gemini API's requirement when built-ins ride alongside function tools, but Vertex AI rejects the flag with `includeServerSideToolInvocations parameter is not supported in Gemini Enterprise Agent Platform (previously known as Vertex AI)`. `--provider=vertex` was completely broken at default invocation for any consumer using `tools.Default()` between v1.0.0 and this fix; `--provider=gemini` is unaffected. The `builtinsLLM` wrapper now learns which backend it's fronting at construction time. Tests pin both branches.
- **`models/gemini`** — tolerate Vertex's streaming SSE heartbeat chunks. Vertex's streaming search-grounding API intermittently emits frames carrying only `UsageMetadata` + `ResponseID` and an empty `Candidates[]`. ADK's stream aggregator (`internal/llminternal/stream_aggregator.go`) treats these as fatal and aborts the stream with `empty response`, poisoning the call before the real grounded chunks land. Observed failure rate against `gemini-3.1-pro-preview` on Vertex with the default tool suite + GoogleSearch was 30–60% before the fix, 0% across 10 consecutive runs after. The `builtinsLLM` wrapper now drops `empty response` errors mid-stream on Vertex only — the direct Gemini API path is untouched, so a genuine "no content" failure there still surfaces normally. Non-streaming Vertex calls are also untouched: an empty non-streaming response is a real failure and should propagate.

### Process

- `docs/v1-acceptance.md` Section 6 (Vertex Gemini smoke) was not exercised when cutting v1.0.0 — single-provider sign-off met the plan's bar at the time. The regression slipped through as a result. Going forward, when a fix is added in one provider's request path, run the equivalent smoke against every sibling backend before tagging. The Vertex heartbeat-chunk bug above was found by following through on this discipline after the first Vertex regression report and is what most of the v1.0.1 investigation actually uncovered — the ADK-level `empty response` was masquerading as a clean Vertex failure, not a known protocol quirk.

---

## [1.0.0] — 2026-05-16

First stable release. Same surface as `v0.1.0` with one bug fix and one documented requirement that emerged from running `docs/v1-acceptance.md` against real Gemini.

### Fixed

- **`models/gemini`** — set `Config.ToolConfig.IncludeServerSideToolInvocations = true` whenever the `builtinsLLM` wrapper injects server-side built-ins (`google_search` / `url_context` / `code_execution`) alongside any function-calling tools. Without this flag, Gemini 3+ rejects the combined request with `Please enable tool_config.include_server_side_tool_invocations to use Built-in tools with Function calling`, blocking `--provider=gemini` for any consumer using `tools.Default()`. Surfaced by the v1.0.0 smoke pass (`docs/v1-acceptance.md`). Fix in `models/gemini/builtins.go`.
- **`usage/pricing.go`** — add `gemini-3.1-flash-lite` and `gemini-3-pro-preview` / `gemini-3-pro` entries to the built-in pricing table. The released-name keys were missing for both, so `core-agent`'s cost tracker reported `$0.0000` for runs against those models even though the corresponding `-preview` keys were present. Same rates as their preview counterparts.

### Documentation

- **Gemini 3.0+ requirement** added to `docs/site/content/docs/providers.md` and `docs/site/content/docs/configuration.md`. When combining `core-agent`'s default tool suite with the Gemini provider's built-in tools (both default-on), the Gemini API requires a 3.0-or-later model — Gemini 2.5 rejects the combination outright. Workarounds for consumers who must use Gemini 2.5: `--no-builtin-tools` (drops the function-calling suite) or library-level `gemini.WithGoogleSearch(false)` + `gemini.WithURLContext(false)` (drops the server-side built-ins). Default model already pins `gemini-3.1-pro-preview`, so consumers who don't override never hit this.
- **`docs/v1-acceptance.md` switched smoke commands** from `gemini-2.5-flash` (which can't combine built-ins with function tools — see above) to `gemini-3.1-flash-lite` (the cheapest 3.x model, exercises the same code paths as `gemini-3.1-pro-preview`). Section 9 records the actual sign-off transcript from cutting this release.

### Stability promise (effective with this release)

The public API surface listed in this file's preamble is now under SemVer. Breaking changes go through a minor-version bump (`v1.X.0`) with a one-version deprecation period when feasible. Patch versions (`v1.0.X`) are bug fixes only.

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

[1.3.0]: https://github.com/go-steer/core-agent/releases/tag/v1.3.0
[1.2.0]: https://github.com/go-steer/core-agent/releases/tag/v1.2.0
[1.1.0]: https://github.com/go-steer/core-agent/releases/tag/v1.1.0
[1.0.1]: https://github.com/go-steer/core-agent/releases/tag/v1.0.1
[1.0.0]: https://github.com/go-steer/core-agent/releases/tag/v1.0.0
[0.1.0]: https://github.com/go-steer/core-agent/releases/tag/v0.1.0
