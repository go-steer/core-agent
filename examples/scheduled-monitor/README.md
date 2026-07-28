# `scheduled-monitor` — Schedulers, schedule_next_turn, and cadenced children

## What this shows

The scheduled-monitoring primitives from
`docs/scheduled-monitoring-design.md`, hermetic and credential-free,
in three parts:

1. **Scheduler primitives** — `SleepScheduler` (blocks until the
   event's `WakeAt`) and `ExitOnDeferScheduler` (returns the
   `ErrSchedulerDefer` sentinel), driven against fake `ScheduleEvent`s
   so you can see what the autonomous driver does between turns.
2. **The `schedule_next_turn` tool** — `NewScheduleTool` registration
   and the channel shape the driver drains after each turn, simulated
   without an LLM in the loop.
3. **Manager default-scheduler wiring** —
   `background.WithDefaultScheduler(SleepScheduler())` so every
   spawned child inherits a cadence unless its `Spec.Scheduler`
   overrides. One child spawns against the echo mock to prove the
   wiring; the full spawn/alert-drain pathway lives in
   [`background-monitor`](../background-monitor/).

## Run it

No credentials needed, runs offline:

```bash
go run ./examples/scheduled-monitor
```

## Key APIs

- `tools.SleepScheduler` / `tools.ExitOnDeferScheduler` / `tools.ErrSchedulerDefer` — `github.com/go-steer/core-agent/v2/pkg/tools`
- `tools.NewScheduleTool` / `tools.ScheduleEvent` — the `schedule_next_turn` tool and its channel
- `background.NewManager` / `background.WithDefaultScheduler` — `github.com/go-steer/core-agent/v2/pkg/agent/background`
- `agent.DefaultSchedulingInstruction` — system-prompt priming for LLM-picked cadence

## What you should see

```
=== Part 1: Scheduler primitives ===
SleepScheduler.BeforeNextTurn returned after 80ms (asked for 80ms)
ExitOnDeferScheduler.BeforeNextTurn returned: ... (== ErrSchedulerDefer: true)

=== Part 2: schedule_next_turn tool emission ===
registered tool "schedule_next_turn" ...

=== Part 3: manager with a default scheduler ===
spawned with default scheduler: monitor-cluster-a -> deferred
```

(The echo child ends `deferred` after its 1-turn budget.)

## Next

- [`background-monitor`](../background-monitor/) — the spawn/alert pathway this example's Part 3 defers to.
- [`autonomous`](../autonomous/) — the driver that consumes `ScheduleEvent`s between turns.
- API reference: [`docs/site/src/content/docs/embed/api.md`](../../docs/site/src/content/docs/embed/api.md)
