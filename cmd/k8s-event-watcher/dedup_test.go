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

func TestDedup_FirstEvent_IsNewIncident(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	got := c.Observe(EventKey{UID: "u1", Reason: "CrashLoopBackOff"})
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
	c.Observe(key)
	got := c.Observe(key)
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
	c.Observe(key)

	// Advance past the window.
	now = now.Add(10 * time.Minute)
	got := c.Observe(key)
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
	c.Observe(key)
	c.BindSession(key, "sess-abc")
	// Second sighting is a duplicate — should carry the bound
	// SessionID so the caller can route the inject to it.
	got := c.Observe(key)
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

	// First: ErrImagePull (the earlier event kubelet emits when a
	// pull attempt fails). Canonicalizes to ImagePullBackOff.
	first := c.Observe(EventKey{UID: "u-payment", Reason: "ErrImagePull"})
	if first.Kind != dedupNewIncident {
		t.Fatalf("ErrImagePull first: want dedupNewIncident, got %v", first.Kind)
	}

	// Second: ImagePullBackOff (the settled kubelet backoff state,
	// same underlying failure, arrives seconds later). Must be
	// treated as a duplicate of the first event, not a new incident.
	second := c.Observe(EventKey{UID: "u-payment", Reason: "ImagePullBackOff"})
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

	c.Observe(EventKey{UID: "u-flappy", Reason: "CrashLoopBackOff"})
	second := c.Observe(EventKey{UID: "u-flappy", Reason: "BackOff"})
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

	a := c.Observe(EventKey{UID: "u-pod-a", Reason: "ImagePullBackOff"})
	b := c.Observe(EventKey{UID: "u-pod-b", Reason: "ImagePullBackOff"})

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

	// First event with a NON-canonical reason. Observe canonicalizes
	// the stored key; BindSession must apply the same mapping so
	// the same entry is found.
	c.Observe(EventKey{UID: "u-payment", Reason: "ErrImagePull"})
	c.BindSession(EventKey{UID: "u-payment", Reason: "ErrImagePull"}, "sess-payment")

	// Follow-up ImagePullBackOff for same UID must resolve to the
	// same session (via canonicalization).
	got := c.Observe(EventKey{UID: "u-payment", Reason: "ImagePullBackOff"})
	if got.SessionID != "sess-payment" {
		t.Errorf("family follow-up should carry bound SessionID; got %q", got.SessionID)
	}
}

func TestDedup_DifferentKeys_AreIndependent(t *testing.T) {
	t.Parallel()
	c := newTestDedup(t, 5*time.Minute, "")
	a := c.Observe(EventKey{UID: "u1", Reason: "CrashLoopBackOff"})
	b := c.Observe(EventKey{UID: "u2", Reason: "CrashLoopBackOff"})
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

	c.Observe(EventKey{UID: "u1", Reason: "R"})
	now = now.Add(1 * time.Second)
	c.Observe(EventKey{UID: "u2", Reason: "R"})
	now = now.Add(1 * time.Second)
	c.Observe(EventKey{UID: "u3", Reason: "R"})
	if c.Len() != 3 {
		t.Errorf("Len = %d, want 3 after three distinct observations", c.Len())
	}
	// Adding a fourth should evict u1 (oldest LastSeen).
	now = now.Add(1 * time.Second)
	c.Observe(EventKey{UID: "u4", Reason: "R"})
	if c.Len() != 3 {
		t.Errorf("Len = %d, want 3 after LRU eviction", c.Len())
	}
	// u1 should now be evicted; observing it again is a fresh incident.
	got := c.Observe(EventKey{UID: "u1", Reason: "R"})
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
	c1.Observe(key)
	c1.BindSession(key, "sess-persist")
	if err := c1.Snapshot(); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	c2 := newTestDedup(t, 5*time.Minute, path)
	got := c2.Observe(key)
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
	c.Observe(EventKey{UID: "u1", Reason: "R"})
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
