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

package effects

import (
	"context"
	"errors"
	"iter"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
)

const (
	testApp  = "effects-test"
	testUser = "op"
)

func sqliteService(t *testing.T) adksession.Service {
	t.Helper()
	svc, err := database.NewSessionService(sqlite.Open(filepath.Join(t.TempDir(), "sessions.db")))
	if err != nil {
		t.Fatalf("NewSessionService: %v", err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	return svc
}

type scriptedModel struct {
	name   string
	script func(req *model.LLMRequest) *model.LLMResponse

	mu   sync.Mutex
	reqs []*model.LLMRequest
}

func (m *scriptedModel) Name() string { return m.name }

func (m *scriptedModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		m.reqs = append(m.reqs, req)
		m.mu.Unlock()
		resp := m.script(req)
		if resp.UsageMetadata == nil {
			resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     10,
				CandidatesTokenCount: 5,
				TotalTokenCount:      15,
			}
		}
		resp.TurnComplete = true
		resp.FinishReason = genai.FinishReasonStop
		yield(resp, nil)
	}
}

func callResponse(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall(name, args)},
		},
	}
}

func textResponse(s string) *model.LLMResponse {
	return &model.LLMResponse{Content: genai.NewContentFromText(s, genai.RoleModel)}
}

func roundScript(calls ...*model.LLMResponse) func(*model.LLMRequest) *model.LLMResponse {
	round := 0
	return func(*model.LLMRequest) *model.LLMResponse {
		defer func() { round++ }()
		if round < len(calls) {
			return calls[round]
		}
		return textResponse("turn done")
	}
}

type scaleArgs struct {
	Delta int `json:"delta"`
}

type harness struct {
	svc     adksession.Service
	runner  *runner.Runner
	model   *scriptedModel
	mu      sync.Mutex
	acks    map[string]time.Time
	scale   int
	list    int
	sawCall map[string]bool
}

func (h *harness) ackEffects(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.acks[sessionID] = time.Now().UTC()
}

func (h *harness) ackedAt(_ context.Context, sessionID string) (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.acks[sessionID]
	return t, ok
}

func newHarness(t *testing.T, script func(*model.LLMRequest) *model.LLMResponse) *harness {
	return newHarnessWith(t, script, nil)
}

func newHarnessWith(t *testing.T, script func(*model.LLMRequest) *model.LLMResponse, external func(context.Context, string) []DanglingIntent) *harness {
	t.Helper()
	h := &harness{
		svc:     sqliteService(t),
		model:   &scriptedModel{name: "scripted", script: script},
		acks:    map[string]time.Time{},
		sawCall: map[string]bool{},
	}

	scaleTool, err := functiontool.New(functiontool.Config{
		Name:        "scale_up",
		Description: "mutating test tool",
	}, func(ctx adkagent.Context, args scaleArgs) (map[string]any, error) {
		h.mu.Lock()
		h.scale++
		h.mu.Unlock()
		resp, gerr := h.svc.Get(ctx, &adksession.GetRequest{
			AppName: testApp, UserID: testUser, SessionID: ctx.SessionID(),
		})
		if gerr != nil {
			return nil, gerr
		}
		h.mu.Lock()
		h.sawCall[ctx.FunctionCallID()] = hasFunctionCall(resp.Session.Events(), ctx.FunctionCallID())
		h.mu.Unlock()
		return map[string]any{"scaled": args.Delta}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New(scale_up): %v", err)
	}
	listTool, err := functiontool.New(functiontool.Config{
		Name:        "list_pods",
		Description: "read-only test tool (via override)",
	}, func(ctx adkagent.Context, args struct{}) (map[string]any, error) {
		h.mu.Lock()
		h.list++
		h.mu.Unlock()
		return map[string]any{"pods": []string{"a", "b"}}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New(list_pods): %v", err)
	}

	root, err := llmagent.New(llmagent.Config{
		Name:        "effects_agent",
		Description: "outbox test agent",
		Model:       h.model,
		Tools:       []tool.Tool{scaleTool, listTool},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	f := false
	p, err := New(Config{
		Predicate:        NewPredicate(Overrides(nil, []ToolPolicy{{Name: "list_pods", Mutating: &f}})),
		AckedAt:          h.ackedAt,
		ExternalDangling: external,
	})
	if err != nil {
		t.Fatalf("effects.New: %v", err)
	}
	h.runner, err = runner.New(runner.Config{
		AppName:           testApp,
		Agent:             root,
		SessionService:    h.svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{p}},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return h
}

func hasFunctionCall(events adksession.Events, callID string) bool {
	for ev := range events.All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionCall != nil && p.FunctionCall.ID == callID {
				return true
			}
		}
	}
	return false
}

func (h *harness) run(t *testing.T, sessionID, text string) []*adksession.Event {
	t.Helper()
	var events []*adksession.Event
	msg := genai.NewContentFromText(text, genai.RoleUser)
	for ev, err := range h.runner.Run(context.Background(), testUser, sessionID, msg, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner.Run: %v", err)
		}
		events = append(events, ev)
	}
	return events
}

func seedDangling(t *testing.T, svc adksession.Service, sessionID, toolName, callID string, longRunning bool) {
	t.Helper()
	ctx := context.Background()
	created, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: testApp, UserID: testUser, SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", sessionID, err)
	}
	ev := adksession.NewEvent(ctx, "prior-inv")
	ev.Author = "effects_agent"
	part := genai.NewPartFromFunctionCall(toolName, map[string]any{"delta": 1})
	part.FunctionCall.ID = callID
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}}
	if longRunning {
		ev.LongRunningToolIDs = []string{callID}
	}
	if err := svc.AppendEvent(ctx, created.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
}

func toolResponses(events []*adksession.Event) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	for _, ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionResponse != nil {
				out[p.FunctionResponse.Name] = append(out[p.FunctionResponse.Name], p.FunctionResponse.Response)
			}
		}
	}
	return out
}

func TestFunctionCallDurableBeforeToolRun(t *testing.T) {
	h := newHarness(t, roundScript(callResponse("scale_up", map[string]any{"delta": 2})))
	h.run(t, "clean-session", "scale it")

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scale != 1 {
		t.Fatalf("scale_up executed %d times, want 1", h.scale)
	}
	if len(h.sawCall) != 1 {
		t.Fatalf("recorded %d call-durability probes, want 1", len(h.sawCall))
	}
	for id, durable := range h.sawCall {
		if !durable {
			t.Errorf("FunctionCall %s was NOT durable before tool.Run", id)
		}
	}
}

func TestDanglingMutatingIntentRefusesMutatingCalls(t *testing.T) {
	h := newHarness(t, roundScript(
		callResponse("scale_up", map[string]any{"delta": 3}),
		callResponse("list_pods", map[string]any{}),
	))
	seedDangling(t, h.svc, "wounded", "scale_up", "call-prior-1", false)

	events := h.run(t, "wounded", "continue the work")

	h.mu.Lock()
	scale, list := h.scale, h.list
	h.mu.Unlock()
	if scale != 0 {
		t.Fatalf("scale_up executed %d times in ambiguous-effect mode, want 0 (fail-closed)", scale)
	}
	if list != 1 {
		t.Fatalf("list_pods executed %d times, want 1 (read-only tools proceed in ambiguous mode)", list)
	}
	resps := toolResponses(events)
	if got := resps["scale_up"]; len(got) != 1 || got[0]["error"] != "ambiguous_prior_effect" {
		t.Fatalf("scale_up response = %v, want one ambiguous_prior_effect refusal", got)
	}
}

func TestPausedHITLSessionDoesNotTripMode(t *testing.T) {
	h := newHarness(t, roundScript(callResponse("scale_up", map[string]any{"delta": 1})))
	seedDangling(t, h.svc, "paused", "adk_request_input", "interrupt-1", false)

	h.run(t, "paused", "resume-ish turn")

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scale != 1 {
		t.Fatalf("scale_up executed %d times on a paused-HITL session, want 1", h.scale)
	}
}

func TestLongRunningCallDoesNotTripMode(t *testing.T) {
	h := newHarness(t, roundScript(callResponse("scale_up", map[string]any{"delta": 1})))
	seedDangling(t, h.svc, "longrun", "scale_up", "call-lr-1", true)

	h.run(t, "longrun", "next turn")

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scale != 1 {
		t.Fatalf("scale_up executed %d times with a pending long-running call, want 1", h.scale)
	}
}

func TestAckLiftsAmbiguousMode(t *testing.T) {
	h := newHarness(t, roundScript(callResponse("scale_up", map[string]any{"delta": 4})))
	seedDangling(t, h.svc, "acked", "scale_up", "call-prior-2", false)
	h.ackEffects("acked")
	time.Sleep(5 * time.Millisecond)

	h.run(t, "acked", "continue after ack")

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.scale != 1 {
		t.Fatalf("scale_up executed %d times after operator ack, want 1", h.scale)
	}
}

func TestScanDangling(t *testing.T) {
	pred := NewPredicate(nil)
	mk := func(inv string, parts ...*genai.Part) *adksession.Event {
		ev := adksession.NewEvent(context.Background(), inv)
		ev.Content = &genai.Content{Role: genai.RoleModel, Parts: parts}
		return ev
	}
	call := func(name, id string) *genai.Part {
		p := genai.NewPartFromFunctionCall(name, map[string]any{})
		p.FunctionCall.ID = id
		return p
	}
	respPart := func(name, id string) *genai.Part {
		p := genai.NewPartFromFunctionResponse(name, map[string]any{"ok": true})
		p.FunctionResponse.ID = id
		return p
	}

	events := eventList{
		mk("inv1", call("scale_up", "c1")),
		mk("inv1", respPart("scale_up", "c1")),
		mk("inv2", call("scale_up", "c2")),
		mk("inv2", call("adk_request_input", "i1")),
		mk("inv2", call("invoke_specialist", "c3")),
	}
	st := scanHistory(events, pred, nil)
	got := st.dangling
	if len(got) != 2 {
		t.Fatalf("scanHistory found %d intents (%v), want 2", len(got), got)
	}
	if got[0].CallID != "c2" || got[1].CallID != "c3" {
		t.Fatalf("scanHistory order = %s,%s want c2,c3", got[0].CallID, got[1].CallID)
	}
	if resp, ok := st.completions["c1"]; !ok || resp["ok"] != true {
		t.Fatalf("completions[c1] = %v,%v want the recorded payload", resp, ok)
	}
}

func TestScanDanglingBuckets(t *testing.T) {
	pred := NewPredicate(map[string]bool{"list_pods": false})
	subAgents := map[string]bool{"triage_bot": true}
	mk := func(inv string, parts ...*genai.Part) *adksession.Event {
		ev := adksession.NewEvent(context.Background(), inv)
		ev.Content = &genai.Content{Role: genai.RoleModel, Parts: parts}
		return ev
	}
	call := func(name, id string) *genai.Part {
		p := genai.NewPartFromFunctionCall(name, map[string]any{})
		p.FunctionCall.ID = id
		return p
	}
	respPart := func(name, id string) *genai.Part {
		p := genai.NewPartFromFunctionResponse(name, map[string]any{"ok": true})
		p.FunctionResponse.ID = id
		return p
	}

	t.Run("split", func(t *testing.T) {
		lrEvent := mk("inv0", call("scale_up", "lr1"))
		lrEvent.LongRunningToolIDs = []string{"lr1"}
		events := eventList{
			lrEvent,
			mk("inv1", call("scale_up", "m1")),
			mk("inv1", call("list_pods", "r1")),
			mk("inv1", call("triage_bot", "d1")),
			mk("inv1", call("adk_request_input", "x1")),
			mk("inv1", call("scale_up", "p1")),
			mk("inv1", respPart("scale_up", "p1")),
		}
		ds := ScanDangling(events, pred, subAgents)
		if len(ds.Mutating) != 1 || ds.Mutating[0].CallID != "m1" {
			t.Fatalf("Mutating = %+v, want [m1]", ds.Mutating)
		}
		if len(ds.Repairable) != 1 || ds.Repairable[0].CallID != "r1" || ds.Repairable[0].EventIndex != 2 {
			t.Fatalf("Repairable = %+v, want [r1@idx2]", ds.Repairable)
		}
		if len(ds.Deferred) != 3 {
			t.Fatalf("Deferred = %+v, want 3 (long-running, sub-agent, control)", ds.Deferred)
		}
		if ds.LastCallEventIndex != 5 {
			t.Fatalf("LastCallEventIndex = %d, want 5", ds.LastCallEventIndex)
		}
	})
}

// TestMast561_ConfirmationApprovedThenInterruptedIsDangling verifies the
// mast #561 fix in pairScan:
//  1. While a mutating call is parked awaiting confirmation, its
//     placeholder FunctionResponse keeps the call in open with
//     excluded=true (Mutating=0, Deferred=2).
//  2. Once the operator approves via adk_request_confirmation
//     (confirmed=true) and the process crashes before the tool's real
//     FunctionResponse is written, pairScan flips open[origID].excluded
//     to false and updates its timestamp to the confirmation event's
//     timestamp so ScanDangling reports Mutating=1 and the next turn
//     refuses mutating calls in ambiguous_prior_effect mode.
//  3. If the operator rejects (confirmed=false or verdict=reject),
//     open[origID] is deleted so it never reports as dangling.
//  4. A synthetic tail-repair event (CustomMetadata[core_agent_synthetic_repair]=true)
//     does NOT close the dangling call.
func TestMast561_ConfirmationApprovedThenInterruptedIsDangling(t *testing.T) {
	pred := NewPredicate(nil)
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(10 * time.Second)
	t2 := t0.Add(20 * time.Second)

	// Event 1: model emits mutating FunctionCall scale_up (call-1).
	evCall := adksession.NewEvent(context.Background(), "inv-1")
	evCall.Timestamp = t0
	fc := genai.NewPartFromFunctionCall("scale_up", map[string]any{"delta": 3})
	fc.FunctionCall.ID = "call-1"
	evCall.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{fc}}

	// Event 2: ADK emits placeholder FunctionResponse for call-1 with RequestedToolConfirmations["call-1"].
	evPlaceholder := adksession.NewEvent(context.Background(), "inv-1")
	evPlaceholder.Timestamp = t0.Add(time.Second)
	frPlaceholder := genai.NewPartFromFunctionResponse("scale_up", map[string]any{"status": "awaiting_operator_approval"})
	frPlaceholder.FunctionResponse.ID = "call-1"
	evPlaceholder.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{frPlaceholder}}
	evPlaceholder.Actions.RequestedToolConfirmations = map[string]toolconfirmation.ToolConfirmation{
		"call-1": {Hint: "approve scale_up?"},
	}

	// Event 3: ADK emits long-running FunctionCall adk_request_confirmation (conf-1) referencing call-1.
	evConfCall := adksession.NewEvent(context.Background(), "inv-1")
	evConfCall.Timestamp = t0.Add(2 * time.Second)
	fcConf := genai.NewPartFromFunctionCall(toolconfirmation.FunctionCallName, map[string]any{
		"originalFunctionCall": map[string]any{"id": "call-1", "name": "scale_up"},
	})
	fcConf.FunctionCall.ID = "conf-1"
	evConfCall.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{fcConf}}
	evConfCall.LongRunningToolIDs = []string{"conf-1"}

	// Step A: While parked awaiting operator decision, call-1 is NOT dangling (Mutating=0).
	parkedEvents := eventList{evCall, evPlaceholder, evConfCall}
	scanParked := ScanDangling(parkedEvents, pred, nil)
	if len(scanParked.Mutating) != 0 {
		t.Errorf("while parked: ScanDangling.Mutating = %+v, want empty", scanParked.Mutating)
	}
	if len(scanParked.Deferred) != 2 {
		t.Errorf("while parked: ScanDangling.Deferred = %+v, want 2 (call-1 and conf-1)", scanParked.Deferred)
	}

	// Step B: Operator approves in inv-1 (ADK v2 reuses the paused turn's InvocationID
	// across resume turns), then process crashes before call-1's real FunctionResponse is written.
	evConfApprove := adksession.NewEvent(context.Background(), "inv-1")
	evConfApprove.Timestamp = t1
	frConfApprove := genai.NewPartFromFunctionResponse(toolconfirmation.FunctionCallName, map[string]any{
		"confirmed": true,
		"payload":   map[string]any{"verdict": "approve"},
	})
	frConfApprove.FunctionResponse.ID = "conf-1"
	evConfApprove.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{frConfApprove}}

	crashedAfterApprove := eventList{evCall, evPlaceholder, evConfCall, evConfApprove}
	scanCrashed := ScanDangling(crashedAfterApprove, pred, nil)
	if len(scanCrashed.Mutating) != 1 {
		t.Errorf("after post-approval crash (#561): ScanDangling.Mutating = %+v, want 1 (call-1)", scanCrashed.Mutating)
	} else {
		if scanCrashed.Mutating[0].CallID != "call-1" || scanCrashed.Mutating[0].ToolName != "scale_up" {
			t.Errorf("after post-approval crash: Mutating[0] = %+v, want call-1/scale_up", scanCrashed.Mutating[0])
		}
		if !scanCrashed.Mutating[0].Timestamp.Equal(t1) {
			t.Errorf("after post-approval crash: Mutating[0].Timestamp = %v, want %v (confirmation timestamp)", scanCrashed.Mutating[0].Timestamp, t1)
		}
	}

	// Step B2: When beforeRun runs at the start of the confirmation turn (evConfApprove is
	// the trailing event), scanHistoryForRun exempts call-1 so it can execute; on a
	// subsequent turn under the same reused "inv-1" (after another user event is appended),
	// scanHistoryForRun reports call-1 as dangling!
	stAtConfirmTurn := scanHistoryForRun(crashedAfterApprove, pred, nil)
	if len(stAtConfirmTurn.dangling) != 0 {
		t.Errorf("at start of confirmation turn: scanHistoryForRun.dangling = %+v, want 0 (trailing confirmation is being dispatched)", stAtConfirmTurn.dangling)
	}
	evNextResumeUser := adksession.NewEvent(context.Background(), "inv-1")
	evNextResumeUser.Timestamp = t2
	evNextResumeUser.Content = genai.NewContentFromText("continue under reused inv-1", genai.RoleUser)
	stOnNextTurnSameInv := scanHistoryForRun(eventList{evCall, evPlaceholder, evConfCall, evConfApprove, evNextResumeUser}, pred, nil)
	if len(stOnNextTurnSameInv.dangling) != 1 || stOnNextTurnSameInv.dangling[0].CallID != "call-1" {
		t.Errorf("on subsequent turn under reused inv-1 after crash: scanHistoryForRun.dangling = %+v, want [call-1]", stOnNextTurnSameInv.dangling)
	}

	// Step B3: JSON-string wrapped confirmation response ({"response": "{\"confirmed\":true,...}"})
	// and contradictory verdict ({"confirmed": true, "payload": {"verdict": "reject"}}).
	if !IsConfirmationApproved(map[string]any{"response": `{"confirmed":true,"payload":{"verdict":"approve"}}`}) {
		t.Error("IsConfirmationApproved(wrapped approve) = false, want true")
	}
	if IsConfirmationApproved(map[string]any{"response": `{"confirmed":true,"payload":{"verdict":"reject"}}`}) {
		t.Error("IsConfirmationApproved(wrapped contradictory reject) = true, want false")
	}
	if IsConfirmationApproved(map[string]any{"confirmed": true, "payload": map[string]any{"verdict": "reject"}}) {
		t.Error("IsConfirmationApproved(top-level contradictory reject) = true, want false")
	}

	// Step C: Synthetic tail-repair event for call-1 must NOT mask the dangling mutating call.
	evSyntheticRepair := adksession.NewEvent(context.Background(), "inv-3")
	evSyntheticRepair.Timestamp = t2
	frRepair := genai.NewPartFromFunctionResponse("scale_up", map[string]any{"error": "[interrupted before tool result was recorded]"})
	frRepair.FunctionResponse.ID = "call-1"
	evSyntheticRepair.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{frRepair}}
	evSyntheticRepair.CustomMetadata = map[string]any{SyntheticRepairMetadataKey: true}

	withSynthetic := eventList{evCall, evPlaceholder, evConfCall, evConfApprove, evSyntheticRepair}
	scanWithSynthetic := ScanDangling(withSynthetic, pred, nil)
	if len(scanWithSynthetic.Mutating) != 1 || scanWithSynthetic.Mutating[0].CallID != "call-1" {
		t.Errorf("after synthetic tail repair: ScanDangling.Mutating = %+v, want [call-1] (synthetic repair must be ignored)", scanWithSynthetic.Mutating)
	}

	// Step D: Real completion closes call-1 and records it in completions.
	evRealDone := adksession.NewEvent(context.Background(), "inv-2")
	evRealDone.Timestamp = t2
	frDone := genai.NewPartFromFunctionResponse("scale_up", map[string]any{"scaled": 3})
	frDone.FunctionResponse.ID = "call-1"
	evRealDone.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{frDone}}

	completedEvents := eventList{evCall, evPlaceholder, evConfCall, evConfApprove, evRealDone}
	scanCompleted := ScanDangling(completedEvents, pred, nil)
	if len(scanCompleted.Mutating) != 0 {
		t.Fatalf("after real completion: ScanDangling.Mutating = %+v, want 0", scanCompleted.Mutating)
	}
	stCompleted := scanHistory(completedEvents, pred, nil)
	if got := stCompleted.completions["call-1"]; got == nil || got["scaled"] != 3 {
		t.Fatalf("after real completion: completions[call-1] = %v, want {scaled: 3}", got)
	}

	// Step E: Operator rejection deletes call-1 from open so it is neither Mutating nor Deferred.
	evConfReject := adksession.NewEvent(context.Background(), "inv-2")
	evConfReject.Timestamp = t1
	frConfReject := genai.NewPartFromFunctionResponse(toolconfirmation.FunctionCallName, map[string]any{
		"confirmed": false,
		"payload":   map[string]any{"verdict": "reject"},
	})
	frConfReject.FunctionResponse.ID = "conf-1"
	evConfReject.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{frConfReject}}

	rejectedEvents := eventList{evCall, evPlaceholder, evConfCall, evConfReject}
	scanRejected := ScanDangling(rejectedEvents, pred, nil)
	if len(scanRejected.Mutating) != 0 || len(scanRejected.Deferred) != 0 {
		t.Fatalf("after operator rejection: Mutating=%+v Deferred=%+v, want both empty", scanRejected.Mutating, scanRejected.Deferred)
	}
}

type eventList []*adksession.Event

func (e eventList) All() iter.Seq[*adksession.Event] {
	return func(yield func(*adksession.Event) bool) {
		for _, ev := range e {
			if !yield(ev) {
				return
			}
		}
	}
}
func (e eventList) Len() int                   { return len(e) }
func (e eventList) At(i int) *adksession.Event { return e[i] }

func fixedIDCallResponse(name, id string, args map[string]any) *model.LLMResponse {
	r := callResponse(name, args)
	r.Content.Parts[0].FunctionCall.ID = id
	return r
}

func seedPair(t *testing.T, svc adksession.Service, sessionID, toolName, callID string, response map[string]any) {
	t.Helper()
	ctx := context.Background()
	created, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName: testApp, UserID: testUser, SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", sessionID, err)
	}
	callEv := adksession.NewEvent(ctx, "prior-inv")
	callEv.Author = "effects_agent"
	cp := genai.NewPartFromFunctionCall(toolName, map[string]any{"delta": 1})
	cp.FunctionCall.ID = callID
	callEv.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{cp}}
	if err := svc.AppendEvent(ctx, created.Session, callEv); err != nil {
		t.Fatalf("AppendEvent(call): %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	respEv := adksession.NewEvent(ctx, "prior-inv")
	respEv.Author = "effects_agent"
	rp := genai.NewPartFromFunctionResponse(toolName, response)
	rp.FunctionResponse.ID = callID
	respEv.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{rp}}
	if err := svc.AppendEvent(ctx, created.Session, respEv); err != nil {
		t.Fatalf("AppendEvent(response): %v", err)
	}
	time.Sleep(5 * time.Millisecond)
}

func TestExactKeyReplayEndToEnd(t *testing.T) {
	h := newHarness(t, roundScript(
		fixedIDCallResponse("scale_up", "call-done-1", map[string]any{"delta": 9}),
	))
	seedPair(t, h.svc, "replayed", "scale_up", "call-done-1", map[string]any{"scaled": 42})

	events := h.run(t, "replayed", "do it again")

	h.mu.Lock()
	scale := h.scale
	h.mu.Unlock()
	if scale != 0 {
		t.Fatalf("scale_up executed %d times, want 0", scale)
	}
	resps := toolResponses(events)
	got := resps["scale_up"]
	if len(got) != 1 {
		t.Fatalf("scale_up responses = %v, want 1", got)
	}
	if v, ok := got[0]["scaled"]; !ok || (v != float64(42) && v != 42) {
		t.Fatalf("replayed response = %v, want {scaled: 42}", got[0])
	}
}

func TestExactKeyReplayNilResponse(t *testing.T) {
	h := newHarness(t, roundScript(
		fixedIDCallResponse("scale_up", "call-done-2", map[string]any{"delta": 1}),
	))
	seedPair(t, h.svc, "nilresp", "scale_up", "call-done-2", nil)

	events := h.run(t, "nilresp", "again")

	h.mu.Lock()
	scale := h.scale
	h.mu.Unlock()
	if scale != 0 {
		t.Fatalf("scale_up executed %d times, want 0", scale)
	}
	resps := toolResponses(events)
	if got := resps["scale_up"]; len(got) != 1 || got[0]["mast_replayed_effect"] != true {
		t.Fatalf("scale_up response = %v, want explicit nil-payload replay marker", got)
	}
}

func confirmProbe(t *testing.T, withOutbox bool) int {
	t.Helper()
	svc := sqliteService(t)

	executed := 0
	scaleTool, err := functiontool.New(functiontool.Config{
		Name:        "scale_up",
		Description: "mutating tool gated behind an operator confirmation",
	}, func(ctx adkagent.Context, args scaleArgs) (map[string]any, error) {
		if c := ctx.ToolConfirmation(); c == nil {
			if err := ctx.RequestConfirmation("scale the deployment?", map[string]any{"delta": args.Delta}); err != nil {
				return nil, err
			}
			return map[string]any{"status": "awaiting operator confirmation"}, nil
		} else if !c.Confirmed {
			return map[string]any{"status": "rejected"}, nil
		}
		executed++
		return map[string]any{"scaled": args.Delta}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	m := &scriptedModel{name: "confirm", script: roundScript(
		callResponse("scale_up", map[string]any{"delta": 3}),
	)}
	root, err := llmagent.New(llmagent.Config{
		Name:        "confirm_agent",
		Description: "confirmation probe",
		Instruction: "act",
		Model:       m,
		Tools:       []tool.Tool{scaleTool},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	rc := runner.Config{
		AppName:           testApp,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
	}
	if withOutbox {
		p, err := New(Config{
			Predicate:     NewPredicate(nil),
			SubAgentNames: SubAgentNames(root),
		})
		if err != nil {
			t.Fatalf("effects.New: %v", err)
		}
		rc.PluginConfig = runner.PluginConfig{Plugins: []*plugin.Plugin{p}}
	}
	r, err := runner.New(rc)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	const sid = "confirm-session"
	run := func(msg *genai.Content) {
		t.Helper()
		for _, err := range r.Run(context.Background(), testUser, sid, msg, adkagent.RunConfig{}) {
			if err != nil {
				t.Fatalf("runner.Run: %v", err)
			}
		}
	}

	run(genai.NewContentFromText("scale it up", genai.RoleUser))

	got, err := svc.Get(context.Background(), &adksession.GetRequest{
		AppName: testApp, UserID: testUser, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	confID := ""
	for ev := range got.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionCall != nil && p.FunctionCall.Name == toolconfirmation.FunctionCallName {
				confID = p.FunctionCall.ID
			}
		}
	}
	if confID == "" {
		t.Fatalf("no %s function call emitted", toolconfirmation.FunctionCallName)
	}

	fr := genai.NewPartFromFunctionResponse(toolconfirmation.FunctionCallName, map[string]any{
		"confirmed": true,
		"hint":      "scale the deployment?",
	})
	fr.FunctionResponse.ID = confID
	run(&genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{fr}})

	return executed
}

func TestConfirmationResumeIsNotFalselyReplayed(t *testing.T) {
	baseline := confirmProbe(t, false)
	if baseline != 1 {
		t.Fatalf("baseline (no outbox): approved tool executed %d time(s), want 1", baseline)
	}
	withPlugin := confirmProbe(t, true)
	if withPlugin != 1 {
		t.Fatalf("with outbox: approved tool executed %d time(s), want 1", withPlugin)
	}
}

type fakeIntentStore struct {
	recorded        []DanglingIntent
	specialistsSeen []string
	completed       []string
	recordErr       error
	doneErr         error
}

func (f *fakeIntentStore) RecordSubRunIntents(_ context.Context, _, specialist string, intents []DanglingIntent) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.recorded = append(f.recorded, intents...)
	f.specialistsSeen = append(f.specialistsSeen, specialist)
	return nil
}

func (f *fakeIntentStore) CompleteSubRunIntents(_ context.Context, _ string, callIDs []string) error {
	if f.doneErr != nil {
		return f.doneErr
	}
	f.completed = append(f.completed, callIDs...)
	return nil
}

func subRunEvent(inv string, parts ...*genai.Part) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), inv)
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: parts}
	ev.Timestamp = time.Now()
	return ev
}

func fcPart(name, id string) *genai.Part {
	p := genai.NewPartFromFunctionCall(name, map[string]any{})
	p.FunctionCall.ID = id
	return p
}

func frPart(name, id string) *genai.Part {
	p := genai.NewPartFromFunctionResponse(name, map[string]any{"ok": true})
	p.FunctionResponse.ID = id
	return p
}

func TestSubRunRecorderRecordsThenPairs(t *testing.T) {
	store := &fakeIntentStore{}
	rec, err := NewSubRunRecorder(SubRunRecorderConfig{
		Store:      store,
		SessionID:  "s1",
		Specialist: "remediator",
		Predicate:  NewPredicate(nil),
	})
	if err != nil {
		t.Fatalf("NewSubRunRecorder: %v", err)
	}

	if err := rec.Observe(subRunEvent("inv1", fcPart("scale_up", "c1"))); err != nil {
		t.Fatalf("Observe call: %v", err)
	}
	if len(store.recorded) != 1 || store.recorded[0].CallID != "c1" {
		t.Fatalf("recorded = %+v, want c1", store.recorded)
	}
	if err := rec.Observe(subRunEvent("inv1", frPart("scale_up", "c1"))); err != nil {
		t.Fatalf("Observe response: %v", err)
	}
	if len(store.completed) != 1 || store.completed[0] != "c1" {
		t.Fatalf("completed = %v, want [c1]", store.completed)
	}
}

func TestSubRunRecorderStopsTheDispatchOnRecordFailure(t *testing.T) {
	boom := errors.New("ops row unwritable")
	store := &fakeIntentStore{recordErr: boom}
	rec, err := NewSubRunRecorder(SubRunRecorderConfig{
		Store: store, SessionID: "s1", Specialist: "remediator",
		Predicate: NewPredicate(map[string]bool{"list_pods": false}),
	})
	if err != nil {
		t.Fatalf("NewSubRunRecorder: %v", err)
	}
	if err := rec.Observe(subRunEvent("inv1", fcPart("scale_up", "c1"))); !errors.Is(err, boom) {
		t.Fatalf("Observe error = %v, want wrap of %v", err, boom)
	}
}

func TestSubSessionScannerFor(t *testing.T) {
	svc := sqliteService(t)
	pred := NewPredicate(map[string]bool{"read_file": false})

	// Seed a parent spawning call and a child session with a dangling mutating call.
	childSID := DeriveSubSessionID("parent-1", "task", "spawn-1")
	seedDangling(t, svc, childSID, "scale_up", "child-mut-1", false)

	scanner := SubSessionScannerFor(svc, testApp, testUser, pred, nil)
	dangling, ok := scanner(context.Background(), "parent-1", DanglingIntent{
		ToolName: "task",
		CallID:   "spawn-1",
	})
	if !ok {
		t.Fatal("SubSessionScannerFor returned inspected=false, want true")
	}
	if len(dangling) != 1 || dangling[0].CallID != "child-mut-1" {
		t.Fatalf("child dangling = %+v, want [child-mut-1]", dangling)
	}
}
