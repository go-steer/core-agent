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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/internal/attachclient"
	"github.com/go-steer/core-agent/pkg/attach"
)

// fakeServer wraps an httptest server and lets tests script
// per-endpoint behavior. Frames pushed to streamFrames stream out
// of GET /events one at a time, with optional artificial delays.
type fakeServer struct {
	*httptest.Server

	mu           sync.Mutex
	injects      []string
	streamFrames []attach.Frame
	streamDelay  time.Duration

	// Response stubs (zero values = "implement this when needed").
	usage   attach.UsageInfo
	ctxInfo attach.ContextInfo
}

// startFakeServer returns a server wired with the common attach
// routes — enough for the adapter to issue Stream + Inject + the
// read endpoints. Add per-test scripting via fs.streamFrames etc.
func startFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /sessions/{sid}/inject", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fs.mu.Lock()
		fs.injects = append(fs.injects, body.Message)
		fs.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /sessions/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		fs.mu.Lock()
		frames := append([]attach.Frame(nil), fs.streamFrames...)
		delay := fs.streamDelay
		fs.mu.Unlock()

		for _, f := range frames {
			if r.Context().Err() != nil {
				return
			}
			payload, _ := json.Marshal(f)
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-r.Context().Done():
					return
				}
			}
		}
		// Hold the connection open so the adapter's range-over-channel
		// doesn't see an EOF before the test cancels its ctx.
		<-r.Context().Done()
	})

	mux.HandleFunc("GET /sessions/{sid}/usage", func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		_ = json.NewEncoder(w).Encode(fs.usage)
	})

	mux.HandleFunc("GET /sessions/{sid}/context", func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		_ = json.NewEncoder(w).Encode(fs.ctxInfo)
	})

	mux.HandleFunc("GET /sessions/{sid}/tools", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tools": []attach.ToolInfo{
			{Name: "read_file", Description: "read a file", Source: "builtin", GateState: "allowed"},
		}})
	})

	mux.HandleFunc("GET /sessions/{sid}/agents", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"agents": []attach.AgentInfo{}})
	})

	mux.HandleFunc("GET /sessions/{sid}/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(attach.StatusInfo{State: "idle", ModelName: "claude-opus-4-7"})
	})

	fs.Server = httptest.NewServer(mux)
	t.Cleanup(fs.Close)
	return fs
}

func TestAdapter_Run_StreamsTextUntilTurnComplete(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)

	// Three frames: partial text, final text, turn-complete usage.
	fs.streamFrames = []attach.Frame{
		{Seq: 1, Event: &session.Event{
			Author:      "user",
			LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: "hello"}}}, Partial: false},
		}},
		{Seq: 2, Event: &session.Event{
			Author:      "model",
			LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: "world"}}}, Partial: true},
		}},
		{Seq: 3, Event: &session.Event{
			Author:      "model",
			LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: " more"}}}, Partial: false, TurnComplete: true},
		}},
	}

	parsed, err := attachclient.ParseURL(fs.URL + "/sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var events []coretui.Event
	for ev, err := range a.Run(ctx, "hello") {
		if err != nil {
			t.Fatalf("yield err: %v", err)
		}
		events = append(events, ev)
	}

	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	if events[0].Text != "hello" {
		t.Errorf("event[0].Text = %q", events[0].Text)
	}
	if events[1].Text != "world" || !events[1].Partial {
		t.Errorf("event[1] = %+v", events[1])
	}
	if events[2].Text != " more" || events[2].Partial {
		t.Errorf("event[2] = %+v", events[2])
	}

	// Inject should have been called with the prompt.
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.injects) != 1 || fs.injects[0] != "hello" {
		t.Errorf("injects = %v", fs.injects)
	}
}

func TestAdapter_Run_ContextCancelEndsIterator(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)
	fs.streamFrames = []attach.Frame{} // no frames; iterator only ends on ctx cancel

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	var lastErr error
	for _, err := range a.Run(ctx, "hello") {
		if err != nil {
			lastErr = err
			break
		}
	}
	if lastErr == nil {
		t.Error("expected ctx error, got nil")
	}
}

func TestAdapter_TranslateEvent_ToolCallAndResult(t *testing.T) {
	t.Parallel()
	ev := &session.Event{LLMResponse: model.LLMResponse{
		Content: &genai.Content{Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{ID: "fc-1", Name: "read_file", Args: map[string]any{"path": "/tmp/x"}}},
			{Text: "calling read_file"},
		}},
	}}
	out := translateEvent(ev)
	if out.Text != "calling read_file" {
		t.Errorf("text = %q", out.Text)
	}
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].Name != "read_file" || out.ToolCalls[0].ID != "fc-1" {
		t.Errorf("toolCalls = %+v", out.ToolCalls)
	}

	ev2 := &session.Event{LLMResponse: model.LLMResponse{
		Content: &genai.Content{Parts: []*genai.Part{
			{FunctionResponse: &genai.FunctionResponse{ID: "fc-1", Name: "read_file", Response: map[string]any{"content": "hello"}}},
		}},
	}}
	out2 := translateEvent(ev2)
	if len(out2.ToolResults) != 1 || out2.ToolResults[0].ID != "fc-1" {
		t.Errorf("toolResults = %+v", out2.ToolResults)
	}
	if out2.ToolResults[0].Response["content"] != "hello" {
		t.Errorf("response = %+v", out2.ToolResults[0].Response)
	}
}

func TestAdapter_TranslateEvent_UsageFromMetadata(t *testing.T) {
	t.Parallel()
	ev := &session.Event{LLMResponse: model.LLMResponse{
		Content: &genai.Content{Parts: []*genai.Part{{Text: "done"}}},
		CustomMetadata: map[string]any{
			"usage": map[string]any{
				"input_tokens":  1200,
				"output_tokens": 800,
				"cost_usd":      0.014,
				"model":         "claude-opus-4-7",
			},
		},
	}}
	out := translateEvent(ev)
	if out.Usage == nil || out.Usage.InputTokens != 1200 || out.Usage.OutputTokens != 800 {
		t.Errorf("usage = %+v", out.Usage)
	}
	if out.CostUSD != 0.014 || out.Model != "claude-opus-4-7" {
		t.Errorf("cost/model = %v / %q", out.CostUSD, out.Model)
	}
}

func TestAdapter_Usage_CachedThenRefreshes(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)
	fs.usage = attach.UsageInfo{
		Overall: attach.UsageTotals{InputTokens: 100, OutputTokens: 50, Turns: 2, CostUSD: 0.01},
	}

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	if got := a.SessionTotals(); got.InputTokens != 100 {
		t.Errorf("first SessionTotals = %+v", got)
	}
	if got := a.SessionCostUSD(); got != 0.01 {
		t.Errorf("first SessionCostUSD = %v", got)
	}
	if got := a.SessionTurns(); got != 2 {
		t.Errorf("first SessionTurns = %d", got)
	}

	// Mutate server state. Cache TTL is 2s so a second immediate
	// call should still see the cached value.
	fs.mu.Lock()
	fs.usage.Overall.InputTokens = 9999
	fs.mu.Unlock()
	if got := a.SessionTotals(); got.InputTokens != 100 {
		t.Errorf("cached SessionTotals = %+v, expected stale cache", got)
	}
}

func TestAdapter_SlashContext_ReturnsSystemMessage(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)
	fs.ctxInfo = attach.ContextInfo{Compactions: 2, Checkpoints: 1, SubtaskTurns: 5}

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	res, err := a.InvokeSlash(context.Background(), "context", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.SystemMessage, "Compactions: 2") || !strings.Contains(res.SystemMessage, "Checkpoints: 1") {
		t.Errorf("systemMessage = %q", res.SystemMessage)
	}
}

func TestAdapter_SlashAsync_BtwReturnsModal(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/{sid}/slash/btw", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(attach.SideQueryResponse{Answer: "an answer"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	parsed, _ := attachclient.ParseURL(server.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	preamble, ch := a.InvokeSlashAsync(context.Background(), "btw", "what's up?")
	if preamble == "" {
		t.Error("expected preamble for /btw")
	}
	out := <-ch
	if out.Err != nil {
		t.Fatal(out.Err)
	}
	if out.Res.ModalAnswer == nil || out.Res.ModalAnswer.Answer != "an answer" {
		t.Errorf("modal = %+v", out.Res.ModalAnswer)
	}
}

func TestParsePricingSet(t *testing.T) {
	t.Parallel()
	req, err := parsePricingSet("claude-opus-4-7 15 75")
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "claude-opus-4-7" || req.InputUSDPerMTok != 15 || req.OutputUSDPerMTok != 75 {
		t.Errorf("req = %+v", req)
	}
	if _, err := parsePricingSet("only-model"); err == nil {
		t.Error("expected error on incomplete input")
	}
}

func TestParseSubagentSpec(t *testing.T) {
	t.Parallel()
	spec, err := parseSubagentSpec("watcher watch deployment myapp")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "watcher" || spec.Goal != "watch deployment myapp" {
		t.Errorf("spec = %+v", spec)
	}
	if _, err := parseSubagentSpec(""); err == nil {
		t.Error("expected error on empty args")
	}
}
