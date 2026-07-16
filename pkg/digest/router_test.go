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
	"strings"
	"testing"
)

func TestRoute_ThresholdShortCircuit(t *testing.T) {
	t.Parallel()
	// Under threshold: everything passthrough, even valid JSON.
	// That's the design ("if it's tiny, don't touch it") — tiny
	// responses shouldn't pay the pruner overhead.
	if got := route([]byte(`{"a":1}`), 100, true); got != MethodPassthrough {
		t.Errorf("under-threshold JSON: got %q, want passthrough", got)
	}
	if got := route([]byte("hello world"), 100, true); got != MethodPassthrough {
		t.Errorf("under-threshold prose: got %q, want passthrough", got)
	}
}

func TestRoute_JSONShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"object", `{"a":1,"b":2}`, MethodStructuralJSON},
		{"array", `[1,2,3]`, MethodStructuralJSON},
		{"leading whitespace", "\n\n   {\"a\":1}", MethodStructuralJSON},
		{"tabs+newlines", "\t\n{\"a\":1}", MethodStructuralJSON},
		// A prose blob that opens with '{' but doesn't parse is prose.
		// Prevents misrouting Markdown / code snippets that happen to
		// start with a brace.
		{"looks like json but isn't", `{this is prose that starts with a brace`, MethodLLMFallback},
		// Scalars are not "shaped" — the pruner has nothing to do.
		{"bare number", `42`, MethodLLMFallback},
		{"bare string", `"hello"`, MethodLLMFallback},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Threshold small enough that no case short-circuits.
			// hasLLMFallback=true so prose-shaped cases route to
			// llm_fallback rather than passthrough.
			if got := route([]byte(tc.payload), 0, true); got != tc.want {
				t.Errorf("route(%q) = %q, want %q", tc.payload, got, tc.want)
			}
		})
	}
}

func TestRoute_NoLLMFallback_ProseDegradesToPassthrough(t *testing.T) {
	t.Parallel()
	// Callers who didn't wire LLMFallback shouldn't crash — they get
	// a (bounded, per digest.go's truncatePassthrough) passthrough.
	if got := route([]byte("just some prose text here"), 0, false); got != MethodPassthrough {
		t.Errorf("prose w/o LLMFallback: got %q, want passthrough", got)
	}
}

func TestLooksLikeJSON_Edges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"empty", "", false},
		{"whitespace only", "   \n\t\r  ", false},
		{"valid object", `{"k":"v"}`, true},
		{"valid array", `[1]`, true},
		{"malformed object", `{"k":`, false},
		{"prose w/ open brace mid-string", "hello {world}", false},
		{"nested valid", `{"a":{"b":[1,2]}}`, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := looksLikeJSON([]byte(tc.payload)); got != tc.want {
				t.Errorf("looksLikeJSON(%q) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}

func TestTruncationSuffix_Format(t *testing.T) {
	t.Parallel()
	// Format is stable so tests + docs can rely on it. The user-facing
	// suffix appears in passthrough truncation and in operator-visible
	// telemetry; pinning it here prevents accidental drift.
	got := truncationSuffix(1234)
	if !strings.Contains(got, "1234") || !strings.HasPrefix(got, "…<") {
		t.Errorf("truncationSuffix(1234) = %q, want something like `…<1234 more bytes>`", got)
	}
}
