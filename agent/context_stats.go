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

// File context_stats.go exposes a snapshot view of the three
// context-management mechanisms (compaction, checkpoint, subtask)
// so hosts can render a /context slash (or whatever surface they
// like) that tells the operator what's happened to their session's
// context window so far. Companion to the existing /stats which
// reports token/cost from usage.Tracker — /context reports the
// SHAPE of the conversation (how many boundaries, how compressed,
// how much of the cost came from subtasks).
//
// Boundary stats (compaction + checkpoint) are derived on-demand
// by scanning the session's event log for events carrying
// CompactionMetadataKey. The scan is O(events) per call; for the
// /context slash this is fine (operator-driven, infrequent). For
// hot-path use, callers should cache or sample.
//
// Subtask stats are accumulated on Agent across RunSubtask calls
// (see recordSubtaskUsage in subtask.go). usage.Tracker bundles
// subtask + parent turns into one totals view because pricing
// per-turn doesn't know whether the turn came from a subtask;
// these counters give us the breakdown without touching the
// tracker.

package agent

import (
	"context"
	"time"

	"google.golang.org/adk/session"
)

// ContextStats is a snapshot view of the three context-management
// mechanisms wired on the Agent. All fields are zero-value-safe;
// the consumer renders "no compactions yet" / "no subtasks yet"
// based on the counts.
//
// Boundary fields (Compaction*, Checkpoint*, TotalSummaryChars)
// are derived from the session event log on each call. Subtask
// fields come from in-memory counters bumped in RunSubtask.
type ContextStats struct {
	// Compaction* report on Mechanism A boundary events.
	CompactionCount     int
	LastCompactionFocus string    // CompactionFocusKey from the last compaction event (empty when none)
	LastCompactionTime  time.Time // zero when none

	// Checkpoint* report on Mechanism C boundary events.
	CheckpointCount    int
	LastCheckpointNote string    // CheckpointNoteKey from the last checkpoint event (empty when none)
	LastCheckpointTime time.Time // zero when none

	// TotalSummaryChars is the aggregate character count of all
	// boundary summary text (compaction + checkpoint) written
	// this session. Proxy for "how much history has been
	// compressed" — useful as a sanity check that compaction is
	// actually doing something.
	TotalSummaryChars int

	// Subtask* report on Mechanism B usage. Count + tokens + cost
	// are accumulated in recordSubtaskUsage; usage.Tracker totals
	// (/stats) include this same cost in their grand total.
	SubtaskCount        int
	SubtaskInputTokens  int
	SubtaskOutputTokens int
	SubtaskCostUSD      float64
}

// ContextStats returns a snapshot of compaction/checkpoint/subtask
// activity for this session. Safe to call from any goroutine.
// Errors fetching the session (e.g. session.Service hiccup) are
// swallowed and result in zero boundary fields — the subtask
// fields are populated regardless since they're in-memory.
//
// Cost: one session.Service.Get() call + O(events) scan. Designed
// for operator-driven /context slash usage, not hot-path
// telemetry — cache or sample if you need it per-turn.
func (a *Agent) ContextStats() ContextStats {
	if a == nil {
		return ContextStats{}
	}

	// Subtask counters: copy under lock so we don't race with
	// concurrent RunSubtask calls writing to them.
	a.mu.Lock()
	stats := ContextStats{
		SubtaskCount:        a.subtaskCount,
		SubtaskInputTokens:  a.subtaskInputTokens,
		SubtaskOutputTokens: a.subtaskOutputTokens,
		SubtaskCostUSD:      a.subtaskCostUSD,
	}
	a.mu.Unlock()

	// Boundary scan. When the session.Service isn't available,
	// the boundary fields stay zero; subtask fields above are
	// still populated.
	if a.sessionService == nil {
		return stats
	}
	resp, err := a.sessionService.Get(context.Background(), &session.GetRequest{
		AppName:   a.appName,
		UserID:    a.userID,
		SessionID: a.sessionID,
	})
	if err != nil || resp == nil || resp.Session == nil {
		return stats
	}
	for ev := range resp.Session.Events().All() {
		if ev == nil || ev.CustomMetadata == nil {
			continue
		}
		v, ok := ev.CustomMetadata[CompactionMetadataKey]
		if !ok {
			continue
		}
		tag, ok := v.(string)
		if !ok {
			continue
		}
		// Sum summary text length regardless of kind — both
		// compaction and checkpoint summaries contribute to
		// "how much history was compressed."
		stats.TotalSummaryChars += contentTextLen(ev)
		switch tag {
		case CompactionEventTag:
			stats.CompactionCount++
			if focus, ok := ev.CustomMetadata[CompactionFocusKey].(string); ok {
				stats.LastCompactionFocus = focus
			}
			stats.LastCompactionTime = ev.Timestamp
		case CheckpointEventTag:
			stats.CheckpointCount++
			if note, ok := ev.CustomMetadata[CheckpointNoteKey].(string); ok {
				stats.LastCheckpointNote = note
			}
			stats.LastCheckpointTime = ev.Timestamp
		}
	}
	return stats
}

// contentTextLen sums the Text field lengths across an event's
// content parts. Used to compute TotalSummaryChars without
// allocating a joined string.
func contentTextLen(ev *session.Event) int {
	if ev == nil || ev.Content == nil {
		return 0
	}
	n := 0
	for _, p := range ev.Content.Parts {
		if p != nil {
			n += len(p.Text)
		}
	}
	return n
}
