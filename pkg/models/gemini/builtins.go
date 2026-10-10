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
	"fmt"
	"os"

	cmgemini "github.com/go-steer/core-models/dialect/gemini"

	"github.com/go-steer/core-agent/v2/pkg/models"
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

// library is the same set in core-models' vocabulary.
func (b BuiltinTools) library() cmgemini.BuiltinTools {
	return cmgemini.BuiltinTools{GoogleSearch: b.GoogleSearch, URLContext: b.URLContext, CodeExecution: b.CodeExecution}
}

// Names reports the enabled built-ins under the provider-neutral names
// the config block uses (see config.BuiltinToolsConfig), so the startup
// summary reads the same whichever provider is resolved and an operator
// can match the line against the keys they typed.
func (b BuiltinTools) Names() []string {
	var out []string
	if b.GoogleSearch {
		out = append(out, "web_search")
	}
	if b.URLContext {
		out = append(out, "url_context")
	}
	if b.CodeExecution {
		out = append(out, "code_execution")
	}
	return out
}

// BuiltinToolNames satisfies models.BuiltinToolsReporter — the effective
// server-side built-in set this Provider will inject, after config and
// options. Reported from what the Provider actually carries rather than
// re-derived from config, so the startup line cannot drift from the
// requests.
//
// Provider-level, so it is an upper bound rather than a per-request
// promise: two paths drop these tools from an individual request
// afterwards — a pre-3.0 model carrying function declarations (the
// library logs its own line when it skips) and an
// explicitly suppressed one-shot (models.BuiltinsSuppressed). Both
// subtract, never add, so a name absent here is a tool that cannot be
// reached at all, which is the direction that matters for the operator
// reading this to confirm a `builtin_tools` disable took.
func (p *Provider) BuiltinToolNames() []string { return p.builtins.Names() }

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
type ContextCacheInitFn = cmgemini.ContextCacheInitFn

// ContextCacheNameFn returns the currently-resolved cache name to
// stamp onto GenerateContentConfig.CachedContent, or "" if no cache
// is available yet (async Init still in flight, failed, or explicitly
// disabled). Empty return = request runs uncached, which is always
// safe — the caller degrades gracefully.
type ContextCacheNameFn = cmgemini.ContextCacheNameFn

// ContextCacheInvalidateFn is called when a GenerateContent response
// carries a NOT_FOUND for the stamped cache reference — the signal
// that Vertex has reaped our cache server-side (TTL elapsed while the
// daemon held a valid-looking Manager handle). Implementations flip
// their manager back to a pre-Init state so:
//
//  1. The follow-up retry (issued by GenerateContent itself) runs
//     uncached.
//  2. The next turn's Init call fires a fresh Create instead of the
//     daemon staying uncached for the rest of its lifetime.
//
// name is the cache reference the failed request carried, so a
// manager can ignore a report about a cache it has already replaced
// (vertexcache.Manager.MarkEvictedName). The reason string is opaque;
// wired into the operator-facing log line so grep-triage can
// distinguish TTL eviction from other 404 shapes as the deployment
// matures.
type ContextCacheInvalidateFn = cmgemini.ContextCacheInvalidateFn

// WithContextCache wires Vertex explicit-cache hooks into every
// GenerateContent call this Provider issues. Only meaningful on the
// Vertex backend — the direct Gemini API rejects the cache-reference
// parameter on some model families and the wrap silently no-ops on
// GeminiAPI even when set (the caller is expected to gate this on
// backend). Passing nil for either hook disables caching.
//
// The hooks compose with builtins (google_search / url_context /
// code_execution): the cache is seeded from the request AFTER the
// builtins-append, so it contains the built-ins exactly when the
// model supports injecting them (see builtinsCompatible, #535) —
// cached and uncached turns always agree on the tool set.
//
// model is the model the cache was created for (vertexcache.Manager's
// Model()): a cache serves one model, so the hooks apply only to
// requests for it.
func WithContextCache(model string, init ContextCacheInitFn, name ContextCacheNameFn) Option {
	return func(p *Provider) {
		p.cacheModel = model
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
// a *genai.Caches client on the same endpoint and credentials
// (Caches).
//
// Not safe to call concurrently with a Model() invocation on the
// same Provider — treat as construction-time-only, invoked before
// the first Model() call.
func (p *Provider) SetContextCache(model string, init ContextCacheInitFn, name ContextCacheNameFn) {
	p.cacheModel = model
	p.cacheInit = init
	p.cacheName = name
}

// SetContextCacheInvalidate installs the eviction-recovery hook —
// called when GenerateContent detects that Vertex has reaped our
// cache server-side. See ContextCacheInvalidateFn. Optional: without
// it, cache-not-found responses surface as hard turn errors instead
// of transparent retry, and the daemon needs a restart to recover.
//
// Same concurrency contract as SetContextCache — treat as
// construction-time-only.
func (p *Provider) SetContextCacheInvalidate(invalidate ContextCacheInvalidateFn) {
	p.cacheInvalidate = invalidate
}

// logf receives the adapter's operator notices (an empty response
// retried, a cache found evicted, built-ins skipped for an old model)
// and the transient-retry log. Package-level so tests can intercept. Defaults to the daemon's standard
// stderr line format ("core-agent: gemini: ...").
var logf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "core-agent: gemini: "+format+"\n", args...)
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
//
// It wraps models.ErrEmptyResponse so a caller can recognize "the
// model said nothing" without importing this adapter. One-shot callers
// need that: for /btw the empty answer IS the result, and rendering
// this sentence at an operator who asked a question would be the
// infra-error-instead-of-an-answer bug all over again.
var ErrEmptyResponse = fmt.Errorf(
	"gemini: model returned no usable content with no finish reason and no error — likely a silent safety filter, streaming truncation, or transient Vertex fault; retrying often succeeds (%w)",
	models.ErrEmptyResponse)
