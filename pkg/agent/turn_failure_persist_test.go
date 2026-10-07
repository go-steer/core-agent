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
	"errors"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/usage"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// Durable per-turn failures (#1258). Before this, a per-turn guardrail
// trip and the turn error a turn ended with reached a log line and a
// typed frame for whoever was attached — and nothing else. Every test
// here drives the real Run loop over a real sqlite eventlog, because
// the defect was the absence of a WRITE, which only shows up in what
// the session holds afterwards.

// errThenRecordLLM fails its first call with err, then answers "ok" and
// records every request — so a test can fail one turn and then read
// what the next turn's model was actually shown.
type errThenRecordLLM struct {
	err error

	mu       sync.Mutex
	calls    int
	requests []*adkmodel.LLMRequest
}

func (*errThenRecordLLM) Name() string { return "err-then-record" }

func (l *errThenRecordLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	l.calls++
	first := l.calls == 1
	l.requests = append(l.requests, req)
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if first {
			yield(nil, l.err)
			return
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ok"}}},
			TurnComplete: true,
		}, nil)
	}
}

func (l *errThenRecordLLM) lastRequest() *adkmodel.LLMRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.requests[len(l.requests)-1]
}

// sessionEvents reads every persisted event of a's session.
func sessionEvents(t *testing.T, a *Agent) []*session.Event {
	t.Helper()
	resp, err := a.eventLog.Service.Get(context.Background(), &session.GetRequest{
		AppName: a.appName, UserID: a.userID, SessionID: a.sessionID,
	})
	if err != nil {
		t.Fatalf("session Get: %v", err)
	}
	var out []*session.Event
	for ev := range resp.Session.Events().All() {
		out = append(out, ev)
	}
	return out
}

func rowsBy(events []*session.Event, author string) []*session.Event {
	var out []*session.Event
	for _, ev := range events {
		if ev.Author == author {
			out = append(out, ev)
		}
	}
	return out
}

// failureFrames captures the typed guardrail-trip and turn-error frames
// with their full payloads (terminalRecorder flattens turn errors to a
// kind, and the event_id is what these tests are about).
type failureFrames struct {
	mu    sync.Mutex
	errs  []attach.TurnError
	trips []attach.GuardrailTrip
}

func (c *failureFrames) attachTo(a *Agent) {
	a.SetOperatorEventEmitter(func(eventType string, payload any) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch eventType {
		case attach.EventTurnError:
			c.errs = append(c.errs, payload.(attach.TurnError))
		case attach.EventGuardrailTrip:
			c.trips = append(c.trips, payload.(attach.GuardrailTrip))
		}
	})
}

func (c *failureFrames) turnErrors() []attach.TurnError {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]attach.TurnError(nil), c.errs...)
}

func (c *failureFrames) guardrailTrips() []attach.GuardrailTrip {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]attach.GuardrailTrip(nil), c.trips...)
}

// burnToCut drives one turn of burnLoopLLM against a $0.05 per-turn
// ceiling, so the in-turn arm cuts it, and returns every event Run
// yielded — which is everything FinalText, the subagent tool and the
// autonomous loop ever read.
func burnToCut(t *testing.T, a *Agent, tr *usage.Tracker, llm *burnLoopLLM) []*session.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var tap usage.TurnTap
	var yielded []*session.Event
	for ev := range a.Run(ctx, "go") { //nolint:revive // the cut surfaces as a cancellation
		if ev != nil {
			yielded = append(yielded, ev)
		}
		tap.Observe(ev)
		if u, ok := tap.Commit(ev); ok {
			tr.AppendUsage(llm.Name(), u, usage.Pricing{InputPerMTok: 10})
		}
	}
	if ctx.Err() != nil {
		t.Fatal("the turn ran to the deadline: the per-turn ceiling never cut it")
	}
	return yielded
}

func newCutAgent(t *testing.T, h *eventlog.Handle, sid string, llm adkmodel.LLM, tr *usage.Tracker) *Agent {
	t.Helper()
	createTestSession(t, h, "core-agent", "u", sid)
	a, err := New(llm,
		WithEventLog(h),
		WithSession("u", sid),
		WithUsageTracker(tr),
		WithCostCeiling(CostCeiling{MaxTurnUSD: 0.05}),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}

// TestTurnFailure_PerTurnCutIsDurable is the #1258 defect in its live
// shape: --max-turn-cost-usd cuts a turn. One trip row, one turn-error
// row, no halt row, and each typed frame names its row.
//
// Fails on pre-#1258 code: neither row is written.
func TestTurnFailure_PerTurnCutIsDurable(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	tr := usage.NewTracker()
	llm := &burnLoopLLM{perCallIn: 1000}
	a := newCutAgent(t, h, "s-1258-cut", llm, tr)
	frames := &failureFrames{}
	frames.attachTo(a)

	yielded := burnToCut(t, a, tr, llm)

	events := sessionEvents(t, a)
	trips := rowsBy(events, attach.GuardrailTurnTripEventAuthor)
	errs := rowsBy(events, attach.TurnErrorEventAuthor)
	if len(trips) != 1 || len(errs) != 1 {
		t.Fatalf("durable rows: %d guardrail-turn-trip, %d turn-error; want 1 and 1", len(trips), len(errs))
	}
	if n := len(rowsBy(events, attach.GuardrailTripEventAuthor)); n != 0 {
		t.Errorf("a per-turn cut wrote %d halt rows; a restart would re-arm a halt that never happened", n)
	}

	gt, ok := attach.GuardrailTurnTrip(trips[0])
	if !ok || gt.Guardrail != attach.GuardrailCostCeiling || !gt.HaltedTurn || !strings.Contains(gt.Reason, "per-turn") {
		t.Errorf("trip row reads back as %+v (ok=%v)", gt, ok)
	}
	te, cutBy, ok := attach.TurnErrorRow(errs[0])
	if !ok || te.Kind != attach.TurnErrorCanceled || cutBy != attach.TurnErrorCostCeiling {
		t.Errorf("turn-error row reads back as %+v cut_by=%q (ok=%v)", te, cutBy, ok)
	}
	if _, ok := errs[0].CustomMetadata["prompt_id"].(string); !ok {
		t.Error("turn-error row of a real turn carries no prompt_id")
	}

	// The typed frames name the rows, so a client holding both counts one.
	if typed := frames.guardrailTrips(); len(typed) != 1 || typed[0].EventID != trips[0].ID {
		t.Errorf("guardrail-trip frame event_id = %+v, want %q", typed, trips[0].ID)
	}
	if typed := frames.turnErrors(); len(typed) != 1 || typed[0].EventID != errs[0].ID {
		t.Errorf("turn-error frame event_id = %+v, want %q", typed, errs[0].ID)
	}

	// Must NOT reach FinalText: everything that collects a turn's text
	// reads Run's yielded events, and the rows are written out of band.
	for _, ev := range yielded {
		if ev.Author == attach.TurnErrorEventAuthor || ev.Author == attach.GuardrailTurnTripEventAuthor {
			t.Errorf("Run yielded a failure row (%s) to its caller", ev.Author)
		}
	}
}

// TestTurnFailure_HaltWritesOnlyTheHaltRow pins exactly-once for the
// other shape: a trip that halts the session is recorded by the #643
// halt row alone, and the frame names THAT row.
func TestTurnFailure_HaltWritesOnlyTheHaltRow(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-1258-halt")
	a, err := New(oneShotLLM{},
		WithEventLog(h),
		WithSession("u", "s-1258-halt"),
		WithWatchdog(&fakeWatchdog{pending: []watchdog.Alert{
			{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Reason: "looping."},
		}}, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	frames := &failureFrames{}
	frames.attachTo(a)
	runTurnToCompletion(t, a)

	events := sessionEvents(t, a)
	halts := rowsBy(events, attach.GuardrailTripEventAuthor)
	if len(halts) != 1 {
		t.Fatalf("halt rows = %d, want 1", len(halts))
	}
	if n := len(rowsBy(events, attach.GuardrailTurnTripEventAuthor)); n != 0 {
		t.Errorf("a halt also wrote %d per-turn trip rows; one trip would count twice", n)
	}
	if n := len(rowsBy(events, attach.TurnErrorEventAuthor)); n != 0 {
		t.Errorf("a turn that completed wrote %d turn-error rows", n)
	}
	if typed := frames.guardrailTrips(); len(typed) != 1 || typed[0].EventID != halts[0].ID {
		t.Errorf("guardrail-trip frame event_id = %+v, want the halt row %q", typed, halts[0].ID)
	}
}

// TestTurnFailure_TurnScopedWatchdogCutIsDurable covers the watchdog's
// per-turn shape (#1090) at the boundary: halted_turn false, a trip
// row, no halt row.
func TestTurnFailure_TurnScopedWatchdogCutIsDurable(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-1258-wd")
	a, err := New(oneShotLLM{},
		WithEventLog(h),
		WithSession("u", "s-1258-wd"),
		WithWatchdog(&fakeWatchdog{pending: []watchdog.Alert{{
			Signal: "no-op-streak", Severity: watchdog.SeverityCritical,
			Scope: watchdog.ScopeTurn, Reason: "nothing changed for 6 calls.",
		}}}, nil),
		WithWatchdogEnforce(),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	runTurnToCompletion(t, a)

	events := sessionEvents(t, a)
	trips := rowsBy(events, attach.GuardrailTurnTripEventAuthor)
	if len(trips) != 1 {
		t.Fatalf("guardrail-turn-trip rows = %d, want 1", len(trips))
	}
	gt, _ := attach.GuardrailTurnTrip(trips[0])
	if gt.Guardrail != attach.GuardrailWatchdog || gt.HaltedTurn {
		t.Errorf("row = %+v, want watchdog with halted_turn false (fired at the boundary)", gt)
	}
	if n := len(rowsBy(events, attach.GuardrailTripEventAuthor)); n != 0 {
		t.Errorf("a turn-scoped cut wrote %d halt rows", n)
	}
}

// TestTurnFailure_ModelErrorIsDurable: an ordinary failed model call.
// The row carries the classified payload verbatim, and the typed frame
// names it.
func TestTurnFailure_ModelErrorIsDurable(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-1258-err")
	llm := &errThenRecordLLM{err: errors.New("Error 429, RESOURCE_EXHAUSTED: quota exceeded for aiplatform")}
	a, err := New(llm, WithEventLog(h), WithSession("u", "s-1258-err"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	frames := &failureFrames{}
	frames.attachTo(a)
	for range a.Run(context.Background(), "hi") { //nolint:revive // draining the failed turn
	}

	errs := rowsBy(sessionEvents(t, a), attach.TurnErrorEventAuthor)
	if len(errs) != 1 {
		t.Fatalf("turn-error rows = %d, want 1", len(errs))
	}
	te, cutBy, _ := attach.TurnErrorRow(errs[0])
	want := attach.ClassifyTurnError(llm.err)
	want.EventID = errs[0].ID
	if te != want {
		t.Errorf("row = %+v, want the classifier's payload %+v", te, want)
	}
	if cutBy != "" {
		t.Errorf("cut_by = %q on an error no guardrail caused", cutBy)
	}
	if typed := frames.turnErrors(); len(typed) != 1 || typed[0] != want {
		t.Errorf("typed frame = %+v, want %+v", typed, want)
	}
	// Must NOT carry the error in the LLMResponse fields: auto-continue
	// reads ErrorCode before it skips content-less rows.
	if errs[0].ErrorCode != "" || errs[0].ErrorMessage != "" || errs[0].Content != nil {
		t.Errorf("row has model-facing fields: code=%q msg=%q content=%v",
			errs[0].ErrorCode, errs[0].ErrorMessage, errs[0].Content)
	}
}

// TestTurnFailure_RefusedTurnIsDurable: a turn the pre-flight refused.
// The driver logs it as a turn error, so A2 needs a row for it, and it
// never got a prompt id or a frame.
func TestTurnFailure_RefusedTurnIsDurable(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-1258-refused")
	a, err := New(oneShotLLM{}, WithEventLog(h), WithSession("u", "s-1258-refused"),
		WithCostCeiling(CostCeiling{MaxSessionUSD: 1}))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	a.mu.Lock()
	a.costCeilingExceeded, a.costCeilingReason = true, "per-session cost ceiling exceeded"
	a.mu.Unlock()
	frames := &failureFrames{}
	frames.attachTo(a)
	var gotErr error
	for _, err := range a.Run(context.Background(), "hi") {
		gotErr = err
	}
	if !IsCostCeilingExceeded(gotErr) {
		t.Fatalf("turn was not refused: %v", gotErr)
	}
	errs := rowsBy(sessionEvents(t, a), attach.TurnErrorEventAuthor)
	if len(errs) != 1 {
		t.Fatalf("turn-error rows for a refused turn = %d, want 1", len(errs))
	}
	te, _, _ := attach.TurnErrorRow(errs[0])
	if te.Kind != attach.TurnErrorCostCeiling {
		t.Errorf("refusal row kind = %q, want %q", te.Kind, attach.TurnErrorCostCeiling)
	}
	if _, has := errs[0].CustomMetadata["prompt_id"]; has {
		t.Error("a refused turn never got a prompt id, but its row claims one")
	}
	if n := len(frames.turnErrors()); n != 0 {
		t.Errorf("a refusal emitted %d turn-error frames; it never opened a turn", n)
	}
}

// TestTurnFailure_NoEventLogNamesNoRow: without an eventlog nothing is
// written, and a frame must not name a row that does not exist.
func TestTurnFailure_NoEventLogNamesNoRow(t *testing.T) {
	t.Parallel()
	llm := &errThenRecordLLM{err: errors.New("Error 503, UNAVAILABLE")}
	a, err := New(llm, WithSession("u", "s-1258-nolog"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	frames := &failureFrames{}
	frames.attachTo(a)
	for range a.Run(context.Background(), "hi") { //nolint:revive // draining the failed turn
	}
	if typed := frames.turnErrors(); len(typed) != 1 || typed[0].EventID != "" {
		t.Errorf("typed frames = %+v, want one with no event_id", typed)
	}
}

// TestTurnFailure_RowsNeverEnterModelContext: after a failed turn, the
// next turn's request carries nothing from the rows. The error text is
// the probe — the failed call persisted nothing of its own, so the text
// can only reach the request through the row.
func TestTurnFailure_RowsNeverEnterModelContext(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-1258-ctx")
	llm := &errThenRecordLLM{err: errors.New("Error 429, RESOURCE_EXHAUSTED: marker-1258-quota")}
	a, err := New(llm, WithEventLog(h), WithSession("u", "s-1258-ctx"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	for range a.Run(context.Background(), "first") { //nolint:revive // draining the failed turn
	}
	if n := len(rowsBy(sessionEvents(t, a), attach.TurnErrorEventAuthor)); n != 1 {
		t.Fatalf("precondition: %d turn-error rows, want 1", n)
	}
	for _, err := range a.Run(context.Background(), "second") {
		if err != nil {
			t.Fatalf("second turn: %v", err)
		}
	}
	got := flattenText(llm.lastRequest().Contents)
	for _, leak := range []string{"marker-1258-quota", attach.TurnErrorEventAuthor, "rate_limited"} {
		if strings.Contains(got, leak) {
			t.Errorf("next turn's model request contains %q from the failure row:\n%s", leak, got)
		}
	}
	if !strings.Contains(got, "second") {
		t.Fatalf("probe is blind: the request does not even carry the prompt: %q", got)
	}
}

// TestTurnFailure_RowsAreNotConversation runs every conversation reader
// that walks the persisted session — compaction's summarizer history,
// the /context stats, the usage rebuild — over a session holding both
// row kinds, and requires each to see exactly what it saw without them.
func TestTurnFailure_RowsAreNotConversation(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	tr := usage.NewTracker()
	llm := &burnLoopLLM{perCallIn: 1000}
	a := newCutAgent(t, h, "s-1258-conv", llm, tr)
	burnToCut(t, a, tr, llm)

	events := sessionEvents(t, a)
	var rows, rest []*session.Event
	for _, ev := range events {
		switch ev.Author {
		case attach.GuardrailTurnTripEventAuthor, attach.TurnErrorEventAuthor:
			rows = append(rows, ev)
		default:
			rest = append(rest, ev)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("precondition: %d failure rows, want 2", len(rows))
	}

	// Compaction: the summarizer's view of the session.
	hist, err := a.summarizerHistory(context.Background())
	if err != nil {
		t.Fatalf("summarizerHistory: %v", err)
	}
	if s := flattenText(hist); strings.Contains(s, "per-turn cost ceiling") || strings.Contains(s, "turn canceled") {
		t.Errorf("summarizer history carries failure-row text: %q", s)
	}

	// /context: no boundary is invented and no summary chars counted.
	if st := a.ContextStats(); st.CompactionCount != 0 || st.CheckpointCount != 0 || st.TotalSummaryChars != 0 {
		t.Errorf("ContextStats over failure rows = %+v, want zero boundaries", st)
	}

	// Usage: rebuilding a tracker from the rows alone books nothing.
	rebuilt := usage.NewTracker()
	seq := func(yield func(*session.Event, error) bool) {
		for _, ev := range rows {
			if !yield(ev, nil) {
				return
			}
		}
	}
	if err := usage.RebuildTrackerFromEvents(context.Background(), rebuilt, seq, "m",
		func(string) usage.Pricing { return usage.Pricing{InputPerMTok: 10} }); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if tot := rebuilt.Totals(); tot.Turns != 0 || tot.CostUSD != 0 {
		t.Errorf("usage rebuilt from failure rows = %+v, want nothing", tot)
	}

	// Auto-continue: the tail verdict is the same with and without the
	// rows. The cut turn's tail is a tool round-trip, so this is the
	// shape where a row read as content would change the answer.
	if with, without := classifyInterruptedTail(events), classifyInterruptedTail(rest); with.Interrupted != without.Interrupted ||
		with.DeclineReason != without.DeclineReason {
		t.Errorf("tail verdict with rows = %+v, without = %+v", with, without)
	}
}

// TestTurnFailure_RowsChangeNoTailVerdict pins the auto-continue
// interaction on hand-built tails, so it holds for the completed-turn
// shape too and not only for whatever the cut test happens to leave.
func TestTurnFailure_RowsChangeNoTailVerdict(t *testing.T) {
	t.Parallel()
	text := func(author, role, s string) *session.Event {
		ev := session.NewEventWithContext(context.Background(), "inv")
		ev.Author = author
		ev.Content = &genai.Content{Role: role, Parts: []*genai.Part{{Text: s}}}
		return ev
	}
	call := session.NewEventWithContext(context.Background(), "inv")
	call.Author = "agent"
	call.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "read_file"}}}}
	resp := session.NewEventWithContext(context.Background(), "inv")
	resp.Author = "agent"
	resp.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "read_file"}}}}
	rows := []*session.Event{
		attach.NewGuardrailTurnTripEvent(attach.GuardrailCostCeiling, "per-turn cost ceiling exceeded", true),
		attach.NewTurnErrorEvent(attach.TurnError{Kind: attach.TurnErrorRateLimited, Code: "429", Message: "quota", Retryable: true}, "p-1", ""),
	}
	for name, tail := range map[string][]*session.Event{
		"completed": {text("user", genai.RoleUser, "hi"), text("agent", genai.RoleModel, "done")},
		"mid-tool":  {text("user", genai.RoleUser, "hi"), call, resp},
	} {
		with := append(append([]*session.Event{}, tail...), rows...)
		if a, b := classifyInterruptedTail(with), classifyInterruptedTail(tail); a.Interrupted != b.Interrupted || a.DeclineReason != b.DeclineReason {
			t.Errorf("%s: verdict with rows %+v != without %+v", name, a, b)
		}
	}
}

// TestTurnFailure_PerTurnRowNeverRestoresAsHalt: the restart half. A
// fresh agent over a session holding a per-turn trip row restores no
// halt and runs its first turn.
func TestTurnFailure_PerTurnRowNeverRestoresAsHalt(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	tr := usage.NewTracker()
	llm := &burnLoopLLM{perCallIn: 1000}
	first := newCutAgent(t, h, "s-1258-restore", llm, tr)
	burnToCut(t, first, tr, llm)
	if n := len(rowsBy(sessionEvents(t, first), attach.GuardrailTurnTripEventAuthor)); n != 1 {
		t.Fatalf("precondition: %d trip rows", n)
	}

	second, err := New(oneShotLLM{}, WithEventLog(h), WithSession("u", "s-1258-restore"),
		WithUsageTracker(usage.NewTracker()),
		WithCostCeiling(CostCeiling{MaxTurnUSD: 0.05, MaxSessionUSD: 100}))
	if err != nil {
		t.Fatalf("agent.New (restart): %v", err)
	}
	if err := second.RestoreGuardrails(context.Background()); err != nil {
		t.Fatalf("RestoreGuardrails: %v", err)
	}
	if halted, reason := second.GuardrailHalted(); halted {
		t.Fatalf("restart restored a halt from a per-turn row: %q", reason)
	}
	for _, err := range second.Run(context.Background(), "next") {
		if err != nil {
			t.Fatalf("restarted agent refused its first turn: %v", err)
		}
	}
}

// TestTurnFailure_StreakHaltIsOneHaltRowThatSaysItCut: the third
// per-turn trip in a row halts the session (#1049). That trip is the
// halt row and only the halt row, and the row records that it cut the
// turn — a TUI replaying it needs that to absorb the `canceled` that
// follows, as it does live.
func TestTurnFailure_StreakHaltIsOneHaltRowThatSaysItCut(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	tr := usage.NewTracker()
	llm := &burnLoopLLM{perCallIn: 1000}
	a := newCutAgent(t, h, "s-1258-streak", llm, tr)
	for i := 0; i < maxConsecutiveTurnCeilingTrips; i++ {
		burnToCut(t, a, tr, llm)
	}
	if halted, _ := a.GuardrailHalted(); !halted {
		t.Fatalf("precondition: %d per-turn trips in a row did not halt the session", maxConsecutiveTurnCeilingTrips)
	}
	events := sessionEvents(t, a)
	trips := rowsBy(events, attach.GuardrailTurnTripEventAuthor)
	halts := rowsBy(events, attach.GuardrailTripEventAuthor)
	errs := rowsBy(events, attach.TurnErrorEventAuthor)
	if len(trips) != maxConsecutiveTurnCeilingTrips-1 || len(halts) != 1 || len(errs) != maxConsecutiveTurnCeilingTrips {
		t.Fatalf("rows: %d per-turn, %d halt, %d turn-error; want %d, 1, %d",
			len(trips), len(halts), len(errs), maxConsecutiveTurnCeilingTrips-1, maxConsecutiveTurnCeilingTrips)
	}
	gt, ok := attach.GuardrailHaltRow(halts[0])
	if !ok || !gt.HaltedTurn {
		t.Errorf("halt row = %+v (ok=%v), want halted_turn true: the streak's last trip cut its turn", gt, ok)
	}
}

// gatedService holds the first AppendEvent until released, so a test
// can put a second drain in the window where the first is mid-write.
//
// A flag rather than a sync.Once: Once blocks every other caller until
// the first returns, which would serialize the drains in the test
// harness and hide exactly the overlap under test.
type gatedService struct {
	session.Service
	gated   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (g *gatedService) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	if g.gated.CompareAndSwap(false, true) {
		g.entered <- struct{}{}
		<-g.release
	}
	return g.Service.AppendEvent(ctx, sess, ev)
}

// TestDrainOutOfBandEvents_ConcurrentDrainsKeepQueueOrder: a row queued
// second must not be written first. Without the drain mutex the second
// drain takes its own batch and appends it while the first is still
// writing, and a replay then shows a turn's `canceled` before the trip
// that explains it (#1258's rows are read back in order). The overlap
// can also lose the first row outright: its drain read the session
// before the second append, and the eventlog refuses the stale write.
func TestDrainOutOfBandEvents_ConcurrentDrainsKeepQueueOrder(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-1258-order")
	gate := &gatedService{Service: h.Service, entered: make(chan struct{}, 1), release: make(chan struct{})}
	a := &Agent{
		eventLog: &eventlog.Handle{Service: gate, Stream: h.Stream},
		appName:  "core-agent", userID: "u", sessionID: "s-1258-order",
	}
	first := attach.NewGuardrailTurnTripEvent(attach.GuardrailCostCeiling, "per-turn", true)
	second := attach.NewTurnErrorEvent(attach.TurnError{Kind: attach.TurnErrorCanceled, Message: "turn canceled"}, "p", "")

	firstDone := make(chan struct{})
	go func() { a.queueOutOfBandEvent(first); close(firstDone) }()
	<-gate.entered
	secondDone := make(chan struct{})
	go func() { a.queueOutOfBandEvent(second); close(secondDone) }()
	// Give the second drain every chance to overtake the first.
	select {
	case <-secondDone:
	case <-time.After(200 * time.Millisecond):
	}
	close(gate.release)
	<-firstDone
	<-secondDone

	resp, err := h.Service.Get(context.Background(), &session.GetRequest{AppName: "core-agent", UserID: "u", SessionID: "s-1258-order"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var order []string
	for ev := range resp.Session.Events().All() {
		order = append(order, ev.ID)
	}
	if len(order) != 2 || order[0] != first.ID || order[1] != second.ID {
		t.Errorf("rows landed as %v, want [%s %s] — queue order", order, first.ID, second.ID)
	}
}
