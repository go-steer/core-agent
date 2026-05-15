# Autonomous-runtime plan

## Status (2026-05-15): shipped

This plan was the design intent before the autonomous-run driver
shipped. Most decisions in the plan landed as written; a few were
adjusted during implementation. Use this doc as **historical
context** — design alternatives, why-this-not-that — not as a
description of the current API.

**Canonical references for what shipped:**
- README's M3 milestone entry — high-level summary
- [`docs/site/content/docs/autonomous.md`](./site/content/docs/autonomous.md) — user-facing reference
- [`docs/site/content/docs/library-api.md`](./site/content/docs/library-api.md) Autonomous + Crash-resume sections — API surface
- [`docs/eventlog-decisions.md`](./eventlog-decisions.md) Phase 3 — `ResumeAutonomous` design + the discovery that only `Completed` should short-circuit resume

---

## Recommendation summary

Ship two related but distinct pieces, in this order:

1. **`tools.NewLifecycleTool`** — generic status emitter the model uses to signal state ("thinking", "blocked", "task_completed", "ask_user", custom). Consumer-supplied handler decides where the events go: stdout, a status file (Scion's pattern), an orchestrator's event log (AX's pattern), a websocket, etc. Useful **both** standalone and inside orchestrator adapters.

2. **`agent.RunAutonomous(ctx, build, goal, opts...)`** — multi-turn driver for **standalone** unattended runs. Loops `agent.Run` toward a goal, enforces run-level budgets (turns / tokens / cost / wallclock + per-turn timeout), watches for a termination signal from the model via an internally-registered done tool, and returns a structured `RunResult`.

Crash-resume and pause-mid-run are explicitly **deferred** — both depend on M3's file-backed sessions and on a session-state checkpoint design that doesn't exist yet.

The plan deliberately separates the two pieces: orchestrator-driven runs (AX today, others coming) **don't use** `RunAutonomous` — the orchestrator IS the driver and calls `agent.RunWithContents` per turn. Those adapters share infrastructure with standalone autonomous runs (lifecycle tool, recording wrapper, mock providers, ask_user) but the loop shape is different. See §"Two shapes of autonomous use" below for the split.

## Context

`docs/autonomous.md` (parked) frames the autonomy story today:

- **Within one turn** — already supported. `agent.Run` drives the model through tool-call cycles; bounded by `cfg.Agent.MaxSteps` (default 50).
- **Across turns** — not shipped. The doc says "~10 lines to write" but that hand-waves the real work: budget enforcement, termination signaling, failure policy, run-level usage rollup.
- **Asking the user during autonomous runs** — shipped today via `tools.NewAskUserTool` + the `--ask` CLI flag (commits `d905b08`, `2e49ae6`).

The five remaining unbuilt items per the doc:

1. `agent.RunAutonomous(...)` driver
2. Built-in `report_done` / `set_status` lifecycle tools
3. Crash-resume (M3-blocked)
4. Run-level budgets (steps/tokens/cost/wallclock)
5. Backpressure / human checkpoints

This plan covers (1), (2), (4), and (5) with a unified design; defers (3) explicitly. It also surfaces gaps the doc didn't mention — listed in §"Anything we've overlooked" below.

### Real consumer demand exists today

This is **not** speculative work — it's responding to active consumer demand:

- **AX** (private `axplore` branch) is a non-Scion orchestrator that drives core-agent for unattended runs. The adapter is shipped; what it lacks is a clean lifecycle-emission story (today the agent's events flow to AX as `AgentOutputs` but there's no explicit "I'm waiting" / "I'm done" signal — the lifecycle tool fills that gap).
- **More orchestrators are coming** — likely shapes include other agent-runtime products, Vertex AI Agents-style hosts, and bespoke internal harnesses. Each will follow the `extras/ax-agent/` adapter pattern. They all benefit from the same shared infrastructure (lifecycle tool, recording wrapper, mock providers, ask_user).
- **Standalone autonomous use** — long-running unattended runs without an external orchestrator (batch jobs, CI tasks, scheduled workers) need the multi-turn driver with budget caps. No external consumer named for this yet, but it's the natural shape for half of the use cases that AGENTS.md prose describes.

### Two shapes of autonomous use

Important distinction, because the same word "autonomous" covers two different shapes that need different code:

| Shape | Driver | API | Where state lives | Examples |
|---|---|---|---|---|
| **Standalone** | `agent.RunAutonomous` (this plan) | Library call returning `RunResult` | In-process session (M3: file-backed) | Batch jobs, CI tasks, scheduled workers, the kube-agents `upgrade-coordinator` if run outside Scion |
| **Orchestrator-driven** | The orchestrator (AX, future others) calls our adapter; adapter calls `agent.RunWithContents` per turn | Adapter-specific (gRPC for AX, etc.) | In the orchestrator (AX event log, etc.) | `extras/ax-agent/`, `extras/scion-agent/`, future orchestrator adapters |

`RunAutonomous` doesn't replace orchestrator adapters — it's the layer for when *core-agent itself* is the orchestrator. Orchestrator adapters get their loop from the orchestrator and just translate per-turn.

Shared infrastructure both shapes need (and that this plan delivers via §"Lifecycle tool"):

- **Lifecycle / status emission** — model signals "thinking" / "blocked" / "done" so the surrounding system (CLI, AX UI, status file) can render it
- **Termination signal tool** — the done-tool for `RunAutonomous` is also useful for orchestrator-driven runs that want a clean "I'm finished this assignment" gesture
- **`ask_user`** — shipped, works in both shapes
- **Recording wrapper** — shipped, works in both shapes
- **Mock providers** — shipped, lets tests cover both shapes credential-free

## Design decisions

| Decision | Choice | Why |
|---|---|---|
| API shape | `RunAutonomous(ctx, *Agent, goal string, ...AutonomousOption) (RunResult, error)` | Mirrors the existing `runner.Headless` shape. Variadic options scale with new knobs without breaking signatures. Returns a structured result rather than printing — the driver is library-shaped, not CLI-shaped. |
| Termination signal | An internal "done" tool the driver registers, plus an optional consumer-facing `WithDoneToolName` override | Tool-based termination is the most reliable shape (model has an explicit gesture). Marker-phrase detection is brittle and rejected. The driver registers the tool itself so consumers don't have to wire it. |
| Lifecycle / status emission | Separate `tools.NewLifecycleTool` — consumers wire it for UI/log/file destinations | Keep "I'm done" (driver-internal) separate from "I'm in state X" (consumer-handled). Generic LifecycleTool handles Scion-like, file-watch, websocket-emit, etc. via a `LifecycleHandler` callback. |
| Budgets | `WithMaxTurns`, `WithMaxTokens(in, out)`, `WithMaxCost(usd)`, `WithMaxWallclock(d)` — all optional, all checked between turns | Between-turn enforcement is simple and predictable. Mid-turn enforcement (cancelling a tool mid-stream) is messier and rejected for v1. |
| Per-turn timeout | `WithPerTurnTimeout(d)` — wraps each `agent.Run` in a `context.WithTimeout` | Distinct from wallclock; a single rogue turn shouldn't hang the run. |
| Failure policy | Default: any iterator error aborts. `WithRetryPolicy(func(err, attempt int) RetryDecision)` opt-in. | Most autonomous use cases want fail-fast. Retries make sense for transient transport errors (rate limits, 5xx) but the right policy varies; ship the hook, don't bake a default. |
| Continuation prompt | Default `"continue"`; `WithContinuationPrompt(s)` overrides | After a turn ends without a done signal, the driver needs *something* to send for the next turn. "continue" is neutral. Real consumers will customize. |
| Permissions guard | Driver refuses to run with `cfg.Permissions.Mode == "ask"` unless a Prompter was wired into the gate | `ask` mode would deadlock waiting for human approval that nobody's there to give. Hard error at startup beats hanging at first tool call. |
| Conversation history | Reuse the agent's session across turns (default behavior) | Same as `agent.Run` repeated. The agent already accumulates history; the driver doesn't reset between turns unless the consumer constructs a fresh agent. |
| Telemetry | Each turn gets an OTEL span; run-level totals roll up via existing `usage.Tracker` | No new telemetry abstraction; reuse what exists. Tracker already accumulates per-turn; driver passes the same tracker to every turn. |
| Recording integration | Free — `recording.NewRecorder` wraps the LLM; the driver doesn't see it | Already-shipped composition. Document the pattern. |
| Mock provider integration | Free — scripted/echo are LLMs; the driver doesn't care which | Tests can use scripted-mode runs to pin termination behavior. |
| Pause / resume | **Deferred.** Needs design for "what does pause mean" and depends on M3 file-backed sessions for serialization. | The right shape isn't obvious without a real consumer ask. Sketched in §"Out of scope" below. |
| Crash-resume | **Deferred to M3.** Requires file-backed `session.Service` + driver state checkpoint format. | Same dependency. Plan documents the seam (`agent.WithSession` is already there). |

## Files

### New
- `agent/autonomous.go` — `RunAutonomous`, all `With*` options, `RunResult`, `StopReason`, `RetryPolicy`/`RetryDecision`. The internal done-tool registration lives here too (uses `tools.NewLifecycleTool` under the hood).
- `agent/autonomous_test.go` — table tests using `models/mock`'s scripted provider to pin: termination on done-tool call, budget enforcement (each kind), context cancellation, retry policy, permissions guard.
- `tools/lifecycle.go` — `NewLifecycleTool`, `LifecycleEvent`, `LifecycleHandler`, `LifecycleOptions`. Independent of the autonomous driver; reusable.
- `tools/lifecycle_test.go` — tool declaration shape; handler invocation; allowed-states enforcement.

### Modified
- `docs/autonomous.md` — promote from parked to committed once impl lands. Update the "What's not built" section to reflect the new state. Cross-link to the API.
- `docs/site/content/docs/library-api.md` — new section "Autonomous runs" between "Recording LLM turns" and "Adding custom tools."
- `docs/DESIGN.md` — short subsection under the existing "Built-in tools" or a new sibling section explaining the autonomous-driver design choice (driver vs. consumer loop) and the deferral of pause/resume.
- `examples/autonomous/main.go` — worked end-to-end example using `--provider=scripted` so it runs credential-free.

### Not modified
- `cmd/core-agent/main.go` — no `--autonomous` flag in v1. The bundled binary is a REPL/headless one-shot tool; long-running autonomous use is a library/script concern. Adding a flag is a small follow-up if a consumer asks.
- `extras/scion-agent/main.go` — Scion has its own loop and lifecycle conventions. Not migrating it; it remains the authoritative "how Scion does it" reference. The plan's design is informed by Scion's pattern but doesn't replace it.
- `models/`, `recording/`, `permissions/` — no changes; the autonomous driver composes these.

## Implementation

### 1. `tools/lifecycle.go` — the generic status emitter

```go
package tools

// LifecycleEvent is what NewLifecycleTool delivers to its handler
// each time the model calls the tool. State is the value the model
// passed; Detail is the optional human-readable context. Time is
// when the handler received it.
type LifecycleEvent struct {
    State  string
    Detail string
    Time   time.Time
}

// LifecycleHandler receives each emit. Returning a non-nil error
// surfaces the error string back to the model as the tool's
// response — useful for "this state isn't valid right now" kinds
// of feedback. nil error returns a generic "ack" to the model.
type LifecycleHandler func(ctx context.Context, ev LifecycleEvent) error

type LifecycleOptions struct {
    Handler       LifecycleHandler // required
    Name          string           // default "set_status"
    Description   string           // default sentence describing emit semantics
    AllowedStates []string         // optional: reject states not in this set
}

func NewLifecycleTool(opts LifecycleOptions) (tool.Tool, error)
```

Tool args: `{state string, detail string}`. Tool result: `{ack string}` ("ok" on success, error message on rejection).

### 2. `agent/autonomous.go` — the driver

```go
package agent

// RunAutonomous drives a multi-turn loop against a, sending goal as
// the first prompt and a continuation prompt thereafter, until one
// of the stop conditions fires. Returns a RunResult describing why
// it stopped and the totals it accumulated, plus any error.
//
// The driver registers an internal "done" tool the model calls to
// signal completion; the tool name is "report_done" by default and
// can be overridden with WithDoneToolName.
func RunAutonomous(ctx context.Context, a *Agent, goal string, opts ...AutonomousOption) (RunResult, error)

type AutonomousOption func(*autoConfig)

// Budget options. All zero-valued mean "no limit" except MaxTurns,
// which defaults to 50 (same scale as cfg.Agent.MaxSteps for
// per-turn cycles).
func WithMaxTurns(n int) AutonomousOption
func WithMaxTokens(input, output int) AutonomousOption
func WithMaxCost(usd float64) AutonomousOption
func WithMaxWallclock(d time.Duration) AutonomousOption
func WithPerTurnTimeout(d time.Duration) AutonomousOption

// Behavioral options.
func WithDoneToolName(name string) AutonomousOption          // default "report_done"
func WithContinuationPrompt(s string) AutonomousOption       // default "continue"
func WithTracker(t *usage.Tracker, p usage.Pricing) AutonomousOption
func WithProgress(cb func(turn int, ev *session.Event)) AutonomousOption
func WithRetryPolicy(p RetryPolicy) AutonomousOption

type RetryPolicy func(turnErr error, attempt int) RetryDecision
type RetryDecision int
const (
    AbortRun RetryDecision = iota
    RetryTurn
    SkipTurn
)

type RunResult struct {
    Reason       StopReason
    FinalText    string         // accumulated text from the final turn
    Turns        int            // turns actually executed
    InputTokens  int
    OutputTokens int
    CostUSD      float64
    Duration     time.Duration
    DoneDetail   string         // when Reason==Completed: detail the model passed to report_done
}

type StopReason string
const (
    StopReasonCompleted         StopReason = "completed"
    StopReasonMaxTurns          StopReason = "max_turns_exceeded"
    StopReasonMaxTokens         StopReason = "max_tokens_exceeded"
    StopReasonMaxCost           StopReason = "max_cost_exceeded"
    StopReasonWallclockExceeded StopReason = "wallclock_exceeded"
    StopReasonContextCancelled  StopReason = "context_cancelled"
    StopReasonRetryAborted      StopReason = "retry_policy_aborted"
)
```

Internal flow per turn:

1. **Pre-turn budget check** — if any budget breached, set Reason and break.
2. **Per-turn context** — `ctx2, cancel := context.WithTimeout(ctx, perTurnTimeout)` if set.
3. **Run** — iterate `a.Run(ctx2, prompt)`:
   - Tap each event for the done-tool call → set `done = true, detail = ev.FunctionCall.Args["detail"]`
   - Tap each event for `UsageMetadata` → update running totals
   - Forward event to `WithProgress` callback if set
   - Accumulate text from `event.Partial` events into a buffer (the final turn's text becomes `RunResult.FinalText`)
4. **Post-turn**:
   - If iteration errored → consult retry policy; abort/retry/skip
   - If `done == true` → set Reason=Completed, break
   - Else update `prompt = continuationPrompt` and loop

Done-tool registration — done in `RunAutonomous` itself before starting the loop:

```go
doneCh := make(chan string, 1) // buffered: tool returns immediately
doneTool, _ := tools.NewLifecycleTool(tools.LifecycleOptions{
    Name: cfg.doneToolName,
    Description: "Signal that the user's goal is complete. Call this when you've finished the task or determined you cannot proceed.",
    AllowedStates: []string{"done"},  // single-value: simplifies the model's mental model
    Handler: func(_ context.Context, ev tools.LifecycleEvent) error {
        select {
        case doneCh <- ev.Detail:
        default: // already signaled — no-op
        }
        return nil
    },
})
// Inject into the agent's tool registry. The driver constructs a
// child agent that wraps the caller's a with this extra tool, so
// the caller's agent isn't mutated.
```

The "construct a child agent" step needs care. Two options:

a) **Mutate-then-restore** — bad; not goroutine-safe.
b) **Construct a fresh agent** inside the driver using the same options as `a`, plus the done tool.

Option (b) requires the agent to expose its construction options. It doesn't today. **The cleanest fix is to add `agent.WithExtraTools(...tools)` that the driver uses internally** — accepts a tool slice and merges with the existing tool set. ~5 lines on `agent.go`.

Alternative: take an `agent.Agent` constructor function instead of an instance. `RunAutonomous(ctx, build func() (*Agent, error), goal, opts...)`. Cleaner separation but heavier API.

**Recommend** the constructor variant: `RunAutonomous(ctx, build func(extraTools []tool.Tool) (*Agent, error), goal, opts...)`. The driver passes `[]tool.Tool{doneTool}` to `build`; consumer composes with their own tools. This avoids the "extra tools" plumbing on `agent.New` and keeps the agent's public API unchanged.

### 3. Permissions guard

Before the loop:

```go
if a.PermissionMode() == permissions.ModeAsk && !a.HasPrompter() {
    return RunResult{}, fmt.Errorf("agent: RunAutonomous requires permissions mode != ask, or a wired Prompter; saw mode=ask with no prompter (would deadlock)")
}
```

Needs `Agent.PermissionMode()` and `Agent.HasPrompter()` accessors. Or — simpler — the driver inspects `cfg.Permissions.Mode` from a passed-in cfg. Either works; pick whatever fits the constructor variant cleanest.

### 4. `examples/autonomous/main.go`

Worked example using `--provider=scripted` so it runs credential-free. Scripted transcript:

- Turn 1: model emits a tool call to `read_file(path="example.txt")`, gets back its content
- Turn 2: model emits text "Here's a summary..." then calls `report_done(detail="summarized example.txt")`
- Driver returns RunResult{Reason: Completed, DoneDetail: "summarized example.txt", Turns: 2}

Single binary, runs in <1s, demonstrates the loop + termination + tool integration.

## Tests

`agent/autonomous_test.go`:

- `TestRunAutonomous_StopsOnDoneTool` — scripted LLM emits report_done on turn 2 → Reason=Completed, Turns=2.
- `TestRunAutonomous_StopsOnMaxTurns` — scripted LLM never emits done; cap at 3 turns → Reason=MaxTurns.
- `TestRunAutonomous_StopsOnMaxTokens` — pin pricing, scripted LLM with high token usage → Reason=MaxTokens.
- `TestRunAutonomous_StopsOnMaxCost` — same pattern with cost cap.
- `TestRunAutonomous_StopsOnWallclock` — slow scripted LLM (artificial sleep) + tight wallclock → Reason=WallclockExceeded.
- `TestRunAutonomous_StopsOnPerTurnTimeout` — single hung turn → returns retry-policy decision.
- `TestRunAutonomous_StopsOnContextCancel` — caller cancels mid-run → Reason=ContextCancelled.
- `TestRunAutonomous_RetryPolicy_RetriesTransient` — failure policy returns RetryTurn → driver retries.
- `TestRunAutonomous_RetryPolicy_AbortsAfter` — policy returns AbortRun on Nth attempt → Reason=RetryAborted.
- `TestRunAutonomous_RejectsAskModeWithoutPrompter` — permissions mode=ask, no prompter → error before first turn.
- `TestRunAutonomous_TracksTokensAndCost` — across multi-turn run, totals match expected.
- `TestRunAutonomous_ProgressCallbackFires` — every event seen by callback in order.
- `TestRunAutonomous_FinalTextIsLastTurnText` — multi-turn, only the final turn's text appears in RunResult.FinalText.

`tools/lifecycle_test.go`:

- `TestLifecycleTool_DeliversToHandler` — call → handler sees the LifecycleEvent.
- `TestLifecycleTool_AllowedStatesRejection` — state outside AllowedStates → error returned to model.
- `TestLifecycleTool_HandlerErrorBecomesToolResult` — handler returns err → tool result contains error message.
- `TestLifecycleTool_DefaultsNameAndDescription`.
- `TestLifecycleTool_RequiresHandler`.

## Documentation

- `docs/autonomous.md` — promote from parked. Update "What's not built" to reflect the new state (driver, lifecycle, budgets all ship; pause/resume + crash-resume still pending). Add a worked example that uses `agent.RunAutonomous` directly.
- `docs/site/content/docs/library-api.md` — new section "Autonomous runs" after "Recording LLM turns." Cover the constructor pattern, budgets, termination signaling, recording-wrapper composition, scripted-mode testing.
- `docs/DESIGN.md` — under "Built-in tools" or a new sibling section, explain the driver-vs-consumer-loop choice (driver because it's the one place to enforce budgets correctly; consumers can still BYO loop with `agent.Run` for unusual shapes), and explicitly defer pause/resume + crash-resume with M3 dependency notes.
- `README.md` — one bullet under Features: "Autonomous-run driver — `agent.RunAutonomous` for unattended multi-turn workers with budget caps and termination signaling. Pair with `--ask=auto` for instructions that say 'ask before doing X' so the agent gets a clean refusal in headless contexts instead of blocking."

## Verification

```bash
cd /home/user/projects/core-agent
go test ./agent/... ./tools/...
go vet ./...
go build ./...
for s in dev/ci/presubmits/*; do bash "$s"; done

# End-to-end smoke (no creds):
go run ./examples/autonomous
# Expected output: scripted-mode run completes in <1s with a
# RunResult printed at the end showing Reason=completed, Turns=2.

# Real smoke against a credentialled provider (manual):
GEMINI_API_KEY=... go run ./examples/autonomous --real \
    "find every TODO comment in the codebase and write a tracking doc"
# Expected: agent uses bash/grep/read_file/write_file across multiple
# turns, calls report_done when finished. Final RunResult.FinalText
# is the agent's summary; Turns matches what the model needed.
```

## Anything we've overlooked

Cross-checking the parked doc against the design space:

- ✅ Multi-turn driver — covered (§2)
- ✅ Lifecycle / status tool — covered (§1)
- ✅ Run-level budgets — covered (steps via MaxTurns, tokens via MaxTokens, cost via MaxCost, wallclock via MaxWallclock, per-turn timeout)
- ✅ Run-level usage rollup — covered via `WithTracker`
- ✅ Backpressure / human checkpoints — partially covered: the lifecycle tool plus a custom handler can implement a per-turn human-confirm gate by blocking in `LifecycleHandler` until the user approves. Documented as a recipe; no new API.
- ✅ Permission-mode interaction — caught by the §3 guard
- ✅ Failure / retry — covered (§"Failure policy" decision + RetryPolicy)
- ✅ Context cancellation — covered (StopReasonContextCancelled)
- ✅ Per-turn timeout — covered (WithPerTurnTimeout)
- ✅ Telemetry / OTEL — reuses existing telemetry; document the span shape
- ✅ Recording integration — composes with `recording.NewRecorder` transparently
- ✅ Mock provider integration — works with scripted/echo for tests

Things the doc didn't mention but worth flagging:

- **Continuation prompt design** — "continue" is bland. Smarter defaults could analyze the prior turn ("what's your next step?") but that adds complexity. Ship simple; document the override.
- **Final text vs structured output** — `RunResult.FinalText` is a string. If a consumer wants a JSON object back, they can either (a) instruct the model to call `report_done(detail="<json>")` and parse the detail, or (b) parse `FinalText`. Don't bake structured-output extraction in.
- **Side-effect undo** — autonomous runs that fail partway can leave the workspace dirty. Out of scope; that's a per-tool / consumer-policy concern.
- **Cost estimation before running** — "this run will cost up to $X" is useful pre-flight. Trivial to compute from MaxTokens × Pricing; document as a recipe.
- **Multiple "done" signals** — what if the model calls report_done twice? Driver handles via buffered channel + select-default; second call is a no-op.
- **Idempotency of report_done across retries** — if a turn errors after report_done was already called, the channel is closed; retry won't re-fire. Document.
- **Test fixtures with the scripted provider** — scripted's JSONL needs FunctionCall events for the tool-call tests. The format already supports this; our existing recording.RecordedTurn shape carries them through. Verify by recording a real session that includes report_done and re-using.

## Out of scope (deferred)

- **Pause / resume mid-run.** The right shape isn't obvious. Pause-between-turns is doable today (consumer cancels context after some signal, then re-runs with stored history); pause-mid-turn requires interrupting the agent loop in a recoverable way that ADK doesn't expose. Defer pending consumer ask.
- **Crash-resume.** Blocked on M3's file-backed sessions. The serialization of `RunResult.{Turns, Tokens, Cost, ...}` and the goal/continuation-prompt state is straightforward; the hard part is session resurrection. Note: when M3 lands, this becomes ~50 lines of "load session, load checkpoint, call RunAutonomous from the right turn number." Plan a follow-up then.
- **Distributed / multi-node autonomous orchestration.** The AX adapter (private `axplore` branch) is the closest answer; the planner-as-orchestrator pattern handles it. No need for a competing in-library shape.
- **Streaming `RunResult` updates.** Could add a `WithProgress` shape that surfaces partial results; the simpler `WithProgress(callback per event)` we have covers the common case. Defer richer shapes until needed.
- **HTTP / web monitoring UI.** Pure consumer territory.
- **Goal refinement / self-planning.** Out of scope; that's a model-side concern. Subagents-plan covers the in-process subagent shape if a consumer wants a planner+executor split.
- **Concurrent autonomous workers.** Each `RunAutonomous` call is independent; consumers spawn goroutines if they want N workers. No new API needed.

## Build order (active work, not parked)

Order matters: the lifecycle tool unblocks both shapes (orchestrator adapters and standalone runs), so it goes first and immediately benefits the AX adapter on `axplore`.

1. **`agent.WithSession` / constructor variant decision** — pick the shape (constructor function vs. `WithExtraTools`) before writing the driver. (~30 min of design talk.)
2. **`tools/lifecycle.go`** + tests — small, independent, useful immediately. Once shipped:
   - The AX adapter on `axplore` can wire it for proper "I'm blocked / I'm done" signaling back to AX (step 3 below).
   - Future orchestrator adapters adopt the same pattern.
   - The standalone driver in step 4 uses it internally.
   (~3 hours w/ tests.)
3. **Wire lifecycle into `extras/ax-agent/`** on `axplore`. Adapter registers a LifecycleTool whose handler emits a corresponding AX `AgentOutputs` event flagged `internal_only: true` (the AX UI sees the state but the conversation history stays clean). First real consumer integration. (~1 hour.)
4. **`agent/autonomous.go`** + tests — standalone driver. Uses the lifecycle tool from (2) as its termination mechanism (registers a single-state "done" instance internally). Needs (1) decided. (~1 day w/ tests.)
5. **`examples/autonomous/main.go`** — credential-free demo via scripted provider. (~30 min once driver works.)
6. **Docs** — promote autonomous.md from parked, add `library-api.md` section, README bullet. (~1 hour.)
7. **Future: orchestrator-adapter shared helpers** — when the second non-AX orchestrator arrives, factor common adapter code (lifecycle wiring, AgentStart→genai conversion, gRPC/HTTP server skeleton) into a shared `extras/orchestrator-common/` or similar. **Defer until the second adapter exists** — YAGNI.
8. **Future: `--autonomous` CLI flag** on `cmd/core-agent` for non-library consumers. ~1 hour; defer until someone asks.

Total estimated effort to step 6: ~2 dev-days plus docs.

## When the deferred pieces become active

- **Pause / resume mid-run** — when a consumer concretely needs it. The orchestrator-driven shape covers most "pause" semantics naturally (the orchestrator just stops calling the adapter); standalone needs more design.
- **Crash-resume** — when M3 ships file-backed sessions. At that point ~50 lines of "load session, load checkpoint, resume from turn N."
- **Orchestrator-adapter common helpers** — when the second non-AX adapter starts; factoring is easier with two examples in hand than one.
