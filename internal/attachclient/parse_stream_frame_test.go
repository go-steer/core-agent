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

package attachclient

import (
	"testing"

	"github.com/go-steer/core-agent/pkg/attach"
)

func TestParseStreamFrame_LegacyAgent(t *testing.T) {
	// Legacy "event: agent" data carries the full Frame shape — seq +
	// session.Event payload. Empty event type defaults to legacy too,
	// matching the standard SSE "no event line means default" rule.
	for _, et := range []string{"", attach.EventAgent} {
		raw := `{"seq": 42, "event": {"id": "evt-1", "author": "user"}}`
		frame, ok := parseStreamFrame(et, raw)
		if !ok {
			t.Fatalf("eventType=%q: parseStreamFrame returned !ok", et)
		}
		if frame.Seq != 42 {
			t.Errorf("eventType=%q: Seq = %d, want 42", et, frame.Seq)
		}
		if frame.Event == nil || frame.Event.ID != "evt-1" {
			t.Errorf("eventType=%q: Event not preserved: %+v", et, frame.Event)
		}
		if frame.Type != "" {
			t.Errorf("eventType=%q: legacy frame should have empty Type, got %q", et, frame.Type)
		}
	}
}

func TestParseStreamFrame_StatusUpdate(t *testing.T) {
	contextPct := 12
	raw := `{"model":"claude-opus-4-7","provider":"anthropic","perm_mode":"ask","turn_state":"idle","context_pct":12}`
	frame, ok := parseStreamFrame(attach.EventStatusUpdate, raw)
	if !ok {
		t.Fatalf("parseStreamFrame returned !ok")
	}
	if frame.Type != attach.EventStatusUpdate {
		t.Errorf("Type = %q, want %q", frame.Type, attach.EventStatusUpdate)
	}
	p, isStatus := frame.TypedData.(*attach.StatusUpdate)
	if !isStatus || p == nil {
		t.Fatalf("TypedData = %T (%v), want *attach.StatusUpdate non-nil", frame.TypedData, frame.TypedData)
	}
	if p.Model != "claude-opus-4-7" || p.Provider != "anthropic" || p.PermMode != "ask" || p.TurnState != "idle" {
		t.Errorf("StatusUpdate fields not preserved: %+v", *p)
	}
	if p.ContextPct == nil || *p.ContextPct != contextPct {
		t.Errorf("ContextPct = %v, want pointer to 12", p.ContextPct)
	}
}

func TestParseStreamFrame_UsageUpdate(t *testing.T) {
	raw := `{
		"tokens_in_total": 12345,
		"tokens_out_total": 678,
		"cost_usd_total": 0.0421,
		"turns_total": 4,
		"by_model": {
			"claude-opus-4-7": {"tokens_in": 10000, "tokens_out": 500, "cost_usd": 0.04, "turns": 3},
			"claude-haiku-4-5": {"tokens_in": 2345, "tokens_out": 178, "cost_usd": 0.0021, "turns": 1}
		}
	}`
	frame, ok := parseStreamFrame(attach.EventUsageUpdate, raw)
	if !ok {
		t.Fatalf("parseStreamFrame returned !ok")
	}
	p := frame.TypedData.(*attach.UsageUpdate)
	if p.TokensInTotal != 12345 || p.TokensOutTotal != 678 || p.TurnsTotal != 4 {
		t.Errorf("totals not preserved: %+v", *p)
	}
	if len(p.ByModel) != 2 {
		t.Errorf("by_model: got %d entries, want 2", len(p.ByModel))
	}
	if got := p.ByModel["claude-opus-4-7"].TokensIn; got != 10000 {
		t.Errorf("by_model[opus].TokensIn = %d, want 10000", got)
	}
}

func TestParseStreamFrame_Inbox(t *testing.T) {
	raw := `{"state":"queued","prompt_id":"prompt-99","queued_at":"2026-06-09T10:00:00Z"}`
	frame, ok := parseStreamFrame(attach.EventInbox, raw)
	if !ok {
		t.Fatalf("parseStreamFrame returned !ok")
	}
	p := frame.TypedData.(*attach.InboxEvent)
	if p.State != "queued" || p.PromptID != "prompt-99" {
		t.Errorf("Inbox fields not preserved: %+v", *p)
	}
	if p.QueuedAt.IsZero() {
		t.Errorf("QueuedAt not parsed")
	}
}

func TestParseStreamFrame_TurnComplete_CostDeferred(t *testing.T) {
	// v1.1.0 cost-deferred case: cost_usd is omitted from the wire,
	// CostUSD remains nil — caller treats nil as "see next usage-update."
	raw := `{"prompt_id":"prompt-1","model":"claude-opus-4-7","tokens_in":1000,"tokens_out":50,"latency_ms":2340}`
	frame, ok := parseStreamFrame(attach.EventTurnComplete, raw)
	if !ok {
		t.Fatalf("parseStreamFrame returned !ok")
	}
	p := frame.TypedData.(*attach.TurnComplete)
	if p.CostUSD != nil {
		t.Errorf("CostUSD = %v, want nil (cost-deferred per v1.1.0)", *p.CostUSD)
	}
	if p.LatencyMs != 2340 {
		t.Errorf("LatencyMs = %d, want 2340", p.LatencyMs)
	}
}

func TestParseStreamFrame_TurnComplete_CostInband(t *testing.T) {
	raw := `{"prompt_id":"prompt-1","model":"claude-opus-4-7","tokens_in":1000,"tokens_out":50,"cost_usd":0.025,"latency_ms":2340}`
	frame, _ := parseStreamFrame(attach.EventTurnComplete, raw)
	p := frame.TypedData.(*attach.TurnComplete)
	if p.CostUSD == nil || *p.CostUSD != 0.025 {
		t.Errorf("CostUSD = %v, want pointer to 0.025", p.CostUSD)
	}
}

func TestParseStreamFrame_TurnError(t *testing.T) {
	raw := `{"kind":"rate_limited","code":"429","message":"too many requests","retryable":true,"hint":"slow down"}`
	frame, ok := parseStreamFrame(attach.EventTurnError, raw)
	if !ok {
		t.Fatalf("parseStreamFrame returned !ok")
	}
	p := frame.TypedData.(*attach.TurnError)
	if p.Kind != "rate_limited" || p.Code != "429" || !p.Retryable || p.Hint != "slow down" {
		t.Errorf("TurnError fields not preserved: %+v", *p)
	}
}

func TestParseStreamFrame_Capabilities(t *testing.T) {
	raw := `{"protocol_version":"1.1.0","event_types":["status-update","usage-update","inbox","turn-complete","turn-error"],"server":"core-agent"}`
	frame, ok := parseStreamFrame(attach.EventCapabilities, raw)
	if !ok {
		t.Fatalf("parseStreamFrame returned !ok")
	}
	p := frame.TypedData.(*attach.Capabilities)
	if p.ProtocolVersion != "1.1.0" {
		t.Errorf("ProtocolVersion = %q, want 1.1.0", p.ProtocolVersion)
	}
	if len(p.EventTypes) != 5 {
		t.Errorf("EventTypes len = %d, want 5", len(p.EventTypes))
	}
}

func TestParseStreamFrame_UnknownEventTypeDropped(t *testing.T) {
	// Forward-compat: unknown event names are silently dropped per
	// spec §3 so future servers don't crash older clients.
	_, ok := parseStreamFrame("future-event-9000", `{"anything":1}`)
	if ok {
		t.Errorf("parseStreamFrame should drop unknown event types, returned ok=true")
	}
}

func TestParseStreamFrame_MalformedJSONDropped(t *testing.T) {
	// Don't crash the stream on a malformed data block; tolerated as
	// no-op same as unknown event types.
	for _, et := range []string{
		"",
		attach.EventStatusUpdate,
		attach.EventUsageUpdate,
		attach.EventInbox,
		attach.EventTurnComplete,
		attach.EventTurnError,
		attach.EventCapabilities,
	} {
		_, ok := parseStreamFrame(et, `{not json}`)
		if ok {
			t.Errorf("eventType=%q: parseStreamFrame returned ok=true on malformed JSON", et)
		}
	}
}
