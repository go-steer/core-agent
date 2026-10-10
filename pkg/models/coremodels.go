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

package models

import (
	"context"
	"iter"

	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-models/adkv1"
	"github.com/go-steer/core-models/llm"
	coreusage "github.com/go-steer/core-models/usage"

	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// Adapt turns a core-models model into the ADK v1 model.LLM core-agent
// runs, for every backend built on the library: the provider profiles,
// and the gemini, vertex, anthropic and anthropic-vertex facades
// (docs/model-support-design.md).
//
// Beyond core-models' adkv1 shim it does three things core-agent needs:
//
//   - Copies the usage record's cache-write counts into the
//     CustomMetadata sidecar keys pkg/usage reads
//     (usage.CacheCreationTokensMetadataKey and the 1-hour share's
//     key), so cost accounting and usage.Rebuild over old event logs see
//     what the library reports. The genai usage fields the library
//     already fills (prompt, completion, cache reads, reasoning) pass
//     through untouched.
//   - Keeps the WithoutBuiltins unwrap: a library model that can drop
//     its server-side built-ins (Gemini's) comes back with a
//     WithoutBuiltins() model.LLM method, the duck type RunSubtask looks
//     for, so a subtask still drives the model with exactly its own
//     tools.
//   - Marks one event per streamed call TurnComplete, as ADK v1's
//     Gemini model does, so usage.TurnTap counts the call once.
func Adapt(m llm.LLM) adkmodel.LLM {
	a := adapted{inner: adkv1.Wrap(m)}
	if wb, ok := m.(interface{ WithoutBuiltins() llm.LLM }); ok {
		return adaptedWithBuiltins{adapted: a, lib: wb}
	}
	return a
}

type adapted struct {
	inner adkmodel.LLM
}

func (a adapted) Name() string { return a.inner.Name() }

func (a adapted) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		partialCompleted := false
		for resp, err := range a.inner.GenerateContent(ctx, req, stream) {
			if resp != nil {
				BridgeUsage(resp)
				// One TurnComplete per streamed call, on the last partial,
				// as ADK v1's own Gemini model yields it. core-models'
				// streams also mark the aggregate that follows, and
				// usage.TurnTap commits a turn's usage on every
				// TurnComplete event, so the call would be counted twice.
				if resp.Partial && resp.TurnComplete {
					partialCompleted = true
				} else if !resp.Partial && partialCompleted {
					resp.TurnComplete = false
				}
			}
			if !yield(resp, err) {
				return
			}
		}
	}
}

type adaptedWithBuiltins struct {
	adapted
	lib interface{ WithoutBuiltins() llm.LLM }
}

// WithoutBuiltins returns the model without its server-side built-ins.
func (a adaptedWithBuiltins) WithoutBuiltins() adkmodel.LLM { return Adapt(a.lib.WithoutBuiltins()) }

// BridgeUsage copies the parts of core-models' usage record that
// genai's UsageMetadata cannot hold into the CustomMetadata sidecar
// pkg/usage reads: the cache-write bucket, and the 1-hour share of it
// that bills at 2x rather than 1.25x (#770). ToolUseTokens is
// deliberately not mapped: genai's field for it sits outside the prompt
// count, so guessing a mapping risks counting tokens twice.
//
// A count the library reports as zero writes nothing, matching what
// the pre-library Anthropic adapter wrote; pkg/usage reads an absent
// key as zero.
func BridgeUsage(resp *adkmodel.LLMResponse) {
	d, ok := coreusage.FromMetadata(resp.CustomMetadata)
	if !ok {
		return
	}
	if d.CacheWriteTokens != nil && *d.CacheWriteTokens > 0 {
		resp.CustomMetadata[usage.CacheCreationTokensMetadataKey] = *d.CacheWriteTokens
	}
	if d.CacheWrite1hTokens != nil && *d.CacheWrite1hTokens > 0 {
		resp.CustomMetadata[usage.CacheCreation1hTokensMetadataKey] = *d.CacheWrite1hTokens
	}
}
