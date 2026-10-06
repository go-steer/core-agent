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

// Rendering the durable failure rows (#1258).
//
// Since protocol 1.19.0 a guardrail trip and a turn error are each
// recorded twice on the stream an attached client reads: once as the
// typed frame (live only, never replayed) and once as an eventlog row
// that arrives as an `agent` frame (live AND on replay). The row is
// what lets a TUI that attached late show the cut at all; the typed
// frame is what an attached TUI has always rendered. Rendering both
// would put every cut in the chat twice.
//
// The rule is "first sighting wins", keyed on the row's event id, which
// the typed frame carries as event_id. The order differs by kind — a
// mid-turn trip's typed frame goes out at the cut and its row lands at
// the turn's cleanup, while a turn error's row is written BEFORE its
// typed frame, which the terminal barrier then holds back — so the
// dedup is symmetric rather than "prefer the frame".
//
// Rows are rendered only from a daemon that announced 1.19.0 or later.
// An older daemon also writes the halt row (#643) but never sets
// event_id, so against it a live halt would render twice; the status
// quo — halt rows not rendered — is the better failure there.

package coretuiremote

import (
	"strings"

	coretui "github.com/go-steer/core-tui/tui"
	"golang.org/x/mod/semver"
	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// failureRowsProtocol is the first protocol whose typed failure frames
// name their durable rows.
const failureRowsProtocol = "v1.19.0"

// maxFailureSightings bounds the dedup set. A pair's two halves arrive
// within one turn of each other, so a few hundred ids is far more
// history than the dedup ever consults; the bound only keeps a
// days-long observer session from growing the set without limit.
const maxFailureSightings = 512

// firstSighting records id and reports whether this is the first time
// the adapter has seen it. An empty id — a pre-1.19.0 frame, or a
// session with no eventlog — is always a first sighting: there is
// nothing to pair it with.
func (a *Adapter) firstSighting(id string) bool {
	if id == "" {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, seen := a.failureSeen[id]; seen {
		return false
	}
	if a.failureSeen == nil {
		a.failureSeen = make(map[string]struct{})
	}
	a.failureSeen[id] = struct{}{}
	a.failureOrder = append(a.failureOrder, id)
	if len(a.failureOrder) > maxFailureSightings {
		delete(a.failureSeen, a.failureOrder[0])
		a.failureOrder = a.failureOrder[1:]
	}
	return true
}

// typedFailureEventID is the event_id a typed guardrail-trip or
// turn-error frame carries, or "" for every other frame.
func typedFailureEventID(frame attach.Frame) string {
	switch p := frame.TypedData.(type) {
	case *attach.GuardrailTrip:
		if p != nil {
			return p.EventID
		}
	case *attach.TurnError:
		if p != nil {
			return p.EventID
		}
	}
	return ""
}

// translateStreamEvent is translateEvent plus the durable failure rows:
// a guardrail row projects to a GuardrailTrip and a turn-error row to a
// TurnError, exactly as their typed frames would, unless the typed frame
// already rendered them (then the result is empty and the caller skips
// it). Every other event is translateEvent's.
func (a *Adapter) translateStreamEvent(raw *session.Event) coretui.Event {
	if !a.daemonNamesFailureRows() {
		return translateEvent(raw)
	}
	if gt, ok := guardrailRow(raw); ok {
		if !a.firstSighting(gt.EventID) {
			return coretui.Event{}
		}
		return coretui.Event{GuardrailTrip: guardrailTripToCoreTui(&gt)}
	}
	if te, _, ok := attach.TurnErrorRow(raw); ok {
		if !a.firstSighting(te.EventID) {
			return coretui.Event{}
		}
		return coretui.Event{TurnError: turnErrorToCoreTui(&te)}
	}
	return translateEvent(raw)
}

// guardrailRow reads either guardrail row kind: the per-turn trip row,
// or the #643 halt row.
func guardrailRow(raw *session.Event) (attach.GuardrailTrip, bool) {
	if gt, ok := attach.GuardrailTurnTrip(raw); ok {
		return gt, true
	}
	return attach.GuardrailHaltRow(raw)
}

// daemonNamesFailureRows reports whether this adapter's daemon speaks a
// protocol whose typed failure frames carry event_id. Unknown — no
// capabilities frame yet — is no, and so is another major.
func (a *Adapter) daemonNamesFailureRows() bool {
	return protocolAtLeast(a.DaemonProtocolVersion(), failureRowsProtocol)
}

// protocolAtLeast reports whether version v is at least floor within
// floor's major. Empty or unparseable is false.
func protocolAtLeast(v, floor string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return semver.IsValid(v) && semver.Major(v) == semver.Major(floor) && semver.Compare(v, floor) >= 0
}
