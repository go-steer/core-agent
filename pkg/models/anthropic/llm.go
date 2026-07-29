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
	"log"

	"github.com/anthropics/anthropic-sdk-go"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// maxPauseTurnContinuations bounds how many times GenerateContent
// re-issues a request after a pause_turn stop. pause_turn means the
// server-side tool loop (e.g. web_search) ran long and the API wants
// the turn resubmitted so it can keep working; a well-behaved turn
// finishes within a couple of continuations, so the cap only exists to
// stop a pathological server from spinning us forever.
const maxPauseTurnContinuations = 4

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

		// The turn may span several requests: a long server-side tool
		// run (WithWebSearch) ends its request with stop_reason
		// pause_turn, and the API expects the paused assistant turn
		// replayed verbatim so it can resume. Content parts and usage
		// accumulate across all requests of the turn; exactly one
		// terminal response is yielded at the end.
		var parts []*genai.Part
		var usage anthropic.Usage

		for continuation := 0; ; continuation++ {
			stream := l.client.Messages.NewStreaming(ctx, params)
			final := anthropic.Message{}

			// Drain inside a closure so the deferred Close releases
			// this request's HTTP connection on EVERY exit path —
			// early consumer stop, accumulate/stream error, the
			// pause_turn continue, and the terminal return (#487).
			// Without it the connection stayed open until the
			// caller's ctx was cancelled; in-tree callers cancel
			// per-turn so the leak was bounded, but library consumers
			// with a long-lived ctx that stop mid-iteration leaked
			// one connection per stop — and the continuation loop
			// opens up to 1+maxPauseTurnContinuations streams per
			// call. Returns false when GenerateContent must stop
			// (consumer said stop, or an error was already yielded).
			drained := func() bool {
				defer func() { _ = stream.Close() }()
				for stream.Next() {
					ev := stream.Current()
					if err := final.Accumulate(ev); err != nil {
						yield(nil, fmt.Errorf("anthropic: accumulate: %w", err))
						return false
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
							return false
						}
					}
				}
				if err := stream.Err(); err != nil {
					yield(nil, fmt.Errorf("anthropic: stream: %w", err))
					return false
				}
				return true
			}()
			if !drained {
				return
			}

			parts = append(parts, contentPartsFromMessage(&final)...)
			addUsage(&usage, final.Usage)

			if final.StopReason == anthropic.StopReasonPauseTurn {
				if continuation < maxPauseTurnContinuations {
					// Replay the paused assistant message exactly as
					// received (ToParam preserves server_tool_use /
					// web_search_tool_result blocks the API needs) and
					// re-issue; the server resumes where it left off.
					params.Messages = append(params.Messages, final.ToParam())
					continue
				}
				// Cap reached: surface what we have instead of spinning.
				// mapStopReason turns pause_turn into FinishReasonOther,
				// which is an honest "stopped for a non-standard reason".
				log.Printf("anthropic: pause_turn continuation cap (%d) reached on model %s; yielding accumulated content", maxPauseTurnContinuations, params.Model)
			}

			yield(&adkmodel.LLMResponse{
				Content:       &genai.Content{Role: genai.RoleModel, Parts: parts},
				UsageMetadata: usageMetadata(usage),
				FinishReason:  mapStopReason(final.StopReason),
				TurnComplete:  true,
			}, nil)
			return
		}
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
