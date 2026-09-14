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

// #975: every context check sat on a turn boundary, and a tool result
// large enough to blow the window arrives in the middle of one. These
// pin both arms — noticing earlier, and cutting before the wall — and
// the fact that a cut says so on the event log.

package agent

import (
	"strings"
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// budgetModel is catalogued, so these tests measure against a real
// window rather than #974's assumed one. Its size is read back rather
// than hard-coded: a number asserted on both sides of a test is untested,
// and the generated window table moves.
const budgetModel = "claude-opus-4-1"

// toolResultEvent builds the event shape the in-turn tap sees when a
// tool returns: one FunctionResponse part carrying n bytes of payload.
func toolResultEvent(name string, n int) *session.Event {
	ev := session.NewEvent("tool")
	ev.Content = &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				Name:     name,
				Response: map[string]any{"output": strings.Repeat("x", n)},
			},
		}},
	}
	return ev
}

// budgetAgent wires an agent whose tracker already reports usedTokens of
// measured context against budgetModel.
func budgetAgent(t *testing.T, sid string, usedTokens int, c Compactor) *Agent {
	t.Helper()
	h, cleanup := openTestEventLog(t)
	t.Cleanup(cleanup)
	createTestSession(t, h, "core-agent", "u", sid)

	tr := usage.NewTracker()
	tr.Append(budgetModel, usedTokens, 10, usage.Pricing{})
	a, err := New(&captureLLM{response: "unused"},
		WithEventLog(h),
		WithSession("u", sid),
		WithUsageTracker(tr),
		WithCompactor(c),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func budgetWindow(t *testing.T) int {
	t.Helper()
	n := usage.ContextWindowSizeFor(budgetModel)
	if n <= 0 {
		t.Fatalf("%s is not in the context-window table; pick a catalogued model for this test", budgetModel)
	}
	return n
}

// bytesForTokens is how many bytes of tool result it takes to move the
// estimate by n tokens.
func bytesForTokens(n int) int { return n * usage.BytesPerTokenEstimate }

// ---------------------------------------------------------------
// Accounting
// ---------------------------------------------------------------

func TestToolResultGrowth_IsCountedAgainstTheWindow(t *testing.T) {
	t.Parallel()
	a := budgetAgent(t, "s-975-count", 1000, NewDefaultCompactor())

	a.observeContextGrowth(toolResultEvent("read_file", 9000))

	if got := a.tracker.PendingContextBytes(); got < 9000 {
		t.Errorf("PendingContextBytes after a 9000-byte tool result = %d, want at least 9000", got)
	}
	used, estimated := a.ContextWindowUsedEstimated()
	if !estimated {
		t.Errorf("ContextWindowUsedEstimated reports a measurement after an unmeasured tool result")
	}
	if used <= 1000 {
		t.Errorf("used = %d, want more than the measured 1000 — the tool result is in the next request", used)
	}
}

// Model text is already counted by the usage record for the call that
// produced it. Counting it here too would inflate every turn's estimate
// by its own output and fire compaction early on sessions with no tool
// calls at all.
func TestContextGrowth_IgnoresModelText(t *testing.T) {
	t.Parallel()
	a := budgetAgent(t, "s-975-text", 1000, NewDefaultCompactor())

	ev := session.NewEvent("model")
	ev.Content = genai.NewContentFromText(strings.Repeat("y", 20_000), genai.RoleModel)
	a.observeContextGrowth(ev)

	if got := a.tracker.PendingContextBytes(); got != 0 {
		t.Errorf("PendingContextBytes after 20000 bytes of model text = %d, want 0", got)
	}
}

func TestContextGrowth_SurvivesAnEmptyOrPartlessEvent(t *testing.T) {
	t.Parallel()
	a := budgetAgent(t, "s-975-empty", 1000, NewDefaultCompactor())
	a.observeContextGrowth(nil)
	a.observeContextGrowth(session.NewEvent("bare"))
	if got := a.tracker.PendingContextBytes(); got != 0 {
		t.Errorf("PendingContextBytes = %d, want 0", got)
	}
}

// ---------------------------------------------------------------
// Arm 1 — notice before the turn boundary
// ---------------------------------------------------------------

// The turn that overflows is still running: it may have a dozen more
// tool calls to make before the post-turn hook gets a chance to decide
// anything. Fails on pre-#975 code, where ContextWindowUsed could not
// move until the next model call landed.
func TestMidTurnGrowth_MarksCompactionPendingBeforeTheTurnBoundary(t *testing.T) {
	t.Parallel()
	window := budgetWindow(t)
	// Just under the frontier 0.85 trigger, and well under the 0.95 cut.
	measured := int(0.80 * float64(window))
	a := budgetAgent(t, "s-975-early", measured, NewDefaultCompactor())

	if a.compactionPendingForTest() {
		t.Fatalf("compaction pending before anything happened")
	}
	// Enough to cross 0.85 but not 0.95.
	a.observeContextGrowth(toolResultEvent("kubectl", bytesForTokens(int(0.07*float64(window)))))

	if !a.compactionPendingForTest() {
		t.Errorf("compaction not pending after a tool result took the estimate past the threshold")
	}
}

// The complement: a session sitting over the threshold with nothing
// unmeasured is the post-turn hook's business, and re-deciding it on
// every streamed text delta would be work for nothing.
func TestContextBudget_DoesNothingWithoutAnUnmeasuredTail(t *testing.T) {
	t.Parallel()
	window := budgetWindow(t)
	a := budgetAgent(t, "s-975-nomotion", int(0.90*float64(window)), NewDefaultCompactor())

	a.enforceContextBudgetInTurn()

	if a.compactionPendingForTest() {
		t.Errorf("the in-turn arm acted on a session with nothing appended since the last measurement")
	}
	if n := countContextReductionDegradedRows(t, a); n != 0 {
		t.Errorf("degraded rows = %d, want 0", n)
	}
}

// ---------------------------------------------------------------
// Arm 2 — cut before the wall
// ---------------------------------------------------------------

func TestOversizedToolResult_CutsTheTurnAndSaysSo(t *testing.T) {
	t.Parallel()
	window := budgetWindow(t)
	measured := int(0.90 * float64(window))
	a := budgetAgent(t, "s-975-cut", measured, NewDefaultCompactor())

	// Past ContextBudgetHardCeiling in one result.
	a.observeContextGrowth(toolResultEvent("kubectl", bytesForTokens(int(0.10*float64(window)))))

	op, detail := findContextReductionDegradedRow(t, a)
	if op != attach.ContextReductionTurnCut {
		t.Fatalf("degraded row operation = %q, want %q", op, attach.ContextReductionTurnCut)
	}
	// An operator reading only this line has to be able to tell how
	// close to the wall it got and that recovery is automatic.
	for _, want := range []string{"compaction", "unmeasured"} {
		if !strings.Contains(detail, want) {
			t.Errorf("cut detail %q does not mention %q", detail, want)
		}
	}
	if !a.compactionPendingForTest() {
		t.Errorf("compaction not pending after a cut; the next turn would rebuild the same oversized request")
	}
}

// A cut is an attempt, not a state, so it announces every time it
// happens — but once per turn, not once per tool result in a turn that
// is already being torn down.
func TestContextBudgetCut_AnnouncedOncePerTurn(t *testing.T) {
	t.Parallel()
	window := budgetWindow(t)
	a := budgetAgent(t, "s-975-once", int(0.90*float64(window)), NewDefaultCompactor())

	for i := 0; i < 4; i++ {
		a.observeContextGrowth(toolResultEvent("kubectl", bytesForTokens(int(0.10*float64(window)))))
	}

	if n := countContextReductionDegradedRows(t, a); n != 1 {
		t.Errorf("degraded rows after four oversized results in one turn = %d, want 1", n)
	}
}

// ...and the latch has to clear, or a session that cut one turn could
// never protect itself again. This is the half a once-per-process latch
// would have got wrong.
func TestContextBudgetCut_LatchClearsBetweenTurns(t *testing.T) {
	t.Parallel()
	window := budgetWindow(t)
	a := budgetAgent(t, "s-975-relatch", int(0.90*float64(window)), NewDefaultCompactor())

	a.observeContextGrowth(toolResultEvent("kubectl", bytesForTokens(int(0.10*float64(window)))))
	a.clearContextBudgetCut()
	a.observeContextGrowth(toolResultEvent("kubectl", bytesForTokens(int(0.10*float64(window)))))

	if n := countContextReductionDegradedRows(t, a); n != 2 {
		t.Errorf("degraded rows across two turns that each cut = %d, want 2", n)
	}
}

// The cut sits above every shipped compaction threshold on purpose: the
// ordinary path must always get to act first, so a result that only
// crosses the trigger marks compaction and leaves the turn alone.
func TestModerateGrowth_TriggersCompactionWithoutCuttingTheTurn(t *testing.T) {
	t.Parallel()
	window := budgetWindow(t)
	a := budgetAgent(t, "s-975-moderate", int(0.80*float64(window)), NewDefaultCompactor())

	a.observeContextGrowth(toolResultEvent("kubectl", bytesForTokens(int(0.07*float64(window)))))

	if !a.compactionPendingForTest() {
		t.Errorf("compaction not pending after crossing the trigger")
	}
	if n := countContextReductionDegradedRows(t, a); n != 0 {
		t.Errorf("degraded rows = %d, want 0 — 0.87 of the window is not a reason to cut a turn", n)
	}
}

// #974 and #975 have to compose: an uncatalogued model gets the assumed
// window, and the in-turn check measures the estimate against that
// rather than declining to act.
func TestContextBudget_UsesTheAssumedWindowForAnUncataloguedModel(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-975-assumed")

	tr := usage.NewTracker()
	tr.Append("some-future-llm-7b", int(0.90*float64(AssumedContextWindowSize)), 10, usage.Pricing{})
	a, err := New(&captureLLM{response: "unused"},
		WithEventLog(h),
		WithSession("u", "s-975-assumed"),
		WithUsageTracker(tr),
		WithCompactor(NewDefaultCompactor()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	a.observeContextGrowth(toolResultEvent("kubectl", bytesForTokens(int(0.10*float64(AssumedContextWindowSize)))))

	var sawCut bool
	for _, ev := range allEventLogEvents(t, a) {
		if op, _, ok := attach.ContextReductionDegraded(ev); ok && op == attach.ContextReductionTurnCut {
			sawCut = true
		}
	}
	if !sawCut {
		t.Errorf("no turn-cut row; the in-turn budget declined to act on an assumed window")
	}
}

// A session with no usage tracker at all must not panic or act — the
// same posture every other in-turn arm takes.
func TestContextBudget_NoTrackerIsANoOp(t *testing.T) {
	t.Parallel()
	a, err := New(&captureLLM{response: "unused"}, WithCompactor(NewDefaultCompactor()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.observeContextGrowth(toolResultEvent("kubectl", 500_000))
	a.enforceContextBudgetInTurn()
}

// compactionPendingForTest reads the flag under the agent's own lock —
// the in-turn arm sets it from the tap goroutine, so an unsynchronised
// read here would be a race the -race build would find.
func (a *Agent) compactionPendingForTest() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.compactionPending
}
