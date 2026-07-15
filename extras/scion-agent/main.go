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

	"github.com/glebarez/sqlite"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/eventlog"
	"github.com/go-steer/core-agent/pkg/instruction"
	"github.com/go-steer/core-agent/pkg/mcp"
	"github.com/go-steer/core-agent/pkg/models"
	_ "github.com/go-steer/core-agent/pkg/models/anthropic"
	_ "github.com/go-steer/core-agent/pkg/models/gemini"
	_ "github.com/go-steer/core-agent/pkg/models/mock"
	"github.com/go-steer/core-agent/pkg/permissions"
	"github.com/go-steer/core-agent/pkg/recording"
	"github.com/go-steer/core-agent/pkg/runner"
	"github.com/go-steer/core-agent/pkg/skills"
	"github.com/go-steer/core-agent/pkg/telemetry"
	"github.com/go-steer/core-agent/pkg/tools"
)

func main() {
	initialInput := flag.String("input", "", "initial task message; the agent processes this before reading stdin")
	cfgPath := flag.String("c", "", "config file path (default: discover .agents/config.json)")
	modelOverride := flag.String("m", "", "override model name from config")
	providerOverride := flag.String("provider", "", "override model.provider (gemini|vertex|anthropic|anthropic-vertex|echo|scripted)")
	noBuiltinTools := flag.Bool("no-builtin-tools", false, "disable the built-in tool suite ("+strings.Join(tools.BuiltinToolNames(), ", ")+")")
	disableTools := flag.String("disable-tools", "", "comma-separated list of built-in tools to disable (e.g. bash,write_file). Composes with cfg.tools.disable; ignored when --no-builtin-tools is set.")
	scriptPath := flag.String("script", "", "JSONL transcript for --provider=scripted (overrides cfg.mock.script)")
	scriptStrict := flag.Bool("script-strict", false, "scripted: assert each incoming request matches the recorded one (overrides cfg.mock.strict)")
	recordTo := flag.String("record-to", "", "write a JSONL recording of all LLM turns to this path (overrides cfg.mock.record)")
	sessionDB := flag.Bool("session-db", false, "persist sessions + audit log to a durable database (default off; in-memory)")
	sessionDBPath := flag.String("session-db-path", "", "override the database path used when --session-db is set (default: ~/.<binary>/sessions.db)")
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

	os.Exit(run(*initialInput, *cfgPath, *modelOverride, *providerOverride, *noBuiltinTools, *disableTools, *scriptPath, *scriptStrict, *recordTo, *sessionDB, *sessionDBPath))
}

func run(initialInput, cfgPath, modelOverride, providerOverride string, noBuiltinTools bool, disableTools string, scriptPath string, scriptStrict bool, recordTo string, sessionDB bool, sessionDBPath string) int {
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
	if scriptPath != "" {
		cfg.Mock.Script = scriptPath
	}
	if scriptStrict {
		cfg.Mock.Strict = true
	}
	if recordTo != "" {
		cfg.Mock.Record = recordTo
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
	if cfg.Mock.Record != "" {
		f, err := os.Create(cfg.Mock.Record)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scion-agent: --record-to: %v\n", err)
			return runner.ExitConfigError
		}
		defer f.Close()
		m = recording.NewRecorder(m, f)
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
	// nil DigestOptions: this example doesn't wire pkg/digest. Real
	// callers pass a *mcp.DigestOptions to enable the structural wrap
	// per docs/digest-design.md.
	_, mcpToolsets, mcpErr := mcp.Build(ctx, agentsDir, send, gate, nil, nil)
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
		reg, err := tools.Build(cfg, gate, agentsDir, b)
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

	agentOpts := []agent.Option{
		agent.WithTools(allTools),
		agent.WithToolsets(allToolsets),
		agent.WithSystemInstructionPrefix(loaded.Instruction),
	}

	// Durable sessions + audit log (off by default; matches
	// cmd/core-agent's flag shape so adapter behavior is uniform).
	if sessionDB || sessionDBPath != "" {
		path, err := resolveSessionDBPath(sessionDBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scion-agent: --session-db-path: %v\n", err)
			return runner.ExitConfigError
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "scion-agent: session db dir: %v\n", err)
			return runner.ExitConfigError
		}
		handle, err := eventlog.Open(ctx, sqlite.Open(path))
		if err != nil {
			fmt.Fprintf(os.Stderr, "scion-agent: open session db %s: %v\n", path, err)
			return runner.ExitConfigError
		}
		defer func() { _ = handle.Close() }()
		agentOpts = append(agentOpts, agent.WithEventLog(handle))
		fmt.Fprintf(os.Stderr, "scion-agent: session db: %s\n", path)
	}

	a, err := agent.New(m, agentOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scion-agent: %v\n", err)
		return runner.ExitAgentError
	}

	// Stdin → inbox goroutine. Each line `scion message <agent>`
	// delivers via tmux send-keys becomes a queued message. The
	// main loop drains the inbox pre-turn (via Agent.Run's built-in
	// drain), so messages arriving while a turn is in flight no
	// longer block — they land on the next turn's prompt as part of
	// the "[Inbox]" block. SIGINT/SIGTERM still cancels via the
	// ctx wired earlier.
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}
			fmt.Fprintf(os.Stdout, "[user]: %s\n", line)
			if err := a.Inject(line); err != nil {
				fmt.Fprintf(os.Stderr, "scion-agent: inject: %v\n", err)
			}
		}
	}()

	// Seed the first turn. --input is treated as "the first inbox
	// message"; if absent, we wait for stdin to deliver one before
	// the first agent turn fires.
	if initialInput != "" {
		fmt.Fprintf(os.Stdout, "[user]: %s\n", initialInput)
		if err := a.Inject(initialInput); err != nil {
			fmt.Fprintf(os.Stderr, "scion-agent: inject --input: %v\n", err)
		}
	}

	// Main loop: each iteration waits for at least one inbox
	// message, then runs a turn. Turn prompts are always
	// "continue" — the actual user input rides into the prompt via
	// Agent.Run's pre-turn inbox drain (which prepends the
	// "[Inbox]" block to "continue" on each turn).
	for {
		select {
		case <-ctx.Done():
			return runner.ExitOK
		case <-stdinDone:
			// Stdin closed. Drain any final pending inbox before
			// exiting so the last message isn't lost.
			select {
			case <-a.InboxArrived():
				if err := streamTurn(ctx, a, "continue", os.Stdout, os.Stderr); err != nil {
					fmt.Fprintf(os.Stderr, "scion-agent: %v\n", err)
					return runner.ExitAgentError
				}
			default:
			}
			return runner.ExitOK
		case <-a.InboxArrived():
			if err := streamTurn(ctx, a, "continue", os.Stdout, os.Stderr); err != nil {
				fmt.Fprintf(os.Stderr, "scion-agent: %v\n", err)
				return runner.ExitAgentError
			}
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

// resolveSessionDBPath returns the database path for --session-db.
// Override wins; default is ~/.<binary>/sessions.db so each adapter
// gets its own directory automatically.
func resolveSessionDBPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	return filepath.Join(home, "."+binaryName(), "sessions.db"), nil
}

// binaryName returns the running executable's basename (sans .exe)
// so default paths sort by binary identity. Falls back to
// "scion-agent" if os.Executable fails.
func binaryName() string {
	if exe, err := os.Executable(); err == nil {
		return strings.TrimSuffix(filepath.Base(exe), ".exe")
	}
	return "scion-agent"
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
