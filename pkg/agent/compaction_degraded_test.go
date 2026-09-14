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

// #974: compaction used to lose both of its dependencies silently — the
// context-window number and the model it summarizes with. These pin the
// two fallbacks and, as much as the fallbacks themselves, the fact that
// each one says out loud that it happened.

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// ---------------------------------------------------------------
// Defect 1 — the window is unknown
// ---------------------------------------------------------------

// The assumption is worthless if nobody is told about it: an operator
// whose session is compacting at a guessed threshold needs to know so
// they can pin a catalogued model instead. Fails on pre-#974 code, which
// wrote nothing anywhere.
func TestUnknownContextWindow_AnnouncesItselfOnTheEventLog(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-974-window")

	tr := usage.NewTracker()
	tr.Append("some-future-llm-7b", 999_999, 100, usage.Pricing{})
	a, err := New(&captureLLM{response: "unused"},
		WithEventLog(h),
		WithSession("u", "s-974-window"),
		WithUsageTracker(tr),
		WithCompactor(NewDefaultCompactor()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if !a.compactor.ShouldCompact(context.Background(), a) {
		t.Fatalf("ShouldCompact = false on an unknown model far past the assumed window")
	}

	op, detail := findContextReductionDegradedRow(t, a)
	if op != attach.ContextReductionWindowUnknown {
		t.Errorf("degraded row operation = %q, want %q", op, attach.ContextReductionWindowUnknown)
	}
	// The two facts an operator needs to act: which model, and what we
	// guessed. "Compaction is degraded" on its own is not actionable.
	if !strings.Contains(detail, "some-future-llm-7b") {
		t.Errorf("detail = %q, want the model id that is missing from the table", detail)
	}
	if !strings.Contains(detail, "128000") {
		t.Errorf("detail = %q, want the assumed window size", detail)
	}
}

// A state, announced once. The post-turn hook calls ShouldCompact on
// every single turn, so a row per call would put thousands of identical
// rows in an overnight run's event log and bury the ones that matter.
func TestUnknownContextWindow_AnnouncedOncePerSession(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-974-once")

	tr := usage.NewTracker()
	tr.Append("some-future-llm-7b", 999_999, 100, usage.Pricing{})
	a, err := New(&captureLLM{response: "unused"},
		WithEventLog(h),
		WithSession("u", "s-974-once"),
		WithUsageTracker(tr),
		WithCompactor(NewDefaultCompactor()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for range 5 {
		a.compactor.ShouldCompact(context.Background(), a)
	}
	if got := countContextReductionDegradedRows(t, a); got != 1 {
		t.Errorf("degraded rows after 5 threshold checks = %d, want 1", got)
	}
}

// A model the table DOES know must stay on its real window. The
// assumption is a fallback, not a replacement — if it fired for
// catalogued models it would drag every 1M-window session down to
// compacting at 109K.
func TestKnownContextWindow_IsNotDegraded(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	tr.Append("gemini-3.5-flash", 200_000, 100, usage.Pricing{})
	a := &Agent{tracker: tr}

	want := usage.ContextWindowSizeFor("gemini-3.5-flash")
	size, known := a.compactionWindowSize()
	if !known || size != want {
		t.Errorf("compactionWindowSize() = (%d, %v), want (%d, true)", size, known, want)
	}
	if a.warnedUnknownWindow {
		t.Errorf("a catalogued model announced a degraded context window")
	}
	// 200K of ~1M is 20%, under the mid-tier 0.65 trigger. Against the
	// assumed 128K window it would be 156% and would fire — which is
	// the regression this guards.
	if NewDefaultCompactor().ShouldCompact(context.Background(), a) {
		t.Errorf("ShouldCompact = true at 20%% of a known 1M window")
	}
}

// ---------------------------------------------------------------
// Defect 2 — the summarizer is the thing that is broken
// ---------------------------------------------------------------

// The headline: a summarizer that keeps failing must not leave history
// growing behind a 32-turn backoff. Fails on pre-#974 code, where the
// only outcome of repeated failure was a longer wait.
func TestPersistentSummarizerFailure_FallsBackToMechanicalTruncation(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "unused", err: errors.New("Error 429, Status: RESOURCE_EXHAUSTED")}
	a, err := New(llm, WithCompactor(NewDefaultCompactor()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "investigate the OOMKill in prod")
	plantEvent(t, a, genai.RoleModel, "checking the pod now")

	// Two consecutive failures, which is MechanicalCompactionAfterFailures.
	// The backoff parks the second attempt, so drive enough turns to get
	// past it.
	for range 4 {
		a.mu.Lock()
		a.compactionPending = true
		a.mu.Unlock()
		a.runPendingCompaction(context.Background())
	}

	boundary := latestBoundaryEvent(t, a)
	if boundary == nil {
		t.Fatal("no compaction boundary was written; history is still unbounded after a dead summarizer")
	}
	if boundary.CustomMetadata[MechanicalCompactionKey] != true {
		t.Errorf("boundary metadata = %v, want it marked mechanical", boundary.CustomMetadata)
	}
	text := boundaryText(boundary)
	if !strings.Contains(text, "MECHANICALLY") {
		t.Errorf("boundary text does not tell the model it is reading a truncation:\n%s", text)
	}
	if !strings.Contains(text, "investigate the OOMKill in prod") {
		t.Errorf("boundary text dropped the recent conversation it was supposed to carry:\n%s", text)
	}
}

// One failure is not enough. A transient that the next attempt clears
// should not cost a whole compaction window its handover summary —
// mechanical truncation is strictly worse than summarization and is only
// the right answer once summarization has stopped being available.
func TestSingleSummarizerFailure_DoesNotTruncateMechanically(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "unused", err: errors.New("transient")}
	a, err := New(llm, WithCompactor(NewDefaultCompactor()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "one turn of history")

	a.mu.Lock()
	a.compactionPending = true
	a.mu.Unlock()
	a.runPendingCompaction(context.Background())

	if ev := latestBoundaryEvent(t, a); ev != nil {
		t.Errorf("a single failure truncated mechanically; want the backoff to get a second chance first")
	}
}

// Clearing the cooldown is load-bearing and easy to lose. The whole
// point of bounding the context was to stop waiting; leaving a 4-turn
// cooldown armed would skip the NEXT compaction too, mechanical or not.
func TestMechanicalFallback_ClearsTheCooldownButNotTheFailureCount(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "unused", err: errors.New("summarizer down")}
	a, err := New(llm, WithCompactor(NewDefaultCompactor()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "history")

	for range 4 {
		a.mu.Lock()
		a.compactionPending = true
		a.mu.Unlock()
		a.runPendingCompaction(context.Background())
	}

	a.mu.Lock()
	cooldown, failures := a.compactionCooldown, a.compactionFailures
	a.mu.Unlock()
	if cooldown != 0 {
		t.Errorf("cooldown after a mechanical fallback = %d, want 0", cooldown)
	}
	// Kept, so the next over-threshold event escalates after one
	// summarizer attempt rather than serving another backoff. That one
	// attempt is the recovery probe.
	if failures < MechanicalCompactionAfterFailures {
		t.Errorf("consecutive-failure count = %d, want it kept at >= %d", failures, MechanicalCompactionAfterFailures)
	}
}

// Degrading in silence is the #974 complaint restated, so the mechanical
// path has to reach an attached operator the same way the failure does.
func TestMechanicalFallback_AnnouncesItselfOnTheEventLog(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-974-mechanical")

	llm := &captureLLM{response: "unused", err: errors.New("Error 429, Status: RESOURCE_EXHAUSTED")}
	a, err := New(llm,
		WithEventLog(h),
		WithSession("u", "s-974-mechanical"),
		WithCompactor(NewDefaultCompactor()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "history over the threshold")

	for range 4 {
		a.mu.Lock()
		a.compactionPending = true
		a.mu.Unlock()
		a.runPendingCompaction(context.Background())
	}

	op, detail := findContextReductionDegradedRow(t, a)
	if op != attach.ContextReductionMechanical {
		t.Errorf("degraded row operation = %q, want %q", op, attach.ContextReductionMechanical)
	}
	if !strings.Contains(detail, "RESOURCE_EXHAUSTED") {
		t.Errorf("detail = %q, want the summarizer failure that caused it", detail)
	}
}

// ---------------------------------------------------------------
// The digest itself
// ---------------------------------------------------------------

// What survives is chosen by what a resuming turn cannot rebuild. A
// model can re-read a file; it cannot know that it already did. So the
// call ledger stays and the results go — which is also where the tokens
// are, and is the only reason the artifact is bounded at all.
func TestMechanicalSummary_KeepsTheCallLedgerAndDropsTheResults(t *testing.T) {
	t.Parallel()
	history := []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "why is the pod restarting"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
			Name: "kubectl_get", Args: map[string]any{"resource": "pod/api-7d9", "namespace": "prod"},
		}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			Name: "kubectl_get", Response: map[string]any{"output": "ENORMOUS-YAML-BLOB"},
		}}}},
	}
	got := mechanicalSummary(history)

	if !strings.Contains(got, "kubectl_get(namespace=prod, resource=pod/api-7d9)") {
		t.Errorf("digest lost the call ledger:\n%s", got)
	}
	if strings.Contains(got, "ENORMOUS-YAML-BLOB") {
		t.Errorf("digest carried a tool RESULT forward; that is the payload it exists to drop:\n%s", got)
	}
	if !strings.Contains(got, "why is the pod restarting") {
		t.Errorf("digest lost the conversation:\n%s", got)
	}
}

// Deterministic by construction, which is what makes the fallback
// testable — and a fallback nobody can test is one nobody finds out is
// broken until the night it is needed. Go randomizes map iteration, so
// unsorted args would make this flaky rather than wrong, which is worse.
func TestMechanicalSummary_IsDeterministic(t *testing.T) {
	t.Parallel()
	history := []*genai.Content{
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
			Name: "read_file",
			Args: map[string]any{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6, "g": 7, "h": 8},
		}}}},
	}
	first := mechanicalSummary(history)
	for range 20 {
		if got := mechanicalSummary(history); got != first {
			t.Fatalf("mechanicalSummary is not deterministic:\n%q\nvs\n%q", first, got)
		}
	}
}

// Bounded output is the entire product here. A truncation that grows
// with the history it truncates has done nothing.
func TestMechanicalSummary_IsBounded(t *testing.T) {
	t.Parallel()
	var history []*genai.Content
	for i := range 500 {
		history = append(history,
			&genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{
				{Text: strings.Repeat("x", 2000)},
			}},
			&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{Name: "read_file", Args: map[string]any{
					"path": strings.Repeat("p", 500), "i": i,
				}}},
			}},
		)
	}
	got := mechanicalSummary(history)
	// Generous ceiling: the header, the capped ledger, and the text
	// budget. The input here is over 2MB.
	const ceiling = 32_000
	if len(got) > ceiling {
		t.Errorf("digest is %d chars from a 2MB window; want under %d", len(got), ceiling)
	}
	if !strings.Contains(got, "earlier calls omitted") {
		t.Errorf("digest capped the ledger without saying so:\n%s", got[:min(len(got), 2000)])
	}
}

// A single message longer than the whole budget must still come through
// clipped rather than dropped. A truncation that returns no content is
// worse than one slightly over its budget.
func TestMechanicalSummary_KeepsAnOversizedFinalMessage(t *testing.T) {
	t.Parallel()
	history := []*genai.Content{
		{Role: genai.RoleModel, Parts: []*genai.Part{{Text: strings.Repeat("z", mechanicalDigestBudget*3)}}},
	}
	got := mechanicalSummary(history)
	if !strings.Contains(got, "zzz") {
		t.Errorf("an oversized final message was dropped entirely:\n%s", got)
	}
	if len(got) > mechanicalDigestBudget*2 {
		t.Errorf("oversized final message was not clipped: digest is %d chars", len(got))
	}
}

// An empty window is not a failure. It means the escalation had nothing
// to do, and reporting it would put noise on the one channel that exists
// to carry signal.
func TestMechanicalCompact_EmptyWindowIsNotReportedAsAFailure(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-974-empty")

	a, err := New(&captureLLM{response: "unused"},
		WithEventLog(h),
		WithSession("u", "s-974-empty"),
		WithCompactor(NewDefaultCompactor()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = a.mechanicalCompact(context.Background())
	if !errors.Is(err, errNothingToTruncate) {
		t.Fatalf("mechanicalCompact on an empty session = %v, want errNothingToTruncate", err)
	}
	a.fallBackToMechanicalCompaction(context.Background(), errors.New("summarizer down"))
	if got := countContextReductionDegradedRows(t, a); got != 0 {
		t.Errorf("degraded rows for an empty window = %d, want 0", got)
	}
}

// ---------------------------------------------------------------
// helpers
// ---------------------------------------------------------------

func findContextReductionDegradedRow(t *testing.T, a *Agent) (operation, detail string) {
	t.Helper()
	for _, ev := range allEventLogEvents(t, a) {
		if op, d, ok := attach.ContextReductionDegraded(ev); ok {
			return op, d
		}
	}
	t.Fatal("no context-reduction degraded row in the event log")
	return "", ""
}

func countContextReductionDegradedRows(t *testing.T, a *Agent) int {
	t.Helper()
	n := 0
	for _, ev := range allEventLogEvents(t, a) {
		if _, _, ok := attach.ContextReductionDegraded(ev); ok {
			n++
		}
	}
	return n
}

// latestBoundaryEvent reads the newest compaction/checkpoint boundary
// out of the agent's own session, or nil when none was written.
func latestBoundaryEvent(t *testing.T, a *Agent) *session.Event {
	t.Helper()
	resp, err := a.sessionService.Get(context.Background(), &session.GetRequest{
		AppName:   a.appName,
		UserID:    a.userID,
		SessionID: a.sessionID,
	})
	if err != nil {
		t.Fatalf("session Get: %v", err)
	}
	var all []*session.Event
	for ev := range resp.Session.Events().All() {
		all = append(all, ev)
	}
	_, ev, _ := findLatestBoundary(all)
	return ev
}

func boundaryText(ev *session.Event) string {
	if ev == nil || ev.Content == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range ev.Content.Parts {
		if p != nil {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
