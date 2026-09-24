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

package autonomous

import (
	"context"
	"errors"
	"testing"
)

// The driver half of #1002. pkg/agent/background renders what the parent
// receives; these pin the two hops that decide whether there is anything
// left to render by the time it gets there.
//
// Both are about the same fact: a return the model already made, and
// whose tool handler already acked, is not undone by the run failing
// afterwards. RunResult is the boundary where that fact either survives
// or does not.

// A return and a failure in the SAME turn. This is the live #1002 shape
// — the ack and the 429 are one agent.Run tool loop apart — and it is
// the hop inside runOneTurn: the stream error returns from the middle of
// the event loop, which used to jump past the done drain at the bottom
// of the function, so the acked result never became a done signal at all.
func TestRunAutonomous_ReturnAckedInTheTurnThatThenFails(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("Error 429, Message: Resource exhausted")
	llm := &stubLLM{scenarios: []scenarioFn{
		// The done call, and then the failure on the follow-up model
		// call the ADK tool loop issues once the tool has run. Both
		// live inside ONE runOneTurn turn, which is the live capture's
		// shape: the ack and the 429 are one tool loop apart.
		doneCallTurn("the emailservice limit was squeezed to 8Mi"),
		errTurn(wantErr),
	}}
	res, err := Run(context.Background(), buildAgent(llm, "banked-same-turn"), "go")
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v — the run did fail and the caller has to learn that", err, wantErr)
	}
	if !res.Returned {
		t.Error("Returned = false; the model called the done tool and was acked, which the failure afterwards does not undo")
	}
	if res.DoneDetail != "the emailservice limit was squeezed to 8Mi" {
		t.Errorf("DoneDetail = %q, want the banked result — a failure after the return must not discard it", res.DoneDetail)
	}
}

// The second exit out of the same branch: a retry policy that keeps the
// run going, and a run that then dies anyway. The banked return is kept
// rather than consumed at the point it is observed, so the copy taken on
// the first failed attempt is still there when a later attempt fails
// with nothing of its own to hand back.
func TestRunAutonomous_BankedReturnSurvivesAFailedRetry(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("Error 429, Message: Resource exhausted")
	var attempts int
	llm := &stubLLM{scenarios: []scenarioFn{
		doneCallTurn("the emailservice limit was squeezed to 8Mi"),
		errTurn(wantErr),
		// The retry: no return of its own, and it dies too.
		errTurn(wantErr),
	}}
	policy := func(_ error, _ int) RetryDecision {
		attempts++
		if attempts == 1 {
			return RetryTurn
		}
		return AbortRun
	}
	res, err := Run(context.Background(), buildAgent(llm, "banked-then-retry"), "go", WithRetryPolicy(policy))
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
	if !res.Returned || res.DoneDetail != "the emailservice limit was squeezed to 8Mi" {
		t.Errorf("Returned = %v, DoneDetail = %q; want the first attempt's banked result to survive the retry that failed with nothing of its own",
			res.Returned, res.DoneDetail)
	}
}

// A banked return must not survive into an outcome it did not produce.
// This is the trap the bank creates: bankReturn deliberately keeps its
// copy across a retry, and a natural end writes DoneDetail from the
// turn's trailing text. If Returned stayed true from the earlier failed
// attempt, the pair would describe two different turns and the parent
// would read stop_reason "natural" — an assertion that the goal was met
// — over whatever the model happened to trail off with. That is #710
// arriving through the door #1002 opened.
func TestRunAutonomous_NaturalEndAfterABankedReturnIsNotAReturn(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		// Turn 1: a real return, then the turn dies.
		returnCallTurn(DefaultReturnToolName, "turn-1 partial finding"),
		errTurn(errors.New("Error 429, Message: Resource exhausted")),
		// The retry: a text-only turn, i.e. a natural end.
		textTurn("standing by, nothing further", 0, 0),
	}}
	var attempts int
	policy := func(_ error, _ int) RetryDecision {
		attempts++
		if attempts == 1 {
			return RetryTurn
		}
		return AbortRun
	}
	// Both terminations wired, which is what a bounded delegation gets:
	// pkg/agent/background always passes WithReturnTool and adds
	// WithStopOnNaturalEnd for bounded specs. WithStopOnNaturalEnd alone
	// registers NO done tool at all (buildDoneTools returns nil), so a
	// version of this test without the return tool would score a return
	// that never happened — vacuously green whatever the driver does.
	res, err := Run(context.Background(), buildAgent(llm, "natural-after-bank"), "go",
		WithRetryPolicy(policy), WithStopOnNaturalEnd(),
		WithReturnTool(ReturnToolConfig{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != StopReasonCompleted {
		t.Fatalf("Reason = %q, want completed", res.Reason)
	}
	if res.DoneDetail != "standing by, nothing further" {
		t.Errorf("DoneDetail = %q, want the natural end's text", res.DoneDetail)
	}
	if res.Returned {
		t.Error("Returned = true on a natural end; DoneDetail here is the turn's trailing text, not a return, and calling it one reports a non-answer to the parent as a finished result (#710)")
	}
}

// A failure with nothing banked stays exactly as it was: no result, and
// Returned false. Splitting an outcome is only safe if the outcome it
// was split from keeps its meaning — pkg/agent/background classifies on
// this field, and a Returned that drifted true would hand a parent a
// deliverable that does not exist.
func TestRunAutonomous_FailureWithNoReturnBanksNothing(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("Error 429, Message: Resource exhausted")
	llm := &stubLLM{scenarios: []scenarioFn{errTurn(wantErr)}}
	res, err := Run(context.Background(), buildAgent(llm, "nothing-banked"), "go")
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
	if res.Returned {
		t.Error("Returned = true on a run that never called the done tool")
	}
	if res.DoneDetail != "" {
		t.Errorf("DoneDetail = %q, want empty", res.DoneDetail)
	}
}
