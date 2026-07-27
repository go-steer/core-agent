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

package background

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/agent/internal/subsession"
	"github.com/go-steer/core-agent/v2/pkg/models"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// newNamedStubTool returns a no-op tool whose Name() is `name`. Used
// by catalog-lookup tests that need a tool with a controlled name
// outside the auto-wired set (schedule_next_turn, report_done, etc.).
func newNamedStubTool(t *testing.T, name string) tool.Tool {
	t.Helper()
	type empty struct{}
	tl, err := functiontool.New(
		functiontool.Config{Name: name, Description: "stub"},
		func(_ tool.Context, _ empty) (empty, error) { return empty{}, nil },
	)
	if err != nil {
		t.Fatalf("functiontool.New(%q): %v", name, err)
	}
	return tl
}

func newFakeManager(t *testing.T) (*Manager, models.Provider) {
	t.Helper()
	prov := mock.NewEcho()
	mgr, err := NewManager(
		WithProvider(prov, "echo"),
		WithMaxConcurrent(4),
		WithAlertBuffer(16),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr, prov
}

// newTestParent constructs a real *Agent wired to the manager and
// backed by the echo mock provider. Tests use this rather than a
// bare struct literal so the session.Service + agent wiring is
// realistic (Spawn dereferences both).
func newTestParent(t *testing.T, mgr *Manager) *agent.Agent {
	t.Helper()
	llm, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("mock provider Model: %v", err)
	}
	a, err := agent.New(llm, agent.WithBackgroundManager(mgr))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}

func TestNewBackgroundAgentManager_ProviderRequired(t *testing.T) {
	t.Parallel()
	_, err := NewManager()
	if err == nil {
		t.Fatalf("expected error when provider is missing")
	}
}

func TestNewBackgroundAgentManager_ModelIDRequired(t *testing.T) {
	t.Parallel()
	_, err := NewManager(WithProvider(mock.NewEcho(), ""))
	if err == nil {
		t.Fatalf("expected error when modelID is empty")
	}
}

func TestSpawn_ParentRequired(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	_, err := mgr.Spawn(context.Background(), "", Spec{
		Name: "x", SystemPrompt: "go", Goal: "go",
	})
	if !errors.Is(err, ErrNoParent) {
		t.Errorf("expected ErrNoParent; got %v", err)
	}
}

func TestSpawn_RejectsInvalidName(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	// Attach a parent so we get past the parent-presence check.
	newTestParent(t, mgr)

	cases := []string{"", " ", "has space", "has.dot", "has/slash"}
	for _, name := range cases {
		_, err := mgr.Spawn(context.Background(), "", Spec{
			Name: name, SystemPrompt: "go", Goal: "go",
		})
		if err == nil {
			t.Errorf("expected error for invalid name %q", name)
		}
	}
}

func TestSpawn_RejectsMissingSystemPromptOrGoal(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	newTestParent(t, mgr)

	if _, err := mgr.Spawn(context.Background(), "", Spec{Name: "n", Goal: "g"}); err == nil {
		t.Errorf("expected error when SystemPrompt is missing")
	}
	if _, err := mgr.Spawn(context.Background(), "", Spec{Name: "n", SystemPrompt: "p"}); err == nil {
		t.Errorf("expected error when Goal is missing")
	}
}

func TestSpawn_DepthCap(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	newTestParent(t, mgr)
	// Default cap is 2; simulate depth 2 in ctx so Spawn rejects.
	ctx := subsession.WithDepth(context.Background(), 2)
	_, err := mgr.Spawn(ctx, "", Spec{Name: "n", SystemPrompt: "p", Goal: "g"})
	if !errors.Is(err, ErrDepthExceeded) {
		t.Errorf("expected ErrDepthExceeded; got %v", err)
	}
}

func TestSpawn_UnknownToolReturnsError(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	newTestParent(t, mgr)
	_, err := mgr.Spawn(context.Background(), "", Spec{
		Name: "n", SystemPrompt: "p", Goal: "g",
		Tools: []string{"no_such_tool"},
	})
	if !errors.Is(err, ErrUnknownTool) {
		t.Errorf("expected ErrUnknownTool; got %v", err)
	}
	// The reservation should have been undone.
	if _, ok := mgr.Get("n"); ok {
		t.Errorf("manager should not have a handle after a failed Spawn")
	}
}

func TestSpawn_NameMustBeUnique(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	parent := newTestParent(t, mgr)
	_ = parent
	// First Spawn should succeed (echo provider means RunAutonomous
	// runs against a no-op model).
	h, err := mgr.Spawn(context.Background(), "", Spec{
		Name: "shared", SystemPrompt: "p", Goal: "g",
	})
	if err != nil {
		t.Fatalf("first Spawn: %v", err)
	}
	defer mgr.Close()
	if h == nil {
		t.Fatal("Spawn returned nil handle on success")
	}
	// Second Spawn with the same name should reject before launching.
	_, err = mgr.Spawn(context.Background(), "", Spec{
		Name: "shared", SystemPrompt: "p", Goal: "g",
	})
	if !errors.Is(err, ErrSubagentExists) {
		t.Errorf("expected ErrSubagentExists on duplicate name; got %v", err)
	}
}

func TestManager_Stop_TransitionsToStopped(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	parent := newTestParent(t, mgr)
	_ = parent
	h, err := mgr.Spawn(context.Background(), "", Spec{
		Name: "stopme", SystemPrompt: "p", Goal: "g",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := mgr.Stop("stopme"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-h.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("subagent goroutine didn't exit after Stop")
	}
	if h.Status() != StatusStopped {
		t.Errorf("status after Stop = %v, want StatusStopped", h.Status())
	}
}

func TestManager_Close_StopsEverything(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	newTestParent(t, mgr)
	for _, name := range []string{"a", "b", "c"} {
		if _, err := mgr.Spawn(context.Background(), "", Spec{
			Name: name, SystemPrompt: "p", Goal: "g",
		}); err != nil {
			t.Fatalf("spawn %s: %v", name, err)
		}
	}
	if err := mgr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Second Close is a no-op.
	if err := mgr.Close(); err != nil {
		t.Errorf("Close second call: %v", err)
	}
	// New Spawn after Close should reject.
	_, err := mgr.Spawn(context.Background(), "", Spec{
		Name: "after", SystemPrompt: "p", Goal: "g",
	})
	if !errors.Is(err, ErrManagerClosed) {
		t.Errorf("expected ErrManagerClosed after Close; got %v", err)
	}
}

func TestPushAlert_DropsOldestWhenFull(t *testing.T) {
	t.Parallel()
	mgr, err := NewManager(
		WithProvider(mock.NewEcho(), "echo"),
		WithAlertBuffer(2),
	)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	// Fill the buffer.
	mgr.pushAlert(Alert{From: "a", Text: "1"})
	mgr.pushAlert(Alert{From: "a", Text: "2"})
	// Third push triggers drop-oldest.
	mgr.pushAlert(Alert{From: "a", Text: "3"})

	got := []string{}
drain:
	for {
		select {
		case a := <-mgr.alerts:
			got = append(got, a.Text)
		case <-time.After(50 * time.Millisecond):
			break drain
		}
	}
	if len(got) != 2 || got[0] != "2" || got[1] != "3" {
		t.Errorf("expected [\"2\", \"3\"] after drop-oldest; got %v", got)
	}
}

func TestPrependPendingAlerts_NoAlertsNoChange(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	got := mgr.PrependPendingAlerts("hello")
	if got != "hello" {
		t.Errorf("expected unchanged prompt; got %q", got)
	}
}

func TestPrependPendingAlerts_PrependsAndDrains(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	mgr.pushAlert(Alert{From: "watch-prod", Text: "pod restarted"})
	mgr.pushAlert(Alert{From: "watch-staging", Text: "all clear", Kind: "completed"})

	got := mgr.PrependPendingAlerts("what should I do?")

	if !strings.Contains(got, "[Background reports]") {
		t.Errorf("expected header in prompt; got %q", got)
	}
	if !strings.Contains(got, "[watch-prod] pod restarted") {
		t.Errorf("expected first alert; got %q", got)
	}
	if !strings.Contains(got, "[watch-staging] (completed) all clear") {
		t.Errorf("expected second alert with kind; got %q", got)
	}
	if !strings.HasSuffix(got, "what should I do?") {
		t.Errorf("expected original prompt at end; got %q", got)
	}
	// Second call should now find an empty channel.
	got2 := mgr.PrependPendingAlerts("again")
	if got2 != "again" {
		t.Errorf("second call should be no-op; got %q", got2)
	}
}

func TestSpawn_TerminalAlertIsPushed(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	parent := newTestParent(t, mgr)
	_ = parent
	h, err := mgr.Spawn(context.Background(), "", Spec{
		Name: "echoer", SystemPrompt: "say hi", Goal: "say hi",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer mgr.Close()
	// Wait for the goroutine to finish (echo provider eventually
	// exhausts budgets or returns).
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("subagent goroutine didn't finish")
	}
	// The terminal goroutine wrapper should have pushed one Alert
	// (Kind one of completed/failed/stopped depending on what
	// RunAutonomous decided). Drain Alerts() once with a short
	// timeout — the channel is unbuffered for tests with size 16.
	select {
	case a := <-mgr.Alerts():
		if a.From != "echoer" {
			t.Errorf("alert.From = %q, want echoer", a.From)
		}
		if a.Kind == "" {
			t.Errorf("alert.Kind should be set (completed/failed/stopped); got empty")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("no terminal alert was pushed after subagent finished")
	}
}

func TestResolveTools_LooksUpByName(t *testing.T) {
	t.Parallel()
	// Use a name outside the auto-wired set so the catalog lookup
	// path is exercised (auto-wired names are silently skipped — see
	// TestResolveTools_SkipsAutoWiredNames below).
	dummy := newNamedStubTool(t, "custom_inspector")
	mgr, err := NewManager(
		WithProvider(mock.NewEcho(), "echo"),
		WithCatalog([]tool.Tool{dummy}),
	)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	got, err := mgr.resolveTools([]string{"custom_inspector"})
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if len(got) != 1 || got[0] != dummy {
		t.Errorf("expected the catalog instance; got %v", got)
	}
	if _, err := mgr.resolveTools([]string{"unknown"}); !errors.Is(err, ErrUnknownTool) {
		t.Errorf("expected ErrUnknownTool; got %v", err)
	}
}

func TestResolveTools_SkipsAutoWiredNames(t *testing.T) {
	t.Parallel()
	// The runtime auto-wires schedule_next_turn / report_done /
	// report_alert / report_completed into every subagent, so a model
	// listing them in spec.Tools must NOT fail (and must not duplicate
	// either). Asserts:
	//   - auto-wired names are accepted (no ErrUnknownTool)
	//   - they're dropped from the returned slice (auto-wired instance
	//     is what actually runs)
	//   - catalog tools alongside them still resolve normally
	custom := newNamedStubTool(t, "custom_inspector")
	mgr, err := NewManager(
		WithProvider(mock.NewEcho(), "echo"),
		WithCatalog([]tool.Tool{custom}),
	)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	got, err := mgr.resolveTools([]string{
		"schedule_next_turn", "report_done", "report_alert",
		"report_completed", "custom_inspector",
	})
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if len(got) != 1 || got[0] != custom {
		t.Errorf("expected only the catalog custom_inspector after auto-wired skipping; got %v", got)
	}
}

// usageProvider is a test-local models.Provider whose LLM emits a
// UsageMetadata block on the final response so the parent tracker's
// AppendUsage path exercises end-to-end. Used to prove that background
// subagent turns roll into the parent tracker after issue #222's
// wiring in background_spawn.go.
type usageProvider struct {
	name string
	in   int32
	out  int32
}

func (p *usageProvider) Name() string { return p.name }
func (p *usageProvider) Model(_ context.Context, _ string) (adkmodel.LLM, error) {
	return &usageEmittingLLM{in: p.in, out: p.out}, nil
}

type usageEmittingLLM struct {
	in  int32
	out int32
}

func (usageEmittingLLM) Name() string { return "usage-emitter" }
func (l usageEmittingLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		content := &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: "done"}},
		}
		yield(&adkmodel.LLMResponse{Content: content, Partial: true}, nil)
		yield(&adkmodel.LLMResponse{
			Content:      content,
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     l.in,
				CandidatesTokenCount: l.out,
				TotalTokenCount:      l.in + l.out,
			},
		}, nil)
	}
}

// TestSpawn_RollsUsageIntoParentTracker is the wire-through for the
// #222 follow-up: background subagent turns must land in the parent
// agent's usage.Tracker so /usage + /stats reflect the actual session
// cost, not just the parent conversation's cost.
//
// Uses a UsageMetadata-emitting stub provider (echoLLM never sets
// usage, so the default mock can't exercise this path).
func TestSpawn_RollsUsageIntoParentTracker(t *testing.T) {
	t.Parallel()
	prov := &usageProvider{name: "usage", in: 1234, out: 56}
	mgr, err := NewManager(
		WithProvider(prov, "usage-emitter"),
		WithMaxConcurrent(2),
		WithAlertBuffer(4),
		WithDefaultBudgets(Budgets{MaxTurns: 1}),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Parent has its own usage tracker wired — the whole point of
	// this test is that background subagent turns show up in it.
	parentLLM, err := prov.Model(context.Background(), "usage-emitter")
	if err != nil {
		t.Fatalf("provider Model: %v", err)
	}
	tracker := usage.NewTracker()
	parent, err := agent.New(parentLLM, agent.WithBackgroundManager(mgr), agent.WithUsageTracker(tracker))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	_ = parent

	if tot := tracker.Totals(); tot.Turns != 0 {
		t.Fatalf("pre-spawn tracker should be empty: %+v", tot)
	}

	h, err := mgr.Spawn(context.Background(), "", Spec{
		Name: "usage-sub", SystemPrompt: "go", Goal: "go",
		Budgets: Budgets{MaxTurns: 1},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer mgr.Close()

	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("subagent goroutine didn't finish")
	}

	tot := tracker.Totals()
	if tot.Turns == 0 {
		t.Fatalf("parent tracker didn't see any subagent turns: %+v", tot)
	}
	// The subagent's model name should show up in TotalsByModel so
	// mixed-model sessions (parent frontier + background flash) can
	// render the per-model split in /usage's PerModel section.
	byModel := tracker.TotalsByModel()
	if _, ok := byModel["usage-emitter"]; !ok {
		t.Errorf("TotalsByModel missing subagent model: %+v", byModel)
	}
	// At least one turn should have carried real input tokens (the
	// stub returned 1234; RunAutonomous drives >=1 turn before the
	// max-turns cap fires).
	if tot.InputTokens == 0 {
		t.Errorf("expected non-zero InputTokens rolled into parent tracker: %+v", tot)
	}
}

// blockingModelProvider blocks inside Model() until release is closed,
// signalling entry on `entered`. This lets a test freeze Spawn in the
// exact window between handle registration (m.agents[name] = handle) and
// the goroutine launch, so it can call Stop() there — the race that used
// to strand a "stopped" subagent that kept running (#366).
type blockingModelProvider struct {
	entered chan struct{}
	release chan struct{}
	llm     *stopRaceLLM
}

func (p *blockingModelProvider) Name() string { return "blocking" }
func (p *blockingModelProvider) Model(_ context.Context, _ string) (adkmodel.LLM, error) {
	close(p.entered)
	<-p.release
	return p.llm, nil
}

// stopRaceLLM records whether GenerateContent was ever called, so a test
// can assert the autonomous loop never ran a turn.
type stopRaceLLM struct {
	genCalled atomic.Bool
}

func (*stopRaceLLM) Name() string { return "stop-race" }
func (l *stopRaceLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.genCalled.Store(true)
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}}
		yield(&adkmodel.LLMResponse{
			Content:      content,
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// TestSpawn_StopDuringResolutionCancelsBeforeLoop is the #366 regression:
// a Stop() arriving while Spawn is still resolving tools/scheduler/model
// (slow, network-capable work that runs after the handle is visible but
// before the goroutine launches) must cancel the goroutine's context so
// RunAutonomous exits immediately. Before the fix, handle.cancel was wired
// only after resolution, so such a Stop found cancel == nil, marked the
// handle Stopped, and returned — while the goroutine still launched and
// ran a full turn, burning budget under a "stopped" status.
func TestSpawn_StopDuringResolutionCancelsBeforeLoop(t *testing.T) {
	t.Parallel()
	llm := &stopRaceLLM{}
	prov := &blockingModelProvider{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		llm:     llm,
	}
	mgr, err := NewManager(
		WithProvider(prov, "stop-race"),
		WithMaxConcurrent(2),
		WithAlertBuffer(4),
		// Bound the loop so a regressed build (which runs a turn) still
		// terminates instead of spinning.
		WithDefaultBudgets(Budgets{MaxTurns: 1}),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Parent uses a separate echo LLM so the blocking provider's Model
	// is only invoked by Spawn (the window under test).
	parentLLM, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("parent Model: %v", err)
	}
	parent, err := agent.New(parentLLM, agent.WithBackgroundManager(mgr))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	_ = parent

	type spawnResult struct {
		h   *Handle
		err error
	}
	done := make(chan spawnResult, 1)
	go func() {
		h, err := mgr.Spawn(context.Background(), "", Spec{
			Name: "racer", SystemPrompt: "go", Goal: "go",
		})
		done <- spawnResult{h, err}
	}()

	// Wait until Spawn is blocked inside provider.Model — the handle is
	// already registered in m.agents but the goroutine hasn't launched.
	select {
	case <-prov.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("provider.Model was never entered")
	}

	if err := mgr.Stop("racer"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Let resolution finish; the goroutine now launches with an
	// already-cancelled context.
	close(prov.release)

	res := <-done
	if res.err != nil {
		t.Fatalf("Spawn returned error: %v", res.err)
	}
	select {
	case <-res.h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("subagent goroutine didn't exit")
	}

	if res.h.Status() != StatusStopped {
		t.Errorf("status = %v, want StatusStopped", res.h.Status())
	}
	// Core regression assertion: because Stop cancelled the context during
	// resolution, RunAutonomous must exit before ever calling the model.
	if llm.genCalled.Load() {
		t.Error("model was invoked after Stop during resolution window; goroutine ran despite being stopped (#366)")
	}
}

func TestSpawn_BudgetExceedanceClassifiedAsDeferred(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	parent := newTestParent(t, mgr)
	_ = parent
	h, err := mgr.Spawn(context.Background(), "", Spec{
		Name:         "budget-exceeder",
		SystemPrompt: "say hi",
		Goal:         "say hi",
		Budgets: Budgets{
			MaxWallclock: 1 * time.Nanosecond,
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer mgr.Close()

	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("subagent goroutine didn't finish")
	}

	if h.Status() != StatusDeferred {
		t.Errorf("status = %v, want StatusDeferred", h.Status())
	}

	select {
	case a := <-mgr.Alerts():
		if a.From != "budget-exceeder" {
			t.Errorf("alert.From = %q, want budget-exceeder", a.From)
		}
		if a.Kind != "deferred" {
			t.Errorf("alert.Kind = %q, want deferred", a.Kind)
		}
		if !strings.Contains(a.Text, "stopped: wallclock_exceeded") {
			t.Errorf("alert.Text = %q, want containing 'stopped: wallclock_exceeded'", a.Text)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("no terminal alert was pushed after subagent finished")
	}
}
