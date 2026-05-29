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

// Example: drive the agent loop offline by replaying a recorded JSONL
// transcript through the mock "scripted" provider — no LLM credentials
// required.
//
//	go run ./examples/replay
//
// In real usage the transcript is captured from a live provider with
// the bundled CLI's --record-to flag (or models/mock.NewRecorder from
// library code). Here it's inlined as a constant so the example runs
// standalone.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/models/mock"
)

// transcript is a 2-turn JSONL fixture. Each line is one RecordedTurn:
// the recorded request, then the response stream the inner LLM yielded
// for that turn (a Partial carrying the text, then a TurnComplete).
//
// In lenient mode (the default) the scripted provider plays these
// responses back in order without checking the incoming request — so
// the prompts in main() don't have to match what's recorded here.
const transcript = `{"request":{"Contents":[{"parts":[{"text":"q1"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"text":"Replay turn 1: hello from the transcript."}],"role":"model"},"Partial":true},{"Content":{"parts":[{"text":"Replay turn 1: hello from the transcript."}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
{"request":{"Contents":[{"parts":[{"text":"q2"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"text":"Replay turn 2: this came from the JSONL fixture."}],"role":"model"},"Partial":true},{"Content":{"parts":[{"text":"Replay turn 2: this came from the JSONL fixture."}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
`

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run is the program body extracted from main so deferred cleanups
// (the temp-file removal below) actually run when an error short-
// circuits — log.Fatal calls os.Exit, which skips defers.
func run() error {
	// Materialize the inlined transcript to a temp file because the
	// scripted provider takes a file path. (Library callers writing
	// real tests typically point this at a checked-in fixture file.)
	scriptPath, err := writeTempScript()
	if err != nil {
		return err
	}
	defer os.Remove(scriptPath)

	// strict=false (lenient) — replay regardless of the incoming
	// prompt. Pass true to assert each request's contents JSON-equal
	// the recorded ones; useful for catching prompt-construction
	// regressions in tests.
	provider, err := mock.NewScripted(scriptPath, false)
	if err != nil {
		return err
	}
	ctx := context.Background()
	m, err := provider.Model(ctx, "")
	if err != nil {
		return err
	}

	a, err := agent.New(m)
	if err != nil {
		return err
	}

	// Two prompts that deliberately don't match the recorded ones.
	// In lenient mode the scripted provider returns turn 0's response
	// for the first call and turn 1's for the second.
	for _, prompt := range []string{"What is 2+2?", "And 3+3?"} {
		fmt.Printf("user: %s\nmodel: ", prompt)
		for event, err := range a.Run(ctx, prompt) {
			if err != nil {
				return err
			}
			if event.Content == nil {
				continue
			}
			for _, p := range event.Content.Parts {
				if p.Text != "" && event.Partial {
					fmt.Print(p.Text)
				}
			}
		}
		fmt.Println()
	}
	return nil
}

func writeTempScript() (string, error) {
	f, err := os.CreateTemp("", "replay-*.jsonl")
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
