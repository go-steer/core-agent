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
	"iter"
	"strings"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// echoLLM returns the user's last message as the model response, with
// no tool calls and no streaming chunks. It exists so consumers can
// boot the binary with no credentials and confirm the agent loop
// dispatches a turn end-to-end.
type echoLLM struct{}

func (echoLLM) Name() string { return "echo" }

func (echoLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		text := lastUserText(req.Contents)
		if text == "" {
			text = "(echo: no user input)"
		}
		content := &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: text}},
		}
		// Runner only prints text from Partial events; TurnComplete is
		// the final summary. Mirror that shape — one partial carrying
		// the text, then a TurnComplete with the full content.
		if !yield(&adkmodel.LLMResponse{Content: content, Partial: true}, nil) {
			return
		}
		yield(&adkmodel.LLMResponse{
			Content:      content,
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// lastUserText walks the conversation backward and returns the joined
// text parts of the most recent user message. Returns "" when there
// is no user message or all parts are non-text.
func lastUserText(contents []*genai.Content) string {
	for i := len(contents) - 1; i >= 0; i-- {
		c := contents[i]
		if c == nil || c.Role != genai.RoleUser {
			continue
		}
		var b strings.Builder
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}
