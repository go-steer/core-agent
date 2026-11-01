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

package judge

import (
	"context"
	"iter"
	"sort"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/core-agent/v3/internal/evals"
	"github.com/go-steer/core-agent/v3/internal/modelretry"
)

// scriptedModel answers a fixed sequence of turns. It is not a stand-in
// for the real model — the tier's whole point is that a scripted model
// cannot choose — but it lets the plumbing (tool wiring, session
// recording, trace extraction, scoring) run in CI without credentials.
type scriptedModel struct {
	mu    sync.Mutex
	turns []func(req *adkmodel.LLMRequest) *adkmodel.LLMResponse
	at    int
}

func (m *scriptedModel) Name() string { return "judge-test-script" }

func (m *scriptedModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		i := m.at
		m.at++
		m.mu.Unlock()
		if i >= len(m.turns) {
			// Falling off the script means the composed shape asked for
			// more turns than the fixture expects. Ending the turn is
			// wrong-but-quiet; the test asserts on what was recorded.
			yield(&adkmodel.LLMResponse{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText("script exhausted")}},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			}, nil)
			return
		}
		yield(m.turns[i](req), nil)
	}
}

func call(name string, args map[string]any) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromFunctionCall(name, args)}},
		TurnComplete: true,
		FinishReason: genai.FinishReasonStop,
	}
}

// TestRig_RunsAScenarioEndToEnd is the offline plumbing check: a
// scripted model reads one tool and reports, and the rig must come back
// with the tool call recorded, the report as final text, and the
// deterministic metrics scored.
func TestRig_RunsAScenarioEndToEnd(t *testing.T) {
	ds := loadCorpus(t)
	tbl := loadIntents(t)
	fx, err := Fixtures(ds, loadOverrides(t))
	if err != nil {
		t.Fatalf("Fixtures: %v", err)
	}

	var sc evals.Scenario
	for _, s := range ds.Scenarios {
		if s.ID == "LC-01-crashloopbackoff" {
			sc = s
		}
	}
	if sc.ID == "" {
		t.Fatal("LC-01 not in the corpus")
	}

	const report = "CRITICAL: api-server-7d8f9c-xkp2v in production is crashlooping because " +
		"the Secret api-server-secrets has no DATABASE_URL key. Recommended action: add the key."

	var sawReading string
	m := &scriptedModel{turns: []func(*adkmodel.LLMRequest) *adkmodel.LLMResponse{
		func(*adkmodel.LLMRequest) *adkmodel.LLMResponse {
			return call("k8s_triage_workload", map[string]any{"scope": "production/api-server"})
		},
		func(req *adkmodel.LLMRequest) *adkmodel.LLMResponse {
			// Capture what the tool actually returned to the model, so
			// the test can assert the fixture reached it.
			for _, c := range req.Contents {
				for _, p := range c.Parts {
					if p.FunctionResponse != nil {
						if r, ok := p.FunctionResponse.Response["reading"].(string); ok {
							sawReading = r
						}
					}
				}
			}
			// Chat mode: the agent's answer is model text, not a
			// finish_task argument.
			return &adkmodel.LLMResponse{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText(report)}},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			}
		},
	}}

	rig, err := NewRig(tbl, fx, m, t.TempDir())
	if err != nil {
		t.Fatalf("NewRig: %v", err)
	}
	out, err := rig.Run(context.Background(), sc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(out.Tools) != 1 || out.Tools[0] != "k8s_triage_workload" {
		t.Errorf("tools called = %v, want one k8s_triage_workload", out.Tools)
	}
	if !strings.Contains(sawReading, "DATABASE_URL not set") {
		t.Errorf("the fixture reading did not reach the model:\n%s", sawReading)
	}
	if !strings.Contains(out.Response, "CRITICAL") {
		t.Errorf("final response did not carry the report: %q", out.Response)
	}

	got := map[string]float64{}
	for _, res := range out.Results {
		got[res.Metric] = res.Score
	}
	if got[evals.MetricSeverityAccuracy] != 1 {
		t.Errorf("severity_accuracy = %v, want 1 (report and ground truth are both CRITICAL): %v",
			got[evals.MetricSeverityAccuracy], out.summarize())
	}
	if got[evals.MetricIntentCoverage] == 0 {
		t.Errorf("intent_coverage = 0 despite a triage call: %v", out.summarize())
	}
	t.Logf("%s: %s (ceiling %.2f)", out.ID, out.summarize(), out.Ceiling)
}

// TestNewRigRefusesANilWrappedModel keeps NewRig's "judge: no model"
// guard honest against the one caller that hands it a wrapper.
//
// The harness wraps unconditionally (internal/modelretry), so "no
// model" arrives here already wrapped. A wrapper returning a typed nil
// pointer instead of a nil interface would make this guard pass and move
// the failure to a dereference somewhere with no context attached —
// which is why modelretry.Policy.Wrap returns model.LLM and short-
// circuits nil. That is asserted there; this is the guard it protects.
func TestNewRigRefusesANilWrappedModel(t *testing.T) {
	wrapped := modelretry.New(modelretry.JudgeConfig()).Wrap(nil)
	if _, err := NewRig(evals.IntentTable{}, nil, wrapped, t.TempDir()); err == nil {
		t.Error("NewRig accepted a nil-wrapping model, want the no-model error")
	}
}

// TestRig_CeilingExposesTheWriteToolGap pins the one scenario a
// read-only surface cannot fully cover. Reported, never folded into the
// score: LC-13 expects kubectl_rollback_deployment, and lookout excludes
// write tools by design.
func TestRig_CeilingExposesTheWriteToolGap(t *testing.T) {
	ds := loadCorpus(t)
	tbl := loadIntents(t)
	fx, err := Fixtures(ds, loadOverrides(t))
	if err != nil {
		t.Fatalf("Fixtures: %v", err)
	}
	rig, err := NewRig(tbl, fx, &scriptedModel{}, t.TempDir())
	if err != nil {
		t.Fatalf("NewRig: %v", err)
	}

	var capped []string
	for _, sc := range ds.Scenarios {
		if c := rig.ceiling(sc); c < 1 {
			capped = append(capped, sc.ID)
		}
	}
	if len(capped) != 1 || capped[0] != "LC-13-rollback-needed-after-bad" {
		t.Errorf("scenarios capped below 1.0 = %v, want only LC-13", capped)
	}
}

// TestRig_RecordsHowEachToolWasCalledAndWhatItFound is #169's end of
// the plumbing: the board used to record which tools a run reached for
// and nothing else, so a run that called the right tool against a scope
// holding nothing was indistinguishable from one that read it well. The
// scripted model here does both in the same run, plus invents an
// argument, and all three have to be legible in the outcome.
func TestRig_RecordsHowEachToolWasCalledAndWhatItFound(t *testing.T) {
	ds := loadCorpus(t)
	tbl := loadIntents(t)
	fx, err := Fixtures(ds, loadOverrides(t))
	if err != nil {
		t.Fatalf("Fixtures: %v", err)
	}

	var sc evals.Scenario
	for _, s := range ds.Scenarios {
		if s.ID == "LC-01-crashloopbackoff" {
			sc = s
		}
	}
	if sc.ID == "" {
		t.Fatal("LC-01 not in the corpus")
	}

	// Pick the empty read from the fixture rather than by guessing which
	// tool looks unrelated: a hand-picked name would quietly start
	// answering the day the intent table grows an edge, and the test
	// would keep passing while measuring nothing.
	cluster, err := NewCluster(tbl, sc, fx[sc.ID])
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	answering := map[string]bool{}
	for _, name := range cluster.AnsweringTools() {
		answering[name] = true
	}
	var reads, blind string
	for _, name := range sortedShapes() {
		switch {
		case answering[name] && reads == "":
			reads = name
		case !answering[name] && blind == "":
			blind = name
		}
	}
	if reads == "" || blind == "" {
		t.Fatalf("LC-01 needs one answering and one non-answering tool; answering=%v", cluster.AnsweringTools())
	}

	m := &scriptedModel{turns: []func(*adkmodel.LLMRequest) *adkmodel.LLMResponse{
		func(*adkmodel.LLMRequest) *adkmodel.LLMResponse {
			return call(reads, map[string]any{"scope": "production/api-server"})
		},
		func(*adkmodel.LLMRequest) *adkmodel.LLMResponse {
			return call(blind, map[string]any{"scope": "kube-system"})
		},
		func(*adkmodel.LLMRequest) *adkmodel.LLMResponse {
			// An argument the tool does not declare. ADK's functiontool
			// rejects the whole call over it, so this one call is both an
			// invented argument and an errored result — which is the
			// point: the old board recorded it as a tool that was used.
			return call(reads, map[string]any{"scope": "production", "since": "1h"})
		},
		func(*adkmodel.LLMRequest) *adkmodel.LLMResponse {
			return &adkmodel.LLMResponse{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText("CRITICAL: the Secret has no DATABASE_URL key. Recommended action: add it.")}},
				TurnComplete: true,
				FinishReason: genai.FinishReasonStop,
			}
		},
	}}

	rig, err := NewRig(tbl, fx, m, t.TempDir())
	if err != nil {
		t.Fatalf("NewRig: %v", err)
	}
	out, err := rig.Run(context.Background(), sc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(out.Calls) != 3 {
		t.Fatalf("recorded %d calls, want 3: %+v", len(out.Calls), out.Calls)
	}
	if got := out.Calls[0].Args["scope"]; got != "production/api-server" {
		t.Errorf("the arguments did not reach the board: %v", out.Calls[0].Args)
	}
	// The digest has to be the reading the tool actually returned, not a
	// label for it: the first words of the fixture's own text, collapsed
	// onto one line.
	reading, ok := cluster.ReadResult(reads, "production/api-server")
	if !ok {
		t.Fatalf("%s was chosen as the answering tool but found nothing", reads)
	}
	if head := strings.Join(strings.Fields(reading)[:8], " "); !strings.HasPrefix(out.Calls[0].Result, head) {
		t.Errorf("the result digest is not the reading the tool returned:\n got %q\nwant a prefix of %q", out.Calls[0].Result, head)
	}
	if strings.Contains(out.Calls[0].Result, "no abnormal findings") {
		t.Errorf("the answering tool's digest says it found nothing: %q", out.Calls[0].Result)
	}
	if !strings.Contains(out.Calls[1].Result, "no abnormal findings") {
		t.Errorf("the empty read's digest did not say it found nothing: %q", out.Calls[1].Result)
	}

	byKind := map[string][]evals.Violation{}
	for _, v := range out.Violations {
		byKind[v.Kind] = append(byKind[v.Kind], v)
	}
	empties := byKind[evals.ViolationEmptyResult]
	if len(empties) != 1 {
		t.Fatalf("want exactly one empty read (%s, not %s): %v", blind, reads, out.Violations)
	}
	if empties[0].CallIndex != 1 || empties[0].Tool != blind {
		t.Errorf("the empty read was attributed to call %d %s, want call 1 %s",
			empties[0].CallIndex, empties[0].Tool, blind)
	}
	if !strings.Contains(empties[0].Detail, "scope=kube-system") {
		t.Errorf("the empty read does not name the scope it read, which is the whole finding: %s", empties[0])
	}
	if extra := byKind[evals.ViolationUndeclaredArg]; len(extra) != 1 || extra[0].Arg != "since" {
		t.Errorf("the invented argument was not reported: %v", out.Violations)
	}
	// ADK refuses the call over the undeclared property, so the same
	// call is also an errored result. Both are reported: the argument
	// says what the model did wrong, the error says what it cost.
	if failed := byKind[evals.ViolationErrorResult]; len(failed) != 1 || failed[0].CallIndex != 2 {
		t.Errorf("the rejected call was not reported as an error result: %v", out.Violations)
	}
	if !strings.HasPrefix(out.Calls[2].Result, "error:") {
		t.Errorf("the rejected call's digest hides the rejection: %q", out.Calls[2].Result)
	}
	if unknown := byKind[evals.ViolationUnknownTool]; len(unknown) > 0 {
		t.Errorf("a fixture tool was validated as unknown, so the schemas did not come from the tools: %v", unknown)
	}
}

// TestToolSchemas_DescribeEveryFixtureTool. The schemas are read off the
// built tools rather than written beside them; if that read ever returns
// nothing useful, every recorded call would validate against an empty
// catalog and the board would fill with unknown_tool.
func TestToolSchemas_DescribeEveryFixtureTool(t *testing.T) {
	tools, err := buildTools(&Cluster{}, &callLog{})
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	schemas, err := toolSchemas(tools)
	if err != nil {
		t.Fatalf("toolSchemas: %v", err)
	}
	if len(schemas) != len(toolShape) {
		t.Fatalf("described %d of %d tools", len(schemas), len(toolShape))
	}
	for name := range toolShape {
		s, ok := schemas[name]
		if !ok {
			t.Errorf("%s has no schema", name)
			continue
		}
		props, _ := s["properties"].(map[string]any)
		spec, _ := props["scope"].(map[string]any)
		if spec == nil {
			t.Errorf("%s declares no scope argument, so every call naming one would be reported as invented: %v", name, s)
			continue
		}
		if got, _ := spec["type"].(string); got != "string" {
			t.Errorf("%s: scope is declared %q, want string", name, got)
		}
	}
}

func sortedShapes() []string {
	out := make([]string, 0, len(toolShape))
	for name := range toolShape {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestBuildTools_EveryShapedToolIsDescribed keeps the model from being
// asked to choose between tools it has no description for.
func TestBuildTools_EveryShapedToolIsDescribed(t *testing.T) {
	for name := range toolShape {
		if _, ok := toolDescription[name]; !ok {
			t.Errorf("tool %q has a shape but no description", name)
		}
	}
	for name := range toolDescription {
		if _, ok := toolShape[name]; !ok {
			t.Errorf("tool %q has a description but no shape", name)
		}
	}
}

// TestRig_ALoopingModelIsStoppedAndScored is #514 on the rig: a model
// that calls one tool with one set of arguments forever used to run
// until the server refused the context. The rig now runs the loop guard
// at mast's default posture, so the run ends at the guard's stop, the
// row completes, and it says why.
func TestRig_ALoopingModelIsStoppedAndScored(t *testing.T) {
	ds := loadCorpus(t)
	tbl := loadIntents(t)
	fx, err := Fixtures(ds, loadOverrides(t))
	if err != nil {
		t.Fatalf("Fixtures: %v", err)
	}
	var sc evals.Scenario
	for _, s := range ds.Scenarios {
		if s.ID == "LC-01-crashloopbackoff" {
			sc = s
		}
	}

	loop := func(*adkmodel.LLMRequest) *adkmodel.LLMResponse {
		return call("k8s_triage_workload", map[string]any{"scope": "production/api-server"})
	}
	m := &scriptedModel{}
	for i := 0; i < 50; i++ {
		m.turns = append(m.turns, loop)
	}

	rig, err := NewRig(tbl, fx, m, t.TempDir())
	if err != nil {
		t.Fatalf("NewRig: %v", err)
	}
	out, err := rig.Run(context.Background(), sc)
	if err != nil {
		t.Fatalf("Run: %v — a guard stop is a completed row, not a failed one", err)
	}
	if out.LoopStop == "" {
		t.Fatal("LoopStop is empty: nothing stopped a model that called one tool 50 times")
	}
	if m.at >= 50 {
		t.Errorf("the model was called %d times; the guard should have ended the turn well before the script ran out", m.at)
	}
	if len(out.Results) == 0 {
		t.Error("the stopped row was not scored")
	}
}
