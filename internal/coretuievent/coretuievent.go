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

// Package coretuievent projects ADK session events into the shapes
// go-steer/core-tui renders.
//
// It exists because both TUI hosts need the same projection and reach
// it by different routes: the remote adapter
// (internal/coretuiremote) hydrates events out of an HTTP response,
// the in-process TUI (cmd/core-agent) reads them straight off the
// event log. A tool call has to summarize identically either way —
// an operator shouldn't be able to tell which transport they're on by
// looking at a tool row.
package coretuievent

import (
	"strings"

	"google.golang.org/adk/session"
	"google.golang.org/genai"

	coretui "github.com/go-steer/core-tui/tui"
)

// ToolCall projects a genai function-call into a coretui.ToolCall. ID
// is the function-call ID the model emits (used by core-tui to dedup
// partial + final echoes of the same call across streamed events).
func ToolCall(p *genai.Part) coretui.ToolCall {
	tc := coretui.ToolCall{
		ID:   p.FunctionCall.ID,
		Name: p.FunctionCall.Name,
	}
	if len(p.FunctionCall.Args) > 0 {
		tc.Args = make(map[string]any, len(p.FunctionCall.Args))
		for k, v := range p.FunctionCall.Args {
			tc.Args[k] = v
		}
	}
	return tc
}

// ToolResult projects a genai function-response. Error strings come
// from a conventional "error" key in the response map; everything else
// is preserved verbatim so core-tui's per-tool renderers can pick the
// relevant fields (`content` for read_file, `stdout`/`stderr` for
// bash, etc.).
func ToolResult(p *genai.Part) coretui.ToolResult {
	tr := coretui.ToolResult{
		ID:   p.FunctionResponse.ID,
		Name: p.FunctionResponse.Name,
	}
	if p.FunctionResponse.Response == nil {
		return tr
	}
	tr.Response = make(map[string]any, len(p.FunctionResponse.Response))
	for k, v := range p.FunctionResponse.Response {
		tr.Response[k] = v
		if k == "error" {
			if s, ok := v.(string); ok {
				tr.Error = s
			}
		}
	}
	return tr
}

// Subagent projects one persisted turn into a coretui.SubagentEvent
// for the turn-log surfaces (the `/subagents <name>` overlay and the
// inline tail under a running sync subagent's tool row). Reports false
// for a turn with nothing to render, which callers drop.
//
// Partial events are skipped: a partial is a prefix of the final text
// for the same turn, so keeping both would print the subagent's answer
// twice, growing a character at a time. The parent's chat stream wants
// partials — that's what makes it stream — but a turn LOG wants the
// settled text.
func Subagent(seq int64, ev *session.Event) (coretui.SubagentEvent, bool) {
	if ev == nil || ev.Partial {
		return coretui.SubagentEvent{}, false
	}
	out := coretui.SubagentEvent{
		Seq:       seq,
		Timestamp: ev.Timestamp,
		Author:    ev.Author,
	}
	if ev.Content != nil {
		var text strings.Builder
		for _, p := range ev.Content.Parts {
			if p == nil {
				continue
			}
			if p.Text != "" {
				text.WriteString(p.Text)
			}
			if p.FunctionCall != nil {
				// The two shapes are field-identical today; the
				// conversion is what makes a future divergence a
				// compile error instead of a silently dropped field.
				out.ToolCalls = append(out.ToolCalls, coretui.SubagentToolCall(ToolCall(p)))
			}
			if p.FunctionResponse != nil {
				tr := ToolResult(p)
				out.ToolResults = append(out.ToolResults, coretui.SubagentToolResult{
					ID: tr.ID, Name: tr.Name, Response: tr.Response, Error: tr.Error,
				})
			}
		}
		out.Text = text.String()
	}
	if out.Text == "" && len(out.ToolCalls) == 0 && len(out.ToolResults) == 0 {
		return coretui.SubagentEvent{}, false
	}
	return out, true
}
