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

package evals

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// TestTraceFinalTextSkipsThinking pins that a run's FinalText is what the run
// said, not what it thought (#370).
//
// FinalText is what the severity evaluator reads and what a failing row prints,
// so a leak here does two things at once: it scores the model on reasoning it
// was never asked to publish — reasoning that habitually contains the words the
// severity rubric matches on, because that is where the model weighs them — and
// it copies the thinking block into the board's own output.
//
// The parts are ordered answer-then-reasoning because this site keeps the LAST
// model text part; under the other order it is correct by accident. See the
// note in internal/cli/thoughtfilter_test.go on why mast does not get to assume an
// order.
//
// Neutralize check: restore `part.Text != ""` in TraceFromEvents and FinalText
// is the reasoning.
func TestTraceFinalTextSkipsThinking(t *testing.T) {
	const canary = "<reasoning> if I call this CRITICAL they will page someone"
	events := eventList{
		userEvent(genai.NewPartFromText("api-server is crashlooping")),
		modelEvent(
			genai.NewPartFromText("WARNING: the api-server deployment is restarting"),
			&genai.Part{Text: canary, Thought: true, ThoughtSignature: []byte("sig-370")},
		),
	}

	tr := TraceFromEvents(events, readOnlyPred(), nil)

	if strings.Contains(tr.FinalText, canary) {
		t.Errorf("FinalText carries a thinking block: %q", tr.FinalText)
	}
	if tr.FinalText != "WARNING: the api-server deployment is restarting" {
		t.Errorf("FinalText = %q, want the visible text alone", tr.FinalText)
	}

	// A run whose only model text is a thinking block produced no answer, and
	// FinalText must say so — an evaluator grading the reasoning instead would
	// report a score for a response that does not exist.
	only := eventList{
		userEvent(genai.NewPartFromText("api-server is crashlooping")),
		modelEvent(&genai.Part{Text: canary, Thought: true, ThoughtSignature: []byte("sig-370")}),
	}
	if got := TraceFromEvents(only, readOnlyPred(), nil).FinalText; got != "" {
		t.Errorf("thinking-only run FinalText = %q, want empty", got)
	}
}
