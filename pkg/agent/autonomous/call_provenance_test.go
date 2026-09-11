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
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent"
)

type probeIn struct {
	Namespace string `json:"namespace"`
}

type probeOut struct {
	Phase string `json:"phase"`
}

// probeCallTurn yields a call to the "probe" tool. It stands in for the
// reads a GKE subagent makes on its parent's behalf — the ones the
// parent then re-issued because it had no other way to cite them.
func probeCallTurn(ns string) scenarioFn {
	return func(_ context.Context, _ *adkmodel.LLMRequest) []stubResp {
		fc := &genai.FunctionCall{Name: "probe", Args: map[string]any{"namespace": ns}}
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
		return []stubResp{
			{resp: &adkmodel.LLMResponse{
				Content:      content,
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			}},
		}
	}
}

// buildProbeAgent wires the stub LLM with a "probe" tool that fails for
// the "boom" namespace and succeeds everywhere else.
func buildProbeAgent(llm *stubLLM, name string) func([]tool.Tool) (*agent.Agent, error) {
	return func(extras []tool.Tool) (*agent.Agent, error) {
		probe, err := functiontool.New(
			functiontool.Config{Name: "probe", Description: "look at a namespace"},
			func(_ tool.Context, in probeIn) (probeOut, error) {
				if in.Namespace == "boom" {
					return probeOut{}, errors.New("forbidden: no list access")
				}
				return probeOut{Phase: "Pending"}, nil
			})
		if err != nil {
			return nil, err
		}
		return agent.New(llm,
			agent.WithName(name),
			agent.WithSession("u-test", "s-test-"+name),
			agent.WithTools(append(append([]tool.Tool(nil), extras...), probe)),
			agent.WithInstruction("test agent; probe, then call report_done."),
		)
	}
}

// TestRunAutonomous_CallsRollUpAcrossTurnsWithoutTheDriverTools is the
// run-level half of #1014. A delegation's provenance is the whole run's
// calls, not the last turn's — a subagent that reads in turn 1 and
// concludes in turn 2 must still hand its parent the turn-1 read — and
// the driver's own control tools must stay out of it, the done tool
// most of all: its argument IS the result's `output`, so recording it
// would ship the same prose twice in one payload.
func TestRunAutonomous_CallsRollUpAcrossTurnsWithoutTheDriverTools(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		// Turn 1: one read, then a plain text answer ends the turn.
		probeCallTurn("shop"),
		textTurn("shop looks unhappy", 5, 3),
		// Turn 2: a second read, then the done tool.
		probeCallTurn("cart"),
		doneCallTurn("shop is unschedulable"),
		textTurn("all done", 5, 3),
	}}
	res, err := Run(context.Background(), buildProbeAgent(llm, "prov"), "look around", WithMaxTurns(4))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Turns != 2 {
		t.Fatalf("Turns = %d, want 2", res.Turns)
	}

	if len(res.Calls) != 2 {
		t.Fatalf("Calls = %+v, want the two probes across two turns", res.Calls)
	}
	for i, ns := range []string{"shop", "cart"} {
		if res.Calls[i].Tool != "probe" {
			t.Errorf("Calls[%d].Tool = %q, want %q", i, res.Calls[i].Tool, "probe")
		}
		if got := res.Calls[i].Args["namespace"]; got != ns {
			t.Errorf("Calls[%d] namespace = %v, want %q", i, got, ns)
		}
		if res.Calls[i].Error != "" {
			t.Errorf("Calls[%d] recorded an error on a call that succeeded: %q", i, res.Calls[i].Error)
		}
	}
	if res.CallsDropped != 0 {
		t.Errorf("CallsDropped = %d, want 0", res.CallsDropped)
	}
	for _, c := range res.Calls {
		if c.Tool == "report_done" {
			t.Error("the done tool landed in the provenance record; its detail is already the result's output")
		}
	}
}

// TestRunAutonomous_AFailedCallIsRecordedAsFailed. A citation to a call
// that failed is worse than no citation — it reads as grounding while
// grounding nothing — so the outcome has to travel with the call.
func TestRunAutonomous_AFailedCallIsRecordedAsFailed(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		probeCallTurn("boom"),
		doneCallTurn("could not look"),
		textTurn("all done", 5, 3),
	}}
	res, err := Run(context.Background(), buildProbeAgent(llm, "prov-fail"), "look around", WithMaxTurns(2))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Calls) != 1 {
		t.Fatalf("Calls = %+v, want the one failed probe", res.Calls)
	}
	if !strings.Contains(res.Calls[0].Error, "forbidden") {
		t.Errorf("Calls[0].Error = %q, want the tool's failure text", res.Calls[0].Error)
	}
}

// TestRunAutonomous_ATurnThatEndsInErrorStillReportsItsCalls pins the
// defer in runOneTurn. The function has several exits and a run that
// died is precisely where "what did it manage to observe" matters, so
// the harvest cannot live at the happy-path return.
func TestRunAutonomous_ATurnThatEndsInErrorStillReportsItsCalls(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		probeCallTurn("shop"),
		errTurn(errors.New("429 resource exhausted")),
	}}
	res, err := Run(context.Background(), buildProbeAgent(llm, "prov-err"), "look around", WithMaxTurns(2))
	if err == nil {
		t.Fatal("Run returned nil error, want the transport failure")
	}
	if len(res.Calls) != 1 || res.Calls[0].Tool != "probe" {
		t.Errorf("Calls = %+v, want the probe made before the turn died", res.Calls)
	}
}
