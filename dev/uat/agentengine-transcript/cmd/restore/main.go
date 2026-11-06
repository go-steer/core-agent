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

// Command restore reads one session tree back from Agent Engine Sessions
// through the agentengine transcript backend and prints it, the way a
// fresh process on a fresh machine would see it. It is the UAT's proof
// that the transcript outlived the local SQLite file: it opens no local
// database at all.
//
//	go run ./dev/uat/agentengine-transcript/cmd/restore \
//	  -engine projects/P/locations/L/reasoningEngines/N \
//	  -app core-agent -user alice -session <sid>
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/statestore/agentengine"
)

func main() { os.Exit(run()) }

func run() int {
	engine := flag.String("engine", os.Getenv("CORE_AGENT_TEST_REASONING_ENGINE"), "projects/P/locations/L/reasoningEngines/N")
	app := flag.String("app", "core-agent", "app name the session was created under")
	user := flag.String("user", "", "user ID the session was created under")
	sid := flag.String("session", "", "root session ID")
	flag.Parse()
	if *engine == "" || *user == "" || *sid == "" {
		fmt.Fprintln(os.Stderr, "restore: -engine, -user and -session are required")
		return 2
	}
	ctx := context.Background()
	tr, err := agentengine.Open(ctx, agentengine.Config{ReasoningEngine: *engine})
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore: %v\n", err)
		return 1
	}
	defer func() { _ = tr.Close() }()

	high, err := tr.HighestSeq(ctx, *app, *user, eventlog.RootOf(*sid))
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore: HighestSeq: %v\n", err)
		return 1
	}
	n := 0
	for e, err := range tr.Since(ctx, 0, eventlog.WithSessionTree(*app, *user, *sid)) {
		if err != nil {
			fmt.Fprintf(os.Stderr, "restore: Since: %v\n", err)
			return 1
		}
		n++
		text := ""
		if e.Event.Content != nil {
			for _, p := range e.Event.Content.Parts {
				switch {
				case p.Text != "":
					text = p.Text
				case p.FunctionCall != nil:
					text = "call " + p.FunctionCall.Name
				case p.FunctionResponse != nil:
					text = "result " + p.FunctionResponse.Name
				}
				if text != "" {
					break
				}
			}
		}
		if len(text) > 60 {
			text = text[:57] + "..."
		}
		text = strings.ReplaceAll(text, "\n", " ")
		usage := ""
		if e.Event.UsageMetadata != nil {
			usage = fmt.Sprintf(" tokens=%d/%d", e.Event.UsageMetadata.PromptTokenCount, e.Event.UsageMetadata.CandidatesTokenCount)
		}
		fmt.Printf("%4d  %-28s %-12s %s%s\n", e.Seq, e.Event.ID, e.Event.Author, text, usage)
	}
	fmt.Printf("restore: %d events, highest seq %d\n", n, high)
	if n == 0 {
		return 1
	}
	return 0
}
