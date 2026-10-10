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

// Package gemini implements models.Provider for the Gemini family,
// covering both the public Gemini API (API-key auth) and Vertex AI
// (Application Default Credentials + GCP project).
//
// The two are exposed as distinct provider names ("gemini" and "vertex")
// so users and automation can pin to a backend explicitly. Both delegate
// to google.golang.org/adk/model/gemini under the hood.
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
// model to ADK v1 through models.Adapt.
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
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	cmgemini "github.com/go-steer/core-models/dialect/gemini"
	"github.com/go-steer/core-models/llm"
	"github.com/go-steer/core-models/retry"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"
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
	baseURL  string // tests only: the library's endpoint override

	// cacheModel, cacheInit, cacheName and cacheInvalidate wire Vertex
	// explicit context caching into every model this Provider builds;
	// see WithContextCache. The library applies them on Vertex only —
	// the direct Gemini API rejects the cache-reference parameter on
	// some model families — and only to requests for cacheModel.
	cacheModel      string
	cacheInit       ContextCacheInitFn
	cacheName       ContextCacheNameFn
	cacheInvalidate ContextCacheInvalidateFn

	// client is the core-models client, built on the first Model()
	// call so hooks installed after construction (SetContextCache)
	// are in it. Every model shares its connection pool.
	mu     sync.Mutex
	client *cmgemini.Client
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

// options is the library configuration for this Provider. withHooks
// leaves the context-cache hooks out, for a client that only builds a
// cache manager.
func (p *Provider) options(withHooks bool) cmgemini.Options {
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
		HTTPClient: &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)},
		// One retry layer, not two: transientRetry (pkg/models.RetryPolicy)
		// keeps 429/503 and the bare-400-after-success retry, its
		// process-wide budget, and the RetryError a transcript shows
		// (#1206). The library's HTTP-layer retry is turned off, the
		// bare-400 rule included (a nil AfterSuccess would get it back).
		Retry: &retry.Policy{AfterSuccess: func(int, []byte) bool { return false }},
	}
	if withHooks {
		o.ContextCacheModel = p.cacheModel
		o.ContextCacheInit = p.cacheInit
		o.ContextCacheName = p.cacheName
		o.ContextCacheInvalidate = p.cacheInvalidate
	}
	return o
}

func (p *Provider) libClient(ctx context.Context) (*cmgemini.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	c, err := cmgemini.New(ctx, p.options(true))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.prefix, err)
	}
	p.client = c
	return c, nil
}

// Caches returns the genai caches service on this Provider's endpoint
// and credentials, for building a vertexcache.Manager (see
// pkg/compose.MaybeWireContextCache) without a second copy of the
// backend, project and auth detection.
func (p *Provider) Caches(ctx context.Context) (*genai.Caches, error) {
	c, err := cmgemini.New(ctx, p.options(false))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.prefix, err)
	}
	return c.Caches(), nil
}

// Model constructs a model.LLM for the given model ID.
func (p *Provider) Model(ctx context.Context, modelID string) (adkmodel.LLM, error) {
	if modelID == "" {
		return nil, fmt.Errorf("%s: model id is required", p.prefix)
	}
	c, err := p.libClient(ctx)
	if err != nil {
		return nil, err
	}
	return newRetrying(models.Adapt(c.Model(modelID))), nil
}

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
