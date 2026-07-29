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

// Example: one assistant turn fans out three independent background
// subagents via the spawn_agent tool family, and their reports drain
// back into the parent's next turn — all with no LLM credentials.
//
// The parent runs on the scripted mock provider: its first recorded
// response emits THREE spawn_agent function calls in a single model
// response (parallel function calling), so one turn launches the
// whole fan-out. The spawned children run on a scripted provider
// supplied via background.WithProvider — each child replays a small
// transcript that calls report_done, which pushes a "completed"
// report onto the manager's alert channel.
//
// The parent's next Run — an empty prompt, the same shape a wake-loop
// turn uses — drains those reports via PrependPendingAlerts: the
// model sees a "[Background reports]" block before anything else.
//
//	go run ./examples/parallel-spawn
//
// Narrative printed: spawn fan-out -> parallel completion -> reports
// drained into the next turn.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/agent/background"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// parentScript records the parent model's three LLM round-trips:
//
//  1. Turn 1, round 1: one response carrying THREE spawn_agent
//     function calls — the fan-out gesture.
//  2. Turn 1, round 2: after the three tool results land, the model
//     acknowledges the launch.
//  3. Turn 2 (empty-prompt drain turn): the model reacts to the
//     "[Background reports]" block the pre-turn drain injected.
const parentScript = `{"request":{"Contents":[{"parts":[{"text":"investigate logs, metrics and traces in parallel"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"functionCall":{"name":"spawn_agent","args":{"name":"recon-logs","system_prompt":"you are a log scout","goal":"scan recent logs for anomalies"}}},{"functionCall":{"name":"spawn_agent","args":{"name":"recon-metrics","system_prompt":"you are a metrics scout","goal":"look for saturation in the dashboards"}}},{"functionCall":{"name":"spawn_agent","args":{"name":"recon-traces","system_prompt":"you are a tracing scout","goal":"find the slowest spans in the last hour"}}}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
{"request":{"Contents":[{"parts":[{"text":"investigate logs, metrics and traces in parallel"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"text":"Fan-out started: recon-logs, recon-metrics and recon-traces are running in parallel. I will fold their reports into my next turn."}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
{"request":{"Contents":[{"parts":[{"text":"(drain turn)"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"text":"All three scouts reported back. Logs, metrics and traces are covered — fan-out complete."}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
`

// childScript is the transcript EVERY spawned child replays (each
// spawn gets a fresh cursor via freshScriptProvider below): round 1
// calls the autonomous driver's report_done lifecycle tool; round 2
// is the final text after the tool result lands. The done detail is
// what surfaces as the "completed" report text on the parent side.
const childScript = `{"request":{"Contents":[{"parts":[{"text":"(child goal)"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"functionCall":{"name":"report_done","args":{"state":"done","detail":"area scanned; nothing anomalous found"}}}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
{"request":{"Contents":[{"parts":[{"text":"(child goal)"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"text":"Scan complete."}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
`

// freshScriptProvider is a tiny models.Provider that returns a FRESH
// scripted replay per Model call. The manager asks its provider for
// one LLM per spawn, so this gives every child its own script cursor
// — three concurrent children never race over shared replay state
// (mock.NewScripted's own Provider hands out one shared LLM).
type freshScriptProvider struct{ path string }

func (p freshScriptProvider) Name() string { return "scripted-per-spawn" }

func (p freshScriptProvider) Model(ctx context.Context, modelID string) (adkmodel.LLM, error) {
	sp, err := mock.NewScripted(p.path, false)
	if err != nil {
		return nil, err
	}
	return sp.Model(ctx, modelID)
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()

	dir, err := os.MkdirTemp("", "parallel-spawn-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	scriptPath := filepath.Join(dir, "parent.jsonl")
	if err := os.WriteFile(scriptPath, []byte(parentScript), 0o600); err != nil {
		return err
	}
	parentProvider, err := mock.NewScripted(scriptPath, false)
	if err != nil {
		return err
	}
	parentLLM, err := parentProvider.Model(ctx, "")
	if err != nil {
		return err
	}

	childPath := filepath.Join(dir, "child.jsonl")
	if err := os.WriteFile(childPath, []byte(childScript), 0o600); err != nil {
		return err
	}

	// The manager owns every spawned child: provider, concurrency
	// cap, budgets. Construction order matters: manager first, spawn
	// tools against the manager, then the parent agent with both
	// wired — agent.New stamps the parent back-reference onto the
	// manager.
	mgr, err := background.NewManager(
		background.WithProvider(freshScriptProvider{path: childPath}, "scripted"),
		background.WithMaxConcurrent(4),
		background.WithDefaultBudgets(background.Budgets{
			MaxTurns:     3,
			MaxWallclock: 15 * time.Second,
		}),
	)
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	defer func() { _ = mgr.Close() }()

	// Side-channel copy of every report as it arrives, so we can show
	// what the pre-turn drain will inject WITHOUT consuming the
	// channel (OnAlert observes; PrependPendingAlerts drains).
	var (
		mu     sync.Mutex
		copies []background.Alert
	)
	mgr.OnAlert(func(a background.Alert) {
		mu.Lock()
		copies = append(copies, a)
		mu.Unlock()
	})

	parent, err := agent.New(parentLLM,
		agent.WithName("parent"),
		agent.WithExtraInstruction("you coordinate; fan independent work out to background subagents"),
		agent.WithTools(background.NewSpawnTools(mgr)), // spawn/list/check/stop
		agent.WithBackgroundManager(mgr),
	)
	if err != nil {
		return fmt.Errorf("agent.New: %w", err)
	}

	// --- turn 1: the model fans out three subagents in ONE response --

	fmt.Println("== turn 1: spawn fan-out ==")
	if err := runTurn(ctx, parent, "investigate logs, metrics and traces in parallel"); err != nil {
		return err
	}

	// --- parallel completion ------------------------------------------

	handles := mgr.List()
	if len(handles) != 3 {
		return fmt.Errorf("expected 3 spawned subagents, got %d", len(handles))
	}
	fmt.Println("\n== waiting for the three subagents (parallel) ==")
	for _, h := range handles {
		select {
		case <-h.Done():
			fmt.Printf("  %-13s -> %s\n", h.Name, h.Status())
		case <-time.After(30 * time.Second):
			return fmt.Errorf("subagent %s did not finish", h.Name)
		}
	}

	// --- turn 2: the pre-turn drain injects the reports ---------------

	// Show what the next turn's prompt will carry. The real drain
	// happens inside parent.Run via PrependPendingAlerts; these are
	// the OnAlert copies, so nothing is consumed early.
	mu.Lock()
	fmt.Println("\n== reports pending for the next turn (prompt-visible) ==")
	fmt.Println("[Background reports]")
	for _, a := range copies {
		fmt.Printf("- [%s] (%s) %s\n", a.From, a.Kind, a.Text)
	}
	n := len(copies)
	mu.Unlock()
	if n < 3 {
		return fmt.Errorf("expected at least 3 pending reports, got %d", n)
	}

	fmt.Println("\n== turn 2: empty prompt — Run drains the reports into the model ==")
	if err := runTurn(ctx, parent, ""); err != nil {
		return err
	}

	fmt.Println("\nfan-out complete: 1 turn spawned 3 subagents; their reports drained into the next turn")
	return nil
}

// runTurn drives one parent turn and prints the interesting events:
// outgoing function calls, tool results, and final text.
func runTurn(ctx context.Context, parent *agent.Agent, prompt string) error {
	for ev, err := range parent.Run(ctx, prompt) {
		if err != nil {
			return err
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p == nil {
				continue
			}
			switch {
			case p.FunctionCall != nil:
				fmt.Printf("  -> %s(%v)\n", p.FunctionCall.Name, p.FunctionCall.Args["name"])
			case p.FunctionResponse != nil:
				fmt.Printf("  <- %s: %v\n", p.FunctionResponse.Name, p.FunctionResponse.Response)
			case p.Text != "" && !ev.Partial:
				fmt.Printf("  text: %s\n", p.Text)
			}
		}
	}
	return nil
}
