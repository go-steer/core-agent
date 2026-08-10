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
	"context"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

func TestSanitizeContentRoles(t *testing.T) {
	t.Parallel()
	user := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}}}
	model := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "yo"}}}
	roleless := &genai.Content{Parts: []*genai.Part{{Text: "audit note"}}} // Role == ""
	bogus := &genai.Content{Role: "system", Parts: []*genai.Part{{Text: "x"}}}

	cases := []struct {
		name    string
		in      []*genai.Content
		want    []*genai.Content
		changed bool
	}{
		{"all valid — unchanged", []*genai.Content{user, model}, []*genai.Content{user, model}, false},
		{"drops role-less", []*genai.Content{user, roleless, model}, []*genai.Content{user, model}, true},
		{"drops unknown role", []*genai.Content{bogus, user}, []*genai.Content{user}, true},
		{"drops nil element", []*genai.Content{user, nil, model}, []*genai.Content{user, model}, true},
		{"leading invalid", []*genai.Content{roleless, user}, []*genai.Content{user}, true},
		{"empty input", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, changed := sanitizeContentRoles(tc.in)
			if changed != tc.changed {
				t.Errorf("changed = %v, want %v", changed, tc.changed)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (%v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
			// Unchanged must return the caller's slice as-is (no copy).
			if !tc.changed && tc.in != nil && &got[0] != &tc.in[0] {
				t.Error("unchanged result should alias the input slice, not copy it")
			}
		})
	}
}

// TestRoleSanitizingLLM_DropsInvalidRoleContentDoesNotMutateCaller
// verifies the decorator filters req.Contents without touching the
// caller's request or its backing slice.
func TestRoleSanitizingLLM_DropsInvalidRoleContentDoesNotMutateCaller(t *testing.T) {
	t.Parallel()
	capture := &captureLLM{response: "ok"}
	llm := newRoleSanitizingLLM(capture)

	orig := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}}},
		{Parts: []*genai.Part{{Text: "audit note"}}}, // role-less
	}
	req := &adkmodel.LLMRequest{Contents: orig}
	for range llm.GenerateContent(context.Background(), req, false) {
	}

	// Caller's request is untouched.
	if len(req.Contents) != 2 {
		t.Errorf("caller req.Contents len = %d, want 2 (must not be mutated)", len(req.Contents))
	}
	// The inner model saw only the valid-role content.
	got := capture.lastRequest()
	if got == nil || len(got.Contents) != 1 || got.Contents[0].Role != genai.RoleUser {
		t.Fatalf("inner request = %v, want a single user content", got)
	}
}

// TestCheckpoint_DropsRoleLessAnnotationFromRequest is the #614
// regression test: a role-less annotation event (the shape autonomous
// notes / grounding audit rows commit) must never reach the model
// request, or Vertex Gemini 400s with "Please use a valid role: user,
// model." The summarizer path (Checkpoint) bypasses ADK's content
// processor, so on pre-fix code the role-less row survives into
// LLMRequest.Contents and this test fails.
func TestCheckpoint_DropsRoleLessAnnotationFromRequest(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "# Task\nDone."}
	a, err := New(llm, WithCheckpointer(NewDefaultCheckpointer()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "investigate the crash")
	plantEvent(t, a, "", "[grounding] audit-only note, not conversation") // role-less annotation row
	plantEvent(t, a, genai.RoleModel, "found the null deref")

	if _, err := a.Checkpoint(context.Background(), ""); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	req := llm.lastRequest()
	if req == nil {
		t.Fatal("model wasn't called")
	}
	for i, c := range req.Contents {
		if c == nil {
			t.Errorf("Contents[%d] is nil", i)
			continue
		}
		if c.Role != genai.RoleUser && c.Role != genai.RoleModel {
			t.Errorf("Contents[%d] has role %q; Gemini accepts only user/model (#614)", i, c.Role)
		}
		if contentText(c) == "[grounding] audit-only note, not conversation" {
			t.Errorf("Contents[%d] is the role-less annotation row; it must be dropped", i)
		}
	}
}
