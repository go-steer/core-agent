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

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/usage"
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

// TestReproduceAgent_WiresCompactorAndCheckpointer is the regression
// gate for the bug where per-session agents created by the multi-
// session daemon were constructed without WithCompactor /
// WithCheckpointer, so /compact and /done returned "no compactor
// wired" / "no checkpointer wired" on daemon-hosted sessions even
// though the default-on advertisements said otherwise.
//
// CostCeiling is wired by the same reproduceAgent code path but has
// no public HasCostCeiling accessor to assert against; the shared
// opts append flow makes a targeted test redundant.
func TestReproduceAgent_WiresCompactorAndCheckpointer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	deps := sessionFactoryDeps{
		daemonCtx: ctx,
		model:     stubLLM{},
		template:  permissions.New(permissions.Options{}),
	}

	ag, cancelAg, err := reproduceAgent(deps, auth.Anonymous, "sid-defaults", "created")
	if err != nil {
		t.Fatalf("reproduceAgent: %v", err)
	}
	t.Cleanup(cancelAg)

	if !ag.HasCompactor() {
		t.Errorf("HasCompactor() = false, want true (default-on; /compact would return ErrNoCompactor)")
	}
	if !ag.HasCheckpointer() {
		t.Errorf("HasCheckpointer() = false, want true (default-on; /done would be unavailable)")
	}
}

// TestReproduceAgent_HonorsDisableFlags asserts that the
// noCompact / noCheckpoint fields on sessionFactoryDeps (fed from
// the --no-compact / --no-checkpoint CLI flags) suppress the
// corresponding option, so the disable flags apply uniformly to
// per-session agents and not just the main-loop agent.
func TestReproduceAgent_HonorsDisableFlags(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	deps := sessionFactoryDeps{
		daemonCtx:    ctx,
		model:        stubLLM{},
		template:     permissions.New(permissions.Options{}),
		noCompact:    true,
		noCheckpoint: true,
	}

	ag, cancelAg, err := reproduceAgent(deps, auth.Anonymous, "sid-disabled", "created")
	if err != nil {
		t.Fatalf("reproduceAgent: %v", err)
	}
	t.Cleanup(cancelAg)

	if ag.HasCompactor() {
		t.Errorf("HasCompactor() = true with noCompact=true, want false")
	}
	if ag.HasCheckpointer() {
		t.Errorf("HasCheckpointer() = true with noCheckpoint=true, want false")
	}
}
