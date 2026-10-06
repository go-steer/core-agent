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

package coretuiremote

import (
	"fmt"
	"testing"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// Durable failure rows in the TUI (#1258): a late attach shows each cut,
// and an attached TUI that receives a failure both ways shows it once.

func adapterSpeaking(version string) *Adapter {
	a := &Adapter{}
	a.consumeTypedFrame(attach.Frame{Type: attach.EventCapabilities,
		TypedData: &attach.Capabilities{ProtocolVersion: version}})
	return a
}

func cutRows() (trip, terr *session.Event) {
	trip = attach.NewGuardrailTurnTripEvent(attach.GuardrailCostCeiling, "per-turn cost ceiling exceeded", true)
	terr = attach.NewTurnErrorEvent(attach.TurnError{
		Kind: attach.TurnErrorCanceled, Code: "CANCELED", Message: "turn canceled",
	}, "p-1", attach.TurnErrorCostCeiling)
	return trip, terr
}

// The acceptance criterion: a TUI that attaches after the cut — so it
// gets the replayed rows and never the typed frames — renders the trip
// and the turn error, in that order, with the trip armed to absorb the
// cancel exactly as the live frame would have.
func TestFailureRows_LateAttachRendersEachCut(t *testing.T) {
	t.Parallel()
	a := adapterSpeaking("1.19.0")
	trip, terr := cutRows()

	ev := a.translateStreamEvent(trip)
	if ev.GuardrailTrip == nil || !ev.GuardrailTrip.HaltedTurn || ev.GuardrailTrip.Guardrail != attach.GuardrailCostCeiling {
		t.Fatalf("replayed trip row projected to %+v", ev)
	}
	if isEmptyEvent(ev) {
		t.Fatal("isEmptyEvent drops the projected trip, so the loop would never yield it")
	}
	ev = a.translateStreamEvent(terr)
	if ev.TurnError == nil || ev.TurnError.Kind != attach.TurnErrorCanceled {
		t.Fatalf("replayed turn-error row projected to %+v", ev)
	}
	if isEmptyEvent(ev) {
		t.Fatal("isEmptyEvent drops the projected turn error")
	}
}

// A mid-turn trip: typed frame first, row at the turn's cleanup.
func TestFailureRows_TypedFirstThenRowRendersOnce(t *testing.T) {
	t.Parallel()
	a := adapterSpeaking("1.19.0")
	trip, _ := cutRows()
	if _, ok := a.consumeTypedFrame(attach.Frame{Type: attach.EventGuardrailTrip,
		TypedData: &attach.GuardrailTrip{Guardrail: attach.GuardrailCostCeiling, Reason: "r", HaltedTurn: true, EventID: trip.ID}}); !ok {
		t.Fatal("first sighting (the typed frame) did not render")
	}
	if ev := a.translateStreamEvent(trip); !isEmptyEvent(ev) {
		t.Errorf("row named by an already-rendered frame rendered again: %+v", ev)
	}
}

// A turn error: the row is written before the typed frame, which the
// terminal barrier holds back, so the row wins and the frame is dropped.
func TestFailureRows_RowFirstThenTypedRendersOnce(t *testing.T) {
	t.Parallel()
	a := adapterSpeaking("1.19.0")
	_, terr := cutRows()
	if ev := a.translateStreamEvent(terr); ev.TurnError == nil {
		t.Fatal("first sighting (the row) did not render")
	}
	if _, ok := a.consumeTypedFrame(attach.Frame{Type: attach.EventTurnError,
		TypedData: &attach.TurnError{Kind: attach.TurnErrorCanceled, Message: "turn canceled", EventID: terr.ID}}); ok {
		t.Error("typed turn-error naming an already-rendered row rendered again")
	}
}

// A frame with no event_id has nothing to pair with and always renders
// — a pre-1.19.0 daemon, or a session without an eventlog.
func TestFailureRows_FrameWithoutEventIDAlwaysRenders(t *testing.T) {
	t.Parallel()
	a := adapterSpeaking("1.19.0")
	for i := 0; i < 2; i++ {
		if _, ok := a.consumeTypedFrame(attach.Frame{Type: attach.EventTurnError,
			TypedData: &attach.TurnError{Kind: attach.TurnErrorCanceled, Message: "turn canceled"}}); !ok {
			t.Fatalf("frame %d without event_id was dropped", i)
		}
	}
}

// Against a pre-1.19.0 daemon rows stay unrendered: such a daemon writes
// the halt row but never sets event_id, so a live halt would render
// twice.
func TestFailureRows_OlderDaemonRowsNotRendered(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"1.18.0", "", "2.0.0", "garbage"} {
		a := adapterSpeaking(v)
		if ev := a.translateStreamEvent(attach.NewGuardrailTripEvent(attach.GuardrailWatchdog, "halted")); !isEmptyEvent(ev) {
			t.Errorf("protocol %q: halt row rendered (%+v); its live frame has no event_id to pair with", v, ev)
		}
	}
	a := adapterSpeaking("1.19.0")
	if ev := a.translateStreamEvent(attach.NewGuardrailTripEvent(attach.GuardrailWatchdog, "halted")); ev.GuardrailTrip == nil {
		t.Error("1.19.0: the halt row should render — it is how a late attach learns of the halt")
	}
}

// The dedup set is bounded and forgets the oldest id first.
func TestFailureRows_SightingsAreBounded(t *testing.T) {
	t.Parallel()
	a := &Adapter{}
	for i := 0; i <= maxFailureSightings; i++ {
		a.firstSighting(fmt.Sprintf("id-%d", i))
	}
	if len(a.failureSeen) != maxFailureSightings || len(a.failureOrder) != maxFailureSightings {
		t.Fatalf("set holds %d/%d ids, want %d", len(a.failureSeen), len(a.failureOrder), maxFailureSightings)
	}
	if !a.firstSighting("id-0") {
		t.Error("the oldest id was not evicted")
	}
	if a.firstSighting(fmt.Sprintf("id-%d", maxFailureSightings)) {
		t.Error("the newest id was evicted")
	}
}
