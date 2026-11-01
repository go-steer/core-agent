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

package compose

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

// spendLedgerFixture is one created session that ran one priced turn
// under rate P1, with its eventlog handle and deps ready for a resume.
type spendLedgerFixture struct {
	h     *eventlog.Handle
	deps  SessionFactoryDeps
	sid   string
	live  usage.Totals
	liveT []usage.Turn
}

// runSessionUnderRate creates a session, runs one scripted turn and
// bills it to the session tracker at rate, then evicts the session.
// The parent is billed the way runner.WakeLoop bills it: a TurnTap over
// Agent.Run committing into Agent().Tracker().
func runSessionUnderRate(t *testing.T, ctx context.Context, rate usage.Pricing) spendLedgerFixture {
	t.Helper()
	h, err := eventlog.Open(ctx, sqlite.Open(filepath.Join(t.TempDir(), "s.db")))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	llm := &scriptedTurnLLM{
		name: "claude-opus-4-6",
		turns: []*adkmodel.LLMResponse{{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 10_000, CandidatesTokenCount: 5_000, TotalTokenCount: 15_000,
			},
			TurnComplete: true,
		}},
	}
	noTitle := false
	deps := SessionFactoryDeps{
		DaemonCtx:      ctx,
		Model:          llm,
		Template:       permissions.New(permissions.Options{Mode: permissions.ModeYolo}),
		EventlogHandle: h,
		PricingRate:    rate,
		Cfg:            &config.Config{Agent: config.AgentConfig{SessionTitle: &noTitle}},
		NoCompact:      true,
		CheckpointMode: config.CheckpointModeOff,
	}
	sid := "sid-spend-ledger-" + t.Name()
	ad, cancel, err := ReproduceAgent(deps, auth.Anonymous, sid, "created")
	if err != nil {
		t.Fatalf("ReproduceAgent(created): %v", err)
	}
	var tap usage.TurnTap
	for ev, runErr := range ad.Agent().Run(ctx, "start") {
		if runErr != nil {
			t.Fatalf("Run: %v", runErr)
		}
		tap.Observe(ev)
		if u, ok := tap.Commit(ev); ok {
			ad.Agent().Tracker().AppendUsage(llm.Name(), u, rate)
		}
	}
	live := ad.Agent().Tracker().Totals()
	if live.Turns != 1 || math.Abs(live.CostUSD-rate.CostUSD(10_000, 5_000)) > 1e-9 {
		t.Fatalf("live totals = %+v, want one turn at P1 (%f)", live, rate.CostUSD(10_000, 5_000))
	}
	liveTurns := ad.Agent().Tracker().All()
	cancel()
	return spendLedgerFixture{h: h, deps: deps, sid: sid, live: live, liveT: liveTurns}
}

func doubled(p usage.Pricing) usage.Pricing {
	p.InputPerMTok *= 2
	p.CachedInputPerMTok *= 2
	p.CacheCreationInputPerMTok *= 2
	p.CacheCreation1hInputPerMTok *= 2
	p.OutputPerMTok *= 2
	return p
}

// TestReproduceAgent_ResumedSpendIsNotRepricedByACatalogChange is the
// #1433 regression test. A session billed under P1 is killed, the
// catalog moves to P2, and the session is resumed. Pre-fix,
// ReproduceAgent rebuilt the tracker from the event stream and priced
// every historical turn at P2 — so the session's total, and the point
// where --max-session-cost-usd trips, moved with a /pricing/refresh.
// With the ledger, the resumed total is what was actually billed.
func TestReproduceAgent_ResumedSpendIsNotRepricedByACatalogChange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	p1 := usage.PriceFor("claude-opus-4-6", nil)
	if p1.Unpriced {
		t.Fatal("expected a built-in rate for claude-opus-4-6")
	}
	fx := runSessionUnderRate(t, ctx, p1)

	// The write path: the live turn is in the ledger, keyed by its
	// ADK event, at the P1 price.
	rows, err := fx.h.Spend.HistoryTree(ctx, spendLedgerApp, auth.Anonymous.Identity, fx.sid)
	if err != nil {
		t.Fatalf("HistoryTree: %v", err)
	}
	if len(rows) != 1 || rows[0].EventID == "" || math.Abs(rows[0].CostUSD-fx.live.CostUSD) > 1e-9 {
		t.Errorf("ledger rows after the live turn = %+v, want one row with an event id at the live cost %f", rows, fx.live.CostUSD)
	} else if rows[0].EventID != fx.liveT[0].EventID {
		t.Errorf("ledger EventID = %q, want the tracker's %q", rows[0].EventID, fx.liveT[0].EventID)
	}

	// The catalog moves between kill and resume.
	p2 := doubled(p1)
	deps := fx.deps
	deps.PricingRate = p2

	adResumed, cancelResumed, err := ReproduceAgent(deps, auth.Anonymous, fx.sid, "resumed")
	if err != nil {
		t.Fatalf("ReproduceAgent(resumed): %v", err)
	}
	t.Cleanup(cancelResumed)

	resumed := adResumed.Agent().Tracker().Totals()
	if resumed.Turns != fx.live.Turns || resumed.InputTokens != fx.live.InputTokens || resumed.OutputTokens != fx.live.OutputTokens {
		t.Errorf("resumed totals = %+v, want the live shape %+v", resumed, fx.live)
	}
	if math.Abs(resumed.CostUSD-fx.live.CostUSD) > 1e-9 {
		t.Errorf("resumed CostUSD = %f, want the P1 figure %f as billed (P2 would be %f): history was re-priced at resume time",
			resumed.CostUSD, fx.live.CostUSD, p2.CostUSD(10_000, 5_000))
	}
	last, ok := adResumed.Agent().Tracker().Last()
	if !ok || last.EventID != fx.liveT[0].EventID || last.At.Sub(fx.liveT[0].At).Abs() > time.Millisecond {
		// An event-stream rebuild would stamp At with the resume
		// time; the ledger restores the time the call was made.
		t.Errorf("resumed Last() = %+v (ok=%v), want the live turn %+v restored verbatim", last, ok, fx.liveT[0])
	}

	// Resuming again must not duplicate anything: the ledger restore
	// does not write, and the row count stays at one.
	rows, err = fx.h.Spend.HistoryTree(ctx, spendLedgerApp, auth.Anonymous.Identity, fx.sid)
	if err != nil {
		t.Fatalf("HistoryTree after resume: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("ledger has %d rows after resume, want 1 (a ledger restore must not re-append)", len(rows))
	}
}

// TestRestoreResumedTracker_LedgerFirstThenEventsFallback pins the path
// choice directly: rows in the ledger win and are replayed verbatim;
// an empty ledger (a session older than #1433) falls back to the event
// stream at today's price AND seeds the ledger so the next resume takes
// the ledger path.
func TestRestoreResumedTracker_LedgerFirstThenEventsFallback(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	p1 := usage.PriceFor("claude-opus-4-6", nil)
	fx := runSessionUnderRate(t, ctx, p1)
	p2 := doubled(p1)
	user := auth.Anonymous.Identity

	// Ledger path.
	tr := usage.NewTracker()
	installSpendLedger(ctx, fx.h.Spend, tr, user, fx.sid)
	path, n := restoreResumedTracker(fx.deps, user, fx.sid, tr, "claude-opus-4-6", p2)
	if path != spendRestoredFromLedger || n != 1 {
		t.Fatalf("restore with ledger rows = (%s, %d), want (ledger, 1)", path, n)
	}
	if got := tr.Totals().CostUSD; math.Abs(got-fx.live.CostUSD) > 1e-9 {
		t.Errorf("ledger-restored cost = %f, want %f", got, fx.live.CostUSD)
	}

	// Wipe the ledger: the shape a pre-#1433 session is in.
	// Note installSpendLedger is deliberately NOT called first, matching
	// ReproduceAgent: restoreResumedTracker seeds atomically via AppendBatch.
	if err := fx.h.DB.WithContext(ctx).Exec("DELETE FROM agent_budget_spend").Error; err != nil {
		t.Fatalf("wipe ledger: %v", err)
	}
	tr = usage.NewTracker()
	path, n = restoreResumedTracker(fx.deps, user, fx.sid, tr, "claude-opus-4-6", p2)
	if path != spendRestoredFromEvents || n != 1 {
		t.Fatalf("restore with an empty ledger = (%s, %d), want (events, 1)", path, n)
	}
	wantP2 := p2.CostUSD(10_000, 5_000)
	if got := tr.Totals(); got.InputTokens != 10_000 || got.OutputTokens != 5_000 || math.Abs(got.CostUSD-wantP2) > 1e-9 {
		t.Errorf("events-restored totals = %+v, want 10000/5000 tokens at today's price %f", got, wantP2)
	}
	rows, err := fx.h.Spend.HistoryTree(ctx, spendLedgerApp, user, fx.sid)
	if err != nil {
		t.Fatalf("HistoryTree: %v", err)
	}
	if len(rows) != 1 || rows[0].EventID != fx.liveT[0].EventID || math.Abs(rows[0].CostUSD-wantP2) > 1e-9 {
		t.Fatalf("ledger after an events-path restore = %+v, want one row keyed by the live event id at %f (the replay must seed the ledger)", rows, wantP2)
	}

	// Third resume: the seeded ledger is now the source.
	tr = usage.NewTracker()
	path, n = restoreResumedTracker(fx.deps, user, fx.sid, tr, "claude-opus-4-6", p1)
	if path != spendRestoredFromLedger || n != 1 || math.Abs(tr.Totals().CostUSD-wantP2) > 1e-9 {
		t.Errorf("restore after seeding = (%s, %d, $%f), want (ledger, 1, $%f): the seeded price sticks and is not re-priced back",
			path, n, tr.Totals().CostUSD, wantP2)
	}
}

// TestReproduceAgent_ResumedWithoutLedgerRowsRebuildsFromEvents is the
// end-to-end fallback: a session whose ledger is empty still comes back
// with its token totals from the event stream.
func TestReproduceAgent_ResumedWithoutLedgerRowsRebuildsFromEvents(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	p1 := usage.PriceFor("claude-opus-4-6", nil)
	fx := runSessionUnderRate(t, ctx, p1)
	if err := fx.h.DB.WithContext(ctx).Exec("DELETE FROM agent_budget_spend").Error; err != nil {
		t.Fatalf("wipe ledger: %v", err)
	}

	adResumed, cancelResumed, err := ReproduceAgent(fx.deps, auth.Anonymous, fx.sid, "resumed")
	if err != nil {
		t.Fatalf("ReproduceAgent(resumed): %v", err)
	}
	t.Cleanup(cancelResumed)
	resumed := adResumed.Agent().Tracker().Totals()
	if resumed.Turns != 1 || resumed.InputTokens != 10_000 || resumed.OutputTokens != 5_000 || math.Abs(resumed.CostUSD-fx.live.CostUSD) > 1e-9 {
		t.Errorf("resumed totals without ledger rows = %+v, want the event-stream rebuild %+v", resumed, fx.live)
	}
}

// TestSpendLedgerAppendFailureDoesNotFailTheTurn: a closed database
// behind the ledger logs and the tracker still records the turn.
func TestSpendLedgerAppendFailureDoesNotFailTheTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, err := eventlog.Open(ctx, sqlite.Open(filepath.Join(t.TempDir(), "s.db")))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	tr := usage.NewTracker()
	installSpendLedger(ctx, h.Spend, tr, "anon", "sid")
	turn := tr.AppendUsage("m", usage.TurnUsage{InputTokens: 10, OutputTokens: 5, EventID: "e1"}, usage.Pricing{InputPerMTok: 1, OutputPerMTok: 1})
	if turn.EventID != "e1" || tr.Totals().Turns != 1 {
		t.Fatalf("turn after a failed ledger write = %+v / %+v, want it billed in memory", turn, tr.Totals())
	}

	// Event-less turns keep an empty EventID so two side calls within one
	// clock tick are stored as separate NULL-keyed rows rather than
	// colliding on a synthetic hash.
	side := usage.Turn{Model: "approver", Side: true, InputTokens: 3, At: time.Unix(1, 2)}
	if rec := spendRecordFromTurn(side); rec.EventID != "" {
		t.Errorf("event-less turn EventID = %q, want empty (NULL in agent_budget_spend)", rec.EventID)
	}
}

// TestInstallSpendLedger_PersistsDuringShutdownDrainWhenBaseCtxCanceled
// covers skeptic P1 #1: on daemon shutdown, cancelWakeLoops() cancels
// DaemonCtx before wakeLoops.WaitFor drains in-flight turns (and
// spawn_agent turns run under context.WithoutCancel). A turn committing
// during that drain window must still persist its spend row.
func TestInstallSpendLedger_PersistsDuringShutdownDrainWhenBaseCtxCanceled(t *testing.T) {
	t.Parallel()
	daemonCtx, cancelDaemon := context.WithCancel(context.Background())
	h, err := eventlog.Open(context.Background(), sqlite.Open(filepath.Join(t.TempDir(), "s.db")))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	tr := usage.NewTracker()
	installSpendLedger(daemonCtx, h.Spend, tr, "anon", "sid-drain")

	// Simulate shutdown: DaemonCtx cancels while an in-flight turn is draining.
	cancelDaemon()

	tr.AppendUsage("claude-opus-4-6", usage.TurnUsage{
		InputTokens:  4_000,
		OutputTokens: 1_000,
		EventID:      "ev-drain-1",
	}, usage.Pricing{InputPerMTok: 15, OutputPerMTok: 75})

	rows, err := h.Spend.HistoryTree(context.Background(), spendLedgerApp, "anon", "sid-drain")
	if err != nil {
		t.Fatalf("HistoryTree: %v", err)
	}
	if len(rows) != 1 || rows[0].EventID != "ev-drain-1" {
		t.Fatalf("ledger rows after drain commit = %+v, want 1 row for ev-drain-1", rows)
	}
}

// TestRestoreResumedTracker_AbortedReplayLeavesLedgerEmpty covers skeptic
// P1 #2: if event-stream replay fails or is cancelled mid-stream for a
// pre-ledger session, zero rows may be committed to agent_budget_spend so
// the next resume still takes the event-stream path rather than restoring
// a truncated ledger.
func TestRestoreResumedTracker_AbortedReplayLeavesLedgerEmpty(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	p1 := usage.PriceFor("claude-opus-4-6", nil)
	fx := runSessionUnderRate(t, ctx, p1)
	user := auth.Anonymous.Identity
	if err := fx.h.DB.WithContext(ctx).Exec("DELETE FROM agent_budget_spend").Error; err != nil {
		t.Fatalf("wipe ledger: %v", err)
	}

	// Cancel DaemonCtx before restoreResumedTracker runs so Since/Rebuild aborts.
	canceledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	abortedDeps := fx.deps
	abortedDeps.DaemonCtx = canceledCtx

	tr := usage.NewTracker()
	restoreResumedTracker(abortedDeps, user, fx.sid, tr, "claude-opus-4-6", p1)

	rows, err := fx.h.Spend.HistoryTree(ctx, spendLedgerApp, user, fx.sid)
	if err != nil {
		t.Fatalf("HistoryTree after aborted replay: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("ledger has %d rows after aborted replay, want 0", len(rows))
	}

	// Subsequent resume with a live context still sees an empty ledger and
	// restores the full turn history cleanly.
	tr2 := usage.NewTracker()
	path, n := restoreResumedTracker(fx.deps, user, fx.sid, tr2, "claude-opus-4-6", p1)
	if path != spendRestoredFromEvents || n != 1 {
		t.Fatalf("subsequent restore = (%s, %d), want (events, 1)", path, n)
	}
}

// TestTurnFromSpendRecord_Legacy1430RowFallback covers skeptic P3: a row
// written against the pre-#1433 schema (Model and per-bucket token columns
// zero, Author and aggregate Tokens populated) restores Model and tokens.
func TestTurnFromSpendRecord_Legacy1430RowFallback(t *testing.T) {
	t.Parallel()
	turn := turnFromSpendRecord(eventlog.SpendRecord{
		Author:  "gemini-2.5-flash",
		Tokens:  1234,
		CostUSD: 0.012,
	})
	if turn.Model != "gemini-2.5-flash" || turn.InputTokens != 1234 || turn.CostUSD != 0.012 {
		t.Fatalf("legacy row restored as %+v, want Model=gemini-2.5-flash InputTokens=1234 CostUSD=0.012", turn)
	}
}
