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

package tui

import (
	"strings"
	"testing"
	"time"
)

func TestQueue_EnqueueAndMarkInFlightFIFO(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	q.enqueue("first")
	q.enqueue("second")
	q.enqueue("third")
	q.markInFlight(2)
	if got := q.entries[0].state; got != queueInFlight {
		t.Errorf("entries[0].state = %v, want %v (in-flight)", got, queueInFlight)
	}
	if got := q.entries[1].state; got != queueInFlight {
		t.Errorf("entries[1].state = %v, want %v (in-flight)", got, queueInFlight)
	}
	if got := q.entries[2].state; got != queueQueued {
		t.Errorf("entries[2].state = %v, want %v (queued)", got, queueQueued)
	}
}

func TestQueue_MarkAllInFlightDone(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	q.enqueue("a")
	q.enqueue("b")
	q.markInFlight(2)
	q.enqueue("c") // arrives during the in-flight turn; should stay queued
	q.markAllInFlightDone()
	if got := q.entries[0].state; got != queueDone {
		t.Errorf("entries[0] state = %v, want done", got)
	}
	if got := q.entries[1].state; got != queueDone {
		t.Errorf("entries[1] state = %v, want done", got)
	}
	if got := q.entries[2].state; got != queueQueued {
		t.Errorf("entries[2] state = %v, want still-queued (arrived during in-flight turn)", got)
	}
}

func TestQueue_CullExpired_DropsAgedDoneEntries(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	q.removeDoneAfter = 50 * time.Millisecond
	q.enqueue("aged")
	q.enqueue("fresh")
	q.markInFlight(2)
	q.markAllInFlightDone()
	// Manually backdate the first entry's done timestamp so cull
	// sees it as expired without us actually sleeping.
	q.entries[0].created = time.Now().Add(-time.Second)
	q.cullExpired()
	if len(q.entries) != 1 {
		t.Fatalf("entries after cull = %d, want 1", len(q.entries))
	}
	if q.entries[0].text != "fresh" {
		t.Errorf("entries[0].text = %q, want %q", q.entries[0].text, "fresh")
	}
}

func TestQueue_MarkFailed_PreservesErrorMessage(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	q.markFailed("bad", "agent: inbox closed")
	if len(q.entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(q.entries))
	}
	e := q.entries[0]
	if e.state != queueFailed {
		t.Errorf("state = %v, want failed", e.state)
	}
	if e.errMsg != "agent: inbox closed" {
		t.Errorf("errMsg = %q, want %q", e.errMsg, "agent: inbox closed")
	}
	if !q.hasFailed() {
		t.Errorf("hasFailed() = false, want true")
	}
	q.clearFailed()
	if len(q.entries) != 0 {
		t.Errorf("clearFailed left %d entries, want 0", len(q.entries))
	}
	if q.hasFailed() {
		t.Errorf("hasFailed() after clear = true, want false")
	}
}

func TestQueue_HasFadeable(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	q.enqueue("only queued")
	if q.hasFadeable() {
		t.Errorf("hasFadeable on queued-only = true, want false")
	}
	q.markInFlight(1)
	q.markAllInFlightDone()
	if !q.hasFadeable() {
		t.Errorf("hasFadeable after markAllInFlightDone = false, want true")
	}
}

func TestQueue_Render_ShowsGlyphsAndLabels(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	q.enqueue("first")
	q.enqueue("second")
	q.markInFlight(1)
	out := q.render(80)
	if !strings.Contains(out, "queued") {
		t.Errorf("render missing 'queued' label: %q", out)
	}
	if !strings.Contains(out, "in-flight") {
		t.Errorf("render missing 'in-flight' label: %q", out)
	}
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Errorf("render missing entry text: %q", out)
	}
}

func TestQueue_Render_EmptyReturnsEmptyString(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	if got := q.render(80); got != "" {
		t.Errorf("empty queue render = %q, want \"\"", got)
	}
}

func TestQueue_Render_FailedShowsErrorMessage(t *testing.T) {
	t.Parallel()
	q := newQueueModel()
	q.markFailed("bad input", "agent: inbox closed")
	out := q.render(80)
	if !strings.Contains(out, "failed") {
		t.Errorf("failed entry render missing 'failed': %q", out)
	}
	if !strings.Contains(out, "inbox closed") {
		t.Errorf("failed entry render missing error text: %q", out)
	}
}
