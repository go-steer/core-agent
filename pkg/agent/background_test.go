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

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/pkg/models"
	"github.com/go-steer/core-agent/pkg/models/mock"
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

func newFakeManager(t *testing.T) (*BackgroundAgentManager, models.Provider) {
	t.Helper()
	prov := mock.NewEcho()
	mgr, err := NewBackgroundAgentManager(
		WithBackgroundProvider(prov, "echo"),
		WithBackgroundMaxConcurrent(4),
		WithBackgroundAlertBuffer(16),
	)
	if err != nil {
		t.Fatalf("NewBackgroundAgentManager: %v", err)
	}
	return mgr, prov
}

// newTestParent constructs a real *Agent wired to the manager and
// backed by the echo mock provider. Tests use this rather than a
// bare struct literal so the session.Service + agent wiring is
// realistic (Spawn dereferences both).
func newTestParent(t *testing.T, mgr *BackgroundAgentManager) *Agent {
	t.Helper()
	llm, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("mock provider Model: %v", err)
	}
	a, err := New(llm, WithBackgroundManager(mgr))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}

func TestNewBackgroundAgentManager_ProviderRequired(t *testing.T) {
	t.Parallel()
	_, err := NewBackgroundAgentManager()
	if err == nil {
		t.Fatalf("expected error when provider is missing")
	}
}

func TestNewBackgroundAgentManager_ModelIDRequired(t *testing.T) {
	t.Parallel()
	_, err := NewBackgroundAgentManager(WithBackgroundProvider(mock.NewEcho(), ""))
	if err == nil {
		t.Fatalf("expected error when modelID is empty")
	}
}

func TestSpawn_ParentRequired(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	_, err := mgr.Spawn(context.Background(), "", BackgroundSpec{
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
	mgr.attachParent(&Agent{appName: "test", userID: "u", sessionID: "s"})

	cases := []string{"", " ", "has space", "has.dot", "has/slash"}
	for _, name := range cases {
		_, err := mgr.Spawn(context.Background(), "", BackgroundSpec{
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
	mgr.attachParent(&Agent{appName: "test", userID: "u", sessionID: "s"})

	if _, err := mgr.Spawn(context.Background(), "", BackgroundSpec{Name: "n", Goal: "g"}); err == nil {
		t.Errorf("expected error when SystemPrompt is missing")
	}
	if _, err := mgr.Spawn(context.Background(), "", BackgroundSpec{Name: "n", SystemPrompt: "p"}); err == nil {
		t.Errorf("expected error when Goal is missing")
	}
}

func TestSpawn_DepthCap(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	mgr.attachParent(&Agent{appName: "test", userID: "u", sessionID: "s"})
	// Default cap is 2; simulate depth 2 in ctx so Spawn rejects.
	ctx := context.WithValue(context.Background(), subagentDepthKey{}, 2)
	_, err := mgr.Spawn(ctx, "", BackgroundSpec{Name: "n", SystemPrompt: "p", Goal: "g"})
	if !errors.Is(err, ErrDepthExceeded) {
		t.Errorf("expected ErrDepthExceeded; got %v", err)
	}
}

func TestSpawn_UnknownToolReturnsError(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	mgr.attachParent(&Agent{appName: "test", userID: "u", sessionID: "s"})
	_, err := mgr.Spawn(context.Background(), "", BackgroundSpec{
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
	h, err := mgr.Spawn(context.Background(), "", BackgroundSpec{
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
	_, err = mgr.Spawn(context.Background(), "", BackgroundSpec{
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
	h, err := mgr.Spawn(context.Background(), "", BackgroundSpec{
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
	mgr.attachParent(&Agent{appName: "test", userID: "u", sessionID: "s"})
	for _, name := range []string{"a", "b", "c"} {
		if _, err := mgr.Spawn(context.Background(), "", BackgroundSpec{
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
	_, err := mgr.Spawn(context.Background(), "", BackgroundSpec{
		Name: "after", SystemPrompt: "p", Goal: "g",
	})
	if !errors.Is(err, ErrManagerClosed) {
		t.Errorf("expected ErrManagerClosed after Close; got %v", err)
	}
}

func TestPushAlert_DropsOldestWhenFull(t *testing.T) {
	t.Parallel()
	mgr, err := NewBackgroundAgentManager(
		WithBackgroundProvider(mock.NewEcho(), "echo"),
		WithBackgroundAlertBuffer(2),
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
	h, err := mgr.Spawn(context.Background(), "", BackgroundSpec{
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
	mgr, err := NewBackgroundAgentManager(
		WithBackgroundProvider(mock.NewEcho(), "echo"),
		WithBackgroundCatalog([]tool.Tool{dummy}),
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
	mgr, err := NewBackgroundAgentManager(
		WithBackgroundProvider(mock.NewEcho(), "echo"),
		WithBackgroundCatalog([]tool.Tool{custom}),
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

func TestSpawn_BudgetExceedanceClassifiedAsDeferred(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	parent := newTestParent(t, mgr)
	_ = parent
	h, err := mgr.Spawn(context.Background(), "", BackgroundSpec{
		Name:         "budget-exceeder",
		SystemPrompt: "say hi",
		Goal:         "say hi",
		Budgets: BackgroundBudgets{
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
