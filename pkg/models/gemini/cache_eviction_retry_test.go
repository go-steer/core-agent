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
	"strings"
	"sync/atomic"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// perCallLLM returns a different slice of pairs on each successive
// GenerateContent invocation — enough to drive the wrapper's cache-
// eviction retry, where turn 1 must return the NOT_FOUND signature
// and turn 2 (the retry) returns a real response.
type perCallLLM struct {
	calls    atomic.Int32
	perCall  [][]pair
	lastReqs []*adkmodel.LLMRequest
}

func (p *perCallLLM) Name() string { return "per-call" }

func (p *perCallLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	// Snapshot the request's cache-relevant fields at call time. Later
	// mutations by the wrapper (the restore-on-retry path) would
	// otherwise clobber what turn 1 saw.
	snap := &adkmodel.LLMRequest{}
	if req.Config != nil {
		cfg := *req.Config
		snap.Config = &cfg
	}
	p.lastReqs = append(p.lastReqs, snap)
	idx := int(p.calls.Add(1) - 1)
	var s []pair
	if idx < len(p.perCall) {
		s = p.perCall[idx]
	}
	return seqOf(s)
}

// TestIsCachedContentNotFound pins the classifier that decides which
// error text signals TTL-eviction of a Vertex explicit cache. Two
// false negatives here would send the wrong recovery path — a
// generic NOT_FOUND (missing model, wrong region) would trigger a
// pointless invalidate + retry; a real cache eviction would surface
// as a hard turn error the operator has to restart around.
func TestIsCachedContentNotFound(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"real TTL eviction (verbatim Vertex text)",
			errors.New("Error 404, Message: Not found: cached content metadata for 6116704758662168576., Status: NOT_FOUND, Details: []"),
			true},
		{"NOT_FOUND on missing model", errors.New("Error 404, Message: publisher model not found, Status: NOT_FOUND"), false},
		{"NOT_FOUND on wrong region", errors.New("resource not found: NOT_FOUND"), false},
		{"cached content but not NOT_FOUND", errors.New("cached content quota exceeded"), false},
		{"case-insensitive cached content match",
			errors.New("Error 404, Message: Cached Content missing, Status: NOT_FOUND"),
			true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCachedContentNotFound(tc.err); got != tc.want {
				t.Errorf("isCachedContentNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestBuiltinsLLM_CacheEviction_RetriesUncachedAndInvalidates pins the
// full recovery path: cache-stamped request → inner returns 404 →
// wrapper calls cacheInvalidate → wrapper restores stripped fields →
// wrapper retries → retry succeeds. Regression signal: if this fails,
// TTL-evicted caches on long-lived daemons wedge sessions with hard
// turn errors and require a daemon restart to recover.
func TestBuiltinsLLM_CacheEviction_RetriesUncachedAndInvalidates(t *testing.T) {
	captureLogf(t) // discard logf calls emitted by the retry path

	// Call 1: NOT_FOUND on the cache reference.
	// Call 2: real content, indicating the retry worked.
	evictionErr := errors.New("Error 404, Message: Not found: cached content metadata for 6116704758662168576., Status: NOT_FOUND, Details: []")
	fake := &perCallLLM{
		perCall: [][]pair{
			{{err: evictionErr}},
			{{resp: mkResponse("hi from uncached retry")}},
		},
	}

	invalidateCalls := 0
	invalidateReasons := []string{}
	wrapped := &builtinsLLM{
		inner:     fake,
		cacheName: func(_ context.Context) string { return "projects/p/locations/l/cachedContents/dead" },
		cacheInvalidate: func(reason string) {
			invalidateCalls++
			invalidateReasons = append(invalidateReasons, reason)
		},
	}

	// The caller (ADK) hands us a request with SystemInstruction +
	// Tools + ToolConfig set — the wrapper strips them for the cached
	// turn AND must restore them for the uncached retry.
	sysInstr := &genai.Content{Parts: []*genai.Part{{Text: "system prompt"}}}
	userTool := &genai.Tool{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "my_func"}}}
	toolCfg := &genai.ToolConfig{}
	req := &adkmodel.LLMRequest{
		Config: &genai.GenerateContentConfig{
			SystemInstruction: sysInstr,
			Tools:             []*genai.Tool{userTool},
			ToolConfig:        toolCfg,
		},
	}
	out := collect(wrapped.GenerateContent(context.Background(), req, false))

	if invalidateCalls != 1 {
		t.Errorf("cacheInvalidate calls = %d, want 1 (%v)", invalidateCalls, invalidateReasons)
	}
	if len(invalidateReasons) == 1 && !strings.Contains(invalidateReasons[0], "404") {
		t.Errorf("invalidate reason should mention 404 for grep-triage: %q", invalidateReasons[0])
	}
	if fake.calls.Load() != 2 {
		t.Fatalf("inner GenerateContent calls = %d, want 2 (initial + retry)", fake.calls.Load())
	}

	// Turn 1's snapshot should show the cached-turn shape: CachedContent
	// stamped, stripped fields nil'd.
	turn1 := fake.lastReqs[0]
	if turn1.Config.CachedContent != "projects/p/locations/l/cachedContents/dead" {
		t.Errorf("turn 1 CachedContent = %q, want the dead cache name", turn1.Config.CachedContent)
	}
	if turn1.Config.SystemInstruction != nil || turn1.Config.Tools != nil || turn1.Config.ToolConfig != nil {
		t.Errorf("turn 1 should have stripped SI/Tools/ToolConfig, got %+v", turn1.Config)
	}

	// Turn 2's snapshot should show the restored uncached shape:
	// CachedContent cleared, SI/Tools/ToolConfig restored.
	turn2 := fake.lastReqs[1]
	if turn2.Config.CachedContent != "" {
		t.Errorf("turn 2 CachedContent should be empty, got %q", turn2.Config.CachedContent)
	}
	if turn2.Config.SystemInstruction != sysInstr {
		t.Errorf("turn 2 should restore SystemInstruction, got %+v", turn2.Config.SystemInstruction)
	}
	if len(turn2.Config.Tools) != 1 || turn2.Config.Tools[0] != userTool {
		t.Errorf("turn 2 should restore Tools, got %+v", turn2.Config.Tools)
	}
	if turn2.Config.ToolConfig != toolCfg {
		t.Errorf("turn 2 should restore ToolConfig, got %+v", turn2.Config.ToolConfig)
	}

	// Caller sees the retry's content only — the eviction error is
	// swallowed, not yielded upstream.
	if len(out) != 1 {
		t.Fatalf("caller should see 1 chunk (the retry's success), got %d: %+v", len(out), out)
	}
	if out[0].err != nil {
		t.Errorf("retry chunk should be error-free, got err=%v", out[0].err)
	}
	if out[0].resp == nil || out[0].resp.Content == nil || len(out[0].resp.Content.Parts) == 0 ||
		out[0].resp.Content.Parts[0].Text != "hi from uncached retry" {
		t.Errorf("retry response text mismatch: %+v", out[0].resp)
	}
}

// TestBuiltinsLLM_CacheEviction_NonCachedTurnPassesThrough pins that
// the retry wrapper is a no-op when the cache name resolves to "".
// Regression signal: if this fails, un-cached turns pay a per-turn
// buffer-and-flush overhead for no gain.
func TestBuiltinsLLM_CacheEviction_NonCachedTurnPassesThrough(t *testing.T) {
	t.Parallel()
	fake := &perCallLLM{
		perCall: [][]pair{
			{{resp: mkResponse("plain")}},
		},
	}
	invalidateCalls := 0
	wrapped := &builtinsLLM{
		inner:           fake,
		cacheName:       func(_ context.Context) string { return "" }, // nothing to stamp
		cacheInvalidate: func(_ string) { invalidateCalls++ },
	}
	out := collect(wrapped.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false))
	if fake.calls.Load() != 1 {
		t.Errorf("inner calls = %d, want 1 (no retry on uncached turn)", fake.calls.Load())
	}
	if invalidateCalls != 0 {
		t.Errorf("invalidateCalls = %d, want 0 (no eviction, no invalidate)", invalidateCalls)
	}
	if len(out) != 1 || out[0].resp == nil {
		t.Errorf("uncached turn should pass through unchanged: %+v", out)
	}
}

// TestBuiltinsLLM_CacheEviction_NilInvalidateHookIsSafe pins that the
// retry still fires when no invalidate hook is wired — the manager
// doesn't reset, but this-turn recovery still works so the operator
// isn't blocked on wiring the hook.
func TestBuiltinsLLM_CacheEviction_NilInvalidateHookIsSafe(t *testing.T) {
	captureLogf(t)
	evictionErr := errors.New("Error 404, Message: Not found: cached content metadata for X., Status: NOT_FOUND")
	fake := &perCallLLM{
		perCall: [][]pair{
			{{err: evictionErr}},
			{{resp: mkResponse("uncached ok")}},
		},
	}
	wrapped := &builtinsLLM{
		inner:     fake,
		cacheName: func(_ context.Context) string { return "some-cache" },
		// cacheInvalidate intentionally nil
	}
	out := collect(wrapped.GenerateContent(context.Background(), &adkmodel.LLMRequest{
		Config: &genai.GenerateContentConfig{},
	}, false))
	if fake.calls.Load() != 2 {
		t.Errorf("inner calls = %d, want 2 (retry still fires without invalidate hook)", fake.calls.Load())
	}
	if len(out) != 1 || out[0].resp == nil {
		t.Errorf("expected retry to yield real content, got %+v", out)
	}
}
