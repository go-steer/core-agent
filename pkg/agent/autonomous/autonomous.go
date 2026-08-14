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

package autonomous

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// Run drives a multi-turn loop against an Agent built by
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
//
// The build closure SHOULD set agent.WithMode(agent.ModeAutonomous)
// (#459) — the driver cannot inject it (drivers don't mutate
// caller-built agents), so an agent built without it runs with the
// interactive overlay and may ask questions nobody will answer. The
// driver logs a one-line warning when it detects that; consumers
// replacing the whole prompt via agent.WithInstruction can ignore it.
func Run(ctx context.Context, build BuildFunc, goal string, opts ...Option) (RunResult, error) {
	if build == nil {
		return RunResult{}, fmt.Errorf("agent: Run: build is required")
	}
	if strings.TrimSpace(goal) == "" {
		return RunResult{}, fmt.Errorf("agent: Run: goal is required")
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
			return RunResult{}, fmt.Errorf("agent: Run: permissions gate is in ask-mode with no Prompter; would deadlock on first tool call (use ModeYolo / ModeAllow for unattended runs, or wire a Prompter)")
		}
	}

	doneCh := make(chan string, 1)
	doneTools, err := buildDoneTools(&cfg, doneCh)
	if err != nil {
		return RunResult{}, fmt.Errorf("agent: Run: build done tool: %w", err)
	}

	// Optional schedule tool: only wired when a Scheduler is installed
	// via WithScheduler. Loops without a scheduler never see the tool,
	// so the model can't emit schedule intent the driver doesn't know
	// how to honor.
	extras := append([]tool.Tool(nil), doneTools...)
	var scheduleCh <-chan coretools.ScheduleEvent
	if cfg.scheduler != nil {
		schTool, ch, err := coretools.NewScheduleTool(coretools.ScheduleOptions{
			Name:        cfg.scheduleToolName,
			Description: cfg.scheduleToolDescription,
			MaxDefer:    cfg.scheduleToolMaxDefer,
		})
		if err != nil {
			return RunResult{}, fmt.Errorf("agent: Run: build schedule tool: %w", err)
		}
		extras = append(extras, schTool)
		scheduleCh = ch
	}

	a, err := build(extras)
	warnIfInteractiveMode(a, err)
	if err != nil {
		return RunResult{}, fmt.Errorf("agent: Run: build agent: %w", err)
	}
	if a == nil {
		return RunResult{}, fmt.Errorf("agent: Run: build returned nil agent")
	}

	startedAt := time.Now()
	prompt := goal
	result := RunResult{}
	// Whether result.FinalText currently holds the text of a turn that
	// used tools. Once it does, a later tool-less turn can't displace
	// it — see keepFinalText (#731).
	haveSubstantive := false

	// core_agent.autonomous.runs (#338): one point per run, recorded
	// via defer because the loop has many exit sites and no single
	// return hook. Runs that error out before any StopReason is
	// assigned record the "error" fallback — never silent. Recorded
	// against context.Background(): ctx is often already cancelled
	// on the paths this most needs to count.
	mp := cfg.meterProvider
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	if runs, cErr := newRunsCounter(mp); cErr == nil {
		defer func() {
			reason := string(result.Reason)
			if reason == "" {
				reason = StopReasonErrorFallback
			}
			runs.Add(context.Background(), 1, metric.WithAttributes(attribute.String(AttrStopReason, reason)))
		}()
	} else {
		// Practically unreachable (fixed valid instrument name; the
		// noop provider never errors) — logged rather than fatal so
		// a metrics bug can't kill an autonomous run.
		log.Printf("autonomous: runs counter unavailable, runs will be uncounted: %v", cErr)
	}

	// Convenience: emit a final checkpoint with the configured
	// stop reason regardless of which exit path the loop takes.
	// Skipped when the agent has no event log; checkpoints are only
	// useful for durable sessions.
	emitFinalCheckpoint := func(reason StopReason) {
		// Detached write: the final checkpoint is the crash-resume
		// anchor and is emitted even when the loop exits because ctx
		// was cancelled — passing the dead ctx here silently lost the
		// checkpoint on exactly the shutdown path where it matters
		// most (#365).
		emitFinalCheckpointDetached(ctx, a, checkpointPayload{
			Turn:                 result.Turns,
			InputTokens:          result.InputTokens,
			OutputTokens:         result.OutputTokens,
			CostUSD:              result.CostUSD,
			Goal:                 goal,
			ContinuationPrompt:   cfg.continuationPrompt,
			StopReason:           string(reason),
			DoneDetail:           result.DoneDetail,
			FinalText:            result.FinalText,
			FinalTextSubstantive: haveSubstantive,
		})
	}

	for {
		// Pre-turn budget checks.
		if reason := budgetStop(&cfg, &result, time.Since(startedAt)); reason != "" {
			result.Reason = reason
			break
		}
		if err := ctx.Err(); err != nil {
			result.Reason = StopReasonContextCancelled
			result.Duration = time.Since(startedAt)
			emitFinalCheckpoint(StopReasonContextCancelled)
			return result, err
		}

		// BeforeTurn hook (used by Handle to implement
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

		turnRes, turnErr := runOneTurn(turnCtx, a, prompt, doneCh, scheduleCh, &cfg, result.Turns+1, result.CostUSD)
		if cancel != nil {
			cancel()
		}

		// Roll up usage from this turn into the overall result.
		result.InputTokens += turnRes.inputTokens
		result.OutputTokens += turnRes.outputTokens
		result.CostUSD += turnRes.costUSD
		result.Turns++
		if keepFinalText(turnRes.text, turnRes.usedTools, haveSubstantive) {
			result.FinalText = turnRes.text
			haveSubstantive = haveSubstantive || turnRes.usedTools
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
				_ = emitCheckpoint(ctx, a, perTurnCheckpoint(result, goal, cfg.continuationPrompt, haveSubstantive))
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

		// A turn the max-cost bound cut short (#729). Same stop reason
		// as the between-turn check — to every consumer this is one
		// budget outcome, just detected earlier — and the same break,
		// so the run's FinalText (already rolled up above) carries
		// whatever the model had established to the caller. Ranked
		// below done: a turn that both finished the work and crossed
		// the bound finished the work.
		if turnRes.costCapped {
			result.Reason = StopReasonMaxCost
			break
		}

		// Natural end of a bounded delegation (#730): the model stopped
		// asking for tools, so it has finished the task it was handed.
		// Its last message is the deliverable. Ranked below the two
		// checks above because both describe a stop the model didn't
		// choose, and below done because a run that signalled done is
		// done for that reason.
		//
		// Off by default: for a standing worker a text-only turn is a
		// status report, not an ending, and the loop keeps going.
		//
		// Ranked *below* an explicit schedule_next_turn (checked just
		// after, and skipped here) even though report_done outranks
		// one: done is a choice the model made, natural end is
		// inferred from the absence of a choice, and an inference
		// must not override the model asking in so many words to be
		// woken again.
		if cfg.stopOnNaturalEnd && !turnRes.requestedTools && !turnRes.scheduleSignaled {
			result.Reason = StopReasonCompleted
			result.DoneDetail = turnRes.text
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
					log.Printf("agent: Run: clamping scheduler wake-time from %s to driver MaxDefer ceiling %s (max_defer=%s)",
						ev.WakeAt.Format(time.RFC3339), ceiling.Format(time.RFC3339), cfg.maxDefer)
					ev.WakeAt = ceiling
				}
			}

			// Per-turn checkpoint with next_wake_at populated so a
			// crash mid-defer can resume to the right wake-time.
			_ = emitCheckpoint(ctx, a, scheduleCheckpoint(result, goal, cfg.continuationPrompt, ev, haveSubstantive))

			// Plumb the agent's wake channel through to the scheduler
			// so SleepScheduler interrupts its sleep on an external
			// wake (alert arrival, operator Inject, future attach-
			// mode /wake).
			schedCtx := coretools.ContextWithWake(ctx, a.WakeRequested())
			serr := cfg.scheduler.BeforeNextTurn(schedCtx, ev)
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
				// for Resume to pick up. break out of the
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
				return result, fmt.Errorf("agent: Run: scheduler: %w", serr)
			}
		}

		// Per-turn checkpoint after a clean (non-done, non-error)
		// turn. Per-turn emission is the cursor Resume
		// continues from; a no-checkpoint run can still resume from
		// turn 0 if its session has events but no checkpoints.
		_ = emitCheckpoint(ctx, a, perTurnCheckpoint(result, goal, cfg.continuationPrompt, haveSubstantive))

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
func perTurnCheckpoint(result RunResult, goal, continuation string, substantive bool) checkpointPayload {
	return checkpointPayload{
		Turn:                 result.Turns,
		InputTokens:          result.InputTokens,
		OutputTokens:         result.OutputTokens,
		CostUSD:              result.CostUSD,
		Goal:                 goal,
		ContinuationPrompt:   continuation,
		FinalText:            result.FinalText,
		FinalTextSubstantive: substantive,
	}
}

// scheduleCheckpoint extends perTurnCheckpoint with the pending
// wake-time. Emitted before the scheduler is consulted so a crash
// mid-defer can be resumed to the correct wake-time. The continuation
// prompt is intentionally the scheduler-supplied NextPrompt when
// present so resume picks the same prompt the scheduler-honored run
// would have used.
func scheduleCheckpoint(result RunResult, goal, fallbackContinuation string, ev coretools.ScheduleEvent, substantive bool) checkpointPayload {
	continuation := ev.NextPrompt
	if continuation == "" {
		continuation = fallbackContinuation
	}
	return checkpointPayload{
		Turn:                 result.Turns,
		InputTokens:          result.InputTokens,
		OutputTokens:         result.OutputTokens,
		CostUSD:              result.CostUSD,
		Goal:                 goal,
		ContinuationPrompt:   continuation,
		FinalText:            result.FinalText,
		NextWakeAt:           ev.WakeAt,
		FinalTextSubstantive: substantive,
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
	// requestedTools records whether the model's LAST response in this
	// turn asked for a tool. False means the tool loop ran itself out
	// — the model had nothing left to call — which for a bounded
	// delegation is the completion signal (#730).
	//
	// Deliberately the last response, not "any tool call this turn":
	// a turn that used a tool at any point is the normal case, so the
	// cumulative reading would never fire.
	requestedTools bool
	// usedTools is the cumulative reading requestedTools deliberately
	// isn't: it records whether the model asked for a tool at ANY
	// point in the turn. Wrong as a termination test, exactly right as
	// a substantiveness test (#731) — a turn that ran tools looked at
	// the world, and its text is worth keeping over a later turn that
	// only narrated having nothing left to do.
	usedTools bool
	// costCapped is set when WithMaxCost was reached partway through
	// this turn and the driver stopped draining events rather than
	// letting the turn spend past the bound. See the enforcement site
	// in runOneTurn and #729.
	costCapped bool
}

// runOneTurn drives one turn of the agent loop. priorCostUSD is the
// spend the run has already accumulated across previous turns; it is
// the baseline the in-turn WithMaxCost check adds this turn's spend
// to, so the bound covers the run as a whole rather than each turn
// independently.
func runOneTurn(ctx context.Context, a *agent.Agent, prompt string, doneCh chan string, scheduleCh <-chan coretools.ScheduleEvent, cfg *autoConfig, turnNo int, priorCostUSD float64) (turnResult, error) {
	var (
		out       turnResult
		buf       strings.Builder
		partials  strings.Builder
		sawFinals bool
		// See the in-turn cost check at the bottom of the event loop.
		doneToolNames   = cfg.doneToolNames()
		deferredForDone bool
		// Tools the driver injected itself. They don't count towards
		// usedTools: calling one is a control-plane gesture, not a
		// look at the world, and a scheduled worker calls
		// schedule_next_turn on every idle turn — counting it would
		// mark every turn substantive and undo #731 for exactly the
		// population that keeps the re-drive loop.
		driverTools = append(append([]string(nil), doneToolNames...), coretools.ScheduleToolName(cfg.scheduleToolName))
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

	// Per-model-turn usage bookkeeping via the shared TurnTap
	// discipline: overwrite last-seen UsageMetadata per event, commit
	// exactly once on TurnComplete. Gemini's UsageMetadata is
	// cumulative across streaming chunks within a model turn, so the
	// old Append-on-every-event path both double-counted tokens and
	// inflated the tracker's turn count (one Append per chunk). See
	// pkg/usage.TurnTap and #156/#353; subtask.go uses the same
	// discipline. Note a.Run can drive several model turns in one
	// runOneTurn call (tool loops), so we accumulate across Commits.
	var tap usage.TurnTap
	for ev, err := range a.Run(ctx, prompt) {
		if err != nil {
			out.text = collectedText(&buf, &partials, sawFinals)
			return out, err
		}
		if ev == nil {
			continue
		}
		if cfg.progress != nil {
			cfg.progress(turnNo, ev)
		}
		// We are already over the cost bound and only still here to let
		// the done tool run (see the check at the bottom of the loop).
		// The moment it has, stop — otherwise the runner would issue
		// one more model call to narrate a run that is already over.
		if deferredForDone {
			select {
			case detail := <-doneCh:
				out.doneSignaled = true
				out.doneDetail = detail
				out.text = collectedText(&buf, &partials, sawFinals)
				return out, nil
			default:
			}
		}
		tap.Observe(ev)
		if turnUsage, ok := tap.Commit(ev); ok {
			out.inputTokens += turnUsage.InputTokens
			out.outputTokens += turnUsage.OutputTokens
			if cfg.tracker != nil {
				// Tag by the LLM model name (matches subtask.go +
				// internal_llm_usage.go). Falls back to the agent
				// name only when the model isn't wired (defensive —
				// New always sets modelName from model.Name()).
				modelName := a.ModelName()
				if modelName == "" && a.Inner() != nil {
					modelName = a.Inner().Name()
				}
				rec := cfg.tracker.AppendUsage(modelName, turnUsage, cfg.pricing)
				out.costUSD += rec.CostUSD
			} else if !cfg.pricing.IsZero() {
				uncached := turnUsage.InputTokens - turnUsage.CachedInputTokens
				if uncached < 0 {
					uncached = 0
				}
				out.costUSD += cfg.pricing.CostUSDWithCache(uncached, turnUsage.CachedInputTokens, turnUsage.OutputTokens)
			}
		}
		// Text collection follows the same discipline as
		// agent.collectFinalText: prefer the consolidated non-partial
		// events, because those are the ones ADK emits in BOTH streaming
		// and non-streaming mode. Collecting only partials (as this did
		// before) left FinalText permanently empty for a non-streaming
		// agent, which in turn left a synchronous subagent spawn with no
		// deliverable to return to its parent (#641). Partials are still
		// accumulated as a fallback so a provider that emits no
		// consolidated event can't regress to empty either.
		if ev.Content != nil {
			for _, p := range ev.Content.Parts {
				if p == nil || p.Text == "" {
					continue
				}
				if ev.Partial {
					partials.WriteString(p.Text)
					continue
				}
				sawFinals = true
				if buf.Len() > 0 {
					buf.WriteByte('\n')
				}
				buf.WriteString(p.Text)
			}
		}

		// Track whether the model is still asking for tools. Only
		// consolidated model-role events count: partials are chunks of
		// a response still being assembled, and function-RESPONSE
		// events come back with the user role. Overwriting per model
		// response (rather than OR-ing across the turn) is what makes
		// this mean "the last response asked for a tool" — see the
		// requestedTools doc comment.
		if !ev.Partial && ev.Content != nil && ev.Content.Role == genai.RoleModel {
			out.requestedTools = hasFunctionCall(ev)
			// The same events, OR-ed instead of overwritten: "did the
			// model do any work this turn" (#731). Scanned over the
			// same guard so the two readings can never disagree about
			// which events counted, but skipping the driver's own
			// tools — see driverTools.
			out.usedTools = out.usedTools || hasFunctionCallExcept(ev, driverTools)
		}

		// In-turn WithMaxCost enforcement (#729). The between-turn
		// budget checks at the top of the loop only run at a turn
		// boundary, and a single turn is an unbounded tool loop — the
		// #144 shape spends indefinitely without ever reaching one. So
		// check the bound where the spend actually lands: right after
		// each committed model call. Placed at the bottom of the body
		// so this event's text is collected first; the tripping call's
		// output is part of what the parent receives.
		//
		// Breaking the range is the whole mechanism: yield returns
		// false, agent.Run's tapped iterator returns, and its cleanup
		// cancels the per-turn context. Deliberately NOT Interrupt() —
		// that records an operator-interrupt audit row, and this is a
		// budget stop, not an operator one.
		if cfg.maxCostUSD > 0 && priorCostUSD+out.costUSD >= cfg.maxCostUSD {
			// One exception, taken at most once: if the call that
			// crossed the bound is the model handing back its result,
			// breaking here would drop the answer on the floor and
			// report a budget stop instead — the exact failure #728
			// exists to prevent, arriving through a different door.
			// The tool hasn't run yet (it executes after this event),
			// so let the loop take one more step and the done signal
			// wins below. Bounded by deferredForDone: a model that
			// keeps emitting done calls can't extend the turn twice.
			if !deferredForDone && callsAnyTool(ev, doneToolNames) {
				deferredForDone = true
				continue
			}
			out.costCapped = true
			log.Printf("autonomous: turn %d stopped mid-turn: cost $%.4f reached the $%.4f max-cost bound", turnNo, priorCostUSD+out.costUSD, cfg.maxCostUSD)
			break
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

	out.text = collectedText(&buf, &partials, sawFinals)
	return out, nil
}

// keepFinalText reports whether this turn's text should replace the
// run's FinalText (#731).
//
// FinalText is not cosmetic: it is the fallback return value on every
// path where the model never got to call a done tool — a budget cap, a
// watchdog halt, a provider failure — which are exactly the paths where
// the parent most needs to know what the subagent found. Overwriting it
// on every turn made it "the last thing the subagent said", which under
// a re-drive loop is reliably the *worst* thing it said: substantive
// turns come early, trailing turns narrate having nothing left to do.
// In the 2026-08-13 GKE UAT the root cause and patch landed on turn 3
// and the returned FinalText was "standing by in a healthy, inactive
// state".
//
// So a turn that used a tool wins, and a turn that didn't cannot
// displace one that did. The haveSubstantive fallback keeps a tool-less
// run — a pure-reasoning agent, or one whose tools are all disabled —
// on last-wins, where iterative refinement really does make the newest
// text the best one. Without it those runs would freeze on turn 1
// forever.
func keepFinalText(turnText string, turnUsedTools, haveSubstantive bool) bool {
	if turnText == "" {
		return false
	}
	return turnUsedTools || !haveSubstantive
}

// hasFunctionCallExcept reports whether ev calls any tool other than
// the named ones. Used to tell work (a tool that looks at the world)
// from bookkeeping (the driver's own done/schedule tools) when deciding
// whether a turn was substantive (#731).
func hasFunctionCallExcept(ev *session.Event, ignore []string) bool {
	if ev == nil || ev.Content == nil {
		return false
	}
	for _, p := range ev.Content.Parts {
		if p == nil || p.FunctionCall == nil {
			continue
		}
		skip := false
		for _, n := range ignore {
			if n != "" && strings.EqualFold(p.FunctionCall.Name, n) {
				skip = true
				break
			}
		}
		if !skip {
			return true
		}
	}
	return false
}

// hasFunctionCall reports whether ev carries any FunctionCall part.
func hasFunctionCall(ev *session.Event) bool {
	if ev == nil || ev.Content == nil {
		return false
	}
	for _, p := range ev.Content.Parts {
		if p != nil && p.FunctionCall != nil {
			return true
		}
	}
	return false
}

// callsAnyTool reports whether ev carries a FunctionCall to one of
// names. Case-insensitive: the names are ours, but the call comes
// from a model.
func callsAnyTool(ev *session.Event, names []string) bool {
	if ev == nil || ev.Content == nil {
		return false
	}
	for _, p := range ev.Content.Parts {
		if p == nil || p.FunctionCall == nil {
			continue
		}
		for _, n := range names {
			if strings.EqualFold(p.FunctionCall.Name, n) {
				return true
			}
		}
	}
	return false
}

// budgetStop reports the first configured budget the run has already
// exhausted, or "" when it may take another turn. Evaluated at a turn
// boundary; the in-turn half of the cost bound lives in runOneTurn.
//
// Shared by Run and Resume so the two drivers can't drift on which
// caps exist or in what order they bind — they were separate copies
// of the same five checks, which is one copy too many for something
// whose whole job is to be the thing that stops a runaway.
func budgetStop(cfg *autoConfig, result *RunResult, elapsed time.Duration) StopReason {
	switch {
	case cfg.maxWallclock > 0 && elapsed >= cfg.maxWallclock:
		return StopReasonWallclockExceeded
	case cfg.maxTurns > 0 && result.Turns >= cfg.maxTurns:
		return StopReasonMaxTurns
	case cfg.maxInputTokens > 0 && result.InputTokens >= cfg.maxInputTokens:
		return StopReasonMaxTokens
	case cfg.maxOutputTokens > 0 && result.OutputTokens >= cfg.maxOutputTokens:
		return StopReasonMaxTokens
	case cfg.maxCostUSD > 0 && result.CostUSD >= cfg.maxCostUSD:
		return StopReasonMaxCost
	}
	return ""
}

// collectedText picks the turn's text: the consolidated non-partial
// events when the run produced any, otherwise the accumulated streaming
// partials. See the collection site for why both are tracked.
func collectedText(finals, partials *strings.Builder, sawFinals bool) string {
	if sawFinals {
		return finals.String()
	}
	return partials.String()
}

// Option mutates Run configuration. Use the With*
// helpers below.
// warnIfInteractiveMode logs the #459 advisory when an autonomous
// driver receives an interactive-mode agent: the interactive overlay
// tells the model a user is present, which is exactly wrong here.
// Warning, not error — a consumer that replaced the whole prompt via
// agent.WithInstruction knows what they're doing, and Mode() still
// reports the (unused) WithMode value in that case.
func warnIfInteractiveMode(a *agent.Agent, buildErr error) {
	if buildErr != nil || a == nil {
		return
	}
	if a.Mode() == agent.ModeInteractive {
		log.Printf("autonomous: agent built with the interactive overlay; did the build closure mean agent.WithMode(agent.ModeAutonomous)?")
	}
}

type Option func(*autoConfig)

type autoConfig struct {
	maxTurns                int
	maxInputTokens          int
	maxOutputTokens         int
	maxCostUSD              float64
	maxWallclock            time.Duration
	perTurnTimeout          time.Duration
	doneToolName            string
	doneToolNameExplicit    bool
	doneToolDescription     string
	returnTool              *ReturnToolConfig
	stopOnNaturalEnd        bool
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
	meterProvider           metric.MeterProvider
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
func WithMaxTurns(n int) Option {
	return func(c *autoConfig) { c.maxTurns = n }
}

// WithMeterProvider overrides the OTel MeterProvider backing the
// core_agent.autonomous.runs counter. Defaults to the global provider
// resolved when Run starts (noop when metrics are disabled).
// Primarily for tests injecting a ManualReader.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *autoConfig) { c.meterProvider = mp }
}

// WithMaxTokens caps the cumulative input + output token totals for
// the run. A zero value for either disables that side of the cap.
func WithMaxTokens(input, output int) Option {
	return func(c *autoConfig) {
		c.maxInputTokens = input
		c.maxOutputTokens = output
	}
}

// WithMaxCost caps the cumulative dollar cost of the run. Requires a
// non-zero pricing source — either WithTracker(tracker, pricing) or
// the recorded UsageMetadata being priced via the same Pricing.
func WithMaxCost(usd float64) Option {
	return func(c *autoConfig) { c.maxCostUSD = usd }
}

// WithStopOnNaturalEnd makes the loop stop the first time a turn ends
// without the model asking for another tool, reporting
// StopReasonCompleted with the turn's text as the result (#730).
//
// This is the termination rule for a BOUNDED delegation — a subagent
// handed one task, which is done when it stops working. The default
// (standing worker) is the opposite: a turn that produces only text is
// a status report, and the loop feeds it the continuation prompt and
// keeps going until a budget or an explicit done signal fires.
//
// On its own it suppresses the done/return tool entirely: with one
// termination path there is nothing for the model to choose between,
// and no way to leave the loop running by forgetting to call
// something.
//
// Pair it with WithReturnTool and the tool IS registered (#745): the
// two are not alternatives but a preference order. A model that calls
// the tool hands back a curated result and ends; a model that just
// stops still ends, with its last message as the result. Nothing can
// hang, because the natural end never stops being a termination path.
// Bounded-without-a-return-tool remains the right shape only where the
// model genuinely has no gesture available.
func WithStopOnNaturalEnd() Option {
	return func(c *autoConfig) { c.stopOnNaturalEnd = true }
}

// WithMaxWallclock caps the wall-clock duration of the run, measured
// from Run entry. Checked between turns; a single rogue turn
// can still exceed this — pair with WithPerTurnTimeout to bound that.
func WithMaxWallclock(d time.Duration) Option {
	return func(c *autoConfig) { c.maxWallclock = d }
}

// WithPerTurnTimeout wraps each turn's context with a timeout so a
// single hung turn cannot stall the whole run. Distinct from
// WithMaxWallclock, which bounds total time.
func WithPerTurnTimeout(d time.Duration) Option {
	return func(c *autoConfig) { c.perTurnTimeout = d }
}

// WithDoneToolName overrides the function name of the internal done
// tool. Useful when "report_done" collides with an existing tool the
// consumer has registered. Default: "report_done".
func WithDoneToolName(name string) Option {
	return func(c *autoConfig) {
		if name = strings.TrimSpace(name); name != "" {
			c.doneToolName = name
			c.doneToolNameExplicit = true
		}
	}
}

// WithDoneToolDescription overrides the description shown to the
// model for the internal done tool. Override when the default prose
// doesn't fit your task — for example to instruct the model to call
// done only after writing a summary.
func WithDoneToolDescription(desc string) Option {
	return func(c *autoConfig) {
		if desc = strings.TrimSpace(desc); desc != "" {
			c.doneToolDescription = desc
		}
	}
}

// WithContinuationPrompt overrides the prompt sent on every turn
// after the first. Default: "continue". Real consumers often pass
// something more specific to their loop ("what's your next step?").
func WithContinuationPrompt(s string) Option {
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
func WithTracker(t *usage.Tracker, p usage.Pricing) Option {
	return func(c *autoConfig) {
		c.tracker = t
		c.pricing = p
	}
}

// WithPricing sets the Pricing used for cost rollup when a
// usage.Tracker is not supplied. Useful for headless runs that just
// want a final dollar number on RunResult.
func WithPricing(p usage.Pricing) Option {
	return func(c *autoConfig) { c.pricing = p }
}

// WithProgress invokes cb for every session.Event observed during
// the run. The turn index is the 1-based count of completed turns at
// the time the event is emitted (always at least 1 inside a turn).
func WithProgress(cb func(turn int, ev *session.Event)) Option {
	return func(c *autoConfig) { c.progress = cb }
}

// WithRetryPolicy installs a callback consulted whenever a turn
// returns an error. The callback receives the error and the
// 1-indexed attempt count and returns one of AbortRun, RetryTurn, or
// SkipTurn. Without a policy, the driver aborts on the first error.
func WithRetryPolicy(p RetryPolicy) Option {
	return func(c *autoConfig) { c.retryPolicy = p }
}

// WithBeforeTurn installs a callback invoked at the top of each
// iteration of the autonomous loop, after budget checks and before
// the turn's runOneTurn call. The callback receives the upcoming
// turn number (1-based). Returning a non-nil error aborts the run
// with that error.
//
// This is the seam Handle uses to implement Pause: the
// callback blocks while paused, returning when Resume fires or the
// run context is cancelled. Library callers can wire arbitrary
// gating logic (rate limits, external approvals, etc.) on top.
func WithBeforeTurn(cb func(ctx context.Context, turnNo int) error) Option {
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
func WithPermissionsGate(g *permissions.Gate) Option {
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
// StopReasonDeferred + RunResult.NextWakeAt populated, Resume
// picks up at the wake-time). See docs/scheduled-monitoring-design.md.
func WithScheduler(s coretools.Scheduler) Option {
	return func(c *autoConfig) { c.scheduler = s }
}

// WithMaxDefer is a driver-level ceiling on how far in the future the
// scheduler can wait. Zero means no cap, matching the existing
// WithMaxTurns / WithMaxWallclock convention. Acts as an operator
// safety net: if a turn emits a schedule intent past this ceiling,
// the driver clamps the wake-time and logs a warning, then proceeds
// with the clamped value. The model-facing cap is configured via
// WithScheduleToolMaxDefer.
func WithMaxDefer(d time.Duration) Option {
	return func(c *autoConfig) { c.maxDefer = d }
}

// WithScheduleToolName overrides the function name of the internal
// schedule tool. Useful when the default "schedule_next_turn" collides
// with a consumer-registered tool. Only takes effect when WithScheduler
// is also set.
func WithScheduleToolName(name string) Option {
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
func WithScheduleToolDescription(desc string) Option {
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
func WithScheduleToolMaxDefer(d time.Duration) Option {
	return func(c *autoConfig) { c.scheduleToolMaxDefer = d }
}

// RetryPolicy decides what Run does when a turn errors.
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

// RunResult is the structured outcome of Run.
type RunResult struct {
	// Reason explains why the loop stopped.
	Reason StopReason
	// FinalText is the accumulated streaming text from the last
	// *substantive* turn — the last turn that both produced output and
	// used a tool. A turn that only produced text cannot displace it
	// (#731): FinalText is the fallback return value wherever
	// DoneDetail is absent, and under a re-drive loop the trailing
	// turns are the model narrating that it has nothing left to do, so
	// last-wins reliably returned the worst thing the run said.
	//
	// A run that never used a tool at all keeps last-wins, since for a
	// pure-reasoning loop the newest text really is the best one.
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
	// Duration is the wall-clock time from Run entry to
	// loop exit.
	Duration time.Duration
	// DoneDetail is the detail string the model passed to the done
	// tool when Reason==StopReasonCompleted.
	DoneDetail string
	// NextWakeAt is set when Reason==StopReasonDeferred — the
	// scheduler returned ErrSchedulerDefer and the loop exited
	// cleanly with a wake-time persisted to the eventlog. Whatever
	// orchestrator wraps the process restarts at or after this time
	// and Resume picks up the deferred checkpoint.
	NextWakeAt time.Time
}

// StopReason explains why Run returned.
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
	// wake-time and Resume picks up.
	StopReasonDeferred StopReason = "deferred"
)
