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

// The residency lease (docs/external-state-design.md decisions 1–3,
// #1192 phase 2).
//
// A Handle opened with WithResidencyLease holds a lease on every session
// tree it is the writer for. A tree becomes resident — the lease is
// taken — when this Handle creates a session in it, when a caller
// resumes it explicitly (AcquireResidency: the multi-session daemon
// does this for every tenant it builds), or on its first write if the
// tree was never resident here. Reading never takes a lease: scans that
// read many sessions (auto-continue, /parks, auto-resume) must not
// claim them. The lease is released on ReleaseResidency (eviction),
// Delete of the root, and Close; another Handle sharing the control
// store gets ErrSessionLocked for the tree until then, or until the
// lease goes stale and it steals it.
//
// A released tree is tombstoned: writes to it are refused until it is
// explicitly made resident again. Without that, a turn still in flight
// when its session was evicted, or a subagent not yet torn down, would
// silently take the lease back on its next write.
//
// The tree a lease covers is the session's RootOf with a known derived
// suffix removed (residencyRoot), so "<sid>:sub:…" subagent sessions and
// the "<sid>:digest" and "<sid>:mast-ops" trees share their session's
// lease, while unrelated caller-chosen IDs that merely contain ":" do
// not.
//
// The tombstone protects until the session is resumed again. A write
// from the evicted generation that is still running at that point — a
// turn eviction did not wait for, a subagent not yet torn down — passes
// the fence under the new lease, because session objects are not tied
// to a lease generation. That is a same-process overlap, not a
// cross-instance one, and it is pinned rather than fenced.
//
// Every write is fenced (decision 3). Agent Engine cannot reject a
// stale writer, so the holder refuses to write once its own deadline,
// SessionLock.ValidUntil, has passed or once Lost() has fired, and the
// write's context gets a deadline at the point a successor could steal
// the row, so a slow write — a quota retry, a cold reload — is cut off
// rather than landing after it. The window that remains is the one
// decision 3 accepts.
//
// Losing a lease stops the session rather than just logging (decision
// 2): the tree is fenced at once, and the registered handler — the
// per-tree one from WatchResidency, else the Handle's fallback from
// WithResidencyLost — evicts it or stops the process.
//
// Taking a lease tells the transcript to forget what it cached about
// the tree (TranscriptInvalidator), because another instance may have
// written to it since this one last held it.
//
// The lease is a row in the same agent_run_lock table as the run lock
// autonomous.Resume and auto-continue take, under its own key, so those
// callers are unchanged. While control lives in a local SQLite file the
// lease excludes only processes sharing that file; it becomes the
// cross-machine guarantee when the control store moves off SQLite
// (phase 4). The fence and the stop-on-loss are real either way.

package eventlog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// residencyKeyPrefix namespaces the residency lease's session_id in
// agent_run_lock so it never collides with a run lock on the same tree.
const residencyKeyPrefix = "residency:"

// ErrNotResident is returned by a write on a lease-holding Handle for a
// tree this instance is not the writer for: evicted, or never resumed.
var ErrNotResident = errors.New("eventlog: session is not resident on this instance")

// TranscriptInvalidator is an OPTIONAL Transcript extension: forget any
// cached view of the trees under root, so the next read goes to the
// store. A lease-holding Handle calls it whenever it takes a lease,
// since another instance may have written while this one did not hold
// it. A Transcript with no cache need not implement it.
type TranscriptInvalidator interface {
	Invalidate(appName, userID, root string)
}

// WithResidencyLease makes OpenTranscript hold a residency lease on the
// session trees it writes and fence every write on it. The shipped CLI
// sets it for the agentengine transcript (the #1192 compatibility table
// gates the lease on a `state` block).
func WithResidencyLease() Option {
	return func(o *openOpts) { o.residencyLease = true }
}

// WithResidencyLost sets the handler called when a tree's residency
// lease is lost and no per-tree handler (WatchResidency) claims it.
// stolen reports whether another holder took the row, as opposed to the
// control store going unreachable for the loss window. Without one, a
// loss only fences the tree.
func WithResidencyLost(f func(appName, userID, root string, stolen bool)) Option {
	return func(o *openOpts) { o.residencyLost = f }
}

// derivedTreeSuffixes are the suffixes this runtime appends to a
// session ID to make a tree of its own that still belongs to that
// session: the MCP digest store and the mast-compat ops stream.
var derivedTreeSuffixes = []string{":digest", ":mast-ops"}

// residencyRoot is the tree a session's lease covers: its RootOf, with a
// known derived suffix removed. It deliberately does not cut at the
// first ":", because a caller-chosen session ID (an A2A contextId, a
// library consumer's) may contain one, and two such sessions must not
// share a lease.
func residencyRoot(sessionID string) string {
	root := RootOf(sessionID)
	for _, suf := range derivedTreeSuffixes {
		if base, ok := strings.CutSuffix(root, suf); ok && base != "" {
			return base
		}
	}
	return root
}

type residencyKey struct{ app, user, root string }

type residentLease struct {
	lock *SessionLock
	done chan struct{} // closed when the lease leaves the map; stops the watcher
}

type residencyWatcher struct {
	f func(stolen bool)
}

// residency is the lease bookkeeping a lease-holding Handle carries.
type residency struct {
	h          *Handle
	fallback   func(appName, userID, root string, stolen bool)
	invalidate func(appName, userID, root string)

	mu       sync.Mutex
	leases   map[residencyKey]*residentLease
	released map[residencyKey]bool // tombstones: no implicit re-acquire
	watchers map[residencyKey]*residencyWatcher
	// pending holds, per key, a channel closed when an asynchronous
	// release has deleted the row. An acquire of the same key waits for
	// it: our own not-yet-deleted row would otherwise refuse us.
	pending map[residencyKey]chan struct{}
	closed  bool
}

func newResidency(h *Handle, fallback func(string, string, string, bool), invalidate func(string, string, string)) *residency {
	return &residency{
		h:          h,
		fallback:   fallback,
		invalidate: invalidate,
		leases:     make(map[residencyKey]*residentLease),
		released:   make(map[residencyKey]bool),
		watchers:   make(map[residencyKey]*residencyWatcher),
		pending:    make(map[residencyKey]chan struct{}),
	}
}

func keyOf(app, user, sessionID string) residencyKey {
	return residencyKey{app, user, residencyRoot(sessionID)}
}

// acquire makes the tree resident. explicit clears a tombstone; an
// implicit acquire (a first write) refuses a tombstoned tree.
func (r *residency) acquire(ctx context.Context, k residencyKey, explicit bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if l := r.leases[k]; l != nil {
		if !leaseLost(l.lock) {
			return nil
		}
		// A lost lease leaves the map and is released before we compete
		// again: after a stall its row still names us, and until it is
		// deleted our own new attempt would be refused by it. (Release
		// does not wait on a heartbeat here — the loop stopped when it
		// declared the loss — and leaves a stolen row alone.)
		delete(r.leases, k)
		close(l.done)
		_ = l.lock.Release()
	}
	if r.released[k] && !explicit {
		return fmt.Errorf("%w: %q was evicted from this instance", ErrNotResident, k.root)
	}
	if ch := r.pending[k]; ch != nil {
		// Our previous lease's row is still being deleted. The release
		// goroutine does not take r.mu, so waiting here cannot deadlock;
		// it is bounded by the lock's own heartbeat and delete timeouts.
		<-ch
		if r.pending[k] == ch {
			delete(r.pending, k)
		}
	}
	lock, err := r.h.AcquireLock(ctx, k.app, k.user, residencyKeyPrefix+k.root)
	if err != nil {
		return fmt.Errorf("eventlog: residency lease for %q: %w", k.root, err)
	}
	delete(r.released, k)
	l := &residentLease{lock: lock, done: make(chan struct{})}
	r.leases[k] = l
	if r.invalidate != nil {
		r.invalidate(k.app, k.user, k.root)
	}
	go r.watch(k, l)
	return nil
}

// dropLocked removes l from the map and releases it asynchronously, so
// a stalled control store cannot hold up the caller. Caller holds r.mu.
func (r *residency) dropLocked(k residencyKey, l *residentLease) {
	delete(r.leases, k)
	close(l.done)
	ch := make(chan struct{})
	r.pending[k] = ch
	go func() {
		_ = l.lock.Release()
		close(ch)
	}()
}

// watch waits for a lease to be lost or to leave the map, and on loss
// runs the tree's handler — but only if this lease is still the tree's
// current one, so a stale watcher never acts on a newer residency.
func (r *residency) watch(k residencyKey, l *residentLease) {
	select {
	case <-l.done:
		return
	case <-l.lock.Lost():
	}
	// stolen is written by the heartbeat goroutine before it closes
	// lost, so reading it after <-Lost() is ordered. A stall (stolen
	// false) is still a loss: the tree is fenced, and must be made
	// resident again to resume.
	stolen := l.lock.stolen
	r.mu.Lock()
	if r.leases[k] != l {
		r.mu.Unlock()
		return
	}
	var f func(bool)
	if w := r.watchers[k]; w != nil {
		f = w.f
	}
	fallback := r.fallback
	r.mu.Unlock()
	switch {
	case f != nil:
		f(stolen)
	case fallback != nil:
		fallback(k.app, k.user, k.root, stolen)
	}
}

// fence reports whether this Handle may write to sessionID's tree now,
// and the deadline the write must finish by: the earliest moment a
// successor could steal the lease (zero when unknown).
func (r *residency) fence(ctx context.Context, app, user, sessionID string) (time.Time, error) {
	k := keyOf(app, user, sessionID)
	r.mu.Lock()
	l := r.leases[k]
	r.mu.Unlock()
	if l == nil {
		// First write to a tree never resident here (a daemon-level
		// stream, say). A tombstoned tree is refused inside acquire.
		if err := r.acquire(ctx, k, false); err != nil {
			if errors.Is(err, ErrNotResident) {
				return time.Time{}, err
			}
			return time.Time{}, fmt.Errorf("%w: %w", ErrNotResident, err)
		}
		r.mu.Lock()
		l = r.leases[k]
		r.mu.Unlock()
		if l == nil {
			return time.Time{}, fmt.Errorf("%w: %q", ErrNotResident, k.root)
		}
	}
	if leaseLost(l.lock) {
		return time.Time{}, fmt.Errorf("eventlog: write to %q refused: %w", k.root, ErrLeaseLost)
	}
	until := l.lock.ValidUntil()
	if !until.IsZero() && time.Now().After(until) {
		return time.Time{}, fmt.Errorf("eventlog: write to %q refused, lease unconfirmed since %s: %w",
			k.root, until.Format(time.RFC3339Nano), ErrLeaseLost)
	}
	if until.IsZero() {
		return time.Time{}, nil
	}
	// ValidUntil sits one heartbeat interval (or staleAfter/2) short of
	// steal eligibility; the write may use that margin, not more.
	return until.Add(l.lock.staleAfter - l.lock.lossWindow()), nil
}

// release lets go of sessionID's tree and tombstones it.
func (r *residency) release(app, user, sessionID string) {
	k := keyOf(app, user, sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released[k] = true
	if l := r.leases[k]; l != nil {
		r.dropLocked(k, l)
	}
}

// forget releases the tree like release but leaves no tombstone: the
// session was deleted, so there is nothing left to protect.
func (r *residency) forget(app, user, sessionID string) {
	k := keyOf(app, user, sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.released, k)
	if l := r.leases[k]; l != nil {
		r.dropLocked(k, l)
	}
}

// closeAll releases every lease and waits for releases already under
// way, so no row outlives the database: Handle.Close calls it before
// the database the leases heartbeat against goes away.
func (r *residency) closeAll() {
	r.mu.Lock()
	r.closed = true
	all := r.leases
	r.leases = make(map[residencyKey]*residentLease)
	pending := r.pending
	r.pending = make(map[residencyKey]chan struct{})
	r.mu.Unlock()
	for _, l := range all {
		close(l.done)
		_ = l.lock.Release()
	}
	for _, ch := range pending {
		<-ch
	}
}

func leaseLost(l *SessionLock) bool {
	select {
	case <-l.Lost():
		return true
	default:
		return false
	}
}

// AcquireResidency makes sessionID's tree resident on this Handle: it
// takes the lease (ErrSessionLocked if another instance holds it) and
// clears a tombstone left by an earlier eviction. Call it when resuming
// a session to write to it. No-op on a Handle without
// WithResidencyLease.
func (h *Handle) AcquireResidency(ctx context.Context, appName, userID, sessionID string) error {
	if h == nil || h.residency == nil {
		return nil
	}
	return h.residency.acquire(ctx, keyOf(appName, userID, sessionID), true)
}

// ReleaseResidency gives up this Handle's residency lease on
// sessionID's tree and refuses further writes to it until it is made
// resident again. Call it when the session is evicted. It returns
// without waiting for the control store; the row is deleted in the
// background. No-op on a Handle without WithResidencyLease.
func (h *Handle) ReleaseResidency(appName, userID, sessionID string) {
	if h == nil || h.residency == nil {
		return
	}
	h.residency.release(appName, userID, sessionID)
}

// WatchResidency registers f to run when this Handle loses the residency
// lease on sessionID's tree, in place of the WithResidencyLost fallback.
// The returned func unregisters exactly this registration, so a later
// one for the same tree is left alone. A multi-session daemon uses it to
// evict exactly the tenant that lost its lease. No-op on a Handle
// without WithResidencyLease.
func (h *Handle) WatchResidency(appName, userID, sessionID string, f func(stolen bool)) func() {
	if h == nil || h.residency == nil || f == nil {
		return func() {}
	}
	k := keyOf(appName, userID, sessionID)
	w := &residencyWatcher{f: f}
	r := h.residency
	r.mu.Lock()
	r.watchers[k] = w
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.watchers[k] == w {
			delete(r.watchers, k)
		}
	}
}

// HoldsResidency reports whether this Handle holds a live residency
// lease on sessionID's tree.
func (h *Handle) HoldsResidency(appName, userID, sessionID string) bool {
	if h == nil || h.residency == nil {
		return false
	}
	r := h.residency
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.leases[keyOf(appName, userID, sessionID)]
	return l != nil && !leaseLost(l.lock)
}
