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

package main

import (
	"context"
	"errors"
	"iter"
	"testing"

	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/pkg/auth"
	"github.com/go-steer/core-agent/pkg/permissions"
	"github.com/go-steer/core-agent/pkg/usage"
)

// stubLLM satisfies adkmodel.LLM without any provider setup. Tests
// that call reproduceAgent don't drive Run() — the assembly is what
// they're exercising, not the LLM loop.
type stubLLM struct{}

func (stubLLM) Name() string { return "stub" }
func (stubLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(nil, errors.New("stubLLM should not be invoked in this test"))
	}
}

// TestReproduceAgent_PerSessionTracker is the regression gate for
// issue #275: every session-created agent must get its own
// *usage.Tracker so AttachUsage / broadcaster snapshots / cost
// ceilings are per-session, not process-global. Two sessions are
// spun up through reproduceAgent; the test captures each session's
// tracker via the newSessionTracker indirection and asserts an
// append against one tracker never surfaces in the other.
func TestReproduceAgent_PerSessionTracker(t *testing.T) {
	// Capture every tracker constructed by reproduceAgent. Not
	// t.Parallel — newSessionTracker is a package var and swapping
	// it under a parallel sibling test would race.
	orig := newSessionTracker
	t.Cleanup(func() { newSessionTracker = orig })

	var captured []*usage.Tracker
	newSessionTracker = func() *usage.Tracker {
		tr := usage.NewTracker()
		captured = append(captured, tr)
		return tr
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	deps := sessionFactoryDeps{
		daemonCtx: ctx,
		model:     stubLLM{},
		template:  permissions.New(permissions.Options{}),
	}

	agA, cancelA, err := reproduceAgent(deps, auth.Anonymous, "sid-a", "created")
	if err != nil {
		t.Fatalf("reproduceAgent(sid-a): %v", err)
	}
	t.Cleanup(cancelA)

	agB, cancelB, err := reproduceAgent(deps, auth.Anonymous, "sid-b", "created")
	if err != nil {
		t.Fatalf("reproduceAgent(sid-b): %v", err)
	}
	t.Cleanup(cancelB)

	if len(captured) != 2 {
		t.Fatalf("newSessionTracker called %d times, want 2", len(captured))
	}
	if captured[0] == captured[1] {
		t.Fatalf("both sessions got the same tracker pointer — per-session invariant broken")
	}

	// Append a turn to sid-a's tracker and prove it doesn't leak
	// into sid-b's AttachUsage.
	captured[0].Append("stub", 10_000, 500, usage.Pricing{})

	if got := agA.AttachUsage().Overall.Turns; got != 1 {
		t.Errorf("sid-a AttachUsage.Overall.Turns = %d, want 1", got)
	}
	if got := agB.AttachUsage().Overall.Turns; got != 0 {
		t.Errorf("sid-b AttachUsage.Overall.Turns = %d, want 0 (turns leaked across sessions)", got)
	}
}
