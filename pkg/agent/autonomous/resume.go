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
	"strings"
	"time"

	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	coretools "github.com/go-steer/core-agent/v3/pkg/tools"
)

// ResumeBuildFunc is the agent constructor accepted by
// Resume. It mirrors Run's BuildFunc signature
// but adds the sessionID the new agent must adopt — implementations
// pass it to agent.WithSession so the constructed agent reuses the
// session being resumed.
type ResumeBuildFunc func(extras []tool.Tool, sessionID string) (*agent.Agent, error)

// SessionRef identifies the session Resume resumes from.
// Handle supplies both the eventlog.Stream (used to find the latest
// checkpoint) and the session.Service (used by the constructed
// agent for live event reads + writes).
type SessionRef struct {
	Handle    *eventlog.Handle
	AppName   string
	UserID    string
	SessionID string
}

// Resume reads the most recent checkpoint event from the
// session's event log, reconstructs RunResult totals, and continues
// the run from the next turn. The build function receives the
// resumed sessionID so the constructed agent rejoins the same
// session via agent.WithSession.
//
// Behavior:
//   - Acquires an exclusive SessionLock on (App, User, Session). A
//     concurrent Resume on the same session returns
//     ErrSessionLocked from eventlog.
//   - If the session has no checkpoint events at all, the run starts
//     from turn 0 with whatever event history the session already
//     holds — "make this existing session autonomous from here" is
//     a valid use case.
//   - If the latest checkpoint has stop_reason set (terminal state),
//     Resume returns that state immediately without
//     constructing the agent or running any turns.
//   - Otherwise, the loop continues with prompt =
//     checkpoint.ContinuationPrompt; budgets carry forward.
func Resume(ctx context.Context, build ResumeBuildFunc, ref SessionRef, opts ...Option) (RunResult, error) {
	if build == nil {
		return RunResult{}, errors.New("agent: Resume: build is required")
	}
	if ref.Handle == nil {
		return RunResult{}, errors.New("agent: Resume: SessionRef.Handle is required")
	}
	if strings.TrimSpace(ref.SessionID) == "" {
		return RunResult{}, errors.New("agent: Resume: SessionRef.SessionID is required")
	}
	cfg := defaultAutoConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	// resumeStart anchors Duration for the cancelled-while-waiting
	// exit below. The loop's startedAt (declared after the deferred-
	// wake wait) intentionally excludes the wait so the wallclock
	// budget only counts active run time — it can't serve here.
	resumeStart := time.Now()

	// Lock first — fail fast if another process is resuming the
	// same session.
	lock, err := ref.Handle.AcquireLock(ctx, ref.AppName, ref.UserID, ref.SessionID)
	if err != nil {
		return RunResult{}, fmt.Errorf("agent: Resume: %w", err)
	}
	defer func() { _ = lock.Release() }()

	// Cancel in-flight turns and scheduled waits immediately if the
	// session lease is stolen or expires during a DB stall (#1361).
	leaseCtx, cancelLease := bindLeaseContext(ctx, lock)
	defer cancelLease()

	// Discover the latest checkpoint by suffix-matching the author
	// so handoffs across binaries (core-agent ↔ scion-agent ↔
	// ax-agent) work.
	latest, found, err := loadLatestCheckpoint(leaseCtx, ref)
	if err != nil {
		if leaseLost(lock) {
			return RunResult{Reason: StopReasonContextCancelled, Duration: time.Since(resumeStart)},
				fmt.Errorf("agent: Resume: %w", eventlog.ErrLeaseLost)
		}
		return RunResult{}, fmt.Errorf("agent: Resume: %w", err)
	}

	// Terminal-state short-circuit: only StopReasonCompleted is
	// truly terminal. Other stop reasons (max_turns, max_tokens,
	// max_cost, wallclock, retry_aborted, context_cancelled) are
	// interruptions — the consumer is allowed to resume them with
	// a bigger budget or a fresh context. Treating those as
	// terminal would defeat the point of crash-resume.
	if found && StopReason(latest.StopReason) == StopReasonCompleted {
		return RunResult{
			Reason:       StopReasonCompleted,
			FinalText:    latest.FinalText,
			Turns:        latest.Turn,
			InputTokens:  latest.InputTokens,
			OutputTokens: latest.OutputTokens,
			CostUSD:      latest.CostUSD,
			DoneDetail:   latest.DoneDetail,
		}, nil
	}

	// Build the resume prompt. If we have a checkpoint, use its
	// continuation prompt; otherwise fall back to the configured
	// default. The goal is not re-sent — it lives in the session's
	// existing event history.
	prompt := cfg.continuationPrompt
	if found && latest.ContinuationPrompt != "" {
		prompt = latest.ContinuationPrompt
	}

	doneCh := make(chan string, 1)
	a, scheduleCh, err := buildResumedAgent(build, ref.SessionID, &cfg, doneCh)
	if err != nil {
		return RunResult{}, err
	}

	if res, exited, err := awaitResumeWake(ctx, lock, latest, found, resumeStart); exited {
		return res, err
	}

	startedAt := time.Now()
	result := RunResult{
		Turns:        latest.Turn,
		InputTokens:  latest.InputTokens,
		OutputTokens: latest.OutputTokens,
		CostUSD:      latest.CostUSD,
		FinalText:    latest.FinalText,
	}
	// Carried across the restart rather than re-derived: a resumed run
	// that forgot the pre-crash FinalText came from a tool-using turn
	// would let the first idle turn after the restart overwrite it,
	// which is #731 with an extra step.
	haveSubstantive := latest.FinalTextSubstantive

	// Use the resume goal-of-record from the checkpoint when we
	// have one; otherwise fall back to the empty string so audit
	// readers can still tell "this was a resume without prior
	// checkpoints."
	goal := latest.Goal

	cp := &resumeCheckpoints{
		ctx: ctx, leaseCtx: leaseCtx, a: a, lock: lock, cfg: &cfg,
		result: &result, goal: goal, haveSubstantive: &haveSubstantive, startedAt: startedAt,
	}
	emitTurnCheckpoint, emitFinalCheckpoint, loseLease := cp.turn, cp.final, cp.loseLease

	for {
		if leaseLost(lock) {
			return loseLease()
		}
		// Pre-turn budget checks — applied to the cumulative
		// (resumed + newly-accumulated) totals.
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

		turnCtx := leaseCtx
		var cancel context.CancelFunc
		if cfg.perTurnTimeout > 0 {
			turnCtx, cancel = context.WithTimeout(leaseCtx, cfg.perTurnTimeout)
		}
		turnRes, turnErr := runOneTurn(turnCtx, a, prompt, doneCh, scheduleCh, &cfg, result.Turns+1, result.CostUSD)
		if cancel != nil {
			cancel()
		}

		result.InputTokens += turnRes.inputTokens
		result.OutputTokens += turnRes.outputTokens
		result.CostUSD += turnRes.costUSD
		result.Turns++
		if keepFinalText(turnRes.text, turnRes.usedTools, haveSubstantive) {
			result.FinalText = turnRes.text
			haveSubstantive = haveSubstantive || turnRes.usedTools
		}

		if leaseLost(lock) {
			bankReturn(&result, turnRes)
			return loseLease()
		}

		if turnErr != nil {
			bankReturn(&result, turnRes)
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
				continue
			case SkipTurn:
				prompt = cfg.continuationPrompt
				emitTurnCheckpoint(perTurnCheckpoint(result, goal, cfg.continuationPrompt, haveSubstantive))
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
			// Same as Run, through the same helper: a resumed run
			// offers the identical termination gesture, so it has to
			// report the identical fact about which one was used
			// (#710), and record it the same way on the error path
			// too (#1002).
			bankReturn(&result, turnRes)
			break
		}

		// Max-cost bound hit partway through the turn (#729). Same
		// handling as Run; see the comment there. result.CostUSD was
		// seeded from the checkpoint, so a resumed run's bound still
		// covers the delegation as a whole, not just this process.
		if turnRes.costCapped {
			result.Reason = StopReasonMaxCost
			break
		}

		// Natural end of a bounded delegation (#730); see Run.
		if cfg.stopOnNaturalEnd && !turnRes.requestedTools && !turnRes.scheduleSignaled {
			result.Reason = StopReasonCompleted
			bankNaturalEnd(&result, turnRes)
			break
		}

		// Schedule emission — same wiring as Run so a
		// resumed daemon continues honoring schedule_next_turn calls
		// across restarts.
		if turnRes.scheduleSignaled && cfg.scheduler != nil {
			ev := turnRes.scheduleEvent
			if cfg.maxDefer > 0 {
				ceiling := time.Now().Add(cfg.maxDefer)
				if ev.WakeAt.After(ceiling) {
					ev.WakeAt = ceiling
				}
			}
			emitTurnCheckpoint(scheduleCheckpoint(result, goal, cfg.continuationPrompt, ev, haveSubstantive))
			serr := awaitScheduledWake(leaseCtx, a, &cfg, ev)
			if leaseLost(lock) {
				return loseLease()
			}
			switch {
			case serr == nil:
				if ev.NextPrompt != "" {
					prompt = ev.NextPrompt
				} else {
					prompt = cfg.continuationPrompt
				}
				continue
			case errors.Is(serr, coretools.ErrSchedulerDefer):
				result.Reason = StopReasonDeferred
				result.NextWakeAt = ev.WakeAt
				goto deferredExit
			case errors.Is(serr, context.Canceled) && ctx.Err() != nil:
				result.Reason = StopReasonContextCancelled
				result.Duration = time.Since(startedAt)
				emitFinalCheckpoint(StopReasonContextCancelled)
				return result, serr
			default:
				result.Reason = StopReasonRetryAborted
				result.Duration = time.Since(startedAt)
				emitFinalCheckpoint(StopReasonRetryAborted)
				return result, fmt.Errorf("agent: Resume: scheduler: %w", serr)
			}
		}

		emitTurnCheckpoint(perTurnCheckpoint(result, goal, cfg.continuationPrompt, haveSubstantive))
		prompt = cfg.continuationPrompt
	}

deferredExit:
	if leaseLost(lock) {
		return loseLease()
	}
	result.Duration = time.Since(startedAt)
	emitFinalCheckpoint(result.Reason)
	return result, nil
}

func leaseLostResult(res RunResult, startedAt time.Time) (RunResult, error) {
	res.Reason = StopReasonContextCancelled
	res.Duration = time.Since(startedAt)
	return res, fmt.Errorf("agent: Resume: %w", eventlog.ErrLeaseLost)
}

// resumeCheckpoints owns the checkpoint writes Resume makes under the
// session lease. It reads Resume's live result through pointers so the
// payload reflects whatever the loop has banked by the time it writes.
type resumeCheckpoints struct {
	ctx, leaseCtx   context.Context
	a               *agent.Agent
	lock            *eventlog.SessionLock
	cfg             *autoConfig
	result          *RunResult
	goal            string
	haveSubstantive *bool
	startedAt       time.Time
}

// turn writes a per-turn or schedule checkpoint unless the lease is gone.
func (c *resumeCheckpoints) turn(payload checkpointPayload) {
	if leaseLost(c.lock) {
		return
	}
	_ = emitCheckpoint(c.leaseCtx, c.a, payload)
}

// writeFinal is the unguarded final write; final and loseLease decide
// whether it may run.
func (c *resumeCheckpoints) writeFinal(reason StopReason) {
	// Detached write: the final checkpoint is the crash-resume anchor
	// and is emitted even when the loop exits because ctx was cancelled
	// — passing the dead ctx here silently lost the checkpoint on
	// exactly the shutdown path where it matters most (#365).
	emitFinalCheckpointDetached(c.ctx, c.a, checkpointPayload{
		Turn:                 c.result.Turns,
		InputTokens:          c.result.InputTokens,
		OutputTokens:         c.result.OutputTokens,
		CostUSD:              c.result.CostUSD,
		Goal:                 c.goal,
		ContinuationPrompt:   c.cfg.continuationPrompt,
		StopReason:           string(reason),
		DoneDetail:           c.result.DoneDetail,
		FinalText:            c.result.FinalText,
		FinalTextSubstantive: *c.haveSubstantive,
	})
}

// final writes the terminal checkpoint for a run that still holds its
// lease. Never after losing it: another process may own the session,
// and appending to its eventlog would commit a split-brain write (#1361).
func (c *resumeCheckpoints) final(reason StopReason) {
	if leaseLost(c.lock) {
		return
	}
	c.writeFinal(reason)
}

// loseLease is the exit for every lease-lost path. Lost() closes for two
// reasons: the row was stolen, or heartbeats stalled past the loss
// window. In a single-replica deployment the second case is a database
// outage with nobody to take the lease, and the partial turn's
// accounting is worth keeping; Reaffirm proves which case this is with
// one conditional UPDATE, and only a proven-still-held lease gets the
// final checkpoint (#1361 review). The run ends with ErrLeaseLost either
// way.
func (c *resumeCheckpoints) loseLease() (RunResult, error) {
	if held, err := c.lock.Reaffirm(c.ctx); err == nil && held {
		c.result.Duration = time.Since(c.startedAt)
		c.writeFinal(StopReasonContextCancelled)
	}
	return leaseLostResult(*c.result, c.startedAt)
}

// bindLeaseContext returns a child context that cancels with
// eventlog.ErrLeaseLost as soon as lock.Lost() closes (#1361).
func bindLeaseContext(ctx context.Context, lock *eventlog.SessionLock) (context.Context, context.CancelFunc) {
	leaseCtx, cancelLease := context.WithCancelCause(ctx)
	go func() {
		select {
		case <-lock.Lost():
			cancelLease(eventlog.ErrLeaseLost)
		case <-leaseCtx.Done():
		}
	}()
	return leaseCtx, func() { cancelLease(nil) }
}

// buildResumedAgent registers the done/return and schedule lifecycle
// tools and invokes the caller's ResumeBuildFunc.
func buildResumedAgent(build ResumeBuildFunc, sessionID string, cfg *autoConfig, doneCh chan string) (*agent.Agent, <-chan coretools.ScheduleEvent, error) {
	doneTools, err := buildDoneTools(cfg, doneCh)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: Resume: build done tool: %w", err)
	}
	extras := append([]tool.Tool(nil), doneTools...)
	var scheduleCh <-chan coretools.ScheduleEvent
	if cfg.scheduler != nil {
		schTool, ch, err := coretools.NewScheduleTool(coretools.ScheduleOptions{
			Name:        cfg.scheduleToolName,
			Description: cfg.scheduleToolDescription,
			MaxDefer:    cfg.scheduleToolMaxDefer,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("agent: Resume: build schedule tool: %w", err)
		}
		extras = append(extras, schTool)
		scheduleCh = ch
	}
	a, err := build(extras, sessionID)
	warnIfInteractiveMode(a, err)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: Resume: build agent: %w", err)
	}
	if a == nil {
		return nil, nil, errors.New("agent: Resume: build returned nil agent")
	}
	return a, scheduleCh, nil
}

// awaitResumeWake honors a deferred checkpoint's NextWakeAt timestamp
// at startup, unblocking early if the caller context cancels or the
// session lock is lost (#1361).
func awaitResumeWake(ctx context.Context, lock *eventlog.SessionLock, latest checkpointPayload, found bool, resumeStart time.Time) (RunResult, bool, error) {
	if !found || latest.NextWakeAt.IsZero() {
		return RunResult{}, false, nil
	}
	wait := time.Until(latest.NextWakeAt)
	if wait <= 0 {
		return RunResult{}, false, nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	base := RunResult{
		Reason:       StopReasonContextCancelled,
		Turns:        latest.Turn,
		InputTokens:  latest.InputTokens,
		OutputTokens: latest.OutputTokens,
		CostUSD:      latest.CostUSD,
		FinalText:    latest.FinalText,
	}
	select {
	case <-t.C:
		return RunResult{}, false, nil
	case <-lock.Lost():
		base.Duration = time.Since(resumeStart)
		return base, true, fmt.Errorf("agent: Resume: %w", eventlog.ErrLeaseLost)
	case <-ctx.Done():
		base.Duration = time.Since(resumeStart)
		return base, true, ctx.Err()
	}
}

func leaseLost(lock *eventlog.SessionLock) bool {
	if lock == nil {
		return false
	}
	select {
	case <-lock.Lost():
		return true
	default:
		return false
	}
}

// loadLatestCheckpoint walks the session's event log via Since(0)
// with author-suffix filtering and returns the most recent
// checkpoint payload (highest seq). found=false when the session
// exists but has no checkpoint events yet — a valid state that
// triggers a turn-0 start in the caller.
func loadLatestCheckpoint(ctx context.Context, ref SessionRef) (checkpointPayload, bool, error) {
	if ref.Handle == nil {
		return checkpointPayload{}, false, errors.New("loadLatestCheckpoint: nil Handle")
	}
	var latest checkpointPayload
	found := false
	for entry, err := range ref.Handle.Stream.Since(ctx, 0,
		eventlog.ForSession(ref.AppName, ref.UserID, ref.SessionID),
		eventlog.WithAuthorSuffix(checkpointAuthorSuffix),
	) {
		if err != nil {
			return checkpointPayload{}, false, fmt.Errorf("scan checkpoints: %w", err)
		}
		if entry.Event == nil || entry.Event.CustomMetadata == nil {
			continue
		}
		latest = checkpointFromMap(entry.Event.CustomMetadata)
		found = true
	}
	return latest, found, nil
}
