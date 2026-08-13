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
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
)

// fakeLLM lets a test decide, per call, what the wrapped model does.
type fakeLLM struct {
	mu    sync.Mutex
	calls int
	fn    func(call int, ctx context.Context, yield func(*adkmodel.LLMResponse, error) bool)
}

func (f *fakeLLM) Name() string { return "fake" }

func (f *fakeLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		f.mu.Lock()
		f.calls++
		call := f.calls
		f.mu.Unlock()
		f.fn(call, ctx, yield)
	}
}

func (f *fakeLLM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// drain consumes the whole sequence, returning how many responses
// arrived and the terminal error, if any.
func drain(seq iter.Seq2[*adkmodel.LLMResponse, error]) (int, error) {
	var n int
	for resp, err := range seq {
		if err != nil {
			return n, err
		}
		_ = resp
		n++
	}
	return n, nil
}

// countingSleep records backoff calls instead of waiting.
func countingSleep(n *int) func(context.Context, time.Duration) error {
	return func(context.Context, time.Duration) error {
		*n++
		return nil
	}
}

func okResponse() *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{TurnComplete: true}
}

func TestRetryRecoversFromTransientError(t *testing.T) {
	inner := &fakeLLM{fn: func(call int, _ context.Context, yield func(*adkmodel.LLMResponse, error) bool) {
		if call == 1 {
			yield(nil, errors.New("googleapi: Error 429: quota exceeded, RESOURCE_EXHAUSTED"))
			return
		}
		yield(okResponse(), nil)
	}}
	var slept []time.Duration
	r := withRetry(inner, withRetryAttempts(3), withRetryPerAttempt(0),
		withRetryBackoff(90*time.Second),
		withRetrySleep(func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}))

	n, err := drain(r.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if n != 1 {
		t.Errorf("delivered %d responses, want 1", n)
	}
	if inner.count() != 2 {
		t.Errorf("inner called %d times, want 2", inner.count())
	}
	if len(slept) != 1 || slept[0] != 90*time.Second {
		t.Errorf("backoff = %v, want one 90s wait", slept)
	}
}

// TestRetryDefaultsMatchUpstream pins the knobs bouncer's monkeypatch
// hardcodes; a quieter default would change failure behaviour on a
// quota-limited project without anyone noticing.
func TestRetryDefaultsMatchUpstream(t *testing.T) {
	r := withRetry(&fakeLLM{})
	if r.attempts != 10 || r.backoff != 60*time.Second {
		t.Errorf("defaults = %d attempts / %s backoff, want 10 / 60s", r.attempts, r.backoff)
	}
}

func TestRetryGivesUpAfterAttempts(t *testing.T) {
	inner := &fakeLLM{fn: func(_ int, _ context.Context, yield func(*adkmodel.LLMResponse, error) bool) {
		yield(nil, errors.New("503 Service Unavailable"))
	}}
	var slept int
	r := withRetry(inner, withRetryAttempts(3), withRetrySleep(countingSleep(&slept)), withRetryPerAttempt(0))

	if _, err := drain(r.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false)); err == nil {
		t.Fatal("expected the final error to surface")
	}
	if inner.count() != 3 {
		t.Errorf("inner called %d times, want 3 (attempts)", inner.count())
	}
	if slept != 2 {
		t.Errorf("slept %d times, want 2 (one fewer than attempts)", slept)
	}
}

func TestRetryIgnoresPermanentError(t *testing.T) {
	inner := &fakeLLM{fn: func(_ int, _ context.Context, yield func(*adkmodel.LLMResponse, error) bool) {
		yield(nil, errors.New("400 INVALID_ARGUMENT: model does not exist"))
	}}
	var slept int
	r := withRetry(inner, withRetryAttempts(5), withRetrySleep(countingSleep(&slept)), withRetryPerAttempt(0))

	if _, err := drain(r.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false)); err == nil {
		t.Fatal("expected the error to surface")
	}
	if inner.count() != 1 {
		t.Errorf("inner called %d times, want 1: a 400 must not be retried", inner.count())
	}
	if slept != 0 {
		t.Errorf("slept %d times, want 0", slept)
	}
}

// TestRetryDoesNotReplayDeliveredResponses pins the one semantic
// difference from bouncer's HTTP-level monkeypatch: once a chunk has
// reached the consumer, retrying would duplicate it in the
// transcript, so the error is surfaced instead.
func TestRetryDoesNotReplayDeliveredResponses(t *testing.T) {
	inner := &fakeLLM{fn: func(_ int, _ context.Context, yield func(*adkmodel.LLMResponse, error) bool) {
		if !yield(okResponse(), nil) {
			return
		}
		yield(nil, errors.New("503 Service Unavailable mid-stream"))
	}}
	var slept int
	r := withRetry(inner, withRetryAttempts(5), withRetrySleep(countingSleep(&slept)), withRetryPerAttempt(0))

	n, err := drain(r.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false))
	if err == nil {
		t.Fatal("expected the mid-stream error to surface")
	}
	if n != 1 {
		t.Errorf("delivered %d responses, want the 1 that arrived before the failure", n)
	}
	if inner.count() != 1 {
		t.Errorf("inner called %d times, want 1: a partially delivered stream must not be replayed", inner.count())
	}
}

func TestRetryPerAttemptTimeout(t *testing.T) {
	inner := &fakeLLM{fn: func(call int, ctx context.Context, yield func(*adkmodel.LLMResponse, error) bool) {
		if call == 1 {
			<-ctx.Done() // hang until the per-attempt deadline fires
			yield(nil, ctx.Err())
			return
		}
		yield(okResponse(), nil)
	}}
	var slept int
	r := withRetry(inner, withRetryAttempts(2), withRetrySleep(countingSleep(&slept)),
		withRetryPerAttempt(20*time.Millisecond))

	n, err := drain(r.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if n != 1 || inner.count() != 2 {
		t.Errorf("delivered=%d calls=%d, want 1 response after 2 calls", n, inner.count())
	}
}

func TestRetryStopsWhenConsumerBreaks(t *testing.T) {
	inner := &fakeLLM{fn: func(_ int, _ context.Context, yield func(*adkmodel.LLMResponse, error) bool) {
		for range 3 {
			if !yield(okResponse(), nil) {
				return
			}
		}
	}}
	r := withRetry(inner, withRetryAttempts(3), withRetryPerAttempt(0))

	var seen int
	for range r.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
		seen++
		break
	}
	if seen != 1 {
		t.Errorf("saw %d responses, want 1", seen)
	}
	if inner.count() != 1 {
		t.Errorf("inner called %d times, want 1", inner.count())
	}
}

func TestRetryAbortsBackoffOnCancel(t *testing.T) {
	inner := &fakeLLM{fn: func(_ int, _ context.Context, yield func(*adkmodel.LLMResponse, error) bool) {
		yield(nil, errors.New("429 RESOURCE_EXHAUSTED"))
	}}
	ctx, cancel := context.WithCancel(context.Background())
	r := withRetry(inner, withRetryAttempts(5), withRetryPerAttempt(0),
		withRetrySleep(func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		}))

	_, err := drain(r.GenerateContent(ctx, &adkmodel.LLMRequest{}, false))
	if err == nil {
		t.Fatal("expected an error when the backoff is aborted")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error %q should still carry the original failure", err)
	}
	if inner.count() != 1 {
		t.Errorf("inner called %d times, want 1", inner.count())
	}
}

func TestRetryNotifiesAndForwardsName(t *testing.T) {
	inner := &fakeLLM{fn: func(call int, _ context.Context, yield func(*adkmodel.LLMResponse, error) bool) {
		if call == 1 {
			yield(nil, errors.New("UNAVAILABLE"))
			return
		}
		yield(okResponse(), nil)
	}}
	var notified int
	r := withRetry(inner, withRetryAttempts(2), withRetryPerAttempt(0),
		withRetrySleep(func(context.Context, time.Duration) error { return nil }),
		withRetryNotify(func(int, error) { notified++ }))

	if r.Name() != "fake" {
		t.Errorf("Name() = %q, want the wrapped model's name", r.Name())
	}
	if _, err := drain(r.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false)); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if notified != 1 {
		t.Errorf("notified %d times, want 1", notified)
	}
}

func TestRetryableModelError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429", errors.New("Error 429"), true},
		{"503", errors.New("Error 503"), true},
		{"resource exhausted", errors.New("RESOURCE_EXHAUSTED"), true},
		{"deadline", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
		{"bad request", errors.New("400 INVALID_ARGUMENT"), false},
		{"wrapped 503", errors.Join(errors.New("call failed"), errors.New("503")), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableModelError(tc.err); got != tc.want {
				t.Errorf("retryableModelError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestSleepCtxRespectsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Minute); err == nil {
		t.Fatal("expected sleepCtx to abort on a cancelled context")
	}
	if err := sleepCtx(context.Background(), 0); err != nil {
		t.Errorf("zero backoff should be a no-op, got %v", err)
	}
}
