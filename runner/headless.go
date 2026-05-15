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

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/usage"
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
// Wraps the agent's event iterator with tapUsage to record token
// counts as they fly by, then hands the wrapped iterator to
// WriteEvents for formatting.
func streamTurn(ctx context.Context, a *agent.Agent, m adkmodel.LLM, prompt string, stdout, stderr io.Writer, tracker *usage.Tracker, pricing usage.Pricing, eventsOpts []EventsOption) (int, error) {
	var lastUsageInput, lastUsageOutput int
	events := tapUsage(a.Run(ctx, prompt), func(in, out int) {
		lastUsageInput, lastUsageOutput = in, out
	})
	if err := WriteEvents(events, stdout, stderr, eventsOpts...); err != nil {
		return ExitAgentError, fmt.Errorf("runner: agent run: %w", err)
	}
	if tracker != nil && (lastUsageInput > 0 || lastUsageOutput > 0) {
		tracker.Append(m.Name(), lastUsageInput, lastUsageOutput, pricing)
	}
	return ExitOK, nil
}

// tapUsage wraps an event iterator and invokes track for every event
// that carries UsageMetadata. The event itself passes through to the
// next consumer unchanged. Used so streamTurn can delegate formatting
// to WriteEvents while still maintaining its per-turn token totals.
func tapUsage(events iter.Seq2[*session.Event, error], track func(input, output int)) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		for ev, err := range events {
			if ev != nil && ev.UsageMetadata != nil {
				track(int(ev.UsageMetadata.PromptTokenCount), int(ev.UsageMetadata.CandidatesTokenCount))
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// WriteSummary emits a one-line usage tally suitable for shell
// pipelines / CI logs. No-op when no turns were recorded.
func WriteSummary(w io.Writer, t *usage.Tracker, modelID string) {
	if t == nil {
		return
	}
	tot := t.Totals()
	if tot.Turns == 0 {
		return
	}
	fmt.Fprintf(w, "core-agent: %d turn(s) · ↑%d ↓%d tokens · $%.4f (%s)\n",
		tot.Turns, tot.InputTokens, tot.OutputTokens, tot.CostUSD, modelID)
}
