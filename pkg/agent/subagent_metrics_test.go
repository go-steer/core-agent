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
	"sync"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// fnCallThenDoneLLM drives exactly one subagent dispatch: its first
// GenerateContent emits a function call to `target`, and every later
// call emits plain final text so the parent turn terminates once the
// tool response comes back.
type fnCallThenDoneLLM struct {
	target string
	mu     sync.Mutex
	calls  int
}

func (l *fnCallThenDoneLLM) Name() string { return "fncall" }

func (l *fnCallThenDoneLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	n := l.calls
	l.calls++
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if n == 0 {
			yield(&adkmodel.LLMResponse{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{{
						FunctionCall: &genai.FunctionCall{
							Name: l.target,
							Args: map[string]any{"request": "do the thing"},
						},
					}},
				},
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: "done"}},
			},
			TurnComplete: true,
		}, nil)
	}
}

// TestMeterProvider_Accessor pins the accessor that subagent
// construction sites read to inherit the parent's resolved provider:
// it returns the WithMeterProvider value, and nil-degrades for
// hand-constructed agents.
func TestMeterProvider_Accessor(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	a, err := New(minimalLLM{}, WithMeterProvider(mp))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.MeterProvider() != mp {
		t.Errorf("MeterProvider() did not return the WithMeterProvider value")
	}

	// A nil receiver is safe (mirrors the rest of the accessor set).
	var nilAgent *Agent
	if nilAgent.MeterProvider() != nil {
		t.Errorf("nil-agent MeterProvider() = non-nil, want nil")
	}
}

// TestSubagentTool_RecordsInvocationDuration is the #338 regression:
// a synchronously-invoked subagent drives the inner ADK runner
// directly (bypassing the inner *Agent.Run wrapper that owns
// recordInvocation), so without the handler recording it itself the
// subagent's turn never lands in gen_ai.agent.invocation.duration.
// The inner agent carries its OWN meter provider so this asserts the
// point on the subagent's histogram, isolated from the parent's turn.
// Fails on pre-change code (zero points on the inner provider).
func TestSubagentTool_RecordsInvocationDuration(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	// The inner subagent gets its own reader so we can assert its
	// invocation point without the parent's turn contaminating it.
	childReader := sdkmetric.NewManualReader()
	childMP := sdkmetric.NewMeterProvider(sdkmetric.WithReader(childReader))
	t.Cleanup(func() { _ = childMP.Shutdown(context.Background()) })

	prov := mock.NewEcho()
	childLLM, err := prov.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("provider.Model: %v", err)
	}
	child, err := New(childLLM,
		WithName("child"),
		WithEventLog(h),
		WithSession("u", "child"),
		WithMeterProvider(childMP),
	)
	if err != nil {
		t.Fatalf("New child: %v", err)
	}

	parent, err := New(&fnCallThenDoneLLM{target: "child"},
		WithName("parent"),
		WithEventLog(h),
		WithSession("u", "parent"),
		WithSubagents([]*Agent{child}),
	)
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}

	for _, err := range parent.Run(context.Background(), "go") {
		if err != nil {
			t.Fatalf("parent.Run: %v", err)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := childReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	pts := invocationPoints(t, rm)
	if len(pts) != 1 {
		t.Fatalf("got %d invocation points on the subagent provider, want 1 (subagent turn never recorded — #338)", len(pts))
	}
	if pts[0].Count != 1 {
		t.Errorf("count = %d, want 1", pts[0].Count)
	}
	if name, _ := invAttr(pts[0].Attributes, AttrGenAIAgentName); name != "child" {
		t.Errorf("%s = %q, want child (bounded by the subagent's own name)", AttrGenAIAgentName, name)
	}
	// A clean run carries no error.type.
	if _, present := invAttr(pts[0].Attributes, AttrErrorType); present {
		t.Errorf("error.type present on a successful subagent turn")
	}
}
