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
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/core-agent/v3/internal/evals"
)

// gradeReply is a grader turn: the JSON object wrapped in the prose and
// fencing a real model tends to add, so the parser is exercised the way
// it will actually be used.
func gradeReply(g Grade) func(*adkmodel.LLMRequest) *adkmodel.LLMResponse {
	return func(*adkmodel.LLMRequest) *adkmodel.LLMResponse {
		b, err := json.Marshal(g)
		if err != nil {
			panic(err)
		}
		text := "Here is my assessment.\n\n```json\n" + string(b) + "\n```\n"
		return &adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText(text)}},
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}
	}
}

func TestGrade_NormalizedMapsTheRubric(t *testing.T) {
	tests := []struct {
		score int
		want  float64
	}{
		{1, 0}, {2, 0.25}, {3, 0.5}, {4, 0.75}, {5, 1},
		// Out-of-rubric scores are clamped rather than propagated: a
		// grader that ignores its own rubric must not push a metric
		// outside 0-1 and make the board unreadable. parseGrade rejects
		// them before they get here; this is the second line.
		{0, 0}, {7, 1},
	}
	for _, tc := range tests {
		if got := (Grade{Score: tc.score}).Normalized(); got != tc.want {
			t.Errorf("score %d normalized to %v, want %v", tc.score, got, tc.want)
		}
	}
}

// TestGrade_ResultCarriesTheRubricFields keeps the three booleans in the
// comment. They are the part of the grade a human can check against the
// response; a bare number cannot be argued with.
func TestGrade_ResultCarriesTheRubricFields(t *testing.T) {
	res := Grade{
		Reasoning: "names the Secret and the missing key",
		Score:     4, Specific: true, Actionable: true, CorrectDiagnosis: false,
	}.Result()

	if res.Metric != MetricResponseQuality {
		t.Errorf("metric = %q, want %q", res.Metric, MetricResponseQuality)
	}
	if res.Score != 0.75 {
		t.Errorf("score = %v, want 0.75", res.Score)
	}
	for _, want := range []string{"score=4/5", "specific=true", "actionable=true", "correct_diagnosis=false", "names the Secret"} {
		if !strings.Contains(res.Comment, want) {
			t.Errorf("comment %q is missing %q", res.Comment, want)
		}
	}
}

// TestJudge_GradesAResponse checks the whole grading path, including
// that both texts reach the grader. A prompt missing the ground truth
// would still parse and still produce a number — it would just be
// grading the response against nothing.
func TestJudge_GradesAResponse(t *testing.T) {
	sc := evals.Scenario{ID: "LC-TEST"}
	sc.Outputs.ExpectedResponse = "CRITICAL: api-server is crashlooping. Recommended action: add DATABASE_URL."
	const response = "CRITICAL: the api-server pod restarts because DATABASE_URL is absent from api-server-secrets."

	var prompt string
	m := &scriptedModel{turns: []func(*adkmodel.LLMRequest) *adkmodel.LLMResponse{
		func(req *adkmodel.LLMRequest) *adkmodel.LLMResponse {
			for _, c := range req.Contents {
				for _, p := range c.Parts {
					prompt += p.Text
				}
			}
			return gradeReply(Grade{
				Reasoning: "matches the diagnosis and names the Secret",
				Score:     5, Specific: true, Actionable: true, CorrectDiagnosis: true,
			})(req)
		},
	}}

	j, err := NewJudge(m)
	if err != nil {
		t.Fatalf("NewJudge: %v", err)
	}
	g, err := j.Grade(context.Background(), sc, response)
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}

	if g.Score != 5 || !g.CorrectDiagnosis {
		t.Errorf("grade = %+v, want the scripted 5/correct", g)
	}
	if g.Normalized() != 1 {
		t.Errorf("normalized = %v, want 1", g.Normalized())
	}
	if !strings.Contains(prompt, sc.Outputs.ExpectedResponse) {
		t.Errorf("the ground truth never reached the grader:\n%s", prompt)
	}
	if !strings.Contains(prompt, response) {
		t.Errorf("the agent response never reached the grader:\n%s", prompt)
	}
}

// TestJudge_EmptyResponseSkipsTheGrader pins the one shortcut in the
// path, and that it really is a shortcut rather than a silent zero: an
// empty run is graded 1 with a reason, and no request is spent.
func TestJudge_EmptyResponseSkipsTheGrader(t *testing.T) {
	m := &scriptedModel{}
	j, err := NewJudge(m)
	if err != nil {
		t.Fatalf("NewJudge: %v", err)
	}

	g, err := j.Grade(context.Background(), evals.Scenario{ID: "LC-TEST"}, "   \n\t")
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if g.Score != 1 {
		t.Errorf("score = %d, want 1 for an empty run", g.Score)
	}
	if g.Reasoning == "" {
		t.Error("an empty run was scored 1 with no reason given")
	}
	if m.at != 0 {
		t.Errorf("the grader was called %d time(s) on an empty response", m.at)
	}
}

func TestNewJudge_RefusesNoModel(t *testing.T) {
	if _, err := NewJudge(nil); err == nil {
		t.Fatal("NewJudge accepted a nil model")
	}
}

// TestParseGrade_RejectsUnusableReplies makes grader failure loud. Every
// case here would otherwise land on the board as a low score for mast,
// which is the wrong party.
func TestParseGrade_RejectsUnusableReplies(t *testing.T) {
	tests := []struct {
		name, reply, wantErr string
	}{
		{"no json", "I would rather not grade this.", "no JSON object"},
		{"malformed json", `{"score": 5,}`, "unparseable"},
		{"score above the rubric", `{"score": 9}`, "outside the 1-5 rubric"},
		{"score below the rubric", `{"score": 0}`, "outside the 1-5 rubric"},
		{"no score field", `{"reasoning": "looks fine"}`, "outside the 1-5 rubric"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseGrade(tc.reply)
			if err == nil {
				t.Fatalf("parseGrade(%q) succeeded", tc.reply)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestParseGrade_IgnoresDecoyBraces is the regression pin for the
// greedy-regexp version of this parser, which took everything from the
// first brace in the reply to the last. Running the whole corpus through
// compose's echo model found it: the echoed prompt carries the JSON
// contract template, and every row came back "unparseable" — a grader
// failure indistinguishable from a real one, silently shortening the
// board by a column.
//
// The decoys here are the three shapes that actually occur: a rubric
// restated before the answer, a Kubernetes object quoted out of the
// response, and a nested object inside the grade itself.
func TestParseGrade_IgnoresDecoyBraces(t *testing.T) {
	tests := []struct {
		name  string
		reply string
	}{
		{
			"contract template restated first",
			`The format is {"reasoning": "<why>", "score": <1-5>}. My answer:
			{"reasoning":"names the Deployment","score":4,"specific":true,"actionable":true,"correct_diagnosis":true}`,
		},
		{
			"resource quoted from the response",
			`The response suggests adding {key='node-role', value='monitoring', effect='NoSchedule'} to the spec.
			{"reasoning":"names the Deployment","score":4,"specific":true,"actionable":true,"correct_diagnosis":true}`,
		},
		{
			"nested object inside the grade",
			`{"reasoning":"names the Deployment","score":4,"specific":true,"actionable":true,"correct_diagnosis":true,"detail":{"resources":["log-collector"]}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, err := parseGrade(tc.reply)
			if err != nil {
				t.Fatalf("parseGrade: %v", err)
			}
			if g.Score != 4 || g.Reasoning != "names the Deployment" {
				t.Errorf("grade = %+v, want the real grade rather than a decoy", g)
			}
		})
	}
}

// TestParseGrade_AcceptsAFencedObject is the other half: the parser has
// to survive the wrapping models add, or a correct grade gets reported
// as a grader failure.
func TestParseGrade_AcceptsAFencedObject(t *testing.T) {
	g, err := parseGrade("Sure.\n```json\n{\"reasoning\":\"ok\",\"score\":3,\"specific\":true,\"actionable\":false,\"correct_diagnosis\":true}\n```\nHope that helps.")
	if err != nil {
		t.Fatalf("parseGrade: %v", err)
	}
	if g.Score != 3 || !g.Specific || g.Actionable || !g.CorrectDiagnosis {
		t.Errorf("grade = %+v, want 3/specific/not-actionable/correct", g)
	}
}

// upstreamAgentText reproduces the accessor at
// sre-agent/evals/evaluators.py:142 (and again at
// upload_online_evals.py:83):
//
//	agent_text = run["outputs"].get("expected_response", "")
//
// "expected_response" is the *ground truth* key — the one create_dataset.py
// writes into example["outputs"]. Nothing on the run side produces it:
// upstream's agent is create_deep_agent, whose output state is keyed
// messages/todos/files. So the accessor misses on every row.
func upstreamAgentText(runOutputs map[string]any) string {
	if s, ok := runOutputs["expected_response"].(string); ok {
		return s
	}
	return ""
}

// TestUpstreamQualityIsAConstantFunction is the third reproduction of an
// upstream evaluator defect, after the two W0.4 pins in
// internal/evals/measurability_test.go. The first two were constant
// *scores*; this one is subtler and worse, because a judge produces a
// plausible-looking spread of numbers either way.
//
// The claim is made without calling a model, and is stronger for it: the
// grader runs at temperature 0, so if the prompt does not vary with the
// agent's answer then neither can the grade. This test shows the prompt
// does not vary — a perfect report, a wrong report and no report at all
// produce byte-identical grader input on every row of the corpus.
//
// mast's Judge is checked on the same three responses for the same
// property, so the test discriminates rather than merely asserting
// upstream is broken.
func TestUpstreamQualityIsAConstantFunction(t *testing.T) {
	ds := loadCorpus(t)

	// Three responses a grader must not confuse: the ground truth
	// itself, a confidently wrong diagnosis, and silence.
	responses := func(sc evals.Scenario) []string {
		return []string{
			sc.Outputs.ExpectedResponse,
			"INFO: the cluster looks healthy, nothing to do.",
			"",
		}
	}

	for _, sc := range ds.Scenarios {
		// Upstream: the deep agent's real output shape, which carries
		// the report under "messages" as every LangGraph agent does.
		upstream := map[string]bool{}
		for _, resp := range responses(sc) {
			runOutputs := map[string]any{
				"messages": []any{map[string]any{"role": "assistant", "content": resp}},
			}
			exampleOutputs := map[string]any{"expected_response": sc.Outputs.ExpectedResponse}
			upstream[fmt.Sprintf(qualityPrompt, exampleOutputs["expected_response"], upstreamAgentText(runOutputs))] = true
		}
		if len(upstream) != 1 {
			t.Fatalf("%s: upstream's grader saw %d distinct prompts; the defect no longer reproduces, so this pin is stale",
				sc.ID, len(upstream))
		}

		// mast: the same three responses must reach the grader as three
		// different prompts.
		var prompts []string
		m := &scriptedModel{}
		for range responses(sc) {
			m.turns = append(m.turns, func(req *adkmodel.LLMRequest) *adkmodel.LLMResponse {
				var b strings.Builder
				for _, c := range req.Contents {
					for _, p := range c.Parts {
						b.WriteString(p.Text)
					}
				}
				prompts = append(prompts, b.String())
				return gradeReply(Grade{Score: 3, Reasoning: "scripted"})(req)
			})
		}
		j, err := NewJudge(m)
		if err != nil {
			t.Fatalf("NewJudge: %v", err)
		}
		for _, resp := range responses(sc) {
			if _, err := j.Grade(context.Background(), sc, resp); err != nil {
				t.Fatalf("%s: Grade: %v", sc.ID, err)
			}
		}
		// The empty response is graded without a call by design, so two
		// prompts is the expected count — and they must differ.
		if len(prompts) != 2 || prompts[0] == prompts[1] {
			t.Fatalf("%s: mast's grader saw %d prompts, distinct=%t; want 2 distinct",
				sc.ID, len(prompts), len(prompts) == 2 && prompts[0] != prompts[1])
		}
	}
}
