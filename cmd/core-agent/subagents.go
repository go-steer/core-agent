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

	adkmodel "google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/instruction"
	"github.com/go-steer/core-agent/v2/pkg/models"
)

// buildDeclaredSubagents turns the config's declarative subagents[] block
// into fully-constructed *agent.Agent values, ready to hand to the parent
// via agent.WithSubagents. It is the one piece of glue that makes
// docs/declarative-subagents-design.md real; pkg/agent's subagent
// substrate is reused unchanged.
//
// Each subagent gets:
//   - its own name / description (shown to the parent's model),
//   - its own instruction — inline or an @include chain expanded through
//     pkg/instruction, scope-confined to projectRoot exactly like the
//     parent's memory,
//   - its own model (its ModelConfig, or the parent's when unset), and
//   - a recursion depth cap (spec.MaxDepth, honored via
//     WithSubagentMaxDepth; 0 = substrate default).
//
// γ.2 (this function): the tool surface is INHERITED from the parent —
// each subagent receives the parent's built-in tools and toolsets. Those
// tool instances already carry the parent's permission gate, so an
// inherited surface cannot escalate. Narrowing by spec.Tools / spec.MCP /
// spec.Skills (inline refs against the shared config) lands in γ.3; until
// then a subagent that declares those fields still inherits the full
// surface (config.Validate accepts them; they are simply not yet applied).
//
// Returns (nil, nil) when no subagents are declared — the caller then
// skips agent.WithSubagents entirely.
func buildDeclaredSubagents(
	ctx context.Context,
	cfg *config.Config,
	parentProvider models.Provider,
	projectRoot string,
	builtinTools []adktool.Tool,
	allToolsets []adktool.Toolset,
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

		opts := []agent.Option{
			agent.WithName(spec.Name),
			agent.WithDescription(spec.Description),
			// A declarative subagent is invoked as a tool with no human
			// reading its output in real time — run it headless, like
			// the other in-tree spawn paths.
			agent.WithMode(agent.ModeAutonomous),
			// γ.2: inherit the parent's full surface. γ.3 replaces these
			// with the name-filtered subset when spec.Tools/MCP/Skills
			// are set.
			agent.WithTools(builtinTools),
			agent.WithToolsets(allToolsets),
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
		send(fmt.Sprintf("subagent %q: model=%s (inherits parent tool surface)", spec.Name, llm.Name()))
	}
	return subs, nil
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
