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

package main

import (
	"strings"
	"testing"
	"time"
)

func TestQueue_LifecycleHappyPath(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	id := q.enqueue("hello there")
	if got := q.entries[0].state; got != queueQueued {
		t.Fatalf("after enqueue: state = %v, want queueQueued", got)
	}
	q.markSending(id)
	if got := q.entries[0].state; got != queueSending {
		t.Errorf("after markSending: state = %v, want queueSending", got)
	}
	q.markAcked(id)
	if got := q.entries[0].state; got != queueAcked {
		t.Errorf("after markAcked: state = %v, want queueAcked", got)
	}
	q.noteUserEvent("hello there")
	if got := q.entries[0].state; got != queueProcessing {
		t.Errorf("after noteUserEvent matching text: state = %v, want queueProcessing", got)
	}
	q.noteModelResponse()
	if got := q.entries[0].state; got != queueDone {
		t.Errorf("after noteModelResponse: state = %v, want queueDone", got)
	}
}

func TestQueue_NoteUserEvent_NoMatchingAcked(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	id := q.enqueue("hello")
	q.markAcked(id)
	// Text doesn't match → no state advance.
	q.noteUserEvent("something else")
	if got := q.entries[0].state; got != queueAcked {
		t.Errorf("non-matching noteUserEvent should leave state as queueAcked; got %v", got)
	}
}

func TestQueue_NoteUserEvent_OnlyAdvancesAcked(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	q.enqueue("ping")               // stays queueQueued
	q.markAcked(q.enqueue("hello")) // becomes queueAcked
	q.noteUserEvent("ping")         // doesn't match queueAcked entry
	// queued entry should still be queueQueued (we don't backfill).
	if got := q.entries[0].state; got != queueQueued {
		t.Errorf("non-acked entry advanced unexpectedly: state = %v, want queueQueued", got)
	}
}

func TestQueue_MarkFailed_StoresError(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	id := q.enqueue("oops")
	q.markFailed(id, "connection refused")
	e := q.entries[0]
	if e.state != queueFailed {
		t.Errorf("state = %v, want queueFailed", e.state)
	}
	if e.errMsg != "connection refused" {
		t.Errorf("errMsg = %q, want %q", e.errMsg, "connection refused")
	}
}

func TestQueue_CullExpired(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	q.removeDoneAfter = 10 * time.Millisecond
	id1 := q.enqueue("first")
	q.markAcked(id1)
	q.noteUserEvent("first")
	q.noteModelResponse() // first → done
	id2 := q.enqueue("second")
	q.markAcked(id2) // second stays acked
	if len(q.entries) != 2 {
		t.Fatalf("len = %d, want 2 before cull", len(q.entries))
	}
	time.Sleep(20 * time.Millisecond)
	q.cullExpired()
	if len(q.entries) != 1 {
		t.Fatalf("len = %d, want 1 after cull", len(q.entries))
	}
	if q.entries[0].text != "second" {
		t.Errorf("wrong entry survived: %q", q.entries[0].text)
	}
}

func TestQueue_ClearFailed(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	id1 := q.enqueue("ok")
	q.markAcked(id1)
	id2 := q.enqueue("bad")
	q.markFailed(id2, "nope")
	q.clearFailed()
	if len(q.entries) != 1 || q.entries[0].text != "ok" {
		t.Errorf("clearFailed should leave only the acked entry; got %+v", q.entries)
	}
}

func TestQueue_Render_EmptyReturnsEmpty(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	if got := q.render(80, 5); got != "" {
		t.Errorf("empty queue render = %q, want empty string", got)
	}
}

func TestQueue_Render_LabelsAndStates(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	q.markSending(q.enqueue("first"))
	id2 := q.enqueue("second")
	q.markFailed(id2, "boom")
	out := q.render(80, 5)
	for _, want := range []string{"sending", "first", "failed", "boom", "second"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered queue missing %q:\n%s", want, out)
		}
	}
}

func TestQueue_Render_CapsByMaxRows(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	for i := 0; i < 10; i++ {
		q.enqueue("entry")
	}
	out := q.render(80, 3)
	// Three lines max — count newlines (the last line has no
	// trailing newline because render TrimRight's it).
	lines := strings.Count(out, "\n") + 1
	if lines != 3 {
		t.Errorf("rendered %d lines with maxRows=3, want 3", lines)
	}
}

func TestQueue_Truncate(t *testing.T) {
	t.Parallel()
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("under-cap: got %q", got)
	}
	if got := truncate("hello world", 5); got != "hell…" {
		t.Errorf("over-cap: got %q, want %q", got, "hell…")
	}
	if got := truncate("anything", 1); got != "…" {
		t.Errorf("min-cap: got %q, want %q", got, "…")
	}
}
