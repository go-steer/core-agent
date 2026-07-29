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

// Example: the supervision-tree topology from
// docs/scheduled-monitoring-design.md, end-to-end with no LLM
// credentials. Three parts run in sequence:
//
//  1. The bare Scheduler primitives (SleepScheduler,
//     ExitOnDeferScheduler) wired against fake ScheduleEvents, so the
//     reader can see what the autonomous driver does between turns.
//
//  2. The schedule_next_turn tool's channel-emit behavior driven
//     directly, no LLM in the loop — what RunAutonomous sees after a
//     turn that calls the tool.
//
//  3. A background.Manager (background.NewManager) configured with
//     background.WithDefaultScheduler(SleepScheduler()) — the wiring
//     a real GKE-monitoring deployment would use. The spawned child
//     runs against the echo mock so the example stays hermetic and
//     runs in CI without credentials. (The rest of the spawn/alert
//     pathway is demonstrated in examples/background-monitor.)
//
// For an LLM-driven demo, replace the echo provider with
// gemini.NewVertex / anthropic.NewVertex and give the parent agent
// agent.DefaultSchedulingInstruction in its system prompt — the
// model picks the cadence per the cadence ladder in the tool
// description.
//
//	go run ./examples/scheduled-monitor
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/agent/background"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()

	if err := part1Schedulers(ctx); err != nil {
		return fmt.Errorf("part 1: %w", err)
	}
	fmt.Println()
	if err := part2ScheduleTool(ctx); err != nil {
		return fmt.Errorf("part 2: %w", err)
	}
	fmt.Println()
	if err := part3SupervisorTopology(ctx); err != nil {
		return fmt.Errorf("part 3: %w", err)
	}
	return nil
}

func part1Schedulers(ctx context.Context) error {
	fmt.Println("=== Part 1: Scheduler primitives ===")

	// SleepScheduler blocks the goroutine until the event's WakeAt.
	sleep := coretools.SleepScheduler()
	wake := time.Now().Add(80 * time.Millisecond)
	start := time.Now()
	if err := sleep.BeforeNextTurn(ctx, coretools.ScheduleEvent{WakeAt: wake}); err != nil {
		return err
	}
	fmt.Printf("SleepScheduler.BeforeNextTurn returned after %v (asked for 80ms)\n", time.Since(start).Round(time.Millisecond))

	// ExitOnDeferScheduler always returns the defer sentinel.
	exit := coretools.ExitOnDeferScheduler()
	err := exit.BeforeNextTurn(ctx, coretools.ScheduleEvent{WakeAt: time.Now().Add(time.Hour)})
	fmt.Printf("ExitOnDeferScheduler.BeforeNextTurn returned: %v (== ErrSchedulerDefer: %v)\n",
		err, errors.Is(err, coretools.ErrSchedulerDefer))
	return nil
}

func part2ScheduleTool(_ context.Context) error {
	fmt.Println("=== Part 2: schedule_next_turn tool emission ===")

	tool, ch, err := coretools.NewScheduleTool(coretools.ScheduleOptions{
		MaxDefer: time.Hour,
	})
	if err != nil {
		return err
	}
	fmt.Printf("registered tool %q with description (truncated): %q...\n",
		tool.Name(), tool.Description()[:60])

	// The autonomous driver registers this tool internally; it drains
	// the channel between turns. To exercise the wiring without an
	// LLM in the loop, drive the tool's internal handler directly.
	// (Real consumers never need to do this — the driver does it.)
	fmt.Println("(simulating an LLM that calls schedule_next_turn with wake_in_sec=2, next_prompt='rescan')")

	// In an LLM run, ADK's runner invokes the tool's handler with
	// the model's args. We can't easily replicate that here without
	// pulling in ADK's runner; what matters is the *channel* shape
	// the driver consumes. Show what the driver sees by simulating
	// one event manually.
	simulated := coretools.ScheduleEvent{
		WakeAt:     time.Now().Add(2 * time.Second),
		NextPrompt: "rescan",
		Detail:     "10m cadence",
		Time:       time.Now(),
	}
	go func() {
		// In real life, the tool handler does this send; the driver
		// drains after the turn ends.
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ch:
		}
	}()
	fmt.Printf("simulated ScheduleEvent: wake_at=%s next_prompt=%q\n",
		simulated.WakeAt.Format(time.RFC3339), simulated.NextPrompt)
	return nil
}

func part3SupervisorTopology(ctx context.Context) error {
	fmt.Println("=== Part 3: manager with a default scheduler ===")

	prov := mock.NewEcho()
	mgr, err := background.NewManager(
		background.WithProvider(prov, "echo"),
		background.WithDefaultBudgets(background.Budgets{
			MaxTurns: 1, MaxWallclock: 5 * time.Second,
		}),
		// The line of interest: every spawned subagent's autonomous
		// run gets WithScheduler(SleepScheduler()) unless the
		// per-spawn Spec.Scheduler overrides — pass "none" to opt out
		// for one-shot triage subagents, "exit_on_defer" for
		// CronJob-managed children, etc.
		background.WithDefaultScheduler(coretools.SleepScheduler()),
	)
	if err != nil {
		return err
	}
	defer func() { _ = mgr.Close() }()

	// Spawn requires a parent wired via agent.WithBackgroundManager.
	// The scheduling-specific line is DefaultSchedulingInstruction —
	// the priming that teaches a real LLM the cadence ladder for
	// schedule_next_turn. Everything else about parent wiring (OnAlert
	// hooks, the PrependPendingAlerts model-context drain, spawn tools
	// in the model's tool list) is demonstrated in
	// examples/background-monitor.
	llm, err := prov.Model(ctx, "echo")
	if err != nil {
		return err
	}
	if _, err := agent.New(llm,
		agent.WithName("supervisor"),
		agent.WithMode(agent.ModeAutonomous),
		agent.WithExtraInstruction(agent.DefaultSchedulingInstruction),
		agent.WithExtraInstruction("You are the supervisor of N cluster monitors. Each child runs schedule_next_turn between scans."),
		agent.WithBackgroundManager(mgr),
	); err != nil {
		return err
	}

	// Spawn one monitor with Spec.Scheduler omitted — it inherits the
	// manager default wired above. With the echo provider it completes
	// immediately; a real LLM would see schedule_next_turn in its tool
	// list and call it between scans.
	h, err := mgr.Spawn(ctx, "", background.Spec{
		Name:         "monitor-cluster-a",
		SystemPrompt: "you watch a cluster; report any anomalies",
		Goal:         "scan cluster health periodically",
	})
	if err != nil {
		return fmt.Errorf("spawn: %w", err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		return fmt.Errorf("subagent %s did not finish", h.Name)
	}
	fmt.Printf("spawned with default scheduler: %s -> %s\n", h.Name, h.Status())

	return nil
}
