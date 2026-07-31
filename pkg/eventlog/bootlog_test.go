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

package eventlog

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
)

func TestBootLog_RoundTripAndWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, err := Open(ctx, sqlite.Open(filepath.Join(t.TempDir(), "s.db")))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	now := time.Now()
	if err := h.RecordBoot(ctx, now.Add(-20*time.Minute), []string{"old-sid"}); err != nil {
		t.Fatalf("RecordBoot(old): %v", err)
	}
	if err := h.RecordBoot(ctx, now.Add(-5*time.Minute), []string{"sid-a", "sid-b"}); err != nil {
		t.Fatalf("RecordBoot: %v", err)
	}
	if err := h.RecordBoot(ctx, now.Add(-time.Minute), nil); err != nil {
		t.Fatalf("RecordBoot(empty): %v", err)
	}

	recent, err := h.RecentBoots(ctx, now.Add(-10*time.Minute))
	if err != nil {
		t.Fatalf("RecentBoots: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("RecentBoots returned %d records, want 2 (window must exclude the 20m-old boot)", len(recent))
	}
	if len(recent[0].Attempted) != 2 || recent[0].Attempted[0] != "sid-a" {
		t.Errorf("first record attempted = %v, want [sid-a sid-b]", recent[0].Attempted)
	}
	if len(recent[1].Attempted) != 0 {
		t.Errorf("empty-attempt record round-tripped as %v, want empty", recent[1].Attempted)
	}
	if !recent[0].BootAt.Before(recent[1].BootAt) {
		t.Error("records not in oldest-first order")
	}
}
