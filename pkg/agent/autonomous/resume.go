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

	loop := &turnLoop{
		callerPrefix:  "agent: Resume",
		ctx:           ctx,
		turnBaseCtx:   leaseCtx,
		a:             a,
		cfg:           &cfg,
		goal:          latest.Goal,
		initialPrompt: prompt,
		doneCh:        doneCh,
		scheduleCh:    scheduleCh,
		startedAt:     time.Now(),
		result: RunResult{
			Turns:        latest.Turn,
			InputTokens:  latest.InputTokens,
			OutputTokens: latest.OutputTokens,
			CostUSD:      latest.CostUSD,
			FinalText:    latest.FinalText,
		},
		// Carried across the restart rather than re-derived: a resumed run
		// that forgot the pre-crash FinalText came from a tool-using turn
		// would let the first idle turn after the restart overwrite it,
		// which is #731 with an extra step.
		haveSubstantive: latest.FinalTextSubstantive,
		lock:            lock,
	}
	return loop.run()
}

func leaseLostResult(res RunResult, startedAt time.Time) (RunResult, error) {
	res.Reason = StopReasonContextCancelled
	res.Duration = time.Since(startedAt)
	return res, fmt.Errorf("agent: Resume: %w", eventlog.ErrLeaseLost)
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
	extras, scheduleCh, err := buildLifecycleTools(cfg, "agent: Resume", doneCh)
	if err != nil {
		return nil, nil, err
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
