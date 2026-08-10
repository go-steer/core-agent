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
	"path/filepath"
	"strings"

	adkmodel "google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/instruction"
	"github.com/go-steer/core-agent/v2/pkg/mcp"
	"github.com/go-steer/core-agent/v2/pkg/models"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/skills"
)

// namedToolset pairs an MCP server's name with its already-started
// toolset, so a scoped subagent can select servers by name. Built from
// the parent's []*mcp.Server at the call site; a server that failed to
// start carries a nil toolset (still listed so name selection can tell
// "exists but down" from "unknown server").
type namedToolset struct {
	name    string
	toolset adktool.Toolset
}

// parentSurface is the parent agent's fully-resolved tool surface, handed
// to buildDeclaredSubagents so each subagent can inherit it whole or take
// a name-scoped subset. It is the single source both the parent and its
// declarative subagents draw from — one config.json + one mcp.json + one
// skills/ tree (docs/declarative-subagents-design.md, resolved OQ1).
type parentSurface struct {
	// builtinTools is the parent's built-in registry (read_file, bash,
	// spawn tools, agentic wrappers, …) after all disable/append passes.
	builtinTools []adktool.Tool
	// mcpToolsets is the parent's already-started MCP servers as (name,
	// toolset) pairs; a scoped subagent selects among them by name and
	// reuses each server's existing toolset (no second mcp.Build, no
	// per-subagent lifecycle).
	mcpToolsets []namedToolset
	// skills is the parent's loaded skill bundle; a scoped subagent gets
	// a name-filtered view via skills.Scoped (no filesystem re-walk).
	skills skills.Skills
}

// subagentDeps bundles the process-scoped collaborators buildDeclaredSubagents
// needs beyond the parent surface — chiefly to stand up a rooted subagent's
// OWN mcp.json + skills/ + persona from a dedicated content root. They are the
// same instances the parent used (one permission gate, one elicitor, one digest
// config), so a rooted subagent's servers and skills are gated and digested
// identically; it gains its own scope, never its own privileges.
type subagentDeps struct {
	// gate is the shared permission gate every subagent's tools run behind.
	gate *permissions.Gate
	// elicitor + digestOpts are handed to mcp.Build for a rooted subagent's
	// own servers, matching the parent's MCP behavior exactly.
	elicitor   mcp.ElicitorFn
	digestOpts *mcp.DigestOptions
	// interp substitutes ${env:VAR} in loaded instruction + skill bodies.
	interp func(string) string
	// send emits a startup line per subagent (and per rooted-MCP hiccup).
	send func(string)
	// rootBase resolves a relative spec.Root, mirroring content_roots: the
	// agents dir when the config was discovered under one, else the cwd.
	rootBase string
}

// buildDeclaredSubagents turns the config's declarative subagents[] block
// into fully-constructed *agent.Agent values, ready to hand to the parent
// via agent.WithSubagents. It is the one piece of glue that makes
// docs/declarative-subagents-design.md real; pkg/agent's subagent
// substrate is reused essentially unchanged.
//
// Each subagent gets:
//   - its own name / description (shown to the parent's model),
//   - its own instruction — inline or an @include chain expanded through
//     pkg/instruction, or (with a root) auto-assembled from the root's own
//     AGENTS.md,
//   - its own model (its ModelConfig, or the parent's when unset),
//   - a recursion depth cap (spec.MaxDepth, honored via
//     WithSubagentMaxDepth; 0 = substrate default), and
//   - a tool surface drawn from one of two sources. Inline (spec.Root
//     unset) resolves MCP + skills by name against the SHARED parent
//     surface — each dimension inherited whole when the field is nil,
//     name-scoped when a non-empty list, or none when an explicit empty
//     list. Rooted (spec.Root set) loads the subagent's OWN mcp.json and
//     skills/ from a dedicated content root, then applies that same
//     nil/list/empty contract WITHIN the root. Built-in tools (spec.Tools)
//     always resolve against the parent registry — built-ins live in the
//     binary, not a directory. Every tool instance carries the shared
//     permission gate, so a subagent cannot escalate.
//
// Returns the subagents plus the MCP servers stood up for rooted subagents
// (empty for the all-inline case). The caller owns their lifecycle: close
// them on shutdown and fold them into the one RegisterMetrics call. On
// error the already-started servers are still returned so the caller can
// close them.
//
// Returns (nil, nil, nil) when no subagents are declared — the caller then
// skips agent.WithSubagents entirely.
func buildDeclaredSubagents(
	ctx context.Context,
	cfg *config.Config,
	parentProvider models.Provider,
	projectRoot string,
	surface parentSurface,
	deps subagentDeps,
) ([]*agent.Agent, []*mcp.Server, error) {
	if len(cfg.Subagents) == 0 {
		return nil, nil, nil
	}
	subs := make([]*agent.Agent, 0, len(cfg.Subagents))
	var rootedServers []*mcp.Server
	for i, spec := range cfg.Subagents {
		llm, err := resolveSubagentModel(ctx, cfg, parentProvider, spec)
		if err != nil {
			return nil, rootedServers, fmt.Errorf("subagents[%d] %q: model: %w", i, spec.Name, err)
		}

		// Built-ins always resolve against the parent registry — they ship
		// in the binary, not in any content root.
		subTools, err := resolveSubagentTools(spec, surface.builtinTools)
		if err != nil {
			return nil, rootedServers, fmt.Errorf("subagents[%d] %q: tools: %w", i, spec.Name, err)
		}

		var (
			subToolsets     []adktool.Toolset
			userInstruction string
			scopeDesc       string
		)
		if spec.Root != "" {
			rootAbs, rootSurface, servers, err := loadSubagentRoot(ctx, spec, deps)
			// servers may be non-nil even on a later error (skills load fails
			// after mcp.Build succeeded) — collect them for shutdown either way.
			rootedServers = append(rootedServers, servers...)
			if err != nil {
				return nil, rootedServers, fmt.Errorf("subagents[%d] %q: %w", i, spec.Name, err)
			}
			subToolsets, scopeDesc, err = resolveSubagentToolsets(ctx, spec, rootSurface)
			if err != nil {
				return nil, rootedServers, fmt.Errorf("subagents[%d] %q: %w", i, spec.Name, err)
			}
			userInstruction, err = rootedSubagentInstruction(spec, rootAbs, deps.interp)
			if err != nil {
				return nil, rootedServers, fmt.Errorf("subagents[%d] %q: instructions: %w", i, spec.Name, err)
			}
			scopeDesc = fmt.Sprintf("root=%s, %s", rootAbs, scopeDesc)
		} else {
			subToolsets, scopeDesc, err = resolveSubagentToolsets(ctx, spec, surface)
			if err != nil {
				return nil, rootedServers, fmt.Errorf("subagents[%d] %q: %w", i, spec.Name, err)
			}
			if spec.Instructions != "" {
				// Inline refs share the project scope: @include resolves
				// against projectRoot exactly like the parent's memory.
				userInstruction, _, err = instruction.Expand(spec.Instructions, projectRoot, projectRoot, instruction.WithInterpolator(deps.interp))
				if err != nil {
					return nil, rootedServers, fmt.Errorf("subagents[%d] %q: instructions: %w", i, spec.Name, err)
				}
			}
		}

		sub, err := assembleSubagent(spec, llm, userInstruction, subTools, subToolsets)
		if err != nil {
			return nil, rootedServers, fmt.Errorf("subagents[%d] %q: %w", i, spec.Name, err)
		}
		subs = append(subs, sub)
		deps.send(fmt.Sprintf("subagent %q: model=%s, %s", spec.Name, llm.Name(), scopeDesc))
	}
	return subs, rootedServers, nil
}

// assembleSubagent builds the *agent.Agent from a resolved model, persona,
// and tool surface — the common tail shared by the inline and rooted paths.
func assembleSubagent(spec config.SubagentSpec, llm adkmodel.LLM, userInstruction string, subTools []adktool.Tool, subToolsets []adktool.Toolset) (*agent.Agent, error) {
	opts := []agent.Option{
		agent.WithName(spec.Name),
		agent.WithDescription(spec.Description),
		// A declarative subagent is invoked as a tool with no human
		// reading its output in real time — run it headless, like
		// the other in-tree spawn paths.
		agent.WithMode(agent.ModeAutonomous),
		agent.WithTools(subTools),
		agent.WithToolsets(subToolsets),
	}
	if userInstruction != "" {
		// Layer 4 (user memory), same slot the parent's AGENTS.md lands in
		// — the harness contract (layers 1–3) stays intact beneath the
		// subagent's persona.
		opts = append(opts, agent.WithUserInstruction(userInstruction))
	}
	if spec.MaxDepth > 0 {
		opts = append(opts, agent.WithSubagentMaxDepth(spec.MaxDepth))
	}
	return agent.New(llm, opts...)
}

// loadSubagentRoot stands up a rooted subagent's OWN scope from a dedicated
// content root: its mcp.json servers and skills/ tree, returned as a
// parentSurface the shared resolveSubagentToolsets can scope exactly as it
// scopes the inline path — the only difference is the surface is the root's,
// not the parent's. A relative spec.Root resolves against deps.rootBase
// (mirroring content_roots); an absolute path passes through. A missing or
// non-directory root is a loud error: the operator declared it, so a typo
// must surface rather than silently yield an empty scope.
//
// The MCP servers are returned (even when a later step errors) so the caller
// can terminate their stdio children on shutdown. mcp.Build errors are
// non-fatal, matching the parent: a down server surfaces as StatusError with
// a nil toolset and is skipped by name selection.
func loadSubagentRoot(ctx context.Context, spec config.SubagentSpec, deps subagentDeps) (string, parentSurface, []*mcp.Server, error) {
	rootAbs := spec.Root
	if !filepath.IsAbs(rootAbs) {
		rootAbs = filepath.Join(deps.rootBase, rootAbs)
	}
	rootAbs = filepath.Clean(rootAbs)
	info, err := os.Stat(rootAbs)
	if err != nil {
		return "", parentSurface{}, nil, fmt.Errorf("root: %w", err)
	}
	if !info.IsDir() {
		return "", parentSurface{}, nil, fmt.Errorf("root %q is not a directory", rootAbs)
	}

	// Own MCP servers from <root>/mcp.json (no home-agents overlay — the
	// root is self-contained, which is the point of an independent scope).
	servers, _, mcpErr := mcp.Build(ctx, rootAbs, "", deps.send, deps.gate, deps.elicitor, deps.digestOpts)
	if mcpErr != nil {
		deps.send(fmt.Sprintf("subagent %q: mcp: %v", spec.Name, mcpErr))
	}
	named := make([]namedToolset, 0, len(servers))
	for _, s := range servers {
		if s == nil {
			continue
		}
		named = append(named, namedToolset{name: s.Name, toolset: s.Toolset()})
	}

	// Own skills from <root>/skills/ (again no home/user overlay).
	rootSkills, err := skills.LoadAll(ctx, rootAbs, "", deps.gate, skills.WithInterpolator(deps.interp))
	if err != nil {
		return "", parentSurface{}, servers, fmt.Errorf("skills: %w", err)
	}

	// builtinTools intentionally nil: resolveSubagentTools already resolved
	// built-ins against the parent registry; resolveSubagentToolsets reads
	// only mcpToolsets + skills from the surface.
	return rootAbs, parentSurface{mcpToolsets: named, skills: rootSkills}, servers, nil
}

// rootedSubagentInstruction resolves a rooted subagent's persona. An inline
// spec.Instructions overrides the root's memory files (with @include confined
// to the root); otherwise the persona auto-assembles from the root's own
// AGENTS.md + AGENTS.d/, loaded as a self-contained content root so an
// @include cannot escape it.
func rootedSubagentInstruction(spec config.SubagentSpec, rootAbs string, interp func(string) string) (string, error) {
	if spec.Instructions != "" {
		expanded, _, err := instruction.Expand(spec.Instructions, rootAbs, rootAbs, instruction.WithInterpolator(interp))
		return expanded, err
	}
	loaded, err := instruction.Load("", "", instruction.WithContentRoots([]string{rootAbs}), instruction.WithInterpolator(interp))
	if err != nil {
		return "", err
	}
	return loaded.Instruction, nil
}

// resolveSubagentTools returns the built-in tool subset a subagent runs
// with. Per the nil-vs-empty contract (pinned by config's
// TestSubagents_EmptyVsOmittedRefs): a nil spec.Tools inherits the
// parent's full registry; a non-nil list selects those tools by name; an
// explicit empty list grants none. An unknown name is a config error
// (fail loud rather than silently dropping a tool the operator asked for).
func resolveSubagentTools(spec config.SubagentSpec, parent []adktool.Tool) ([]adktool.Tool, error) {
	if spec.Tools == nil {
		return parent, nil
	}
	byName := make(map[string]adktool.Tool, len(parent))
	for _, t := range parent {
		byName[t.Name()] = t
	}
	out := make([]adktool.Tool, 0, len(spec.Tools))
	for _, name := range spec.Tools {
		t, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown tool %q (not among the %d built-in tools)", name, len(parent))
		}
		out = append(out, t)
	}
	return out, nil
}

// resolveSubagentToolsets assembles a subagent's MCP + skills toolsets
// from the shared parent surface, honoring the same nil=inherit /
// list=scope / empty=none contract per dimension. It also returns a short
// human-readable scope description for the startup log.
func resolveSubagentToolsets(ctx context.Context, spec config.SubagentSpec, surface parentSurface) ([]adktool.Toolset, string, error) {
	var out []adktool.Toolset
	var desc []string

	// MCP: reuse each already-started server's toolset — never a second
	// mcp.Build. A named server that exists but failed to start has a nil
	// toolset; skip it silently (the parent skips it too, and its
	// StatusError is already surfaced in the startup summary).
	switch spec.MCP {
	case nil:
		for _, s := range surface.mcpToolsets {
			if s.toolset != nil {
				out = append(out, s.toolset)
			}
		}
		desc = append(desc, "mcp=inherit")
	default:
		byName := make(map[string]namedToolset, len(surface.mcpToolsets))
		for _, s := range surface.mcpToolsets {
			byName[s.name] = s
		}
		for _, name := range spec.MCP {
			s, ok := byName[name]
			if !ok {
				return nil, "", fmt.Errorf("mcp: unknown server %q (not in mcp.json)", name)
			}
			if s.toolset != nil {
				out = append(out, s.toolset)
			}
		}
		desc = append(desc, fmt.Sprintf("mcp=[%s]", strings.Join(spec.MCP, " ")))
	}

	// Skills: inherit the full toolset, or a name-scoped view.
	switch spec.Skills {
	case nil:
		if !surface.skills.Empty() {
			out = append(out, surface.skills.Toolset)
		}
		desc = append(desc, "skills=inherit")
	default:
		scoped, err := surface.skills.Scoped(ctx, spec.Skills)
		if err != nil {
			return nil, "", err
		}
		if !scoped.Empty() {
			out = append(out, scoped.Toolset)
		}
		desc = append(desc, fmt.Sprintf("skills=[%s]", strings.Join(spec.Skills, " ")))
	}

	if spec.Tools == nil {
		desc = append(desc, "tools=inherit")
	} else {
		desc = append(desc, fmt.Sprintf("tools=[%s]", strings.Join(spec.Tools, " ")))
	}

	return out, strings.Join(desc, ", "), nil
}

// resolveSubagentModel returns the LLM a subagent runs on: its own
// ModelConfig when spec.Model is set, otherwise the parent's model built
// from the parent's already-resolved provider. When the subagent declares
// its own model, a provider is resolved for it through the same
// models.Resolve path the parent used — so provider-specific config
// (Vertex project/location, Anthropic-Vertex, scripted transcript) and
// env auto-detection behave identically.
func resolveSubagentModel(ctx context.Context, cfg *config.Config, parentProvider models.Provider, spec config.SubagentSpec) (adkmodel.LLM, error) {
	if spec.Model == nil {
		// Inherit: reuse the parent's provider, ask it for the parent's
		// model. A fresh LLM instance (not the parent's *m*) keeps the
		// subagent independent of any parent-side recorder wrapping.
		return parentProvider.Model(ctx, cfg.Model.Name)
	}
	// Own model: shallow-copy cfg with the subagent's Model so
	// models.Resolve reads the right provider + provider-specific
	// sub-blocks. Only Model is overwritten; the copy shares the rest of
	// cfg by value, which Resolve does not mutate.
	subCfg := *cfg
	subCfg.Model = *spec.Model
	p, err := models.Resolve(&subCfg)
	if err != nil {
		return nil, fmt.Errorf("resolve provider: %w", err)
	}
	return p.Model(ctx, spec.Model.Name)
}
