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
	"gorm.io/gorm"
)

// TestSpendStoreAppendIsIdempotentOnEventID pins the #1433 idempotency
// key: the same event billed twice lands once, while a row that names
// no event is never deduplicated.
func TestSpendStoreAppendIsIdempotentOnEventID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openSpendStore(t)

	rec := SpendRecord{
		EventID: "evt-1", Author: "claude-opus-4-6", Model: "claude-opus-4-6", Backend: "anthropic",
		Tokens: 1_500, InputTokens: 1_000, OutputTokens: 500, CostUSD: 0.05,
	}
	for i := 0; i < 3; i++ {
		if err := s.Append(ctx, spendTestApp, spendTestUser, spendTestSID, rec); err != nil {
			t.Fatalf("Append #%d of the same event: %v", i+1, err)
		}
	}
	// Same event ID in another session is a different row.
	if err := s.Append(ctx, spendTestApp, spendTestUser, "other", rec); err != nil {
		t.Fatalf("Append same event in another session: %v", err)
	}
	// Key-less rows make no idempotency claim.
	for i := 0; i < 2; i++ {
		if err := s.Append(ctx, spendTestApp, spendTestUser, spendTestSID, SpendRecord{Author: "approver", Tokens: 10, CostUSD: 0.001}); err != nil {
			t.Fatalf("Append key-less row #%d: %v", i+1, err)
		}
	}

	hist, err := s.History(ctx, spendTestApp, spendTestUser, spendTestSID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("History has %d rows, want 3 (one for evt-1, two key-less): %+v", len(hist), hist)
	}
	got := hist[0]
	if got.EventID != "evt-1" || got.Model != "claude-opus-4-6" || got.Backend != "anthropic" ||
		got.InputTokens != 1_000 || got.OutputTokens != 500 || !sameUSD(got.CostUSD, 0.05) {
		t.Errorf("round-tripped row = %+v, want the appended shape", got)
	}
	st, err := s.Fold(ctx, spendTestApp, spendTestUser, spendTestSID)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if st.Session.Calls != 3 || !sameUSD(st.Session.CostUSD, 0.052) {
		t.Errorf("Fold = %+v, want 3 calls / $0.052 (the duplicate must not be summed)", st.Session)
	}
}

// legacyBudgetSpendRow is agent_budget_spend as #1430 created it,
// before the #1433 columns existed.
type legacyBudgetSpendRow struct {
	Seq       int64  `gorm:"primaryKey;autoIncrement"`
	AppName   string `gorm:"not null;index:idx_budget_spend_session,priority:1"`
	UserID    string `gorm:"not null;index:idx_budget_spend_session,priority:2"`
	SessionID string `gorm:"not null;index:idx_budget_spend_session,priority:3"`
	Author    string
	Tokens    int64
	CostUSD   float64
	Unpriced  bool
	At        time.Time `gorm:"not null"`
}

func (legacyBudgetSpendRow) TableName() string { return "agent_budget_spend" }

// TestSpendStoreMigratesAPreExistingTable opens a database whose
// agent_budget_spend has the #1430 shape and rows, and checks that
// AutoMigrate adds the #1433 columns and the unique index without
// tripping over the legacy rows (whose event_id is NULL), that those
// rows still read back, and that new keyed rows deduplicate.
func TestSpendStoreMigratesAPreExistingTable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open legacy: %v", err)
	}
	if err := legacy.AutoMigrate(&legacyBudgetSpendRow{}); err != nil {
		t.Fatalf("AutoMigrate legacy shape: %v", err)
	}
	at := time.Date(2026, 8, 18, 14, 0, 0, 0, time.UTC)
	// Two legacy rows in the same session: with a NOT NULL or
	// defaulted event_id column the unique index could not be built
	// over them.
	for i := 0; i < 2; i++ {
		row := legacyBudgetSpendRow{AppName: spendTestApp, UserID: spendTestUser, SessionID: spendTestSID,
			Author: "coordinator", Tokens: 100, CostUSD: 0.01, At: at}
		if err := legacy.Create(&row).Error; err != nil {
			t.Fatalf("Create legacy row %d: %v", i, err)
		}
	}
	if sqlDB, err := legacy.DB(); err == nil {
		_ = sqlDB.Close()
	}

	h, err := Open(ctx, sqlite.Open(path))
	if err != nil {
		t.Fatalf("Open over the legacy database: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if h.Spend == nil {
		t.Fatal("Handle.Spend is nil after Open")
	}

	hist, err := h.Spend.History(ctx, spendTestApp, spendTestUser, spendTestSID)
	if err != nil {
		t.Fatalf("History after migration: %v", err)
	}
	if len(hist) != 2 || hist[0].EventID != "" || hist[0].Tokens != 100 {
		t.Fatalf("legacy rows after migration = %+v, want the two #1430 rows with empty EventID", hist)
	}

	rec := SpendRecord{EventID: "evt-new", Author: "coordinator", Model: "m", Tokens: 50, CostUSD: 0.005}
	for i := 0; i < 2; i++ {
		if err := h.Spend.Append(ctx, spendTestApp, spendTestUser, spendTestSID, rec); err != nil {
			t.Fatalf("Append keyed row #%d after migration: %v", i+1, err)
		}
	}
	hist, err = h.Spend.History(ctx, spendTestApp, spendTestUser, spendTestSID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("History has %d rows, want 3 (two legacy + one keyed; the unique index must exist on the migrated table)", len(hist))
	}
}

// TestSpendStoreHistoryTreeSpansDerivedSubSessions mirrors FoldTree
// for the row-level read the resume path uses.
func TestSpendStoreHistoryTreeSpansDerivedSubSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openSpendStore(t)

	for i, w := range []struct{ sid, ev string }{
		{spendTestSID, "e1"},
		{spendTestSID + ":sub:researcher:inv1", "e2"},
		{"other:sub:researcher:inv2", "e3"},
		{spendTestSID, "e4"},
	} {
		if err := s.Append(ctx, spendTestApp, spendTestUser, w.sid, SpendRecord{EventID: w.ev, Author: "a", Tokens: int64(i + 1)}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	hist, err := s.HistoryTree(ctx, spendTestApp, spendTestUser, spendTestSID)
	if err != nil {
		t.Fatalf("HistoryTree: %v", err)
	}
	var ids []string
	for _, r := range hist {
		ids = append(ids, r.EventID)
	}
	if len(ids) != 3 || ids[0] != "e1" || ids[1] != "e2" || ids[2] != "e4" {
		t.Fatalf("HistoryTree event ids = %v, want [e1 e2 e4] oldest-first, other trees excluded", ids)
	}

	var nilStore *SpendStore
	if hist, err := nilStore.HistoryTree(ctx, spendTestApp, spendTestUser, spendTestSID); err != nil || hist != nil {
		t.Errorf("HistoryTree on a nil store = %v, %v; want nil, nil", hist, err)
	}
}

func TestSpendStoreAppendBatchIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openSpendStore(t)

	batch := []SpendRecord{
		{EventID: "b1", Author: "m", Model: "m", Tokens: 100, InputTokens: 80, OutputTokens: 20, CostUSD: 0.01},
		{EventID: "b2", Author: "m", Model: "m", Tokens: 200, InputTokens: 150, OutputTokens: 50, CostUSD: 0.02},
		{EventID: "b1", Author: "m", Model: "m", Tokens: 999, CostUSD: 9.99}, // duplicate EventID within batch
	}
	if err := s.AppendBatch(ctx, spendTestApp, spendTestUser, spendTestSID, batch); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	// Repeating the batch is a no-op on keyed rows.
	if err := s.AppendBatch(ctx, spendTestApp, spendTestUser, spendTestSID, batch[:2]); err != nil {
		t.Fatalf("AppendBatch repeat: %v", err)
	}
	hist, err := s.History(ctx, spendTestApp, spendTestUser, spendTestSID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 2 || hist[0].CostUSD != 0.01 || hist[1].CostUSD != 0.02 {
		t.Fatalf("History after idempotent AppendBatch = %+v, want 2 rows [0.01, 0.02]", hist)
	}
}
