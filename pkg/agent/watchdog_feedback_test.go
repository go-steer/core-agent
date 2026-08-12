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

	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// Feedback-mode tests (#159). The property under test is routing: the
// watchdog already produces the right observation, and warn mode sends
// it to a reader who cannot act on it. These assert the observation
// reaches the model, reaches it exactly once, survives the paths that
// would otherwise drop it (an operator reset, a process restart), and
// stays absent under warn.

func TestWithWatchdogFeedback_SetsOption(t *testing.T) {
	t.Parallel()
	o := &options{}
	WithWatchdogFeedback()(o)
	if !o.watchdogFeedback {
		t.Errorf("WithWatchdogFeedback should set options.watchdogFeedback")
	}
}

// The core routing property. Fails on pre-#159 code: drainWatchdogAlerts
// dispatched to the operator callback and nothing touched the prompt, so
// turn 2's request carried no watchdog text at all.
func TestRun_WatchdogFeedback_InjectsObservationIntoNextTurn(t *testing.T) {
	t.Parallel()
	rec := &recordingLLM{}
	w := &fakeWatchdog{pending: []watchdog.Alert{{
		Signal:   "repeated-tool-call",
		Severity: watchdog.SeverityCritical,
		Reason:   "operator-facing text mentioning /interrupt",
		Guidance: "You called read_file 5 times in a row with identical arguments.",
	}}}
	a, err := New(rec,
		WithSession("u-wdf", "s-wdf"),
		WithWatchdog(w, nil),
		WithWatchdogFeedback(),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	ctx := context.Background()

	// Turn 1: the alert is raised by the post-turn drain, so this turn's
	// own request must be clean — the observation is about behavior that
	// hadn't happened yet when the prompt was built.
	for _, err := range a.Run(ctx, "first") {
		if err != nil {
			t.Fatalf("turn 1: %v", err)
		}
	}
	if got := flattenText(rec.lastRequest().Contents); strings.Contains(got, watchdog.FeedbackHeader) {
		t.Fatalf("turn 1 should not carry a watchdog block: %q", got)
	}

	// Turn 2: the queued observation is prepended.
	for _, err := range a.Run(ctx, "second") {
		if err != nil {
			t.Fatalf("turn 2: %v", err)
		}
	}
	got := flattenText(rec.lastRequest().Contents)
	if !strings.Contains(got, watchdog.FeedbackHeader) {
		t.Fatalf("turn 2 should carry the watchdog block; got %q", got)
	}
	if !strings.Contains(got, "You called read_file 5 times") {
		t.Errorf("turn 2 should carry the model-facing guidance; got %q", got)
	}
	if strings.Contains(got, "/interrupt") {
		t.Errorf("the operator-facing Reason leaked into the model's context; got %q", got)
	}
	if !strings.Contains(got, "second") {
		t.Errorf("the operator's own prompt must survive the prepend; got %q", got)
	}

	// Turn 3: delivered once, not every turn. A block that repeats until
	// the loop stops would itself become the loop. The turn-2 copy stays
	// visible in history, so count occurrences rather than absence.
	for _, err := range a.Run(ctx, "third") {
		if err != nil {
			t.Fatalf("turn 3: %v", err)
		}
	}
	if n := strings.Count(flattenText(rec.lastRequest().Contents), watchdog.FeedbackHeader); n != 1 {
		t.Errorf("observation should be delivered once; found %d copies in turn 3's request", n)
	}
}

// Mode boundary: warn observes and logs, and must not touch the prompt.
// Injecting under warn would change every existing operator's context
// silently, which is the reason feedback is its own mode.
func TestRun_WatchdogWarn_DoesNotInject(t *testing.T) {
	t.Parallel()
	rec := &recordingLLM{}
	var dispatched int
	w := &fakeWatchdog{pending: []watchdog.Alert{{
		Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical,
		Reason: "loop", Guidance: "stop looping",
	}}}
	a, err := New(rec,
		WithSession("u-wdw", "s-wdw"),
		WithWatchdog(w, func(watchdog.Alert) { dispatched++ }),
		// no WithWatchdogFeedback
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	ctx := context.Background()
	for _, err := range a.Run(ctx, "first") {
		if err != nil {
			t.Fatalf("turn 1: %v", err)
		}
	}
	for _, err := range a.Run(ctx, "second") {
		if err != nil {
			t.Fatalf("turn 2: %v", err)
		}
	}
	if got := flattenText(rec.lastRequest().Contents); strings.Contains(got, watchdog.FeedbackHeader) {
		t.Errorf("warn mode must not inject into the model's context: %q", got)
	}
	if dispatched == 0 {
		t.Errorf("warn mode should still dispatch to the operator callback")
	}
}

// Enforce implies feedback. An enforce-mode halt is cleared by an
// operator reset, and the reset resumes a model whose context still ends
// in the loop it was halted for — so without the injection the first
// post-reset turn re-issues the same call and re-trips. Fails on
// pre-#159 code, where the post-reset turn's prompt was just "resumed".
func TestRun_WatchdogEnforce_PostResetTurnCarriesTheObservation(t *testing.T) {
	t.Parallel()
	rec := &recordingLLM{}
	w := &fakeWatchdog{pending: []watchdog.Alert{{
		Signal:   "repeated-tool-call",
		Severity: watchdog.SeverityCritical,
		Reason:   "looping on read_file 5x.",
		Guidance: "You called read_file 5 times in a row with identical arguments.",
	}}}
	a, err := New(rec,
		WithSession("u-wde", "s-wde"),
		WithWatchdog(w, nil),
		WithWatchdogEnforce(), // no explicit WithWatchdogFeedback
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	if got := a.WatchdogMode(); got != "enforce" {
		t.Fatalf("WatchdogMode() = %q, want enforce", got)
	}
	ctx := context.Background()

	for _, err := range a.Run(ctx, "first") {
		if err != nil {
			t.Fatalf("turn 1: %v", err)
		}
	}
	if tripped, _ := a.WatchdogTripped(); !tripped {
		t.Fatalf("enforce should have tripped on the Critical alert")
	}
	a.ResetWatchdog()
	for _, err := range a.Run(ctx, "resumed") {
		if err != nil {
			t.Fatalf("post-reset turn: %v", err)
		}
	}
	got := flattenText(rec.lastRequest().Contents)
	if !strings.Contains(got, watchdog.FeedbackHeader) {
		t.Fatalf("the post-reset turn must carry the observation, or the reset is a treadmill: %q", got)
	}
	if !strings.Contains(got, "You called read_file 5 times") {
		t.Errorf("post-reset turn should carry the guidance; got %q", got)
	}
}

// ResetWatchdog clears the halt and the signal state. It must not clear
// the queued observation — that is the half that stops the resumed turn
// from walking straight back into the loop.
func TestResetWatchdog_KeepsQueuedFeedback(t *testing.T) {
	t.Parallel()
	a := &Agent{
		watchdog:         &fakeWatchdog{},
		watchdogEnforce:  true,
		watchdogFeedback: true,
		watchdogPending:  []watchdog.Alert{{Signal: "repeated-tool-call", Guidance: "stop looping"}},
	}
	a.ResetWatchdog()
	if got := a.prependWatchdogFeedback("next"); !strings.Contains(got, "stop looping") {
		t.Errorf("ResetWatchdog dropped the queued observation: %q", got)
	}
}

func TestQueueWatchdogFeedback_BoundsTheQueue(t *testing.T) {
	t.Parallel()
	a := &Agent{watchdogFeedback: true}
	for i := 0; i < maxPendingWatchdogFeedback+3; i++ {
		a.queueWatchdogFeedback([]watchdog.Alert{{Signal: "s", Guidance: string(rune('a' + i))}})
	}
	a.mu.Lock()
	n := len(a.watchdogPending)
	oldestKept := a.watchdogPending[0].Guidance
	a.mu.Unlock()
	if n != maxPendingWatchdogFeedback {
		t.Errorf("queue length = %d, want the cap %d", n, maxPendingWatchdogFeedback)
	}
	// Oldest are dropped: the newest observation describes what the
	// model is about to repeat.
	if oldestKept == "a" {
		t.Errorf("cap should drop the oldest entries, kept %q first", oldestKept)
	}
}

func TestPrependWatchdogFeedback_NoPendingIsIdentity(t *testing.T) {
	t.Parallel()
	a := &Agent{watchdogFeedback: true}
	if got := a.prependWatchdogFeedback("just the prompt"); got != "just the prompt" {
		t.Errorf("empty queue should pass the prompt through unchanged; got %q", got)
	}
}

// Feedback must not queue when the mode is off, or a host that later
// flips the bit would deliver a backlog of stale observations.
func TestDrainWatchdogAlerts_WarnModeQueuesNothing(t *testing.T) {
	t.Parallel()
	a := &Agent{watchdog: &fakeWatchdog{pending: []watchdog.Alert{
		{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Guidance: "stop"},
	}}}
	a.drainWatchdogAlerts()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.watchdogPending) != 0 {
		t.Errorf("warn mode queued %d alerts for injection; want 0", len(a.watchdogPending))
	}
}

// The model-facing route must not depend on whether the host wired the
// operator-facing one — the same reason enforcement sits outside that
// guard. An embedder that passes WithWatchdog(w, nil) is the headless
// case feedback mode exists for.
func TestDrainWatchdogAlerts_FeedbackQueuesWithoutCallback(t *testing.T) {
	t.Parallel()
	a := &Agent{
		watchdog: &fakeWatchdog{pending: []watchdog.Alert{
			{Signal: "repeated-tool-call", Severity: watchdog.SeverityCritical, Guidance: "stop"},
		}},
		watchdogFeedback: true,
	}
	a.drainWatchdogAlerts()
	if got := a.prependWatchdogFeedback("next"); !strings.Contains(got, "stop") {
		t.Errorf("feedback should queue with no operator callback wired; got %q", got)
	}
}

func TestWatchdogMode_Feedback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []Option
		want string
	}{
		{"observe and tell the model", []Option{WithWatchdog(&fakeWatchdog{}, nil), WithWatchdogFeedback()}, "feedback"},
		// Enforce is the stronger rung and reports as itself even though
		// it implies feedback — an operator reading "feedback" would
		// conclude nothing halts the session.
		{"enforce reports as enforce", []Option{WithWatchdog(&fakeWatchdog{}, nil), WithWatchdogFeedback(), WithWatchdogEnforce()}, "enforce"},
		// Same no-op contract as WithWatchdogEnforce: without a watchdog
		// there is nothing to route, so the mode must not claim otherwise.
		{"feedback without a watchdog", []Option{WithWatchdogFeedback()}, "off"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, err := New(oneShotLLM{}, append([]Option{WithSession("u-wdfm", "s-wdfm")}, tc.opts...)...)
			if err != nil {
				t.Fatalf("agent.New: %v", err)
			}
			if got := a.WatchdogMode(); got != tc.want {
				t.Errorf("WatchdogMode() = %q, want %q", got, tc.want)
			}
		})
	}
}
