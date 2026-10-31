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

package compose

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

// stubLLM satisfies adkmodel.LLM without any provider setup. Tests
// that call ReproduceAgent don't drive Run() — the assembly is what
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
// spun up through ReproduceAgent; the test captures each session's
// tracker via the newSessionTracker indirection and asserts an
// append against one tracker never surfaces in the other.
func TestReproduceAgent_PerSessionTracker(t *testing.T) {
	// Capture every tracker constructed by ReproduceAgent. Not
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

	deps := SessionFactoryDeps{
		DaemonCtx: ctx,
		Model:     stubLLM{},
		Template:  permissions.New(permissions.Options{}),
	}

	agA, cancelA, err := ReproduceAgent(deps, auth.Anonymous, "sid-a", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent(sid-a): %v", err)
	}
	t.Cleanup(cancelA)

	agB, cancelB, err := ReproduceAgent(deps, auth.Anonymous, "sid-b", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent(sid-b): %v", err)
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
// CostCeiling is wired by the same ReproduceAgent code path but has
// no public HasCostCeiling accessor to assert against; the shared
// opts append flow makes a targeted test redundant.
func TestReproduceAgent_WiresCompactorAndCheckpointer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	deps := SessionFactoryDeps{
		DaemonCtx: ctx,
		Model:     stubLLM{},
		Template:  permissions.New(permissions.Options{}),
	}

	ag, cancelAg, err := ReproduceAgent(deps, auth.Anonymous, "sid-defaults", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent: %v", err)
	}
	t.Cleanup(cancelAg)

	if !ag.Agent().HasCompactor() {
		t.Errorf("HasCompactor() = false, want true (default-on; /compact would return ErrNoCompactor)")
	}
	if !ag.Agent().HasCheckpointer() {
		t.Errorf("HasCheckpointer() = false, want true (default-on; /done would be unavailable)")
	}
}

// TestReproduceAgent_HonorsDisableFlags asserts that the NoCompact
// field and CheckpointMode=off on SessionFactoryDeps (fed from the
// --no-compact / --checkpoint CLI flags) suppress the corresponding
// option, so the disable flags apply uniformly to per-session agents
// and not just the main-loop agent.
func TestReproduceAgent_HonorsDisableFlags(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	deps := SessionFactoryDeps{
		DaemonCtx:      ctx,
		Model:          stubLLM{},
		Template:       permissions.New(permissions.Options{}),
		NoCompact:      true,
		CheckpointMode: config.CheckpointModeOff,
	}

	ag, cancelAg, err := ReproduceAgent(deps, auth.Anonymous, "sid-disabled", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent: %v", err)
	}
	t.Cleanup(cancelAg)

	if ag.Agent().HasCompactor() {
		t.Errorf("HasCompactor() = true with NoCompact=true, want false")
	}
	if ag.Agent().HasCheckpointer() {
		t.Errorf("HasCheckpointer() = true with CheckpointMode=off, want false")
	}
}

// TestReproduceAgent_CheckpointModeOperator is the #905 wiring guard
// for the surface that matters most: a multi-session daemon's POST
// /sessions agents. "operator" has to keep the checkpointer (so /done
// and Agent.Checkpoint still work) while withholding mark_task_done —
// wiring it as plain on/off on this path would hand the model its
// trigger back on exactly the long-lived deployment the mode exists
// for.
func TestReproduceAgent_CheckpointModeOperator(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	deps := SessionFactoryDeps{
		DaemonCtx:      ctx,
		Model:          stubLLM{},
		Template:       permissions.New(permissions.Options{}),
		CheckpointMode: config.CheckpointModeOperator,
	}

	ag, cancelAg, err := ReproduceAgent(deps, auth.Anonymous, "sid-operator", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent: %v", err)
	}
	t.Cleanup(cancelAg)

	if !ag.Agent().HasCheckpointer() {
		t.Errorf("HasCheckpointer() = false with CheckpointMode=operator, want true (/done must survive)")
	}
	for _, tl := range ag.Agent().Tools() {
		if tl.Name() == "mark_task_done" {
			t.Errorf("mark_task_done is registered with CheckpointMode=operator; want it withheld")
		}
	}
}

// TestReproduceAgent_WatchdogMode is the #642 wiring guard for the
// surface the fix would otherwise miss: a multi-session daemon's
// POST /sessions agents. Before this change SessionFactoryDeps had no
// watchdog field at all, so every tenant session came up un-backstopped
// even with --watchdog=enforce on the daemon — the primary session
// halted on a runaway and the tenant sessions kept looping.
func TestReproduceAgent_WatchdogMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		mode string
		want string
	}{
		{config.WatchdogEnforce, "enforce"},
		// #159: a daemon whose primary session self-corrects while its
		// tenant sessions loop silently is the same inheritance gap #642
		// closed, wearing a different label.
		{config.WatchdogFeedback, "feedback"},
		{config.WatchdogWarn, "warn"},
		{config.WatchdogOff, "off"},
		{"", "off"}, // a caller predating the field keeps its old behavior
	}
	for _, tc := range tests {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			ad, cancelSess, err := ReproduceAgent(SessionFactoryDeps{
				DaemonCtx:    ctx,
				Model:        stubLLM{},
				Template:     permissions.New(permissions.Options{}),
				WatchdogMode: tc.mode,
			}, auth.Anonymous, "sid-wd-"+tc.mode, "created")
			if err != nil {
				t.Fatalf("ReproduceAgent: %v", err)
			}
			t.Cleanup(cancelSess)

			if got := ad.Agent().WatchdogMode(); got != tc.want {
				t.Errorf("session agent WatchdogMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

// loopingStubLLM answers every invocation with the same tool call and
// honours ctx cancellation, so an enforce-mode halt truncates it.
type loopingStubLLM struct {
	mu    sync.Mutex
	calls int
}

func (*loopingStubLLM) Name() string { return "looping-stub" }
func (l *loopingStubLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(nil, err)
			return
		}
		l.mu.Lock()
		l.calls++
		n := l.calls
		l.mu.Unlock()
		yield(&adkmodel.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					ID:   fmt.Sprintf("call_%d", n),
					Name: "todo",
					Args: map[string]any{"action": "list"},
				},
			}}},
		}, nil)
	}
}

// TestReproduceAgent_WatchdogEnforceActuallyHalts closes the gap that
// let #705 be filed against a surface that WAS wired: the sibling test
// above asserts only that WatchdogMode() reads back "enforce", which a
// mode string can satisfy while nothing ever halts. Here a tenant
// session created through the factory drives a real looping model and
// has to stop on its own.
//
// The bound is what matters. Without an in-turn drain this loop never
// terminates — the halt only ever arrives at a turn boundary the model
// never reaches — so the failure mode is a hang, caught by the deadline.
func TestReproduceAgent_WatchdogEnforceActuallyHalts(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	llm := &loopingStubLLM{}
	ad, cancelSess, err := ReproduceAgent(SessionFactoryDeps{
		DaemonCtx:    ctx,
		Model:        llm,
		Template:     permissions.New(permissions.Options{}),
		WatchdogMode: config.WatchdogEnforce,
	}, auth.Anonymous, "sid-wd-halts", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent: %v", err)
	}
	t.Cleanup(cancelSess)

	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	t.Cleanup(runCancel)
	for _, err := range ad.Agent().Run(runCtx, "go") {
		_ = err // the halt surfaces as a cancellation
	}
	if runCtx.Err() != nil {
		t.Fatal("the tenant session looped until the deadline: enforce never halted it")
	}

	tripped, reason := ad.Agent().WatchdogTripped()
	if !tripped {
		t.Fatalf("tenant session did not trip after %d identical tool calls", llm.calls)
	}
	t.Logf("halted after %d model invocations: %s", llm.calls, reason)
}

type scriptedTurnLLM struct {
	name  string
	mu    sync.Mutex
	calls int
	turns []*adkmodel.LLMResponse
}

func (s *scriptedTurnLLM) Name() string { return s.name }

func (s *scriptedTurnLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		s.mu.Lock()
		idx := s.calls
		s.calls++
		s.mu.Unlock()
		if idx >= len(s.turns) {
			yield(&adkmodel.LLMResponse{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}},
				TurnComplete: true,
			}, nil)
			return
		}
		yield(s.turns[idx], nil)
	}
}

// TestReproduceAgent_ResumedRebuildsSubagentSpendAcrossSessionTree is the
// kill-and-resume regression test for #1359.
//
// Pre-fix, ReproduceAgent reconstructed the per-session usage tracker on
// origin=="resumed" by scanning eventlog.ForSession(app, user, sid) and
// pricing every event at the parent model's pricingRate. Delegated and
// background subagent turns live in child sessions (<sid>:sub:...) so
// ForSession missed them completely, and even if included, a constant
// pricingFor callback would bill a flash subagent at Opus rates.
// Meanwhile, SkipParentUsage subtasks (such as the shared MCP digest LLM
// fallback) ALSO live in child sessions (<sid>:sub:sub.mcp_digest:...)
// and must NOT be billed to the host session on a tree scan.
func TestReproduceAgent_ResumedRebuildsSubagentSpendAcrossSessionTree(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h, err := eventlog.Open(ctx, sqlite.Open(filepath.Join(t.TempDir(), "s.db")))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	// Subagent LLM intentionally leaves LLMResponse.ModelVersion empty:
	// ADK's database persistence strips ModelVersion anyway, so
	// BranchInjectingService + eventlog must preserve subModel.Name() via
	// CustomMetadata["model_version"].
	subLLM := &scriptedTurnLLM{
		name: "gemini-2.5-flash",
		turns: []*adkmodel.LLMResponse{
			{
				Content: &genai.Content{
					Role:  genai.RoleModel,
					Parts: []*genai.Part{{Text: "subagent findings"}},
				},
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount:     40_000,
					CandidatesTokenCount: 10_000,
					TotalTokenCount:      50_000,
				},
				TurnComplete: true,
			},
		},
	}
	subAgent, err := agent.New(subLLM, agent.WithName("researcher"), agent.WithInstruction("research"))
	if err != nil {
		t.Fatalf("agent.New(researcher): %v", err)
	}

	parentLLM := &scriptedTurnLLM{
		name: "claude-opus-4-6",
		turns: []*adkmodel.LLMResponse{
			{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{{
						FunctionCall: &genai.FunctionCall{
							ID:   "call_sub_1",
							Name: "researcher",
							Args: map[string]any{"request": "investigate"},
						},
					}},
				},
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount:     10_000,
					CandidatesTokenCount: 5_000,
					TotalTokenCount:      15_000,
				},
				TurnComplete: true,
			},
			{
				Content: &genai.Content{
					Role:  genai.RoleModel,
					Parts: []*genai.Part{{Text: "all done"}},
				},
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount:     20_000,
					CandidatesTokenCount: 10_000,
					TotalTokenCount:      30_000,
				},
				TurnComplete: true,
			},
			// Third response is used by the SkipParentUsage subtask below.
			{
				Content: &genai.Content{
					Role:  genai.RoleModel,
					Parts: []*genai.Part{{Text: "digest output"}},
				},
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount:     50_000,
					CandidatesTokenCount: 25_000,
					TotalTokenCount:      75_000,
				},
				TurnComplete: true,
			},
		},
	}

	parentRate := usage.PriceFor("claude-opus-4-6", nil)
	subRate := usage.PriceFor("gemini-2.5-flash", nil)
	if parentRate.Unpriced || subRate.Unpriced {
		t.Fatalf("expected built-in rates for claude-opus-4-6 and gemini-2.5-flash, got %+v / %+v", parentRate, subRate)
	}

	noTitle := false
	cfg := &config.Config{
		Agent: config.AgentConfig{
			SessionTitle: &noTitle,
		},
	}

	deps := SessionFactoryDeps{
		DaemonCtx:      ctx,
		Model:          parentLLM,
		Template:       permissions.New(permissions.Options{Mode: permissions.ModeYolo}),
		Subagents:      []*agent.Agent{subAgent},
		EventlogHandle: h,
		PricingRate:    parentRate,
		Cfg:            cfg,
		NoCompact:      true,
		CheckpointMode: config.CheckpointModeOff,
	}

	const sid = "sid-resume-subagent-spend"
	adCreated, cancelCreated, err := ReproduceAgent(deps, auth.Anonymous, sid, "created")
	if err != nil {
		t.Fatalf("ReproduceAgent(created): %v", err)
	}

	var tap usage.TurnTap
	for ev, runErr := range adCreated.Agent().Run(ctx, "start") {
		if runErr != nil {
			t.Fatalf("Run: %v", runErr)
		}
		tap.Observe(ev)
		if u, ok := tap.Commit(ev); ok {
			adCreated.Agent().Tracker().AppendUsage(parentLLM.Name(), u, parentRate)
		}
	}

	// Run a SkipParentUsage subtask under the same parent session (simulating
	// the primary agent's shared MCP digest LLM fallback, #717). Its child
	// session (<sid>:sub:sub.mcp_digest:...) lives in WithSessionTree, so
	// resume rebuild must skip its UsageMetadata rather than billing it to
	// the parent session.
	if _, err := adCreated.Agent().RunSubtask(ctx, agent.SubtaskSpec{
		Name:            "mcp_digest",
		SystemPrompt:    "summarize",
		UserMessage:     "raw payload",
		SkipParentUsage: true,
	}); err != nil {
		t.Fatalf("RunSubtask(SkipParentUsage): %v", err)
	}

	liveTotals := adCreated.Agent().Tracker().Totals()
	liveByModel := adCreated.Agent().Tracker().TotalsByModel()

	wantCost := parentRate.CostUSD(30_000, 15_000) + subRate.CostUSD(40_000, 10_000)
	if liveTotals.InputTokens != 70_000 || liveTotals.OutputTokens != 25_000 {
		t.Fatalf("pre-kill liveTotals tokens = (%d in, %d out), want (70000 in, 25000 out)",
			liveTotals.InputTokens, liveTotals.OutputTokens)
	}
	if math.Abs(liveTotals.CostUSD-wantCost) > 1e-6 {
		t.Fatalf("pre-kill liveTotals.CostUSD = %f, want %f", liveTotals.CostUSD, wantCost)
	}

	// Kill/evict the in-memory session and resume from eventlog.
	cancelCreated()

	adResumed, cancelResumed, err := ReproduceAgent(deps, auth.Anonymous, sid, "resumed")
	if err != nil {
		t.Fatalf("ReproduceAgent(resumed): %v", err)
	}
	t.Cleanup(cancelResumed)

	resumedTotals := adResumed.Agent().Tracker().Totals()
	resumedByModel := adResumed.Agent().Tracker().TotalsByModel()

	if resumedTotals.Turns != liveTotals.Turns {
		t.Errorf("resumed Turns = %d, want %d (subagent turns dropped or SkipParentUsage leaked)",
			resumedTotals.Turns, liveTotals.Turns)
	}
	if resumedTotals.InputTokens != liveTotals.InputTokens {
		t.Errorf("resumed InputTokens = %d, want %d", resumedTotals.InputTokens, liveTotals.InputTokens)
	}
	if resumedTotals.OutputTokens != liveTotals.OutputTokens {
		t.Errorf("resumed OutputTokens = %d, want %d", resumedTotals.OutputTokens, liveTotals.OutputTokens)
	}
	if math.Abs(resumedTotals.CostUSD-liveTotals.CostUSD) > 1e-6 {
		t.Errorf("resumed CostUSD = %f, want %f (subagent spend missing or mispriced at parent rate)",
			resumedTotals.CostUSD, liveTotals.CostUSD)
	}
	for modelName, want := range liveByModel {
		got, ok := resumedByModel[modelName]
		if !ok {
			t.Errorf("resumed TotalsByModel missing model %q (got keys: %v)", modelName, resumedByModel)
			continue
		}
		if got.InputTokens != want.InputTokens || got.OutputTokens != want.OutputTokens || math.Abs(got.CostUSD-want.CostUSD) > 1e-6 {
			t.Errorf("resumed TotalsByModel[%q] = %+v, want %+v", modelName, got, want)
		}
	}
}
