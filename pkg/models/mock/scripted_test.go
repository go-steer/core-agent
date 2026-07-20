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
	"bytes"
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/recording"
)

func TestScripted_PlaysTurnsInOrder(t *testing.T) {
	t.Parallel()
	turns := []recording.RecordedTurn{
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
	llm := &scriptedLLM{turns: []recording.RecordedTurn{
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
		turns: []recording.RecordedTurn{{
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
		turns: []recording.RecordedTurn{{
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

// TestScripted_PlaysFromRecording is the cross-package integration
// check: writes a session through recording.NewRecorder, loads the
// JSONL via this package's decodeScript, and replays it. Pins the
// wire-format contract between the two packages.
func TestScripted_PlaysFromRecording(t *testing.T) {
	t.Parallel()
	inner := &recordingInnerLLM{
		responses: []*adkmodel.LLMResponse{
			{Content: textContent(genai.RoleModel, "first"), TurnComplete: true},
		},
	}
	var buf bytes.Buffer
	rec := recording.NewRecorder(inner, &buf)

	req := &adkmodel.LLMRequest{Contents: []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "go"}}},
	}}
	for _, err := range rec.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("recorder: %v", err)
		}
	}

	turns, err := decodeScript(&buf, "buf")
	if err != nil {
		t.Fatalf("decodeScript: %v", err)
	}
	scripted := &scriptedLLM{turns: turns}
	got := drain(t, scripted, req)
	if len(got) != 1 || got[0].Content.Parts[0].Text != "first" {
		t.Errorf("scripted replay didn't reproduce recorded response, got %+v", got)
	}
}

// recordingInnerLLM is a tiny stand-in inner LLM for the cross-package
// test above; mirrors recorder_test.go's fakeLLM but local to this
// package so we don't have to export it.
type recordingInnerLLM struct {
	responses []*adkmodel.LLMResponse
}

func (r *recordingInnerLLM) Name() string { return "fake" }

func (r *recordingInnerLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		for _, resp := range r.responses {
			if !yield(resp, nil) {
				return
			}
		}
	}
}
