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
	"fmt"
	"strings"
	"time"
)

// queueState is the lifecycle of one inject as the TUI sees it.
// Drawn with one of the symbols in queueGlyph.
type queueState int

const (
	queueQueued     queueState = iota // pending; injectCmd hasn't fired yet
	queueSending                      // POST /inject in flight
	queueAcked                        // server returned 200; in the agent's inbox
	queueProcessing                   // matching user event observed in the SSE stream
	queueDone                         // model emitted a response after this entry; fade out
	queueFailed                       // POST returned non-2xx OR network error
)

// queueGlyph maps state → display symbol. Single-rune symbols so
// they line up regardless of theme.
var queueGlyph = map[queueState]string{
	queueQueued:     "⏳",
	queueSending:    "↑",
	queueAcked:      "✓",
	queueProcessing: "…",
	queueDone:       "·", // fades but keeps slot until removeAfter expires
	queueFailed:     "✗",
}

// queueLabel maps state → human-readable label for hover / focus
// detail. Kept short; queue panel is tight on horizontal space.
var queueLabel = map[queueState]string{
	queueQueued:     "queued",
	queueSending:    "sending",
	queueAcked:      "acked",
	queueProcessing: "processing",
	queueDone:       "done",
	queueFailed:     "failed",
}

// queueEntry is one row in the queue panel.
type queueEntry struct {
	id      uint64
	text    string
	state   queueState
	errMsg  string    // populated when state == queueFailed
	created time.Time // for visual ordering + the fade timer on Done
}

// queueModel is the queue panel state. Append-only in v1 (no
// reorder / edit), but operators can clear failed entries by
// focusing the panel and pressing Esc (handled in chat.UpdateInner).
type queueModel struct {
	entries []*queueEntry
	// nextID is a monotonic counter for entry IDs. The queue is
	// only mutated from the main tea.Program goroutine (Update),
	// so a plain uint64 is safe — no atomics needed.
	nextID uint64

	// removeDoneAfter is how long a Done entry sits in the panel
	// before being culled. Operators see the green check briefly
	// to confirm the inject's full lifecycle completed.
	removeDoneAfter time.Duration
}

func newQueueModel() queueModel {
	return queueModel{removeDoneAfter: 2 * time.Second}
}

// enqueue records a new inject the operator just submitted. Returns
// the entry's ID so the caller can later update its state via
// markSending / markAcked / markFailed.
func (q *queueModel) enqueue(text string) uint64 {
	q.nextID++
	id := q.nextID
	q.entries = append(q.entries, &queueEntry{
		id:      id,
		text:    text,
		state:   queueQueued,
		created: time.Now(),
	})
	return id
}

// markSending advances an entry's state to "sending" (POST is in
// flight). No-op when the id isn't found (queue may have been
// cleared / culled).
func (q *queueModel) markSending(id uint64) {
	if e := q.find(id); e != nil {
		e.state = queueSending
	}
}

// markAcked advances to "acked" (server returned 200; payload is
// in the agent's inbox awaiting drain).
func (q *queueModel) markAcked(id uint64) {
	if e := q.find(id); e != nil {
		e.state = queueAcked
	}
}

// markFailed advances to "failed" and stores the error message
// for display.
func (q *queueModel) markFailed(id uint64, errMsg string) {
	if e := q.find(id); e != nil {
		e.state = queueFailed
		e.errMsg = errMsg
	}
}

// noteUserEvent is called by chat.applyFrame when it sees a `user`
// event in the SSE stream. Best-effort matches the event's text
// against acked entries; the first match advances processing → done.
// v1 uses text equality; v1.1 will use request_id when PR A's
// server-side ID work ships.
func (q *queueModel) noteUserEvent(text string) {
	for _, e := range q.entries {
		if e.state == queueAcked && strings.TrimSpace(e.text) == strings.TrimSpace(text) {
			e.state = queueProcessing
			return
		}
	}
}

// noteModelResponse advances processing entries to done after a
// model response lands. Called when chat.applyFrame sees the model
// emit text following an inbox drain.
func (q *queueModel) noteModelResponse() {
	for _, e := range q.entries {
		if e.state == queueProcessing {
			e.state = queueDone
			e.created = time.Now() // re-use for fade timer
		}
	}
}

// cullExpired removes Done entries whose fade window has elapsed.
// Called from the chat View loop so the panel naturally clears
// without explicit operator action.
func (q *queueModel) cullExpired() {
	if len(q.entries) == 0 {
		return
	}
	cutoff := time.Now().Add(-q.removeDoneAfter)
	kept := q.entries[:0]
	for _, e := range q.entries {
		if e.state == queueDone && e.created.Before(cutoff) {
			continue
		}
		kept = append(kept, e)
	}
	q.entries = kept
}

// clearFailed removes all failed entries — bound to a focused-panel
// Esc keystroke. Lets the operator dismiss errors they've already
// seen without restarting the TUI.
func (q *queueModel) clearFailed() {
	if len(q.entries) == 0 {
		return
	}
	kept := q.entries[:0]
	for _, e := range q.entries {
		if e.state != queueFailed {
			kept = append(kept, e)
		}
	}
	q.entries = kept
}

func (q *queueModel) find(id uint64) *queueEntry {
	for _, e := range q.entries {
		if e.id == id {
			return e
		}
	}
	return nil
}

// render returns the multi-line panel string. Empty when no
// entries; one line per entry capped at maxRows (older Done
// entries fade out via cullExpired before we hit the cap). width
// is the available terminal width; entries truncate their text
// portion to fit.
func (q *queueModel) render(width, maxRows int) string {
	if len(q.entries) == 0 {
		return ""
	}
	// Show the most recent maxRows entries — operators care
	// about what's in flight RIGHT NOW; older Done items have
	// already faded via cullExpired.
	start := 0
	if len(q.entries) > maxRows {
		start = len(q.entries) - maxRows
	}

	var sb strings.Builder
	for i := start; i < len(q.entries); i++ {
		e := q.entries[i]
		glyph := queueGlyph[e.state]
		label := queueLabel[e.state]
		// "  ⏳ <text>   (queued)"
		// Reserve a few chars for the trailing "  (label)" suffix.
		suffix := fmt.Sprintf("   (%s)", label)
		if e.state == queueFailed && e.errMsg != "" {
			suffix = fmt.Sprintf("   (failed: %s)", truncate(e.errMsg, 40))
		}
		body := truncate(e.text, max(width-len(glyph)-len(suffix)-6, 8))
		style := styleBubbleTool
		switch e.state {
		case queueFailed:
			style = styleBubbleErr
		case queueDone:
			style = styleHint
		case queueProcessing, queueSending:
			style = styleBubbleAsst
		}
		line := fmt.Sprintf("  %s %q%s", glyph, body, suffix)
		sb.WriteString(style.Render(line))
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

func truncate(s string, n int) string {
	if n <= 1 {
		return "…"
	}
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
