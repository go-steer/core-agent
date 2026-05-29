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

// Example: drive an agent through agent.RunAutonomous, simulate a
// crash via a tight max-turns budget, then continue the run with
// agent.ResumeAutonomous against the same SQLite event log.
//
//	go run ./examples/autonomous-resume
//
// No LLM credentials needed — uses the scripted mock provider.
//
// What this demonstrates end-to-end:
//
//   - Per-turn checkpoint events emitted into a durable event log.
//   - ResumeAutonomous picking up where the prior run stopped, with
//     turn count + token totals carried forward.
//   - The session lock primitive (acquired automatically) preventing
//     two simultaneous resumers from clobbering each other.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/eventlog"
	"github.com/go-steer/core-agent/pkg/models/mock"
)

// firstScript: two text turns, then exit because we cap MaxTurns at 2.
// The driver emits per-turn checkpoints + a final
// "max_turns_exceeded" checkpoint.
const firstScript = `{"request":{"Contents":[{"parts":[{"text":"work the problem"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"text":"step 1"}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
{"request":{"Contents":[{"parts":[{"text":"continue"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"text":"step 2"}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
`

// resumeScript: one turn that calls report_done, then the post-tool
// follow-up. ResumeAutonomous walks the event log, finds the latest
// (non-terminal) checkpoint, picks up where it left off.
const resumeScript = `{"request":{"Contents":[{"parts":[{"text":"continue"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"functionCall":{"name":"report_done","args":{"state":"done","detail":"resumed and finished"}}}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
{"request":{"Contents":[{"parts":[{"text":"continue"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"text":"All done after resume."}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
`

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "autonomous-resume-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	dbPath := filepath.Join(dir, "session.db")

	handle, err := eventlog.Open(ctx, sqlite.Open(dbPath))
	if err != nil {
		return fmt.Errorf("eventlog.Open: %v", err)
	}
	defer func() { _ = handle.Close() }()
	fmt.Printf("event log: %s\n\n", dbPath)

	const (
		appName   = "autonomous-resume-demo"
		userID    = "demo"
		sessionID = "long-running-task"
	)

	// === Phase 1: initial run, capped at 2 turns to simulate a
	// crash/interruption. The model never gets to call report_done.
	firstScriptPath, err := writeScript(dir, "first.jsonl", firstScript)
	if err != nil {
		return err
	}
	llm1, err := scriptedLLM(ctx, firstScriptPath)
	if err != nil {
		return err
	}
	fmt.Println("== Phase 1: RunAutonomous with MaxTurns(2) ==")
	res1, err := agent.RunAutonomous(ctx,
		func(extras []adktool.Tool) (*agent.Agent, error) {
			return agent.New(llm1,
				agent.WithAppName(appName),
				agent.WithSession(userID, sessionID),
				agent.WithEventLog(handle),
				agent.WithTools(extras),
				agent.WithInstruction("autonomous worker; call report_done when finished"),
			)
		},
		"work the problem",
		agent.WithMaxTurns(2),
	)
	if err != nil {
		return fmt.Errorf("first RunAutonomous: %v", err)
	}
	printResult("Phase 1", res1)

	// === Phase 2: resume against the same event log. The new LLM
	// completes the task on the next turn.
	resumeScriptPath, err := writeScript(dir, "resume.jsonl", resumeScript)
	if err != nil {
		return err
	}
	llm2, err := scriptedLLM(ctx, resumeScriptPath)
	if err != nil {
		return err
	}
	fmt.Println("== Phase 2: ResumeAutonomous picks up at the next turn ==")
	res2, err := agent.ResumeAutonomous(ctx,
		func(extras []adktool.Tool, sess string) (*agent.Agent, error) {
			return agent.New(llm2,
				agent.WithAppName(appName),
				agent.WithSession(userID, sess),
				agent.WithEventLog(handle),
				agent.WithTools(extras),
				agent.WithInstruction("autonomous worker; call report_done when finished"),
			)
		},
		agent.SessionRef{
			Handle:    handle,
			AppName:   appName,
			UserID:    userID,
			SessionID: sessionID,
		},
		agent.WithMaxTurns(10),
	)
	if err != nil {
		return fmt.Errorf("ResumeAutonomous: %v", err)
	}
	printResult("Phase 2", res2)

	return nil
}

func printResult(label string, r agent.RunResult) {
	fmt.Printf("[%s] reason=%s turns=%d done_detail=%q final_text=%q\n\n",
		label, r.Reason, r.Turns, r.DoneDetail, r.FinalText)
}

func writeScript(dir, name, body string) (string, error) {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

func scriptedLLM(ctx context.Context, path string) (adkmodel.LLM, error) {
	provider, err := mock.NewScripted(path, false)
	if err != nil {
		return nil, err
	}
	return provider.Model(ctx, "")
}
