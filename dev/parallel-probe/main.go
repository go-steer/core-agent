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

// Diagnostic: measure how often the model emits multiple tool calls
// per assistant turn. ADK already dispatches a single-message
// multi-call response concurrently (see
// google.golang.org/adk/internal/llminternal/base_flow.go:585
// handleFunctionCalls — sync.WaitGroup over fnCalls). The remaining
// question is whether the model produces those multi-call responses
// in the first place. This probe answers that for a real workflow.
//
// Usage (from the repo root, with Vertex env sourced):
//
//	go run ./dev/parallel-probe                       # search task, no nudge
//	go run ./dev/parallel-probe --nudge               # same task, with batching nudge
//	go run ./dev/parallel-probe --task=multiread      # baseline: 5 independent reads
//	go run ./dev/parallel-probe --task=multiread --nudge
//
// Output is a per-turn batch histogram so you can eyeball whether
// the nudge changes behavior before committing to anything.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/models"
	_ "github.com/go-steer/core-agent/pkg/models/anthropic"
	_ "github.com/go-steer/core-agent/pkg/models/gemini"
	"github.com/go-steer/core-agent/pkg/permissions"
	"github.com/go-steer/core-agent/pkg/tools"
)

const nudgeText = " When you need to perform multiple independent operations (e.g. reading several files, running multiple greps on different paths), emit ALL the tool calls in a single assistant message so they run in parallel. Do not issue them one at a time when you already know the set you need."

var tasks = map[string]string{
	"multiread": `Read each of these five files and report how many lines each one has: tools/grep.go, tools/glob.go, tools/file.go, tools/bash.go, tools/todo.go. Output one line per file as "<path>: N lines". No other commentary.`,
	"search":    `Find every place in the current working directory (a Go codebase) where an error string containing the substring "tool not found" is constructed or returned. For each occurrence give the file path, the enclosing function name, and one sentence describing when that error triggers.`,
}

func main() {
	nudge := flag.Bool("nudge", false, "add the parallel-batching instruction to the system prompt")
	taskID := flag.String("task", "search", "task to run: search | multiread")
	verbose := flag.Bool("v", false, "print per-event details as they arrive")
	providerFlag := flag.String("provider", "vertex", "provider: vertex | anthropic-vertex | anthropic | gemini")
	modelFlag := flag.String("model", "", "model name (default chosen per provider)")
	noBash := flag.Bool("no-bash", false, "disable the bash tool — forces the model onto structured tools")
	flag.Parse()

	prompt, ok := tasks[*taskID]
	if !ok {
		log.Fatalf("unknown task %q (have: search, multiread)", *taskID)
	}

	cfg := config.DefaultConfig()
	cfg.Model.Provider = *providerFlag
	if *modelFlag != "" {
		cfg.Model.Name = *modelFlag
	} else if *providerFlag == config.ProviderAnthropic || *providerFlag == config.ProviderAnthropicVertex {
		cfg.Model.Name = "claude-opus-4-7"
	}
	cfg.Permissions.Mode = config.PermissionModeYolo

	provider, err := models.Resolve(cfg)
	if err != nil {
		log.Fatalf("resolve provider: %v", err)
	}
	ctx := context.Background()
	m, err := provider.Model(ctx, cfg.Model.Name)
	if err != nil {
		log.Fatalf("build model: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	gate, err := permissions.FromConfig(cfg, cwd, "", nil)
	if err != nil {
		log.Fatalf("permissions: %v", err)
	}

	builtins := tools.Default()
	if *noBash {
		builtins.Bash = false
	}
	reg, err := tools.Build(cfg, gate, "", builtins)
	if err != nil {
		log.Fatalf("tools.Build: %v", err)
	}

	instruction := "You are a code-investigation agent operating in the current working directory. Use the provided tools to explore the codebase and answer the question. Be thorough but efficient."
	if *nudge {
		instruction += nudgeText
	}

	a, err := agent.New(m,
		agent.WithInstruction(instruction),
		agent.WithTools(reg.Tools),
	)
	if err != nil {
		log.Fatalf("agent.New: %v", err)
	}

	type batch struct {
		Turn  int
		Tools []string
	}
	var batches []batch
	var finalText string
	start := time.Now()

	for event, err := range a.Run(ctx, prompt) {
		if err != nil {
			log.Fatalf("run: %v", err)
		}
		if event == nil || event.Content == nil || event.Partial {
			continue
		}
		if event.Content.Role != "model" {
			continue
		}
		var names []string
		var text string
		for _, p := range event.Content.Parts {
			if p.FunctionCall != nil {
				names = append(names, p.FunctionCall.Name)
			}
			if p.Text != "" {
				text += p.Text
			}
		}
		if len(names) > 0 {
			turn := len(batches) + 1
			batches = append(batches, batch{Turn: turn, Tools: names})
			if *verbose {
				fmt.Fprintf(os.Stderr, "turn %d: %d tool calls — %v\n", turn, len(names), names)
			}
		}
		if text != "" {
			finalText = text
		}
	}
	elapsed := time.Since(start)

	histogram := map[int]int{}
	total := 0
	maxSize := 0
	for _, b := range batches {
		n := len(b.Tools)
		histogram[n]++
		total += n
		if n > maxSize {
			maxSize = n
		}
	}
	keys := make([]int, 0, len(histogram))
	for k := range histogram {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	fmt.Println()
	fmt.Println("=== probe summary ===")
	fmt.Printf("task          : %s\n", *taskID)
	fmt.Printf("nudge         : %v\n", *nudge)
	fmt.Printf("no-bash       : %v\n", *noBash)
	fmt.Printf("model         : %s (provider=%s)\n", cfg.Model.Name, cfg.Model.Provider)
	fmt.Printf("elapsed       : %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("tool-call turns: %d\n", len(batches))
	fmt.Printf("total calls   : %d\n", total)
	if len(batches) > 0 {
		fmt.Printf("mean batch    : %.2f\n", float64(total)/float64(len(batches)))
		fmt.Printf("max batch     : %d\n", maxSize)
	}
	fmt.Println("batch histogram (size → turns):")
	for _, k := range keys {
		fmt.Printf("  %d → %d\n", k, histogram[k])
	}
	if len(batches) > 0 {
		fmt.Println("per-turn detail:")
		for _, b := range batches {
			counts := map[string]int{}
			for _, n := range b.Tools {
				counts[n]++
			}
			j, _ := json.Marshal(counts)
			fmt.Printf("  turn %d (%d calls): %s\n", b.Turn, len(b.Tools), string(j))
		}
	}
	if finalText != "" {
		fmt.Println()
		fmt.Println("=== final assistant text (head) ===")
		head := finalText
		if len(head) > 600 {
			head = head[:600] + "…"
		}
		fmt.Println(head)
	}
}
