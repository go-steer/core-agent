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
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/model"
)

// This file is the Go replacement for bouncer's
// `BaseApiClient.async_request` monkeypatch (repeated verbatim in
// generator/, checker/, dreamer/ and lookup/agent.py): ten attempts,
// a 120s per-attempt timeout, and a flat 60s sleep whenever the
// backend answers 429 / 503 / RESOURCE_EXHAUSTED.
//
// In Python that had to be a monkeypatch because the retry point sat
// inside the vendored SDK's HTTP client. ADK's model.LLM is a
// two-method interface, so the same policy is an ordinary decorator
// here — no vendored internals touched, and the wrapped model is a
// plain adkmodel.LLM that agent.New accepts like any other.
//
// The decorator is deliberately transparent: it forwards Name() and
// adds no methods of its own, so nothing downstream that type-asserts
// the model for extra capabilities loses them beyond what the ADK
// interface already exposes.

const (
	defaultRetryAttempts   = 10
	defaultRetryBackoff    = 60 * time.Second
	defaultRetryPerAttempt = 120 * time.Second
)

// retryLLM decorates an adkmodel.LLM with bounded retries on
// transient backend failures.
type retryLLM struct {
	inner adkmodel.LLM

	// attempts is the total number of calls (1 = no retry).
	attempts int
	// backoff is the flat delay between attempts. bouncer uses a
	// flat 60s rather than exponential growth: Gemini quota windows
	// refill on a wall-clock schedule, so backing off further buys
	// nothing.
	backoff time.Duration
	// perAttempt bounds a single GenerateContent call. Zero disables
	// the per-attempt deadline.
	perAttempt time.Duration

	// sleep is injectable so tests don't wait a real minute.
	sleep func(context.Context, time.Duration) error
	// onRetry, when set, is called before each backoff sleep.
	onRetry func(attempt int, err error)
}

// withRetry wraps m with the bouncer retry policy. opts are applied
// after the defaults.
func withRetry(m adkmodel.LLM, opts ...retryOption) *retryLLM {
	r := &retryLLM{
		inner:      m,
		attempts:   defaultRetryAttempts,
		backoff:    defaultRetryBackoff,
		perAttempt: defaultRetryPerAttempt,
		sleep:      sleepCtx,
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.attempts < 1 {
		r.attempts = 1
	}
	return r
}

type retryOption func(*retryLLM)

func withRetryAttempts(n int) retryOption { return func(r *retryLLM) { r.attempts = n } }

func withRetryBackoff(d time.Duration) retryOption { return func(r *retryLLM) { r.backoff = d } }

func withRetryPerAttempt(d time.Duration) retryOption {
	return func(r *retryLLM) { r.perAttempt = d }
}

func withRetrySleep(f func(context.Context, time.Duration) error) retryOption {
	return func(r *retryLLM) { r.sleep = f }
}

func withRetryNotify(f func(attempt int, err error)) retryOption {
	return func(r *retryLLM) { r.onRetry = f }
}

// Name reports the wrapped model's name unchanged.
func (r *retryLLM) Name() string { return r.inner.Name() }

// GenerateContent forwards to the wrapped model, retrying the whole
// call on transient failures.
//
// IMPORTANT semantic difference from bouncer's monkeypatch: that one
// retries inside the HTTP client, below the streaming boundary, so a
// retry is invisible to the caller. Here the retry point sits above
// an iterator that may already have handed responses to the consumer.
// Replaying those would duplicate content in the transcript, so once
// any response has been delivered for an attempt the error is
// surfaced rather than retried. In practice 429/503 arrive on the
// first chunk, which is exactly the case this covers.
func (r *retryLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		for attempt := 1; ; attempt++ {
			res := r.one(ctx, req, stream, yield)
			switch {
			case res.consumerStopped:
				return
			case res.err == nil:
				return
			case res.delivered, attempt >= r.attempts, ctx.Err() != nil, !retryableModelError(res.err):
				yield(nil, res.err)
				return
			}
			if r.onRetry != nil {
				r.onRetry(attempt, res.err)
			}
			if err := r.sleep(ctx, r.backoff); err != nil {
				yield(nil, errors.Join(res.err, err))
				return
			}
		}
	}
}

// attemptResult reports what happened during a single inner call.
type attemptResult struct {
	// delivered is true once at least one response reached the
	// consumer — after that a retry would duplicate content.
	delivered bool
	// consumerStopped is true when the consumer broke out of the
	// range loop; the whole sequence must end immediately.
	consumerStopped bool
	// err is the failure the inner model reported, if any.
	err error
}

func (r *retryLLM) one(ctx context.Context, req *adkmodel.LLMRequest, stream bool, yield func(*adkmodel.LLMResponse, error) bool) attemptResult {
	callCtx := ctx
	if r.perAttempt > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, r.perAttempt)
		defer cancel()
	}
	var res attemptResult
	for resp, err := range r.inner.GenerateContent(callCtx, req, stream) {
		if err != nil {
			res.err = err
			return res
		}
		res.delivered = true
		if !yield(resp, nil) {
			res.consumerStopped = true
			return res
		}
	}
	return res
}

// retryableTokens are the substrings that mark a transient backend
// failure. Error text is the only signal the ADK surface offers —
// GenerateContent returns a bare error, not a typed status — so this
// mirrors bouncer's `if "429" in str(e) or "503" in str(e) or
// "RESOURCE_EXHAUSTED" in str(e)`.
var retryableTokens = []string{
	"429",
	"503",
	"RESOURCE_EXHAUSTED",
	"UNAVAILABLE",
	"DEADLINE_EXCEEDED",
	"connection reset",
	"EOF",
}

func retryableModelError(err error) bool {
	if err == nil {
		return false
	}
	// A per-attempt timeout is retryable; a caller-cancelled context
	// is not (the GenerateContent loop above checks ctx.Err()
	// separately, so this only fires for our own deadline).
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	msg := err.Error()
	for _, tok := range retryableTokens {
		if strings.Contains(msg, tok) {
			return true
		}
	}
	return false
}

// sleepCtx waits for d, aborting early if ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("retry backoff aborted: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}
