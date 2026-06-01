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

// Example: parallel fan-out of N background subagents from a single
// parent assistant message — a "GKE incident triage" scenario where
// the parent receives a degradation alert for a namespace and
// dispatches one investigator per service to work concurrently. Each
// subagent's terminal report queues into the parent's alert channel;
// the parent's NEXT turn drains them via PrependPendingAlerts and
// synthesizes the root cause.
//
// Wiring shown:
//
//	BackgroundAgentManager        (capacity-capped subagent pool)
//	    │
//	    ├── NewBackgroundSpawnTools(mgr)   (model-facing spawn_agent / list_agents / check_agent / stop_agent)
//	    │
//	    └── parent agent.New(WithBackgroundManager(mgr))
//	            ↑
//	            └── scripted-mock LLM emits a turn with 4 parallel
//	                spawn_agent function calls, then synthesizes
//	                the drained alerts on the next turn.
//
// Subagents themselves run against an echo mock provider so they
// complete in one turn — this keeps the demo hermetic for CI. The
// SHAPE of the orchestration is what's being exercised; swap in
// Gemini/Anthropic + real kubectl tools to drive against a live
// cluster.
//
// What it demonstrates (relevant to GKE PE):
//
//   - One parent decision fans out to N independent investigators
//     running in parallel — wall-clock = max(per-investigation),
//     not sum.
//   - Each investigation's output is summarized into an alert
//     before reaching the parent's context, so the parent's
//     context budget scales with N findings, not N raw outputs.
//   - The orchestration is in-process — no separate scheduler,
//     no fleet coordinator. For multi-cluster fan-out across pods,
//     compose with the AX integration (extras/ax-agent/).
//
// Run:
//
//	go run ./examples/parallel-spawn
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/models/mock"
)

// Services in the (fictional) degraded namespace: api-gateway,
// checkout-svc, fraud-detector, notification-svc. Hardcoded into
// parentScript below as four spawn_agent function calls; the README
// shows how to derive them dynamically from a kubectl listing in a
// real workflow.

// parentScript: three scripted model responses.
//
// Response 1 (turn 1 model invocation) — emits four parallel
// spawn_agent function calls in a single response. Triggered by the
// operator's "investigate" prompt.
//
// Response 2 (turn 1 follow-up, after the four spawn results
// land via ADK's function-calling loop) — brief ack so the parent
// closes turn 1 cleanly without trying to synthesize before the
// subagents have reported.
//
// Response 3 (turn 2) — after the operator's follow-up prompt
// AND after the pre-turn drain prepends each completed subagent's
// terminal report, the parent synthesizes the root cause.
//
// Lenient-mode scripted mock plays these in order.
const parentScript = `{"request":{"Contents":[{"parts":[{"text":"payments-prod is degraded — investigate all four services in parallel and report back"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"functionCall":{"name":"spawn_agent","args":{"name":"triage-api-gateway","system_prompt":"you investigate a single GKE service for the on-call team","goal":"check api-gateway in payments-prod: pod status, recent restarts, last 50 log lines","max_turns":1}}},{"functionCall":{"name":"spawn_agent","args":{"name":"triage-checkout-svc","system_prompt":"you investigate a single GKE service for the on-call team","goal":"check checkout-svc in payments-prod: pod status, recent restarts, last 50 log lines","max_turns":1}}},{"functionCall":{"name":"spawn_agent","args":{"name":"triage-fraud-detector","system_prompt":"you investigate a single GKE service for the on-call team","goal":"check fraud-detector in payments-prod: pod status, recent restarts, last 50 log lines","max_turns":1}}},{"functionCall":{"name":"spawn_agent","args":{"name":"triage-notification-svc","system_prompt":"you investigate a single GKE service for the on-call team","goal":"check notification-svc in payments-prod: pod status, recent restarts, last 50 log lines","max_turns":1}}}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
{"request":{"Contents":[]},"responses":[{"Content":{"parts":[{"text":"Dispatched 4 investigators against payments-prod (api-gateway, checkout-svc, fraud-detector, notification-svc). I'll synthesize when their reports land."}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
{"request":{"Contents":[{"parts":[{"text":"what did the investigators find?"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"text":"Triage roll-up across the four payments-prod services: api-gateway and notification-svc are healthy. checkout-svc has 3 CrashLoopBackOff pods correlating with a fraud-detector connection-pool exhaustion. Root cause is in fraud-detector; recommend roll-back of fraud-detector to the prior image while we continue. Mitigation can run while the investigators continue."}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
`

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	tmp, err := os.MkdirTemp("", "parallel-spawn-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	// Scripted parent provider (deterministic two-turn arc).
	parentScriptPath := filepath.Join(tmp, "parent.jsonl")
	if err := os.WriteFile(parentScriptPath, []byte(parentScript), 0o600); err != nil {
		return err
	}
	parentProvider, err := mock.NewScripted(parentScriptPath, false)
	if err != nil {
		return err
	}

	// Echo provider for subagents. Each subagent burns its 1-turn
	// budget against echo and completes; the manager emits a
	// "report_done" alert on its behalf so the parent's pre-turn
	// drain has something to consume on turn 2.
	subagentProvider := mock.NewEcho()

	// BackgroundAgentManager: the substrate that owns subagent
	// lifecycle (capacity caps, budgets, alert channel).
	mgr, err := agent.NewBackgroundAgentManager(
		agent.WithBackgroundProvider(subagentProvider, "echo"),
		agent.WithBackgroundMaxConcurrent(8),
		agent.WithBackgroundDefaultBudgets(agent.BackgroundBudgets{
			MaxTurns:     1,
			MaxWallclock: 5 * time.Second,
		}),
	)
	if err != nil {
		return fmt.Errorf("background manager: %w", err)
	}
	defer func() { _ = mgr.Close() }()

	// OnAlert hook prints each subagent report as it lands — same
	// pattern an operator-facing TUI would use to surface progress.
	mgr.OnAlert(func(a agent.Alert) {
		fmt.Printf("  ← alert  %-30s  %s: %s\n", a.From, a.Kind, oneLine(a.Text))
	})

	// Build the parent. WithBackgroundManager wires the manager;
	// NewBackgroundSpawnTools(mgr) returns the four model-facing
	// tools (spawn_agent / list_agents / check_agent / stop_agent)
	// the model can call to manage the pool.
	parentLLM, err := parentProvider.Model(ctx, "")
	if err != nil {
		return err
	}
	parent, err := agent.New(parentLLM,
		agent.WithName("on-call-orchestrator"),
		agent.WithInstruction("you orchestrate incident triage by dispatching one investigator per service in parallel, then synthesizing their reports"),
		agent.WithBackgroundManager(mgr),
		agent.WithTools(agent.NewBackgroundSpawnTools(mgr)),
	)
	if err != nil {
		return fmt.Errorf("parent agent: %w", err)
	}

	// === Turn 1: parent receives incident, fans out 4 investigators ===
	fmt.Println("== turn 1: incident dispatch ==")
	fmt.Println("operator: payments-prod is degraded — investigate all four services in parallel and report back")
	for ev, err := range parent.Run(ctx, "payments-prod is degraded — investigate all four services in parallel and report back") {
		if err != nil {
			return fmt.Errorf("turn 1: %w", err)
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
				fmt.Printf("  → spawn_agent(%s)\n", p.FunctionCall.Args["name"])
			case p.FunctionResponse != nil:
				if status, ok := p.FunctionResponse.Response["status"]; ok {
					fmt.Printf("  ← %s -> status=%v\n", p.FunctionResponse.Name, status)
				}
			case p.Text != "" && !ev.Partial:
				fmt.Printf("  parent: %s\n", p.Text)
			}
		}
	}

	// Wait for all subagents to finish so their alerts queue
	// before the parent's next turn.
	fmt.Println("\n== waiting for investigators to complete ==")
	for _, h := range mgr.List() {
		select {
		case <-h.Done():
			fmt.Printf("  ✓ %s (status=%s)\n", h.Name, h.Status())
		case <-time.After(10 * time.Second):
			return fmt.Errorf("subagent %s did not finish", h.Name)
		}
	}

	// === Turn 2: parent's pre-turn drain prepends all alerts ===
	fmt.Println("\n== turn 2: synthesis ==")
	fmt.Println("operator: what did the investigators find?")
	for ev, err := range parent.Run(ctx, "what did the investigators find?") {
		if err != nil {
			return fmt.Errorf("turn 2: %w", err)
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p == nil || p.Text == "" || ev.Partial {
				continue
			}
			fmt.Printf("\n  parent synthesis:\n  %s\n", p.Text)
		}
	}

	fmt.Println("\n== done ==")
	fmt.Printf("(swap echo+scripted providers for real LLMs and real kubectl tools to drive against a live cluster — see README for the recipe)\n")
	return nil
}

// oneLine collapses a multi-line alert body into a single-line
// preview for the OnAlert print. The full text is still in the
// eventlog; this is just for the demo's stdout.
func oneLine(s string) string {
	const max = 80
	out := ""
	for _, r := range s {
		if r == '\n' || r == '\r' {
			if out != "" && out[len(out)-1] != ' ' {
				out += " "
			}
			continue
		}
		out += string(r)
	}
	if len(out) > max {
		out = out[:max-1] + "…"
	}
	return out
}
