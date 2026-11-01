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
	"os"
	"path/filepath"
	"slices"
	"strings"

	adktool "google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/instruction"
	"github.com/go-steer/core-agent/v3/pkg/mcp"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	"github.com/go-steer/core-agent/v3/pkg/skills"
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
		diskCfg, err := config.Load(agentsDir)
		if err != nil {
			return nil, err
		}
		effectiveCfg := mergeComposeConfig(diskCfg, cfg.Config)

		if len(effectiveCfg.Subagents) > 0 {
			subagents, err := expandComposeSubagents(effectiveCfg.Subagents, projectRoot)
			if err != nil {
				return nil, err
			}
			if cfg.Config == nil {
				cfg.Config = &config.Config{Subagents: subagents}
			} else {
				cp := *cfg.Config
				cp.Subagents = subagents
				cfg.Config = &cp
			}
		}

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

		servers, mcpToolsets, mcpErr := mcp.Build(ctx, agentsDir, "", nil, gate, nil, nil)
		defer mcp.CloseAll(servers)
		if mcpErr != nil {
			return nil, fmt.Errorf("compose: mcp: %w", mcpErr)
		}

		contentRoots := resolveComposeContentRoots(agentsDir, effectiveCfg.ContentRoots)
		shellRegistered := cfg.EnableBuiltinTools && !slices.Contains(effectiveCfg.Tools.Disable, "bash")
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
			}
		}
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
	if override.ContentRoots != nil {
		cp.ContentRoots = override.ContentRoots
	}
	if override.Subagents != nil {
		cp.Subagents = override.Subagents
	}
	return &cp
}

func expandComposeSubagents(specs []config.SubagentSpec, projectRoot string) ([]config.SubagentSpec, error) {
	out := slices.Clone(specs)
	for i := range out {
		if out[i].Instructions == "" {
			continue
		}
		expanded, _, err := instruction.Expand(out[i].Instructions, projectRoot, projectRoot)
		if err != nil && out[i].BaseDir() != "" && out[i].BaseDir() != projectRoot {
			if alt, _, altErr := instruction.Expand(out[i].Instructions, out[i].BaseDir(), projectRoot); altErr == nil {
				expanded = alt
				err = nil
			}
		}
		if err != nil {
			return nil, fmt.Errorf("compose: subagent %q instructions: %w", out[i].Name, err)
		}
		out[i].Instructions = expanded
	}
	return out, nil
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
