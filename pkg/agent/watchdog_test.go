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
	a.observeToolCallsForWatchdog(ev, map[string]struct{}{})
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
	a.observeToolCallsForWatchdog(nil, map[string]struct{}{}) // nil watchdog AND nil ev
	a.watchdog = &fakeWatchdog{}
	a.observeToolCallsForWatchdog(nil, map[string]struct{}{})
	a.observeToolCallsForWatchdog(&session.Event{}, map[string]struct{}{}) // nil content
	a.observeToolCallsForWatchdog(&session.Event{
		LLMResponse: model.LLMResponse{Content: &genai.Content{}}, // empty parts
	}, map[string]struct{}{})
	a.observeToolCallsForWatchdog(&session.Event{
		LLMResponse: model.LLMResponse{Content: &genai.Content{
			Parts: []*genai.Part{nil, {Text: "x"}},
		}},
	}, map[string]struct{}{})
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

// TestObserveToolCallsForWatchdog_DedupsAggregatorReEmission is the
// #363 regression gate, now covering the ID backstop rather than the
// primary guard: re-emission is kept out of the count by skipping
// partial events (#915), and this pins what happens if the same part
// reaches the tap twice at non-partial level anyway. Without dedup
// each real call counted up to twice and the repeated-tool-call
// signal tripped at ~half the configured threshold. Same-ID
// re-emission dedups; a legitimate parallel call with identical args
// but a distinct ID still counts; and a fresh Run (fresh seen set)
// counts again — repetition IS the watchdog's signal.
func TestObserveToolCallsForWatchdog_DedupsAggregatorReEmission(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{}
	a := &Agent{watchdog: w}

	call := &genai.FunctionCall{ID: "fc-1", Name: "grep", Args: map[string]any{"pattern": "foo"}}
	evIntermediate := &session.Event{LLMResponse: model.LLMResponse{Content: &genai.Content{
		Parts: []*genai.Part{{FunctionCall: call}},
	}}}
	evFinal := &session.Event{LLMResponse: model.LLMResponse{Content: &genai.Content{
		Parts: []*genai.Part{{FunctionCall: call}},
	}}}

	seen := map[string]struct{}{}
	a.observeToolCallsForWatchdog(evIntermediate, seen)
	a.observeToolCallsForWatchdog(evFinal, seen)
	if got := len(w.observed); got != 1 {
		t.Fatalf("re-emitted part observed %d times, want 1", got)
	}

	// A legitimate parallel call: same name+args, DIFFERENT ID.
	evParallel := &session.Event{LLMResponse: model.LLMResponse{Content: &genai.Content{
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
			ID: "fc-2", Name: "grep", Args: map[string]any{"pattern": "foo"},
		}}},
	}}}
	a.observeToolCallsForWatchdog(evParallel, seen)
	if got := len(w.observed); got != 2 {
		t.Fatalf("distinct-ID parallel call observed total %d, want 2", got)
	}

	// Next turn: fresh seen set — the SAME call must count again
	// (cross-turn repetition is the runaway signal).
	a.observeToolCallsForWatchdog(evFinal, map[string]struct{}{})
	if got := len(w.observed); got != 3 {
		t.Fatalf("cross-turn repeat observed total %d, want 3 (dedup must not span turns)", got)
	}
}

// TestObserveToolCallsForWatchdog_IDLessCallsEachCount is #915's
// regression gate, driven into a REAL watchdog rather than the fake:
// the assertion is not "n observations arrived" but "the signal the
// observations exist to feed actually fires".
//
// An ID-less call used to dedup on name+args, which is the key every
// args-sensitive detector is looking for a repeat of — so five
// identical calls arrived as one and `repeated-tool-call` could not
// reach its threshold from any number of them. Pre-fix this observes
// one call and raises nothing.
func TestObserveToolCallsForWatchdog_IDLessCallsEachCount(t *testing.T) {
	t.Parallel()
	w := watchdog.NewDefaultWatchdog()
	a := &Agent{watchdog: w}

	ev := &session.Event{LLMResponse: model.LLMResponse{Content: &genai.Content{
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
			Name: "grep", Args: map[string]any{"pattern": "foo"},
		}}},
	}}}
	seen := map[string]struct{}{}
	for range watchdog.DefaultRepeatThreshold {
		a.observeToolCallsForWatchdog(ev, seen)
	}

	var found bool
	for _, al := range w.Check() {
		if al.Signal != "repeated-tool-call" {
			continue
		}
		found = true
		if al.Severity != watchdog.SeverityCritical {
			t.Errorf("repeated-tool-call severity = %v, want Critical", al.Severity)
		}
	}
	if !found {
		t.Errorf("%d identical ID-less calls raised no repeated-tool-call alert", watchdog.DefaultRepeatThreshold)
	}
}

// TestObserveToolCallsForWatchdog_SkipsPartialEvents pins the
// mechanism that replaced the content key (#915). A partial event's
// parts are re-delivered on the aggregated event that follows, and it
// is the aggregated one ADK executes the tool from, so observing the
// partial can only double-count.
func TestObserveToolCallsForWatchdog_SkipsPartialEvents(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{}
	a := &Agent{watchdog: w}

	call := &genai.FunctionCall{Name: "grep", Args: map[string]any{"pattern": "foo"}}
	partial := &session.Event{LLMResponse: model.LLMResponse{
		Content: &genai.Content{Parts: []*genai.Part{{FunctionCall: call}}},
		Partial: true,
	}}
	if observed := a.observeToolCallsForWatchdog(partial, map[string]struct{}{}); observed {
		t.Errorf("a partial event reported an observation; the aggregate that follows is the one to count")
	}
	if got := len(w.observed); got != 0 {
		t.Fatalf("partial event fed %d calls to the watchdog, want 0", got)
	}
}
