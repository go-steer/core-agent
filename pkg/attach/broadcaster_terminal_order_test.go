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

package attach

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// subscribeForFrames opens an /events stream against the test server
// and returns its parsed frame channel, having already drained the
// boot frames (capabilities + status snapshot) so the caller sees only
// what happens next.
func subscribeForFrames(t *testing.T, ctx context.Context, base, sid string) <-chan sseFrame {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/sessions/core-agent/"+sid+"/events", nil)
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the ctx cancel the caller owns
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subscribe status %d", resp.StatusCode)
	}
	frames := readSSEFrames(t, resp.Body)
	// capabilities is required first; the status snapshot follows it.
	if f := mustReadFrame(t, frames, 2*time.Second, "capabilities"); f.Event != EventCapabilities {
		t.Fatalf("first frame = %q, want %q", f.Event, EventCapabilities)
	}
	if f := mustReadFrame(t, frames, 2*time.Second, "status snapshot"); f.Event != EventStatusUpdate {
		t.Fatalf("second frame = %q, want %q", f.Event, EventStatusUpdate)
	}
	return frames
}

// TestEvents_TerminalFrameFollowsTheTurnsLastEvent pins #864.
//
// `agent` frames and typed frames reach a subscriber by two different
// routes: the former are written to the eventlog and fanned out when
// the pump's Watch next polls, the latter go straight into every
// subscriber's channel. The turn emits its terminal frame as soon as
// the ADK stream drains, which is not the same instant the pump has
// caught up — so turn-complete could overtake the turn's own final
// text, and a consumer that finalizes its render on the terminal frame
// dropped the agent's answer.
//
// The test reproduces the exact shape: append the turn's last event,
// then immediately emit turn-complete, and require the subscriber to
// see them in that order. On pre-fix code the terminal frame wins by
// most of a watch-poll interval.
func TestEvents_TerminalFrameFollowsTheTurnsLastEvent(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &operatorEventTargetStub{
		eventfulRegistrant: eventfulRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "order"},
			handle:         h,
		},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	// One prior event so the session exists and the subscriber joins
	// against a non-empty log, as a real attach does.
	seedTestEvents(t, h, "core-agent", "u", "order", 1)

	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	frames := subscribeForFrames(t, ctx, base, "order")

	// The turn's final model text lands in the eventlog, and the turn's
	// cleanup emits its terminal frame right behind it — no sleep in
	// between, which is the whole point.
	//
	// The database is fresh per test and holds one session, so the seed
	// event is seq 1 and this one — the frame that must not be
	// overtaken — is seq 2.
	const finalSeq = 2
	appendMoreTestEvents(t, h, "core-agent", "u", "order", 1, 1)
	ag.fire(EventTurnComplete, TurnComplete{PromptID: "p-1", Model: "test-model"})

	done, prior := awaitFrame(t, frames, 10*time.Second, func(f sseFrame) bool {
		return f.Event == EventTurnComplete
	})
	if done.Event != EventTurnComplete {
		t.Fatalf("turn-complete never arrived (saw %d frames: %v)", len(prior), frameTypes(prior))
	}
	if !containsAgentSeq(t, prior, finalSeq) {
		t.Errorf("turn-complete arrived before the turn's last `agent` frame (seq %d); frames ahead of it were %v.\n"+
			"A consumer that finalizes its render here has nowhere to put the text that follows (#864).",
			finalSeq, frameTypes(prior))
	}
}

// TestEvents_TerminalBarrierDegradesWhenThePumpCannotCatchUp checks the
// barrier's failure mode is the OLD behavior and not a hung turn: with
// the wait shortened below the pump's poll interval, so the log's head
// is out of reach, Emit must still deliver — late order and all.
func TestEvents_TerminalBarrierDegradesWhenThePumpCannotCatchUp(t *testing.T) {
	old := terminalBarrierTimeout
	terminalBarrierTimeout = 50 * time.Millisecond
	defer func() { terminalBarrierTimeout = old }()

	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	reg := NewSessionRegistry()
	ag := &operatorEventTargetStub{
		eventfulRegistrant: eventfulRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "wedged"},
			handle:         h,
		},
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	seedTestEvents(t, h, "core-agent", "u", "wedged", 1)

	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	frames := subscribeForFrames(t, ctx, base, "wedged")

	appendMoreTestEvents(t, h, "core-agent", "u", "wedged", 1, 1)

	// 50ms is well under the eventlog's watch-poll interval, so the
	// pump cannot possibly have delivered the row we just wrote.
	start := time.Now()
	ag.fire(EventTurnComplete, TurnComplete{PromptID: "p-1"})
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("Emit blocked for %s — the barrier must bound its wait, not hold the turn", waited)
	}

	done, _ := awaitFrame(t, frames, 10*time.Second, func(f sseFrame) bool {
		return f.Event == EventTurnComplete
	})
	if done.Event != EventTurnComplete {
		t.Fatal("turn-complete was never delivered after the barrier gave up; degradation must mean unordered, not dropped")
	}
}

// TestEvents_TerminalBarrierSkipsWhenNobodyIsAttached guards the cheap
// path: with no subscribers there is no ordering to protect, so Emit
// must not pay a head query or a wait for an audience of nobody.
func TestEvents_TerminalBarrierSkipsWhenNobodyIsAttached(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()

	ag := &eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "quiet"},
		handle:         h,
	}
	seedTestEvents(t, h, "core-agent", "u", "quiet", 1)

	b, err := newBroadcaster(&Entry{
		AppName: "core-agent", UserID: "u", SessionID: "quiet", Agent: ag,
	})
	if err != nil {
		t.Fatalf("newBroadcaster: %v", err)
	}
	defer b.Close()

	start := time.Now()
	b.Emit(EventTurnComplete, TurnComplete{PromptID: "p-1"})
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("Emit with no subscribers took %s; the barrier should be skipped entirely", waited)
	}
}

func frameTypes(frames []sseFrame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Event)
	}
	return out
}

// containsAgentSeq reports whether frames include the legacy `agent`
// frame for exactly seq want. Deliberately seq-specific: a subscriber
// replays the session's earlier events on connect, so "some agent
// frame arrived" is satisfied by history and would pass against the
// unordered delivery this test exists to catch.
func containsAgentSeq(t *testing.T, frames []sseFrame, want int64) bool {
	t.Helper()
	for _, f := range frames {
		if f.Event != EventAgent {
			continue
		}
		var fr struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal([]byte(f.Data), &fr); err != nil {
			t.Fatalf("agent frame JSON: %v (data=%s)", err, f.Data)
		}
		if fr.Seq == want {
			return true
		}
	}
	return false
}
