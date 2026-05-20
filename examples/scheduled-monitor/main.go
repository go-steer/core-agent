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
//  3. A BackgroundAgentManager configured with
//     WithBackgroundDefaultScheduler(SleepScheduler()) — the wiring
//     a real GKE-monitoring deployment would use. Children run
//     against the echo mock so the example stays hermetic and runs
//     in CI without credentials.
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

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/models/mock"
	coretools "github.com/go-steer/core-agent/tools"
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
	fmt.Println("=== Part 3: supervisor topology with WithBackgroundDefaultScheduler ===")

	prov := mock.NewEcho()
	mgr, err := agent.NewBackgroundAgentManager(
		agent.WithBackgroundProvider(prov, "echo"),
		agent.WithBackgroundMaxConcurrent(4),
		agent.WithBackgroundDefaultBudgets(agent.BackgroundBudgets{
			MaxTurns: 1, MaxWallclock: 5 * time.Second,
		}),
		// The line of interest: every spawned subagent's
		// RunAutonomous gets WithScheduler(SleepScheduler()) unless
		// the per-spawn BackgroundSpec.Scheduler overrides.
		agent.WithBackgroundDefaultScheduler(coretools.SleepScheduler()),
	)
	if err != nil {
		return err
	}
	defer func() { _ = mgr.Close() }()

	mgr.OnAlert(func(a agent.Alert) {
		fmt.Printf("[hook] %s %s: %s\n", a.From, a.Kind, a.Text)
	})

	// Construct the parent against the same echo provider; the real
	// CLI wires the spawn tools so the model can call them — this
	// example calls mgr.Spawn directly to exercise the lifecycle
	// without an LLM round-trip.
	llm, err := prov.Model(ctx, "echo")
	if err != nil {
		return err
	}
	parent, err := agent.New(llm,
		agent.WithName("supervisor"),
		agent.WithInstruction(
			agent.DefaultInstruction+"\n\n"+
				agent.DefaultSchedulingInstruction+"\n\n"+
				"You are the supervisor of N cluster monitors. Each child runs schedule_next_turn between scans.",
		),
		agent.WithBackgroundManager(mgr),
	)
	if err != nil {
		return err
	}
	_ = parent

	// Spawn two monitors. With the echo provider they complete
	// immediately (no real schedule_next_turn call); the wiring is
	// what's being demonstrated. A real LLM would see the
	// schedule_next_turn tool in its tool list and call it between
	// scans.
	for _, name := range []string{"monitor-cluster-a", "monitor-cluster-b"} {
		h, err := mgr.Spawn(ctx, "", agent.BackgroundSpec{
			Name:         name,
			SystemPrompt: "you watch a cluster; report any anomalies",
			Goal:         "scan cluster health periodically",
			// Scheduler omitted → uses the manager default
			// (SleepScheduler we wired above). Pass "none" to opt
			// out for one-shot triage subagents, "exit_on_defer" for
			// CronJob-managed children, etc.
		})
		if err != nil {
			return fmt.Errorf("spawn %s: %w", name, err)
		}
		fmt.Printf("spawned: %s (branch=%s, status=%s)\n", h.Name, h.Branch, h.Status())
	}

	// Wait for both children to finish (the echo provider returns the
	// prompt verbatim, so they hit their 1-turn budget immediately).
	for _, h := range mgr.List() {
		select {
		case <-h.Done():
		case <-time.After(10 * time.Second):
			return fmt.Errorf("subagent %s did not finish", h.Name)
		}
	}

	fmt.Println("\nfinal handle states:")
	for _, h := range mgr.List() {
		fmt.Printf("  %s -> %s\n", h.Name, h.Status())
	}
	return nil
}
