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
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// modelTextEvent is a completed final model turn.
func modelTextEvent(text string) *session.Event {
	ev := session.NewEvent("inv-model")
	ev.Author = defaultAgentName
	ev.LLMResponse = adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}},
	}
	return ev
}

// annotationEvent mimics autonomous notes: content-bearing but
// role-less, which ADK's content processor (and our classifier) skip.
func annotationEvent() *session.Event {
	ev := session.NewEvent("inv-note")
	ev.Author = "core-agent/autonomous"
	ev.LLMResponse = adkmodel.LLMResponse{
		Content:        &genai.Content{Parts: []*genai.Part{{Text: "paused"}}},
		CustomMetadata: map[string]any{"kind": "note"},
	}
	return ev
}

func summaryEvent(text string) *session.Event {
	ev := modelTextEvent(text)
	ev.CustomMetadata = map[string]any{CompactionMetadataKey: CompactionEventTag}
	return ev
}

// truncatedTextEvent is a model turn that emitted text but hit the output
// cap: FinishReason MAX_TOKENS, stamped into CustomMetadata by the
// eventlog overlay (#582). It must read as interrupted (resume-able).
func truncatedTextEvent(text string) *session.Event {
	ev := modelTextEvent(text)
	ev.CustomMetadata = map[string]any{
		eventlog.FinishReasonMetadataKey: string(genai.FinishReasonMaxTokens),
	}
	return ev
}

// stoppedTextEvent is a normally-completed model turn whose STOP reason
// was stamped: it must still classify as completed (robustness against a
// future overlay that records STOP too).
func stoppedTextEvent(text string) *session.Event {
	ev := modelTextEvent(text)
	ev.CustomMetadata = map[string]any{
		eventlog.FinishReasonMetadataKey: string(genai.FinishReasonStop),
	}
	return ev
}

func branched(ev *session.Event) *session.Event {
	ev.Branch = "subagent_1"
	return ev
}

func interruptAuditRow() *session.Event {
	ev := session.NewEvent("inv-audit")
	ev.Author = interruptAuditAuthor // contentless audit row, mirrors pkg/attach
	return ev
}

func errorFinalEvent(code string) *session.Event {
	ev := session.NewEvent("inv-err")
	ev.Author = defaultAgentName
	ev.LLMResponse = adkmodel.LLMResponse{ErrorCode: code}
	return ev
}

func emptyModelFinal() *session.Event {
	ev := session.NewEvent("inv-empty")
	ev.Author = defaultAgentName
	ev.LLMResponse = adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{}},
	}
	return ev
}

func skipSummarizationResponse(callID string) *session.Event {
	ev := responseEvent(defaultAgentName, &genai.FunctionResponse{ID: callID, Name: "bash"})
	ev.Actions.SkipSummarization = true
	return ev
}

func TestClassifyInterruptedTail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		events      []*session.Event
		interrupted bool
	}{
		{
			name:        "unanswered user message",
			events:      []*session.Event{modelTextEvent("earlier answer"), userTextEvent("hello?")},
			interrupted: true,
		},
		{
			name: "dangling tool call (pre-repair crash tail)",
			events: []*session.Event{
				userTextEvent("do it"),
				callEvent(defaultAgentName, "inv-1", nil, &genai.FunctionCall{ID: "c1", Name: "bash"}),
			},
			interrupted: true,
		},
		{
			name: "tool response with no model turn after (post-repair tail)",
			events: []*session.Event{
				userTextEvent("do it"),
				callEvent(defaultAgentName, "inv-1", nil, &genai.FunctionCall{ID: "c1", Name: "bash"}),
				responseEvent(defaultAgentName, &genai.FunctionResponse{ID: "c1", Name: "bash"}),
			},
			interrupted: true,
		},
		{
			name:        "completed model turn",
			events:      []*session.Event{userTextEvent("q"), modelTextEvent("done, here's the answer")},
			interrupted: false,
		},
		{
			// #582: a MAX_TOKENS truncation that still emitted text is NOT
			// a completed turn — the eventlog stamps the reason and we
			// resume it. Fails on pre-fix code, which ignores the stamp.
			name: "max-tokens-truncated text tail is continued",
			events: []*session.Event{
				userTextEvent("write me something very long"),
				truncatedTextEvent("here is the start of a very long ans"),
			},
			interrupted: true,
		},
		{
			// A stamped STOP is a genuine completion — stays terminal.
			name: "stamped STOP text tail is a completed turn",
			events: []*session.Event{
				userTextEvent("q"),
				stoppedTextEvent("done"),
			},
			interrupted: false,
		},
		{
			name: "long-running-only call tail is a completed turn",
			events: []*session.Event{
				userTextEvent("kick it off"),
				callEvent(defaultAgentName, "inv-lr", []string{"lr1"}, &genai.FunctionCall{ID: "lr1", Name: "poll_job"}),
			},
			interrupted: false,
		},
		{
			name: "compaction summary after a completed turn is skipped",
			events: []*session.Event{
				userTextEvent("q"),
				modelTextEvent("answer"),
				summaryEvent("running summary"),
			},
			interrupted: false,
		},
		{
			name: "annotation rows after an interrupted turn are skipped",
			events: []*session.Event{
				userTextEvent("hello?"),
				annotationEvent(),
			},
			interrupted: true,
		},
		{
			name: "subagent branch tail does not mask the parent's completed turn",
			events: []*session.Event{
				modelTextEvent("parent done"),
				branched(userTextEvent("subagent goal")),
			},
			interrupted: false,
		},
		{
			name:        "empty history",
			events:      nil,
			interrupted: false,
		},
		{
			// Review P1: an operator interrupt is a deliberate kill —
			// the audit row postdates the dangling tail and must veto
			// continuation.
			name: "operator interrupt audit row vetoes continuation",
			events: []*session.Event{
				userTextEvent("do it"),
				callEvent(defaultAgentName, "inv-1", nil, &genai.FunctionCall{ID: "c1", Name: "bash"}),
				interruptAuditRow(),
			},
			interrupted: false,
		},
		{
			// An ErrorCode final ends the turn — the user already saw
			// the error; walking past it would misread the earlier
			// response event.
			name: "error-code final is terminal",
			events: []*session.Event{
				userTextEvent("q"),
				callEvent(defaultAgentName, "inv-1", nil, &genai.FunctionCall{ID: "c1", Name: "bash"}),
				responseEvent(defaultAgentName, &genai.FunctionResponse{ID: "c1", Name: "bash"}),
				errorFinalEvent("SAFETY"),
			},
			interrupted: false,
		},
		{
			// Gemini streaming can close a COMPLETED turn with an
			// empty aggregate; walking past it would misread the
			// response event below as an interrupted tail.
			name: "empty-parts model final is terminal",
			events: []*session.Event{
				userTextEvent("q"),
				callEvent(defaultAgentName, "inv-1", nil, &genai.FunctionCall{ID: "c1", Name: "bash"}),
				responseEvent(defaultAgentName, &genai.FunctionResponse{ID: "c1", Name: "bash"}),
				emptyModelFinal(),
			},
			interrupted: false,
		},
		{
			name: "turn parked on tool confirmation is not interrupted",
			events: []*session.Event{
				userTextEvent("dangerous thing please"),
				callEvent(defaultAgentName, "inv-c", []string{"conf-1"},
					&genai.FunctionCall{ID: "conf-1", Name: confirmationCallName},
				),
			},
			interrupted: false,
		},
		{
			name: "skip-summarization response final is a completed turn",
			events: []*session.Event{
				userTextEvent("q"),
				callEvent(defaultAgentName, "inv-1", nil, &genai.FunctionCall{ID: "c1", Name: "bash"}),
				skipSummarizationResponse("c1"),
			},
			interrupted: false,
		},
		{
			// Crash-loop bound: a committed continuation note whose
			// turn then died must NOT trigger a second attempt.
			name: "committed continuation note is not re-continued",
			events: []*session.Event{
				userTextEvent("hello?"),
				userTextEvent("[Inbox]\n1. " + AutoContinueNote(time.Now())),
			},
			interrupted: false,
		},
		{
			// Migration: a note committed by a pre-#615 binary carries the
			// legacy "daemon restart" marker. The classifier must still
			// recognize it so an in-flight note across an upgrade isn't
			// re-continued into a loop.
			name: "legacy-marker continuation note is not re-continued",
			events: []*session.Event{
				userTextEvent("hello?"),
				userTextEvent("[system note] The previous turn was interrupted by a daemon restart at 2026-08-10T00:00:00Z. Continue the task."),
			},
			interrupted: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			at, got := ClassifyInterruptedTail(tc.events)
			if got != tc.interrupted {
				t.Fatalf("interrupted = %v, want %v", got, tc.interrupted)
			}
			if got && at.IsZero() {
				t.Error("interrupted but interruptedAt is zero — freshness window would misfire")
			}
		})
	}
}

func TestClassifyInterruptedTail_TimestampIsTailEvent(t *testing.T) {
	t.Parallel()
	old := userTextEvent("hello?")
	old.Timestamp = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	at, interrupted := ClassifyInterruptedTail([]*session.Event{modelTextEvent("prior"), old})
	if !interrupted || !at.Equal(old.Timestamp) {
		t.Errorf("(at=%v, interrupted=%v), want the tail event's timestamp", at, interrupted)
	}
}
