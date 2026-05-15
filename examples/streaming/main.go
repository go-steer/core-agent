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

// Example: drive the agent with the standard built-in tools (read_file,
// list_dir, bash, etc.) and render the response as an interactive chat
// session via runner.WriteEvents.
//
// Tool calls render in cyan, partial assistant text in green when the
// output is a terminal (auto-detected via runner.IsTerminal). Pipe
// the output (e.g. `... | cat`) and you'll get plain text — same code
// path, no escape codes leak through.
//
// Usage:
//
//	GEMINI_API_KEY=...   go run ./examples/streaming "what's in main.go in this directory?"
//	ANTHROPIC_API_KEY=... go run ./examples/streaming --provider anthropic "list the .go files here"
//
// Pick a prompt that requires tool use to see the formatter shine:
//   - "what files are in this directory?" → list_dir
//   - "summarize main.go"                  → read_file
//   - "how many lines of go code in this repo?" → bash
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/models"
	_ "github.com/go-steer/core-agent/models/anthropic"
	_ "github.com/go-steer/core-agent/models/gemini"
	"github.com/go-steer/core-agent/permissions"
	"github.com/go-steer/core-agent/runner"
	"github.com/go-steer/core-agent/tools"
)

func main() {
	provider := flag.String("provider", "", "model provider (gemini|anthropic|...); empty auto-detects from env")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: streaming [--provider NAME] <prompt>")
		os.Exit(2)
	}
	prompt := strings.Join(flag.Args(), " ")

	cfg := config.DefaultConfig()
	if *provider != "" {
		cfg.Model.Provider = *provider
	}
	// No human in the loop, so approve every tool call automatically.
	// In a real app you'd typically leave this on "ask" and wire a
	// prompter into permissions.FromConfig.
	cfg.Permissions.Mode = string(permissions.ModeYolo)

	prov, err := models.Resolve(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	m, err := prov.Model(ctx, cfg.Model.Name)
	if err != nil {
		log.Fatal(err)
	}

	cwd, _ := os.Getwd()
	gate, err := permissions.FromConfig(cfg, cwd, "", nil)
	if err != nil {
		log.Fatal(err)
	}
	reg, err := tools.Build(cfg, gate, tools.Default())
	if err != nil {
		log.Fatal(err)
	}

	a, err := agent.New(m, agent.WithTools(reg.Tools))
	if err != nil {
		log.Fatal(err)
	}

	// Echo the prompt so the on-screen log reads like a chat session.
	fmt.Printf("> %s\n", prompt)

	// out + info pointed at the same writer (stdout) gives one combined
	// stream. Easier to scan than out=stdout, info=stderr when you're
	// just watching a single agent.
	if err := runner.WriteEvents(
		a.Run(ctx, prompt),
		os.Stdout, os.Stdout,
		runner.WithColor(runner.IsTerminal(os.Stdout)),
	); err != nil {
		log.Fatal(err)
	}
}
