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
	"strings"
	"testing"

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/usage"
)

func TestCompact_NoCompactorReturnsSentinel(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "irrelevant"}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Compact(context.Background(), ""); !errors.Is(err, ErrNoCompactor) {
		t.Errorf("Compact without WithCompactor = %v, want ErrNoCompactor", err)
	}
}

func TestCompact_EmptyHistoryIsSkipped(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "should not be called"}
	a, err := New(llm, WithCompactor(NewDefaultCompactor()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// No prior Run() calls — session has no events.
	res, err := a.Compact(context.Background(), "")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !res.Skipped {
		t.Errorf("Compact on empty session should set Skipped=true, got %#v", res)
	}
	if len(llm.reqs) != 0 {
		t.Errorf("model was called for empty-history Compact; want skipped without LLM call")
	}
}

func TestCompact_WritesSummaryEvent(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "# Current state\nProject in flight."}
	a, err := New(llm, WithCompactor(NewDefaultCompactor()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Plant a synthetic user event so the session isn't empty.
	plantEvent(t, a, genai.RoleUser, "let's build a thing")

	res, err := a.Compact(context.Background(), "focus on the auth module")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.SummaryEventID == "" {
		t.Errorf("SummaryEventID empty; want a non-empty ID")
	}
	if !strings.Contains(res.SummaryText, "Project in flight") {
		t.Errorf("SummaryText = %q, want the model's text", res.SummaryText)
	}

	// The summary event should be findable in the session with the
	// correct CustomMetadata markers + focus.
	events := loadAllSessionEvents(t, a)
	idx, ev, tag := findLatestBoundary(events)
	if idx < 0 || ev == nil {
		t.Fatalf("compaction summary event not found in session; events=%d", len(events))
	}
	if tag != CompactionEventTag {
		t.Errorf("tag = %q, want %q (compaction summary)", tag, CompactionEventTag)
	}
	if got := ev.CustomMetadata[CompactionFocusKey]; got != "focus on the auth module" {
		t.Errorf("CompactionFocusKey = %v, want focus on the auth module", got)
	}

	// The summarizer's system instruction should reach the model via
	// the Config field, NOT have tools attached.
	req := llm.lastRequest()
	if req == nil {
		t.Fatal("model wasn't called")
	}
	if req.Config == nil || req.Config.SystemInstruction == nil {
		t.Fatalf("LLMRequest.Config.SystemInstruction nil; want the summarizer prompt")
	}
	sysText := contentText(req.Config.SystemInstruction)
	if !strings.Contains(sysText, "Current state") || !strings.Contains(sysText, "Files & changes") {
		t.Errorf("system instruction missing the five-section template: %q", sysText)
	}
	if !strings.Contains(sysText, "focus on the auth module") {
		t.Errorf("system instruction missing the focus hint: %q", sysText)
	}
	if len(req.Tools) != 0 {
		t.Errorf("Tools = %d, want 0 (compactor is tool-less)", len(req.Tools))
	}
}

func TestSliceFromSummary_NoSummaryIsIdentity(t *testing.T) {
	t.Parallel()
	events := []*session.Event{
		mkEvent(genai.RoleUser, "a"),
		mkEvent(genai.RoleModel, "b"),
		mkEvent(genai.RoleUser, "c"),
	}
	out := sliceFromBoundary(events)
	if len(out) != len(events) {
		t.Errorf("expected pass-through when no summary present; got len=%d", len(out))
	}
}

func TestSliceFromSummary_DropsPreSummaryEvents(t *testing.T) {
	t.Parallel()
	pre1 := mkEvent(genai.RoleUser, "old prompt")
	pre2 := mkEvent(genai.RoleModel, "old response")
	summary := mkSummaryEvent("session compacted: state X, files Y")
	post1 := mkEvent(genai.RoleUser, "fresh prompt after compaction")
	events := []*session.Event{pre1, pre2, summary, post1}

	out := sliceFromBoundary(events)

	if len(out) != 2 {
		t.Fatalf("sliced len = %d, want 2 (summary + post1)", len(out))
	}
	// First entry is the rewritten summary.
	if out[0].Content.Role != genai.RoleUser {
		t.Errorf("summary role = %q, want RoleUser (rewritten for resuming model)", out[0].Content.Role)
	}
	if !strings.Contains(contentText(out[0].Content), "session compacted") {
		t.Errorf("summary text not preserved: %q", contentText(out[0].Content))
	}
	// Framing wraps the summary so the model reads it as prior-
	// conversation context, not as a fresh task spec. Smoke
	// observation: without the prefix, models tend to ignore the
	// summary and start exploring the filesystem.
	if !strings.Contains(contentText(out[0].Content), "Conversation compacted") {
		t.Errorf("summary missing compactionPrefix framing: %q", contentText(out[0].Content))
	}
	if !strings.Contains(contentText(out[0].Content), "[End of summary") {
		t.Errorf("summary missing compactionSuffix framing: %q", contentText(out[0].Content))
	}
	// Second entry is the post-summary event, unchanged.
	if !strings.Contains(contentText(out[1].Content), "fresh prompt") {
		t.Errorf("post-summary event missing: %q", contentText(out[1].Content))
	}
	// Original event must NOT be mutated.
	if summary.Content.Role != genai.RoleModel {
		t.Errorf("original summary event mutated! role now %q", summary.Content.Role)
	}
	if len(summary.Content.Parts) != 1 {
		t.Errorf("original summary parts mutated! len now %d (should still be 1, the raw summary text)", len(summary.Content.Parts))
	}
}

func TestDefaultCompactor_ShouldCompact_UnknownWindowSkips(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	// Unknown model — ContextWindowSize returns 0 → never compact.
	tr.Append("some-future-llm-7b", 999_999, 100, usage.Pricing{})
	a := &Agent{tracker: tr}
	c := NewDefaultCompactor()
	if c.ShouldCompact(context.Background(), a) {
		t.Errorf("ShouldCompact = true on unknown window; want false (skip when size=0)")
	}
}

func TestDefaultCompactor_ShouldCompact_OverThreshold(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	// gemini-3.5-flash → 1M context. 900K input = 90% utilization > 0.85 threshold.
	tr.Append("gemini-3.5-flash", 900_000, 100, usage.Pricing{})
	a := &Agent{tracker: tr}
	c := NewDefaultCompactor()
	if !c.ShouldCompact(context.Background(), a) {
		t.Errorf("ShouldCompact = false at 90%% utilization; want true")
	}
}

func TestDefaultCompactor_ShouldCompact_UnderThreshold(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	// 100K of 1M = 10% utilization → under 0.85 threshold.
	tr.Append("gemini-3.5-flash", 100_000, 100, usage.Pricing{})
	a := &Agent{tracker: tr}
	c := NewDefaultCompactor()
	if c.ShouldCompact(context.Background(), a) {
		t.Errorf("ShouldCompact = true at 10%% utilization; want false")
	}
}

func TestCompactIfNeeded_SkipsWhenUnderThreshold(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "should not be called"}
	tr := usage.NewTracker()
	tr.Append("gemini-3.5-flash", 100_000, 100, usage.Pricing{}) // 10% util
	a, err := New(llm, WithCompactor(NewDefaultCompactor()), WithUsageTracker(tr))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "hi")
	res, err := a.CompactIfNeeded(context.Background(), "")
	if err != nil {
		t.Fatalf("CompactIfNeeded: %v", err)
	}
	if !res.Skipped {
		t.Errorf("under-threshold should be Skipped, got %#v", res)
	}
	if len(llm.reqs) != 0 {
		t.Errorf("LLM called under threshold; want skipped")
	}
}

// TestRun_PostCompactRequestContainsSummary is the end-to-end
// regression test for the user-reported bug "model has no context
// after /compact." Simulates the exact scenario:
//
//  1. Three prior turns (user prompts + model responses) land in
//     the session via the runner.
//  2. /compact runs (Agent.Compact writes a summary event).
//  3. A new user prompt arrives via Agent.Run.
//  4. Assert the LLMRequest the model receives on step 3 includes
//     the summary text.
//
// Without slicing working correctly, step 4 fails — either the
// summary is missing (slicing dropped it) or the full history
// reaches the model (slicing isn't engaging at all). Both are
// real bugs we hit during the smoke sweep.
func TestRun_PostCompactRequestContainsSummary(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "ack"}
	a, err := New(llm, WithCompactor(NewDefaultCompactor()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Three rounds of synthetic prior conversation to mirror the
	// smoke sweep's "what are the main subsystems / pick one / what
	// would a v3 look like" pattern.
	plantEvent(t, a, genai.RoleUser, "what are the main subsystems in this repo?")
	plantEvent(t, a, genai.RoleModel, "agent, models, tools, mcp, skills, permissions, …")
	plantEvent(t, a, genai.RoleUser, "pick one and walk me through its package layout")
	plantEvent(t, a, genai.RoleModel, "Permissions has gate.go, policy.go, scope.go, …")
	plantEvent(t, a, genai.RoleUser, "what would a v3 look like? two paragraphs")
	plantEvent(t, a, genai.RoleModel, "v3 would add hierarchical bounded delegation + semantic gating, …")

	// Step 2 — compact. Use a distinctive summary string so the
	// final assertion can find it.
	llm.response = "[COMPACT-MARKER]\n# Current state\nWe walked through permissions and sketched v3."
	res, err := a.Compact(context.Background(), "")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.Skipped {
		t.Fatalf("Compact reported Skipped; want a real summary write")
	}

	// Step 3 — new turn via the real runner. Drain the iterator so
	// the runner actually fires the LLM call (lazy iter).
	llm.reqs = nil // reset capture buffer
	llm.response = "got it, drawn from the summary"
	for ev, err := range a.Run(context.Background(), "recap what we discussed about this repo") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		_ = ev
	}

	// Step 4 — assert. The LAST request the LLM saw on this turn
	// should include the summary's distinctive marker somewhere in
	// its Contents. Without slicing, the Contents would also include
	// the 6 pre-compact events; with broken slicing, it might omit
	// the summary entirely.
	req := llm.lastRequest()
	if req == nil {
		t.Fatalf("model wasn't called on Run; iterator drain didn't fire ADK's LLM call")
	}
	var allText strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				allText.WriteString(p.Text)
				allText.WriteByte('\n')
			}
		}
	}
	combined := allText.String()
	if !strings.Contains(combined, "[COMPACT-MARKER]") {
		t.Errorf("LLMRequest.Contents missing the summary marker after /compact + Run; the slicing wrapper is either not engaging or dropping the summary event.\n\nCombined Contents text:\n%s", combined)
	}
	if strings.Contains(combined, "what are the main subsystems in this repo?") {
		t.Errorf("LLMRequest.Contents includes pre-compact event text; slicing should have dropped it.\n\nCombined Contents text:\n%s", combined)
	}
}

// TestCompactingService_AppendEvent_UnwrapsSlicedSession is the
// regression test for the smoke-§4 bug ("unexpected session type
// *agent.slicedSession for session ID default"). The runner calls
// our compactingService.Get to load history, gets back a
// *slicedSession wrapper, then later calls AppendEvent with that
// wrapped session as the second argument. The inner service
// type-asserts the session and rejects anything that isn't its
// own concrete type — so AppendEvent has to unwrap first.
//
// Without the fix, this test fails with the same error operators
// see in the chat.
func TestCompactingService_AppendEvent_UnwrapsSlicedSession(t *testing.T) {
	t.Parallel()
	inner := session.InMemoryService()
	ctx := context.Background()

	// Create + seed the session through the inner service, then
	// plant a summary event so Get() returns a *slicedSession.
	const appName, userID, sessionID = "test-app", "test-user", "test-session"
	if _, err := inner.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID, SessionID: sessionID}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	getResp, err := inner.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: sessionID})
	if err != nil {
		t.Fatalf("Get pre-seed: %v", err)
	}
	if err := inner.AppendEvent(ctx, getResp.Session, mkEvent(genai.RoleUser, "old prompt")); err != nil {
		t.Fatalf("AppendEvent pre-seed: %v", err)
	}
	if err := inner.AppendEvent(ctx, getResp.Session, mkSummaryEvent("compacted state")); err != nil {
		t.Fatalf("AppendEvent summary: %v", err)
	}

	// Now exercise the wrapper exactly as the runner would.
	wrapped := &compactingService{inner: inner}
	wResp, err := wrapped.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: sessionID})
	if err != nil {
		t.Fatalf("wrapped.Get: %v", err)
	}
	if _, isSliced := wResp.Session.(*slicedSession); !isSliced {
		t.Fatalf("wrapped.Get returned %T, want *slicedSession (summary was present so it should be wrapped)", wResp.Session)
	}

	// THIS is the call the runner makes that previously failed
	// with "unexpected session type *agent.slicedSession". After
	// the fix, AppendEvent unwraps before delegating.
	ev := mkEvent(genai.RoleUser, "fresh prompt post-compact")
	if err := wrapped.AppendEvent(ctx, wResp.Session, ev); err != nil {
		t.Fatalf("AppendEvent with sliced session must unwrap and succeed; got %v", err)
	}

	// The new event landed in the REAL underlying session, not
	// just in the sliced view (which is read-only from the
	// wrapper's perspective). Verify by reading back through the
	// inner service directly.
	rawResp, err := inner.Get(ctx, &session.GetRequest{AppName: appName, UserID: userID, SessionID: sessionID})
	if err != nil {
		t.Fatalf("inner.Get post-append: %v", err)
	}
	var found bool
	for raw := range rawResp.Session.Events().All() {
		if raw != nil && contentText(raw.Content) == "fresh prompt post-compact" {
			found = true
		}
	}
	if !found {
		t.Errorf("fresh event didn't land in underlying session storage; sliced-session unwrap must pass through to inner")
	}
}

// --- test helpers ---

// plantEvent writes a synthetic event to the agent's session so
// tests can simulate prior turns without invoking the full runner.
func plantEvent(t *testing.T, a *Agent, role, text string) {
	t.Helper()
	resp, err := a.sessionService.Get(context.Background(), &session.GetRequest{
		AppName: a.appName, UserID: a.userID, SessionID: a.sessionID,
	})
	if err != nil {
		// Session doesn't exist yet — create it.
		_, err := a.sessionService.Create(context.Background(), &session.CreateRequest{
			AppName: a.appName, UserID: a.userID, SessionID: a.sessionID,
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		resp, err = a.sessionService.Get(context.Background(), &session.GetRequest{
			AppName: a.appName, UserID: a.userID, SessionID: a.sessionID,
		})
		if err != nil {
			t.Fatalf("get session after create: %v", err)
		}
	}
	ev := mkEvent(role, text)
	if err := a.sessionService.AppendEvent(context.Background(), resp.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

func loadAllSessionEvents(t *testing.T, a *Agent) []*session.Event {
	t.Helper()
	resp, err := a.sessionService.Get(context.Background(), &session.GetRequest{
		AppName: a.appName, UserID: a.userID, SessionID: a.sessionID,
	})
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if resp == nil || resp.Session == nil {
		return nil
	}
	var out []*session.Event
	for ev := range resp.Session.Events().All() {
		out = append(out, ev)
	}
	return out
}

func mkEvent(role, text string) *session.Event {
	return &session.Event{
		LLMResponse: adkmodel.LLMResponse{
			Content: &genai.Content{
				Role:  role,
				Parts: []*genai.Part{{Text: text}},
			},
		},
	}
}

func mkSummaryEvent(text string) *session.Event {
	return &session.Event{
		ID: "synthetic-summary",
		LLMResponse: adkmodel.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: text}},
			},
			CustomMetadata: map[string]any{
				CompactionMetadataKey: CompactionEventTag,
			},
		},
	}
}

func contentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p != nil && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func TestCompact_RollsCostUpToParentTracker(t *testing.T) {
	t.Parallel()
	// Issue #61: the summarizer's own LLM call must reach the
	// parent's usage.Tracker so /stats + /context don't under-
	// report cost in proportion to how often compaction or
	// checkpoints fire. Single-turn call (no tool round-trips,
	// no multi-turn), so we expect exactly ONE Append per Compact
	// invocation.
	llm := &captureLLM{
		response:     "compaction summary text",
		inputTokens:  int32(2_000),
		outputTokens: int32(300),
	}
	tracker := usage.NewTracker()
	a, err := New(llm, WithCompactor(NewDefaultCompactor()), WithUsageTracker(tracker))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "some prior context to summarize")

	preTurns := tracker.Totals().Turns
	if _, err := a.Compact(context.Background(), ""); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	post := tracker.Totals()
	if post.Turns != preTurns+1 {
		t.Errorf("tracker.Turns = %d, want %d (one Append per Compact)", post.Turns, preTurns+1)
	}
	if post.InputTokens < 2_000 || post.OutputTokens < 300 {
		t.Errorf("tracker totals didn't pick up summarizer usage: %+v (want >= 2000 in / 300 out)", post)
	}
}

func TestCheckpoint_RollsCostUpToParentTracker(t *testing.T) {
	t.Parallel()
	// Same as TestCompact_RollsCostUpToParentTracker but via
	// /done's Checkpoint path. Both go through runSummarizer, so
	// the fix benefits both — this test pins the checkpoint side
	// so a future refactor that breaks one but not the other gets
	// caught.
	llm := &captureLLM{
		response:     "checkpoint completion record",
		inputTokens:  int32(1_500),
		outputTokens: int32(250),
	}
	tracker := usage.NewTracker()
	a, err := New(llm, WithCheckpointer(NewDefaultCheckpointer()), WithUsageTracker(tracker))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "task setup turn")

	preTurns := tracker.Totals().Turns
	if _, err := a.Checkpoint(context.Background(), "finished task"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	post := tracker.Totals()
	if post.Turns != preTurns+1 {
		t.Errorf("tracker.Turns = %d, want %d (one Append per Checkpoint)", post.Turns, preTurns+1)
	}
	if post.InputTokens < 1_500 || post.OutputTokens < 250 {
		t.Errorf("tracker totals didn't pick up summarizer usage: %+v (want >= 1500 in / 250 out)", post)
	}
}
