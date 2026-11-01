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

package harness

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"

	"google.golang.org/adk/v2/model"

	"github.com/go-steer/core-agent/v3/internal/evals"
	"github.com/go-steer/core-agent/v3/internal/evals/evalcompose"
)

// TokenUsage is what a model reported over a run, summed from the
// usage metadata on each complete response.
type TokenUsage struct {
	Calls        int64 `json:"calls"`
	PromptTokens int64 `json:"prompt_tokens"`
	CachedTokens int64 `json:"cached_tokens"`
	// CacheWriteTokens is the input a provider reported writing to its
	// prompt cache (Anthropic's cache_creation_input_tokens), read from
	// the usage sidecar: genai's usage struct has no field for it.
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	// CacheWrite1hTokens is the share of CacheWriteTokens written with a
	// 1-hour breakpoint, billed at its own rate.
	CacheWrite1hTokens int64 `json:"cache_write_1h_tokens,omitempty"`
	OutputTokens       int64 `json:"output_tokens"`
	ThoughtsTokens     int64 `json:"thoughts_tokens"`
}

// usageCounter sums the usage a model reports. It counts complete
// responses only: a streamed call's partials carry no usage, and a
// call the retry wrapper absorbed never produced one.
type usageCounter struct {
	inner model.LLM
	mu    sync.Mutex
	u     TokenUsage
}

func (c *usageCounter) Name() string { return c.inner.Name() }

func (c *usageCounter) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for resp, err := range c.inner.GenerateContent(ctx, req, stream) {
			if err == nil && resp != nil && !resp.Partial {
				c.add(resp)
			}
			if !yield(resp, err) {
				return
			}
		}
	}
}

func (c *usageCounter) add(resp *model.LLMResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.u.Calls++
	if m := resp.UsageMetadata; m != nil {
		c.u.PromptTokens += int64(m.PromptTokenCount)
		c.u.CachedTokens += int64(m.CachedContentTokenCount)
		c.u.OutputTokens += int64(m.CandidatesTokenCount)
		c.u.ThoughtsTokens += int64(m.ThoughtsTokenCount)
	}
	if d, ok := resp.CustomMetadata[evalcompose.DetailKey].(evalcompose.Detailer); ok && d != nil {
		_, cw, cw1h, _, _ := d.UsageDetail()
		if cw != nil {
			c.u.CacheWriteTokens += *cw
		}
		if cw1h != nil {
			c.u.CacheWrite1hTokens += *cw1h
		}
	}
}

func (c *usageCounter) snapshot() *TokenUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.u
	return &u
}

// selectRows narrows ds to the named scenarios, in corpus order. An id
// the corpus does not have is an error, not an empty run.
func selectRows(ds evals.Dataset, ids []string) (evals.Dataset, error) {
	if len(ids) == 0 {
		return ds, nil
	}
	known := map[string]bool{}
	for _, sc := range ds.Scenarios {
		known[sc.ID] = true
	}
	var missing []string
	for _, id := range ids {
		if !known[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return evals.Dataset{}, fmt.Errorf("harness: --rows names %s, which the corpus does not have", strings.Join(missing, ", "))
	}
	out := ds
	out.Scenarios = slices.DeleteFunc(slices.Clone(ds.Scenarios), func(sc evals.Scenario) bool {
		return !slices.Contains(ids, sc.ID)
	})
	return out, nil
}
