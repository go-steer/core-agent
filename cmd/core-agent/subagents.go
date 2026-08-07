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
	"strings"

	adkmodel "google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/instruction"
	"github.com/go-steer/core-agent/v2/pkg/models"
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

// buildDeclaredSubagents turns the config's declarative subagents[] block
// into fully-constructed *agent.Agent values, ready to hand to the parent
// via agent.WithSubagents. It is the one piece of glue that makes
// docs/declarative-subagents-design.md real; pkg/agent's subagent
// substrate is reused essentially unchanged.
//
// Each subagent gets:
//   - its own name / description (shown to the parent's model),
//   - its own instruction — inline or an @include chain expanded through
//     pkg/instruction, scope-confined to projectRoot exactly like the
//     parent's memory,
//   - its own model (its ModelConfig, or the parent's when unset),
//   - a recursion depth cap (spec.MaxDepth, honored via
//     WithSubagentMaxDepth; 0 = substrate default), and
//   - a tool surface: built-ins (spec.Tools), MCP servers (spec.MCP), and
//     skills (spec.Skills) — each dimension inherited whole when the field
//     is nil, name-scoped when it's a non-empty list, or granted none when
//     it's an explicit empty list. Inline refs resolve by name against the
//     shared parent surface; an inherited tool instance already carries
//     the parent's permission gate, so a subagent cannot escalate.
//
// Returns (nil, nil) when no subagents are declared — the caller then
// skips agent.WithSubagents entirely.
func buildDeclaredSubagents(
	ctx context.Context,
	cfg *config.Config,
	parentProvider models.Provider,
	projectRoot string,
	surface parentSurface,
	interp func(string) string,
	send func(string),
) ([]*agent.Agent, error) {
	if len(cfg.Subagents) == 0 {
		return nil, nil
	}
	subs := make([]*agent.Agent, 0, len(cfg.Subagents))
	for i, spec := range cfg.Subagents {
		llm, err := resolveSubagentModel(ctx, cfg, parentProvider, spec)
		if err != nil {
			return nil, fmt.Errorf("subagents[%d] %q: model: %w", i, spec.Name, err)
		}

		subTools, err := resolveSubagentTools(spec, surface.builtinTools)
		if err != nil {
			return nil, fmt.Errorf("subagents[%d] %q: tools: %w", i, spec.Name, err)
		}
		subToolsets, scopeDesc, err := resolveSubagentToolsets(ctx, spec, surface)
		if err != nil {
			return nil, fmt.Errorf("subagents[%d] %q: %w", i, spec.Name, err)
		}

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

		if spec.Instructions != "" {
			expanded, _, err := instruction.Expand(spec.Instructions, projectRoot, projectRoot, instruction.WithInterpolator(interp))
			if err != nil {
				return nil, fmt.Errorf("subagents[%d] %q: instructions: %w", i, spec.Name, err)
			}
			// Layer 4 (user memory), same slot the parent's AGENTS.md
			// lands in — the harness contract (layers 1–3) stays intact
			// beneath the subagent's persona.
			opts = append(opts, agent.WithUserInstruction(expanded))
		}
		if spec.MaxDepth > 0 {
			opts = append(opts, agent.WithSubagentMaxDepth(spec.MaxDepth))
		}

		sub, err := agent.New(llm, opts...)
		if err != nil {
			return nil, fmt.Errorf("subagents[%d] %q: %w", i, spec.Name, err)
		}
		subs = append(subs, sub)
		send(fmt.Sprintf("subagent %q: model=%s, %s", spec.Name, llm.Name(), scopeDesc))
	}
	return subs, nil
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
