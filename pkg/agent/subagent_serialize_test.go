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
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// subagentGauge records how many inner subagent runs are in flight at
// once, and how many ran at all.
type subagentGauge struct {
	inFlight atomic.Int32
	max      atomic.Int32
	ran      atomic.Int32
}

func (g *subagentGauge) enter() {
	g.ran.Add(1)
	cur := g.inFlight.Add(1)
	for {
		prev := g.max.Load()
		if cur <= prev || g.max.CompareAndSwap(prev, cur) {
			break
		}
	}
}

func (g *subagentGauge) leave() { g.inFlight.Add(-1) }

// gaugedLLM is an inner subagent's model: it records its own overlap
// window, dwells long enough for a concurrent sibling to be observed,
// then answers.
type gaugedLLM struct{ g *subagentGauge }

func (gaugedLLM) Name() string { return "gauged" }

func (m gaugedLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.g.enter()
		time.Sleep(150 * time.Millisecond)
		m.g.leave()
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ok"}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// delegatingFanoutLLM is fanoutLLM with the one argument a subagent
// tool's schema requires. The bare fanoutLLM would have every call
// rejected by schema validation before reaching a subagent, which
// reads as "no overlap" and would make this test pass for the wrong
// reason — hence gauge.ran below.
type delegatingFanoutLLM struct {
	calls atomic.Int32
	fns   []string
}

func (*delegatingFanoutLLM) Name() string { return "delegating-fanout" }

func (f *delegatingFanoutLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	n := f.calls.Add(1)
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if n == 1 {
			parts := make([]*genai.Part, 0, len(f.fns))
			for i, name := range f.fns {
				parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{
					ID: name + "-" + string(rune('a'+i)), Name: name,
					Args: map[string]any{"request": "write something"},
				}})
			}
			yield(&adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: parts}}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}},
			TurnComplete: true,
		}, nil)
	}
}

// Two synchronous subagent delegations fanned out in ONE model response
// do not overlap, and the reason is #460: WithSubagents appends the
// subagent tools to the parent's tool list BEFORE SerializeMutating
// wraps it, so a delegation is just another mutating call on the
// parent's one serializer.
//
// This is load-bearing for #653. The parallel-write guard in
// pkg/agent/background refuses concurrent write-capable BACKGROUND
// subagents and says nothing about synchronous ones — which is only
// correct while this holds. Wire the subagent tools after the wrap and
// the guard silently develops a hole on the other door.
//
// The control for "can this harness see overlap at all" is
// TestDispatch_SerializesMutatingKeepsReadOnlyConcurrent next door: its
// cross-barrier construction requires a read-only tool to enter while a
// mutating one holds the lock, so a harness that serialized everything
// would fail there rather than pass quietly here.
func TestConcurrentSyncSubagentDelegationsDoNotOverlap(t *testing.T) {
	g := &subagentGauge{}
	h, cleanup := openTestEventLog(t)
	t.Cleanup(cleanup)

	mk := func(name string) *Agent {
		a, err := New(gaugedLLM{g: g}, WithName(name), WithEventLog(h), WithSession("u", "s-"+name))
		if err != nil {
			t.Fatalf("New %s: %v", name, err)
		}
		return a
	}
	parent, err := New(&delegatingFanoutLLM{fns: []string{"writer_a", "writer_b"}},
		WithEventLog(h), WithSession("u", "parent"),
		WithSubagents([]*Agent{mk("writer_a"), mk("writer_b")}))
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}
	for _, err := range parent.Run(context.Background(), "go") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	// Non-vacuity first: a schema rejection, a gate denial or a renamed
	// tool would all leave the gauge at zero and the overlap assertion
	// trivially satisfied.
	if ran := g.ran.Load(); ran != 2 {
		t.Fatalf("expected both subagents to run, got %d — the overlap check below would be vacuous", ran)
	}
	if max := g.max.Load(); max != 1 {
		t.Errorf("max concurrent subagent runs = %d, want 1: two synchronous delegations reached the "+
			"shared working directory at the same instant. Check that WithSubagents still appends to "+
			"o.tools BEFORE SerializeMutating wraps them (#460), and widen the #653 background guard "+
			"to the synchronous door if it does not", max)
	}
}
