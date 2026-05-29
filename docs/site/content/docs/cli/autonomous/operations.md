---
title: Autonomous runs
weight: 9
---


`agent.RunAutonomous` is the multi-turn driver for unattended workers — batch jobs, CI tasks, scheduled scripts, anything that needs to keep working after a single `agent.Run` turn would have ended. It loops `agent.Run` against a goal, enforces run-level budgets, and stops when the model signals "done" via an internal lifecycle tool.

Two senses of "autonomous" matter here:

| Sense | Driver | When to reach for it |
|---|---|---|
| **Within one turn** | `agent.Run` already loops the model through tool-call cycles until a final response | Single self-contained tasks: "find every TODO in the repo and write a list" |
| **Across turns** | `agent.RunAutonomous` loops `agent.Run` against a goal | Long-running work the model decomposes into multiple turns |

This page covers across-turn autonomy. For the within-turn case, see [Library API → Streaming events]({{< relref "/docs/library/api.md#streaming-events-to-a-chat-like-ui" >}}).

---

## Quick example

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
)
```

The driver returns a structured `RunResult{Reason, Turns, InputTokens, OutputTokens, CostUSD, Duration, FinalText, DoneDetail}` plus any error.

---

## Constructor pattern

`build` is a constructor — `func(extras []tool.Tool) (*agent.Agent, error)` — not an `*Agent` instance. The driver registers an internal `report_done` tool and passes it to your build function via `extras`; you compose it with your own tools and return a fresh agent.

This shape avoids two problems: mutating a caller-supplied agent across runs (which would race), and polluting `agent.New`'s public surface with "extra tools" plumbing only the autonomous driver needs.

---

## Termination signal

The driver registers a single-purpose `tools.LifecycleTool` named `report_done`. The model calls it to end the run. Override the name with `WithDoneToolName` if it collides with one of your own tools; override the description with `WithDoneToolDescription` to teach the model when "done" really means done (e.g. "only after writing a summary file").

Marker-phrase detection ("look for TASK_COMPLETE in the text") is **not** supported and not recommended — the model can hallucinate the marker. Tool-based termination is unambiguous.

---

## Budgets

| Option | Caps |
|---|---|
| `WithMaxTurns(n)` | Number of `agent.Run` invocations. Default 50. |
| `WithMaxTokens(in, out)` | Cumulative input / output token totals across all turns. |
| `WithMaxCost(usd)` | Cumulative dollar cost. Requires `WithPricing` or `WithTracker`. |
| `WithMaxWallclock(d)` | Total wall-clock duration. |
| `WithPerTurnTimeout(d)` | Per-turn `context.WithTimeout`; one rogue turn can't stall the run. |

Budgets are evaluated between turns. A turn already in flight when the cap fires runs to completion (or to per-turn timeout) before the driver stops.

### Surviving the context wall on long runs

A long autonomous run that doesn't manage its context will eventually outgrow the model's window. The autonomous driver respects the same context-management mechanisms documented in [Context management]({{< relref "/docs/reference/context-management.md" >}}) — if the agent was constructed with `WithCompactor(NewDefaultCompactor())` the post-turn threshold check fires between autonomous turns and the next turn drains a pending compaction before its actual work. Same for `WithCheckpointer` when the model calls `mark_task_done` between subgoals.

For runs that do heavy file reads or web fetches, also wire the agentic wrappers (`tools/agentic.AgenticReadFile`, `AgenticGrep`, etc.) so the bulk of tool output never lands in the parent's context to begin with. The combination keeps prompt-cache hit rates high and bounds per-turn cost on runs that otherwise grow unboundedly.

---

## Failure policy

By default any turn-level error aborts the run. Install `WithRetryPolicy` for transient-error recovery:

```go
agent.WithRetryPolicy(func(err error, attempt int) agent.RetryDecision {
    if attempt > 3 { return agent.AbortRun }
    if isTransient(err) { return agent.RetryTurn }
    return agent.SkipTurn
})
```

`AbortRun` returns `RunResult{Reason: StopReasonRetryAborted}` plus the underlying error. `RetryTurn` re-runs the same prompt. `SkipTurn` advances to `WithContinuationPrompt` (default `"continue"`) and treats the failed turn as if it had completed without a done signal.

---

## Permission modes

For unattended runs, use `permissions.ModeYolo` (or `ModeAllow` with an explicit allowlist) — `ModeAsk` would deadlock waiting for a human nobody's there to be. If you do use `ModeAsk`, wire a `permissions.Prompter` that fails fast (e.g. `tools.RefusePrompter`).

When your `build` function constructs gated tools, pass the gate to the driver via `WithPermissionsGate(g)`. The driver does a single startup check — `Mode==ask && !HasPrompter` errors out before invoking `build`, so you don't burn an LLM round-trip discovering the misconfiguration. Runtime gating is still enforced by the tools themselves; `WithPermissionsGate` only enables the deadlock guard.

See [Permissions]({{< relref "/docs/reference/permissions.md" >}}) for the underlying gate semantics.

---

## Asking the user during autonomous runs

A real tension: the agent's instructions might say *"always ask before any cluster modification"* but there's no human staring at a REPL prompt. Two patterns work depending on how long the wait might be.

**In-turn (the agent waits inside one turn).** The agent calls an `ask_user` tool whose handler blocks until the answer arrives, then returns the answer as the tool result. `tools.NewAskUserTool` ships this; the bundled CLI exposes it as `--ask=stdin|auto|off`. Best fit for short clarifications inside a working turn.

**Status + new turn (the agent yields between turns).** For long waits — human is on lunch, another agent has to finish first — the agent emits a status (a `tools.LifecycleTool` call that returns immediately) and ends its turn. A driver loop on the consumer side reads the next input from wherever and starts a fresh `agent.Run`.

The bundled prompters cover the common shapes:

| Prompter | When to use |
|---|---|
| `tools.StdinPrompter(in, out)` | Interactive CLI; Scion-style stdin-fed adapters |
| `tools.RefusePrompter(reason)` | Headless / batch / CI runs where no human is reachable. The agent gets the refusal as the tool result and adapts ("running unattended; proceed with reasonable defaults") instead of blocking forever |
| `tools.StaticPrompter(answer)` | Test fixture |

---

## Crash-resume

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
    agent.WithMaxTurns(20))
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

Behavior worth knowing:

- **Terminal short-circuit only on `Completed`.** A checkpoint with `stop_reason == "completed"` (the model called `report_done`) returns the stored `RunResult` immediately. Other stop reasons (`max_turns_exceeded`, `wallclock_exceeded`, `context_cancelled`, `retry_aborted`) are interruptions, not terminations — those resume normally with carried-forward totals.
- **No-checkpoint = turn-0 start.** A session with no `/autonomous`-suffix checkpoints is treated as a fresh start. "Take this existing conversation and make it autonomous from here" is a valid use.
- **Cross-binary resume.** Checkpoints carry `Author = "<binary>/autonomous"` (from `os.Executable()`). Discovery filters by the `/autonomous` suffix so a run started under `core-agent` can be resumed under `scion-agent` or `ax-agent` without losing its trail.
- **Budgets carry forward.** `WithMaxTurns(3)` on resume against a session that already used 3 turns fires the pre-turn budget check immediately. Pass a higher budget to extend.
- **Session lock.** `ResumeAutonomous` takes an exclusive lease on `(AppName, UserID, SessionID)` for its lifetime; concurrent attempts return `eventlog.ErrSessionLocked` with the holder identifier in the error message. See [Sessions → Session lock]({{< relref "/docs/reference/sessions.md#session-lock" >}}).

`examples/autonomous-resume/` runs end-to-end with no credentials — uses the scripted mock provider, drives a Phase 1 run capped at 2 turns, then a Phase 2 resume that completes the task.

---

## Lifecycle tool for state emission

`tools.NewLifecycleTool` is the generic state-emission primitive the autonomous driver uses internally for its `report_done` signal. It's also exported for direct use — orchestrator adapters (Scion, AX) wire it for "I'm thinking / blocked / done" emission to their UI even though they have their own loops.

```go
import "github.com/go-steer/core-agent/tools"

statusTool, _ := tools.NewLifecycleTool(tools.LifecycleOptions{
    Handler: func(ctx context.Context, ev tools.LifecycleEvent) error {
        log.Printf("agent status: %s — %s", ev.State, ev.Detail)
        return nil
    },
})
```

`AllowedStates` constrains what state labels the model can emit; the autonomous driver uses `[]string{"done"}` to scope its internal instance. Consumer-supplied handlers route emissions wherever they need to go — stderr, a status file, a websocket, an orchestrator's event log.

---

## Subagents

For tasks where the model should delegate focused work to a specialized agent (research, planning, summarizing), wire one or more subagents via `agent.WithSubagents([]*Agent)`:

```go
research, _ := agent.New(researchModel,
    agent.WithName("research"),
    agent.WithEventLog(handle),
    agent.WithInstruction("you are a researcher; answer concisely"),
)
parent, _ := agent.New(parentModel,
    agent.WithEventLog(handle),
    agent.WithSubagents([]*agent.Agent{research}),
    agent.WithInstruction("delegate fact-finding to the research subagent"),
)
```

The parent's model sees a `research` tool it can call with a `request` string. The handler dispatches the inner agent's runner; the joined final text comes back as the tool result. Subagent events stream live into the parent's audit log under `Branch="<parent_branch>.research"`.

See [Library API → Subagents]({{< relref "/docs/library/api.md#subagents" >}}) for the full API surface — depth caps, custom branch labels, per-subagent options. `examples/with-subagent/` runs end-to-end with no credentials.

---

## Composition with recording and mock providers

Both layers compose transparently. To record a run for offline replay, wrap the model before passing it into `build`:

```go
m = recording.NewRecorder(m, recordFile)
build := func(extras []adktool.Tool) (*agent.Agent, error) {
    return agent.New(m, agent.WithTools(extras))
}
```

To test the loop without burning quota, drive `RunAutonomous` against a `mock.NewScripted(...)` model. `examples/autonomous/` runs end-to-end this way with no credentials.

---

## What's deferred

- **Pause / resume mid-run.** The orchestrator-driven pattern (Scion, AX) covers this naturally; standalone needs more design.
- **Streaming structured results.** `WithProgress(callback)` covers per-event observation today; richer shapes will land when a consumer asks.
- **`--autonomous` CLI flag.** The bundled `cmd/core-agent` is a REPL / one-shot tool. Long-running autonomous use is a library / script concern.
