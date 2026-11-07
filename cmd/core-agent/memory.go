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

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/memory"
	_ "github.com/go-steer/core-agent/v3/pkg/memory/memorybank"
	"github.com/go-steer/core-agent/v3/pkg/memory/memtools"
	_ "github.com/go-steer/core-agent/v3/pkg/memory/sqlfts"
)

// memoryFlags carries the stateless --memory-* and --agent-engine CLI flags
// into run() via subagentOpts (#948).
type memoryFlags struct {
	ref         string
	agentEngine string
	namespace   string
	agent       string
	user        string
	shared      []string
	preload     string
	consolidate string
}

func firstNonEmpty(a, b string) string {
	if s := strings.TrimSpace(a); s != "" {
		return s
	}
	return strings.TrimSpace(b)
}

// applyMemoryFlagsAndEnv overlays --memory-* / --agent-engine CLI flags and
// CORE_AGENT_* environment variables onto cfg in precedence order:
// CLI flags > CORE_AGENT_* env vars > .agents/config.json > substrate defaults.
// Returns the resolved user identity override (--memory-user / CORE_AGENT_MEMORY_USER)
// when set.
func applyMemoryFlagsAndEnv(cfg *config.Config, flags memoryFlags, getenv func(string) string) (string, error) {
	if cfg == nil {
		return "", nil
	}
	if getenv == nil {
		getenv = func(string) string { return "" }
	}

	engine := firstNonEmpty(flags.agentEngine, getenv("CORE_AGENT_AGENT_ENGINE"))
	if engine != "" {
		if cfg.AgentEngine == nil {
			cfg.AgentEngine = &config.AgentEngineConfig{}
		}
		cfg.AgentEngine.Resource = engine
	}

	ref := firstNonEmpty(flags.ref, getenv("CORE_AGENT_MEMORY_REF"))
	ns := firstNonEmpty(flags.namespace, getenv("CORE_AGENT_MEMORY_NAMESPACE"))
	agentSpec := firstNonEmpty(flags.agent, getenv("CORE_AGENT_MEMORY_AGENT"))
	userOverride := firstNonEmpty(flags.user, getenv("CORE_AGENT_MEMORY_USER"))
	preload := firstNonEmpty(flags.preload, getenv("CORE_AGENT_MEMORY_PRELOAD"))
	consolidate := firstNonEmpty(flags.consolidate, getenv("CORE_AGENT_MEMORY_CONSOLIDATE"))

	var sharedSpecs []string
	if len(flags.shared) > 0 {
		for _, item := range flags.shared {
			for _, part := range strings.Split(item, ",") {
				if trimmed := strings.TrimSpace(part); trimmed != "" {
					sharedSpecs = append(sharedSpecs, trimmed)
				}
			}
		}
	} else if envShared := strings.TrimSpace(getenv("CORE_AGENT_MEMORY_SHARED")); envShared != "" {
		for _, part := range strings.Split(envShared, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				sharedSpecs = append(sharedSpecs, trimmed)
			}
		}
	}

	hasMemOverride := ref != "" || ns != "" || agentSpec != "" || userOverride != "" ||
		len(sharedSpecs) > 0 || preload != "" || consolidate != ""
	if cfg.Memory == nil && !hasMemOverride {
		return userOverride, nil
	}
	if cfg.Memory == nil {
		cfg.Memory = &config.MemoryConfig{}
	}

	if ref != "" {
		cfg.Memory.Ref = ref
	}
	if ns != "" {
		cfg.Memory.Namespace = ns
	}
	if agentSpec != "" {
		m, err := parseMemoryAgentSpec(agentSpec)
		if err != nil {
			return "", err
		}
		if cfg.Memory.Agent != nil {
			if m.ID == "" {
				m.ID = cfg.Memory.Agent.ID
			}
			m.WriteMode = cfg.Memory.Agent.WriteMode
			m.PartitionByUser = cfg.Memory.Agent.PartitionByUser
			m.TTL = cfg.Memory.Agent.TTL
		}
		cfg.Memory.Agent = m
	}
	if userOverride != "" {
		if cfg.Memory.Agent == nil {
			cfg.Memory.Agent = &config.AgentMemoryConfig{
				Mode:            config.MemoryModeRW,
				PartitionByUser: true,
			}
		} else {
			cfg.Memory.Agent.PartitionByUser = true
		}
	}
	for _, spec := range sharedSpecs {
		m, err := parseMemorySharedSpec(spec)
		if err != nil {
			return "", err
		}
		replaced := false
		for i := range cfg.Memory.Shared {
			if cfg.Memory.Shared[i].Pool == m.Pool {
				cfg.Memory.Shared[i].Mode = m.Mode
				replaced = true
				break
			}
		}
		if !replaced {
			cfg.Memory.Shared = append(cfg.Memory.Shared, m)
		}
	}
	if preload != "" {
		cfg.Memory.Preload = preload
	}
	if consolidate != "" {
		cfg.Memory.Consolidate = consolidate
	}
	return userOverride, nil
}

func parseMemoryAgentSpec(raw string) (*config.AgentMemoryConfig, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("--memory-agent requires a non-empty value")
	}
	switch s {
	case config.MemoryModeOff, config.MemoryModeRO, config.MemoryModeRW:
		return &config.AgentMemoryConfig{Mode: s}, nil
	}
	if idx := strings.LastIndex(s, ":"); idx >= 0 {
		id := strings.TrimSpace(s[:idx])
		mode := strings.TrimSpace(s[idx+1:])
		if id == "" {
			return nil, fmt.Errorf("--memory-agent %q: agent id before ':' must not be empty", raw)
		}
		switch mode {
		case config.MemoryModeRO, config.MemoryModeRW, config.MemoryModeOff:
			return &config.AgentMemoryConfig{ID: id, Mode: mode}, nil
		default:
			return nil, fmt.Errorf("--memory-agent %q: access must be ro, rw, or off", raw)
		}
	}
	return &config.AgentMemoryConfig{ID: s, Mode: config.MemoryModeRW}, nil
}

func parseMemorySharedSpec(raw string) (config.SharedMemoryConfig, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return config.SharedMemoryConfig{}, fmt.Errorf("--memory-shared requires <pool>[:ro|rw]")
	}
	pool, mode, hasColon := strings.Cut(s, ":")
	pool = strings.TrimSpace(pool)
	mode = strings.TrimSpace(mode)
	if pool == "" {
		return config.SharedMemoryConfig{}, fmt.Errorf("--memory-shared %q: pool name must not be empty", raw)
	}
	if !hasColon || mode == "" {
		mode = config.MemoryModeRO
	}
	switch mode {
	case config.MemoryModeRO, config.MemoryModeRW:
		return config.SharedMemoryConfig{Pool: pool, Mode: mode}, nil
	default:
		return config.SharedMemoryConfig{}, fmt.Errorf("--memory-shared %q: access must be ro or rw", raw)
	}
}

func openDaemonMemory(
	ctx context.Context,
	cfg *config.Config,
	flags memoryFlags,
	projectRoot string,
	noBuiltinTools bool,
	disabledTools []string,
	send func(string),
) (*memory.Router, agent.MemorySettings, func(), error) {
	noop := func() {}
	userOverride, err := applyMemoryFlagsAndEnv(cfg, flags, os.Getenv)
	if err != nil {
		return nil, agent.MemorySettings{}, noop, err
	}
	if cfg == nil || cfg.Memory == nil {
		return nil, agent.MemorySettings{}, noop, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, agent.MemorySettings{}, noop, err
	}
	defaultAgentID := strings.TrimSpace(cfg.Agent.DisplayName)
	if defaultAgentID == "" && projectRoot != "" {
		defaultAgentID = "default"
	}
	router, settings, closeFn, err := agent.OpenConfigMemoryRouter(ctx, cfg, defaultAgentID, os.Getenv)
	if err != nil {
		return nil, agent.MemorySettings{}, noop, err
	}
	if router == nil {
		return nil, agent.MemorySettings{}, closeFn, nil
	}
	if userOverride != "" {
		router = router.With(memory.WithUserResolver(func(context.Context) string {
			return userOverride
		}))
	}
	if noBuiltinTools {
		settings.DisabledTools = []string{memtools.RecallToolName, memtools.PersistToolName, memtools.ForgetToolName}
	} else if len(disabledTools) > 0 {
		settings.DisabledTools = append([]string(nil), disabledTools...)
	}
	if send != nil {
		ref, _ := cfg.ResolveMemoryRef(os.Getenv)
		send(fmt.Sprintf("memory: ref=%s namespace=%s preload=%s consolidate=%s",
			ref, router.Namespace(), settings.Preload, settings.Consolidate))
	}
	return router, settings, closeFn, nil
}
