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

import "testing"

// TestSubscriberSend_DedupesByLastSent pins the fix for the bug
// where pump + replayThenTail both delivered the catch-up range
// (since..currentMax] to every subscriber, causing every event to
// reach downstream consumers twice. The first send for a seq wins;
// the other goroutine's attempt is a no-op skip.
func TestSubscriberSend_DedupesByLastSent(t *testing.T) {
	t.Parallel()
	b := &Broadcaster{
		entry: &Entry{AppName: "core-agent", SessionID: "test"},
	}
	sub := &subscriber{
		ch:       make(chan Frame, 16),
		since:    0,
		lastSent: 0,
	}

	// First delivery of seq=1: accepted.
	if !b.send(sub, Frame{Seq: 1}) {
		t.Fatal("first send(seq=1) returned false")
	}
	// Duplicate delivery of seq=1: skipped (returns true to indicate
	// the source goroutine should keep going, but nothing reaches
	// the channel).
	if !b.send(sub, Frame{Seq: 1}) {
		t.Fatal("duplicate send(seq=1) returned false")
	}
	// Forward delivery: accepted.
	if !b.send(sub, Frame{Seq: 2}) {
		t.Fatal("send(seq=2) returned false")
	}

	close(sub.ch)
	var got []int64
	for f := range sub.ch {
		got = append(got, f.Seq)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("subscriber received %v, want [1 2] (no duplicate of seq=1)", got)
	}
}

// TestSubscriberSend_FiltersBelowSince pins that a subscriber with
// since=5 never sees events 1..5 even if a shared pump tries to
// deliver them. Without the since baseline, a second subscriber
// joining an active broadcaster would receive every event the pump
// had ever broadcast.
func TestSubscriberSend_FiltersBelowSince(t *testing.T) {
	t.Parallel()
	b := &Broadcaster{
		entry: &Entry{AppName: "core-agent", SessionID: "test"},
	}
	sub := &subscriber{
		ch:       make(chan Frame, 16),
		since:    5,
		lastSent: 5,
	}

	// Pump pushing historical events 1..5 — all skipped.
	for i := int64(1); i <= 5; i++ {
		if !b.send(sub, Frame{Seq: i}) {
			t.Fatalf("send(seq=%d) returned false", i)
		}
	}
	// Live event 6 — accepted.
	if !b.send(sub, Frame{Seq: 6}) {
		t.Fatal("send(seq=6) returned false")
	}

	close(sub.ch)
	var got []int64
	for f := range sub.ch {
		got = append(got, f.Seq)
	}
	if len(got) != 1 || got[0] != 6 {
		t.Errorf("subscriber received %v, want [6] only", got)
	}
}
