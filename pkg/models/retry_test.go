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

// oneFailingCall drives a whole wrapped call that fails transiently on
// every attempt, and reports how many underlying requests it made — 2
// if the retry fired, 1 if the budget suppressed it.
func oneFailingCall(t *testing.T, p *RetryPolicy) int {
	t.Helper()
	fn, calls := scripted([]step{{nil, errTransient}}, []step{{nil, errTransient}})
	if _, errs := drain(p.Wrap(context.Background(), fn)); len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly the transient error", errs)
	}
	return *calls
}

// A correlated shed is the case the single-timestamp guard got wrong
// (#1039): several callers are rejected within seconds of each other,
// and handing the one rescue in the window to whichever was rejected
// first abandons the rest. On a live cluster that killed a delegation
// six seconds after a parent's 429 had spent the window. The budget
// holds RetryBurst retries, so the whole correlated burst is covered.
func TestRetryPolicy_BudgetCoversACorrelatedBurst(t *testing.T) {
	p, _, clock := testPolicy(t)
	p.Cooldown = 30 * time.Second

	for i := 1; i <= RetryBurst; i++ {
		*clock = clock.Add(2 * time.Second) // well inside one window
		if got := oneFailingCall(t, p); got != 2 {
			t.Fatalf("call %d of the burst made %d requests, want 2 — it is inside the burst and must retry", i, got)
		}
	}
}

// The burst is a burst, not an exemption: past it a sustained shed
// degrades to plain pass-through, which is what keeps a retry from
// doubling traffic against a provider that is already shedding.
func TestRetryPolicy_BudgetSuppressesPastTheBurst(t *testing.T) {
	p, logs, clock := testPolicy(t)
	p.Cooldown = 30 * time.Second

	for i := 1; i <= RetryBurst; i++ {
		*clock = clock.Add(time.Second)
		if got := oneFailingCall(t, p); got != 2 {
			t.Fatalf("call %d of the burst made %d requests, want 2", i, got)
		}
	}
	for i := 1; i <= 3; i++ {
		*clock = clock.Add(time.Second)
		if got := oneFailingCall(t, p); got != 1 {
			t.Errorf("call %d past the burst made %d requests, want 1 — the budget is spent", i, got)
		}
	}
	if !strings.Contains(strings.Join(*logs, "|"), "NOT retried") {
		t.Errorf("suppression not logged: %v", *logs)
	}
}

// Steady state is unchanged from the guard this replaced: one extra
// request per cooldown, however many calls are failing. Only the burst
// is new.
func TestRetryPolicy_BudgetRefillsAtOnePerCooldown(t *testing.T) {
	p, _, clock := testPolicy(t)
	p.Cooldown = 30 * time.Second

	for i := 1; i <= RetryBurst; i++ {
		if got := oneFailingCall(t, p); got != 2 {
			t.Fatalf("call %d of the burst made %d requests, want 2", i, got)
		}
	}
	if got := oneFailingCall(t, p); got != 1 {
		t.Fatalf("the call past the burst made %d requests, want 1", got)
	}

	*clock = clock.Add(30 * time.Second)
	if got := oneFailingCall(t, p); got != 2 {
		t.Errorf("after one cooldown the call made %d requests, want 2 — a token had refilled", got)
	}
	if got := oneFailingCall(t, p); got != 1 {
		t.Errorf("the very next call made %d requests, want 1 — one cooldown buys exactly one retry", got)
	}
}

// An idle policy must not bank an unbounded rescue allowance: the
// bucket caps at RetryBurst however long nothing has failed.
func TestRetryPolicy_BudgetCapsAtTheBurst(t *testing.T) {
	p, _, clock := testPolicy(t)
	p.Cooldown = 30 * time.Second

	if got := oneFailingCall(t, p); got != 2 { // seeds the bucket
		t.Fatalf("first call made %d requests, want 2", got)
	}
	*clock = clock.Add(time.Hour) // 120 windows of idle

	for i := 1; i <= RetryBurst; i++ {
		if got := oneFailingCall(t, p); got != 2 {
			t.Fatalf("call %d after the idle made %d requests, want 2", i, got)
		}
	}
	if got := oneFailingCall(t, p); got != 1 {
		t.Errorf("call %d after the idle made %d requests, want 1 — an hour of idle must not bank more than the burst", RetryBurst+1, got)
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

// outcomeLines returns the logged lines that report what became of a
// retry, so a test can assert both which one fired and that only one
// did. The test Log seam records format strings, so these match the
// literal formats in retry.go.
func outcomeLines(logs []string) []string {
	var out []string
	for _, l := range logs {
		switch {
		case strings.Contains(l, "recovered on retry"),
			strings.Contains(l, "persisted after retry"),
			strings.Contains(l, "retry abandoned"),
			strings.Contains(l, "retry ended with no outcome"):
			out = append(out, l)
		}
	}
	return out
}

func assertOneOutcome(t *testing.T, logs []string, want string) {
	t.Helper()
	got := outcomeLines(logs)
	if len(got) != 1 {
		t.Fatalf("outcome lines = %v, want exactly one; full log: %v", got, logs)
	}
	if !strings.Contains(got[0], want) {
		t.Errorf("outcome line = %q, want one containing %q", got[0], want)
	}
}

// The whole point of logging a retry is that a rescue is otherwise
// invisible, and a consumer stopping mid-stream is the ordinary case,
// not an edge one — the agent loop stops reading the moment it has what
// it needs. Logging at the end of the happy path meant 10 of 13 real
// retries on a live GKE batch reported nothing (#1039).
func TestRetryPolicy_RecoveryIsLoggedEvenWhenTheConsumerStopsEarly(t *testing.T) {
	p, logs, _ := testPolicy(t)
	fn, _ := scripted(
		[]step{{nil, errTransient}},
		[]step{{text("first"), nil}, {text("second"), nil}, {text("third"), nil}},
	)

	var got []string
	for resp, err := range p.Wrap(context.Background(), fn) {
		if err == nil && resp != nil && len(resp.Content.Parts) > 0 {
			got = append(got, resp.Content.Parts[0].Text)
		}
		break // the consumer has what it needs and walks away
	}

	if len(got) != 1 {
		t.Fatalf("got %v, want to have stopped after one", got)
	}
	assertOneOutcome(t, *logs, "recovered on retry")
}

// A retry that fired and then hit a dead context is not a silent
// non-event: an operator reading the log needs to see that the rescue
// was attempted and cut short, not just the original rejection.
func TestRetryPolicy_AbandonedBackoffIsLogged(t *testing.T) {
	p, logs, _ := testPolicy(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fn, _ := scripted([]step{{nil, errTransient}}, []step{{text("unreached"), nil}})
	drain(p.Wrap(ctx, fn))

	assertOneOutcome(t, *logs, "retry abandoned")
}

// The retry ran, produced nothing at all, and the caller got nothing.
// That is neither a recovery nor a persistence and used to log neither.
func TestRetryPolicy_RetryThatProducesNothingIsLogged(t *testing.T) {
	p, logs, _ := testPolicy(t)
	fn, calls := scripted([]step{{nil, errTransient}}, nil)

	texts, errs := drain(p.Wrap(context.Background(), fn))

	if *calls != 2 {
		t.Fatalf("underlying calls = %d, want 2", *calls)
	}
	if len(texts) != 0 || len(errs) != 0 {
		t.Fatalf("texts=%v errs=%v, want both empty — the retry returned nothing", texts, errs)
	}
	assertOneOutcome(t, *logs, "retry ended with no outcome")
}

func TestRetryPolicy_PersistentFailureLogsExactlyOneOutcome(t *testing.T) {
	p, logs, _ := testPolicy(t)
	fn, _ := scripted([]step{{nil, errTransient}}, []step{{nil, errTransient}})

	drain(p.Wrap(context.Background(), fn))

	assertOneOutcome(t, *logs, "persisted after retry")
}

// A call that never retried has no outcome to report, and saying
// anything about it would drown the lines that matter.
func TestRetryPolicy_NoOutcomeLineWithoutARetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script [][]step
	}{
		{"clean call", [][]step{{{text("fine"), nil}}}},
		{"non-transient error", [][]step{{{nil, errors.New("400 INVALID_ARGUMENT")}}}},
		{"suppressed by the budget", nil}, // filled in below
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, logs, _ := testPolicy(t)
			if tc.script == nil {
				p.Cooldown = time.Hour
				for i := 0; i < RetryBurst; i++ {
					oneFailingCall(t, p)
				}
				*logs = nil // the burst's own outcomes are not what this asserts
				oneFailingCall(t, p)
			} else {
				fn, _ := scripted(tc.script...)
				drain(p.Wrap(context.Background(), fn))
			}
			if got := outcomeLines(*logs); len(got) != 0 {
				t.Errorf("outcome lines = %v, want none — no retry fired", got)
			}
		})
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
