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
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

func TestEcho_EchoesLastUserMessage(t *testing.T) {
	t.Parallel()
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{
			{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "first"}}},
			{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ack"}}},
			{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "latest"}}},
		},
	}
	resps := drain(t, echoLLM{}, req)
	if len(resps) != 2 {
		t.Fatalf("expected 2 responses (partial + TurnComplete), got %d", len(resps))
	}
	if !resps[0].Partial || resps[0].Content.Parts[0].Text != "latest" {
		t.Errorf("first response should be Partial with text 'latest', got %+v", resps[0])
	}
	final := resps[1]
	if !final.TurnComplete {
		t.Errorf("second response should be TurnComplete")
	}
	if final.Content == nil || final.Content.Parts[0].Text != "latest" {
		t.Errorf("TurnComplete should carry full content, got %+v", final.Content)
	}
	if final.FinishReason != genai.FinishReasonStop {
		t.Errorf("expected FinishReasonStop, got %q", final.FinishReason)
	}
}

func TestEcho_EmptyContentsFallback(t *testing.T) {
	t.Parallel()
	resps := drain(t, echoLLM{}, &adkmodel.LLMRequest{})
	if len(resps) != 2 || resps[0].Content.Parts[0].Text != "(echo: no user input)" {
		t.Errorf("expected fallback text, got %+v", resps)
	}
}

func TestEcho_OnlyModelRoleFallback(t *testing.T) {
	t.Parallel()
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{
			{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "no user yet"}}},
		},
	}
	resps := drain(t, echoLLM{}, req)
	if resps[0].Content.Parts[0].Text != "(echo: no user input)" {
		t.Errorf("model-only history should fall through to fallback, got %q", resps[0].Content.Parts[0].Text)
	}
}

func TestEcho_JoinsMultipleParts(t *testing.T) {
	t.Parallel()
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{
			{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hello"}, {Text: "world"}}},
		},
	}
	resps := drain(t, echoLLM{}, req)
	if got := resps[0].Content.Parts[0].Text; got != "hello world" {
		t.Errorf("expected 'hello world', got %q", got)
	}
}

func TestEcho_Name(t *testing.T) {
	t.Parallel()
	if (echoLLM{}).Name() != "echo" {
		t.Errorf("Name() should be 'echo'")
	}
}

// drain consumes the entire iterator and returns every response. Test
// helper to avoid repeating the for-range boilerplate.
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
