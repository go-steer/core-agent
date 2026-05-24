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

//go:build !no_tui

package main

import (
	"context"
	"fmt"

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/internal/tui"
	"github.com/go-steer/core-agent/mcp"
	"github.com/go-steer/core-agent/permissions"
)

// pkgElicitor is the TUI elicitor instance shared between main.go's
// mcp.Build call (which receives its Elicit method as the
// ElicitorFn) and launchTUI (which threads the same handle into
// tui.Options so the bubble-tea program can attach itself after
// construction). Set in makeMCPElicitor; consumed in launchTUI.
//
// Package-level rather than wired through every call site because
// the two halves are inherently coupled (you can't have one
// without the other in the !no_tui build).
var pkgElicitor *tui.Elicitor

// makeMCPElicitor constructs the TUI elicitor and returns its
// ElicitorFn binding for mcp.Build. The same elicitor instance is
// later attached to the bubble-tea program inside launchTUI.
//
// In the no_tui build (tui_disabled.go) this returns nil — MCP
// elicit requests then fail with the standard "no elicitor"
// decline, which the SDK surfaces as a server-side cancel.
func makeMCPElicitor() mcp.ElicitorFn {
	pkgElicitor = tui.NewElicitor()
	return pkgElicitor.Elicit
}

// launchTUI assembles the tui.Options + runs the bubble-tea TUI.
// Returns (didRun, exitCode, err). didRun=false means the caller
// should fall through to the REPL fallback; in this build that
// only happens when the launch itself fails before tui.Run is
// reached — successful launches always return didRun=true even
// when the user /quits immediately.
//
// The no_tui build's launchTUI always returns didRun=false so the
// caller falls through to REPL silently.
func launchTUI(ctx context.Context, deps tuiDeps) (didRun bool, exitCode int, err error) {
	a, err := agent.New(deps.Model, deps.AgentOpts...)
	if err != nil {
		return false, 0, fmt.Errorf("agent.New: %w", err)
	}

	tuiOpts := tui.Options{
		Agent:      a,
		Cfg:        deps.Cfg,
		Gate:       deps.Gate,
		Tracker:    deps.Tracker,
		Memory:     deps.Memory,
		MCPServers: deps.MCPServers,
		Skills:     deps.LoadedSkills,
		AgentsDir:  deps.AgentsDir,
		Elicitor:   pkgElicitor,

		// /model swaps the model mid-session, preserving tools +
		// toolsets + instruction. We re-resolve the provider for
		// the new model name through the same path startup uses.
		RebuildAgent: func(modelID string) (*agent.Agent, error) {
			newLLM, lerr := deps.Provider.Model(ctx, modelID)
			if lerr != nil {
				return nil, lerr
			}
			return agent.New(newLLM, deps.AgentOpts...)
		},

		// Always-allow doesn't need agentsDir to function, only to
		// persist path-scope additions to disk.
		AlwaysAllow: func(req permissions.PromptRequest) error {
			if req.PersistTool != "path_scope" || deps.AgentsDir == "" {
				return nil
			}
			return appendPathScope(deps.AgentsDir, req.PersistKey)
		},
		SessionApprovals: deps.Gate.Approvals,
	}

	// Disk-persistence callbacks wire only when there's a project
	// root to write into. Without .agents/ the slash commands
	// degrade to a clean "no project root" message rather than
	// scribbling files into cwd.
	if deps.AgentsDir != "" {
		agentsDir := deps.AgentsDir
		tuiOpts.PersistModelChoice = func(modelID string) error {
			return persistModelChoice(agentsDir, modelID)
		}
		tuiOpts.AddAllowPatterns = func(patterns []string) error {
			if err := deps.Gate.AddAllowPatterns(patterns); err != nil {
				return err
			}
			return appendPermissionsAllow(agentsDir, patterns)
		}
		tuiOpts.AddDenyPatterns = func(patterns []string) error {
			if err := deps.Gate.AddDenyPatterns(patterns); err != nil {
				return err
			}
			return appendPermissionsDeny(agentsDir, patterns)
		}
		tuiOpts.AddBuiltinAllowExtra = func(name string) error {
			entries, ok := permissions.Bundles[name]
			if !ok {
				return fmt.Errorf("unknown bundle %q (want one of %v)", name, permissions.KnownBundles())
			}
			if err := deps.Gate.AddAllowPatterns(entries); err != nil {
				return err
			}
			return appendBuiltinAllowExtra(agentsDir, name)
		}
	}

	// /pricing refresh + /pricing set callbacks require a writable
	// user-home directory. When unset the slash handlers degrade
	// with a "not available" message.
	if deps.CoreHome != "" {
		coreHome := deps.CoreHome
		agentsDir := deps.AgentsDir
		cfg := deps.Cfg
		tuiOpts.RefreshPricing = func(rctx context.Context) (string, error) {
			return refreshPricingForTUI(rctx, cfg, agentsDir, coreHome)
		}
		tuiOpts.SetPricing = func(model string, in, out float64) (string, error) {
			return setPricingForTUI(cfg, agentsDir, coreHome, model, in, out)
		}
	}

	code, err := tui.Run(ctx, tuiOpts)
	return true, code, err
}
