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
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// ctxCaptureLLM records the context it was invoked with so tests can
// assert on the per-turn context's lifecycle. Yields one final text
// response.
type ctxCaptureLLM struct {
	mu  sync.Mutex
	ctx context.Context
}

func (l *ctxCaptureLLM) Name() string { return "ctxcap" }

func (l *ctxCaptureLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	l.ctx = ctx
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}},
			TurnComplete: true,
		}, nil)
	}
}

func (l *ctxCaptureLLM) capturedCtx() context.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ctx
}

// TestAgent_Run_CancelsPerTurnContextOnCompletion is the #359
// leak regression: an uninterrupted turn must release its per-turn
// cancellable context on cleanup. Before the fix only Interrupt()
// called cancel(), so every completed turn leaked a live cancellable
// child of the process-lifetime parent ctx (classic lostcancel).
func TestAgent_Run_CancelsPerTurnContextOnCompletion(t *testing.T) {
	t.Parallel()

	llm := &ctxCaptureLLM{}
	a, err := New(llm, WithSession("u-leak", "s-leak"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	// Parent ctx that stays alive for the whole test — proves the
	// cancellation comes from cleanup, not from the parent.
	parent, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	for _, err := range a.Run(parent, "hi") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	turnCtx := llm.capturedCtx()
	if turnCtx == nil {
		t.Fatal("model was never invoked; cannot assert on per-turn ctx")
	}
	select {
	case <-turnCtx.Done():
		// expected: cleanup cancelled the per-turn context tree.
	default:
		t.Errorf("per-turn context still live after turn completion (lostcancel leak)")
	}
	if a.turnInFlight() {
		t.Errorf("cancelInFlight not cleared after turn completion")
	}
}

// TestAgent_ClearCancelInFlight_StaleCleanupDoesNotClobber is the
// #359 generation regression: a late-firing older-turn cleanup must
// NOT clear a newer turn's registered cancel. The old pointer-identity
// guard (reflect.Value.Pointer on a CancelFunc) compared every cancel
// as equal, so a stale cleanup silently clobbered the live turn's
// cancel and Interrupt() became a no-op.
func TestAgent_ClearCancelInFlight_StaleCleanupDoesNotClobber(t *testing.T) {
	t.Parallel()

	provider := mock.NewEcho()
	m, err := provider.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	a, err := New(m)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	// Turn A registers its cancel, then a follow-up turn B replaces it.
	_, cancelA := context.WithCancel(context.Background())
	genA := a.setCancelInFlight(cancelA)
	turnB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	_ = a.setCancelInFlight(cancelB)

	// Turn A's cleanup fires LATE (after B started). It must be a no-op
	// against B's generation.
	a.clearCancelInFlight(genA)

	// Interrupt must still fire turn B's cancel.
	if !a.Interrupt() {
		t.Fatalf("Interrupt was a no-op after a stale cleanup; turn B's cancel was clobbered (#359)")
	}
	select {
	case <-turnB.Done():
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Errorf("turn B ctx not cancelled by Interrupt within 100ms")
	}
}

func TestAgent_Interrupt_NoOpWhenIdle(t *testing.T) {
	t.Parallel()

	provider := mock.NewEcho()
	m, err := provider.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	a, err := New(m)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	// No turn in flight → Interrupt is a clean no-op returning false.
	if got := a.Interrupt(); got {
		t.Errorf("Interrupt on idle agent returned true, want false")
	}
	// Second call also a no-op (defensive; the underlying cancel
	// was already nilled out by the first call).
	if got := a.Interrupt(); got {
		t.Errorf("second Interrupt on idle agent returned true, want false")
	}
}

func TestAgent_Interrupt_CancelsInFlightContext(t *testing.T) {
	t.Parallel()

	provider := mock.NewEcho()
	m, err := provider.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	a, err := New(m)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	// Stage a cancel func directly (the wrapping pattern from
	// Run() — we don't drive a full turn here because the echo
	// provider returns immediately and there's no opportunity to
	// race in an interrupt before the iterator completes). What
	// we're testing: Interrupt() invokes the stored cancel and
	// reports true.
	parent, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()
	turnCtx, turnCancel := context.WithCancel(parent)
	a.setCancelInFlight(turnCancel)

	if got := a.Interrupt(); !got {
		t.Errorf("Interrupt with stored cancel returned false, want true")
	}
	select {
	case <-turnCtx.Done():
		// expected: ctx is now canceled
	case <-time.After(100 * time.Millisecond):
		t.Errorf("turnCtx not canceled within 100ms of Interrupt")
	}
	// And the stored cancel is now cleared — a second Interrupt
	// is a no-op.
	if got := a.Interrupt(); got {
		t.Errorf("second Interrupt returned true; want stored cancel cleared after first")
	}
}
