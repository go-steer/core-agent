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
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/usage"
)

type stubProvider []usage.TrackedSession

func (s stubProvider) Trackers() []usage.TrackedSession { return s }

// TestPrimaryTrackerProvider_DedupsByTrackerPointer pins the
// merge contract: the primary session registers in the attach
// registry too, so the same *usage.Tracker arriving from both
// sources must yield exactly one TrackedSession — the registry's
// (complete identity triple) — or aggregated (labels-off) mode
// double-counts every series.
func TestPrimaryTrackerProvider_DedupsByTrackerPointer(t *testing.T) {
	t.Parallel()
	shared := usage.NewTracker()
	other := usage.NewTracker()

	p := &primaryTrackerProvider{tracker: shared}
	// Identity intentionally left unstamped: the registry's triple
	// must win regardless.
	p.SetRegistry(stubProvider{
		{Tracker: shared, SessionID: "primary", AppName: "app", UserID: "u"},
		{Tracker: other, SessionID: "attach-1", AppName: "app", UserID: "u"},
	})

	got := p.Trackers()
	if len(got) != 2 {
		t.Fatalf("Trackers() len = %d, want 2 (shared tracker deduped)", len(got))
	}
	byTracker := map[*usage.Tracker]usage.TrackedSession{}
	for _, ts := range got {
		if _, dup := byTracker[ts.Tracker]; dup {
			t.Fatalf("tracker %p listed twice", ts.Tracker)
		}
		byTracker[ts.Tracker] = ts
	}
	if ts := byTracker[shared]; ts.SessionID != "primary" {
		t.Errorf("shared tracker identity = %q, want the registry entry's %q", ts.SessionID, "primary")
	}
}

// TestPrimaryTrackerProvider_NoRegistry pins the pre-attach shape:
// primary only, nil-tracker guarded.
func TestPrimaryTrackerProvider_NoRegistry(t *testing.T) {
	t.Parallel()
	p := &primaryTrackerProvider{tracker: usage.NewTracker()}
	if got := p.Trackers(); len(got) != 1 {
		t.Fatalf("Trackers() len = %d, want 1", len(got))
	}
	empty := &primaryTrackerProvider{}
	if got := empty.Trackers(); len(got) != 0 {
		t.Fatalf("nil-tracker Trackers() len = %d, want 0", len(got))
	}
}
