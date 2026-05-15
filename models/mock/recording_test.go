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
	"errors"
	"iter"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// fakeLLM yields a canned sequence of responses (and optionally a
// terminating error) for testing the recorder wrapper.
type fakeLLM struct {
	name      string
	responses []*adkmodel.LLMResponse
	finalErr  error
}

func (f *fakeLLM) Name() string { return f.name }

func (f *fakeLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		for _, r := range f.responses {
			if !yield(r, nil) {
				return
			}
		}
		if f.finalErr != nil {
			yield(nil, f.finalErr)
		}
	}
}

func TestRecorder_RoundTrip(t *testing.T) {
	t.Parallel()
	inner := &fakeLLM{
		name: "fake",
		responses: []*adkmodel.LLMResponse{
			{Content: textContent(genai.RoleModel, "hel"), Partial: true},
			{Content: textContent(genai.RoleModel, "lo"), Partial: true},
			{Content: textContent(genai.RoleModel, "hello"), TurnComplete: true, FinishReason: genai.FinishReasonStop},
		},
	}
	var buf bytes.Buffer
	rec := NewRecorder(inner, &buf)

	req := &adkmodel.LLMRequest{
		Model: "test-model",
		Contents: []*genai.Content{
			{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "say hi"}}},
		},
	}
	got := drain(t, rec, req)
	if len(got) != 3 {
		t.Fatalf("expected 3 responses passed through, got %d", len(got))
	}
	if rec.Name() != "fake" {
		t.Errorf("Name() should delegate to inner, got %q", rec.Name())
	}

	// Round-trip: parse the buffer back via decodeScript and assert we
	// see one turn with the same three responses.
	turns, err := decodeScript(&buf, "buf")
	if err != nil {
		t.Fatalf("decodeScript: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 recorded turn, got %d", len(turns))
	}
	tu := turns[0]
	if tu.Request == nil || tu.Request.Model != "test-model" {
		t.Errorf("recorded request lost model field: %+v", tu.Request)
	}
	if len(tu.Request.Contents) != 1 || tu.Request.Contents[0].Parts[0].Text != "say hi" {
		t.Errorf("recorded contents wrong: %+v", tu.Request.Contents)
	}
	if len(tu.Responses) != 3 {
		t.Fatalf("expected 3 recorded responses, got %d", len(tu.Responses))
	}
	if !tu.Responses[2].TurnComplete {
		t.Errorf("third response should be TurnComplete")
	}
	if tu.Responses[2].FinishReason != genai.FinishReasonStop {
		t.Errorf("FinishReason lost in round-trip: %q", tu.Responses[2].FinishReason)
	}
}

func TestRecorder_PassesThroughErrors(t *testing.T) {
	t.Parallel()
	inner := &fakeLLM{
		name: "fake",
		responses: []*adkmodel.LLMResponse{
			{Content: textContent(genai.RoleModel, "partial"), Partial: true},
		},
		finalErr: errors.New("boom"),
	}
	var buf bytes.Buffer
	rec := NewRecorder(inner, &buf)

	var sawErr error
	for _, err := range rec.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
		if err != nil {
			sawErr = err
		}
	}
	if sawErr == nil || !strings.Contains(sawErr.Error(), "boom") {
		t.Fatalf("expected propagated error, got %v", sawErr)
	}

	// Even with a final error, the partial we did receive should be
	// recorded so a debugger can see what the model produced before
	// things went sideways.
	turns, err := decodeScript(&buf, "buf")
	if err != nil {
		t.Fatalf("decodeScript: %v", err)
	}
	if len(turns) != 1 || len(turns[0].Responses) != 1 {
		t.Fatalf("expected 1 turn with 1 partial recorded, got %+v", turns)
	}
	if !turns[0].Responses[0].Partial {
		t.Errorf("recorded response should still be marked Partial")
	}
}

func TestRecorder_RecordThenReplayWithScripted(t *testing.T) {
	t.Parallel()
	inner := &fakeLLM{
		name: "fake",
		responses: []*adkmodel.LLMResponse{
			{Content: textContent(genai.RoleModel, "first"), TurnComplete: true},
		},
	}
	var buf bytes.Buffer
	rec := NewRecorder(inner, &buf)

	req := &adkmodel.LLMRequest{Contents: []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "go"}}},
	}}
	_ = drain(t, rec, req)

	// Feed the recording into a scripted LLM and verify it replays.
	turns, err := decodeScript(&buf, "buf")
	if err != nil {
		t.Fatal(err)
	}
	scripted := &scriptedLLM{turns: turns}
	got := drain(t, scripted, req)
	if len(got) != 1 || got[0].Content.Parts[0].Text != "first" {
		t.Errorf("scripted replay didn't reproduce recorded response, got %+v", got)
	}
}
