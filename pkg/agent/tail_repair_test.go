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

package agent

import (
	"context"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// defaultAgentName mirrors defaultOptions().name — the Author ADK
// stamps on this agent's model-response and tool-response events.
const defaultAgentName = "core_agent"

// newTailRepairAgent builds an agent over the given service with a
// fixed session triple so tests can seed history first.
func newTailRepairAgent(t *testing.T, svc session.Service) *Agent {
	t.Helper()
	a, err := New(&recordingLLM{}, WithSessionService(svc), WithSession("tail-user", "tail-sid"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// seedTailSession creates the (DefaultAppName, tail-user, tail-sid)
// session and appends the given events in order.
func seedTailSession(t *testing.T, svc session.Service, events ...*session.Event) {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.Create(ctx, &session.CreateRequest{AppName: DefaultAppName, UserID: "tail-user", SessionID: "tail-sid"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	resp, err := svc.Get(ctx, &session.GetRequest{AppName: DefaultAppName, UserID: "tail-user", SessionID: "tail-sid"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, ev := range events {
		if err := svc.AppendEvent(ctx, resp.Session, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
}

// tailEvents re-reads the seeded session's full event list.
func tailEvents(t *testing.T, svc session.Service) []*session.Event {
	t.Helper()
	resp, err := svc.Get(context.Background(), &session.GetRequest{AppName: DefaultAppName, UserID: "tail-user", SessionID: "tail-sid"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var out []*session.Event
	for ev := range resp.Session.Events().All() {
		out = append(out, ev)
	}
	return out
}

func userTextEvent(text string) *session.Event {
	ev := session.NewEvent("inv-user")
	ev.Author = "user"
	ev.LLMResponse = adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: text}}},
	}
	return ev
}

func callEvent(author, invocationID string, longRunning []string, calls ...*genai.FunctionCall) *session.Event {
	ev := session.NewEvent(invocationID)
	ev.Author = author
	ev.LongRunningToolIDs = longRunning
	parts := []*genai.Part{{Text: "calling a tool"}}
	for _, fc := range calls {
		parts = append(parts, &genai.Part{FunctionCall: fc})
	}
	ev.LLMResponse = adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: parts},
	}
	return ev
}

func responseEvent(author string, resps ...*genai.FunctionResponse) *session.Event {
	ev := session.NewEvent("inv-resp")
	ev.Author = author
	var parts []*genai.Part
	for _, fr := range resps {
		parts = append(parts, &genai.Part{FunctionResponse: fr})
	}
	ev.LLMResponse = adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleUser, Parts: parts},
	}
	return ev
}

// syntheticResponses returns the repair events appended after the
// seeded prefix, verifying each is a tail-repair row.
func syntheticResponses(t *testing.T, svc session.Service, seeded int) []*session.Event {
	t.Helper()
	events := tailEvents(t, svc)
	if len(events) < seeded {
		t.Fatalf("session lost events: have %d, seeded %d", len(events), seeded)
	}
	extra := events[seeded:]
	for _, ev := range extra {
		if ev.CustomMetadata["kind"] != tailRepairKind {
			t.Errorf("appended event kind = %v, want %q", ev.CustomMetadata["kind"], tailRepairKind)
		}
	}
	return extra
}

func TestTailRepair_DanglingCallGetsSynthesizedResponse(t *testing.T) {
	t.Parallel()
	svc := session.InMemoryService()
	seedTailSession(t, svc,
		userTextEvent("run the thing"),
		callEvent(defaultAgentName, "inv-1", nil, &genai.FunctionCall{ID: "call-1", Name: "bash", Args: map[string]any{"cmd": "ls"}}),
	)
	a := newTailRepairAgent(t, svc)
	a.repairDanglingToolCalls(context.Background())

	extra := syntheticResponses(t, svc, 2)
	if len(extra) != 1 {
		t.Fatalf("appended %d events, want 1", len(extra))
	}
	ev := extra[0]
	if ev.Author != defaultAgentName {
		t.Errorf("Author = %q, want %q (a foreign author would be textified by ADK and the pair would stay broken)", ev.Author, defaultAgentName)
	}
	if ev.InvocationID != "inv-1" {
		t.Errorf("InvocationID = %q, want inv-1", ev.InvocationID)
	}
	if n := len(ev.Content.Parts); n != 1 {
		t.Fatalf("synthetic event has %d parts, want 1", n)
	}
	fr := ev.Content.Parts[0].FunctionResponse
	if fr == nil || fr.ID != "call-1" || fr.Name != "bash" {
		t.Fatalf("synthetic FunctionResponse = %+v, want ID=call-1 Name=bash", fr)
	}
	if msg, _ := fr.Response["error"].(string); !strings.Contains(msg, "interrupted") {
		t.Errorf("synthetic response error = %q, want interruption notice", msg)
	}

	// Idempotent: a second pass sees the pair matched and appends nothing.
	a.repairDanglingToolCalls(context.Background())
	if extra := syntheticResponses(t, svc, 2); len(extra) != 1 {
		t.Errorf("second repair pass appended events: total extra = %d, want still 1", len(extra))
	}
}

func TestTailRepair_MatchedPairUntouched(t *testing.T) {
	t.Parallel()
	svc := session.InMemoryService()
	seedTailSession(t, svc,
		userTextEvent("run the thing"),
		callEvent(defaultAgentName, "inv-1", nil, &genai.FunctionCall{ID: "call-1", Name: "bash"}),
		responseEvent(defaultAgentName, &genai.FunctionResponse{ID: "call-1", Name: "bash", Response: map[string]any{"output": "ok"}}),
	)
	a := newTailRepairAgent(t, svc)
	a.repairDanglingToolCalls(context.Background())
	if extra := syntheticResponses(t, svc, 3); len(extra) != 0 {
		t.Errorf("repair appended %d events to a healthy history, want 0", len(extra))
	}
}

func TestTailRepair_AnswersOnlyUnansweredCallsInEvent(t *testing.T) {
	t.Parallel()
	svc := session.InMemoryService()
	seedTailSession(t, svc,
		callEvent(defaultAgentName, "inv-1", nil,
			&genai.FunctionCall{ID: "call-a", Name: "bash"},
			&genai.FunctionCall{ID: "call-b", Name: "read_file"},
		),
		responseEvent(defaultAgentName, &genai.FunctionResponse{ID: "call-a", Name: "bash", Response: map[string]any{"output": "ok"}}),
	)
	a := newTailRepairAgent(t, svc)
	a.repairDanglingToolCalls(context.Background())

	extra := syntheticResponses(t, svc, 2)
	if len(extra) != 1 {
		t.Fatalf("appended %d events, want 1", len(extra))
	}
	parts := extra[0].Content.Parts
	if len(parts) != 1 || parts[0].FunctionResponse == nil || parts[0].FunctionResponse.ID != "call-b" {
		t.Fatalf("synthetic parts = %+v, want exactly one response for call-b", parts)
	}
}

func TestTailRepair_OneSyntheticEventPerDamagedCallEvent(t *testing.T) {
	t.Parallel()
	svc := session.InMemoryService()
	seedTailSession(t, svc,
		callEvent(defaultAgentName, "inv-1", nil, &genai.FunctionCall{ID: "call-1", Name: "bash"}),
		callEvent(defaultAgentName, "inv-2", nil, &genai.FunctionCall{ID: "call-2", Name: "read_file"}),
	)
	a := newTailRepairAgent(t, svc)
	a.repairDanglingToolCalls(context.Background())

	// One merged event answering both would be relocated after EACH
	// call event by ADK's rearrangement, duplicating responses — the
	// repair must emit one event per damaged call event.
	extra := syntheticResponses(t, svc, 2)
	if len(extra) != 2 {
		t.Fatalf("appended %d events, want 2 (one per damaged call event)", len(extra))
	}
	for i, wantID := range []string{"call-1", "call-2"} {
		parts := extra[i].Content.Parts
		if len(parts) != 1 || parts[0].FunctionResponse == nil || parts[0].FunctionResponse.ID != wantID {
			t.Errorf("synthetic event %d parts = %+v, want single response for %s", i, parts, wantID)
		}
	}
}

func TestTailRepair_SkipsForeignAuthorLongRunningAndControlCalls(t *testing.T) {
	t.Parallel()
	svc := session.InMemoryService()
	seedTailSession(t, svc,
		// A background subagent's in-flight call must not be stomped:
		// it may still be executing, and ADK textifies foreign-author
		// parts anyway so it can't poison this agent's requests.
		callEvent("bg_subagent", "inv-f", nil, &genai.FunctionCall{ID: "call-f", Name: "bash"}),
		// Long-running calls legitimately get their responses in a
		// later user turn.
		callEvent(defaultAgentName, "inv-lr", []string{"call-lr"}, &genai.FunctionCall{ID: "call-lr", Name: "poll_job"}),
		// Control-flow pseudo-calls are excluded from LLM contents by
		// ADK, so an unanswered one is harmless.
		callEvent(defaultAgentName, "inv-c", nil,
			&genai.FunctionCall{ID: "call-c1", Name: confirmationCallName},
			&genai.FunctionCall{ID: "call-c2", Name: credentialCallName},
		),
	)
	a := newTailRepairAgent(t, svc)
	a.repairDanglingToolCalls(context.Background())
	if extra := syntheticResponses(t, svc, 3); len(extra) != 0 {
		t.Errorf("repair appended %d events, want 0 (foreign / long-running / control calls must be skipped)", len(extra))
	}
}

func TestTailRepair_SkipsCallParkedBehindConfirmation(t *testing.T) {
	t.Parallel()
	svc := session.InMemoryService()
	// ADK's RequireConfirmation flow: the original call sits unanswered
	// (and NOT long-running-marked) while the adk_request_confirmation
	// pseudo-call awaits the user. Args reference the original call —
	// exercise both the live-struct and DB-round-trip (nested map)
	// encodings.
	seedTailSession(t, svc,
		callEvent(defaultAgentName, "inv-1", nil,
			&genai.FunctionCall{ID: "call-parked", Name: "bash"},
			&genai.FunctionCall{ID: "call-parked-2", Name: "read_file"},
		),
		callEvent(defaultAgentName, "inv-1", []string{"conf-1", "conf-2"},
			&genai.FunctionCall{ID: "conf-1", Name: confirmationCallName, Args: map[string]any{
				"originalFunctionCall": &genai.FunctionCall{ID: "call-parked", Name: "bash"},
			}},
			&genai.FunctionCall{ID: "conf-2", Name: confirmationCallName, Args: map[string]any{
				"originalFunctionCall": map[string]any{"id": "call-parked-2", "name": "read_file"},
			}},
		),
	)
	a := newTailRepairAgent(t, svc)
	a.repairDanglingToolCalls(context.Background())
	if extra := syntheticResponses(t, svc, 2); len(extra) != 0 {
		t.Errorf("repair appended %d events, want 0 — calls awaiting confirmation must not be answered", len(extra))
	}
}

func TestTailRepair_NoSessionIsNoop(t *testing.T) {
	t.Parallel()
	svc := session.InMemoryService()
	a := newTailRepairAgent(t, svc)
	// Must not create the session or panic — AutoCreateSession owns
	// first-turn creation.
	a.repairDanglingToolCalls(context.Background())
	resp, err := svc.Get(context.Background(), &session.GetRequest{AppName: DefaultAppName, UserID: "tail-user", SessionID: "tail-sid"})
	if err == nil && resp != nil && resp.Session != nil {
		t.Error("repair created a session on the no-session path")
	}
}

// TestRun_RepairsDanglingTailBeforeModelCall is the end-to-end
// guarantee: a session whose last turn died mid-tool produces a
// well-formed LLM request on the next Run — every functionCall in the
// request contents is answered.
func TestRun_RepairsDanglingTailBeforeModelCall(t *testing.T) {
	t.Parallel()
	svc := session.InMemoryService()
	seedTailSession(t, svc,
		userTextEvent("run the thing"),
		callEvent(defaultAgentName, "inv-1", nil, &genai.FunctionCall{ID: "call-1", Name: "bash", Args: map[string]any{"cmd": "ls"}}),
	)
	rec := &recordingLLM{}
	a, err := New(rec, WithSessionService(svc), WithSession("tail-user", "tail-sid"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, err := range a.Run(context.Background(), "and now?") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	req := rec.lastRequest()
	if req == nil {
		t.Fatal("LLM was never invoked")
	}
	calls, responses := 0, 0
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			if p.FunctionCall != nil {
				calls++
			}
			if p.FunctionResponse != nil {
				responses++
				if !strings.Contains(strings.ToLower(strings.TrimSpace(anyToString(p.FunctionResponse.Response["error"]))), "interrupted") {
					t.Errorf("repaired response payload = %v, want interruption notice", p.FunctionResponse.Response)
				}
			}
		}
	}
	if calls != 1 || responses != 1 {
		t.Errorf("request has %d functionCalls and %d functionResponses, want 1 and 1 (dangling call must be answered)", calls, responses)
	}
}

func anyToString(v any) string {
	s, _ := v.(string)
	return s
}
