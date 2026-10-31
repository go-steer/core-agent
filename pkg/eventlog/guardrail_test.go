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

	"github.com/glebarez/sqlite"
)

func TestGuardrailStore_TripClearBudgetAndLoad(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "guardrail.db")
	h, err := Open(ctx, sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	store := h.Guardrail
	if store == nil {
		t.Fatal("Handle.Guardrail is nil")
	}

	// Empty state for unknown session.
	st, err := store.Load(ctx, "app", "u1", "s1")
	if err != nil {
		t.Fatalf("Load(empty): %v", err)
	}
	if st.WatchdogTripped || st.CostTripped || st.BudgetExtensionUSD != 0 {
		t.Fatalf("unexpected non-empty state: %+v", st)
	}

	// Trip watchdog.
	if err := store.TripWatchdog(ctx, "app", "u1", "s1", "looping on bash"); err != nil {
		t.Fatalf("TripWatchdog: %v", err)
	}
	st, err = store.Load(ctx, "app", "u1", "s1")
	if err != nil {
		t.Fatalf("Load(after watchdog trip): %v", err)
	}
	if !st.WatchdogTripped || st.WatchdogReason != "looping on bash" || st.CostTripped {
		t.Fatalf("after TripWatchdog: %+v", st)
	}

	// Trip cost without clearing watchdog.
	if err := store.TripCost(ctx, "app", "u1", "s1", "exceeded $10.00"); err != nil {
		t.Fatalf("TripCost: %v", err)
	}
	st, err = store.Load(ctx, "app", "u1", "s1")
	if err != nil {
		t.Fatalf("Load(after cost trip): %v", err)
	}
	if !st.WatchdogTripped || !st.CostTripped || st.CostReason != "exceeded $10.00" {
		t.Fatalf("after TripCost: %+v", st)
	}

	// Add budget extension twice and verify accumulation.
	if err := store.AddBudgetExtension(ctx, "app", "u1", "s1", 5.0); err != nil {
		t.Fatalf("AddBudgetExtension(5): %v", err)
	}
	if err := store.AddBudgetExtension(ctx, "app", "u1", "s1", 2.5); err != nil {
		t.Fatalf("AddBudgetExtension(2.5): %v", err)
	}
	st, err = store.Load(ctx, "app", "u1", "s1")
	if err != nil {
		t.Fatalf("Load(after budget extensions): %v", err)
	}
	if st.BudgetExtensionUSD != 7.5 || !st.WatchdogTripped || !st.CostTripped {
		t.Fatalf("after AddBudgetExtension: %+v", st)
	}

	// Clear watchdog only.
	if err := store.ClearWatchdog(ctx, "app", "u1", "s1"); err != nil {
		t.Fatalf("ClearWatchdog: %v", err)
	}
	st, err = store.Load(ctx, "app", "u1", "s1")
	if err != nil {
		t.Fatalf("Load(after clear watchdog): %v", err)
	}
	if st.WatchdogTripped || st.WatchdogReason != "" || !st.CostTripped || st.BudgetExtensionUSD != 7.5 {
		t.Fatalf("after ClearWatchdog: %+v", st)
	}

	// Clear cost.
	if err := store.ClearCost(ctx, "app", "u1", "s1"); err != nil {
		t.Fatalf("ClearCost: %v", err)
	}
	st, err = store.Load(ctx, "app", "u1", "s1")
	if err != nil {
		t.Fatalf("Load(after clear cost): %v", err)
	}
	if st.WatchdogTripped || st.CostTripped || st.CostReason != "" || st.BudgetExtensionUSD != 7.5 {
		t.Fatalf("after ClearCost: %+v", st)
	}
}
