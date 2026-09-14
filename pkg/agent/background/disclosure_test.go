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

// #1036: a delegation can fail, the parent can quietly do the work
// itself, and the run can score full marks with the operator never
// told. Four runs in the 38-run GKE drill archive did exactly that and
// one of them disclosed nothing. These pin the sentence that makes the
// disclosure a requirement rather than a mood, on every surface a
// parent can meet a delegation that delivered nothing.

package background

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/agent/autonomous"
)

// Which classes carry the requirement is the whole design decision, so
// it is asserted in both directions. A caveat on every outcome is a
// caveat on none, which is why the partials and the deferral are listed
// as explicit negatives rather than simply left out.
func TestStopGuidance_OnlyUnusableOutcomesDemandDisclosure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		class StopClass
		want  bool
		why   string
	}{
		{StopError, true, "the measured case: the subagent died and left an error string, so the parent has nothing but its own reads"},
		{StopNoReturn, true, "its own guidance ends \"or do the work yourself\" — the same invitation, visible in the source instead of the archive"},
		{StopMaxSteps, false, "a partial is real delegated work; finishing it is not a run that stopped being delegated"},
		{StopBudget, false, "same as max_steps"},
		{StopDeferred, false, "not over, so absorbing is the wrong move and the guidance says so instead"},
		{StopStopped, false, "the parent asked for the stop, so it already knows"},
		{StopNatural, false, "nothing failed"},
	} {
		t.Run(string(tc.class), func(t *testing.T) {
			t.Parallel()
			got := strings.Contains(stopGuidance(tc.class), AbsorbDisclosure)
			if got != tc.want {
				t.Errorf("guidance for %q carries the disclosure requirement = %v, want %v — %s\ngot: %q",
					tc.class, got, tc.want, tc.why, stopGuidance(tc.class))
			}
		})
	}
}

// The requirement has to survive the trip through the real sync result
// builder, not just exist as a string in a switch. This is the shape of
// the four archived runs: a child killed by a 429 after one call.
func TestCompletionResult_FailedDelegationDemandsDisclosure(t *testing.T) {
	t.Parallel()
	res := completionResult(finished(StatusFailed, &autonomous.RunResult{
		Calls: childCalls[:1],
	}, errors.New("Error 429, Status: RESOURCE_EXHAUSTED")))

	if res.StopReason != StopError {
		t.Fatalf("stop_reason = %q, want %q", res.StopReason, StopError)
	}
	if !strings.Contains(res.Guidance, AbsorbDisclosure) {
		t.Errorf("guidance = %q, want it to require disclosure if the parent absorbs the work", res.Guidance)
	}
	// The disclosure must not have cost the parent the rest of the
	// result: the error and the one call the child managed are still
	// the most useful things on this path (#1014).
	if res.Output == "" || len(res.Calls) != 1 {
		t.Errorf("output=%q calls=%d — the disclosure displaced the payload", res.Output, len(res.Calls))
	}
}

// A refusal never launched anything at all, so absorbing the work is
// the single most likely next move — and it is the one path with no
// stop class to hang the requirement off.
func TestRefusedSpawn_DemandsDisclosure(t *testing.T) {
	t.Parallel()
	res := refusedSpawn("cluster", errors.New("concurrency cap reached"))

	if !strings.Contains(res.Guidance, AbsorbDisclosure) {
		t.Errorf("guidance = %q, want the disclosure requirement on a spawn that never happened", res.Guidance)
	}
	// #746: a refusal must still read as a refusal everywhere else.
	if res.Error == "" || !strings.HasPrefix(res.Status, "error: ") {
		t.Errorf("refusal lost its error affordance: status=%q error=%q", res.Status, res.Error)
	}
}

// The async twin. A fire-and-continue delegation, or a wait that timed
// out, is delivered as a [Background reports] alert instead — which
// carries no `guidance` field at all, so without this the parent would
// meet a dead delegation as an error string and an enum, the exact
// shape #710 established does not change a model's behaviour.
func TestTerminalAlertText_FailedDelegationDemandsDisclosure(t *testing.T) {
	t.Parallel()
	_, text := terminalAlertText(StatusFailed, autonomous.RunResult{
		FinalText: "got as far as the Secret",
	}, errors.New("Error 429, Status: RESOURCE_EXHAUSTED"))

	if !strings.Contains(text, AbsorbDisclosure) {
		t.Errorf("alert = %q, want the disclosure requirement", text)
	}
	if !strings.Contains(text, "got as far as the Secret") {
		t.Errorf("alert = %q, want the child's findings kept alongside it", text)
	}
}

// And the contrast on the same surface: a deferral is not a failed
// delegation, and annotating it would make the annotation background
// noise on the alerts that matter.
func TestTerminalAlertText_DeferralDoesNotDemandDisclosure(t *testing.T) {
	t.Parallel()
	_, text := terminalAlertText(StatusDeferred, autonomous.RunResult{
		Reason:    autonomous.StopReasonMaxTurns,
		FinalText: "got as far as the Secret",
	}, nil)

	if strings.Contains(text, AbsorbDisclosure) {
		t.Errorf("alert = %q, want no disclosure requirement on a partial", text)
	}
}

// The sentence has to be readable as an instruction by a language
// model, which means it has to name the two things the operator needs:
// that the parent did the work, and which delegation failed. Pinned
// because the value of this change is entirely in the wording — there
// is no mechanism behind it — and a later edit that trims it to
// "disclose the failure" would keep every other test green.
func TestAbsorbDisclosure_SaysWhatToDoAndWhy(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"yourself",     // the condition it applies to
		"final answer", // where the disclosure has to appear
		"name the",     // what to say
		"went wrong",   // and the other half of what to say
		"operator",     // who it is for
	} {
		if !strings.Contains(AbsorbDisclosure, want) {
			t.Errorf("disclosure sentence does not mention %q: %q", want, AbsorbDisclosure)
		}
	}
}
