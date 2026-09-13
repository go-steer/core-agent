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

// Wake-loop health, and the one thing /healthz can honestly say about
// it (#978).
//
// A wake loop cannot die — it logs every turn error and goes back to
// blocking, which is right for liveness and useless for recovery. From
// outside, a daemon whose every turn fails looks exactly like a daemon
// with nothing to do: a running pod, a green readiness probe, and no
// work getting done. This file is the state that tells those apart.
//
// The reporting granularity is deliberate and is the one design
// decision here worth arguing about. A multi-session daemon hosts many
// loops, and the tempting shape — one HealthCheck per session — is
// wrong twice over: the check list would grow without bound under a
// kubelet re-probing it on a fixed period, and HealthCheck.Name is
// documented as a compile-time constant precisely because the endpoint
// is unauthenticated and a session id is not the caller's business. So
// the daemon registers ONE aggregate check and the per-session detail
// goes to the daemon log, where the healthz handler already writes the
// error text for a failing check and the operator reading it has
// already authenticated to the node.
//
// The aggregate fails only when every live loop is failing. One
// session whose RBAC was revoked must not pull a pod serving forty-nine
// healthy ones out of its Service — readiness is a routing decision,
// not a notification channel. "Every loop is failing with nothing
// getting done" is the condition the issue actually describes, and it
// is the one a readiness gate should act on.
//
// And it takes unhealthyAfterFailures consecutive failures, not one.
// The evidence from #935 is that a single provider 429 kills a turn
// roughly every six minutes under load; descheduling a pod for one of
// those would make the readiness gate a worse signal than no gate,
// because an operator who watches it flap learns to ignore it.
package runner

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// unhealthyAfterFailures is how many consecutive failing turns a loop
// must have before it counts against the aggregate. See the file
// comment: the readiness gate must not flap on one bad turn.
const unhealthyAfterFailures = 3

// LoopState is the coarse condition of one wake loop.
type LoopState string

const (
	// LoopIdle: parked on the wake channel with nothing to do. The
	// overwhelmingly common state, and indistinguishable from
	// LoopFailing to anything outside the process before #978.
	LoopIdle LoopState = "idle"
	// LoopWorking: a turn is running right now.
	LoopWorking LoopState = "working"
	// LoopFailing: consecutive turns have ended in a fault. The loop
	// is still listening and still accepting input — see WakeLoop for
	// why giving up is not an option it has.
	LoopFailing LoopState = "failing"
	// LoopStopped: ctx was cancelled and the loop returned. Its inbox
	// is closed; nothing more will run.
	LoopStopped LoopState = "stopped"
)

// LoopStatus is a point-in-time reading of one wake loop.
type LoopStatus struct {
	// Session is the loop's session id. Used for the daemon log and
	// never for the /healthz body.
	Session string
	State   LoopState
	// ConsecutiveFailures counts TURNS, not yielded errors: a turn
	// that yields three errors is one failure. Reset by any turn that
	// ends clean.
	ConsecutiveFailures int
	// LastErrorKind is the stable attach.ClassifyTurnError kind of the
	// most recent counted failure, not the provider's prose. The prose
	// can carry a DSN or a hostname; the kind is an enum.
	LastErrorKind string
	// TurnsObserved counts completed turns of every outcome. Zero means
	// the loop has never run one, which is the difference between "this
	// session is healthy" and "this session has never been asked to do
	// anything" — see LoopHealthSet.Err.
	TurnsObserved int
	// Since is when the loop entered State.
	Since time.Time
}

// LoopHealth is one wake loop's health, safe for concurrent reads
// while the loop writes it. The zero value is usable and reports an
// idle loop with no session name; WakeLoop fills the session in when
// it starts.
type LoopHealth struct {
	mu sync.Mutex
	st LoopStatus
}

// NewLoopHealth returns a handle for a loop on the named session.
func NewLoopHealth(session string) *LoopHealth {
	return &LoopHealth{st: LoopStatus{Session: session, State: LoopIdle, Since: time.Now()}}
}

// Snapshot returns the current reading. Nil-safe.
func (h *LoopHealth) Snapshot() LoopStatus {
	if h == nil {
		return LoopStatus{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.st
}

// setState moves to state, stamping Since only on a real transition so
// "failing since" means what it says rather than "failing as of the
// most recent failure".
func (h *LoopHealth) setState(state LoopState) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.st.State != state {
		h.st.State = state
		h.st.Since = time.Now()
	}
}

// turnOutcome is what one completed turn said about the loop's health.
type turnOutcome int

const (
	// turnClean: the turn yielded no errors at all. Evidence the loop
	// works, so it clears everything.
	turnClean turnOutcome = iota
	// turnObeyed: the turn's only errors were obeyed stops — an
	// operator interrupt, a guardrail halt (see countsAsFailure). It is
	// NOT evidence of health: nothing was attempted. Treating it as
	// clean would let an operator pressing stop during a real outage
	// silently reset the failure count and turn the aggregate green,
	// which is the one moment it most needs to stay red.
	turnObeyed
	// turnFailed: the turn ended in a fault the loop owns.
	turnFailed
)

// observeTurn records one COMPLETED turn and returns the resulting
// consecutive-failure count. kind is the attach kind of the last
// counted error and is read only for turnFailed.
func (h *LoopHealth) observeTurn(outcome turnOutcome, kind string) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.st.TurnsObserved++
	switch outcome {
	case turnFailed:
		h.st.ConsecutiveFailures++
		h.st.LastErrorKind = kind
	case turnClean:
		h.st.ConsecutiveFailures = 0
		h.st.LastErrorKind = ""
	case turnObeyed:
		// Leave the counters exactly as they were.
	}
	// The resting state has to be reinstated either way: the loop moved
	// to LoopWorking when the turn started, and leaving it there would
	// report a turn in flight for as long as the loop then sits parked.
	resting := LoopIdle
	if h.st.ConsecutiveFailures > 0 {
		resting = LoopFailing
	}
	if h.st.State != resting {
		h.st.State = resting
		h.st.Since = time.Now()
	}
	return h.st.ConsecutiveFailures
}

// setSession names a loop that was handed a bare &LoopHealth{}.
func (h *LoopHealth) setSession(sid string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.st.Session == "" {
		h.st.Session = sid
	}
	if h.st.Since.IsZero() {
		h.st.Since = time.Now()
	}
	if h.st.State == "" {
		h.st.State = LoopIdle
	}
}

// LoopHealthSet is the daemon's registry of live wake loops, and the
// thing the aggregate /healthz check reads. The zero value is ready to
// use and every method is nil-safe, so a host that wants no health
// reporting leaves the field unset.
type LoopHealthSet struct {
	mu    sync.Mutex
	loops map[*LoopHealth]struct{}
}

// Register adds a loop for the named session and returns it along with
// the func that removes it again. Call the remover when the loop
// returns: a stopped loop that stays registered would keep an evicted
// session's last failure in the aggregate forever, which is how a
// health signal becomes a thing operators learn to ignore.
func (s *LoopHealthSet) Register(session string) (*LoopHealth, func()) {
	h := NewLoopHealth(session)
	if s == nil {
		return h, func() {}
	}
	s.mu.Lock()
	if s.loops == nil {
		s.loops = map[*LoopHealth]struct{}{}
	}
	s.loops[h] = struct{}{}
	s.mu.Unlock()
	return h, func() {
		s.mu.Lock()
		delete(s.loops, h)
		s.mu.Unlock()
	}
}

// Snapshot returns a reading of every registered loop, ordered by
// session id so a log line is stable between probes.
func (s *LoopHealthSet) Snapshot() []LoopStatus {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	out := make([]LoopStatus, 0, len(s.loops))
	for h := range s.loops {
		out = append(out, h.Snapshot())
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out
}

// Err reports the aggregate condition: non-nil only when at least one
// loop is live and EVERY live loop has failed unhealthyAfterFailures
// turns in a row without a clean one since.
//
// The partial case returns nil on purpose. A readiness probe decides
// whether to route traffic to this pod, and one broken session is not
// a reason to stop serving the others — the operator learns about it
// from the daemon log, which is where the detail can safely go. See
// the file comment.
//
// Three details are easy to get wrong and all three are deliberate.
// The predicate is the failure COUNT, not LoopFailing: a loop that is
// mid-retry reads as LoopWorking, and a probe landing inside that turn
// must not see a broken session as recovered. A stopped loop is
// skipped rather than counted as healthy — it unregisters immediately
// in every in-tree host, but if one lingered, counting it would let a
// dead loop mask a genuine all-failing daemon. And a loop that has
// never completed a turn is skipped for the same reason in the other
// direction: it is not evidence of health, and counting it as such
// would silently disable this check on the deployment it was written
// for. A multi-session `--no-repl` daemon — every bundled GKE recipe —
// starts a bootstrap `default` session whose loop nothing ever drives,
// so "every live loop is failing" could never be true while it sat
// there reading idle.
func (s *LoopHealthSet) Err() error {
	if s == nil {
		return nil
	}
	var live, failing []LoopStatus
	for _, st := range s.Snapshot() {
		if st.State == LoopStopped || st.TurnsObserved == 0 {
			continue
		}
		live = append(live, st)
		if st.ConsecutiveFailures >= unhealthyAfterFailures {
			failing = append(failing, st)
		}
	}
	if len(live) == 0 || len(failing) != len(live) {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "all %d wake loop(s) are failing:", len(failing))
	for _, st := range failing {
		fmt.Fprintf(&b, " [session=%s failures=%d kind=%s since=%s]",
			st.Session, st.ConsecutiveFailures, st.LastErrorKind, st.Since.UTC().Format(time.RFC3339))
	}
	return errors.New(b.String())
}
