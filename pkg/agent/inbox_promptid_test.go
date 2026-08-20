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

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// The prompt_id an inject returns has to be THE id — the one stamped
// on the queued message and published on the `inbox`/queued event —
// not a second one minted for the caller. An id that names nothing on
// the wire is worse than no id: the client keys state on it and waits
// for a turn that can never mention it (#840).

// captureInboxIDs wires an operator emitter that records the PromptID
// of every `inbox` event, and returns the accessor.
func captureInboxIDs(a *Agent) func() []string {
	var ids []string
	a.SetOperatorEventEmitter(func(eventType string, payload any) {
		if eventType != attach.EventInbox {
			return
		}
		ev, ok := payload.(attach.InboxEvent)
		if !ok {
			return
		}
		ids = append(ids, ev.PromptID)
	})
	return func() []string { return ids }
}

func TestInjectWithID_ReturnsTheIDOnTheQueuedMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Agent) (string, error)
	}{
		{"waking", func(a *Agent) (string, error) {
			return a.InjectAsContextWithID(context.Background(), "hello", auth.Caller{Identity: "op"})
		}},
		{"deferred", func(a *Agent) (string, error) {
			return a.QueueAsContextWithID(context.Background(), "hello", auth.Caller{Identity: "op"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &Agent{inbox: newInbox()}
			events := captureInboxIDs(a)

			id, err := tc.call(a)
			if err != nil {
				t.Fatalf("inject: %v", err)
			}
			if id == "" {
				t.Fatal("returned prompt id is empty")
			}
			msgs := a.inbox.drain()
			if len(msgs) != 1 {
				t.Fatalf("drained %d messages, want 1", len(msgs))
			}
			if msgs[0].id != id {
				t.Errorf("returned id = %q, queued id = %q; they must be the same handle", id, msgs[0].id)
			}
			if got := events(); len(got) != 1 || got[0] != id {
				t.Errorf("`inbox` event ids = %v, want exactly [%q]", got, id)
			}
		})
	}
}

// The id-returning entry points must be the SAME operation as the ones
// they shadow, not a third delivery mode. QueueAsContextWithID in
// particular must keep not waking — an id is not worth a preemption.
func TestInjectWithID_KeepsTheDeliverySemantics(t *testing.T) {
	t.Parallel()
	a := &Agent{inbox: newInbox()}
	if _, err := a.QueueAsContextWithID(context.Background(), "fyi", auth.Caller{}); err != nil {
		t.Fatalf("QueueAsContextWithID: %v", err)
	}
	select {
	case <-a.InboxArrived():
		t.Fatal("QueueAsContextWithID fired InboxArrived; the deferred path must not drive a turn")
	default:
	}

	b := &Agent{inbox: newInbox()}
	if _, err := b.InjectAsContextWithID(context.Background(), "act on this", auth.Caller{}); err != nil {
		t.Fatalf("InjectAsContextWithID: %v", err)
	}
	select {
	case <-b.InboxArrived():
	default:
		t.Error("InjectAsContextWithID did not fire InboxArrived; the waking path must")
	}
}

// The error paths return no id, so a caller can't mistake a
// zero-value string for a real handle on a message that was never
// queued.
func TestInjectWithID_ReturnsNoIDOnFailure(t *testing.T) {
	t.Parallel()
	a := &Agent{inbox: newInbox()}
	a.inbox.close()

	id, err := a.InjectAsContextWithID(context.Background(), "hello", auth.Caller{})
	if err == nil {
		t.Fatal("inject into a closed inbox succeeded, want ErrInboxClosed")
	}
	if id != "" {
		t.Errorf("prompt id = %q on a failed inject, want empty", id)
	}
}
