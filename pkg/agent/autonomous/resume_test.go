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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

// openTestEventLog returns a Handle backed by a fresh on-disk SQLite
// database. Mirrors the helper in eventlog tests; we duplicate
// rather than export so the agent test package stays self-contained.
func openTestEventLog(t *testing.T) (*eventlog.Handle, func()) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "session.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	return h, func() { _ = h.Close() }
}

// resumeBuilder assembles a ResumeBuildFunc that wires the supplied
// LLM, the supplied event log handle, and the resumed session ID.
// Mirrors what a real consumer would write.
func resumeBuilder(llm *stubLLM, h *eventlog.Handle, app, user string) ResumeBuildFunc {
	return func(extras []tool.Tool, sessionID string) (*agent.Agent, error) {
		return agent.New(llm,
			agent.WithAppName(app),
			agent.WithSession(user, sessionID),
			agent.WithEventLog(h),
			agent.WithTools(extras),
			agent.WithInstruction("test agent"),
		)
	}
}

// runBuilder mirrors resumeBuilder for the fresh-run case.
func runBuilder(llm *stubLLM, h *eventlog.Handle, app, user, sess string) func(extras []tool.Tool) (*agent.Agent, error) {
	return func(extras []tool.Tool) (*agent.Agent, error) {
		return agent.New(llm,
			agent.WithAppName(app),
			agent.WithSession(user, sess),
			agent.WithEventLog(h),
			agent.WithTools(extras),
			agent.WithInstruction("test agent"),
		)
	}
}

func TestResumeAutonomous_RequiresBuild(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	_, err := Resume(context.Background(), nil, SessionRef{
		Handle: h, AppName: "app", UserID: "u", SessionID: "s",
	})
	if err == nil || !strings.Contains(err.Error(), "build is required") {
		t.Errorf("expected build-required error, got %v", err)
	}
}

func TestResumeAutonomous_RequiresHandle(t *testing.T) {
	t.Parallel()
	_, err := Resume(context.Background(),
		func([]tool.Tool, string) (*agent.Agent, error) { return nil, nil },
		SessionRef{AppName: "app", UserID: "u", SessionID: "s"})
	if err == nil || !strings.Contains(err.Error(), "Handle is required") {
		t.Errorf("expected Handle-required error, got %v", err)
	}
}

func TestResumeAutonomous_RequiresSessionID(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	_, err := Resume(context.Background(),
		func([]tool.Tool, string) (*agent.Agent, error) { return nil, nil },
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: " "})
	if err == nil || !strings.Contains(err.Error(), "SessionID is required") {
		t.Errorf("expected SessionID-required error, got %v", err)
	}
}

func TestRunAutonomous_EmitsCheckpointPerTurn(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("one", 5, 5),
		textTurn("two", 5, 5),
		doneCallTurn("done after two"),
		textTurn("ok", 0, 0),
	}}
	res, err := Run(context.Background(),
		runBuilder(llm, h, "app", "u", "checkpoints"),
		"go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want completed", res.Reason)
	}

	// Walk the eventlog and count checkpoint events. Per-turn
	// checkpoints fire after non-done turns; the final checkpoint
	// fires on loop exit. So for a 3-turn run that ends with done,
	// expect 2 per-turn + 1 final = 3 checkpoint events.
	var checkpoints int
	for entry, err := range h.Stream.Since(context.Background(), 0,
		eventlog.ForSession("app", "u", "checkpoints"),
		eventlog.WithAuthorSuffix(checkpointAuthorSuffix)) {
		if err != nil {
			t.Fatalf("Since: %v", err)
		}
		if entry.Event != nil {
			checkpoints++
		}
	}
	if checkpoints < 2 {
		t.Errorf("expected at least 2 checkpoint events, got %d", checkpoints)
	}
}

func TestResumeAutonomous_TerminalCheckpointReturnsImmediately(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	llm := &stubLLM{scenarios: []scenarioFn{
		doneCallTurn("the result"),
		textTurn("ok", 4, 2),
	}}
	first, err := Run(context.Background(),
		runBuilder(llm, h, "app", "u", "terminal"),
		"go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if first.Reason != StopReasonCompleted {
		t.Fatalf("first run Reason = %q, want completed", first.Reason)
	}

	// Resume — should return the terminal state without
	// constructing the agent or calling the LLM again. We pin this
	// by tracking the LLM call count.
	llm2 := &stubLLM{scenarios: []scenarioFn{
		textTurn("should not be invoked", 0, 0),
	}}
	res, err := Resume(context.Background(),
		resumeBuilder(llm2, h, "app", "u"),
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "terminal"})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("resumed Reason = %q, want completed", res.Reason)
	}
	if res.DoneDetail != "the result" {
		t.Errorf("DoneDetail = %q, want %q", res.DoneDetail, "the result")
	}
	if llm2.calls != 0 {
		t.Errorf("LLM was invoked %d times on terminal-state resume; want 0", llm2.calls)
	}
}

// Resume carries the same loop shape as Run and has drifted from it
// before — returntool.go says so in as many words, about this exact
// termination gesture. So the #1002 bank is asserted on both drivers
// rather than on the one the spawn path happens to use: a deferred
// subagent that returns and then 429s on resume loses its deliverable
// in precisely the same way, and deleting Resume's bankReturn is
// invisible to every test that only drives Run.
func TestResumeAutonomous_BankedReturnSurvivesALaterFailure(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	// A first run that stops on its turn cap, leaving a resumable
	// mid-run checkpoint.
	llm1 := &stubLLM{scenarios: []scenarioFn{textTurn("looking", 1, 1)}}
	if _, err := Run(context.Background(),
		runBuilder(llm1, h, "app", "u", "banked"), "go", WithMaxTurns(1)); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// The resumed run returns a result and its next model call dies.
	wantErr := errors.New("Error 429, Message: Resource exhausted")
	llm2 := &stubLLM{scenarios: []scenarioFn{
		doneCallTurn("the emailservice limit was squeezed to 8Mi"),
		errTurn(wantErr),
	}}
	res, err := Resume(context.Background(),
		resumeBuilder(llm2, h, "app", "u"),
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "banked"},
		WithMaxTurns(10))
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
	if !res.Returned || res.DoneDetail != "the emailservice limit was squeezed to 8Mi" {
		t.Errorf("Returned = %v, DoneDetail = %q; want the banked result to survive the failure on a resumed run too",
			res.Returned, res.DoneDetail)
	}
}

func TestResumeAutonomous_ContinuesFromMidRun(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	// First run: 2 turns with no done, capped at 2 turns.
	llm1 := &stubLLM{scenarios: []scenarioFn{
		textTurn("first", 7, 3),
		textTurn("second", 7, 3),
	}}
	first, err := Run(context.Background(),
		runBuilder(llm1, h, "app", "u", "midrun"),
		"go", WithMaxTurns(2))
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if first.Reason != StopReasonMaxTurns {
		t.Fatalf("first Reason = %q, want %q", first.Reason, StopReasonMaxTurns)
	}

	// Resume with more turns budget. The next turn calls done.
	llm2 := &stubLLM{scenarios: []scenarioFn{
		doneCallTurn("done on resume"),
		textTurn("ok", 1, 1),
	}}
	res, err := Resume(context.Background(),
		resumeBuilder(llm2, h, "app", "u"),
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "midrun"},
		WithMaxTurns(10))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("resumed Reason = %q, want completed", res.Reason)
	}
	if res.DoneDetail != "done on resume" {
		t.Errorf("DoneDetail = %q", res.DoneDetail)
	}
	// Cumulative totals carry forward: 2 turns from the first run +
	// 1 done turn here = 3 total turns recorded.
	if res.Turns != 3 {
		t.Errorf("Turns = %d, want 3 (2 resumed + 1 new)", res.Turns)
	}
	// Token totals should also accumulate: 2*(7+3) from first +
	// done turn + the textTurn after the tool call.
	if res.InputTokens < 14 {
		t.Errorf("InputTokens = %d, want >= 14 (carried forward from first run)", res.InputTokens)
	}
}

func TestResumeAutonomous_BudgetsCarryForward(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	// First run: hits max_turns=3 with no done.
	llm1 := &stubLLM{scenarios: []scenarioFn{
		textTurn("a", 0, 0),
		textTurn("b", 0, 0),
		textTurn("c", 0, 0),
	}}
	if _, err := Run(context.Background(),
		runBuilder(llm1, h, "app", "u", "budgets"),
		"go", WithMaxTurns(3)); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Resume with the same max_turns budget — already exceeded,
	// should stop immediately on pre-turn check without running a
	// new turn.
	llm2 := &stubLLM{scenarios: []scenarioFn{
		textTurn("never", 0, 0),
	}}
	res, err := Resume(context.Background(),
		resumeBuilder(llm2, h, "app", "u"),
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "budgets"},
		WithMaxTurns(3))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Reason != StopReasonMaxTurns {
		t.Errorf("Reason = %q, want %q (budget should fire on resumed totals)", res.Reason, StopReasonMaxTurns)
	}
	if res.Turns != 3 {
		t.Errorf("Turns = %d, want 3 (no new turns ran)", res.Turns)
	}
	if llm2.calls != 0 {
		t.Errorf("LLM was invoked %d times when budget was already exhausted; want 0", llm2.calls)
	}
}

func TestResumeAutonomous_NoCheckpointStartsAtZero(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	// Pre-create an empty session — no events, no checkpoints.
	if _, err := h.Service.Create(context.Background(), &session.CreateRequest{
		AppName: "app", UserID: "u", SessionID: "fresh",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	llm := &stubLLM{scenarios: []scenarioFn{
		doneCallTurn("first done"),
		textTurn("ok", 0, 0),
	}}
	res, err := Resume(context.Background(),
		resumeBuilder(llm, h, "app", "u"),
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "fresh"})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Errorf("Reason = %q, want completed", res.Reason)
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1 (started from zero)", res.Turns)
	}
}

func TestResumeAutonomous_LockBlocksConcurrent(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	// Plant a checkpoint and a held lock to simulate another
	// process running Resume concurrently.
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("seed", 0, 0),
	}}
	if _, err := Run(context.Background(),
		runBuilder(llm, h, "app", "u", "locked"),
		"go", WithMaxTurns(1)); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// Take the lock from another "process".
	otherLock, err := h.AcquireLock(context.Background(), "app", "u", "locked")
	if err != nil {
		t.Fatalf("plant lock: %v", err)
	}
	defer otherLock.Release()

	// Resume must refuse with a clear error.
	_, err = Resume(context.Background(),
		resumeBuilder(llm, h, "app", "u"),
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "locked"})
	if !errors.Is(err, eventlog.ErrSessionLocked) {
		t.Errorf("err = %v, want eventlog.ErrSessionLocked", err)
	}
}

// TestEmitFinalCheckpointDetached_WritesDespiteCancelledContext is the
// #365 regression: the final checkpoint is emitted on every loop exit,
// including context cancellation/shutdown — exactly when crash-resume
// needs it most. Passing the already-cancelled ctx straight to
// emitCheckpoint failed the DB write (error swallowed), losing the
// checkpoint. The detached write must land it anyway.
func TestEmitFinalCheckpointDetached_WritesDespiteCancelledContext(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	a, err := agent.New(&stubLLM{},
		agent.WithAppName("app"),
		agent.WithSession("u", "cancelled-cp"),
		agent.WithEventLog(h),
		agent.WithInstruction("test agent"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The session already exists by the time the final checkpoint
	// fires (the loop has run turns); the shutdown path is only special
	// because ctx is dead. Create it up front with a live context.
	if _, err := a.SessionService().Create(context.Background(), &session.CreateRequest{
		AppName:   "app",
		UserID:    "u",
		SessionID: "cancelled-cp",
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// The shutdown path: a context that's already cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	payload := checkpointPayload{Turn: 1, StopReason: string(StopReasonContextCancelled)}

	// Baseline: the old behavior (dead ctx → emitCheckpoint) fails, so
	// the checkpoint would be silently lost. Assert the failure so the
	// test genuinely exercises the cancellation path — if the backend
	// ignored cancellation the bug couldn't manifest and this guard
	// would flag it.
	if err := emitCheckpoint(ctx, a, payload); err == nil {
		t.Fatalf("emitCheckpoint with a cancelled ctx unexpectedly succeeded; cannot exercise the #365 path")
	}

	// The fix: the detached write lands the checkpoint despite the
	// cancelled ctx.
	emitFinalCheckpointDetached(ctx, a, payload)

	var checkpoints int
	for entry, err := range h.Stream.Since(context.Background(), 0,
		eventlog.ForSession("app", "u", "cancelled-cp"),
		eventlog.WithAuthorSuffix(checkpointAuthorSuffix)) {
		if err != nil {
			t.Fatalf("Since: %v", err)
		}
		if entry.Event != nil {
			checkpoints++
		}
	}
	if checkpoints != 1 {
		t.Errorf("checkpoint events = %d, want 1 (detached final checkpoint must land despite cancelled ctx; #365)", checkpoints)
	}
}

// TestResumeAutonomous_CancelledWhileWaitingReportsElapsedDuration
// pins the cancelled-while-waiting exit of the deferred-wake wait:
// Duration must measure elapsed time since the resume started. The
// original code computed time.Since(time.Now()), which is always ~0
// (#372).
func TestResumeAutonomous_CancelledWhileWaitingReportsElapsedDuration(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	// Seed a non-terminal checkpoint whose NextWakeAt is far in the
	// future so Resume parks in the deferred-wake wait.
	llm := &stubLLM{}
	seed, err := resumeBuilder(llm, h, "app", "u")(nil, "wakewait")
	if err != nil {
		t.Fatalf("build seeding agent: %v", err)
	}
	if _, err := seed.SessionService().Create(context.Background(), &session.CreateRequest{
		AppName: "app", UserID: "u", SessionID: "wakewait",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := emitCheckpoint(context.Background(), seed, checkpointPayload{
		Turn:               2,
		ContinuationPrompt: "continue",
		NextWakeAt:         time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("emitCheckpoint: %v", err)
	}

	// 1s deadline: the resume's setup (eventlog scan + agent build)
	// races this timer before the deferred-wake wait is reached; a
	// tight 100ms flaked under full-suite -race load. If setup STILL
	// exceeds the deadline, the target code path was never entered —
	// skip rather than mis-fail (Reason is only stamped by the
	// wait's cancel exit).
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	res, rerr := Resume(ctx,
		resumeBuilder(llm, h, "app", "u"),
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "wakewait"})
	elapsed := time.Since(start)
	if rerr == nil {
		t.Fatalf("expected context error, got nil (res=%+v)", res)
	}
	if res.Reason == "" {
		t.Skipf("setup exceeded the ctx deadline before reaching the deferred-wake wait (elapsed=%v); environment too loaded to exercise the target path", elapsed)
	}
	if res.Reason != StopReasonContextCancelled {
		t.Errorf("Reason = %q, want %q", res.Reason, StopReasonContextCancelled)
	}
	// The wait was reached, so the ctx fired at ~1s and Duration
	// must reflect real elapsed time (the old bug reported
	// ~0/negative) and no more than what this test observed.
	if res.Duration < 300*time.Millisecond {
		t.Errorf("Duration = %v, want >= 300ms (elapsed time since resume start)", res.Duration)
	}
	if res.Duration > elapsed {
		t.Errorf("Duration = %v, want <= observed elapsed %v", res.Duration, elapsed)
	}
}

// countCheckpoints returns the number of checkpoint events recorded
// for (app, user, sessionID).
func countCheckpoints(t *testing.T, h *eventlog.Handle, app, user, sessionID string) int {
	t.Helper()
	var n int
	for entry, err := range h.Stream.Since(context.Background(), 0,
		eventlog.ForSession(app, user, sessionID),
		eventlog.WithAuthorSuffix(checkpointAuthorSuffix)) {
		if err != nil {
			t.Fatalf("Since: %v", err)
		}
		if entry.Event != nil {
			n++
		}
	}
	return n
}

// stealSessionLock overwrites the holder column in agent_run_lock for
// (app, user, sessionID) so the active SessionLock's next heartbeat
// matches zero rows and closes Lost().
func stealSessionLock(t *testing.T, h *eventlog.Handle, app, user, sessionID, newHolder string) {
	t.Helper()
	res := h.DB.WithContext(context.Background()).
		Table("agent_run_lock").
		Where("app_name = ? AND user_id = ? AND session_id = ?", app, user, sessionID).
		Updates(map[string]any{"holder": newHolder, "heartbeat_at": time.Now()})
	if res.Error != nil {
		t.Fatalf("stealSessionLock: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Fatalf("stealSessionLock affected %d rows, want 1", res.RowsAffected)
	}
}

// TestResumeAutonomous_AbortsWhenSessionLockLostMidTurn is the
// end-to-end regression test for #1361: when SessionLock.Lost() fires
// while a resumed turn is in flight, Resume must cancel the turn,
// return eventlog.ErrLeaseLost with StopReasonContextCancelled, and
// skip emitting a final checkpoint so it does not write split-brain
// state into the successor holder's session.
func TestResumeAutonomous_AbortsWhenSessionLockLostMidTurn(t *testing.T) {
	t.Parallel()
	dsn := filepath.Join(t.TempDir(), "session.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dsn),
		eventlog.WithLockHeartbeat(10*time.Millisecond, 100*time.Millisecond))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	defer h.Close()

	// Seed a resumable 1-turn checkpoint.
	seedLLM := &stubLLM{scenarios: []scenarioFn{textTurn("seed", 1, 1)}}
	if _, err := Run(context.Background(),
		runBuilder(seedLLM, h, "app", "u", "stolen-midturn"),
		"go", WithMaxTurns(1)); err != nil {
		t.Fatalf("seed Run: %v", err)
	}
	beforeCheckpoints := countCheckpoints(t, h, "app", "u", "stolen-midturn")

	turnEntered := make(chan struct{})
	blockLLM := &stubLLM{scenarios: []scenarioFn{
		func(ctx context.Context, _ *adkmodel.LLMRequest) []stubResp {
			close(turnEntered)
			<-ctx.Done()
			return []stubResp{{err: ctx.Err()}}
		},
	}}

	type resumeOut struct {
		res RunResult
		err error
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	done := make(chan resumeOut, 1)
	go func() {
		res, rerr := Resume(runCtx,
			resumeBuilder(blockLLM, h, "app", "u"),
			SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "stolen-midturn"},
			WithMaxTurns(5))
		done <- resumeOut{res: res, err: rerr}
	}()

	select {
	case <-turnEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Resume to enter the model turn")
	}

	// Overwrite the lock holder so the next 10ms heartbeat sees 0 rows
	// affected and closes lock.Lost().
	stealSessionLock(t, h, "app", "u", "stolen-midturn", "successor/2/beef")

	var out resumeOut
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Resume did not abort after SessionLock.Lost() fired")
	}

	if !errors.Is(out.err, eventlog.ErrLeaseLost) {
		t.Errorf("Resume err = %v, want errors.Is(..., eventlog.ErrLeaseLost)", out.err)
	}
	if out.res.Reason != StopReasonContextCancelled {
		t.Errorf("Reason = %q, want %q", out.res.Reason, StopReasonContextCancelled)
	}

	// Crucial split-brain invariant: the evicted holder must NOT write
	// a per-turn or final checkpoint after losing the lease.
	afterCheckpoints := countCheckpoints(t, h, "app", "u", "stolen-midturn")
	if afterCheckpoints != beforeCheckpoints {
		t.Errorf("checkpoint count changed from %d to %d after lease loss; evicted Resume must not emit split-brain checkpoints",
			beforeCheckpoints, afterCheckpoints)
	}
}

// TestResumeAutonomous_AbortsWhenSessionLockLostDuringWakeWait verifies
// that losing the session lock while parked on a deferred NextWakeAt
// wait unblocks the wait immediately with eventlog.ErrLeaseLost and
// writes no checkpoint (#1361).
func TestResumeAutonomous_AbortsWhenSessionLockLostDuringWakeWait(t *testing.T) {
	t.Parallel()
	dsn := filepath.Join(t.TempDir(), "session.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dsn),
		eventlog.WithLockHeartbeat(10*time.Millisecond, 100*time.Millisecond))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	defer h.Close()

	llm := &stubLLM{}
	seed, err := resumeBuilder(llm, h, "app", "u")(nil, "stolen-wakewait")
	if err != nil {
		t.Fatalf("build seeding agent: %v", err)
	}
	if _, err := seed.SessionService().Create(context.Background(), &session.CreateRequest{
		AppName: "app", UserID: "u", SessionID: "stolen-wakewait",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := emitCheckpoint(context.Background(), seed, checkpointPayload{
		Turn:               2,
		ContinuationPrompt: "continue",
		NextWakeAt:         time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("emitCheckpoint: %v", err)
	}
	beforeCheckpoints := countCheckpoints(t, h, "app", "u", "stolen-wakewait")

	type resumeOut struct {
		res RunResult
		err error
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	done := make(chan resumeOut, 1)
	go func() {
		res, rerr := Resume(runCtx,
			resumeBuilder(llm, h, "app", "u"),
			SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "stolen-wakewait"})
		done <- resumeOut{res: res, err: rerr}
	}()

	// Wait until Resume has acquired the row in agent_run_lock.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var count int64
		h.DB.WithContext(context.Background()).
			Table("agent_run_lock").
			Where("app_name = ? AND user_id = ? AND session_id = ?", "app", "u", "stolen-wakewait").
			Count(&count)
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Resume to acquire agent_run_lock")
		}
		time.Sleep(5 * time.Millisecond)
	}

	stealSessionLock(t, h, "app", "u", "stolen-wakewait", "successor/2/cafe")

	var out resumeOut
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Resume did not abort from deferred-wake wait after SessionLock.Lost() fired")
	}

	if !errors.Is(out.err, eventlog.ErrLeaseLost) {
		t.Errorf("Resume err = %v, want errors.Is(..., eventlog.ErrLeaseLost)", out.err)
	}
	if out.res.Reason != StopReasonContextCancelled {
		t.Errorf("Reason = %q, want %q", out.res.Reason, StopReasonContextCancelled)
	}
	if after := countCheckpoints(t, h, "app", "u", "stolen-wakewait"); after != beforeCheckpoints {
		t.Errorf("checkpoint count changed from %d to %d after wake-wait lease loss", beforeCheckpoints, after)
	}
}

// TestResumeAutonomous_StallLossStillWritesFinalCheckpointWhenRowKept
// covers the single-replica outage (#1361 review): heartbeats fail past
// the loss window so Lost() fires and the turn is cancelled, but nobody
// took the row. Once the database is back, Resume re-proves ownership
// with Reaffirm and writes the final checkpoint, so the partial turn's
// accounting is not lost; the error is still ErrLeaseLost so the caller
// exits and restarts. Contrast the mid-turn steal test above, where the
// row is someone else's and no checkpoint may be written.
func TestResumeAutonomous_StallLossStillWritesFinalCheckpointWhenRowKept(t *testing.T) {
	t.Parallel()
	dsn := filepath.Join(t.TempDir(), "session.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dsn),
		eventlog.WithLockHeartbeat(10*time.Millisecond, 100*time.Millisecond))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	defer h.Close()

	seedLLM := &stubLLM{scenarios: []scenarioFn{textTurn("seed", 1, 1)}}
	if _, err := Run(context.Background(),
		runBuilder(seedLLM, h, "app", "u", "stall-kept"),
		"go", WithMaxTurns(1)); err != nil {
		t.Fatalf("seed Run: %v", err)
	}
	before := countCheckpoints(t, h, "app", "u", "stall-kept")

	turnEntered := make(chan struct{})
	turnCancelled := make(chan struct{})
	proceed := make(chan struct{})
	blockLLM := &stubLLM{scenarios: []scenarioFn{
		func(ctx context.Context, _ *adkmodel.LLMRequest) []stubResp {
			close(turnEntered)
			<-ctx.Done()
			close(turnCancelled)
			// Hold the turn open until the test has restored the table,
			// so Resume's Reaffirm runs against a healthy database.
			<-proceed
			return []stubResp{{err: ctx.Err()}}
		},
	}}

	type resumeOut struct {
		res RunResult
		err error
	}
	done := make(chan resumeOut, 1)
	go func() {
		res, rerr := Resume(context.Background(),
			resumeBuilder(blockLLM, h, "app", "u"),
			SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "stall-kept"},
			WithMaxTurns(5))
		done <- resumeOut{res: res, err: rerr}
	}()

	select {
	case <-turnEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Resume to enter the model turn")
	}

	// Stall: every heartbeat UPDATE now fails ("no such table"). The
	// row itself is untouched, parked under another name.
	if err := h.DB.Exec("ALTER TABLE agent_run_lock RENAME TO agent_run_lock_stalled").Error; err != nil {
		t.Fatalf("rename away: %v", err)
	}
	select {
	case <-turnCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the lease-bound turn was not cancelled after heartbeats stalled past the loss window")
	}
	// Recovery: the row comes back with our holder on it.
	if err := h.DB.Exec("ALTER TABLE agent_run_lock_stalled RENAME TO agent_run_lock").Error; err != nil {
		t.Fatalf("rename back: %v", err)
	}
	close(proceed)

	var out resumeOut
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Resume did not return after the stalled turn was released")
	}
	if !errors.Is(out.err, eventlog.ErrLeaseLost) {
		t.Errorf("Resume err = %v, want errors.Is(..., eventlog.ErrLeaseLost): a stall still ends the run", out.err)
	}
	if after := countCheckpoints(t, h, "app", "u", "stall-kept"); after != before+1 {
		t.Errorf("checkpoint count %d → %d; a stall with the row still ours must write exactly one final checkpoint", before, after)
	}
}
