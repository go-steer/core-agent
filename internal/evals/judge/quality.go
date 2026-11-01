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

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/core-agent/v3/internal/evals"
	"github.com/go-steer/core-agent/v3/internal/modeltext"
)

// MetricResponseQuality is the judge tier's own metric name. It sits
// alongside the deterministic metric names in internal/evals so a
// scoreboard row can be labelled the same way whichever tier produced
// it.
const MetricResponseQuality = "response_quality"

// Grade is upstream's QualityGrade, field for field
// (sre-agent/evals/evaluators.py:110-115). Keeping the shape identical
// is the point: the number is only comparable if the thing being asked
// is the same thing.
type Grade struct {
	Reasoning        string `json:"reasoning"`
	Score            int    `json:"score"`
	Specific         bool   `json:"specific"`
	Actionable       bool   `json:"actionable"`
	CorrectDiagnosis bool   `json:"correct_diagnosis"`
}

// Normalized maps the 1-5 score to 0.0-1.0, the same mapping upstream
// applies before reporting.
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

// qualityPrompt is upstream's _QUALITY_PROMPT verbatim
// (sre-agent/evals/evaluators.py:122-138), with the JSON contract
// appended because mast asks a text model for structured output rather
// than going through LangChain's with_structured_output.
//
// Verbatim matters here in a way it did not for the agent's own
// instruction. The agent prompt had to change because upstream's names
// eight tools mast does not have; the judge prompt names nothing but
// the two texts it compares, so any edit would be mast marking its own
// homework on a rubric it wrote.
const qualityPrompt = `You are evaluating an SRE agent's response to a Kubernetes incident or health-check query.

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

// Judge grades an agent response against the corpus's ground truth.
type Judge struct{ model adkmodel.LLM }

// NewJudge wraps a model as the grader.
//
// The grader must be a different model instance from the one under
// test, and the harness passes a cheap tier for it — upstream grades
// with gpt-4o-mini at temperature 0 for the same reason. Nothing here
// can enforce "different", so the harness names both models in the
// report and the reader can see when they are the same.
func NewJudge(m adkmodel.LLM) (*Judge, error) {
	if m == nil {
		return nil, fmt.Errorf("judge: no grading model")
	}
	return &Judge{model: m}, nil
}

// Grade scores one response.
//
// An empty response is graded 1 without a model call. That is not a
// shortcut: it is the case upstream's own evaluator silently produces
// for every row (see TestUpstreamQualityIsAConstantFunction), and
// spending a request to be told an empty string is a poor incident
// report would only make the defect more expensive to reproduce.
func (j *Judge) Grade(ctx context.Context, sc evals.Scenario, response string) (Grade, error) {
	if strings.TrimSpace(response) == "" {
		return Grade{
			Score:     1,
			Reasoning: "the run produced no final text, so there is nothing to grade",
		}, nil
	}

	prompt := fmt.Sprintf(qualityPrompt, sc.Outputs.ExpectedResponse, response)
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)},
		Config: &genai.GenerateContentConfig{
			Temperature: genai.Ptr[float32](0),
		},
	}

	var text strings.Builder
	for resp, err := range j.model.GenerateContent(ctx, req, false) {
		if err != nil {
			return Grade{}, fmt.Errorf("judge: grade %s: %w", sc.ID, err)
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

	g, err := parseGrade(text.String())
	if err != nil {
		return Grade{}, fmt.Errorf("judge: grade %s: %w", sc.ID, err)
	}
	return g, nil
}

// jsonObjects returns the reply's top-level brace-balanced spans, in the
// order they appear.
//
// This is a scanner rather than a regexp for the same reason
// [quotedSpans] is. The obvious `(?s)\{.*\}` is greedy across the whole
// reply, so a decoy brace anywhere before the grade — a restated rubric,
// a Kubernetes toleration like {key='node-role'} quoted from the
// response, the prompt echoed back — swallows everything up to the last
// closing brace and the grade parses as garbage. A grader failure is
// reported as a missing column, so the cost of getting this wrong is a
// silently short board rather than a visible error.
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

// parseGrade reads the grade out of a reply.
//
// Candidates are tried last-first: when a model prefaces its answer with
// an example or a restatement, the grade is the object it finishes on.
// The first error encountered is the one reported, so the message names
// the object that looked most like an answer.
func parseGrade(reply string) (Grade, error) {
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
