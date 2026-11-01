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

package approval

import (
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/genai"
)

func producerSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"detail": {Type: genai.TypeString},
			ChangeSetField: {Type: genai.TypeArray, Items: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"tool":      {Type: genai.TypeString},
					"arguments": {Type: genai.TypeString},
				},
				Required: []string{"tool", "arguments"},
			}},
		},
		Required: []string{"detail"},
	}
}

func producerGate(t *testing.T, checker *ChangeSetChecker) llmagent.BeforeToolCallback {
	t.Helper()
	g, err := newWriteGate(Config{
		Policy:    OnMutationApply,
		Mutating:  func(string) bool { return false },
		ChangeSet: checker,
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("newWriteGate: %v", err)
	}
	return g.beforeTool
}

func report(changes ...map[string]any) map[string]any {
	list := make([]any, 0, len(changes))
	for _, c := range changes {
		list = append(list, c)
	}
	return map[string]any{"detail": "api is OOMKilled", ChangeSetField: list}
}

func change(tool, args string) map[string]any {
	return map[string]any{"tool": tool, "arguments": args}
}

func recordedChangeSet(t *testing.T, p *reportProbe, specialist string) []ProposedChange {
	t.Helper()
	state := sessionState(t, p.svc)
	v, ok := state[ChangeSetStateKey(specialist)]
	if !ok {
		return nil
	}
	got, err := DecodeChangeSet(v)
	if err != nil {
		t.Fatalf("DecodeChangeSet: %v", err)
	}
	return got
}

func TestProducerContractRecordsAnExecutableChange(t *testing.T) {
	c := catalog()
	p := runReportProbe(t, producerSchema(),
		report(change("scale_deployment", `{"deployment":"api","replicas":2}`)),
		producerGate(t, &c))

	if len(p.responses) != 1 {
		t.Fatalf("finish_task responses = %v, want 1", p.responses)
	}
	if _, refused := p.responses[0]["error"]; refused {
		t.Fatalf("an executable change was refused: %v", p.responses[0])
	}

	got := recordedChangeSet(t, p, "reporter")
	if len(got) != 1 {
		t.Fatalf("recorded change set = %+v, want the one entry the specialist proposed", got)
	}
	sig, err := got[0].Signature()
	if err != nil {
		t.Fatalf("Signature: %v", err)
	}
	if sig != `scale_deployment({"deployment":"api","replicas":2})` {
		t.Errorf("recorded signature = %s", sig)
	}
}

func TestProducerContractRefusesAnInventedTool(t *testing.T) {
	c := catalog()
	p := runReportProbe(t, producerSchema(),
		report(change("kubectl_scale", `{"deployment":"api"}`)),
		producerGate(t, &c))

	if len(p.responses) == 0 {
		t.Fatal("no finish_task response at all")
	}
	if p.responses[0]["error"] != "invalid_proposed_change" {
		t.Fatalf("the model was told %v, want the producer contract's refusal", p.responses[0])
	}
	detail, _ := p.responses[0]["detail"].(string)
	for _, want := range []string{"kubectl_scale", "tool_catalog", "empty list"} {
		if !strings.Contains(detail, want) {
			t.Errorf("refusal does not mention %q: %s", want, detail)
		}
	}
	if p.rounds < 2 {
		t.Errorf("model was asked %d time(s); a refused report must come back for another turn", p.rounds)
	}
	if got := recordedChangeSet(t, p, "reporter"); got != nil {
		t.Errorf("a refused change set was recorded anyway: %+v", got)
	}
}

func TestProducerContractRefusesArgumentsTheToolWouldReject(t *testing.T) {
	c := catalog()
	p := runReportProbe(t, producerSchema(),
		report(change("scale_deployment", `{"deployment":"api","replicas":"two"}`)),
		producerGate(t, &c))

	if len(p.responses) == 0 {
		t.Fatal("no finish_task response at all")
	}
	if p.responses[0]["error"] != "invalid_proposed_change" {
		t.Fatalf("the model was told %v, want the producer contract's refusal", p.responses[0])
	}
	if got := recordedChangeSet(t, p, "reporter"); got != nil {
		t.Errorf("a change set with unrunnable arguments was recorded: %+v", got)
	}
}

func TestProducerContractAcceptsAnEmptyChangeSet(t *testing.T) {
	c := catalog()
	p := runReportProbe(t, producerSchema(), report(), producerGate(t, &c))

	if len(p.responses) != 1 {
		t.Fatalf("finish_task responses = %v, want 1", p.responses)
	}
	if _, refused := p.responses[0]["error"]; refused {
		t.Fatalf("an empty change set was refused: %v", p.responses[0])
	}
	if p.rounds != 1 {
		t.Errorf("model was asked %d time(s), want 1", p.rounds)
	}
	if got := recordedChangeSet(t, p, "reporter"); got != nil {
		t.Errorf("an empty change set was recorded as a proposal: %+v", got)
	}
}

func TestNoCheckerLeavesReportsAlone(t *testing.T) {
	p := runReportProbe(t, producerSchema(),
		report(change("kubectl_scale", `{"deployment":"api"}`)),
		producerGate(t, nil))

	if len(p.responses) != 1 {
		t.Fatalf("finish_task responses = %v, want 1", p.responses)
	}
	if _, refused := p.responses[0]["error"]; refused {
		t.Fatalf("a gate with no checker refused a report: %v", p.responses[0])
	}
	if got := recordedChangeSet(t, p, "reporter"); got != nil {
		t.Errorf("a gate with no checker recorded %+v", got)
	}
}

func TestProducerContractIgnoresReportsWithNoChangeSet(t *testing.T) {
	c := catalog()
	p := runReportProbe(t, &genai.Schema{
		Type:       genai.TypeObject,
		Properties: map[string]*genai.Schema{"detail": {Type: genai.TypeString}},
		Required:   []string{"detail"},
	}, map[string]any{"detail": "api is OOMKilled"}, producerGate(t, &c))

	if len(p.responses) != 1 {
		t.Fatalf("finish_task responses = %v, want 1", p.responses)
	}
	if _, refused := p.responses[0]["error"]; refused {
		t.Fatalf("a report with no change set was refused: %v", p.responses[0])
	}
}
