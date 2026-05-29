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
	"testing"
	"time"

	"github.com/go-steer/core-agent/pkg/models/mock"
)

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

func TestAgent_AttachInterrupt_SameAsInterrupt(t *testing.T) {
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

	// Idle: both methods return false.
	if a.AttachInterrupt() {
		t.Errorf("AttachInterrupt on idle returned true, want false")
	}
	// With a cancel installed: both methods return true (only the
	// first one fires the cancel; AttachInterrupt and Interrupt
	// share the same implementation).
	_, c := context.WithCancel(context.Background())
	a.setCancelInFlight(c)
	if !a.AttachInterrupt() {
		t.Errorf("AttachInterrupt with cancel installed returned false, want true")
	}
}
