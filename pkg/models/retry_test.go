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
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// The shared helper matches on the predicate, never on text, so this
// sentinel does not need to look like any provider's wire format —
// the two real predicates are tested in their own packages against
// the strings their providers actually emit.
var errTransient = errors.New("transient provider rejection")

func isErrTransient(err error) bool { return errors.Is(err, errTransient) }

// text builds a response carrying usable content.
func text(s string) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: s}},
		},
	}
}

// empty builds a response with no usable content — the heartbeat shape.
func empty() *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel}}
}

type step struct {
	resp *model.LLMResponse
	err  error
}

// scripted returns a factory whose Nth invocation yields the Nth
// script entry, and counts invocations.
func scripted(script ...[]step) (func() iter.Seq2[*model.LLMResponse, error], *int) {
	calls := 0
	return func() iter.Seq2[*model.LLMResponse, error] {
		i := calls
		calls++
		return func(yield func(*model.LLMResponse, error) bool) {
			if i >= len(script) {
				return
			}
			for _, s := range script[i] {
				if !yield(s.resp, s.err) {
					return
				}
			}
		}
	}, &calls
}

// drain collects everything the wrapped iterator produces.
func drain(seq iter.Seq2[*model.LLMResponse, error]) (texts []string, errs []error) {
	for resp, err := range seq {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if resp != nil && resp.Content != nil && len(resp.Content.Parts) > 0 {
			texts = append(texts, resp.Content.Parts[0].Text)
		}
	}
	return texts, errs
}

// testPolicy is a policy with the real clock replaced, so no test
// sleeps and the cooldown is driven explicitly.
func testPolicy(t *testing.T) (*RetryPolicy, *[]string, *time.Time) {
	t.Helper()
	var logs []string
	clock := time.Unix(1_700_000_000, 0)
	p := &RetryPolicy{
		IsTransient: isErrTransient,
		Backoff:     time.Hour, // never actually waited; sleep is stubbed
		Log:         func(f string, a ...any) { logs = append(logs, f) },
		now:         func() time.Time { return clock },
		sleep:       func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil },
	}
	return p, &logs, &clock
}

func TestRetryPolicy_RetriesTransientAndRecovers(t *testing.T) {
	p, logs, _ := testPolicy(t)
	fn, calls := scripted(
		[]step{{nil, errTransient}},
		[]step{{text("recovered"), nil}},
	)

	texts, errs := drain(p.Wrap(context.Background(), fn))

	if *calls != 2 {
		t.Errorf("underlying calls = %d, want 2", *calls)
	}
	if len(errs) != 0 {
		t.Errorf("caller saw errors %v, want none — the retry succeeded", errs)
	}
	if len(texts) != 1 || texts[0] != "recovered" {
		t.Errorf("texts = %v, want [recovered]", texts)
	}
	if !strings.Contains(strings.Join(*logs, "|"), "retrying once") {
		t.Errorf("no retry line in log: %v", *logs)
	}
}

func TestRetryPolicy_OneRetryOnly(t *testing.T) {
	p, _, _ := testPolicy(t)
	fn, calls := scripted(
		[]step{{nil, errTransient}},
		[]step{{nil, errTransient}},
		[]step{{text("never reached"), nil}},
	)

	texts, errs := drain(p.Wrap(context.Background(), fn))

	if *calls != 2 {
		t.Errorf("underlying calls = %d, want exactly 2 — one retry, never two", *calls)
	}
	if len(texts) != 0 {
		t.Errorf("texts = %v, want none", texts)
	}
	if len(errs) != 1 || !errors.Is(errs[0], errTransient) {
		t.Errorf("errs = %v, want the transient error surfaced exactly once", errs)
	}
}

func TestRetryPolicy_NonTransientNotRetried(t *testing.T) {
	p, _, _ := testPolicy(t)
	boom := errors.New("400 INVALID_ARGUMENT: malformed request")
	fn, calls := scripted([]step{{nil, boom}})

	_, errs := drain(p.Wrap(context.Background(), fn))

	if *calls != 1 {
		t.Errorf("underlying calls = %d, want 1 — a permanent error must not be retried", *calls)
	}
	if len(errs) != 1 || !errors.Is(errs[0], boom) {
		t.Errorf("errs = %v, want the original error unchanged", errs)
	}
}

// Once content has reached the caller it cannot be taken back, so a
// later transient error must surface rather than trigger a replay that
// would duplicate the content already delivered.
func TestRetryPolicy_NoRetryAfterContentDelivered(t *testing.T) {
	p, _, _ := testPolicy(t)
	fn, calls := scripted(
		[]step{{text("first half"), nil}, {nil, errTransient}},
		[]step{{text("would be a duplicate"), nil}},
	)

	texts, errs := drain(p.Wrap(context.Background(), fn))

	if *calls != 1 {
		t.Errorf("underlying calls = %d, want 1 — content had already been yielded", *calls)
	}
	if len(texts) != 1 || texts[0] != "first half" {
		t.Errorf("texts = %v, want exactly the content already delivered", texts)
	}
	if len(errs) != 1 || !errors.Is(errs[0], errTransient) {
		t.Errorf("errs = %v, want the transient error passed through", errs)
	}
}

// Chunks that arrived before the failure belong to the abandoned
// attempt. Replaying them alongside the retry's output would
// double-deliver the turn.
func TestRetryPolicy_DiscardsPartialsFromTheAbandonedAttempt(t *testing.T) {
	p, _, _ := testPolicy(t)
	fn, _ := scripted(
		[]step{{empty(), nil}, {empty(), nil}, {nil, errTransient}},
		[]step{{text("clean"), nil}},
	)

	texts, errs := drain(p.Wrap(context.Background(), fn))

	if len(errs) != 0 {
		t.Errorf("errs = %v, want none", errs)
	}
	if len(texts) != 1 || texts[0] != "clean" {
		t.Errorf("texts = %v, want only the retry's output", texts)
	}
}

// A transient error must not be allowed to launder away a real one:
// the retry discards the buffer, so if a hard error is sitting in it
// the caller would never hear about the thing that actually broke.
func TestRetryPolicy_HardErrorInBufferSuppressesRetry(t *testing.T) {
	p, _, _ := testPolicy(t)
	boom := errors.New("anthropic: accumulate: bad frame")
	fn, calls := scripted(
		[]step{{nil, boom}, {nil, errTransient}},
		[]step{{text("would hide the real error"), nil}},
	)

	texts, errs := drain(p.Wrap(context.Background(), fn))

	if *calls != 1 {
		t.Errorf("underlying calls = %d, want 1", *calls)
	}
	if len(texts) != 0 {
		t.Errorf("texts = %v, want none", texts)
	}
	if len(errs) != 2 || !errors.Is(errs[0], boom) || !errors.Is(errs[1], errTransient) {
		t.Errorf("errs = %v, want the hard error first and the transient one after", errs)
	}
}

// The cooldown is the whole rate-limit guard: under a sustained shed
// the first call retries and every later one degrades to pass-through,
// so added load stays at one request per window however many calls are
// failing.
func TestRetryPolicy_CooldownSuppressesTheSecondRetry(t *testing.T) {
	p, logs, clock := testPolicy(t)
	p.Cooldown = 30 * time.Second

	fnA, callsA := scripted([]step{{nil, errTransient}}, []step{{nil, errTransient}})
	if _, errs := drain(p.Wrap(context.Background(), fnA)); len(errs) != 1 {
		t.Fatalf("first call errs = %v, want 1", errs)
	}
	if *callsA != 2 {
		t.Fatalf("first call made %d requests, want 2 — it should have retried", *callsA)
	}

	// 10s later, still inside the cooldown.
	*clock = clock.Add(10 * time.Second)
	fnB, callsB := scripted([]step{{nil, errTransient}}, []step{{text("unreached"), nil}})
	if _, errs := drain(p.Wrap(context.Background(), fnB)); len(errs) != 1 {
		t.Fatalf("second call errs = %v, want 1", errs)
	}
	if *callsB != 1 {
		t.Errorf("second call made %d requests, want 1 — the cooldown should have suppressed the retry", *callsB)
	}
	if !strings.Contains(strings.Join(*logs, "|"), "NOT retried") {
		t.Errorf("suppression not logged: %v", *logs)
	}

	// Past the window, retrying is allowed again.
	*clock = clock.Add(31 * time.Second)
	fnC, callsC := scripted([]step{{nil, errTransient}}, []step{{text("ok"), nil}})
	texts, errs := drain(p.Wrap(context.Background(), fnC))
	if *callsC != 2 {
		t.Errorf("third call made %d requests, want 2 — the cooldown had elapsed", *callsC)
	}
	if len(errs) != 0 || len(texts) != 1 {
		t.Errorf("third call texts=%v errs=%v, want one text and no error", texts, errs)
	}
}

func TestRetryPolicy_NilPolicyAndNilPredicateArePassThrough(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    *RetryPolicy
	}{
		{"nil policy", nil},
		{"nil predicate", &RetryPolicy{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fn, calls := scripted(
				[]step{{nil, errTransient}},
				[]step{{text("unreached"), nil}},
			)
			_, errs := drain(tc.p.Wrap(context.Background(), fn))
			if *calls != 1 {
				t.Errorf("underlying calls = %d, want 1", *calls)
			}
			if len(errs) != 1 || !errors.Is(errs[0], errTransient) {
				t.Errorf("errs = %v, want the error passed straight through", errs)
			}
		})
	}
}

// A cancelled context must not be answered with a context error in
// place of the provider's: the rejection is the diagnostic the operator
// needs, and ctx.Err() is available to the caller anyway.
func TestRetryPolicy_ContextCancelledDuringBackoffSurfacesTheProviderError(t *testing.T) {
	p, _, _ := testPolicy(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fn, calls := scripted([]step{{nil, errTransient}}, []step{{text("unreached"), nil}})
	texts, errs := drain(p.Wrap(ctx, fn))

	if *calls != 1 {
		t.Errorf("underlying calls = %d, want 1 — the backoff was cut short", *calls)
	}
	if len(texts) != 0 {
		t.Errorf("texts = %v, want none", texts)
	}
	if len(errs) != 1 || !errors.Is(errs[0], errTransient) {
		t.Errorf("errs = %v, want the provider error, not a context error", errs)
	}
}

// A consumer that stops early must stop the wrapper too — including
// while the buffer is being flushed.
func TestRetryPolicy_ConsumerStopIsHonoured(t *testing.T) {
	p, _, _ := testPolicy(t)
	fn, _ := scripted([]step{{text("one"), nil}, {text("two"), nil}, {text("three"), nil}})

	var got []string
	for resp, err := range p.Wrap(context.Background(), fn) {
		if err == nil && resp != nil && len(resp.Content.Parts) > 0 {
			got = append(got, resp.Content.Parts[0].Text)
		}
		if len(got) == 2 {
			break
		}
	}
	if len(got) != 2 {
		t.Errorf("got %v, want to have stopped after two", got)
	}
}

// The cooldown is shared state; a daemon runs a parent and its
// subagents against one policy concurrently.
func TestRetryPolicy_ConcurrentUse(t *testing.T) {
	p := &RetryPolicy{
		IsTransient: isErrTransient,
		Cooldown:    time.Millisecond,
		sleep:       func(context.Context, time.Duration) bool { return true },
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn, _ := scripted([]step{{nil, errTransient}}, []step{{text("ok"), nil}})
			drain(p.Wrap(context.Background(), fn))
		}()
	}
	wg.Wait()
}
