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

package pricing

// builtin holds the compiled-in pricing table — the zero-config
// fallback that ships in the binary. Stays small; major new models
// land here on releases. PR B's LiteLLM refresh reduces but does
// not eliminate the need for this layer: air-gapped pods, offline
// CI, and fresh installs all need *some* pricing without files or
// network.
//
// Keys are lowercased on insert (matches lookup precedence in
// pricing.go). Suffixed/preview variants (date-stamped, custom
// fine-tunes) resolve via the longest-prefix fallback.
//
// Numbers are USD per million tokens at upstream public list rates
// as of the doc date; revisit on each release. Anthropic / OpenAI
// entries are deliberately omitted until PR B can supply them
// authoritatively — the previous file shipped only Gemini, which
// at least signaled "rate unknown" honestly for everything else.
var builtin = map[string]Rates{
	"gemini-3.1-pro-preview":         {InputPerMTok: 1.25, OutputPerMTok: 5.00},
	"gemini-3.1-pro":                 {InputPerMTok: 1.25, OutputPerMTok: 5.00},
	"gemini-3-pro-preview":           {InputPerMTok: 1.25, OutputPerMTok: 5.00},
	"gemini-3-pro":                   {InputPerMTok: 1.25, OutputPerMTok: 5.00},
	"gemini-3.5-flash":               {InputPerMTok: 0.075, OutputPerMTok: 0.30},
	"gemini-3-flash-preview":         {InputPerMTok: 0.075, OutputPerMTok: 0.30},
	"gemini-3-flash":                 {InputPerMTok: 0.075, OutputPerMTok: 0.30},
	"gemini-3.1-flash-lite-preview":  {InputPerMTok: 0.04, OutputPerMTok: 0.15},
	"gemini-3.1-flash-lite":          {InputPerMTok: 0.04, OutputPerMTok: 0.15},
	"gemini-3.1-flash-image-preview": {InputPerMTok: 0.10, OutputPerMTok: 0.40},
}

// Builtin returns a defensive copy of the compiled-in table. Used
// by tests + by tools that want to inspect what shipped (e.g. a
// future `/pricing list builtin` view).
func Builtin() map[string]Rates {
	out := make(map[string]Rates, len(builtin))
	for k, v := range builtin {
		out[k] = v
	}
	return out
}
