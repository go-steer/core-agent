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

// Durable per-session spend ledger wiring (#1433, design row of #1359).
//
// #1430 shipped eventlog.SpendStore with nothing writing to it and
// nothing reading from it, so a resumed session still re-priced its
// history from the event stream at the catalog in force at RESUME
// time. A /pricing/refresh between kill and resume changed a session's
// historical total and moved the point where --max-session-cost-usd
// trips. This file closes both ends: every Turn the per-session
// usage.Tracker records is appended to the ledger at the price it was
// billed, and a resumed tracker is restored from those rows verbatim.

package compose

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

// spendLedgerApp is the app name the ledger rows are scoped by —
// the same constant ReproduceAgent passes to eventlog.WithSessionTree.
const spendLedgerApp = "core-agent"

// spendLedgerWriteTimeout bounds each detached spend-ledger write
// (above SQLite's 5s busy_timeout, matching guardrail_persist.go).
const spendLedgerWriteTimeout = 10 * time.Second

// installSpendLedger makes tracker write every billed Turn to store
// under (spendLedgerApp, user, sid). Synchronous: one INSERT per model
// call, on the goroutine that billed it. Writes detach from caller
// cancellation (context.WithoutCancel + spendLedgerWriteTimeout) so an
// in-flight turn completing during daemon shutdown drain after
// DaemonCtx is cancelled still persists its cost row. A write failure
// is logged and the turn proceeds; the ledger is a record of spend, not
// a gate on it. A nil store installs nothing.
//
// Install AFTER restoreResumedTracker so a partial event-stream failure
// cannot leave a truncated ledger behind; restoreResumedTracker seeds
// historical rows atomically once RebuildTrackerFromEvents succeeds.
func installSpendLedger(ctx context.Context, store *eventlog.SpendStore, tracker *usage.Tracker, user, sid string) {
	if store == nil || tracker == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	baseCtx := context.WithoutCancel(ctx)
	tracker.SetOnTurn(func(turn usage.Turn) {
		writeCtx, cancel := context.WithTimeout(baseCtx, spendLedgerWriteTimeout)
		defer cancel()
		if err := store.Append(writeCtx, spendLedgerApp, user, sid, spendRecordFromTurn(turn)); err != nil {
			slog.Warn("compose: spend ledger append failed; the turn is billed in memory only",
				"session", sid, "model", turn.Model, "event_id", turn.EventID, "err", err)
		}
	})
}

// spendRecordFromTurn is the ledger row for one Turn. Author carries
// the model attribution so Fold's ByAuthor is a per-model breakdown.
// EventID is forwarded directly: event-backed turns (every TurnTap site
// and digest savings sidecars) deduplicate on their ADK event ID, while
// calls with no event at call time (in-turn approver billing, internal
// compaction/side-question calls) pass "" and land with a NULL event_id
// so two calls within one clock tick are never falsely deduplicated.
func spendRecordFromTurn(turn usage.Turn) eventlog.SpendRecord {
	return eventlog.SpendRecord{
		EventID:                  turn.EventID,
		Author:                   turn.Model,
		Model:                    turn.Model,
		Backend:                  turn.Backend,
		Side:                     turn.Side,
		Tokens:                   int64(turn.InputTokens + turn.OutputTokens + turn.ThoughtsTokens),
		InputTokens:              int64(turn.InputTokens),
		CachedInputTokens:        int64(turn.CachedInputTokens),
		CacheCreationInputTokens: int64(turn.CacheCreationInputTokens),
		OutputTokens:             int64(turn.OutputTokens),
		ThoughtsTokens:           int64(turn.ThoughtsTokens),
		ToolUseTokens:            int64(turn.ToolUseTokens),
		CostUSD:                  turn.CostUSD,
		Unpriced:                 turn.Unpriced,
		At:                       turn.At,
	}
}

// turnFromSpendRecord is the inverse of spendRecordFromTurn: the Turn
// as it was billed, cost included, never re-priced. Rows written against
// a #1430-era schema (Model and per-bucket columns empty, Author and
// aggregate Tokens populated) fall back to Author and Tokens so model
// attribution and token totals survive.
func turnFromSpendRecord(rec eventlog.SpendRecord) usage.Turn {
	model := rec.Model
	if model == "" {
		model = rec.Author
	}
	inTok := int(rec.InputTokens)
	if inTok == 0 && rec.CachedInputTokens == 0 && rec.CacheCreationInputTokens == 0 &&
		rec.OutputTokens == 0 && rec.ThoughtsTokens == 0 && rec.ToolUseTokens == 0 && rec.Tokens > 0 {
		inTok = int(rec.Tokens)
	}
	return usage.Turn{
		Model:                    model,
		InputTokens:              inTok,
		CachedInputTokens:        int(rec.CachedInputTokens),
		CacheCreationInputTokens: int(rec.CacheCreationInputTokens),
		OutputTokens:             int(rec.OutputTokens),
		ThoughtsTokens:           int(rec.ThoughtsTokens),
		ToolUseTokens:            int(rec.ToolUseTokens),
		CostUSD:                  rec.CostUSD,
		At:                       rec.At,
		Unpriced:                 rec.Unpriced,
		Side:                     rec.Side,
		Backend:                  rec.Backend,
		EventID:                  rec.EventID,
	}
}

// spendRestorePath names which source a resumed tracker was restored
// from; surfaced on the session's stderr line and read by tests.
type spendRestorePath string

const (
	// spendRestoredFromLedger: the agent_budget_spend rows for the
	// session tree were replayed verbatim.
	spendRestoredFromLedger spendRestorePath = "ledger"
	// spendRestoredFromEvents: the ledger had no rows for the tree
	// (a session older than #1433), so the event stream was replayed
	// and priced at today's catalog, which also seeds the ledger.
	spendRestoredFromEvents spendRestorePath = "events"
)

// restoreResumedTracker fills a resumed session's fresh tracker with
// its history (#1433). The spend ledger is authoritative when it has
// any row for the session tree: those rows carry each call's cost as
// billed, so a catalog change between kill and resume cannot move the
// total or the point where a cost ceiling trips. Only a tree with no
// ledger rows falls back to replaying the event stream through
// usage.RebuildTrackerFromEvents, priced at the current catalog — the
// best information there is for a session that predates the ledger —
// and once that replay succeeds in full, seeds the ledger atomically
// via SpendStore.AppendBatch for the next resume.
//
// Returns the path taken and how many turns were restored. Errors on
// either path are non-fatal: the session still functions, with a
// zero (or partial) baseline, and the failure is logged.
func restoreResumedTracker(deps SessionFactoryDeps, user, sid string, tracker *usage.Tracker,
	modelName string, pricingRate usage.Pricing,
) (spendRestorePath, int) {
	h := deps.EventlogHandle
	if h == nil {
		return spendRestoredFromEvents, 0
	}
	ctx := deps.DaemonCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if h.Spend != nil {
		recs, err := h.Spend.HistoryTree(ctx, spendLedgerApp, user, sid)
		switch {
		case err != nil:
			slog.Warn("compose: spend ledger read failed; rebuilding from the event stream",
				"session", sid, "err", err)
		case len(recs) > 0:
			turns := make([]usage.Turn, 0, len(recs))
			for _, rec := range recs {
				turns = append(turns, turnFromSpendRecord(rec))
			}
			tracker.RestoreTurns(turns...)
			return spendRestoredFromLedger, len(turns)
		}
	}
	if h.Stream == nil {
		return spendRestoredFromEvents, 0
	}
	events := h.Stream.Since(ctx, 0, eventlog.WithSessionTree(spendLedgerApp, user, sid))
	// Adapter: eventlog yields (Entry, error); tracker rebuild
	// wants (*session.Event, error). Peel the event field.
	eventsSeq := func(yield func(*session.Event, error) bool) {
		for entry, err := range events {
			if !yield(entry.Event, err) {
				return
			}
		}
	}
	err := usage.RebuildTrackerFromEvents(ctx, tracker, eventsSeq, modelName,
		func(model string) usage.Pricing {
			if model == "" || model == modelName {
				return pricingRate
			}
			return usage.PriceFor(model, deps.Cfg)
		})
	if err != nil {
		// Non-fatal — the session still functions, just with
		// zero baseline aggregate. Log so operators can spot
		// silent rebuild failures, and skip seeding so a partial
		// replay cannot shadow remaining historical turns on the
		// next resume.
		fmt.Fprintf(os.Stderr, "core-agent: rebuild tracker for resumed session %s: %v\n", sid, err)
		return spendRestoredFromEvents, tracker.Totals().Turns
	}
	if h.Spend != nil {
		turns := tracker.All()
		if len(turns) > 0 {
			batch := make([]eventlog.SpendRecord, len(turns))
			for i, turn := range turns {
				batch[i] = spendRecordFromTurn(turn)
			}
			seedCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spendLedgerWriteTimeout)
			defer cancel()
			if seedErr := h.Spend.AppendBatch(seedCtx, spendLedgerApp, user, sid, batch); seedErr != nil {
				slog.Warn("compose: spend ledger seed after event rebuild failed; next resume will replay events",
					"session", sid, "err", seedErr)
			}
		}
	}
	return spendRestoredFromEvents, tracker.Totals().Turns
}
