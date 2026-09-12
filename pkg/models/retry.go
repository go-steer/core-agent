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
	"sync"
	"time"

	adkmodel "google.golang.org/adk/model"
)

// Default retry timings. Both are deliberately modest; see RetryPolicy
// for why the cooldown matters more than the backoff.
const (
	DefaultRetryBackoff  = 2 * time.Second
	DefaultRetryCooldown = 30 * time.Second
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
// persisted. That is why Cooldown is not optional. It bounds the extra
// load this can generate to one additional request per cooldown window
// per policy, whatever the provider is doing — so a genuine shed, where
// every call fails, costs one retry and then degrades to plain
// pass-through instead of doubling traffic.
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
// across the calls of one provider instance — the cooldown is only
// meaningful if it is.
type RetryPolicy struct {
	// IsTransient reports whether err is a transient provider
	// rejection worth one retry. Nil disables retrying entirely.
	IsTransient func(error) bool

	// Backoff is how long to wait before the retry. Zero means
	// DefaultRetryBackoff.
	Backoff time.Duration

	// Cooldown is the minimum interval between two retries from this
	// policy. Zero means DefaultRetryCooldown. A negative value
	// disables the guard, which is not recommended and exists so a
	// test can exercise back-to-back retries.
	Cooldown time.Duration

	// Log receives one line per retry decision, so a recovered failure
	// is visible in the daemon log rather than silent. Nil discards.
	Log func(format string, args ...any)

	// Test seams. Nil means real time.
	now   func() time.Time
	sleep func(context.Context, time.Duration) bool

	mu        sync.Mutex
	lastRetry time.Time
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

	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		type pending struct {
			resp *adkmodel.LLMResponse
			err  error
		}
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			var buf []pending
			var transient error
			flushed := false
			hardErr := false

			for resp, err := range fn() {
				if flushed {
					if !yield(resp, err) {
						return
					}
					continue
				}
				if err != nil && p.IsTransient(err) {
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
					for _, b := range buf {
						if !yield(b.resp, b.err) {
							return
						}
					}
					buf = nil
					flushed = true
					if !yield(resp, err) {
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
				if attempt > 1 {
					p.logf("transient provider error recovered on retry (attempt %d/%d)", attempt, maxAttempts)
				}
				return
			}
			if transient != nil && !hardErr && attempt < maxAttempts {
				if !p.allowRetry() {
					p.logf("transient provider error (%v) NOT retried: another retry fired within the %s cooldown", transient, p.cooldown())
				} else {
					p.logf("transient provider error (%v) — retrying once after %s", transient, p.backoff())
					if !p.wait(ctx, p.backoff()) {
						// Context died during the backoff. Surface the
						// original error rather than a context one:
						// the provider rejection is what the caller
						// needs to see, and ctx.Err() is available to
						// them anyway.
						for _, b := range buf {
							if !yield(b.resp, b.err) {
								return
							}
						}
						yield(nil, transient)
						return
					}
					continue
				}
			}

			// Give up. Flush anything buffered — it may include real
			// non-transient errors that must not be swallowed — then
			// surface the transient error if that is how it ended.
			for _, b := range buf {
				if !yield(b.resp, b.err) {
					return
				}
			}
			if transient != nil {
				if attempt > 1 {
					p.logf("transient provider error persisted after retry — surfacing to caller: %v", transient)
				}
				yield(nil, transient)
			}
			return
		}
	}
}

// allowRetry reports whether a retry may fire now, recording it if so.
// This is the whole rate-limit guard: one extra request per cooldown,
// no matter how many calls are failing.
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
	if !p.lastRetry.IsZero() && t.Sub(p.lastRetry) < cd {
		return false
	}
	p.lastRetry = t
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
