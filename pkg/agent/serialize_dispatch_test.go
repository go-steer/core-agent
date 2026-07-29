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
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

// fanoutLLM emits ONE model response carrying several FunctionCall
// parts (the shape ADK fans out concurrently), then answers the
// tool-results round with plain text. This drives the REAL dispatch
// path — llminternal's goroutine-per-call — through agent.New's #460
// serialization wrapping.
type fanoutLLM struct {
	calls atomic.Int32
	fns   []string
}

func (f *fanoutLLM) Name() string { return "fanout" }

func (f *fanoutLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	n := f.calls.Add(1)
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if n == 1 {
			parts := make([]*genai.Part, 0, len(f.fns))
			for i, name := range f.fns {
				parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{
					ID: name + "-" + string(rune('a'+i)), Name: name, Args: map[string]any{},
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

// TestDispatch_SerializesMutatingKeepsReadOnlyConcurrent is the #460
// contract test, driven through real ADK dispatch. One response fans
// out two mutating stubs (unlisted names — proving the fail-safe
// default) and one read-only tool (named "grep" — proving the
// builtin table). Cross-barrier construction makes both properties
// deterministic rather than timing-sniffed:
//
//   - mut_a enters first (holds the mutation lock), then BLOCKS until
//     the read-only tool has provably entered. If read-only tools
//     were wrongly serialized on the same lock, grep could never
//     enter while mut_a holds it and mut_a would time out — failing
//     loudly instead of passing vacuously.
//   - an atomic in-flight gauge across both mutating stubs must never
//     exceed 1 — the mutual-exclusion guarantee itself.
func TestDispatch_SerializesMutatingKeepsReadOnlyConcurrent(t *testing.T) {
	t.Parallel()

	var (
		mutInFlight    atomic.Int32
		mutMaxInFlight atomic.Int32
		mutStarted     = make(chan struct{})
		grepEntered    = make(chan struct{})
		startOnce      sync.Once
		grepOnce       sync.Once
		mutATimedOut   atomic.Bool
	)
	type empty struct{}
	mkMut := func(name string, first bool) tool.Tool {
		tl, err := functiontool.New(functiontool.Config{Name: name, Description: "mutating stub"},
			func(_ tool.Context, _ empty) (empty, error) {
				cur := mutInFlight.Add(1)
				for {
					prev := mutMaxInFlight.Load()
					if cur <= prev || mutMaxInFlight.CompareAndSwap(prev, cur) {
						break
					}
				}
				startOnce.Do(func() { close(mutStarted) })
				if first {
					select {
					case <-grepEntered:
					case <-time.After(5 * time.Second):
						mutATimedOut.Store(true)
					}
				}
				time.Sleep(10 * time.Millisecond)
				mutInFlight.Add(-1)
				return empty{}, nil
			})
		if err != nil {
			t.Fatalf("functiontool.New(%s): %v", name, err)
		}
		return tl
	}
	grepTool, err := functiontool.New(functiontool.Config{Name: "grep", Description: "read-only stub"},
		func(_ tool.Context, _ empty) (empty, error) {
			select {
			case <-mutStarted: // wait until a mutating tool provably holds the lock
			case <-time.After(5 * time.Second):
			}
			grepOnce.Do(func() { close(grepEntered) })
			return empty{}, nil
		})
	if err != nil {
		t.Fatalf("functiontool.New(grep): %v", err)
	}

	// mut_a must be the FIRST mutating tool to hold the lock for the
	// barrier logic; dispatch order isn't guaranteed, so make both
	// mutating stubs symmetric: whichever enters first performs the
	// wait. Simplest: both are "first" via the once-guarded barrier —
	// only the first entrant's select actually waits (the second
	// entrant can't enter until the lock frees, by which time
	// grepEntered is closed and its select returns immediately).
	m := &fanoutLLM{fns: []string{"mut_a", "mut_b", "grep"}}
	a, err := New(m, WithTools([]tool.Tool{mkMut("mut_a", true), mkMut("mut_b", true), grepTool}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		for _, err := range a.Run(context.Background(), "go") {
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("turn never completed — dispatch deadlocked")
	}

	if mutATimedOut.Load() {
		t.Fatal("read-only tool never ran while a mutating tool held the lock — read-only dispatch is wrongly serialized")
	}
	if max := mutMaxInFlight.Load(); max != 1 {
		t.Fatalf("mutating in-flight max = %d, want exactly 1 — parallel mutations can race and corrupt state (#460)", max)
	}
}
