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
	"google.golang.org/adk/session"
	"google.golang.org/genai"
	"google.golang.org/protobuf/types/known/structpb"

	axproto "github.com/go-steer/core-agent/extras/ax-agent/internal/axproto"
)

// axMessagesToGenai turns AX's wire conversation history into the
// genai.Contents shape the ADK runner expects. The trailing message
// in the AX history is the new user prompt; the rest is conversation
// history. Both are emitted in order; the caller (server.go) is
// responsible for splitting the trailing user message from the prefix
// when feeding it into agent.RunWithContents.
//
// Role mapping: "user" → genai.RoleUser; "assistant" or "model" →
// genai.RoleModel; anything else defaults to user (defensive — better
// than dropping content).
func axMessagesToGenai(msgs []*axproto.Message) []*genai.Content {
	out := make([]*genai.Content, 0, len(msgs))
	for _, m := range msgs {
		if m == nil {
			continue
		}
		c := &genai.Content{Role: axRoleToGenai(m.GetRole())}
		if part := axContentToPart(m.GetContent()); part != nil {
			c.Parts = []*genai.Part{part}
		}
		// Skip entries with no parts — they'd be opaque to the model.
		if len(c.Parts) == 0 {
			continue
		}
		out = append(out, c)
	}
	return out
}

func axRoleToGenai(role string) string {
	switch role {
	case "user":
		return genai.RoleUser
	case "assistant", "model":
		return genai.RoleModel
	default:
		return genai.RoleUser
	}
}

// axContentToPart projects one AX Content (a oneof) into a genai.Part.
// Returns nil for content variants we don't handle today (image,
// audio, video, document, thought, confirmation) — adding them is
// additive when a real consumer needs them.
func axContentToPart(c *axproto.Content) *genai.Part {
	if c == nil {
		return nil
	}
	switch t := c.GetType().(type) {
	case *axproto.Content_Text:
		return &genai.Part{Text: t.Text.GetText()}
	case *axproto.Content_ToolCall:
		fc := t.ToolCall.GetFunctionCall()
		if fc == nil {
			return nil
		}
		return &genai.Part{FunctionCall: &genai.FunctionCall{
			ID:   t.ToolCall.GetId(),
			Name: fc.GetName(),
			Args: fc.GetArguments().AsMap(),
		}}
	case *axproto.Content_ToolResult:
		fr := t.ToolResult.GetFunctionResult()
		if fr == nil {
			return nil
		}
		var resp map[string]any
		if r, ok := fr.GetResult().(*axproto.FunctionResultContent_Response); ok && r.Response != nil {
			resp = r.Response.AsMap()
		}
		return &genai.Part{FunctionResponse: &genai.FunctionResponse{
			ID:       t.ToolResult.GetCallId(),
			Name:     fr.GetName(),
			Response: resp,
		}}
	}
	return nil
}

// genaiEventToAXOutputs converts one ADK session.Event into an
// AgentOutputs payload, or nil when the event has no externally
// meaningful content (e.g. a usage-only update, an empty content).
//
// Tool calls and tool responses are flagged InternalOnly: true — they
// belong in the AX execution log but shouldn't surface in the
// user-facing conversation render. Final assistant text and partial
// text both stay InternalOnly: false so the UI sees streaming output.
func genaiEventToAXOutputs(ev *session.Event) *axproto.AgentOutputs {
	if ev == nil || ev.Content == nil {
		return nil
	}
	role := genaiRoleToAX(ev.Content.Role)
	var msgs []*axproto.Message
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		switch {
		case p.Text != "":
			msgs = append(msgs, &axproto.Message{
				Role: role,
				Content: &axproto.Content{Type: &axproto.Content_Text{
					Text: &axproto.TextContent{Text: p.Text},
				}},
			})
		case p.FunctionCall != nil:
			args, _ := structpb.NewStruct(p.FunctionCall.Args)
			msgs = append(msgs, &axproto.Message{
				Role:         role,
				InternalOnly: true,
				Content: &axproto.Content{Type: &axproto.Content_ToolCall{
					ToolCall: &axproto.ToolCallContent{
						Id: p.FunctionCall.ID,
						Type: &axproto.ToolCallContent_FunctionCall{
							FunctionCall: &axproto.FunctionCallContent{
								Name:      p.FunctionCall.Name,
								Arguments: args,
							},
						},
					},
				}},
			})
		case p.FunctionResponse != nil:
			resp, _ := structpb.NewStruct(p.FunctionResponse.Response)
			msgs = append(msgs, &axproto.Message{
				Role:         role,
				InternalOnly: true,
				Content: &axproto.Content{Type: &axproto.Content_ToolResult{
					ToolResult: &axproto.ToolResultContent{
						CallId: p.FunctionResponse.ID,
						Type: &axproto.ToolResultContent_FunctionResult{
							FunctionResult: &axproto.FunctionResultContent{
								Name: p.FunctionResponse.Name,
								Result: &axproto.FunctionResultContent_Response{
									Response: resp,
								},
							},
						},
					},
				}},
			})
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return &axproto.AgentOutputs{Messages: msgs}
}

func genaiRoleToAX(role string) string {
	if role == genai.RoleUser {
		return "user"
	}
	return "assistant"
}
