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
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
)

// The per-turn failure rows (#1258) on the wire. The row contract
// itself is pinned in pkg/attach/wire; these cover the conformance
// fixtures and the replay through a real server.

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
