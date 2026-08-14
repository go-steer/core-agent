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
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"
)

// buildParams turns a genai-shaped LLMRequest into the Anthropic SDK's
// MessageNewParams. System prompts come from Config.SystemInstruction;
// tools come from Config.Tools (the ADK's req.Tools map is unused —
// the Gemini backend ignores it too, real tool decls live on Config).
//
// cacheSystem opts in to prompt caching on the last system block.
// builtins enables Anthropic's server-side tools (e.g. web_search) by
// appending them to the request's Tools slice after the function decls.
func buildParams(modelID string, contents []*genai.Content, cfg *genai.GenerateContentConfig, cacheSystem bool, builtins BuiltinTools) (anthropic.MessageNewParams, error) {
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
	// Append server-side built-ins (e.g. web_search) after function
	// decls so a request with both shapes carries both.
	tools = append(tools, builtins.asAnthropicTools()...)
	if len(tools) > 0 {
		params.Tools = tools
	}

	applyGenerationConfig(&params, cfg)

	return params, nil
}

// applyGenerationConfig maps the sampling / stop / tool-choice /
// thinking knobs from a genai config onto the Anthropic params.
// Unset genai fields leave the corresponding param unset so the API
// defaults apply.
func applyGenerationConfig(params *anthropic.MessageNewParams, cfg *genai.GenerateContentConfig) {
	if cfg == nil {
		return
	}
	if cfg.Temperature != nil {
		params.Temperature = anthropic.Float(float64(*cfg.Temperature))
	}
	if cfg.TopP != nil {
		params.TopP = anthropic.Float(float64(*cfg.TopP))
	}
	if cfg.TopK != nil {
		// genai models TopK as a float; Anthropic takes an integer.
		params.TopK = anthropic.Int(int64(*cfg.TopK))
	}
	if len(cfg.StopSequences) > 0 {
		params.StopSequences = cfg.StopSequences
	}
	if tc := toolChoiceParam(cfg.ToolConfig); tc != nil {
		params.ToolChoice = *tc
	}
	// Thinking: only an explicit positive budget opts in. genai's
	// IncludeThoughts alone has no Anthropic equivalent (thinking
	// blocks are always returned when thinking is enabled).
	if cfg.ThinkingConfig != nil && cfg.ThinkingConfig.ThinkingBudget != nil &&
		*cfg.ThinkingConfig.ThinkingBudget > 0 {
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(int64(*cfg.ThinkingConfig.ThinkingBudget))
	}
}

// toolChoiceParam maps genai's FunctionCallingConfig onto Anthropic's
// tool_choice. AUTO / unspecified return nil — Anthropic defaults to
// auto, so leaving the param unset preserves byte-identical requests
// for configs that never set a mode. ANY with exactly one allowed
// function pins that specific tool (Anthropic's closest equivalent of
// Gemini's allowed-names constraint); ANY otherwise maps to "any";
// NONE maps to "none".
func toolChoiceParam(tc *genai.ToolConfig) *anthropic.ToolChoiceUnionParam {
	if tc == nil || tc.FunctionCallingConfig == nil {
		return nil
	}
	fcc := tc.FunctionCallingConfig
	switch fcc.Mode {
	case genai.FunctionCallingConfigModeAny:
		if len(fcc.AllowedFunctionNames) == 1 {
			choice := anthropic.ToolChoiceParamOfTool(fcc.AllowedFunctionNames[0])
			return &choice
		}
		return &anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
	case genai.FunctionCallingConfigModeNone:
		return &anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
	default:
		// AUTO / unspecified: Anthropic's default is auto.
		return nil
	}
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
	ids := newIDSynthesizer()
	for _, c := range contents {
		if c == nil {
			continue
		}
		role := mapRole(c.Role)
		if role == "" {
			continue
		}
		blocks, err := partsToBlocks(c.Parts, ids)
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

// idSynthesizer allocates deterministic per-request IDs for tool
// calls/results whose genai parts carry no ID — parallel calls to the
// same tool (or a replayed Gemini-origin history, which frequently
// omits IDs) would otherwise all synthesize the identical
// "call_<name>" and Anthropic 400s duplicate tool_use IDs (#367).
//
// Pairing invariant: when IDs are absent, a tool_result pairs with
// its tool_use by NAME in ORDER (ADK appends results in call order),
// so the call-side and result-side counters advance independently per
// name and stay aligned across the request. The first occurrence
// keeps the historical bare "call_<name>" so single-call histories
// produce byte-identical requests; only collisions get a suffix.
// One synthesizer per request — counters must never leak across
// requests or the same history would re-pair differently.
type idSynthesizer struct {
	calls map[string]int
	resps map[string]int
}

func newIDSynthesizer() *idSynthesizer {
	return &idSynthesizer{calls: map[string]int{}, resps: map[string]int{}}
}

func (s *idSynthesizer) callID(name string) string {
	s.calls[name]++
	return synthToolID(name, s.calls[name])
}

func (s *idSynthesizer) respID(name string) string {
	s.resps[name]++
	return synthToolID(name, s.resps[name])
}

func synthToolID(name string, n int) string {
	if n == 1 {
		return "call_" + name
	}
	return fmt.Sprintf("call_%s_%d", name, n)
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

// redactedThinkingPrefix marks a ThoughtSignature as carrying an
// Anthropic redacted_thinking payload rather than a plain thinking
// signature. genai.Part has no field for the opaque encrypted Data a
// redacted block must echo back verbatim, so it rides in
// ThoughtSignature behind this marker; finalResponseFromMessage writes
// it, partsToBlocks peels it. The prefix can't collide with a real
// signature reading it back — signatures are base64-ish opaque tokens
// and the prefix is only interpreted on parts we stamped Thought=true.
const redactedThinkingPrefix = "anthropic-redacted-thinking:"

// partsToBlocks converts genai Parts into Anthropic content blocks.
// Supported part types: thought (assistant thinking/redacted_thinking,
// round-tripped for #357), text, FunctionCall (assistant tool_use),
// FunctionResponse (user tool_result). Inline image data + other
// genai part types are skipped with a TODO marker — easy to add later.
func partsToBlocks(parts []*genai.Part, ids *idSynthesizer) ([]anthropic.ContentBlockParamUnion, error) {
	out := make([]anthropic.ContentBlockParamUnion, 0, len(parts))
	for _, p := range parts {
		if p == nil {
			continue
		}
		switch {
		case p.Thought:
			// Must precede the Text case: a thinking part carries its
			// text in Part.Text with Thought=true.
			if block, ok := thoughtBlock(p); ok {
				out = append(out, block)
			}
		case p.Text != "":
			out = append(out, anthropic.NewTextBlock(p.Text))
		case p.FunctionCall != nil:
			block, err := functionCallBlock(p.FunctionCall, ids)
			if err != nil {
				return nil, err
			}
			out = append(out, block)
		case p.FunctionResponse != nil:
			out = append(out, functionResponseBlock(p.FunctionResponse, ids))
		}
	}
	return out, nil
}

// thoughtBlock rebuilds the Anthropic thinking/redacted_thinking block
// a Thought part was converted from. Returns ok=false for thought
// parts that can't be replayed to Anthropic: parts with no signature
// (e.g. Gemini thought summaries after a mid-session provider switch,
// or display-only thoughts) — the API rejects thinking blocks without
// a valid signature, and it only requires replay of blocks it itself
// produced, so dropping foreign ones is both necessary and safe.
func thoughtBlock(p *genai.Part) (anthropic.ContentBlockParamUnion, bool) {
	sig := string(p.ThoughtSignature)
	if data, ok := strings.CutPrefix(sig, redactedThinkingPrefix); ok {
		return anthropic.ContentBlockParamUnion{
			OfRedactedThinking: &anthropic.RedactedThinkingBlockParam{Data: data},
		}, true
	}
	if sig == "" || p.Text == "" {
		return anthropic.ContentBlockParamUnion{}, false
	}
	return anthropic.ContentBlockParamUnion{
		OfThinking: &anthropic.ThinkingBlockParam{
			Thinking:  p.Text,
			Signature: sig,
		},
	}, true
}

// functionCallBlock builds an assistant-side tool_use content block.
// Anthropic requires a non-empty ID so the user-side tool_result can
// be matched back. Genai may omit ID; we synthesize a per-request
// unique one from the function name in that case (see idSynthesizer).
func functionCallBlock(fc *genai.FunctionCall, ids *idSynthesizer) (anthropic.ContentBlockParamUnion, error) {
	id := fc.ID
	if id == "" {
		id = ids.callID(fc.Name)
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
func functionResponseBlock(fr *genai.FunctionResponse, ids *idSynthesizer) anthropic.ContentBlockParamUnion {
	id := fr.ID
	if id == "" {
		id = ids.respID(fr.Name)
	}
	body := ""
	if fr.Response != nil {
		raw, err := json.Marshal(fr.Response)
		if err != nil {
			// Don't swallow the failure into an empty, is_error=false
			// result — the model would read that as a clean success.
			// Surface it as an errored tool result instead.
			return anthropic.NewToolResultBlock(id,
				fmt.Sprintf("core-agent: failed to marshal tool result: %v", err), true)
		}
		body = string(raw)
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
			switch {
			case fd.Parameters != nil:
				props, required, err := schemaToInput(fd.Parameters)
				if err != nil {
					return nil, fmt.Errorf("anthropic: tool %q: %w", fd.Name, err)
				}
				tool.InputSchema = anthropic.ToolInputSchemaParam{
					Properties: props,
					Required:   required,
				}
			case fd.ParametersJsonSchema != nil:
				props, required, extra, err := jsonSchemaToInput(fd.ParametersJsonSchema)
				if err != nil {
					return nil, fmt.Errorf("anthropic: tool %q: %w", fd.Name, err)
				}
				tool.InputSchema = anthropic.ToolInputSchemaParam{
					Properties:  props,
					Required:    required,
					ExtraFields: extra,
				}
			default:
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
	normalizeSchemaTypes(generic)
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

// jsonSchemaToInput projects a genai.FunctionDeclaration's
// ParametersJsonSchema — an opaque `any` holding an already-valid JSON
// Schema — into the (Properties, Required, ExtraFields) triple
// Anthropic's ToolInputSchemaParam expects.
//
// This is the path every ADK-built tool takes: functiontool.New derives
// the declaration from the Go args struct and populates
// ParametersJsonSchema, leaving the typed Parameters field nil. Without
// this branch such tools reach the wire as
// {"type":"object","properties":{}} — the model is shown a tool's name
// and description but none of its parameters, so it guesses argument
// names and every call is rejected by schema validation.
//
// Unlike schemaToInput there is no type normalization to do: the value
// is JSON Schema already, not a marshaled genai.Schema, so its types
// are draft 2020-12 spellings. Keys other than the three that map onto
// typed fields ("type" is always object for a tool input) are carried
// through verbatim as ExtraFields, which is what preserves
// "additionalProperties": false and any "$defs" the generator emitted.
func jsonSchemaToInput(js any) (map[string]any, []string, map[string]any, error) {
	raw, err := json.Marshal(js)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal parametersJsonSchema: %w", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, nil, nil, fmt.Errorf("unmarshal parametersJsonSchema: %w", err)
	}
	props := map[string]any{}
	if p, ok := generic["properties"].(map[string]any); ok {
		props = p
	}
	var required []string
	if r, ok := generic["required"].([]any); ok {
		for _, v := range r {
			if s, ok := v.(string); ok {
				required = append(required, s)
			}
		}
	}
	var extra map[string]any
	for k, v := range generic {
		switch k {
		case "type", "properties", "required":
			// Carried by ToolInputSchemaParam's typed fields;
			// re-emitting them here would duplicate the JSON keys.
		default:
			if extra == nil {
				extra = map[string]any{}
			}
			extra[k] = v
		}
	}
	return props, required, extra, nil
}

// normalizeSchemaTypes rewrites genai.Type enum spellings into JSON
// Schema draft 2020-12 types, recursively. genai marshals Type as its
// proto enum string ("OBJECT", "STRING", ...), which Anthropic's
// strict input_schema validation rejects with 400 invalid_request
// ("It must match JSON Schema draft 2020-12") — any tool declaration
// built from genai typed fields trips it (#532).
//
// The walk is positional: schemas can only appear at the node itself,
// its "items", its "anyOf" elements, and its "properties" values —
// the complete set of schema-valued fields on genai.Schema (which
// this map was just marshaled from). Instance data ("default",
// "example", "enum" values, a property literally named "type") is
// never descended into, so a default that happens to be an object
// with an enum-spelled "type" member passes through untouched.
func normalizeSchemaTypes(schema map[string]any) {
	if ts, ok := schema["type"].(string); ok {
		switch ts {
		case "TYPE_UNSPECIFIED":
			// genai's zero enum; draft 2020-12 has no equivalent —
			// absent "type" means unconstrained, which matches.
			delete(schema, "type")
		case "STRING", "NUMBER", "INTEGER", "BOOLEAN", "ARRAY", "OBJECT", "NULL":
			schema["type"] = strings.ToLower(ts)
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		normalizeSchemaTypes(items)
	}
	if anyOf, ok := schema["anyOf"].([]any); ok {
		for _, alt := range anyOf {
			if s, ok := alt.(map[string]any); ok {
				normalizeSchemaTypes(s)
			}
		}
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		for _, p := range props {
			if s, ok := p.(map[string]any); ok {
				normalizeSchemaTypes(s)
			}
		}
	}
}
