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

package modelretry

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/go-steer/core-models/dialect/openaichat"
	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/v2/model"
)

// judgeBackoff is the eval tier's schedule, which most of this file
// exercises because it is the one with more than one step in it.
var judgeBackoff = JudgeConfig().Backoff

// vertex429 is the error the 2026-08-21 nightly actually received,
// reproduced through the wrapping ADK's Gemini path applies to it
// (model/gemini/gemini.go: "failed to call model: %w"). Constructed
// this way rather than as a bare APIError so the test would notice if
// the classifier ever regressed to reading the message text — the
// string it would have to match is here, one %w away.
func vertex429() error {
	return fmt.Errorf("failed to call model: %w", genai.APIError{
		Code:    429,
		Status:  "RESOURCE_EXHAUSTED",
		Message: "Resource exhausted. Please try again later.",
	})
}

// flakyModel replays one outcome per call.
type flakyModel struct {
	name string
	// turns[i] is what the i-th call does: yield each response in order,
	// then fail with err if it is non-nil.
	turns []flakyTurn

	mu    sync.Mutex
	calls int
}

type flakyTurn struct {
	yields []*adkmodel.LLMResponse
	err    error
}

func (m *flakyModel) Name() string {
	if m.name == "" {
		return "scripted"
	}
	return m.name
}

func (m *flakyModel) rounds() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *flakyModel) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.mu.Lock()
	n := m.calls
	m.calls++
	m.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if n >= len(m.turns) {
			yield(nil, fmt.Errorf("flakyModel: call %d is off the end of the script", n+1))
			return
		}
		t := m.turns[n]
		for _, r := range t.yields {
			if !yield(r, nil) {
				return
			}
		}
		if t.err != nil {
			yield(nil, t.err)
		}
	}
}

func textResponse(s string) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{Content: genai.NewContentFromText(s, genai.RoleModel)}
}

// harness is one policy wired for a test: waits are recorded rather
// than served, and terminal events are collected.
type harness struct {
	policy *Policy
	slept  *[]time.Duration
	events *[]Event
}

// newHarness builds a policy on cfg whose waits cost no wall clock.
func newHarness(cfg Config) *harness {
	var (
		mu     sync.Mutex
		slept  []time.Duration
		events []Event
	)
	cfg.Sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		slept = append(slept, d)
		return nil
	}
	if cfg.Observer == nil {
		cfg.Observer = func(ev Event) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
		}
	}
	return &harness{policy: New(cfg), slept: &slept, events: &events}
}

// newJudgeHarness is the eval tier's policy, which most of these tests
// were written against.
func newJudgeHarness() *harness { return newHarness(JudgeConfig()) }

// drain collects everything a wrapped model yields.
func drain(t *testing.T, m adkmodel.LLM) ([]*adkmodel.LLMResponse, error) {
	t.Helper()
	var (
		out []*adkmodel.LLMResponse
		err error
	)
	for resp, e := range m.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
		if e != nil {
			err = e
			continue
		}
		out = append(out, resp)
	}
	return out, err
}

// outcomes is the sequence of terminal labels the observer saw.
func (h *harness) outcomes() []Outcome {
	var out []Outcome
	for _, ev := range *h.events {
		out = append(out, ev.Outcome)
	}
	return out
}

// TestA429BeforeTheModelSaysAnythingCostsAWaitNotARow is #239's whole
// point: on 2026-08-21 three of thirty-one corpus rows were lost to
// this error, and a lost row is what makes the nightly red.
func TestA429BeforeTheModelSaysAnythingCostsAWaitNotARow(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{
		{err: vertex429()},
		{yields: []*adkmodel.LLMResponse{textResponse("CRITICAL: the pod is OOMKilled")}},
	}}
	h := newJudgeHarness()

	got, err := drain(t, h.policy.Wrap(inner))
	if err != nil {
		t.Fatalf("GenerateContent returned %v, want the retry to have succeeded", err)
	}
	if len(got) != 1 || got[0].Content.Parts[0].Text != "CRITICAL: the pod is OOMKilled" {
		t.Fatalf("got %d response(s) %+v, want the second attempt's answer", len(got), got)
	}
	if inner.rounds() != 2 {
		t.Errorf("called the model %d time(s), want 2", inner.rounds())
	}
	if want := judgeBackoff[0]; len(*h.slept) != 1 || (*h.slept)[0] != want {
		t.Errorf("waited %v, want %v", *h.slept, want)
	}
	retries, declined, waited := h.policy.Stats()
	if retries != 1 || declined != 0 || waited != judgeBackoff[0] {
		t.Errorf("Stats() = (%d, %d, %v), want (1, 0, %v)", retries, declined, waited, judgeBackoff[0])
	}
	if got := h.outcomes(); len(got) != 1 || got[0] != Recovered {
		t.Errorf("observer saw %v, want exactly [%s]", got, Recovered)
	}
}

// TestTheRetryScheduleIsBoundedAndThenReportsTheProvidersError. A
// nightly with a 90-minute timeout and thirty-one sequential metered
// rows cannot wait out a real outage; it has to fail and say the board
// is short.
func TestTheRetryScheduleIsBoundedAndThenReportsTheProvidersError(t *testing.T) {
	turns := make([]flakyTurn, len(judgeBackoff)+1)
	for i := range turns {
		turns[i] = flakyTurn{err: vertex429()}
	}
	inner := &flakyModel{turns: turns}
	h := newJudgeHarness()

	got, err := drain(t, h.policy.Wrap(inner))
	if len(got) != 0 {
		t.Errorf("yielded %d response(s), want none", len(got))
	}
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 429 {
		t.Fatalf("final error = %v, want the provider's 429 rather than something of our own", err)
	}
	if want := len(judgeBackoff) + 1; inner.rounds() != want {
		t.Errorf("called the model %d time(s), want %d (one attempt per backoff step, plus the first)", inner.rounds(), want)
	}
	if len(*h.slept) != len(judgeBackoff) {
		t.Fatalf("waited %v, want the whole schedule %v", *h.slept, judgeBackoff)
	}
	for i, d := range *h.slept {
		if d != judgeBackoff[i] {
			t.Errorf("wait %d = %v, want %v", i, d, judgeBackoff[i])
		}
	}
	if got := h.outcomes(); len(got) != 1 || got[0] != Exhausted {
		t.Errorf("observer saw %v, want exactly [%s]", got, Exhausted)
	}
}

// TestAnErrorThatWillFailIdenticallyForeverIsNotRetried. A 400 is the
// request's own fault; spending the schedule on it only reaches the
// same row half a minute later.
//
// It also pins the reporting gate. A malformed request is not an
// exhausted retry schedule, and an operator watching
// mast_provider_retries_total for provider pressure would read one as
// the other if every failure landed in the family.
func TestAnErrorThatWillFailIdenticallyForeverIsNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a bad request", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT"}},
		{"an expired credential", genai.APIError{Code: 403, Status: "PERMISSION_DENIED"}},
		{"an internal error that reproduces", genai.APIError{Code: 500, Status: "INTERNAL"}},
		{"an error the classifier cannot read", errors.New("connection reset by peer")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := &flakyModel{turns: []flakyTurn{{err: tc.err}}}
			h := newJudgeHarness()

			if _, err := drain(t, h.policy.Wrap(inner)); err == nil {
				t.Fatal("GenerateContent succeeded, want the error passed through")
			}
			if inner.rounds() != 1 {
				t.Errorf("called the model %d time(s), want 1", inner.rounds())
			}
			if len(*h.slept) != 0 {
				t.Errorf("waited %v, want no wait at all", *h.slept)
			}
			if got := h.outcomes(); len(got) != 0 {
				t.Errorf("observer saw %v, want nothing: no retry was in play", got)
			}
		})
	}
}

// TestA503IsRetried. The provider restarting something is the other
// "not now", and the corpus should survive it for the same reason.
func TestA503IsRetried(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{
		{err: genai.APIError{Code: 503, Status: "UNAVAILABLE"}},
		{yields: []*adkmodel.LLMResponse{textResponse("ok")}},
	}}
	h := newJudgeHarness()

	if _, err := drain(t, h.policy.Wrap(inner)); err != nil {
		t.Fatalf("GenerateContent returned %v, want the retry to have succeeded", err)
	}
	if inner.rounds() != 2 {
		t.Errorf("called the model %d time(s), want 2", inner.rounds())
	}
}

// TestBothProvidersAre429AwareThroughTheirOwnErrorType. mast is pointed
// at Gemini and Claude, and the whole reason the 2026-08-21 gap was
// invisible is that only one of the two SDKs retries on its own. A
// classifier that reads one of them puts the asymmetry back.
func TestBothProvidersAre429AwareThroughTheirOwnErrorType(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"gemini 429", vertex429(), true},
		{"gemini 400", genai.APIError{Code: 400}, false},
		{"anthropic 429", fmt.Errorf("wrapped: %w", anthropicErr(429)), true},
		{"anthropic 503", anthropicErr(503), true},
		{"anthropic 401", anthropicErr(401), false},
		{"provider profile 429", fmt.Errorf("wrapped: %w", &openaichat.APIError{StatusCode: 429}), true},
		{"provider profile 503", &openaichat.APIError{StatusCode: 503}, true},
		{"provider profile 400", &openaichat.APIError{StatusCode: 400}, false},
		{"provider profile in-stream error", &openaichat.APIError{Message: "out of memory"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Reports tc.name rather than the error: *anthropic.Error's
			// Error() dereferences the http.Request and Response it was
			// built from, so formatting a hand-made one turns a failing
			// assertion into a panic in the assertion.
			if got := isRetryable(tc.err); got != tc.want {
				t.Errorf("isRetryable(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// anthropicErr builds the SDK's error with the request and response it
// would carry in the wild, because Error() reads both.
func anthropicErr(status int) error {
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if err != nil {
		panic(err)
	}
	return &anthropic.Error{
		StatusCode: status,
		Request:    req,
		Response:   &http.Response{StatusCode: status},
	}
}

// TestTheClassifierCannotBeFooledByThePayloadItIsGrading. The judge
// corpus is thirty-one Kubernetes incidents, several of them literally
// about exhausted resources, and the grader is handed those responses
// verbatim. In production the hazard is worse: a workload whose job is
// triaging a cluster puts the phrase in a tool result.
func TestTheClassifierCannotBeFooledByThePayloadItIsGrading(t *testing.T) {
	err := fmt.Errorf("judge: grade LC-05: no JSON object in reply: %q",
		"CRITICAL: the namespace quota is exhausted — Error 429, Status: RESOURCE_EXHAUSTED on every admission")
	if isRetryable(err) {
		t.Error("a grader error quoting a model's answer was classified as a provider 429")
	}
}

// TestAFailureAfterTheModelStartedTalkingIsNotRetried. Replaying a call
// that already yielded would hand the caller that content twice, and
// ADK assembles the yields into one turn.
func TestAFailureAfterTheModelStartedTalkingIsNotRetried(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{
		{yields: []*adkmodel.LLMResponse{textResponse("CRITICAL: the pod")}, err: vertex429()},
		{yields: []*adkmodel.LLMResponse{textResponse("should never be reached")}},
	}}
	h := newJudgeHarness()

	got, err := drain(t, h.policy.Wrap(inner))
	if err == nil {
		t.Fatal("GenerateContent succeeded, want the mid-stream error reported")
	}
	if len(got) != 1 || got[0].Content.Parts[0].Text != "CRITICAL: the pod" {
		t.Errorf("got %+v, want the one partial response, delivered once", got)
	}
	if inner.rounds() != 1 {
		t.Errorf("called the model %d time(s), want 1 — a started stream cannot be replayed", inner.rounds())
	}
	if len(*h.slept) != 0 {
		t.Errorf("waited %v, want no wait", *h.slept)
	}
	if retries, _, _ := h.policy.Stats(); retries != 0 {
		t.Errorf("Stats() counted %d retries, want 0", retries)
	}
}

// TestACancelledRunIsNotRetried. Ctrl-C, or the nightly's timeout,
// should stop rather than spend the schedule discovering that three
// more times.
func TestACancelledRunIsNotRetried(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{{err: vertex429()}, {err: vertex429()}}}
	h := newJudgeHarness()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var errs []error
	for _, e := range h.policy.Wrap(inner).GenerateContent(ctx, &adkmodel.LLMRequest{}, false) {
		if e != nil {
			errs = append(errs, e)
		}
	}
	if len(errs) != 1 {
		t.Fatalf("yielded %d error(s), want exactly 1", len(errs))
	}
	if inner.rounds() != 1 {
		t.Errorf("called the model %d time(s), want 1", inner.rounds())
	}
	if len(*h.slept) != 0 {
		t.Errorf("waited %v on a cancelled context, want no wait", *h.slept)
	}
}

// TestAnInterruptedWaitReportsWhyTheCallFailedNotWhyWeStoppedWaiting.
// The reader of a red board wants the 429, not "context canceled".
func TestAnInterruptedWaitReportsWhyTheCallFailedNotWhyWeStoppedWaiting(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{{err: vertex429()}, {err: vertex429()}}}
	cfg := JudgeConfig()
	cfg.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	p := New(cfg)

	_, err := drain(t, p.Wrap(inner))
	var apiErr genai.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 429 {
		t.Fatalf("final error = %v, want the provider's 429", err)
	}
	if inner.rounds() != 1 {
		t.Errorf("called the model %d time(s), want 1", inner.rounds())
	}
	retries, _, waited := p.Stats()
	if retries != 0 || waited != 0 {
		t.Errorf("Stats() = (%d, %v), want (0, 0) — an unserved wait is not a retry", retries, waited)
	}
}

// TestAConsumerThatStopsEarlyStopsTheModel. ADK's runner breaks out of
// the sequence on its own conditions; a wrapper that kept calling after
// that would spend money on turns nobody reads.
func TestAConsumerThatStopsEarlyStopsTheModel(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{
		{yields: []*adkmodel.LLMResponse{textResponse("one"), textResponse("two")}},
	}}
	h := newJudgeHarness()

	var seen int
	for range h.policy.Wrap(inner).GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false) {
		seen++
		break
	}
	if seen != 1 {
		t.Errorf("saw %d response(s) after breaking, want 1", seen)
	}
	if inner.rounds() != 1 {
		t.Errorf("called the model %d time(s), want 1", inner.rounds())
	}
}

// TestTheWrapperDoesNotRenameTheModel. The board prints the model under
// test, J-cost-tier prices calls by model name, and pkg/budget meters
// against it; a renamed model would make all three describe something
// nobody can buy.
func TestTheWrapperDoesNotRenameTheModel(t *testing.T) {
	r := New(JudgeConfig()).Wrap(&flakyModel{name: "gemini-3.7-flash"})
	if got := r.Name(); got != "gemini-3.7-flash" {
		t.Errorf("Name() = %q, want the wrapped model's own name", got)
	}
}

// TestWrappingNilStaysNil so a caller can wrap unconditionally without
// turning "no model" into a wrapper around nothing.
//
// The typed nil is the trap worth a test: a *retryingLLM(nil) handed to
// a model.LLM parameter is a non-nil INTERFACE, so a constructor's
// `m == nil` guard would wave it through and the dereference would land
// somewhere with no context attached. The judge rig's guard is the one
// that actually depends on this; see
// internal/evals/judge.TestNewRigRefusesANilWrappedModel.
func TestWrappingNilStaysNil(t *testing.T) {
	// Wrap's return type is already adkmodel.LLM, so this comparison is
	// against a nil interface and not against a nil pointer.
	m := New(JudgeConfig()).Wrap(nil)
	if m != nil {
		t.Errorf("Wrap(nil) = %#v, want a nil interface", m)
	}
}

// TestAWaitIsAnnouncedBeforeItIsServed. A 27-second pause nobody
// narrated is indistinguishable from a hung run in a nightly's log.
func TestAWaitIsAnnouncedBeforeItIsServed(t *testing.T) {
	inner := &flakyModel{name: "gemini-3.7-flash", turns: []flakyTurn{
		{err: vertex429()},
		{yields: []*adkmodel.LLMResponse{textResponse("ok")}},
	}}
	var announced []string
	cfg := JudgeConfig()
	cfg.OnWait = func(model string, attempt int, wait time.Duration, err error) {
		announced = append(announced, fmt.Sprintf("%s attempt %d after %s: %v", model, attempt, wait, err))
	}
	h := newHarness(cfg)

	if _, err := drain(t, h.policy.Wrap(inner)); err != nil {
		t.Fatalf("GenerateContent returned %v", err)
	}
	if len(announced) != 1 {
		t.Fatalf("announced %v, want exactly one line", announced)
	}
	// The model name is on the hook rather than implied by the caller
	// because the judge tier runs two models under ONE policy — the
	// model under test and the grader — and a narration that could not
	// tell them apart would print the same line for both.
	if want := "gemini-3.7-flash attempt 1 after 3s: failed to call model: "; announced[0][:len(want)] != want {
		t.Errorf("announced %q, want it to start %q", announced[0], want)
	}
}

// TestTheFirstCallHappensWhenGenerateContentIsCalled, not when the
// sequence it returns is first walked.
//
// mast's Gemini layer populates req.Config.Tools at invocation rather
// than at iteration (internal/providers/gemini), and that population IS the
// built-in-tool gate #324 put in front of server-side web search. A
// wrapper that deferred the inner call to first iteration would move a
// security boundary to a later moment and delete it outright for a
// caller that asks for a sequence it does not walk — which is exactly
// what internal/compose's builtin-tool tests do.
func TestTheFirstCallHappensWhenGenerateContentIsCalled(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{{yields: []*adkmodel.LLMResponse{textResponse("ok")}}}}
	h := newHarness(ProductionConfig())

	seq := h.policy.Wrap(inner).GenerateContent(context.Background(), &adkmodel.LLMRequest{}, false)
	if got := inner.rounds(); got != 1 {
		t.Errorf("the wrapped model was called %d time(s) before the sequence was walked, want 1 — "+
			"the same moment the bare model would have been called", got)
	}
	// And walking it does not call a second time.
	for range seq {
	}
	if got := inner.rounds(); got != 1 {
		t.Errorf("walking the sequence called the model again (%d calls total), want 1", got)
	}
}

// --- the production policy (#452) ---

// TestTheProductionPolicyRetriesOnceAndNoMore. The evidence for
// retrying at all is that the next call a few seconds later succeeds.
// Four attempts over half a minute is a different bet, and an
// unattended turn that waits that long has stopped being late and
// started being wedged.
func TestTheProductionPolicyRetriesOnceAndNoMore(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{
		{err: vertex429()}, {err: vertex429()}, {err: vertex429()},
	}}
	h := newHarness(ProductionConfig())

	if _, err := drain(t, h.policy.Wrap(inner)); err == nil {
		t.Fatal("GenerateContent succeeded, want the provider's error after the one retry")
	}
	if inner.rounds() != 2 {
		t.Errorf("called the model %d time(s), want 2 (the call, then one retry)", inner.rounds())
	}
	if want := []time.Duration{2 * time.Second}; len(*h.slept) != 1 || (*h.slept)[0] != want[0] {
		t.Errorf("waited %v, want %v", *h.slept, want)
	}
}

// TestAFanOutMeetingOneShedProducesOneRetryNotEight is the reason the
// production policy has a cooldown the judge tier does not: the judge
// runs its rows one at a time, and mast dispatches specialists in
// parallel. Without it, "retrying under pressure adds load" is not an
// objection to answer but a description of what happens.
//
// Fails before #452 at the import: this policy did not exist outside
// the eval tier, and the copy that did had no cooldown at all.
func TestAFanOutMeetingOneShedProducesOneRetryNotEight(t *testing.T) {
	const fanout = 8
	h := newHarness(ProductionConfig())

	var wg sync.WaitGroup
	for i := range fanout {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inner := &flakyModel{
				name:  fmt.Sprintf("sp-%d", i),
				turns: []flakyTurn{{err: vertex429()}, {yields: []*adkmodel.LLMResponse{textResponse("ok")}}},
			}
			drain(t, h.policy.Wrap(inner))
		}()
	}
	wg.Wait()

	retries, declined, _ := h.policy.Stats()
	if retries != 1 {
		t.Errorf("served %d retries across %d simultaneous rejections, want 1", retries, fanout)
	}
	if want := fanout - 1; declined != want {
		t.Errorf("declined %d retries, want %d", declined, want)
	}
	// Every call that met the shed is accounted for, so an operator
	// reading the family sees the pressure rather than one retry and
	// seven silences.
	if got := len(*h.events); got != fanout {
		t.Errorf("observer saw %d event(s), want one per affected call (%d)", got, fanout)
	}
}

// TestACooldownRefusalIsItsOwnOutcome. "The provider said no and we did
// not even try again" is a different operational fact from "we tried
// and it said no twice": the first says this process is already under
// its own rate limit, which is the signal that a workload's fan-out is
// too wide for its quota.
func TestACooldownRefusalIsItsOwnOutcome(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{
		{err: vertex429()},
		{yields: []*adkmodel.LLMResponse{textResponse("ok")}},
	}}
	cfg := ProductionConfig()
	// A policy that has already spent its window.
	cfg.Now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	h := newHarness(cfg)
	h.policy.lastRetry = cfg.Now()

	if _, err := drain(t, h.policy.Wrap(inner)); err == nil {
		t.Fatal("GenerateContent succeeded, want the rejection passed straight through")
	}
	if inner.rounds() != 1 {
		t.Errorf("called the model %d time(s), want 1 — the cooldown refused the retry", inner.rounds())
	}
	if len(*h.slept) != 0 {
		t.Errorf("waited %v, want no wait: a declined retry costs no latency", *h.slept)
	}
	if got := h.outcomes(); len(got) != 1 || got[0] != Declined {
		t.Errorf("observer saw %v, want exactly [%s]", got, Declined)
	}
	if _, declined, _ := h.policy.Stats(); declined != 1 {
		t.Errorf("Stats() counted %d declined, want 1", declined)
	}
}

// TestTheCooldownReopens, or a process that met one 429 at startup
// would spend the rest of its life refusing to retry anything.
func TestTheCooldownReopens(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cfg := ProductionConfig()
	cfg.Now = func() time.Time { return now }
	h := newHarness(cfg)

	shed := func() *flakyModel {
		m := &flakyModel{turns: []flakyTurn{
			{err: vertex429()},
			{yields: []*adkmodel.LLMResponse{textResponse("ok")}},
		}}
		drain(t, h.policy.Wrap(m))
		return m
	}

	if got := shed().rounds(); got != 2 {
		t.Fatalf("the first rejection got %d call(s), want 2", got)
	}
	if got := shed().rounds(); got != 1 {
		t.Fatalf("a second rejection inside the window got %d call(s), want 1", got)
	}
	now = now.Add(cfg.Cooldown)
	if got := shed().rounds(); got != 2 {
		t.Errorf("a rejection after the cooldown elapsed got %d call(s), want 2", got)
	}
}

// TestSharedIsOneCooldownForTheWholeProcess. A specialist's `model:`
// override builds its own model.LLM through the same BuildModel, so a
// cooldown scoped to a model instance would give a roster of eight one
// window each — which is the same as no cooldown at all.
func TestSharedIsOneCooldownForTheWholeProcess(t *testing.T) {
	if a, b := Shared(), Shared(); a != b {
		t.Fatalf("Shared() returned two policies (%p, %p), want the one the whole process shares", a, b)
	}
	if got := Shared().cooldown; got != ProductionConfig().Cooldown {
		t.Errorf("Shared().cooldown = %v, want the production policy's %v", got, ProductionConfig().Cooldown)
	}
}

// TestAnObserverInstalledAfterTheFactStillSees. cmd/mast sets this at
// startup, because the daemon knows its workload name and
// compose.BuildModel — which does the wrapping — does not.
func TestAnObserverInstalledAfterTheFactStillSees(t *testing.T) {
	p := New(Config{
		Backoff: []time.Duration{time.Second},
		Sleep:   func(context.Context, time.Duration) error { return nil },
	})
	m := p.Wrap(&flakyModel{turns: []flakyTurn{
		{err: vertex429()},
		{yields: []*adkmodel.LLMResponse{textResponse("ok")}},
	}})

	var seen []Event
	p.SetObserver(func(ev Event) { seen = append(seen, ev) })

	if _, err := drain(t, m); err != nil {
		t.Fatalf("GenerateContent returned %v", err)
	}
	if len(seen) != 1 || seen[0].Outcome != Recovered {
		t.Fatalf("observer saw %+v, want one %s event", seen, Recovered)
	}
	if seen[0].Attempts != 1 || seen[0].Waited != time.Second {
		t.Errorf("event = {Attempts: %d, Waited: %v}, want {1, 1s}", seen[0].Attempts, seen[0].Waited)
	}
	if seen[0].Err == nil {
		t.Error("event carries no error; a recovered call still has a rejection worth naming")
	}
}

// TestAPolicyWithNoBackoffRetriesNothing, so "retries off" is
// expressible and does not silently become a default schedule.
func TestAPolicyWithNoBackoffRetriesNothing(t *testing.T) {
	inner := &flakyModel{turns: []flakyTurn{{err: vertex429()}}}
	h := newHarness(Config{})

	if _, err := drain(t, h.policy.Wrap(inner)); err == nil {
		t.Fatal("GenerateContent succeeded, want the rejection passed through")
	}
	if inner.rounds() != 1 {
		t.Errorf("called the model %d time(s), want 1", inner.rounds())
	}
	if got := h.outcomes(); len(got) != 0 {
		t.Errorf("observer saw %v, want nothing: no retry was ever on offer", got)
	}
}

// TestASuccessfulCallSaysNothing. One event per call would bury the
// three in thirty-one that matter; the denominator lives in
// mast_model_calls_total.
func TestASuccessfulCallSaysNothing(t *testing.T) {
	h := newHarness(ProductionConfig())
	inner := &flakyModel{turns: []flakyTurn{{yields: []*adkmodel.LLMResponse{textResponse("ok")}}}}

	if _, err := drain(t, h.policy.Wrap(inner)); err != nil {
		t.Fatalf("GenerateContent returned %v", err)
	}
	if got := h.outcomes(); len(got) != 0 {
		t.Errorf("observer saw %v, want nothing at all", got)
	}
}
