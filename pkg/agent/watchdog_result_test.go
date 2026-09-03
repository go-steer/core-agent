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
	"errors"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// Tool-outcome bridge (#639). fakeWatchdog (watchdog_test.go)
// deliberately does NOT implement watchdog.ToolResultObserver — that
// is the "call-only third-party watchdog" case, and it must stay
// silent rather than panic. resultWatchdog is the opted-in shape.
type resultWatchdog struct {
	fakeWatchdog
	results []watchdog.ToolResult
}

func (f *resultWatchdog) ObserveToolResult(tr watchdog.ToolResult) {
	f.results = append(f.results, tr)
}

func wdResultEvent(parts ...*genai.Part) *session.Event {
	return &session.Event{LLMResponse: model.LLMResponse{
		Content: &genai.Content{Parts: parts},
	}}
}

func wdResultPart(id, name string, resp map[string]any) *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{
		ID: id, Name: name, Response: resp,
	}}
}

func TestObserveToolResultsForWatchdog_SplitsSuccessFromFailure(t *testing.T) {
	t.Parallel()
	w := &resultWatchdog{}
	a := &Agent{watchdog: w}

	a.observeToolResultsForWatchdog(wdResultEvent(
		wdResultPart("1", "read_file", map[string]any{"content": "hi"}),
		wdResultPart("2", "gke_get_pod", map[string]any{"error": "PermissionDenied"}),
	), map[string]struct{}{})

	if got := len(w.results); got != 2 {
		t.Fatalf("observed %d results, want 2", got)
	}
	if w.results[0].Failed() {
		t.Errorf("[0] = %+v, want success", w.results[0])
	}
	if !w.results[1].Failed() || w.results[1].Error != "PermissionDenied" {
		t.Errorf("[1] = %+v, want the ADK error key surfaced", w.results[1])
	}
	if w.results[1].Name != "gke_get_pod" {
		t.Errorf("[1].Name = %q, want gke_get_pod", w.results[1].Name)
	}
}

// A watchdog that only counts calls must be left alone, not crashed
// and not silently required to grow a method.
func TestObserveToolResultsForWatchdog_SkipsNonObservers(t *testing.T) {
	t.Parallel()
	w := &fakeWatchdog{}
	a := &Agent{watchdog: w}
	a.observeToolResultsForWatchdog(wdResultEvent(
		wdResultPart("1", "x", map[string]any{"error": "boom"}),
	), map[string]struct{}{})
	if len(w.observed) != 0 {
		t.Errorf("call observations = %d, want 0: results are not calls", len(w.observed))
	}
}

func TestObserveToolResultsForWatchdog_NilSafe(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	a.observeToolResultsForWatchdog(nil, map[string]struct{}{}) // nil watchdog and nil event
	a.watchdog = &resultWatchdog{}
	a.observeToolResultsForWatchdog(nil, map[string]struct{}{})
	a.observeToolResultsForWatchdog(&session.Event{}, map[string]struct{}{}) // nil content
	a.observeToolResultsForWatchdog(wdResultEvent(), map[string]struct{}{})
	a.observeToolResultsForWatchdog(wdResultEvent(nil, &genai.Part{Text: "x"}), map[string]struct{}{})
	a.observeToolResultsForWatchdog(wdResultEvent(
		&genai.Part{FunctionResponse: &genai.FunctionResponse{Name: "x"}}, // nil Response map
	), map[string]struct{}{})
}

// The streaming aggregator re-emits response parts exactly as it
// re-emits call parts; a double-counted failure would trip the streak
// signal at half its threshold.
func TestObserveToolResultsForWatchdog_DedupsAggregatorReEmission(t *testing.T) {
	t.Parallel()
	w := &resultWatchdog{}
	a := &Agent{watchdog: w}
	seen := map[string]struct{}{}

	ev := wdResultEvent(wdResultPart("call-1", "gke_get_pod", map[string]any{"error": "boom"}))
	a.observeToolResultsForWatchdog(ev, seen)
	a.observeToolResultsForWatchdog(ev, seen) // re-emission on the final aggregate
	if got := len(w.results); got != 1 {
		t.Fatalf("observed %d, want 1 (deduped on ID)", got)
	}
	// A genuinely different call with the same error is a separate ID.
	a.observeToolResultsForWatchdog(wdResultEvent(
		wdResultPart("call-2", "gke_get_pod", map[string]any{"error": "boom"}),
	), seen)
	if got := len(w.results); got != 2 {
		t.Errorf("observed %d, want 2: a distinct call ID is a distinct result", got)
	}
}

// Calls and results share the dedup set. Distinct key spaces keep a
// call's ID from suppressing its own response.
func TestObserveToolResultsForWatchdog_SharesSeenSetWithoutColliding(t *testing.T) {
	t.Parallel()
	w := &resultWatchdog{}
	a := &Agent{watchdog: w}
	seen := map[string]struct{}{}

	a.observeToolCallsForWatchdog(&session.Event{LLMResponse: model.LLMResponse{
		Content: &genai.Content{Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
			ID: "call-1", Name: "gke_get_pod", Args: map[string]any{},
		}}}},
	}}, seen)
	a.observeToolResultsForWatchdog(wdResultEvent(
		wdResultPart("call-1", "gke_get_pod", map[string]any{"error": "boom"}),
	), seen)

	if len(w.observed) != 1 {
		t.Errorf("call observations = %d, want 1", len(w.observed))
	}
	if len(w.results) != 1 {
		t.Errorf("result observations = %d, want 1: the call's ID must not suppress its response", len(w.results))
	}
}

func TestToolResponseError_ReadsTheADKConvention(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		resp map[string]any
		want string
	}{
		{"success", map[string]any{"content": "hi"}, ""},
		{"nil map", nil, ""},
		{"string error", map[string]any{"error": "denied"}, "denied"},
		{"empty string error", map[string]any{"error": ""}, ""},
		{"nil error value", map[string]any{"error": nil}, ""},
		{"error value", map[string]any{"error": errors.New("boom")}, "boom"},
		// A structured error object is still a failure; treating an
		// unrecognized shape as success would drop the observation.
		{"structured error", map[string]any{"error": map[string]any{"code": 403}}, "map[code:403]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := toolResponseError(tc.resp); got != tc.want {
				t.Errorf("toolResponseError(%v) = %q, want %q", tc.resp, got, tc.want)
			}
		})
	}
}

// TestObserveToolResultsForWatchdog_CarriesNoOp: the reserved "no_op"
// key has to survive the flatten, or NoOpStreakSignal never sees the
// only evidence it reads (#907).
func TestObserveToolResultsForWatchdog_CarriesNoOp(t *testing.T) {
	t.Parallel()
	w := &resultWatchdog{}
	a := &Agent{watchdog: w}

	a.observeToolResultsForWatchdog(wdResultEvent(
		wdResultPart("1", "mark_task_done", map[string]any{"status": "acknowledged"}),
		wdResultPart("2", "mark_task_done", map[string]any{"status": "already recorded", "no_op": true}),
	), map[string]struct{}{})

	if got := len(w.results); got != 2 {
		t.Fatalf("observed %d results, want 2", got)
	}
	if w.results[0].NoOp {
		t.Errorf("[0] = %+v, want NoOp false — the first call armed the checkpoint", w.results[0])
	}
	if !w.results[1].NoOp {
		t.Errorf("[1] = %+v, want NoOp true", w.results[1])
	}
}

// TestObserveToolResultsForWatchdog_IDLessResultsAreNotCollapsed is the
// regression for the bug that made #907 inert on the exact deployment
// it was written for. The per-turn dedup set used to key an ID-less
// response on name+error. Gemini functionCall parts carry no ID (ADK
// copies FunctionCall.ID straight through and never synthesizes one),
// and a rejection is not an error, so every "already recorded for this
// turn" no-op in a turn hashed to one key and thirteen of them reached
// the watchdog as a single observation — three short of the threshold,
// forever.
//
// Each part here arrives in a separate event, which is how
// handleFunctionCalls actually emits them: one call, one response.
func TestObserveToolResultsForWatchdog_IDLessResultsAreNotCollapsed(t *testing.T) {
	t.Parallel()
	w := &resultWatchdog{}
	a := &Agent{watchdog: w}
	seen := map[string]struct{}{}

	noOpResp := map[string]any{"status": markTaskDoneRepeatStatus, "no_op": true}
	for range 5 {
		a.observeToolResultsForWatchdog(
			wdResultEvent(wdResultPart("", "mark_task_done", noOpResp)), seen)
	}

	if got := len(w.results); got != 5 {
		t.Fatalf("observed %d ID-less no-op results, want 5 — the dedup set is "+
			"collapsing distinct results, and NoOpStreakSignal can never reach its "+
			"threshold on a Gemini deployment", got)
	}
	for i, r := range w.results {
		if !r.NoOp {
			t.Errorf("[%d] = %+v, want NoOp true", i, r)
		}
	}
}

// TestObserveToolResultsForWatchdog_TripsTheObservedLoopEndToEnd drives
// the #905 trace through the real bridge into a real DefaultWatchdog,
// with the empty IDs a Gemini session actually produces. The sibling
// tests in pkg/watchdog exercise the signal in isolation; the bug above
// lived in the seam between them, which is why it survived seven green
// signal-level tests.
func TestObserveToolResultsForWatchdog_TripsTheObservedLoopEndToEnd(t *testing.T) {
	t.Parallel()
	w := watchdog.NewDefaultWatchdog()
	a := &Agent{watchdog: w}
	seen := map[string]struct{}{}

	emit := func(name string, resp map[string]any) {
		a.observeToolResultsForWatchdog(wdResultEvent(wdResultPart("", name, resp)), seen)
	}
	noOpResp := map[string]any{"status": markTaskDoneRepeatStatus, "no_op": true}

	// The first call armed the checkpoint; seven rejections; one real
	// read that split the run; six more rejections.
	emit("mark_task_done", map[string]any{"status": "acknowledged"})
	for range 7 {
		emit("mark_task_done", noOpResp)
	}
	emit("gke_get_k8s_resource", map[string]any{"kind": "Deployment"})
	for range 6 {
		emit("mark_task_done", noOpResp)
	}

	var noOpAlerts int
	for _, al := range w.Check() {
		if al.Signal != "no-op-streak" {
			continue
		}
		noOpAlerts++
		if al.Severity != watchdog.SeverityCritical {
			t.Errorf("Severity = %v, want Critical", al.Severity)
		}
	}
	if noOpAlerts != 2 {
		t.Fatalf("the observed loop raised %d no-op-streak alerts through the bridge, "+
			"want 2 (one per half of the 7+6 split)", noOpAlerts)
	}
}

// TestObserveToolResultsForWatchdog_DedupsRepeatedIDs: dropping the
// ID-less fallback must not drop dedup for responses that DO carry an
// ID. That is the path where a future ADK change re-emitting responses
// would be observable, and a double-counted result trips a streak
// signal at half its threshold.
func TestObserveToolResultsForWatchdog_DedupsRepeatedIDs(t *testing.T) {
	t.Parallel()
	w := &resultWatchdog{}
	a := &Agent{watchdog: w}
	seen := map[string]struct{}{}

	part := wdResultPart("call-1", "mark_task_done", map[string]any{"no_op": true})
	a.observeToolResultsForWatchdog(wdResultEvent(part), seen)
	a.observeToolResultsForWatchdog(wdResultEvent(part), seen)

	if got := len(w.results); got != 1 {
		t.Fatalf("a re-emitted response with the same ID was observed %d times, want 1", got)
	}
}

// TestToolResponseNoOp: fail-OPEN on an unrecognized value, which is
// the opposite of toolResponseError's fail-safe reading of "error".
// NoOpStreakSignal is Critical and halts the agent under
// --watchdog=enforce, so a garbled value must not be able to
// manufacture a halt.
func TestToolResponseNoOp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		resp map[string]any
		want bool
	}{
		{"absent", map[string]any{"status": "ok"}, false},
		{"nil resp", nil, false},
		{"explicit nil", map[string]any{"no_op": nil}, false},
		{"bool true", map[string]any{"no_op": true}, true},
		{"bool false", map[string]any{"no_op": false}, false},
		{"string true", map[string]any{"no_op": "true"}, true},
		{"string false", map[string]any{"no_op": "false"}, false},
		{"garbled string", map[string]any{"no_op": "yes"}, false},
		{"number", map[string]any{"no_op": 1}, false},
		{"object", map[string]any{"no_op": map[string]any{"why": "repeat"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := toolResponseNoOp(tc.resp); got != tc.want {
				t.Errorf("toolResponseNoOp(%v) = %v, want %v", tc.resp, got, tc.want)
			}
		})
	}
}
