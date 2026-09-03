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

package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/usage"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// Durable guardrail state (#643). "Simulate a restart" here means what
// a restart actually is from the agent's point of view: a brand-new
// Agent value over the same eventlog and session ID. Nothing is shared
// but the database, which is exactly the property under test.

func createTestSession(t *testing.T, h *eventlog.Handle, app, user, sid string) {
	t.Helper()
	if _, err := h.Service.Create(context.Background(), &session.CreateRequest{
		AppName: app, UserID: user, SessionID: sid,
	}); err != nil {
		t.Fatalf("session Create: %v", err)
	}
}

func countGuardrailRows(t *testing.T, a *Agent, author string) int {
	t.Helper()
	resp, err := a.eventLog.Service.Get(context.Background(), &session.GetRequest{
		AppName:   a.appName,
		UserID:    a.userID,
		SessionID: a.sessionID,
	})
	if err != nil {
		t.Fatalf("session Get: %v", err)
	}
	n := 0
	for ev := range resp.Session.Events().All() {
		if ev.Author == author {
			n++
		}
	}
	return n
}

// TestGuardrailPersist_WatchdogTripSurvivesRestart is the #643
// acceptance criterion: trip → restart → still halted.
//
// Fails on pre-#643 code in both halves — the trip writes no row, and a
// fresh agent has nothing to restore, so the successor process accepts
// the very turn the watchdog halted.
func TestGuardrailPersist_WatchdogTripSurvivesRestart(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-643-wd")

	w := &fakeWatchdog{pending: []watchdog.Alert{
		{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Reason: "looping on read_file 5x."},
	}}
	first, err := New(oneShotLLM{},
		WithEventLog(h),
		WithSession("u", "s-643-wd"),
		WithWatchdog(w, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	runTurnToCompletion(t, first)
	if tripped, _ := first.WatchdogTripped(); !tripped {
		t.Fatalf("watchdog should have tripped at the post-turn drain")
	}
	if got := countGuardrailRows(t, first, attach.GuardrailTripEventAuthor); got != 1 {
		t.Fatalf("durable trip rows = %d, want 1", got)
	}

	// The restart. A fresh watchdog with no pending alerts: if the
	// successor halts, it can only be because it restored the trip.
	second, err := New(oneShotLLM{},
		WithEventLog(h),
		WithSession("u", "s-643-wd"),
		WithWatchdog(&fakeWatchdog{}, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New (restart): %v", err)
	}
	var gotErr error
	for _, err := range second.Run(context.Background(), "keep going") {
		if err != nil {
			gotErr = err
		}
	}
	if !IsWatchdogTripped(gotErr) {
		t.Fatalf("restarted agent should refuse the turn; got err=%v", gotErr)
	}
	if tripped, reason := second.WatchdogTripped(); !tripped {
		t.Error("restarted agent reports the watchdog as untripped")
	} else if reason == "" {
		t.Error("restored halt lost its reason; the operator gets no explanation")
	}
}

// The #159 half of the same restart. The halt comes back, the operator
// clears it — and the successor process's model has no memory of the
// loop, because the feedback queue lived in the dead process. Without a
// reconstructed observation the resumed turn re-issues the same call.
//
// Fails on pre-#159 code: applyGuardrailState restored the flag and the
// reason and queued nothing, so the post-reset prompt was bare.
func TestGuardrailPersist_WatchdogFeedbackSurvivesRestart(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-159-wd")

	w := &fakeWatchdog{pending: []watchdog.Alert{{
		Signal:   "repeated-tool-call",
		Severity: watchdog.SeverityCritical,
		Reason:   "looping on read_file 5x.",
		Guidance: "You called read_file 5 times in a row.",
	}}}
	first, err := New(oneShotLLM{},
		WithEventLog(h),
		WithSession("u", "s-159-wd"),
		WithWatchdog(w, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	runTurnToCompletion(t, first)
	if tripped, _ := first.WatchdogTripped(); !tripped {
		t.Fatalf("watchdog should have tripped at the post-turn drain")
	}

	// The restart: fresh agent, fresh watchdog with nothing pending.
	rec := &recordingLLM{}
	second, err := New(rec,
		WithEventLog(h),
		WithSession("u", "s-159-wd"),
		WithWatchdog(&fakeWatchdog{}, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New (restart): %v", err)
	}
	// First turn is refused — that's #643 — and the refusal is what
	// triggers the restore that queues the observation.
	for range second.Run(context.Background(), "keep going") { //nolint:revive // draining the refusal
	}
	second.ResetWatchdog()
	for _, err := range second.Run(context.Background(), "resumed") {
		if err != nil {
			t.Fatalf("post-reset turn: %v", err)
		}
	}
	got := flattenText(rec.lastRequest().Contents)
	if !strings.Contains(got, watchdog.FeedbackHeader) {
		t.Fatalf("post-restart, post-reset turn carries no watchdog observation: %q", got)
	}
	if !strings.Contains(got, "read_file") {
		t.Errorf("reconstructed observation lost the halting reason: %q", got)
	}
}

// A reset before the restart must survive it too — otherwise the
// operator clears the halt, the pod rolls, and the halt comes back with
// no way to make it stick.
func TestGuardrailPersist_ResetSurvivesRestart(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-643-reset")

	w := &fakeWatchdog{pending: []watchdog.Alert{
		{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Reason: "looping."},
	}}
	first, err := New(oneShotLLM{},
		WithEventLog(h),
		WithSession("u", "s-643-reset"),
		WithWatchdog(w, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	runTurnToCompletion(t, first)

	first.ResetWatchdog()
	first.RecordGuardrailReset([]string{attach.GuardrailWatchdog}, 0, "alice@example.com")
	if got := countGuardrailRows(t, first, attach.GuardrailResetEventAuthor); got != 1 {
		t.Fatalf("durable reset rows = %d, want 1", got)
	}

	second, err := New(oneShotLLM{},
		WithEventLog(h),
		WithSession("u", "s-643-reset"),
		WithWatchdog(&fakeWatchdog{}, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New (restart): %v", err)
	}
	for _, err := range second.Run(context.Background(), "resumed") {
		if err != nil {
			t.Fatalf("restarted agent should accept turns after a reset; got %v", err)
		}
	}
}

// A reset that clears nothing and adds nothing is not an event. A
// defensive `/guardrail reset` against a healthy session shouldn't leave
// a row implying something was wrong.
func TestRecordGuardrailReset_NoOpWritesNothing(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-643-noop")

	a, err := New(minimalLLM{}, WithEventLog(h), WithSession("u", "s-643-noop"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	a.RecordGuardrailReset(nil, 0, "alice@example.com")
	if got := countGuardrailRows(t, a, attach.GuardrailResetEventAuthor); got != 0 {
		t.Errorf("reset rows = %d, want 0 for a reset that cleared nothing", got)
	}
}

// Budget an operator bought before the restart is re-applied on top of
// the CONFIGURED ceiling. Losing it would re-halt the session at the old
// bar the moment it resumed — the operator's $5 evaporating in a pod
// roll they didn't cause.
func TestGuardrailPersist_AddedBudgetSurvivesRestart(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-643-budget")

	first, err := New(minimalLLM{},
		WithEventLog(h),
		WithSession("u", "s-643-budget"),
		WithCostCeiling(CostCeiling{MaxSessionUSD: 10}),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	if _, err := first.AddSessionCostBudget(5); err != nil {
		t.Fatalf("AddSessionCostBudget: %v", err)
	}
	first.RecordGuardrailReset(nil, 5, "alice@example.com")

	second, err := New(minimalLLM{},
		WithEventLog(h),
		WithSession("u", "s-643-budget"),
		WithCostCeiling(CostCeiling{MaxSessionUSD: 10}),
	)
	if err != nil {
		t.Fatalf("agent.New (restart): %v", err)
	}
	if err := second.RestoreGuardrails(context.Background()); err != nil {
		t.Fatalf("RestoreGuardrails: %v", err)
	}
	if got := second.CostCeilingLimits().MaxSessionUSD; got != 15 {
		t.Errorf("restored MaxSessionUSD = %v, want 15 ($10 configured + $5 granted)", got)
	}
}

// Durable state restores a halt the operator hasn't cleared; it does not
// re-enable a backstop the operator has since turned off. An operator
// who restarts with --watchdog=warn has said "stop halting me", and a
// row from the previous process must not overrule that.
func TestGuardrailPersist_ConfigWinsOverRestoredState(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-643-config")

	w := &fakeWatchdog{pending: []watchdog.Alert{
		{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Reason: "looping."},
	}}
	first, err := New(oneShotLLM{},
		WithEventLog(h),
		WithSession("u", "s-643-config"),
		WithWatchdog(w, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	runTurnToCompletion(t, first)

	// Restart in warn mode — same session, same durable trip row.
	warn, err := New(oneShotLLM{},
		WithEventLog(h),
		WithSession("u", "s-643-config"),
		WithWatchdog(&fakeWatchdog{}, nil),
		// no WithWatchdogEnforce
	)
	if err != nil {
		t.Fatalf("agent.New (warn restart): %v", err)
	}
	for _, err := range warn.Run(context.Background(), "hi") {
		if err != nil {
			t.Fatalf("warn-mode restart should accept turns; got %v", err)
		}
	}
	if tripped, _ := warn.WatchdogTripped(); tripped {
		t.Error("warn-mode agent restored an enforce-mode halt")
	}

	// Likewise a budget grant against a ceiling that is no longer
	// configured: adding runway to a disabled bound would silently ARM
	// it, which is a tighter posture than the operator asked for.
	first.RecordGuardrailReset(nil, 5, "alice@example.com")
	noCeiling, err := New(minimalLLM{}, WithEventLog(h), WithSession("u", "s-643-config"))
	if err != nil {
		t.Fatalf("agent.New (no-ceiling restart): %v", err)
	}
	if err := noCeiling.RestoreGuardrails(context.Background()); err != nil {
		t.Fatalf("RestoreGuardrails: %v", err)
	}
	if got := noCeiling.CostCeilingLimits(); got.active() {
		t.Errorf("restore armed a ceiling the operator did not configure: %+v", got)
	}
}

// A cost trip restores the same way, and the restored halt is evaluated
// against the ceiling this process was configured with.
func TestGuardrailPersist_CostTripSurvivesRestart(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-643-cost")

	tr := usage.NewTracker()
	first, err := New(minimalLLM{},
		WithEventLog(h),
		WithSession("u", "s-643-cost"),
		WithUsageTracker(tr),
		WithCostCeiling(CostCeiling{MaxSessionUSD: 0.10}),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	tr.Append("test", 1_500_000, 0, usage.Pricing{InputPerMTok: 0.10}) // $0.15 > $0.10
	first.maybeEnforceCostCeiling()
	if tripped, _ := first.CostCeilingTripped(); !tripped {
		t.Fatalf("cost ceiling should have tripped")
	}

	second, err := New(minimalLLM{},
		WithEventLog(h),
		WithSession("u", "s-643-cost"),
		WithUsageTracker(usage.NewTracker()),
		WithCostCeiling(CostCeiling{MaxSessionUSD: 0.10}),
	)
	if err != nil {
		t.Fatalf("agent.New (restart): %v", err)
	}
	var gotErr error
	for _, err := range second.Run(context.Background(), "keep going") {
		if err != nil {
			gotErr = err
		}
	}
	if !IsCostCeilingExceeded(gotErr) {
		t.Fatalf("restarted agent should refuse the turn; got err=%v", gotErr)
	}
}

// No eventlog → no persistence, and no panic. Embedders that run
// entirely in memory keep exactly the behavior they had.
func TestGuardrailPersist_NoEventLogIsNoOp(t *testing.T) {
	t.Parallel()
	a, err := New(oneShotLLM{},
		WithSession("u", "s-643-mem"),
		WithWatchdog(&fakeWatchdog{pending: []watchdog.Alert{
			{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Reason: "looping."},
		}}, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	runTurnToCompletion(t, a)
	if tripped, _ := a.WatchdogTripped(); !tripped {
		t.Error("in-memory enforce should still trip")
	}
	a.RecordGuardrailReset([]string{attach.GuardrailWatchdog}, 5, "alice")
	if err := a.RestoreGuardrails(context.Background()); err != nil {
		t.Errorf("RestoreGuardrails with no eventlog = %v, want nil", err)
	}
}

// TestMaybeEnforceCostCeiling_ResumedSessionDoesNotFalseTripPerTurn is
// the latent bug #643's restore work surfaced: on a resumed session the
// tracker is rebuilt with the whole prior spend BEFORE the agent is
// constructed (pkg/compose/multi_session.go), so a per-turn check with a
// zero baseline measures the entire history as this turn's delta.
//
// Fails on pre-fix code: without the turnStartCostSet gate the per-turn
// ceiling trips on a turn that has not spent a cent.
func TestMaybeEnforceCostCeiling_ResumedSessionDoesNotFalseTripPerTurn(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	tr.Append("prior-turns", 1_500_000, 0, usage.Pricing{InputPerMTok: 0.10}) // $0.15 of history
	a := &Agent{
		tracker:     tr,
		costCeiling: CostCeiling{MaxTurnUSD: 0.10},
	}
	// No snapshotTurnStartCost: this process has not run a turn yet.
	a.maybeEnforceCostCeiling()
	if tripped, reason := a.CostCeilingTripped(); tripped {
		t.Errorf("per-turn ceiling tripped on replayed history, not on this turn's spend: %s", reason)
	}

	// The per-SESSION bound is unaffected — it reads the accumulator
	// directly, which is exactly what should carry across a resume.
	b := &Agent{
		tracker:     tr,
		costCeiling: CostCeiling{MaxSessionUSD: 0.10},
	}
	b.maybeEnforceCostCeiling()
	if tripped, _ := b.CostCeilingTripped(); !tripped {
		t.Error("per-session ceiling should still trip on replayed spend")
	}
}

// The other restart tests share one eventlog.Handle, so they cannot
// prove the rows survive the database rather than a live session cache.
// This one closes the handle and reopens the same file — a real process
// boundary — which is also the only way to exercise the metadata's JSON
// round-trip: in-process the "reset" list is a []string and comes back
// from the column as []any, and a fold that handles only one of those
// works in tests and fails in production, or the reverse.
func TestGuardrailPersist_SurvivesAReopenedDatabase(t *testing.T) {
	t.Parallel()
	dsn := filepath.Join(t.TempDir(), "session.db")
	first, err := eventlog.Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	createTestSession(t, first, "core-agent", "u", "s-643-reopen")

	a, err := New(minimalLLM{},
		WithEventLog(first),
		WithSession("u", "s-643-reopen"),
		WithCostCeiling(CostCeiling{MaxSessionUSD: 10}),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	a.queueOutOfBandEvent(attach.NewGuardrailTripEvent(attach.GuardrailCostCeiling, "blew the budget"))
	a.queueOutOfBandEvent(attach.NewGuardrailTripEvent(attach.GuardrailWatchdog, "looping"))
	a.RecordGuardrailReset([]string{attach.GuardrailWatchdog}, 5, "alice@example.com")
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := eventlog.Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open (reopen): %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	b, err := New(minimalLLM{},
		WithEventLog(second),
		WithSession("u", "s-643-reopen"),
		WithCostCeiling(CostCeiling{MaxSessionUSD: 10}),
		WithWatchdog(&fakeWatchdog{}, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New (reopen): %v", err)
	}
	if err := b.RestoreGuardrails(context.Background()); err != nil {
		t.Fatalf("RestoreGuardrails: %v", err)
	}
	if tripped, reason := b.CostCeilingTripped(); !tripped {
		t.Error("cost halt did not survive a reopened database")
	} else if reason != "blew the budget" {
		t.Errorf("restored reason = %q, want the original verbatim", reason)
	}
	// The reset row named only the watchdog, so it must clear that halt
	// and leave the cost halt standing. This is the assertion that
	// actually exercises the []any decoding: a fold that dropped the
	// list would leave the watchdog tripped here.
	if tripped, _ := b.WatchdogTripped(); tripped {
		t.Error("the reset row naming watchdog did not clear the watchdog halt after a reopen")
	}
	if got := b.CostCeilingLimits().MaxSessionUSD; got != 15 {
		t.Errorf("restored MaxSessionUSD = %v, want 15 (the granted $5 survived the round-trip)", got)
	}
}

// A restore that fails must be retried, not latched. A sync.Once here
// would mean one transient database error at the wrong moment leaves
// the backstop disarmed for the whole life of the process — which is
// the failure mode durable state exists to remove, reintroduced one
// layer up.
//
// Fails on a sync.Once implementation: the second call is swallowed and
// the halt is never restored.
func TestRestoreGuardrails_RetriesAfterAFailedRead(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-643-retry")

	writer, err := New(minimalLLM{},
		WithEventLog(h),
		WithSession("u", "s-643-retry"),
		WithCostCeiling(CostCeiling{MaxSessionUSD: 10}),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	writer.queueOutOfBandEvent(attach.NewGuardrailTripEvent(attach.GuardrailCostCeiling, "blew the budget"))

	// The restart, with a first read that cannot succeed: the session
	// ID it looks for does not exist yet.
	reader, err := New(minimalLLM{},
		WithEventLog(h),
		WithSession("u", "s-643-missing"),
		WithCostCeiling(CostCeiling{MaxSessionUSD: 10}),
	)
	if err != nil {
		t.Fatalf("agent.New (restart): %v", err)
	}
	if err := reader.RestoreGuardrails(context.Background()); err == nil {
		t.Fatal("setup: expected the first restore to fail on a missing session")
	}
	if tripped, _ := reader.CostCeilingTripped(); tripped {
		t.Fatal("a failed restore must not apply state")
	}

	// Point it at the session that does exist and try again. A latched
	// implementation never re-reads.
	reader.sessionID = "s-643-retry"
	if err := reader.RestoreGuardrails(context.Background()); err != nil {
		t.Fatalf("second RestoreGuardrails: %v", err)
	}
	if tripped, _ := reader.CostCeilingTripped(); !tripped {
		t.Error("a retried restore did not apply the halt")
	}
}

// Restoring twice must not double-count granted budget — the operator
// bought $5 of runway, not $10.
func TestRestoreGuardrails_AppliesBudgetOnce(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-643-once")

	a, err := New(minimalLLM{},
		WithEventLog(h),
		WithSession("u", "s-643-once"),
		WithCostCeiling(CostCeiling{MaxSessionUSD: 10}),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	a.RecordGuardrailReset(nil, 5, "alice@example.com")

	b, err := New(minimalLLM{},
		WithEventLog(h),
		WithSession("u", "s-643-once"),
		WithCostCeiling(CostCeiling{MaxSessionUSD: 10}),
	)
	if err != nil {
		t.Fatalf("agent.New (restart): %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := b.RestoreGuardrails(context.Background()); err != nil {
			t.Fatalf("RestoreGuardrails #%d: %v", i, err)
		}
	}
	if got := b.CostCeilingLimits().MaxSessionUSD; got != 15 {
		t.Errorf("MaxSessionUSD = %v after three restores, want 15", got)
	}
}
