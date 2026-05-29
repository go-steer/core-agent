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
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/pkg/eventlog"
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
	return func(extras []tool.Tool, sessionID string) (*Agent, error) {
		return New(llm,
			WithAppName(app),
			WithSession(user, sessionID),
			WithEventLog(h),
			WithTools(extras),
			WithInstruction("test agent"),
		)
	}
}

// runBuilder mirrors resumeBuilder for the fresh-run case.
func runBuilder(llm *stubLLM, h *eventlog.Handle, app, user, sess string) func(extras []tool.Tool) (*Agent, error) {
	return func(extras []tool.Tool) (*Agent, error) {
		return New(llm,
			WithAppName(app),
			WithSession(user, sess),
			WithEventLog(h),
			WithTools(extras),
			WithInstruction("test agent"),
		)
	}
}

func TestResumeAutonomous_RequiresBuild(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	_, err := ResumeAutonomous(context.Background(), nil, SessionRef{
		Handle: h, AppName: "app", UserID: "u", SessionID: "s",
	})
	if err == nil || !strings.Contains(err.Error(), "build is required") {
		t.Errorf("expected build-required error, got %v", err)
	}
}

func TestResumeAutonomous_RequiresHandle(t *testing.T) {
	t.Parallel()
	_, err := ResumeAutonomous(context.Background(),
		func([]tool.Tool, string) (*Agent, error) { return nil, nil },
		SessionRef{AppName: "app", UserID: "u", SessionID: "s"})
	if err == nil || !strings.Contains(err.Error(), "Handle is required") {
		t.Errorf("expected Handle-required error, got %v", err)
	}
}

func TestResumeAutonomous_RequiresSessionID(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	_, err := ResumeAutonomous(context.Background(),
		func([]tool.Tool, string) (*Agent, error) { return nil, nil },
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
	res, err := RunAutonomous(context.Background(),
		runBuilder(llm, h, "app", "u", "checkpoints"),
		"go")
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
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
	first, err := RunAutonomous(context.Background(),
		runBuilder(llm, h, "app", "u", "terminal"),
		"go")
	if err != nil {
		t.Fatalf("RunAutonomous: %v", err)
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
	res, err := ResumeAutonomous(context.Background(),
		resumeBuilder(llm2, h, "app", "u"),
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "terminal"})
	if err != nil {
		t.Fatalf("ResumeAutonomous: %v", err)
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

func TestResumeAutonomous_ContinuesFromMidRun(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	// First run: 2 turns with no done, capped at 2 turns.
	llm1 := &stubLLM{scenarios: []scenarioFn{
		textTurn("first", 7, 3),
		textTurn("second", 7, 3),
	}}
	first, err := RunAutonomous(context.Background(),
		runBuilder(llm1, h, "app", "u", "midrun"),
		"go", WithMaxTurns(2))
	if err != nil {
		t.Fatalf("first RunAutonomous: %v", err)
	}
	if first.Reason != StopReasonMaxTurns {
		t.Fatalf("first Reason = %q, want %q", first.Reason, StopReasonMaxTurns)
	}

	// Resume with more turns budget. The next turn calls done.
	llm2 := &stubLLM{scenarios: []scenarioFn{
		doneCallTurn("done on resume"),
		textTurn("ok", 1, 1),
	}}
	res, err := ResumeAutonomous(context.Background(),
		resumeBuilder(llm2, h, "app", "u"),
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "midrun"},
		WithMaxTurns(10))
	if err != nil {
		t.Fatalf("ResumeAutonomous: %v", err)
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
	if _, err := RunAutonomous(context.Background(),
		runBuilder(llm1, h, "app", "u", "budgets"),
		"go", WithMaxTurns(3)); err != nil {
		t.Fatalf("first RunAutonomous: %v", err)
	}

	// Resume with the same max_turns budget — already exceeded,
	// should stop immediately on pre-turn check without running a
	// new turn.
	llm2 := &stubLLM{scenarios: []scenarioFn{
		textTurn("never", 0, 0),
	}}
	res, err := ResumeAutonomous(context.Background(),
		resumeBuilder(llm2, h, "app", "u"),
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "budgets"},
		WithMaxTurns(3))
	if err != nil {
		t.Fatalf("ResumeAutonomous: %v", err)
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
	res, err := ResumeAutonomous(context.Background(),
		resumeBuilder(llm, h, "app", "u"),
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "fresh"})
	if err != nil {
		t.Fatalf("ResumeAutonomous: %v", err)
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
	// process running ResumeAutonomous concurrently.
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("seed", 0, 0),
	}}
	if _, err := RunAutonomous(context.Background(),
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

	// ResumeAutonomous must refuse with a clear error.
	_, err = ResumeAutonomous(context.Background(),
		resumeBuilder(llm, h, "app", "u"),
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "locked"})
	if !errors.Is(err, eventlog.ErrSessionLocked) {
		t.Errorf("err = %v, want eventlog.ErrSessionLocked", err)
	}
}
