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
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"
)

// buildParams turns a genai-shaped LLMRequest into the Anthropic SDK's
// MessageNewParams. System prompts come from Config.SystemInstruction;
// tools come from Config.Tools (the ADK's req.Tools map is unused —
// the Gemini backend ignores it too, real tool decls live on Config).
//
// cacheSystem opts in to prompt caching on the last system block.
func buildParams(modelID string, contents []*genai.Content, cfg *genai.GenerateContentConfig, cacheSystem bool) (anthropic.MessageNewParams, error) {
	if modelID == "" {
		modelID = DefaultModel
	}

	params := anthropic.MessageNewParams{
		Model:     modelID,
		MaxTokens: int64(maxTokens(cfg)),
	}

	system := systemBlocks(cfg, cacheSystem)
	if len(system) > 0 {
		params.System = system
	}

	msgs, err := contentsToMessages(contents)
	if err != nil {
		return anthropic.MessageNewParams{}, err
	}
	params.Messages = msgs

	tools, err := toolsParam(cfg)
	if err != nil {
		return anthropic.MessageNewParams{}, err
	}
	if len(tools) > 0 {
		params.Tools = tools
	}

	return params, nil
}

// maxTokens picks a MaxTokens value, preferring an explicit override
// from the genai config and falling back to DefaultMaxTokens.
func maxTokens(cfg *genai.GenerateContentConfig) int {
	if cfg != nil && cfg.MaxOutputTokens > 0 {
		return int(cfg.MaxOutputTokens)
	}
	return DefaultMaxTokens
}

// systemBlocks extracts the system instruction from a genai config.
// Returns nil when there's no system content. When cacheSystem is true,
// the last block carries an ephemeral CacheControl marker so repeated
// turns with the same system prompt benefit from prompt caching.
func systemBlocks(cfg *genai.GenerateContentConfig, cacheSystem bool) []anthropic.TextBlockParam {
	if cfg == nil || cfg.SystemInstruction == nil {
		return nil
	}
	var out []anthropic.TextBlockParam
	for _, p := range cfg.SystemInstruction.Parts {
		if p == nil || p.Text == "" {
			continue
		}
		out = append(out, anthropic.TextBlockParam{Text: p.Text})
	}
	if cacheSystem && len(out) > 0 {
		out[len(out)-1].CacheControl = anthropic.NewCacheControlEphemeralParam()
	}
	return out
}

// contentsToMessages converts genai Contents (the chat history) into
// Anthropic MessageParams. Genai uses "user" / "model" for roles; we
// map "model" → assistant. System-role contents are dropped here —
// system prompts must live on Config.SystemInstruction so the caller
// can hoist them to the top-level System field on Anthropic's API.
func contentsToMessages(contents []*genai.Content) ([]anthropic.MessageParam, error) {
	out := make([]anthropic.MessageParam, 0, len(contents))
	for _, c := range contents {
		if c == nil {
			continue
		}
		role := mapRole(c.Role)
		if role == "" {
			continue
		}
		blocks, err := partsToBlocks(c.Parts)
		if err != nil {
			return nil, err
		}
		if len(blocks) == 0 {
			continue
		}
		out = append(out, anthropic.MessageParam{Role: role, Content: blocks})
	}
	return out, nil
}

func mapRole(r string) anthropic.MessageParamRole {
	switch r {
	case genai.RoleUser, "":
		// Empty role from ADK is treated as user (matches genai
		// defaults).
		return anthropic.MessageParamRoleUser
	case genai.RoleModel:
		return anthropic.MessageParamRoleAssistant
	default:
		return ""
	}
}

// partsToBlocks converts genai Parts into Anthropic content blocks.
// Supported part types: text, FunctionCall (assistant tool_use),
// FunctionResponse (user tool_result). Inline image data + other
// genai part types are skipped with a TODO marker — easy to add later.
func partsToBlocks(parts []*genai.Part) ([]anthropic.ContentBlockParamUnion, error) {
	out := make([]anthropic.ContentBlockParamUnion, 0, len(parts))
	for _, p := range parts {
		if p == nil {
			continue
		}
		switch {
		case p.Text != "":
			out = append(out, anthropic.NewTextBlock(p.Text))
		case p.FunctionCall != nil:
			block, err := functionCallBlock(p.FunctionCall)
			if err != nil {
				return nil, err
			}
			out = append(out, block)
		case p.FunctionResponse != nil:
			out = append(out, functionResponseBlock(p.FunctionResponse))
		}
	}
	return out, nil
}

// functionCallBlock builds an assistant-side tool_use content block.
// Anthropic requires a non-empty ID so the user-side tool_result can
// be matched back. Genai may omit ID; we synthesize from the function
// name in that case.
func functionCallBlock(fc *genai.FunctionCall) (anthropic.ContentBlockParamUnion, error) {
	id := fc.ID
	if id == "" {
		id = "call_" + fc.Name
	}
	args := fc.Args
	if args == nil {
		args = map[string]any{}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return anthropic.ContentBlockParamUnion{}, fmt.Errorf("anthropic: marshal tool args: %w", err)
	}
	return anthropic.ContentBlockParamUnion{
		OfToolUse: &anthropic.ToolUseBlockParam{
			ID:    id,
			Name:  fc.Name,
			Input: json.RawMessage(raw),
		},
	}, nil
}

// functionResponseBlock builds a user-side tool_result content block.
// We collapse the genai FunctionResponse.Response map into JSON text;
// Anthropic accepts string content blocks for tool results.
func functionResponseBlock(fr *genai.FunctionResponse) anthropic.ContentBlockParamUnion {
	id := fr.ID
	if id == "" {
		id = "call_" + fr.Name
	}
	body := ""
	if fr.Response != nil {
		if raw, err := json.Marshal(fr.Response); err == nil {
			body = string(raw)
		}
	}
	return anthropic.NewToolResultBlock(id, body, false)
}

// toolsParam converts genai.Tool entries into Anthropic ToolUnionParams.
// Only FunctionDeclarations are mapped; provider-specific tools
// (GoogleSearch, ComputerUse, etc.) are skipped silently.
//
// Each genai.Schema is JSON-roundtripped to a map[string]any so it
// can populate ToolInputSchemaParam.Properties — this avoids hand-
// writing a Schema → JSON-Schema converter.
func toolsParam(cfg *genai.GenerateContentConfig) ([]anthropic.ToolUnionParam, error) {
	if cfg == nil {
		return nil, nil
	}
	var out []anthropic.ToolUnionParam
	for _, t := range cfg.Tools {
		if t == nil {
			continue
		}
		for _, fd := range t.FunctionDeclarations {
			if fd == nil || fd.Name == "" {
				continue
			}
			tool := anthropic.ToolParam{Name: fd.Name}
			if fd.Description != "" {
				tool.Description = anthropic.String(fd.Description)
			}
			if fd.Parameters != nil {
				props, required, err := schemaToInput(fd.Parameters)
				if err != nil {
					return nil, fmt.Errorf("anthropic: tool %q: %w", fd.Name, err)
				}
				tool.InputSchema = anthropic.ToolInputSchemaParam{
					Properties: props,
					Required:   required,
				}
			} else {
				// Anthropic requires a non-nil InputSchema; an empty
				// object is the canonical "no parameters" shape.
				tool.InputSchema = anthropic.ToolInputSchemaParam{
					Properties: map[string]any{},
				}
			}
			out = append(out, anthropic.ToolUnionParam{OfTool: &tool})
		}
	}
	return out, nil
}

// schemaToInput projects a genai.Schema into the (Properties, Required)
// pair Anthropic's ToolInputSchemaParam expects. JSON round-trip keeps
// the conversion robust against future genai.Schema field additions.
func schemaToInput(s *genai.Schema) (map[string]any, []string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal schema: %w", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, nil, fmt.Errorf("unmarshal schema: %w", err)
	}
	var props map[string]any
	if p, ok := generic["properties"].(map[string]any); ok {
		props = p
	} else {
		props = map[string]any{}
	}
	var required []string
	if r, ok := generic["required"].([]any); ok {
		for _, v := range r {
			if s, ok := v.(string); ok {
				required = append(required, s)
			}
		}
	}
	return props, required, nil
}
