---
title: Library API
weight: 8
---


`core-agent` is designed to be embedded as a Go library. The bundled `cmd/core-agent` is a thin reference wrapper; production consumers will typically write their own binary that composes these packages.

---

## Package overview

| Import path | Purpose |
|---|---|
| `github.com/go-steer/core-agent/agent` | Multi-turn agent wrapping ADK's `llmagent + runner`. |
| `github.com/go-steer/core-agent/instruction` | `AGENTS.md` / `CLAUDE.md` / `GEMINI.md` fallback loader. |
| `github.com/go-steer/core-agent/config` | `.agents/config.json` schema, discovery, atomic persist. |
| `github.com/go-steer/core-agent/permissions` | Ask / allow / yolo gate; bash denylist; path scope. |
| `github.com/go-steer/core-agent/tools` | `GateToolset` wrapper bridging permissions to ADK toolsets. |
| `github.com/go-steer/core-agent/mcp` | MCP server lifecycle from `.agents/mcp.json`. |
| `github.com/go-steer/core-agent/skills` | `SKILL.md` discovery → ADK `skilltoolset`. |
| `github.com/go-steer/core-agent/models` | `Provider` interface + registry / `Resolve()`. |
| `github.com/go-steer/core-agent/models/gemini` | Gemini API + Vertex AI provider. |
| `github.com/go-steer/core-agent/models/anthropic` | Anthropic / Claude provider (first-party + Vertex). |
| `github.com/go-steer/core-agent/telemetry` | OpenTelemetry exporter setup. |
| `github.com/go-steer/core-agent/usage` | Per-turn token + cost tracker. |
| `github.com/go-steer/core-agent/session` | Transcript persistence (`.agents/sessions/`). |
| `github.com/go-steer/core-agent/runner` | Headless (one-shot) + REPL (multi-turn) drivers. |

---

## Minimal example

The shortest possible program: pick a Gemini model, run one turn, print partial text:

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/go-steer/core-agent/agent"
    "github.com/go-steer/core-agent/config"
    "github.com/go-steer/core-agent/models"
    _ "github.com/go-steer/core-agent/models/gemini"
)

func main() {
    cfg := config.DefaultConfig()
    cfg.Model.Provider = config.ProviderGemini

    provider, err := models.Resolve(cfg)
    if err != nil { log.Fatal(err) }

    ctx := context.Background()
    m, err := provider.Model(ctx, cfg.Model.Name)
    if err != nil { log.Fatal(err) }

    a, err := agent.New(m, agent.WithInstruction("Be concise."))
    if err != nil { log.Fatal(err) }

    for event, err := range a.Run(ctx, "What is the capital of France?") {
        if err != nil { log.Fatal(err) }
        if event.Content == nil { continue }
        for _, p := range event.Content.Parts {
            if p.Text != "" && event.Partial {
                fmt.Print(p.Text)
            }
        }
    }
    fmt.Println()
}
```

The blank import on the provider package matters — it triggers the `init()` that registers the provider with `models.Register`. Without it, `models.Resolve` errors with "unknown provider".

---

## Multi-turn conversation

`agent.Agent` reuses the same `runner.Runner` across `Run()` calls. The ADK's session service appends events on each call, so the second call sees the first turn's history automatically:

```go
a, _ := agent.New(m)
ctx := context.Background()

for _, prompt := range []string{"My name is Alex.", "What's my name?"} {
    for event, err := range a.Run(ctx, prompt) {
        if err != nil { log.Fatal(err) }
        // …consume partial text…
    }
}
```

The default session ID is `"default"`. To run multiple isolated conversations from one process, construct distinct agents per session and pass `agent.WithSession(userID, sessionID)`:

```go
a1, _ := agent.New(m, agent.WithSession("alice", "session-1"))
a2, _ := agent.New(m, agent.WithSession("bob",   "session-2"))
```

---

## Agent options

```go
agent.WithAppName(s string)            // identity used by ADK runner; default "core-agent"
agent.WithName(s string)               // agent display name (visible in OTEL spans)
agent.WithDescription(s string)        // agent description
agent.WithInstruction(s string)        // base system instruction; default is agent.DefaultInstruction
agent.WithSystemInstructionPrefix(s)   // prepends s to the instruction
agent.WithStreaming(m StreamingMode)   // override; default is StreamingModeSSE
agent.WithSession(userID, sessionID)   // override session identity
agent.WithTools(ts []tool.Tool)        // register individual tools
agent.WithToolsets(ts []tool.Toolset)  // register groups (MCP, skills, ...)
```

Options are applied in the order they're passed. Tools and toolsets accumulate across multiple calls.

### Default instruction

When `WithInstruction` is not used, agents get `agent.DefaultInstruction` — a baseline helpfulness directive plus a parallelism mandate adapted from `google-gemini/gemini-cli`. The mandate tells the model to batch independent tool calls in a single response; it's load-bearing for Gemini, which otherwise emits one tool call per turn even when batching is obvious. To layer your own guidance on top of the default rather than replacing it:

```go
agent.WithInstruction(agent.DefaultInstruction + "\n\n" + extraGuidance)
```

---

## Built-in tools

The `tools/` package ships a nine-tool baseline suitable for any agent that acts on its workspace: `read_file`, `read_many_files`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash`, `todo`. All nine route through `permissions.Gate` (so the bash denylist and path-scope checks apply), and all honor the per-tool output caps from `cfg.ToolOutput`.

`glob` walks a directory and returns paths whose basename matches a `filepath.Match` pattern (e.g. `*.go`); `grep` walks a directory (or a single file) and returns matching lines for an RE2 regex with file path + 1-based line number + the matching line text. Both use stdlib only — no `bmatcuk/doublestar`, so `**` recursive-glob is not supported (use an explicit walk root instead). Both skip `.git`, `.svn`, `.hg`, `node_modules`, `vendor` and don't follow symlinks.

`read_many_files` reads multiple files in a single call. Accepts `paths` (an explicit list), `pattern` (a basename glob walked from `path`, default `.`), or both together — results are deduplicated and explicit paths come first. Each entry carries `path`, `content`, an optional `truncated` flag (per-file content cap is 64KB), and an optional `skipped` reason when the file was denied by the gate, missing, or a directory. Strictly preferred over multiple parallel `read_file` calls when you already know which files you need — Gemini in particular handles one tool call taking a list better than N parallel calls.

Wire them up in your binary:

```go
import "github.com/go-steer/core-agent/tools"

reg, err := tools.Build(cfg, gate, tools.Default())
if err != nil { /* ... */ }

a, _ := agent.New(m, agent.WithTools(reg.Tools))
```

`reg.Todo` exposes the underlying `*TodoStore` so a host can render plan progress (e.g. for a `/todo` slash command in a TUI) without round-tripping through the model.

To turn one off, set the field directly or use `Disable` (handy when you're applying a list of names from config or a CLI flag):

```go
b := tools.Default()
b.Bash = false             // by field
_ = b.Disable("write_file") // by canonical name; errors on typos
reg, _ := tools.Build(cfg, gate, b)
```

Or replace wholesale:

```go
reg, _ := tools.Build(cfg, gate, tools.BuiltinTools{ReadFile: true, ListDir: true})
```

`tools.Build` requires both `cfg` and `gate` — passing nil returns an error. We deliberately don't ship ungated tools (the bash denylist + path scope would silently stop applying).

The bundled `cmd/core-agent` enables the full set by default. `--no-builtin-tools` disables the whole suite; `--disable-tools=bash,write_file` (or `tools.disable` in config) turn off specific entries.

---

## Streaming events to a chat-like UI

`runner.WriteEvents(events, out, info)` formats an `agent.Run(...)` event iterator for human-readable streaming display — the chat-style output that the bundled CLI's REPL uses, exposed for library callers so you don't have to copy the loop.

```go
import (
    "os"
    "github.com/go-steer/core-agent/agent"
    "github.com/go-steer/core-agent/runner"
)

a, _ := agent.New(m)
events := a.Run(ctx, "what's in main.go?")
if err := runner.WriteEvents(events, os.Stdout, os.Stderr); err != nil {
    log.Fatal(err)
}
```

Output looks like:

```
→ read_file(path="main.go")          ← stderr
← read_file(content="package main…") ← stderr
This file is a small HTTP server…    ← stdout (streams as the model emits partials)
```

Routing:
- **Partial text** streams to `out` with no prefix, so a model's reply renders character-by-character.
- **Tool calls / responses** render as `→ name(key=value, ...)` / `← name(key=value, ...)` to `info`. Args are JSON-encoded and truncated at 80 chars per value so a single big payload doesn't dominate the display.

Pass the same writer for both `out` and `info` (e.g., `os.Stdout`) when you want one combined stream — useful for tmux capture (`tmux pipe-pane`) or piping to a file.

### Color

Pass `runner.WithColor(true)` to wrap tool calls in cyan and partial assistant text in green. Server-side built-in evidence (Gemini grounding) renders in magenta with a `↪` sigil. Off by default — colored output looks like garbage when piped, so opt in (typically gated on `runner.IsTerminal(out)` so the same code does the right thing in both cases):

```go
runner.WriteEvents(events, os.Stdout, os.Stderr,
    runner.WithColor(runner.IsTerminal(os.Stdout)))
```

`IsTerminal` returns false for `bytes.Buffer`, pipes, and any non-`*os.File` writer, so test code (which usually captures into a buffer) gets uncolored output by default.

### Server-side built-in lines

When events carry Gemini `GroundingMetadata` (set by `GoogleSearch` / `URLContext`), `WriteEvents` renders one `↪ google_search:` line per distinct query and grounded source after the model's text. No opt-in required — if the metadata's there, you see it. See [Providers → Surfacing grounded search activity]({{< relref "providers.md" >}}) for the full data flow and the audit-trail counterpart.

---

## Recording LLM turns

`recording.NewRecorder(inner, w io.Writer)` wraps any `model.LLM` and appends each turn (request + response stream) to `w` as a single JSONL line in the shared `recording.RecordedTurn` shape. The wrapper is transparent — callers see the inner LLM's responses unchanged — and the writer's lifecycle is the caller's responsibility.

```go
import "github.com/go-steer/core-agent/recording"

f, err := os.Create("session.jsonl")
if err != nil { /* ... */ }
defer f.Close()

m, _ := provider.Model(ctx, cfg.Model.Name)
m = recording.NewRecorder(m, f)

a, _ := agent.New(m, agent.WithTools(reg.Tools))
```

Replay the captured file with `mock.NewScripted(path, strict)` (or `--provider=scripted --script=path` from the CLI). See [Providers → Scripted]({{< relref "providers.md#scripted-mock" >}}) for the lenient/strict tradeoff and the "tool environment isn't recorded" caveat.

The recorder lives in `recording/` (not `models/mock/`) because it's production observability, not a test fixture — the package name shouldn't suggest you're only allowed to use it in tests.

---

## Durable sessions and audit log

`eventlog.Open(...)` returns a `*Handle` bundling a SQLite/Postgres/MySQL-backed `session.Service` (so every event the agent emits is persisted) and a `Stream` with monotonic seq numbers, `Since(fromSeq)` replay, and `Watch(fromSeq)` live-tail. Wire both into an agent in one option call:

```go
import (
    "github.com/glebarez/sqlite"
    "github.com/go-steer/core-agent/agent"
    "github.com/go-steer/core-agent/eventlog"
)

handle, err := eventlog.Open(ctx, sqlite.Open("sessions.db"))
if err != nil { /* ... */ }
defer handle.Close()

a, _ := agent.New(m,
    agent.WithEventLog(handle),
    agent.WithTools(reg.Tools),
)
```

The same database holds ADK's standard `events`, `sessions`, `app_states`, `user_states` tables plus an `agent_eventlog` overlay table whose `seq INTEGER PRIMARY KEY AUTOINCREMENT` column gives every event a stable monotonic ordering for replay. ADK ships the GORM-backed session service we wrap; we only own the overlay.

### Replay and live tail

```go
// Replay a session from the start.
for entry, err := range handle.Stream.Since(ctx, 0,
    eventlog.ForSession("core-agent", "local", "default")) {
    if err != nil { /* ... */ }
    fmt.Printf("seq=%d author=%s\n", entry.Seq, entry.Event.Author)
}

// Watch for new events as they arrive (blocks until ctx is cancelled).
for entry, err := range handle.Stream.Watch(ctx, lastSeq,
    eventlog.ForSession("core-agent", "local", "default")) {
    if err != nil { /* ... */ }
    handleLive(entry)
}
```

Filter with `WithBranchPrefix(prefix)` to scope to a subagent subtree (subagent runners set `Branch`), `WithAuthor(name)` to find checkpoint events from `RunAutonomous`, or `WithLimit(n)` to cap the result set.

### Multi-driver

Pass any GORM dialector — SQLite, MySQL, Postgres. The CLI wires SQLite by default for the zero-config case; library callers swap in `postgres.Open(dsn)` or `mysql.Open(dsn)` and everything else is the same.

### CLI flags

```
--session-db                    persist sessions + audit log to a durable database (default off)
--session-db-path=PATH          override the database path (default: ~/.<binary>/sessions.db)
```

Either flag enables. The default path is derived from `os.Executable()` so `core-agent`, `scion-agent`, and forks each get their own directory automatically.

### Consistency model

`AppendEvent` writes to ADK's events table first (so the event has its assigned ID), then to the overlay so it picks up a seq. The overlay has a unique index on `event_id`, so a retry of the same event is a no-op rather than a duplicate. Spanning a single transaction across both layers is not done in v1; surfaced overlay-write errors let callers retry safely.

WAL mode is enabled by default for SQLite (`PRAGMA journal_mode=WAL`) so concurrent readers can run alongside the single writer. For workloads that need true concurrent writers across processes, Postgres is the answer — same `eventlog.Open` API, swap the dialector.

### Projecting server-side built-in evidence

Wrap the handle's `Service` with `gemini.GroundingProjection(...)` to project Gemini `GoogleSearch` activity (queries + grounded URLs) into the eventlog as queryable `gemini/google_search`-authored rows. Synthetic events inherit the parent event's branch + invocation ID and are deduplicated; their `Content.Role` is empty so ADK's content processor doesn't reinject them as conversation history on subsequent turns.

```go
import "github.com/go-steer/core-agent/models/gemini"

handle, _ := eventlog.Open(ctx, sqlite.Open("sessions.db"))
handle.Service = gemini.GroundingProjection(handle.Service)
a, _ := agent.New(m, agent.WithEventLog(handle))
```

The bundled CLI wires this automatically when `--session-db` is combined with `--provider=gemini` / `vertex`. Library callers using Anthropic or non-Gemini providers don't need to wrap. See [Providers → Surfacing grounded search activity]({{< relref "providers.md" >}}) for the data flow and the runner-side display story.

### Session lock

Both `RunAutonomous` (when its agent is wired with `WithEventLog`) and `ResumeAutonomous` acquire an exclusive lease on `(AppName, UserID, SessionID)` via `Handle.AcquireLock`. A heartbeat goroutine refreshes the lease every 5 seconds; a lease is considered stale after 30 seconds without a heartbeat and is automatically stolen by the next acquirer (recovers from crashed processes). Concurrent attempts on a fresh lease return `eventlog.ErrSessionLocked` with the holder identifier in the error message for diagnostics.

The lock lives in its own `agent_run_lock` table in the same database; callers don't manage it directly.

---

## Autonomous runs

`agent.RunAutonomous` is a multi-turn driver for unattended workers — batch jobs, CI tasks, scheduled scripts. It loops `agent.Run` against a goal, enforces run-level budgets, and stops when the model signals "done" via an internal lifecycle tool.

```go
import (
    adktool "google.golang.org/adk/tool"
    "github.com/go-steer/core-agent/agent"
)

build := func(extras []adktool.Tool) (*agent.Agent, error) {
    return agent.New(m,
        agent.WithInstruction(
            "You are an autonomous worker. Complete the user's goal end-to-end "+
                "without asking clarifying questions. When finished, call "+
                "report_done with state=\"done\" and a one-sentence detail.",
        ),
        agent.WithTools(append(extras, myTools...)),
    )
}

res, err := agent.RunAutonomous(ctx, build,
    "find every TODO comment and write a tracking doc",
    agent.WithMaxTurns(20),
    agent.WithMaxWallclock(10*time.Minute),
    agent.WithPerTurnTimeout(2*time.Minute),
    agent.WithPricing(usage.PriceFor(cfg.Model.Name, cfg)),
    agent.WithMaxCost(2.50),
)
fmt.Printf("%s after %d turns ($%.4f): %s\n",
    res.Reason, res.Turns, res.CostUSD, res.DoneDetail)
```

### Constructor pattern

`build` is a constructor, not an `*Agent` instance. The driver passes it the internal `report_done` tool so the consumer can compose it with their own tools. This avoids mutating a caller-supplied agent across runs and keeps `agent.New`'s surface free of "extra tools" plumbing that only matters here.

### Termination signal

The driver registers a single-purpose `tools.LifecycleTool` (state="done") under the name `report_done`. The model calls it to end the run. Override the name with `WithDoneToolName` if it collides; override the description with `WithDoneToolDescription` to teach the model when "done" actually means done (e.g. "only after writing a summary file").

Marker-phrase detection ("look for TASK_COMPLETE in the text") is not supported and not recommended — the model can hallucinate the marker. Tool-based termination is unambiguous.

### Budgets

| Option | Caps |
|---|---|
| `WithMaxTurns(n)` | Number of `agent.Run` invocations. Default 50. |
| `WithMaxTokens(in, out)` | Cumulative input / output token totals across all turns. |
| `WithMaxCost(usd)` | Cumulative dollar cost (requires `WithPricing` or `WithTracker`). |
| `WithMaxWallclock(d)` | Total wall-clock duration of the run. |
| `WithPerTurnTimeout(d)` | Per-turn `context.WithTimeout`; one rogue turn can't stall the run. |

Budgets are checked between turns. A turn already in flight when the cap fires runs to completion (or to per-turn timeout) before the driver stops.

### Failure policy

By default any turn-level error aborts the run. Install `WithRetryPolicy` for transient-error recovery:

```go
agent.WithRetryPolicy(func(err error, attempt int) agent.RetryDecision {
    if attempt > 3 { return agent.AbortRun }
    if isTransient(err) { return agent.RetryTurn }
    return agent.SkipTurn // continue with the configured continuation prompt
})
```

`AbortRun` returns `RunResult{Reason: StopReasonRetryAborted}` plus the underlying error. `RetryTurn` re-runs the same prompt. `SkipTurn` advances to `WithContinuationPrompt` (default `"continue"`) and treats the failed turn as if it had completed without a done signal.

### Permission modes

For unattended runs, use `permissions.ModeYolo` (or `ModeAllow` with an explicit allowlist) — `ModeAsk` would deadlock on the first tool call waiting for a human nobody's there to be. If you do use `ModeAsk`, wire a `permissions.Prompter` that fails fast (e.g. `tools.RefusePrompter` plus a custom prompter that just denies).

When your `build` function constructs gated tools, pass the gate to `RunAutonomous` via `WithPermissionsGate(g)`. The driver does a single startup check — `Mode==ask && !HasPrompter` errors out before invoking `build`, so you don't burn an LLM round-trip discovering the misconfiguration. Runtime gating is still enforced by the tools themselves; `WithPermissionsGate` only enables the deadlock guard.

### Composition with recording and mock providers

Both layers compose transparently. To record an autonomous run for offline replay, wrap the model before passing it into `build`:

```go
m = recording.NewRecorder(m, recordFile)
build := func(extras []adktool.Tool) (*agent.Agent, error) {
    return agent.New(m, agent.WithTools(extras))
}
```

To test the loop without burning quota, drive `RunAutonomous` against a `mock.NewScripted(...)` model. `examples/autonomous/` runs end-to-end this way with no credentials.

### Crash-resume

When the agent is wired with `WithEventLog`, `RunAutonomous` emits a checkpoint event after every turn (and a final checkpoint with `stop_reason` on loop exit). A later `ResumeAutonomous` call against the same session walks the event log, re-derives the run totals from the latest checkpoint, and continues from the next turn.

```go
import (
    "github.com/glebarez/sqlite"
    "github.com/go-steer/core-agent/agent"
    "github.com/go-steer/core-agent/eventlog"
)

handle, _ := eventlog.Open(ctx, sqlite.Open("/path/to/sessions.db"))
defer handle.Close()

// Phase 1: original run, capped at 5 turns.
res1, _ := agent.RunAutonomous(ctx, build, "the goal",
    agent.WithMaxTurns(5))
// ... process exits, machine reboots, whatever ...

// Phase 2: pick up where Phase 1 left off.
res2, _ := agent.ResumeAutonomous(ctx, resumeBuild,
    agent.SessionRef{
        Handle:    handle,
        AppName:   "my-app",
        UserID:    "alice",
        SessionID: "long-running-task",
    },
    agent.WithMaxTurns(20)) // bigger budget; carries forward Phase 1's totals
```

`ResumeBuildFunc` differs from `RunAutonomous`'s `BuildFunc` in one detail — it receives the resumed session ID so the constructed agent rejoins the same session via `agent.WithSession`:

```go
resumeBuild := func(extras []adktool.Tool, sess string) (*agent.Agent, error) {
    return agent.New(m,
        agent.WithAppName("my-app"),
        agent.WithSession("alice", sess),
        agent.WithEventLog(handle),
        agent.WithTools(extras),
    )
}
```

Behavior:

- **Terminal-state short-circuit** — if the latest checkpoint has `stop_reason == "completed"` (the model called `report_done`), `ResumeAutonomous` returns the stored `RunResult` immediately without running any new turns. Other stop reasons (`max_turns_exceeded`, `wallclock_exceeded`, `context_cancelled`, etc.) are interruptions, not terminations — those resume normally with the carried-forward totals.
- **No-checkpoint case** — a session with no `/autonomous`-suffix checkpoint events is treated as a fresh start (turn 0). Useful for taking over an existing conversation: "make this session autonomous from here."
- **Cross-binary resume** — the checkpoint author is `<binary>/autonomous` (e.g. `core-agent/autonomous`, `scion-agent/autonomous`). Discovery filters by the `/autonomous` suffix so a run started from one binary can be resumed from another.
- **Budgets carry forward** — if the prior run accumulated 3 turns and the resume passes `WithMaxTurns(3)`, the pre-turn budget check fires immediately and `ResumeAutonomous` returns without running any new turns. Pass a higher budget to extend.
- **Session lock** — `ResumeAutonomous` acquires the session lock (see "Durable sessions and audit log → Session lock"); concurrent attempts return `eventlog.ErrSessionLocked`.

`examples/autonomous-resume/` runs end-to-end with no credentials — uses the scripted mock provider, drives a Phase 1 run capped at 2 turns, then a Phase 2 resume that completes the task.

### What's deferred

- **Mid-turn Pause** — `AutonomousHandle.Pause` waits for the current turn to finish before honoring the pause. Mid-turn (cancel current LLM call and wait) needs more design and will ship with `Redirect` when a consumer hits the seam.
- **Streaming structured results** — pass a `WithProgress` callback if you need per-event observation; richer shapes will land when a consumer asks.

---

## Soft interrupt and programmatic control (v1.3.0+)

`agent.RunAutonomous` is synchronous and fire-and-forget. For harness embedding (Scion, custom orchestrators, anything that needs to push instructions to a running loop) v1.3.0 ships two new surfaces.

### `Agent.Inject(message)` — queue a message for the next turn

Any caller can queue a message on an agent's inbox. The next `Agent.Run` call drains the queue and prepends the messages as a `[Inbox]` block to the prompt the model sees:

```go
go func() {
    sc := bufio.NewScanner(os.Stdin)
    for sc.Scan() {
        _ = a.Inject(sc.Text())   // safe to call from any goroutine
    }
}()

// Each turn the model sees:
//   [Inbox]
//   - <queued message 1>
//   - <queued message 2>
//
//   ---
//
//   <prompt argument from Run()>
```

The inbox is per-agent (not per-manager) so consumers without a `BackgroundAgentManager` get it for free. Drop-oldest backpressure at 256 messages keeps a stuck consumer from deadlocking the agent. `Agent.InboxArrived() <-chan struct{}` exposes a 1-buffer notify channel for harnesses that want to wake on input instead of polling:

```go
for {
    select {
    case <-ctx.Done():
        return
    case <-a.InboxArrived():
        runOneTurn(a, "continue")   // inbox drained automatically
    }
}
```

The bundled Scion adapter uses exactly this pattern — see `extras/scion-agent/main.go`.

### `agent.StartAutonomous` + `AutonomousHandle`

Programmatic control over an autonomous run. `StartAutonomous` launches the loop in a goroutine and returns a handle:

```go
h, err := agent.StartAutonomous(ctx, build, "monitor cluster X",
    agent.WithMaxTurns(0),                // no cap; we'll Stop manually
    agent.WithMaxWallclock(1*time.Hour),  // safety net
)
if err != nil { /* ... */ }
defer h.Stop()

// Push instructions as they arrive from outside:
h.Inject("priority changed: focus on Q4 review")

// Pause briefly:
h.Pause()
// ... do something synchronous ...
h.Resume()

// Block until terminal:
result, err := h.Wait()
```

| Method | Effect |
|---|---|
| `Pause()` | Set a flag the loop checks at the next pre-turn checkpoint. Current turn finishes normally; subsequent turns block until `Resume()` fires. Synthetic `paused` event emitted to eventlog. |
| `Resume()` | Unblock the BeforeTurn hook. Synthetic `resumed` event emitted. |
| `Stop()` | Hard cancel via the run's `ctx.Cancel`. Current LLM call returns `Canceled`; loop exits. Idempotent; unblocks Pause too. |
| `Inject(msg)` | Thin wrapper around the underlying `Agent.Inject`. |
| `Status()` | `Running` / `Paused` / `Stopped` / `Completed` / `Failed`. |
| `Wait()` | Block until the goroutine exits; returns the same `RunResult` + error pair `RunAutonomous` does. |
| `Done()` | Channel that closes when the goroutine exits, for select-style integration. |

`RunAutonomous` keeps working unchanged — it's now a synchronous convenience that wraps `StartAutonomous(...).Wait()`.

### Custom BeforeTurn hook

`agent.WithBeforeTurn(func(ctx, turnNo) error)` lets library callers gate the loop at the per-turn checkpoint. The hook runs after budget checks and before `runOneTurn`. Returning a non-nil error aborts the run. `AutonomousHandle.Pause` uses this internally; library callers can wire arbitrary gating (rate limits, external approvals) on top.

Heads-up: `StartAutonomous` appends its own BeforeTurn hook after the caller's options, so a user-supplied hook gets replaced. If you need both, chain them in your callback yourself for now.

### Pause semantics

The currently-running turn finishes before `Pause` takes effect — clean checkpoint cadence matching the eventlog. If you need immediate mid-turn cancellation, use `Stop()` (which cancels via ctx); a future `Redirect(newGoal)` will combine cancel + restart with a new goal.

### Example

`examples/autonomous-handle/` runs end-to-end with no credentials. Uses a thin slow-LLM wrapper around the echo mock so the Pause window is observable. Demonstrates the full lifecycle: `StartAutonomous` → `Pause` → `Inject` → `Resume` → `Wait`.

---

## Subagents

`agent.WithSubagents([]*Agent)` registers each agent as a callable tool the parent's model can invoke by name. The subagent runs through ADK's runner using the parent's session.Service (so its events stream live into the same audit log) with `session.Event.Branch` set to `"<parent_branch>.<subagent_name>"`.

```go
research, _ := agent.New(researchModel,
    agent.WithName("research"),
    agent.WithDescription("a focused research subagent"),
    agent.WithEventLog(handle),
    agent.WithSession("u", "research"),
    agent.WithInstruction("you are a researcher; answer concisely"),
)

parent, _ := agent.New(parentModel,
    agent.WithName("parent"),
    agent.WithEventLog(handle),
    agent.WithSession("u", "parent"),
    agent.WithSubagents([]*agent.Agent{research}),
    agent.WithInstruction("you summarize; delegate fact-finding to research"),
)
```

The parent's model now sees a `research` tool it can invoke with a `request` string argument. The handler dispatches the inner agent's runner; the joined final text comes back as the tool result.

### Audit log and isolation

Each subagent runs in a derived session row (`<parent>:sub:<branch>`), not the parent's own session row — needed because ADK's database session service uses optimistic concurrency on `last_update_time` and would reject the parent's resumed write after the subagent's writes advanced the timestamp. The events still land in the same database; the easiest way to query the full audit trail of one logical "run" is `WithSessionTree`:

```go
for entry, err := range handle.Stream.Since(ctx, 0,
    eventlog.WithSessionTree("my-app", "alice", "task-1")) {
    // ... parent's events + every subagent's events under task-1 ...
}
```

`WithSessionTree(app, user, parent)` matches the parent session ID exactly plus every `<parent>:sub:%` descendant in one query — the one-query equivalent of running `ForSession(parent)` and `WithBranchPrefix(branch)` separately. For "every subagent of a given name across sessions" use `WithBranchPrefix` instead.

The derived-session shape gives strong context isolation by construction — the subagent's runner sees only its own session, not the parent's history. If the model needs context, the parent passes it via the `request` argument when calling the subagent.

### Per-subagent options

For finer control (custom name, description, depth cap, branch label) call `agent.NewSubagentTool` directly:

```go
researchTool, _ := agent.NewSubagentTool(agent.SubagentOptions{
    Inner:       research,
    Name:        "lookup",        // override the tool name
    Description: "look something up",
    MaxDepth:    3,               // depth cap (default 2)
    Branch:      "lookup",        // branch label (default = tool name)
})
parent, _ := agent.New(model,
    agent.WithEventLog(handle),
    agent.WithTools([]adktool.Tool{researchTool}),
)
```

`MaxDepth` prevents infinite recursion if a subagent registers itself (or another subagent) as a tool. The depth is tracked in the call's `context.Context`; `agent.CurrentSubagentDepth(ctx)` reads it.

`examples/with-subagent/` runs end-to-end with no credentials — uses two scripted-mock providers (one per agent) to demonstrate the full parent→subagent→parent dispatch and inspects the resulting audit log.

### What's deferred

- **Default research-safe tool subset.** The inner agent's tool list is whatever you construct it with; we don't auto-restrict to read-only tools. Add per-subagent gates if your subagent shouldn't have write access.
- **Token / cost rollup** from subagent runs into the parent's `usage.Tracker`. Defer until a consumer asks.
- **`--enable-subagent` CLI flag.** Library-only feature for v1.

---

## Dynamic background subagents (v1.2.0+)

`WithSubagents` is **static** — you wire the subagent population at build time and the parent's model invokes registered subagents *synchronously* (parent blocks until the subagent returns). For long-running monitors, parallel fan-out work, and any case where the parent's model decides at runtime what kind of subagent it needs, use the `BackgroundAgentManager` + `spawn_agent` family instead.

```go
import (
    "github.com/go-steer/core-agent/agent"
    "github.com/go-steer/core-agent/models/gemini"
    "github.com/go-steer/core-agent/permissions"
    "github.com/go-steer/core-agent/tools"
)

provider, _ := gemini.NewVertex(project, location)
m, _ := provider.Model(ctx, "gemini-3.1-pro-preview")
gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})

builtins := tools.Default()
reg, _ := tools.Build(cfg, gate, builtins)

mgr, _ := agent.NewBackgroundAgentManager(
    agent.WithBackgroundProvider(provider, "gemini-3.1-pro-preview"),
    agent.WithBackgroundGate(gate),
    agent.WithBackgroundCatalog(reg.Tools),
    agent.WithBackgroundMaxDepth(2),
    agent.WithBackgroundMaxConcurrent(8),
    agent.WithBackgroundDefaultBudgets(agent.BackgroundBudgets{
        MaxTurns: 50, MaxCost: 1.0, MaxWallclock: 10*time.Minute,
    }),
)
defer mgr.Close()

a, _ := agent.New(m,
    agent.WithTools(append(reg.Tools, agent.NewBackgroundSpawnTools(mgr)...)),
    agent.WithBackgroundManager(mgr),
)
```

The parent's model now sees four extra tools:

| Tool | Use |
|---|---|
| `spawn_agent` | Launch a new in-process background subagent (name, system prompt, goal, tools, optional budgets). |
| `list_agents` | See every subagent the model has spawned and their current status. |
| `check_agent` | Get detailed status + final result for one named subagent. |
| `stop_agent` | Cancel a running subagent. |

Each spawned subagent gets:

- A **fresh `model.LLM`** built from the same provider + modelID (sidesteps any unknowns around concurrent streaming on a shared SDK client).
- A **derived session row** (`<parent>:sub:bg.<name>`) so concurrent goroutines don't race ADK's optimistic-concurrency check.
- A **branch label** (`bg.<name>` at the root, `<parent_branch>.bg.<name>` when nested) so eventlog queries by `WithBranchPrefix("bg.")` find them.
- A **`report_alert` and `report_completed`** tool injected automatically — the subagent's model calls these to signal back to the parent.
- The **parent's permission gate**, inherited by reference. Subagent prompts include `[<subagent-name>]` source attribution; concurrent prompts serialize through a mutex.

### Reports flowing back to the parent

When a subagent calls `report_alert(text)`, the manager pushes an `Alert` onto a buffered channel (default 256, drop-oldest backpressure). Two consumers see it:

1. **Synchronous `OnAlert` hook** — for inline display in the parent's UI. The bundled CLI's REPL installs one that writes `↪ <from> alert: <text>` in magenta to stderr.
2. **Pre-turn drain** — `Agent.Run` calls `mgr.PrependPendingAlerts(prompt)` before each turn, which drains every pending alert (non-blocking) and prepends them as a `[Background reports]` block to the prompt the model sees.

Both consumers see every alert (the hook runs synchronously before the channel push). One-shot headless (`core-agent -p ...`) has no next turn so alerts arrive only in the eventlog and via the hook; REPL and `RunAutonomous` see them through both paths.

### Custom UI sinks

Wire your own alert display by setting an `OnAlert` hook:

```go
mgr.OnAlert(func(a agent.Alert) {
    // Slack / webhook / TUI / etc.
    fmt.Printf("[bg] %s says %s: %s\n", a.From, a.Kind, a.Text)
})
```

The bundled formatter `runner.FormatAlertLine(from, kind, text)` produces the same `↪ ...` shape the CLI uses; pair with `runner.AnsiMagenta()` for matching color.

### Remote (out-of-process) subagents

For subagents that should run elsewhere — gRPC to a remote agent server, K8s Jobs, Cloud Run, NATS-dispatched workers — implement `agent.RemoteAgentSpawner` and pass it to `agent.NewSpawnRemoteAgentTool`. The model gets a `spawn_remote_agent` tool with the same shape as `spawn_agent`; your spawner is responsible for transport + lifecycle. Events the consumer puts on the handle's `Events()` channel are mapped onto the same alert pipeline as in-process subagents, so `list_agents` / `check_agent` / `stop_agent` work uniformly.

```go
type myK8sSpawner struct{ kubeconfig string }

func (s *myK8sSpawner) Spawn(ctx context.Context, spec agent.RemoteAgentSpec) (agent.RemoteAgentHandle, error) {
    // create a K8s Job; return a handle whose Events() channel
    // is populated from the Job's pod logs or a sidecar gRPC stream
}

remoteTool, _ := agent.NewSpawnRemoteAgentTool(&myK8sSpawner{...}, mgr)
a, _ := agent.New(m, agent.WithTools([]tool.Tool{remoteTool, ...}))
```

When you don't want to wire a real spawner (headless / unattended / CI), use `agent.RefuseRemoteAgentSpawner(reason)` — analog of `tools.RefusePrompter`. The model sees a clean error result it can adapt to.

#### Reference implementation: Scion

`extras/scion-remote-agent/` is a working `RemoteAgentSpawner` against [Scion](https://github.com/GoogleCloudPlatform/scion)'s Hub HTTP API. It ships as a **separate Go module** so Scion's heavy transitive deps (cloud.google.com/go, ent ORM, etc.) stay out of consumers who don't use Scion.

```go
import (
    "github.com/go-steer/core-agent/agent"
    scionremote "github.com/go-steer/core-agent/extras/scion-remote-agent"
)

// Auto-detects SCION_HUB_ENDPOINT / SCION_AGENT_TOKEN /
// SCION_PROJECT_ID / SCION_DEFAULT_TEMPLATE from the env.
// Returns scionremote.ErrNotInsideScion when config is incomplete
// so the caller can fall back to agent.RefuseRemoteAgentSpawner.
spawner, err := scionremote.New(
    scionremote.WithTemplate("research-investigator"),
)
if errors.Is(err, scionremote.ErrNotInsideScion) {
    // Local dev: refuse cleanly instead of crashing.
    spawner = agent.RefuseRemoteAgentSpawner("scion not configured")
} else if err != nil {
    return err
}

remoteTool, _ := agent.NewSpawnRemoteAgentTool(spawner, bgMgr)
a, _ := agent.New(m, agent.WithTools([]tool.Tool{remoteTool, ...}))
```

Each `Spawn` provisions a sibling Scion container via the Hub's Create API; the returned handle drains Scion's SSE cloud-logs stream and classifies each entry into an `agent.RemoteAgentEvent`. Three classification strategies are bundled:

- **`scionremote.PreferStructuredPayload`** (default) — looks at `jsonPayload.kind` / `text` first (clean if the spawned agent emits structured log entries), falls back to `StringPrefix`.
- **`scionremote.StringPrefix`** — recognises `[REPORT_ALERT] <text>`, `[REPORT_COMPLETED] <text>`, `[REPORT_FAILED] <text>` at the start of a line. Easy convention for any agent that follows the format.
- **`scionremote.Verbose`** — every log entry becomes a `Kind="log"` event. Use during development to see the raw stream.

Override the default via `scionremote.WithClassifier(...)`. See [`examples/scion-research-demo/`](https://github.com/go-steer/core-agent/tree/main/examples/scion-research-demo) for a full orchestrator-escalates-to-investigator scenario built on this spawner.

### Bundled CLI

`core-agent` ships with all four spawn-related tools enabled by default. `--no-background-agents` disables them. The manager uses `provider` + `cfg.Model.Name` from your config, the same permissions gate as the rest of the CLI, and `tools.Default()` (minus `--disable-tools`) as the catalog of tools subagents may request.

`examples/background-monitor/` runs end-to-end with no credentials and exercises the full Spawn → terminal alert → pre-turn drain path against the echo mock provider.

### Prompting patterns

Just registering the tools isn't enough — the model needs to know that background subagents *exist* and when they're the right move. Without a hint in the system instruction or the user prompt, most models will try to do everything synchronously. A few patterns that work:

**System instruction nudge (the most reliable lever):**

```text
You have access to four background-agent tools: spawn_agent,
list_agents, check_agent, stop_agent. Use them when:

- You're asked to monitor something continuously (a cluster, a queue,
  a log stream). Spawn one subagent per thing to monitor; they should
  call report_alert when they find something noteworthy and
  report_done when their goal is satisfied.
- You're asked to fan out independent work that can run in parallel
  (e.g. "research these 5 topics"). Spawn one subagent per topic
  with a focused system prompt; each reports its findings via
  report_alert; you synthesize after they finish.
- A task would take many turns of your own time but is bounded and
  delegate-able (e.g. "summarize this 200-file directory"). Spawn
  one subagent with a tight scope; check_agent for results.

When you spawn a subagent, give it:
- a clear, narrow system_prompt so it stays focused
- a single-sentence goal
- the minimum tools it needs (read_file, list_dir, glob, grep, bash, etc.)
- a budget appropriate to the task (max_turns, max_cost_usd,
  max_wallclock_seconds) — defaults are conservative.

Subagent reports arrive automatically as a "[Background reports]"
block prepended to your next turn. React to them or use check_agent
to poll explicitly.

Don't spawn a subagent for trivial work you can do in one or two
turns yourself.
```

Drop that block into your `AGENTS.md` (or pass via `agent.WithInstruction` / `agent.WithSystemInstructionPrefix`) and the model will use the tools when the situation matches.

**User prompt patterns that imply background work:**

- *"Keep an eye on the prod cluster for the next hour and let me know if any pod restarts more than 3 times."* — implies a long-running monitor; the model spawns one subagent and uses `report_alert` for findings.
- *"For each of these 5 repos, summarize the recent commits. You can do them in parallel."* — implies fan-out; the model spawns 5 subagents and synthesizes when they all return.
- *"Run `npm test` in the background while you read through the README and propose changes."* — implies parallel mixed work; one subagent for the test run, parent handles the README.

**A complete minimal example:**

```bash
core-agent --provider=vertex -p "
You're an orchestrator. Use spawn_agent to launch two background
subagents: one named 'count-up' that counts from 1 to 5 then calls
report_alert with the final number, and one named 'count-down' that
counts from 10 to 6 then calls report_alert. Each should also call
report_done when finished. Then call check_agent for both and tell
me what they reported.
"
```

The first time you wire background subagents into a new deployment, give the model an explicit nudge like this — once you've watched it use them correctly a few times, you can pare the instruction down.

### Audit log queries

Background subagent activity is visible in the eventlog under branches starting with `bg.`:

```sql
SELECT seq, branch, author FROM agent_eventlog
WHERE branch LIKE 'bg.%'
  AND app_name = 'core-agent' AND user_id = 'me'
ORDER BY seq;
```

Use `eventlog.WithBranchPrefix("bg.")` for the Go API equivalent, or `WithBranchPrefix("bg.<name>")` for one specific subagent's activity.

### What's deferred

- **Bounded permission subsets + parent-as-arbiter** (subagent gets a subset of parent's grants, out-of-subset requests bubble up to the parent's model). Worth doing; v1.3+.
- **Persistence across main-agent restarts.** Subagents die with the parent process; cross-restart resume needs registry-in-eventlog work.
- **Subagent → subagent messaging.** Only parent ↔ subagent today.
- **MCP / skill tools in the default catalog.** The bundled CLI's catalog is the built-in tool suite only. Library callers pass additional tools via `WithBackgroundCatalog`.
- **Budget pooling across siblings.** Each subagent has its own budget; no global cap across the tree.

---

## Adding custom tools

Use ADK's `functiontool.New` to wrap a Go function as a tool the agent can call. Schema is generated from the input/output struct types via `jsonschema` tags.

```go
import (
    adktool "google.golang.org/adk/tool"
    "google.golang.org/adk/tool/functiontool"
)

type addArgs struct {
    A int `json:"a" jsonschema_description:"first number"`
    B int `json:"b" jsonschema_description:"second number"`
}

type addResult struct {
    Sum int `json:"sum"`
}

func addTool() adktool.Tool {
    t, err := functiontool.New(
        functiontool.Config{
            Name:        "add",
            Description: "Add two integers and return the sum.",
        },
        func(_ adktool.Context, in addArgs) (addResult, error) {
            return addResult{Sum: in.A + in.B}, nil
        },
    )
    if err != nil { panic(err) }
    return t
}

a, _ := agent.New(m, agent.WithTools([]adktool.Tool{addTool()}))
```

See `examples/with-tools/` in the repo for a fuller example.

---

## Adding custom providers

Implement `models.Provider` and register it from an `init()`:

```go
package myprovider

import (
    "context"

    adkmodel "google.golang.org/adk/model"
    "github.com/go-steer/core-agent/config"
    "github.com/go-steer/core-agent/models"
)

func init() {
    models.Register("my-provider", newProvider)
}

type Provider struct{ /* …client state… */ }

func (p *Provider) Name() string { return "my-provider" }

func (p *Provider) Model(ctx context.Context, modelID string) (adkmodel.LLM, error) {
    // return a type implementing google.golang.org/adk/model.LLM
    return &llm{...}, nil
}

func newProvider(cfg *config.Config) (models.Provider, error) {
    // read cfg, return constructor
    return &Provider{...}, nil
}
```

See `models/anthropic/anthropic.go` for the canonical example. The `model.LLM` interface is small but exact — it streams genai-shaped events, so providers wrapping non-Gemini APIs need a conversion layer (see `models/anthropic/convert.go` and `stream.go`).

---

## Composing the full stack

The bundled `cmd/core-agent/main.go` is the canonical reference for wiring everything together. The minimum useful composition is roughly:

```go
cfg, agentsDir, _ := config.LoadOrDefault(cwd)
provider, _ := models.Resolve(cfg)
m, _ := provider.Model(ctx, cfg.Model.Name)

gate, _ := permissions.FromConfig(cfg, cwd, userHome, prompter)

instr, _ := instruction.Load(projectRoot, userHome)

_, mcpToolsets, _ := mcp.Build(ctx, agentsDir, sendFn, gate, elicitor)
loadedSkills, _   := skills.Load(ctx, agentsDir, gate)

allToolsets := append([]adktool.Toolset{}, mcpToolsets...)
if !loadedSkills.Empty() {
    allToolsets = append(allToolsets, loadedSkills.Toolset)
}

a, _ := agent.New(m,
    agent.WithToolsets(allToolsets),
    agent.WithSystemInstructionPrefix(instr.Instruction),
    agent.WithTools(myCustomTools),
)
```

Each step is independent — skip the ones you don't need (e.g. no MCP, no skills, no permission gate) and the layers below still work.

---

## Prompter

The permission gate's `Prompter` is the seam for interactive consent in `ask` mode. From v1.1.0 the package ships `permissions.StdinPrompter(in, out)` for terminal use, and the bundled CLI auto-wires it when stdin is a TTY (`--yolo` bypasses the gate entirely for headless runs). To plug in your own custom UI:

```go
type myPrompter struct{ /* UI handle */ }

func (p *myPrompter) AskApproval(ctx context.Context, req permissions.PromptRequest) (permissions.Decision, error) {
    // open a modal / read from terminal / call a Slack bot
    // return one of:
    //   permissions.DecisionDeny
    //   permissions.DecisionAllowOnce
    //   permissions.DecisionAllowSession
    //   permissions.DecisionAllowSessionTool
    //   permissions.DecisionAllowAlways
    return permissions.DecisionAllowOnce, nil
}

gate, _ := permissions.FromConfig(cfg, cwd, userHome, &myPrompter{})
```

`permissions.StdinPrompter` is the reference implementation; for chat / Slack / web-based approval flows, write your own. When picking `DecisionAllowAlways`, the caller is responsible for persisting `req.PersistTool` + `req.PersistKey` into `cfg.Permissions.Allow` (or `cfg.PathScope.Allow` for path-scope prompts) and writing it back via `config.Save`.

### Source attribution and serialization (v1.2.0+)

`PromptRequest.Source` carries the originating agent name when the request comes from a background subagent. `StdinPrompter` renders it as `[<source>] tool wants to ...` in the heading so the human knows which agent is asking. The gate populates `Source` from a context value (`permissions.WithSubagentSource(ctx, name)`) stamped by the spawn machinery; custom prompters that ignore the field still work.

When the gate is shared across goroutines (any setup with background subagents), wrap the prompter in `permissions.Serialize(...)` so concurrent `AskApproval` calls run one at a time. Without this, multiple subagents racing for `os.Stdin` deadlock or interleave garbage. The bundled CLI does this automatically when a `BackgroundAgentManager` is wired; library callers using their own gate construction should do the same.

---

## MCP status

`mcp.Build()` returns three values: per-server records, the toolsets to register, and an error.

```go
servers, toolsets, err := mcp.Build(ctx, agentsDir, send, gate, elicitor)
if err != nil { /* unrecoverable error in mcp.json itself */ }

for _, s := range servers {
    fmt.Printf("%-20s %s  %v\n", s.Name, s.Status, s.Tools)
    if s.Status == mcp.StatusError {
        fmt.Printf("  error: %v\n", s.Err)
    }
}
```

The records are how you build a `/mcp` slash command — they survive even when `Toolset()` returns nil for a failed server. Call `Server.Close()` on each before exiting (or before reloading) to terminate stdio child processes.

---

## MCP elicitation

The `mcp.ElicitorFn` signature:

```go
type ElicitorFn func(
    ctx context.Context,
    serverName string,
    req *mcp.ElicitRequest,
) (*mcp.ElicitResult, error)
```

Pass nil for `mcp.Build`'s elicitor argument to use the bundled `DeclineHandler`, which auto-declines every request and emits a one-line notice. For interactive hosts:

```go
elicitor := func(ctx context.Context, server string, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
    answer, err := ui.PromptUserFor(req.Params.Message, req.Params.RequestedSchema)
    if err != nil { return nil, err }
    return &mcp.ElicitResult{Action: "accept", Content: answer}, nil
}

_, _, _ = mcp.Build(ctx, agentsDir, send, gate, elicitor)
```

---

## Headless and REPL drivers

`runner/headless.go` and `runner/repl.go` are the canonical drivers used by the bundled CLI:

```go
runner.Headless(ctx, m, prompt, stdout, stderr, tracker, pricing, agentOpts...)
runner.REPL    (ctx, m, stdin,  stdout, stderr, tracker, pricing, agentOpts...)
runner.WriteSummary(stderr, tracker, m.Name())
```

They share a one-turn streamer that consumes the agent's event iterator, splits partial text → stdout / tool-call summaries → stderr, and updates the usage tracker. Reach for them when you want the same I/O conventions in your own binary; replace them when you need different rendering (e.g. JSON-stream output, Slack formatting, Bubble Tea TUI).

### REPL keybindings (v1.3.0+)

When `runner.REPL` is called with a real TTY for stdin (the bundled CLI's default; not the case when stdin is piped or redirected), each turn runs inside a `turnInterrupter` that puts stdin in raw input mode and reads single bytes:

| Key | Effect |
|---|---|
| **ESC** | Cancel the current turn. Conversation history is preserved (ADK streams events into the session as they happen, so partial state survives). REPL returns to the `> ` prompt; the next user input is the next turn. |
| **Ctrl+C** (single) | Same as ESC, plus prints a hint: `(press Ctrl+C again within 1s to exit)`. |
| **Ctrl+C** (twice within 1s) | Exit the REPL cleanly. Terminal is restored before the process exits. |
| **Ctrl+D** | EOF — exit the REPL (existing behavior). |
| `/exit`, `/quit` | Same. |

Tools that are in flight when the cancel fires: `bash` (which uses `exec.CommandContext`) cancels its subprocess promptly. Tools that ignore ctx finish their in-flight work before the loop unwinds — best-effort.

When stdin **isn't** a TTY (piped input, redirected file, CI), the interrupter is silently disabled and Ctrl+C falls back to its pre-v1.3.0 behavior (process-level SIGINT → exit). The REPL's startup banner reflects which mode is active.

The interrupter is package-private inside `runner/`. Library callers building custom REPLs can copy the pattern from `runner/interrupt.go` directly, or wait for it to be promoted to a public package when a real third-party consumer asks.

---

## Telemetry

```go
shutdown, err := telemetry.Setup(ctx, cfg.OTEL.Exporter)
if err != nil { /* ... */ }
defer func() { _ = shutdown(context.Background()) }()
```

Modes: `none` (default — no spans), `console` (stderr JSON), `otlp` (honors standard `OTEL_EXPORTER_OTLP_*` env vars). The shutdown function flushes buffered spans — call it before `os.Exit` or you'll lose recent activity.

---

## Transcripts

```go
session.Save(agentsDir, session.Transcript{
    StartedAt: started,
    Model:     m.Name(),
    Messages:  []session.Message{{Role: "user", Text: prompt}},
    Usage:     session.Usage{Turns: tot.Turns, InputTokens: tot.InputTokens, ...},
})
```

Atomic write to `<agentsDir>/sessions/<RFC3339-timestamp>.json`. Empty `agentsDir` is a no-op (no project root → nowhere to write). Schema is versioned (`SchemaVersion = 1`) for forward compatibility.
