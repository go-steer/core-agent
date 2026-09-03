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

// The wire between #906 and #907 (#918). #906 gave recordPlanResult an
// Outcome field and documented it as "what a behavioral detector can key
// on"; #907 shipped that detector, and it keys on exactly one reserved
// response key, `no_op`, which recordPlanResult did not set. Both halves
// read correctly in isolation and the seam between them carried nothing.
//
// These tests pin the tools-side half: which outcome is inert, and what
// that looks like on the wire. The agent-side half — that the key the
// struct tag spells is the key the runtime reads, and that three repeats
// actually trip the signal — is pinned by
// pkg/agent/record_plan_noop_wire_test.go, because pkg/tools cannot
// import pkg/agent.

package tools

import (
	"encoding/json"
	"testing"
)

// Only planOutcomeUnchanged is inert. planOutcomeUpdated overwrites the
// artifact, which is work: reporting it as a no-op would let an agent
// genuinely revising its plan across three calls accumulate a streak
// toward a Critical halt, and revising in place is the behaviour #906
// deliberately steered models toward.
func TestRecordPlan_OnlyTheUnchangedOutcomeIsANoOp(t *testing.T) {
	t.Parallel()
	d := newPlanDriver(t, armedGate())
	const first = "## Goal\nRestart the deployment."
	const revised = first + "\n\nThen drain the node."

	recorded := d.call("core_agent", "s1", "inv-1", first)
	updated := d.call("core_agent", "s1", "inv-1", revised)
	unchanged := d.call("core_agent", "s1", "inv-1", revised)

	for _, tc := range []struct {
		label string
		res   recordPlanResult
		want  string
		noOp  bool
	}{
		{"first call", recorded, planOutcomeRecorded, false},
		{"revision", updated, planOutcomeUpdated, false},
		{"repeat", unchanged, planOutcomeUnchanged, true},
	} {
		if tc.res.Outcome != tc.want {
			t.Errorf("%s: outcome = %q, want %q", tc.label, tc.res.Outcome, tc.want)
		}
		if tc.res.NoOp != tc.noOp {
			t.Errorf("%s: NoOp = %v, want %v (outcome %q)", tc.label, tc.res.NoOp, tc.noOp, tc.res.Outcome)
		}
	}
}

// The detector reads a JSON key, not a Go field, so the tag is the
// contract. `omitempty` keeps a productive call's response free of a
// false-y marker rather than shipping `"no_op": false` on every write —
// toolResponseNoOp reads absent and false the same way, but the model
// sees this response too and a field that is always present reads as a
// property of the tool rather than a claim about this call.
func TestRecordPlan_NoOpMarshalsUnderTheReservedKey(t *testing.T) {
	t.Parallel()

	inert, err := json.Marshal(recordPlanResult{Outcome: planOutcomeUnchanged, NoOp: true})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(inert, &got); err != nil {
		t.Fatal(err)
	}
	if got["no_op"] != true {
		t.Errorf(`marshalled inert result = %s, want "no_op": true`, inert)
	}

	productive, err := json.Marshal(recordPlanResult{Outcome: planOutcomeRecorded})
	if err != nil {
		t.Fatal(err)
	}
	var got2 map[string]any
	if err := json.Unmarshal(productive, &got2); err != nil {
		t.Fatal(err)
	}
	if _, present := got2["no_op"]; present {
		t.Errorf("marshalled productive result = %s, want no no_op key at all", productive)
	}
}
