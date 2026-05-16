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
// built-in tools into Config.Tools on every request. Stateless: the
// same wrapper handles concurrent calls.
type builtinsLLM struct {
	inner    adkmodel.LLM
	builtins []*genai.Tool
}

func (l *builtinsLLM) Name() string { return l.inner.Name() }

func (l *builtinsLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	if len(l.builtins) > 0 {
		if req.Config == nil {
			req.Config = &genai.GenerateContentConfig{}
		}
		// Append, don't replace — preserves any function declarations
		// the agent's tool registry already contributed.
		req.Config.Tools = append(req.Config.Tools, l.builtins...)

		// Gemini 3+ requires this flag whenever server-side built-ins
		// (google_search / url_context / code_execution) coexist with
		// client-side function calling in the same request. Without
		// it the API rejects with "Please enable
		// tool_config.include_server_side_tool_invocations to use
		// Built-in tools with Function calling." We set it
		// unconditionally because (a) we're injecting built-ins, so
		// the consumer asked for them, and (b) it's a no-op when
		// there are no function tools to combine with.
		//
		// Gemini 2.5 and older reject the combination outright with
		// a different error; core-agent requires Gemini 3.0+ when
		// using built-in tools alongside the agent's tool registry.
		if req.Config.ToolConfig == nil {
			req.Config.ToolConfig = &genai.ToolConfig{}
		}
		t := true
		req.Config.ToolConfig.IncludeServerSideToolInvocations = &t
	}
	return l.inner.GenerateContent(ctx, req, stream)
}
