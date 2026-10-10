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
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-models/llm"

	"github.com/go-steer/core-agent/v2/pkg/models"
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
	o := p.options(true)
	if o.Retry == nil || o.Retry.MaxRetries != 0 {
		t.Fatalf("library retry = %+v, want MaxRetries 0", o.Retry)
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
		hc := p.options(true).HTTPClient
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
