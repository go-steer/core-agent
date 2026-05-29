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

// Example: drive an agent through agent.RunAutonomous end-to-end with
// no LLM credentials. The mock "scripted" provider replays a JSONL
// transcript that the model would normally produce; the autonomous
// driver loops, watches for the report_done tool call, and returns a
// structured RunResult.
//
//	go run ./examples/autonomous
//
// Real consumers swap in a credentialled provider:
//
//	GEMINI_API_KEY=... go run ./examples/autonomous --real
//
// (the --real path is intentionally not implemented here; this
// example focuses on the loop shape and termination gesture).
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/models/mock"
)

// transcript scripts two LLM round-trips inside a single
// RunAutonomous turn:
//
//   - Round 1: the model emits a function call to report_done with
//     state="done" and a brief detail describing what it did.
//   - Round 2: after the runner executes the tool and feeds the
//     response back, the model emits a final text summary.
//
// The driver detects the done call (via the LifecycleTool handler it
// registered internally) and returns RunResult{Reason:"completed",
// Turns:1, DoneDetail:"..."}.
const transcript = `{"request":{"Contents":[{"parts":[{"text":"summarize the project"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"functionCall":{"name":"report_done","args":{"state":"done","detail":"summarized example.txt"}}}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
{"request":{"Contents":[{"parts":[{"text":"summarize the project"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"text":"Done. The project ships an autonomous-run driver."}],"role":"model"},"Partial":true},{"Content":{"parts":[{"text":"Done. The project ships an autonomous-run driver."}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
`

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run is extracted from main so deferred temp-file cleanup actually
// fires when an error short-circuits — log.Fatal calls os.Exit, which
// skips defers.
func run() error {
	scriptPath, err := writeTempScript()
	if err != nil {
		return err
	}
	defer os.Remove(scriptPath)

	provider, err := mock.NewScripted(scriptPath, false)
	if err != nil {
		return err
	}
	ctx := context.Background()
	m, err := provider.Model(ctx, "")
	if err != nil {
		return err
	}

	// build constructs the agent each time RunAutonomous starts.
	// extras carries the internal "report_done" tool the driver
	// registers; consumers compose it with their own tools here.
	build := func(extras []adktool.Tool) (*agent.Agent, error) {
		return agent.New(m,
			agent.WithInstruction(
				"You are an autonomous worker. Complete the user's goal "+
					"end-to-end without asking clarifying questions. When "+
					"finished, call report_done with state=\"done\" and a "+
					"one-sentence detail.",
			),
			agent.WithTools(extras),
		)
	}

	res, err := agent.RunAutonomous(ctx, build, "summarize the project",
		agent.WithMaxTurns(5),
	)
	if err != nil {
		return fmt.Errorf("RunAutonomous: %w", err)
	}

	fmt.Printf("reason:      %s\n", res.Reason)
	fmt.Printf("turns:       %d\n", res.Turns)
	fmt.Printf("done detail: %s\n", res.DoneDetail)
	fmt.Printf("final text:  %s\n", res.FinalText)
	fmt.Printf("duration:    %s\n", res.Duration)
	return nil
}

func writeTempScript() (string, error) {
	f, err := os.CreateTemp("", "autonomous-*.jsonl")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(transcript); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
