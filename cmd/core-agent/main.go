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

// Command core-agent is a thin CLI wrapper around the core-agent
// library. With -p PROMPT it runs a single turn and exits; without
// -p it drops into a stdin REPL that preserves conversation history
// across turns.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/instruction"
	"github.com/go-steer/core-agent/mcp"
	"github.com/go-steer/core-agent/models"
	_ "github.com/go-steer/core-agent/models/anthropic"
	_ "github.com/go-steer/core-agent/models/gemini"
	"github.com/go-steer/core-agent/permissions"
	"github.com/go-steer/core-agent/runner"
	"github.com/go-steer/core-agent/session"
	"github.com/go-steer/core-agent/skills"
	"github.com/go-steer/core-agent/telemetry"
	"github.com/go-steer/core-agent/tools"
	"github.com/go-steer/core-agent/usage"
)

func main() {
	prompt := flag.String("p", "", "single prompt; runs one turn and exits (REPL otherwise)")
	cfgPath := flag.String("c", "", "config file path (default: discover .agents/config.json)")
	modelOverride := flag.String("m", "", "override model name from config")
	providerOverride := flag.String("provider", "", "override model.provider (gemini|vertex|anthropic|anthropic-vertex)")
	noBuiltinTools := flag.Bool("no-builtin-tools", false, "disable the built-in tool suite (read_file, write_file, edit_file, list_dir, bash, todo)")
	disableTools := flag.String("disable-tools", "", "comma-separated list of built-in tools to disable (e.g. bash,write_file). Composes with cfg.tools.disable; ignored when --no-builtin-tools is set.")
	flag.Parse()

	code := run(*prompt, *cfgPath, *modelOverride, *providerOverride, *noBuiltinTools, *disableTools)
	os.Exit(code)
}

func run(prompt, cfgPath, modelOverride, providerOverride string, noBuiltinTools bool, disableTools string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cwd, _ := os.Getwd()
	cfg, agentsDir, err := loadConfig(cfgPath, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	if modelOverride != "" {
		cfg.Model.Name = modelOverride
	}
	if providerOverride != "" {
		cfg.Model.Provider = providerOverride
	}

	otelShutdown, err := telemetry.Setup(ctx, cfg.OTEL.Exporter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: telemetry setup: %v\n", err)
	}
	defer func() { _ = otelShutdown(context.Background()) }()

	provider, err := models.Resolve(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	m, err := provider.Model(ctx, cfg.Model.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}

	userHome, _ := os.UserHomeDir()
	coreHome := ""
	if userHome != "" {
		coreHome = filepath.Join(userHome, ".core-agent")
	}

	gate, err := permissions.FromConfig(cfg, cwd, coreHome, nil /* no prompter in v1 */)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}

	projectRoot := cwd
	if agentsDir != "" {
		projectRoot = filepath.Dir(agentsDir)
	}
	loaded, err := instruction.Load(projectRoot, coreHome)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: instruction load: %v\n", err)
	}

	send := func(s string) { fmt.Fprintln(os.Stderr, "core-agent: "+s) }
	_, mcpToolsets, mcpErr := mcp.Build(ctx, agentsDir, send, gate, nil)
	if mcpErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: mcp: %v\n", mcpErr)
	}
	loadedSkills, skillsErr := skills.Load(ctx, agentsDir, gate)
	if skillsErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: skills: %v\n", skillsErr)
	}

	allToolsets := append([]adktool.Toolset{}, mcpToolsets...)
	if !loadedSkills.Empty() {
		allToolsets = append(allToolsets, loadedSkills.Toolset)
	}

	// Built-in tools (read_file, write_file, edit_file, list_dir,
	// bash, todo) ship on by default. --no-builtin-tools disables
	// the whole suite; --disable-tools / cfg.tools.disable turn off
	// specific entries (composed by union).
	var builtinTools []adktool.Tool
	if !noBuiltinTools {
		b := tools.Default()
		for _, name := range cfg.Tools.Disable {
			if err := b.Disable(name); err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: config tools.disable: %v\n", err)
				return runner.ExitConfigError
			}
		}
		for _, name := range strings.Split(disableTools, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if err := b.Disable(name); err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: --disable-tools: %v\n", err)
				return runner.ExitConfigError
			}
		}
		reg, err := tools.Build(cfg, gate, b)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: built-in tools: %v\n", err)
			return runner.ExitConfigError
		}
		builtinTools = reg.Tools
	}

	tracker := usage.NewTracker()
	pricing := usage.PriceFor(cfg.Model.Name, cfg)

	opts := []agent.Option{
		agent.WithTools(builtinTools),
		agent.WithToolsets(allToolsets),
		agent.WithSystemInstructionPrefix(loaded.Instruction),
	}

	var code int
	if prompt != "" {
		code, err = runner.Headless(ctx, m, prompt, os.Stdout, os.Stderr, tracker, pricing, opts...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		}
		if code == runner.ExitOK {
			runner.WriteSummary(os.Stderr, tracker, m.Name())
			persistTranscript(agentsDir, m.Name(), prompt, tracker)
		}
		return code
	}

	code, err = runner.REPL(ctx, m, os.Stdin, os.Stdout, os.Stderr, tracker, pricing, opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
	}
	if code == runner.ExitOK {
		runner.WriteSummary(os.Stderr, tracker, m.Name())
	}
	return code
}

// loadConfig resolves the config from cfgPath (when set) or by walking
// up from cwd looking for .agents/. Returns the config plus the
// resolved agentsDir (empty when none was found).
func loadConfig(cfgPath, cwd string) (*config.Config, string, error) {
	if cfgPath != "" {
		cfg := config.DefaultConfig()
		body, err := os.ReadFile(cfgPath)
		if err != nil {
			return nil, "", fmt.Errorf("read %s: %w", cfgPath, err)
		}
		if err := json.Unmarshal(body, cfg); err != nil {
			return nil, "", fmt.Errorf("parse %s: %w", cfgPath, err)
		}
		if err := cfg.Validate(); err != nil {
			return nil, "", err
		}
		// Treat the directory containing -c as the agentsDir so MCP /
		// skills resolve relative to it.
		return cfg, filepath.Dir(cfgPath), nil
	}
	return config.LoadOrDefault(cwd)
}

func persistTranscript(agentsDir, model, prompt string, tracker *usage.Tracker) {
	if agentsDir == "" {
		return
	}
	tot := tracker.Totals()
	_, _ = session.Save(agentsDir, session.Transcript{
		Model: model,
		Messages: []session.Message{
			{Role: "user", Text: prompt},
		},
		Usage: session.Usage{
			Turns:        tot.Turns,
			InputTokens:  tot.InputTokens,
			OutputTokens: tot.OutputTokens,
			CostUSD:      tot.CostUSD,
		},
	})
}
