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
	"strings"
	"sync"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/agent/workflowagents/parallelagent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
)

// DefaultMaxConcurrency is the fan-out branch concurrency cap when a
// bundle leaves Fanout.MaxConcurrency unset (0).
const DefaultMaxConcurrency = 4

type analystBranch struct {
	name   string
	agent  adkagent.Agent
	budget SpecialistBudget
}

type fanoutFindings struct {
	reported []fanoutFinding
	silent   []string
}

type fanoutFinding struct {
	analyst string
	payload any
}

type branchSemaphores struct {
	limit int

	mu   sync.Mutex
	byID map[string]chan struct{}
}

func (s *branchSemaphores) acquire(ctx adkagent.Context) (release func(), ok bool) {
	if s == nil || s.limit <= 0 {
		return func() {}, true
	}
	s.mu.Lock()
	if s.byID == nil {
		s.byID = map[string]chan struct{}{}
	}
	id := ctx.InvocationID()
	sem, found := s.byID[id]
	if !found {
		sem = make(chan struct{}, s.limit)
		s.byID[id] = sem
	}
	s.mu.Unlock()

	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	case <-ctx.Done():
		return func() {}, false
	}
}

func (s *branchSemaphores) forget(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.byID, id)
	s.mu.Unlock()
}

func maxConcurrency(b Bundle) int {
	if b.Fanout.MaxConcurrency != 0 {
		return b.Fanout.MaxConcurrency
	}
	return DefaultMaxConcurrency
}

func buildFanout(b Bundle, analysts []analystBranch, synthesis adkagent.Agent, synthBudget SpecialistBudget) (adkagent.Agent, error) {
	if len(analysts) == 0 {
		return nil, fmt.Errorf("graph: workload %q has no fan-out analysts", b.Name)
	}
	if synthesis == nil {
		return nil, fmt.Errorf("graph: workload %q has no %q specialist", b.Name, SynthesisName)
	}

	order := make([]string, len(analysts))
	sems := &branchSemaphores{limit: maxConcurrency(b)}
	branches := make([]adkagent.Agent, 0, len(analysts))
	for i, a := range analysts {
		order[i] = a.name
		br, err := wrapAnalystBranch(a, sems)
		if err != nil {
			return nil, err
		}
		branches = append(branches, br)
	}

	fan, err := parallelagent.New(parallelagent.Config{AgentConfig: adkagent.Config{
		Name:        b.Name + "_fan",
		Description: "runs the analyst roster concurrently over one incident",
		SubAgents:   branches,
	}})
	if err != nil {
		return nil, fmt.Errorf("graph: build fan-out agent: %w", err)
	}
	fanNode, err := workflow.NewAgentNode(fan, workflow.NodeConfig{})
	if err != nil {
		return nil, fmt.Errorf("graph: wrap fan-out agent: %w", err)
	}

	fanOutNode := workflow.NewDynamicNode[any, *fanoutFindings]("fan_out",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (*fanoutFindings, error) {
			defer sems.forget(ctx.InvocationID())
			if _, err := workflow.RunNode[any](ctx, fanNode, incidentText(ctx)); err != nil {
				return nil, err
			}
			return collectFindings(ctx, order), nil
		}, workflow.NodeConfig{})

	synthNode, err := workflow.NewAgentNode(synthesis, nodeConfig(synthBudget))
	if err != nil {
		return nil, fmt.Errorf("graph: wrap %q: %w", SynthesisName, err)
	}
	runSynthesis := workflow.NewDynamicNode[*fanoutFindings, any]("run_"+SynthesisName,
		func(ctx adkagent.Context, findings *fanoutFindings, _ func(*session.Event) error) (any, error) {
			if findings == nil {
				findings = &fanoutFindings{}
			}
			result := map[string]any{
				"analysts": len(findings.reported) + len(findings.silent),
				"reported": len(findings.reported),
				"silent":   findings.silent,
			}
			if len(findings.reported) == 0 {
				result["report"] = ""
				return result, nil
			}
			report, err := workflow.RunNode[any](ctx, synthNode, synthesisPrompt(findings))
			if err != nil {
				return nil, err
			}
			result["report"] = report
			return result, nil
		}, workflow.NodeConfig{})

	return workflowagent.New(workflowagent.Config{
		Name:        b.Name + "_fanout",
		Description: b.Description,
		Edges:       workflow.Chain(workflow.Start, fanOutNode, runSynthesis),
		SubAgents:   []adkagent.Agent{fan, synthesis},
	})
}

func wrapAnalystBranch(a analystBranch, sems *branchSemaphores) (adkagent.Agent, error) {
	node, err := workflow.NewAgentNode(a.agent, nodeConfig(a.budget))
	if err != nil {
		return nil, fmt.Errorf("graph: wrap analyst %q: %w", a.name, err)
	}
	run := workflow.NewDynamicNode[any, any]("run_"+a.name,
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (any, error) {
			release, ok := sems.acquire(ctx)
			if !ok {
				return nil, ctx.Err()
			}
			defer release()
			return workflow.RunNode[any](ctx, node, incidentText(ctx), workflow.WithUseAsOutput())
		}, workflow.NodeConfig{})
	return workflowagent.New(workflowagent.Config{
		Name:        "branch_" + a.name,
		Description: a.agent.Description(),
		Edges:       workflow.Chain(workflow.Start, run),
		SubAgents:   []adkagent.Agent{a.agent},
	})
}

func nodeConfig(b SpecialistBudget) workflow.NodeConfig {
	var cfg workflow.NodeConfig
	if b.MaxWallclockSeconds > 0 {
		cfg.Timeout = time.Duration(b.MaxWallclockSeconds) * time.Second
	}
	return cfg
}

func incidentText(ctx adkagent.Context) string {
	uc := ctx.UserContent()
	if uc == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range uc.Parts {
		if p != nil {
			sb.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(sb.String())
}

func collectFindings(ctx adkagent.Context, order []string) *fanoutFindings {
	want := make(map[string]bool, len(order))
	for _, name := range order {
		want[name] = true
	}
	latest := make(map[string]any, len(order))
	for ev := range ctx.Session().Events().All() {
		if ev == nil || ev.Output == nil || ev.InvocationID != ctx.InvocationID() {
			continue
		}
		if want[ev.Author] {
			latest[ev.Author] = ev.Output
		}
	}
	out := &fanoutFindings{}
	for _, name := range order {
		payload, ok := latest[name]
		if !ok || payload == nil {
			out.silent = append(out.silent, name)
			continue
		}
		out.reported = append(out.reported, fanoutFinding{analyst: name, payload: payload})
	}
	return out
}

func synthesisPrompt(f *fanoutFindings) string {
	var sb strings.Builder
	sb.WriteString("Merge the analyst findings below into one report.\n")
	sb.WriteString("These payloads are the complete record: anything an analyst did\n")
	sb.WriteString("but did not return is not available to you.\n")
	for _, fi := range f.reported {
		fmt.Fprintf(&sb, "\n## %s\n%v\n", fi.analyst, fi.payload)
	}
	if len(f.silent) > 0 {
		fmt.Fprintf(&sb, "\nReturned no finding: %s\n", strings.Join(f.silent, ", "))
	}
	return sb.String()
}
