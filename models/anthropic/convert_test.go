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

package anthropic

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"
)

func TestBuildParams_TextOnly(t *testing.T) {
	t.Parallel()
	p, err := buildParams("claude-opus-4-7", []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}}},
	}, nil, false, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if p.Model != "claude-opus-4-7" {
		t.Errorf("model = %q", p.Model)
	}
	if p.MaxTokens != int64(DefaultMaxTokens) {
		t.Errorf("MaxTokens = %d, want %d", p.MaxTokens, DefaultMaxTokens)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("messages = %+v", p.Messages)
	}
}

func TestBuildParams_SystemExtractedAndCached(t *testing.T) {
	t.Parallel()
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "be terse"}}},
	}
	p, err := buildParams("claude-opus-4-7", nil, cfg, true, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if len(p.System) != 1 || p.System[0].Text != "be terse" {
		t.Fatalf("system = %+v", p.System)
	}
	// CacheControl is the ephemeral param struct on TextBlockParam.
	// Type is a const that marshals as "ephemeral" when set; we check
	// that the field has been populated by NewCacheControlEphemeralParam.
	raw, err := json.Marshal(p.System[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"cache_control"`)) {
		t.Errorf("expected cache_control in marshaled system block: %s", raw)
	}
}

func TestBuildParams_RoleMapping(t *testing.T) {
	t.Parallel()
	p, err := buildParams("claude-opus-4-7", []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "q"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "a"}}},
	}, nil, false, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if len(p.Messages) != 2 {
		t.Fatalf("messages = %+v", p.Messages)
	}
	if p.Messages[0].Role != anthropic.MessageParamRoleUser ||
		p.Messages[1].Role != anthropic.MessageParamRoleAssistant {
		t.Errorf("roles = %v / %v", p.Messages[0].Role, p.Messages[1].Role)
	}
}

func TestBuildParams_ToolRoundTrip(t *testing.T) {
	t.Parallel()
	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "what's the weather"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{
				ID: "tu_1", Name: "get_weather",
				Args: map[string]any{"city": "Paris"},
			}},
		}},
		{Role: genai.RoleUser, Parts: []*genai.Part{
			{FunctionResponse: &genai.FunctionResponse{
				ID: "tu_1", Name: "get_weather",
				Response: map[string]any{"temp": 72},
			}},
		}},
	}
	p, err := buildParams("claude-opus-4-7", contents, nil, false, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if len(p.Messages) != 3 {
		t.Fatalf("messages = %+v", p.Messages)
	}
	// Assistant turn should carry one tool_use block.
	if p.Messages[1].Content[0].OfToolUse == nil {
		t.Fatalf("expected tool_use on assistant turn: %+v", p.Messages[1].Content[0])
	}
	if p.Messages[1].Content[0].OfToolUse.ID != "tu_1" {
		t.Errorf("tool_use id = %q", p.Messages[1].Content[0].OfToolUse.ID)
	}
	// User follow-up should carry one tool_result block.
	if p.Messages[2].Content[0].OfToolResult == nil {
		t.Fatalf("expected tool_result on user turn: %+v", p.Messages[2].Content[0])
	}
	if p.Messages[2].Content[0].OfToolResult.ToolUseID != "tu_1" {
		t.Errorf("tool_result id = %q", p.Messages[2].Content[0].OfToolResult.ToolUseID)
	}
}

func TestBuildParams_ToolDeclarations(t *testing.T) {
	t.Parallel()
	cfg := &genai.GenerateContentConfig{
		Tools: []*genai.Tool{{
			FunctionDeclarations: []*genai.FunctionDeclaration{{
				Name:        "search",
				Description: "Search the web",
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"q": {Type: genai.TypeString, Description: "query"},
					},
					Required: []string{"q"},
				},
			}},
		}},
	}
	p, err := buildParams("claude-opus-4-7", nil, cfg, false, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if len(p.Tools) != 1 {
		t.Fatalf("tools = %+v", p.Tools)
	}
	tool := p.Tools[0].OfTool
	if tool == nil || tool.Name != "search" {
		t.Fatalf("tool = %+v", tool)
	}
	if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != "q" {
		t.Errorf("required = %v", tool.InputSchema.Required)
	}
	if _, ok := tool.InputSchema.Properties.(map[string]any)["q"]; !ok {
		t.Errorf("expected `q` in properties: %+v", tool.InputSchema.Properties)
	}
}

func TestBuildParams_MaxTokensOverride(t *testing.T) {
	t.Parallel()
	cfg := &genai.GenerateContentConfig{MaxOutputTokens: 2048}
	p, _ := buildParams("claude-opus-4-7", nil, cfg, false, BuiltinTools{})
	if p.MaxTokens != 2048 {
		t.Errorf("MaxTokens = %d, want 2048", p.MaxTokens)
	}
}

func TestMapStopReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   anthropic.StopReason
		want genai.FinishReason
	}{
		{anthropic.StopReasonEndTurn, genai.FinishReasonStop},
		{anthropic.StopReasonToolUse, genai.FinishReasonStop},
		{anthropic.StopReasonStopSequence, genai.FinishReasonStop},
		{anthropic.StopReasonMaxTokens, genai.FinishReasonMaxTokens},
		{anthropic.StopReasonRefusal, genai.FinishReasonSafety},
		{"", genai.FinishReasonUnspecified},
		{"weird", genai.FinishReasonOther},
	}
	for _, tc := range cases {
		if got := mapStopReason(tc.in); got != tc.want {
			t.Errorf("mapStopReason(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestFinalResponseFromMessage_TextAndToolUse(t *testing.T) {
	t.Parallel()
	// Build a Message by hand in the shape the SDK would produce after
	// accumulation. Content is []ContentBlockUnion — we marshal/
	// unmarshal via JSON to populate the union variants correctly.
	msgJSON := `{
		"id": "msg_1",
		"type": "message",
		"role": "assistant",
		"model": "claude-opus-4-7",
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 11, "output_tokens": 22},
		"content": [
			{"type": "text", "text": "let me check"},
			{"type": "tool_use", "id": "tu_2", "name": "lookup", "input": {"key": "val"}}
		]
	}`
	var msg anthropic.Message
	if err := json.Unmarshal([]byte(msgJSON), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	content, finish, usage := finalResponseFromMessage(&msg)
	if finish != genai.FinishReasonStop {
		t.Errorf("finish = %v", finish)
	}
	if usage.PromptTokenCount != 11 || usage.CandidatesTokenCount != 22 {
		t.Errorf("usage = %+v", usage)
	}
	if len(content.Parts) != 2 {
		t.Fatalf("parts = %d", len(content.Parts))
	}
	if content.Parts[0].Text != "let me check" {
		t.Errorf("text = %q", content.Parts[0].Text)
	}
	if content.Parts[1].FunctionCall == nil ||
		content.Parts[1].FunctionCall.Name != "lookup" ||
		content.Parts[1].FunctionCall.Args["key"] != "val" {
		t.Errorf("function call = %+v", content.Parts[1].FunctionCall)
	}
}
