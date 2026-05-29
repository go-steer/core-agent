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

package gemini

import (
	"context"
	"errors"
	"iter"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

func TestDefaultBuiltinTools(t *testing.T) {
	t.Parallel()
	d := DefaultBuiltinTools()
	if !d.GoogleSearch {
		t.Errorf("GoogleSearch should be on by default")
	}
	if !d.URLContext {
		t.Errorf("URLContext should be on by default")
	}
	if d.CodeExecution {
		t.Errorf("CodeExecution should be OFF by default — opt-in only")
	}
}

func TestBuiltinTools_AsTools_Default(t *testing.T) {
	t.Parallel()
	tools := DefaultBuiltinTools().asTools()
	if len(tools) != 2 {
		t.Fatalf("default produces 2 tools, got %d", len(tools))
	}
	if tools[0].GoogleSearch == nil {
		t.Errorf("tools[0] should be GoogleSearch")
	}
	if tools[1].URLContext == nil {
		t.Errorf("tools[1] should be URLContext")
	}
}

func TestBuiltinTools_AsTools_AllOn(t *testing.T) {
	t.Parallel()
	tools := BuiltinTools{
		GoogleSearch:  true,
		URLContext:    true,
		CodeExecution: true,
	}.asTools()
	if len(tools) != 3 {
		t.Fatalf("all-on produces 3 tools, got %d", len(tools))
	}
	if tools[2].CodeExecution == nil {
		t.Errorf("tools[2] should be CodeExecution")
	}
}

func TestBuiltinTools_AsTools_Empty(t *testing.T) {
	t.Parallel()
	tools := BuiltinTools{}.asTools()
	if len(tools) != 0 {
		t.Fatalf("zero-value should produce no tools, got %d", len(tools))
	}
}

func TestNewAPIKey_AppliesDefaults(t *testing.T) {
	t.Parallel()
	p, err := NewAPIKey("test-key")
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	if !p.builtins.GoogleSearch || !p.builtins.URLContext {
		t.Errorf("defaults not applied: %+v", p.builtins)
	}
	if p.builtins.CodeExecution {
		t.Errorf("CodeExecution should be off by default")
	}
}

func TestNewAPIKey_OptionsOverrideDefaults(t *testing.T) {
	t.Parallel()
	p, err := NewAPIKey("test-key",
		WithGoogleSearch(false),
		WithCodeExecution(true),
	)
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	if p.builtins.GoogleSearch {
		t.Errorf("WithGoogleSearch(false) didn't take")
	}
	if !p.builtins.URLContext {
		t.Errorf("URLContext should still be on (default)")
	}
	if !p.builtins.CodeExecution {
		t.Errorf("WithCodeExecution(true) didn't take")
	}
}

func TestNewAPIKey_WithBuiltinTools_ReplacesWholesale(t *testing.T) {
	t.Parallel()
	p, err := NewAPIKey("test-key",
		WithBuiltinTools(BuiltinTools{CodeExecution: true}),
	)
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	if p.builtins.GoogleSearch || p.builtins.URLContext {
		t.Errorf("WithBuiltinTools should replace wholesale, got: %+v", p.builtins)
	}
	if !p.builtins.CodeExecution {
		t.Errorf("CodeExecution: true didn't survive")
	}
}

// fakeLLM records the most recent request it was asked to handle so
// tests can assert how the wrapper mutates Config. If `events` is
// non-nil it is replayed as the streaming response — used to exercise
// the wrapper's "empty response" heartbeat filter.
type fakeLLM struct {
	last   *adkmodel.LLMRequest
	events []fakeEvent
}

type fakeEvent struct {
	resp *adkmodel.LLMResponse
	err  error
}

func (f *fakeLLM) Name() string { return "fake" }

func (f *fakeLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	f.last = req
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		for _, e := range f.events {
			if !yield(e.resp, e.err) {
				return
			}
		}
	}
}

func TestBuiltinsLLM_InjectsIntoConfigTools(t *testing.T) {
	t.Parallel()
	fake := &fakeLLM{}
	wrapped := &builtinsLLM{
		inner:    fake,
		builtins: DefaultBuiltinTools().asTools(),
	}
	req := &adkmodel.LLMRequest{}
	for range wrapped.GenerateContent(context.Background(), req, false) {
		// drain
	}
	if fake.last.Config == nil {
		t.Fatalf("Config should have been initialized by the wrapper")
	}
	if len(fake.last.Config.Tools) != 2 {
		t.Fatalf("expected 2 injected tools, got %d", len(fake.last.Config.Tools))
	}
}

func TestBuiltinsLLM_PreservesExistingTools(t *testing.T) {
	t.Parallel()
	fake := &fakeLLM{}
	wrapped := &builtinsLLM{
		inner:    fake,
		builtins: BuiltinTools{GoogleSearch: true}.asTools(),
	}
	// Caller already supplied a function-declaration tool. The wrapper
	// must append, not replace.
	userTool := &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "my_func"}},
	}
	req := &adkmodel.LLMRequest{
		Config: &genai.GenerateContentConfig{Tools: []*genai.Tool{userTool}},
	}
	for range wrapped.GenerateContent(context.Background(), req, false) {
	}
	if len(fake.last.Config.Tools) != 2 {
		t.Fatalf("expected 1 user tool + 1 injected, got %d", len(fake.last.Config.Tools))
	}
	if fake.last.Config.Tools[0] != userTool {
		t.Errorf("user tool should remain at index 0")
	}
	if fake.last.Config.Tools[1].GoogleSearch == nil {
		t.Errorf("injected tool should be GoogleSearch")
	}
}

func TestBuiltinsLLM_NameDelegates(t *testing.T) {
	t.Parallel()
	wrapped := &builtinsLLM{inner: &fakeLLM{}}
	if wrapped.Name() != "fake" {
		t.Errorf("Name should delegate to inner LLM")
	}
}

func TestBuiltinsLLM_SetsIncludeServerSideToolInvocations_OnDirectAPI(t *testing.T) {
	t.Parallel()
	fake := &fakeLLM{}
	wrapped := &builtinsLLM{
		inner:             fake,
		builtins:          DefaultBuiltinTools().asTools(),
		isDirectGeminiAPI: true,
	}
	for range wrapped.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
	}
	if fake.last.Config.ToolConfig == nil {
		t.Fatalf("ToolConfig should be set on direct Gemini API")
	}
	got := fake.last.Config.ToolConfig.IncludeServerSideToolInvocations
	if got == nil || *got != true {
		t.Errorf("IncludeServerSideToolInvocations = %v, want true on direct Gemini API", got)
	}
}

func TestBuiltinsLLM_OmitsIncludeServerSideToolInvocations_OnVertex(t *testing.T) {
	t.Parallel()
	// Vertex AI rejects the parameter with
	// "includeServerSideToolInvocations parameter is not supported
	// in Gemini Enterprise Agent Platform (previously known as
	// Vertex AI)". The wrapper must not set it for Vertex.
	fake := &fakeLLM{}
	wrapped := &builtinsLLM{
		inner:             fake,
		builtins:          DefaultBuiltinTools().asTools(),
		isDirectGeminiAPI: false, // Vertex backend
	}
	for range wrapped.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
	}
	if fake.last.Config.ToolConfig != nil &&
		fake.last.Config.ToolConfig.IncludeServerSideToolInvocations != nil {
		t.Errorf("IncludeServerSideToolInvocations should remain unset on Vertex; got %v",
			fake.last.Config.ToolConfig.IncludeServerSideToolInvocations)
	}
}

func TestBuiltinsLLM_SwallowsEmptyResponseHeartbeats_OnVertexStream(t *testing.T) {
	t.Parallel()
	// Vertex's streaming search-grounding path emits SSE chunks with
	// empty Candidates[] (heartbeat-like), which ADK surfaces as
	// "empty response" errors mid-stream. The wrapper must drop
	// those frames so the surrounding real chunks reach the caller.
	r1 := &adkmodel.LLMResponse{}
	r2 := &adkmodel.LLMResponse{}
	fake := &fakeLLM{
		events: []fakeEvent{
			{resp: r1},
			{err: errors.New("empty response")}, // heartbeat
			{resp: r2},
		},
	}
	wrapped := &builtinsLLM{
		inner:               fake,
		builtins:            DefaultBuiltinTools().asTools(),
		tolerateEmptyChunks: true,
	}
	var got []*adkmodel.LLMResponse
	for resp, err := range wrapped.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, true) {
		if err != nil {
			t.Fatalf("did not expect error to propagate; got %v", err)
		}
		got = append(got, resp)
	}
	if len(got) != 2 || got[0] != r1 || got[1] != r2 {
		t.Fatalf("expected r1, r2; got %v", got)
	}
}

func TestBuiltinsLLM_DoesNotSwallowEmptyResponseInNonStreamingMode(t *testing.T) {
	t.Parallel()
	// Non-streaming "empty response" means the model genuinely
	// returned no content — that's a real failure, not a heartbeat.
	// Surface it.
	fake := &fakeLLM{events: []fakeEvent{{err: errors.New("empty response")}}}
	wrapped := &builtinsLLM{
		inner:               fake,
		builtins:            DefaultBuiltinTools().asTools(),
		tolerateEmptyChunks: true,
	}
	saw := false
	for _, err := range wrapped.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
		if err != nil && err.Error() == "empty response" {
			saw = true
		}
	}
	if !saw {
		t.Errorf("non-streaming empty response should propagate, not be swallowed")
	}
}

func TestBuiltinsLLM_DoesNotSwallowOtherErrors(t *testing.T) {
	t.Parallel()
	// Auth / network / quota / etc. must still bubble up — only the
	// literal "empty response" heartbeat string is filtered.
	fake := &fakeLLM{events: []fakeEvent{{err: errors.New("rate limited")}}}
	wrapped := &builtinsLLM{
		inner:               fake,
		builtins:            DefaultBuiltinTools().asTools(),
		tolerateEmptyChunks: true,
	}
	got := ""
	for _, err := range wrapped.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, true) {
		if err != nil {
			got = err.Error()
		}
	}
	if got != "rate limited" {
		t.Errorf("expected the upstream error to propagate; got %q", got)
	}
}

func TestBuiltinsLLM_DoesNotSwallowEmptyResponseWhenToleranceOff(t *testing.T) {
	t.Parallel()
	// Direct Gemini API path (tolerateEmptyChunks=false) must surface
	// the error normally so a real "no content" failure isn't hidden.
	fake := &fakeLLM{events: []fakeEvent{{err: errors.New("empty response")}}}
	wrapped := &builtinsLLM{
		inner:               fake,
		builtins:            DefaultBuiltinTools().asTools(),
		tolerateEmptyChunks: false,
	}
	saw := false
	for _, err := range wrapped.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, true) {
		if err != nil && err.Error() == "empty response" {
			saw = true
		}
	}
	if !saw {
		t.Errorf("with tolerance off the error should propagate; nothing was yielded")
	}
}

// TestBuiltinsLLM_WithoutBuiltinsReturnsInner pins the duck-typed
// hook the agent package's RunSubtask path uses to strip auto-
// injected built-ins (so subtasks pass EXACTLY their own tool set
// — Gemini 2.5 Flash rejects mixing function tools with search-
// side built-ins). Smoking-gun test: after WithoutBuiltins, a
// request that previously had GoogleSearch + URLContext appended
// now has no extra tools touched.
func TestBuiltinsLLM_WithoutBuiltinsReturnsInner(t *testing.T) {
	t.Parallel()
	fake := &fakeLLM{events: []fakeEvent{{resp: &adkmodel.LLMResponse{}}}}
	wrapped := &builtinsLLM{
		inner:    fake,
		builtins: DefaultBuiltinTools().asTools(),
	}

	stripped := wrapped.WithoutBuiltins()
	if stripped == nil {
		t.Fatalf("WithoutBuiltins returned nil")
	}
	if stripped == adkmodel.LLM(wrapped) {
		t.Fatalf("WithoutBuiltins returned the wrapper itself; want the inner LLM")
	}

	// Drive a request through the stripped LLM and verify the
	// fake saw NO tools injected — proving the builtins layer
	// is bypassed.
	req := &adkmodel.LLMRequest{}
	for range stripped.GenerateContent(context.Background(), req, false) {
	}
	if req.Config != nil && len(req.Config.Tools) > 0 {
		t.Errorf("stripped LLM still injected tools: %d entries", len(req.Config.Tools))
	}

	// Sanity: the wrapper still injects on its own path.
	req2 := &adkmodel.LLMRequest{}
	for range wrapped.GenerateContent(context.Background(), req2, false) {
	}
	if req2.Config == nil || len(req2.Config.Tools) == 0 {
		t.Errorf("wrapper should still inject builtins; got %v", req2.Config)
	}
}
