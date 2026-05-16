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

package usage

import (
	"strings"

	"github.com/go-steer/core-agent/config"
)

// Pricing is the per-million-token rate for one model. Both fields are
// in USD and apply to a single direction (input or output).
type Pricing struct {
	InputPerMTok  float64 // USD per 1,000,000 input tokens
	OutputPerMTok float64 // USD per 1,000,000 output tokens
}

// IsZero reports whether neither rate is set (we don't know how to
// price this model).
func (p Pricing) IsZero() bool { return p.InputPerMTok == 0 && p.OutputPerMTok == 0 }

// builtinPricing holds default Gemini 3.x preview rates. Numbers are
// placeholders modeled on Gemini 2.5 list prices; refine when Google
// publishes 3.x rates. Anthropic and other providers are intentionally
// omitted — consumers override via cfg.Model.Pricing.
//
// Keys are matched case-insensitively. A prefix match (modelID starts
// with key) is also accepted so date-suffixed variants like
// "gemini-3.1-pro-preview-05-15" still get reasonable rates.
var builtinPricing = map[string]Pricing{
	"gemini-3.1-pro-preview":         {InputPerMTok: 1.25, OutputPerMTok: 5.00},
	"gemini-3.1-pro":                 {InputPerMTok: 1.25, OutputPerMTok: 5.00},
	"gemini-3-pro-preview":           {InputPerMTok: 1.25, OutputPerMTok: 5.00},
	"gemini-3-pro":                   {InputPerMTok: 1.25, OutputPerMTok: 5.00},
	"gemini-3-flash-preview":         {InputPerMTok: 0.075, OutputPerMTok: 0.30},
	"gemini-3-flash":                 {InputPerMTok: 0.075, OutputPerMTok: 0.30},
	"gemini-3.1-flash-lite-preview":  {InputPerMTok: 0.04, OutputPerMTok: 0.15},
	"gemini-3.1-flash-lite":          {InputPerMTok: 0.04, OutputPerMTok: 0.15},
	"gemini-3.1-flash-image-preview": {InputPerMTok: 0.10, OutputPerMTok: 0.40},
}

// PriceFor returns the Pricing for modelID. Resolution order:
//  1. Explicit cfg override (cfg.Model.Pricing) when modelID matches
//     cfg.Model.Name (case-insensitively).
//  2. Exact match in the built-in table.
//  3. Longest-prefix match in the built-in table.
//  4. Zero pricing — caller should treat cost as unknown.
func PriceFor(modelID string, cfg *config.Config) Pricing {
	low := strings.ToLower(strings.TrimSpace(modelID))
	if cfg != nil && cfg.Model.Pricing != nil &&
		strings.EqualFold(cfg.Model.Name, modelID) &&
		(cfg.Model.Pricing.InputPerMTok > 0 || cfg.Model.Pricing.OutputPerMTok > 0) {
		return Pricing{
			InputPerMTok:  cfg.Model.Pricing.InputPerMTok,
			OutputPerMTok: cfg.Model.Pricing.OutputPerMTok,
		}
	}
	if p, ok := builtinPricing[low]; ok {
		return p
	}
	var best string
	for k := range builtinPricing {
		if strings.HasPrefix(low, k) && len(k) > len(best) {
			best = k
		}
	}
	if best != "" {
		return builtinPricing[best]
	}
	return Pricing{}
}

// CostUSD returns the dollar cost of (input, output) tokens at p.
func (p Pricing) CostUSD(inputTokens, outputTokens int) float64 {
	const million = 1_000_000.0
	return (float64(inputTokens)/million)*p.InputPerMTok +
		(float64(outputTokens)/million)*p.OutputPerMTok
}
