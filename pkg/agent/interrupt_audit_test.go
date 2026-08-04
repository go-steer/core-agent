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
	"testing"

	"google.golang.org/adk/session"
)

// countInterruptAuditRows returns how many operator-interrupt audit rows
// (Author == interruptAuditAuthor) are present in the agent's session.
func countInterruptAuditRows(t *testing.T, a *Agent) int {
	t.Helper()
	resp, err := a.eventLog.Service.Get(context.Background(), &session.GetRequest{
		AppName:   a.appName,
		UserID:    a.userID,
		SessionID: a.sessionID,
	})
	if err != nil {
		t.Fatalf("session Get: %v", err)
	}
	n := 0
	for ev := range resp.Session.Events().All() {
		if ev.Author == interruptAuditAuthor {
			n++
		}
	}
	return n
}

func runTurnToCompletion(t *testing.T, a *Agent) {
	t.Helper()
	for _, err := range a.Run(context.Background(), "hi") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
}

// TestAgent_InterruptAudit_DeferredToTurnCleanup is the #565 regression.
//
// The operator-interrupt audit row must NOT be written out-of-band the
// moment the interrupt is marked (that's what raced the runner's
// in-flight session handle and got mislabeled as a stale-session error).
// It must be written from the post-turn cleanup, after the interrupted
// turn has fully unwound — exactly once, no double-write on the next turn.
//
// Fails-first proof: neuter drainInterruptAudit to a no-op (or move the
// append into MarkInterruptPending) and one of the assertions below trips.
func TestAgent_InterruptAudit_DeferredToTurnCleanup(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	a, err := New(oneShotLLM{}, WithEventLog(h), WithSession("u", "s-565"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	if _, err := h.Service.Create(context.Background(), &session.CreateRequest{
		AppName: a.appName, UserID: "u", SessionID: "s-565",
	}); err != nil {
		t.Fatalf("session Create: %v", err)
	}

	if got := countInterruptAuditRows(t, a); got != 0 {
		t.Fatalf("audit rows before any interrupt = %d, want 0", got)
	}

	// Mark an operator interrupt. The row must NOT appear yet — the fix
	// defers the write to the turn loop. A pre-fix out-of-band append
	// would already have written it here, tripping this assertion.
	a.MarkInterruptPending()
	if got := countInterruptAuditRows(t, a); got != 0 {
		t.Fatalf("audit rows after MarkInterruptPending (before any turn) = %d, want 0 (write must be deferred to turn cleanup)", got)
	}

	// The next turn's cleanup drains the pending audit.
	runTurnToCompletion(t, a)
	if got := countInterruptAuditRows(t, a); got != 1 {
		t.Fatalf("audit rows after one turn = %d, want 1 (turn cleanup must drain the pending audit)", got)
	}

	// A subsequent turn must not re-write it — the flag is cleared.
	runTurnToCompletion(t, a)
	if got := countInterruptAuditRows(t, a); got != 1 {
		t.Fatalf("audit rows after second turn = %d, want 1 (drain must not double-write)", got)
	}
}

// TestAgent_InterruptAudit_NoopWhenNotPending guards against spurious
// audit rows: an ordinary turn with no operator interrupt must never
// emit one.
func TestAgent_InterruptAudit_NoopWhenNotPending(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	a, err := New(oneShotLLM{}, WithEventLog(h), WithSession("u", "s-noaudit"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	if _, err := h.Service.Create(context.Background(), &session.CreateRequest{
		AppName: a.appName, UserID: "u", SessionID: "s-noaudit",
	}); err != nil {
		t.Fatalf("session Create: %v", err)
	}

	runTurnToCompletion(t, a)
	if got := countInterruptAuditRows(t, a); got != 0 {
		t.Errorf("audit rows after a turn with no interrupt = %d, want 0", got)
	}
}

// TestAgent_MarkInterruptPending_NilSafe mirrors the nil-receiver
// tolerance the other Agent methods carry.
func TestAgent_MarkInterruptPending_NilSafe(t *testing.T) {
	t.Parallel()
	var a *Agent
	a.MarkInterruptPending() // must not panic
	a.drainInterruptAudit()  // must not panic (nil receiver + nil eventlog)
}
