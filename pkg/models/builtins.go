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

package models

import "context"

// noBuiltinsKey is the context key carrying the per-request opt-out.
type noBuiltinsKey struct{}

// WithoutBuiltins marks ctx as a request that must go to the model
// with EXACTLY the tools the caller put on it: no provider-injected
// server-side built-ins (Gemini's google_search / url_context /
// code_execution) and no context-cache reference stamped on top.
//
// The /btw side question is the motivating caller. It is documented as
// tool-less — the operator asks about what already happened, and the
// answer should come from the transcript, not from a web search the
// model decided to run. Building the request with a nil Config isn't
// enough to get that: the Gemini wrapper creates the Config itself and
// appends its built-ins, and on a Vertex cached turn it also stamps
// CachedContent onto a request that already carries the full history.
//
// This is the per-call sibling of the model-level
// builtinsLLM.WithoutBuiltins() unwrap that RunSubtask uses. The
// unwrap is the right tool when a caller drives the inner model for a
// whole subtask; the context marker is the right tool for one call on
// the SHARED model, because it keeps the wrapper's other behavior —
// notably the empty-response retry — in place. For a feature whose
// reported symptom is blank answers, dropping that retry to get
// tool-lessness would trade one bug for another.
//
// A hint, not a guarantee: providers with no built-ins to inject
// (Anthropic today) ignore it, and it can only take tools away.
func WithoutBuiltins(ctx context.Context) context.Context {
	return context.WithValue(ctx, noBuiltinsKey{}, true)
}

// BuiltinsSuppressed reports whether WithoutBuiltins was applied to
// ctx. Backends consult it per request, since one model.LLM serves
// both the agentic loop and the one-shot side calls.
func BuiltinsSuppressed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	suppressed, _ := ctx.Value(noBuiltinsKey{}).(bool)
	return suppressed
}
