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

package main

import (
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
	"google.golang.org/protobuf/types/known/structpb"

	axproto "github.com/google/ax/proto"
)

func TestAXMessagesToGenai_TextRoundTrip(t *testing.T) {
	t.Parallel()
	got := axMessagesToGenai([]*axproto.Message{
		{
			Role:    "user",
			Content: textContent("hello"),
		},
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 content, got %d", len(got))
	}
	if got[0].Role != genai.RoleUser {
		t.Errorf("role: got %q want %q", got[0].Role, genai.RoleUser)
	}
	if got[0].Parts[0].Text != "hello" {
		t.Errorf("text: got %q", got[0].Parts[0].Text)
	}
}

func TestAXMessagesToGenai_RoleMapping(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"user":      genai.RoleUser,
		"assistant": genai.RoleModel,
		"model":     genai.RoleModel,
		"unknown":   genai.RoleUser, // defensive default
	}
	for axRole, want := range cases {
		t.Run(axRole, func(t *testing.T) {
			t.Parallel()
			got := axMessagesToGenai([]*axproto.Message{{Role: axRole, Content: textContent("x")}})
			if got[0].Role != want {
				t.Errorf("role %q → %q, want %q", axRole, got[0].Role, want)
			}
		})
	}
}

func TestAXMessagesToGenai_SkipsEmptyContent(t *testing.T) {
	t.Parallel()
	got := axMessagesToGenai([]*axproto.Message{
		{Role: "user", Content: textContent("kept")},
		{Role: "user", Content: nil},
		nil,
		{Role: "user", Content: textContent("also kept")},
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 contents (skipping nil + empty), got %d: %+v", len(got), got)
	}
	if got[0].Parts[0].Text != "kept" || got[1].Parts[0].Text != "also kept" {
		t.Errorf("ordering or filtering wrong: %+v", got)
	}
}

func TestAXMessagesToGenai_ToolCallRoundTrip(t *testing.T) {
	t.Parallel()
	args, _ := structpb.NewStruct(map[string]any{"city": "Paris"})
	got := axMessagesToGenai([]*axproto.Message{
		{
			Role: "assistant",
			Content: &axproto.Content{Type: &axproto.Content_ToolCall{
				ToolCall: &axproto.ToolCallContent{
					Id: "call-1",
					Type: &axproto.ToolCallContent_FunctionCall{
						FunctionCall: &axproto.FunctionCallContent{
							Name:      "get_weather",
							Arguments: args,
						},
					},
				},
			}},
		},
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 content, got %d", len(got))
	}
	fc := got[0].Parts[0].FunctionCall
	if fc == nil {
		t.Fatalf("expected FunctionCall, got %+v", got[0].Parts[0])
	}
	if fc.Name != "get_weather" || fc.ID != "call-1" {
		t.Errorf("name/id wrong: %+v", fc)
	}
	if fc.Args["city"] != "Paris" {
		t.Errorf("args wrong: %+v", fc.Args)
	}
}

func TestAXMessagesToGenai_ToolResultRoundTrip(t *testing.T) {
	t.Parallel()
	resp, _ := structpb.NewStruct(map[string]any{"temp": float64(72)})
	got := axMessagesToGenai([]*axproto.Message{
		{
			Role: "user",
			Content: &axproto.Content{Type: &axproto.Content_ToolResult{
				ToolResult: &axproto.ToolResultContent{
					CallId: "call-1",
					Type: &axproto.ToolResultContent_FunctionResult{
						FunctionResult: &axproto.FunctionResultContent{
							Name: "get_weather",
							Result: &axproto.FunctionResultContent_Response{
								Response: resp,
							},
						},
					},
				},
			}},
		},
	})
	fr := got[0].Parts[0].FunctionResponse
	if fr == nil {
		t.Fatalf("expected FunctionResponse, got %+v", got[0].Parts[0])
	}
	if fr.Name != "get_weather" || fr.ID != "call-1" {
		t.Errorf("name/id wrong: %+v", fr)
	}
	if fr.Response["temp"] != float64(72) {
		t.Errorf("response wrong: %+v", fr.Response)
	}
}

func TestGenaiEventToAXOutputs_PartialText(t *testing.T) {
	t.Parallel()
	ev := &session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: "hello"}},
		},
		Partial: true,
	}}
	got := genaiEventToAXOutputs(ev)
	if got == nil || len(got.Messages) != 1 {
		t.Fatalf("expected 1 message, got %+v", got)
	}
	m := got.Messages[0]
	if m.Role != "assistant" {
		t.Errorf("role: got %q", m.Role)
	}
	if m.InternalOnly {
		t.Errorf("text should not be InternalOnly")
	}
	tc, ok := m.Content.Type.(*axproto.Content_Text)
	if !ok || tc.Text.GetText() != "hello" {
		t.Errorf("expected text content 'hello', got %+v", m.Content)
	}
}

func TestGenaiEventToAXOutputs_FunctionCallInternalOnly(t *testing.T) {
	t.Parallel()
	ev := &session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					ID:   "call-9",
					Name: "search",
					Args: map[string]any{"q": "go"},
				},
			}},
		},
	}}
	got := genaiEventToAXOutputs(ev)
	if got == nil || len(got.Messages) != 1 {
		t.Fatalf("expected 1 message, got %+v", got)
	}
	m := got.Messages[0]
	if !m.InternalOnly {
		t.Errorf("function call should be InternalOnly")
	}
	tc, ok := m.Content.Type.(*axproto.Content_ToolCall)
	if !ok {
		t.Fatalf("expected ToolCall content, got %T", m.Content.Type)
	}
	if tc.ToolCall.Id != "call-9" {
		t.Errorf("call id: got %q", tc.ToolCall.Id)
	}
	fc := tc.ToolCall.GetFunctionCall()
	if fc == nil || fc.Name != "search" {
		t.Errorf("func name: got %+v", fc)
	}
}

func TestGenaiEventToAXOutputs_NilOnEmptyContent(t *testing.T) {
	t.Parallel()
	if got := genaiEventToAXOutputs(nil); got != nil {
		t.Errorf("nil event should yield nil, got %+v", got)
	}
	if got := genaiEventToAXOutputs(&session.Event{}); got != nil {
		t.Errorf("event with no content should yield nil, got %+v", got)
	}
}

// textContent is a shorthand for building a TextContent variant.
func textContent(s string) *axproto.Content {
	return &axproto.Content{Type: &axproto.Content_Text{
		Text: &axproto.TextContent{Text: s},
	}}
}
