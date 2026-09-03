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
	"iter"
	"sync/atomic"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// End-to-end watchdog coverage over the STREAMING event shape (#915).
// The unit tests in watchdog_test.go call the tap directly with events
// a test composed; these drive a real Run with a real DefaultWatchdog
// and let ADK build the events, which is the only way to see what the
// tap is actually handed — in particular that ADK stamps an
// `adk-`-prefixed UUID on every ID-less FunctionCall before the event
// is yielded (finalizeModelResponseEvent →
// utils.PopulateClientFunctionCallID). Reasoning about the tap from
// the provider's wire shape alone gets that wrong, and #907 and #915
// both did.

// streamingLoopLLM reproduces, at the model.LLM boundary, what ADK's
// gemini model yields in streaming mode: a partial response per chunk,
// then one non-partial aggregate carrying the whole sequence
// (generateStream → aggregator.ProcessResponse per chunk, then
// aggregator.Close). It calls one tool with fixed args `calls` times
// and then finishes.
//
// rebuildOnAggregate picks between the aggregator's two shapes, which
// differ in exactly the way that matters here:
//
//   - false: the chunk carried a complete FunctionCall, and the
//     aggregate forwards THE SAME *genai.Part pointer (sequence =
//     append(sequence, part)). Both events therefore carry the one ID
//     ADK synthesized when it finalized the first of them.
//   - true: the chunk carried PartialArgs, so the aggregate emits a
//     freshly built part (flushFunctionCallToSequence). Chunk and
//     aggregate are distinct parts, each gets its own synthesized ID,
//     and ID dedup cannot pair them.
type streamingLoopLLM struct {
	served             atomic.Int32
	tool               string
	calls              int32
	rebuildOnAggregate bool
}

func (*streamingLoopLLM) Name() string { return "streaming-loop" }

func (l *streamingLoopLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	n := l.served.Add(1)
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if n > l.calls {
			yield(&adkmodel.LLMResponse{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}},
				TurnComplete: true,
			}, nil)
			return
		}
		aggregate := &genai.Part{FunctionCall: &genai.FunctionCall{
			Name: l.tool, Args: map[string]any{"q": "same"},
		}}
		chunk := aggregate
		if l.rebuildOnAggregate {
			chunk = &genai.Part{FunctionCall: &genai.FunctionCall{
				Name:        l.tool,
				PartialArgs: []*genai.PartialArg{{JsonPath: "$.q", StringValue: "same"}},
			}}
		}
		if !yield(&adkmodel.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{chunk}},
			Partial: true,
		}, nil) {
			return
		}
		yield(&adkmodel.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{aggregate}},
		}, nil)
	}
}

// runStreamingLoop runs one turn in which the model calls the same tool
// with the same args `calls` times, and reports every watchdog alert
// the turn raised.
func runStreamingLoop(t *testing.T, calls int32, rebuildOnAggregate bool) []watchdog.Alert {
	t.Helper()
	type probeArgs struct {
		Q string `json:"q"`
	}
	type probeResult struct {
		OK bool `json:"ok"`
	}
	probe, err := functiontool.New(
		functiontool.Config{Name: "probe", Description: "reads back the value it is given"},
		func(_ tool.Context, a probeArgs) (probeResult, error) { return probeResult{OK: a.Q != ""}, nil },
	)
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}
	var alerts []watchdog.Alert
	a, err := New(&streamingLoopLLM{tool: "probe", calls: calls, rebuildOnAggregate: rebuildOnAggregate},
		WithTools([]tool.Tool{probe}),
		WithWatchdog(watchdog.NewDefaultWatchdog(), func(al watchdog.Alert) { alerts = append(alerts, al) }),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, err := range a.Run(context.Background(), "go") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	return alerts
}

func alertBySignal(alerts []watchdog.Alert, signal string) *watchdog.Alert {
	for i := range alerts {
		if alerts[i].Signal == signal {
			return &alerts[i]
		}
	}
	return nil
}

// TestStreamingLoop_RebuiltAggregateCountsOnce is the #915 gate. When
// the aggregator rebuilds the call part, the chunk and the aggregate
// are two parts with two synthesized IDs, so every real call reached
// the watchdog twice — once with the chunk's empty args and once
// complete. Five consecutive IDENTICAL calls then never happen, and
// the alternating detector reads the a/b/a/b that pattern makes as a
// two-call cycle the model never ran.
//
// Pre-fix this raises alternating-tool-cycle and no repeated-tool-call.
func TestStreamingLoop_RebuiltAggregateCountsOnce(t *testing.T) {
	t.Parallel()
	alerts := runStreamingLoop(t, int32(watchdog.DefaultRepeatThreshold), true)
	if alertBySignal(alerts, "repeated-tool-call") == nil {
		t.Errorf("%d identical calls raised no repeated-tool-call; got %v",
			watchdog.DefaultRepeatThreshold, signals(alerts))
	}
	if al := alertBySignal(alerts, "alternating-tool-cycle"); al != nil {
		t.Errorf("one tool called repeatedly was reported as a cycle: %s", al.Reason)
	}
}

// TestStreamingLoop_ForwardedAggregateCountsOnce is #363's gate at the
// same level: the shape where chunk and aggregate share a part must
// not count twice either. One call short of the threshold must stay
// quiet — double-counting shows up here as an alert at half the
// configured repeat count, which is exactly how #363 presented.
func TestStreamingLoop_ForwardedAggregateCountsOnce(t *testing.T) {
	t.Parallel()
	alerts := runStreamingLoop(t, int32(watchdog.DefaultRepeatThreshold)-1, false)
	if al := alertBySignal(alerts, "repeated-tool-call"); al != nil {
		t.Errorf("%d calls tripped a %d-call threshold, so a call was counted more than once: %s",
			watchdog.DefaultRepeatThreshold-1, watchdog.DefaultRepeatThreshold, al.Reason)
	}
	// And the detector is not simply dead: one more call trips it.
	alerts = runStreamingLoop(t, int32(watchdog.DefaultRepeatThreshold), false)
	if alertBySignal(alerts, "repeated-tool-call") == nil {
		t.Errorf("%d identical calls raised no repeated-tool-call; got %v",
			watchdog.DefaultRepeatThreshold, signals(alerts))
	}
}

func signals(alerts []watchdog.Alert) []string {
	out := make([]string, 0, len(alerts))
	for _, al := range alerts {
		out = append(out, al.Signal)
	}
	return out
}
