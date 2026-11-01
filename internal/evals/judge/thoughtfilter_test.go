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
	"testing"

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/core-agent/v3/internal/evals"
)

// TestJudgeSkipsThinking pins that the grader's reply is read from its answer
// and not from its thinking block (#370).
//
// Grade concatenates every part into one buffer and then scans that buffer for
// the grade object, so a thinking block does not merely ride along here — it is
// parser input. jsonObjects already documents what a stray brace does to the
// scan, and reasoning is where a grader restates the rubric, so the brace it
// takes is the likely one rather than a contrived one. The failure is silent in
// the worst way: a grader error is reported as a missing column, so the board
// comes back short rather than red, and the judged tier quietly stops measuring
// the rows it could not parse.
//
// The thought here carries a single unbalanced `{`. Broken, the scan never
// returns to depth zero, finds no candidate at all, and Grade fails with
// "grader returned no JSON object".
//
// Neutralize check: restore `p.Text != ""` in Grade's part loop and this
// returns that error instead of the scripted 4.
func TestJudgeSkipsThinking(t *testing.T) {
	const canary = "the rubric says {4 is correct diagnosis, mostly specific, remediation vague"

	m := &scriptedModel{turns: []func(*adkmodel.LLMRequest) *adkmodel.LLMResponse{
		func(req *adkmodel.LLMRequest) *adkmodel.LLMResponse {
			answer := gradeReply(Grade{
				Reasoning: "correct diagnosis, remediation is vague",
				Score:     4, Specific: true, CorrectDiagnosis: true,
			})(req)
			answer.Content.Parts = append(
				[]*genai.Part{{Text: canary, Thought: true, ThoughtSignature: []byte("sig-370")}},
				answer.Content.Parts...,
			)
			return answer
		},
	}}

	j, err := NewJudge(m)
	if err != nil {
		t.Fatalf("NewJudge: %v", err)
	}
	sc := evals.Scenario{ID: "LC-THOUGHT"}
	sc.Outputs.ExpectedResponse = "CRITICAL: api-server is crashlooping."
	g, err := j.Grade(context.Background(), sc, "the api-server pod restarts")
	if err != nil {
		t.Fatalf("Grade: %v (the grader's thinking block reached the JSON scanner)", err)
	}
	if g.Score != 4 || !g.CorrectDiagnosis {
		t.Errorf("grade = %+v, want the scripted 4/correct", g)
	}
}
