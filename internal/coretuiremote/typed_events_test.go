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
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

func TestTranslateTypedFrame_StatusUpdate(t *testing.T) {
	ctxPct := 47
	src := &attach.StatusUpdate{
		Model: "claude-opus-4-7", Provider: "anthropic", PermMode: "ask",
		TurnState: "streaming", ContextPct: &ctxPct,
	}
	ev, ok := translateTypedFrame(attach.Frame{Type: attach.EventStatusUpdate, TypedData: src})
	if !ok {
		t.Fatalf("translateTypedFrame !ok")
	}
	if ev.StatusUpdate == nil {
		t.Fatal("StatusUpdate field nil")
	}
	if ev.StatusUpdate.Model != src.Model || ev.StatusUpdate.Provider != src.Provider {
		t.Errorf("StatusUpdate not projected: %+v", *ev.StatusUpdate)
	}
	if ev.StatusUpdate.ContextPct == nil || *ev.StatusUpdate.ContextPct != ctxPct {
		t.Errorf("ContextPct not preserved: %v", ev.StatusUpdate.ContextPct)
	}
}

func TestTranslateTypedFrame_UsageUpdate_WithByModel(t *testing.T) {
	src := &attach.UsageUpdate{
		TokensInTotal: 100, TokensOutTotal: 20, CostUSDTotal: 0.05, TurnsTotal: 3,
		ByModel: map[string]attach.UsageByModel{
			"claude-opus-4-7":  {TokensIn: 80, TokensOut: 15, CostUSD: 0.04, Turns: 2},
			"claude-haiku-4-5": {TokensIn: 20, TokensOut: 5, CostUSD: 0.01, Turns: 1},
		},
	}
	ev, _ := translateTypedFrame(attach.Frame{Type: attach.EventUsageUpdate, TypedData: src})
	if ev.UsageUpdate == nil {
		t.Fatal("UsageUpdate field nil")
	}
	if ev.UsageUpdate.TokensInTotal != 100 || ev.UsageUpdate.CostUSDTotal != 0.05 {
		t.Errorf("totals not projected: %+v", *ev.UsageUpdate)
	}
	if len(ev.UsageUpdate.ByModel) != 2 {
		t.Errorf("ByModel len = %d, want 2", len(ev.UsageUpdate.ByModel))
	}
	if got := ev.UsageUpdate.ByModel["claude-opus-4-7"].TokensIn; got != 80 {
		t.Errorf("ByModel[opus].TokensIn = %d, want 80", got)
	}
	// Snapshot form (no LastTurn) — Usage / CostUSD / Model stay
	// zero-valued so the framework's emitEvent doesn't fabricate a
	// per-turn footer update from cumulative totals.
	if ev.Usage != nil || ev.CostUSD != 0 || ev.Model != "" {
		t.Errorf("snapshot UsageUpdate should not populate per-turn fields: usage=%+v cost=%v model=%q",
			ev.Usage, ev.CostUSD, ev.Model)
	}
}

// TestTranslateTypedFrame_UsageUpdate_LastTurnFansOut pins the
// authoritative per-turn cost path: when UsageUpdate carries LastTurn
// (populated by the server's tracker after each turn commits), the
// translator sets Usage + CostUSD + Model on the coretui.Event so
// emitEvent fans out both usageMsg (per-turn currentCost) AND
// usageUpdateMsg (session totals) from the same wire event. That's
// what makes the remote per-turn footer render "$X.XX" without a
// separate pricing round-trip.
func TestTranslateTypedFrame_UsageUpdate_LastTurnFansOut(t *testing.T) {
	src := &attach.UsageUpdate{
		TokensInTotal: 20_000, TokensOutTotal: 1_000, CostUSDTotal: 0.03, TurnsTotal: 2,
		LastTurn: &attach.UsageLastTurn{
			TokensIn:       10_000,
			TokensInCached: 8_000,
			TokensOut:      500,
			CostUSD:        0.0025,
			Model:          "gemini-3.1-pro",
		},
	}
	ev, _ := translateTypedFrame(attach.Frame{Type: attach.EventUsageUpdate, TypedData: src})
	if ev.UsageUpdate == nil {
		t.Fatal("UsageUpdate field nil")
	}
	if ev.Usage == nil {
		t.Fatal("per-turn Usage not populated from LastTurn — remote footer won't show cost")
	}
	if ev.Usage.InputTokens != 10_000 || ev.Usage.OutputTokens != 500 {
		t.Errorf("per-turn tokens = %+v, want in=10000 out=500", ev.Usage)
	}
	if ev.CostUSD != 0.0025 {
		t.Errorf("per-turn CostUSD = %v, want 0.0025", ev.CostUSD)
	}
	if ev.Model != "gemini-3.1-pro" {
		t.Errorf("per-turn Model = %q, want gemini-3.1-pro", ev.Model)
	}
}

func TestTranslateTypedFrame_Inbox(t *testing.T) {
	queuedAt := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	src := &attach.InboxEvent{State: "queued", PromptID: "p-1", QueuedAt: queuedAt}
	ev, _ := translateTypedFrame(attach.Frame{Type: attach.EventInbox, TypedData: src})
	if ev.Inbox == nil || ev.Inbox.State != "queued" || ev.Inbox.PromptID != "p-1" {
		t.Errorf("Inbox not projected: %+v", ev.Inbox)
	}
	if !ev.Inbox.QueuedAt.Equal(queuedAt) {
		t.Errorf("QueuedAt = %v, want %v", ev.Inbox.QueuedAt, queuedAt)
	}
}

func TestTranslateTypedFrame_TurnComplete_CostDeferred(t *testing.T) {
	// nil CostUSD on the wire → 0 in coretui.TurnSummary per v1.1.0
	// (consumer treats 0 as "cost deferred to following usage-update").
	src := &attach.TurnComplete{
		PromptID: "p-1", Model: "claude-opus-4-7",
		TokensIn: 1000, TokensOut: 50, LatencyMs: 2340,
		// CostUSD intentionally nil
	}
	ev, _ := translateTypedFrame(attach.Frame{Type: attach.EventTurnComplete, TypedData: src})
	if ev.TurnComplete == nil {
		t.Fatal("TurnComplete nil")
	}
	if ev.TurnComplete.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0 (deferred)", ev.TurnComplete.CostUSD)
	}
	if ev.TurnComplete.LatencyMs != 2340 {
		t.Errorf("LatencyMs = %d, want 2340", ev.TurnComplete.LatencyMs)
	}
}

func TestTranslateTypedFrame_TurnComplete_CostInband(t *testing.T) {
	cost := 0.025
	src := &attach.TurnComplete{
		PromptID: "p-1", Model: "claude-opus-4-7",
		TokensIn: 1000, TokensOut: 50, LatencyMs: 2340,
		CostUSD: &cost,
	}
	ev, _ := translateTypedFrame(attach.Frame{Type: attach.EventTurnComplete, TypedData: src})
	if ev.TurnComplete.CostUSD != 0.025 {
		t.Errorf("CostUSD = %v, want 0.025", ev.TurnComplete.CostUSD)
	}
}

func TestTranslateTypedFrame_TurnError(t *testing.T) {
	src := &attach.TurnError{
		Kind: "rate_limited", Code: "429",
		Message: "too many requests", Retryable: true, Hint: "slow down",
	}
	ev, _ := translateTypedFrame(attach.Frame{Type: attach.EventTurnError, TypedData: src})
	if ev.TurnError == nil {
		t.Fatal("TurnError nil")
	}
	if ev.TurnError.Kind != "rate_limited" || ev.TurnError.Code != "429" {
		t.Errorf("TurnError fields not projected: %+v", *ev.TurnError)
	}
	if !ev.TurnError.Retryable {
		t.Error("Retryable should be true")
	}
}

func TestTranslateTypedFrame_CapabilitiesIgnored(t *testing.T) {
	// Capabilities is a handshake frame for Phase 2 negotiation; for
	// v1 we acknowledge and drop. Returning ok=false keeps the event
	// off the operator-facing chat surface.
	src := &attach.Capabilities{
		ProtocolVersion: "1.1.0",
		EventTypes:      []string{attach.EventStatusUpdate},
		Server:          "core-agent",
	}
	_, ok := translateTypedFrame(attach.Frame{Type: attach.EventCapabilities, TypedData: src})
	if ok {
		t.Errorf("Capabilities should not emit a coretui.Event in v1")
	}
}

func TestTranslateTypedFrame_UnknownTypeDropped(t *testing.T) {
	_, ok := translateTypedFrame(attach.Frame{Type: "future-event-9000", TypedData: "anything"})
	if ok {
		t.Errorf("Unknown event type should drop, not emit")
	}
}

func TestTranslateTypedFrame_WrongPayloadTypeDropped(t *testing.T) {
	// Defensive — if a future refactor populates Type+TypedData with
	// mismatched concrete types, we drop silently rather than panic.
	_, ok := translateTypedFrame(attach.Frame{
		Type:      attach.EventStatusUpdate,
		TypedData: &attach.TurnError{Kind: "auth_error"}, // wrong type for this Type
	})
	if ok {
		t.Errorf("Mismatched payload type should drop, not emit")
	}
}

func TestTranslateTypedFrame_NilPayloadDropped(t *testing.T) {
	for _, et := range []string{
		attach.EventStatusUpdate, attach.EventUsageUpdate,
		attach.EventInbox, attach.EventTurnComplete, attach.EventTurnError,
	} {
		_, ok := translateTypedFrame(attach.Frame{Type: et, TypedData: nil})
		if ok {
			t.Errorf("eventType=%q: nil TypedData should drop", et)
		}
	}
}
