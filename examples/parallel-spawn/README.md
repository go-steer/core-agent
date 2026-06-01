# `parallel-spawn` — fan-out N background subagents from one parent decision

Worked example of the `BackgroundAgentManager` + `spawn_agent` /
`list_agents` / `check_agent` / `stop_agent` tools driving a parallel
fan-out: one parent agent receives an incident, dispatches N independent
investigators in a single assistant turn, then synthesizes their reports
on its next turn via the pre-turn alert drain.

Scenario is GKE-flavored — `payments-prod` namespace is degraded, four
services need triage in parallel — but the orchestration shape is the
useful bit. The same wiring works for multi-cluster compliance scans,
post-deploy verification across N services, parallel investigation of a
failing rollout, fan-out across regions, etc.

## Run

```bash
go run ./examples/parallel-spawn
```

No credentials needed. Parent uses the scripted mock provider for a
deterministic two-turn arc; subagents use the echo mock so they
complete in one turn. Hermetic for CI.

Expected output is shown at the bottom of this README.

## What it demonstrates

| Concern | How the example shows it |
|---|---|
| **One parent decision → N parallel investigators** | Turn 1's response is a single model output carrying four `spawn_agent` function calls. ADK's executor dispatches them concurrently. |
| **Wall-clock = `max(per-investigation)`, not sum** | The four subagents run in parallel goroutines under the `BackgroundAgentManager`'s capacity cap. The demo's wait loop blocks on each subagent's `Done()` channel — none of them block each other. |
| **Context budget scales with N findings, not N raw outputs** | Each subagent's terminal report becomes a single `Alert` row in the parent's queue. The parent's pre-turn drain prepends them as a compact system message — the parent never sees raw `kubectl get pods/events/logs` output, just the digests. |
| **Capacity, depth, and budget caps** | `WithBackgroundMaxConcurrent(8)` caps how many subagents can be Running at once. `WithBackgroundDefaultBudgets(...)` sets the default `MaxTurns` / `MaxCostUSD` / `MaxWallclock`. Per-spawn args (`max_turns`, etc.) override. Spawn requests that would exceed caps come back as a tool-result error the model can adapt to (e.g. "wait for sibling to finish first"). |
| **Alert lifecycle hooks** | `mgr.OnAlert(func(a Alert) { ... })` lets a host (TUI, observability sink, AX harness) react to alerts as they land. The parent's `Run` ALSO consumes alerts via `PrependPendingAlerts` on its next turn — the two paths are independent; both fire. |

## Wiring (3 lines of substrate)

```go
mgr, _ := agent.NewBackgroundAgentManager(
    agent.WithBackgroundProvider(subagentProvider, "echo"),
    agent.WithBackgroundMaxConcurrent(8),
    agent.WithBackgroundDefaultBudgets(agent.BackgroundBudgets{MaxTurns: 1}),
)
parent, _ := agent.New(parentLLM,
    agent.WithBackgroundManager(mgr),                       // attach manager to parent
    agent.WithTools(agent.NewBackgroundSpawnTools(mgr)),    // model-facing spawn/list/check/stop
)
```

That's all the agent-side glue. Everything else is whatever your parent
normally does (instruction, model, other tools).

## Adapting to a real GKE incident-triage workflow

The example is intentionally hermetic. To drive against a live cluster:

### 1. Swap the parent provider to a real LLM

```go
parentProvider := anthropic.NewVertex(...)   // or gemini.NewVertex(...)
parentLLM, _ := parentProvider.Model(ctx, "claude-sonnet-4-6")
```

Drop the `parentScript` constant + the scripted-mock construction.

### 2. Swap the subagent provider to a cheaper LLM

```go
subagentProvider := gemini.NewAPIKey(...)
mgr, _ := agent.NewBackgroundAgentManager(
    agent.WithBackgroundProvider(subagentProvider, "gemini-2.5-flash"),
    ...
)
```

The cost-efficiency lever: parent on the frontier model (good at
synthesis), subagents on a fast/cheap model (good enough at digesting
known-shape data like `kubectl` output). Same pattern as the
`--agentic-tools --agentic-small-model` substrate in `cmd/core-agent`.

### 3. Register real `kubectl` tools in the manager's catalog

The subagents need actual tools to do work. `BackgroundAgentManager`
takes a tool catalog at construction time; subagents request a subset
by name via the `tools` arg on `spawn_agent`.

```go
kubectlTools := []tool.Tool{
    kubectl.GetPods(),       // your custom tools — see pkg/tools for the shape
    kubectl.GetEvents(),
    kubectl.Logs(),
    kubectl.Describe(),
}
mgr, _ := agent.NewBackgroundAgentManager(
    agent.WithBackgroundProvider(subagentProvider, "gemini-2.5-flash"),
    agent.WithBackgroundCatalog(kubectlTools),
)
```

Then the parent's `spawn_agent` call lists the tools the subagent
should get:

```json
{
  "name": "triage-checkout-svc",
  "system_prompt": "you are a GKE on-call investigator. Run focused diagnostics. Return a 3-bullet digest.",
  "goal": "check checkout-svc in payments-prod: pod status, recent restarts, last 50 log lines, recent events",
  "tools": ["kubectl_get_pods", "kubectl_get_events", "kubectl_logs"],
  "max_turns": 5,
  "max_wallclock_seconds": 60
}
```

### 4. (Optional) Operator surface

For human-in-the-loop, run the parent under `core-agent --attach-listen`
and attach with `core-agent-tui`. The operator sees each spawned
investigator in `/subagents`, the alert stream lands inline in chat
via the LiveAgent observer mode, and the synthesis emits as the parent's
next turn. See [`docs/site/content/docs/reference/attach-tui.md`](../../docs/site/content/docs/reference/attach-tui.md).

For headless / scheduled (cron-driven post-deploy verification, etc.),
drive the parent with `--no-repl` + `--attach-listen` and POST the
incident prompt via `/sessions/<sid>/inject` from your scheduler.

## Composing with the rest of the substrate

- **Audit log**: spawn the parent with `agent.WithEventLog(handle)` and
  pass the same handle into the manager (via the parent — subagents
  inherit). Each subagent's events land in a branch (`sub:triage-*`) of
  the parent's session row. `handle.Stream.Since(ctx, 0, WithSessionTree(...))`
  returns parent + all descendants in one query. See `examples/with-subagent`
  for the audit-query shape.
- **Cross-machine fan-out**: the in-process manager pools subagents on
  one binary. For fan-out across pods or regions, compose with the AX
  integration (`extras/ax-agent/`) — see [`docs/ax-integration-audit.md`](../../docs/ax-integration-audit.md).
- **Scheduled / autonomous mode**: the parent can run under
  `agent.RunAutonomous` (driven by a Scheduler) with `spawn_agent` in
  its tool palette — the model decides when to fan out based on what
  it observes. See `examples/scheduled-monitor` for the substrate
  shape.

## Expected output

```
== turn 1: incident dispatch ==
operator: payments-prod is degraded — investigate all four services in parallel and report back
  → spawn_agent(triage-api-gateway)
  → spawn_agent(triage-checkout-svc)
  → spawn_agent(triage-fraud-detector)
  → spawn_agent(triage-notification-svc)
  ← alert  triage-notification-svc         deferred: stopped: max_turns_exceeded
  ← alert  triage-api-gateway              deferred: stopped: max_turns_exceeded
  ← alert  triage-fraud-detector           deferred: stopped: max_turns_exceeded
  ← alert  triage-checkout-svc             deferred: stopped: max_turns_exceeded
  ← spawn_agent -> status=running
  ← spawn_agent -> status=running
  ← spawn_agent -> status=running
  ← spawn_agent -> status=running
  parent: Dispatched 4 investigators against payments-prod (api-gateway, checkout-svc, fraud-detector, notification-svc). I'll synthesize when their reports land.

== waiting for investigators to complete ==
  ✓ triage-notification-svc (status=deferred)
  ✓ triage-fraud-detector (status=deferred)
  ✓ triage-api-gateway (status=deferred)
  ✓ triage-checkout-svc (status=deferred)

== turn 2: synthesis ==
operator: what did the investigators find?

  parent synthesis:
  Triage roll-up across the four payments-prod services: api-gateway and notification-svc are healthy. checkout-svc has 3 CrashLoopBackOff pods correlating with a fraud-detector connection-pool exhaustion. Root cause is in fraud-detector; recommend roll-back of fraud-detector to the prior image while we continue. Mitigation can run while the investigators continue.

== done ==
```

The `deferred: stopped: max_turns_exceeded` status is an artifact of
the echo subagent provider running out of its 1-turn budget — in a
real run with a Flash subagent the status would be `task_completed`
and the alert body would carry the actual digest the subagent
produced. The example's synthesis text is scripted; with a real parent
LLM it would derive from the alert bodies.
