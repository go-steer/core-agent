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
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
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

func TestGuardrailStore_FallbackPartialCommitHealsOnRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "fallback-retry.db")
	h, err := Open(ctx, sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	svc, ok := h.Service.(*service)
	if !ok {
		t.Fatalf("Service type = %T, want *service", h.Service)
	}
	// Force the non-hook two-write fallback path (#1438).
	svc.stream.overlayInTx = false

	createResp, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u1", SessionID: "s-fallback",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ev := session.NewEvent(ctx, "inv-1")
	ev.ID = "ev-partial-1438"
	ev.Author = guardrailResetAuthor
	ev.CustomMetadata = map[string]any{
		"reset":             []string{guardrailKindWatchdog},
		guardrailMetaBudget: 4.5,
	}
	ev.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "reset"}}}

	// Simulate ADK committing the events row before stream.Append /
	// guardrail.applyEvent ran (e.g. a transient error right after inner.AppendEvent).
	if err := svc.inner.AppendEvent(ctx, createResp.Session, ev); err != nil {
		t.Fatalf("inner.AppendEvent: %v", err)
	}

	getResp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: "app", UserID: "u1", SessionID: "s-fallback", NumRecentEvents: 1,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Retrying through Service.AppendEvent with the same event ID must
	// heal the missing overlay + guardrail state instead of failing with a
	// primary-key violation on events.
	if err := h.Service.AppendEvent(ctx, getResp.Session, ev); err != nil {
		t.Fatalf("retry AppendEvent failed on committed ADK event: %v", err)
	}
	// A second retry must remain idempotent (no duplicate seq, no double budget).
	if err := h.Service.AppendEvent(ctx, getResp.Session, ev); err != nil {
		t.Fatalf("second retry AppendEvent failed: %v", err)
	}

	var entries int
	for _, sErr := range h.Stream.Since(ctx, 0, ForSession("app", "u1", "s-fallback")) {
		if sErr != nil {
			t.Fatalf("Since: %v", sErr)
		}
		entries++
	}
	if entries != 1 {
		t.Fatalf("overlay entries = %d, want 1", entries)
	}
	st, err := h.Guardrail.Load(ctx, "app", "u1", "s-fallback")
	if err != nil {
		t.Fatalf("Guardrail.Load: %v", err)
	}
	if st.BudgetExtensionUSD != 4.5 {
		t.Fatalf("BudgetExtensionUSD = %v, want 4.5 (must apply once across retries)", st.BudgetExtensionUSD)
	}
}

func TestGuardrailStore_LegacyCheckUsesCompositeIndexAndSeedsSentinel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "legacy-index.db")
	h, err := Open(ctx, sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close()

	// Querying a nonexistent session must not pollute agent_guardrail_state.
	if _, needsLegacy, err := h.Guardrail.LoadWithLegacyCheck(ctx, "app", "u1", "missing"); err != nil || needsLegacy {
		t.Fatalf("missing session: needsLegacy=%v err=%v", needsLegacy, err)
	}
	var count int64
	if err := h.DB.Model(&GuardrailState{}).Where("session_id = ?", "missing").Count(&count).Error; err != nil {
		t.Fatalf("count missing: %v", err)
	}
	if count != 0 {
		t.Fatalf("sentinel rows for nonexistent session = %d, want 0", count)
	}

	// Creating a session seeds a zero sentinel row immediately.
	createResp, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u1", SessionID: "s-clean",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.DB.Model(&GuardrailState{}).Where("session_id = ?", "s-clean").Count(&count).Error; err != nil {
		t.Fatalf("count created: %v", err)
	}
	if count != 1 {
		t.Fatalf("sentinel rows after Create = %d, want 1", count)
	}

	// Append ordinary conversation events and delete agent_guardrail_state
	// to simulate a pre-upgrade never-tripped session.
	sess := createResp.Session
	for i := range 10 {
		ev := session.NewEvent(ctx, "inv")
		ev.Author = "user"
		ev.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "turn"}}}
		if err := h.Service.AppendEvent(ctx, sess, ev); err != nil {
			t.Fatalf("AppendEvent(%d): %v", i, err)
		}
	}
	if err := h.DB.Exec("DELETE FROM agent_guardrail_state WHERE session_id = ?", "s-clean").Error; err != nil {
		t.Fatalf("delete state row: %v", err)
	}

	// Verify SQLite uses idx_agent_eventlog_session_author for the legacy probe.
	type planRow struct {
		Detail string
	}
	var plans []planRow
	if err := h.DB.Raw(
		"EXPLAIN QUERY PLAN SELECT seq FROM agent_eventlog WHERE app_name = ? AND user_id = ? AND session_id = ? AND author IN (?, ?) LIMIT 1",
		"app", "u1", "s-clean", guardrailTripAuthor, guardrailResetAuthor,
	).Scan(&plans).Error; err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	var joined strings.Builder
	for _, p := range plans {
		joined.WriteString(p.Detail)
		joined.WriteByte('\n')
	}
	if !strings.Contains(joined.String(), "idx_agent_eventlog_session_author") {
		t.Fatalf("query plan did not use idx_agent_eventlog_session_author:\n%s", joined.String())
	}

	// Negative legacy check on an existing session must seed a zero sentinel row
	// so every subsequent LoadWithLegacyCheck is a direct PK lookup.
	st, needsLegacy, err := h.Guardrail.LoadWithLegacyCheck(ctx, "app", "u1", "s-clean")
	if err != nil {
		t.Fatalf("LoadWithLegacyCheck: %v", err)
	}
	if needsLegacy || st.WatchdogTripped || st.CostTripped || st.BudgetExtensionUSD != 0 {
		t.Fatalf("unexpected state: needsLegacy=%v st=%+v", needsLegacy, st)
	}
	if err := h.DB.Model(&GuardrailState{}).Where("session_id = ?", "s-clean").Count(&count).Error; err != nil {
		t.Fatalf("count after negative legacy check: %v", err)
	}
	if count != 1 {
		t.Fatalf("sentinel rows after negative legacy check = %d, want 1", count)
	}
}
