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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-models/auth"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/retry"

	"github.com/go-steer/core-agent/v3/pkg/agentenv"
	"github.com/go-steer/core-agent/v3/pkg/models"
)

// These tests pin what the facade owns: core-agent's defaults and
// options onto the library's, one retry layer, the empty-response
// sentinel, and core-agent's context markers reaching the adapter. The
// adapter itself — built-in injection and its pre-3.0 skip, the
// server-side-invocations flag, context-cache stamping and eviction
// recovery, the empty-response retry — is tested in core-models.

// captureLogf swaps the package logf sink for the duration of the test
// and returns the captured messages.
func captureLogf(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	var captured []string
	prev := logf
	logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { logf = prev })
	return &captured
}

func TestDefaultBuiltinTools(t *testing.T) {
	t.Parallel()
	d := DefaultBuiltinTools()
	if !d.GoogleSearch || !d.URLContext {
		t.Errorf("GoogleSearch and URLContext should be on by default: %+v", d)
	}
	if d.CodeExecution {
		t.Errorf("CodeExecution should be OFF by default — opt-in only")
	}
}

func TestNewAPIKey_OptionsOverrideDefaults(t *testing.T) {
	t.Parallel()
	p, err := NewAPIKey("test-key", WithGoogleSearch(false), WithCodeExecution(true))
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	if p.builtins.GoogleSearch || !p.builtins.URLContext || !p.builtins.CodeExecution {
		t.Errorf("builtins = %+v, want search off, url context on (default), code execution on", p.builtins)
	}
	p, err = NewAPIKey("test-key", WithBuiltinTools(BuiltinTools{CodeExecution: true}))
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	if p.builtins != (BuiltinTools{CodeExecution: true}) {
		t.Errorf("WithBuiltinTools should replace wholesale, got %+v", p.builtins)
	}
}

// TestErrEmptyResponse_WrapsTheSharedSentinel is the seam that lets a
// caller outside this package recognize "the model said nothing"
// without importing the Gemini adapter; /btw depends on it.
func TestErrEmptyResponse_WrapsTheSharedSentinel(t *testing.T) {
	t.Parallel()
	if !errors.Is(ErrEmptyResponse, models.ErrEmptyResponse) {
		t.Fatal("ErrEmptyResponse no longer wraps models.ErrEmptyResponse")
	}
	if !strings.Contains(ErrEmptyResponse.Error(), "silent safety filter") {
		t.Error("the operator-facing diagnosis was lost from the message")
	}
}

// TestFacade_LibraryEmptyResponseIsErrEmptyResponse: the library's own
// sentinel, after its one retry, reaches callers as ErrEmptyResponse —
// so models.ErrEmptyResponse still identifies it — and keeps the
// library's identity too.
func TestFacade_LibraryEmptyResponseIsErrEmptyResponse(t *testing.T) {
	t.Parallel()
	inner := &scriptedLLM{script: [][]fakeEvent{{{nil, fmt.Errorf("gemini: %w", llm.ErrEmptyResponse)}}}}
	_, errs := drainLLM(t, retrying{inner: inner})
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want one", errs)
	}
	for _, target := range []error{ErrEmptyResponse, models.ErrEmptyResponse, llm.ErrEmptyResponse} {
		if !errors.Is(errs[0], target) {
			t.Errorf("error %v is not %v", errs[0], target)
		}
	}
}

// TestFacade_OneRetryLayer pins the decision that core-agent's
// transientRetry is the only retry: the library's HTTP layer is off,
// its bare-400 rule included, so a 429 is never re-sent twice over and
// every retry is the one a transcript shows (#1206).
func TestFacade_OneRetryLayer(t *testing.T) {
	t.Parallel()
	p, err := NewAPIKey("test-key")
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	o := p.options()
	if o.Retry == nil || o.Retry.MaxRetries != 0 {
		t.Fatalf("library retry = %+v, want MaxRetries 0", o.Retry)
	}
	if o.Retry.HeaderTimeout != retry.Default().HeaderTimeout || o.Retry.HeaderTimeout == 0 {
		t.Errorf("HeaderTimeout = %v, want retry.Default()'s %v — a silent server would hang the turn", o.Retry.HeaderTimeout, retry.Default().HeaderTimeout)
	}
	// Non-streaming calls (the compaction summarizer, titles, the
	// approver) get no headers until the whole answer exists, so they
	// wait UnaryHeaderTimeout instead. A hand-built Policy leaves it 0,
	// which the library reads as no limit.
	if o.Retry.UnaryHeaderTimeout != retry.Default().UnaryHeaderTimeout || o.Retry.UnaryHeaderTimeout == 0 {
		t.Errorf("UnaryHeaderTimeout = %v, want retry.Default()'s %v", o.Retry.UnaryHeaderTimeout, retry.Default().UnaryHeaderTimeout)
	}
	if o.Retry.AfterSuccess == nil || o.Retry.AfterSuccess(400, []byte(`{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`)) {
		t.Error("the library's bare-400 rule is on; it would retry beside transientRetry")
	}
}

// TestFacade_TracedHTTPClient pins #325 on both backends: outbound
// calls go through an otelhttp transport. On Vertex this is a change —
// genai used to build its own instrumented ADC client — because the
// library authenticates with its own bearer transport over ours.
func TestFacade_TracedHTTPClient(t *testing.T) {
	t.Parallel()
	for _, p := range []*Provider{
		{name: "gemini", backend: genai.BackendGeminiAPI, apiKey: "k"},
		{name: "vertex", backend: genai.BackendVertexAI, project: "p", location: "us-central1"},
	} {
		hc := p.options().HTTPClient
		if hc == nil {
			t.Fatalf("%s: no HTTP client; calls would be untraced", p.name)
		}
		if _, ok := hc.Transport.(*otelhttp.Transport); !ok {
			t.Errorf("%s: transport is %T, want *otelhttp.Transport", p.name, hc.Transport)
		}
	}
}

// fakeGenerate is a Gemini Developer API endpoint that answers every
// generateContent with one text candidate and records the request.
type fakeGenerate struct {
	srv *httptest.Server

	mu     sync.Mutex
	auth   []string
	bodies []map[string]any
}

func newFakeGenerate(t *testing.T) *fakeGenerate {
	t.Helper()
	f := &fakeGenerate{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":3,"totalTokenCount":15}}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGenerate) last(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		t.Fatal("the fake server saw no request")
	}
	return f.bodies[len(f.bodies)-1]
}

func toolKinds(body map[string]any) []string {
	tools, _ := body["tools"].([]any)
	var out []string
	for _, tl := range tools {
		m, _ := tl.(map[string]any)
		for k := range m {
			out = append(out, k)
		}
	}
	return out
}

func generateOnce(t *testing.T, ctx context.Context, m adkmodel.LLM) *adkmodel.LLMResponse {
	t.Helper()
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)},
		Config: &genai.GenerateContentConfig{Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name: "read_file", Description: "reads a file",
		}}}}},
	}
	var last *adkmodel.LLMResponse
	for resp, err := range m.GenerateContent(ctx, req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		last = resp
	}
	if last == nil {
		t.Fatal("no response")
	}
	return last
}

// TestFacade_EndToEnd drives Provider.Model against a fake Developer
// API: core-agent's default built-ins ride with the function tool and
// the server-side-invocations flag the direct API needs (#505); the
// /btw marker and the subtask unwrap both send only the caller's tools;
// usage passes through.
func TestFacade_EndToEnd(t *testing.T) {
	t.Parallel()
	f := newFakeGenerate(t)
	p, err := NewAPIKey("test-key")
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	p.baseURL = f.srv.URL
	m, err := p.Model(context.Background(), "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}

	resp := generateOnce(t, context.Background(), m)
	if u := resp.UsageMetadata; u == nil || u.PromptTokenCount != 12 || u.CandidatesTokenCount != 3 {
		t.Errorf("usage = %+v, want 12 prompt / 3 candidates", resp.UsageMetadata)
	}
	body := f.last(t)
	kinds := strings.Join(toolKinds(body), ",")
	for _, want := range []string{"functionDeclarations", "googleSearch", "urlContext"} {
		if !strings.Contains(kinds, want) {
			t.Errorf("tools = %s, want %s", kinds, want)
		}
	}
	tc, _ := body["toolConfig"].(map[string]any)
	if tc["includeServerSideToolInvocations"] != true {
		t.Errorf("toolConfig = %v, want includeServerSideToolInvocations on the direct API", tc)
	}

	generateOnce(t, models.WithoutBuiltins(context.Background()), m)
	if kinds := toolKinds(f.last(t)); len(kinds) != 1 || kinds[0] != "functionDeclarations" {
		t.Errorf("WithoutBuiltins request tools = %v, want only the function declarations", kinds)
	}

	wb, ok := m.(interface{ WithoutBuiltins() adkmodel.LLM })
	if !ok {
		t.Fatalf("model %T lost the WithoutBuiltins unwrap RunSubtask relies on", m)
	}
	generateOnce(t, context.Background(), wb.WithoutBuiltins())
	if kinds := toolKinds(f.last(t)); len(kinds) != 1 || kinds[0] != "functionDeclarations" {
		t.Errorf("unwrapped model's request tools = %v, want only the function declarations", kinds)
	}
	switch u := wb.WithoutBuiltins().(type) {
	case retrying, retryingWithBuiltins:
	default:
		t.Errorf("the unwrapped model is %T, want it still under the retry layer", u)
	}
}

// testVertex is a Vertex Provider pointed at f with a static bearer
// credential, so no ADC is needed.
func testVertex(t *testing.T, f *fakeGenerate) *Provider {
	t.Helper()
	cred, err := auth.Config{Kind: auth.Bearer, Env: "TEST_TOKEN"}.Resolve(context.Background(),
		auth.Options{Getenv: func(string) string { return "test-token" }})
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	p, err := NewVertex("proj", "us-central1", WithBuiltinTools(BuiltinTools{}))
	if err != nil {
		t.Fatalf("NewVertex: %v", err)
	}
	p.baseURL, p.cred = f.srv.URL, cred
	return p
}

// TestFacade_HooksInstalledAfterModelStillApply: the daemon resolves
// the provider, and code paths can build a model, before
// MaybeWireContextCache installs the hooks. A model from before the
// hooks must still stamp the cache on its next call.
func TestFacade_HooksInstalledAfterModelStillApply(t *testing.T) {
	t.Parallel()
	f := newFakeGenerate(t)
	p := testVertex(t, f)
	m, err := p.Model(context.Background(), "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	generateOnce(t, context.Background(), m)
	if got := f.last(t)["cachedContent"]; got != nil {
		t.Fatalf("uncached baseline carried cachedContent %v", got)
	}

	var inits int
	var mu sync.Mutex
	p.SetContextCache("gemini-3.5-flash",
		func(context.Context, *genai.Content, []*genai.Tool) { mu.Lock(); inits++; mu.Unlock() },
		func(context.Context) string { return "projects/proj/locations/us-central1/cachedContents/abc" })
	generateOnce(t, context.Background(), m)
	if got := f.last(t)["cachedContent"]; got != "projects/proj/locations/us-central1/cachedContents/abc" {
		t.Errorf("request after SetContextCache carried cachedContent %v, want the hook's name", got)
	}
}

// TestFacade_BuiltinsSkippedNoticeOncePerModel: the library logs once
// per model handle when a pre-3.0 model cannot mix built-ins with
// function tools. It hands back a fresh handle from every Client.Model
// call, so resolving the model per call (lateModel) repeated the line
// on every turn. Not parallel: captureLogf swaps a package global.
func TestFacade_BuiltinsSkippedNoticeOncePerModel(t *testing.T) {
	logs := captureLogf(t)
	f := newFakeGenerate(t)
	p, err := NewAPIKey("test-key")
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	p.baseURL = f.srv.URL
	m, err := p.Model(context.Background(), "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	for range 3 {
		generateOnce(t, context.Background(), m)
	}
	n := 0
	for _, l := range *logs {
		if strings.Contains(l, "predates mixed built-in + function tools") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("built-ins-skipped notice logged %d times over 3 calls, want once:\n%s", n, strings.Join(*logs, "\n"))
	}
}

// TestFacade_CachesSharesTheClient: building the cache manager reuses
// the Provider's client rather than opening a second one.
func TestFacade_CachesSharesTheClient(t *testing.T) {
	t.Parallel()
	f := newFakeGenerate(t)
	p := testVertex(t, f)
	if _, err := p.Model(context.Background(), "gemini-3.5-flash"); err != nil {
		t.Fatalf("Model: %v", err)
	}
	before := p.client
	caches, err := p.Caches(context.Background())
	if err != nil {
		t.Fatalf("Caches: %v", err)
	}
	if p.client != before || caches != before.Caches() {
		t.Error("Caches opened a second client")
	}
}

// TestFacade_InstallingHooksKeepsTheClient: MaybeWireContextCache builds
// the manager on Caches() and then installs the hooks. Before core-models
// v0.8.0 (#31) the library took hooks only at construction, so
// installing them dropped the client and the next call built a second
// one, leaving the manager's genai.Caches on a client nothing else used.
// The hooks now go onto the client in place: same client, same cached
// library model, and a model from before the hooks stamps the cache.
func TestFacade_InstallingHooksKeepsTheClient(t *testing.T) {
	t.Parallel()
	f := newFakeGenerate(t)
	p := testVertex(t, f)
	m, err := p.Model(context.Background(), "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	generateOnce(t, context.Background(), m)
	client := p.client
	lib := p.models["gemini-3.5-flash"]
	if _, err := p.Caches(context.Background()); err != nil {
		t.Fatalf("Caches: %v", err)
	}

	p.SetContextCache("gemini-3.5-flash",
		func(context.Context, *genai.Content, []*genai.Tool) {},
		func(context.Context) string { return "projects/proj/locations/us-central1/cachedContents/abc" })
	p.SetContextCacheInvalidate(func(string, string) {})
	if p.client != client {
		t.Fatal("installing context-cache hooks built a second client")
	}
	generateOnce(t, context.Background(), m)
	if got := f.last(t)["cachedContent"]; got != "projects/proj/locations/us-central1/cachedContents/abc" {
		t.Errorf("request after the hooks carried cachedContent %v, want the hook's name", got)
	}
	if p.client != client || p.models["gemini-3.5-flash"] != lib {
		t.Error("a call after the hooks ran on a rebuilt client or library model")
	}
}

// TestFacade_RejectedHooksStillSurfaceOnTheNextCall: hooks with no model
// to scope them to are a wiring error the library refuses. Setting them
// on a built client cannot return it (SetContextCache has no error), so
// the client is dropped and the next call's rebuild reports it — the
// pre-v0.8.0 behaviour, rather than a silently ignored hook.
func TestFacade_RejectedHooksStillSurfaceOnTheNextCall(t *testing.T) {
	t.Parallel()
	f := newFakeGenerate(t)
	p := testVertex(t, f)
	m, err := p.Model(context.Background(), "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	p.SetContextCacheInvalidate(func(string, string) {})
	if p.client != nil {
		t.Fatal("a hook the library rejected left the client in place")
	}
	var callErr error
	for _, err := range m.GenerateContent(context.Background(), &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)},
	}, false) {
		if err != nil {
			callErr = err
		}
	}
	if callErr == nil || !strings.Contains(callErr.Error(), "ContextCacheModel") {
		t.Errorf("next call error = %v, want the library's missing-ContextCacheModel error", callErr)
	}
}

// TestVertex_InjectedGoogleAuthReachesTheRequest verifies that setting
// CORE_AGENT_GOOGLE_AUTH=injected skips ADC discovery and sends
// "Authorization: Bearer placeholder" for an egress proxy to replace.
// Not parallel: t.Setenv.
func TestVertex_InjectedGoogleAuthReachesTheRequest(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", t.TempDir()+"/missing.json")
	t.Setenv(agentenv.EnvGoogleAuth, agentenv.GoogleAuthInjected)

	f := newFakeGenerate(t)
	p, err := NewVertex("proj", "us-central1", WithBuiltinTools(BuiltinTools{}))
	if err != nil {
		t.Fatalf("NewVertex: %v", err)
	}
	p.baseURL = f.srv.URL
	m, err := p.Model(context.Background(), "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	generateOnce(t, context.Background(), m)

	f.mu.Lock()
	gotAuth := append([]string(nil), f.auth...)
	f.mu.Unlock()
	if len(gotAuth) != 1 || gotAuth[0] != "Bearer "+agentenv.InjectedGooglePlaceholder {
		t.Errorf("Authorization = %v, want [Bearer %s]", gotAuth, agentenv.InjectedGooglePlaceholder)
	}
}
