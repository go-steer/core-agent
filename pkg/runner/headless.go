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

// Package runner drives the agent through a real conversation —
// either a single one-shot prompt (Headless) or a multi-turn stdin
// REPL (REPL). Both share an Agent under the hood and route partial
// text + tool-call summaries through the same streaming consumer.
package runner

import (
	"context"
	"fmt"
	"io"
	"iter"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/usage"
)

// Exit codes — kept distinct so CI can disambiguate failure modes.
const (
	ExitOK          = 0
	ExitAgentError  = 1
	ExitConfigError = 2
)

// Headless executes prompt against m and streams the assistant's text
// to stdout as partial events arrive. Tool-call summaries are written
// to stderr as one line per call. Returns an exit code suitable for
// os.Exit.
//
// agentOpts lets the caller pass extra agent.Options (typically
// WithTools, WithToolsets, WithSystemInstructionPrefix). Pass nil for
// no tools and the default instruction.
//
// tracker (optional) records per-turn usage; when supplied, the caller
// can write a summary using its totals after Headless returns. Pass
// nil to skip accounting.
//
// eventsOpts forwards through to WriteEvents (e.g. WithColor) so
// callers can opt into ANSI styling without reaching into the
// formatter directly.
//
// A trailing newline is always added to stdout when at least one chunk
// was written, so shell pipelines see a clean terminator.
func Headless(ctx context.Context, m adkmodel.LLM, prompt string, stdout, stderr io.Writer, tracker *usage.Tracker, pricing usage.Pricing, agentOpts []agent.Option, eventsOpts ...EventsOption) (int, error) {
	if prompt == "" {
		return ExitConfigError, fmt.Errorf("runner: prompt is required")
	}

	a, err := agent.New(m, agentOpts...)
	if err != nil {
		return ExitAgentError, err
	}

	return streamTurn(ctx, a, m, prompt, stdout, stderr, tracker, pricing, eventsOpts)
}

// streamTurn is the shared one-turn driver used by Headless and REPL.
// Wraps the agent's event iterator so per-turn usage lands in tracker
// via the shared TurnTap discipline (overwrite-per-event, commit-once-
// per-TurnComplete, reset between turns — see pkg/usage.TurnTap), then
// hands the wrapped iterator to WriteEvents for formatting.
func streamTurn(ctx context.Context, a *agent.Agent, m adkmodel.LLM, prompt string, stdout, stderr io.Writer, tracker *usage.Tracker, pricing usage.Pricing, eventsOpts []EventsOption) (int, error) {
	events := tapTracker(a.Run(ctx, prompt), tracker, m.Name(), pricing)
	if err := WriteEvents(events, stdout, stderr, eventsOpts...); err != nil {
		return ExitAgentError, fmt.Errorf("runner: agent run: %w", err)
	}
	return ExitOK, nil
}

// tapTracker wraps an event iterator so per-turn usage is appended to
// tracker exactly once per TurnComplete. Events pass through unchanged
// so WriteEvents (an opaque consumer) sees the raw stream. A nil
// tracker makes the wrapper a straight passthrough.
func tapTracker(events iter.Seq2[*session.Event, error], tracker *usage.Tracker, modelName string, pricing usage.Pricing) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		var tap usage.TurnTap
		for ev, err := range events {
			tap.Observe(ev)
			if u, ok := tap.Commit(ev); ok && tracker != nil {
				tracker.AppendUsage(modelName, u, pricing)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// WriteSummary emits a one-line usage tally suitable for shell
// pipelines / CI logs. No-op when no turns were recorded.
//
// When the resolved pricing for the model used during the session
// is zero (e.g. a custom / fine-tuned variant not in the built-in
// rate table and without a cfg.Model.Pricing override), we surface
// "$—" rather than "$0.0000" — the latter implied "free", which is
// misleading and historically confusing for operators who actually
// spent money.
func WriteSummary(w io.Writer, t *usage.Tracker, modelID string) {
	if t == nil {
		return
	}
	tot := t.Totals()
	if tot.Turns == 0 {
		return
	}
	cost := fmt.Sprintf("$%.4f", tot.CostUSD)
	if tot.CostUSD == 0 && (tot.InputTokens > 0 || tot.OutputTokens > 0) {
		cost = "$— (pricing not configured for this model)"
	}
	fmt.Fprintf(w, "core-agent: %d turn(s) · ↑%d ↓%d tokens · %s (%s)\n",
		tot.Turns, tot.InputTokens, tot.OutputTokens, cost, modelID)
}
