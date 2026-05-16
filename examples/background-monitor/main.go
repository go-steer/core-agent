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

// Example: wire a BackgroundAgentManager and demonstrate the
// in-process spawn pathway end-to-end with no LLM credentials. We
// spawn two "monitor" subagents (running against the echo mock), wait
// for them to finish, and show the resulting alerts flowing through
// both the OnAlert side channel (for display) and the model-context
// drain (PrependPendingAlerts).
//
// This example is intentionally credential-free so it can run in CI.
// For a real LLM-driven demo, replace the echo provider with
// gemini.NewVertex(...) / gemini.NewAPIKey(...) and give the spawn
// tools to a real parent agent — the model decides when to spawn.
//
//	go run ./examples/background-monitor
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/models/mock"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	prov := mock.NewEcho()

	mgr, err := agent.NewBackgroundAgentManager(
		agent.WithBackgroundProvider(prov, "echo"),
		agent.WithBackgroundMaxConcurrent(4),
		agent.WithBackgroundDefaultBudgets(agent.BackgroundBudgets{
			MaxTurns: 1, MaxWallclock: 5 * time.Second,
		}),
	)
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	defer func() { _ = mgr.Close() }()

	// Install an OnAlert hook to show alerts inline as they arrive
	// — same shape the bundled CLI's REPL uses.
	mgr.OnAlert(func(a agent.Alert) {
		fmt.Printf("[hook] ↪ %s %s: %s\n", a.From, a.Kind, a.Text)
	})

	// Construct a parent agent against the same mock provider. The
	// real CLI wires the spawn tools into the parent's tool list so
	// the model can call them; this example calls Spawn directly
	// to exercise the lifecycle without an LLM round-trip.
	llm, err := prov.Model(ctx, "echo")
	if err != nil {
		return err
	}
	parent, err := agent.New(llm,
		agent.WithName("parent"),
		agent.WithInstruction("you are the parent; subagents run in parallel"),
		agent.WithBackgroundManager(mgr),
	)
	if err != nil {
		return err
	}

	// Spawn two background subagents — they'll burn their 1-turn
	// budget against the echo provider and complete.
	for _, name := range []string{"watch-cluster-a", "watch-cluster-b"} {
		h, err := mgr.Spawn(ctx, "", agent.BackgroundSpec{
			Name:         name,
			SystemPrompt: "you watch a cluster; on this echo provider you just complete",
			Goal:         "report any issues you find",
		})
		if err != nil {
			return fmt.Errorf("spawn %s: %w", name, err)
		}
		fmt.Printf("spawned: %s (branch=%s, status=%s)\n", h.Name, h.Branch, h.Status())
	}

	// Wait for both to finish so terminal alerts land before we
	// check.
	for _, h := range mgr.List() {
		select {
		case <-h.Done():
		case <-time.After(10 * time.Second):
			return fmt.Errorf("subagent %s did not finish", h.Name)
		}
	}

	// Show what the parent's next turn would see — the pre-turn
	// drain prepends the alerts to the model's prompt.
	prompt := "what's the status of the monitors?"
	rewritten := mgr.PrependPendingAlerts(prompt)
	fmt.Println("\n--- model would see ---")
	fmt.Println(rewritten)
	fmt.Println("--- end ---")

	// Note: Agent.Run would consume these alerts automatically when
	// called next; we're just printing what's pending.
	_ = parent

	// Final status summary.
	fmt.Println("\nfinal handle states:")
	for _, h := range mgr.List() {
		fmt.Printf("  %s -> %s\n", h.Name, h.Status())
	}
	return nil
}
