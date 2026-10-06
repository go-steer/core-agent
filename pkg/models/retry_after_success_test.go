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

// #1247: RetryPolicy.IsTransientAfterSuccess, the predicate consulted
// only for a call whose session has already been served.

package models

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// errAmbiguous stands in for Vertex's bare 400. Like errTransient it
// is matched by predicate, never by text; the real predicate is tested
// in pkg/models/gemini against the string Vertex sends.
var errAmbiguous = errors.New("ambiguous provider rejection")

func isErrAmbiguous(err error) bool { return errors.Is(err, errAmbiguous) }

func afterSuccessPolicy(t *testing.T) (*RetryPolicy, *[]string) {
	t.Helper()
	p, logs, _ := testPolicy(t)
	p.IsTransientAfterSuccess = isErrAmbiguous
	return p, logs
}

// served returns a context whose session has had a model call succeed.
func served() context.Context {
	rec := NewPriorSuccess()
	rec.Mark()
	return WithPriorSuccess(context.Background(), rec)
}

func TestRetryAfterSuccess_RecoveredIsStamped(t *testing.T) {
	p, logs := afterSuccessPolicy(t)
	fn, calls := scripted(
		[]step{{nil, errAmbiguous}},
		[]step{{text("recovered"), nil}},
	)

	resps, errs := collect(p.Wrap(served(), fn))

	if *calls != 2 {
		t.Errorf("underlying calls = %d, want 2", *calls)
	}
	if len(errs) != 0 {
		t.Errorf("caller saw %v, want none — the retry recovered", errs)
	}
	stamps := retryStamps(resps)
	if len(stamps) != 1 || stamps[0]["outcome"] != "recovered" || stamps[0]["error"] != errAmbiguous.Error() {
		t.Errorf("stamps = %v, want one recovered stamp naming the rejection", stamps)
	}
	joined := strings.Join(*logs, "|")
	if !strings.Contains(joined, "transient provider error (%v) — retrying once after %s") {
		t.Errorf("no retry line a2_count can count: %v", *logs)
	}
	assertOneOutcome(t, *logs, "recovered on retry")
}

func TestRetryAfterSuccess_PersistedIsARetryError(t *testing.T) {
	p, logs := afterSuccessPolicy(t)
	fn, calls := scripted(
		[]step{{nil, errAmbiguous}},
		[]step{{nil, errAmbiguous}},
		[]step{{text("never reached"), nil}},
	)

	_, errs := drain(p.Wrap(served(), fn))

	if *calls != 2 {
		t.Errorf("underlying calls = %d, want 2 — one retry, never two", *calls)
	}
	var re *RetryError
	if len(errs) != 1 || !errors.As(errs[0], &re) || re.Outcome != "persisted" || !errors.Is(errs[0], errAmbiguous) {
		t.Fatalf("errs = %v, want one RetryError{persisted} unwrapping to the rejection", errs)
	}
	if !strings.HasPrefix(errs[0].Error(), "provider retry persisted: ") {
		t.Errorf("error %q lost the prefix", errs[0])
	}
	assertOneOutcome(t, *logs, "persisted after retry")
}

// The narrowness, one row per reason the ambiguous error must surface
// exactly as it did before #1247: one call, the bare error, no retry
// claimed anywhere.
func TestRetryAfterSuccess_NotRetriedWithoutAServedSession(t *testing.T) {
	unmarked := WithPriorSuccess(context.Background(), NewPriorSuccess())
	shadowed := WithPriorSuccess(served(), nil)
	sideCall := AsSideCall(served(), "approver")
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"no record on ctx", context.Background()},
		{"record never marked", unmarked},
		{"record shadowed by a nested run", shadowed},
		{"side call in a served session", sideCall},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, logs := afterSuccessPolicy(t)
			fn, calls := scripted(
				[]step{{nil, errAmbiguous}},
				[]step{{text("unreached"), nil}},
			)

			_, errs := drain(p.Wrap(tc.ctx, fn))

			if *calls != 1 {
				t.Errorf("underlying calls = %d, want 1", *calls)
			}
			if len(errs) != 1 || errs[0] != errAmbiguous {
				t.Errorf("errs = %v, want the bare rejection, unwrapped", errs)
			}
			if len(*logs) != 0 {
				t.Errorf("logged %v, want nothing — no retry was considered", *logs)
			}
		})
	}
}

// Without IsTransientAfterSuccess a served session changes nothing:
// the field is the only door.
func TestRetryAfterSuccess_NilPredicateIsTodaysBehaviour(t *testing.T) {
	p, _, _ := testPolicy(t)
	fn, calls := scripted([]step{{nil, errAmbiguous}}, []step{{text("unreached"), nil}})

	_, errs := drain(p.Wrap(served(), fn))

	if *calls != 1 || len(errs) != 1 || errs[0] != errAmbiguous {
		t.Errorf("calls = %d, errs = %v, want 1 call and the bare rejection", *calls, errs)
	}
}

// The record is read when the error arrives, not when Wrap is called.
func TestRetryAfterSuccess_RecordIsReadAtTheRejection(t *testing.T) {
	p, _ := afterSuccessPolicy(t)
	rec := NewPriorSuccess()
	ctx := WithPriorSuccess(context.Background(), rec)
	fn, calls := scripted([]step{{nil, errAmbiguous}}, []step{{text("recovered"), nil}})

	seq := p.Wrap(ctx, fn)
	rec.Mark()
	texts, errs := drain(seq)

	if *calls != 2 || len(errs) != 0 || len(texts) != 1 {
		t.Errorf("calls = %d, texts = %v, errs = %v, want a recovered retry", *calls, texts, errs)
	}
}

// One budget for both predicates: an ambiguous 400 spends the tokens a
// 429 would, and is refused by a spent bucket the same way.
func TestRetryAfterSuccess_SharesTheBudget(t *testing.T) {
	p, logs := afterSuccessPolicy(t)
	p.Cooldown = 0 // the real default; the test clock never advances
	for i := 1; i <= RetryBurst; i++ {
		fn, calls := scripted([]step{{nil, errAmbiguous}}, []step{{nil, errAmbiguous}})
		drain(p.Wrap(served(), fn))
		if *calls != 2 {
			t.Fatalf("call %d made %d requests, want 2 — it is inside the burst", i, *calls)
		}
	}

	for _, rej := range []error{errAmbiguous, errTransient} {
		fn, calls := scripted([]step{{nil, rej}}, []step{{text("unreached"), nil}})
		_, errs := drain(p.Wrap(served(), fn))
		if *calls != 1 {
			t.Errorf("%v past the burst made %d requests, want 1", rej, *calls)
		}
		var re *RetryError
		if len(errs) != 1 || !errors.As(errs[0], &re) || re.Outcome != "skipped" {
			t.Errorf("%v past the burst surfaced %v, want RetryError{skipped}", rej, errs)
		}
	}
	if !strings.Contains(strings.Join(*logs, "|"), "NOT retried") {
		t.Errorf("no budget-spent line: %v", *logs)
	}
}

func TestPriorSuccess_NilSafe(t *testing.T) {
	var rec *PriorSuccess
	rec.Mark()
	if rec.Succeeded() {
		t.Error("a nil record reported success")
	}
	if PriorSuccessFrom(context.Background()) != nil {
		t.Error("an empty context carried a record")
	}
}
