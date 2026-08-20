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

package compose

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// failingProvider resolves no models. Stands in for a misconfigured
// --mcp-digest-model (typo'd id, provider that doesn't serve it).
type failingProvider struct{ err error }

func (failingProvider) Name() string { return "failing" }
func (p failingProvider) Model(context.Context, string) (adkmodel.LLM, error) {
	return nil, p.err
}

// countingProvider records how many times Model was called so the
// once-and-cache contract can be asserted.
type countingProvider struct {
	inner adkmodel.LLM
	calls int
}

func (countingProvider) Name() string { return "counting" }
func (p *countingProvider) Model(context.Context, string) (adkmodel.LLM, error) {
	p.calls++
	return p.inner, nil
}

// meteredLLM answers once with a complete turn AND reports token
// usage. The echo mock backing newTestAgent reports none, which makes
// any "was this billed?" assertion vacuous.
type meteredLLM struct{}

func (meteredLLM) Name() string { return "metered" }
func (meteredLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content: &genai.Content{
				Role:  "model",
				Parts: []*genai.Part{{Text: "digested"}},
			},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     500,
				CandidatesTokenCount: 50,
			},
			TurnComplete: true,
		}, nil)
	}
}

// cachingLLM answers with a prompt that was mostly served from cache
// and partly written to it — the shape Anthropic reports once #714's
// prompt caching is on. CachedContentTokenCount is the read subset;
// the write bucket rides CustomMetadata, since genai's usage struct
// has no third input field (#263). Both are subsets of
// PromptTokenCount.
type cachingLLM struct{}

func (cachingLLM) Name() string { return "caching" }
func (cachingLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content: &genai.Content{
				Role:  "model",
				Parts: []*genai.Part{{Text: "digested"}},
			},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:        10_000,
				CachedContentTokenCount: 9_000,
				CandidatesTokenCount:    100,
			},
			CustomMetadata: map[string]any{usage.CacheCreationTokensMetadataKey: int64(500)},
			TurnComplete:   true,
		}, nil)
	}
}

// TestBuildMCPDigestLLMFallback_ForwardsTheSubagentCacheBuckets is the
// middle link of #771. RunSubtask knows how the digest's prompt broke
// down across the three input buckets; the calling session re-prices
// the digest from the sidecar. Dropping the buckets in between is what
// made that re-pricing bill the whole prompt at the uncached rate.
func TestBuildMCPDigestLLMFallback_ForwardsTheSubagentCacheBuckets(t *testing.T) {
	t.Parallel()

	a, err := agent.New(cachingLLM{},
		agent.WithAppName("app"),
		agent.WithSession("u", "sess-digest-buckets"),
		agent.WithUsageTracker(usage.NewTracker()),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	fn := BuildMCPDigestLLMFallback(&a, nil, "small-model")

	res, err := fn(context.Background(), []byte(`{"pods":[{"name":"api-7d9"}]}`))
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if res.SubagentInputTokens != 10_000 {
		t.Errorf("SubagentInputTokens = %d, want 10000 (the buckets are subsets, not addends)",
			res.SubagentInputTokens)
	}
	if res.SubagentCachedInputTokens != 9_000 {
		t.Errorf("SubagentCachedInputTokens = %d, want 9000", res.SubagentCachedInputTokens)
	}
	if res.SubagentCacheCreationInputTokens != 500 {
		t.Errorf("SubagentCacheCreationInputTokens = %d, want 500", res.SubagentCacheCreationInputTokens)
	}
}

func TestBuildMCPDigestLLMFallback_UnboundAgent(t *testing.T) {
	t.Parallel()

	// Late binding: mcp.Build runs before agent.New, so the closure
	// captures a **agent.Agent that's still nil at construction. If
	// the wrap ever fires before the post-construct hook populates
	// it, that has to be a legible error rather than a nil deref
	// taking the daemon down mid-turn.
	var ref *agent.Agent
	fn := BuildMCPDigestLLMFallback(&ref, nil, "")
	if fn == nil {
		t.Fatal("BuildMCPDigestLLMFallback returned nil")
	}

	res, err := fn(context.Background(), []byte("{}"))
	if err == nil {
		t.Fatalf("got %+v and no error, want an error", res)
	}
	if !strings.Contains(err.Error(), "agent not yet bound") {
		t.Errorf("error = %v, want it to name the binding race", err)
	}
}

func TestBuildMCPDigestLLMFallback_ModelResolutionFailure(t *testing.T) {
	t.Parallel()

	a := newTestAgent(t, "app", "u", "sess-digest-fail")
	sentinel := errors.New("model not found")
	fn := BuildMCPDigestLLMFallback(&a, failingProvider{err: sentinel}, "typo-model")

	_, err := fn(context.Background(), []byte(`{"items":[]}`))
	if err == nil {
		t.Fatal("want an error when the digest model can't be resolved")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "typo-model") {
		t.Errorf("error = %v, want it to quote the unresolvable model id", err)
	}
}

func TestBuildMCPDigestLLMFallback_ResolvesTheModelOnce(t *testing.T) {
	t.Parallel()

	// The closure caches the resolved LLM: an MCP-heavy session fires
	// the fallback on every oversized response, and re-resolving per
	// call would mean a provider round trip per tool call.
	a := newTestAgent(t, "app", "u", "sess-digest-cache")
	prov := &countingProvider{inner: stubLLM{}}
	fn := BuildMCPDigestLLMFallback(&a, prov, "small-model")

	for range 3 {
		// stubLLM errors on invocation, so the subtask fails — the
		// resolution has already happened by then, which is what's
		// under test.
		_, _ = fn(context.Background(), []byte(`{"items":[]}`))
	}
	if prov.calls != 1 {
		t.Errorf("provider.Model called %d times across 3 fallbacks, want 1", prov.calls)
	}
}

func TestBuildMCPDigestLLMFallback_InheritsParentModelWhenUnset(t *testing.T) {
	t.Parallel()

	// Empty modelID means "no cheap tier configured; run the subtask
	// on the parent's model". The provider must not be consulted at
	// all — asking it for "" is how you get a confusing resolve
	// error on a supported configuration.
	a := newTestAgent(t, "app", "u", "sess-digest-inherit")
	prov := &countingProvider{inner: stubLLM{}}
	fn := BuildMCPDigestLLMFallback(&a, prov, "")

	_, _ = fn(context.Background(), []byte(`{"items":[]}`))
	if prov.calls != 0 {
		t.Errorf("provider.Model called %d times with an empty model id, want 0", prov.calls)
	}
}

func TestBuildMCPDigestLLMFallback_DigestsThroughTheSubtask(t *testing.T) {
	t.Parallel()

	// End to end against an echo-backed agent: the raw MCP payload
	// must reach the subtask and the digest must come back as the
	// result Text, with SubagentModel mirroring the configured id so
	// display-side pricing bills the subtask at its own tier rather
	// than the parent's.
	a := newTestAgent(t, "app", "u", "sess-digest-ok")
	fn := BuildMCPDigestLLMFallback(&a, nil, "small-model")

	const raw = `{"pods":[{"name":"api-7d9","status":"CrashLoopBackOff"}]}`
	res, err := fn(context.Background(), []byte(raw))
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if res.Text == "" {
		t.Error("digest text is empty")
	}
	if !strings.Contains(res.Text, "CrashLoopBackOff") {
		t.Errorf("digest %q doesn't reflect the raw payload — the subtask didn't receive it", res.Text)
	}
	if res.SubagentModel != "small-model" {
		t.Errorf("SubagentModel = %q, want the configured digest model", res.SubagentModel)
	}
}

// TestBuildMCPDigestLLMFallback_ReportsParentModelWhenInheriting: with
// no cheap tier configured the subtask runs on the parent's model, and
// the sidecar has to say so.
//
// Post-#717 the sidecar is the only channel by which a digest's cost
// reaches a session's ledger — the running agent no longer bills
// itself — and pkg/mcp drops the token fields entirely when
// SubagentModel is empty. So an empty value here isn't cosmetic; it
// silently loses the spend.
func TestBuildMCPDigestLLMFallback_ReportsParentModelWhenInheriting(t *testing.T) {
	t.Parallel()

	a := newTestAgent(t, "app", "u", "sess-digest-inherit-model")
	fn := BuildMCPDigestLLMFallback(&a, nil, "")

	res, err := fn(context.Background(), []byte(`{"pods":[{"name":"api-7d9"}]}`))
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if res.SubagentModel == "" {
		t.Error("SubagentModel is empty on the inherit-model path; the digest's cost would never be billed to any session")
	}
	if want := a.ModelName(); res.SubagentModel != want {
		t.Errorf("SubagentModel = %q, want the parent's model %q — that's what actually spent the tokens", res.SubagentModel, want)
	}
}

// TestBuildMCPDigestLLMFallback_DoesNotBillTheBoundAgent pins the
// compose-side half of #717: the closure captures the PRIMARY agent
// (late-bound at boot), but it fires for whichever session made the
// MCP call. Billing the bound agent charged every session's digests to
// the primary and counted them against the primary's cost ceiling.
func TestBuildMCPDigestLLMFallback_DoesNotBillTheBoundAgent(t *testing.T) {
	t.Parallel()

	tr := usage.NewTracker()
	// meteredLLM, not the echo mock: the echo mock reports no
	// UsageMetadata, so RunSubtask has nothing to append and the
	// assertion below would hold whether or not SkipParentUsage is set.
	a, err := agent.New(meteredLLM{},
		agent.WithAppName("app"),
		agent.WithSession("u", "sess-digest-nobill"),
		agent.WithUsageTracker(tr),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	fn := BuildMCPDigestLLMFallback(&a, nil, "small-model")

	if _, err := fn(context.Background(), []byte(`{"pods":[{"name":"api-7d9"}]}`)); err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if got := tr.Totals().Turns; got != 0 {
		t.Errorf("bound agent's tracker recorded %d turn(s); want 0 — the calling session picks the cost up from the savings sidecar", got)
	}
}
