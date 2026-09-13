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

package runner

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoopHealth_ObserveTurnCountsTurnsNotErrors(t *testing.T) {
	t.Parallel()
	h := NewLoopHealth("s1")

	// One turn that yielded three errors is ONE failure: WakeLoop
	// latches a single kind per turn and calls observeTurn once.
	if got := h.observeTurn(turnFailed, "unknown"); got != 1 {
		t.Errorf("first failing turn = %d, want 1", got)
	}
	if got := h.observeTurn(turnFailed, "auth_error"); got != 2 {
		t.Errorf("second failing turn = %d, want 2", got)
	}
	st := h.Snapshot()
	if st.State != LoopFailing {
		t.Errorf("State = %q, want %q", st.State, LoopFailing)
	}
	if st.LastErrorKind != "auth_error" {
		t.Errorf("LastErrorKind = %q, want auth_error", st.LastErrorKind)
	}

	// A clean turn resets both the count and the state, so a loop that
	// recovers stops holding the pod out of its Service.
	if got := h.observeTurn(turnClean, ""); got != 0 {
		t.Errorf("clean turn = %d, want 0", got)
	}
	st = h.Snapshot()
	if st.State != LoopIdle {
		t.Errorf("State after recovery = %q, want %q", st.State, LoopIdle)
	}
	if st.LastErrorKind != "" {
		t.Errorf("LastErrorKind after recovery = %q, want empty", st.LastErrorKind)
	}
}

// An operator pressing stop, or a guardrail halting the session, tells
// us nothing about whether the fault is still there. Reading it as a
// clean turn would let an interrupt during a real outage silently reset
// the count and turn the aggregate green at the worst moment.
func TestLoopHealth_AnObeyedStopIsNotARecovery(t *testing.T) {
	t.Parallel()
	h := NewLoopHealth("s1")
	for range 3 {
		h.observeTurn(turnFailed, "auth_error")
	}

	h.setState(LoopWorking)
	if got := h.observeTurn(turnObeyed, ""); got != 3 {
		t.Errorf("after an obeyed stop = %d failures, want the 3 still standing", got)
	}
	st := h.Snapshot()
	if st.State != LoopFailing {
		t.Errorf("State = %q, want %q — the loop is parked, not working", st.State, LoopFailing)
	}
	if st.LastErrorKind != "auth_error" {
		t.Errorf("LastErrorKind = %q, want the fault that is still standing", st.LastErrorKind)
	}

	// It is not an extra failure either, and it must not leave the loop
	// reading as mid-turn once the turn is over.
	h.setState(LoopWorking)
	h.observeTurn(turnClean, "")
	if st := h.Snapshot(); st.State != LoopIdle || st.ConsecutiveFailures != 0 {
		t.Errorf("after a clean turn State=%q failures=%d, want idle/0", st.State, st.ConsecutiveFailures)
	}
}

// Since answers "failing since when", not "most recently observed to be
// failing" — a monitor that reads it wants the start of the outage.
func TestLoopHealth_SinceStampsOnlyOnTransition(t *testing.T) {
	t.Parallel()
	h := NewLoopHealth("s1")
	h.observeTurn(turnFailed, "rate_limited")
	first := h.Snapshot().Since

	time.Sleep(2 * time.Millisecond)
	h.observeTurn(turnFailed, "rate_limited")
	if got := h.Snapshot().Since; !got.Equal(first) {
		t.Errorf("Since moved on a repeat failure: %s -> %s", first, got)
	}

	h.setState(LoopWorking)
	if got := h.Snapshot().Since; got.Equal(first) {
		t.Errorf("Since did not move on a real transition (still %s)", got)
	}
}

// A partial outage keeps the aggregate green. Readiness is a routing
// decision: one session with revoked RBAC must not pull a pod serving
// healthy ones out of its Service.
func TestLoopHealthSet_ErrOnlyWhenEveryLoopIsFailing(t *testing.T) {
	t.Parallel()
	var set LoopHealthSet

	if err := set.Err(); err != nil {
		t.Errorf("empty set Err() = %v, want nil", err)
	}

	a, _ := set.Register("alpha")
	b, unregisterB := set.Register("bravo")
	// Both have to have run a turn to be counted at all; see
	// TestLoopHealthSet_ALoopThatNeverRanDoesNotMaskAnOutage.
	a.observeTurn(turnClean, "")
	b.observeTurn(turnClean, "")

	for range unhealthyAfterFailures {
		a.observeTurn(turnFailed, "auth_error")
	}
	if err := set.Err(); err != nil {
		t.Errorf("one failing of two Err() = %v, want nil", err)
	}

	for range unhealthyAfterFailures {
		b.observeTurn(turnFailed, "auth_error")
	}
	err := set.Err()
	if err == nil {
		t.Fatal("all failing Err() = nil, want an error")
	}
	for _, want := range []string{"alpha", "bravo", "auth_error"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Err() = %q, want it to mention %q", err, want)
		}
	}

	// The detail is for the daemon log, so it has to survive a loop
	// leaving: an unregistered loop must stop contributing.
	unregisterB()
	if got := set.Err(); got == nil || strings.Contains(got.Error(), "bravo") {
		t.Errorf("Err() after unregister = %v, want an alpha-only error", got)
	}

	a.observeTurn(turnClean, "")
	if err := set.Err(); err != nil {
		t.Errorf("Err() after the last loop recovered = %v, want nil", err)
	}
}

// One bad turn is not an outage. #935's evidence is that a provider 429
// kills a turn roughly every six minutes under load, and descheduling a
// pod for one of those would teach operators to ignore the gate.
func TestLoopHealthSet_ErrTakesMoreThanOneBadTurn(t *testing.T) {
	t.Parallel()
	var set LoopHealthSet
	h, _ := set.Register("alpha")
	for i := 1; i < unhealthyAfterFailures; i++ {
		h.observeTurn(turnFailed, "rate_limited")
		if err := set.Err(); err != nil {
			t.Fatalf("Err() after %d consecutive failures = %v, want nil", i, err)
		}
	}
	h.observeTurn(turnFailed, "rate_limited")
	if err := set.Err(); err == nil {
		t.Errorf("Err() after %d consecutive failures = nil, want an error", unhealthyAfterFailures)
	}
}

// A loop mid-retry reads as LoopWorking. The aggregate keys off the
// failure count, not the state, so a probe landing inside that turn
// does not see a broken session as recovered.
func TestLoopHealthSet_AFailingLoopMidTurnStillCounts(t *testing.T) {
	t.Parallel()
	var set LoopHealthSet
	h, _ := set.Register("alpha")
	for range unhealthyAfterFailures {
		h.observeTurn(turnFailed, "auth_error")
	}
	h.setState(LoopWorking)
	if err := set.Err(); err == nil {
		t.Error("Err() = nil while a session with three failures is mid-retry")
	}
}

// A stopped loop is skipped, not counted as healthy: WakeLoop's
// deferred setState(LoopStopped) runs before the unregister in a
// multi-session host. Skipping it both keeps a shutting-down daemon
// from flashing unhealthy and stops a lingering dead loop from masking
// a genuinely all-failing one.
func TestLoopHealthSet_StoppedLoopIsSkipped(t *testing.T) {
	t.Parallel()
	var set LoopHealthSet
	a, _ := set.Register("alpha")
	for range unhealthyAfterFailures {
		a.observeTurn(turnFailed, "auth_error")
	}
	a.setState(LoopStopped)
	if err := set.Err(); err != nil {
		t.Errorf("Err() with one stopped loop = %v, want nil", err)
	}

	b, _ := set.Register("bravo")
	for range unhealthyAfterFailures {
		b.observeTurn(turnFailed, "auth_error")
	}
	got := set.Err()
	if got == nil {
		t.Fatal("Err() = nil; a stopped loop masked the one live failing loop")
	}
	if strings.Contains(got.Error(), "alpha") {
		t.Errorf("Err() = %q, want the stopped loop left out", got)
	}
}

// A loop that has never run a turn is not evidence of health. Every
// bundled GKE recipe is a multi-session `--no-repl` daemon, which
// starts a bootstrap `default` session nothing ever drives — counting
// it as healthy would make "every live loop is failing" unreachable and
// silently disable the check on the exact deployment it exists for.
func TestLoopHealthSet_ALoopThatNeverRanDoesNotMaskAnOutage(t *testing.T) {
	t.Parallel()
	var set LoopHealthSet
	set.Register("default") // the bootstrap session, never driven
	worker, _ := set.Register("s-live")
	for range unhealthyAfterFailures {
		worker.observeTurn(turnFailed, "auth_error")
	}
	got := set.Err()
	if got == nil {
		t.Fatal("Err() = nil; an undriven bootstrap session masked the outage")
	}
	if strings.Contains(got.Error(), "default") {
		t.Errorf("Err() = %q, want the undriven session left out", got)
	}

	// The same rule keeps a daemon that has simply not done anything
	// yet from reporting itself broken.
	var fresh LoopHealthSet
	fresh.Register("s-new")
	if err := fresh.Err(); err != nil {
		t.Errorf("Err() on a daemon that has run no turns = %v, want nil", err)
	}
}

func TestLoopHealthSet_SnapshotIsOrderedBySession(t *testing.T) {
	t.Parallel()
	var set LoopHealthSet
	for _, sid := range []string{"charlie", "alpha", "bravo"} {
		set.Register(sid)
	}
	got := set.Snapshot()
	var names []string
	for _, st := range got {
		names = append(names, st.Session)
	}
	if strings.Join(names, ",") != "alpha,bravo,charlie" {
		t.Errorf("Snapshot order = %v, want alpha,bravo,charlie", names)
	}
}

// Every method is nil-safe so a host that wants no health reporting
// leaves the fields unset rather than wiring a sink.
func TestLoopHealth_NilReceiversAreInert(t *testing.T) {
	t.Parallel()
	var h *LoopHealth
	var set *LoopHealthSet
	h.setState(LoopFailing)
	h.setSession("s")
	if got := h.observeTurn(turnFailed, "auth_error"); got != 0 {
		t.Errorf("nil observeTurn = %d, want 0", got)
	}
	if got := h.Snapshot(); got != (LoopStatus{}) {
		t.Errorf("nil Snapshot = %+v, want zero", got)
	}
	if got := set.Snapshot(); got != nil {
		t.Errorf("nil set Snapshot = %v, want nil", got)
	}
	if err := set.Err(); err != nil {
		t.Errorf("nil set Err = %v, want nil", err)
	}
	nh, unregister := set.Register("s")
	if nh == nil {
		t.Fatal("nil set Register returned a nil health handle")
	}
	unregister()
}

func TestLoopHealth_ConcurrentReadsAndWrites(t *testing.T) {
	t.Parallel()
	var set LoopHealthSet
	h, _ := set.Register("alpha")
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				h.observeTurn(turnFailed, "auth_error")
				h.setState(LoopWorking)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 400 {
			_ = set.Err()
			_ = set.Snapshot()
		}
	}()
	wg.Wait()
}
