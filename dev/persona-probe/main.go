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

// persona-probe measures whether deleting the persona line ("You are
// a helpful assistant. Be concise and accurate.") from the system
// prompt regresses Gemini — the #459 merge gate (resolved question 1
// in docs/system-prompt-layering-design.md). Same posture and
// evidence standard as dev/parallel-probe: run a real tool workflow
// under both prompt variants, emit per-run behavioral metrics, and
// eyeball the delta before committing.
//
// The layered prompt (agent.CoreInstruction + quirks + overlay) ships
// WITHOUT the persona line; if this probe shows a regression on
// Gemini — degraded task completion, runaway verbosity, tool-use
// breakdown — the line ships as a GeminiPersonaQuirk with the probe
// result cited in its doc comment. The layer architecture is
// identical in both outcomes.
//
// Usage (from the repo root, with Vertex env sourced):
//
//	go run ./dev/persona-probe                       # layered prompt, no persona
//	go run ./dev/persona-probe --persona             # same, with the persona line appended
//	go run ./dev/persona-probe --task=multiread [--persona]
//	go run ./dev/persona-probe --runs=3 [...]        # repeat for stability
//
// Compare the metric lines (turns, tool calls, output chars, final
// answer present) and the transcripts across the two variants. No
// automated verdict on purpose — the parallel-probe precedent is
// human-eyeballed evidence recorded in the doc comment of whatever
// ships.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/models"
	_ "github.com/go-steer/core-agent/v2/pkg/models/gemini"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/tools"
)

// personaLine is the exact text deleted from the pre-#459 monolith,
// re-appended under --persona for the A/B comparison.
const personaLine = "You are a helpful assistant. Be concise and accurate."

var tasks = map[string]string{
	"search":    `Find every place in the current working directory (a Go codebase) where an error string containing the substring "tool not found" is constructed or returned. For each occurrence give the file path, the enclosing function name, and one sentence describing when that error triggers.`,
	"multiread": `Read each of these five files and report how many lines each one has: tools/grep.go, tools/glob.go, tools/file.go, tools/bash.go, tools/todo.go. Output one line per file as "<path>: N lines". No other commentary.`,
	"explain":   `Explain in at most five sentences what pkg/permissions does in this codebase and name its two most important exported types. Read only what you need.`,
}

func main() {
	persona := flag.Bool("persona", false, "append the pre-#459 persona line to the layered prompt")
	taskID := flag.String("task", "search", "task: search | multiread | explain")
	runs := flag.Int("runs", 1, "repetitions (report each)")
	providerFlag := flag.String("provider", "vertex", "provider: vertex | gemini")
	modelFlag := flag.String("model", "", "model name (default from provider)")
	verbose := flag.Bool("v", false, "print full final answers")
	flag.Parse()

	prompt, ok := tasks[*taskID]
	if !ok {
		log.Fatalf("unknown task %q (have: search, multiread, explain)", *taskID)
	}

	cfg := config.DefaultConfig()
	cfg.Model.Provider = *providerFlag
	if *modelFlag != "" {
		cfg.Model.Name = *modelFlag
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
	reg, err := tools.Build(cfg, gate, "", tools.Default())
	if err != nil {
		log.Fatalf("tools.Build: %v", err)
	}

	variant := "layered (no persona)"
	if *persona {
		variant = "layered + persona line"
	}
	fmt.Printf("== persona-probe: model=%s task=%s variant=%s ==\n", m.Name(), *taskID, variant)

	for run := 1; run <= *runs; run++ {
		opts := []agent.Option{agent.WithTools(reg.Tools)}
		if *persona {
			opts = append(opts, agent.WithExtraInstruction(personaLine))
		}
		a, err := agent.New(m, opts...)
		if err != nil {
			log.Fatalf("agent.New: %v", err)
		}

		var toolCalls, modelTurns, outputChars int
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
			modelTurns++
			for _, p := range event.Content.Parts {
				if p.FunctionCall != nil {
					toolCalls++
				}
				if p.Text != "" {
					outputChars += len(p.Text)
					finalText = p.Text
				}
			}
		}
		fmt.Printf("run %d: turns=%d tool_calls=%d output_chars=%d elapsed=%s final_answer=%v\n",
			run, modelTurns, toolCalls, outputChars, time.Since(start).Round(time.Second), finalText != "")
		if *verbose {
			fmt.Printf("--- final answer ---\n%s\n--------------------\n", finalText)
		}
	}
	fmt.Println("Compare against the other variant's output; record the verdict in the #459 PR (and in GeminiPersonaQuirk's doc comment if the persona line has to ship).")
}
