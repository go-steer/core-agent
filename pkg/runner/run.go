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
	"io"
	"os"

	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// RunOptions configures Run, the consolidated multi-turn REPL entry
// point (#492). It replaces four positional variants (REPL,
// REPLWithInitialPrompt, REPLWithAgent,
// REPLWithAgentAndInitialPrompt) whose widest form had grown to ten
// parameters — and whose agent-taking forms invited an
// agent/model mismatch by accepting both.
type RunOptions struct {
	// Model drives every turn. Required when Agent is nil (it is
	// what the constructed Agent runs on). When Agent is non-nil,
	// leave Model nil — Run derives it from Agent.Model() (#510) —
	// or, if set, it MUST be the same model the agent was built
	// with; a mismatch produces confusing turns.
	Model adkmodel.LLM

	// Agent, when non-nil, is used as-is — for hosts that construct
	// (and decorate, e.g. wrap with pkg/attachadapter and register
	// for attach-mode) the Agent themselves. When nil, Run constructs
	// one from Model + AgentOptions.
	Agent *agent.Agent

	// AgentOptions configure the Agent Run constructs. Ignored when
	// Agent is non-nil.
	AgentOptions []agent.Option

	// InitialPrompt, when non-empty, seeds the first turn before the
	// stdin/wake select loop — the --prompt startup behavior. The
	// seed runs through the same turn machinery as a typed
	// submission (ESC interrupt, usage tracking, tool prompts).
	InitialPrompt string

	// Stdin / Stdout / Stderr default to the process's os.Stdin /
	// os.Stdout / os.Stderr when nil. Mid-turn ESC interrupt and the
	// double-Ctrl+C exit gesture require Stdin to be a real terminal
	// (*os.File on a TTY); anything else degrades gracefully.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Tracker + Pricing accumulate per-turn cost across the whole
	// session so the final summary is meaningful. Optional.
	Tracker *usage.Tracker
	Pricing usage.Pricing

	// EventsOptions forward to WriteEvents on every turn (e.g.
	// WithColor).
	EventsOptions []EventsOption
}

// Run drives the multi-turn stdin REPL described on REPL, configured
// by a single options struct. Returns the process exit code and any
// terminal error, same contract as the deprecated positional
// variants it replaces.
func Run(ctx context.Context, opts RunOptions) (int, error) {
	if opts.Model == nil && opts.Agent != nil {
		// Derive the streaming model from the pre-built agent (#510)
		// — the agent knows its model, and requiring both invited
		// the mismatch the #492 assessment flagged.
		opts.Model = opts.Agent.Model()
	}
	if opts.Model == nil {
		return ExitConfigError, errors.New("runner: Run: Model is required (or pass a pre-built Agent to derive it from)")
	}
	a := opts.Agent
	if a == nil {
		var err error
		a, err = agent.New(opts.Model, opts.AgentOptions...)
		if err != nil {
			return ExitAgentError, err
		}
	}
	stdin := opts.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	return replCore(ctx, a, opts.Model, opts.InitialPrompt, stdin, stdout, stderr, opts.Tracker, opts.Pricing, opts.EventsOptions...)
}
