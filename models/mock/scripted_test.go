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

package mock

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

func TestScripted_PlaysTurnsInOrder(t *testing.T) {
	t.Parallel()
	turns := []RecordedTurn{
		{
			Request: &adkmodel.LLMRequest{Model: "m"},
			Responses: []*adkmodel.LLMResponse{
				{Content: textContent(genai.RoleModel, "first"), TurnComplete: true},
			},
		},
		{
			Request: &adkmodel.LLMRequest{Model: "m"},
			Responses: []*adkmodel.LLMResponse{
				{Content: textContent(genai.RoleModel, "second"), TurnComplete: true},
			},
		},
	}
	llm := &scriptedLLM{turns: turns}

	got1 := drain(t, llm, &adkmodel.LLMRequest{})
	if got1[0].Content.Parts[0].Text != "first" {
		t.Errorf("turn 0: got %q", got1[0].Content.Parts[0].Text)
	}
	got2 := drain(t, llm, &adkmodel.LLMRequest{})
	if got2[0].Content.Parts[0].Text != "second" {
		t.Errorf("turn 1: got %q", got2[0].Content.Parts[0].Text)
	}
}

func TestScripted_ExhaustionIsAnError(t *testing.T) {
	t.Parallel()
	llm := &scriptedLLM{turns: []RecordedTurn{
		{Responses: []*adkmodel.LLMResponse{{TurnComplete: true}}},
	}}
	_ = drain(t, llm, &adkmodel.LLMRequest{})

	// Second call must error.
	for _, err := range llm.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
		if err == nil {
			t.Fatal("expected exhaustion error on second call")
		}
		if !strings.Contains(err.Error(), "script exhausted") {
			t.Errorf("error %q missing 'script exhausted'", err.Error())
		}
		return // first iteration is enough
	}
	t.Fatal("expected an iteration with an error")
}

func TestScripted_StrictMatch(t *testing.T) {
	t.Parallel()
	contents := []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hello"}}}}
	llm := &scriptedLLM{
		strict: true,
		turns: []RecordedTurn{{
			Request:   &adkmodel.LLMRequest{Contents: contents},
			Responses: []*adkmodel.LLMResponse{{TurnComplete: true}},
		}},
	}
	got := drain(t, llm, &adkmodel.LLMRequest{Contents: contents})
	if len(got) != 1 || !got[0].TurnComplete {
		t.Errorf("expected matching strict turn to play through, got %+v", got)
	}
}

func TestScripted_StrictMismatch(t *testing.T) {
	t.Parallel()
	llm := &scriptedLLM{
		strict: true,
		turns: []RecordedTurn{{
			Request: &adkmodel.LLMRequest{Contents: []*genai.Content{
				{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "recorded"}}},
			}},
			Responses: []*adkmodel.LLMResponse{{TurnComplete: true}},
		}},
	}
	incoming := &adkmodel.LLMRequest{Contents: []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "different"}}},
	}}
	for _, err := range llm.GenerateContent(context.Background(), incoming, false) {
		if err == nil {
			t.Fatal("expected strict mismatch error")
		}
		if !strings.Contains(err.Error(), "strict mismatch") {
			t.Errorf("error %q missing 'strict mismatch'", err.Error())
		}
		return
	}
	t.Fatal("expected an iteration with an error")
}

func TestLoadScript_TolerantOfBlankAndCommentLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "script.jsonl")
	body := "" +
		"# this is a comment\n" +
		"\n" +
		`{"request":{"model":"m"},"responses":[{"turnComplete":true}]}` + "\n" +
		"# trailing comment\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	turns, err := loadScript(path)
	if err != nil {
		t.Fatalf("loadScript: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}
}

func TestLoadScript_BadJSONL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(path, []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadScript(path)
	if err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("expected line-1 parse error, got %v", err)
	}
}

func TestLoadScript_EmptyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadScript(path)
	if err == nil || !strings.Contains(err.Error(), "no turns found") {
		t.Errorf("expected no-turns error, got %v", err)
	}
}

func textContent(role, s string) *genai.Content {
	return &genai.Content{Role: role, Parts: []*genai.Part{{Text: s}}}
}
