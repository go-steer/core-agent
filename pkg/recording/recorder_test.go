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

package recording

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

	turns := decodeJSONL(t, &buf)
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
	turns := decodeJSONL(t, &buf)
	if len(turns) != 1 || len(turns[0].Responses) != 1 {
		t.Fatalf("expected 1 turn with 1 partial recorded, got %+v", turns)
	}
	if !turns[0].Responses[0].Partial {
		t.Errorf("recorded response should still be marked Partial")
	}
}

// decodeJSONL parses a JSONL buffer into RecordedTurns. Test helper
// kept local so the recording package has zero dep on models/mock's
// scripted decoder.
func decodeJSONL(t *testing.T, buf *bytes.Buffer) []RecordedTurn {
	t.Helper()
	var out []RecordedTurn
	sc := bufio.NewScanner(buf)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var turn RecordedTurn
		if err := json.Unmarshal(raw, &turn); err != nil {
			t.Fatalf("decodeJSONL: %v", err)
		}
		out = append(out, turn)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("decodeJSONL scan: %v", err)
	}
	return out
}

func textContent(role, s string) *genai.Content {
	return &genai.Content{Role: role, Parts: []*genai.Part{{Text: s}}}
}

func drain(t *testing.T, llm adkmodel.LLM, req *adkmodel.LLMRequest) []*adkmodel.LLMResponse {
	t.Helper()
	var out []*adkmodel.LLMResponse
	for resp, err := range llm.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out = append(out, resp)
	}
	return out
}
