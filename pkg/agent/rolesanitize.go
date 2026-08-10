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

// roleSanitizingLLM guards every model request against role-less /
// invalid-role Content reaching a provider that rejects it (#614).
//
// genai accepts only two content roles — "user" and "model". The
// codebase deliberately writes role-LESS annotation events (grounding/
// search audit rows in pkg/models/gemini/projection.go, autonomous
// "note" events in pkg/agent/autonomous/persist.go) as audit + display
// state, NOT conversation context. On the main turn ADK's content
// processor skips them when it assembles the request, but the internal
// history builders (summarizerHistory for Checkpoint/Compact,
// sessionHistory for AskSideQuestion) bypass ADK and pass event Content
// through verbatim — so a role-less row with text parts reaches Vertex
// Gemini and triggers "400 INVALID_ARGUMENT — Please use a valid role:
// user, model." (The Anthropic provider is immune because its converter
// coerces empty role → user and drops unknown roles.)
//
// agent.New hands one *model* value to BOTH llmagent.New (the main-turn
// runner) and the Agent struct (a.model, used by the summarizer/side-
// question paths), so wrapping it once here is the single sanitizer that
// covers every request path — exactly what #614 asks for. It sits
// OUTERMOST of any provider wrapper (gemini's builtinsLLM, the cache-
// eviction retry), so it filters once before provider-specific mutation.
//
// We DROP invalid-role content rather than coerce it to "user": these
// rows are audit/display by design (matching ADK's own main-turn skip),
// and coercing would inject audit noise into the model's context.
// Content with a valid role is left untouched, so function-call/response
// pairing — which always carries a real role — is preserved.

package agent

import (
	"context"
	"iter"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// roleSanitizingLLM decorates an adkmodel.LLM, stripping Content whose
// role Gemini would reject from each request before delegating.
type roleSanitizingLLM struct {
	inner adkmodel.LLM
}

// newRoleSanitizingLLM wraps inner so every GenerateContent request has
// its Contents filtered to valid genai roles. Returns inner unchanged
// when it is nil (agent.New rejects a nil model before this is called;
// the guard keeps the wrapper safe for direct/test use).
func newRoleSanitizingLLM(inner adkmodel.LLM) adkmodel.LLM {
	if inner == nil {
		return nil
	}
	return &roleSanitizingLLM{inner: inner}
}

// Name delegates to the wrapped model — the sanitizer is transparent to
// model identity (instruction assembly, telemetry, cost attribution).
func (l *roleSanitizingLLM) Name() string { return l.inner.Name() }

// WithoutBuiltins forwards the duck-typed unwrap RunSubtask uses to strip
// a provider's auto-injected tools (gemini's builtinsLLM). The wrapper
// must stay transparent to that assertion — otherwise a subtask
// inheriting the parent's (now-wrapped) model would keep the built-ins.
// The stripped model is re-wrapped so subtasks stay role-sanitized too.
// When the inner model has nothing to strip, the wrapper is returned
// unchanged (still sanitized).
func (l *roleSanitizingLLM) WithoutBuiltins() adkmodel.LLM {
	u, ok := l.inner.(interface {
		WithoutBuiltins() adkmodel.LLM
	})
	if !ok {
		return l
	}
	return newRoleSanitizingLLM(u.WithoutBuiltins())
}

// GenerateContent filters invalid-role Content out of req.Contents, then
// delegates. When nothing needs dropping the original req is passed
// through untouched (the common case — a well-formed main-turn request).
// Otherwise a shallow copy carries the filtered slice so the caller's
// req and its Contents backing array are never mutated.
func (l *roleSanitizingLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	if req != nil {
		if cleaned, changed := sanitizeContentRoles(req.Contents); changed {
			cp := *req
			cp.Contents = cleaned
			req = &cp
		}
	}
	return l.inner.GenerateContent(ctx, req, stream)
}

// validGenaiRole reports whether r is a content role Gemini accepts.
func validGenaiRole(r string) bool {
	return r == genai.RoleUser || r == genai.RoleModel
}

// sanitizeContentRoles returns contents with every Content whose role is
// not "user"/"model" removed, and reports whether anything was dropped.
// When nothing is dropped it returns the input slice unchanged (changed
// == false) so callers can skip the copy. A dropped element is never
// mutated; only the slice is rebuilt.
func sanitizeContentRoles(contents []*genai.Content) (out []*genai.Content, changed bool) {
	for i, c := range contents {
		if c != nil && validGenaiRole(c.Role) {
			continue
		}
		// First drop: copy the prefix we already validated, then append
		// only the survivors from here on.
		out = make([]*genai.Content, i, len(contents))
		copy(out, contents[:i])
		for _, c := range contents[i:] {
			if c != nil && validGenaiRole(c.Role) {
				out = append(out, c)
			}
		}
		return out, true
	}
	return contents, false
}
