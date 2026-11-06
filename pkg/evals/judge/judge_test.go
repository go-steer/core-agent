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

package judge_test

import (
	"context"
	"iter"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/evals"
	"github.com/go-steer/core-agent/v3/pkg/evals/judge"
)

type stubLLM struct {
	reply string
}

func (s *stubLLM) Name() string { return "stub-judge" }

func (s *stubLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content: genai.NewContentFromText(s.reply, genai.RoleModel),
		}, nil)
	}
}

func TestJudge_GradeAndGradeText(t *testing.T) {
	t.Parallel()

	j, err := judge.NewJudge(&stubLLM{
		reply: `{"reasoning":"matched root cause and named pod","score":5,"specific":true,"actionable":true,"correct_diagnosis":true}`,
	})
	if err != nil {
		t.Fatalf("NewJudge: %v", err)
	}

	g, err := j.GradeText(context.Background(), "S-1", "expected OOMKill in payments-api", "CRITICAL: payments-api-7f8b OOMKilled; increase memory limit.")
	if err != nil {
		t.Fatalf("GradeText: %v", err)
	}
	if g.Score != 5 || !g.Specific || !g.Actionable || !g.CorrectDiagnosis {
		t.Errorf("Grade = %+v", g)
	}
	if g.Normalized() != 1.0 {
		t.Errorf("Normalized() = %v, want 1.0", g.Normalized())
	}
	res := g.Result()
	if res.Metric != judge.MetricResponseQuality || !res.Passed() {
		t.Errorf("Result() = %+v", res)
	}

	sc := evals.Scenario{
		ID:      "S-1",
		Outputs: evals.ScenarioOutputs{ExpectedResponse: "expected OOMKill"},
	}
	emptyGrade, err := j.Grade(context.Background(), sc, "   ")
	if err != nil {
		t.Fatalf("Grade(empty): %v", err)
	}
	if emptyGrade.Score != 1 || emptyGrade.Normalized() != 0 {
		t.Errorf("emptyGrade = %+v, want Score=1, Normalized=0", emptyGrade)
	}
}
