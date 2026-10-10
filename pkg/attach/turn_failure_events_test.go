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

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
)

// The per-turn failure rows (#1258). The agent-side tests drive the
// real turn loop; these pin the row contract itself — what a reader of
// the eventlog, or of an `agent` frame, may rely on.

func TestGuardrailTurnTripEvent_RoundTripsThroughJSON(t *testing.T) {
	t.Parallel()
	ev := NewGuardrailTurnTripEvent(GuardrailCostCeiling, "per-turn cost ceiling exceeded", true)
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back session.Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	gt, ok := GuardrailTurnTrip(&back)
	want := GuardrailTrip{Guardrail: GuardrailCostCeiling, Reason: "per-turn cost ceiling exceeded", HaltedTurn: true, EventID: ev.ID}
	if !ok || gt != want {
		t.Errorf("after a JSON hop: %+v (ok=%v), want %+v", gt, ok, want)
	}
}

func TestTurnErrorEvent_RoundTripsThroughJSON(t *testing.T) {
	t.Parallel()
	te := TurnError{Kind: TurnErrorRateLimited, Code: "429", Message: "quota exceeded", Retryable: true, Hint: "slow down"}
	ev := NewTurnErrorEvent(te, "p-1", "")
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back session.Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got, cutBy, ok := TurnErrorRow(&back)
	te.EventID = ev.ID
	if !ok || got != te || cutBy != "" {
		t.Errorf("after a JSON hop: %+v cut_by=%q (ok=%v), want %+v", got, cutBy, ok, te)
	}
	if back.CustomMetadata[turnErrMetaPromptID] != "p-1" {
		t.Errorf("prompt_id lost: %v", back.CustomMetadata)
	}
}

// Optional keys are omitted rather than written empty.
func TestTurnErrorEvent_OmitsEmptyOptionalKeys(t *testing.T) {
	t.Parallel()
	ev := NewTurnErrorEvent(TurnError{Kind: TurnErrorCanceled, Message: "turn canceled"}, "", "")
	for _, k := range []string{turnErrMetaCode, turnErrMetaHint, turnErrMetaPromptID, turnErrMetaCutBy} {
		if _, present := ev.CustomMetadata[k]; present {
			t.Errorf("empty optional key %q written", k)
		}
	}
	ev = NewTurnErrorEvent(TurnError{Kind: TurnErrorCanceled, Message: "turn canceled"}, "p", TurnErrorRefusalStorm)
	if _, cutBy, _ := TurnErrorRow(ev); cutBy != TurnErrorRefusalStorm {
		t.Errorf("cut_by = %q, want %q", cutBy, TurnErrorRefusalStorm)
	}
}

// Must NOT enter model context, and must NOT read as an in-band model
// error: no Content (ADK's contents processor and every conversation
// reader skip content-less events) and no LLMResponse error fields
// (auto-continue's tail classifier reads ErrorCode before it skips).
func TestTurnFailureRows_CarryNoModelFacingFields(t *testing.T) {
	t.Parallel()
	for _, ev := range []*session.Event{
		NewGuardrailTurnTripEvent(GuardrailWatchdog, "cut", false),
		NewTurnErrorEvent(TurnError{Kind: TurnErrorAuth, Code: "403", Message: "denied"}, "p", ""),
	} {
		if ev.Content != nil || ev.ErrorCode != "" || ev.ErrorMessage != "" || ev.UsageMetadata != nil || ev.Partial {
			t.Errorf("%s row has model-facing fields: %+v", ev.Author, ev.LLMResponse)
		}
	}
}

// Must NOT restore as a halt: the #643 fold keys on the halt row's
// author, and a session full of per-turn trip rows and turn errors is a
// running session.
func TestFoldGuardrailEvents_IgnoresPerTurnRows(t *testing.T) {
	t.Parallel()
	events := []*session.Event{
		NewGuardrailTurnTripEvent(GuardrailCostCeiling, "per-turn cost ceiling exceeded", true),
		NewGuardrailTurnTripEvent(GuardrailWatchdog, "watchdog cut the turn", false),
		NewTurnErrorEvent(TurnError{Kind: TurnErrorCostCeiling, Message: "turn refused"}, "", ""),
		NewTurnErrorEvent(TurnError{Kind: TurnErrorWatchdog, Message: "turn refused"}, "", ""),
	}
	if st := FoldGuardrailEvents(slices.Values(events)); st.Halted() || st.BudgetAddedUSD != 0 {
		t.Errorf("fold over per-turn rows = %+v, want nothing halted", st)
	}
	// And the halt row still folds when it is among them.
	events = append(events, NewGuardrailTripEvent(GuardrailWatchdog, "halted"))
	if st := FoldGuardrailEvents(slices.Values(events)); !st.WatchdogTripped || st.CostTripped {
		t.Errorf("fold with a halt row = %+v, want only the watchdog halted", st)
	}
}

// Each reader claims only its own rows, on an undifferentiated stream.
func TestTurnFailureReaders_IgnoreOtherEvents(t *testing.T) {
	t.Parallel()
	impostor := NewGuardrailTurnTripEvent(GuardrailWatchdog, "x", true)
	impostor.InvocationID = "something-else"
	others := []*session.Event{nil, session.NewEvent(context.Background(), "bare"), impostor,
		NewContextReductionFailedEvent(ContextReductionCompaction, "boom", 0, 0)}
	for _, ev := range others {
		if _, ok := GuardrailTurnTrip(ev); ok {
			t.Errorf("GuardrailTurnTrip claimed %+v", ev)
		}
		if _, _, ok := TurnErrorRow(ev); ok {
			t.Errorf("TurnErrorRow claimed %+v", ev)
		}
		if _, ok := GuardrailHaltRow(ev); ok {
			t.Errorf("GuardrailHaltRow claimed %+v", ev)
		}
	}
	if _, ok := GuardrailTurnTrip(NewGuardrailTripEvent(GuardrailWatchdog, "halted")); ok {
		t.Error("GuardrailTurnTrip claimed the halt row — one trip would count twice")
	}
	if _, ok := GuardrailHaltRow(NewGuardrailTurnTripEvent(GuardrailWatchdog, "cut", true)); ok {
		t.Error("GuardrailHaltRow claimed a per-turn row")
	}
}

// The halt row written since #1258 records whether the halt cut a turn;
// the fold is unaffected, and a pre-#1258 halt row reads as false.
func TestGuardrailHaltEvent_CarriesHaltedTurnAndStillFolds(t *testing.T) {
	t.Parallel()
	ev := NewGuardrailHaltEvent(GuardrailCostCeiling, "3 in a row", true)
	if gt, ok := GuardrailHaltRow(ev); !ok || !gt.HaltedTurn || gt.EventID != ev.ID {
		t.Errorf("GuardrailHaltRow = %+v (ok=%v), want halted_turn true", gt, ok)
	}
	if st := FoldGuardrailEvents(slices.Values([]*session.Event{ev})); !st.CostTripped || st.CostReason != "3 in a row" {
		t.Errorf("fold = %+v, want the cost halt restored", st)
	}
	if gt, _ := GuardrailHaltRow(NewGuardrailTripEvent(GuardrailWatchdog, "old")); gt.HaltedTurn {
		t.Error("a pre-#1258 halt row has no halted_turn and must read as false")
	}
}

func TestConformance_GuardrailTripEventIDV1_19_0(t *testing.T) {
	t.Parallel()
	assertMatchesConformanceFixture(t,
		"testdata/conformance/guardrail-trip-cut-v1.19.0.json",
		GuardrailTrip{
			Guardrail: GuardrailCostCeiling,
			Reason: "per-turn cost ceiling exceeded: this turn cost $0.0112, ceiling is $0.0100. " +
				"The turn was stopped with its work unfinished; the session is NOT halted.",
			HaltedTurn: true,
			EventID:    "5b0f3c1e-7a52-4d0e-9a3b-2f1d7c9e8a40",
		})
}

func TestConformance_TurnErrorEventIDV1_19_0(t *testing.T) {
	t.Parallel()
	assertMatchesConformanceFixture(t,
		"testdata/conformance/turn-error-v1.19.0.json",
		TurnError{
			Kind:      TurnErrorCanceled,
			Code:      "CANCELED",
			Message:   "turn canceled",
			Retryable: false,
			EventID:   "c3e8a9d2-14b6-4f7e-8d05-6a2b9e1f0c77",
		})
}

// TestEvents_ReplayCarriesPerTurnFailureRows: the rows survive the
// eventlog and come back out of GET …/events?since=0 as `agent` frames
// a client can read with the exported readers. The agent package runs
// the same check end to end from a real cut turn.
func TestEvents_ReplayCarriesPerTurnFailureRows(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()
	reg := NewSessionRegistry()
	ag := &eventfulRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "failures"},
		handle:         h,
	}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := h.Service.Create(ctx, &session.CreateRequest{AppName: "core-agent", UserID: "u", SessionID: "failures"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := h.Service.Get(ctx, &session.GetRequest{AppName: "core-agent", UserID: "u", SessionID: "failures"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	trip := NewGuardrailTurnTripEvent(GuardrailCostCeiling, "per-turn cost ceiling exceeded", true)
	terr := NewTurnErrorEvent(TurnError{Kind: TurnErrorCanceled, Code: "CANCELED", Message: "turn canceled"}, "p-9", TurnErrorCostCeiling)
	for _, ev := range []*session.Event{trip, terr} {
		if err := h.Service.AppendEvent(ctx, got.Session, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	base, cleanupSrv := startTestServer(t, reg)
	defer cleanupSrv()
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodGet, base+"/sessions/core-agent/failures/events?since=0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()

	frames := readSSEFrames(t, resp.Body)
	var sawTrip, sawErr bool
	for !sawTrip || !sawErr {
		f, _ := awaitFrame(t, frames, 4*time.Second, func(f sseFrame) bool { return f.Event == EventAgent })
		if f.Event == "" {
			t.Fatalf("replay ended without both rows (trip=%v error=%v)", sawTrip, sawErr)
		}
		var fr struct {
			Event *session.Event `json:"event"`
		}
		if err := json.Unmarshal([]byte(f.Data), &fr); err != nil {
			t.Fatalf("frame JSON: %v", err)
		}
		if gt, ok := GuardrailTurnTrip(fr.Event); ok {
			sawTrip = gt.EventID == trip.ID && gt.HaltedTurn
		}
		if te, cutBy, ok := TurnErrorRow(fr.Event); ok {
			sawErr = te.EventID == terr.ID && cutBy == TurnErrorCostCeiling
		}
	}
}
