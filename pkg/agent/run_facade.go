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

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/models"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	"github.com/go-steer/core-agent/v3/pkg/tools"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

// OneShotConfig configures a single-call agent execution via RunOneShot,
// RunRecipe, or RunWorkload (#1404).
//
// It lives in pkg/agent (rather than only pkg/compose) so slim embedders can
// invoke a recipe or programmatic config in one call without pulling MCP,
// skills, OTel exporters, or the TUI into their binary dependency graph
// (enforced by dev/tools/verify-slim-deps).
type OneShotConfig struct {
	// Dir, when non-empty, loads .agents/config.json and .agents/subagents/*.md
	// from Dir (or Dir/.agents when present) before applying programmatic
	// overrides below.
	Dir string

	// Config supplies or overrides the loaded configuration. When Dir is empty
	// and Config is nil, config.DefaultConfig() is used.
	Config *config.Config

	// Model is a pre-built model.LLM. When nil, the model is resolved via
	// models.Resolve from the effective Config.Model (after applying Provider
	// and ModelName overrides).
	Model     adkmodel.LLM
	Provider  string
	ModelName string

	// Name and Description configure the root agent identity.
	Name        string
	Description string

	// Instruction is the user/project system instruction (e.g. AGENTS.md body).
	// When empty and Dir is set, RunOneShot reads AGENTS.md from Dir (and
	// Dir/.agents/AGENTS.md if present). Config.Agent.AppendSystemPrompt is
	// appended as an extra instruction layer when non-empty.
	Instruction string

	// EnableBuiltinTools registers the standard built-in file, shell, search,
	// and todo tools from pkg/tools (filtered by Config.Tools.Disable).
	EnableBuiltinTools bool

	// Tools and Toolsets are caller-supplied tools and toolsets. Every tool and
	// toolset is automatically wrapped through the permission Gate via
	// tools.GateTools / tools.GateToolset so nothing bypasses governance.
	Tools    []adktool.Tool
	Toolsets []adktool.Toolset

	// Gate is an optional pre-constructed permission gate. When nil, a gate is
	// built from the effective Config via permissions.FromConfig and wired to
	// Prompter.
	Gate     *permissions.Gate
	Prompter permissions.Prompter

	// SessionService, EventLog, and SessionID configure optional durability.
	SessionService session.Service
	EventLog       *eventlog.Handle
	SessionID      string

	// AgentOptions are additional Option values passed to New for the root agent.
	AgentOptions []Option
}

// OneShotResult is the outcome of a single-call agent execution (#1404).
type OneShotResult struct {
	// Text is the final assistant text produced by the turn.
	Text string `json:"text"`

	// Output is the parsed structured output when the turn produced a JSON
	// object response (via WithOutputSchema or a JSON object final message).
	Output map[string]any `json:"output,omitempty"`

	// SessionID is the session identifier the turn ran under.
	SessionID string `json:"session_id"`

	// Usage holds the cumulative token and cost totals for the turn, including
	// any synchronous subagent delegations.
	Usage usage.Totals `json:"usage"`
}

// RunOneShot executes a single agent turn from cfg and prompt, wiring config
// discovery, model resolution, permission gating, tool wrapping, declarative
// subagents, and usage tracking in one call (#1404).
func RunOneShot(ctx context.Context, cfg OneShotConfig, prompt string) (*OneShotResult, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("agent: prompt must not be empty")
	}

	projectRoot, agentsDir, err := resolveFacadeDir(cfg.Dir)
	if err != nil {
		return nil, err
	}

	loadedCfg, err := resolveFacadeConfig(cfg, agentsDir)
	if err != nil {
		return nil, err
	}

	llm, provider, backend, err := resolveFacadeModel(ctx, cfg, loadedCfg)
	if err != nil {
		return nil, err
	}

	gate, err := resolveFacadeGate(cfg, loadedCfg, projectRoot)
	if err != nil {
		return nil, err
	}

	rootTools, rootToolsets, builtinTools, err := resolveFacadeTools(cfg, loadedCfg, gate, projectRoot, agentsDir)
	if err != nil {
		return nil, err
	}

	tracker := usage.NewTracker()
	subagents, err := buildFacadeSubagents(
		ctx, cfg, loadedCfg, llm, provider, backend, gate, tracker,
		rootTools, builtinTools, rootToolsets, projectRoot, agentsDir,
	)
	if err != nil {
		return nil, err
	}

	userInst := cfg.Instruction
	if userInst == "" && cfg.Dir != "" {
		userInst, err = readDirInstruction(projectRoot, agentsDir)
		if err != nil {
			return nil, err
		}
	}

	opts := buildFacadeAgentOptions(
		cfg, loadedCfg, backend, gate, tracker,
		rootTools, rootToolsets, subagents, userInst,
	)
	a, err := New(llm, opts...)
	if err != nil {
		return nil, err
	}

	return collectFacadeTurn(ctx, a, prompt, tracker, llm.Name(), loadedCfg)
}

// RunRecipe loads the recipe directory dir (containing .agents/config.json,
// .agents/subagents/*.md, and/or AGENTS.md) and executes a single turn with
// prompt (#1404). Any non-zero fields on cfg act as programmatic overrides.
func RunRecipe(ctx context.Context, dir string, cfg OneShotConfig, prompt string) (*OneShotResult, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("agent: recipe dir must not be empty")
	}
	cfg.Dir = dir
	return RunOneShot(ctx, cfg, prompt)
}

// RunWorkload is an alias for RunRecipe providing API parity with mast's
// RunWorkload entry point (#1404).
func RunWorkload(ctx context.Context, dir string, cfg OneShotConfig, prompt string) (*OneShotResult, error) {
	return RunRecipe(ctx, dir, cfg, prompt)
}

func resolveFacadeDir(dir string) (projectRoot, agentsDir string, err error) {
	if strings.TrimSpace(dir) == "" {
		cwd, _ := os.Getwd()
		return cwd, "", nil
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", "", fmt.Errorf("agent: resolve dir %q: %w", dir, err)
	}
	info, err := os.Stat(absDir)
	if err != nil {
		return "", "", fmt.Errorf("agent: stat dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("agent: %q is not a directory", dir)
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

func resolveFacadeConfig(cfg OneShotConfig, agentsDir string) (*config.Config, error) {
	var base *config.Config
	if agentsDir != "" {
		loaded, err := config.Load(agentsDir)
		if err != nil {
			return nil, err
		}
		base = loaded
	} else {
		base = config.DefaultConfig()
	}

	out := base
	if cfg.Config != nil {
		out = mergeFacadeConfig(base, cfg.Config)
	}
	if cfg.Provider != "" {
		out.Model.Provider = cfg.Provider
	}
	if cfg.ModelName != "" {
		out.Model.Name = cfg.ModelName
	}
	explicitModelName := cfg.ModelName != "" || (cfg.Config != nil && cfg.Config.Model.Name != "") || agentsDir != ""
	if !explicitModelName && cfg.Model != nil && cfg.Model.Name() != "" {
		out.Model.Name = cfg.Model.Name()
	}
	if out.Model.Name == "" {
		out.Model.Name = config.DefaultConfig().Model.Name
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

func mergeFacadeConfig(base, override *config.Config) *config.Config {
	cp := *base
	if override.Version != 0 {
		cp.Version = override.Version
	}
	mergeFacadeModelConfig(&cp.Model, &override.Model)
	mergeFacadePermissionsConfig(&cp.Permissions, &override.Permissions)
	mergeFacadeAgentConfig(&cp.Agent, &override.Agent)
	mergeFacadeToolsConfig(&cp, override)
	mergeFacadeScopesAndRuntime(&cp, override)
	return &cp
}

func mergeFacadeModelConfig(dst, src *config.ModelConfig) {
	if src == nil {
		return
	}
	if src.Provider != "" {
		dst.Provider = src.Provider
	}
	if src.Name != "" {
		dst.Name = src.Name
	}
	if src.APIKey != "" {
		dst.APIKey = src.APIKey
	}
	if src.Vertex != nil {
		dst.Vertex = mergeFacadeVertexConfig(dst.Vertex, src.Vertex)
	}
	if src.Anthropic != nil {
		dst.Anthropic = mergeFacadeAnthropicConfig(dst.Anthropic, src.Anthropic)
	}
	if len(src.Pricing) > 0 {
		merged := make(config.PricingMap, len(dst.Pricing)+len(src.Pricing))
		maps.Copy(merged, dst.Pricing)
		maps.Copy(merged, src.Pricing)
		dst.Pricing = merged
	}
	if src.BuiltinTools != nil {
		dst.BuiltinTools = mergeFacadeBuiltinToolsConfig(dst.BuiltinTools, src.BuiltinTools)
	}
}

func mergeFacadeVertexConfig(base, over *config.VertexConfig) *config.VertexConfig {
	if over == nil {
		return base
	}
	if base == nil {
		cp := *over
		return &cp
	}
	cp := *base
	if over.Project != "" {
		cp.Project = over.Project
	}
	if over.Location != "" {
		cp.Location = over.Location
	}
	if over.ContextCache != nil {
		cp.ContextCache = over.ContextCache
	}
	return &cp
}

func mergeFacadeAnthropicConfig(base, over *config.AnthropicConfig) *config.AnthropicConfig {
	if over == nil {
		return base
	}
	if base == nil {
		cp := *over
		return &cp
	}
	cp := *base
	if over.APIKey != "" {
		cp.APIKey = over.APIKey
	}
	if over.Vertex != nil {
		cp.Vertex = mergeFacadeVertexConfig(cp.Vertex, over.Vertex)
	}
	if over.PromptCache != nil {
		cp.PromptCache = over.PromptCache
	}
	return &cp
}

func mergeFacadeBuiltinToolsConfig(base, over *config.BuiltinToolsConfig) *config.BuiltinToolsConfig {
	if over == nil {
		return base
	}
	if base == nil {
		cp := *over
		return &cp
	}
	cp := *base
	if over.WebSearch != nil {
		cp.WebSearch = over.WebSearch
	}
	if over.URLContext != nil {
		cp.URLContext = over.URLContext
	}
	if over.CodeExecution != nil {
		cp.CodeExecution = over.CodeExecution
	}
	return &cp
}

func mergeFacadePermissionsConfig(dst, src *config.PermissionsConfig) {
	if src.Mode != "" {
		dst.Mode = src.Mode
	}
	if src.Allow != nil {
		dst.Allow = src.Allow
	}
	if src.Deny != nil {
		dst.Deny = src.Deny
	}
	if src.UseBuiltinAllow != nil {
		dst.UseBuiltinAllow = src.UseBuiltinAllow
	}
	if src.BuiltinAllowExtras != nil {
		dst.BuiltinAllowExtras = src.BuiltinAllowExtras
	}
	if src.PlanModeSet() {
		dst.NormalizePlanMode(src.ResolvedPlanMode())
	}
	if src.ApprovalTimeout != "" {
		dst.ApprovalTimeout = src.ApprovalTimeout
	}
	if src.ApprovalNotify != "" {
		dst.ApprovalNotify = src.ApprovalNotify
	}
	if src.ApprovalNotifyAfter != "" {
		dst.ApprovalNotifyAfter = src.ApprovalNotifyAfter
	}
	if src.Auto != nil {
		dst.Auto = src.Auto
	}
}

func mergeFacadeAgentConfig(dst, src *config.AgentConfig) {
	if src.MaxSteps != 0 {
		dst.MaxSteps = src.MaxSteps
	}
	if src.AppendSystemPrompt != "" {
		dst.AppendSystemPrompt = src.AppendSystemPrompt
	}
	if src.SystemPromptFile != "" {
		dst.SystemPromptFile = src.SystemPromptFile
	}
	if src.MaxTurnCostUSD != nil {
		dst.MaxTurnCostUSD = src.MaxTurnCostUSD
	}
	if src.MaxSessionCostUSD != nil {
		dst.MaxSessionCostUSD = src.MaxSessionCostUSD
	}
	if src.AutoContinue != nil {
		dst.AutoContinue = src.AutoContinue
	}
	if src.SessionTitle != nil {
		dst.SessionTitle = src.SessionTitle
	}
	if src.DisplayName != "" {
		dst.DisplayName = src.DisplayName
	}
	if src.Description != "" {
		dst.Description = src.Description
	}
}

func mergeFacadeToolsConfig(dst, src *config.Config) {
	if src.ToolOutput.MaxBytes != 0 {
		dst.ToolOutput.MaxBytes = src.ToolOutput.MaxBytes
	}
	if src.ToolOutput.MaxLines != 0 {
		dst.ToolOutput.MaxLines = src.ToolOutput.MaxLines
	}
	if len(src.ToolOutput.PerTool) > 0 {
		merged := make(map[string]config.ToolOutputPerToolCaps, len(dst.ToolOutput.PerTool)+len(src.ToolOutput.PerTool))
		maps.Copy(merged, dst.ToolOutput.PerTool)
		maps.Copy(merged, src.ToolOutput.PerTool)
		dst.ToolOutput.PerTool = merged
	}
	if src.Tools.Disable != nil {
		dst.Tools.Disable = src.Tools.Disable
	}
	if src.Tools.WaitAndVerify.PollAllow != nil {
		dst.Tools.WaitAndVerify.PollAllow = src.Tools.WaitAndVerify.PollAllow
	}
	if src.Tools.WaitAndVerify.MaxTimeoutSeconds != 0 {
		dst.Tools.WaitAndVerify.MaxTimeoutSeconds = src.Tools.WaitAndVerify.MaxTimeoutSeconds
	}
	if src.Tools.WaitAndVerify.MaxAttempts != 0 {
		dst.Tools.WaitAndVerify.MaxAttempts = src.Tools.WaitAndVerify.MaxAttempts
	}
	if src.Tools.CallPeer != (config.CallPeerConfig{}) {
		dst.Tools.CallPeer = src.Tools.CallPeer
	}
	if src.Tools.SpawnAgent.SyncWaitTimeout != "" {
		dst.Tools.SpawnAgent.SyncWaitTimeout = src.Tools.SpawnAgent.SyncWaitTimeout
	}
	if src.Subagents != nil {
		dst.Subagents = src.Subagents
	}
}

func mergeFacadeScopesAndRuntime(dst, src *config.Config) {
	if src.PathScope.Allow != nil {
		dst.PathScope.Allow = src.PathScope.Allow
	}
	if src.PathScope.AllowPaths != nil {
		dst.PathScope.AllowPaths = src.PathScope.AllowPaths
	}
	if src.URLScope.Allow != nil {
		dst.URLScope.Allow = src.URLScope.Allow
	}
	if src.URLScope.Deny != nil {
		dst.URLScope.Deny = src.URLScope.Deny
	}
	if src.URLScope.MaxBodyBytes != 0 {
		dst.URLScope.MaxBodyBytes = src.URLScope.MaxBodyBytes
	}
	if src.URLScope.TimeoutSeconds != 0 {
		dst.URLScope.TimeoutSeconds = src.URLScope.TimeoutSeconds
	}
	if src.URLScope.Headers != nil {
		dst.URLScope.Headers = src.URLScope.Headers
	}
	if src.URLScope.AllowMetadataEndpoints {
		dst.URLScope.AllowMetadataEndpoints = true
	}
	if src.URLScope.Proxy != "" {
		dst.URLScope.Proxy = src.URLScope.Proxy
	}
	if src.Safety.SmallTierParent != "" {
		dst.Safety.SmallTierParent = src.Safety.SmallTierParent
	}
	if src.Safety.Watchdog != "" {
		dst.Safety.Watchdog = src.Safety.Watchdog
	}
	if src.Safety.BashSearchGate != "" {
		dst.Safety.BashSearchGate = src.Safety.BashSearchGate
	}
	if src.Safety.ParallelSubagentWrites != "" {
		dst.Safety.ParallelSubagentWrites = src.Safety.ParallelSubagentWrites
	}
	if src.Providers != nil {
		dst.Providers = src.Providers
	}
	if src.ContentRoots != nil {
		dst.ContentRoots = src.ContentRoots
	}
	if src.Mock.Script != "" {
		dst.Mock.Script = src.Mock.Script
	}
	if src.Mock.Strict {
		dst.Mock.Strict = true
	}
	if src.Mock.Record != "" {
		dst.Mock.Record = src.Mock.Record
	}
	if src.Alerts.Targets != nil {
		dst.Alerts.Targets = src.Alerts.Targets
	}
	if src.Session.TaskClass != "" {
		dst.Session.TaskClass = src.Session.TaskClass
	}
	if src.Checkpoint.Mode != "" {
		dst.Checkpoint.Mode = src.Checkpoint.Mode
	}
	if src.Compaction.Threshold != nil {
		dst.Compaction.Threshold = src.Compaction.Threshold
	}
	if src.Compaction.ThresholdByTier != nil {
		dst.Compaction.ThresholdByTier = src.Compaction.ThresholdByTier
	}
}

func resolveFacadeModel(ctx context.Context, cfg OneShotConfig, loadedCfg *config.Config) (adkmodel.LLM, models.Provider, string, error) {
	if cfg.Model != nil {
		return cfg.Model, nil, loadedCfg.Model.Provider, nil
	}
	provider, err := models.Resolve(loadedCfg)
	if err != nil {
		return nil, nil, "", err
	}
	llm, err := provider.Model(ctx, loadedCfg.Model.Name)
	if err != nil {
		return nil, nil, "", err
	}
	return llm, provider, models.BackendOf(provider), nil
}

func resolveFacadeGate(cfg OneShotConfig, loadedCfg *config.Config, projectRoot string) (*permissions.Gate, error) {
	if cfg.Gate != nil {
		if cfg.Prompter != nil {
			cfg.Gate.SetPrompter(cfg.Prompter)
		}
		return cfg.Gate, nil
	}
	return permissions.FromConfig(loadedCfg, projectRoot, "", cfg.Prompter)
}

func resolveFacadeTools(
	cfg OneShotConfig,
	loadedCfg *config.Config,
	gate *permissions.Gate,
	projectRoot, agentsDir string,
) (rootTools []adktool.Tool, rootToolsets []adktool.Toolset, builtinTools []adktool.Tool, err error) {
	needBuiltins := cfg.EnableBuiltinTools
	if !needBuiltins {
		for _, sub := range loadedCfg.Subagents {
			if len(sub.Tools) > 0 {
				needBuiltins = true
				break
			}
		}
	}
	if needBuiltins {
		b := tools.Default()
		for _, name := range loadedCfg.Tools.Disable {
			if err := b.Disable(name); err != nil {
				return nil, nil, nil, err
			}
		}
		stateDir := agentsDir
		if stateDir == "" && projectRoot != "" {
			stateDir = filepath.Join(projectRoot, config.AgentsDirName)
		}
		reg, err := tools.Build(loadedCfg, gate, stateDir, b)
		if err != nil {
			return nil, nil, nil, err
		}
		builtinTools = reg.Tools
	}

	if cfg.EnableBuiltinTools {
		rootTools = append(rootTools, builtinTools...)
	}
	if len(cfg.Tools) > 0 {
		rootTools = append(rootTools, tools.GateTools(cfg.Tools, gate, "")...)
	}
	for _, ts := range cfg.Toolsets {
		if ts == nil {
			continue
		}
		rootToolsets = append(rootToolsets, tools.GateToolset(ts, gate, ts.Name()))
	}
	return rootTools, rootToolsets, builtinTools, nil
}

func buildFacadeSubagents(
	ctx context.Context,
	cfg OneShotConfig,
	loadedCfg *config.Config,
	rootLLM adkmodel.LLM,
	provider models.Provider,
	backend string,
	gate *permissions.Gate,
	tracker *usage.Tracker,
	rootTools, builtinTools []adktool.Tool,
	rootToolsets []adktool.Toolset,
	projectRoot, agentsDir string,
) ([]*Agent, error) {
	if len(loadedCfg.Subagents) == 0 {
		return nil, nil
	}

	candidateTools := mergeUniqueTools(builtinTools, rootTools)
	if len(cfg.Tools) > 0 {
		candidateTools = mergeUniqueTools(candidateTools, tools.GateTools(cfg.Tools, gate, ""))
	}
	out := make([]*Agent, 0, len(loadedCfg.Subagents))
	for _, spec := range loadedCfg.Subagents {
		if spec.Remote != nil {
			return nil, fmt.Errorf("agent: subagent %q: remote subagents are not supported in RunOneShot", spec.Name)
		}
		subLLM, subBackend, err := resolveFacadeSubagentModel(ctx, spec, loadedCfg, rootLLM, provider, backend)
		if err != nil {
			return nil, err
		}
		sub, err := buildSingleFacadeSubagent(
			ctx, spec, loadedCfg, subLLM, subBackend, gate, tracker,
			rootTools, candidateTools, rootToolsets, projectRoot, agentsDir,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, nil
}

func resolveFacadeSubagentModel(
	ctx context.Context,
	spec config.SubagentSpec,
	loadedCfg *config.Config,
	rootLLM adkmodel.LLM,
	provider models.Provider,
	backend string,
) (adkmodel.LLM, string, error) {
	if spec.Model == nil && spec.Tier == "" {
		return rootLLM, backend, nil
	}
	mc := spec.Model
	if mc == nil && spec.Tier != "" {
		resolved, err := loadedCfg.ResolveTierModel(spec.Tier)
		if err != nil {
			return nil, "", fmt.Errorf("agent: subagent %q: %w", spec.Name, err)
		}
		if provider != nil && (resolved.Provider == "" || resolved.Provider == loadedCfg.Model.Provider) {
			llm, err := provider.Model(ctx, resolved.Name)
			if err != nil {
				return nil, "", fmt.Errorf("agent: subagent %q model: %w", spec.Name, err)
			}
			return llm, backend, nil
		}
		if provider == nil && loadedCfg.Model.Provider == "" {
			return rootLLM, backend, nil
		}
		mc = resolved
	}
	if mc == nil {
		return rootLLM, backend, nil
	}
	subCfg := *loadedCfg
	mergeFacadeModelConfig(&subCfg.Model, mc)
	subProv, err := models.Resolve(&subCfg)
	if err != nil {
		return nil, "", fmt.Errorf("agent: subagent %q model: %w", spec.Name, err)
	}
	llm, err := subProv.Model(ctx, subCfg.Model.Name)
	if err != nil {
		return nil, "", fmt.Errorf("agent: subagent %q model: %w", spec.Name, err)
	}
	return llm, models.BackendOf(subProv), nil
}

func buildSingleFacadeSubagent(
	ctx context.Context,
	spec config.SubagentSpec,
	loadedCfg *config.Config,
	subLLM adkmodel.LLM,
	subBackend string,
	gate *permissions.Gate,
	tracker *usage.Tracker,
	rootTools, candidateTools []adktool.Tool,
	rootToolsets []adktool.Toolset,
	projectRoot, agentsDir string,
) (*Agent, error) {
	mode := SubagentMode(spec.EffectiveMode())
	maxTurnUSD := derefFloat64(loadedCfg.Agent.MaxTurnCostUSD)
	var budgets SubagentBudgets
	if spec.Budgets != nil {
		if spec.Budgets.MaxTurns > 0 {
			budgets.MaxTurns = spec.Budgets.MaxTurns
		}
		if spec.Budgets.MaxCostUSD > 0 {
			maxTurnUSD = spec.Budgets.MaxCostUSD
			budgets.MaxCostUSD = spec.Budgets.MaxCostUSD
		}
		if spec.Budgets.MaxWallclockSeconds > 0 {
			budgets.MaxWallclock = time.Duration(spec.Budgets.MaxWallclockSeconds) * time.Second
		}
	}

	returnContract := SubagentReturnContract("")
	if mode == SubagentModeTask {
		returnContract = SubagentReturnContract(FinishTaskToolName)
	}

	subOpts := []Option{
		WithName(spec.Name),
		WithDescription(spec.Description),
		WithSubagentMode(mode),
		WithSubagentMaxDepth(spec.MaxDepth),
		WithSubagentBudgets(budgets),
		WithMode(ModeAutonomous),
		WithUserInstruction(spec.Instructions),
		WithExtraInstruction(loadedCfg.Agent.AppendSystemPrompt),
		WithExtraInstruction(returnContract),
		WithGate(gate),
		WithUsageTracker(tracker),
		WithBillingBackend(subBackend),
		WithCostCeiling(CostCeiling{
			MaxTurnUSD:    maxTurnUSD,
			MaxSessionUSD: derefFloat64(loadedCfg.Agent.MaxSessionCostUSD),
		}),
	}
	if spec.OutputSchema != "" {
		schema, err := loadFacadeOutputSchema(
			spec.OutputSchema,
			spec.BaseDir(),
			filepath.Join(agentsDir, config.SubagentsDirName),
			agentsDir,
			projectRoot,
		)
		if err != nil {
			return nil, fmt.Errorf("agent: subagent %q: %w", spec.Name, err)
		}
		subOpts = append(subOpts, WithOutputSchema(schema))
	}
	if mode != SubagentModeSingleTurn {
		selectedTools, err := selectFacadeSubagentTools(spec, rootTools, candidateTools)
		if err != nil {
			return nil, err
		}
		selectedToolsets, err := selectFacadeSubagentToolsets(spec, rootToolsets)
		if err != nil {
			return nil, err
		}
		if err := checkFacadeSubagentCapability(ctx, spec, selectedTools, selectedToolsets); err != nil {
			return nil, err
		}
		if len(selectedTools) > 0 {
			subOpts = append(subOpts, WithTools(selectedTools))
		}
		if len(selectedToolsets) > 0 {
			subOpts = append(subOpts, WithToolsets(selectedToolsets))
		}
	}
	return New(subLLM, subOpts...)
}

func loadFacadeOutputSchema(ref string, baseDirs ...string) (*genai.Schema, error) {
	if strings.HasPrefix(strings.TrimSpace(ref), "{") {
		return config.LoadOutputSchema("", ref)
	}
	var lastErr error
	for _, dir := range baseDirs {
		if dir == "" {
			continue
		}
		s, err := config.LoadOutputSchema(dir, ref)
		if err == nil {
			return s, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return ParseOutputSchema(ref, baseDirs...)
}

func derefFloat64(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

var facadeSpawnToolCarveOut = map[string]bool{
	"spawn_agent":        true,
	"spawn_remote_agent": true,
}

func selectFacadeSubagentTools(spec config.SubagentSpec, rootTools, candidateTools []adktool.Tool) ([]adktool.Tool, error) {
	if spec.Tools == nil {
		out := make([]adktool.Tool, 0, len(rootTools))
		for _, t := range rootTools {
			if t == nil || facadeSpawnToolCarveOut[t.Name()] {
				continue
			}
			out = append(out, t)
		}
		return out, nil
	}
	if len(spec.Tools) == 0 {
		return nil, nil
	}
	byName := make(map[string]adktool.Tool, len(candidateTools))
	for _, t := range candidateTools {
		if t != nil {
			byName[t.Name()] = t
		}
	}
	out := make([]adktool.Tool, 0, len(spec.Tools))
	for _, name := range spec.Tools {
		t, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("agent: subagent %q: unknown tool %q (not among the %d available tools)", spec.Name, name, len(candidateTools))
		}
		out = append(out, t)
	}
	return out, nil
}

func selectFacadeSubagentToolsets(spec config.SubagentSpec, rootToolsets []adktool.Toolset) ([]adktool.Toolset, error) {
	if len(rootToolsets) == 0 {
		return nil, nil
	}
	var skillSets, mcpSets []adktool.Toolset
	for _, ts := range rootToolsets {
		if ts == nil {
			continue
		}
		if ts.Name() == "skill_toolset" {
			skillSets = append(skillSets, ts)
		} else {
			mcpSets = append(mcpSets, ts)
		}
	}

	var out []adktool.Toolset
	switch {
	case spec.MCP == nil:
		out = append(out, mcpSets...)
	case len(spec.MCP) == 0:
		// Explicit empty mcp: [] withholds MCP/custom toolsets.
	default:
		for _, name := range spec.MCP {
			matched := false
			for _, ts := range mcpSets {
				if ts.Name() == name || strings.HasPrefix(ts.Name(), name+"_") {
					out = append(out, ts)
					matched = true
				}
			}
			if !matched {
				return nil, fmt.Errorf("agent: subagent %q: unknown mcp server %q", spec.Name, name)
			}
		}
	}

	if spec.Skills == nil || len(spec.Skills) > 0 {
		out = append(out, skillSets...)
	}
	return out, nil
}

func checkFacadeSubagentCapability(
	ctx context.Context,
	spec config.SubagentSpec,
	subTools []adktool.Tool,
	subToolsets []adktool.Toolset,
) error {
	if (spec.Capability != config.CapabilityReadOnly && spec.Capability != "diagnoser") || spec.EffectiveMode() == config.SubagentModeSingleTurn {
		return nil
	}
	for _, t := range subTools {
		if t == nil {
			continue
		}
		if !tools.IsReadOnlyTool(t) {
			return fmt.Errorf("agent: subagent %q: capability %q forbids mutating built-in tool %q; drop it or declare capability: %q", spec.Name, spec.Capability, t.Name(), config.CapabilityChangeExecutor)
		}
	}
	for _, ts := range subToolsets {
		if ts == nil || ts.Name() == "skill_toolset" {
			continue
		}
		tsTools, err := ts.Tools(facadeReadonlyCtx{Context: ctx})
		if err != nil {
			return fmt.Errorf("agent: subagent %q: capability %q: failed to list tools on toolset %q: %w", spec.Name, spec.Capability, ts.Name(), err)
		}
		for _, t := range tsTools {
			if t == nil {
				continue
			}
			if !tools.IsReadOnlyTool(t) {
				return fmt.Errorf("agent: subagent %q: capability %q forbids mutating tool %q on toolset %q; narrow tools to read-only or declare capability: %q", spec.Name, spec.Capability, t.Name(), ts.Name(), config.CapabilityChangeExecutor)
			}
		}
	}
	return nil
}

type facadeReadonlyCtx struct{ context.Context }

func (facadeReadonlyCtx) UserContent() *genai.Content          { return nil }
func (facadeReadonlyCtx) InvocationID() string                 { return "" }
func (facadeReadonlyCtx) AgentName() string                    { return "" }
func (facadeReadonlyCtx) UserID() string                       { return "" }
func (facadeReadonlyCtx) AppName() string                      { return "" }
func (facadeReadonlyCtx) SessionID() string                    { return "" }
func (facadeReadonlyCtx) Branch() string                       { return "" }
func (facadeReadonlyCtx) ReadonlyState() session.ReadonlyState { return facadeEmptyState{} }

type facadeEmptyState struct{}

func (facadeEmptyState) Get(string) (any, error) { return nil, session.ErrStateKeyNotExist }
func (facadeEmptyState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {}
}

func mergeUniqueTools(primary, secondary []adktool.Tool) []adktool.Tool {
	seen := make(map[string]bool, len(primary)+len(secondary))
	var out []adktool.Tool
	for _, t := range primary {
		if t == nil || seen[t.Name()] {
			continue
		}
		seen[t.Name()] = true
		out = append(out, t)
	}
	for _, t := range secondary {
		if t == nil || seen[t.Name()] {
			continue
		}
		seen[t.Name()] = true
		out = append(out, t)
	}
	return out
}

func readDirInstruction(projectRoot, agentsDir string) (string, error) {
	var paths []string
	if projectRoot != "" {
		paths = append(paths, filepath.Join(projectRoot, "AGENTS.md"))
	}
	if agentsDir != "" && agentsDir != projectRoot {
		paths = append(paths, filepath.Join(agentsDir, "AGENTS.md"))
	}
	var sections []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", fmt.Errorf("agent: read instruction %q: %w", p, err)
		}
		if trimmed := strings.TrimSpace(string(data)); trimmed != "" {
			sections = append(sections, trimmed)
		}
	}
	return strings.Join(sections, "\n\n"), nil
}

func buildFacadeAgentOptions(
	cfg OneShotConfig,
	loadedCfg *config.Config,
	backend string,
	gate *permissions.Gate,
	tracker *usage.Tracker,
	rootTools []adktool.Tool,
	rootToolsets []adktool.Toolset,
	subagents []*Agent,
	userInst string,
) []Option {
	opts := []Option{
		WithMode(ModeAutonomous),
		WithGate(gate),
		WithUsageTracker(tracker),
		WithBillingBackend(backend),
		WithCostCeiling(CostCeiling{
			MaxTurnUSD:    derefFloat64(loadedCfg.Agent.MaxTurnCostUSD),
			MaxSessionUSD: derefFloat64(loadedCfg.Agent.MaxSessionCostUSD),
		}),
	}
	name := cfg.Name
	if name == "" && loadedCfg.Agent.DisplayName != "" {
		name = loadedCfg.Agent.DisplayName
	}
	if name != "" {
		opts = append(opts, WithName(name))
	}
	desc := cfg.Description
	if desc == "" && loadedCfg.Agent.Description != "" {
		desc = loadedCfg.Agent.Description
	}
	if desc != "" {
		opts = append(opts, WithDescription(desc))
	}
	if userInst != "" {
		opts = append(opts, WithUserInstruction(userInst))
	}
	if loadedCfg.Agent.AppendSystemPrompt != "" {
		opts = append(opts, WithExtraInstruction(loadedCfg.Agent.AppendSystemPrompt))
	}
	if len(rootTools) > 0 {
		opts = append(opts, WithTools(rootTools))
	}
	if len(rootToolsets) > 0 {
		opts = append(opts, WithToolsets(rootToolsets))
	}
	if len(subagents) > 0 {
		opts = append(opts, WithSubagents(subagents))
	}
	if cfg.SessionService != nil {
		opts = append(opts, WithSessionService(cfg.SessionService))
	}
	if cfg.EventLog != nil {
		opts = append(opts, WithEventLog(cfg.EventLog))
	}
	if cfg.SessionID != "" {
		opts = append(opts, WithSession(defaultUserID, cfg.SessionID))
	}
	opts = append(opts, cfg.AgentOptions...)
	return opts
}

func collectFacadeTurn(
	ctx context.Context,
	a *Agent,
	prompt string,
	tracker *usage.Tracker,
	modelName string,
	loadedCfg *config.Config,
) (*OneShotResult, error) {
	var (
		sb           strings.Builder
		lastTurnText string
		lastOutput   any
		tap          usage.TurnTap
		rate         = usage.PriceFor(modelName, loadedCfg)
	)
	for ev, err := range a.Run(WithOperatorTask(ctx, prompt), prompt) {
		if err != nil {
			return nil, err
		}
		if ev != nil && ev.Output != nil {
			lastOutput = ev.Output
		}
		if turnText := extractEventText(ev); turnText != "" {
			lastTurnText = turnText
		}
		collectFinalText(&sb, ev)
		tap.Observe(ev)
		if turnUsage, ok := tap.Commit(ev); ok {
			tracker.AppendUsage(modelName, turnUsage, rate)
		}
	}

	text := strings.TrimSpace(sb.String())
	output := extractStructuredOutput(lastOutput, lastTurnText, text)
	return &OneShotResult{
		Text:      text,
		Output:    output,
		SessionID: a.SessionID(),
		Usage:     tracker.Totals(),
	}, nil
}

func extractEventText(ev *session.Event) string {
	if ev == nil || ev.Content == nil || ev.Partial {
		return ""
	}
	var b strings.Builder
	for _, p := range ev.Content.Parts {
		if p == nil || p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p.Text)
	}
	return strings.TrimSpace(b.String())
}

func extractStructuredOutput(lastOutput any, lastTurnText, fullText string) map[string]any {
	if m, ok := facadeToMap(lastOutput); ok && len(m) > 0 {
		return m
	}
	for _, candidate := range []string{lastTurnText, fullText} {
		trimmed := strings.TrimSpace(candidate)
		if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(trimmed), &m); err == nil && m != nil {
			return m
		}
	}
	return nil
}

func facadeToMap(v any) (map[string]any, bool) {
	if v == nil {
		return nil, false
	}
	if m, ok := v.(map[string]any); ok {
		return m, true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false
	}
	return m, true
}
