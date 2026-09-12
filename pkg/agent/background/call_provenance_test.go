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
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/agent/autonomous"
	"github.com/go-steer/core-agent/v2/pkg/agent/internal/toolcalls"
)

// finished builds the handle a terminal spawn leaves behind. The fields
// are set directly rather than driven through a run because what is
// under test is the rendering: given a run that made these calls, does
// the parent get told about them.
func finished(status Status, r *autonomous.RunResult, runErr error) *Handle {
	h := &Handle{Name: "cluster", Branch: "b1", status: status, result: r, err: runErr, done: make(chan struct{})}
	close(h.done)
	return h
}

var childCalls = []toolcalls.Call{
	{Tool: "kubectl_get", Args: map[string]any{"resource": "pods", "namespace": "shop"}},
	{Tool: "read_file", Args: map[string]any{"path": "/etc/config.yaml"}},
}

// TestCompletionResultCarriesTheCallsTheChildMade is the #1014 fix at
// the spawn_agent door: in 4 of 6 GKE drill OOMKill runs the parent
// repeated a read its subagent had already done, because prose cannot
// be cited and the result carried nothing else. It carries something
// else now, and the figure is 0 of 11 (#1034).
func TestCompletionResultCarriesTheCallsTheChildMade(t *testing.T) {
	t.Parallel()
	res := completionResult(finished(StatusCompleted, &autonomous.RunResult{
		DoneDetail: "the shop deployment is unschedulable",
		Returned:   true,
		Calls:      childCalls,
	}, nil))

	if len(res.Calls) != 2 || res.Calls[0].Tool != "kubectl_get" {
		t.Fatalf("calls = %+v, want the two the child made", res.Calls)
	}
	if res.CallsNote == "" {
		t.Error("calls arrived with no note; #710's lesson is that the array alone does not change what the parent does with it")
	}
	if !strings.Contains(res.CallsNote, "Cite them") {
		t.Errorf("note does not tell the parent it may cite the calls: %q", res.CallsNote)
	}
	if res.Output == "" {
		t.Error("provenance displaced the findings; it is meant to travel beside them")
	}
}

// TestAFailedRunStillCarriesItsCalls pins the assignment's position
// ahead of the runErr early return. A delegation that died partway
// through is exactly where the parent most needs to know what the child
// managed to observe — the 2026-09-11 drill run lost a subagent to a
// 429 after one call, and that call was the only citable thing about it.
func TestAFailedRunStillCarriesItsCalls(t *testing.T) {
	t.Parallel()
	res := completionResult(finished(StatusFailed, &autonomous.RunResult{
		Calls: childCalls[:1],
	}, errors.New("429 resource exhausted")))

	if len(res.Calls) != 1 {
		t.Fatalf("a failed run reported %d calls, want the 1 it made before dying", len(res.Calls))
	}
	if res.Output != "429 resource exhausted" {
		t.Errorf("output = %q, want the run error", res.Output)
	}
	if res.CallsNote == "" {
		t.Error("failed run carried calls with no note")
	}
}

// TestTruncationIsAdmittedToTheParent: a silently short list is worse
// than no list, because the parent reads absence as "it did not happen"
// and stops looking.
func TestTruncationIsAdmittedToTheParent(t *testing.T) {
	t.Parallel()
	res := completionResult(finished(StatusCompleted, &autonomous.RunResult{
		DoneDetail:   "done",
		Returned:     true,
		Calls:        childCalls,
		CallsDropped: 5,
	}, nil))

	if res.CallsTruncated != 5 {
		t.Errorf("calls_truncated = %d, want 5", res.CallsTruncated)
	}
	if !strings.Contains(res.CallsNote, "does not mean it did not happen") {
		t.Errorf("note hides the truncation: %q", res.CallsNote)
	}
}

// TestASilentChildAddsNoFields keeps the result shape unchanged for the
// delegations this work does not touch: a subagent that called no tools
// should serialize exactly as it did before, not grow an empty array
// and an explanatory sentence about it.
func TestASilentChildAddsNoFields(t *testing.T) {
	t.Parallel()
	res := completionResult(finished(StatusCompleted, &autonomous.RunResult{
		DoneDetail: "nothing to do",
		Returned:   true,
	}, nil))

	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{"calls", "calls_note", "calls_truncated"} {
		if strings.Contains(string(raw), `"`+field+`"`) {
			t.Errorf("a child that called nothing still emitted %q: %s", field, raw)
		}
	}
}

// TestTheDescriptionTellsTheParentTheListExists. A field the parent is
// never told about is a field it does not use: the model decides
// whether to re-read while reading the tool description, not while
// reading the result schema.
func TestTheDescriptionTellsTheParentTheListExists(t *testing.T) {
	t.Parallel()
	if !strings.Contains(spawnAgentDescription, "'calls' list") {
		t.Error("spawn_agent's description never mentions the calls list")
	}
	if !strings.Contains(spawnAgentDescription, "you do not need to repeat a read your subagent already did") {
		t.Error("the description advertises the list without saying what it is for")
	}
}
