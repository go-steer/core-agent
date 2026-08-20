# `parallel-spawn` — one turn fans out three background subagents

A single assistant turn emits **three `spawn_agent` calls in one
model response**; the children run in parallel under the
`background.Manager`; their completion reports drain back into the
parent's next turn as a `[Background reports]` block. Credential-free
end to end — the parent replays a scripted transcript, the children
replay their own.

## What this shows

- Parallel function calling as the fan-out gesture: the scripted
  parent's first response carries three `spawn_agent` function calls,
  so ONE turn launches all three workers.
- `background.NewManager(background.WithProvider(...), ...)` +
  `background.NewSpawnTools(mgr)` registered on the parent via
  `agent.WithTools`, with `agent.WithBackgroundManager(mgr)` stamping
  the parent back-reference — the documented construction order.
- Children calling the autonomous driver's `report_done` tool; the
  manager turns that into a `completed` alert carrying the done
  detail.
- The pre-turn drain: `parent.Run(ctx, "")` (the same empty-prompt
  shape a wake-loop turn uses) prepends every pending report to the
  model's prompt via `PrependPendingAlerts` — the parent model reacts
  to all three reports in one turn.

## Run it

```bash
go run ./examples/parallel-spawn
```

Hermetic: scripted + per-spawn scripted mocks, temp dir for the
transcripts, no listeners. Exits 0.

## Key APIs

| Step | API |
|---|---|
| manager | `background.NewManager`, `WithProvider`, `WithDefaultBudgets`, `WithMaxConcurrent`, `WithAllowAdhoc` |
| per-spawn replay | `mock.NewScriptedPerCall` (a fresh script cursor per `Model` call) |
| tools on the parent | `background.NewSpawnTools` (spawn_agent + stop_agent) |
| wiring | `agent.WithBackgroundManager` (+ construction order in `Manager` docs) |
| observe | `Manager.OnAlert` (non-consuming), `Manager.List`, `Handle.Done/Status` |
| drain | `Agent.Run(ctx, "")` → `Manager.PrependPendingAlerts` internally |

Two subtleties worth copying:

- **`mock.NewScriptedPerCall`, not `mock.NewScripted`.** The manager
  asks its provider for one LLM per spawn, and `NewScripted` hands the
  same replay — one cursor — to all of them, so three children would
  divide the two-turn transcript between them. `NewScriptedPerCall`
  builds a fresh replay per `Model` call, so every child starts at
  turn 0.
- **`background.WithAllowAdhoc(true)`.** These children are *ad-hoc*:
  the parent's model writes each `system_prompt` at spawn time. That
  is off by default, because an unattended daemon should only spawn
  operator-vetted specs — a daemon fans out by name instead
  (`spawn_agent{agent: "recon-logs"}` against a subagent declared in
  config). The example opts in so the fan-out gesture, rather than the
  roster wiring, is what you read.

## What you should see

```
== turn 1: spawn fan-out ==
  -> spawn_agent(recon-logs)
  -> spawn_agent(recon-metrics)
  -> spawn_agent(recon-traces)
== waiting for the three subagents (parallel) ==
  recon-...     -> completed
== reports pending for the next turn (prompt-visible) ==
[Background reports]
- [recon-logs] (completed) area scanned; nothing anomalous found
== turn 2: empty prompt — Run drains the reports into the model ==
  text: All three scouts reported back. ...
```

## Next pointers

- Manager lifecycle without a model in the loop (direct `Spawn`):
  [`examples/background-monitor`](../background-monitor). Synchronous
  in-turn delegation: [`examples/with-subagent`](../with-subagent).
- Budgets, depth caps, alert semantics:
  `docs/background-subagents-design.md`.
