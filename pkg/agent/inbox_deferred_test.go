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
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// newDeferredTestAgent returns an agent over the echo mock, whose
// responses repeat the prompt verbatim — which is how these tests see
// whether a queued message reached the model.
func newDeferredTestAgent(t *testing.T) *Agent {
	t.Helper()
	prov := mock.NewEcho()
	llm, err := prov.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("provider.Model: %v", err)
	}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// wakePending reports whether a wake is currently signalled, consuming
// it if so.
func wakePending(a *Agent) bool {
	select {
	case <-a.WakeRequested():
		return true
	default:
		return false
	}
}

// The whole point: queued, not woken.
func TestQueueAsContext_QueuesWithoutWaking(t *testing.T) {
	t.Parallel()
	a := newDeferredTestAgent(t)

	if err := a.QueueAsContext(context.Background(), "node pool drained", auth.Caller{Identity: "watcher"}); err != nil {
		t.Fatalf("QueueAsContext: %v", err)
	}
	if wakePending(a) {
		t.Error("QueueAsContext fired the wake signal; the message must not preempt a sleep")
	}
	// The harness-side door has to stay shut too — InboxArrived is the
	// documented "start a turn now" signal for library consumers, so a
	// message that skipped the wake must not drive a turn through it.
	select {
	case <-a.InboxArrived():
		t.Error("QueueAsContext fired InboxArrived; a deferred message must not drive a turn")
	default:
	}
	if n := a.PendingInboxCount(); n != 1 {
		t.Fatalf("PendingInboxCount = %d, want the message queued anyway", n)
	}
}

// Deferred is about timing, not delivery: the next turn — whenever it
// comes and whatever causes it — must still see the message.
func TestQueueAsContext_DrainsIntoTheNextTurn(t *testing.T) {
	t.Parallel()
	a := newDeferredTestAgent(t)

	if err := a.QueueAsContext(context.Background(), "second alert corroborates the first", auth.Caller{}); err != nil {
		t.Fatalf("QueueAsContext: %v", err)
	}
	var saw string
	for ev, err := range a.Run(context.Background(), "status?") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.Text != "" && !ev.Partial {
				saw += p.Text
			}
		}
	}
	if !strings.Contains(saw, "second alert corroborates the first") {
		t.Errorf("the next turn did not see the deferred message; got %q", saw)
	}
	if !strings.Contains(saw, "[Inbox]") {
		t.Errorf("deferred messages should arrive in the ordinary [Inbox] block; got %q", saw)
	}
}

// Several deferred messages between turns are the case the feature
// exists for: they drain together as ONE block instead of each driving
// its own turn.
func TestQueueAsContext_MultipleDrainAsOneBlock(t *testing.T) {
	t.Parallel()
	a := newDeferredTestAgent(t)

	for _, m := range []string{"alert one", "alert two", "alert three"} {
		if err := a.QueueAsContext(context.Background(), m, auth.Caller{}); err != nil {
			t.Fatalf("QueueAsContext %q: %v", m, err)
		}
	}
	if wakePending(a) {
		t.Error("three deferred messages fired a wake between them")
	}
	drained := a.DrainInbox()
	if len(drained) != 3 {
		t.Fatalf("DrainInbox returned %d messages, want all 3 batched", len(drained))
	}
}

// Neither delivery un-parks a loop an operator deliberately parked.
//
// Deferring never did; injecting stopped in #878, when it turned out
// that "an operator is answering the what-now prompt" was being
// inferred from "a message arrived" and the two are not the same thing
// on a door machines also use.
//
// What still separates them against a PARKED agent is nothing at all,
// which is worth pinning: the axis QueueAsContext owns is the wake
// (#698), and a parked loop isn't sleeping. The difference shows up on
// a sleeping agent, which TestQueueAsContext_DoesNotWake covers.
func TestQueueAsContext_DoesNotReleaseAPauseHold(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		file func(a *Agent) error
	}{
		{"deferred", func(a *Agent) error {
			return a.QueueAsContext(context.Background(), "fyi", auth.Caller{Identity: "watcher"})
		}},
		{"ordinary inject", func(a *Agent) error {
			return a.InjectAs("do this instead", auth.Caller{Identity: "alice"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newDeferredTestAgent(t)

			if !a.Pause("operator parked the loop") {
				t.Fatal("Pause did not park the loop")
			}
			if err := tc.file(a); err != nil {
				t.Fatalf("file message: %v", err)
			}
			if !a.Paused() {
				t.Error("a queued message resumed a paused loop; only a resume may open the gate (#878)")
			}
			if got := len(a.DrainInbox()); got != 1 {
				t.Errorf("drained %d messages, want 1 (holding the gate must not drop the message)", got)
			}
		})
	}
}

// auto-continue stands down when operator input is already queued,
// because that input will drive the next turn itself. A deferred
// message will not, so counting it would leave an interrupted session
// with nothing to re-drive it AND the deferred context undrained.
func TestQueueAsContext_IsNotPendingOperatorInput(t *testing.T) {
	t.Parallel()
	a := newDeferredTestAgent(t)

	if err := a.QueueAsContext(context.Background(), "fyi", auth.Caller{Identity: "watcher"}); err != nil {
		t.Fatalf("QueueAsContext: %v", err)
	}
	if a.HasPendingOperatorInput() {
		t.Error("a deferred message counted as pending operator input; auto-continue would stand down for a turn that is never coming")
	}
	// Same identity, ordinary inject: that one IS input waiting to drive.
	if err := a.InjectAs("stop", auth.Caller{Identity: "watcher"}); err != nil {
		t.Fatalf("InjectAs: %v", err)
	}
	if !a.HasPendingOperatorInput() {
		t.Error("an ordinary inject must still count as pending operator input")
	}
}

// The turn originator stamps eventlog attribution and selects the
// per-caller MCP credentials, so it has to be whoever ASKED. A watcher
// filing context in the seconds between an operator's inject and the
// turn it caused must not take the turn over.
func TestQueueAsContext_DoesNotStealTheTurnOriginator(t *testing.T) {
	t.Parallel()
	a := newDeferredTestAgent(t)

	if err := a.InjectAs("restart the deployment", auth.Caller{Identity: "alice@example.com"}); err != nil {
		t.Fatalf("InjectAs: %v", err)
	}
	// Arrives LAST, and would win under a plain last-caller-wins rule.
	if err := a.QueueAsContext(context.Background(), "node pool drained", auth.Caller{Identity: "watcher"}); err != nil {
		t.Fatalf("QueueAsContext: %v", err)
	}
	if got := a.drainInboxFull().originator.Identity; got != "alice@example.com" {
		t.Errorf("turn originator = %q, want the operator who actually asked", got)
	}
}

// A batch with nothing but deferred messages still attributes to the
// last of them: there, the machine producer is the only ask there is.
func TestQueueAsContext_AllDeferredBatchKeepsItsIdentity(t *testing.T) {
	t.Parallel()
	a := newDeferredTestAgent(t)

	for _, id := range []string{"watcher-a", "watcher-b"} {
		if err := a.QueueAsContext(context.Background(), "signal", auth.Caller{Identity: id}); err != nil {
			t.Fatalf("QueueAsContext %s: %v", id, err)
		}
	}
	if got := a.drainInboxFull().originator.Identity; got != "watcher-b" {
		t.Errorf("turn originator = %q, want the last deferred caller when nothing woke", got)
	}
}

func TestQueueAsContext_NilReceiverAndClosedInbox(t *testing.T) {
	t.Parallel()
	var nilAgent *Agent
	if err := nilAgent.QueueAsContext(context.Background(), "x", auth.Caller{}); err == nil {
		t.Error("QueueAsContext on a nil agent returned no error")
	}
	a := newDeferredTestAgent(t)
	a.CloseInbox()
	if err := a.QueueAsContext(context.Background(), "x", auth.Caller{}); err == nil {
		t.Error("QueueAsContext into a closed inbox must fail like Inject, not be acknowledged and lost")
	}
}
