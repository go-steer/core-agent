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
	"context"
	"fmt"
	"iter"

	"github.com/anthropics/anthropic-sdk-go"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// llm implements google.golang.org/adk/model.LLM for Anthropic Claude.
// One llm corresponds to one model ID; the Provider mints a fresh
// instance per Model() call.
type llm struct {
	client      anthropic.Client
	modelID     string
	cacheSystem bool
	builtins    BuiltinTools
}

// Name reports the model ID — used by ADK telemetry and the runner.
func (l *llm) Name() string { return l.modelID }

// GenerateContent implements model.LLM. The returned iterator yields
// streaming partial-text events (Partial: true) followed by exactly
// one terminal event (TurnComplete: true) carrying the full content,
// usage, and mapped FinishReason. Errors are yielded inline and stop
// the iteration.
func (l *llm) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		params, err := buildParams(req.Model, req.Contents, req.Config, l.cacheSystem, l.builtins)
		if err != nil {
			yield(nil, fmt.Errorf("anthropic: build request: %w", err))
			return
		}
		// Use the Provider-bound model when LLMRequest didn't carry
		// one; the Provider's modelID came from cfg.Model.Name.
		if req.Model == "" {
			params.Model = l.modelID
		}

		stream := l.client.Messages.NewStreaming(ctx, params)
		final := anthropic.Message{}

		for stream.Next() {
			ev := stream.Current()
			if err := final.Accumulate(ev); err != nil {
				yield(nil, fmt.Errorf("anthropic: accumulate: %w", err))
				return
			}
			if delta, ok := textDelta(ev); ok {
				partial := &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{{Text: delta}},
					},
					Partial: true,
				}
				if !yield(partial, nil) {
					return
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield(nil, fmt.Errorf("anthropic: stream: %w", err))
			return
		}

		content, finish, usage := finalResponseFromMessage(&final)
		yield(&adkmodel.LLMResponse{
			Content:       content,
			UsageMetadata: usage,
			FinishReason:  finish,
			TurnComplete:  true,
		}, nil)
	}
}

// textDelta extracts incremental assistant text from a stream event.
// Returns ("", false) for everything other than a content_block_delta
// carrying a TextDelta — tool-use input deltas, message-stop events,
// and so on are accumulated by Message.Accumulate but not surfaced as
// partials.
func textDelta(ev anthropic.MessageStreamEventUnion) (string, bool) {
	delta, ok := ev.AsAny().(anthropic.ContentBlockDeltaEvent)
	if !ok {
		return "", false
	}
	td, ok := delta.Delta.AsAny().(anthropic.TextDelta)
	if !ok {
		return "", false
	}
	return td.Text, td.Text != ""
}
