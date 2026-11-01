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

package evalcompose

import (
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agent"
)

// Mode is the ADK v2 agent mode a specialist runs in.
type Mode string

const (
	ModeTask       Mode = "Task"
	ModeSingleTurn Mode = "SingleTurn"
)

// Capability is what a specialist is allowed to do to the world.
type Capability string

const (
	CapabilityReadOnly       Capability = "read_only"
	CapabilityChangeExecutor Capability = "change_executor"
)

// SpecialistBudget captures per-specialist runtime bounds.
type SpecialistBudget struct {
	MaxTurns            int
	MaxWallclockSeconds int
	MaxCostUSD          float64
}

// MCPAllowlist is the per-MCP-server tool allowlist for a specialist.
type MCPAllowlist struct {
	Server string
	Tools  []string
}

// ToolAllowlist is the composite allowlist of built-in tools, MCP
// tools, and skills a specialist may invoke.
type ToolAllowlist struct {
	Builtin []string
	MCP     []MCPAllowlist
	Skills  []string
}

// InheritsAllMCP reports whether this allowlist leaves the MCP axis
// unrestricted (nil slice).
func (t ToolAllowlist) InheritsAllMCP() bool { return t.MCP == nil }

// Spec is a specialist definition used by the cost-tier eval check.
type Spec struct {
	Name         string
	Description  string
	Mode         Mode
	Model        string
	Tier         string
	Capability   Capability
	Budget       SpecialistBudget
	Tools        ToolAllowlist
	Instruction  string
	OutputSchema *genai.Schema
}

// ModelResolver turns a specialist's `model:` override into a model.LLM.
type ModelResolver func(name string) (model.LLM, error)

// TierResolver turns a specialist's `tier:` declaration into a model.LLM.
type TierResolver func(tier string) (model.LLM, error)

// BuildOptions carries the runtime bindings a Spec needs to become a
// concrete ADK agent.
type BuildOptions struct {
	Model       model.LLM
	Resolve     ModelResolver
	ResolveTier TierResolver
	Tools       []tool.Tool
	Toolsets    []tool.Toolset
}

func (o BuildOptions) modelFor(spec Spec) (model.LLM, error) {
	if spec.Model != "" && spec.Tier != "" {
		return nil, fmt.Errorf("specialists: build %q: declares both model %q and tier %q (use one)", spec.Name, spec.Model, spec.Tier)
	}
	if spec.Tier != "" {
		if o.ResolveTier == nil {
			return nil, fmt.Errorf("specialists: build %q: tier %q declared but BuildOptions.ResolveTier is nil", spec.Name, spec.Tier)
		}
		m, err := o.ResolveTier(spec.Tier)
		if err != nil {
			return nil, fmt.Errorf("specialists: build %q: resolve tier %q: %w", spec.Name, spec.Tier, err)
		}
		if m == nil {
			return nil, fmt.Errorf("specialists: build %q: tier resolver returned nil for tier %q", spec.Name, spec.Tier)
		}
		return m, nil
	}
	if spec.Model == "" {
		return o.Model, nil
	}
	if o.Resolve == nil {
		return nil, fmt.Errorf("specialists: build %q: model override %q declared but BuildOptions.Resolve is nil", spec.Name, spec.Model)
	}
	m, err := o.Resolve(spec.Model)
	if err != nil {
		return nil, fmt.Errorf("specialists: build %q: resolve model override %q: %w", spec.Name, spec.Model, err)
	}
	if m == nil {
		return nil, fmt.Errorf("specialists: build %q: model resolver returned nil for override %q", spec.Name, spec.Model)
	}
	return m, nil
}

type filteredToolset struct {
	inner   tool.Toolset
	allowed map[string]bool
}

func (f *filteredToolset) Name() string { return f.inner.Name() }

func (f *filteredToolset) Tools(ctx adkagent.ReadonlyContext) ([]tool.Tool, error) {
	offered, err := f.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	var kept []tool.Tool
	for _, t := range offered {
		if f.allowed[t.Name()] {
			kept = append(kept, t)
		}
	}
	return kept, nil
}

func filterToolsets(spec Spec, offered []tool.Toolset) []tool.Toolset {
	if spec.Tools.InheritsAllMCP() {
		return offered
	}
	byServer := make(map[string]MCPAllowlist, len(spec.Tools.MCP))
	for _, al := range spec.Tools.MCP {
		byServer[al.Server] = al
	}
	var out []tool.Toolset
	for _, ts := range offered {
		al, ok := byServer[ts.Name()]
		if !ok {
			continue
		}
		if len(al.Tools) == 0 {
			out = append(out, ts)
			continue
		}
		allowed := make(map[string]bool, len(al.Tools))
		for _, n := range al.Tools {
			allowed[n] = true
		}
		out = append(out, &filteredToolset{inner: ts, allowed: allowed})
	}
	return out
}

// Build turns a Spec into an ADK agent using core-agent's mode constructors
// (agent.NewTaskAgent / agent.NewSingleTurnAgent), wiring CallGate refusal
// and stall completion automatically.
func Build(spec Spec, opts BuildOptions) (adkagent.Agent, error) {
	if opts.Model == nil {
		return nil, fmt.Errorf("specialists: build %q: BuildOptions.Model is required", spec.Name)
	}
	llm, err := opts.modelFor(spec)
	if err != nil {
		return nil, err
	}
	switch spec.Mode {
	case ModeTask, "":
		var after []llmagent.AfterModelCallback
		if spec.OutputSchema == nil {
			after = []llmagent.AfterModelCallback{agent.FinishOnStall(spec.Name, nil)}
		}
		return agent.NewTaskAgent(agent.TaskAgentConfig{
			Name:                spec.Name,
			Description:         spec.Description,
			Instruction:         spec.Instruction,
			Model:               llm,
			Tools:               opts.Tools,
			Toolsets:            filterToolsets(spec, opts.Toolsets),
			OutputSchema:        spec.OutputSchema,
			AfterModelCallbacks: after,
		})
	case ModeSingleTurn:
		return agent.NewSingleTurnAgent(agent.SingleTurnAgentConfig{
			Name:         spec.Name,
			Description:  spec.Description,
			Instruction:  spec.Instruction,
			Model:        llm,
			OutputSchema: spec.OutputSchema,
		})
	default:
		return nil, fmt.Errorf("specialists: build %q: unknown mode %q", spec.Name, spec.Mode)
	}
}

// Reserved specialist names.
const (
	SynthesisName = "_synthesis"
	FallbackName  = "_fallback"
)

// Dispatch selects the root shape BuildRoot assembles.
type Dispatch string

const (
	DispatchCoordinator Dispatch = "coordinator"
	DispatchGraph       Dispatch = "graph"
	DispatchFanout      Dispatch = "fanout"
	DispatchAuto        Dispatch = "auto"
)

// Resolve returns the dispatch shape to build, given the caller's choice
// and the bundle's own declaration.
func (d Dispatch) Resolve(b Bundle) Dispatch {
	if d != "" && d != DispatchAuto {
		return d
	}
	if b.Dispatch != "" {
		return Dispatch(b.Dispatch)
	}
	if d == "" {
		return DispatchAuto
	}
	return d
}

// RosterShape reads the dispatch shape out of a roster.
func RosterShape(specs []Spec) Dispatch {
	var hasSynthesis, hasFallback, hasClassifier bool
	for _, s := range specs {
		if s.Mode == ModeSingleTurn {
			hasClassifier = true
			continue
		}
		switch s.Name {
		case SynthesisName:
			hasSynthesis = true
		case FallbackName:
			hasFallback = true
		}
	}
	switch {
	case hasSynthesis:
		return DispatchFanout
	case hasClassifier && hasFallback:
		return DispatchGraph
	default:
		return DispatchCoordinator
	}
}

// FanoutConfig configures the fan-out dispatch shape.
type FanoutConfig struct {
	MaxConcurrency int
}

// Bundle is the workload definition subset used by the evals.
type Bundle struct {
	Name        string
	Description string
	Specialists []string
	Dispatch    string
	Fanout      FanoutConfig
}
