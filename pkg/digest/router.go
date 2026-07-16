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

package digest

import (
	"encoding/json"
)

// route decides which method Process should dispatch to. Intentionally
// shallow — adding routes (Go AST, schema-tagged JSON, etc.) is a
// later patch, not a v1 design decision.
//
// Rules (in order):
//
//  1. Payload smaller than threshold → passthrough (verbatim).
//     Guards against router+pruner overhead on tiny responses.
//  2. Looks like JSON (first non-whitespace byte is `{` or `[`, and
//     json.Valid returns true) → structural_json.
//  3. Otherwise, if hasLLMFallback → llm_fallback.
//  4. Otherwise → passthrough (bounded by MaxPassthroughBytes at the
//     caller). Callers who care about prose compression must wire
//     LLMFallback; the router doesn't fabricate one.
//
// The threshold check runs FIRST — a small JSON blob under threshold
// still passes through unmodified. That matches operator intuition
// ("if it's tiny, don't touch it") over "always structurally prune."
func route(payload []byte, threshold int, hasLLMFallback bool) string {
	if len(payload) < threshold {
		return MethodPassthrough
	}
	if looksLikeJSON(payload) {
		return MethodStructuralJSON
	}
	if hasLLMFallback {
		return MethodLLMFallback
	}
	return MethodPassthrough
}

// looksLikeJSON is the cheap sniff: skip leading whitespace, check
// that the first meaningful byte opens a JSON container (`{` or `[`),
// then confirm with json.Valid so we don't misroute a text file that
// happens to start with `{`.
//
// The two-stage check (cheap byte peek → full validation) means we
// pay the json.Valid cost only for payloads that plausibly look like
// JSON, not every prose blob.
func looksLikeJSON(payload []byte) bool {
	for _, b := range payload {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{', '[':
			return json.Valid(payload)
		default:
			return false
		}
	}
	return false
}
