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
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/internal/effects"
	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/compose"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/tools"
)

type scheduleStubLLM struct {
	reply string
}

func (s scheduleStubLLM) Name() string { return "schedule-stub" }
func (s scheduleStubLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s.reply}}},
			TurnComplete: true,
		}, nil)
	}
}

func TestStartDaemonSchedule_SeedsAndRunsBehindLease(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "sched.db")
	h, err := eventlog.Open(ctx, sqlite.Open(dbPath))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	defer func() { _ = h.Close() }()

	fired := make(chan compose.ScheduledFire, 1)
	sh, err := startDaemonSchedule(ctx, daemonScheduleDeps{
		EventLog: h,
		AppName:  "core-agent",
		Workload: "nightly-audit",
		Schedule: &config.ScheduleConfig{
			Every:  "1s",
			Jitter: "0s",
			Prompt: "Run audit.",
		},
		Fire: func(_ context.Context, f compose.ScheduledFire) error {
			select {
			case fired <- f:
			default:
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("startDaemonSchedule: %v", err)
	}
	defer sh.Stop()

	if !sh.Drives || sh.Trigger == nil {
		t.Fatalf("handle = %+v, want Drives=true and non-nil Trigger", sh)
	}
	rec, err := h.Schedule(ctx, "core-agent", "nightly-audit")
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if rec == nil || rec.Interval != time.Second {
		t.Fatalf("Schedule record = %+v, want seeded 1s interval", rec)
	}

	select {
	case f := <-fired:
		if f.FireNum != 1 {
			t.Errorf("FireNum = %d, want 1", f.FireNum)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for scheduled fire")
	}
}

type fakeHintedToolset struct {
	tools []adktool.Tool
}

func (f fakeHintedToolset) Name() string { return "fake-mcp" }
func (f fakeHintedToolset) Tools(adkagent.ReadonlyContext) ([]adktool.Tool, error) {
	return f.tools, nil
}

type roWrappedTool struct {
	name string
	ro   bool
}

func (w roWrappedTool) Name() string        { return w.name }
func (w roWrappedTool) Description() string { return w.name }
func (w roWrappedTool) IsLongRunning() bool { return false }
func (w roWrappedTool) ReadOnlyHint() bool  { return w.ro }

var _ adktool.Tool = roWrappedTool{}
var _ tools.ReadOnlyHinter = roWrappedTool{}

func TestBuildDaemonEffectControls_HonorsMCPReadOnlyHintsAndPrefixedPolicies(t *testing.T) {
	t.Parallel()
	ts := fakeHintedToolset{
		tools: []adktool.Tool{roWrappedTool{name: "k8s_list_pods", ro: true}},
	}

	f := false
	tr := true
	cfg := &config.Config{
		Tools: config.ToolsConfig{
			Policies: []config.ToolPolicyConfig{
				{Name: "get_nodes", Mutating: &f},
				{Name: "scale_deployment", Mutating: &tr},
				{Name: "file", Mutating: &f},
				{Name: "confirmation", Mutating: &tr},
			},
		},
		Subagents: []config.SubagentSpec{{Name: "diagnoser"}},
	}

	pred, subNames := buildDaemonEffectControls(cfg, ts)
	if !subNames["diagnoser"] {
		t.Errorf("subNames = %v, want diagnoser=true", subNames)
	}
	if got := pred("k8s_list_pods"); got != effects.ClassReadOnly {
		t.Errorf("pred(k8s_list_pods) = %v, want ClassReadOnly from MCP ReadOnlyHint", got)
	}
	if got := pred("k8s_get_nodes"); got != effects.ClassReadOnly {
		t.Errorf("pred(k8s_get_nodes) = %v, want ClassReadOnly from unprefixed policy match", got)
	}
	if got := pred("k8s_scale_deployment"); got != effects.ClassMutating {
		t.Errorf("pred(k8s_scale_deployment) = %v, want ClassMutating from unprefixed policy match", got)
	}
	if got := pred("k8s_unknown_write"); got != effects.ClassMutating {
		t.Errorf("pred(k8s_unknown_write) = %v, want ClassMutating default-deny", got)
	}
	if got := pred("write_file"); got != effects.ClassMutating {
		t.Errorf("pred(write_file) = %v, want ClassMutating (builtin must not suffix-match policy \"file\")", got)
	}
	if got := pred("adk_request_confirmation"); got != effects.ClassReadOnly {
		t.Errorf("pred(adk_request_confirmation) = %v, want ClassReadOnly (control call must not suffix-match policy \"confirmation\")", got)
	}
}

func TestScheduledTurnWaiter_IgnoresUnrelatedInFlightTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ag, err := agent.New(scheduleStubLLM{reply: "scheduled reply"})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	// Run an initial turn so ag has reached drain-inbox before the scheduled
	// prompt is injected mid-flight.
	for _, runErr := range ag.Run(ctx, "initial operator turn") {
		if runErr != nil {
			t.Fatalf("initial ag.Run: %v", runErr)
		}
	}

	waiter := &scheduledTurnWaiter{}
	sh := &daemonScheduleHandle{waiter: waiter}

	done := make(chan error, 1)
	go func() {
		done <- waiter.runScheduledTurn(ctx, ag, compose.ScheduledFire{
			FireNum: 1,
			Payload: "scheduled prompt",
		}, nil, nil)
	}()

	// Wait until runScheduledTurn has injected the payload and registered w.out.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		waiter.mu.Lock()
		ready := waiter.out != nil
		waiter.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Simulate an already-in-flight unrelated turn finishing (both success and
	// error) while the scheduled prompt is still sitting undrained in ag's inbox.
	sh.AfterTurn("unrelated turn output", nil)
	sh.AfterTurn("", errors.New("unrelated in-flight error"))

	select {
	case err := <-done:
		t.Fatalf("runScheduledTurn resolved early on unrelated turn (err=%v)", err)
	case <-time.After(30 * time.Millisecond):
	}

	// Now run the turn that drains the inbox and invoke AfterTurn.
	for _, runErr := range ag.Run(ctx, "") {
		if runErr != nil {
			t.Fatalf("ag.Run: %v", runErr)
		}
	}
	sh.AfterTurn("scheduled reply", nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runScheduledTurn err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for runScheduledTurn to complete after draining turn")
	}
}

func TestStartDaemonSchedule_ForwardsMaxFires(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fired := make(chan int64, 4)
	sh, err := startDaemonSchedule(ctx, daemonScheduleDeps{
		AppName:  "core-agent",
		Workload: "bounded-audit",
		Schedule: &config.ScheduleConfig{
			Every:    "1s",
			Jitter:   "0s",
			Prompt:   "Run bounded audit.",
			MaxFires: 1,
			Timeout:  "5s",
		},
		Fire: func(_ context.Context, f compose.ScheduledFire) error {
			fired <- f.FireNum
			return nil
		},
	})
	if err != nil {
		t.Fatalf("startDaemonSchedule: %v", err)
	}
	defer sh.Stop()

	select {
	case n := <-fired:
		if n != 1 {
			t.Errorf("FireNum = %d, want 1", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for fire 1")
	}

	// Trigger goroutine should exit on its own after reaching MaxFires=1.
	select {
	case <-sh.done:
	case <-time.After(3 * time.Second):
		t.Fatal("expected trigger to stop after MaxFires=1")
	}
}
