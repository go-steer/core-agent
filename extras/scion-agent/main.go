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

// Command scion-agent runs core-agent inside a Scion-managed
// container. It mirrors cmd/core-agent's wiring (config / gate /
// model / built-in tools / MCP / skills / instruction loading) but
// replaces the interactive REPL with the Scion lifecycle contract:
//
//  1. --input <task> seeds the first turn (Scion's harness appends
//     this when starting the agent with a task).
//  2. Each turn ranges over agent.Run()'s event stream and emits
//     transient activity (thinking/executing/working) to
//     $HOME/agent-info.json so Scion's UI can show what's happening.
//  3. After each turn, stdin is read for follow-up messages —
//     `scion message <agent>` delivers them via tmux send-keys, so
//     this is just a line scanner.
//  4. The model reports sticky lifecycle states (ask_user,
//     task_completed, etc.) by calling the sciontool_status ADK tool,
//     which shells out to scion's `sciontool` binary.
//
// Outside a Scion container (no $HOME, no sciontool on PATH) the
// adapter still works — the lifecycle hooks degrade to no-ops so the
// same binary is usable for local development.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	"github.com/go-steer/core-agent/skills"
	"github.com/go-steer/core-agent/telemetry"
	"github.com/go-steer/core-agent/tools"
)

func main() {
	initialInput := flag.String("input", "", "initial task message; the agent processes this before reading stdin")
	cfgPath := flag.String("c", "", "config file path (default: discover .agents/config.json)")
	modelOverride := flag.String("m", "", "override model name from config")
	providerOverride := flag.String("provider", "", "override model.provider (gemini|vertex|anthropic|anthropic-vertex)")
	noBuiltinTools := flag.Bool("no-builtin-tools", false, "disable the built-in tool suite (read_file, write_file, edit_file, list_dir, bash, todo)")
	disableTools := flag.String("disable-tools", "", "comma-separated list of built-in tools to disable (e.g. bash,write_file). Composes with cfg.tools.disable; ignored when --no-builtin-tools is set.")
	flag.Parse()

	// Scion's Gemini harness sets GEMINI_API_KEY; core-agent's gemini
	// provider also reads GEMINI_API_KEY directly, but mirror the
	// Python adapter's bridge for consistency with anything else that
	// only reads GOOGLE_API_KEY.
	if os.Getenv("GOOGLE_API_KEY") == "" && os.Getenv("GEMINI_API_KEY") != "" {
		if err := os.Setenv("GOOGLE_API_KEY", os.Getenv("GEMINI_API_KEY")); err != nil {
			fmt.Fprintf(os.Stderr, "scion-agent: bridge GEMINI_API_KEY: %v\n", err)
		}
	}

	os.Exit(run(*initialInput, *cfgPath, *modelOverride, *providerOverride, *noBuiltinTools, *disableTools))
}

func run(initialInput, cfgPath, modelOverride, providerOverride string, noBuiltinTools bool, disableTools string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cwd, _ := os.Getwd()
	cfg, agentsDir, err := loadConfig(cfgPath, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scion-agent: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "scion-agent: telemetry setup: %v\n", err)
	}
	defer func() { _ = otelShutdown(context.Background()) }()

	provider, err := models.Resolve(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scion-agent: %v\n", err)
		return runner.ExitConfigError
	}
	m, err := provider.Model(ctx, cfg.Model.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scion-agent: %v\n", err)
		return runner.ExitConfigError
	}

	userHome, _ := os.UserHomeDir()
	coreHome := ""
	if userHome != "" {
		coreHome = filepath.Join(userHome, ".core-agent")
	}

	gate, err := permissions.FromConfig(cfg, cwd, coreHome, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scion-agent: %v\n", err)
		return runner.ExitConfigError
	}

	projectRoot := cwd
	if agentsDir != "" {
		projectRoot = filepath.Dir(agentsDir)
	}
	loaded, err := instruction.Load(projectRoot, coreHome)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scion-agent: instruction load: %v\n", err)
	}

	send := func(s string) { fmt.Fprintln(os.Stderr, "scion-agent: "+s) }
	_, mcpToolsets, mcpErr := mcp.Build(ctx, agentsDir, send, gate, nil)
	if mcpErr != nil {
		fmt.Fprintf(os.Stderr, "scion-agent: mcp: %v\n", mcpErr)
	}
	loadedSkills, skillsErr := skills.Load(ctx, agentsDir, gate)
	if skillsErr != nil {
		fmt.Fprintf(os.Stderr, "scion-agent: skills: %v\n", skillsErr)
	}

	allToolsets := append([]adktool.Toolset{}, mcpToolsets...)
	if !loadedSkills.Empty() {
		allToolsets = append(allToolsets, loadedSkills.Toolset)
	}

	var allTools []adktool.Tool
	if !noBuiltinTools {
		b := tools.Default()
		for _, name := range cfg.Tools.Disable {
			if err := b.Disable(name); err != nil {
				fmt.Fprintf(os.Stderr, "scion-agent: config tools.disable: %v\n", err)
				return runner.ExitConfigError
			}
		}
		for _, name := range strings.Split(disableTools, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if err := b.Disable(name); err != nil {
				fmt.Fprintf(os.Stderr, "scion-agent: --disable-tools: %v\n", err)
				return runner.ExitConfigError
			}
		}
		reg, err := tools.Build(cfg, gate, b)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scion-agent: built-in tools: %v\n", err)
			return runner.ExitConfigError
		}
		allTools = append(allTools, reg.Tools...)
	}

	statusTool, err := StatusTool()
	if err != nil {
		fmt.Fprintf(os.Stderr, "scion-agent: status tool: %v\n", err)
		return runner.ExitConfigError
	}
	allTools = append(allTools, statusTool)

	a, err := agent.New(m,
		agent.WithTools(allTools),
		agent.WithToolsets(allToolsets),
		agent.WithSystemInstructionPrefix(loaded.Instruction),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scion-agent: %v\n", err)
		return runner.ExitAgentError
	}

	stdin := bufio.NewScanner(os.Stdin)
	stdin.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	first := initialInput
	for {
		var prompt string
		if first != "" {
			prompt = first
			first = ""
			fmt.Fprintf(os.Stdout, "[user]: %s\n", prompt)
		} else {
			fmt.Fprint(os.Stdout, "[user]: ")
			if !stdin.Scan() {
				// EOF or signal — exit cleanly. We deliberately do NOT
				// emit task_completed here; the model is responsible for
				// declaring its own task state via sciontool_status.
				return runner.ExitOK
			}
			prompt = stdin.Text()
			if prompt == "" {
				continue
			}
		}

		if err := streamTurn(ctx, a, prompt, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "scion-agent: %v\n", err)
			return runner.ExitAgentError
		}
	}
}

// streamTurn drives one turn of the agent and routes its events to
// stdout/stderr while emitting transient Scion activity on tool
// boundaries. Mirrors runner.streamTurn but without usage tracking
// (Scion's hub is the canonical aggregator) and with WriteActivity
// hooks at agent-start, tool-start, tool-end, and agent-finish.
func streamTurn(ctx context.Context, a *agent.Agent, prompt string, stdout, stderr io.Writer) error {
	WriteActivity("thinking")
	defer WriteActivity("working")

	wroteAnything := false
	for event, err := range a.Run(ctx, prompt) {
		if err != nil {
			if wroteAnything {
				fmt.Fprintln(stdout)
			}
			return fmt.Errorf("agent run: %w", err)
		}
		if event.Content == nil {
			continue
		}
		for _, p := range event.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				fmt.Fprintf(stderr, "→ %s\n", p.FunctionCall.Name)
				WriteActivity("executing")
			case p.FunctionResponse != nil:
				fmt.Fprintf(stderr, "← %s\n", p.FunctionResponse.Name)
				WriteActivity("thinking")
			case p.Text != "" && event.Partial:
				if _, err := io.WriteString(stdout, p.Text); err != nil {
					return fmt.Errorf("write stdout: %w", err)
				}
				wroteAnything = true
			}
		}
	}
	if wroteAnything {
		fmt.Fprintln(stdout)
	}
	return nil
}

// loadConfig resolves the config from cfgPath (when set) or by walking
// up from cwd looking for .agents/. Mirrors cmd/core-agent's helper —
// duplicated rather than exported because both binaries are the only
// callers and the function is small.
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
		return cfg, filepath.Dir(cfgPath), nil
	}
	return config.LoadOrDefault(cwd)
}
