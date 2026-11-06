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

package compose

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	adktool "google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/instruction"
	"github.com/go-steer/core-agent/v3/pkg/mcp"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	"github.com/go-steer/core-agent/v3/pkg/skills"
	"github.com/go-steer/core-agent/v3/pkg/tools"
	"github.com/go-steer/core-agent/v3/pkg/tools/vcs"
)

// RunConfig configures a single-call agent execution via compose.Run,
// compose.RunRecipe, or compose.RunWorkload (#1404).
//
// Aliased to agent.OneShotConfig so callers can move between the slim
// pkg/agent entry point and the batteries-included pkg/compose entry point
// without converting types.
type RunConfig = agent.OneShotConfig

// Result is the outcome of a single-call agent execution (#1404).
type Result = agent.OneShotResult

// Run executes a single agent turn from cfg and prompt (#1404).
//
// When cfg.Dir is set, Run wires the full recipe directory surface —
// .agents/mcp.json servers (closed on exit), .agents/skills/ bundles,
// AGENTS.md / AGENTS.d/*.md / @include instruction trees, and @include
// directives inside declarative subagent instructions — before delegating to
// agent.RunOneShot.
func Run(ctx context.Context, cfg RunConfig, prompt string) (*Result, error) {
	if strings.TrimSpace(cfg.Dir) != "" {
		projectRoot, agentsDir, err := resolveRecipeDirs(cfg.Dir)
		if err != nil {
			return nil, err
		}
		diskCfg, err := loadComposeDirConfig(projectRoot, agentsDir)
		if err != nil {
			return nil, err
		}
		effectiveCfg := mergeComposeConfig(diskCfg, cfg.Config)

		gate := cfg.Gate
		if gate == nil {
			gate, err = permissions.FromConfig(effectiveCfg, projectRoot, "", cfg.Prompter)
			if err != nil {
				return nil, err
			}
			cfg.Gate = gate
		} else if cfg.Prompter != nil {
			gate.SetPrompter(cfg.Prompter)
		}

		mcpDir, mainInheritsMCP := resolveComposeMainMCPDir(projectRoot, agentsDir, effectiveCfg.Subagents)
		servers, mcpToolsets, mcpErr := mcp.Build(ctx, mcpDir, "", nil, gate, nil, nil)
		defer mcp.CloseAll(servers)
		if mcpErr != nil {
			return nil, fmt.Errorf("compose: mcp: %w", mcpErr)
		}
		if !mainInheritsMCP {
			mcpToolsets = nil
		}

		contentRoots := resolveComposeContentRoots(agentsDir, effectiveCfg.ContentRoots)
		shellRegistered := resolveComposeShellRegistered(cfg, effectiveCfg)
		loadedSkills, err := skills.LoadAll(
			ctx,
			agentsDir,
			"",
			gate,
			skills.WithContentRoots(contentRoots),
			skills.WithShellTool(shellRegistered),
		)
		if err != nil {
			return nil, fmt.Errorf("compose: skills: %w", err)
		}

		if effectiveCfg.VCS.Enabled() && resolveComposeVCSRegistered(cfg, effectiveCfg) {
			vcsTools, vcsErr := BuildVCSTools(
				effectiveCfg,
				gate,
				projectRoot,
				nil,
				vcs.WithEventSink(VCSEventSink(func() *eventlog.Handle { return cfg.EventLog })),
			)
			if vcsErr != nil {
				return nil, fmt.Errorf("compose: vcs: %w", vcsErr)
			}
			cfg.Tools = append(slices.Clone(cfg.Tools), vcsTools...)
		}

		if len(effectiveCfg.Subagents) > 0 {
			parentMCP := buildComposeNamedMCP(servers, cfg.Toolsets, gate)
			subagents, subToolsets, rootedServers, subErr := prepareComposeSubagents(
				ctx, effectiveCfg.Subagents, projectRoot, agentsDir,
				gate, shellRegistered, parentMCP, loadedSkills,
			)
			defer mcp.CloseAll(rootedServers)
			if subErr != nil {
				return nil, subErr
			}
			if cfg.Config == nil {
				cfg.Config = &config.Config{Subagents: subagents}
			} else {
				cp := *cfg.Config
				cp.Subagents = subagents
				cfg.Config = &cp
			}
			mergedSubToolsets := make(map[string][]adktool.Toolset, len(subToolsets)+len(cfg.SubagentToolsets))
			maps.Copy(mergedSubToolsets, subToolsets)
			maps.Copy(mergedSubToolsets, cfg.SubagentToolsets)
			cfg.SubagentToolsets = mergedSubToolsets
		}

		if len(mcpToolsets) > 0 || !loadedSkills.Empty() {
			combined := make([]adktool.Toolset, 0, len(cfg.Toolsets)+len(mcpToolsets)+1)
			combined = append(combined, cfg.Toolsets...)
			combined = append(combined, mcpToolsets...)
			if !loadedSkills.Empty() {
				combined = append(combined, loadedSkills.Toolset)
			}
			cfg.Toolsets = combined
		}

		if cfg.Instruction == "" {
			loaded, err := instruction.Load(
				projectRoot,
				"",
				instruction.WithContentRoots(contentRoots),
			)
			if err != nil {
				return nil, err
			}
			if !loaded.Empty() {
				cfg.Instruction = loaded.Instruction
			} else if coordData, cerr := os.ReadFile(filepath.Join(projectRoot, "coordinator.md")); cerr == nil {
				if trimmed := strings.TrimSpace(string(coordData)); trimmed != "" {
					expanded, _, expErr := instruction.Expand(trimmed, projectRoot, projectRoot, instruction.WithContentRoots(contentRoots))
					if expErr != nil {
						return nil, expErr
					}
					cfg.Instruction = expanded
				}
			}
		}
	} else if cfg.Config != nil && len(cfg.Config.Subagents) > 0 {
		var rootedServers []*mcp.Server
		var err error
		cfg, rootedServers, err = prepareComposeDirlessSubagents(ctx, cfg)
		defer mcp.CloseAll(rootedServers)
		if err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(cfg.Dir) == "" && cfg.Config != nil && cfg.Config.VCS.Enabled() && resolveComposeVCSRegistered(cfg, cfg.Config) {
		cwd, _ := os.Getwd()
		gate := cfg.Gate
		if gate == nil {
			var err error
			gate, err = permissions.FromConfig(cfg.Config, cwd, "", cfg.Prompter)
			if err != nil {
				return nil, err
			}
			cfg.Gate = gate
		} else if cfg.Prompter != nil {
			gate.SetPrompter(cfg.Prompter)
		}
		vcsTools, err := BuildVCSTools(
			cfg.Config,
			gate,
			cwd,
			nil,
			vcs.WithEventSink(VCSEventSink(func() *eventlog.Handle { return cfg.EventLog })),
		)
		if err != nil {
			return nil, fmt.Errorf("compose: vcs: %w", err)
		}
		cfg.Tools = append(slices.Clone(cfg.Tools), vcsTools...)
	}
	return agent.RunOneShot(ctx, cfg, prompt)
}

// RunRecipe loads the recipe directory dir (expanding AGENTS.md, AGENTS.d/*.md,
// .agents/config.json, .agents/mcp.json, .agents/skills/, and
// .agents/subagents/*.md) and executes a single turn with prompt (#1404).
func RunRecipe(ctx context.Context, dir string, cfg RunConfig, prompt string) (*Result, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("compose: recipe dir must not be empty")
	}
	cfg.Dir = dir
	return Run(ctx, cfg, prompt)
}

// RunWorkload is an alias for RunRecipe providing API parity with mast's
// RunWorkload entry point (#1404).
func RunWorkload(ctx context.Context, dir string, cfg RunConfig, prompt string) (*Result, error) {
	return RunRecipe(ctx, dir, cfg, prompt)
}

func resolveRecipeDirs(dir string) (projectRoot, agentsDir string, err error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", "", fmt.Errorf("compose: resolve dir %q: %w", dir, err)
	}
	info, err := os.Stat(absDir)
	if err != nil {
		return "", "", fmt.Errorf("compose: stat dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("compose: %q is not a directory", dir)
	}
	dotAgents := filepath.Join(absDir, config.AgentsDirName)
	if fi, err := os.Stat(dotAgents); err == nil && fi.IsDir() {
		return absDir, dotAgents, nil
	}
	if filepath.Base(absDir) == config.AgentsDirName {
		return filepath.Dir(absDir), absDir, nil
	}
	return absDir, absDir, nil
}

func mergeComposeConfig(base, override *config.Config) *config.Config {
	if override == nil {
		return base
	}
	cp := *base
	if override.Permissions.Mode != "" {
		cp.Permissions.Mode = override.Permissions.Mode
	}
	if override.Permissions.Allow != nil {
		cp.Permissions.Allow = override.Permissions.Allow
	}
	if override.Permissions.Deny != nil {
		cp.Permissions.Deny = override.Permissions.Deny
	}
	if override.Permissions.UseBuiltinAllow != nil {
		cp.Permissions.UseBuiltinAllow = override.Permissions.UseBuiltinAllow
	}
	if override.Permissions.BuiltinAllowExtras != nil {
		cp.Permissions.BuiltinAllowExtras = override.Permissions.BuiltinAllowExtras
	}
	if override.Permissions.PlanModeSet() {
		cp.Permissions.NormalizePlanMode(override.Permissions.ResolvedPlanMode())
	}
	if override.Permissions.ApprovalTimeout != "" {
		cp.Permissions.ApprovalTimeout = override.Permissions.ApprovalTimeout
	}
	if override.Permissions.ApprovalNotify != "" {
		cp.Permissions.ApprovalNotify = override.Permissions.ApprovalNotify
	}
	if override.Permissions.ApprovalNotifyAfter != "" {
		cp.Permissions.ApprovalNotifyAfter = override.Permissions.ApprovalNotifyAfter
	}
	if override.Permissions.Auto != nil {
		cp.Permissions.Auto = override.Permissions.Auto
	}
	if override.PathScope.Allow != nil {
		cp.PathScope.Allow = override.PathScope.Allow
	}
	if override.PathScope.AllowPaths != nil {
		cp.PathScope.AllowPaths = override.PathScope.AllowPaths
	}
	if override.Safety.BashSearchGate != "" {
		cp.Safety.BashSearchGate = override.Safety.BashSearchGate
	}
	if override.Tools.Disable != nil {
		cp.Tools.Disable = override.Tools.Disable
	}
	if override.Tools.Policies != nil {
		cp.Tools.Policies = override.Tools.Policies
	}
	if override.Agent.MaxWallclockSeconds != 0 {
		cp.Agent.MaxWallclockSeconds = override.Agent.MaxWallclockSeconds
	}
	defaultMaxSteps := config.DefaultConfig().Agent.MaxSteps
	if override.Agent.MaxSteps != 0 && (cp.Agent.MaxSteps == 0 || cp.Agent.MaxSteps == defaultMaxSteps || override.Agent.MaxSteps != defaultMaxSteps) {
		cp.Agent.MaxSteps = override.Agent.MaxSteps
	}
	if override.Agent.FinalReport {
		cp.Agent.FinalReport = true
	}
	if override.ContentRoots != nil {
		cp.ContentRoots = override.ContentRoots
	}
	if override.Subagents != nil {
		cp.Subagents = override.Subagents
	}
	if override.Schedule != nil {
		cp.Schedule = override.Schedule
	}
	if override.Monitor != nil {
		cp.Monitor = override.Monitor
	}
	cp.VCS.MergeFrom(override.VCS)
	return &cp
}

func loadComposeDirConfig(projectRoot, agentsDir string) (*config.Config, error) {
	cfgFile := filepath.Join(agentsDir, config.ConfigFileName)
	if _, err := os.Stat(cfgFile); errors.Is(err, os.ErrNotExist) {
		for _, dir := range []string{projectRoot, agentsDir} {
			if dir == "" {
				continue
			}
			if _, werr := os.Stat(filepath.Join(dir, "workload.yaml")); werr == nil {
				return config.LoadWorkloadConfig(dir)
			}
		}
	}
	return config.Load(agentsDir)
}

func resolveComposeMainMCPDir(projectRoot, agentsDir string, subagents []config.SubagentSpec) (mcpDir string, mainInheritsMCP bool) {
	if _, err := os.Stat(filepath.Join(agentsDir, "mcp.json")); !errors.Is(err, fs.ErrNotExist) {
		if agentsDir == projectRoot {
			if _, cerr := os.Stat(filepath.Join(agentsDir, config.ConfigFileName)); errors.Is(cerr, fs.ErrNotExist) {
				if _, werr := os.Stat(filepath.Join(projectRoot, config.WorkloadFileName)); werr == nil {
					return projectRoot, false
				}
			}
		}
		return agentsDir, true
	}
	if _, perr := os.Stat(filepath.Join(projectRoot, "mcp.json")); perr == nil {
		if _, werr := os.Stat(filepath.Join(projectRoot, config.WorkloadFileName)); werr == nil {
			if hasUnrootedMCPSubagents(subagents) {
				return projectRoot, false
			}
			return agentsDir, false
		}
		return projectRoot, true
	}
	return agentsDir, true
}

func hasUnrootedMCPSubagents(specs []config.SubagentSpec) bool {
	for _, s := range specs {
		if s.EffectiveMode() == config.SubagentModeSingleTurn {
			continue
		}
		if s.Root == "" && (len(s.MCPTools) > 0 || len(s.MCP) > 0 || s.MCP == nil) {
			return true
		}
	}
	return false
}

type composeNamedToolset struct {
	name    string
	toolset adktool.Toolset
}

func resolveComposeShellRegistered(cfg RunConfig, effectiveCfg *config.Config) bool {
	if slices.Contains(effectiveCfg.Tools.Disable, "bash") {
		return false
	}
	if cfg.EnableBuiltinTools {
		return true
	}
	for _, sub := range effectiveCfg.Subagents {
		if slices.Contains(sub.Tools, "bash") {
			return true
		}
	}
	return false
}

func resolveComposeVCSRegistered(cfg RunConfig, effectiveCfg *config.Config) bool {
	if cfg.EnableBuiltinTools {
		return true
	}
	for _, sub := range effectiveCfg.Subagents {
		for _, t := range sub.Tools {
			if vcs.IsToolName(t) {
				return true
			}
		}
	}
	return false
}

func buildComposeNamedMCP(servers []*mcp.Server, extraToolsets []adktool.Toolset, gate *permissions.Gate) []composeNamedToolset {
	out := make([]composeNamedToolset, 0, len(servers)+len(extraToolsets))
	seen := make(map[string]bool, len(servers)+len(extraToolsets))
	for _, s := range servers {
		if s == nil {
			continue
		}
		out = append(out, composeNamedToolset{name: s.Name, toolset: s.Toolset()})
		seen[s.Name] = true
	}
	for _, ts := range extraToolsets {
		if ts == nil || ts.Name() == "skill_toolset" || seen[ts.Name()] {
			continue
		}
		out = append(out, composeNamedToolset{
			name:    ts.Name(),
			toolset: tools.GateToolset(ts, gate, ts.Name()),
		})
		seen[ts.Name()] = true
	}
	return out
}

func prepareComposeDirlessSubagents(ctx context.Context, cfg RunConfig) (RunConfig, []*mcp.Server, error) {
	projectRoot, _ := os.Getwd()
	gate := cfg.Gate
	shellRegistered := resolveComposeShellRegistered(cfg, cfg.Config)
	specs := slices.Clone(cfg.Config.Subagents)
	var rootedServers []*mcp.Server
	var subToolsets map[string][]adktool.Toolset

	for i := range specs {
		spec := &specs[i]
		if spec.Root != "" {
			if gate == nil {
				var err error
				gate, err = permissions.FromConfig(cfg.Config, projectRoot, "", cfg.Prompter)
				if err != nil {
					return cfg, rootedServers, err
				}
				cfg.Gate = gate
			}
			rootAbs, rootMCP, rootSkills, servers, err := loadComposeSubagentRoot(ctx, *spec, projectRoot, gate, shellRegistered)
			rootedServers = append(rootedServers, servers...)
			if err != nil {
				return cfg, rootedServers, fmt.Errorf("compose: subagent %q: %w", spec.Name, err)
			}
			sets, err := resolveComposeSubagentToolsets(ctx, *spec, rootMCP, rootSkills)
			if err != nil {
				return cfg, rootedServers, fmt.Errorf("compose: subagent %q: %w", spec.Name, err)
			}
			if subToolsets == nil {
				subToolsets = make(map[string][]adktool.Toolset)
			}
			subToolsets[spec.Name] = sets
			inst, err := resolveComposeRootedInstruction(*spec, rootAbs)
			if err != nil {
				return cfg, rootedServers, fmt.Errorf("compose: subagent %q instructions: %w", spec.Name, err)
			}
			spec.Instructions = inst
			continue
		}
		if spec.Instructions != "" {
			expanded, err := expandComposeInstruction(spec.Instructions, spec.BaseDir(), projectRoot)
			if err != nil {
				return cfg, rootedServers, fmt.Errorf("compose: subagent %q instructions: %w", spec.Name, err)
			}
			spec.Instructions = expanded
		}
	}

	cp := *cfg.Config
	cp.Subagents = specs
	cfg.Config = &cp
	if len(subToolsets) > 0 {
		merged := make(map[string][]adktool.Toolset, len(subToolsets)+len(cfg.SubagentToolsets))
		maps.Copy(merged, subToolsets)
		maps.Copy(merged, cfg.SubagentToolsets)
		cfg.SubagentToolsets = merged
	}
	return cfg, rootedServers, nil
}

func prepareComposeSubagents(
	ctx context.Context,
	specs []config.SubagentSpec,
	projectRoot, agentsDir string,
	gate *permissions.Gate,
	shellRegistered bool,
	parentMCP []composeNamedToolset,
	parentSkills skills.Skills,
) ([]config.SubagentSpec, map[string][]adktool.Toolset, []*mcp.Server, error) {
	out := slices.Clone(specs)
	toolsetsBySub := make(map[string][]adktool.Toolset, len(out))
	var rootedServers []*mcp.Server

	rootBase := agentsDir
	if rootBase == "" {
		rootBase = projectRoot
	}

	for i := range out {
		spec := &out[i]
		if spec.Root != "" {
			rootAbs, rootMCP, rootSkills, servers, err := loadComposeSubagentRoot(ctx, *spec, rootBase, gate, shellRegistered)
			rootedServers = append(rootedServers, servers...)
			if err != nil {
				return nil, nil, rootedServers, fmt.Errorf("compose: subagent %q: %w", spec.Name, err)
			}
			sets, err := resolveComposeSubagentToolsets(ctx, *spec, rootMCP, rootSkills)
			if err != nil {
				return nil, nil, rootedServers, fmt.Errorf("compose: subagent %q: %w", spec.Name, err)
			}
			toolsetsBySub[spec.Name] = sets

			inst, err := resolveComposeRootedInstruction(*spec, rootAbs)
			if err != nil {
				return nil, nil, rootedServers, fmt.Errorf("compose: subagent %q instructions: %w", spec.Name, err)
			}
			spec.Instructions = inst
			continue
		}

		sets, err := resolveComposeSubagentToolsets(ctx, *spec, parentMCP, parentSkills)
		if err != nil {
			return nil, nil, rootedServers, fmt.Errorf("compose: subagent %q: %w", spec.Name, err)
		}
		toolsetsBySub[spec.Name] = sets

		if spec.Instructions != "" {
			expanded, err := expandComposeInstruction(spec.Instructions, spec.BaseDir(), projectRoot)
			if err != nil {
				return nil, nil, rootedServers, fmt.Errorf("compose: subagent %q instructions: %w", spec.Name, err)
			}
			spec.Instructions = expanded
		}
	}
	return out, toolsetsBySub, rootedServers, nil
}

func loadComposeSubagentRoot(
	ctx context.Context,
	spec config.SubagentSpec,
	rootBase string,
	gate *permissions.Gate,
	shellRegistered bool,
) (string, []composeNamedToolset, skills.Skills, []*mcp.Server, error) {
	rootAbs := spec.Root
	if !filepath.IsAbs(rootAbs) && rootBase != "" {
		rootAbs = filepath.Join(rootBase, rootAbs)
	}
	rootAbs = filepath.Clean(rootAbs)
	info, err := os.Stat(rootAbs)
	if err != nil {
		return "", nil, skills.Skills{}, nil, fmt.Errorf("root: %w", err)
	}
	if !info.IsDir() {
		return "", nil, skills.Skills{}, nil, fmt.Errorf("root %q is not a directory", rootAbs)
	}

	servers, _, mcpErr := mcp.Build(ctx, rootAbs, "", nil, gate, nil, nil)
	if mcpErr != nil {
		return "", nil, skills.Skills{}, servers, fmt.Errorf("mcp: %w", mcpErr)
	}
	named := buildComposeNamedMCP(servers, nil, gate)

	rootSkills, err := skills.LoadAll(ctx, rootAbs, "", gate, skills.WithShellTool(shellRegistered))
	if err != nil {
		return "", nil, skills.Skills{}, servers, fmt.Errorf("skills: %w", err)
	}
	return rootAbs, named, rootSkills, servers, nil
}

func resolveComposeRootedInstruction(spec config.SubagentSpec, rootAbs string) (string, error) {
	if spec.Instructions != "" {
		expanded, _, err := instruction.Expand(spec.Instructions, rootAbs, rootAbs)
		return expanded, err
	}
	loaded, err := instruction.Load("", "", instruction.WithContentRoots([]string{rootAbs}))
	if err != nil {
		return "", err
	}
	return loaded.Instruction, nil
}

func expandComposeInstruction(raw, baseDir, projectRoot string) (string, error) {
	expanded, _, err := instruction.Expand(raw, projectRoot, projectRoot)
	if err != nil && baseDir != "" && baseDir != projectRoot {
		if alt, _, altErr := instruction.Expand(raw, baseDir, projectRoot); altErr == nil {
			return alt, nil
		}
	}
	return expanded, err
}

func resolveComposeSubagentToolsets(
	ctx context.Context,
	spec config.SubagentSpec,
	mcpSurface []composeNamedToolset,
	skillSurface skills.Skills,
) ([]adktool.Toolset, error) {
	if spec.EffectiveMode() == config.SubagentModeSingleTurn {
		return nil, nil
	}

	byName := make(map[string]composeNamedToolset, len(mcpSurface))
	for _, s := range mcpSurface {
		byName[s.name] = s
	}

	var mcpToolsByServer map[string][]string
	if len(spec.MCPTools) > 0 {
		mcpToolsByServer = make(map[string][]string, len(spec.MCPTools)*2)
		for _, mt := range spec.MCPTools {
			s, ok := lookupComposeMCP(byName, mcpSurface, mt.Server)
			if !ok {
				return nil, fmt.Errorf("mcp_tools: unknown server %q (not in mcp.json)", mt.Server)
			}
			mcpToolsByServer[mt.Server] = mt.Tools
			mcpToolsByServer[s.name] = mt.Tools
		}
	}

	var sets []adktool.Toolset
	take := func(s composeNamedToolset, requestedName string) {
		if s.toolset == nil {
			return
		}
		allowed, hasFilter := mcpToolsByServer[s.name]
		if !hasFilter && requestedName != "" {
			allowed, hasFilter = mcpToolsByServer[requestedName]
		}
		if hasFilter {
			if len(allowed) == 0 {
				return
			}
			filterServer := canonicalComposeMCPServerName(s.name)
			if requestedName != "" {
				filterServer = canonicalComposeMCPServerName(requestedName)
			}
			sets = append(sets, mcp.FilterToolset(s.toolset, filterServer, allowed))
			return
		}
		sets = append(sets, s.toolset)
	}

	switch {
	case spec.MCP == nil && len(spec.MCPTools) > 0:
		for _, mt := range spec.MCPTools {
			if s, ok := lookupComposeMCP(byName, mcpSurface, mt.Server); ok {
				take(s, mt.Server)
			}
		}
	case spec.MCP == nil:
		for _, s := range mcpSurface {
			take(s, s.name)
		}
	default:
		for _, name := range spec.MCP {
			s, ok := lookupComposeMCP(byName, mcpSurface, name)
			if !ok {
				return nil, fmt.Errorf("mcp: unknown server %q (not in mcp.json)", name)
			}
			take(s, name)
		}
	}

	switch spec.Skills {
	case nil:
		if !skillSurface.Empty() {
			sets = append(sets, skillSurface.Toolset)
		}
	default:
		scoped, err := skillSurface.Scoped(ctx, spec.Skills)
		if err != nil {
			return nil, err
		}
		if !scoped.Empty() {
			sets = append(sets, scoped.Toolset)
		}
	}
	return sets, nil
}

func lookupComposeMCP(byName map[string]composeNamedToolset, list []composeNamedToolset, serverName string) (composeNamedToolset, bool) {
	if s, ok := byName[serverName]; ok {
		return s, true
	}
	sanitized := sanitizeComposeMCPPrefix(serverName)
	if s, ok := byName[sanitized]; ok {
		return s, true
	}
	for _, s := range list {
		if base, ok := strings.CutSuffix(s.name, "_mcp_tool_set"); ok && (base == serverName || base == sanitized) {
			return s, true
		}
		if base, ok := strings.CutSuffix(s.name, "_toolset"); ok && (base == serverName || base == sanitized) {
			return s, true
		}
	}
	return composeNamedToolset{}, false
}

func canonicalComposeMCPServerName(name string) string {
	if base, ok := strings.CutSuffix(name, "_mcp_tool_set"); ok {
		return base
	}
	if base, ok := strings.CutSuffix(name, "_toolset"); ok {
		return base
	}
	return name
}

func sanitizeComposeMCPPrefix(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func resolveComposeContentRoots(agentsDir string, roots []string) []string {
	if len(roots) == 0 {
		return nil
	}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !filepath.IsAbs(r) && agentsDir != "" {
			r = filepath.Join(agentsDir, r)
		}
		out = append(out, filepath.Clean(r))
	}
	return out
}
