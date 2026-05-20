# Scheduled monitoring: a `Scheduler` primitive for paced autonomous loops

Design doc for the M4-era killer-feature short list. Sibling to
`docs/attach-mode-design.md`, `docs/bidirectional-mcp-design.md`,
`docs/code-mode-design.md`, `docs/ax-integration-audit.md`.

## Context

The canonical use case driving this doc: a single operator wants to
point core-agent at a GKE project and say *"watch everything;
periodically rescan and alert me on anything odd — error spikes, new
deployments, deployments that disappeared, pods stuck in
CrashLoopBackOff."* The agent should run effectively forever,
monitoring multiple clusters and namespaces independently, without
the operator stitching together a k8s CronJob, a queue, and three
shell scripts.

Today the consumer can already get partway there:

- `agent.RunAutonomous` loops `agent.Run` against a goal with budget
  caps and a model-driven `report_done` signal
  (`docs/autonomous.md`).
- `agent.BackgroundAgentManager` + the `spawn_agent` tool family
  fan out N in-process subagents, each running its own
  `RunAutonomous`, with alerts draining back to the parent via the
  pre-turn injection channel (`docs/background-subagents-design.md`,
  `examples/background-monitor/`).
- `ResumeAutonomous` + eventlog checkpoints carry state across
  process restarts (`agent/autonomous.go`).
- MCP and skills inject the K8s / GKE tooling the agent actually
  uses to look at clusters.

What's missing is **between-turn pacing.** The driver has no notion
of "scan, then wait 10 minutes, then scan again." The operator's
only options today are:

1. Tell the model to `sleep 600` via the `bash` tool inside a turn.
   Works, but holds the conversation context through the sleep,
   defeats Anthropic's 5-min prompt-cache TTL on every cycle, and
   prevents clean shutdown.
2. Run core-agent under an external scheduler (k8s CronJob, systemd
   timer). Works for single-cluster monitoring, but adds operator
   burden and doesn't compose with the in-process background-subagent
   model — each cluster's pacing has to be expressed outside the
   agent.

Neither shape is satisfying for a fleet-of-monitors topology. We
need a primitive that lets the model say *"my work for now is done;
wake me at T+10m with this continuation"* and have the driver honor
that signal cleanly — sleeping the goroutine in a long-running
daemon, or persisting the wake-time and exiting under a CronJob.

### Settled design decisions (do not relitigate — design around them)

These came out of the design conversation that produced this doc.
Recorded here so the implementation phase doesn't reopen them.

- **No sleep tool in core-agent.** A trivial `sleep` tool that
  blocks the goroutine is user-land — a 20-line consumer tool
  registered via `WithTools`. Same posture as Scion's
  `sciontool_status`. Documenting the *non*-decision so nobody
  ships one by accident.
- **Lifecycle pattern: tool primitive + consumer-supplied behavior.**
  Same shape as the existing `tools.NewLifecycleTool` + handler,
  `tools.NewAskUserTool` + `Prompter`, `tools.NewSpawnRemoteAgentTool`
  + `RemoteAgentSpawner`. Core-agent ships the tool wiring; the
  consumer supplies the `Scheduler` implementation that decides what
  "wake at T" actually means in their deployment.
- **Tool semantically distinct from `report_done`.** The autonomous
  driver currently treats lifecycle emissions as terminal. Overloading
  the done tool with a "but actually come back later" mode muddies the
  contract. Ship a separate `schedule_next_turn` tool that the driver
  knows means "end this turn but keep the loop alive."
- **Per-loop, not per-process.** Each `RunAutonomous` call takes its
  own `WithScheduler(s)`. The `BackgroundAgentManager` doesn't impose
  a single scheduler on its children. Different monitors want different
  cadences (error-rate poll every 30s, deployment-diff every 5m), and
  some children may not want pacing at all (a one-shot triage
  subagent the parent spawned in response to an alert).
- **Caller chooses daemon vs CronJob via the `Scheduler` impl, not
  the agent.** The model writes the same code either way. Operators
  who run a long-lived process pick `tools.SleepScheduler`. Operators
  who run under k8s CronJob pick `tools.ExitOnDeferScheduler`. The
  agent's instructions don't change between deployment shapes.

## Goals and non-goals

### Goals

- Let the model emit a "rescan at T+N" intent the autonomous driver
  honors without burning context or cache.
- Compose cleanly with `BackgroundAgentManager` so a parent
  supervisor can fan out N independently-paced cluster monitors.
- Survive process restart for the CronJob deployment shape: a
  deferred wake-time persists to eventlog and `ResumeAutonomous`
  picks it up.
- Stay zero-cost on autonomous loops that don't use scheduling —
  the existing `report_done` flow continues to work unchanged.

### Non-goals

- **No cron expressions in the tool surface.** The model emits
  *next wake* — an absolute time or a duration. Recurrence is a
  property of the loop, not the tool call. If the model wants to
  poll every 5m, it calls the tool with `+5m` on each turn. This
  matches how the harness's `ScheduleWakeup` works (vs `CronCreate`'s
  recurring shape) — recurring schedules are a different feature,
  not yet motivated by a consumer.
- **No driver-side recurrence machinery.** The `Scheduler` interface
  is consulted *only* when the schedule tool fired on the prior
  turn. The driver doesn't track "scan every N minutes" state —
  that's the model's job, expressed turn by turn.
- **No queue / distributed-scheduler integration.** AX
  (`reference_ax_runtime`) is the layer above for cross-process
  scheduling. The `ExitOnDeferScheduler` impl emits enough state for
  AX (or a k8s CronJob, or a NATS queue worker) to pick up where the
  agent left off; bridging is consumer-supplied.
- **No mid-turn defer.** The model emits a defer via a tool call; the
  driver acts on it after the turn ends. No interruption of an
  in-flight turn.

## Architecture

```
┌──────────────────────────────────────────────────────────────────────┐
│ agent.RunAutonomous loop                                             │
│                                                                      │
│   ┌────────────────────────────────────────────────────────────┐     │
│   │ Pre-turn budget checks (max_turns / max_wallclock / ...)   │     │
│   └────────────────────────┬───────────────────────────────────┘     │
│                            ▼                                         │
│   ┌────────────────────────────────────────────────────────────┐     │
│   │ agent.Run(prompt) — model calls tools, may call:           │     │
│   │   - report_done(...)        → exit loop                    │     │
│   │   - schedule_next_turn(...) → record wakeAt + nextPrompt   │     │
│   └────────────────────────┬───────────────────────────────────┘     │
│                            ▼                                         │
│   ┌────────────────────────────────────────────────────────────┐     │
│   │ Post-turn reckoning: tokens, cost, checkpoint emission     │     │
│   └────────────────────────┬───────────────────────────────────┘     │
│                            ▼                                         │
│   ┌────────────────────────────────────────────────────────────┐     │
│   │ Did this turn emit a schedule intent?                      │     │
│   │   no  → continue immediately (today's behavior)            │     │
│   │   yes → Scheduler.BeforeNextTurn(ctx, wakeAt, nextPrompt)  │     │
│   │           - SleepScheduler: time.Sleep, return nil         │     │
│   │           - ExitOnDeferScheduler: persist, return ErrExit  │     │
│   │           - consumer impl: queue, AX dispatch, etc.        │     │
│   └────────────────────────┬───────────────────────────────────┘     │
│                            ▼                                         │
│   ┌────────────────────────────────────────────────────────────┐     │
│   │ Next iteration: prompt = nextPrompt (or continuationPrompt)│     │
│   └────────────────────────────────────────────────────────────┘     │
└──────────────────────────────────────────────────────────────────────┘
```

The schedule tool is a sibling of the done tool — same lifecycle-event
shape, distinct semantics. The driver inspects emissions after each
turn and routes them to the scheduler (defer) or the exit path (done).

## Tool surface

### `tools.NewScheduleTool`

```go
// ScheduleEvent is what the schedule tool emits when the model calls it.
type ScheduleEvent struct {
    // WakeAt is the absolute time the next turn should run.
    // Resolved from either an absolute time or a relative duration in
    // the tool args before the handler sees the event.
    WakeAt time.Time

    // NextPrompt is the continuation message the next turn should
    // receive. Empty string means use the driver's WithContinuationPrompt
    // (default "continue").
    NextPrompt string

    // Detail is a freeform one-liner the model passes for telemetry /
    // operator visibility ("scanning cluster-A in 5m").
    Detail string
}

// ScheduleOptions configures the tool wiring.
type ScheduleOptions struct {
    // Name overrides the function name exposed to the model.
    // Default "schedule_next_turn".
    Name string

    // Description overrides the model-facing description.
    // Default explains: "end this turn cleanly; resume at the
    // requested time with the next prompt."
    Description string

    // MaxDefer caps how far in the future a single call can defer
    // to (e.g. 24*time.Hour). Zero means no cap. Calls past the cap
    // return an error to the model.
    MaxDefer time.Duration
}

// NewScheduleTool returns the tool plus a channel the driver consumes
// from after each turn to detect schedule emissions. The driver wires
// the channel; consumers wire their own Scheduler implementation via
// WithScheduler (below). They don't see the tool directly.
func NewScheduleTool(opts ScheduleOptions) (tool.Tool, <-chan ScheduleEvent, error) {
    // ... emits at most one event per call; buffered(1) channel
}
```

Argument schema seen by the model:

```jsonc
{
  "wake_at":     "2026-05-20T15:30:00Z",  // absolute time, optional
  "wake_in_sec": 600,                      // OR relative seconds, optional
                                           // (exactly one of the two required)
  "next_prompt": "Rescan cluster-A for new errors since the last pass.",
  "detail":      "polling cluster-A on 10m cadence"
}
```

The tool result returned to the model is a confirmation
(`"deferred until 2026-05-20T15:30:00Z"`). The model's turn ends
shortly after — it should not emit more tool calls after `schedule_next_turn`
since the loop is about to pause.

### `agent.WithScheduler` (new `RunAutonomous` option)

```go
// Scheduler decides what to do when a turn emits a schedule intent.
// Consulted only after a turn that called the schedule tool —
// loops that never schedule incur zero overhead.
type Scheduler interface {
    // BeforeNextTurn runs after the post-turn reckoning. The driver
    // has already written its checkpoint (when an eventlog is wired).
    // Return nil to continue the loop with prompt=event.NextPrompt
    // after wakeAt has been honored. Return ErrSchedulerDefer to
    // exit the loop with StopReason="deferred"; the next ResumeAutonomous
    // call picks up the persisted wake state.
    BeforeNextTurn(ctx context.Context, ev ScheduleEvent) error
}

// ErrSchedulerDefer is the sentinel the exit-on-defer impl returns
// to break out of the loop with StopReasonDeferred.
var ErrSchedulerDefer = errors.New("scheduler: defer to next process")

func WithScheduler(s Scheduler) AutonomousOption { ... }
```

The driver wires the schedule tool internally (same way it wires
`report_done` today), passes it to `build` alongside the done tool
in `extras`, and consults `Scheduler.BeforeNextTurn` between turns
when the schedule channel fired.

### New `StopReason` value

```go
const (
    // ... existing reasons ...

    // StopReasonDeferred means the Scheduler returned ErrSchedulerDefer
    // — the loop exited cleanly with a pending wake-time persisted to
    // the eventlog. ResumeAutonomous picks up at the wake-time.
    StopReasonDeferred StopReason = "deferred"
)
```

And a new field on `RunResult`:

```go
type RunResult struct {
    // ... existing fields ...

    // NextWakeAt is set when Reason == StopReasonDeferred. The
    // orchestrator restarts the process at or after this time.
    NextWakeAt time.Time
}
```

### Steering the model

A capable tool isn't useful if the model picks the wrong one or uses
the right one badly. We ship steering in two layers, mirroring the
v1.4.0 split between in-tool guidance and `agent.DefaultInstruction`.

**Layer 1 — in the tool description itself.** Point-of-use, short.
The model sees this every time it considers calling the tool, so
it's the right home for trap-avoidance ("don't confuse with
`report_done`") and one-line cadence steering. Default description:

> `schedule_next_turn` — Pause the autonomous loop and resume later
> with a new prompt. **Use this instead of `report_done` when there
> is more periodic work to do** — `report_done` exits the loop
> permanently and the loop will not resume. Keep `next_prompt` short
> and action-oriented; the original goal and instructions are
> already in the system prompt and will be re-presented on the next
> turn. Prefer longer waits over shorter ones — cost scales linearly
> with wake frequency. Don't call this in the same turn as
> `report_done`; if both are called, `report_done` wins and the loop
> exits.

Overridable via `ScheduleOptions.Description` for consumers with
domain-specific shape (e.g. *"always wake by the top of the
hour"*).

**Layer 2 — composable system-instruction constant.** Cross-cutting
policy that doesn't fit in a tool description. Consumers concat into
their system prompt the same way they do with
`agent.DefaultInstruction`:

```go
// Exported from package agent.
const DefaultSchedulingInstruction = `When running a paced loop with schedule_next_turn:

1. Default to slow cadences. Most monitoring tasks tolerate 5-15
   minute gaps; some tolerate hours. Cost scales linearly with
   wake frequency. Start slow and tighten only when you observe
   active anomalies.

2. Adaptive cadence is encouraged. When you see anomalies in
   flight, shorten the cadence for the next few turns to track
   resolution. When the system is quiet for several cycles,
   lengthen the cadence again.

3. State doesn't survive a defer except in the eventlog. The
   conversation context resets between turns; only files you
   wrote and todo entries you created persist. To carry a
   baseline ("deployments I saw last scan", "error counts at
   last poll") across turns, write it to a file or todo entry
   on this turn and read it back on the next.

4. The next_prompt is a hook, not a full restatement. Keep it
   short and action-oriented ("rescan and diff vs baseline.json").
   The original goal and your system instructions are already in
   the next turn's context.

5. Don't call schedule_next_turn and report_done in the same turn.
   If you do, report_done wins and the loop exits.
`
```

**Opt-in posture — no auto-injection.** The driver does not append
`DefaultSchedulingInstruction` to the system prompt automatically
when `WithScheduler` is wired. Reasons:

- Consumers should keep explicit control over what their model is
  told (some have strict system-prompt review processes).
- The eccentric cases (a monitor that genuinely needs 100ms cadence
  for a 1-minute burst, or a domain where adaptive cadence is the
  wrong default) shouldn't have to fight a baked-in policy.
- Mirrors `agent.DefaultInstruction`'s opt-in posture for the same
  reasons.

Recommended composition in consumer code:

```go
agent.New(m,
    agent.WithInstruction(
        agent.DefaultInstruction + "\n\n" +
        agent.DefaultSchedulingInstruction + "\n\n" +
        myConsumerInstruction,
    ),
    agent.WithTools(...),
)
```

The bundled CLI's `--autonomous` mode (once it lands, separate
work) will compose this stack by default, since it's the
canonical autonomous-monitoring entry point.

## Bundled `Scheduler` implementations

Two ship with core-agent; consumers write their own for distributed
shapes (AX dispatch, NATS queue, custom orchestrator).

### `tools.SleepScheduler`

Long-running daemon. Sleeps the calling goroutine until `WakeAt`,
respects context cancellation, returns nil.

```go
func SleepScheduler() Scheduler {
    return schedulerFunc(func(ctx context.Context, ev ScheduleEvent) error {
        wait := time.Until(ev.WakeAt)
        if wait <= 0 { return nil }
        select {
        case <-time.After(wait):
            return nil
        case <-ctx.Done():
            return ctx.Err()
        }
    })
}
```

### `tools.ExitOnDeferScheduler`

CronJob / orchestrator-managed deployment. Persists wake state to
the eventlog (via a checkpoint event the `ResumeAutonomous` path
already understands), returns `ErrSchedulerDefer`.

```go
func ExitOnDeferScheduler() Scheduler {
    return schedulerFunc(func(ctx context.Context, ev ScheduleEvent) error {
        // Driver has already emitted the per-turn checkpoint; this
        // returns the sentinel so the loop exits with NextWakeAt
        // populated on RunResult and StopReasonDeferred.
        return ErrSchedulerDefer
    })
}
```

The wake-time persistence rides on the existing checkpoint shape —
no new event type. The checkpoint payload gets a `next_wake_at` field
(zero when no deferral pending). `ResumeAutonomous`'s recovery logic
reads this and either waits inline (daemon resume) or refuses to start
until wall-clock has passed (CronJob resume safety).

## Composition with `BackgroundAgentManager`

The supervision-tree shape this enables, end-to-end:

```
            [ Parent Supervisor Agent ]            ← foreground; reactive to alerts
                       │                            no scheduler — it doesn't sleep
                       │ spawns at runtime via spawn_agent
                       │
        ┌──────────────┼──────────────┐
        ▼              ▼              ▼
  [ cluster-A     [ cluster-B    [ namespace-X
    monitor ]      monitor ]      monitor ]      ← each: own RunAutonomous + own Scheduler
        │              │              │            (SleepScheduler in daemon mode)
        │              │              │
        └──────────────┴──────────────┘
                       │
                       ▼
              [ Alert drain → parent ]
            (PrependPendingAlerts on next turn,
             OnAlert hook fires inline as anomalies surface)
```

Each background subagent runs on its own goroutine with the same
`WithScheduler` it was constructed with by the manager. When monitor-A
sleeps 10m, monitor-B's goroutine is unaffected; alerts from B drain
into the parent's pre-turn inbox in real-time.

For the CronJob deployment shape, the `BackgroundAgentManager` itself
isn't really useful — the parent process exits between scans, and N
independently-scheduled k8s CronJobs (one per cluster) take over the
fan-out. Same agent code, different deployment.

### Worked example: GKE fleet monitoring end-to-end

The user gives the parent **one** prompt and walks away. The parent
agent owns the rest of the topology — it decides what to monitor,
how often, and when to react.

**1. Operator prompt** (one-shot, fire and forget):

```
Watch this GKE project. Cover clusters cluster-A and cluster-B
plus namespace ingress-system. Scan periodically and alert me on
anything odd — error spikes, new deployments, deployments that
disappeared, pods in CrashLoopBackOff.
```

**2. Parent's first turn** — the parent has `spawn_agent` and the
K8s/GKE MCP tools in its palette. It decides the fan-out itself:

```
→ spawn_agent(
    name="monitor-cluster-A",
    prompt="Poll cluster-A health. Each turn: list errors via
            kubectl logs --since=<last_scan>, diff deployments
            vs prior snapshot, then call schedule_next_turn with
            wake_in_sec=600 and next_prompt=\"rescan and diff\".
            Emit alerts via report_alert when you find anomalies.",
    scheduler="default"
  )
→ spawn_agent(name="monitor-cluster-B", prompt="(same for cluster-B)")
→ spawn_agent(
    name="monitor-ingress",
    prompt="Watch ingress-system namespace; 30s cadence; alert on
            pod restarts and Service endpoint changes.",
    scheduler="default"
  )
→ report_done(detail="3 monitors spawned; standing by for alerts")
```

The parent's `RunAutonomous` exits cleanly. The manager keeps the
three child goroutines alive.

**3. Each child runs its own loop** — same code path, different
cadences and goals expressed via the spawn prompt:

```
[monitor-cluster-A goroutine]
turn 1: kubectl get pods,deploy → snapshot baseline; no alerts yet
        → schedule_next_turn(wake_in_sec=600, next_prompt="rescan and diff")
        [SleepScheduler.BeforeNextTurn: time.Sleep(10m)]

turn 2: kubectl get pods,deploy → diff vs baseline
        → finds 3 new errors in payments/v2 since last scan
        → report_alert(kind="errors", text="3 new errors in payments/v2")
        → schedule_next_turn(wake_in_sec=600, ...)
        [sleep 10m]

turn 3: deployment "checkout-v1" missing from snapshot
        → report_alert(kind="deployment_removed", text="checkout-v1 gone")
        → schedule_next_turn(...)
```

**4. Parent reactivity** — the manager's `OnAlert` hook fires inline
for operator visibility, and the alerts also queue into the parent's
pre-turn inbox. When the operator (or an external trigger, e.g. attach
mode) sends a new prompt to the parent, the alerts get prepended:

```
[operator nudges parent via attach mode: "summarize what's happened"]

parent's next turn sees:
  [pending alerts]
  - monitor-cluster-A: 3 new errors in payments/v2
  - monitor-cluster-A: checkout-v1 deployment removed
  - monitor-ingress: ingress-nginx-controller restarted 4x in 5m
  [user prompt] summarize what's happened

→ parent's model decides: "ingress-nginx restarts look correlated
  with a recent rollout; let me spawn a triage subagent"
→ spawn_agent(
    name="triage-ingress",
    prompt="Investigate ingress-nginx restart pattern in last 30m...",
    scheduler="none"   ← one-shot, no defer cycle
  )
→ final text to operator: "..."
```

**5. The parent owns the topology lifecycle** — using the same
`spawn_agent` tool family it can `list_agents`, `stop_agent`, or
spawn replacements:

```
operator: "we decommissioned cluster-B; stop monitoring it"
→ stop_agent(name="monitor-cluster-B")
```

The whole thing — N independent paced monitors, one reactive
supervisor, runtime topology decisions — composes from three
existing capabilities (`RunAutonomous`, `BackgroundAgentManager`,
`spawn_agent`) plus the one new primitive this doc proposes
(`Scheduler` + `schedule_next_turn` tool). No external orchestrator,
no cron infra, no per-cluster boilerplate.

### Manager-level option for default scheduler

To avoid forcing each `spawn_agent` call to specify its scheduler,
the manager grows one option:

```go
agent.WithBackgroundDefaultScheduler(s Scheduler) BackgroundManagerOption
```

If set, every spawned subagent's `RunAutonomous` gets `WithScheduler(s)`
unless the spawn spec overrode it. Matches the existing pattern of
`WithBackgroundDefaultBudgets`.

The spawn tool's JSON schema gets one optional field:

```jsonc
{
  "scheduler": "default" | "sleep" | "exit_on_defer" | "none"
}
```

`"none"` means the subagent has no scheduler — the schedule tool will
return an error to the model. Useful for one-shot triage subagents
the parent spawns reactively.

## Crash-resume interaction

The existing `ResumeAutonomous` machinery already replays the last
checkpoint and continues. With `StopReasonDeferred`:

- **Daemon mode** (`SleepScheduler`): a `kill -9` mid-sleep leaves a
  deferred checkpoint in the eventlog. `ResumeAutonomous` reads
  `next_wake_at`, checks wall-clock, and either sleeps the remainder
  inline (if started before the wake-time) or proceeds immediately
  (if started after).
- **CronJob mode** (`ExitOnDeferScheduler`): the process exited
  cleanly; the CronJob fires at the wake-time and the new process's
  `ResumeAutonomous` call finds the deferred checkpoint and
  continues. If the CronJob fires *late*, `ResumeAutonomous`
  proceeds immediately rather than re-deferring.

Both modes use the same code path on resume. The difference is just
whether the process stayed alive between turns.

## Permission gate interaction

The schedule tool itself doesn't need gating — it doesn't touch the
filesystem, network, or shell. It changes loop control flow, which is
already governed by the autonomous driver's budget caps.

But: a scheduled-wake loop running under `ModeAsk` with no prompter
is a deadlock waiting to happen, same as `RunAutonomous` is today.
The existing startup check `Mode==ask && !HasPrompter` already
catches this; no new validation needed.

For background subagents, the existing rule (subagent inherits parent's
gate wholesale, `docs/background-subagents-design.md`) carries over.
A daemon-mode parent with `ModeYolo` produces daemon-mode children
with `ModeYolo`.

## Telemetry and observability

Each `schedule_next_turn` call surfaces in:

- The chat-style streaming UI: `↪ schedule_next_turn: wake_in 10m — polling cluster-A`
- The eventlog: a regular tool-call event followed by a checkpoint
  with `next_wake_at` populated.
- `WithProgress` callbacks: same event shape as any other tool call.

When the loop exits via `StopReasonDeferred`, `RunResult.NextWakeAt`
is the canonical wake-time for the orchestrator.

A new `WithScheduleHook(func(ScheduleEvent))` option mirrors
`WithProgress` for consumers who want to observe deferrals without
implementing a full `Scheduler`. Deferred unless a consumer asks —
`WithProgress` should already cover it.

## Implementation sketch

Files touched / added:

- `tools/schedule.go` — `NewScheduleTool`, `ScheduleEvent`,
  `ScheduleOptions`, the schedule-tool event channel plumbing.
  (~150 LoC + tests.)
- `tools/scheduler.go` — `Scheduler` interface, `ErrSchedulerDefer`,
  `SleepScheduler`, `ExitOnDeferScheduler`, `schedulerFunc` adapter.
  (~80 LoC + tests.)
- `agent/autonomous.go` — `WithScheduler` option, schedule-channel
  consumption in the loop body, `StopReasonDeferred`,
  `RunResult.NextWakeAt`, checkpoint payload extension. (~100 LoC
  changed.)
- `agent/autonomous_resume.go` — deferred-checkpoint handling on
  resume. (~40 LoC.)
- `agent/background.go` — `WithBackgroundDefaultScheduler` option,
  per-spawn override in `BackgroundSpec`. (~30 LoC.)
- `agent/background_spawn.go` — schedule-related JSON schema field,
  resolution to a `Scheduler` impl. (~30 LoC.)
- `examples/scheduled-monitor/` — end-to-end example using the mock
  provider: parent spawns two monitors, each defers via
  `SleepScheduler` with a short cadence, alerts drain back.
- `docs/site/content/docs/autonomous.md` — new section "Pacing
  long-running loops with `Scheduler`" once shipped.
- `CHANGELOG.md` — under the release that lands this.

## Open questions

1. **Schedule emission from the `report_done` tool's slot?** The
   done tool currently uses `tools.NewLifecycleTool` under the hood
   with `AllowedStates=["done"]`. We could extend that to
   `["done", "deferred"]` and have the driver route on state.
   *Argument for:* one tool, less surface. *Argument against:*
   muddies the "I'm finished" semantic the done tool was designed
   for; bumps the AllowedStates contract; consumers who customize
   the done tool name expecting it means "terminal" get surprised.
   Lean: separate tool. Calling out for confirmation.

2. **Should `MaxDefer` be enforced driver-side too?** A model could
   call `wake_in_sec: 86400` and tie up an in-process daemon for a
   day. The tool-level `MaxDefer` is one defense; a driver-level
   `WithMaxDefer` matching the wallclock-budget pattern is another.
   Probably yes — same shape as the existing budget options.

3. **Does `BackgroundAgentManager` need a "wake any deferred"
   admin API?** For the daemon case: operator decides ad-hoc to
   trigger an immediate rescan of cluster-A. Could ride the
   existing `Agent.Inject` machinery (the deferred goroutine wakes
   on a new injection). Probably yes, but worth implementing the
   simple case first and letting attach mode
   (`docs/attach-mode-design.md`) provide the operator surface.

4. **Does this obsolete the CronJob deployment pattern for v1?**
   With `SleepScheduler` + supervisor topology, a single long-lived
   pod can monitor a hundred clusters. The CronJob shape stops being
   the obvious answer except where the operator already has cron
   infra and doesn't want a daemon. `ExitOnDeferScheduler` exists
   anyway for that case; documenting the trade-off in the
   user-facing docs once shipped.

5. **Cadence: prompt-driven or structural spawn arg?** Current
   design: the parent tells each child *"use 10m cadence"* in the
   spawn prompt; the child calls `schedule_next_turn` with whatever
   `wake_in_sec` it chooses on each turn. Alternative: the spawn
   tool grows a `cadence_seconds` arg; the framework auto-injects a
   `schedule_next_turn` call after every turn at that fixed cadence;
   the model can't drift but also can't adapt (e.g. *"errors spiked,
   poll faster for the next hour"*). *Lean:* prompt-driven —
   matches the agentic posture of the rest of the framework, mirrors
   how `report_done` works today (model decides when), and the
   adaptive-cadence story is genuinely valuable for monitoring use
   cases. Structural cadence can land later as an additive opt-in
   if a consumer asks. Calling out for confirmation; this is the
   one question where the user's K8s monitoring use case might
   actually pull toward the structural option (predictable cadence
   = simpler operator mental model).

## Acceptance

Two checks: a hermetic smoketest that runs in CI on every PR, and a
manual UAT against a real K8s cluster that the operator runs before
sign-off. Capturing both here so they don't get reinvented when
implementation lands.

### Smoketest (hermetic, runs in CI)

Same posture as `examples/background-monitor/` — uses the scripted
mock provider, no credentials, no network, runs in seconds.

**Location:** `examples/scheduled-monitor/` + unit tests in
`tools/scheduler_test.go`, `agent/autonomous_test.go`,
`agent/background_test.go`.

**Coverage matrix:**

| # | Scenario | Validates |
|---|---|---|
| S1 | `SleepScheduler` with `wake_in_sec=0.1` | Driver consults scheduler; loop resumes with `next_prompt`; total wallclock matches expected |
| S2 | `SleepScheduler` + `ctx.Cancel()` mid-sleep | Goroutine returns promptly; loop exits with `StopReasonContextCancelled` |
| S3 | `ExitOnDeferScheduler` | Loop exits with `StopReasonDeferred`, `RunResult.NextWakeAt` populated, checkpoint persisted to eventlog |
| S4 | `ResumeAutonomous` against a deferred-checkpoint session | Picks up at `next_wake_at`; if wall-clock past wake-time, proceeds immediately; if before, waits |
| S5 | Tool-level `MaxDefer` | Call with `wake_in_sec` past cap returns tool error to model; loop continues; model adapts |
| S6 | Driver-level `WithMaxDefer` (if Q2 resolves yes) | Same as S5 but enforced one layer up |
| S7 | `BackgroundAgentManager` + `WithBackgroundDefaultScheduler(SleepScheduler())` | Parent spawns 2 children; each defers; both wake on schedule; alerts drain back to parent via `OnAlert` and pre-turn injection |
| S8 | Per-spawn `scheduler` override in `BackgroundSpec` | Child A gets default scheduler; child B gets `"none"`; calls to `schedule_next_turn` from B return tool error |
| S9 | `schedule_next_turn` called after `report_done` in same turn | Driver treats `report_done` as winning; loop exits, no defer |
| S10 | Loop with no scheduler installed and model calls `schedule_next_turn` | Tool not registered; model gets standard "no such tool" error and adapts |
| S11 | `WithScheduleHook` callback fires for every schedule emission | Observable without implementing full `Scheduler` |
| S12 | Concurrent children: monitor-A sleeps 5s, monitor-B alerts at 2s | B's alert reaches parent's drain immediately; A's goroutine not affected |
| S13 | `agent.DefaultSchedulingInstruction` wiring | Constant is non-empty; composes via standard `WithInstruction` concat; the registered `schedule_next_turn` tool's description (as visible to the model via the tool schema) contains the report_done distinction and the cadence-cost warning |

**CI gate:** the bundled `examples/scheduled-monitor/` runs as part
of `go test ./examples/...` (via the existing `examples_test.go`
runner pattern); failure blocks merge.

### UAT (manual, against real K8s)

Run by the operator before tagging the release that ships this
feature. Validates the canonical use case end-to-end with real
infrastructure.

**Prerequisites:**

- A test K8s cluster the operator can break and restore (kind /
  minikube / disposable GKE project). Two clusters preferred to
  exercise the fan-out story.
- A real LLM provider configured: Anthropic via Vertex AI (preferred
  for prompt-caching validation) or Gemini via Vertex / API key.
- K8s tooling wired into core-agent via either (a) a K8s MCP server,
  or (b) the bundled `bash` tool with a permissions allowlist for
  `kubectl get *`, `kubectl logs *`, `kubectl describe *` (read-only).
- `--session-db` enabled so the eventlog captures the full run for
  post-hoc replay and the resume scenarios work.

**Scenarios:**

| # | Step | Expected |
|---|---|---|
| U1 | Operator gives the [worked-example prompt](#worked-example-gke-fleet-monitoring-end-to-end); walks away | Parent spawns 3 children within first turn; `report_done` |
| U2 | Wait 15 minutes; observe eventlog or attach-mode tail | Each child shows ≥1 completed scan cycle, ≥1 successful `schedule_next_turn` call; no busy-loop, no tool errors |
| U3 | Introduce anomalies: `kubectl scale deploy/payments --replicas=0`, then 5m later `kubectl apply` a deployment with a bad image | Each anomaly produces a `report_alert` from the relevant monitor within one cadence cycle (≤10m for cluster monitors, ≤30s for ingress) |
| U4 | Nudge parent via attach mode: *"summarize what's happened"* | Parent's response includes the alerts that drained from children; no alert loss |
| U5 | `kill -9` the daemon process mid-cycle (between scans, during a `SleepScheduler` wait) | Process dies cleanly |
| U6 | Restart the daemon with `ResumeAutonomous` against the same session ID | All 3 children rehydrate from deferred checkpoints; cycles resume; no duplicate alerts for events the previous run already reported |
| U7 | Operator: *"we decommissioned cluster-B; stop monitoring it"* | Parent calls `stop_agent(name="monitor-cluster-B")`; that goroutine ends; other two unaffected |
| U8 | Run for ≥1 hour with mixed light/heavy activity | Memory stable (RSS within 10% of baseline after 1h), goroutine count stable (matches `len(manager.List())+constant`), no leaked HTTP connections (pprof `net/http` profile) |
| U9 | (Anthropic only) inspect usage logs | Prompt-cache hit rate >50% on continuation prompts — confirms deferring between turns preserves the cache vs the alternative of `bash: sleep` inside a turn |
| U10 | Cost check | Per-monitor token spend roughly linear in scan count, not in wallclock — confirms idle children aren't billing |

**Sign-off:** all U1–U10 pass; release notes call out the new
`Scheduler` primitive, the two bundled impls, and the
`BackgroundAgentManager` integration; `docs/site/content/docs/autonomous.md`
gets the "Pacing long-running loops with `Scheduler`" section before
the tag.

### Model-steering eval (real-LLM, nightly)

The smoketest validates wiring; the UAT validates real K8s
behavior. Neither validates that `DefaultSchedulingInstruction`
actually steers the model toward the correct choices. That's what
this eval is for.

Same posture as the `dev/parallel-probe/` measurement that
backed the v1.4.0 Gemini tool-description rewrites — a small,
budgeted, real-LLM probe runs in nightly CI (not per-PR; costs
real money) and emits a structured pass/fail.

**Location:** `dev/scheduling-probe/` — mirrors
`dev/parallel-probe/` shape.

**Models under test:** Claude Sonnet 4.6 (Vertex), Gemini 3.1 Pro
(Vertex). Same two model families the parallel-probe targets.

**Probe matrix:** each scenario is run with the bundled instruction
and without, 10 trials each, and the steering rate (correct-choice
fraction) is compared.

| # | Scenario | Tools palette | Correct choice | Pass criterion |
|---|---|---|---|---|
| E1 | "Check cluster-A pods. If healthy, come back in 10 minutes and recheck." | `bash`, `schedule_next_turn`, `report_done` | `schedule_next_turn(wake_in_sec=600)` after the check | ≥9/10 trials with instruction; **delta vs no-instruction ≥+30%** |
| E2 | "Scan namespace ingress-system once and tell me what's there." | same | `report_done` after one scan, no schedule call | ≥9/10 with instruction (catches "always defer" overcorrection) |
| E3 | "Carry forward the deployment list you see this turn so you can diff next time." | `bash`, `write_file`, `schedule_next_turn`, `report_done` | Writes deployment list to a file before deferring | ≥8/10 with instruction (state-persistence rule) |
| E4 | "You found 5 new errors. Decide next cadence." (mid-stream prompt simulating an active anomaly) | same | `schedule_next_turn` with `wake_in_sec` ≤300 (≤5m) | ≥7/10 with instruction (adaptive-cadence rule) |
| E5 | "All clear for the last 3 cycles. Decide next cadence." | same | `schedule_next_turn` with `wake_in_sec` ≥900 (≥15m) | ≥7/10 with instruction (adaptive-cadence relaxation) |

**Anti-pattern check:** in any scenario where the model picks
`bash: sleep N` instead of `schedule_next_turn`, the trial counts
as a fail — even if the duration was correct. Catches the
prompt-cache-defeating path the instruction is meant to prevent.

**Reporting:** the probe emits a markdown table to
`dev/scheduling-probe/results/<date>.md` with steering rate per
scenario per model, with-instruction vs without. A regression
(steering rate drops >10% on any scenario across two consecutive
nightlies) opens an issue automatically — same shape as
parallel-probe's regression gate.

**Budget:** ≤$2/night per model at current pricing. 5 scenarios ×
20 trials × ~2k tokens average × $0.003/1k input = ~$0.60/model;
output tokens push it to ~$1.50–2.00/model. Caps via the existing
`WithMaxCost` budget on each trial's `RunAutonomous`.

**Tuning loop:** if a scenario's steering rate drops below
threshold, the instruction text or tool description gets tightened
and the probe re-run. This is the only design-doc commitment that
changes over time as model behavior shifts — same pattern as the
parallel-probe-driven tool-description rewrites in v1.4.0.
