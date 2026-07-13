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

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// BuiltinTools toggles Gemini's server-side built-in tools surfaced
// by core-agent. Each enabled flag becomes its own *genai.Tool entry
// injected into the request's Config.Tools alongside any user-defined
// function declarations.
//
// Defaults:
//   - GoogleSearch + URLContext are on (universally useful, no setup)
//   - CodeExecution is off (useful but a real action surface — opt in
//     when you've decided sandboxed Python on Google's servers is
//     acceptable for your security and cost posture)
//
// To turn one off:
//
//	provider, _ := gemini.NewAPIKey(key, gemini.WithURLContext(false))
//
// To turn CodeExecution on:
//
//	provider, _ := gemini.NewAPIKey(key, gemini.WithCodeExecution(true))
//
// To replace the whole set:
//
//	provider, _ := gemini.NewAPIKey(key, gemini.WithBuiltinTools(gemini.BuiltinTools{
//	    GoogleSearch: true, // URL context + CodeExecution off
//	}))
//
// Other genai built-ins (FileSearch, GoogleMaps, ComputerUse,
// EnterpriseWebSearch, GoogleSearchRetrieval, Retrieval) aren't
// surfaced here. They require upstream setup (a corpus, a Maps API
// key, a hosted environment) or are Vertex-only — flipping them on
// without configuring the upstream resource yields an API error, not
// a working tool. Add them when an actual consumer needs them.
type BuiltinTools struct {
	GoogleSearch  bool // Public web search grounding (default: on)
	URLContext    bool // Fetch + ground on URLs the model decides to visit (default: on)
	CodeExecution bool // Sandboxed Python execution on Google's servers (default: off)
}

// DefaultBuiltinTools returns the on-by-default baseline applied to
// every Provider unless overridden via WithBuiltinTools or one of the
// per-tool helpers.
func DefaultBuiltinTools() BuiltinTools {
	return BuiltinTools{
		GoogleSearch: true,
		URLContext:   true,
	}
}

// asTools projects the toggles into a slice of *genai.Tool entries.
// Order matches the field order in the struct so the request shape
// is deterministic across runs (matters for prompt caching).
func (b BuiltinTools) asTools() []*genai.Tool {
	var out []*genai.Tool
	if b.GoogleSearch {
		out = append(out, &genai.Tool{GoogleSearch: &genai.GoogleSearch{}})
	}
	if b.URLContext {
		out = append(out, &genai.Tool{URLContext: &genai.URLContext{}})
	}
	if b.CodeExecution {
		out = append(out, &genai.Tool{CodeExecution: &genai.ToolCodeExecution{}})
	}
	return out
}

// Option configures a Gemini Provider at construction time.
type Option func(*Provider)

// WithBuiltinTools replaces the Provider's whole BuiltinTools set.
func WithBuiltinTools(b BuiltinTools) Option {
	return func(p *Provider) { p.builtins = b }
}

// WithGoogleSearch toggles the Google Search built-in.
func WithGoogleSearch(on bool) Option {
	return func(p *Provider) { p.builtins.GoogleSearch = on }
}

// WithURLContext toggles the URL Context built-in.
func WithURLContext(on bool) Option {
	return func(p *Provider) { p.builtins.URLContext = on }
}

// WithCodeExecution toggles the CodeExecution built-in (sandboxed
// Python on Google's servers). Off by default.
func WithCodeExecution(on bool) Option {
	return func(p *Provider) { p.builtins.CodeExecution = on }
}

// builtinsLLM wraps an upstream model.LLM, injecting the configured
// built-in tools into Config.Tools on every request and smoothing
// over a small set of backend quirks. Stateless: the same wrapper
// handles concurrent calls.
//
// isDirectGeminiAPI controls whether we also set
// Config.ToolConfig.IncludeServerSideToolInvocations on the request.
// The direct Gemini API requires this flag when built-ins ride
// alongside function tools; Vertex AI rejects it outright. The
// wrapper learns which backend it's fronting at construction time
// in Provider.Model — see Provider.Model in gemini.go.
//
// tolerateEmptyChunks swallows the "empty response" mid-stream error
// ADK raises when an SSE chunk carries no Candidates[]. Vertex's
// streaming search-grounding path emits such heartbeat chunks
// (UsageMetadata + ResponseID only); ADK treats them as fatal, which
// poisons the stream before the grounded chunks arrive. The direct
// Gemini API doesn't exhibit this in practice, so the toggle stays
// off there to preserve real "no content" failure signaling.
type builtinsLLM struct {
	inner               adkmodel.LLM
	builtins            []*genai.Tool
	isDirectGeminiAPI   bool
	tolerateEmptyChunks bool
}

func (l *builtinsLLM) Name() string { return l.inner.Name() }

// WithoutBuiltins returns the inner adkmodel.LLM without the
// server-side built-in tool injection (GoogleSearch / URLContext /
// CodeExecution). Use this when the caller wants to drive the
// model with EXACTLY the tools they pass, no auto-injection on
// top — chiefly the agent package's RunSubtask path, where the
// subtask's tool set is the whole point and Gemini 2.5 Flash
// errors out ("Multiple tools are supported only when they are
// all search tools") if function tools coexist with the
// search-side built-ins.
//
// The "tolerate empty chunks" backend quirk (Vertex's streaming
// heartbeat workaround) is also dropped — subtasks run short
// focused requests where heartbeat tolerance isn't load-bearing.
// If a subtask use case ever needs it, route through a tiny
// wrapper that preserves only that field.
//
// Recognized via a duck-typed interface in the agent package
// (no import dep on this package); see RunSubtask for the
// type-assertion site.
func (l *builtinsLLM) WithoutBuiltins() adkmodel.LLM { return l.inner }

func (l *builtinsLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	if len(l.builtins) > 0 {
		if req.Config == nil {
			req.Config = &genai.GenerateContentConfig{}
		}
		// Append, don't replace — preserves any function declarations
		// the agent's tool registry already contributed.
		req.Config.Tools = append(req.Config.Tools, l.builtins...)

		// Gemini 3+ on the direct Gemini API requires this flag
		// whenever server-side built-ins (google_search / url_context
		// / code_execution) coexist with client-side function calling
		// in the same request. Without it the API rejects with
		// "Please enable tool_config.include_server_side_tool_invocations
		// to use Built-in tools with Function calling."
		//
		// Vertex AI for Gemini does NOT accept this parameter — it
		// rejects with "includeServerSideToolInvocations parameter
		// is not supported in Gemini Enterprise Agent Platform
		// (previously known as Vertex AI)" — but it allows the
		// combination unconditionally instead. So we set the flag
		// only when fronting the direct API.
		//
		// Gemini 2.5 and older reject the combination outright with
		// a different error regardless of this flag; core-agent
		// requires Gemini 3.0+ when using built-in tools alongside
		// the agent's tool registry.
		if l.isDirectGeminiAPI {
			if req.Config.ToolConfig == nil {
				req.Config.ToolConfig = &genai.ToolConfig{}
			}
			t := true
			req.Config.ToolConfig.IncludeServerSideToolInvocations = &t
		}
	}

	inner := l.inner.GenerateContent(ctx, req, stream)
	// Both wrappers below track whether the turn emitted anything
	// usable so we can surface a synthetic error if Vertex/Gemini
	// returned an empty response without any signaled reason
	// (#220 — silent-hang bug observed live during v2.6 GKE-
	// troubleshoot demo drive).
	return wrapEmptyTailDetection(inner, stream, l.tolerateEmptyChunks)
}

// wrapEmptyTailDetection wraps a raw model iterator with two
// invariants:
//
//  1. When tolerateEmptyChunks + stream is on, drop ADK's
//     "empty response" per-chunk errors — those are Vertex
//     heartbeat SSE frames and the caller shouldn't see them.
//  2. If the entire iteration completes without emitting a
//     SINGLE usable response AND without emitting any error,
//     synthesize ErrEmptyResponse at the tail. This catches the
//     #220 silent-hang case where the model returns
//     Content{role:model, parts:nil} with FinishReason=""
//     ErrorCode="" — ADK forwards as-is, agent loop has no
//     next action, session goes idle indefinitely.
//
// "Usable response" = non-nil response with either non-empty
// Content.Parts OR a non-empty FinishReason OR a non-empty
// ErrorCode. A finish reason (even STOP with no parts) counts
// because that's the model's explicit "I'm done" signal.
func wrapEmptyTailDetection(inner iter.Seq2[*adkmodel.LLMResponse, error], stream, tolerateEmpty bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		sawUsable := false
		sawError := false
		for resp, err := range inner {
			if err != nil {
				// Streaming Vertex heartbeats surface as ADK's
				// "empty response" error — drop them in that
				// specific configuration (same as before).
				if tolerateEmpty && stream && err.Error() == adkEmptyResponseError {
					continue
				}
				sawError = true
				if !yield(resp, err) {
					return
				}
				continue
			}
			if isUsableResponse(resp) {
				sawUsable = true
			}
			if !yield(resp, err) {
				return
			}
		}
		// Tail check: iteration finished without any usable content
		// AND without any error. This is the exact silent-hang shape
		// #220 was filed for. Surface as an error so the agent loop
		// can retry / escalate rather than going idle indefinitely.
		if !sawUsable && !sawError {
			yield(nil, ErrEmptyResponse)
		}
	}
}

// isUsableResponse reports whether an LLMResponse carries a real
// signal — parts, a finish reason, or an error code. Empty-shell
// responses (heartbeats, aborted streams) return false.
func isUsableResponse(resp *adkmodel.LLMResponse) bool {
	if resp == nil {
		return false
	}
	if resp.Content != nil && len(resp.Content.Parts) > 0 {
		return true
	}
	if resp.FinishReason != "" {
		return true
	}
	if resp.ErrorCode != "" || resp.ErrorMessage != "" {
		return true
	}
	return false
}

// ErrEmptyResponse is surfaced by the Gemini adapter when the
// model returns no usable content AND no explicit finish reason
// AND no error — the "silent hang" pattern #220 documents. The
// error text names the likely upstream causes so operators
// reading the daemon log get an actionable next step rather than
// a mystery.
//
// Callers (typically the agent loop) may treat this as retryable —
// empty responses from Vertex are usually transient (safety filter
// race, streaming truncation, provisional-throughput mismatch). A
// second attempt often succeeds; a persistent pattern signals a
// deeper Vertex-side issue worth escalating.
var ErrEmptyResponse = errors.New("gemini: model returned no usable content with no finish reason and no error — likely a silent safety filter, streaming truncation, or transient Vertex fault; retrying often succeeds")

// adkEmptyResponseError is the literal error text ADK's streaming
// aggregator (google.golang.org/adk/internal/llminternal) and
// non-streaming gemini model raise when a response carries no
// Candidates[]. We string-match because the error isn't exported.
const adkEmptyResponseError = "empty response"
