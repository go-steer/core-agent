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

// Package gemini implements models.Provider for Gemini, on the Gemini
// Developer API ("gemini") and on Vertex AI ("vertex").
//
// The adapter lives in core-models (dialect/gemini,
// docs/model-support-design.md): the genai-native model, the stream
// aggregation, built-in injection and its pre-3.0 skip, the
// IncludeServerSideToolInvocations flag set from the backend, the
// empty-response retry and the Vertex context-cache stamping with its
// eviction recovery. This package is core-agent's facade over it: it
// maps config.Config onto the library's options, keeps core-agent's
// defaults (built-ins web search and URL context on) and exported API,
// keeps core-agent's own transient retry and its process-wide budget
// (pkg/models.RetryPolicy) as the one retry layer, and adapts each
// model to ADK v2 through models.Adapt.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"os"
	"sync"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-models/auth"
	cmgemini "github.com/go-steer/core-models/dialect/gemini"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/retry"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/models"
)

func init() {
	models.Register(config.ProviderGemini, newGeminiAPI)
	models.Register(config.ProviderVertex, newVertexAI)
}

// Provider is the Gemini-family implementation of models.Provider.
type Provider struct {
	name     string
	prefix   string
	backend  genai.Backend
	apiKey   string
	project  string
	location string
	builtins BuiltinTools
	baseURL  string           // tests only: the library's endpoint override
	cred     *auth.Credential // tests only: Vertex credential in place of ADC

	// cacheModel, cacheInit, cacheName and cacheInvalidate wire Vertex
	// explicit context caching into every model this Provider builds;
	// see WithContextCache. The library applies them on Vertex only —
	// the direct Gemini API rejects the cache-reference parameter on
	// some model families — and only to requests for cacheModel.
	cacheModel      string
	cacheInit       ContextCacheInitFn
	cacheName       ContextCacheNameFn
	cacheInvalidate ContextCacheInvalidateFn

	// client is the one core-models client every model shares, built on
	// first use. Installing hooks (SetContextCache,
	// SetContextCacheInvalidate) sets them on this client, and the
	// library's models read them per call, so a model returned before
	// the hooks were installed still gets them. mu guards the hooks and
	// client.
	mu     sync.Mutex
	client *cmgemini.Client
	// models caches the library model per id for the current client.
	// core-models hands back a fresh model from every Client.Model call,
	// and its once-per-model state (the "built-ins skipped for this
	// model" notice) would otherwise reset on every turn. The library
	// model is immutable apart from that sync.Once, so concurrent calls
	// share it. Dropped with the client.
	models map[string]llm.LLM
}

// Name reports the provider identity (e.g. "gemini" or "vertex").
func (p *Provider) Name() string { return p.name }

// DefaultSmallModelID is the Gemini cheap-tier model used by default
// for agentic subtasks when the operator hasn't pinned one with
// --agentic-small-model. Moves in lockstep with taskclass's gemini
// small tier (TestModelForTier_ConsistentWithSmallModelDefaulters).
const DefaultSmallModelID = "gemini-3.5-flash-lite"

// DefaultSmallModel satisfies models.SmallModelDefaulter so core-agent
// can route subtask digesting to a cheap-tier Gemini model without
// requiring the operator to set --agentic-small-model.
func (p *Provider) DefaultSmallModel() string { return DefaultSmallModelID }

// options is the library configuration for this Provider. Call with
// p.mu held, or before the Provider is shared.
func (p *Provider) options() cmgemini.Options {
	o := cmgemini.Options{
		Backend:      p.backend,
		APIKey:       p.apiKey,
		Project:      p.project,
		Location:     p.location,
		BaseURL:      p.baseURL,
		BackendName:  p.name,
		BuiltinTools: p.builtins.library(),
		Logf:         func(format string, args ...any) { logf(format, args...) },
		// Outbound calls get an HTTP client span and traceparent on both
		// backends (#325). The library authenticates Vertex with its own
		// ADC bearer transport over this one, so genai's own
		// otelhttp-wrapped ADC client is not in the path to double-count.
		HTTPClient:             &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)},
		Retry:                  geminiRetry(),
		Credential:             p.cred,
		ContextCacheModel:      p.cacheModel,
		ContextCacheInit:       p.cacheInit,
		ContextCacheName:       p.cacheName,
		ContextCacheInvalidate: p.cacheInvalidate,
	}
	return o
}

// geminiRetry is the library's HTTP-layer policy for Gemini: one retry
// layer, not two. transientRetry (pkg/models.RetryPolicy) keeps 429/503
// and the bare-400-after-success retry, its process-wide budget, and the
// RetryError a transcript shows (#1206), so the library re-sends
// nothing — the bare-400 rule included, which a nil AfterSuccess would
// bring back. What it keeps from retry.Default is the header timeout:
// an attempt that gets no response headers in five minutes fails rather
// than hanging the turn, and transientRetry decides what happens next.
func geminiRetry() *retry.Policy {
	pol := retry.Default()
	pol.MaxRetries = 0
	pol.AfterSuccess = func(int, []byte) bool { return false }
	return &pol
}

// setHooks applies a hook change to the client already built, which
// every model reads per call, so nothing is rebuilt: Caches() and the
// running models stay on the one client. A combination the library
// rejects (hooks with no model to scope them to) drops the client
// instead, so the next call's rebuild reports it, as before #31.
func (p *Provider) setHooks(apply func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	apply()
	if p.client == nil {
		return
	}
	if err := p.client.SetContextCache(p.contextCache()); err != nil {
		p.client = nil
		p.models = nil
	}
}

// contextCache is the hooks in force. Call with p.mu held.
func (p *Provider) contextCache() cmgemini.ContextCache {
	return cmgemini.ContextCache{
		Model:      p.cacheModel,
		Init:       p.cacheInit,
		Name:       p.cacheName,
		Invalidate: p.cacheInvalidate,
	}
}

// libModel returns the library model for id on the current client,
// built once per client.
func (p *Provider) libModel(ctx context.Context, id string) (llm.LLM, error) {
	c, err := p.libClient(ctx)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != c {
		// Hooks replaced the client between the two locks. Serve this
		// call from the client it got; the next call caches on the new one.
		return c.Model(id), nil
	}
	if lm, ok := p.models[id]; ok {
		return lm, nil
	}
	if p.models == nil {
		p.models = map[string]llm.LLM{}
	}
	lm := c.Model(id)
	p.models[id] = lm
	return lm, nil
}

func (p *Provider) libClient(ctx context.Context) (*cmgemini.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	o := p.options()
	if o.Backend == genai.BackendVertexAI && o.APIKey == "" && o.Credential == nil {
		cred, err := models.InjectedGoogleCredential(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.prefix, err)
		}
		o.Credential = cred
	}
	c, err := cmgemini.New(ctx, o)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.prefix, err)
	}
	p.client = c
	return c, nil
}

// Caches returns the genai caches service on this Provider's endpoint
// and credentials, for building a vertexcache.Manager (see
// pkg/compose.MaybeWireContextCache) without a second copy of the
// backend, project and auth detection. It is the shared client's, and
// installing hooks afterwards keeps that client.
func (p *Provider) Caches(ctx context.Context) (*genai.Caches, error) {
	c, err := p.libClient(ctx)
	if err != nil {
		return nil, err
	}
	return c.Caches(), nil
}

// Model constructs a model.LLM for the given model ID. The client is
// built here, so a missing credential fails at startup rather than on
// the first turn.
func (p *Provider) Model(ctx context.Context, modelID string) (adkmodel.LLM, error) {
	if modelID == "" {
		return nil, fmt.Errorf("%s: model id is required", p.prefix)
	}
	if _, err := p.libClient(ctx); err != nil {
		return nil, err
	}
	return newRetrying(models.Adapt(lateModel{p: p, id: modelID})), nil
}

// lateModel is one model id on whatever client the Provider holds at
// call time, so hooks installed after Model() still apply.
type lateModel struct {
	p    *Provider
	id   string
	bare bool // without server-side built-ins
}

func (m lateModel) Name() string { return m.id }

func (m lateModel) GenerateContent(ctx context.Context, req *llm.Request, stream bool) iter.Seq2[*llm.Response, error] {
	return func(yield func(*llm.Response, error) bool) {
		lm, err := m.p.libModel(ctx, m.id)
		if err != nil {
			yield(nil, err)
			return
		}
		if wb, ok := lm.(interface{ WithoutBuiltins() llm.LLM }); ok && m.bare {
			lm = wb.WithoutBuiltins()
		}
		for resp, err := range lm.GenerateContent(ctx, req, stream) {
			if !yield(resp, err) {
				return
			}
		}
	}
}

// WithoutBuiltins is the same model without server-side built-ins.
func (m lateModel) WithoutBuiltins() llm.LLM { return lateModel{p: m.p, id: m.id, bare: true} }

// transientRetry is the one retry layer for Gemini: 429 and 503, and
// Vertex's bare 400 once the session has been served (#898, #1247),
// against pkg/models' process-wide budget. The predicates are the
// library's, so its error classification and this policy agree.
var transientRetry = &models.RetryPolicy{
	IsTransient:             cmgemini.IsTransient,
	IsTransientAfterSuccess: cmgemini.IsBareInvalidArgument,
	Log:                     func(format string, args ...any) { logf(format, args...) },
}

// IsTransient reports whether err is a Gemini rate limit or overload
// worth retrying. It is the library's classification.
func IsTransient(err error) bool { return cmgemini.IsTransient(err) }

// IsBareInvalidArgument reports whether err is Vertex's detail-less
// 400 INVALID_ARGUMENT. It is the library's classification.
func IsBareInvalidArgument(err error) bool { return cmgemini.IsBareInvalidArgument(err) }

// retrying runs every call under transientRetry, and maps the library's
// empty-response error onto ErrEmptyResponse, which callers recognize
// through models.ErrEmptyResponse.
type retrying struct {
	inner adkmodel.LLM
}

// newRetrying wraps m, keeping the WithoutBuiltins unwrap when m has it.
func newRetrying(m adkmodel.LLM) adkmodel.LLM {
	r := retrying{inner: m}
	if _, ok := m.(interface{ WithoutBuiltins() adkmodel.LLM }); ok {
		return retryingWithBuiltins{r}
	}
	return r
}

func (r retrying) Name() string { return r.inner.Name() }

func (r retrying) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return transientRetry.Wrap(ctx, func() iter.Seq2[*adkmodel.LLMResponse, error] {
		return func(yield func(*adkmodel.LLMResponse, error) bool) {
			for resp, err := range r.inner.GenerateContent(ctx, req, stream) {
				if errors.Is(err, llm.ErrEmptyResponse) {
					err = fmt.Errorf("%w [%w]", ErrEmptyResponse, err)
				}
				if !yield(resp, err) {
					return
				}
			}
		}
	})
}

type retryingWithBuiltins struct{ retrying }

// WithoutBuiltins returns the model without its server-side built-ins,
// still under the retry layer — see the duck type RunSubtask uses.
func (r retryingWithBuiltins) WithoutBuiltins() adkmodel.LLM {
	return newRetrying(r.inner.(interface{ WithoutBuiltins() adkmodel.LLM }).WithoutBuiltins())
}

// NewAPIKey returns a Provider authenticated against the public Gemini API
// using key. Empty key is rejected so the failure mode is clear at startup.
//
// Built-in tools (Google Search + URL Context) are enabled by default;
// pass WithBuiltinTools / WithGoogleSearch / WithURLContext to override.
func NewAPIKey(key string, opts ...Option) (*Provider, error) {
	if key == "" {
		return nil, fmt.Errorf("gemini: api key is required (set GOOGLE_API_KEY or GEMINI_API_KEY, or model.api_key in .agents/config.json)")
	}
	p := &Provider{
		name:     config.ProviderGemini,
		prefix:   "gemini",
		backend:  genai.BackendGeminiAPI,
		apiKey:   key,
		builtins: DefaultBuiltinTools(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// NewVertex returns a Provider authenticated against Vertex AI for the
// given GCP project and location, using Application Default Credentials.
//
// Built-in tools (Google Search + URL Context) are enabled by default;
// pass WithBuiltinTools / WithGoogleSearch / WithURLContext to override.
func NewVertex(project, location string, opts ...Option) (*Provider, error) {
	if project == "" || location == "" {
		return nil, fmt.Errorf("vertex: project and location are required (set model.vertex.{project,location} in .agents/config.json or GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION env vars)")
	}
	p := &Provider{
		name:     config.ProviderVertex,
		prefix:   "vertex",
		backend:  genai.BackendVertexAI,
		project:  project,
		location: location,
		builtins: DefaultBuiltinTools(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

func newGeminiAPI(cfg *config.Config) (models.Provider, error) {
	key := cfg.Model.APIKey
	if key == "" {
		// GOOGLE_API_KEY is the umbrella name; GEMINI_API_KEY is the
		// one Gemini's own docs and tutorials use. Accept either.
		key = firstNonEmpty(os.Getenv("GOOGLE_API_KEY"), os.Getenv("GEMINI_API_KEY"))
	}
	return NewAPIKey(key, builtinToolsFromConfig(cfg)...)
}

// builtinToolsFromConfig maps the provider-neutral model.builtin_tools
// block onto this provider's toggles. Each field is tri-state: absent
// yields no option at all, so DefaultBuiltinTools survives untouched
// and only an explicit true/false moves a tool.
//
// Neutral name → Gemini tool: web_search is google_search here (and
// Anthropic's web_search there), url_context and code_execution keep
// their names. See config.BuiltinToolsConfig for why the config surface
// doesn't spell the native names.
func builtinToolsFromConfig(cfg *config.Config) []Option {
	if cfg == nil || cfg.Model.BuiltinTools == nil {
		return nil
	}
	bt := cfg.Model.BuiltinTools
	var opts []Option
	if bt.WebSearch != nil {
		opts = append(opts, WithGoogleSearch(*bt.WebSearch))
	}
	if bt.URLContext != nil {
		opts = append(opts, WithURLContext(*bt.URLContext))
	}
	if bt.CodeExecution != nil {
		opts = append(opts, WithCodeExecution(*bt.CodeExecution))
	}
	return opts
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func newVertexAI(cfg *config.Config) (models.Provider, error) {
	project, location := "", ""
	if cfg.Model.Vertex != nil {
		project = cfg.Model.Vertex.Project
		location = cfg.Model.Vertex.Location
	}
	if project == "" {
		project = os.Getenv("GOOGLE_CLOUD_PROJECT")
	}
	if location == "" {
		location = os.Getenv("GOOGLE_CLOUD_LOCATION")
	}
	return NewVertex(project, location, builtinToolsFromConfig(cfg)...)
}
