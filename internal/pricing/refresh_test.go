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

package pricing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// sampleLiteLLM mirrors the subset of LiteLLM's schema we parse,
// with both happy entries (chat-mode + costs) and shapes we should
// filter out (image generation, missing-cost entries, the
// documentation sample_spec row).
const sampleLiteLLM = `{
  "sample_spec": {"input_cost_per_token": 999, "output_cost_per_token": 999, "mode": "chat"},
  "gemini-3.5-flash": {
    "input_cost_per_token": 0.0000015,
    "output_cost_per_token": 0.000009,
    "cache_read_input_token_cost": 0.00000015,
    "litellm_provider": "vertex_ai-language-models",
    "mode": "chat"
  },
  "claude-opus-4-7": {
    "input_cost_per_token": 0.000015,
    "output_cost_per_token": 0.000075,
    "cache_read_input_token_cost": 0.0000015,
    "cache_creation_input_token_cost": 0.00001875,
    "mode": "chat"
  },
  "dall-e-3": {
    "litellm_provider": "openai",
    "mode": "image_generation"
  },
  "embedding-only": {
    "input_cost_per_token": 0.0000001,
    "mode": "embedding"
  }
}`

func TestRefresh_HappyPathWritesExternalSection(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `W/"abc123"`)
		_, _ = w.Write([]byte(sampleLiteLLM))
	}))
	defer srv.Close()

	home := t.TempDir()
	out, err := Refresh(context.Background(), home, RefreshOptions{
		Source: srv.URL,
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if out.ModelCount != 2 {
		t.Errorf("ModelCount = %d, want 2 (gemini + claude; image+embedding filtered, sample_spec dropped)", out.ModelCount)
	}

	uf, err := LoadUserFile(home)
	if err != nil {
		t.Fatalf("LoadUserFile: %v", err)
	}
	if uf.External == nil {
		t.Fatal("External section not written")
	}
	if uf.External.ETag != `W/"abc123"` {
		t.Errorf("ETag = %q, want %q", uf.External.ETag, `W/"abc123"`)
	}
	gemini, ok := uf.External.Models["gemini-3.5-flash"]
	if !ok {
		t.Fatal("gemini-3.5-flash not written")
	}
	// 0.0000015 per token * 1e6 = 1.50 per million.
	if gemini.InputPerMTok != 1.50 {
		t.Errorf("gemini-3.5-flash InputPerMTok = %v, want 1.50", gemini.InputPerMTok)
	}
	// 0.00000015 per token * 1e6 = 0.15 per million (10% of input,
	// per Google's published Gemini 3.5 Flash cache-read rate).
	if gemini.CachedInputPerMTok != 0.15 {
		t.Errorf("gemini-3.5-flash CachedInputPerMTok = %v, want 0.15", gemini.CachedInputPerMTok)
	}
	// Anthropic gets a cache-read rate too. Cache-creation is captured
	// on the LiteLLM entry but not yet plumbed into our Rates schema
	// (Slice B follow-up).
	claude, ok := uf.External.Models["claude-opus-4-7"]
	if !ok {
		t.Fatal("claude-opus-4-7 not written")
	}
	if claude.CachedInputPerMTok != 1.50 {
		t.Errorf("claude-opus-4-7 CachedInputPerMTok = %v, want 1.50 (10%% of $15/M input)", claude.CachedInputPerMTok)
	}
	if _, ok := uf.External.Models["dall-e-3"]; ok {
		t.Error("image_generation entry should have been filtered")
	}
	if _, ok := uf.External.Models["embedding-only"]; ok {
		t.Error("embedding entry should have been filtered")
	}
}

func TestRefresh_PreservesManualSection(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleLiteLLM))
	}))
	defer srv.Close()

	home := t.TempDir()
	// Pre-populate manual section.
	pre := &UserFile{
		Version: 1,
		Manual: &ManualSection{Models: map[string]ModelRates{
			"my-internal-model": {InputPerMTok: 1.0, OutputPerMTok: 2.0},
		}},
	}
	if err := SaveUserFile(home, pre); err != nil {
		t.Fatalf("SaveUserFile pre: %v", err)
	}

	if _, err := Refresh(context.Background(), home, RefreshOptions{Source: srv.URL}); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	uf, _ := LoadUserFile(home)
	if uf.Manual == nil || uf.Manual.Models["my-internal-model"].InputPerMTok != 1.0 {
		t.Errorf("manual section lost: %+v", uf.Manual)
	}
	if uf.External == nil || len(uf.External.Models) == 0 {
		t.Error("external section not written")
	}
}

func TestRefresh_SkipsWhenCacheFresh(t *testing.T) {
	t.Parallel()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(sampleLiteLLM))
	}))
	defer srv.Close()

	home := t.TempDir()
	// Seed a fresh cache (10 min old).
	pre := &UserFile{
		Version: 1,
		External: &ExternalSource{
			FetchedAt: time.Now().Add(-10 * time.Minute),
			Source:    srv.URL,
			Models:    map[string]ModelRates{"x": {InputPerMTok: 1, OutputPerMTok: 1}},
		},
	}
	if err := SaveUserFile(home, pre); err != nil {
		t.Fatalf("SaveUserFile: %v", err)
	}

	out, err := Refresh(context.Background(), home, RefreshOptions{Source: srv.URL})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !out.Skipped {
		t.Errorf("expected Skipped=true for fresh cache; got %+v", out)
	}
	if hits != 0 {
		t.Errorf("upstream was hit %d times; should have been skipped entirely", hits)
	}
}

func TestRefresh_ForcesWhenIntervalNegative(t *testing.T) {
	t.Parallel()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(sampleLiteLLM))
	}))
	defer srv.Close()

	home := t.TempDir()
	// Even a 0s-old cache shouldn't skip when MinInterval is negative.
	pre := &UserFile{
		Version:  1,
		External: &ExternalSource{FetchedAt: time.Now(), Models: map[string]ModelRates{"x": {InputPerMTok: 1}}},
	}
	_ = SaveUserFile(home, pre)

	out, err := Refresh(context.Background(), home, RefreshOptions{
		Source:      srv.URL,
		MinInterval: -1 * time.Second,
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if out.Skipped {
		t.Error("negative MinInterval should force a fetch, not skip")
	}
	if hits != 1 {
		t.Errorf("upstream hits = %d, want 1", hits)
	}
}

func TestRefresh_RespectsIfNoneMatch(t *testing.T) {
	t.Parallel()
	hits := 0
	const wantETag = `"abc123"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("If-None-Match") == wantETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", wantETag)
		_, _ = w.Write([]byte(sampleLiteLLM))
	}))
	defer srv.Close()

	home := t.TempDir()
	// Stale cache (older than default 24h) WITH a known etag.
	pre := &UserFile{
		Version: 1,
		External: &ExternalSource{
			FetchedAt: time.Now().Add(-48 * time.Hour),
			Source:    srv.URL,
			ETag:      wantETag,
			Models:    map[string]ModelRates{"x": {InputPerMTok: 1}},
		},
	}
	_ = SaveUserFile(home, pre)

	out, err := Refresh(context.Background(), home, RefreshOptions{Source: srv.URL})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !out.NotModified {
		t.Errorf("expected NotModified=true on 304 reply; got %+v", out)
	}
	if hits != 1 {
		t.Errorf("expected exactly 1 upstream hit; got %d", hits)
	}
	// FetchedAt should have been bumped so the next call within
	// MinInterval is skipped.
	uf, _ := LoadUserFile(home)
	if time.Since(uf.External.FetchedAt) > 2*time.Second {
		t.Errorf("FetchedAt not bumped after 304: %v", uf.External.FetchedAt)
	}
}

func TestRefresh_NetworkFailureUsesCache(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	home := t.TempDir()
	pre := &UserFile{
		Version: 1,
		External: &ExternalSource{
			FetchedAt: time.Now().Add(-72 * time.Hour),
			Source:    srv.URL,
			Models:    map[string]ModelRates{"x": {InputPerMTok: 5}},
		},
	}
	_ = SaveUserFile(home, pre)

	out, err := Refresh(context.Background(), home, RefreshOptions{Source: srv.URL})
	if err != nil {
		t.Fatalf("Refresh should not error on 5xx: %v", err)
	}
	if !out.NetworkFailed {
		t.Errorf("expected NetworkFailed=true; got %+v", out)
	}
	if out.NetworkError == nil {
		t.Error("expected NetworkError populated on 5xx")
	}
	if out.StaleAge < 71*time.Hour {
		t.Errorf("StaleAge = %v, want ≥71h", out.StaleAge)
	}
	// Cache should be intact.
	uf, _ := LoadUserFile(home)
	if uf.External == nil || uf.External.Models["x"].InputPerMTok != 5 {
		t.Errorf("cache was clobbered on failure: %+v", uf)
	}
}

func TestRefresh_MalformedBodyKeepsCache(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer srv.Close()

	home := t.TempDir()
	pre := &UserFile{
		Version: 1,
		External: &ExternalSource{
			FetchedAt: time.Now().Add(-2 * 24 * time.Hour),
			Models:    map[string]ModelRates{"keep-me": {InputPerMTok: 7}},
		},
	}
	_ = SaveUserFile(home, pre)

	out, err := Refresh(context.Background(), home, RefreshOptions{Source: srv.URL})
	if err != nil {
		t.Fatalf("Refresh should not error on malformed body: %v", err)
	}
	if !out.NetworkFailed {
		t.Error("expected NetworkFailed=true on malformed body")
	}
	uf, _ := LoadUserFile(home)
	if uf.External.Models["keep-me"].InputPerMTok != 7 {
		t.Error("cache was clobbered by malformed-body fall-through")
	}
}

func TestRefresh_EmptyUserHomeErrors(t *testing.T) {
	t.Parallel()
	_, err := Refresh(context.Background(), "", RefreshOptions{})
	if err == nil {
		t.Error("Refresh with empty userHome should error")
	}
}

func TestParseLiteLLMBody_FiltersAndConverts(t *testing.T) {
	t.Parallel()
	parsed, err := parseLiteLLMBody([]byte(sampleLiteLLM))
	if err != nil {
		t.Fatalf("parseLiteLLMBody: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("len = %d, want 2 (gemini + claude); got %v", len(parsed), keysOf(parsed))
	}
	got := parsed["gemini-3.5-flash"]
	wantInput := 1.50
	if got.InputPerMTok != wantInput {
		t.Errorf("per-token → per-MTok conversion off: got %v, want %v", got.InputPerMTok, wantInput)
	}
	// Cache-read rate present + correctly converted.
	if got.CachedInputPerMTok != 0.15 {
		t.Errorf("cache_read_input_token_cost not mapped: got %v, want 0.15", got.CachedInputPerMTok)
	}
	// Anthropic entry also carries the cache-read rate.
	claude := parsed["claude-opus-4-7"]
	if claude.CachedInputPerMTok != 1.50 {
		t.Errorf("claude cache-read: got %v, want 1.50", claude.CachedInputPerMTok)
	}
}

func TestParseLiteLLMBody_EmptyBodyErrors(t *testing.T) {
	t.Parallel()
	_, err := parseLiteLLMBody([]byte(`{}`))
	if err == nil {
		t.Error("empty body should error (no usable entries)")
	}
}

// Verify the sample we use is JSON-valid; catches typos in the test
// constant before they cascade into confusing per-test failures.
func TestSampleLiteLLM_IsValidJSON(t *testing.T) {
	t.Parallel()
	var x map[string]json.RawMessage
	if err := json.Unmarshal([]byte(sampleLiteLLM), &x); err != nil {
		t.Fatalf("sampleLiteLLM invalid: %v", err)
	}
}

// keysOf is a small helper for test diagnostics.
func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Sanity check that a malformed URL returns the error from
// http.NewRequestWithContext rather than masquerading as a
// NetworkFailed soft-fail.
func TestRefresh_BadSourceURLHardErrors(t *testing.T) {
	t.Parallel()
	_, err := Refresh(context.Background(), t.TempDir(), RefreshOptions{
		Source: "::not-a-url::",
	})
	if err == nil {
		t.Error("malformed Source URL should hard-error")
	}
}
