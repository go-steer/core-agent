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

package background

import (
	"context"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/agent/autonomous"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// register plants a handle under a name, the way a completed spawn
// leaves one: terminal handles stay registered until the next same-name
// Spawn evicts them, which is exactly what makes the "stop something
// that already finished" case reachable.
func register(mgr *Manager, name string, status Status) *Handle {
	h := &Handle{Name: name, status: status, done: make(chan struct{})}
	if status != StatusRunning {
		close(h.done)
	}
	mgr.mu.Lock()
	mgr.agents[name] = h
	mgr.mu.Unlock()
	return h
}

// TestStopAndReport_OnlyCreditsARunningSubagent pins the distinction
// the operator route is built on: the transition is observed under the
// handle's own lock, so "I stopped it" cannot be claimed for a subagent
// that had already reached a terminal status (#897).
func TestStopAndReport_OnlyCreditsARunningSubagent(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)

	cases := []struct {
		name    string
		status  Status
		stopped bool
	}{
		{"live", StatusRunning, true},
		{"done", StatusCompleted, false},
		{"broke", StatusFailed, false},
		{"halted", StatusStopped, false},
		{"parked", StatusDeferred, false},
	}
	for _, tc := range cases {
		register(mgr, tc.name, tc.status)
		got, err := mgr.StopAndReport(tc.name)
		if err != nil {
			t.Fatalf("%s: StopAndReport: %v", tc.name, err)
		}
		if got != tc.stopped {
			t.Errorf("%s (%s): StopAndReport = %v, want %v", tc.name, tc.status, got, tc.stopped)
		}
	}

	if _, err := mgr.StopAndReport("ghost"); err == nil {
		t.Error("StopAndReport on an unregistered name returned nil, want not-found")
	}
}

// TestStopAndReport_IsIdempotentAfterTheFirstCall — the second stop of
// the same live subagent halted nothing, because the first one did.
func TestStopAndReport_IsIdempotentAfterTheFirstCall(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	register(mgr, "cluster", StatusRunning)

	if stopped, err := mgr.StopAndReport("cluster"); err != nil || !stopped {
		t.Fatalf("first stop = (%v, %v), want (true, nil)", stopped, err)
	}
	stopped, err := mgr.StopAndReport("cluster")
	if err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if stopped {
		t.Error("second stop reported true; only one call can have done it")
	}
}

// TestStopSubagent_ReportsTheThreeAnswersTheRouteNeeds covers the seam
// attachadapter hands to the HTTP handler. Before #897 this returned a
// bare bool that meant "the name is registered", so the completed case
// was indistinguishable from a real stop and the route answered 200
// `stopped: true` to both.
func TestStopSubagent_ReportsTheThreeAnswersTheRouteNeeds(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	register(mgr, "cluster", StatusRunning)
	register(mgr, "scribe", StatusCompleted)
	register(mgr, "auditor", StatusFailed)

	cases := []struct {
		name string
		want attach.StopAgentOutcome
	}{
		{"cluster", attach.StopAgentOutcome{Found: true, Stopped: true, Status: "stopped"}},
		{"scribe", attach.StopAgentOutcome{Found: true, Status: "completed"}},
		{"auditor", attach.StopAgentOutcome{Found: true, Status: "failed"}},
		{"ghost", attach.StopAgentOutcome{}},
	}
	for _, tc := range cases {
		got, err := mgr.StopSubagent(tc.name)
		if err != nil {
			t.Fatalf("%s: StopSubagent: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("StopSubagent(%q) = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestStopSubagent_NilManagerIsNotFound — the adapter reaches here with
// a nil manager whenever the agent has no background surface at all,
// and that has to read as "no such subagent", not a 500.
func TestStopSubagent_NilManagerIsNotFound(t *testing.T) {
	t.Parallel()
	var mgr *Manager
	got, err := mgr.StopSubagent("anything")
	if err != nil {
		t.Fatalf("StopSubagent on a nil manager: %v", err)
	}
	if got.Found {
		t.Errorf("got %+v, want a not-found outcome", got)
	}
}

// TestStopTool_SaysWhenTheSubagentHadAlreadyFinished is #1164, the tool
// half of #897. In the #1116 T2 run the parent stopped a reviewer that
// had ended on its max-cost bound twenty seconds earlier, got back a
// bare `"status": "deferred"` against a description promising
// 'stopped', and read it as a stop that had not taken. The tool now
// says which of the two happened, and why an already-finished subagent
// finished.
func TestStopTool_SaysWhenTheSubagentHadAlreadyFinished(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	register(mgr, "live", StatusRunning)
	capped := register(mgr, "capped", StatusDeferred)
	capped.result = &autonomous.RunResult{Reason: autonomous.StopReasonMaxCost}
	register(mgr, "done", StatusCompleted)
	stopTool := NewStopAgentTool(mgr)

	resp := runToolJSON(t, stopTool, context.Background(), map[string]any{"name": "live"})
	if resp["stopped"] != true || resp["status"] != "stopped" {
		t.Errorf("live subagent: got %v, want stopped=true status=stopped", resp)
	}
	if note, _ := resp["note"].(string); note != "" {
		t.Errorf("live subagent: note = %q, want none; the stop did what it says", note)
	}

	resp = runToolJSON(t, stopTool, context.Background(), map[string]any{"name": "capped"})
	if resp["stopped"] != false {
		t.Errorf("capped subagent: stopped = %v, want false — it had already ended on its budget", resp["stopped"])
	}
	if resp["status"] != "deferred" {
		t.Errorf("capped subagent: status = %v, want the status it finished with, deferred", resp["status"])
	}
	note, _ := resp["note"].(string)
	for _, want := range []string{"already finished", "max_cost_exceeded", "nothing was stopped"} {
		if !strings.Contains(note, want) {
			t.Errorf("capped subagent: note = %q, want it to contain %q", note, want)
		}
	}
	if got := errorText(t, resp); got != "" {
		t.Errorf("capped subagent: error = %q, want none — finding it finished is an answer, not a failure", got)
	}

	resp = runToolJSON(t, stopTool, context.Background(), map[string]any{"name": "done"})
	note, _ = resp["note"].(string)
	if resp["stopped"] != false || !strings.Contains(note, "already finished") {
		t.Errorf("completed subagent: got %v, want stopped=false with an already-finished note", resp)
	}

	// The second stop of the same subagent halted nothing: the first did.
	resp = runToolJSON(t, stopTool, context.Background(), map[string]any{"name": "live"})
	if resp["stopped"] != false {
		t.Errorf("second stop: stopped = %v, want false", resp["stopped"])
	}
}

// TestAlreadyFinishedNote_EdgeWording pins the two shapes the handler
// cannot reach in an ordinary test: no handle left to read, where the
// fallback status "stopping" must not be reported as how it finished,
// and a reason that only repeats the status.
func TestAlreadyFinishedNote_EdgeWording(t *testing.T) {
	t.Parallel()

	if got := alreadyFinishedNote("stopping", nil); strings.Contains(got, "'stopping'") {
		t.Errorf("no handle: note = %q, reports a non-terminal status as how it finished", got)
	}

	h := &Handle{result: &autonomous.RunResult{Reason: autonomous.StopReasonCompleted}}
	if got := alreadyFinishedNote(string(autonomous.StopReasonCompleted), h); strings.Contains(got, "(completed)") {
		t.Errorf("reason equal to status: note = %q, repeats it", got)
	}
}
