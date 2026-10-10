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
	"iter"
	"maps"
	"strings"
	"sync"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
)

// Default retry timings. Both are deliberately modest; see RetryPolicy
// for why the cooldown matters more than the backoff.
const (
	DefaultRetryBackoff  = 2 * time.Second
	DefaultRetryCooldown = 30 * time.Second
)

// RetryBurst is how many retries the shared budget holds at once
// (#1039). The budget refills at one per Cooldown, so the steady-state
// added load is unchanged from the single-timestamp guard this replaces;
// the burst is what a correlated rejection can draw down.
//
// Three, because 429s arrive correlated by construction. A provider
// shedding load rejects several concurrent callers within seconds of
// each other, and a one-rescue-per-window budget hands that rescue to
// whichever caller was rejected first and abandons the rest — measured
// on a live cluster as thirteen retries that rescued their caller and
// one suppression that killed a delegation, which is the exact failure
// #935 was opened to remove. Three covers a parent plus two subagents,
// the largest correlated burst the drill archive shows, at a worst case
// of three extra requests per window rather than one.
const RetryBurst = 3

// ProviderRetryMetadataKey marks the response a retry recovered with
// (#1206). Its value is a map with "outcome" ("recovered"), "attempts"
// and "error" (the rejection the retry fired for). It is stamped on the
// first NON-partial response after recovery, because that is the one
// ADK persists as the session event; a partial chunk never reaches the
// transcript, and the stream aggregator builds its final response
// fresh, so a stamp on a chunk would be lost.
const ProviderRetryMetadataKey = "provider_retry"

// RetryPrefix returns the text Error puts before the provider error,
// so a surface that replaces the message — the turn-error frame's fixed
// text for a timeout or a cancel — can keep it.
func (e *RetryError) RetryPrefix() string {
	return strings.TrimSuffix(e.Error(), e.Err.Error())
}

// RetryError is how a retry that did not rescue its call reaches the
// caller (#1206): the rejection, persisted after one retry, abandoned
// during the backoff, or not retried because the budget was spent.
//
// Before it, the daemon log was the only place a retry existed: a turn
// error read the same whether or not a retry had been tried, and the
// 2026-09-13 batch counted 13 retries in the log and 0 in 21
// transcripts. The outcome is a prefix, not a suffix, because the
// turn-error frame keeps only the first 240 characters and a provider
// error is often longer than that; and it names no word the turn-error
// classifier keys on, so the frame's kind and code are the
// rejection's own. Unwrap keeps errors.As / errors.Is working on the
// provider error underneath.
//
// Two outcomes are not the rejection at all, and still name the retry
// because the daemon log counted it: "failed" is a retry answered by a
// different error (a 400, an empty response), and "interrupted" is a
// retry that recovered and whose stream then failed before the final
// response that would have carried the ProviderRetryMetadataKey stamp.
type RetryError struct {
	// Outcome is "persisted", "abandoned", "skipped", "failed" or
	// "interrupted".
	Outcome string
	Err     error
}

func (e *RetryError) Error() string {
	switch e.Outcome {
	case "skipped":
		return "provider retry skipped, budget spent: " + e.Err.Error()
	case "failed":
		return "provider retry failed with another error: " + e.Err.Error()
	case "interrupted":
		return "provider retry interrupted after recovering: " + e.Err.Error()
	default:
		return "provider retry " + e.Outcome + ": " + e.Err.Error()
	}
}

func (e *RetryError) Unwrap() error { return e.Err }

type sideCallKey struct{}

// AsSideCall marks ctx as a one-shot internal call — the approver, the
// compaction summarizer, a session title, a /btw question — so a retry
// it triggers logs as one (#1206). A side call's response is never a
// session event and its error never a turn error, so its retry has no
// transcript surface by construction; labelling the log line lets box
// A2's counter tell those retries from the ones a transcript must show,
// instead of failing the run on them.
func AsSideCall(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, sideCallKey{}, name)
}

// SideCallName returns the name AsSideCall gave ctx, or "".
func SideCallName(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	name, _ := ctx.Value(sideCallKey{}).(string)
	return name
}

// sideCallPrefix is the log prefix for a call AsSideCall marked.
func sideCallPrefix(ctx context.Context) string {
	if name := SideCallName(ctx); name != "" {
		return "side call (" + name + "): "
	}
	return ""
}

// retryOutcome is what became of a retry that fired, for the one
// summary line Wrap logs on the way out. outcomeUnset is a real
// answer, not a missing one: it means the retry neither recovered nor
// persisted, which is what a consumer that stopped reading looks like.
type retryOutcome int

const (
	outcomeUnset retryOutcome = iota
	outcomeRecovered
	outcomePersisted
	outcomeAbandoned
	outcomeFailed
)

// RetryPolicy retries a streaming model call once when the provider
// rejects it transiently, and does nothing otherwise.
//
// # Why this exists
//
// The evidence is the GKE drill archive (#935). Across 38 archived runs
// against a live cluster, four lost a subagent delegation to a Vertex
// 429 RESOURCE_EXHAUSTED — three of them inside a single 20-run batch,
// so ~15% of that batch. In three of the four the child died on its
// very first model call, having recorded a plan and read nothing, and
// the parent then quietly did the cluster work itself. So the visible
// symptom is not a failed run. It is a run that silently loses the
// context isolation delegation exists to provide, at full parent-context
// cost, and in one of the four the operator was never told.
//
// # Why retrying is safe here, which is not obvious
//
// A retry that fires while the provider is shedding load adds requests
// at exactly the wrong moment, and that objection is the reason this
// was split out of #898 rather than shipped with it. The archive
// answers it: in 4 of 4 cases the parent's next model-backed call — same
// session, same model, 3 to 4 seconds later — succeeded. These are
// momentary rejections inside a window the provider is otherwise
// serving, which is the case a single retry is for.
//
// The counter-evidence is real and shapes the guard: two of the four
// arrived 8 minutes apart at the tail of a sustained batch, so
// cumulative pressure exists even though no individual rejection
// persisted. That is why the budget is not optional. It bounds the
// extra load this can generate to a steady state of one additional
// request per Cooldown per policy, whatever the provider is doing — so
// a genuine shed, where every call fails, costs a few retries and then
// degrades to plain pass-through instead of doubling traffic.
//
// The budget is a token bucket, not a single timestamp (#1039). A
// timestamp is right for the steady state and wrong for the burst: 429s
// are correlated, several concurrent callers get rejected within
// seconds of each other, and a one-rescue-per-window budget gives that
// rescue to whichever was rejected first and abandons the rest. See
// RetryBurst.
//
// # What it will not do
//
// It retries only when NO usable content reached the caller. Once a
// response with content has been yielded, a retry would duplicate it,
// so from that point the stream is pass-through and a late error
// surfaces unchanged. Buffer-and-drop-partials is lifted from the Gemini
// cache-eviction wrapper, which solved the same problem for a different
// trigger: chunks are held until the retry decision is moot, and
// discarded if the retry fires, so the retry supersedes any partial
// content rather than appending to it.
//
// One retry. Two attempts total, never more.
//
// # Per-provider predicates over a shared mechanism
//
// IsTransient is supplied by the adapter because providers do not spell
// transience the same way and some cannot distinguish it at all. The
// mechanism — buffering, the attempt cap, the cooldown, the backoff — is
// identical for everyone, and a Gemini-only version of it would leave
// the second shipped provider permanently less resilient.
//
// A zero RetryPolicy, or one with a nil IsTransient, is a no-op
// pass-through. That is the intended behaviour for a provider that
// cannot tell a transient rejection from a permanent one: guessing is
// worse than not retrying.
//
// A RetryPolicy is safe for concurrent use and is meant to be shared
// across the calls of one provider instance — the budget is only
// meaningful if it is.
type RetryPolicy struct {
	// IsTransient reports whether err is a transient provider
	// rejection worth one retry. Nil disables retrying entirely.
	IsTransient func(error) bool

	// IsTransientAfterSuccess reports whether err is a rejection that
	// is ambiguous on its own and is worth one retry only after the
	// calling session has already been served (#1247). It is consulted
	// only for a call whose context carries a marked PriorSuccess and
	// is not a side call; anywhere else the error is judged by
	// IsTransient alone, which is today's behaviour. A retry it licenses
	// spends the same budget, logs the same lines and surfaces the same
	// way as any other. Nil disables it.
	IsTransientAfterSuccess func(error) bool

	// Backoff is how long to wait before the retry. Zero means
	// DefaultRetryBackoff.
	Backoff time.Duration

	// Cooldown is the refill interval of this policy's retry budget:
	// one retry is returned to the bucket per Cooldown, up to a
	// ceiling of RetryBurst. Zero means DefaultRetryCooldown. A
	// negative value disables the guard, which is not recommended and
	// exists so a test can exercise back-to-back retries.
	Cooldown time.Duration

	// Log receives one line per retry decision, so a recovered failure
	// is visible in the daemon log rather than silent. Nil discards.
	Log func(format string, args ...any)

	// Test seams. Nil means real time.
	now   func() time.Time
	sleep func(context.Context, time.Duration) bool

	mu sync.Mutex
	// tokens is the retry budget, in whole retries; lastRefill is when
	// it was last brought up to date. A zero lastRefill means the
	// bucket has never been touched and starts full.
	tokens     float64
	lastRefill time.Time
}

// Wrap returns an iterator that runs fn, and on a transient failure
// that produced no usable content, runs it once more.
//
// fn is an iterator FACTORY rather than an iterator: a retry has to
// re-invoke the underlying call, and an already-constructed
// iter.Seq2 cannot be replayed.
func (p *RetryPolicy) Wrap(ctx context.Context, fn func() iter.Seq2[*adkmodel.LLMResponse, error]) iter.Seq2[*adkmodel.LLMResponse, error] {
	if p == nil || p.IsTransient == nil {
		return fn()
	}
	const maxAttempts = 2
	isTransient := p.transientFor(ctx)
	pfx := sideCallPrefix(ctx)
	logf := func(format string, args ...any) { p.logf(pfx+format, args...) }

	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		type pending struct {
			resp *adkmodel.LLMResponse
			err  error
		}

		// Outcome reporting is a defer, not a line at the end of the
		// happy path (#1039). This iterator has eight `return`s and
		// six of them are a consumer that stopped reading mid-flush,
		// so logging where the stream happens to finish tidily meant
		// most real retries reported nothing at all: on a live GKE
		// batch, 10 of 13 retries that rescued their caller left no
		// record of having done so, which is most of the point of
		// retrying visibly. A defer fires from every exit.
		//
		// retriedFor is non-nil exactly when a retry fired, and is the
		// error it fired for; nothing is logged unless it is set,
		// because a call that never retried has no outcome to report.
		var retriedFor error
		outcome, outcomeAttempt := outcomeUnset, 0
		budgetSpent := false

		// out is yield for the content path. After a recovery it stamps
		// the first non-partial response — the one that becomes the
		// session event — so the transcript carries the retry (#1206).
		stamped := false
		out := func(resp *adkmodel.LLMResponse, err error) bool {
			if outcome == outcomeRecovered && !stamped {
				resp, err, stamped = recordRecovery(resp, err, retriedFor, outcomeAttempt)
			}
			return yield(resp, err)
		}
		defer func() {
			if retriedFor != nil {
				logOutcome(logf, outcome, outcomeAttempt, maxAttempts, p.backoff(), retriedFor)
			}
		}()

		for attempt := 1; attempt <= maxAttempts; attempt++ {
			var buf []pending
			var transient error
			flushed := false
			hardErr := false

			for resp, err := range fn() {
				if flushed {
					if !out(resp, err) {
						return
					}
					continue
				}
				if err != nil && isTransient(err) {
					// Hold it. Whether this is retried or surfaced is
					// decided once the iteration has ended, because a
					// stream that goes on to produce content has
					// recovered on its own and must not be retried.
					transient = err
					continue
				}
				if err == nil && resp != nil && resp.Content != nil && len(resp.Content.Parts) > 0 {
					// Past the point where a retry decision is
					// meaningful: content is about to reach the caller
					// and cannot be taken back.
					if attempt > 1 && outcome == outcomeUnset {
						// Recorded BEFORE the first yield, deliberately.
						// The retry has already succeeded by the time
						// usable content exists; whether the consumer
						// goes on to read all of it is a separate
						// question, and answering it here would lose
						// the line for every early stop.
						outcome, outcomeAttempt = outcomeRecovered, attempt
					}
					for _, b := range buf {
						if !out(b.resp, b.err) {
							return
						}
					}
					buf = nil
					flushed = true
					if !out(resp, err) {
						return
					}
					continue
				}
				// Not-yet-usable chunk, or a NON-transient error.
				// Buffer it: if the stream recovers these still belong
				// to the caller, and if it does not they bubble on the
				// give-up flush. A non-transient error deliberately
				// does not trigger the retry.
				if err != nil {
					// And it suppresses one. A retry discards the
					// buffer, so retrying past a real error would
					// silently drop it — the caller would be told
					// about the rate limit and never about the
					// malformed request that actually killed the call.
					hardErr = true
				}
				buf = append(buf, pending{resp, err})
			}

			if flushed {
				return
			}
			if transient != nil && !hardErr && attempt < maxAttempts {
				if !p.allowRetry() {
					budgetSpent = true
					logf("transient provider error (%v) NOT retried: the shared retry budget is spent (burst %d, one refill per %s)", transient, RetryBurst, p.cooldown())
				} else {
					retriedFor = transient
					logf("transient provider error (%v) — retrying once after %s", transient, p.backoff())
					if !p.wait(ctx, p.backoff()) {
						// Context died during the backoff. Surface the
						// original error rather than a context one:
						// the provider rejection is what the caller
						// needs to see, and ctx.Err() is available to
						// them anyway.
						outcome = outcomeAbandoned
						for _, b := range buf {
							if !yield(b.resp, b.err) {
								return
							}
						}
						yield(nil, &RetryError{Outcome: "abandoned", Err: transient})
						return
					}
					continue
				}
			}

			// Give up. Flush anything buffered — it may include real
			// non-transient errors that must not be swallowed — then
			// surface the transient error if that is how it ended.
			for _, b := range buf {
				if attempt > 1 && transient == nil && outcome == outcomeUnset {
					var done bool
					if b.resp, b.err, done = recordOtherAnswer(b.resp, b.err, retriedFor, attempt); done {
						outcome = outcomeFailed
					}
				}
				if !yield(b.resp, b.err) {
					return
				}
			}
			if transient != nil {
				if attempt > 1 {
					outcome = outcomePersisted
				}
				yield(nil, surfaced(transient, attempt > 1, budgetSpent))
			}
			return
		}
	}
}

// transientFor returns the predicate Wrap judges this call's errors by:
// IsTransient, widened by IsTransientAfterSuccess when the call's
// session has already been served (#1247). The record is read when an
// error arrives rather than when Wrap is called, so the answer is the
// session's state at the moment of the rejection.
func (p *RetryPolicy) transientFor(ctx context.Context) func(error) bool {
	if p.IsTransientAfterSuccess == nil {
		return p.IsTransient
	}
	return func(err error) bool {
		if p.IsTransient(err) {
			return true
		}
		return priorCallSucceeded(ctx) && p.IsTransientAfterSuccess(err)
	}
}

// recordRecovery puts a recovered retry on the transcript (#1206): a
// stamp on the first non-partial response, the one that becomes the
// session event — or, when the stream fails before that response
// arrives, on the error that ended it. done reports that the retry is
// now recorded and nothing later may record it again.
func recordRecovery(resp *adkmodel.LLMResponse, err, retriedFor error, attempts int) (_ *adkmodel.LLMResponse, _ error, done bool) {
	switch {
	case err != nil:
		return resp, &RetryError{Outcome: "interrupted", Err: err}, true
	case resp != nil && !resp.Partial:
		return stampRetryAs(resp, "recovered", retriedFor, attempts), nil, true
	}
	return resp, err, false
}

// recordOtherAnswer records a retry answered by something other than
// usable content: a different error (a 400, an empty response), which
// is wrapped, or a final response with no parts — a SAFETY or
// MAX_TOKENS finish — which ADK still persists as the session event,
// so it is stamped (#1206). The log counted the retry either way.
func recordOtherAnswer(resp *adkmodel.LLMResponse, err, retriedFor error, attempts int) (_ *adkmodel.LLMResponse, _ error, done bool) {
	switch {
	case err != nil:
		return resp, &RetryError{Outcome: "failed", Err: err}, true
	case resp != nil && !resp.Partial:
		return stampRetryAs(resp, "no content", retriedFor, attempts), nil, true
	}
	return resp, err, false
}

// surfaced is the error a transient rejection reaches the caller as:
// named as a retry outcome when the log counted one, bare otherwise.
func surfaced(transient error, retried, budgetSpent bool) error {
	switch {
	case retried:
		return &RetryError{Outcome: "persisted", Err: transient}
	case budgetSpent:
		return &RetryError{Outcome: "skipped", Err: transient}
	}
	return transient
}

// logOutcome writes the one outcome line a retry that fired gets.
func logOutcome(logf func(string, ...any), outcome retryOutcome, attempt, maxAttempts int, backoff time.Duration, retriedFor error) {
	switch outcome {
	case outcomeRecovered:
		logf("transient provider error recovered on retry (attempt %d/%d)", attempt, maxAttempts)
	case outcomePersisted:
		logf("transient provider error persisted after retry — surfacing to caller: %v", retriedFor)
	case outcomeAbandoned:
		logf("transient provider error retry abandoned: context ended during the %s backoff, surfacing the original error: %v", backoff, retriedFor)
	case outcomeFailed:
		logf("transient provider error retry was answered by something other than content — surfacing it (original error: %v)", retriedFor)
	default:
		logf("transient provider error retry ended with no outcome: the consumer stopped reading, or the retry returned nothing usable (original error: %v)", retriedFor)
	}
}

// stampRetryAs returns a copy of resp carrying ProviderRetryMetadataKey
// with the given outcome. A copy, so the provider's own response value
// is never mutated.
func stampRetryAs(resp *adkmodel.LLMResponse, outcome string, retriedFor error, attempts int) *adkmodel.LLMResponse {
	cp := *resp
	cp.CustomMetadata = maps.Clone(resp.CustomMetadata)
	if cp.CustomMetadata == nil {
		cp.CustomMetadata = map[string]any{}
	}
	msg := retriedFor.Error()
	if len(msg) > 240 {
		msg = strings.ToValidUTF8(msg[:240], "")
	}
	cp.CustomMetadata[ProviderRetryMetadataKey] = map[string]any{
		"outcome":  outcome,
		"attempts": attempts,
		"error":    msg,
	}
	return &cp
}

// allowRetry reports whether a retry may fire now, spending a token
// from the shared budget if so. This is the whole rate-limit guard:
// RetryBurst retries in hand, refilling at one per cooldown, no matter
// how many calls are failing.
func (p *RetryPolicy) allowRetry() bool {
	cd := p.cooldown()
	if cd < 0 {
		return true
	}
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	t := now()

	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.lastRefill.IsZero():
		p.tokens = RetryBurst
	case t.After(p.lastRefill):
		p.tokens = min(RetryBurst, p.tokens+float64(t.Sub(p.lastRefill))/float64(cd))
	}
	p.lastRefill = t
	if p.tokens < 1 {
		return false
	}
	p.tokens--
	return true
}

// wait sleeps for d, reporting false if ctx ended first.
func (p *RetryPolicy) wait(ctx context.Context, d time.Duration) bool {
	if p.sleep != nil {
		return p.sleep(ctx, d)
	}
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *RetryPolicy) backoff() time.Duration {
	if p.Backoff == 0 {
		return DefaultRetryBackoff
	}
	return p.Backoff
}

func (p *RetryPolicy) cooldown() time.Duration {
	if p.Cooldown == 0 {
		return DefaultRetryCooldown
	}
	return p.Cooldown
}

func (p *RetryPolicy) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}
