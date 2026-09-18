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
	"strings"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// textWatchdog is the opted-in shape for the assistant-text extension
// (#655). fakeWatchdog (watchdog_test.go) deliberately does not
// implement it — that is the third-party watchdog written before this
// existed, and it must be left alone rather than panicked on.
type textWatchdog struct {
	fakeWatchdog
	texts []string
}

func (f *textWatchdog) ObserveAssistantText(s string) { f.texts = append(f.texts, s) }

const wdAgentName = "core_agent"

func wdTextEvent(author string, parts ...*genai.Part) *session.Event {
	return &session.Event{
		Author:      author,
		LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: parts}},
	}
}

func TestObserveAssistantTextForWatchdog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ev   *session.Event
		want []string
	}{
		{
			name: "the model speaking",
			ev:   wdTextEvent(wdAgentName, &genai.Part{Text: "The pods are crash-looping."}),
			want: []string{"The pods are crash-looping."},
		},
		{
			name: "every text part of one event",
			ev: wdTextEvent(wdAgentName,
				&genai.Part{Text: "first"},
				&genai.Part{FunctionCall: &genai.FunctionCall{Name: "gke_get_pod"}},
				&genai.Part{Text: "second"}),
			want: []string{"first", "second"},
		},
		{
			// The user talking TO a silently grinding agent must not
			// clear its run. This is the filter the signal depends on
			// most: inbox injections and user turns are text parts on
			// events the model did not author.
			name: "the user speaking is not the model speaking",
			ev:   wdTextEvent("user", &genai.Part{Text: "any update?"}),
			want: nil,
		},
		{
			// A subagent's own events carry its name, not the parent's.
			// The parent grinding in silence is what the parent's
			// watchdog is watching.
			name: "another author is not this agent",
			ev:   wdTextEvent("gke-triage", &genai.Part{Text: "done"}),
			want: nil,
		},
		{
			// Reasoning is not reporting. Nobody is shown it.
			name: "a thought is not speech",
			ev:   wdTextEvent(wdAgentName, &genai.Part{Text: "I should check the events", Thought: true}),
			want: nil,
		},
		{
			name: "whitespace is not speech",
			ev:   wdTextEvent(wdAgentName, &genai.Part{Text: "  \n "}),
			want: nil,
		},
		{
			name: "a tool call carries no text",
			ev:   wdTextEvent(wdAgentName, &genai.Part{FunctionCall: &genai.FunctionCall{Name: "read_file"}}),
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := &textWatchdog{}
			a := &Agent{watchdog: w, agentName: wdAgentName}
			a.observeAssistantTextForWatchdog(tc.ev)
			if strings.Join(w.texts, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("observed %q, want %q", w.texts, tc.want)
			}
		})
	}
}

// TestObserveAssistantTextForWatchdog_SkipsPartials is the streaming
// case, and it is the difference between the signal working and the
// signal being unfireable. ADK emits one event per chunk with Partial
// set, then one aggregated event with it clear; counting the chunks
// would clear the run on every token of a streaming turn.
func TestObserveAssistantTextForWatchdog_SkipsPartials(t *testing.T) {
	t.Parallel()

	w := &textWatchdog{}
	a := &Agent{watchdog: w, agentName: wdAgentName}

	partial := wdTextEvent(wdAgentName, &genai.Part{Text: "The pods"})
	partial.Partial = true
	a.observeAssistantTextForWatchdog(partial)
	if len(w.texts) != 0 {
		t.Fatalf("a partial event reached the watchdog: %q", w.texts)
	}

	a.observeAssistantTextForWatchdog(wdTextEvent(wdAgentName, &genai.Part{Text: "The pods are crash-looping."}))
	if len(w.texts) != 1 {
		t.Fatalf("the aggregated event did not reach the watchdog: %q", w.texts)
	}
}

// TestObserveAssistantTextForWatchdog_Degenerate: the tap must survive
// every shape a host or a provider can hand it, including a watchdog
// that predates the extension.
func TestObserveAssistantTextForWatchdog_Degenerate(t *testing.T) {
	t.Parallel()

	var nilAgent *Agent
	nilAgent.observeAssistantTextForWatchdog(wdTextEvent(wdAgentName, &genai.Part{Text: "x"}))

	// A watchdog that never heard of assistant text (every third-party
	// implementation written before #655) is skipped, not panicked on.
	a := &Agent{watchdog: &fakeWatchdog{}, agentName: wdAgentName}
	if a.observeAssistantTextForWatchdog(wdTextEvent(wdAgentName, &genai.Part{Text: "x"})) {
		t.Error("a call-only watchdog reported an observation")
	}

	b := &Agent{watchdog: &textWatchdog{}, agentName: wdAgentName}
	b.observeAssistantTextForWatchdog(nil)
	b.observeAssistantTextForWatchdog(&session.Event{})
	b.observeAssistantTextForWatchdog(wdTextEvent(wdAgentName))
	b.observeAssistantTextForWatchdog(wdTextEvent(wdAgentName, nil))
}

// TestRun_FeedsAssistantTextToTheWatchdog is the wiring test, and on the
// evidence it is the most important one in this file. Every other test
// here calls observeAssistantTextForWatchdog directly, so deleting its
// single call site in Agent.Run's event tap leaves the signal permanently
// un-clearable in production — a silent run would never be cleared by
// anything, and the alert would fire on the twelfth call of every
// tool-using session — while the entire suite stays green. Confirmed by
// mutation: removing that one line failed nothing before this test
// existed.
func TestRun_FeedsAssistantTextToTheWatchdog(t *testing.T) {
	t.Parallel()

	w := &textWatchdog{}
	a, err := New(&recordingLLM{}, WithSession("u-wdt", "s-wdt"), WithWatchdog(w, nil))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	for _, err := range a.Run(context.Background(), "why are the pods restarting?") {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	// recordingLLM answers "ok" as the model.
	if len(w.texts) == 0 {
		t.Fatal("a completed turn fed no assistant text to the watchdog; " +
			"the tap in Agent.Run is not wired")
	}
	// And the operator's own prompt travels through the same iterator, so
	// pin that it did not arrive as the agent's speech.
	for _, got := range w.texts {
		if strings.Contains(got, "why are the pods restarting?") {
			t.Errorf("the user's prompt reached the watchdog as assistant text: %q", got)
		}
	}
}

// TestSilenceIsBrokenOnlyByTheAgentsOwnWords wires the tap to the real
// default watchdog and drives it with the event shapes a live turn
// produces. The unit tests above prove the filter; this proves the
// filter is the one the shipped signal ends up seeing.
//
// The shape it pins is the one a filter bug would silently destroy: an
// agent making DefaultToolsWithoutText calls in silence, while a user
// message and its own reasoning stream past, must still alert.
func TestSilenceIsBrokenOnlyByTheAgentsOwnWords(t *testing.T) {
	t.Parallel()

	var got []watchdog.Alert
	w := watchdog.NewDefaultWatchdog()
	a := &Agent{watchdog: w, agentName: wdAgentName}

	for i := range watchdog.DefaultToolsWithoutText {
		// Things that are not the agent reporting: a user asking for an
		// update, and the model's own private reasoning.
		a.observeAssistantTextForWatchdog(wdTextEvent("user", &genai.Part{Text: "any update?"}))
		a.observeAssistantTextForWatchdog(wdTextEvent(wdAgentName,
			&genai.Part{Text: "still working", Thought: true}))
		a.observeToolCallsForWatchdog(&session.Event{
			Author: wdAgentName,
			LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{
					ID: string(rune('a' + i)), Name: "gke_get_k8s_resource",
					Args: map[string]any{"i": i},
				}},
			}}},
		}, map[string]struct{}{})
	}
	got = append(got, w.Check()...)

	found := false
	for _, alert := range got {
		if alert.Signal == "tools-without-text" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a silent run of %d calls did not trip tools-without-text; alerts = %+v",
			watchdog.DefaultToolsWithoutText, got)
	}

	// And one sentence from the agent itself clears it.
	w.Reset()
	for i := range watchdog.DefaultToolsWithoutText {
		if i == watchdog.DefaultToolsWithoutText/2 {
			a.observeAssistantTextForWatchdog(wdTextEvent(wdAgentName,
				&genai.Part{Text: "So far: the deployment is failing its image pull."}))
		}
		a.observeToolCallsForWatchdog(&session.Event{
			Author: wdAgentName,
			LLMResponse: model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{
					ID: string(rune('A' + i)), Name: "gke_get_k8s_resource",
					Args: map[string]any{"i": i},
				}},
			}}},
		}, map[string]struct{}{})
	}
	if alerts := w.Check(); len(alerts) != 0 {
		t.Fatalf("the agent reported mid-run and the watchdog alerted anyway: %+v", alerts)
	}
}
