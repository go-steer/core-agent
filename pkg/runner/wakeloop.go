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
	"fmt"
	"os"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// WakeLoopOptions configures WakeLoop. The zero value is usable:
// no usage accounting, turn errors to stderr, no debug tracing.
type WakeLoopOptions struct {
	// Tracker receives one AppendUsage per completed turn (keyed by
	// Model, priced by Pricing) via the usage.TurnTap discipline —
	// overwrite-per-event, commit exactly once on TurnComplete.
	// Nil disables accounting.
	Tracker *usage.Tracker
	Model   string
	Pricing usage.Pricing

	// OnTurnError is invoked for each error the turn iterator
	// yields. The loop always keeps running — one bad turn must not
	// kill an attach-only daemon or a hosted session. Nil writes
	// "core-agent: session <sid> turn: <err>" to stderr, matching
	// the historical inline loops.
	OnTurnError func(error)

	// Debugf, when non-nil, receives trace-level lifecycle lines
	// (loop start/stop, wake fired, turn finished). Wire the host's
	// debug logger; nil is silent.
	Debugf func(format string, args ...any)
}

// WakeLoop is the wake-driven inbox drain every headless agent
// surface runs: block until an attach client's POST /inject (or any
// other Inject caller) fires WakeRequested, run one empty-prompt
// turn so the inbox drains into a real model turn, account its
// usage, repeat. Returns when ctx is cancelled.
//
// The empty prompt means "no user text this turn, just drain the
// inbox" — the same path the REPL uses between submissions. The
// turn's events flow through the eventlog to the attach broadcaster,
// which is what a remote operator's TUI renders.
//
// This consolidates the previously duplicated loops in
// cmd/core-agent (the --no-repl inline loop and the per-session
// multi-session loop) behind pkg/runner, which already owns the
// "drive the agent through a conversation" surface. Extracted as
// part of the pkg/compose work (#386,
// docs/compose-extraction-design.md).
func WakeLoop(ctx context.Context, a *agent.Agent, opts WakeLoopOptions) {
	debugf := opts.Debugf
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	onErr := opts.OnTurnError
	if onErr == nil {
		onErr = func(err error) {
			fmt.Fprintf(os.Stderr, "core-agent: session %s turn: %v\n", a.SessionID(), err)
		}
	}
	debugf("wake loop starting (session=%s model=%s)", a.SessionID(), opts.Model)
	for {
		select {
		case <-ctx.Done():
			debugf("wake loop ending (ctx cancelled)")
			return
		case <-a.WakeRequested():
			debugf("wake fired; calling Run")
			var tap usage.TurnTap
			var evCount int
			for ev, runErr := range a.Run(ctx, "") {
				evCount++
				tap.Observe(ev)
				if u, ok := tap.Commit(ev); ok && opts.Tracker != nil {
					opts.Tracker.AppendUsage(opts.Model, u, opts.Pricing)
				}
				if runErr != nil {
					onErr(runErr)
				}
			}
			debugf("Run finished (events=%d)", evCount)
		}
	}
}
