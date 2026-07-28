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

// Offline double for the streaming glue (#396): GenerateContent is
// driven end-to-end against an httptest server that speaks the real
// Messages API SSE wire format (message_start → content_block_* →
// message_delta → message_stop), so the SDK's own decoder and
// accumulator run for real. No credentials, no network.
//
// The tests construct the unexported llm directly with a client
// pointed at the fake server (same-package test), so no production
// seam was needed.

package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// messagesSSEFixture is a canonical streaming response: two text
// deltas, then a tool_use block whose input arrives via two
// input_json_delta chunks, closed by a message_delta carrying the
// stop_reason + final output-token count.
const messagesSSEFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_test_01","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":25,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":" \"Paris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":15}}

event: message_stop
data: {"type":"message_stop"}

`

// pauseTurnSSEFixture is request #1 of a long server-side web_search
// turn: the model issues a server_tool_use invocation, then the API
// pauses the turn (stop_reason pause_turn) because the server-side
// tool loop ran long. The adapter must NOT treat this as terminal — it
// must replay the paused assistant message and re-issue the request.
const pauseTurnSSEFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_pause_01","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":30,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_01","name":"web_search","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":" \"latest Go release\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"pause_turn","stop_sequence":null},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

// webSearchDoneSSEFixture is request #2: the resumed turn returns the
// search results (web_search_tool_result) followed by the model's
// answer text, and ends normally.
const webSearchDoneSSEFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_done_01","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":50,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_01","content":[{"type":"web_search_result","title":"Go 1.26 released","url":"https://go.dev/blog/go1.26","encrypted_content":"opaque-encrypted-blob","page_age":"2 days ago"}]}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Go 1.26 shipped"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" two days ago."}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":12}}

event: message_stop
data: {"type":"message_stop"}

`

// capturedRequest is what the fake Messages endpoint saw — used to
// assert the adapter built the request correctly.
type capturedRequest struct {
	path   string
	method string
	body   map[string]any
}

// newOfflineLLM stands up a fake Messages API endpoint that replies
// with sse, and returns an llm wired to it plus a channel delivering
// the captured request.
func newOfflineLLM(t *testing.T, modelID, sse string) (*llm, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		captured.method = r.Method
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured.body)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sse)
	}))
	t.Cleanup(srv.Close)

	return &llm{
		client: sdk.NewClient(
			option.WithAPIKey("test-key-not-real"),
			option.WithBaseURL(srv.URL),
		),
		modelID:  modelID,
		builtins: BuiltinTools{}, // no server-side built-ins in the fixture
	}, captured
}

// newOfflineLLMSeq is newOfflineLLM for multi-request turns: request
// #i is answered with sses[i] (the last fixture repeats once the list
// is exhausted, so a fixture ending in pause_turn simulates a server
// that pauses forever). Every request body is captured in order.
func newOfflineLLMSeq(t *testing.T, modelID string, sses []string) (*llm, *[]capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	captured := &[]capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		req := capturedRequest{path: r.URL.Path, method: r.Method}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req.body)
		*captured = append(*captured, req)
		i := len(*captured) - 1
		mu.Unlock()

		if i >= len(sses) {
			i = len(sses) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sses[i])
	}))
	t.Cleanup(srv.Close)

	return &llm{
		client: sdk.NewClient(
			option.WithAPIKey("test-key-not-real"),
			option.WithBaseURL(srv.URL),
		),
		modelID:  modelID,
		builtins: BuiltinTools{WebSearch: true},
	}, captured
}

func userText(text string) []*genai.Content {
	return []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: text}},
	}}
}

func TestGenerateContent_OfflineStream_PartialsAndFinal(t *testing.T) {
	t.Parallel()
	l, captured := newOfflineLLM(t, "claude-test", messagesSSEFixture)

	var partials []*adkmodel.LLMResponse
	var final *adkmodel.LLMResponse
	for resp, err := range l.GenerateContent(context.Background(), &adkmodel.LLMRequest{
		Contents: userText("what's the weather in Paris?"),
	}, true) {
		if err != nil {
			t.Fatalf("GenerateContent yielded error: %v", err)
		}
		if resp.Partial {
			partials = append(partials, resp)
			continue
		}
		if final != nil {
			t.Fatalf("more than one terminal response: %+v then %+v", final, resp)
		}
		final = resp
	}

	// --- request shape ---
	if captured.method != http.MethodPost || captured.path != "/v1/messages" {
		t.Errorf("request = %s %s, want POST /v1/messages", captured.method, captured.path)
	}
	if got := captured.body["model"]; got != "claude-test" {
		t.Errorf("request model = %v, want claude-test (llm.modelID must backfill an empty LLMRequest.Model)", got)
	}
	if got := captured.body["stream"]; got != true {
		t.Errorf("request stream = %v, want true", got)
	}

	// --- partial text events ---
	wantPartials := []string{"Hello", " world"}
	if len(partials) != len(wantPartials) {
		t.Fatalf("got %d partials, want %d: %+v", len(partials), len(wantPartials), partials)
	}
	for i, want := range wantPartials {
		p := partials[i]
		if !p.Partial || p.TurnComplete {
			t.Errorf("partial[%d] flags = Partial:%v TurnComplete:%v, want Partial-only", i, p.Partial, p.TurnComplete)
		}
		if p.Content == nil || p.Content.Role != genai.RoleModel ||
			len(p.Content.Parts) != 1 || p.Content.Parts[0].Text != want {
			t.Errorf("partial[%d] = %+v, want single model-role text part %q", i, p.Content, want)
		}
	}

	// --- terminal response ---
	if final == nil {
		t.Fatal("no terminal (TurnComplete) response was yielded")
	}
	if !final.TurnComplete {
		t.Error("terminal response TurnComplete = false, want true")
	}
	if final.Content == nil || final.Content.Role != genai.RoleModel {
		t.Fatalf("terminal content = %+v, want model-role content", final.Content)
	}
	if len(final.Content.Parts) != 2 {
		t.Fatalf("terminal content has %d parts, want 2 (text + tool_use): %+v", len(final.Content.Parts), final.Content.Parts)
	}
	if got := final.Content.Parts[0].Text; got != "Hello world" {
		t.Errorf("terminal text = %q, want the full accumulated \"Hello world\"", got)
	}
	fc := final.Content.Parts[1].FunctionCall
	if fc == nil {
		t.Fatalf("terminal part[1] = %+v, want a FunctionCall", final.Content.Parts[1])
	}
	if fc.ID != "toolu_01" || fc.Name != "get_weather" {
		t.Errorf("FunctionCall id/name = %q/%q, want toolu_01/get_weather", fc.ID, fc.Name)
	}
	if wantArgs := map[string]any{"city": "Paris"}; !reflect.DeepEqual(fc.Args, wantArgs) {
		t.Errorf("FunctionCall args = %#v, want %#v (accumulated across input_json_delta chunks)", fc.Args, wantArgs)
	}

	// stop_reason tool_use maps to FinishReasonStop per the design
	// table (the runner treats tool dispatch as a normal stop).
	if final.FinishReason != genai.FinishReasonStop {
		t.Errorf("FinishReason = %v, want %v", final.FinishReason, genai.FinishReasonStop)
	}

	// Usage mapping: input from message_start, output replaced by the
	// message_delta's final count (15, not the boot value 1).
	u := final.UsageMetadata
	if u == nil {
		t.Fatal("terminal response carries no UsageMetadata")
	}
	if u.PromptTokenCount != 25 || u.CandidatesTokenCount != 15 || u.TotalTokenCount != 40 {
		t.Errorf("usage = prompt:%d candidates:%d total:%d, want 25/15/40",
			u.PromptTokenCount, u.CandidatesTokenCount, u.TotalTokenCount)
	}
	if u.CachedContentTokenCount != 0 {
		t.Errorf("CachedContentTokenCount = %d, want 0 (fixture has no cache reads)", u.CachedContentTokenCount)
	}
}

// TestGenerateContent_OfflineStream_ExplicitModelWins pins that a
// non-empty LLMRequest.Model rides through to the wire untouched.
func TestGenerateContent_OfflineStream_ExplicitModelWins(t *testing.T) {
	t.Parallel()
	l, captured := newOfflineLLM(t, "claude-provider-default", messagesSSEFixture)

	for resp, err := range l.GenerateContent(context.Background(), &adkmodel.LLMRequest{
		Model:    "claude-explicit-override",
		Contents: userText("hi"),
	}, true) {
		if err != nil {
			t.Fatalf("GenerateContent yielded error: %v", err)
		}
		_ = resp
	}
	if got := captured.body["model"]; got != "claude-explicit-override" {
		t.Errorf("request model = %v, want claude-explicit-override", got)
	}
}

// TestGenerateContent_OfflineStream_HTTPErrorYieldsError drives the
// non-2xx path: the iterator must yield exactly one inline error and
// stop, never a TurnComplete.
func TestGenerateContent_OfflineStream_HTTPErrorYieldsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 400 is deliberately non-retryable — a 5xx would make the
		// SDK's default retry policy sleep through backoffs.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"broken on purpose"}}`)
	}))
	t.Cleanup(srv.Close)

	l := &llm{
		client: sdk.NewClient(
			option.WithAPIKey("test-key-not-real"),
			option.WithBaseURL(srv.URL),
		),
		modelID: "claude-test",
	}

	var errs []error
	var responses []*adkmodel.LLMResponse
	for resp, err := range l.GenerateContent(context.Background(), &adkmodel.LLMRequest{
		Contents: userText("hi"),
	}, true) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		responses = append(responses, resp)
	}
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want exactly 1: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "anthropic: stream:") {
		t.Errorf("error = %q, want the \"anthropic: stream:\" prefix", errs[0].Error())
	}
	for _, r := range responses {
		if r.TurnComplete {
			t.Errorf("a TurnComplete response was yielded despite the HTTP error: %+v", r)
		}
	}
}

// TestGenerateContent_PauseTurnContinuation drives the two-request
// web-search turn end to end (#461): request #1 ends in pause_turn
// with a server_tool_use block, so the adapter must replay the paused
// assistant message verbatim and re-issue; request #2 completes the
// turn. Asserts the request count, the replayed blocks, the terminal
// content, the summed usage, and that server-side tool blocks are not
// surfaced as parts.
func TestGenerateContent_PauseTurnContinuation(t *testing.T) {
	t.Parallel()
	l, captured := newOfflineLLMSeq(t, "claude-test",
		[]string{pauseTurnSSEFixture, webSearchDoneSSEFixture})

	var partials []string
	var final *adkmodel.LLMResponse
	for resp, err := range l.GenerateContent(context.Background(), &adkmodel.LLMRequest{
		Contents: userText("what's the latest Go release?"),
	}, true) {
		if err != nil {
			t.Fatalf("GenerateContent yielded error: %v", err)
		}
		if resp.Partial {
			partials = append(partials, resp.Content.Parts[0].Text)
			continue
		}
		if final != nil {
			t.Fatalf("more than one terminal response: %+v then %+v", final, resp)
		}
		final = resp
	}

	// --- exactly two requests were issued ---
	if len(*captured) != 2 {
		t.Fatalf("adapter issued %d requests, want 2 (initial + one pause_turn continuation)", len(*captured))
	}

	// --- request #1: just the user turn ---
	firstMsgs, _ := (*captured)[0].body["messages"].([]any)
	if len(firstMsgs) != 1 {
		t.Fatalf("request #1 has %d messages, want 1 (user only): %+v", len(firstMsgs), firstMsgs)
	}

	// --- request #2: user turn + the replayed paused assistant turn ---
	secondMsgs, _ := (*captured)[1].body["messages"].([]any)
	if len(secondMsgs) != 2 {
		t.Fatalf("request #2 has %d messages, want 2 (user + replayed assistant): %+v", len(secondMsgs), secondMsgs)
	}
	replayed, _ := secondMsgs[1].(map[string]any)
	if got := replayed["role"]; got != "assistant" {
		t.Errorf("replayed message role = %v, want assistant", got)
	}
	blocks, _ := replayed["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("replayed message has %d blocks, want 1 (server_tool_use): %+v", len(blocks), blocks)
	}
	block, _ := blocks[0].(map[string]any)
	if block["type"] != "server_tool_use" || block["id"] != "srvtoolu_01" || block["name"] != "web_search" {
		t.Errorf("replayed block = %+v, want server_tool_use srvtoolu_01/web_search", block)
	}
	if input, _ := block["input"].(map[string]any); input["query"] != "latest Go release" {
		t.Errorf("replayed block input = %+v, want the accumulated {query: latest Go release}", block["input"])
	}

	// --- partial text kept flowing during the continuation ---
	if want := []string{"Go 1.26 shipped", " two days ago."}; !reflect.DeepEqual(partials, want) {
		t.Errorf("partials = %q, want %q (continuation deltas surfaced)", partials, want)
	}

	// --- terminal response carries the completed text only ---
	if final == nil {
		t.Fatal("no terminal (TurnComplete) response was yielded")
	}
	if final.FinishReason != genai.FinishReasonStop {
		t.Errorf("FinishReason = %v, want %v (end_turn after the continuation)", final.FinishReason, genai.FinishReasonStop)
	}
	if len(final.Content.Parts) != 1 || final.Content.Parts[0].Text != "Go 1.26 shipped two days ago." {
		t.Errorf("terminal parts = %+v, want the single completed text part (server_tool_use / web_search_tool_result blocks are not surfaced)", final.Content.Parts)
	}
	for i, p := range final.Content.Parts {
		if p.FunctionCall != nil {
			t.Errorf("part[%d] surfaced a FunctionCall %+v — server_tool_use must not become a dispatchable call", i, p.FunctionCall)
		}
	}

	// --- usage is the SUM across both requests ---
	u := final.UsageMetadata
	if u == nil {
		t.Fatal("terminal response carries no UsageMetadata")
	}
	if u.PromptTokenCount != 30+50 || u.CandidatesTokenCount != 7+12 || u.TotalTokenCount != 30+50+7+12 {
		t.Errorf("usage = prompt:%d candidates:%d total:%d, want 80/19/99 (summed across the paused and resumed requests)",
			u.PromptTokenCount, u.CandidatesTokenCount, u.TotalTokenCount)
	}
}

// TestGenerateContent_PauseTurnLoopBound pins the runaway guard: a
// server that answers pause_turn forever must stop the adapter at
// 1 + maxPauseTurnContinuations requests with a clean terminal
// response (FinishReasonOther) rather than hanging or erroring.
func TestGenerateContent_PauseTurnLoopBound(t *testing.T) {
	t.Parallel()
	l, captured := newOfflineLLMSeq(t, "claude-test", []string{pauseTurnSSEFixture})

	var final *adkmodel.LLMResponse
	for resp, err := range l.GenerateContent(context.Background(), &adkmodel.LLMRequest{
		Contents: userText("search forever"),
	}, true) {
		if err != nil {
			t.Fatalf("GenerateContent yielded error: %v", err)
		}
		if resp.Partial {
			continue
		}
		if final != nil {
			t.Fatalf("more than one terminal response: %+v then %+v", final, resp)
		}
		final = resp
	}

	wantRequests := 1 + maxPauseTurnContinuations
	if len(*captured) != wantRequests {
		t.Fatalf("adapter issued %d requests, want %d (initial + %d bounded continuations)",
			len(*captured), wantRequests, maxPauseTurnContinuations)
	}
	// Each continuation replays everything accumulated so far: request
	// #N carries the user turn plus N-1 paused assistant messages.
	lastMsgs, _ := (*captured)[wantRequests-1].body["messages"].([]any)
	if len(lastMsgs) != 1+maxPauseTurnContinuations {
		t.Errorf("last request has %d messages, want %d (user + %d replayed assistant turns)",
			len(lastMsgs), 1+maxPauseTurnContinuations, maxPauseTurnContinuations)
	}

	if final == nil {
		t.Fatal("no terminal response after hitting the continuation cap")
	}
	if final.FinishReason != genai.FinishReasonOther {
		t.Errorf("FinishReason = %v, want %v (pause_turn at the cap maps to Other)", final.FinishReason, genai.FinishReasonOther)
	}
	if !final.TurnComplete {
		t.Error("terminal response TurnComplete = false, want true")
	}
	// Usage still reflects every request made.
	if u := final.UsageMetadata; u == nil || u.PromptTokenCount != int32(30*wantRequests) || u.CandidatesTokenCount != int32(7*wantRequests) {
		t.Errorf("usage = %+v, want prompt:%d candidates:%d (summed across all %d requests)",
			final.UsageMetadata, 30*wantRequests, 7*wantRequests, wantRequests)
	}
}
