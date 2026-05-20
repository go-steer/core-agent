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
	"strings"
	"time"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/eventlog"
	coretools "github.com/go-steer/core-agent/tools"
)

// ResumeBuildFunc is the agent constructor accepted by
// ResumeAutonomous. It mirrors RunAutonomous's BuildFunc signature
// but adds the sessionID the new agent must adopt — implementations
// pass it to agent.WithSession so the constructed agent reuses the
// session being resumed.
type ResumeBuildFunc func(extras []tool.Tool, sessionID string) (*Agent, error)

// SessionRef identifies the session ResumeAutonomous resumes from.
// Handle supplies both the eventlog.Stream (used to find the latest
// checkpoint) and the session.Service (used by the constructed
// agent for live event reads + writes).
type SessionRef struct {
	Handle    *eventlog.Handle
	AppName   string
	UserID    string
	SessionID string
}

// ResumeAutonomous reads the most recent checkpoint event from the
// session's event log, reconstructs RunResult totals, and continues
// the run from the next turn. The build function receives the
// resumed sessionID so the constructed agent rejoins the same
// session via agent.WithSession.
//
// Behavior:
//   - Acquires an exclusive SessionLock on (App, User, Session). A
//     concurrent ResumeAutonomous on the same session returns
//     ErrSessionLocked from eventlog.
//   - If the session has no checkpoint events at all, the run starts
//     from turn 0 with whatever event history the session already
//     holds — "make this existing session autonomous from here" is
//     a valid use case.
//   - If the latest checkpoint has stop_reason set (terminal state),
//     ResumeAutonomous returns that state immediately without
//     constructing the agent or running any turns.
//   - Otherwise, the loop continues with prompt =
//     checkpoint.ContinuationPrompt; budgets carry forward.
func ResumeAutonomous(ctx context.Context, build ResumeBuildFunc, ref SessionRef, opts ...AutonomousOption) (RunResult, error) {
	if build == nil {
		return RunResult{}, errors.New("agent: ResumeAutonomous: build is required")
	}
	if ref.Handle == nil {
		return RunResult{}, errors.New("agent: ResumeAutonomous: SessionRef.Handle is required")
	}
	if strings.TrimSpace(ref.SessionID) == "" {
		return RunResult{}, errors.New("agent: ResumeAutonomous: SessionRef.SessionID is required")
	}
	cfg := defaultAutoConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	// Lock first — fail fast if another process is resuming the
	// same session.
	lock, err := ref.Handle.AcquireLock(ctx, ref.AppName, ref.UserID, ref.SessionID)
	if err != nil {
		return RunResult{}, fmt.Errorf("agent: ResumeAutonomous: %w", err)
	}
	defer func() { _ = lock.Release() }()

	// Discover the latest checkpoint by suffix-matching the author
	// so handoffs across binaries (core-agent ↔ scion-agent ↔
	// ax-agent) work.
	latest, found, err := loadLatestCheckpoint(ctx, ref)
	if err != nil {
		return RunResult{}, fmt.Errorf("agent: ResumeAutonomous: %w", err)
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

	// Done-tool registration mirrors RunAutonomous so the model has
	// the same termination gesture available on resume.
	doneCh := make(chan string, 1)
	doneTool, err := coretools.NewLifecycleTool(coretools.LifecycleOptions{
		Name:          cfg.doneToolName,
		Description:   cfg.doneToolDescription,
		AllowedStates: []string{"done"},
		Handler: func(_ context.Context, ev coretools.LifecycleEvent) error {
			select {
			case doneCh <- ev.Detail:
			default:
			}
			return nil
		},
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("agent: ResumeAutonomous: build done tool: %w", err)
	}

	extras := []tool.Tool{doneTool}
	var scheduleCh <-chan coretools.ScheduleEvent
	if cfg.scheduler != nil {
		schTool, ch, err := coretools.NewScheduleTool(coretools.ScheduleOptions{
			Name:        cfg.scheduleToolName,
			Description: cfg.scheduleToolDescription,
			MaxDefer:    cfg.scheduleToolMaxDefer,
		})
		if err != nil {
			return RunResult{}, fmt.Errorf("agent: ResumeAutonomous: build schedule tool: %w", err)
		}
		extras = append(extras, schTool)
		scheduleCh = ch
	}

	a, err := build(extras, ref.SessionID)
	if err != nil {
		return RunResult{}, fmt.Errorf("agent: ResumeAutonomous: build agent: %w", err)
	}
	if a == nil {
		return RunResult{}, errors.New("agent: ResumeAutonomous: build returned nil agent")
	}

	// If the latest checkpoint is a deferred-state checkpoint, honor
	// the wake-time at startup so daemon-mode resume picks up at the
	// scheduled wake rather than firing immediately. ExitOnDefer-mode
	// resume reaches this code path when the orchestrator fires after
	// the wake-time, in which case time.Until returns <=0 and we
	// proceed without delay. A cancelled context unblocks promptly.
	if found && !latest.NextWakeAt.IsZero() {
		wait := time.Until(latest.NextWakeAt)
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				result := RunResult{
					Reason:       StopReasonContextCancelled,
					Turns:        latest.Turn,
					InputTokens:  latest.InputTokens,
					OutputTokens: latest.OutputTokens,
					CostUSD:      latest.CostUSD,
					FinalText:    latest.FinalText,
				}
				result.Duration = time.Since(time.Now())
				return result, ctx.Err()
			}
		}
	}

	startedAt := time.Now()
	result := RunResult{
		Turns:        latest.Turn,
		InputTokens:  latest.InputTokens,
		OutputTokens: latest.OutputTokens,
		CostUSD:      latest.CostUSD,
		FinalText:    latest.FinalText,
	}

	// Use the resume goal-of-record from the checkpoint when we
	// have one; otherwise fall back to the empty string so audit
	// readers can still tell "this was a resume without prior
	// checkpoints."
	goal := latest.Goal

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
		// Pre-turn budget checks — applied to the cumulative
		// (resumed + newly-accumulated) totals.
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

		turnCtx := ctx
		var cancel context.CancelFunc
		if cfg.perTurnTimeout > 0 {
			turnCtx, cancel = context.WithTimeout(ctx, cfg.perTurnTimeout)
		}
		turnRes, turnErr := runOneTurn(turnCtx, a, prompt, doneCh, scheduleCh, &cfg, result.Turns+1)
		if cancel != nil {
			cancel()
		}

		result.InputTokens += turnRes.inputTokens
		result.OutputTokens += turnRes.outputTokens
		result.CostUSD += turnRes.costUSD
		result.Turns++
		if turnRes.text != "" {
			result.FinalText = turnRes.text
		}

		if turnErr != nil {
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

		// Schedule emission — same wiring as RunAutonomous so a
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
			_ = emitCheckpoint(ctx, a, scheduleCheckpoint(result, goal, cfg.continuationPrompt, ev))
			serr := cfg.scheduler.BeforeNextTurn(ctx, ev)
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
				return result, fmt.Errorf("agent: ResumeAutonomous: scheduler: %w", serr)
			}
		}

		_ = emitCheckpoint(ctx, a, perTurnCheckpoint(result, goal, cfg.continuationPrompt))
		prompt = cfg.continuationPrompt
	}

deferredExit:
	result.Duration = time.Since(startedAt)
	emitFinalCheckpoint(result.Reason)
	return result, nil
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
