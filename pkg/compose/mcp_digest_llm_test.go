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
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/v2/pkg/agent"
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
