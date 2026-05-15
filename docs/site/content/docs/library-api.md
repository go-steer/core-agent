---
title: Library API
weight: 8
---

# Library API

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
agent.WithInstruction(s string)        // base system instruction
agent.WithSystemInstructionPrefix(s)   // prepends s to the instruction
agent.WithStreaming(m StreamingMode)   // override; default is StreamingModeSSE
agent.WithSession(userID, sessionID)   // override session identity
agent.WithTools(ts []tool.Tool)        // register individual tools
agent.WithToolsets(ts []tool.Toolset)  // register groups (MCP, skills, ...)
```

Options are applied in the order they're passed. Tools and toolsets accumulate across multiple calls.

---

## Built-in tools

The `tools/` package ships an eight-tool baseline suitable for any agent that acts on its workspace: `read_file`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash`, `todo`. All eight route through `permissions.Gate` (so the bash denylist and path-scope checks apply), and all honor the per-tool output caps from `cfg.ToolOutput`.

`glob` walks a directory and returns paths whose basename matches a `filepath.Match` pattern (e.g. `*.go`); `grep` walks a directory (or a single file) and returns matching lines for an RE2 regex with file path + 1-based line number + the matching line text. Both use stdlib only — no `bmatcuk/doublestar`, so `**` recursive-glob is not supported (use an explicit walk root instead). Both skip `.git`, `.svn`, `.hg`, `node_modules`, `vendor` and don't follow symlinks.

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

Pass `runner.WithColor(true)` to wrap tool calls in cyan and partial assistant text in green. Off by default — colored output looks like garbage when piped, so opt in (typically gated on `runner.IsTerminal(out)` so the same code does the right thing in both cases):

```go
runner.WriteEvents(events, os.Stdout, os.Stderr,
    runner.WithColor(runner.IsTerminal(os.Stdout)))
```

`IsTerminal` returns false for `bytes.Buffer`, pipes, and any non-`*os.File` writer, so test code (which usually captures into a buffer) gets uncolored output by default.

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

- **Pause / resume mid-run** — the orchestrator-driven pattern (Scion, AX) covers this naturally; standalone needs more design.
- **Streaming structured results** — pass a `WithProgress` callback if you need per-event observation; richer shapes will land when a consumer asks.

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

The permission gate's `Prompter` is the seam for interactive consent in `ask` mode. The bundled CLI doesn't ship one, so REPL-with-tools effectively requires `mode: yolo` or pre-baked allowlists. To plug in your own:

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

When picking `DecisionAllowAlways`, the caller is responsible for persisting `req.PersistTool` + `req.PersistKey` into `cfg.Permissions.Allow` (or `cfg.PathScope.Allow` for path-scope prompts) and writing it back via `config.Save`.

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
