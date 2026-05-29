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

// Example: drive a parent agent with a subagent end-to-end with no
// LLM credentials. Two scripted-mock providers — one for the parent,
// one for the research subagent — replay JSONL transcripts that
// capture: parent calls subagent, subagent returns its answer,
// parent emits a final summary.
//
// Both agents share an eventlog handle; the research subagent's
// events land in the parent's session row under branch="research"
// so an audit query against the parent session returns both. We
// inspect the resulting SQLite database at the end to show the
// agent_eventlog rows tagged by branch.
//
//	go run ./examples/with-subagent
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/eventlog"
	"github.com/go-steer/core-agent/pkg/models/mock"
)

// parentScript: the parent emits a function call to the research
// subagent on turn 1, then a final text on turn 2 after the tool
// result lands.
const parentScript = `{"request":{"Contents":[{"parts":[{"text":"summarize the project"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"functionCall":{"name":"research","args":{"request":"what does the project ship"}}}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
{"request":{"Contents":[{"parts":[{"text":"summarize the project"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"text":"The project ships an autonomous-run driver and a durable event log."}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
`

// researchScript: the subagent receives one turn and returns a
// text answer.
const researchScript = `{"request":{"Contents":[{"parts":[{"text":"what does the project ship"}],"role":"user"}]},"responses":[{"Content":{"parts":[{"text":"core-agent ships RunAutonomous, an eventlog package, and a Scion adapter."}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}
`

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "with-subagent-*")
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

	// Two scripted providers, one per agent.
	parentScriptPath, err := writeScript(dir, "parent.jsonl", parentScript)
	if err != nil {
		return err
	}
	researchScriptPath, err := writeScript(dir, "research.jsonl", researchScript)
	if err != nil {
		return err
	}
	parentProvider, err := mock.NewScripted(parentScriptPath, false)
	if err != nil {
		return err
	}
	researchProvider, err := mock.NewScripted(researchScriptPath, false)
	if err != nil {
		return err
	}
	parentLLM, err := parentProvider.Model(ctx, "")
	if err != nil {
		return err
	}
	researchLLM, err := researchProvider.Model(ctx, "")
	if err != nil {
		return err
	}

	// Build the research subagent. It shares the eventlog handle
	// so its events stream into the same database; WithSubagents
	// will overwrite its session triple to match the parent's at
	// tool-construction time.
	research, err := agent.New(researchLLM,
		agent.WithName("research"),
		agent.WithDescription("a focused research subagent"),
		agent.WithEventLog(handle),
		agent.WithSession("u", "research-session"),
		agent.WithInstruction("you are a researcher; answer concisely"),
	)
	if err != nil {
		return err
	}

	// Parent agent — wires the research subagent via WithSubagents.
	parent, err := agent.New(parentLLM,
		agent.WithName("parent"),
		agent.WithEventLog(handle),
		agent.WithSession("u", "parent-session"),
		agent.WithInstruction("you summarize; delegate fact-finding to the research subagent"),
		agent.WithSubagents([]*agent.Agent{research}),
	)
	if err != nil {
		return err
	}

	// Drive the parent. The model calls research; the tool dispatches
	// the inner runner against the parent's session.Service (with a
	// branch-injecting wrapper) and the result feeds back to the
	// parent's next turn.
	fmt.Println("== parent run ==")
	for ev, err := range parent.Run(ctx, "summarize the project") {
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
				fmt.Printf("  → %s(%v)\n", p.FunctionCall.Name, p.FunctionCall.Args)
			case p.FunctionResponse != nil:
				fmt.Printf("  ← %s -> %v\n", p.FunctionResponse.Name, p.FunctionResponse.Response)
			case p.Text != "" && !ev.Partial:
				fmt.Printf("  text: %s\n", p.Text)
			}
		}
	}

	// Inspect the audit log. The subagent runs in its own session
	// row (derived from the parent's: "parent-session:sub:research")
	// so two concurrent runners don't trip ADK's stale-session
	// optimistic-concurrency check. WithSessionTree returns parent
	// + every "<parent>:sub:%" descendant in one query.
	fmt.Println("\n== full session tree (parent + every subagent) ==")
	for entry, err := range handle.Stream.Since(ctx, 0,
		eventlog.WithSessionTree("core-agent", "u", "parent-session")) {
		if err != nil {
			return err
		}
		printEntry(entry)
	}

	return nil
}

func printEntry(entry eventlog.Entry) {
	branch := entry.Event.Branch
	if branch == "" {
		branch = "(root)"
	}
	author := entry.Event.Author
	if author == "" {
		author = "(unknown)"
	}
	fmt.Printf("  seq=%d branch=%-10s author=%s\n", entry.Seq, branch, author)
}

func writeScript(dir, name, body string) (string, error) {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		return "", err
	}
	return p, nil
}
