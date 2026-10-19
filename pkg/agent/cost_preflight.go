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
	"log"
	"sync"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/core-agent/v3/pkg/usage"
)

type costPreflightContextKey struct{}

// preflightOwner identifies the agent whose model a request comes from,
// by pointer identity. The byte keeps two owners at two addresses: Go may
// give distinct zero-size values the same one.
type preflightOwner struct{ _ byte }

// preflightCall is one model request as the cost checks screening it see
// it. A check installed by a parent agent also screens its subagents'
// requests through the context chain, so the requester is not always the
// agent that installed the check.
type preflightCall struct {
	req *adkmodel.LLMRequest
	// backend is the requesting agent's billing backend; a subagent on
	// another provider bills there, not on the checking agent's backend.
	backend string
	// owner is the requesting agent. A check may use its own measured
	// conversation only for its own requests: a subagent's request
	// carries the subagent's conversation, not the parent's. nil from a
	// wrapper built outside agent.New.
	owner *preflightOwner
}

// preflightRelease ends a passed check's reservation. produced reports
// whether the request yielded a response carrying usage. Only then will
// that usage reach a tracker and settle the estimate a check keeps for
// it, so a request that errored, was refused by a later check, or
// produced nothing must not leave an estimate behind for usage that will
// never arrive.
type preflightRelease func(produced bool)

// costPreflightCheck screens one model request before it is sent.
type costPreflightCheck func(call preflightCall) (preflightRelease, error)

func withCostPreflight(ctx context.Context, check costPreflightCheck) context.Context {
	if check == nil {
		return ctx
	}
	if parent := costPreflightFromContext(ctx); parent != nil {
		child := check
		check = func(call preflightCall) (preflightRelease, error) {
			parentRelease, err := parent(call)
			if err != nil {
				return nil, err
			}
			childRelease, err := child(call)
			if err != nil {
				// The request is never sent, so nothing will settle
				// the parent's estimate.
				if parentRelease != nil {
					parentRelease(false)
				}
				return nil, err
			}
			return func(produced bool) {
				if childRelease != nil {
					childRelease(produced)
				}
				if parentRelease != nil {
					parentRelease(produced)
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
//
// tracker and history must describe the conversation this request
// continues. A check screening another agent's request passes neither:
// that request carries its own, usually much smaller, context, and
// pricing it at the checking agent's window refused a 25-token request
// after a 400K-token turn.
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

// estimateInputCost prices a request's estimated input. Callers check
// price.Unpriced first: an unpriced model is not screened at all (see
// warnUnpricedOnce), because a refusal per request would trip the turn
// for a configuration problem no reset can fix.
func estimateInputCost(req *adkmodel.LLMRequest, model string, tracker *usage.Tracker, history *requestUsageHistory, price usage.Pricing) (float64, error) {
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

// warnUnpricedOnce logs, once per model and backend for this agent, that
// a cost ceiling cannot screen a model nobody prices.
//
// The ceiling checks skip such a request rather than refuse it. Pricing
// is a property of the configuration, not of a turn, so the place to
// stop it is where the operator configured it: startup refuses an
// operator-set ceiling over an unpriced model, and so do /model and a
// runtime spawn. A model that still arrives here got past those doors.
// Refusing it per request, as this check first did, tripped the turn at
// $0 spent and advanced the guardrail streak, and three such turns
// halted a session that no reset could revive while the model stayed
// unpriced. It never counts toward the streak.
func (a *Agent) warnUnpricedOnce(model, backend string) {
	if a == nil {
		return
	}
	key := backend + "/" + model
	a.mu.Lock()
	if a.unpricedWarned[key] {
		a.mu.Unlock()
		return
	}
	if a.unpricedWarned == nil {
		a.unpricedWarned = make(map[string]bool)
	}
	a.unpricedWarned[key] = true
	a.mu.Unlock()
	log.Printf("agent:%s %s", a.logSessionSuffix(), unpricedCeilingWarning(model, backend))
}

// unpricedCeilingWarning is the runtime half of the unpriced-model
// message. It names the same remedies as the startup refusal in
// cmd/core-agent (checkCeilingPricing), so an operator sees one story.
func unpricedCeilingWarning(model, backend string) string {
	where := ""
	if backend != "" {
		where = fmt.Sprintf(" on backend %q", backend)
	}
	return fmt.Sprintf("cost ceiling: model %q%s has no price, so the ceiling cannot screen its requests and counts its calls as $0. "+
		"Declare models[].rates for it (zero rates price a self-hosted model at $0), set model.pricing, or drop the ceiling",
		model, where)
}
