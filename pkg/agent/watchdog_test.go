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
	"strings"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// Bridge tests — focused on the agent-side wiring (event-tap →
// watchdog.ObserveToolCall, post-turn → watchdog.Check). The
// watchdog's own behavior is exercised in pkg/watchdog/watchdog_test.go.
// Here we verify the *plumbing*: the bridge correctly extracts tool
// calls from session events, serializes args stably, and fans alerts
// to the callback.

// fakeWatchdog records every observation and lets a test inject alerts
// to be returned from the next Check. Keeps the test independent of
// the real signal logic — we're verifying the bridge, not the signal.
type fakeWatchdog struct {
	observed []watchdog.ToolCall
	pending  []watchdog.Alert
	resets   int
}

func (f *fakeWatchdog) ObserveToolCall(tc watchdog.ToolCall) {
	f.observed = append(f.observed, tc)
}

func (f *fakeWatchdog) Check() []watchdog.Alert {
	out := f.pending
	f.pending = nil
	return out
}

func (f *fakeWatchdog) Reset() { f.resets++ }

func TestObserveToolCallsForWatchdog_ExtractsFunctionCalls(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{}
	a := &Agent{watchdog: w}
	ev := &session.Event{
		LLMResponse: model.LLMResponse{
			Content: &genai.Content{
				Parts: []*genai.Part{
					{Text: "I'll read the file."},
					{FunctionCall: &genai.FunctionCall{
						Name: "read_file",
						Args: map[string]any{"path": "main.go"},
					}},
					{FunctionCall: &genai.FunctionCall{
						Name: "grep",
						Args: map[string]any{"pattern": "foo"},
					}},
				},
			},
		},
	}
	a.observeToolCallsForWatchdog(ev)
	if got, want := len(w.observed), 2; got != want {
		t.Fatalf("observed %d calls, want %d", got, want)
	}
	if w.observed[0].Name != "read_file" {
		t.Errorf("[0].Name = %q, want read_file", w.observed[0].Name)
	}
	if !strings.Contains(w.observed[0].Args, "main.go") {
		t.Errorf("[0].Args should embed path arg; got %q", w.observed[0].Args)
	}
	if w.observed[1].Name != "grep" {
		t.Errorf("[1].Name = %q, want grep", w.observed[1].Name)
	}
}

func TestObserveToolCallsForWatchdog_NilSafe(t *testing.T) {
	t.Parallel()
	// All four no-op paths: nil watchdog, nil event, nil content,
	// empty parts. None should panic. (Bridge runs from the streaming
	// event loop — a panic here would tear down the agent mid-turn.)
	a := &Agent{}
	a.observeToolCallsForWatchdog(nil) // nil watchdog AND nil ev
	a.watchdog = &fakeWatchdog{}
	a.observeToolCallsForWatchdog(nil)
	a.observeToolCallsForWatchdog(&session.Event{}) // nil content
	a.observeToolCallsForWatchdog(&session.Event{
		LLMResponse: model.LLMResponse{Content: &genai.Content{}}, // empty parts
	})
	a.observeToolCallsForWatchdog(&session.Event{
		LLMResponse: model.LLMResponse{Content: &genai.Content{
			Parts: []*genai.Part{nil, {Text: "x"}},
		}},
	})
}

func TestSerializeArgsForWatchdog_StableAcrossMapOrder(t *testing.T) {
	t.Parallel()
	// Go's map iteration is randomized. The serializer MUST produce
	// the same string for the same logical args every call — otherwise
	// the watchdog's literal-string-compare detector would see every
	// call as distinct and never trip on a real loop.
	args := map[string]any{
		"path":      "main.go",
		"max_lines": 100,
		"recursive": false,
		"glob":      "*.go",
	}
	first := serializeArgsForWatchdog(args)
	for i := 0; i < 20; i++ {
		if got := serializeArgsForWatchdog(args); got != first {
			t.Fatalf("iteration %d: got %q, want stable %q", i, got, first)
		}
	}
}

func TestSerializeArgsForWatchdog_EmptyArgs(t *testing.T) {
	t.Parallel()
	if got := serializeArgsForWatchdog(nil); got != "{}" {
		t.Errorf("nil args → %q, want %q", got, "{}")
	}
	if got := serializeArgsForWatchdog(map[string]any{}); got != "{}" {
		t.Errorf("empty args → %q, want %q", got, "{}")
	}
}

func TestDrainWatchdogAlerts_DispatchesToCallback(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{pending: []watchdog.Alert{
		{Signal: "repeated-tool-call", Severity: watchdog.SeverityWarn, Reason: "looping on read_file"},
		{Signal: "repeated-tool-call", Severity: watchdog.SeverityWarn, Reason: "looping on grep"},
	}}
	var got []watchdog.Alert
	a := &Agent{
		watchdog:        w,
		onWatchdogAlert: func(al watchdog.Alert) { got = append(got, al) },
	}
	a.drainWatchdogAlerts()
	if len(got) != 2 {
		t.Fatalf("expected 2 alerts dispatched; got %d", len(got))
	}
	if got[0].Signal != "repeated-tool-call" || got[1].Reason != "looping on grep" {
		t.Errorf("unexpected dispatched alerts: %+v", got)
	}
}

func TestDrainWatchdogAlerts_NoCallbackDrainsButDoesNotPanic(t *testing.T) {
	t.Parallel()
	// Bridge contract: if no callback is wired, alerts are pulled
	// (so they don't leak into the next turn) but silently discarded.
	w := &fakeWatchdog{pending: []watchdog.Alert{{Signal: "x"}}}
	a := &Agent{watchdog: w /* no onWatchdogAlert */}
	a.drainWatchdogAlerts()
}

func TestDrainWatchdogAlerts_NilWatchdogIsNoOp(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	// Should NOT panic, should NOT call the callback (no callback,
	// no watchdog — pure no-op).
	a.drainWatchdogAlerts()
}

func TestWithWatchdog_SetsBothFields(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{}
	cb := func(watchdog.Alert) {}
	o := &options{}
	WithWatchdog(w, cb)(o)
	if o.watchdog != w {
		t.Errorf("options.watchdog not set")
	}
	if o.onWatchdogAlert == nil {
		t.Errorf("options.onWatchdogAlert not set")
	}
}
