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

package modeltext

import (
	"testing"

	"google.golang.org/genai"
)

// This is the predicate's own table. It proves the predicate works and nothing
// else — that the nine call sites actually use it is pinned one test per site,
// next to each site, because eight of the nine were an omission rather than a
// mistake and a shared helper cannot fail for a caller who never calls it.
func TestText(t *testing.T) {
	for _, tc := range []struct {
		name string
		part *genai.Part
		want string
		ok   bool
	}{
		{"nil part", nil, "", false},
		{"plain text", &genai.Part{Text: "the pod is crashlooping"}, "the pod is crashlooping", true},
		{"empty text", &genai.Part{}, "", false},
		{"thinking block", &genai.Part{Text: "let me check the registry", Thought: true}, "", false},
		{
			// Anthropic's redacted thinking: the payload is entirely in the
			// signature and there is no text at all. It must read as "not
			// caller-facing" and not as "an empty answer".
			"redacted thinking", &genai.Part{Thought: true, ThoughtSignature: []byte("opaque")}, "", false,
		},
		{
			// A signature on a part that is NOT flagged Thought is ordinary
			// answer text a provider happened to sign. Filtering on the
			// signature instead of the flag would drop the answer.
			"signed answer", &genai.Part{Text: "restarted it", ThoughtSignature: []byte("sig")}, "restarted it", true,
		},
		{"function call", &genai.Part{FunctionCall: &genai.FunctionCall{Name: "scale"}}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Text(tc.part)
			if got != tc.want || ok != tc.ok {
				t.Errorf("Text() = (%q, %t), want (%q, %t)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestThought is the publication predicate's table. The case that carries the
// decision is "signature-only": a thinking block whose whole payload is the
// signature must read as "nothing to publish" and never as "here is the
// signature", because that value is a provider replay credential and the one
// consumer of this function hands what it returns to a browser.
func TestThought(t *testing.T) {
	for _, tc := range []struct {
		name string
		part *genai.Part
		want string
		ok   bool
	}{
		{"nil part", nil, "", false},
		{"thinking block", &genai.Part{Text: "let me check the registry", Thought: true}, "let me check the registry", true},
		{"plain text", &genai.Part{Text: "the pod is crashlooping"}, "", false},
		{
			// Today's common case: claude-opus-5 under the request mast sends
			// returns a signed block with an empty body. An opted-in workload
			// publishes nothing for it — and above all not the signature.
			"signature-only thinking", &genai.Part{Thought: true, ThoughtSignature: []byte("opaque")}, "", false,
		},
		{
			// Signed answer text is an answer, not a thought. Keying on the
			// signature rather than the flag would publish the answer twice
			// and label half of it reasoning.
			"signed answer", &genai.Part{Text: "restarted it", ThoughtSignature: []byte("sig")}, "", false,
		},
		{"function call", &genai.Part{FunctionCall: &genai.FunctionCall{Name: "scale"}}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Thought(tc.part)
			if got != tc.want || ok != tc.ok {
				t.Errorf("Thought() = (%q, %t), want (%q, %t)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestTextAndThoughtAreDisjoint pins the property the two predicates are only
// safe as a pair if they have: no part is both an answer and a thought.
//
// It is worth its own test because the failure it guards is not a crash. If
// the two ever overlapped, a workload with agui.emit_reasoning on would
// publish the same text twice — once as the answer and once as reasoning —
// and a workload with it OFF could have reasoning reach the answer stream,
// which is the exact leak internal/modeltext exists to have already closed.
// Mutation-checked: dropping the Thought guard from Text() fails this.
func TestTextAndThoughtAreDisjoint(t *testing.T) {
	for _, p := range []*genai.Part{
		nil,
		{},
		{Text: "answer"},
		{Text: "reasoning", Thought: true},
		{Thought: true, ThoughtSignature: []byte("opaque")},
		{Text: "signed answer", ThoughtSignature: []byte("sig")},
		{Text: "signed reasoning", Thought: true, ThoughtSignature: []byte("sig")},
		{FunctionCall: &genai.FunctionCall{Name: "scale"}},
	} {
		_, answer := Text(p)
		_, thought := Thought(p)
		if answer && thought {
			t.Errorf("part %+v reads as both an answer and a thought", p)
		}
	}
}
