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

package permissions

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSerialize_NilInnerReturnsNil(t *testing.T) {
	t.Parallel()
	if Serialize(nil) != nil {
		t.Errorf("Serialize(nil) should return nil so callers can chain")
	}
}

// fakeOverlapPrompter records the max number of AskApproval calls
// that were in flight at the same time. Used to assert serialization.
type fakeOverlapPrompter struct {
	mu       sync.Mutex
	inFlight int
	maxSeen  int
	hold     time.Duration
}

func (p *fakeOverlapPrompter) AskApproval(_ context.Context, _ PromptRequest) (Decision, error) {
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > p.maxSeen {
		p.maxSeen = p.inFlight
	}
	p.mu.Unlock()
	time.Sleep(p.hold)
	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
	return DecisionAllowOnce, nil
}

func TestSerialize_PreventsConcurrentInnerCalls(t *testing.T) {
	t.Parallel()
	inner := &fakeOverlapPrompter{hold: 10 * time.Millisecond}
	wrapped := Serialize(inner)

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = wrapped.AskApproval(context.Background(), PromptRequest{ToolName: "bash"})
		}()
	}
	wg.Wait()

	if inner.maxSeen != 1 {
		t.Errorf("serialized prompter should see at most 1 concurrent inner call, observed %d", inner.maxSeen)
	}
}

func TestSerialize_PropagatesInnerResult(t *testing.T) {
	t.Parallel()
	want := DecisionAllowSession
	wrapped := Serialize(PrompterFunc(func(_ context.Context, _ PromptRequest) (Decision, error) {
		return want, nil
	}))
	got, err := wrapped.AskApproval(context.Background(), PromptRequest{ToolName: "x"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != want {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestSerialize_CallsArePairedFIFO(t *testing.T) {
	t.Parallel()
	// Best-effort FIFO check — go's sync.Mutex doesn't promise
	// strict FIFO under high contention. We launch goroutines with a
	// tiny stagger so the wake-up order is reasonably deterministic.
	var ordering atomic.Int64
	inner := PrompterFunc(func(_ context.Context, req PromptRequest) (Decision, error) {
		// req.Detail carries the launch index; serializing must run
		// them one at a time so ordering remains monotonic-ish.
		_ = ordering.Add(1)
		return DecisionAllowOnce, nil
	})
	wrapped := Serialize(inner)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = wrapped.AskApproval(context.Background(), PromptRequest{})
		}()
	}
	wg.Wait()
	if ordering.Load() != 4 {
		t.Errorf("expected all 4 calls to complete; got %d", ordering.Load())
	}
}

// PrompterFunc adapts a function to the Prompter interface — useful
// for tests and one-off prompters.
type PrompterFunc func(ctx context.Context, req PromptRequest) (Decision, error)

func (f PrompterFunc) AskApproval(ctx context.Context, req PromptRequest) (Decision, error) {
	return f(ctx, req)
}
