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

// TestPartsToBlocks_ThoughtPartsRoundTrip is the #357 regression gate,
// request side: Thought parts reconstructed from session history must
// replay as thinking / redacted_thinking blocks (signature and order
// preserved, thinking before tool_use — the shape the API demands on
// the assistant turn preceding a tool_result). Unsigned thought parts
// (e.g. Gemini thought summaries after a mid-session provider switch)
// are dropped: the API rejects thinking blocks without a valid
// signature and only requires replay of blocks it itself produced.
func TestPartsToBlocks_ThoughtPartsRoundTrip(t *testing.T) {
	t.Parallel()
	parts := []*genai.Part{
		{Text: "let me check the file", Thought: true, ThoughtSignature: []byte("sig-abc123")},
		{Thought: true, ThoughtSignature: []byte(redactedThinkingPrefix + "opaque-encrypted-payload")},
		{Text: "orphan thought summary", Thought: true}, // unsigned → dropped
		{FunctionCall: &genai.FunctionCall{ID: "toolu_01", Name: "read_file", Args: map[string]any{"path": "a.txt"}}},
	}
	blocks, err := partsToBlocks(parts, newIDSynthesizer())
	if err != nil {
		t.Fatalf("partsToBlocks: %v", err)
	}
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3 (thinking, redacted_thinking, tool_use): %+v", len(blocks), blocks)
	}

	th := blocks[0].OfThinking
	if th == nil || th.Thinking != "let me check the file" || th.Signature != "sig-abc123" {
		t.Errorf("blocks[0] = %+v, want thinking block with text+signature preserved", blocks[0])
	}
	red := blocks[1].OfRedactedThinking
	if red == nil || red.Data != "opaque-encrypted-payload" {
		t.Errorf("blocks[1] = %+v, want redacted_thinking with the opaque payload (prefix peeled)", blocks[1])
	}
	tu := blocks[2].OfToolUse
	if tu == nil || tu.ID != "toolu_01" {
		t.Errorf("blocks[2] = %+v, want the tool_use block after thinking", blocks[2])
	}
}

// TestContentsToMessages_ThinkingToolLoopShape pins the exact history
// shape of the failing #357 scenario: assistant turn with
// thinking+tool_use, then the user turn with the tool_result. The
// rebuilt assistant message must carry the thinking block first.
func TestContentsToMessages_ThinkingToolLoopShape(t *testing.T) {
	t.Parallel()
	contents := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "read a.txt"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{
			{Text: "checking", Thought: true, ThoughtSignature: []byte("sig-1")},
			{FunctionCall: &genai.FunctionCall{ID: "toolu_9", Name: "read_file", Args: map[string]any{"path": "a.txt"}}},
		}},
		{Role: genai.RoleUser, Parts: []*genai.Part{
			{FunctionResponse: &genai.FunctionResponse{ID: "toolu_9", Name: "read_file", Response: map[string]any{"output": "hi"}}},
		}},
	}
	msgs, err := contentsToMessages(contents)
	if err != nil {
		t.Fatalf("contentsToMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	asst := msgs[1]
	if asst.Role != "assistant" || len(asst.Content) != 2 {
		t.Fatalf("assistant msg = %+v, want 2 blocks", asst)
	}
	if asst.Content[0].OfThinking == nil {
		t.Errorf("assistant block[0] = %+v, want thinking FIRST (API 400s a bare tool_use on thinking models)", asst.Content[0])
	}
	if asst.Content[1].OfToolUse == nil {
		t.Errorf("assistant block[1] = %+v, want tool_use after thinking", asst.Content[1])
	}
}

// TestContentsToMessages_ParallelSameToolCallsGetUniqueIDs is the
// #367 regression gate: two ID-less parallel calls to the same tool
// in one assistant turn (the common shape in replayed Gemini-origin
// histories, which frequently omit IDs) must synthesize UNIQUE
// tool_use IDs — Anthropic 400s duplicates — while each tool_result
// pairs with its call by name-occurrence order. The first occurrence
// keeps the historical bare "call_<name>" so single-call histories
// produce byte-identical requests.
func TestContentsToMessages_ParallelSameToolCallsGetUniqueIDs(t *testing.T) {
	t.Parallel()
	contents := []*genai.Content{
		{Role: genai.RoleModel, Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{Name: "grep", Args: map[string]any{"pattern": "foo"}}},
			{FunctionCall: &genai.FunctionCall{Name: "grep", Args: map[string]any{"pattern": "bar"}}},
		}},
		{Role: genai.RoleUser, Parts: []*genai.Part{
			{FunctionResponse: &genai.FunctionResponse{Name: "grep", Response: map[string]any{"output": "foo-hits"}}},
			{FunctionResponse: &genai.FunctionResponse{Name: "grep", Response: map[string]any{"output": "bar-hits"}}},
		}},
	}
	msgs, err := contentsToMessages(contents)
	if err != nil {
		t.Fatalf("contentsToMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}

	callA := msgs[0].Content[0].OfToolUse
	callB := msgs[0].Content[1].OfToolUse
	if callA == nil || callB == nil {
		t.Fatalf("assistant blocks not tool_use: %+v", msgs[0].Content)
	}
	if callA.ID == callB.ID {
		t.Fatalf("duplicate synthesized tool_use IDs %q — Anthropic 400s these", callA.ID)
	}
	if callA.ID != "call_grep" {
		t.Errorf("first ID = %q, want the historical bare call_grep", callA.ID)
	}

	resA := msgs[1].Content[0].OfToolResult
	resB := msgs[1].Content[1].OfToolResult
	if resA == nil || resB == nil {
		t.Fatalf("user blocks not tool_result: %+v", msgs[1].Content)
	}
	if resA.ToolUseID != callA.ID || resB.ToolUseID != callB.ID {
		t.Errorf("result pairing broken: results (%q, %q) vs calls (%q, %q) — must pair by name-occurrence order",
			resA.ToolUseID, resB.ToolUseID, callA.ID, callB.ID)
	}
}

// TestContentsToMessages_IDSynthesisPerRequest pins that the
// uniquifying counters reset per contentsToMessages call: replaying
// the same history twice yields identical IDs both times, so a
// persisted session re-pairs deterministically across requests.
func TestContentsToMessages_IDSynthesisPerRequest(t *testing.T) {
	t.Parallel()
	contents := []*genai.Content{
		{Role: genai.RoleModel, Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{Name: "ls", Args: map[string]any{}}},
			{FunctionCall: &genai.FunctionCall{Name: "ls", Args: map[string]any{}}},
		}},
	}
	first, err := contentsToMessages(contents)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	second, err := contentsToMessages(contents)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	for i := range first[0].Content {
		a, b := first[0].Content[i].OfToolUse.ID, second[0].Content[i].OfToolUse.ID
		if a != b {
			t.Errorf("block %d: IDs differ across passes (%q vs %q) — counters leaked between requests", i, a, b)
		}
	}
	if first[0].Content[1].OfToolUse.ID != "call_ls_2" {
		t.Errorf("second occurrence = %q, want call_ls_2", first[0].Content[1].OfToolUse.ID)
	}
}
