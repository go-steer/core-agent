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
	"fmt"
	"iter"
	"os"

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

// ContextCacheInitFn is called on the first GenerateContent request
// with the fully-assembled system instruction + tools ADK is about
// to send. Implementations typically snapshot these into a Vertex
// explicit-cache Create call (async — must not block the request).
type ContextCacheInitFn func(ctx context.Context, systemInstruction *genai.Content, tools []*genai.Tool)

// ContextCacheNameFn returns the currently-resolved cache name to
// stamp onto GenerateContentConfig.CachedContent, or "" if no cache
// is available yet (async Init still in flight, failed, or explicitly
// disabled). Empty return = request runs uncached, which is always
// safe — the caller degrades gracefully.
type ContextCacheNameFn func(ctx context.Context) string

// WithContextCache wires Vertex explicit-cache hooks into every
// GenerateContent call this Provider issues. Only meaningful on the
// Vertex backend — the direct Gemini API rejects the cache-reference
// parameter on some model families and the wrap silently no-ops on
// GeminiAPI even when set (the caller is expected to gate this on
// backend). Passing nil for either hook disables caching.
//
// The hooks compose with builtins (google_search / url_context /
// code_execution) — nothing special about their interaction.
func WithContextCache(init ContextCacheInitFn, name ContextCacheNameFn) Option {
	return func(p *Provider) {
		p.cacheInit = init
		p.cacheName = name
	}
}

// SetContextCache installs Vertex explicit-cache hooks on an
// already-constructed Provider. Same effect as WithContextCache
// but usable when the Provider comes from a registry (models.Resolve)
// that doesn't thread arbitrary options through — the daemon's
// wiring in cmd/core-agent constructs the vertexcache.Manager
// AFTER Resolve() returns because Manager needs cfg.Model.Name +
// a *genai.Caches client bound to the same ClientConfig the
// Provider already owns.
//
// Not safe to call concurrently with a Model() invocation on the
// same Provider — treat as construction-time-only, invoked before
// the first Model() call.
func (p *Provider) SetContextCache(init ContextCacheInitFn, name ContextCacheNameFn) {
	p.cacheInit = init
	p.cacheName = name
}

// ClientConfig returns a copy of the Provider's underlying genai
// client config so callers (chiefly cmd/core-agent) can construct
// a sibling *genai.Client for the vertexcache.Manager without
// duplicating auth/backend/project detection. Returns nil if the
// Provider was constructed without one (shouldn't happen via the
// public constructors, but defensive against tests).
func (p *Provider) ClientConfig() *genai.ClientConfig {
	if p.cfg == nil {
		return nil
	}
	cfg := *p.cfg
	return &cfg
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

	// cacheInit + cacheName wire Vertex explicit context caching.
	// Both nil = no caching (behavior identical to pre-#221).
	//
	// cacheInit runs on every call — the manager it points at is
	// at-most-once internally (see internal/vertexcache.Manager.Init),
	// so repeated fires are cheap. Kept here (not sync.Once-guarded)
	// so builtinsLLM stays stateless. cacheName runs on every call
	// and stamps the resolved cache handle (or "") onto the request
	// config.
	cacheInit ContextCacheInitFn
	cacheName ContextCacheNameFn
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
	// Context-cache Init: fire on every call — the underlying manager
	// is at-most-once so repeated calls after the first are cheap
	// state checks. Capturing req.Config.SystemInstruction + Tools
	// here is deliberate: this is the moment ADK has finished
	// composing the request, so what we cache is byte-for-byte what
	// subsequent requests would send. Turn 1 misses (async Init still
	// in flight); turn 2+ benefits. See docs/vertex-context-caching-design.md.
	if l.cacheInit != nil && req.Config != nil {
		l.cacheInit(ctx, req.Config.SystemInstruction, req.Config.Tools)
	}
	// Context-cache reference: read the resolved cache name (or "")
	// and stamp it. Must happen BEFORE the builtins-append block
	// below — Vertex requires the cache reference to match the
	// content originally cached, so we don't want the per-call
	// builtins injection to muddy the cached-vs-request comparison.
	if l.cacheName != nil {
		if name := l.cacheName(ctx); name != "" {
			if req.Config == nil {
				req.Config = &genai.GenerateContentConfig{}
			}
			req.Config.CachedContent = name
		}
	}
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

	// Three composed wrappers, innermost first:
	//   1. inner.GenerateContent — the raw ADK model call
	//   2. wrapEmptyTailDetection — surfaces ErrEmptyResponse when
	//      the model returns no usable content (heartbeat-only
	//      streams, bare STOP with empty parts, etc.). #220.
	//   3. retryOnceOnEmpty — transparently retries the whole
	//      call once on ErrEmptyResponse, so transient Vertex
	//      silent-STOP behavior doesn't hang the agent loop.
	//      Emits a stderr alert on detect / recover / persist so
	//      operators see the event in the daemon log even when
	//      recovery succeeds. #78 follow-up.
	return retryOnceOnEmpty(func() iter.Seq2[*adkmodel.LLMResponse, error] {
		return wrapEmptyTailDetection(
			l.inner.GenerateContent(ctx, req, stream),
			stream, l.tolerateEmptyChunks,
		)
	})
}

// logf is the daemon-log alert hook the retry wrapper uses to
// surface silent-hang events. Package-level so tests can intercept
// (see empty_response_test.go). Defaults to the daemon's standard
// stderr line format ("core-agent: gemini: ...").
var logf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "core-agent: gemini: "+format+"\n", args...)
}

// retryOnceOnEmpty wraps a per-invocation iterator factory and
// retries the whole call once if the entire iteration produces
// ErrEmptyResponse without ever yielding usable content. Callers
// see a single continuous stream regardless of whether the retry
// fired.
//
// Buffering semantics: chunks are held internally until the first
// usable response arrives. At that point the buffer is flushed and
// the wrapper switches to pass-through for the remainder of the
// stream. If instead the iteration ends with ErrEmptyResponse and
// no usable chunk was seen, the buffer is DISCARDED (so ADK never
// records the empty session event) and the factory is invoked
// again. This keeps session state clean across retries.
//
// Cap: one retry (two attempts total). Persistent empty responses
// after retry surface as ErrEmptyResponse to the caller.
//
// Every retry decision logs to the daemon stderr so operators see
// the event even when recovery is silent from the user's view.
func retryOnceOnEmpty(fn func() iter.Seq2[*adkmodel.LLMResponse, error]) iter.Seq2[*adkmodel.LLMResponse, error] {
	const maxAttempts = 2
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		type pending struct {
			resp *adkmodel.LLMResponse
			err  error
		}
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			var buf []pending
			var flushed, gotEmptyTail bool
			for resp, err := range fn() {
				if flushed {
					if !yield(resp, err) {
						return
					}
					continue
				}
				if errors.Is(err, ErrEmptyResponse) {
					// Tail signal from wrapEmptyTailDetection —
					// don't yield it; decide retry first.
					gotEmptyTail = true
					continue
				}
				if err == nil && isUsableResponse(resp) {
					for _, b := range buf {
						if !yield(b.resp, b.err) {
							return
						}
					}
					buf = nil
					flushed = true
					if !yield(resp, err) {
						return
					}
					continue
				}
				// Not-yet-usable chunk (empty response OR a real
				// error). Buffer until we know if the stream will
				// produce usable content. Real errors that arrive
				// here bubble on flush; they don't trigger retry
				// (wrapEmptyTailDetection wouldn't have appended
				// ErrEmptyResponse if it also saw a real error).
				buf = append(buf, pending{resp, err})
			}
			if flushed {
				if attempt > 1 {
					logf("empty response recovered on retry (attempt %d/%d)",
						attempt, maxAttempts)
				}
				return
			}
			if gotEmptyTail && attempt < maxAttempts {
				logf("empty response detected — retrying (attempt %d/%d)",
					attempt+1, maxAttempts)
				continue
			}
			// Give up: flush any buffered items (may include real
			// non-empty errors from inner we must not swallow),
			// then surface the terminal ErrEmptyResponse if that
			// was the tail signal.
			for _, b := range buf {
				if !yield(b.resp, b.err) {
					return
				}
			}
			if gotEmptyTail {
				logf("empty response persisted after retry — surfacing ErrEmptyResponse to caller")
				yield(nil, ErrEmptyResponse)
			}
			return
		}
	}
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
//     silent-hang shapes #220 was filed for: an empty
//     Content{}/nil-parts turn, and — as observed in the v2.6
//     GKE-triage drive — a bare FinishReason=STOP with no
//     content. ADK forwards both as-is, agent loop has no next
//     action, session goes idle indefinitely.
//
// "Usable response" = non-nil response with either non-empty
// Content.Parts OR a non-STOP FinishReason (SAFETY, RECITATION,
// MAX_TOKENS, OTHER, ...) OR a non-empty ErrorCode. Bare STOP
// with no parts is NOT usable: the model claims to be done but
// produced nothing, which for our agentic loop is a hang.
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
// signal — parts, an operator-visible finish reason (SAFETY,
// RECITATION, MAX_TOKENS, OTHER, ...), or an error code. Empty-
// shell responses (heartbeats, aborted streams) return false.
//
// Bare FinishReason=STOP with no parts does NOT count: it's the
// exact silent-hang shape observed during the v2.6 GKE-triage
// drive (session 019f...daf0d, 2026-07-14 turn 4). Vertex
// returned a STOP frame with zero content, ADK forwarded it
// unchanged, the agent loop treated it as "turn complete, wait
// for input" and the session went idle. Classifying bare STOP
// as non-usable lets wrapEmptyTailDetection synthesize
// ErrEmptyResponse so the caller can retry / surface an error.
func isUsableResponse(resp *adkmodel.LLMResponse) bool {
	if resp == nil {
		return false
	}
	if resp.Content != nil && len(resp.Content.Parts) > 0 {
		return true
	}
	if resp.ErrorCode != "" || resp.ErrorMessage != "" {
		return true
	}
	if resp.FinishReason != "" && resp.FinishReason != genai.FinishReasonStop {
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
