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

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/core-agent/v3/pkg/usage"
)

type costPreflightContextKey struct{}

// costPreflightCheck screens one model request before it is sent.
// backend is the billing backend of the agent whose model is making the
// request, which may differ from the agent that installed the check: a
// parent's per-turn ceiling also screens its subagents' requests.
type costPreflightCheck func(req *adkmodel.LLMRequest, backend string) (func(), error)

func withCostPreflight(ctx context.Context, check costPreflightCheck) context.Context {
	if check == nil {
		return ctx
	}
	if parent := costPreflightFromContext(ctx); parent != nil {
		child := check
		check = func(req *adkmodel.LLMRequest, backend string) (func(), error) {
			parentRelease, err := parent(req, backend)
			if err != nil {
				return nil, err
			}
			childRelease, err := child(req, backend)
			if err != nil {
				if parentRelease != nil {
					parentRelease()
				}
				return nil, err
			}
			return func() {
				if childRelease != nil {
					childRelease()
				}
				if parentRelease != nil {
					parentRelease()
				}
			}, nil
		}
	}
	return context.WithValue(ctx, costPreflightContextKey{}, check)
}

func costPreflightFromContext(ctx context.Context) costPreflightCheck {
	if ctx == nil {
		return nil
	}
	check, _ := ctx.Value(costPreflightContextKey{}).(costPreflightCheck)
	return check
}

type costPreflightError struct{ reason string }

func (e *costPreflightError) Error() string { return e.reason }

// requestUsageHistory tracks the most recent measured request and the
// tool results appended since it for a subagent without its own tracker.
type requestUsageHistory struct {
	mu                  sync.Mutex
	model               string
	inputTokens         int
	cachedInputTokens   int
	pendingContextBytes int
	measured            bool
}

func (h *requestUsageHistory) record(model string, u usage.TurnUsage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	u = u.Clamped()
	h.model = model
	h.inputTokens = u.InputTokens
	h.cachedInputTokens = u.CachedInputTokens
	h.pendingContextBytes = 0
	h.measured = h.inputTokens > 0
}

func (h *requestUsageHistory) addPendingContextBytes(n int) {
	if n > 0 {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.pendingContextBytes += n
	}
}

func (h *requestUsageHistory) estimate(model string) (tokens, cached int, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.measured || h.model != model {
		return 0, 0, false
	}
	tokens = h.inputTokens + usage.EstimateTokensFromBytes(h.pendingContextBytes)
	return tokens, h.cachedInputTokens, true
}

// estimateRequestInput returns the larger of the previous measured
// conversation size (plus its unmeasured tail) and a byte-based estimate
// of this request. Observed cache reads are priced at the cache-read rate.
func estimateRequestInput(req *adkmodel.LLMRequest, model string, tracker *usage.Tracker, history *requestUsageHistory) (tokens, cached int, ok bool) {
	if req == nil {
		return 0, 0, false
	}
	requestTokens, ok := estimateSerializedRequestTokens(req)
	if !ok {
		return 0, 0, false
	}
	tokens = requestTokens

	if history != nil {
		historyTokens, historyCached, measured := history.estimate(model)
		if measured {
			if historyTokens > tokens {
				tokens = historyTokens
			}
			cached = historyCached
		} else if tracker != nil {
			if last, hasLast := tracker.Last(); hasLast && last.Model == model {
				historyTokens, _ := tracker.ContextWindowUsedEstimated()
				if historyTokens > tokens {
					tokens = historyTokens
				}
				cached = last.CachedInputTokens
			}
		}
	} else if tracker != nil {
		if last, hasLast := tracker.Last(); hasLast && last.Model == model {
			historyTokens, _ := tracker.ContextWindowUsedEstimated()
			if historyTokens > tokens {
				tokens = historyTokens
			}
			cached = last.CachedInputTokens
		}
	}
	if cached > tokens {
		cached = tokens
	}
	return tokens, cached, true
}

func estimateSerializedRequestTokens(req *adkmodel.LLMRequest) (int, bool) {
	encoded, err := json.Marshal(struct {
		Contents any `json:"contents,omitempty"`
		Config   any `json:"config,omitempty"`
	}{
		Contents: req.Contents,
		Config:   req.Config,
	})
	if err != nil {
		return 0, false
	}
	if len(encoded) == 0 {
		return 0, false
	}
	return usage.EstimateTokensFromBytes(len(encoded)), true
}

func estimateInputCost(req *adkmodel.LLMRequest, model string, tracker *usage.Tracker, history *requestUsageHistory, price usage.Pricing) (float64, error) {
	if price.Unpriced {
		return 0, &costPreflightError{reason: fmt.Sprintf("model %q has no price; refusing the request while a cost budget is active", model)}
	}
	tokens, cached, ok := estimateRequestInput(req, model, tracker, history)
	if !ok {
		return 0, &costPreflightError{reason: fmt.Sprintf("could not estimate input tokens for model %q; refusing the request while a cost budget is active", model)}
	}
	return price.CostUSDWithCache(tokens-cached, cached, 0), nil
}

func isCostPreflightError(err error) bool {
	var target *costPreflightError
	return errors.As(err, &target)
}
