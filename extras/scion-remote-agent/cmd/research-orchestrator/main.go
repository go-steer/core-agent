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

// Command research-orchestrator is the parent agent for the
// scion-research-demo. It runs inside a Scion-managed container,
// fans research work out to in-process subagents via spawn_agent,
// and escalates flagged findings to sibling Scion containers via
// spawn_remote_agent (backed by scionremote.Spawner).
//
// Wiring is intentionally similar to extras/scion-agent/main.go:
//
//   - --input <task> seeds the first turn
//   - Stdin lines arrive via Agent.Inject(...) for mid-run messages
//     (Scion delivers `scion message <agent>` via tmux send-keys)
//   - BackgroundAgentManager + the four background tools register
//     spawn_agent / list_agents / check_agent / stop_agent
//   - scionremote.Spawner registers spawn_remote_agent so the model
//     can escalate to a sibling Scion container when it sees
//     something that warrants deeper investigation
//
// The investigator template uses the stock scion-agent binary
// (extras/scion-agent) — only its agents.md is customised so the
// investigator emits structured log entries (`[REPORT_ALERT]` /
// `[REPORT_COMPLETED]`) the orchestrator's Spawner classifies via
// StringPrefix.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/eventlog"
	"github.com/go-steer/core-agent/instruction"
	"github.com/go-steer/core-agent/mcp"
	"github.com/go-steer/core-agent/models"
	_ "github.com/go-steer/core-agent/models/anthropic"
	_ "github.com/go-steer/core-agent/models/gemini"
	_ "github.com/go-steer/core-agent/models/mock"
	"github.com/go-steer/core-agent/permissions"
	"github.com/go-steer/core-agent/recording"
	"github.com/go-steer/core-agent/runner"
	"github.com/go-steer/core-agent/skills"
	"github.com/go-steer/core-agent/telemetry"
	"github.com/go-steer/core-agent/tools"

	scionremote "github.com/go-steer/core-agent/extras/scion-remote-agent"
)

func main() {
	initialInput := flag.String("input", "", "initial task message; the agent processes this before reading stdin")
	cfgPath := flag.String("c", "", "config file path (default: discover .agents/config.json)")
	modelOverride := flag.String("m", "", "override model name from config")
	providerOverride := flag.String("provider", "", "override model.provider (gemini|vertex|anthropic|anthropic-vertex|echo|scripted)")
	scriptPath := flag.String("script", "", "JSONL transcript for --provider=scripted (overrides cfg.mock.script)")
	scriptStrict := flag.Bool("script-strict", false, "scripted: assert each incoming request matches the recorded one (overrides cfg.mock.strict)")
	recordTo := flag.String("record-to", "", "write a JSONL recording of all LLM turns to this path (overrides cfg.mock.record)")
	sessionDB := flag.Bool("session-db", false, "persist sessions + audit log to a durable database (default off; in-memory)")
	sessionDBPath := flag.String("session-db-path", "", "override the database path used when --session-db is set")
	investigatorTemplate := flag.String("investigator-template", "research-investigator", "Scion template name spawn_remote_agent should use for sibling containers")
	flag.Parse()

	if os.Getenv("GOOGLE_API_KEY") == "" && os.Getenv("GEMINI_API_KEY") != "" {
		_ = os.Setenv("GOOGLE_API_KEY", os.Getenv("GEMINI_API_KEY"))
	}

	os.Exit(run(*initialInput, *cfgPath, *modelOverride, *providerOverride, *scriptPath, *scriptStrict, *recordTo, *sessionDB, *sessionDBPath, *investigatorTemplate))
}

func run(initialInput, cfgPath, modelOverride, providerOverride, scriptPath string, scriptStrict bool, recordTo string, sessionDB bool, sessionDBPath, investigatorTemplate string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cwd, _ := os.Getwd()
	cfg, agentsDir, err := loadConfig(cfgPath, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "research-orchestrator: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "research-orchestrator: telemetry setup: %v\n", err)
	}
	defer func() { _ = otelShutdown(context.Background()) }()

	provider, err := models.Resolve(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "research-orchestrator: %v\n", err)
		return runner.ExitConfigError
	}
	m, err := provider.Model(ctx, cfg.Model.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "research-orchestrator: %v\n", err)
		return runner.ExitConfigError
	}
	if cfg.Mock.Record != "" {
		f, err := os.Create(cfg.Mock.Record)
		if err != nil {
			fmt.Fprintf(os.Stderr, "research-orchestrator: --record-to: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "research-orchestrator: %v\n", err)
		return runner.ExitConfigError
	}

	projectRoot := cwd
	if agentsDir != "" {
		projectRoot = filepath.Dir(agentsDir)
	}
	loaded, err := instruction.Load(projectRoot, coreHome)
	if err != nil {
		fmt.Fprintf(os.Stderr, "research-orchestrator: instruction load: %v\n", err)
	}

	send := func(s string) { fmt.Fprintln(os.Stderr, "research-orchestrator: "+s) }
	_, mcpToolsets, mcpErr := mcp.Build(ctx, agentsDir, send, gate, nil)
	if mcpErr != nil {
		fmt.Fprintf(os.Stderr, "research-orchestrator: mcp: %v\n", mcpErr)
	}
	loadedSkills, skillsErr := skills.Load(ctx, agentsDir, gate)
	if skillsErr != nil {
		fmt.Fprintf(os.Stderr, "research-orchestrator: skills: %v\n", skillsErr)
	}

	allToolsets := append([]adktool.Toolset{}, mcpToolsets...)
	if !loadedSkills.Empty() {
		allToolsets = append(allToolsets, loadedSkills.Toolset)
	}

	// Built-in tool registry (read_file, write_file, bash, etc.). The
	// catalog feeds the BackgroundAgentManager so background
	// subagents can request any of these by name.
	b := tools.Default()
	for _, name := range cfg.Tools.Disable {
		if err := b.Disable(name); err != nil {
			fmt.Fprintf(os.Stderr, "research-orchestrator: config tools.disable: %v\n", err)
			return runner.ExitConfigError
		}
	}
	reg, err := tools.Build(cfg, gate, b)
	if err != nil {
		fmt.Fprintf(os.Stderr, "research-orchestrator: built-in tools: %v\n", err)
		return runner.ExitConfigError
	}
	allTools := append([]adktool.Tool{}, reg.Tools...)

	// Scion lifecycle status tool. When sciontool isn't on PATH (local
	// development), the call is a no-op and the demo still works end-
	// to-end against scripted providers.
	statusTool, err := newScionStatusTool()
	if err != nil {
		fmt.Fprintf(os.Stderr, "research-orchestrator: status tool: %v\n", err)
		return runner.ExitConfigError
	}
	allTools = append(allTools, statusTool)

	// BackgroundAgentManager — wires spawn_agent / list_agents /
	// check_agent / stop_agent. Subagents inherit the parent's gate
	// and pick tools from the same catalog the parent uses.
	bgMgr, err := agent.NewBackgroundAgentManager(
		agent.WithBackgroundProvider(provider, cfg.Model.Name),
		agent.WithBackgroundGate(gate),
		agent.WithBackgroundCatalog(allTools),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "research-orchestrator: background manager: %v\n", err)
		return runner.ExitAgentError
	}
	allTools = append(allTools, agent.NewBackgroundSpawnTools(bgMgr)...)

	// Scion-backed RemoteAgentSpawner. New() returns ErrNotInsideScion
	// when SCION_HUB_ENDPOINT / SCION_AGENT_TOKEN / SCION_PROJECT_ID
	// are unset (running locally outside a Scion container). In that
	// case fall back to RefuseRemoteAgentSpawner so the model gets a
	// clean tool-result error instead of a panic.
	var remoteSpawner agent.RemoteAgentSpawner
	scionSpawner, err := scionremote.New(
		scionremote.WithTemplate(investigatorTemplate),
		// Default classifier = PreferStructuredPayload (falls back
		// to StringPrefix), matching the investigator template's
		// agents.md conventions.
	)
	switch {
	case errors.Is(err, scionremote.ErrNotInsideScion):
		fmt.Fprintln(os.Stderr, "research-orchestrator: scion env not detected — spawn_remote_agent will refuse with a clean error")
		remoteSpawner = agent.RefuseRemoteAgentSpawner("scion environment not configured (set SCION_HUB_ENDPOINT, SCION_AGENT_TOKEN, SCION_PROJECT_ID to enable remote spawning)")
	case err != nil:
		fmt.Fprintf(os.Stderr, "research-orchestrator: scion spawner: %v\n", err)
		return runner.ExitAgentError
	default:
		remoteSpawner = scionSpawner
	}
	remoteTool, err := agent.NewSpawnRemoteAgentTool(remoteSpawner, bgMgr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "research-orchestrator: spawn_remote_agent: %v\n", err)
		return runner.ExitAgentError
	}
	allTools = append(allTools, remoteTool)

	agentOpts := []agent.Option{
		agent.WithTools(allTools),
		agent.WithToolsets(allToolsets),
		agent.WithSystemInstructionPrefix(loaded.Instruction),
		agent.WithBackgroundManager(bgMgr),
	}

	if sessionDB || sessionDBPath != "" {
		path, err := resolveSessionDBPath(sessionDBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "research-orchestrator: --session-db-path: %v\n", err)
			return runner.ExitConfigError
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "research-orchestrator: session db dir: %v\n", err)
			return runner.ExitConfigError
		}
		handle, err := eventlog.Open(ctx, sqlite.Open(path))
		if err != nil {
			fmt.Fprintf(os.Stderr, "research-orchestrator: open session db %s: %v\n", path, err)
			return runner.ExitConfigError
		}
		defer func() { _ = handle.Close() }()
		agentOpts = append(agentOpts, agent.WithEventLog(handle))
		fmt.Fprintf(os.Stderr, "research-orchestrator: session db: %s\n", path)
	}

	a, err := agent.New(m, agentOpts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "research-orchestrator: %v\n", err)
		return runner.ExitAgentError
	}

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
				fmt.Fprintf(os.Stderr, "research-orchestrator: inject: %v\n", err)
			}
		}
	}()

	if initialInput != "" {
		fmt.Fprintf(os.Stdout, "[user]: %s\n", initialInput)
		if err := a.Inject(initialInput); err != nil {
			fmt.Fprintf(os.Stderr, "research-orchestrator: inject --input: %v\n", err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return runner.ExitOK
		case <-stdinDone:
			select {
			case <-a.InboxArrived():
				if err := streamTurn(ctx, a, "continue", os.Stdout, os.Stderr); err != nil {
					fmt.Fprintf(os.Stderr, "research-orchestrator: %v\n", err)
					return runner.ExitAgentError
				}
			default:
			}
			return runner.ExitOK
		case <-a.InboxArrived():
			if err := streamTurn(ctx, a, "continue", os.Stdout, os.Stderr); err != nil {
				fmt.Fprintf(os.Stderr, "research-orchestrator: %v\n", err)
				return runner.ExitAgentError
			}
		}
	}
}

func streamTurn(ctx context.Context, a *agent.Agent, prompt string, stdout, stderr io.Writer) error {
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
			case p.FunctionResponse != nil:
				fmt.Fprintf(stderr, "← %s\n", p.FunctionResponse.Name)
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

func resolveSessionDBPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	return filepath.Join(home, ".research-orchestrator", "sessions.db"), nil
}

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

// trimSpace is a tiny convenience used by the status tool wrapper
// below. Avoids pulling in strings just to clip whitespace.
func trimSpace(s string) string { return strings.TrimSpace(s) }
