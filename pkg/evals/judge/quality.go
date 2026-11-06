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

// Package judge provides the LLM-as-judge quality rubric for grading agent
// responses against ground-truth diagnoses (#1538).
package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/core-agent/v3/internal/modeltext"
	"github.com/go-steer/core-agent/v3/pkg/evals"
)

// MetricResponseQuality is the judge tier's metric name.
const MetricResponseQuality = "response_quality"

// Grade is the structured quality rubric verdict returned by Judge.
type Grade struct {
	Reasoning        string `json:"reasoning"`
	Score            int    `json:"score"`
	Specific         bool   `json:"specific"`
	Actionable       bool   `json:"actionable"`
	CorrectDiagnosis bool   `json:"correct_diagnosis"`
}

// Normalized maps the 1-5 score to 0.0-1.0.
func (g Grade) Normalized() float64 {
	if g.Score < 1 {
		return 0
	}
	if g.Score > 5 {
		return 1
	}
	return float64(g.Score-1) / 4
}

// Result renders the grade as an evals.MetricResult so the judge metric
// prints and serializes beside the deterministic ones.
func (g Grade) Result() evals.MetricResult {
	return evals.MetricResult{
		Metric: MetricResponseQuality,
		Score:  g.Normalized(),
		Comment: fmt.Sprintf("score=%d/5 | specific=%t, actionable=%t, correct_diagnosis=%t | %s",
			g.Score, g.Specific, g.Actionable, g.CorrectDiagnosis, g.Reasoning),
	}
}

// QualityPrompt is the fixed 1-5 quality rubric prompt template comparing
// expected ground truth and agent response text.
const QualityPrompt = `You are evaluating an SRE agent's response to a Kubernetes incident or health-check query.

## Expected response (ground truth)
%s

## Agent response (to evaluate)
%s

## Scoring rubric (1–5)
5 — Matches expected diagnosis; names specific resources; gives actionable remediation steps
4 — Correct diagnosis; mostly specific; remediation present but vague in one area
3 — Partially correct; some specifics missing; some actionable advice
2 — Diagnosis is off or too generic; little actionable content
1 — Wrong diagnosis or irrelevant response

Evaluate and return structured output.

Reply with a single JSON object and nothing else:
{"reasoning": "<step-by-step reasoning for the score>", "score": <integer 1-5>, "specific": <true if the response names specific resources such as pod names or namespaces>, "actionable": <true if the response gives clear next steps or remediation advice>, "correct_diagnosis": <true if the root cause identified matches the expected response>}`

// Judge grades an agent response against ground truth using an ADK LLM.
type Judge struct{ model adkmodel.LLM }

// NewJudge wraps a model as the grader.
func NewJudge(m adkmodel.LLM) (*Judge, error) {
	if m == nil {
		return nil, fmt.Errorf("judge: no grading model")
	}
	return &Judge{model: m}, nil
}

// Grade scores one scenario response against sc.Outputs.ExpectedResponse.
func (j *Judge) Grade(ctx context.Context, sc evals.Scenario, response string) (Grade, error) {
	return j.GradeText(ctx, sc.ID, sc.Outputs.ExpectedResponse, response)
}

// GradeText scores an agent response against an expected ground-truth response
// without requiring a full Scenario wrapper.
func (j *Judge) GradeText(ctx context.Context, id, expectedResponse, response string) (Grade, error) {
	if strings.TrimSpace(response) == "" {
		return Grade{
			Score:     1,
			Reasoning: "the run produced no final text, so there is nothing to grade",
		}, nil
	}

	prompt := fmt.Sprintf(QualityPrompt, expectedResponse, response)
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)},
		Config: &genai.GenerateContentConfig{
			Temperature: genai.Ptr[float32](0),
		},
	}

	var text strings.Builder
	for resp, err := range j.model.GenerateContent(ctx, req, false) {
		if err != nil {
			return Grade{}, fmt.Errorf("judge: grade %s: %w", id, err)
		}
		if resp == nil || resp.Content == nil {
			continue
		}
		for _, p := range resp.Content.Parts {
			if s, ok := modeltext.Text(p); ok {
				text.WriteString(s)
			}
		}
	}

	g, err := ParseGrade(text.String())
	if err != nil {
		return Grade{}, fmt.Errorf("judge: grade %s: %w", id, err)
	}
	return g, nil
}

func jsonObjects(s string) []string {
	var (
		out     []string
		depth   int
		start   int
		inStr   bool
		escaped bool
	)
	for i, r := range s {
		switch {
		case escaped:
			escaped = false
		case inStr && r == '\\':
			escaped = true
		case r == '"':
			inStr = !inStr
		case inStr:
		case r == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case r == '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 {
				out = append(out, s[start:i+1])
			}
		}
	}
	return out
}

// ParseGrade reads the structured Grade JSON object out of a model reply.
func ParseGrade(reply string) (Grade, error) {
	cands := jsonObjects(reply)
	if len(cands) == 0 {
		return Grade{}, fmt.Errorf("grader returned no JSON object: %q", truncate(reply, 200))
	}
	var firstErr error
	keep := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for i := len(cands) - 1; i >= 0; i-- {
		var g Grade
		if err := json.Unmarshal([]byte(cands[i]), &g); err != nil {
			keep(fmt.Errorf("grader returned unparseable JSON (%w): %q", err, truncate(cands[i], 200)))
			continue
		}
		if g.Score < 1 || g.Score > 5 {
			keep(fmt.Errorf("grader returned score %d, outside the 1-5 rubric: %q", g.Score, truncate(cands[i], 200)))
			continue
		}
		return g, nil
	}
	return Grade{}, firstErr
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
