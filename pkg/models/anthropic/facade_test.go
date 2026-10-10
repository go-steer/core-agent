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

package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// These tests drive the facade end to end — Provider.Model through
// core-models' adapter to a fake Messages server — and pin what
// core-agent owns: config onto the library's options, core-agent's
// context markers reaching it, and the library's usage record reaching
// pkg/usage. The adapter's own behaviour (conversion, streaming,
// breakpoint placement, thinking) is tested in core-models.

// cacheWritingSSE is one turn that read 20,000 cached tokens and wrote
// 4,000, 1,500 of them at the one-hour TTL.
const cacheWritingSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1000,"cache_creation_input_tokens":4000,"cache_read_input_tokens":20000,"cache_creation":{"ephemeral_5m_input_tokens":2500,"ephemeral_1h_input_tokens":1500},"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"warm"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":500}}

event: message_stop
data: {"type":"message_stop"}

`

// fakeMessages serves cacheWritingSSE and records each request.
type fakeMessages struct {
	srv *httptest.Server

	mu      sync.Mutex
	bodies  []map[string]any
	headers []http.Header
}

func newFakeMessages(t *testing.T) *fakeMessages {
	t.Helper()
	f := &fakeMessages{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, cacheWritingSSE)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMessages) last(t *testing.T) (map[string]any, http.Header) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		t.Fatal("the fake server saw no request")
	}
	return f.bodies[len(f.bodies)-1], f.headers[len(f.headers)-1]
}

// pointAt redirects p's profile to the fake server. The provider has
// already opened once against the real endpoint (New does, to fail
// fast on a bad credential); dropping the cache makes the next Model()
// reopen against the fake.
func pointAt(p *Provider, f *fakeMessages) {
	p.prof.BaseURL = f.srv.URL
	p.opened = nil
}

func generate(t *testing.T, ctx context.Context, p *Provider, req *adkmodel.LLMRequest) *adkmodel.LLMResponse {
	t.Helper()
	m, err := p.Model(ctx, "claude-test")
	if err != nil {
		t.Fatalf("Model: %v", err)
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

func historyRequest() *adkmodel.LLMRequest {
	var contents []*genai.Content
	for i := 0; i < 6; i++ {
		role := genai.RoleUser
		if i%2 == 1 {
			role = genai.RoleModel
		}
		contents = append(contents, genai.NewContentFromText(strings.Repeat("turn ", 50), genai.Role(role)))
	}
	return &adkmodel.LLMRequest{
		Contents: contents,
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("you are a test", genai.RoleUser),
		},
	}
}

func countCacheControl(v any) (n int, ttls []string) {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			if k == "cache_control" {
				n++
				if m, ok := vv.(map[string]any); ok {
					if ttl, ok := m["ttl"].(string); ok {
						ttls = append(ttls, ttl)
					}
				}
				continue
			}
			c, t := countCacheControl(vv)
			n, ttls = n+c, append(ttls, t...)
		}
	case []any:
		for _, vv := range x {
			c, t := countCacheControl(vv)
			n, ttls = n+c, append(ttls, t...)
		}
	}
	return n, ttls
}

// TestFacade_CacheWritesReachTheMeter is the accounting contract the
// library has to keep for core-agent: the genai buckets carry the
// prompt total and the read subset, and the write bucket — with its
// one-hour share, which bills at 2x rather than 1.25x (#770) — reaches
// pkg/usage through the sidecar keys, across the runner's event seam.
func TestFacade_CacheWritesReachTheMeter(t *testing.T) {
	t.Parallel()
	f := newFakeMessages(t)
	p, err := New("test-key-not-real")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pointAt(p, f)
	terminal := generate(t, context.Background(), p, historyRequest())

	u := terminal.UsageMetadata
	if u == nil {
		t.Fatal("terminal response carries no UsageMetadata")
	}
	if u.PromptTokenCount != 25_000 || u.CachedContentTokenCount != 20_000 {
		t.Errorf("prompt/cached = %d/%d, want 25000/20000", u.PromptTokenCount, u.CachedContentTokenCount)
	}

	var tap usage.TurnTap
	ev := &session.Event{LLMResponse: *terminal}
	tap.Observe(ev)
	got, ok := tap.Commit(ev)
	if !ok {
		t.Fatal("TurnTap did not commit the terminal event")
	}
	if got.CacheCreationInputTokens != 4_000 || got.CacheCreation1hInputTokens != 1_500 {
		t.Errorf("cache writes = %d (1h %d), want 4000 (1h 1500); CustomMetadata = %#v",
			got.CacheCreationInputTokens, got.CacheCreation1hInputTokens, terminal.CustomMetadata)
	}
	if got.UncachedInputTokens() != 1_000 {
		t.Errorf("UncachedInputTokens() = %d, want 1000", got.UncachedInputTokens())
	}
}

// TestFacade_ContextOptOutsReachTheLibrary pins that core-agent's
// per-call markers are the library's: the summarizer's
// models.WithoutPromptCache places no breakpoints, and the approver's
// models.WithoutBuiltins sends no server-side tools, on the same model.
func TestFacade_ContextOptOutsReachTheLibrary(t *testing.T) {
	t.Parallel()
	f := newFakeMessages(t)
	p, err := New("test-key-not-real", WithWebSearch(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pointAt(p, f)

	generate(t, context.Background(), p, historyRequest())
	body, _ := f.last(t)
	if n, _ := countCacheControl(body); n == 0 {
		t.Fatal("baseline request carried no cache_control; the opt-out check proves nothing")
	}
	if tools, _ := body["tools"].([]any); len(tools) == 0 {
		t.Fatal("baseline request carried no tools; the opt-out check proves nothing")
	}

	generate(t, models.WithoutPromptCache(context.Background()), p, historyRequest())
	body, _ = f.last(t)
	if n, _ := countCacheControl(body); n != 0 {
		t.Errorf("WithoutPromptCache request carried %d cache_control markers, want 0", n)
	}

	generate(t, models.WithoutBuiltins(context.Background()), p, historyRequest())
	body, _ = f.last(t)
	if tools, ok := body["tools"]; ok {
		t.Errorf("WithoutBuiltins request carried tools %v, want none", tools)
	}
}

// TestFacade_SetPromptCacheAfterConstruction covers the daemon's wiring
// order: the kill switch and the 1-hour TTL arrive after Resolve, and
// must still shape the next Model().
func TestFacade_SetPromptCacheAfterConstruction(t *testing.T) {
	t.Parallel()
	f := newFakeMessages(t)
	p, err := New("test-key-not-real")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pointAt(p, f)

	p.SetPromptCache(CacheOptions{System: true, History: true, TTL: config.PromptCacheTTL1h})
	generate(t, context.Background(), p, historyRequest())
	body, _ := f.last(t)
	n, ttls := countCacheControl(body)
	if n == 0 || len(ttls) != n {
		t.Fatalf("1h policy: %d markers, %d with a ttl (%v); want every marker to carry one", n, len(ttls), ttls)
	}
	for _, ttl := range ttls {
		if ttl != "1h" {
			t.Errorf("marker ttl = %q, want 1h", ttl)
		}
	}

	p.SetPromptCache(CacheOptions{})
	generate(t, context.Background(), p, historyRequest())
	body, _ = f.last(t)
	if n, _ := countCacheControl(body); n != 0 {
		t.Errorf("caching off: %d markers, want 0", n)
	}
}

// TestFacade_ConfigKeyBeatsTheEnvironment: model.anthropic.api_key in
// config is the key on the wire even when ANTHROPIC_API_KEY holds
// another.
func TestFacade_ConfigKeyBeatsTheEnvironment(t *testing.T) {
	t.Setenv(EnvAPIKey, "env-key")
	f := newFakeMessages(t)
	p, err := New("config-key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pointAt(p, f)
	generate(t, context.Background(), p, historyRequest())
	_, h := f.last(t)
	if got := h.Get("X-Api-Key"); got != "config-key" {
		t.Errorf("x-api-key = %q, want the config key", got)
	}
}

func withAnthropic(a *config.AnthropicConfig) *config.Config {
	cfg := &config.Config{}
	cfg.Model.Anthropic = a
	return cfg
}

func TestCacheOptionsFromConfig(t *testing.T) {
	t.Parallel()
	off, on := false, true
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want CacheOptions
	}{
		{"nil config", nil, DefaultCacheOptions()},
		{"no anthropic block", &config.Config{}, DefaultCacheOptions()},
		{"no prompt_cache block", withAnthropic(&config.AnthropicConfig{}), DefaultCacheOptions()},
		{"enabled unset", withAnthropic(&config.AnthropicConfig{PromptCache: &config.PromptCacheConfig{}}), DefaultCacheOptions()},
		{"enabled true", withAnthropic(&config.AnthropicConfig{PromptCache: &config.PromptCacheConfig{Enabled: &on}}), DefaultCacheOptions()},
		{"enabled false", withAnthropic(&config.AnthropicConfig{PromptCache: &config.PromptCacheConfig{Enabled: &off}}), CacheOptions{}},
	} {
		if got := cacheOptionsFromConfig(tc.cfg); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestCacheOptionsFromConfig_TTL(t *testing.T) {
	t.Parallel()
	on := true
	for _, tc := range []struct {
		name string
		pc   *config.PromptCacheConfig
		want string
	}{
		{"no block", nil, config.PromptCacheTTL5m},
		{"empty ttl", &config.PromptCacheConfig{}, config.PromptCacheTTL5m},
		{"explicit 5m", &config.PromptCacheConfig{TTL: "5m"}, config.PromptCacheTTL5m},
		{"1h", &config.PromptCacheConfig{TTL: "1h"}, config.PromptCacheTTL1h},
		{"1h with enabled", &config.PromptCacheConfig{Enabled: &on, TTL: "1h"}, config.PromptCacheTTL1h},
		{"whitespace tolerated", &config.PromptCacheConfig{TTL: " 1h "}, config.PromptCacheTTL1h},
		{"garbage falls back", &config.PromptCacheConfig{TTL: "nope"}, config.PromptCacheTTL5m},
	} {
		cfg := withAnthropic(&config.AnthropicConfig{PromptCache: tc.pc})
		if got := cacheOptionsFromConfig(cfg).TTL; got != tc.want {
			t.Errorf("%s: TTL = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestWithCacheSystem_StillMeansNoCachingWhenFalse pins the compat
// contract for the pre-#714 option: WithCacheSystem(false) means no
// caching at all, not "no system marker, rolling history markers on".
func TestWithCacheSystem_StillMeansNoCachingWhenFalse(t *testing.T) {
	t.Parallel()
	off, err := New("test-key-not-real", WithCacheSystem(false))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := off.PromptCache(); got.Enabled() {
		t.Errorf("WithCacheSystem(false) left %+v, want caching fully off", got)
	}
	on, err := New("test-key-not-real", WithCacheSystem(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := on.PromptCache(); !got.System || got.History {
		t.Errorf("WithCacheSystem(true) left %+v, want the system marker only", got)
	}
}

func TestBuiltinDefaultsAndOptions(t *testing.T) {
	t.Parallel()
	if DefaultBuiltinTools().WebSearch {
		t.Error("WebSearch should be OFF by default — opt-in due to per-search billing")
	}
	p, err := New("test-key", WithWebSearch(true), WithBuiltinTools(BuiltinTools{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.builtins.WebSearch {
		t.Error("WithBuiltinTools should replace wholesale; WebSearch still on")
	}
	p, err = New("test-key", WithWebSearch(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.BuiltinToolNames(); len(got) != 1 || got[0] != "web_search" {
		t.Errorf("BuiltinToolNames = %v, want [web_search]", got)
	}
}
