// Copyright 2026 The go-steer team
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

//go:build !no_tui

package main

import (
	"context"
	"fmt"
	"iter"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/agent"
	"google.golang.org/genai"
)

// (internal/pricing + permissions will be referenced once the
// PermissionController / PricingController capability adapters
// land — kept out of the import set until then to satisfy
// goimports.)
var _ = pricingPlaceholder
var pricingPlaceholder int

// launchTUIv2 is the core-tui-backed alternative to launchTUI. Same
// inputs, same return contract; differs only in which TUI library
// drives the operator surface. Picked at runtime by the
// --use-core-tui CLI flag (see main.go). While both code paths
// coexist (PRs 6-9 of docs/core-tui-adapter-design.md), this lets
// operators A/B the two and stick on either until the migration
// settles.
func launchTUIv2(ctx context.Context, deps tuiDeps) (didRun bool, exitCode int, err error) {
	a, err := agent.New(deps.Model, deps.AgentOpts...)
	if err != nil {
		return false, 0, fmt.Errorf("agent.New: %w", err)
	}

	wrapped := &coreAgentAdapter{
		inner:    a,
		deps:     deps,
		ctxBuild: ctx,
	}

	opts := coretui.Options{
		Agent: wrapped,
	}

	if err := coretui.Run(ctx, opts); err != nil {
		return true, 1, err
	}
	return true, 0, nil
}

// coreAgentAdapter wraps *agent.Agent so it satisfies core-tui's
// tui.Agent plus every optional capability interface core-agent
// can support. Built incrementally — capabilities the host can't
// support yet (none today; everything has a backing in deps or on
// the agent) are simply not implemented and core-tui's type
// assertions silently degrade those slash commands to "not
// available."
//
// Each interface assertion is documented inline so the next reader
// can see at a glance which capability slot each method fills.
type coreAgentAdapter struct {
	inner    *agent.Agent
	deps     tuiDeps
	ctxBuild context.Context // captured at launchTUIv2 time for rebuildAgent re-resolves
}

// Run satisfies coretui.Agent. Translates each *session.Event from
// the ADK iterator into a coretui.Event. Same shape as the
// MIGRATION.md §2.3 sketch — accumulate Text parts, fan out tool
// calls, snapshot Usage.
func (a *coreAgentAdapter) Run(ctx context.Context, prompt string) iter.Seq2[coretui.Event, error] {
	return func(yield func(coretui.Event, error) bool) {
		for ev, err := range a.inner.Run(ctx, prompt) {
			if err != nil {
				yield(coretui.Event{}, err)
				return
			}
			te := coretui.Event{Partial: ev.Partial}
			if ev.UsageMetadata != nil {
				te.Usage = &coretui.Usage{
					InputTokens:  int(ev.UsageMetadata.PromptTokenCount),
					OutputTokens: int(ev.UsageMetadata.CandidatesTokenCount),
				}
			}
			if ev.Content != nil {
				for _, p := range ev.Content.Parts {
					if p.FunctionCall != nil {
						te.ToolCalls = append(te.ToolCalls, coretui.ToolCall{
							ID:   p.FunctionCall.ID,
							Name: p.FunctionCall.Name,
							Args: argsToMap(p.FunctionCall.Args),
						})
					}
					if p.Text != "" {
						te.Text += p.Text
					}
				}
			}
			if !yield(te, nil) {
				return
			}
		}
	}
}

// Interrupt satisfies coretui.Interruptible. core-agent already
// distinguishes interrupt from ctx-cancel (Agent.Interrupt returns
// whether anything was active); core-tui's Esc cascade uses the
// returned bool to decide whether to also emit an "(interrupted)"
// notice.
func (a *coreAgentAdapter) Interrupt() bool { return a.inner.Interrupt() }

// Inject satisfies coretui.InjectableAgent (R-CHAT-11). Required
// when Options.MidTurnInjectionMode == InjectIntoCurrent — wires
// the agent's inbox so operator-typed-during-streaming entries
// land in the running turn's context.
func (a *coreAgentAdapter) Inject(message string) error {
	return a.inner.Inject(message)
}

// WakeRequested satisfies coretui.WakeRequester (R-WAKE-1).
// Background sub-agents that report completion via the wake
// channel surface as transient toast banners.
func (a *coreAgentAdapter) WakeRequested() <-chan struct{} {
	return a.inner.WakeRequested()
}

// argsToMap converts a *genai.FunctionCall.Args (which is a
// map[string]any of genai.Schema-shaped values) into the plain
// map[string]any core-tui's ToolCall.Args carries. Defensive against
// nil to avoid panics on tool calls with no arguments.
func argsToMap(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
	}
	return out
}

// ensureGenaiImport keeps the genai import live for the
// argsToMap signature; the linter would otherwise complain about an
// unused import if a future refactor stops mentioning genai
// directly here. (`genai.Schema` is in flux upstream so the args
// shape may move into a typed value later.)
var _ = genai.Schema{}
