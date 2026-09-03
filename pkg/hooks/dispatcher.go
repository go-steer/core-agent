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

package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"google.golang.org/adk/session"
)

// CommandGate is the subset of *permissions.Gate the dispatcher needs
// to gate hook command execution. Declared as an interface (rather
// than importing permissions) so the hooks package stays dependency-
// light; *permissions.Gate satisfies it via CheckBash.
//
// CheckBash applies the built-in bash denylist, the configured mode,
// and any deny patterns before returning nil (allowed) or an error
// (blocked). A nil error means the command may run.
type CommandGate interface {
	CheckBash(ctx context.Context, command string) error
}

// Dispatcher fires configured shell commands on agent event boundaries.
// Construct one per agent (or per session) via New, pass OnEvent and
// OnTurnEnd to agent.WithEventHook, and the agent tap loop drives the
// rest.
//
// Concurrency: OnEvent and OnTurnEnd are safe to call from a single
// goroutine (the agent's event tap). Handlers run synchronously in
// that goroutine with per-command timeouts — a misbehaving hook stalls
// the agent for at most Handler.TimeoutSeconds, matching the semantics
// of Scion's Antigravity harness.
//
// Hook commands are operator-authored config, but `.agents/config.json`
// is itself writable by the model, so a compromised/injected config
// could register a hook that runs arbitrary `/bin/sh -c`. Every hook
// command is therefore routed through the permission gate (as a
// bash-kind check) before execution; a dispatcher with no gate fails
// closed and runs nothing (#378).
type Dispatcher struct {
	events    map[string][]Handler
	sessionID string
	stderr    io.Writer
	gate      CommandGate

	mu              sync.Mutex
	modelStartFired bool // true after we've fired model-start this turn

	// runCmd is the subprocess-spawn function. Overridable so tests
	// can substitute a fake without shelling out.
	runCmd func(ctx context.Context, command string, envelope []byte) error
}

// New builds a Dispatcher from cfg. sessionID is threaded onto every
// envelope's `session_id` field (empty is allowed — the field is
// omitted from the envelope). stderr receives one line per hook that
// fails; pass io.Discard to suppress.
//
// gate gates every hook command through the permission system before
// it runs (bash-kind check: denylist + mode + deny patterns). Passing
// nil fails closed — the dispatcher refuses to run any command,
// logging a diagnostic — so an ungated execution path can't slip
// through. Callers that genuinely have no gate must pass an explicit
// allow-all gate and accept the risk.
//
// cfg is copied — mutating the passed-in Config after New has no
// effect. Validate cfg before calling if you want early config errors.
func New(cfg Config, sessionID string, stderr io.Writer, gate CommandGate) *Dispatcher {
	if stderr == nil {
		stderr = io.Discard
	}
	d := &Dispatcher{
		events:    make(map[string][]Handler, len(cfg)),
		sessionID: sessionID,
		stderr:    stderr,
		gate:      gate,
	}
	for name, handlers := range cfg {
		copied := make([]Handler, len(handlers))
		copy(copied, handlers)
		d.events[name] = copied
	}
	d.runCmd = d.execCommand
	return d
}

// Empty reports whether the dispatcher has no configured handlers.
// Callers can skip wiring it into the agent when Empty is true to
// avoid the per-event bookkeeping cost.
func (d *Dispatcher) Empty() bool {
	return len(d.events) == 0
}

// OnEvent is the per-event callback. Walks ev's content parts and
// fires the corresponding hook events:
//
//   - FunctionCall part     → "tool-start"
//   - FunctionResponse part → "tool-end"
//   - First Text+Partial after turn-start or tool-end → "model-start"
//
// Nil or empty events are no-ops, and so are the two tool arms on a
// partial event. ADK's streaming aggregator yields one partial event
// per chunk and then one aggregated event carrying the whole model
// call, so a streamed tool call reaches this callback twice — and
// when the model streams a call's arguments incrementally the
// aggregate is rebuilt from the accumulated args rather than
// forwarding the chunk's part, so the two copies carry different
// ADK-synthesized IDs and different args: the chunk's are empty.
// Firing on both runs an operator's hook twice per call, the first
// time with tool_input: {}. The aggregate carries every part of the
// call and is the event the runtime actually executes the tool from
// (base_flow skips partial responses before handleFunctionCalls), so
// acting on it alone is both complete and exactly once (#926, the
// same reasoning the watchdog took in #915).
//
// The model-start arm keeps reading partials on purpose — streaming
// text is precisely what it announces.
func (d *Dispatcher) OnEvent(ev *session.Event) {
	if ev == nil || ev.Content == nil {
		return
	}
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		if ev.Partial && (p.FunctionCall != nil || p.FunctionResponse != nil) {
			continue
		}
		switch {
		case p.FunctionCall != nil:
			d.fire("tool-start", map[string]any{
				"tool_name":  p.FunctionCall.Name,
				"tool_input": p.FunctionCall.Args,
			})
			// Model-start fires again the next time the model produces
			// text (after this tool completes).
			d.setModelStartFired(false)
		case p.FunctionResponse != nil:
			d.fire("tool-end", map[string]any{
				"tool_name":   p.FunctionResponse.Name,
				"tool_output": p.FunctionResponse.Response,
			})
			d.setModelStartFired(false)
		case p.Text != "" && ev.Partial:
			if !d.setModelStartFiredOnce() {
				d.fire("model-start", nil)
			}
		}
	}
}

// OnTurnEnd is the end-of-turn callback. Fires "agent-end" and clears
// per-turn state. Wired into the same post-turn cleanup that runs
// watchdog / compaction / checkpoint hooks.
func (d *Dispatcher) OnTurnEnd() {
	d.setModelStartFired(false)
	d.fire("agent-end", nil)
}

// setModelStartFired unconditionally sets the flag.
func (d *Dispatcher) setModelStartFired(v bool) {
	d.mu.Lock()
	d.modelStartFired = v
	d.mu.Unlock()
}

// setModelStartFiredOnce sets the flag and returns its previous value
// atomically — used by OnEvent to fire model-start at most once per
// "thinking window" (turn-start until next tool boundary).
func (d *Dispatcher) setModelStartFiredOnce() bool {
	d.mu.Lock()
	prev := d.modelStartFired
	d.modelStartFired = true
	d.mu.Unlock()
	return prev
}

// fire builds the envelope for eventName and runs every configured
// handler for it in sequence.
func (d *Dispatcher) fire(eventName string, extra map[string]any) {
	handlers := d.events[eventName]
	if len(handlers) == 0 {
		return
	}
	envelope := d.envelope(eventName, extra)
	body, err := json.Marshal(envelope)
	if err != nil {
		fmt.Fprintf(d.stderr, "hooks: %s: marshal envelope: %v\n", eventName, err)
		return
	}
	for i, h := range handlers {
		timeout := time.Duration(h.TimeoutSeconds) * time.Second
		if h.TimeoutSeconds == 0 {
			timeout = DefaultTimeoutSeconds * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		// Gate the command before executing. Fail closed when no gate
		// is wired — running an ungated `/bin/sh -c` from
		// (model-writable) config is exactly the escalation #378
		// closes.
		if d.gate == nil {
			fmt.Fprintf(d.stderr, "hooks: %s handler #%d (%q): refused — no permission gate wired\n", eventName, i, h.Command)
			cancel()
			continue
		}
		if err := d.gate.CheckBash(ctx, h.Command); err != nil {
			fmt.Fprintf(d.stderr, "hooks: %s handler #%d (%q): permission gate refused: %v\n", eventName, i, h.Command, err)
			cancel()
			continue
		}
		if err := d.runCmd(ctx, h.Command, body); err != nil {
			fmt.Fprintf(d.stderr, "hooks: %s handler #%d (%q): %v\n", eventName, i, h.Command, err)
		}
		cancel()
	}
}

// envelope constructs the JSON payload for a hook event. Top-level
// field names match what Scion's sciontool hook auto-extracts
// (pkg/sciontool/hooks/dialects/mapping.go), so a dialect.yaml with
// identity mappings covers the common case.
func (d *Dispatcher) envelope(eventName string, extra map[string]any) map[string]any {
	env := map[string]any{
		"hook_event_name": eventName,
	}
	if d.sessionID != "" {
		env["session_id"] = d.sessionID
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// execCommand shells out via /bin/sh -c so pipes and redirections in
// the command string work (matches Antigravity harness). The envelope
// JSON is piped to stdin.
func (d *Dispatcher) execCommand(ctx context.Context, command string, envelope []byte) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command) //nolint:gosec // operator-authored config command
	cmd.Stdin = bytes.NewReader(envelope)
	// Capture combined output so failures include context in the stderr
	// line the dispatcher logs. Successful runs discard it silently.
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, bytes.TrimRight(out, "\n"))
	}
	return nil
}
