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

package main

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestDedup(t *testing.T, window time.Duration, persistPath string) *dedupCache {
	t.Helper()
	c, err := newDedupCache(window, persistPath)
	if err != nil {
		t.Fatalf("newDedupCache: %v", err)
	}
	return c
}

// tsGen returns a helper that yields monotonically increasing
// timestamps starting at base. Used by tests that need distinct
// k8s Event.LastTimestamp values across Observe calls without
// caring about their absolute values.
func tsGen(base time.Time, step time.Duration) func() time.Time {
	t := base
	return func() time.Time {
		t = t.Add(step)
		return t
	}
}

func TestDedup_FirstEvent_IsNewIncident(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	got := c.Observe(EventKey{UID: "u1", Reason: "CrashLoopBackOff"}, time.Now())
	if got.Kind != dedupNewIncident {
		t.Errorf("first sighting: kind = %v, want dedupNewIncident", got.Kind)
	}
	if got.Count != 1 {
		t.Errorf("first sighting: count = %d, want 1", got.Count)
	}
}

func TestDedup_SecondEventWithinWindow_IsDuplicate(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	key := EventKey{UID: "u1", Reason: "CrashLoopBackOff"}
	next := tsGen(time.Now(), 1*time.Second)
	c.Observe(key, next())
	got := c.Observe(key, next())
	if got.Kind != dedupDuplicate {
		t.Errorf("second sighting: kind = %v, want dedupDuplicate", got.Kind)
	}
	if got.Count != 2 {
		t.Errorf("second sighting: count = %d, want 2", got.Count)
	}
}

func TestDedup_EventAfterWindow_IsNewIncident(t *testing.T) {
	t.Parallel()
	// Use the injectable clock so we can simulate window rollover
	// without sleeping.
	c := newTestDedup(t, 5*time.Minute, "")
	now := time.Now()
	c.now = func() time.Time { return now }
	key := EventKey{UID: "u1", Reason: "CrashLoopBackOff"}
	c.Observe(key, now)

	// Advance past the window AND advance eventLastTS so this is
	// classified as real new activity (not a replay), which is
	// the shape that triggers the retry-safety-net path.
	now = now.Add(10 * time.Minute)
	got := c.Observe(key, now)
	if got.Kind != dedupNewIncident {
		t.Errorf("post-window sighting: kind = %v, want dedupNewIncident (window should have expired)", got.Kind)
	}
	if got.Count != 1 {
		t.Errorf("post-window sighting: count = %d, want 1 (fresh window)", got.Count)
	}
}

func TestDedup_BindSession_AttachesToEntry(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	key := EventKey{UID: "u1", Reason: "CrashLoopBackOff"}
	next := tsGen(time.Now(), 1*time.Second)
	c.Observe(key, next())
	c.BindSession(key, "sess-abc")
	// Second sighting is a duplicate — should carry the bound
	// SessionID so the caller can route the inject to it.
	got := c.Observe(key, next())
	if got.SessionID != "sess-abc" {
		t.Errorf("duplicate should carry bound SessionID; got %q", got.SessionID)
	}
}

func TestDedup_BindSession_NoOp_OnMissingEntry(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	// Race case: BindSession called on a key whose entry has since
	// been evicted. Must not panic.
	c.BindSession(EventKey{UID: "u-gone", Reason: "X"}, "sess-orphan")
}

// TestCanonicalizeReason pins the reason-family mapping (#219).
// Reasons in the map collapse to their canonical primary; every
// other reason maps to itself.
func TestCanonicalizeReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"ErrImagePull", "ImagePullBackOff"},
		{"BackOff", "CrashLoopBackOff"},
		{"ImagePullBackOff", "ImagePullBackOff"}, // canonical stays itself
		{"CrashLoopBackOff", "CrashLoopBackOff"}, // canonical stays itself
		{"OOMKilled", "OOMKilled"},               // not in map → identity
		{"FailedMount", "FailedMount"},
		{"", ""}, // edge: empty stays empty
		{"Unknown", "Unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := canonicalizeReason(tc.in)
			if got != tc.want {
				t.Errorf("canonicalizeReason(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDedup_ReasonFamilyCollapsesIntoOneSlot is the #219 regression
// test. Before the fix, ImagePullBackOff and ErrImagePull for the
// same pod produced two independent dedup entries → two parallel
// sessions (observed live: 4 sessions per incident, 4× cost).
// After canonicalization, both hit the same slot; second sighting
// is a duplicate.
func TestDedup_ReasonFamilyCollapsesIntoOneSlot(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	next := tsGen(time.Now(), 1*time.Second)

	// First: ErrImagePull (the earlier event kubelet emits when a
	// pull attempt fails). Canonicalizes to ImagePullBackOff.
	first := c.Observe(EventKey{UID: "u-payment", Reason: "ErrImagePull"}, next())
	if first.Kind != dedupNewIncident {
		t.Fatalf("ErrImagePull first: want dedupNewIncident, got %v", first.Kind)
	}

	// Second: ImagePullBackOff (the settled kubelet backoff state,
	// same underlying failure, arrives seconds later). Must be
	// treated as a duplicate of the first event, not a new incident.
	second := c.Observe(EventKey{UID: "u-payment", Reason: "ImagePullBackOff"}, next())
	if second.Kind != dedupDuplicate {
		t.Errorf("ImagePullBackOff after ErrImagePull for same UID: want dedupDuplicate (family collision), got %v", second.Kind)
	}
	if second.Count != 2 {
		t.Errorf("family-collision count: want 2 (first + second), got %d", second.Count)
	}
}

// TestDedup_BackOff_CanonicalizesTo_CrashLoopBackOff — the second
// documented reason-family mapping. Locks in behavior operators
// depend on for the crash-loop cycle.
func TestDedup_BackOff_CanonicalizesTo_CrashLoopBackOff(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	next := tsGen(time.Now(), 1*time.Second)

	c.Observe(EventKey{UID: "u-flappy", Reason: "CrashLoopBackOff"}, next())
	second := c.Observe(EventKey{UID: "u-flappy", Reason: "BackOff"}, next())
	if second.Kind != dedupDuplicate {
		t.Errorf("BackOff after CrashLoopBackOff same UID: want dedupDuplicate, got %v", second.Kind)
	}
}

// TestDedup_DifferentPodsDontCollide — sanity check that
// canonicalization only collapses SAME-UID events. Different pods
// with related reasons stay independent.
func TestDedup_DifferentPodsDontCollide(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	next := tsGen(time.Now(), 1*time.Second)

	a := c.Observe(EventKey{UID: "u-pod-a", Reason: "ImagePullBackOff"}, next())
	b := c.Observe(EventKey{UID: "u-pod-b", Reason: "ImagePullBackOff"}, next())

	if a.Kind != dedupNewIncident {
		t.Errorf("pod-a: want dedupNewIncident, got %v", a.Kind)
	}
	if b.Kind != dedupNewIncident {
		t.Errorf("pod-b (different UID): want dedupNewIncident, got %v", b.Kind)
	}
}

// TestDedup_BindSession_CanonicalizesLookup verifies the caller
// can pass the wire-level reason when binding a session and have
// the lookup find the entry via canonicalization. Otherwise the
// duplicate-routing SessionID would be lost.
func TestDedup_BindSession_CanonicalizesLookup(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	next := tsGen(time.Now(), 1*time.Second)

	// First event with a NON-canonical reason. Observe canonicalizes
	// the stored key; BindSession must apply the same mapping so
	// the same entry is found.
	c.Observe(EventKey{UID: "u-payment", Reason: "ErrImagePull"}, next())
	c.BindSession(EventKey{UID: "u-payment", Reason: "ErrImagePull"}, "sess-payment")

	// Follow-up ImagePullBackOff for same UID must resolve to the
	// same session (via canonicalization).
	got := c.Observe(EventKey{UID: "u-payment", Reason: "ImagePullBackOff"}, next())
	if got.SessionID != "sess-payment" {
		t.Errorf("family follow-up should carry bound SessionID; got %q", got.SessionID)
	}
}

func TestDedup_DifferentKeys_AreIndependent(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	next := tsGen(time.Now(), 1*time.Second)
	a := c.Observe(EventKey{UID: "u1", Reason: "CrashLoopBackOff"}, next())
	b := c.Observe(EventKey{UID: "u2", Reason: "CrashLoopBackOff"}, next())
	if a.Kind != dedupNewIncident || b.Kind != dedupNewIncident {
		t.Errorf("distinct UIDs should both be new incidents (a=%v, b=%v)", a.Kind, b.Kind)
	}
}

func TestDedup_LRUEvictionAtCapacity(t *testing.T) {
	t.Parallel()
	// Override the cache cap so we don't need 10k entries in the
	// test. Reach into the internals — this file lives in the
	// same package.
	c := newTestDedup(t, 5*time.Minute, "")
	c.max = 3
	now := time.Now()
	c.now = func() time.Time { return now }

	c.Observe(EventKey{UID: "u1", Reason: "R"}, now)
	now = now.Add(1 * time.Second)
	c.Observe(EventKey{UID: "u2", Reason: "R"}, now)
	now = now.Add(1 * time.Second)
	c.Observe(EventKey{UID: "u3", Reason: "R"}, now)
	if c.Len() != 3 {
		t.Errorf("Len = %d, want 3 after three distinct observations", c.Len())
	}
	// Adding a fourth should evict u1 (oldest LastSeen).
	now = now.Add(1 * time.Second)
	c.Observe(EventKey{UID: "u4", Reason: "R"}, now)
	if c.Len() != 3 {
		t.Errorf("Len = %d, want 3 after LRU eviction", c.Len())
	}
	// u1 should now be evicted; observing it again is a fresh incident.
	got := c.Observe(EventKey{UID: "u1", Reason: "R"}, now)
	if got.Kind != dedupNewIncident {
		t.Errorf("evicted key re-observed: kind = %v, want dedupNewIncident", got.Kind)
	}
}

func TestDedup_Snapshot_RoundTrip(t *testing.T) {
	t.Parallel()
	// Snapshot then restore into a fresh cache; state should
	// survive intact.
	path := filepath.Join(t.TempDir(), "dedup.json")
	c1 := newTestDedup(t, 5*time.Minute, path)
	key := EventKey{UID: "u-persist", Reason: "CrashLoopBackOff"}
	next := tsGen(time.Now(), 1*time.Second)
	c1.Observe(key, next())
	c1.BindSession(key, "sess-persist")
	if err := c1.Snapshot(); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	c2 := newTestDedup(t, 5*time.Minute, path)
	got := c2.Observe(key, next())
	if got.Kind != dedupDuplicate {
		t.Errorf("restored key: kind = %v, want dedupDuplicate (should be within window from restored state)", got.Kind)
	}
	if got.SessionID != "sess-persist" {
		t.Errorf("restored key: SessionID = %q, want sess-persist", got.SessionID)
	}
}

func TestDedup_Snapshot_NoPersistPathIsNoOp(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	c.Observe(EventKey{UID: "u1", Reason: "R"}, time.Now())
	if err := c.Snapshot(); err != nil {
		t.Errorf("Snapshot on non-persisted cache should succeed as no-op; got %v", err)
	}
}

func TestDedup_NegativeWindow_Rejected(t *testing.T) {
	t.Parallel()
	if _, err := newDedupCache(0, ""); err == nil {
		t.Error("zero window should be rejected")
	}
	if _, err := newDedupCache(-1*time.Second, ""); err == nil {
		t.Error("negative window should be rejected")
	}
}

func TestSerializeDeserializeKey_RoundTrip(t *testing.T) {
	t.Parallel()
	orig := EventKey{UID: "abc-123-def-456", Reason: "CrashLoopBackOff"}
	got, ok := deserializeKey(serializeKey(orig))
	if !ok {
		t.Fatal("deserialize failed")
	}
	if got != orig {
		t.Errorf("round-trip: got %+v, want %+v", got, orig)
	}
}

// TestDedup_ReplayOfSameEventTimestampDedups is the informer
// re-List regression. Live demo drive 2026-07-14 produced 4
// sessions for one ImagePullBackOff because the client-go
// informer re-Lists Events every ~15-25min on watch-connection
// rotation, and the re-Listed event's arrival was outside the
// wall-clock dedup window even though the Event object itself
// hadn't advanced. Comparing incoming eventLastTS against the
// recorded value now catches this — same LastTimestamp = same
// activity, dedup regardless of arrival time.
func TestDedup_ReplayOfSameEventTimestampDedups(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	now := time.Now()
	c.now = func() time.Time { return now }
	key := EventKey{UID: "u-payment", Reason: "ImagePullBackOff"}
	eventTS := now // k8s Event.LastTimestamp for this incident

	first := c.Observe(key, eventTS)
	if first.Kind != dedupNewIncident {
		t.Fatalf("first sighting: want dedupNewIncident, got %v", first.Kind)
	}

	// Simulate informer re-List after wall clock advances past the
	// window (kube-apiserver rotates watch connections every
	// ~15-25min). The same Event object is re-delivered with the
	// SAME LastTimestamp — this must NOT fire a new session.
	now = now.Add(20 * time.Minute) // past the 5m dedup window
	replay := c.Observe(key, eventTS)
	if replay.Kind != dedupDuplicate {
		t.Errorf("replay past wall-clock window: kind = %v, want dedupDuplicate (same eventLastTS = replay, not new activity)", replay.Kind)
	}
	if replay.Count != 2 {
		t.Errorf("replay count = %d, want 2 (initial + replay)", replay.Count)
	}
}

// TestDedup_ReplayDoesNotAdvanceLastSeen — subtle but load-bearing:
// replay dedup bumps Count but must NOT bump LastSeen. Otherwise
// the retry-safety-net cooldown would keep resetting every time
// the informer re-Lists and a stalled session would never get a
// second attempt at the incident.
func TestDedup_ReplayDoesNotAdvanceLastSeen(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	now := time.Now()
	c.now = func() time.Time { return now }
	key := EventKey{UID: "u", Reason: "R"}
	firstEventTS := now

	c.Observe(key, firstEventTS)
	firstLastSeen := c.entries[key].LastSeen

	// A dozen informer replays over 30 minutes with the SAME
	// eventLastTS. Cooldown timer must remain anchored to
	// firstLastSeen.
	for i := 1; i <= 12; i++ {
		now = now.Add(3 * time.Minute)
		c.Observe(key, firstEventTS) // replay: same eventLastTS
	}

	if !c.entries[key].LastSeen.Equal(firstLastSeen) {
		t.Errorf("LastSeen advanced by replays; want %v, got %v", firstLastSeen, c.entries[key].LastSeen)
	}
	if c.entries[key].Count != 13 {
		t.Errorf("Count = %d, want 13 (initial + 12 replays)", c.entries[key].Count)
	}
}

// TestDedup_NewActivityPastCooldownFiresRetrySafetyNet — with the
// new replay-aware logic, the retry safety net still triggers when
// k8s reports REAL new activity (advancing eventLastTS) past the
// cooldown. This is the "agent failed to process, incident still
// ongoing" case the operator flagged as a concern: if the pod is
// still emitting BackOff events and enough wall-clock has passed,
// we spin up a fresh session.
func TestDedup_NewActivityPastCooldownFiresRetrySafetyNet(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	now := time.Now()
	c.now = func() time.Time { return now }
	key := EventKey{UID: "u", Reason: "ImagePullBackOff"}

	c.Observe(key, now)
	c.BindSession(key, "sess-first")

	// Advance both the wall clock (past the cooldown) AND the
	// event's LastTimestamp (k8s aggregated a fresh backoff cycle
	// into the same Event object — this is real new activity, not
	// a replay). Retry safety net should fire.
	now = now.Add(10 * time.Minute)
	newEventTS := now // k8s bumped LastTimestamp for this Event
	got := c.Observe(key, newEventTS)
	if got.Kind != dedupNewIncident {
		t.Errorf("real new activity past cooldown: kind = %v, want dedupNewIncident (retry safety net)", got.Kind)
	}
	if got.Count != 1 {
		t.Errorf("retry fresh window count = %d, want 1", got.Count)
	}
}

// TestDedup_NewActivityWithinCooldownDedups — real new activity
// (advancing eventLastTS) but within the cooldown window routes to
// the existing session. Matches the "same incident, still active"
// case: k8s keeps emitting BackOff events, we've already spun a
// session, no need for another.
func TestDedup_NewActivityWithinCooldownDedups(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	now := time.Now()
	c.now = func() time.Time { return now }
	key := EventKey{UID: "u", Reason: "ImagePullBackOff"}

	c.Observe(key, now)
	c.BindSession(key, "sess-first")

	// 2 minutes later, k8s advances the Event's LastTimestamp
	// (real new activity). Well within cooldown → dedup to same session.
	now = now.Add(2 * time.Minute)
	got := c.Observe(key, now)
	if got.Kind != dedupDuplicate {
		t.Errorf("real new activity within cooldown: kind = %v, want dedupDuplicate", got.Kind)
	}
	if got.SessionID != "sess-first" {
		t.Errorf("SessionID = %q, want sess-first (routed to existing session)", got.SessionID)
	}
	if got.Count != 2 {
		t.Errorf("Count = %d, want 2", got.Count)
	}
}

// TestDedup_BackwardsEventTimestampTreatedAsReplay — defensive: if
// somehow (misconfigured k8s Event source, wall-clock drift) the
// incoming eventLastTS is EARLIER than what we recorded, treat as
// replay. Better to under-fire than to spuriously spin new sessions.
func TestDedup_BackwardsEventTimestampTreatedAsReplay(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	key := EventKey{UID: "u", Reason: "R"}
	base := time.Now()

	c.Observe(key, base)
	got := c.Observe(key, base.Add(-1*time.Minute)) // earlier ts

	if got.Kind != dedupDuplicate {
		t.Errorf("backwards eventLastTS: kind = %v, want dedupDuplicate (treat as replay)", got.Kind)
	}
}
