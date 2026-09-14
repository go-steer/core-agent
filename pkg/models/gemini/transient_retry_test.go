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

// End-to-end wiring for #935: the predicate and models.RetryPolicy are
// unit-tested in their own packages, but neither proves the policy is
// actually attached to GenerateContent, or that it sits outside the
// empty-response and cache-eviction wrappers rather than getting
// swallowed by them. These drive the real composed stack.

package gemini

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/models"
)

// scriptedLLM answers its Nth invocation with the Nth script entry.
type scriptedLLM struct {
	script [][]fakeEvent
	calls  int
}

func (s *scriptedLLM) Name() string { return "scripted" }

func (s *scriptedLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	i := s.calls
	s.calls++
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if i >= len(s.script) {
			return
		}
		for _, e := range s.script[i] {
			if !yield(e.resp, e.err) {
				return
			}
		}
	}
}

func modelText(s string) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: s}},
		},
	}
}

// useFastRetryPolicy swaps the package policy for one that does not
// make the test wait two seconds or leak a 30s cooldown into whatever
// runs next.
func useFastRetryPolicy(t *testing.T) {
	t.Helper()
	prev := transientRetry
	transientRetry = &models.RetryPolicy{
		IsTransient: IsTransient,
		Backoff:     time.Millisecond,
		Cooldown:    -1, // each test gets a clean slate
		Log:         func(format string, args ...any) { logf(format, args...) },
	}
	t.Cleanup(func() { transientRetry = prev })
}

func drainLLM(t *testing.T, l adkmodel.LLM) (texts []string, errs []error) {
	t.Helper()
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}}}},
		Config:   &genai.GenerateContentConfig{},
	}
	for resp, err := range l.GenerateContent(context.Background(), req, false) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if resp != nil && resp.Content != nil {
			for _, p := range resp.Content.Parts {
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			}
		}
	}
	return texts, errs
}

// The #935 case, driven through GenerateContent: the error text is the
// one recorded verbatim in the four drill runs that lost a delegation.
func TestGenerateContent_RetriesArchived429(t *testing.T) {
	useFastRetryPolicy(t)
	inner := &scriptedLLM{script: [][]fakeEvent{
		{{nil, errors.New(archived429)}},
		{{modelText("the answer"), nil}},
	}}
	wrapped := &builtinsLLM{inner: inner}

	texts, errs := drainLLM(t, wrapped)

	if inner.calls != 2 {
		t.Errorf("inner calls = %d, want 2 — the retry did not reach the model", inner.calls)
	}
	if len(errs) != 0 {
		t.Errorf("caller saw %v, want no error", errs)
	}
	if len(texts) != 1 || texts[0] != "the answer" {
		t.Errorf("texts = %v, want [the answer]", texts)
	}
}

// A persistent 429 must surface rather than loop, and must cost exactly
// two requests.
func TestGenerateContent_PersistentRateLimitSurfaces(t *testing.T) {
	useFastRetryPolicy(t)
	inner := &scriptedLLM{script: [][]fakeEvent{
		{{nil, errors.New(archived429)}},
		{{nil, errors.New(archived429)}},
		{{modelText("unreached"), nil}},
	}}
	wrapped := &builtinsLLM{inner: inner}

	texts, errs := drainLLM(t, wrapped)

	if inner.calls != 2 {
		t.Errorf("inner calls = %d, want exactly 2", inner.calls)
	}
	if len(texts) != 0 {
		t.Errorf("texts = %v, want none", texts)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want the 429 surfaced once", errs)
	}
}

// The retry budget has to hold across GenerateContent calls on
// different builtinsLLM instances — a daemon's parent and its subagents
// each get their own wrapper from Model(), and quota is per project. If
// the policy were a per-instance field, every wrapper below would find
// a full budget and retry.
//
// Since #1039 the budget is a burst of models.RetryBurst rather than a
// single timestamp, so sharing is proved by spending the whole budget
// through separate wrappers and then watching a fresh one find it
// already gone.
func TestTransientRetryBudgetIsProcessWide(t *testing.T) {
	prev := transientRetry
	transientRetry = &models.RetryPolicy{
		IsTransient: IsTransient,
		Backoff:     time.Millisecond,
		Cooldown:    time.Hour, // nothing refills inside this test
	}
	t.Cleanup(func() { transientRetry = prev })

	for i := 1; i <= models.RetryBurst; i++ {
		spender := &scriptedLLM{script: [][]fakeEvent{
			{{nil, errors.New(archived429)}},
			{{nil, errors.New(archived429)}},
		}}
		drainLLM(t, &builtinsLLM{inner: spender})
		if spender.calls != 2 {
			t.Fatalf("wrapper %d made %d calls, want 2 — it is inside the burst", i, spender.calls)
		}
	}

	last := &scriptedLLM{script: [][]fakeEvent{
		{{nil, errors.New(archived429)}},
		{{modelText("unreached"), nil}},
	}}
	drainLLM(t, &builtinsLLM{inner: last})
	if last.calls != 1 {
		t.Errorf("the wrapper past the burst made %d calls, want 1 — the budget is not shared", last.calls)
	}
}

// A non-transient failure must not be retried by the new wrapper, and
// the wrappers already there must keep behaving as they did.
func TestGenerateContent_PermanentErrorStillNotRetried(t *testing.T) {
	useFastRetryPolicy(t)
	boom := errors.New("Error 400, Message: Request contains an invalid argument., Status: INVALID_ARGUMENT, Details: []")
	inner := &scriptedLLM{script: [][]fakeEvent{
		{{nil, boom}},
		{{modelText("unreached"), nil}},
	}}
	wrapped := &builtinsLLM{inner: inner}

	_, errs := drainLLM(t, wrapped)

	if inner.calls != 1 {
		t.Errorf("inner calls = %d, want 1 — a 400 must not be retried", inner.calls)
	}
	if len(errs) != 1 || !errors.Is(errs[0], boom) {
		t.Errorf("errs = %v, want the 400 unchanged", errs)
	}
}
