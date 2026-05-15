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

// Command ax-agent runs core-agent as an AX (github.com/google/ax)
// remote agent. It mirrors cmd/core-agent's wiring for config,
// permissions, model, and tools, but instead of running a REPL it
// binds a gRPC AgentService server that AX dials.
//
// Each AX execution arrives as one AgentStart carrying the full
// conversation history; the adapter rebuilds a fresh genai.Contents
// slice from those messages, runs agent.RunWithContents, streams text
// and tool-call events back as AgentOutputs, then sends AgentEnd.
// Stateless per turn — no persistent session, full history on every
// call. AX is responsible for resumption and event-log persistence.
//
// See the ../ax-multi-agent example and docs/ax-plan.md for the
// design rationale and the multi-agent ax.yaml configuration.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"google.golang.org/adk/tool"
	"google.golang.org/grpc"

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/config"
	axproto "github.com/go-steer/core-agent/extras/ax-agent/internal/axproto"
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
)

func main() {
	listen := flag.String("listen", ":50051", "gRPC bind address for the AX AgentService")
	cfgPath := flag.String("c", "", "config file path (default: discover .agents/config.json)")
	modelOverride := flag.String("m", "", "override model name from config")
	providerOverride := flag.String("provider", "", "override model.provider (gemini|vertex|anthropic|anthropic-vertex|echo|scripted)")
	noBuiltinTools := flag.Bool("no-builtin-tools", false, "disable the built-in tool suite ("+strings.Join(tools.BuiltinToolNames(), ", ")+")")
	disableTools := flag.String("disable-tools", "", "comma-separated list of built-in tools to disable. Composes with cfg.tools.disable; ignored when --no-builtin-tools is set.")
	scriptPath := flag.String("script", "", "JSONL transcript for --provider=scripted (overrides cfg.mock.script)")
	scriptStrict := flag.Bool("script-strict", false, "scripted: assert each incoming request matches the recorded one (overrides cfg.mock.strict)")
	recordTo := flag.String("record-to", "", "write a JSONL recording of all LLM turns to this path (overrides cfg.mock.record)")
	flag.Parse()

	os.Exit(run(*listen, *cfgPath, *modelOverride, *providerOverride, *noBuiltinTools, *disableTools, *scriptPath, *scriptStrict, *recordTo))
}

func run(listen, cfgPath, modelOverride, providerOverride string, noBuiltinTools bool, disableTools, scriptPath string, scriptStrict bool, recordTo string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cwd, _ := os.Getwd()
	cfg, agentsDir, err := loadConfig(cfgPath, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax-agent: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ax-agent: telemetry setup: %v\n", err)
	}
	defer func() { _ = otelShutdown(context.Background()) }()

	provider, err := models.Resolve(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax-agent: %v\n", err)
		return runner.ExitConfigError
	}
	m, err := provider.Model(ctx, cfg.Model.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax-agent: %v\n", err)
		return runner.ExitConfigError
	}
	if cfg.Mock.Record != "" {
		f, err := os.Create(cfg.Mock.Record)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ax-agent: --record-to: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ax-agent: %v\n", err)
		return runner.ExitConfigError
	}

	projectRoot := cwd
	if agentsDir != "" {
		projectRoot = filepath.Dir(agentsDir)
	}
	loaded, err := instruction.Load(projectRoot, coreHome)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax-agent: instruction load: %v\n", err)
	}

	send := func(s string) { fmt.Fprintln(os.Stderr, "ax-agent: "+s) }
	_, mcpToolsets, mcpErr := mcp.Build(ctx, agentsDir, send, gate, nil)
	if mcpErr != nil {
		fmt.Fprintf(os.Stderr, "ax-agent: mcp: %v\n", mcpErr)
	}
	loadedSkills, skillsErr := skills.Load(ctx, agentsDir, gate)
	if skillsErr != nil {
		fmt.Fprintf(os.Stderr, "ax-agent: skills: %v\n", skillsErr)
	}

	allToolsets := append([]tool.Toolset{}, mcpToolsets...)
	if !loadedSkills.Empty() {
		allToolsets = append(allToolsets, loadedSkills.Toolset)
	}

	var builtinTools []tool.Tool
	if !noBuiltinTools {
		b := tools.Default()
		for _, name := range cfg.Tools.Disable {
			if err := b.Disable(name); err != nil {
				fmt.Fprintf(os.Stderr, "ax-agent: config tools.disable: %v\n", err)
				return runner.ExitConfigError
			}
		}
		for _, name := range strings.Split(disableTools, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if err := b.Disable(name); err != nil {
				fmt.Fprintf(os.Stderr, "ax-agent: --disable-tools: %v\n", err)
				return runner.ExitConfigError
			}
		}
		reg, err := tools.Build(cfg, gate, b)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ax-agent: built-in tools: %v\n", err)
			return runner.ExitConfigError
		}
		builtinTools = reg.Tools
	}

	// One agent factory shared by every Connect call. Each call
	// constructs its own *agent.Agent so RunWithContents can use a
	// fresh session per turn.
	agentFactory := func() (*agent.Agent, error) {
		return agent.New(m,
			agent.WithTools(builtinTools),
			agent.WithToolsets(allToolsets),
			agent.WithSystemInstructionPrefix(loaded.Instruction),
		)
	}

	srv := &axServer{agentFactory: agentFactory}

	lis, err := net.Listen("tcp", listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ax-agent: listen %s: %v\n", listen, err)
		return runner.ExitConfigError
	}
	grpcServer := grpc.NewServer()
	axproto.RegisterAgentServiceServer(grpcServer, srv)
	fmt.Fprintf(os.Stderr, "ax-agent: listening on %s (provider=%s model=%s)\n", listen, cfg.Model.Provider, cfg.Model.Name)

	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()
	if err := grpcServer.Serve(lis); err != nil {
		fmt.Fprintf(os.Stderr, "ax-agent: serve: %v\n", err)
		return runner.ExitAgentError
	}
	return runner.ExitOK
}

// loadConfig resolves the config from cfgPath (when set) or by walking
// up from cwd looking for .agents/. Identical shape to cmd/core-agent
// and extras/scion-agent — kept private to this binary per the
// established "binaries don't share helpers" convention in this repo.
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
