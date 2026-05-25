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
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// queueState is the lifecycle of one operator-typed-during-streaming
// entry as the in-process TUI sees it. Simpler than the remote TUI's
// queue (no HTTP round-trip means no `sending` / `acked` states).
type queueState int

const (
	queueQueued   queueState = iota // submitted during streaming; waiting for current turn to end
	queueInFlight                   // current auto-continue turn is processing this entry
	queueDone                       // auto-continue turn finished; fades from panel
	queueFailed                     // agent.Inject returned an error
)

// queueGlyph maps state → display symbol.
var queueGlyph = map[queueState]string{
	queueQueued:   "⏳",
	queueInFlight: "↻",
	queueDone:     "·",
	queueFailed:   "✗",
}

// queueLabel maps state → human-readable label for the panel suffix.
var queueLabel = map[queueState]string{
	queueQueued:   "queued",
	queueInFlight: "in-flight",
	queueDone:     "done",
	queueFailed:   "failed",
}

// queueEntry is one row in the queue panel.
type queueEntry struct {
	text    string
	state   queueState
	errMsg  string // populated when state == queueFailed
	created time.Time
}

// queueModel is the TUI-local mirror of operator-typed-during-streaming
// entries. The agent's inbox is the actual source of truth for
// what the auto-continue turn drains; this model exists so the
// operator can see what's pending. FIFO: drain order matches
// enqueue order, so the first N queued entries advance to
// in-flight when the auto-continue draws N messages off the inbox.
type queueModel struct {
	entries []*queueEntry

	// removeDoneAfter is how long a Done entry sits in the panel
	// before being culled. Mirrors the remote TUI's 2s fade so
	// operators see the completion confirmation briefly.
	removeDoneAfter time.Duration
}

func newQueueModel() queueModel {
	return queueModel{removeDoneAfter: 2 * time.Second}
}

// enqueue records a new entry as queued. Called from
// handleSubmitDuringStreaming after agent.Inject succeeds.
func (q *queueModel) enqueue(text string) {
	q.entries = append(q.entries, &queueEntry{
		text:    text,
		state:   queueQueued,
		created: time.Now(),
	})
}

// markFailed flags the most-recently enqueued entry as failed.
// Called when agent.Inject returns an error during streaming.
func (q *queueModel) markFailed(text, errMsg string) {
	q.entries = append(q.entries, &queueEntry{
		text:    text,
		state:   queueFailed,
		errMsg:  errMsg,
		created: time.Now(),
	})
}

// markInFlight advances the first N queued entries to in-flight,
// where N is the number of messages the auto-continue actually
// drained. Called from handleTurnDone's auto-continue path before
// startAgentTurn fires.
func (q *queueModel) markInFlight(n int) {
	advanced := 0
	for _, e := range q.entries {
		if e.state == queueQueued && advanced < n {
			e.state = queueInFlight
			advanced++
		}
	}
}

// markAllInFlightDone advances every in-flight entry to done. Called
// when the auto-continue turn completes. Note: a turn that produces
// the auto-continue (i.e. the auto-continue itself completing) may
// have queued additional entries while it was running; those stay
// in queued state and the next handleTurnDone picks them up.
func (q *queueModel) markAllInFlightDone() {
	for _, e := range q.entries {
		if e.state == queueInFlight {
			e.state = queueDone
			e.created = time.Now() // re-use as fade timer
		}
	}
}

// cullExpired removes Done entries whose fade window has elapsed.
// Called from the view loop so the panel naturally clears without
// explicit operator action.
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
// Esc keystroke. Lets the operator dismiss errors after they've seen
// them.
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

// hasFailed reports whether any entry is in the failed state. Used
// by Update's bare-chat Esc handler to decide whether to consume the
// Esc keystroke or fall through to the textarea.
func (q *queueModel) hasFailed() bool {
	for _, e := range q.entries {
		if e.state == queueFailed {
			return true
		}
	}
	return false
}

// hasFadeable reports whether any entry is in the done state (i.e.
// will be culled by cullExpired once its fade window elapses). Used
// by Update to decide whether to keep scheduling queueCullCmd ticks.
func (q *queueModel) hasFadeable() bool {
	for _, e := range q.entries {
		if e.state == queueDone {
			return true
		}
	}
	return false
}

// queuePanelMaxRows caps how many entries render at once in the
// panel. The render window slides to show the most-recent N so the
// operator always sees what they just typed; older Done items have
// already faded via cullExpired anyway.
const queuePanelMaxRows = 4

// render returns the multi-line panel string. Empty when no entries;
// one line per entry capped at queuePanelMaxRows. width is the
// available terminal width; entries truncate their text to fit.
func (q *queueModel) render(width int) string {
	if len(q.entries) == 0 {
		return ""
	}
	// Show the most recent queuePanelMaxRows entries — operators
	// care about what's queued right now; older Done items already
	// faded via cullExpired.
	start := 0
	if len(q.entries) > queuePanelMaxRows {
		start = len(q.entries) - queuePanelMaxRows
	}

	queuedStyle := lipgloss.NewStyle().Foreground(brandSlate)
	inFlightStyle := lipgloss.NewStyle().Foreground(brandPink).Bold(true)
	doneStyle := lipgloss.NewStyle().Foreground(brandSlate).Faint(true)
	failedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)

	var sb strings.Builder
	for i := start; i < len(q.entries); i++ {
		e := q.entries[i]
		glyph := queueGlyph[e.state]
		label := queueLabel[e.state]
		suffix := fmt.Sprintf("   (%s)", label)
		if e.state == queueFailed && e.errMsg != "" {
			suffix = fmt.Sprintf("   (failed: %s)", queueTruncate(e.errMsg, 40))
		}
		body := queueTruncate(e.text, queueMax(width-len(glyph)-len(suffix)-6, 8))
		var style lipgloss.Style
		switch e.state {
		case queueQueued:
			style = queuedStyle
		case queueInFlight:
			style = inFlightStyle
		case queueDone:
			style = doneStyle
		case queueFailed:
			style = failedStyle
		}
		line := fmt.Sprintf("  %s %q%s", glyph, body, suffix)
		sb.WriteString(style.Render(line))
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

func queueTruncate(s string, n int) string {
	if n <= 1 {
		return "…"
	}
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func queueMax(a, b int) int {
	if a > b {
		return a
	}
	return b
}
