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
	"math"
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

	mu             sync.Mutex
	injects        []string
	streamFrames   []attach.Frame
	streamDelay    time.Duration
	streamHoldOpen bool // when false, the SSE handler returns after draining frames (simulates daemon EOF)

	// Response stubs (zero values = "implement this when needed").
	usage   attach.UsageInfo
	ctxInfo attach.ContextInfo
}

// startFakeServer returns a server wired with the common attach
// routes — enough for the adapter to issue Stream + Inject + the
// read endpoints. Add per-test scripting via fs.streamFrames etc.
func startFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := &fakeServer{streamHoldOpen: true}
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
		holdOpen := fs.streamHoldOpen
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
		// Hold the connection open by default so the adapter's
		// range-over-channel doesn't see an EOF before the test
		// cancels its ctx. Tests that want to simulate a daemon
		// disconnect (e.g., reconnect-path coverage) set
		// streamHoldOpen=false and let the handler return after
		// draining frames.
		if holdOpen {
			<-r.Context().Done()
		}
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

// TestAdapter_LastTurn_FallsBackToUsageSnapshot pins the fix for the
// observer-mode /stats "Last turn: 0" bug (companion to core-tui #57):
// when the streaming Run/Events loop hasn't populated a.lastTurn (which
// happens when the operator attaches after the last non-partial
// UsageMetadata event, or in LiveAgent mode when text-bearing chunks
// don't reach applyPricing), LastTurn() must fall back to the last
// per-turn entry from the /usage snapshot rather than reporting
// zeros. That entry carries authoritative server-side cost.
func TestAdapter_LastTurn_FallsBackToUsageSnapshot(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)
	fs.usage = attach.UsageInfo{
		Overall: attach.UsageTotals{InputTokens: 74450, OutputTokens: 1349, Turns: 3, CostUSD: 0.1238},
		PerTurn: []attach.UsageTurn{
			{Turn: 1, InputTokens: 23183, OutputTokens: 255, CostUSD: 0.0371},
			{Turn: 2, InputTokens: 24711, OutputTokens: 472, CostUSD: 0.0413},
			{Turn: 3, InputTokens: 26556, OutputTokens: 622, CostUSD: 0.045432},
		},
	}

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	// a.lastTurn is zero — no streaming events observed yet.
	// LastTurn() should read the tail per_turn entry from the
	// snapshot rather than returning zeros.
	got, cost := a.LastTurn()
	if got.InputTokens != 26556 || got.OutputTokens != 622 {
		t.Errorf("fallback LastTurn tokens = %+v, want in=26556 out=622", got)
	}
	if cost != 0.045432 {
		t.Errorf("fallback LastTurn cost = %v, want 0.045432 (server-authoritative)", cost)
	}
}

// TestAdapter_LastTurn_LiveStateWinsOverFallback ensures the fallback
// activates ONLY when the streaming loop hasn't populated a.lastTurn.
// When live-stream state is present, we prefer it (fresher data than
// the 2s-TTL /usage cache).
func TestAdapter_LastTurn_LiveStateWinsOverFallback(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)
	fs.usage = attach.UsageInfo{
		Overall: attach.UsageTotals{Turns: 1},
		PerTurn: []attach.UsageTurn{
			{Turn: 1, InputTokens: 100, OutputTokens: 10, CostUSD: 0.001},
		},
	}

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	// Simulate the Run/Events loop having observed a fresher turn.
	a.mu.Lock()
	a.lastTurn = coretui.Usage{InputTokens: 500, OutputTokens: 50}
	a.pricingIn = 1.0
	a.pricingOut = 2.0
	a.mu.Unlock()

	got, cost := a.LastTurn()
	if got.InputTokens != 500 || got.OutputTokens != 50 {
		t.Errorf("live-state LastTurn = %+v, want in=500 out=50 (not the snapshot's 100/10)", got)
	}
	// Cost from client-side rates: 500 * 1.0/M + 50 * 2.0/M = 0.0006
	if math.Abs(cost-0.0006) > 1e-9 {
		t.Errorf("live-state LastTurn cost = %v, want 0.0006 (client-computed, not snapshot's 0.001)", cost)
	}
}

// TestAdapter_LastTurn_ZeroWhenNoDataAnywhere pins the empty-session
// edge case — no live state, no per_turn entries. Must not panic and
// should return zero values (previous behavior preserved).
func TestAdapter_LastTurn_ZeroWhenNoDataAnywhere(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)
	fs.usage = attach.UsageInfo{
		Overall: attach.UsageTotals{Turns: 0},
	}

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	got, cost := a.LastTurn()
	if got.InputTokens != 0 || got.OutputTokens != 0 || cost != 0 {
		t.Errorf("empty-session LastTurn = %+v cost=%v, want all zero", got, cost)
	}
}

// TestAdapter_ContextWindow_ResolvesFromPerTurnModel pins the fix for
// the remote-TUI "/stats Context: (unknown)" regression. The adapter
// used to hardcode 0 for both ContextWindowSize/Used; it now sources
// the last per-turn model from the /usage snapshot, delegates the
// window-size lookup to pkg/usage (same table the in-process TUI
// uses), and returns the last turn's InputTokens as the fill.
func TestAdapter_ContextWindow_ResolvesFromPerTurnModel(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)
	fs.usage = attach.UsageInfo{
		Overall: attach.UsageTotals{InputTokens: 20049, OutputTokens: 319, Turns: 1, CostUSD: 0.0117},
		PerTurn: []attach.UsageTurn{
			{Turn: 1, Model: "gemini-3.5-flash", InputTokens: 20049, OutputTokens: 319, CostUSD: 0.0329},
		},
	}

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	if got := a.ContextWindowSize(); got != 1_000_000 {
		t.Errorf("ContextWindowSize = %d, want 1_000_000 (gemini-3.5-flash cap)", got)
	}
	if got := a.ContextWindowUsed(); got != 20049 {
		t.Errorf("ContextWindowUsed = %d, want 20049 (last per-turn InputTokens)", got)
	}
}

// TestAdapter_ContextWindow_LiveStateWinsOverSnapshot ensures the
// streaming-loop's per-event usage (fresher than the 2s /usage cache
// TTL) drives ContextWindowUsed once the operator has actually seen a
// turn tick by. Mirrors LastTurn's live-wins-over-fallback freshness
// policy so /stats stays coherent with the per-turn footer.
func TestAdapter_ContextWindow_LiveStateWinsOverSnapshot(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)
	fs.usage = attach.UsageInfo{
		Overall: attach.UsageTotals{Turns: 1},
		PerTurn: []attach.UsageTurn{
			{Turn: 1, Model: "gemini-3.5-flash", InputTokens: 100, OutputTokens: 10},
		},
	}

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	a.mu.Lock()
	a.lastTurn = coretui.Usage{InputTokens: 42_000, OutputTokens: 300}
	a.mu.Unlock()

	if got := a.ContextWindowUsed(); got != 42_000 {
		t.Errorf("ContextWindowUsed = %d, want 42_000 (live state, not snapshot 100)", got)
	}
}

// TestAdapter_ContextWindow_UnknownModelReturnsZero pins the
// "coretui renders (unknown)" contract: when the last per-turn model
// isn't in pkg/usage's lookup table, ContextWindowSize returns 0 so
// coretui's slash_builtin.go falls into the "(unknown)" branch rather
// than dividing by a made-up cap.
func TestAdapter_ContextWindow_UnknownModelReturnsZero(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)
	fs.usage = attach.UsageInfo{
		Overall: attach.UsageTotals{Turns: 1},
		PerTurn: []attach.UsageTurn{
			{Turn: 1, Model: "some-unrecognized-model", InputTokens: 500, OutputTokens: 50},
		},
	}

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	if got := a.ContextWindowSize(); got != 0 {
		t.Errorf("ContextWindowSize = %d, want 0 for unrecognized model", got)
	}
}

// TestAdapter_ContextWindow_FallsBackToPricingModel exercises the
// pre-first-turn path: /usage has no PerTurn entries yet (fresh
// session, no turn landed), but the operator has already streamed a
// usage-bearing event so applyPricing cached the pricing snapshot's
// CurrentModel. ContextWindowSize should use that as the fallback
// model identity rather than returning 0.
func TestAdapter_ContextWindow_FallsBackToPricingModel(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)
	fs.usage = attach.UsageInfo{
		Overall: attach.UsageTotals{Turns: 0},
		// No PerTurn entries — pre-first-turn state.
	}

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	a.mu.Lock()
	a.pricingModel = "claude-opus-4-7"
	a.mu.Unlock()

	if got := a.ContextWindowSize(); got != 200_000 {
		t.Errorf("ContextWindowSize = %d, want 200_000 (claude-opus-4 base cap from pricingModel fallback)", got)
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

// TestAdapter_Run_PopulatesPerTurnCostFromPricing pins the per-turn
// cost path end-to-end: an event carrying UsageMetadata + a /pricing
// endpoint that returns non-zero rates must yield a coretui.Event with
// CostUSD > 0 so core-tui's per-turn footer renders "$X.XX". Guards
// against regressions where applyPricing stops firing or the pricing
// fetch silently returns zero rates.
func TestAdapter_Run_PopulatesPerTurnCostFromPricing(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)

	// Wire the /pricing endpoint the adapter lazy-fetches on the first
	// usage-carrying event. Real rates so cost math is exercised, not
	// short-circuited by IsZero.
	pricingHits := 0
	fs.Server.Config.Handler.(*http.ServeMux).HandleFunc("GET /sessions/{sid}/pricing", func(w http.ResponseWriter, r *http.Request) {
		pricingHits++
		_ = json.NewEncoder(w).Encode(attach.PricingInfo{
			CurrentModel: "gemini-3.1-pro",
			Current: &attach.ModelPricing{
				InputUSDPerMTok:  1.25,
				OutputUSDPerMTok: 5.00,
			},
		})
	})

	// Final chunk carries UsageMetadata + TurnComplete — matches the
	// Gemini streaming shape. 10k input * $1.25/M + 500 out * $5/M
	// = $0.0125 + $0.0025 = $0.015.
	fs.streamFrames = []attach.Frame{
		{Seq: 1, Event: &session.Event{
			Author: "model",
			LLMResponse: model.LLMResponse{
				Content: &genai.Content{Parts: []*genai.Part{{Text: "done"}}},
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount:     10_000,
					CandidatesTokenCount: 500,
					TotalTokenCount:      10_500,
				},
				Partial:      false,
				TurnComplete: true,
			},
		}},
	}

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var gotCost float64
	var gotModel string
	for ev, err := range a.Run(ctx, "hi") {
		if err != nil {
			// Stream disconnect after final frame is expected — the
			// fake server closes when holdOpen=false, and even with
			// hold-open the ctx timeout eventually fires. Break out
			// cleanly and check what we captured.
			break
		}
		if ev.CostUSD > 0 {
			gotCost = ev.CostUSD
			gotModel = ev.Model
		}
	}

	if pricingHits == 0 {
		t.Errorf("/pricing was never fetched — applyPricing didn't fire on any UsageMetadata event")
	}
	if gotCost == 0 {
		t.Errorf("per-turn CostUSD stayed at 0; want ~0.015 (10k in @ $1.25/M + 500 out @ $5/M)")
	}
	if gotModel != "gemini-3.1-pro" {
		t.Errorf("per-turn Model = %q, want gemini-3.1-pro", gotModel)
	}
}

func TestAdapter_SlashUsage_ReturnsFormattedBlock(t *testing.T) {
	t.Parallel()
	fs := startFakeServer(t)
	// Two turns: one cold, one warm. RenderUsage should surface the
	// cache-savings line because the reference cost exceeds the actual.
	fs.usage = attach.UsageInfo{
		Overall: attach.UsageTotals{
			Turns:                    2,
			InputTokens:              20_000,
			InputTokensCached:        8_000,
			InputTokensUncached:      12_000,
			OutputTokens:             1_000,
			CostUSD:                  0.0175,
			CostUSDUncachedReference: 0.030,
		},
	}

	parsed, _ := attachclient.ParseURL(fs.URL + "/sessions/s1")
	client := attachclient.New(parsed, "", 0)
	a := New(client, "/sessions/s1")

	res, err := a.InvokeSlash(context.Background(), "usage", "")
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: shape check, not full format match (RenderUsage is
	// exhaustively unit-tested in pkg/attach — this just confirms the
	// slash dispatch reaches the formatter with real data).
	if !strings.Contains(res.SystemMessage, "Session totals") {
		t.Errorf("expected Session totals header:\n%s", res.SystemMessage)
	}
	if !strings.Contains(res.SystemMessage, "cache saved") {
		t.Errorf("expected cache-savings line:\n%s", res.SystemMessage)
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

func TestIsTurnEnd_HeuristicWhenTurnCompleteUnset(t *testing.T) {
	t.Parallel()

	// Final model event — Author=core_agent, Partial=false, has text,
	// no tool calls/results, TurnComplete unset (mirrors what
	// core-agent's eventlog actually stores). Heuristic should fire.
	final := &session.Event{
		Author: "core_agent",
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{Parts: []*genai.Part{{Text: "done"}}},
			Partial: false,
		},
	}
	if !isTurnEnd(final, translateEvent(final)) {
		t.Error("final model event should end turn under heuristic")
	}

	// Partial model event: still streaming.
	partial := &session.Event{
		Author: "core_agent",
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{Parts: []*genai.Part{{Text: "still..."}}},
			Partial: true,
		},
	}
	if isTurnEnd(partial, translateEvent(partial)) {
		t.Error("partial model event must NOT end turn")
	}

	// Model event carrying a tool call: turn continues (tool result
	// + follow-up model speech still coming).
	toolCall := &session.Event{
		Author: "core_agent",
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{ID: "fc-1", Name: "bash"}},
			}},
			Partial: false,
		},
	}
	if isTurnEnd(toolCall, translateEvent(toolCall)) {
		t.Error("tool-call event must NOT end turn")
	}

	// User echo: not an end signal.
	userEcho := &session.Event{
		Author: "user",
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{Parts: []*genai.Part{{Text: "hello"}}},
			Partial: false,
		},
	}
	if isTurnEnd(userEcho, translateEvent(userEcho)) {
		t.Error("user-authored event must NOT end turn")
	}

	// TurnComplete=true overrides everything — kept as the primary
	// signal for environments where the eventlog DOES preserve it.
	explicit := &session.Event{
		Author: "core_agent",
		LLMResponse: model.LLMResponse{
			Content:      &genai.Content{Parts: []*genai.Part{{Text: ""}}},
			Partial:      false,
			TurnComplete: true,
		},
	}
	if !isTurnEnd(explicit, translateEvent(explicit)) {
		t.Error("explicit TurnComplete=true must end turn")
	}
}

func TestIsReplay(t *testing.T) {
	t.Parallel()
	connectedAt := time.Date(2026, 5, 30, 22, 0, 0, 0, time.UTC)

	// Older than connectedAt - replayGrace → replay.
	old := connectedAt.Add(-1 * time.Hour)
	if !isReplay(old, connectedAt) {
		t.Error("hour-old event should be flagged as replay")
	}

	// Within replayGrace before connect → live (don't lose it).
	recent := connectedAt.Add(-500 * time.Millisecond)
	if isReplay(recent, connectedAt) {
		t.Error("event 500ms before connect should be live, not replay")
	}

	// Future / same as connect → live.
	live := connectedAt.Add(100 * time.Millisecond)
	if isReplay(live, connectedAt) {
		t.Error("event after connect should be live")
	}

	// Zero timestamp → live (fail-open to avoid silently swallowing).
	if isReplay(time.Time{}, connectedAt) {
		t.Error("zero ts should default to live, not replay")
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
