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
	//
	// r1 and r2 carry real content (Parts set) so isUsableResponse
	// classifies them as legitimate — mimics what production Vertex
	// responses look like between heartbeats. Zero-value LLMResponse
	// sentinels would trip the #220 empty-tail detection.
	r1 := &adkmodel.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: "chunk-1"}}}}
	r2 := &adkmodel.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: "chunk-2"}}}}
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

// TestBuiltinsLLM_ContextCacheHooksFire verifies the Vertex explicit
// context-cache wiring (#221):
//
//   - cacheInit runs on every GenerateContent call with the
//     request's fully-assembled SystemInstruction + Tools (the
//     capture-on-first-call design that lets us cache exactly what
//     ADK would send).
//   - cacheName runs on every call; when it returns a non-empty
//     name the wrapper stamps it onto Config.CachedContent.
//   - Empty cacheName return means uncached — Config.CachedContent
//     stays unset so Vertex doesn't reject the request.
func TestBuiltinsLLM_ContextCacheHooksFire(t *testing.T) {
	t.Parallel()

	var initCalls int
	var lastInitSys *genai.Content
	var lastInitTools []*genai.Tool
	initFn := func(_ context.Context, sys *genai.Content, tools []*genai.Tool) {
		initCalls++
		lastInitSys = sys
		lastInitTools = tools
	}
	nameFn := func(_ context.Context) string {
		return "" // no cache ready yet on this call
	}

	fake := &fakeLLM{}
	wrapped := &builtinsLLM{
		inner:     fake,
		cacheInit: initFn,
		cacheName: nameFn,
	}

	// Call 1: ADK assembles a request with SystemInstruction + Tools.
	// Init must capture both; CachedContent must NOT be set (name
	// hook returned empty).
	req1 := &adkmodel.LLMRequest{Config: &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "you are a test agent"}}},
		Tools:             []*genai.Tool{{}},
	}}
	for range wrapped.GenerateContent(context.Background(), req1, false) {
	}
	if initCalls != 1 {
		t.Errorf("cacheInit called %d times on call 1, want 1", initCalls)
	}
	if lastInitSys == nil || len(lastInitSys.Parts) == 0 || lastInitSys.Parts[0].Text != "you are a test agent" {
		t.Errorf("cacheInit didn't receive the request's SystemInstruction: %+v", lastInitSys)
	}
	if len(lastInitTools) != 1 {
		t.Errorf("cacheInit didn't receive the request's Tools: %+v", lastInitTools)
	}
	if req1.Config.CachedContent != "" {
		t.Errorf("CachedContent set when name hook returned empty: %q", req1.Config.CachedContent)
	}

	// Call 2: name hook now returns a cache name. Wrapper stamps it.
	nameFn2 := func(_ context.Context) string {
		return "projects/p/locations/l/cachedContents/abc"
	}
	wrapped.cacheName = nameFn2
	req2 := &adkmodel.LLMRequest{Config: &genai.GenerateContentConfig{}}
	for range wrapped.GenerateContent(context.Background(), req2, false) {
	}
	if req2.Config.CachedContent != "projects/p/locations/l/cachedContents/abc" {
		t.Errorf("CachedContent not stamped: %q", req2.Config.CachedContent)
	}
	// cacheInit gates on !cachedTurn so it's only called on uncached
	// turns (turn 1 above). Cached turn 2 must NOT re-fire it — the
	// stripped Config.Tools would corrupt what a real Manager captures.
	if initCalls != 1 {
		t.Errorf("cacheInit called %d total times, want 1 (uncached turn 1 only, not cached turn 2)", initCalls)
	}
}

// TestBuiltinsLLM_CachedTurn_StripsForbiddenFields is the load-bearing
// contract test for #221's cached-turn shape. Vertex rejects a request
// with 400 "Tool config, tools and system instruction should not be
// set in the request when using cached content" if we set
// CachedContent AND leave those fields populated. The wrapper must
// strip them + skip the builtins-append so the downstream request
// carries only Contents + CachedContent.
//
// Regression signal: if this test fails, cached turns 400 on Vertex
// and every session with #221 enabled breaks after turn 1.
func TestBuiltinsLLM_CachedTurn_StripsForbiddenFields(t *testing.T) {
	t.Parallel()

	// Cache is ready — name hook returns a non-empty handle.
	nameFn := func(_ context.Context) string {
		return "projects/p/locations/l/cachedContents/abc"
	}
	fake := &fakeLLM{}
	wrapped := &builtinsLLM{
		inner:     fake,
		builtins:  DefaultBuiltinTools().asTools(), // would append 2 tools if not skipped
		cacheName: nameFn,
	}

	// ADK has already populated Tools + SystemInstruction + ToolConfig
	// on this request. Real Vertex traffic would 400 on these coexisting
	// with a CachedContent reference. The wrapper must strip them.
	initialToolConfig := &genai.ToolConfig{}
	req := &adkmodel.LLMRequest{Config: &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "sys"}}},
		Tools:             []*genai.Tool{{}, {}},
		ToolConfig:        initialToolConfig,
	}}
	for range wrapped.GenerateContent(context.Background(), req, false) {
	}

	if fake.last.Config.CachedContent != "projects/p/locations/l/cachedContents/abc" {
		t.Errorf("CachedContent not stamped: %q", fake.last.Config.CachedContent)
	}
	if fake.last.Config.SystemInstruction != nil {
		t.Errorf("SystemInstruction not stripped on cached turn: %+v", fake.last.Config.SystemInstruction)
	}
	if fake.last.Config.Tools != nil {
		t.Errorf("Tools not stripped on cached turn: %d entries — Vertex will 400", len(fake.last.Config.Tools))
	}
	if fake.last.Config.ToolConfig != nil {
		t.Errorf("ToolConfig not stripped on cached turn: %+v", fake.last.Config.ToolConfig)
	}
}

// TestBuiltinsLLM_UncachedTurn_KeepsFields is the mirror: when the
// cache isn't ready (Name returns ""), the wrapper must NOT strip
// SystemInstruction/Tools/ToolConfig — turn 1 is uncached and needs
// those fields to work.
func TestBuiltinsLLM_UncachedTurn_KeepsFields(t *testing.T) {
	t.Parallel()

	// Name hook returns empty — cache not ready (async Init still in flight).
	nameFn := func(_ context.Context) string { return "" }
	fake := &fakeLLM{}
	wrapped := &builtinsLLM{
		inner:     fake,
		builtins:  DefaultBuiltinTools().asTools(),
		cacheName: nameFn,
	}

	req := &adkmodel.LLMRequest{Config: &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "sys"}}},
	}}
	for range wrapped.GenerateContent(context.Background(), req, false) {
	}

	if fake.last.Config.CachedContent != "" {
		t.Errorf("CachedContent set on uncached turn: %q", fake.last.Config.CachedContent)
	}
	if fake.last.Config.SystemInstruction == nil {
		t.Errorf("SystemInstruction stripped on uncached turn — would break turn 1")
	}
	// Builtins-append still fires on uncached turns (2 default builtins).
	if len(fake.last.Config.Tools) != 2 {
		t.Errorf("builtins not appended on uncached turn: %d tools, want 2", len(fake.last.Config.Tools))
	}
}

// TestBuiltinsLLM_NoCacheHooks_LeavesConfigUnchanged is the safety
// property: when cache hooks are nil (the pre-#221 pathway and every
// non-Vertex Provider), CachedContent must never be set and no
// panics can fire from a bare Config.
func TestBuiltinsLLM_NoCacheHooks_LeavesConfigUnchanged(t *testing.T) {
	t.Parallel()
	fake := &fakeLLM{}
	wrapped := &builtinsLLM{inner: fake} // no cache hooks
	req := &adkmodel.LLMRequest{Config: &genai.GenerateContentConfig{}}
	for range wrapped.GenerateContent(context.Background(), req, false) {
	}
	if req.Config.CachedContent != "" {
		t.Errorf("CachedContent set without hooks: %q", req.Config.CachedContent)
	}
}
