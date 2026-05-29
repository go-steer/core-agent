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

package runner

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestInterrupter builds a turnInterrupter without touching the
// real terminal. Replaces the cancel function with a counter and the
// clock with a controllable one so the state machine can be tested
// in isolation from terminal mode handling and wall-clock timing.
func newTestInterrupter() (*turnInterrupter, *bytes.Buffer, *int32, *fakeClock) {
	stderr := &bytes.Buffer{}
	clk := &fakeClock{now: time.Unix(0, 0)}
	var cancelCount int32
	i := &turnInterrupter{
		stderr:     stderr,
		now:        clk.Now,
		turnCancel: func() { atomic.AddInt32(&cancelCount, 1) },
	}
	// Pretend Start ran successfully so cancelTurn has a non-nil
	// func to call and so handleByte doesn't no-op.
	i.started = true
	return i, stderr, &cancelCount, clk
}

type fakeClock struct {
	mu  testingMutex
	now time.Time
}

// testingMutex is a tiny shim so the fake clock plays nice with the
// race detector across goroutines (none of our tests actually
// concurrent the clock, but we keep the door open).
type testingMutex struct{ _ [0]byte }

func (m *testingMutex) Lock()   {}
func (m *testingMutex) Unlock() {}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestTurnInterrupter_ESCCancelsAndMarksInterrupted(t *testing.T) {
	t.Parallel()
	i, stderr, cancels, _ := newTestInterrupter()
	i.handleByte(keyESC)
	if !i.Interrupted() {
		t.Errorf("ESC should set Interrupted")
	}
	if i.ExitRequested() {
		t.Errorf("ESC should not set ExitRequested")
	}
	if atomic.LoadInt32(cancels) != 1 {
		t.Errorf("ESC should cancel exactly once; got %d", atomic.LoadInt32(cancels))
	}
	if !strings.Contains(stderr.String(), "interrupted") {
		t.Errorf("ESC should write interrupted banner; got %q", stderr.String())
	}
}

func TestTurnInterrupter_SingleCtrlCCancelsAndShowsHint(t *testing.T) {
	t.Parallel()
	i, stderr, cancels, _ := newTestInterrupter()
	i.handleByte(keyCtrlC)
	if !i.Interrupted() {
		t.Errorf("Ctrl+C should set Interrupted")
	}
	if i.ExitRequested() {
		t.Errorf("single Ctrl+C should not set ExitRequested")
	}
	if atomic.LoadInt32(cancels) != 1 {
		t.Errorf("single Ctrl+C should cancel once; got %d", atomic.LoadInt32(cancels))
	}
	if !strings.Contains(stderr.String(), "press Ctrl+C again") {
		t.Errorf("single Ctrl+C should print the exit hint; got %q", stderr.String())
	}
}

func TestTurnInterrupter_DoubleCtrlCWithinWindowRequestsExit(t *testing.T) {
	t.Parallel()
	i, _, cancels, clk := newTestInterrupter()
	i.handleByte(keyCtrlC)
	clk.Advance(ctrlCExitWindow / 2)
	i.handleByte(keyCtrlC)
	if !i.ExitRequested() {
		t.Errorf("two Ctrl+C within window should set ExitRequested")
	}
	if atomic.LoadInt32(cancels) != 2 {
		t.Errorf("two Ctrl+C should cancel twice; got %d", atomic.LoadInt32(cancels))
	}
}

func TestTurnInterrupter_DoubleCtrlCOutsideWindowDoesNotExit(t *testing.T) {
	t.Parallel()
	i, stderr, cancels, clk := newTestInterrupter()
	i.handleByte(keyCtrlC)
	clk.Advance(ctrlCExitWindow + 100*time.Millisecond)
	i.handleByte(keyCtrlC)
	if i.ExitRequested() {
		t.Errorf("Ctrl+C outside window should NOT set ExitRequested")
	}
	if atomic.LoadInt32(cancels) != 2 {
		t.Errorf("expected 2 cancels (both treated as single); got %d", atomic.LoadInt32(cancels))
	}
	// Hint should appear once (set on first Ctrl+C, not reset on
	// the second-outside-window press).
	hintCount := strings.Count(stderr.String(), "press Ctrl+C again")
	if hintCount != 1 {
		t.Errorf("hint should print exactly once across both Ctrl+Cs; got %d", hintCount)
	}
}

func TestTurnInterrupter_OtherKeysIgnored(t *testing.T) {
	t.Parallel()
	i, stderr, cancels, _ := newTestInterrupter()
	for _, b := range []byte{'a', 'A', '?', '\n', '\r', 0x7f /* DEL */, 0x09 /* tab */} {
		i.handleByte(b)
	}
	if i.Interrupted() {
		t.Errorf("random keys should not set Interrupted")
	}
	if atomic.LoadInt32(cancels) != 0 {
		t.Errorf("random keys should not cancel; got %d", atomic.LoadInt32(cancels))
	}
	if stderr.Len() != 0 {
		t.Errorf("random keys should not write anything; got %q", stderr.String())
	}
}

func TestTurnInterrupter_BannerEmittedOncePerESCBurst(t *testing.T) {
	t.Parallel()
	// Multiple ESCs during one turn (which the model can't usefully
	// respond to anyway, since the cancel already fired on the
	// first) should not spam the user.
	i, stderr, _, _ := newTestInterrupter()
	for j := 0; j < 5; j++ {
		i.handleByte(keyESC)
	}
	bannerCount := strings.Count(stderr.String(), "interrupted")
	if bannerCount != 1 {
		t.Errorf("ESC banner should print once across multiple presses; got %d", bannerCount)
	}
}

func TestTurnInterrupter_HintPrintedOncePerTurn(t *testing.T) {
	t.Parallel()
	i, stderr, _, clk := newTestInterrupter()
	// First Ctrl+C → prints hint.
	i.handleByte(keyCtrlC)
	// Advance past window, press Ctrl+C again — should NOT print
	// hint again (avoid nag spam).
	clk.Advance(2 * ctrlCExitWindow)
	i.handleByte(keyCtrlC)
	hintCount := strings.Count(stderr.String(), "press Ctrl+C again")
	if hintCount != 1 {
		t.Errorf("hint should print once per interrupter lifetime; got %d", hintCount)
	}
}

func TestNewTurnInterrupter_RejectsNonTTY(t *testing.T) {
	t.Parallel()
	// os.Pipe gives us a non-TTY *os.File — newTurnInterrupter
	// should return ErrNotTerminal so the REPL falls back.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	_, err = newTurnInterrupter(r, &bytes.Buffer{})
	if !errors.Is(err, ErrNotTerminal) {
		t.Errorf("expected ErrNotTerminal; got %v", err)
	}
}

func TestNewTurnInterrupter_RejectsNilStdin(t *testing.T) {
	t.Parallel()
	_, err := newTurnInterrupter(nil, &bytes.Buffer{})
	if err == nil {
		t.Errorf("expected error for nil stdin")
	}
}

func TestTurnInterrupter_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	i, _, _, _ := newTestInterrupter()
	// Mark not actually started so Close takes the no-tear-down path
	// (avoids needing a real stop channel + goroutine).
	i.started = false
	if err := i.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := i.Close(); err != nil {
		t.Errorf("second Close should be no-op; got %v", err)
	}
}
