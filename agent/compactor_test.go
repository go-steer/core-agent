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
	idx, ev := findLatestCompactionSummary(events)
	if idx < 0 || ev == nil {
		t.Fatalf("compaction summary event not found in session; events=%d", len(events))
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
	out := sliceFromSummary(events)
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

	out := sliceFromSummary(events)

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
	// Second entry is the post-summary event, unchanged.
	if !strings.Contains(contentText(out[1].Content), "fresh prompt") {
		t.Errorf("post-summary event missing: %q", contentText(out[1].Content))
	}
	// Original event must NOT be mutated.
	if summary.Content.Role != genai.RoleModel {
		t.Errorf("original summary event mutated! role now %q", summary.Content.Role)
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
