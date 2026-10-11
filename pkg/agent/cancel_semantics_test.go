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

package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync/atomic"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
)

// ADK v2 runs llmagent on a workflow scheduler whose handling of
// cancellation is not what v1's was, and core-agent's turn-outcome
// classification (operator stop vs fault, guardrail cut labelling, the
// wake loop's failure backoff) depends on which shape reaches it. These
// two tests pin both sides of that.
//
// The rule, from workflow/scheduler.go (handleCompletion + run): when the
// invocation's own context dies, Run yields context.Cause(parent) after
// draining. But a node error that merely MATCHES context.Canceled while
// the invocation context is still live is taken for a sibling
// cancellation: the node is marked cancelled and the turn is reported as
// clean, with no error at all.
//
// Every production stop today cancels the context passed into
// runner.Run (Interrupt, the cost-ceiling / watchdog / refusal-storm
// cuts that call it, subagent and subtask wall-clock timeouts), so it
// takes the first path. A future model wrapper that cancels a context
// of its OWN — a per-call timeout, a retry layer with its own budget —
// and returns that error would take the second, and its failure would
// vanish. If you are writing such a wrapper, make its error not match
// context.Canceled, or cancel the turn instead.

// cancelLLM either interrupts the turn the way a real stop does, or
// returns a wrapped context.Canceled while the turn is still live.
type cancelLLM struct {
	interrupt func() bool
	selfOwned atomic.Bool
}

func (*cancelLLM) Name() string { return "cancel" }

func (l *cancelLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if l.selfOwned.Load() {
			// A context the wrapper owned died; the turn's did not.
			yield(nil, fmt.Errorf("model call: per-call budget: %w", context.Canceled))
			return
		}
		if !l.interrupt() {
			yield(nil, errors.New("cancelLLM: no turn in flight"))
			return
		}
		<-ctx.Done()
		yield(nil, fmt.Errorf("model call: %w", ctx.Err()))
	}
}

func runErrors(t *testing.T, a *Agent) []error {
	t.Helper()
	var errs []error
	for _, err := range a.Run(context.Background(), "go") {
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// The production invariant: a stop that cancels the turn reaches the
// caller as context.Canceled, which is what repl.go, the autonomous
// loop and the wake loop classify as an obeyed stop.
func TestRun_InterruptSurfacesAsCanceled(t *testing.T) {
	t.Parallel()
	llm := &cancelLLM{}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	llm.interrupt = a.Interrupt

	errs := runErrors(t, a)
	canceled := false
	for _, e := range errs {
		if errors.Is(e, context.Canceled) {
			canceled = true
		}
	}
	if !canceled {
		t.Fatalf("an interrupted turn yielded %v; want an error matching context.Canceled", errs)
	}
}

// The trap, pinned. If this starts failing, ADK changed how it treats a
// node's Canceled under a live invocation — good news, and the comment
// above (and the wake loop test's note) should be updated with it.
func TestRun_ModelCanceledUnderLiveTurnIsReportedClean(t *testing.T) {
	t.Parallel()
	llm := &cancelLLM{}
	llm.selfOwned.Store(true)
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	llm.interrupt = a.Interrupt

	if errs := runErrors(t, a); len(errs) != 0 {
		t.Fatalf("ADK v2 now surfaces a model's own Canceled (%v); update the note at the top of this file", errs)
	}
}
