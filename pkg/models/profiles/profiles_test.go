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

package profiles

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-models/profile"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// fakeServer is an OpenAI Chat Completions endpoint answering every
// request with one text reply and a usage block that carries cache
// reads, cache writes and reasoning — the fields core-agent has to
// carry into pkg/usage.
type fakeServer struct {
	mu   sync.Mutex
	auth []string
	body []map[string]any
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.body = append(f.body, body)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"google/gemma-4-26B-A4B-it",
		"choices":[{"index":0,"message":{"role":"assistant","content":"pod is pending"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,
			"prompt_tokens_details":{"cached_tokens":600,"created_cache_tokens":200},
			"completion_tokens_details":{"reasoning_tokens":20}}}`)
}

// profileConfig declares one profile pointing at srv and selects it.
func profileConfig(t *testing.T, name, url string) *config.Config {
	t.Helper()
	t.Setenv("TEST_PROFILE_KEY", "sekret")
	cfg := config.DefaultConfig()
	cfg.Model.Provider = name
	cfg.Model.Name = "google/gemma-4-26B-A4B-it"
	cfg.Providers = []profile.Profile{{
		Name:    name,
		Extends: "openai-compatible",
		BaseURL: url + "/v1",
		Models:  []profile.Model{{ID: "google/gemma-4-26B-A4B-it"}},
		Tiers:   map[profile.Tier]string{profile.Mid: "google/gemma-4-26B-A4B-it", profile.Small: "small-model"},
	}}
	cfg.Providers[0].Auth.Kind = "bearer"
	cfg.Providers[0].Auth.Env = "TEST_PROFILE_KEY"
	return cfg
}

// TestResolveAProfileEndToEnd: models.Resolve reaches the profile
// constructor for a declared name, the model talks to the server with
// the profile's credential, and the usage that comes back is what
// pkg/usage reads — cache writes included, through the legacy sidecar
// key usage.Rebuild also reads.
func TestResolveAProfileEndToEnd(t *testing.T) {
	srv := &fakeServer{}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	cfg := profileConfig(t, "test-e2e", ts.URL)

	p, err := models.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Name() != "test-e2e" {
		t.Errorf("Name = %q", p.Name())
	}
	if got := models.ResolveSmallModel(p, ""); got != "small-model" {
		t.Errorf("small model = %q, want the profile's small tier", got)
	}
	m, err := p.Model(context.Background(), cfg.Model.Name)
	if err != nil {
		t.Fatalf("Model: %v", err)
	}

	req := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("why is the pod pending?", genai.RoleUser)}}
	var last *model.LLMResponse
	for resp, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		last = resp
	}
	if last == nil || last.Content == nil || !strings.Contains(last.Content.Parts[0].Text, "pending") {
		t.Fatalf("response = %+v", last)
	}
	if len(srv.auth) != 1 || srv.auth[0] != "Bearer sekret" {
		t.Errorf("Authorization = %v, want the profile's bearer key", srv.auth)
	}

	tu := usage.TurnUsageFromMetadata(last.UsageMetadata, last.CustomMetadata)
	if tu.InputTokens != 1000 || tu.CachedInputTokens != 600 || tu.ThoughtsTokens != 20 {
		t.Errorf("TurnUsage = %+v, want prompt 1000, cache reads 600, thoughts 20", tu)
	}
	if tu.CacheCreationInputTokens != 200 {
		t.Errorf("CacheCreationInputTokens = %d, want 200 — the cache-write bucket did not reach pkg/usage", tu.CacheCreationInputTokens)
	}
}

// A closed profile refuses a model it does not list. openai-compatible
// is open by default, so the profile closes itself.
func TestModelOutsideAClosedProfileIsRefused(t *testing.T) {
	ts := httptest.NewServer(&fakeServer{})
	defer ts.Close()
	cfg := profileConfig(t, "test-closed", ts.URL)
	closed := false
	cfg.Providers[0].OpenModels = &closed
	delete(cfg.Providers[0].Tiers, profile.Small)
	p, err := models.Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := p.Model(context.Background(), "gemini-3.7-flash"); err == nil {
		t.Error("a closed profile served a model it does not list")
	}
}

// A server-side built-in the config turns on is refused at
// construction, not silently dropped.
func TestBuiltinToolsAreRefused(t *testing.T) {
	cfg := profileConfig(t, "test-builtins", "http://127.0.0.1:1")
	on := true
	cfg.Model.BuiltinTools = &config.BuiltinToolsConfig{WebSearch: &on}
	if _, err := models.Resolve(cfg); err == nil || !strings.Contains(err.Error(), "web_search") {
		t.Fatalf("Resolve err = %v, want web_search refused", err)
	}
}

// A missing credential fails at Resolve, naming the profile.
func TestMissingCredentialFailsAtResolve(t *testing.T) {
	cfg := profileConfig(t, "test-nokey", "http://127.0.0.1:1")
	t.Setenv("TEST_PROFILE_KEY", "")
	if _, err := models.Resolve(cfg); err == nil || !strings.Contains(err.Error(), "test-nokey") {
		t.Fatalf("Resolve err = %v, want a credential error naming the profile", err)
	}
}

func TestDefaultModel(t *testing.T) {
	cfg := profileConfig(t, "test-tiers", "http://127.0.0.1:1")
	if id, err := DefaultModel(cfg, "test-tiers", ""); err != nil || id != "google/gemma-4-26B-A4B-it" {
		t.Errorf("DefaultModel(no class) = %q, %v; want the mid tier", id, err)
	}
	if _, err := DefaultModel(cfg, "test-tiers", "frontier"); err == nil || !strings.Contains(err.Error(), "--model") {
		t.Errorf("DefaultModel(frontier) err = %v, want the fix named", err)
	}
}
