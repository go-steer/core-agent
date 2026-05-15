# Autonomous operation

How to drive core-agent autonomously — without a human in the loop on every turn. Two senses of "autonomous" need to be distinguished, because they're solved by different code paths:

1. **Within one turn** — already supported. A single `agent.Run` call is itself an autonomous loop: the model reasons, calls tools, sees results, calls more tools, until it emits a final response. Bounded by `cfg.Agent.MaxSteps` (default 50) so a runaway tool-call cycle can't go forever.
2. **Across turns** — shipped as `agent.RunAutonomous`. A multi-turn driver that loops `agent.Run` against a goal, enforces run-level budgets (turns / tokens / cost / wallclock + per-turn timeout), and stops when the model signals "done" via an internal lifecycle tool.

For "give the agent a goal, let it work end-to-end" use cases, sense (1) is often enough. Set the system prompt to discourage clarification-asking and the permission gate to `yolo` (since `ask` would block on prompts no human is reading), and you've got a one-shot autonomous worker. When the goal needs to span more than one model turn, reach for sense (2).

## Within one turn — the easy case

```go
a, _ := agent.New(m,
    agent.WithInstruction(`You are an autonomous worker. Complete the
user's request end-to-end without asking clarification questions.
When you've finished or if you can't make progress, output a final
summary explaining what you did or why you stopped.`),
)

for ev, err := range a.Run(ctx, "find every TODO in the codebase and write a tracking doc") {
    if err != nil { return err }
    // observe events as desired
}
```

The single `agent.Run` call drives the model through as many tool-call cycles as it needs. Each tool result is fed back into the next reasoning step automatically. The turn ends when the model emits content with no further tool calls (a "final response").

**Levers within a turn:**

- **`agent.WithInstruction(...)`** — the system prompt. Most powerful lever. Tell the model it's autonomous and what "done" looks like.
- **`cfg.Agent.MaxSteps`** — caps the per-turn tool-call cycle count. Default 50; raise for complex tasks, lower as a safety net.
- **`cfg.Permissions.Mode = "yolo"`** — bypasses the prompt-for-approval flow. Required for any agent without a human watching, since `ask` mode would deadlock waiting for input.
- **Bash denylist** still applies even in yolo. The non-overridable refusals (`rm -rf /`, etc.) remain.
- **Tool selection** — disable `bash` / `write_file` / `edit_file` (`cfg.Tools.Disable`) for read-only autonomous research; keep them on for productive tasks.

## Across turns — `agent.RunAutonomous`

When one turn isn't enough — for example a long-running research-and-write task whose plan changes as the model discovers new things — use the bundled multi-turn driver:

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

res, err := agent.RunAutonomous(ctx, build, "find every TODO and write a tracking doc",
    agent.WithMaxTurns(20),
    agent.WithMaxWallclock(10*time.Minute),
    agent.WithPerTurnTimeout(2*time.Minute),
)
```

`build` is a constructor — the driver registers an internal `report_done` tool and passes it through `extras` so the consumer composes it with their own tool registry. This avoids mutating a caller-supplied agent across runs.

The driver returns a structured `RunResult{Reason, Turns, InputTokens, OutputTokens, CostUSD, Duration, FinalText, DoneDetail}` plus any error. `Reason` is one of `completed`, `max_turns_exceeded`, `max_tokens_exceeded`, `max_cost_exceeded`, `wallclock_exceeded`, `context_cancelled`, or `retry_policy_aborted`.

See [Library API → Autonomous runs]({{< relref "/docs/library-api.md#autonomous-runs" >}}) for the full option list and recipes.

## Asking the user during autonomous runs

A real tension shows up when an autonomous agent has instructions like *"always ask before any cluster modification."* The model wants to ask, but there's no human staring at a REPL prompt — and nothing in the library blocks the agent from continuing.

Two patterns work, depending on how long the wait might be:

### In-turn (the agent waits inside one turn)

The agent calls an `ask_user` tool whose handler blocks until the answer arrives, then returns the answer as the tool result. The agent's reasoning continues in the same turn, with the user's input woven into its conversation history.

`tools.NewAskUserTool` ships this:

```go
import "github.com/go-steer/core-agent/tools"

ask, _ := tools.NewAskUserTool(tools.AskUserOptions{
    Prompter: tools.StdinPrompter(os.Stdin, os.Stderr),
})
a, _ := agent.New(m, agent.WithTools([]adktool.Tool{ask /* + your other tools */}))
```

When the model emits an `ask_user` tool call, the prompter delivers the question, the consumer replies, and the answer flows back as the tool's response. Best fit for short clarifications inside a working turn.

The bundled `cmd/core-agent` exposes this as the `--ask` flag:

- `--ask=stdin` — registers `ask_user` with `StdinPrompter`.
- `--ask=auto` — picks `StdinPrompter` when `os.Stdin` is a terminal, `RefusePrompter` otherwise. The right choice for scripts that *might* be run interactively or might be piped — the agent's behavior adapts.
- `--ask=off` (default) — `ask_user` is not registered; the model has no way to ask. Conservative default; opt in when your AGENTS.md tells the model to ask.

### Status + new turn (the agent yields between turns)

For long waits — human is on lunch; another agent has to finish first — the agent emits a *status* (a tool call that returns immediately) and ends its turn. A driver loop on the consumer side reads the next input from wherever (stdin, message queue, websocket) and starts a fresh `agent.Run`. The Scion adapter (`extras/scion-agent/main.go`) is the reference implementation: AGENTS.md tells the model to call `sciontool status ask_user "..."` (a bash command); the Scion harness shows the question to the human; their reply comes back via tmux → stdin → the adapter's stdin loop → next `agent.Run`. No ask_user tool needed because the bash tool already runs the status emitter.

For generic status emission — "I'm thinking", "I'm blocked", custom states — `tools.NewLifecycleTool` ships a single building block. The consumer supplies a `LifecycleHandler` that decides where the events go (stdout, a status file, a websocket, an orchestrator's event log). It's the same mechanism `RunAutonomous` uses internally for its `report_done` signal, just exposed for direct consumer use.

### Built-in prompters

Three ship in the `tools/` package; they cover the common shapes. Anything more involved (HTTP, websocket, message queue) is a custom `Prompter` implementation — it's a one-method interface.

| Prompter | When to use |
|---|---|
| `tools.StdinPrompter(in, out)` | Interactive CLI; Scion-style stdin-fed adapters. Reads one newline-terminated line per ask. |
| `tools.RefusePrompter(reason)` | Headless / batch / CI runs where no human is reachable. The agent gets the refusal as the tool result and adapts ("running unattended; proceed with reasonable defaults") instead of blocking forever. |
| `tools.StaticPrompter(answer)` | Test fixture. Returns a canned answer with no delay. |

Errors from the prompter come back as the tool's result string — phrased as `(no user available: <reason>)` — so the model sees them in conversation context and can adapt. The tool call itself never fails, which means a missing prompter doesn't abort the turn; the model just learns it can't ask and reasons accordingly.

### Picking the right pattern

| Situation | Pattern | Why |
|---|---|---|
| Interactive CLI ("just ask me") | In-turn + `StdinPrompter` | Same terminal, one prompt at a time, fast. |
| Multi-agent / Scion / harness with its own UI | Status + new turn | Long waits across processes; the "asking" agent shouldn't hold a stream open. |
| Batch / CI / long-running daemon with no human reachable | In-turn + `RefusePrompter` | Tells the agent "no one's home" so it doesn't hang. The model adapts. |
| Tests | In-turn + `StaticPrompter` | Deterministic answer; no I/O. |

## What's not built (and why)

- **Crash-resume across long autonomous runs.** Today's session is in-memory; a process restart loses everything. M3's file-backed sessions (mentioned in `DESIGN.md:84-87`) would close this gap. Plan documents the seam (`agent.WithSession` is already there).
- **Pause / resume mid-run.** The orchestrator-driven shape (Scion adapter, the AX adapter on the private `axplore` branch) covers most "pause" semantics naturally — the orchestrator just stops calling the adapter. Standalone pause needs a "what does pause mean" design that doesn't exist yet.
- **Backpressure / human checkpoints baked into the driver.** Implementable today as a recipe: register a custom `LifecycleTool` that blocks in its handler until a human approves the next chunk. No new API needed.
- **`--autonomous` CLI flag on `cmd/core-agent`.** The bundled binary is a REPL / headless one-shot tool. Long-running autonomous use is library / script territory; the flag would just wrap `agent.RunAutonomous` with no real value-add until a consumer asks.

## When to revisit

- M3 ships file-backed sessions → wire crash-resume (~50 lines of "load session, load checkpoint, resume from turn N").
- A second non-AX orchestrator adapter arrives → factor common adapter code into `extras/orchestrator-common/` (currently YAGNI with one example).
- A consumer asks for pause/resume mid-run → design what "pause" means in standalone vs. orchestrator-driven shapes.
