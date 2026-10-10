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

// The agent side of the per-turn failure rows (#1258) — see
// pkg/attach/turn_failure_events.go for the rows themselves and
// docs/durable-turn-failures-design.md for the decisions.
//
// Two writers, each at the site that already reports the failure on the
// other two surfaces, so a row cannot exist without its log line and
// frame or the reverse:
//
//   - emitGuardrailTrip (guardrail_halt.go) writes one row per trip.
//   - Run writes one turn-error row per error it reports: in the turn's
//     cleanup, beside the typed `turn-error` frame, and on the pre-turn
//     refusal path, which reports its error to the driver (and so to the
//     driver's log line) but has never emitted a frame.
//
// Both go through queueOutOfBandEvent, the #565 write window: a trip
// that fires mid-turn is parked until the cleanup's drain, after the
// runner has released its session handle.

package agent

import (
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// persistFailureRow queues a failure row and returns its event id, for
// the typed frame's EventID. Returns "" when nothing will be written —
// no eventlog — so a frame never names a row that does not exist.
func (a *Agent) persistFailureRow(row *session.Event) string {
	if a == nil || row == nil || a.eventLog == nil {
		return ""
	}
	a.queueOutOfBandEvent(row)
	return row.ID
}

// recordTurnError persists te as a turn-error row and returns te with
// its EventID set, ready to go out as the typed frame. promptID is the
// turn's correlation handle (empty for a pre-turn refusal, which never
// got one); cutBy is the guardrail kind whose cut produced the error, as
// consumeGuardrailHalt reports it, or empty.
func (a *Agent) recordTurnError(te attach.TurnError, promptID, cutBy string) attach.TurnError {
	te.EventID = a.persistFailureRow(attach.NewTurnErrorEvent(te, promptID, cutBy))
	return te
}
