// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/permissions"
	coretools "github.com/go-steer/core-agent/tools"
	"github.com/go-steer/core-agent/usage"
)

// RunAutonomous drives a multi-turn loop against an Agent built by
// build, sending goal as the first prompt and a continuation prompt
// thereafter, until one of the stop conditions fires. Returns a
// RunResult describing why it stopped and the totals it accumulated,
// plus any error.
//
// The driver constructs the agent via build, passing in an extra
// "done" tool the model calls to signal completion. The tool name is
// "report_done" by default and can be overridden with
// WithDoneToolName. Consumers compose the done tool with their own
// tool registry inside build (see examples/autonomous for the
// pattern).
//
// The constructor pattern keeps the driver from mutating a
// caller-supplied Agent (which would race with concurrent runs) and
// keeps agent.New's surface free of "extra tools" plumbing that only
// matters here.
func RunAutonomous(ctx context.Context, build func(extraTools []tool.Tool) (*Agent, error), goal string, opts ...AutonomousOption) (RunResult, error) {
	if build == nil {
		return RunResult{}, fmt.Errorf("agent: RunAutonomous: build is required")
	}
	if strings.TrimSpace(goal) == "" {
		return RunResult{}, fmt.Errorf("agent: RunAutonomous: goal is required")
	}
	cfg := defaultAutoConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	// Permissions deadlock guard. If the consumer wired a gate via
	// WithPermissionsGate and the gate is in ask-mode without a
	// prompter, the first tool call will fail with ErrNoPrompter
	// after wasting an LLM round-trip. Catch it before the loop
	// starts. When the consumer doesn't pass a gate we can't
	// introspect their wiring; the docs steer them to ModeYolo or
	// ModeAllow for unattended runs.
	if cfg.permissionsGate != nil {
		g := cfg.permissionsGate
		if g.Mode() == permissions.ModeAsk && !g.HasPrompter() {
			return RunResult{}, fmt.Errorf("agent: RunAutonomous: permissions gate is in ask-mode with no Prompter; would deadlock on first tool call (use ModeYolo / ModeAllow for unattended runs, or wire a Prompter)")
		}
	}

	doneCh := make(chan string, 1)
	doneTool, err := coretools.NewLifecycleTool(coretools.LifecycleOptions{
		Name:          cfg.doneToolName,
		Description:   cfg.doneToolDescription,
		AllowedStates: []string{"done"},
		Handler: func(_ context.Context, ev coretools.LifecycleEvent) error {
			select {
			case doneCh <- ev.Detail:
			default:
				// Already signaled; treat the second call as a no-op
				// rather than blocking the tool handler.
			}
			return nil
		},
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("agent: RunAutonomous: build done tool: %w", err)
	}

	// Optional schedule tool: only wired when a Scheduler is installed
	// via WithScheduler. Loops without a scheduler never see the tool,
	// so the model can't emit schedule intent the driver doesn't know
	// how to honor.
	extras := []tool.Tool{doneTool}
	var scheduleCh <-chan coretools.ScheduleEvent
	if cfg.scheduler != nil {
		schTool, ch, err := coretools.NewScheduleTool(coretools.ScheduleOptions{
			Name:        cfg.scheduleToolName,
			Description: cfg.scheduleToolDescription,
			MaxDefer:    cfg.scheduleToolMaxDefer,
		})
		if err != nil {
			return RunResult{}, fmt.Errorf("agent: RunAutonomous: build schedule tool: %w", err)
		}
		extras = append(extras, schTool)
		scheduleCh = ch
	}

	a, err := build(extras)
	if err != nil {
		return RunResult{}, fmt.Errorf("agent: RunAutonomous: build agent: %w", err)
	}
	if a == nil {
		return RunResult{}, fmt.Errorf("agent: RunAutonomous: build returned nil agent")
	}

	startedAt := time.Now()
	prompt := goal
	result := RunResult{}

	// Convenience: emit a final checkpoint with the configured
	// stop reason regardless of which exit path the loop takes.
	// Skipped when the agent has no event log; checkpoints are only
	// useful for durable sessions.
	emitFinalCheckpoint := func(reason StopReason) {
		_ = emitCheckpoint(ctx, a, checkpointPayload{
			Turn:               result.Turns,
			InputTokens:        result.InputTokens,
			OutputTokens:       result.OutputTokens,
			CostUSD:            result.CostUSD,
			Goal:               goal,
			ContinuationPrompt: cfg.continuationPrompt,
			StopReason:         string(reason),
			DoneDetail:         result.DoneDetail,
			FinalText:          result.FinalText,
		})
	}

	for {
		// Pre-turn budget checks.
		if cfg.maxWallclock > 0 && time.Since(startedAt) >= cfg.maxWallclock {
			result.Reason = StopReasonWallclockExceeded
			break
		}
		if cfg.maxTurns > 0 && result.Turns >= cfg.maxTurns {
			result.Reason = StopReasonMaxTurns
			break
		}
		if cfg.maxInputTokens > 0 && result.InputTokens >= cfg.maxInputTokens {
			result.Reason = StopReasonMaxTokens
			break
		}
		if cfg.maxOutputTokens > 0 && result.OutputTokens >= cfg.maxOutputTokens {
			result.Reason = StopReasonMaxTokens
			break
		}
		if cfg.maxCostUSD > 0 && result.CostUSD >= cfg.maxCostUSD {
			result.Reason = StopReasonMaxCost
			break
		}
		if err := ctx.Err(); err != nil {
			result.Reason = StopReasonContextCancelled
			result.Duration = time.Since(startedAt)
			emitFinalCheckpoint(StopReasonContextCancelled)
			return result, err
		}

		// BeforeTurn hook (used by AutonomousHandle to implement
		// Pause). Runs after budget + ctx checks; may block (e.g.
		// pause waits for resume) and may return an error to abort.
		if cfg.beforeTurn != nil {
			if err := cfg.beforeTurn(ctx, result.Turns+1); err != nil {
				// Treat hook-returned errors as a stop signal. If the
				// ctx itself was cancelled while the hook was blocked,
				// classify as ContextCancelled to match the rest of the
				// loop; otherwise the hook's error becomes the run
				// error and we use the RetryAborted reason.
				if errors.Is(err, context.Canceled) && ctx.Err() != nil {
					result.Reason = StopReasonContextCancelled
				} else {
					result.Reason = StopReasonRetryAborted
				}
				result.Duration = time.Since(startedAt)
				emitFinalCheckpoint(result.Reason)
				return result, err
			}
		}

		// Per-turn context (timeout is optional).
		turnCtx := ctx
		var cancel context.CancelFunc
		if cfg.perTurnTimeout > 0 {
			turnCtx, cancel = context.WithTimeout(ctx, cfg.perTurnTimeout)
		}

		turnRes, turnErr := runOneTurn(turnCtx, a, prompt, doneCh, scheduleCh, &cfg, result.Turns+1)
		if cancel != nil {
			cancel()
		}

		// Roll up usage from this turn into the overall result.
		result.InputTokens += turnRes.inputTokens
		result.OutputTokens += turnRes.outputTokens
		result.CostUSD += turnRes.costUSD
		result.Turns++
		if turnRes.text != "" {
			result.FinalText = turnRes.text
		}

		if turnErr != nil {
			// Context cancellation propagates immediately regardless of
			// retry policy — the caller asked us to stop.
			if errors.Is(turnErr, context.Canceled) && ctx.Err() != nil {
				result.Reason = StopReasonContextCancelled
				result.Duration = time.Since(startedAt)
				emitFinalCheckpoint(StopReasonContextCancelled)
				return result, turnErr
			}
			decision := AbortRun
			if cfg.retryPolicy != nil {
				decision = cfg.retryPolicy(turnErr, result.Turns)
			}
			switch decision {
			case RetryTurn:
				// Re-run the same prompt next iteration. Treat the
				// failed turn as not-counted so a tight max_turns cap
				// still allows the retry to land — but we keep the
				// turn counter incremented so retry policy's attempt
				// number stays accurate.
				continue
			case SkipTurn:
				// Move on to the continuation prompt as if the turn
				// had completed without producing a done signal.
				prompt = cfg.continuationPrompt
				_ = emitCheckpoint(ctx, a, perTurnCheckpoint(result, goal, cfg.continuationPrompt))
				continue
			default:
				result.Reason = StopReasonRetryAborted
				result.Duration = time.Since(startedAt)
				emitFinalCheckpoint(StopReasonRetryAborted)
				return result, turnErr
			}
		}

		if turnRes.doneSignaled {
			result.Reason = StopReasonCompleted
			result.DoneDetail = turnRes.doneDetail
			break
		}

		// Schedule emission: if the model called schedule_next_turn
		// AND a scheduler is wired, hand the event off to the
		// scheduler between turns. report_done already won above if
		// both were emitted in the same turn.
		if turnRes.scheduleSignaled && cfg.scheduler != nil {
			ev := turnRes.scheduleEvent
			// Driver-level MaxDefer is a silent ceiling — the tool-
			// level cap (ScheduleOptions.MaxDefer) is the model-facing
			// surface. If the model somehow exceeds the driver cap
			// anyway, clamp and log so an operator can spot the drift.
			if cfg.maxDefer > 0 {
				ceiling := time.Now().Add(cfg.maxDefer)
				if ev.WakeAt.After(ceiling) {
					log.Printf("agent: RunAutonomous: clamping scheduler wake-time from %s to driver MaxDefer ceiling %s (max_defer=%s)",
						ev.WakeAt.Format(time.RFC3339), ceiling.Format(time.RFC3339), cfg.maxDefer)
					ev.WakeAt = ceiling
				}
			}

			// Per-turn checkpoint with next_wake_at populated so a
			// crash mid-defer can resume to the right wake-time.
			_ = emitCheckpoint(ctx, a, scheduleCheckpoint(result, goal, cfg.continuationPrompt, ev))

			serr := cfg.scheduler.BeforeNextTurn(ctx, ev)
			switch {
			case serr == nil:
				// Scheduler honored the wait (or no wait was needed).
				// Continue the loop with the model-supplied prompt, or
				// fall back to the default continuation prompt.
				if ev.NextPrompt != "" {
					prompt = ev.NextPrompt
				} else {
					prompt = cfg.continuationPrompt
				}
				continue
			case errors.Is(serr, coretools.ErrSchedulerDefer):
				// Orchestrator-managed exit: the process should end
				// cleanly with the wake-time persisted to the eventlog
				// for ResumeAutonomous to pick up. break out of the
				// for-select via the labeled break below.
				result.Reason = StopReasonDeferred
				result.NextWakeAt = ev.WakeAt
				goto deferredExit // break out of the outer for loop
			case errors.Is(serr, context.Canceled) && ctx.Err() != nil:
				result.Reason = StopReasonContextCancelled
				result.Duration = time.Since(startedAt)
				emitFinalCheckpoint(StopReasonContextCancelled)
				return result, serr
			default:
				// Treat any other scheduler error as a hard abort.
				result.Reason = StopReasonRetryAborted
				result.Duration = time.Since(startedAt)
				emitFinalCheckpoint(StopReasonRetryAborted)
				return result, fmt.Errorf("agent: RunAutonomous: scheduler: %w", serr)
			}
		}

		// Per-turn checkpoint after a clean (non-done, non-error)
		// turn. Per-turn emission is the cursor ResumeAutonomous
		// continues from; a no-checkpoint run can still resume from
		// turn 0 if its session has events but no checkpoints.
		_ = emitCheckpoint(ctx, a, perTurnCheckpoint(result, goal, cfg.continuationPrompt))

		prompt = cfg.continuationPrompt
	}

deferredExit:
	result.Duration = time.Since(startedAt)
	emitFinalCheckpoint(result.Reason)
	return result, nil
}

// perTurnCheckpoint builds the payload for the checkpoint emitted
// after a successful (non-done, non-error) turn. Shared between the
// SkipTurn retry path and the normal continuation path so emissions
// stay consistent.
func perTurnCheckpoint(result RunResult, goal, continuation string) checkpointPayload {
	return checkpointPayload{
		Turn:               result.Turns,
		InputTokens:        result.InputTokens,
		OutputTokens:       result.OutputTokens,
		CostUSD:            result.CostUSD,
		Goal:               goal,
		ContinuationPrompt: continuation,
		FinalText:          result.FinalText,
	}
}

// scheduleCheckpoint extends perTurnCheckpoint with the pending
// wake-time. Emitted before the scheduler is consulted so a crash
// mid-defer can be resumed to the correct wake-time. The continuation
// prompt is intentionally the scheduler-supplied NextPrompt when
// present so resume picks the same prompt the scheduler-honored run
// would have used.
func scheduleCheckpoint(result RunResult, goal, fallbackContinuation string, ev coretools.ScheduleEvent) checkpointPayload {
	continuation := ev.NextPrompt
	if continuation == "" {
		continuation = fallbackContinuation
	}
	return checkpointPayload{
		Turn:               result.Turns,
		InputTokens:        result.InputTokens,
		OutputTokens:       result.OutputTokens,
		CostUSD:            result.CostUSD,
		Goal:               goal,
		ContinuationPrompt: continuation,
		FinalText:          result.FinalText,
		NextWakeAt:         ev.WakeAt,
	}
}

// turnResult captures everything one turn produced that the driver
// cares about. Kept private — callers see RunResult.
type turnResult struct {
	inputTokens      int
	outputTokens     int
	costUSD          float64
	text             string
	doneSignaled     bool
	doneDetail       string
	scheduleSignaled bool
	scheduleEvent    coretools.ScheduleEvent
}

func runOneTurn(ctx context.Context, a *Agent, prompt string, doneCh chan string, scheduleCh <-chan coretools.ScheduleEvent, cfg *autoConfig, turnNo int) (turnResult, error) {
	var (
		out turnResult
		buf strings.Builder
	)

	// Drain any stale done signal from a previous turn (defensive —
	// only one turn is in flight at a time, but a previous turn
	// could have signaled done while a budget cap fired between
	// turns and we're now being re-entered).
	select {
	case <-doneCh:
	default:
	}
	// Same defensive drain on the schedule channel.
	if scheduleCh != nil {
		select {
		case <-scheduleCh:
		default:
		}
	}

	for ev, err := range a.Run(ctx, prompt) {
		if err != nil {
			out.text = buf.String()
			return out, err
		}
		if ev == nil {
			continue
		}
		if cfg.progress != nil {
			cfg.progress(turnNo, ev)
		}
		if u := ev.UsageMetadata; u != nil {
			inTok := int(u.PromptTokenCount)
			outTok := int(u.CandidatesTokenCount)
			out.inputTokens += inTok
			out.outputTokens += outTok
			if cfg.tracker != nil {
				modelName := ""
				if a.inner != nil {
					modelName = a.inner.Name()
				}
				rec := cfg.tracker.Append(modelName, inTok, outTok, cfg.pricing)
				out.costUSD += rec.CostUSD
			} else if !cfg.pricing.IsZero() {
				out.costUSD += cfg.pricing.CostUSD(inTok, outTok)
			}
		}
		if ev.Content != nil {
			for _, p := range ev.Content.Parts {
				if p == nil {
					continue
				}
				if p.Text != "" && ev.Partial {
					buf.WriteString(p.Text)
				}
			}
		}
	}

	// The done signal lives on doneCh because only a successful tool
	// invocation (state="done", handler fired) sets it — false
	// positives like rejected calls from the model never reach us.
	select {
	case detail := <-doneCh:
		out.doneSignaled = true
		out.doneDetail = detail
	default:
	}

	// Same idea for schedule emission. Done wins over schedule when
	// both are emitted in the same turn (the loop check above happens
	// first); we still drain here so a per-turn schedule call doesn't
	// leak forward into the next turn's stale-drain.
	if scheduleCh != nil {
		select {
		case ev := <-scheduleCh:
			out.scheduleSignaled = true
			out.scheduleEvent = ev
		default:
		}
	}

	out.text = buf.String()
	return out, nil
}

// AutonomousOption mutates RunAutonomous configuration. Use the With*
// helpers below.
type AutonomousOption func(*autoConfig)

type autoConfig struct {
	maxTurns                int
	maxInputTokens          int
	maxOutputTokens         int
	maxCostUSD              float64
	maxWallclock            time.Duration
	perTurnTimeout          time.Duration
	doneToolName            string
	doneToolDescription     string
	continuationPrompt      string
	tracker                 *usage.Tracker
	pricing                 usage.Pricing
	progress                func(turn int, ev *session.Event)
	retryPolicy             RetryPolicy
	permissionsGate         *permissions.Gate
	beforeTurn              func(ctx context.Context, turnNo int) error
	scheduler               coretools.Scheduler
	maxDefer                time.Duration
	scheduleToolName        string
	scheduleToolDescription string
	scheduleToolMaxDefer    time.Duration
}

// Sensible defaults used when no With* options override them. MaxTurns
// mirrors cfg.Agent.MaxSteps (50) so a simple "leave it running"
// without a budget still stops in finite time.
func defaultAutoConfig() autoConfig {
	return autoConfig{
		maxTurns:            50,
		doneToolName:        "report_done",
		doneToolDescription: "Signal that the user's goal is complete or that you cannot proceed any further. Call this with state=\"done\" and a one-sentence detail explaining what you accomplished or why you stopped.",
		continuationPrompt:  "continue",
	}
}

// WithMaxTurns caps the number of turns the loop will execute. Zero
// disables the cap (use with caution; pair with another budget). The
// default is 50.
func WithMaxTurns(n int) AutonomousOption {
	return func(c *autoConfig) { c.maxTurns = n }
}

// WithMaxTokens caps the cumulative input + output token totals for
// the run. A zero value for either disables that side of the cap.
func WithMaxTokens(input, output int) AutonomousOption {
	return func(c *autoConfig) {
		c.maxInputTokens = input
		c.maxOutputTokens = output
	}
}

// WithMaxCost caps the cumulative dollar cost of the run. Requires a
// non-zero pricing source — either WithTracker(tracker, pricing) or
// the recorded UsageMetadata being priced via the same Pricing.
func WithMaxCost(usd float64) AutonomousOption {
	return func(c *autoConfig) { c.maxCostUSD = usd }
}

// WithMaxWallclock caps the wall-clock duration of the run, measured
// from RunAutonomous entry. Checked between turns; a single rogue turn
// can still exceed this — pair with WithPerTurnTimeout to bound that.
func WithMaxWallclock(d time.Duration) AutonomousOption {
	return func(c *autoConfig) { c.maxWallclock = d }
}

// WithPerTurnTimeout wraps each turn's context with a timeout so a
// single hung turn cannot stall the whole run. Distinct from
// WithMaxWallclock, which bounds total time.
func WithPerTurnTimeout(d time.Duration) AutonomousOption {
	return func(c *autoConfig) { c.perTurnTimeout = d }
}

// WithDoneToolName overrides the function name of the internal done
// tool. Useful when "report_done" collides with an existing tool the
// consumer has registered. Default: "report_done".
func WithDoneToolName(name string) AutonomousOption {
	return func(c *autoConfig) {
		if name = strings.TrimSpace(name); name != "" {
			c.doneToolName = name
		}
	}
}

// WithDoneToolDescription overrides the description shown to the
// model for the internal done tool. Override when the default prose
// doesn't fit your task — for example to instruct the model to call
// done only after writing a summary.
func WithDoneToolDescription(desc string) AutonomousOption {
	return func(c *autoConfig) {
		if desc = strings.TrimSpace(desc); desc != "" {
			c.doneToolDescription = desc
		}
	}
}

// WithContinuationPrompt overrides the prompt sent on every turn
// after the first. Default: "continue". Real consumers often pass
// something more specific to their loop ("what's your next step?").
func WithContinuationPrompt(s string) AutonomousOption {
	return func(c *autoConfig) {
		if s = strings.TrimSpace(s); s != "" {
			c.continuationPrompt = s
		}
	}
}

// WithTracker hands the driver an existing usage.Tracker plus the
// Pricing to use for per-turn cost accounting. Each turn appends to
// the tracker; RunResult also rolls up totals independently so
// callers can read them without touching the tracker.
//
// When omitted, RunResult still tracks tokens — but cost is zero
// unless a non-zero Pricing is supplied via WithPricing.
func WithTracker(t *usage.Tracker, p usage.Pricing) AutonomousOption {
	return func(c *autoConfig) {
		c.tracker = t
		c.pricing = p
	}
}

// WithPricing sets the Pricing used for cost rollup when a
// usage.Tracker is not supplied. Useful for headless runs that just
// want a final dollar number on RunResult.
func WithPricing(p usage.Pricing) AutonomousOption {
	return func(c *autoConfig) { c.pricing = p }
}

// WithProgress invokes cb for every session.Event observed during
// the run. The turn index is the 1-based count of completed turns at
// the time the event is emitted (always at least 1 inside a turn).
func WithProgress(cb func(turn int, ev *session.Event)) AutonomousOption {
	return func(c *autoConfig) { c.progress = cb }
}

// WithRetryPolicy installs a callback consulted whenever a turn
// returns an error. The callback receives the error and the
// 1-indexed attempt count and returns one of AbortRun, RetryTurn, or
// SkipTurn. Without a policy, the driver aborts on the first error.
func WithRetryPolicy(p RetryPolicy) AutonomousOption {
	return func(c *autoConfig) { c.retryPolicy = p }
}

// WithBeforeTurn installs a callback invoked at the top of each
// iteration of the autonomous loop, after budget checks and before
// the turn's runOneTurn call. The callback receives the upcoming
// turn number (1-based). Returning a non-nil error aborts the run
// with that error.
//
// This is the seam AutonomousHandle uses to implement Pause: the
// callback blocks while paused, returning when Resume fires or the
// run context is cancelled. Library callers can wire arbitrary
// gating logic (rate limits, external approvals, etc.) on top.
func WithBeforeTurn(cb func(ctx context.Context, turnNo int) error) AutonomousOption {
	return func(c *autoConfig) { c.beforeTurn = cb }
}

// WithPermissionsGate hands the driver a reference to the permissions
// gate the consumer wired into their tools. The driver only uses this
// for one purpose: a startup check that rejects ask-mode + no-prompter
// configurations that would deadlock on the first tool call. The gate
// is otherwise enforced by the tools themselves; passing it here does
// not change runtime gating behavior.
//
// Pass this when your build function constructs gated tools and your
// permission mode might be ask. Omit it for ModeYolo / ModeAllow runs
// where deadlock isn't a risk.
func WithPermissionsGate(g *permissions.Gate) AutonomousOption {
	return func(c *autoConfig) { c.permissionsGate = g }
}

// WithScheduler installs a tools.Scheduler that's consulted between
// turns when the prior turn emitted a schedule intent via the
// schedule_next_turn tool. Loops without a scheduler don't get the
// tool registered at all, so the model can't emit intent the driver
// has no way to honor.
//
// Bundled schedulers: tools.SleepScheduler() for long-lived daemons
// (sleeps the goroutine between turns), tools.ExitOnDeferScheduler()
// for orchestrator-managed deployments (exits with
// StopReasonDeferred + RunResult.NextWakeAt populated, ResumeAutonomous
// picks up at the wake-time). See docs/scheduled-monitoring-design.md.
func WithScheduler(s coretools.Scheduler) AutonomousOption {
	return func(c *autoConfig) { c.scheduler = s }
}

// WithMaxDefer is a driver-level ceiling on how far in the future the
// scheduler can wait. Zero means no cap, matching the existing
// WithMaxTurns / WithMaxWallclock convention. Acts as an operator
// safety net: if a turn emits a schedule intent past this ceiling,
// the driver clamps the wake-time and logs a warning, then proceeds
// with the clamped value. The model-facing cap is configured via
// WithScheduleToolMaxDefer.
func WithMaxDefer(d time.Duration) AutonomousOption {
	return func(c *autoConfig) { c.maxDefer = d }
}

// WithScheduleToolName overrides the function name of the internal
// schedule tool. Useful when the default "schedule_next_turn" collides
// with a consumer-registered tool. Only takes effect when WithScheduler
// is also set.
func WithScheduleToolName(name string) AutonomousOption {
	return func(c *autoConfig) {
		if name = strings.TrimSpace(name); name != "" {
			c.scheduleToolName = name
		}
	}
}

// WithScheduleToolDescription overrides the description shown to the
// model for the internal schedule tool. The default includes a cadence
// ladder, good-vs-bad next_prompt examples, and the state-persistence
// reminder; override when domain-specific guidance is needed (e.g.
// "always wake by the top of the hour"). Only takes effect when
// WithScheduler is also set.
func WithScheduleToolDescription(desc string) AutonomousOption {
	return func(c *autoConfig) {
		if desc = strings.TrimSpace(desc); desc != "" {
			c.scheduleToolDescription = desc
		}
	}
}

// WithScheduleToolMaxDefer sets the tool-level cap on how far the
// model may schedule a wake. Calls past the cap return a tool-result
// error to the model so it can adapt. Zero means no cap. Distinct
// from WithMaxDefer, which is the driver's silent safety net. Only
// takes effect when WithScheduler is also set.
func WithScheduleToolMaxDefer(d time.Duration) AutonomousOption {
	return func(c *autoConfig) { c.scheduleToolMaxDefer = d }
}

// RetryPolicy decides what RunAutonomous does when a turn errors.
// The callback receives the error and the 1-indexed attempt count
// (the first failure is attempt=1, second is attempt=2, etc.).
type RetryPolicy func(turnErr error, attempt int) RetryDecision

// RetryDecision tells the driver what to do after a turn fails.
type RetryDecision int

const (
	// AbortRun stops the run immediately and propagates the error.
	AbortRun RetryDecision = iota
	// RetryTurn re-runs the same prompt for another attempt.
	RetryTurn
	// SkipTurn moves on to the continuation prompt as if the failed
	// turn had completed normally without a done signal.
	SkipTurn
)

// RunResult is the structured outcome of RunAutonomous.
type RunResult struct {
	// Reason explains why the loop stopped.
	Reason StopReason
	// FinalText is the accumulated streaming text from the last turn
	// that produced any output.
	FinalText string
	// Turns is the number of turns the driver actually executed
	// (including failed ones that were retried or skipped).
	Turns int
	// InputTokens / OutputTokens are summed from each turn's
	// UsageMetadata. Zero when no usage info was returned.
	InputTokens  int
	OutputTokens int
	// CostUSD is the cumulative dollar cost computed via the
	// configured Pricing. Zero when pricing is zero.
	CostUSD float64
	// Duration is the wall-clock time from RunAutonomous entry to
	// loop exit.
	Duration time.Duration
	// DoneDetail is the detail string the model passed to the done
	// tool when Reason==StopReasonCompleted.
	DoneDetail string
	// NextWakeAt is set when Reason==StopReasonDeferred — the
	// scheduler returned ErrSchedulerDefer and the loop exited
	// cleanly with a wake-time persisted to the eventlog. Whatever
	// orchestrator wraps the process restarts at or after this time
	// and ResumeAutonomous picks up the deferred checkpoint.
	NextWakeAt time.Time
}

// StopReason explains why RunAutonomous returned.
type StopReason string

const (
	// StopReasonCompleted means the model called the done tool.
	StopReasonCompleted StopReason = "completed"
	// StopReasonMaxTurns means WithMaxTurns was hit.
	StopReasonMaxTurns StopReason = "max_turns_exceeded"
	// StopReasonMaxTokens means WithMaxTokens (input or output) was hit.
	StopReasonMaxTokens StopReason = "max_tokens_exceeded" //nolint:gosec // not a credential
	// StopReasonMaxCost means WithMaxCost was hit.
	StopReasonMaxCost StopReason = "max_cost_exceeded"
	// StopReasonWallclockExceeded means WithMaxWallclock was hit.
	StopReasonWallclockExceeded StopReason = "wallclock_exceeded"
	// StopReasonContextCancelled means the supplied context was
	// cancelled or its deadline expired.
	StopReasonContextCancelled StopReason = "context_cancelled"
	// StopReasonRetryAborted means the configured RetryPolicy
	// returned AbortRun for a turn error.
	StopReasonRetryAborted StopReason = "retry_policy_aborted"
	// StopReasonDeferred means the configured Scheduler returned
	// ErrSchedulerDefer in response to a schedule emission. The loop
	// exited cleanly with RunResult.NextWakeAt populated; whatever
	// orchestrator wraps the process restarts at or after the
	// wake-time and ResumeAutonomous picks up.
	StopReasonDeferred StopReason = "deferred"
)
