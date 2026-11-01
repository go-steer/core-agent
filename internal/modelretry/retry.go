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

// Package modelretry waits out a provider that says "not now".
//
// # Why this exists (#239, then #452)
//
// It was written for the judge eval tier. That tier's expensive failure
// is not a low score — a low score reports and does not gate — it is a
// row that never ran, which makes the board incomplete and the night
// red. On 2026-08-21 three of thirty-one rows were lost to
// `Error 429 ... RESOURCE_EXHAUSTED` from Vertex, and the tier made no
// attempt to get them back, so a provider-side quota blip presented as
// a broken measurement of mast.
//
// The asymmetry that hid this is worth stating, because it is invisible
// from mast's side of the interface: anthropic-sdk-go retries 429s and
// 5xxs twice of its own accord, and google.golang.org/genai returns
// APIError{Code: 429} straight out of api_client.go with no retry at
// all. So the same corpus, on the same night, was measuring two
// different amounts of resilience depending on which model it named.
// Retrying at model.LLM — the one interface both providers arrive
// through — is what makes the two boards comparable.
//
// # The promotion (#452)
//
// For eight releases this lived in internal/evals/judge and wrapped
// nothing but the eval harness's own models. That is the inversion
// #452 named: mast's *measurement* of mast was resilient to a 429 and
// the product was not. The measurement is mast's own and it is dated —
// three rows in thirty-one, through compose.BuildModel, against real
// Vertex — so the issue's "only if the evals say yes" gate was already
// answered by mast's own artifact and did not need a second study.
//
// What moved is the mechanism. What did not move is the judge tier's
// reasoning about its own numbers: a nightly with a 90-minute budget
// and thirty-one sequential metered rows wants four attempts over half
// a minute, and an unattended turn does not. The two policies are
// [ProductionConfig] and [JudgeConfig], side by side, so the divergence
// is a diff rather than a rediscovery.
//
// This does not soften anything. An incomplete board still fails and a
// dispatch that ends short still reports short; the point is to stop
// paying for a provider blip with a result.
package modelretry

import (
	"context"
	"errors"
	"iter"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/v2/model"
)

// retryableCodes are the HTTP statuses worth waiting out.
//
// Deliberately short. 429 is the quota blip this was written for and
// 503 is the provider restarting something; both are the provider
// saying "not now" rather than "not ever". 500 is excluded: an internal
// error that reproduces is a real result, and retrying it three times
// only spends money to reach the same row three times slower. 4xx other
// than 429 are the request's own fault and will fail identically
// forever.
var retryableCodes = map[int]bool{429: true, 503: true}

// Outcome is how one call's brush with a transient rejection ended. It
// is a metric label value; see docs/site/.../reference/metrics.md.
type Outcome string

const (
	// Recovered: a retry was served and the call then produced content.
	// The provider's "not now" cost a wait instead of a result.
	Recovered Outcome = "recovered"

	// Exhausted: a retry was served and the call failed anyway, or the
	// schedule ran out. The caller got the provider's error.
	Exhausted Outcome = "exhausted"

	// Declined: the error qualified for a retry and the cooldown refused
	// one, because this process already spent a retry recently. The
	// caller got the provider's error without a wait.
	Declined Outcome = "declined"
)

// Event is one call's terminal retry accounting, emitted once, and only
// for a call that met a retryable rejection. A call that never saw one
// emits nothing — the denominator lives in mast_model_calls_total, and
// an event per successful call would bury the three that matter.
type Event struct {
	// Model is the wrapped model's name, so an operator can tell which
	// provider is shedding without reading the error.
	Model string

	// Outcome is one of the constants above.
	Outcome Outcome

	// Attempts is how many retries were served (0 when Declined).
	Attempts int

	// Waited is the total time spent sleeping before those retries.
	Waited time.Duration

	// Err is the provider's rejection — the last one, if there were
	// several. Non-nil even on Recovered, where it is the rejection that
	// was waited out.
	Err error
}

// Config builds a [Policy].
type Config struct {
	// Backoff is the wait before each retry; its length is the retry
	// budget. Required — a nil Backoff is a policy that retries nothing,
	// which is a thing a caller may legitimately want and must therefore
	// not be silently replaced with a default.
	Backoff []time.Duration

	// Cooldown is the minimum gap between two retries served by this
	// policy, across every call it wraps. Zero disables it.
	Cooldown time.Duration

	// OnWait announces a wait before it is served. Optional, and the
	// judge tier passes one: a 27-second pause nobody narrated is
	// indistinguishable from a hung run in a nightly's progress log.
	OnWait func(model string, attempt int, wait time.Duration, err error)

	// Observer receives one [Event] per affected call. Optional; a host
	// with a metric registry sets it, a library embed leaves it nil and
	// reads [Policy.Stats] instead.
	Observer func(Event)

	// Sleep is time.Sleep in production and a recorder in tests. Nil
	// means the real one. It returns the context's error if the wait was
	// interrupted.
	Sleep func(ctx context.Context, d time.Duration) error

	// Now is time.Now in production and a clock in tests. Nil means the
	// real one.
	Now func() time.Time
}

// ProductionConfig is what compose.BuildModel wraps a real provider
// model in: one retry, two seconds later, at most once a minute
// process-wide.
//
// Every number differs from [JudgeConfig], and each difference is the
// same fact read from the other side.
//
// **One attempt, not four.** The evidence for a retry at all is that the
// next call a few seconds later succeeds; four attempts over half a
// minute is not more of that argument, it is a different bet. The judge
// tier takes it because a lost row costs a whole night and the tier is
// already measured in tens of minutes. An unattended turn is not: a
// workload on a fifteen-minute cadence can absorb two seconds and
// should not absorb thirty-nine, because a turn that waits that long
// has stopped being late and started being wedged.
//
// **A cooldown, which the judge has none of.** The judge runs its rows
// one at a time, so "retry under pressure adds load" bounded itself.
// mast dispatches in parallel: a fan-out of eight specialists that all
// meet the same shed would, with no cooldown, answer it with eight
// retries two seconds later — precisely the objection. One retry per
// minute per process is the answer, and it is process-wide rather than
// per-model for the same reason: a specialist's `model:` override
// builds its own model.LLM, so per-instance state would give a roster
// of eight one cooldown each and no cooldown at all.
//
// **Still no jitter.** The judge declined it because there was one
// serial caller and nothing to desynchronize. Under the cooldown there
// is at most one retry in flight per window, which is the same
// conclusion by the stronger route: jitter spreads a thundering herd,
// and the cooldown has already deleted it.
func ProductionConfig() Config {
	return Config{
		Backoff:  []time.Duration{2 * time.Second},
		Cooldown: time.Minute,
	}
}

// JudgeConfig is the eval tier's policy: four attempts total, spread
// over about half a minute, no cooldown.
//
// Sized against what a Vertex per-region quota window actually is:
// quota refills on the order of a minute, so a sub-second retry storm
// would burn the budget inside one exhausted window and report the same
// failure faster. It is bounded rather than open-ended because a
// nightly with a 90-minute timeout and thirty-one sequential metered
// rows cannot afford to wait out a real outage — it should fail and say
// the board is short.
//
// No cooldown, because the tier's rows are sequential and each one is a
// separate measurement: suppressing the second row's retry because the
// first row used one would lose a row to bookkeeping.
func JudgeConfig() Config {
	return Config{Backoff: []time.Duration{3 * time.Second, 9 * time.Second, 27 * time.Second}}
}

// Policy is one retry policy and the counters it has accumulated.
// Safe for concurrent use; a shared policy is the point.
type Policy struct {
	backoff  []time.Duration
	cooldown time.Duration
	onWait   func(string, int, time.Duration, error)
	sleep    func(context.Context, time.Duration) error
	now      func() time.Time

	mu        sync.Mutex
	observer  func(Event)
	retries   int
	declined  int
	waited    time.Duration
	lastRetry time.Time
}

// New builds a policy from cfg.
func New(cfg Config) *Policy {
	p := &Policy{
		backoff:  cfg.Backoff,
		cooldown: cfg.Cooldown,
		onWait:   cfg.OnWait,
		observer: cfg.Observer,
		sleep:    cfg.Sleep,
		now:      cfg.Now,
	}
	if p.sleep == nil {
		p.sleep = sleepCtx
	}
	if p.now == nil {
		p.now = time.Now
	}
	return p
}

var (
	sharedOnce sync.Once
	shared     *Policy
)

// Shared is the process-wide production policy, the one
// compose.BuildModel wraps every real provider model in.
//
// A package-level singleton, which is not a shape this codebase reaches
// for lightly. It is the right one here because the cooldown is the
// part that has to be shared: a cooldown scoped to anything smaller
// than the process does not bound what the process sends, which is the
// only thing a provider shedding load can observe.
func Shared() *Policy {
	sharedOnce.Do(func() { shared = New(ProductionConfig()) })
	return shared
}

// SetObserver installs the terminal-event hook, replacing any previous
// one. Last writer wins, and the expected caller is the host at startup
// — cmd/mast, once, when it knows its workload name and has a registry
// to count into.
func (p *Policy) SetObserver(fn func(Event)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observer = fn
}

// Stats reports what this policy has spent: retries served, retries the
// cooldown declined, and total time waited.
//
// Reported, not swallowed. A retry nobody can see turns a provider
// under sustained pressure into a green board, which is the failure
// where a measurement quietly stops measuring.
func (p *Policy) Stats() (retries, declined int, waited time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.retries, p.declined, p.waited
}

// Wrap returns m driven under this policy.
//
// It returns model.LLM rather than *retryingLLM on purpose. A nil model
// has to come back as a nil *interface*, so that a constructor's
// `m == nil` guard still fires — returning a typed nil pointer would
// move the failure to a dereference with no context attached.
func (p *Policy) Wrap(m adkmodel.LLM) adkmodel.LLM {
	if m == nil {
		return nil
	}
	return &retryingLLM{policy: p, inner: m}
}

// PolicyOf reports the policy driving m, or nil if m is not wrapped.
//
// Exists so a caller can assert that a model it did not construct is
// resilient. "Is this path retrying?" is otherwise only answerable by
// pointing a real provider at a real quota, which is the check nobody
// runs — and an unwired retry is indistinguishable from a wired one
// until the night it was needed.
func PolicyOf(m adkmodel.LLM) *Policy {
	if r, ok := m.(*retryingLLM); ok {
		return r.policy
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryingLLM is one model under a policy. The state lives on the
// policy, not here, so two models sharing a policy share its cooldown.
type retryingLLM struct {
	policy *Policy
	inner  adkmodel.LLM
}

// Name reports the wrapped model's name unchanged.
//
// Unchanged is load-bearing rather than lazy: the board prints the
// model under test, the cost check prices calls by model name, and
// pkg/budget meters against it. A wrapper that renamed the model would
// make all three describe a model nobody can buy.
func (r *retryingLLM) Name() string { return r.inner.Name() }

// GenerateContent calls the wrapped model, retrying a transient failure
// that happened before the model said anything.
//
// "Before the model said anything" is the whole safety argument. A
// stream that already yielded a response has already handed the caller
// content; replaying the call would yield that content twice, and ADK
// assembles those yields into one turn. So the retry window closes the
// instant the first response is yielded, and a mid-stream 429 is
// reported like any other error. Under StreamingModeNone this costs
// nothing — the turn is a single yield — but the wrapper is not allowed
// to assume its caller, and the daemon's attach surface does stream.
func (r *retryingLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	// The first inner call happens HERE, not inside the closure, so that
	// calling this method has the same effects at the same moment as
	// calling the model it wraps. A model may do its work when
	// GenerateContent is invoked rather than when the sequence is walked
	// — mast's former Gemini layer applied the built-in-tool gate, a
	// security boundary, to the request at that moment — and a wrapper
	// that deferred everything to first iteration would move that work to
	// a later moment, or erase it for a caller that asks for a sequence it
	// does not walk. core-models' adapters build their request from a
	// copy and leave the caller's alone; the wrapper keeps the eager call
	// because it may not assume which model it wraps.
	first := r.inner.GenerateContent(ctx, req, stream)
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		var (
			attempts int
			waited   time.Duration
			lastErr  error
		)
		// Emitted on the way out rather than at each branch, so a call
		// accounts for its brush with a rejection exactly once.
		//
		// The gate is `lastErr != nil`, which is what keeps this family
		// to the three calls in thirty-one that matter: a call that
		// never met a rejection says nothing, and its denominator lives
		// in mast_model_calls_total.
		report := func(out Outcome) {
			if lastErr == nil {
				return
			}
			r.policy.emit(Event{
				Model: r.inner.Name(), Outcome: out,
				Attempts: attempts, Waited: waited, Err: lastErr,
			})
		}
		seq := first
		for attempt := 0; ; attempt++ {
			var (
				yielded bool
				failure error
			)
			for resp, err := range seq {
				if err != nil && !yielded {
					// Hold it: it is only an error to report if the
					// retry below decides not to take it.
					failure = err
					break
				}
				yielded = true
				if !yield(resp, err) {
					// The consumer walked away mid-stream. If a retry
					// got us here it did its job — content reached the
					// caller — so the outcome is the same either way.
					report(Recovered)
					return
				}
			}
			if failure == nil {
				report(Recovered)
				return
			}
			lastErr = failure
			wait, decision := r.policy.next(ctx, attempt, failure)
			switch decision {
			case decisionDeclined:
				yield(nil, failure)
				report(Declined)
				return
			case decisionStop:
				yield(nil, failure)
				// Only a call that actually bought a retry can have
				// exhausted one. A plain 400 is not an exhausted retry
				// schedule, and labelling it as one would put every
				// malformed request in the family an operator watches
				// for provider pressure.
				if attempts > 0 {
					report(Exhausted)
				}
				return
			}
			if r.policy.onWait != nil {
				r.policy.onWait(r.inner.Name(), attempt+1, wait, failure)
			}
			if err := r.policy.sleep(ctx, wait); err != nil {
				// The wait was cancelled. Report the provider's error
				// rather than the context's: the caller wants to know
				// why the call failed, not why we stopped waiting.
				yield(nil, failure)
				if attempts > 0 {
					report(Exhausted)
				}
				return
			}
			attempts++
			waited += wait
			r.policy.record(wait)
			seq = r.inner.GenerateContent(ctx, req, stream)
		}
	}
}

// decision is what next decided about one failure.
type decision int

const (
	decisionRetry    decision = iota // go again after the returned wait
	decisionStop                     // report the provider's error as-is
	decisionDeclined                 // the cooldown refused an otherwise-eligible retry
)

// next decides whether attempt's failure earns another try.
func (p *Policy) next(ctx context.Context, attempt int, err error) (time.Duration, decision) {
	if attempt >= len(p.backoff) {
		return 0, decisionStop
	}
	// A cancelled run is not a transient provider error, and retrying it
	// would spend the backoff schedule discovering that three more times.
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0, decisionStop
	}
	if !isRetryable(err) {
		return 0, decisionStop
	}
	if !p.claim() {
		return 0, decisionDeclined
	}
	return p.backoff[attempt], decisionRetry
}

// claim takes the cooldown's permission to serve a retry, or reports
// that the window is still closed.
//
// The claim is taken BEFORE the wait rather than after, so eight calls
// meeting one shed at the same instant produce one retry and seven
// immediate failures — not eight retries that each discover the others
// two seconds later.
func (p *Policy) claim() bool {
	if p.cooldown == 0 {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if !p.lastRetry.IsZero() && now.Sub(p.lastRetry) < p.cooldown {
		p.declined++
		return false
	}
	p.lastRetry = now
	return true
}

func (p *Policy) record(waited time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.retries++
	p.waited += waited
}

func (p *Policy) emit(ev Event) {
	p.mu.Lock()
	fn := p.observer
	p.mu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

// isRetryable reports whether err is the provider saying "not now".
//
// Typed, for every provider mast can be pointed at:
// google.golang.org/genai returns APIError by value and ADK's Gemini
// path wraps it with %w; anthropic-sdk-go returns *anthropic.Error.
// Either way errors.As reaches the status code without reading English.
//
// There is no string fallback on purpose. Matching "429" or "resource
// exhausted" in a message would also match a model *describing* one —
// the judge corpus is thirty-one Kubernetes incidents, several of them
// about exhausted resources, and the grader is handed those responses
// verbatim. In production the same hazard is worse, not better: a
// workload whose whole job is triaging a cluster will put the phrase in
// a tool result. A classifier that can be fooled by its own payload is
// worse than one that misses a provider we have not met yet: a provider
// whose errors this cannot read simply gets no retries, which is
// exactly the pre-#452 behaviour.
func isRetryable(err error) bool {
	var geminiErr genai.APIError
	if errors.As(err, &geminiErr) {
		return retryableCodes[geminiErr.Code]
	}
	var anthropicErr *anthropic.Error
	if errors.As(err, &anthropicErr) {
		return retryableCodes[anthropicErr.StatusCode]
	}
	// core-models' adapters (provider profiles) return errors that state
	// their HTTP status. Still typed, still no string matching. Those
	// adapters have already retried at the HTTP layer, honoring the
	// provider's Retry-After; this is the one outer retry every model
	// gets.
	var status interface{ HTTPStatus() int }
	if errors.As(err, &status) {
		return retryableCodes[status.HTTPStatus()]
	}
	return false
}
