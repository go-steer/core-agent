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
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent/autonomous"
)

// The banked deliverable and the error that followed it, kept distinct
// so an assertion can tell which field carries which.
const (
	bankedRCA = "RCA: emailservice is OOMKilled; its memory limit was squeezed to 8Mi, previously request 64Mi / limit 128Mi per the last-applied-configuration annotation. Restore the limit."
	lateErr   = "Error 429, Message: Resource exhausted. RESOURCE_EXHAUSTED"
)

// returnThenFailLLM is the #1002 shape as the live run produced it: the
// subagent calls return_result with its findings, the tool handler acks
// it — and the very next model call dies. A Vertex 429 is what did it on
// 2026-09-06; any provider error reaches the driver the same way.
//
// The return and the failure land in the SAME turn on purpose. That is
// what the live capture shows (the ack at seq 1493/1494 and the 429
// immediately after, inside one agent.Run tool loop), and it is the
// case the driver used to lose: runOneTurn's stream-error exit skipped
// the done drain, so the acked result never became a doneSignaled at
// all.
type returnThenFailLLM struct{ calls atomic.Int32 }

func (*returnThenFailLLM) Name() string { return "return-then-fail" }

func (l *returnThenFailLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if l.calls.Add(1) == 1 {
			fc := &genai.FunctionCall{Name: "return_result", Args: map[string]any{"result": bankedRCA}}
			content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
			yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
			return
		}
		yield(nil, errors.New(lateErr))
	}
}

// TestAwaitResult_BankedReturnSurvivesALaterFailure is #1002: a subagent
// that returned a result and was acked has banked a real deliverable,
// and a failure after that must not discard it.
//
// Driven through the real spawn path rather than a hand-built Handle,
// because a hand-built one cannot see the defect. The issue put the
// cause in completionResult, and that is genuinely the last place the
// result is lost — but on a live run it is unreachable, since the
// autonomous driver drops the signal two hops earlier (runOneTurn's
// stream-error exit skips the done drain; Run's turnErr branch returns
// before its doneSignaled check). A fix confined to completionResult
// passes a handle-built test and changes nothing for a real subagent.
//
// The live cost of getting this wrong: the parent read "whatever text
// is here is incidental, not a result", discarded an acked root-cause
// analysis, and re-investigated with seven of its own cluster reads,
// pushing the run to 18 of a 25-call ceiling.
func TestAwaitResult_BankedReturnSurvivesALaterFailure(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &returnThenFailLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		// Standing: this scenario terminates through the return tool,
		// which only a standing worker is offered since #730.
		Mode: ModeStanding,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 4}), WithSyncWaitTimeout(30*time.Second))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "triage emailservice"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	res := mgr.awaitResult(context.Background(), h)

	if res.Status != "failed" {
		t.Errorf("status = %q, want failed — the run did fail, and hiding that would be the opposite error", res.Status)
	}
	if res.Output != bankedRCA {
		t.Errorf("output = %q, want the banked result %q — it was returned and acked before the run died", res.Output, bankedRCA)
	}
	if !strings.Contains(res.RunError, lateErr) {
		t.Errorf("run_error = %q, want it to carry %q — the parent has to learn the run failed after returning", res.RunError, lateErr)
	}
	if res.StopReason != StopReturnedThenFailed {
		t.Errorf("stop_reason = %q, want %q — %q is for a failure with nothing banked, and its guidance tells the parent to throw this away",
			res.StopReason, StopReturnedThenFailed, StopError)
	}
	if strings.Contains(res.Guidance, AbsorbDisclosure) {
		t.Errorf("guidance = %q, want no absorb-disclosure: the delegation DID deliver, so there is nothing for the parent to disclose absorbing", res.Guidance)
	}
	if res.Guidance == "" {
		t.Error("guidance is empty; a non-natural outcome the parent has to reason about needs a line in language (#710)")
	}
}

// failsWithNothingBankedLLM never calls the return tool: its first model
// call dies. This is the case StopError's guidance was written for and
// the case #1002 must not change.
type failsWithNothingBankedLLM struct{}

func (*failsWithNothingBankedLLM) Name() string { return "fails-with-nothing-banked" }

func (*failsWithNothingBankedLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(nil, errors.New(lateErr))
	}
}

// TestAwaitResult_FailureWithNothingBankedIsUnchanged is the other half
// of #1002, and the half that is a regression risk rather than a fix: a
// subagent that died having returned nothing still reports the error as
// its output, still classifies as "error", and still carries the
// absorb-disclosure requirement (#1036). Splitting the classes is only
// correct if the original one keeps meaning what it meant.
func TestAwaitResult_FailureWithNothingBankedIsUnchanged(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &failsWithNothingBankedLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		Mode:         ModeStanding,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 4}), WithSyncWaitTimeout(30*time.Second))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "triage emailservice"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	res := mgr.awaitResult(context.Background(), h)

	if res.StopReason != StopError {
		t.Errorf("stop_reason = %q, want %q — nothing was banked, so this is the unchanged case", res.StopReason, StopError)
	}
	if !strings.Contains(res.Output, lateErr) {
		t.Errorf("output = %q, want the error text: with nothing banked it is the only thing there is to report", res.Output)
	}
	if res.RunError != "" {
		t.Errorf("run_error = %q, want empty — it exists to sit BESIDE a banked result, not to duplicate output", res.RunError)
	}
	if !strings.Contains(res.Guidance, AbsorbDisclosure) {
		t.Errorf("guidance = %q, want the absorb-disclosure requirement (#1036) still attached", res.Guidance)
	}
}

// TestCompletionResult_ATrailedOffDetailIsNotABankedReturn is the
// mirror-image defect, and the reason bankedResult asks for Returned
// rather than only for a non-empty DoneDetail.
//
// Under WithStopOnNaturalEnd the driver writes DoneDetail from a turn
// that merely stopped calling tools, and the lifecycle done tool (the
// non-WithReturnTool branch) signals whatever detail it was handed,
// empty included. Neither is a deliverable anyone asserted. Classifying
// one as returned_then_failed would promise the parent findings that do
// not exist — the same defect as #1002 pointing the other way, and the
// worse direction of the two, since a parent that trusts an absent
// result has nothing to fall back on.
//
// Built from a handle rather than a spawn on purpose: this is about the
// rendering decision, and the state it needs (a failed run carrying a
// DoneDetail with Returned false) is one the driver will not produce.
func TestCompletionResult_ATrailedOffDetailIsNotABankedReturn(t *testing.T) {
	t.Parallel()
	res := completionResult(finished(StatusFailed, &autonomous.RunResult{
		DoneDetail: "let me know if you would like me to continue",
		Returned:   false,
	}, errors.New(lateErr)))

	if res.StopReason != StopError {
		t.Errorf("stop_reason = %q, want %q — nobody returned this, so it is not a banked result", res.StopReason, StopError)
	}
	if res.RunError != "" {
		t.Errorf("run_error = %q, want empty: it exists to sit beside a banked result and there is none", res.RunError)
	}
	if !strings.Contains(res.Output, lateErr) {
		t.Errorf("output = %q, want the error text", res.Output)
	}
}

// TestStopGuidance_ReturnedThenFailedSaysWhatToDoWithTheResult pins the
// guidance's content, not merely its existence.
//
// The entire user-visible value of this stop class is these sentences:
// there is no mechanism behind the enum, and the parent is a language
// model. A later trim to "the subagent returned and then failed" would
// keep every other test green while restoring the behaviour #1002 is
// about, because what changed the parent's mind was being told the
// result is real and what to do with it. Same reasoning as the
// AbsorbDisclosure wording test next door.
func TestStopGuidance_ReturnedThenFailedSaysWhatToDoWithTheResult(t *testing.T) {
	t.Parallel()
	g := stopGuidance(StopReturnedThenFailed)
	for _, want := range []struct{ substr, why string }{
		{"run_error", "names the field carrying what went wrong, so the parent can find it"},
		{"result is real", "the assertion that overturns the previous guidance's 'incidental, not a result'"},
		{"before the failure", "says WHY it is real — it was handed back, not scraped from a dead run"},
		{"re-ask", "tells the parent what to do about the work the failure may have cut"},
	} {
		if !strings.Contains(g, want.substr) {
			t.Errorf("guidance is missing %q — %s\ngot: %q", want.substr, want.why, g)
		}
	}
	// The opposite instruction, which is what this class exists to stop
	// the parent being given.
	if strings.Contains(g, "incidental") {
		t.Errorf("guidance tells the parent the text is incidental; that is StopError's line and it is what discarded the result:\n%q", g)
	}
}

// TestTerminalAlertText_BankedReturnSurvivesALaterFailure is the async
// twin. A wait: true whose wait times out is delivered as a
// [Background reports] alert instead, so a fix that only reaches the
// sync rendering is unreachable exactly when the subagent ran long
// enough to be worth waiting for — the #691 rule.
func TestTerminalAlertText_BankedReturnSurvivesALaterFailure(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &returnThenFailLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		Mode:         ModeStanding,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 4}))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "triage emailservice"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	waitDone(t, h)

	var found bool
	for _, a := range drainAlerts(mgr, 2*time.Second) {
		if a.From != h.Name {
			continue
		}
		found = true
		if !strings.Contains(a.Text, bankedRCA) {
			t.Errorf("alert text = %q, want it to carry the banked result %q", a.Text, bankedRCA)
		}
		// Once, not twice. An alert is one text blob, so the banked
		// result leads it AND the generic "carry DoneDetail too" append
		// below would add it again — the dedup guard there is what
		// stops that, and a Contains-only assertion cannot see a
		// duplicated payload.
		if n := strings.Count(a.Text, bankedRCA); n != 1 {
			t.Errorf("alert repeats the banked result %d times; want once:\n%s", n, a.Text)
		}
		if !strings.Contains(a.Text, lateErr) {
			t.Errorf("alert text = %q, want it to carry the run error %q", a.Text, lateErr)
		}
		if !strings.Contains(a.Text, "stop_reason: "+string(StopReturnedThenFailed)) {
			t.Errorf("alert text = %q, want the %q trailer", a.Text, StopReturnedThenFailed)
		}
		if strings.Contains(a.Text, AbsorbDisclosure) {
			t.Errorf("alert text = %q, want no absorb-disclosure: the delegation delivered", a.Text)
		}
	}
	if !found {
		t.Fatal("no terminal alert for the subagent; the async path delivers this outcome and must carry the result")
	}
}
