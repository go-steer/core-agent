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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
)

// Default windows for the double-Ctrl+C exit gesture and the
// background polling loop that lets the key-reader goroutine notice
// when Close has been called. Both are var (not const) so the unit
// tests can shrink them.
var (
	ctrlCExitWindow = 1 * time.Second
	keyReadTimeout  = 100 * time.Millisecond
)

const (
	keyESC   byte = 0x1b
	keyCtrlC byte = 0x03
)

// turnInterrupter owns the per-turn ESC + Ctrl+C capture for an
// interactive REPL turn. Created once per turn (cheap), Started
// before the turn fires, Closed after.
//
// Lifetime:
//
//	i, err := newTurnInterrupter(os.Stdin, os.Stderr)
//	if err != nil { ... fall back to legacy behavior ... }
//	turnCtx, cancel, err := i.Start(parentCtx)
//	defer cancel()
//	defer i.Close()
//	// ... run the turn against turnCtx ...
//	if i.Interrupted() { /* show "✕ interrupted" */ }
//	if i.ExitRequested() { /* return from REPL */ }
//
// The Start/Close split lets us defer Close even when Start fails
// partway through.
type turnInterrupter struct {
	stdin  *os.File
	stderr io.Writer

	// now is the clock for double-Ctrl+C window tracking. Mocked
	// in tests.
	now func() time.Time

	// State guarded by mu.
	mu            sync.Mutex
	prevState     *term.State
	started       bool
	closed        bool
	turnCancel    context.CancelFunc
	interrupted   bool
	exitRequested bool
	firstCtrlCAt  time.Time
	hintPrinted   bool
	stopCh        chan struct{} // closed by Close to unblock the key goroutine
	goroutineDone chan struct{} // closed by the goroutine when it returns
}

// ErrNotTerminal is returned when newTurnInterrupter is called with
// a stdin that isn't a terminal. The REPL falls back to legacy
// behavior in that case (no mid-turn cancel; the existing process-
// level SIGINT continues to mean "exit").
var ErrNotTerminal = errors.New("runner: stdin is not a terminal; turnInterrupter unavailable")

// newTurnInterrupter constructs an interrupter for one turn. Returns
// ErrNotTerminal when stdin isn't a TTY; the caller falls back to
// the legacy no-interrupter path.
func newTurnInterrupter(stdin *os.File, stderr io.Writer) (*turnInterrupter, error) {
	if stdin == nil {
		return nil, errors.New("runner: turnInterrupter: stdin is required")
	}
	if !term.IsTerminal(int(stdin.Fd())) {
		return nil, ErrNotTerminal
	}
	return &turnInterrupter{
		stdin:  stdin,
		stderr: stderr,
		now:    time.Now,
	}, nil
}

// Start enters raw terminal mode (so we can read individual ESC and
// Ctrl+C bytes), spawns the key-reader goroutine, and returns a ctx
// derived from parent that gets cancelled whenever the user presses
// ESC or single-Ctrl+C. The returned cancel must be called when the
// turn ends so the goroutine can exit promptly.
//
// On any failure entering raw mode, returns the error and the
// interrupter remains in an un-started state (Close is a no-op).
func (i *turnInterrupter) Start(parent context.Context) (context.Context, context.CancelFunc, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.started {
		return nil, nil, errors.New("runner: turnInterrupter: already started")
	}
	if i.closed {
		return nil, nil, errors.New("runner: turnInterrupter: already closed")
	}

	prev, err := term.MakeRaw(int(i.stdin.Fd()))
	if err != nil {
		return nil, nil, fmt.Errorf("runner: turnInterrupter: enter raw mode: %w", err)
	}
	i.prevState = prev

	turnCtx, cancel := context.WithCancel(parent)
	i.turnCancel = cancel
	i.stopCh = make(chan struct{})
	i.goroutineDone = make(chan struct{})
	i.started = true

	go i.readLoop()

	return turnCtx, cancel, nil
}

// Close restores the terminal mode, signals the key goroutine to
// exit, and waits for it. Idempotent.
func (i *turnInterrupter) Close() error {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return nil
	}
	i.closed = true
	if !i.started {
		// Started failed or was never called; nothing to tear down.
		i.mu.Unlock()
		return nil
	}
	prev := i.prevState
	stopCh := i.stopCh
	doneCh := i.goroutineDone
	i.mu.Unlock()

	// Signal the goroutine to exit.
	close(stopCh)
	// Restore terminal mode FIRST so even if the goroutine is wedged
	// on a Read, the user's terminal is usable again. The goroutine
	// will exit on its next deadline tick.
	if prev != nil {
		_ = term.Restore(int(i.stdin.Fd()), prev)
	}
	// Best-effort wait for the goroutine to exit, with a small
	// timeout so we don't block the REPL loop if Read is stuck.
	select {
	case <-doneCh:
	case <-time.After(2 * keyReadTimeout):
	}
	return nil
}

// Interrupted reports whether ESC or single-Ctrl+C was observed
// during this turn. The REPL uses this to render the "✕ interrupted"
// line. Safe for concurrent callers.
func (i *turnInterrupter) Interrupted() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.interrupted
}

// ExitRequested reports whether two Ctrl+C presses landed within
// ctrlCExitWindow of each other. The REPL uses this to break out of
// its loop after the current turn unwinds. Safe for concurrent
// callers.
func (i *turnInterrupter) ExitRequested() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.exitRequested
}

// readLoop is the background goroutine that reads single bytes from
// stdin and reacts to ESC / Ctrl+C. Exits when stopCh closes or a
// read fails. Uses SetReadDeadline so it can periodically check
// stopCh even while no input arrives.
func (i *turnInterrupter) readLoop() {
	defer close(i.goroutineDone)
	buf := make([]byte, 1)
	for {
		select {
		case <-i.stopCh:
			return
		default:
		}
		_ = i.stdin.SetReadDeadline(i.now().Add(keyReadTimeout))
		n, err := i.stdin.Read(buf)
		if err != nil {
			// Read deadline expired → loop and re-check stopCh.
			// Any other error means the file is gone; bail out.
			if isDeadlineErr(err) {
				continue
			}
			return
		}
		if n == 0 {
			continue
		}
		i.handleByte(buf[0])
	}
}

// handleByte processes one byte from stdin. Public-ish (lowercase,
// same package) so tests can exercise the state machine directly
// without an actual terminal.
func (i *turnInterrupter) handleByte(b byte) {
	switch b {
	case keyESC:
		// Leading \r\x1b[K resets cursor to column 0 and clears to
		// end of line so the banner lands cleanly even when the
		// model's streaming text left the cursor mid-line. Trailing
		// \n then advances to a fresh line before the REPL's next
		// "> " prompt is written.
		i.markInterrupted("\r\x1b[K\x1b[33m✕ interrupted\x1b[0m\n")
	case keyCtrlC:
		i.handleCtrlC()
	default:
		// Ignore all other keys during a turn — the user can't type
		// useful input until the turn ends anyway, and consuming
		// random keystrokes silently is the right UX (we're in raw
		// mode so echo is off).
	}
}

// handleCtrlC implements the double-Ctrl+C exit gesture. First press
// cancels the turn and arms a 1-second window; second press within
// the window sets ExitRequested. A press outside the window resets
// the window and behaves like a first press again.
func (i *turnInterrupter) handleCtrlC() {
	i.mu.Lock()
	now := i.now()
	firstWindowActive := !i.firstCtrlCAt.IsZero() && now.Sub(i.firstCtrlCAt) < ctrlCExitWindow
	if firstWindowActive {
		i.exitRequested = true
		i.mu.Unlock()
		// Cancel the turn so the REPL loop returns promptly.
		i.cancelTurn()
		return
	}
	i.firstCtrlCAt = now
	i.interrupted = true
	i.mu.Unlock()
	i.cancelTurn()
	i.printHintOnce()
}

// markInterrupted records that the turn was cancelled by the user,
// emits banner to stderr, and cancels the turn ctx.
func (i *turnInterrupter) markInterrupted(banner string) {
	i.mu.Lock()
	already := i.interrupted
	i.interrupted = true
	i.mu.Unlock()
	if !already && banner != "" {
		_, _ = io.WriteString(i.stderr, banner)
	}
	i.cancelTurn()
}

// cancelTurn invokes the saved turnCancel func, safely under the
// mutex. No-op when Start hasn't run.
func (i *turnInterrupter) cancelTurn() {
	i.mu.Lock()
	cancel := i.turnCancel
	i.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// printHintOnce writes the "press Ctrl+C again to exit" hint to
// stderr exactly once per interrupter (per turn). Subsequent
// Ctrl+Cs reset the window but don't re-print the hint — the user
// already knows.
func (i *turnInterrupter) printHintOnce() {
	i.mu.Lock()
	if i.hintPrinted {
		i.mu.Unlock()
		return
	}
	i.hintPrinted = true
	i.mu.Unlock()
	// \r\x1b[K to reset to column 0 + clear-line, then the banner.
	_, _ = fmt.Fprintf(i.stderr, "\r\x1b[K\x1b[33m✕ interrupted\x1b[0m \x1b[2m(press Ctrl+C again within %s to exit)\x1b[0m\n",
		ctrlCExitWindow)
}

// isDeadlineErr classifies whether an os.File read error is the
// expected deadline-exceeded case. Wraps the stdlib check.
func isDeadlineErr(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded)
}
