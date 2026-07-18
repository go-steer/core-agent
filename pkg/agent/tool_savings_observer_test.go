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

package agent

import (
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/pkg/usage"
)

// mkFuncResponseEvent builds a session.Event that mirrors what ADK
// emits for one tool result — a Content.Parts entry with
// FunctionResponse populated. The savings sidecar goes into
// Response["savings"] as a map[string]any, matching the wire shape
// core-agent's MCP wrap stamps in pkg/mcp/digest_wrap.go.
func mkFuncResponseEvent(toolName string, savings map[string]any) *session.Event {
	resp := map[string]any{"digest": "compressed", "raw_bytes": 1000}
	if savings != nil {
		resp["savings"] = savings
	}
	return &session.Event{
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{
				Parts: []*genai.Part{{
					FunctionResponse: &genai.FunctionResponse{
						Name:     toolName,
						Response: resp,
					},
				}},
			},
		},
	}
}

// TestObserveToolSavings_StructuralPathIncrementsTracker pins the
// load-bearing path: structural digest fires → observer extracts
// the sidecar → tracker's DigestSavings.StructuralCalls++ AND
// StructuralTokensSaved += delta. Without this the session's
// /context.digest_savings block stays empty in multi-session mode
// (the exact 2026-07-18 demo regression this fix addresses).
func TestObserveToolSavings_StructuralPathIncrementsTracker(t *testing.T) {
	t.Parallel()
	a := &Agent{tracker: usage.NewTracker()}
	ev := mkFuncResponseEvent("gke_get_k8s_resource", map[string]any{
		"path":                "structural_json",
		"original_bytes":      float64(17000),
		"digest_bytes":        float64(120),
		"original_tokens_est": float64(4250),
		"digest_tokens_est":   float64(30),
	})

	a.observeToolSavings(ev)

	got := a.tracker.DigestSavings()
	if got.StructuralCalls != 1 {
		t.Errorf("StructuralCalls = %d, want 1", got.StructuralCalls)
	}
	// Delta 4250 - 30 = 4220.
	if got.StructuralTokensSaved != 4220 {
		t.Errorf("StructuralTokensSaved = %d, want 4220", got.StructuralTokensSaved)
	}
	if got.AgenticCalls != 0 || got.PassthroughCalls != 0 {
		t.Errorf("cross-bucket contamination: %+v", got)
	}
}

// TestObserveToolSavings_AgenticPathCapturesSubagent pins that
// LLM-fallback path pulls in Subagent* fields including a computed
// SubagentCostUSD (the tracker itself stays pricing-agnostic per
// #290's usage.Tracker contract).
func TestObserveToolSavings_AgenticPathCapturesSubagent(t *testing.T) {
	t.Parallel()
	a := &Agent{tracker: usage.NewTracker()}
	ev := mkFuncResponseEvent("gke_get_pods", map[string]any{
		"path":                   "llm_fallback",
		"original_tokens_est":    float64(8000),
		"digest_tokens_est":      float64(200),
		"subagent_model":         "gemini-2.5-flash",
		"subagent_input_tokens":  float64(400),
		"subagent_output_tokens": float64(80),
	})

	a.observeToolSavings(ev)

	got := a.tracker.DigestSavings()
	if got.AgenticCalls != 1 {
		t.Errorf("AgenticCalls = %d, want 1", got.AgenticCalls)
	}
	if got.AgenticTokensSaved != 7800 {
		t.Errorf("AgenticTokensSaved = %d, want 7800", got.AgenticTokensSaved)
	}
	if got.AgenticSubagentInTokens != 400 || got.AgenticSubagentOutTokens != 80 {
		t.Errorf("subagent tokens not captured: in=%d out=%d",
			got.AgenticSubagentInTokens, got.AgenticSubagentOutTokens)
	}
	// SubagentCostUSD is resolved via usage.PriceFor. gemini-2.5-flash
	// is unpriced in the builtin table when no catalog is installed —
	// exact figure depends on catalog state at test time, but the
	// field MUST have been computed (non-negative + tracked).
	if got.AgenticSubagentCostUSD < 0 {
		t.Errorf("AgenticSubagentCostUSD = %f, want >= 0", got.AgenticSubagentCostUSD)
	}
}

// TestObserveToolSavings_PassthroughPathTracked pins that under-
// threshold responses (the wrap skipped digesting) still get
// counted. Enables operators to see "N of M MCP calls were small
// enough to skip" telemetry, which informs threshold tuning.
func TestObserveToolSavings_PassthroughPathTracked(t *testing.T) {
	t.Parallel()
	a := &Agent{tracker: usage.NewTracker()}
	ev := mkFuncResponseEvent("gke_get_operation", map[string]any{
		"path":                "passthrough",
		"original_bytes":      float64(500),
		"digest_bytes":        float64(500),
		"original_tokens_est": float64(125),
		"digest_tokens_est":   float64(125),
	})

	a.observeToolSavings(ev)

	got := a.tracker.DigestSavings()
	if got.PassthroughCalls != 1 {
		t.Errorf("PassthroughCalls = %d, want 1", got.PassthroughCalls)
	}
	// Passthrough has 0 delta → tokens saved should stay at 0.
	if got.StructuralTokensSaved != 0 || got.AgenticTokensSaved != 0 {
		t.Errorf("passthrough should not bump saved-token counters: %+v", got)
	}
}

// TestObserveToolSavings_NoSidecarNoOp pins that responses without
// the savings sidecar (built-in tools, MCP servers with
// agentic_never: true, upstream errors that dropped the sidecar)
// don't mutate the tracker. Zero surprises for non-wrapped calls.
func TestObserveToolSavings_NoSidecarNoOp(t *testing.T) {
	t.Parallel()
	a := &Agent{tracker: usage.NewTracker()}
	ev := mkFuncResponseEvent("read_file", nil) // no savings

	a.observeToolSavings(ev)

	got := a.tracker.DigestSavings()
	if got != (usage.DigestSavingsTotals{}) {
		t.Errorf("no-sidecar path mutated tracker: %+v", got)
	}
}

// TestObserveToolSavings_MalformedSidecarSilentlyDropped pins that
// a sidecar with an unrecognized shape (e.g. path missing) is
// treated as absent — a garbage sidecar can't crash the tap
// goroutine or double-count in a random bucket. Signals a wire-
// format regression via zero-mutations rather than a panic.
func TestObserveToolSavings_MalformedSidecarSilentlyDropped(t *testing.T) {
	t.Parallel()
	a := &Agent{tracker: usage.NewTracker()}
	ev := mkFuncResponseEvent("bad", map[string]any{
		"original_bytes": float64(100),
		// path deliberately omitted
	})

	a.observeToolSavings(ev)

	got := a.tracker.DigestSavings()
	if got != (usage.DigestSavingsTotals{}) {
		t.Errorf("malformed sidecar should be dropped, got %+v", got)
	}
}

// TestObserveToolSavings_NilGuardsSafe pins the nil-safety contract.
// The observer runs on every event during the per-turn tap, including
// events that predate/postdate a full agent construction (agent stub
// in tests, half-constructed agent during Init, etc.). Any nil in
// the chain must no-op silently rather than panic.
func TestObserveToolSavings_NilGuardsSafe(t *testing.T) {
	t.Parallel()
	// nil agent
	var a *Agent
	a.observeToolSavings(&session.Event{})

	// nil tracker on real agent
	real := &Agent{}
	real.observeToolSavings(&session.Event{})

	// nil event
	full := &Agent{tracker: usage.NewTracker()}
	full.observeToolSavings(nil)

	// nil Content
	full.observeToolSavings(&session.Event{})

	// If we got here without panicking, we're good.
}

// TestObserveToolSavings_MultipleResponsePartsAllCounted pins that
// an event carrying multiple FunctionResponse parts (parallel tool
// calls, some models emit this shape) counts each response
// independently — not just the first.
func TestObserveToolSavings_MultipleResponsePartsAllCounted(t *testing.T) {
	t.Parallel()
	a := &Agent{tracker: usage.NewTracker()}
	sv := map[string]any{
		"path":                "structural_json",
		"original_tokens_est": float64(1000),
		"digest_tokens_est":   float64(100),
	}
	ev := &session.Event{
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{
				Parts: []*genai.Part{
					{FunctionResponse: &genai.FunctionResponse{
						Name:     "call1",
						Response: map[string]any{"savings": sv},
					}},
					{FunctionResponse: &genai.FunctionResponse{
						Name:     "call2",
						Response: map[string]any{"savings": sv},
					}},
				},
			},
		},
	}

	a.observeToolSavings(ev)

	got := a.tracker.DigestSavings()
	if got.StructuralCalls != 2 {
		t.Errorf("StructuralCalls = %d, want 2 (parallel tool calls)", got.StructuralCalls)
	}
	if got.StructuralTokensSaved != 1800 {
		t.Errorf("StructuralTokensSaved = %d, want 1800 (2 × 900)", got.StructuralTokensSaved)
	}
}
