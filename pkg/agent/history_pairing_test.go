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

package agent

import (
	"testing"

	"google.golang.org/genai"
)

func textContent(role, text string) *genai.Content {
	return &genai.Content{Role: role, Parts: []*genai.Part{{Text: text}}}
}

func callContent(ids ...string) *genai.Content {
	c := &genai.Content{Role: genai.RoleModel}
	for _, id := range ids {
		c.Parts = append(c.Parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: id, Name: "tool_" + id}})
	}
	return c
}

func respContent(ids ...string) *genai.Content {
	c := &genai.Content{Role: genai.RoleUser}
	for _, id := range ids {
		c.Parts = append(c.Parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: id, Name: "tool_" + id, Response: map[string]any{"ok": true}}})
	}
	return c
}

// shape renders the normalized slice compactly for assertions:
// t=<text>, c=<callID>, r=<respID>, parts within a content joined by
// "+", contents by " ".
func shape(contents []*genai.Content) string {
	s := ""
	for i, c := range contents {
		if i > 0 {
			s += " "
		}
		for j, p := range c.Parts {
			if j > 0 {
				s += "+"
			}
			switch {
			case p == nil:
				// Renders rather than panics: emitting a nil part is a
				// regression this helper should report, not crash on.
				s += "<nil>"
			case p.FunctionCall != nil:
				s += "c=" + p.FunctionCall.ID
			case p.FunctionResponse != nil:
				s += "r=" + p.FunctionResponse.ID
			default:
				s += "t=" + p.Text
			}
		}
	}
	return s
}

func TestNormalizeToolPairs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []*genai.Content
		want string
	}{
		{
			name: "paired adjacent history unchanged",
			in: []*genai.Content{
				textContent(genai.RoleUser, "q"),
				callContent("1"),
				respContent("1"),
				textContent(genai.RoleModel, "a"),
			},
			want: "t=q c=1 r=1 t=a",
		},
		{
			name: "dangling call stripped, surrounding text survives",
			in: []*genai.Content{
				textContent(genai.RoleUser, "q"),
				{Role: genai.RoleModel, Parts: []*genai.Part{
					{Text: "let me check"},
					{FunctionCall: &genai.FunctionCall{ID: "dead", Name: "tool_dead"}},
				}},
			},
			want: "t=q t=let me check",
		},
		{
			name: "call-only content with no response dropped entirely",
			in: []*genai.Content{
				textContent(genai.RoleUser, "q"),
				callContent("dead"),
			},
			want: "t=q",
		},
		{
			name: "orphaned response dropped",
			in: []*genai.Content{
				respContent("ghost"),
				textContent(genai.RoleUser, "q"),
			},
			want: "t=q",
		},
		{
			name: "distant repair response relocated behind its call",
			in: []*genai.Content{
				callContent("1"),
				textContent(genai.RoleModel, "note in between"),
				textContent(genai.RoleUser, "another message"),
				respContent("1"), // e.g. #537 tail-repair event
			},
			want: "c=1 r=1 t=note in between t=another message",
		},
		{
			name: "parallel calls one answered one not",
			in: []*genai.Content{
				callContent("a", "b"),
				respContent("a"),
			},
			want: "c=a r=a",
		},
		{
			name: "two call events each pull their own response",
			in: []*genai.Content{
				callContent("1"),
				respContent("1"),
				callContent("2"),
				respContent("2"),
			},
			want: "c=1 r=1 c=2 r=2",
		},
		{
			name: "empty input",
			in:   nil,
			want: "",
		},
		{
			// Review F1: empty-ID pairs (replayed Gemini-origin
			// histories, #367) must pair positionally by name+order —
			// a valid two-pair history must come out unchanged, not
			// collapse both responses behind the first call.
			name: "empty-ID pairs pair positionally",
			in: []*genai.Content{
				callContent(""),
				respContent(""),
				textContent(genai.RoleUser, "mid"),
				callContent(""),
				respContent(""),
			},
			want: "c= r= t=mid c= r=",
		},
		{
			// Review F2: an orphaned response sharing a content with
			// text loses only the response part, not the text.
			name: "orphaned response part dropped, sibling text survives",
			in: []*genai.Content{
				textContent(genai.RoleUser, "q"),
				{Role: genai.RoleUser, Parts: []*genai.Part{
					{FunctionResponse: &genai.FunctionResponse{ID: "ghost", Name: "tool_ghost"}},
					{Text: "user typed this too"},
				}},
			},
			want: "t=q t=user typed this too",
		},
		{
			// Review F3: a degenerate content mixing a response with a
			// further call keeps the whole chain answered and adjacent.
			name: "mixed response+call content chains correctly",
			in: []*genai.Content{
				callContent("x"),
				{Role: genai.RoleUser, Parts: []*genai.Part{
					{FunctionResponse: &genai.FunctionResponse{ID: "x", Name: "tool_x"}},
					{FunctionCall: &genai.FunctionCall{ID: "y", Name: "tool_y"}},
				}},
				respContent("y"),
			},
			want: "c=x r=x+c=y r=y",
		},
		{
			// F1: an annotation-only part carries no `data` oneof
			// member, so Vertex 400s the whole request over it. Its
			// text sibling has to survive the drop.
			name: "thought-only part dropped, sibling text survives",
			in: []*genai.Content{
				{Role: genai.RoleModel, Parts: []*genai.Part{
					{Text: "a"},
					{Thought: true, ThoughtSignature: []byte("sig")},
				}},
			},
			want: "t=a",
		},
		{
			name: "content of nothing but an empty part is dropped",
			in: []*genai.Content{
				textContent(genai.RoleUser, "q"),
				{Role: genai.RoleModel, Parts: []*genai.Part{{}}},
				textContent(genai.RoleModel, "a"),
			},
			want: "t=q t=a",
		},
		{
			name: "nil part dropped",
			in: []*genai.Content{
				{Role: genai.RoleModel, Parts: []*genai.Part{nil, {Text: "a"}}},
			},
			want: "t=a",
		},
		{
			// The drop must not disturb pairing: the call still finds
			// its response and stays adjacent to it.
			name: "empty part between a call and its response",
			in: []*genai.Content{
				{Role: genai.RoleModel, Parts: []*genai.Part{
					{FunctionCall: &genai.FunctionCall{ID: "1", Name: "tool_1"}},
					{},
				}},
				respContent("1"),
			},
			want: "c=1 r=1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shape(normalizeToolPairs(tc.in)); got != tc.want {
				t.Errorf("normalizeToolPairs = %q, want %q", got, tc.want)
			}
		})
	}
}

// F1 regression, stated as the invariant rather than as a shape: no
// part normalization emits may be one the API will reject.
//
// A single data-less part anywhere in a session's history is not a
// dropped part — it is a 400 for the entire request:
//
//	contents[N].parts[M].data: required oneof field 'data' must have
//	one initialized field
//
// which killed every /btw call and every compaction for the rest of
// that session's life during the #799 GKE smoke run. Enumerating
// shapes is not enough here; the property is what matters, because the
// producer of the bad part was never identified and the next one may
// look different.
func TestNormalizeToolPairs_EmitsNoDataLessPart(t *testing.T) {
	t.Parallel()
	in := []*genai.Content{
		textContent(genai.RoleUser, "q"),
		{Role: genai.RoleModel, Parts: []*genai.Part{{}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{nil}},
		{Role: genai.RoleModel, Parts: []*genai.Part{
			{Thought: true, ThoughtSignature: []byte("sig")},
			{Text: "thinking out loud"},
		}},
		{Role: genai.RoleModel, Parts: []*genai.Part{
			{VideoMetadata: &genai.VideoMetadata{}},
			{FunctionCall: &genai.FunctionCall{ID: "1", Name: "tool_1"}},
		}},
		respContent("1"),
		{Role: genai.RoleModel, Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{ID: "dead", Name: "tool_dead"}},
		}},
	}
	got := normalizeToolPairs(in)
	if len(got) == 0 {
		t.Fatal("normalizeToolPairs dropped everything; the good parts must survive")
	}
	for i, c := range got {
		if len(c.Parts) == 0 {
			t.Errorf("content %d emitted with zero parts", i)
		}
		for j, p := range c.Parts {
			if !partCarriesData(p) {
				t.Errorf("content %d part %d carries no data oneof member: %#v", i, j, p)
			}
		}
	}
	// And the surviving content is still the real conversation.
	if want := "t=q t=thinking out loud c=1 r=1"; shape(got) != want {
		t.Errorf("shape = %q, want %q", shape(got), want)
	}
}

// The originals alias live event contents — normalization must never
// mutate them, even when it filters parts.
func TestNormalizeToolPairs_DoesNotMutateInput(t *testing.T) {
	t.Parallel()
	mixed := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
		{Text: "keep"},
		{FunctionCall: &genai.FunctionCall{ID: "dead", Name: "tool_dead"}},
	}}
	in := []*genai.Content{mixed}
	_ = normalizeToolPairs(in)
	if len(mixed.Parts) != 2 {
		t.Errorf("input content mutated: %d parts, want 2", len(mixed.Parts))
	}
}
