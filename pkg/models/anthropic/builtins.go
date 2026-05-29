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

package anthropic

import (
	"github.com/anthropics/anthropic-sdk-go"
)

// BuiltinTools toggles Anthropic's server-side built-in tools surfaced
// by core-agent. Each enabled flag becomes one entry on the request's
// Tools slice alongside any user-defined function declarations.
//
// Defaults: everything OFF. Anthropic's server-side tools are billed
// per use on top of token cost (web_search is per-search), so we apply
// the same "active surface = opt in" rule that keeps Gemini's
// CodeExecution off by default. The library caller decides explicitly
// whether the cost and external-action posture are acceptable.
//
// To turn one on:
//
//	provider, _ := anthropic.New(key, anthropic.WithWebSearch(true))
//
// To replace the whole set:
//
//	provider, _ := anthropic.New(key, anthropic.WithBuiltinTools(anthropic.BuiltinTools{
//	    WebSearch: true,
//	}))
//
// Other Anthropic server-side tools (web_fetch, code_execution,
// text_editor, bash, memory) aren't surfaced today. Add them under
// the same struct when a concrete consumer needs one.
type BuiltinTools struct {
	WebSearch bool // Server-side web search; per-search billing on top of tokens.
}

// DefaultBuiltinTools returns the on-by-default baseline applied to
// every Provider unless overridden via WithBuiltinTools or one of the
// per-tool helpers. Currently empty — see BuiltinTools doc for why.
func DefaultBuiltinTools() BuiltinTools {
	return BuiltinTools{}
}

// asAnthropicTools projects the toggles into the SDK's ToolUnionParam
// shape. Order matches the field order in the struct so the request
// shape is deterministic across runs (matters for prompt caching).
func (b BuiltinTools) asAnthropicTools() []anthropic.ToolUnionParam {
	var out []anthropic.ToolUnionParam
	if b.WebSearch {
		out = append(out, anthropic.ToolUnionParam{
			OfWebSearchTool20260209: &anthropic.WebSearchTool20260209Param{},
		})
	}
	return out
}

// WithBuiltinTools replaces the Provider's whole BuiltinTools set.
func WithBuiltinTools(b BuiltinTools) Option {
	return func(p *Provider) { p.builtins = b }
}

// WithWebSearch toggles Anthropic's server-side web_search tool.
// Off by default — opt in when you've decided the per-search cost
// and external-call posture are acceptable.
func WithWebSearch(on bool) Option {
	return func(p *Provider) { p.builtins.WebSearch = on }
}
