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
	"context"
	"fmt"
	"iter"
	"math"
	"slices"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-models/pricing"
	"github.com/go-steer/core-models/profile"

	"github.com/go-steer/core-agent/v3/pkg/agent"
)

type priceTable struct {
	cat      *pricing.Catalog
	declared map[string]pricing.Rates
}

func (t *priceTable) lookup(backend, modelID string) (pricing.Rates, bool) {
	if r, ok := t.cat.LookupFor(backend, modelID); ok && !r.IsZero() {
		return r, true
	}
	if backend != "" {
		if r, ok := t.declared[strings.ToLower(backend+"/"+modelID)]; ok && !r.IsZero() {
			return r, true
		}
	}
	return pricing.Rates{}, false
}

func pricingTable() *priceTable {
	profileRegistry.mu.RLock()
	t := profileRegistry.prices
	profileRegistry.mu.RUnlock()
	if t != nil {
		return t
	}
	profileRegistry.mu.Lock()
	defer profileRegistry.mu.Unlock()
	if profileRegistry.prices != nil {
		return profileRegistry.prices
	}
	opts := pricing.Options{
		Declared: pricing.DeclaredFromProfiles(declaringProfiles(profileRegistry.declared)),
		Profiles: profileRegistry.declared,
	}
	c, err := pricing.NewCatalog(opts)
	if err != nil {
		panic(fmt.Sprintf("evalcompose: pricing catalog: %v", err))
	}
	t = &priceTable{cat: c, declared: make(map[string]pricing.Rates, len(opts.Declared))}
	for k, r := range opts.Declared {
		t.declared[strings.ToLower(k)] = r.Rates()
	}
	profileRegistry.prices = t
	return t
}

func declaringProfiles(declared []profile.Profile) []profile.Profile {
	var out []profile.Profile
	seen := map[string]bool{}
	all := profile.BuiltinNames()
	for _, p := range declared {
		all = append(all, p.Name)
	}
	for _, name := range all {
		if seen[name] {
			continue
		}
		seen[name] = true
		p, err := profile.Find(name, declared)
		if err != nil {
			continue
		}
		if p, err = profile.Expand(p); err == nil {
			out = append(out, p)
		}
	}
	slices.Reverse(out)
	return out
}

type catalogPricer struct{}

func (catalogPricer) PriceCall(backend, modelID string, c Call) (float64, bool) {
	r, ok := pricingTable().lookup(backend, modelID)
	if !ok {
		return 0, false
	}
	return r.CostUSDWithCacheTTLs(
		int(c.UncachedInputTokens),
		int(c.CachedInputTokens),
		int(c.CacheWriteTokens),
		int(c.CacheWrite1hTokens),
		int(c.OutputTokens),
	), true
}

// RatePer1K derives the flat USD-per-1K-total-tokens rate for a model.
func RatePer1K(provider, modelName string) float64 {
	backend := Backend(provider, modelName)
	blend := func() (float64, bool) {
		r, ok := pricingTable().lookup(backend, modelName)
		if !ok {
			return 0, false
		}
		return (r.InputPerMTok + r.OutputPerMTok) / 2 / 1000, true
	}
	switch {
	case modelName == "echo", modelName == "scripted", modelName == "toolactor":
		return 0.05
	case strings.HasPrefix(modelName, "claude-"):
		if rate, ok := blend(); ok {
			return rate
		}
		return 0.003
	case strings.HasPrefix(modelName, "gemini-"):
		if rate, ok := blend(); ok {
			return rate
		}
		return 0.0006
	default:
		if IsProfileProvider(provider) {
			if rate, ok := blend(); ok {
				return rate
			}
			return 0
		}
		return 0.001
	}
}

// ModelLimits is the price half of a meter scope for one model.
func ModelLimits(provider, modelName string) Limits {
	return Limits{
		Backend:   Backend(provider, modelName),
		Model:     modelName,
		Pricer:    catalogPricer{},
		RatePer1K: RatePer1K(provider, modelName),
	}
}

// MeterScopes derives the per-specialist budget scopes for a roster.
func MeterScopes(specs []Spec, provider, rootModelName string) map[string]Limits {
	fake := IsOfflineFake(rootModelName)
	var scopes map[string]Limits
	for _, s := range specs {
		l := Limits{
			MaxTurns:   s.Budget.MaxTurns,
			MaxCostUSD: s.Budget.MaxCostUSD,
		}
		if name := SpecModelName(s, provider, rootModelName); name != "" && !fake {
			p := ModelLimits(provider, name)
			l.Backend, l.Model, l.Pricer, l.RatePer1K = p.Backend, p.Model, p.Pricer, p.RatePer1K
		}
		if l.IsZero() {
			continue
		}
		if scopes == nil {
			scopes = make(map[string]Limits, len(specs))
		}
		scopes[s.Name] = l
	}
	return scopes
}

// RootConfig carries everything BuildRoot needs to turn a bundle and
// specs into a runnable root agent.
type RootConfig struct {
	Bundle             Bundle
	Specs              []Spec
	ProjectInstruction string
	Model              model.LLM
	ModelName          string
	Provider           string
	Tools              []tool.Tool
	Toolsets           []tool.Toolset
	Dispatch           Dispatch
}

// BuildRoot builds the roster and assembles the requested dispatch shape
// (coordinator or fan-out).
func BuildRoot(ctx context.Context, cfg RootConfig) (adkagent.Agent, []tool.Tool, error) {
	dispatch := cfg.Dispatch.Resolve(cfg.Bundle)
	if dispatch == DispatchAuto {
		dispatch = RosterShape(cfg.Specs)
	}

	resolve := NewModelResolver(ctx, cfg.Provider, cfg.ModelName, cfg.Model, ModelOptions{})
	resolveTier := NewTierResolver(cfg.Provider, cfg.ModelName, cfg.Model, resolve)

	byName := make(map[string]adkagent.Agent, len(cfg.Specs))
	var analysts []analystBranch
	var synthAgent adkagent.Agent
	var synthBudget SpecialistBudget
	for _, spec := range cfg.Specs {
		opts := BuildOptions{
			Model:       cfg.Model,
			Resolve:     resolve,
			ResolveTier: resolveTier,
		}
		if spec.Mode != ModeSingleTurn {
			opts.Tools = cfg.Tools
			opts.Toolsets = cfg.Toolsets
		}
		a, err := Build(spec, opts)
		if err != nil {
			return nil, nil, fmt.Errorf("build specialist %q: %w", spec.Name, err)
		}
		byName[spec.Name] = a
		if spec.Name == SynthesisName {
			synthAgent = a
			synthBudget = spec.Budget
		} else if spec.Mode != ModeSingleTurn && spec.Name != FallbackName {
			analysts = append(analysts, analystBranch{
				name:   spec.Name,
				agent:  a,
				budget: spec.Budget,
			})
		}
	}

	switch dispatch {
	case DispatchFanout:
		root, err := buildFanout(cfg.Bundle, analysts, synthAgent, synthBudget)
		return root, nil, err
	case DispatchCoordinator:
		order := cfg.Bundle.Specialists
		if len(order) == 0 {
			for _, s := range cfg.Specs {
				order = append(order, s.Name)
			}
		}
		subAgents := make([]adkagent.Agent, 0, len(order))
		for _, name := range order {
			sp, ok := byName[name]
			if !ok {
				return nil, nil, fmt.Errorf("coordinator: workload %q references specialist %q which is not loaded", cfg.Bundle.Name, name)
			}
			subAgents = append(subAgents, sp)
		}
		name := cfg.Bundle.Name
		if name == "" {
			name = "eval"
		}
		root, err := agent.NewCoordinator(agent.CoordinatorConfig{
			Name:        name + "_coordinator",
			Description: cfg.Bundle.Description,
			Instruction: defaultCoordinatorInstruction(cfg.Bundle),
			Model:       cfg.Model,
			SubAgents:   subAgents,
		})
		return root, nil, err
	default:
		return nil, nil, fmt.Errorf("evalcompose: unsupported dispatch %q (want coordinator, fanout, or auto)", dispatch)
	}
}

func defaultCoordinatorInstruction(b Bundle) string {
	pick := "Consult the tool descriptions\nto pick the right one for the reported failure mode."
	if slices.Contains(b.Specialists, FallbackName) {
		pick = "Consult the tool descriptions\nto pick the right one for the reported failure mode; fall back to the\n\"_fallback\" specialist when no per-failure-mode specialist applies."
	}
	return fmt.Sprintf(`You are the coordinator for the %q workload.

You will receive an incident envelope on each turn. Your job is to
choose the right specialist for the incident and delegate to it, then
summarise the specialist's finding as a short structured "INCIDENT
SUMMARY" block for the operator.

Each specialist is available as a tool. %s

Do not attempt remediations yourself — return analysis only. Be
concise; operators are on-call.`, b.Name, pick)
}

type evalEchoModel struct {
	name string
}

func (m *evalEchoModel) Name() string { return m.name }

func (m *evalEchoModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		last := lastEchoUserText(req)
		usage := &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     int32(min(len(last)/4, math.MaxInt32)), // #nosec G115
			CandidatesTokenCount: 16,
		}
		usage.TotalTokenCount = usage.PromptTokenCount + usage.CandidatesTokenCount
		if _, ok := req.Tools["finish_task"]; ok {
			yield(&model.LLMResponse{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{genai.NewPartFromFunctionCall("finish_task", map[string]any{
						"result": "[echo triage] diagnosed from envelope",
					})},
				},
				UsageMetadata: usage,
				TurnComplete:  true,
				FinishReason:  genai.FinishReasonStop,
			}, nil)
			return
		}
		reply := fmt.Sprintf("[echo] acknowledged: %s", last)
		yield(&model.LLMResponse{
			Content:       genai.NewContentFromText(reply, genai.RoleModel),
			UsageMetadata: usage,
			TurnComplete:  true,
			FinishReason:  genai.FinishReasonStop,
		}, nil)
	}
}

func lastEchoUserText(req *model.LLMRequest) string {
	if req == nil {
		return ""
	}
	for i := len(req.Contents) - 1; i >= 0; i-- {
		c := req.Contents[i]
		if c == nil || c.Role != genai.RoleUser {
			continue
		}
		for _, part := range c.Parts {
			if part != nil && part.Text != "" {
				return part.Text
			}
		}
	}
	return ""
}
