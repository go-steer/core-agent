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
	"time"

	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/agent/internal/toolcalls"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	coretools "github.com/go-steer/core-agent/v3/pkg/tools"
)

// buildLifecycleTools constructs the driver-injected done/return tool(s) and
// optional schedule tool shared by Run and Resume.
func buildLifecycleTools(cfg *autoConfig, callerPrefix string, doneCh chan string) ([]tool.Tool, <-chan coretools.ScheduleEvent, error) {
	doneTools, err := buildDoneTools(cfg, doneCh)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: build done tool: %w", callerPrefix, err)
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
			return nil, nil, fmt.Errorf("%s: build schedule tool: %w", callerPrefix, err)
		}
		extras = append(extras, schTool)
		scheduleCh = ch
	}
	return extras, scheduleCh, nil
}

// turnLoop holds the state and dependencies shared by Run and Resume while
// driving the multi-turn autonomous loop (#1370). When lock is nil (Run),
// lease checks are no-ops and turnBaseCtx is ctx; when lock is non-nil
// (Resume), turnBaseCtx is the lease-bound context and checkpoint writes are
// gated on the session lease.
type turnLoop struct {
	callerPrefix    string
	ctx             context.Context
	turnBaseCtx     context.Context
	a               *agent.Agent
	cfg             *autoConfig
	goal            string
	initialPrompt   string
	doneCh          chan string
	scheduleCh      <-chan coretools.ScheduleEvent
	startedAt       time.Time
	result          RunResult
	haveSubstantive bool
	lock            *eventlog.SessionLock
}

func (l *turnLoop) run() (RunResult, error) {
	prompt := l.initialPrompt
	for {
		if leaseLost(l.lock) {
			return l.loseLease()
		}
		// Pre-turn budget checks (applied to cumulative totals on Resume).
		if reason := budgetStop(l.cfg, &l.result, time.Since(l.startedAt)); reason != "" {
			l.result.Reason = reason
			break
		}
		if err := l.ctx.Err(); err != nil {
			return l.finishWithError(StopReasonContextCancelled, err)
		}

		// BeforeTurn hook (used by Handle to implement Pause). Runs after
		// budget + ctx checks; may block and may return an error to abort.
		if l.cfg.beforeTurn != nil {
			if err := l.cfg.beforeTurn(l.turnBaseCtx, l.result.Turns+1); err != nil {
				if leaseLost(l.lock) {
					return l.loseLease()
				}
				reason := StopReasonRetryAborted
				if errors.Is(err, context.Canceled) && l.ctx.Err() != nil {
					reason = StopReasonContextCancelled
				}
				return l.finishWithError(reason, err)
			}
		}

		turnRes, turnErr := l.executeTurn(prompt)
		if leaseLost(l.lock) {
			bankReturn(&l.result, turnRes)
			return l.loseLease()
		}

		if turnErr != nil {
			cont, err := l.handleTurnError(turnRes, turnErr, &prompt)
			if !cont {
				return l.result, err
			}
			continue
		}

		if l.checkStopAfterTurn(turnRes) {
			break
		}

		if turnRes.scheduleSignaled && l.cfg.scheduler != nil {
			cont, err := l.handleSchedule(turnRes.scheduleEvent, &prompt)
			if err != nil {
				return l.result, err
			}
			if !cont {
				break
			}
			continue
		}

		// Per-turn checkpoint after a clean (non-done, non-error) turn.
		l.emitTurn(perTurnCheckpoint(l.result, l.goal, l.cfg.continuationPrompt, l.haveSubstantive))
		prompt = l.cfg.continuationPrompt
	}

	if leaseLost(l.lock) {
		return l.loseLease()
	}
	l.result.Duration = time.Since(l.startedAt)
	l.emitFinal(l.result.Reason)
	return l.result, nil
}

func (l *turnLoop) executeTurn(prompt string) (turnResult, error) {
	turnCtx := l.turnBaseCtx
	var cancel context.CancelFunc
	if l.cfg.perTurnTimeout > 0 {
		turnCtx, cancel = context.WithTimeout(l.turnBaseCtx, l.cfg.perTurnTimeout)
	}
	// A goal a person wrote governs every turn of the run, so every turn
	// hands it to the auto-mode approver as the task (#1175). The
	// continuation prompts never are: the loop wrote them.
	if l.cfg.operatorGoal && l.goal != "" {
		turnCtx = agent.WithOperatorTask(turnCtx, l.goal)
	}
	turnRes, turnErr := runOneTurn(turnCtx, l.a, prompt, l.doneCh, l.scheduleCh, l.cfg, l.result.Turns+1, l.result.CostUSD)
	if cancel != nil {
		cancel()
	}

	// Roll up usage and provenance from this turn into the overall result
	// before any early error/lease return so callers see what ran.
	l.result.InputTokens += turnRes.inputTokens
	l.result.OutputTokens += turnRes.outputTokens
	l.result.CostUSD += turnRes.costUSD
	l.result.Turns++
	var dropped int
	l.result.Calls, dropped = toolcalls.Append(l.result.Calls, turnRes.calls, toolcalls.DefaultMaxCalls)
	l.result.CallsDropped += turnRes.callsDropped + dropped
	if keepFinalText(turnRes.text, turnRes.usedTools, l.haveSubstantive) {
		l.result.FinalText = turnRes.text
		l.haveSubstantive = l.haveSubstantive || turnRes.usedTools
	}
	return turnRes, turnErr
}

func (l *turnLoop) handleTurnError(turnRes turnResult, turnErr error, prompt *string) (bool, error) {
	bankReturn(&l.result, turnRes)
	// Context cancellation propagates immediately regardless of retry policy.
	if errors.Is(turnErr, context.Canceled) && l.ctx.Err() != nil {
		_, err := l.finishWithError(StopReasonContextCancelled, turnErr)
		return false, err
	}
	decision := AbortRun
	if l.cfg.retryPolicy != nil {
		decision = l.cfg.retryPolicy(turnErr, l.result.Turns)
	}
	switch decision {
	case RetryTurn:
		return true, nil
	case SkipTurn:
		*prompt = l.cfg.continuationPrompt
		l.emitTurn(perTurnCheckpoint(l.result, l.goal, l.cfg.continuationPrompt, l.haveSubstantive))
		return true, nil
	default:
		_, err := l.finishWithError(StopReasonRetryAborted, turnErr)
		return false, err
	}
}

func (l *turnLoop) checkStopAfterTurn(turnRes turnResult) bool {
	if turnRes.doneSignaled {
		l.result.Reason = StopReasonCompleted
		bankReturn(&l.result, turnRes)
		return true
	}
	if turnRes.costCapped {
		l.result.Reason = StopReasonMaxCost
		return true
	}
	if l.cfg.stopOnNaturalEnd && !turnRes.requestedTools && !turnRes.scheduleSignaled {
		l.result.Reason = StopReasonCompleted
		bankNaturalEnd(&l.result, turnRes)
		return true
	}
	return false
}

func (l *turnLoop) handleSchedule(ev coretools.ScheduleEvent, prompt *string) (bool, error) {
	if l.cfg.maxDefer > 0 {
		ceiling := time.Now().Add(l.cfg.maxDefer)
		if ev.WakeAt.After(ceiling) {
			log.Printf("%s: clamping scheduler wake-time from %s to driver MaxDefer ceiling %s (max_defer=%s)",
				l.callerPrefix, ev.WakeAt.Format(time.RFC3339), ceiling.Format(time.RFC3339), l.cfg.maxDefer)
			ev.WakeAt = ceiling
		}
	}
	l.emitTurn(scheduleCheckpoint(l.result, l.goal, l.cfg.continuationPrompt, ev, l.haveSubstantive))

	serr := awaitScheduledWake(l.turnBaseCtx, l.a, l.cfg, ev)
	if leaseLost(l.lock) {
		_, err := l.loseLease()
		return false, err
	}
	switch {
	case serr == nil:
		if ev.NextPrompt != "" {
			*prompt = ev.NextPrompt
		} else {
			*prompt = l.cfg.continuationPrompt
		}
		return true, nil
	case errors.Is(serr, coretools.ErrSchedulerDefer):
		l.result.Reason = StopReasonDeferred
		l.result.NextWakeAt = ev.WakeAt
		return false, nil
	case errors.Is(serr, context.Canceled) && l.ctx.Err() != nil:
		_, err := l.finishWithError(StopReasonContextCancelled, serr)
		return false, err
	default:
		_, err := l.finishWithError(StopReasonRetryAborted, fmt.Errorf("%s: scheduler: %w", l.callerPrefix, serr))
		return false, err
	}
}

func (l *turnLoop) finishWithError(reason StopReason, err error) (RunResult, error) {
	l.result.Reason = reason
	l.result.Duration = time.Since(l.startedAt)
	l.emitFinal(reason)
	return l.result, err
}

// emitTurn writes a per-turn or schedule checkpoint unless the session lease
// has been lost.
func (l *turnLoop) emitTurn(payload checkpointPayload) {
	if leaseLost(l.lock) {
		return
	}
	_ = emitCheckpoint(l.turnBaseCtx, l.a, payload)
}

// writeFinal is the unguarded detached final checkpoint write; emitFinal and
// loseLease decide whether it may run.
func (l *turnLoop) writeFinal(reason StopReason) {
	// Detached write: the final checkpoint is the crash-resume anchor and is
	// emitted even when the loop exits because ctx was cancelled (#365).
	emitFinalCheckpointDetached(l.ctx, l.a, checkpointPayload{
		Turn:                 l.result.Turns,
		InputTokens:          l.result.InputTokens,
		OutputTokens:         l.result.OutputTokens,
		CostUSD:              l.result.CostUSD,
		Goal:                 l.goal,
		ContinuationPrompt:   l.cfg.continuationPrompt,
		StopReason:           string(reason),
		DoneDetail:           l.result.DoneDetail,
		FinalText:            l.result.FinalText,
		FinalTextSubstantive: l.haveSubstantive,
	})
}

// emitFinal writes the terminal checkpoint for a run that still holds its
// lease (or an unleased Run). Never after losing a lease: another process may
// own the session, and appending to its eventlog would commit a split-brain
// write (#1361).
func (l *turnLoop) emitFinal(reason StopReason) {
	if leaseLost(l.lock) {
		return
	}
	l.writeFinal(reason)
}

// loseLease is the exit for every lease-lost path. Reaffirm proves whether the
// row was stolen or heartbeats merely stalled during a DB outage; only a
// proven-still-held lease gets the final checkpoint (#1361).
func (l *turnLoop) loseLease() (RunResult, error) {
	if held, err := l.lock.Reaffirm(l.ctx); err == nil && held {
		l.result.Duration = time.Since(l.startedAt)
		l.writeFinal(StopReasonContextCancelled)
	}
	var err error
	l.result, err = leaseLostResult(l.result, l.startedAt)
	return l.result, err
}
