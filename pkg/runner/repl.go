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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"

	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/usage"
)

// REPL drives a multi-turn stdin loop against m. Each line is sent
// through the same Agent so the ADK runner accumulates conversation
// history across turns (the in-memory session service appends events
// per session ID, which the Agent reuses).
//
// Prompts are written to stdout, partial assistant text streams back
// inline, and tool-call summaries go to stderr — same shape as
// Headless. Built-in commands: `/exit`, `/quit`, EOF (Ctrl-D).
//
// agentOpts mirrors Headless. The same tracker/pricing pair is used
// across every turn so the final summary is meaningful. eventsOpts
// (e.g. WithColor) forward through to WriteEvents for every turn.
//
// SIGINT (Ctrl+C) is owned by the REPL when stdin is a TTY: the
// double-press-within-1s exits gesture works both during a turn
// (the per-turn turnInterrupter handles raw-mode bytes) and between
// turns (this function's signal handler manages the SIGINT state
// machine). The bundled CLI's main.go does NOT include SIGINT in
// its signal.NotifyContext for that reason; passing a ctx whose
// parent cancels on SIGINT would defeat the gesture.
func REPL(ctx context.Context, m adkmodel.LLM, stdin io.Reader, stdout, stderr io.Writer, tracker *usage.Tracker, pricing usage.Pricing, agentOpts []agent.Option, eventsOpts ...EventsOption) (int, error) {
	a, err := agent.New(m, agentOpts...)
	if err != nil {
		return ExitAgentError, err
	}
	return REPLWithAgent(ctx, a, m, stdin, stdout, stderr, tracker, pricing, eventsOpts...)
}

// REPLWithInitialPrompt behaves like REPL but seeds the first turn
// with initialPrompt before entering the main select loop (issue
// #291). The seed runs through the same runREPLTurn used mid-loop, so
// ESC interrupt, usage tracking, and tool-approval prompts all work
// identically to a user-typed submission.
//
// initialPrompt == "" is exactly equivalent to REPL — no seed, no
// behavior change. Prefer REPL when you don't need the seed.
func REPLWithInitialPrompt(ctx context.Context, m adkmodel.LLM, initialPrompt string, stdin io.Reader, stdout, stderr io.Writer, tracker *usage.Tracker, pricing usage.Pricing, agentOpts []agent.Option, eventsOpts ...EventsOption) (int, error) {
	a, err := agent.New(m, agentOpts...)
	if err != nil {
		return ExitAgentError, err
	}
	return replCore(ctx, a, m, initialPrompt, stdin, stdout, stderr, tracker, pricing, eventsOpts...)
}

// REPLWithAgent runs the REPL against a pre-constructed Agent. Useful
// for tests that need a reference to the agent (to call Inject from
// outside the loop, for example) and for library consumers that
// construct the Agent themselves. Equivalent to REPL minus the
// agent.New() call at the top.
func REPLWithAgent(ctx context.Context, a *agent.Agent, m adkmodel.LLM, stdin io.Reader, stdout, stderr io.Writer, tracker *usage.Tracker, pricing usage.Pricing, eventsOpts ...EventsOption) (int, error) {
	return replCore(ctx, a, m, "", stdin, stdout, stderr, tracker, pricing, eventsOpts...)
}

// replCore is the shared body of REPL / REPLWithAgent /
// REPLWithInitialPrompt. When initialPrompt is non-empty, it fires
// exactly one runREPLTurn before entering the stdin/wake select loop
// so the seed prompt behaves like a user-typed first submission
// (including ESC interrupt via the per-turn interrupter and cost
// accounting via the shared tracker).
func replCore(ctx context.Context, a *agent.Agent, m adkmodel.LLM, initialPrompt string, stdin io.Reader, stdout, stderr io.Writer, tracker *usage.Tracker, pricing usage.Pricing, eventsOpts ...EventsOption) (int, error) {
	// If a BackgroundAgentManager is wired, install an alert hook so
	// the human running the REPL sees subagent reports inline as
	// they arrive — same ↪ magenta sigil used for Gemini grounding
	// in WriteEvents. The hook is purely a side channel; the model
	// still receives the same alerts via Agent.Run's pre-turn drain.
	colorOn := eventsConfigFromOpts(eventsOpts).color
	if mgr := a.BackgroundManager(); mgr != nil {
		mgr.OnAlert(func(al agent.Alert) {
			line := FormatAlertLine(al.From, al.Kind, al.Text)
			_, _ = fmt.Fprintln(stderr, paint(line, ansiMagenta, colorOn))
		})
	}

	br := bufio.NewReader(stdin)
	// Detect whether stdin is a real terminal — we need *os.File for
	// the interrupter's raw-mode setup AND for the between-turn
	// SIGINT handler to make sense. If stdin is piped (Scanner,
	// pipe, bytes.Reader, etc.) or not a TTY, both fall back: no
	// mid-turn ESC, no double-Ctrl+C state machine. Single SIGINT
	// then ends the process via the existing default Go behavior
	// (or via the caller's own ctx if they wired one).
	stdinFile, _ := stdin.(*os.File)
	hasInterrupter := false
	if stdinFile != nil {
		probe, _ := newTurnInterrupter(stdinFile, stderr)
		if probe != nil {
			hasInterrupter = true
		}
	}

	banner := "core-agent REPL — /exit or Ctrl-D to quit"
	if hasInterrupter {
		banner = "core-agent REPL — ESC interrupts a turn, Ctrl+C twice exits, /exit or Ctrl-D quits"
	}
	fmt.Fprintln(stderr, banner)

	// REPL-scoped ctx so the between-turn SIGINT handler can cancel
	// independently of the parent.
	replCtx, replCancel := context.WithCancel(ctx)
	defer replCancel()

	// Between-turn SIGINT state machine. Only installed when we
	// have a real terminal; otherwise leave SIGINT to default Go
	// behavior (terminate at exit code 130) which is fine for
	// piped / scripted use.
	var sigState *betweenTurnSigState
	if hasInterrupter {
		sigState = newBetweenTurnSigState(stderr, replCancel)
		defer sigState.stop()
	}

	// Persistent stdin reader. Lives for the whole REPL session and
	// feeds one line per ReadString into stdinLines. Selecting on
	// this channel (instead of calling readLineCtx synchronously)
	// lets the main loop also react to Agent.WakeRequested() —
	// which fires whenever an external Inject lands while the REPL
	// is waiting between turns. Without this, a POST /inject
	// against a REPL-mode agent would queue silently and only get
	// processed when the local user happened to type something.
	stdinLines := make(chan stdinLine, 4)
	go func() {
		for {
			line, err := br.ReadString('\n')
			select {
			case stdinLines <- stdinLine{line: line, err: err}:
			case <-replCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// Seed the first turn from -i / --interactive-prompt before
	// entering the main select loop. The seed reuses runREPLTurn so
	// interrupt handling, usage tracking, and tool prompts behave
	// identically to a real submission — the operator sees the
	// initial prompt echo on the "> " prefix, the model responds,
	// then the loop hands control back to stdin/wake. Empty
	// initialPrompt is a no-op and matches the pre-seed REPL
	// behavior byte-for-byte. Slash + /exit shortcuts are refused
	// so a caller can't accidentally exit-before-start.
	if seed := strings.TrimSpace(initialPrompt); seed != "" {
		switch seed {
		case "/exit", "/quit":
			fmt.Fprintln(stderr, "core-agent: -i cannot be /exit or /quit")
			return ExitConfigError, nil
		}
		fmt.Fprintln(stdout, "> "+seed)
		exit, err := runREPLTurn(replCtx, a, m, stdinFile, seed, stdout, stderr, tracker, pricing, eventsOpts)
		if err != nil {
			fmt.Fprintf(stderr, "core-agent: %v\n", err)
			// A failed seed shouldn't tank the whole session — fall
			// through to the loop so the operator can retry.
		}
		if exit {
			return ExitOK, nil
		}
	}

	for {
		if err := replCtx.Err(); err != nil {
			return ExitOK, nil
		}
		fmt.Fprint(stdout, "> ")

		var prompt string
		var isWake bool
		select {
		case <-replCtx.Done():
			return ExitOK, nil
		case r := <-stdinLines:
			if r.err == io.EOF {
				fmt.Fprintln(stdout)
				return ExitOK, nil
			}
			if r.err != nil {
				return ExitAgentError, fmt.Errorf("runner: read stdin: %w", r.err)
			}
			prompt = strings.TrimSpace(r.line)
			if prompt == "" {
				continue
			}
			if prompt == "/exit" || prompt == "/quit" {
				return ExitOK, nil
			}
		case <-a.WakeRequested():
			// An external Inject (typically via attach-mode's
			// POST /inject) queued a message in the agent's inbox
			// AND fired wake. Run a turn with an empty prompt;
			// Agent.Run's pre-turn drain prepends every queued
			// inbox message via formatInboxForPrompt, so the
			// model sees them as a "[Inbox]" block. The local
			// "> " prompt we just printed is left in place — the
			// model output renders below it, then the next loop
			// iteration writes a fresh "> ".
			prompt = ""
			isWake = true
			fmt.Fprintln(stderr, "")
			fmt.Fprintln(stderr, paint("[wake] inbox arrived — processing", ansiCyan, colorOn))
		}

		// User has typed something useful — reset the between-turn
		// Ctrl+C window so a stale first-press from earlier doesn't
		// haunt them. Wake-driven turns don't count as user input
		// so we leave the arming intact.
		if !isWake && sigState != nil {
			sigState.reset()
		}
		exit, err := runREPLTurn(replCtx, a, m, stdinFile, prompt, stdout, stderr, tracker, pricing, eventsOpts)
		if err != nil {
			fmt.Fprintf(stderr, "core-agent: %v\n", err)
			// Don't exit on a single turn error — let the user retry.
			continue
		}
		if exit {
			return ExitOK, nil
		}
	}
}

// stdinLine is one ReadString result. Pulled out so the stdin-reader
// goroutine and the main loop's select can share a single channel
// element type without an anonymous struct.
type stdinLine struct {
	line string
	err  error
}

// runREPLTurn drives one REPL turn with optional mid-turn interrupt
// support. When stdinFile is non-nil and is a terminal, the turn is
// wrapped in a turnInterrupter; pressing ESC or single-Ctrl+C
// cancels the turn (preserving session history), and double-Ctrl+C
// within ctrlCExitWindow returns exit=true so REPL breaks out.
//
// When stdinFile is nil or not a terminal, behaves identically to
// the pre-v1.3.0 path: streamTurn against ctx, no per-turn cancel,
// caller-supplied ctx cancellation still works.
//
// A turn error from streamTurn is surfaced as the returned error;
// the REPL loop prints it and continues. Interrupter setup errors
// are NOT fatal — we fall back to legacy on any setup failure so a
// transient termios glitch doesn't break the REPL.
func runREPLTurn(ctx context.Context, a *agent.Agent, m adkmodel.LLM, stdinFile *os.File, prompt string, stdout, stderr io.Writer, tracker *usage.Tracker, pricing usage.Pricing, eventsOpts []EventsOption) (exit bool, err error) {
	if stdinFile == nil {
		_, err := streamTurn(ctx, a, m, prompt, stdout, stderr, tracker, pricing, eventsOpts)
		return false, err
	}
	interrupter, ierr := newTurnInterrupter(stdinFile, stderr)
	if ierr != nil {
		// Non-TTY or other setup failure — silently fall back.
		_, err := streamTurn(ctx, a, m, prompt, stdout, stderr, tracker, pricing, eventsOpts)
		return false, err
	}
	turnCtx, cancel, serr := interrupter.Start(ctx)
	if serr != nil {
		_ = interrupter.Close()
		_, err := streamTurn(ctx, a, m, prompt, stdout, stderr, tracker, pricing, eventsOpts)
		return false, err
	}
	defer func() {
		cancel()
		_ = interrupter.Close()
		// Raw mode disables OPOST, so any "\n" the model's streaming
		// text wrote during the turn moved the cursor down but
		// didn't return to column 0. After we've restored cooked
		// mode (interrupter.Close above), reset the cursor to
		// column 0 + clear-to-end-of-line so the next "> " prompt
		// lands at the left margin. Cheap and idempotent: if the
		// cursor was already at column 0, the \r is a no-op.
		_, _ = io.WriteString(stdout, "\r\x1b[K")
	}()
	_, terr := streamTurn(turnCtx, a, m, prompt, stdout, stderr, tracker, pricing, eventsOpts)
	// A ctx.Canceled coming back from streamTurn is the expected
	// shape of a user-initiated interrupt — don't propagate it as a
	// turn error. Everything else (real LLM/tool errors) flows
	// through.
	if terr != nil && errors.Is(terr, context.Canceled) && interrupter.Interrupted() {
		terr = nil
	}
	return interrupter.ExitRequested(), terr
}

// betweenTurnSigState manages SIGINT handling between turns. During
// a turn, the per-turn turnInterrupter puts stdin in raw mode (ISIG
// off) so Ctrl+C arrives as byte 0x03 and never reaches this
// handler — by design. Between turns, stdin is in cooked mode (ISIG
// on) and Ctrl+C does generate SIGINT; this state machine catches
// it and implements the same double-press-to-exit semantic.
//
// Semantics: first Ctrl+C arms an "exit primed" flag and prints a
// hint. Any subsequent Ctrl+C without intervening user input exits
// cleanly. Typing a new line (any prompt) clears the flag via
// reset(), so a stale arming from minutes ago doesn't escalate the
// next time the user hits Ctrl+C. Deliberately no time window —
// at human keystroke speed a 1-second window forced unnatural
// double-tapping; the typing-resets-the-arming model is more
// forgiving without losing the "you really meant it" property.
type betweenTurnSigState struct {
	stderr     io.Writer
	cancelREPL context.CancelFunc

	mu    sync.Mutex
	armed bool

	sigCh    chan os.Signal
	doneCh   chan struct{}
	stopOnce sync.Once
}

func newBetweenTurnSigState(stderr io.Writer, cancelREPL context.CancelFunc) *betweenTurnSigState {
	s := &betweenTurnSigState{
		stderr:     stderr,
		cancelREPL: cancelREPL,
		sigCh:      make(chan os.Signal, 1),
		doneCh:     make(chan struct{}),
	}
	signal.Notify(s.sigCh, os.Interrupt)
	go s.loop()
	return s
}

func (s *betweenTurnSigState) loop() {
	defer close(s.doneCh)
	for sig := range s.sigCh {
		_ = sig // we only listen for one signal type
		s.mu.Lock()
		wasArmed := s.armed
		s.armed = true
		s.mu.Unlock()
		if wasArmed {
			// Second press without typing → exit cleanly. Newline
			// first so the shell prompt lands on its own line.
			_, _ = fmt.Fprintln(s.stderr)
			s.cancelREPL()
			return
		}
		// First press: print the hint. The leading "\n" gets us
		// onto a fresh line since the terminal usually echoed "^C"
		// in-line.
		_, _ = fmt.Fprintln(s.stderr, "\n\x1b[33m✕\x1b[0m \x1b[2m(press Ctrl+C again to exit, or /exit / Ctrl-D)\x1b[0m")
	}
}

// reset clears the armed flag so a stale first-press from earlier
// doesn't escalate when the user types and the next Ctrl+C lands.
// Called by the REPL whenever fresh user input arrives.
func (s *betweenTurnSigState) reset() {
	s.mu.Lock()
	s.armed = false
	s.mu.Unlock()
}

// stop removes the signal handler and waits for the goroutine to
// exit. Idempotent.
func (s *betweenTurnSigState) stop() {
	s.stopOnce.Do(func() {
		signal.Stop(s.sigCh)
		close(s.sigCh)
		<-s.doneCh
	})
}
