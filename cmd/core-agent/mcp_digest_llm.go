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

package main

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/mcp"
	"github.com/go-steer/core-agent/v2/pkg/models"
)

// mcpDigestTracer scopes the subagent.llm_call span emitted from the
// LLMFallback closure. Instrument name intentionally distinct from
// pkg/mcp's "core-agent/mcp" and pkg/digest's "core-agent/digest" so
// operators can filter by producer in trace UIs.
var mcpDigestTracer = otel.Tracer("core-agent/mcp-digest-llm")

// mcpDigestSubagentSystemPrompt is the fixed instruction handed to the
// digesting subtask. Deliberately generic — MCP tools cover an
// unbounded surface (GKE, GitHub, Linear, filesystem, ...) and the
// wrapper doesn't know which. Preserve identifier-shaped payload
// content (IDs, names, statuses, timestamps, error text) and shed the
// verbose descriptions the structural pruner couldn't collapse.
//
// Mirrors the built-in agentic wrappers' pattern of one static prompt
// per tool class rather than operator-tunable per MCP tool — the
// design doc's Non-goals list explicitly resists per-tool prompt
// customization as configuration explosion.
const mcpDigestSubagentSystemPrompt = "You are a digesting subtask. Summarize the following MCP tool response, preserving identifying values (names, IDs, URLs, statuses, counts, error messages, timestamps, and any field name that looks like a primary key). Discard verbose descriptions, redundant metadata, and visual formatting. Stay under 500 tokens."

// buildMCPDigestLLMFallback constructs the closure that pkg/mcp
// invokes on responses the structural pruner can't reduce below
// threshold. Returns nil when the operator hasn't opted in — callers
// nil-check before setting DigestOptions.LLMFallback so the wrap
// stays on the shipped structural-only default.
//
// Late binding via **agent.Agent: mcp.Build runs before agent.New
// (the toolsets need to be constructed to pass into the agent's
// options), so agentRef is populated by the WithPostConstruct hook
// after mcp.Build has already recorded the closure into
// DigestOptions.LLMFallback. The model can't invoke an MCP tool
// through the wrap before its first turn anyway, so the pointer is
// non-nil by the time the closure fires in practice.
//
// modelID is resolved up-front so it appears in the startup log; the
// closure caches the adkmodel.LLM on first invocation and reuses it.
// Empty modelID → subagent inherits the parent's model (functionally
// correct, no cost win). SubagentModel on the returned result mirrors
// modelID verbatim so display-side pricing lookup uses the resolved
// tier, not the parent.
func buildMCPDigestLLMFallback(
	agentRef **agent.Agent,
	provider models.Provider,
	modelID string,
) func(ctx context.Context, raw []byte) (mcp.LLMFallbackResult, error) {
	var (
		mu       sync.Mutex
		resolved adkmodel.LLM
		done     bool
	)
	return func(ctx context.Context, raw []byte) (mcp.LLMFallbackResult, error) {
		a := *agentRef
		if a == nil {
			return mcp.LLMFallbackResult{}, fmt.Errorf("mcp digest LLM: agent not yet bound (registration race?)")
		}

		mu.Lock()
		if !done {
			if modelID != "" && provider != nil {
				m, err := provider.Model(ctx, modelID)
				if err != nil {
					mu.Unlock()
					return mcp.LLMFallbackResult{}, fmt.Errorf("mcp digest LLM: resolve model %q: %w", modelID, err)
				}
				resolved = m
			}
			done = true
		}
		m := resolved
		mu.Unlock()

		// subagent.llm_call span. Client kind because the subagent's
		// underlying model call crosses a provider boundary (Vertex /
		// Gemini / Anthropic). Attributes carry both core_agent.* (for
		// dashboards that filter by our namespace) and gen_ai.*
		// semantic-conventions (so LLM-aware trace UIs — Honeycomb LLM
		// view, GCP Vertex AI Trace — pick up the model + token
		// numbers automatically).
		spanCtx, span := mcpDigestTracer.Start(ctx, "subagent.llm_call", trace.WithSpanKind(trace.SpanKindClient))
		defer span.End()
		if modelID != "" {
			span.SetAttributes(
				attribute.String("core_agent.subagent.model", modelID),
				attribute.String("gen_ai.request.model", modelID),
			)
		}
		if provider != nil {
			span.SetAttributes(attribute.String("gen_ai.system", provider.Name()))
		}

		res, err := a.RunSubtask(spanCtx, agent.SubtaskSpec{
			Name:         "mcp_digest",
			SystemPrompt: mcpDigestSubagentSystemPrompt,
			UserMessage:  string(raw),
			Model:        m,
			Budgets:      agent.SubtaskBudgets{MaxTurns: 2},
		})
		if err != nil {
			return mcp.LLMFallbackResult{}, err
		}
		span.SetAttributes(
			attribute.Int("core_agent.subagent.input_tokens", res.InputTokens),
			attribute.Int("core_agent.subagent.output_tokens", res.OutputTokens),
			attribute.Int("core_agent.subagent.total_tokens", res.InputTokens+res.OutputTokens),
			attribute.Float64("core_agent.subagent.cost_usd", res.CostUSD),
			attribute.Int("gen_ai.usage.input_tokens", res.InputTokens),
			attribute.Int("gen_ai.usage.output_tokens", res.OutputTokens),
		)
		return mcp.LLMFallbackResult{
			Text:                 res.Digest,
			SubagentModel:        modelID,
			SubagentInputTokens:  res.InputTokens,
			SubagentOutputTokens: res.OutputTokens,
		}, nil
	}
}
