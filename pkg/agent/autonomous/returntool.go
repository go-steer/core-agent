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

package autonomous

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
)

// DefaultReturnToolName is the name WithReturnTool uses when
// ReturnToolConfig.Name is empty.
const DefaultReturnToolName = "return_result"

const defaultReturnToolDescription = "Return your result to the agent that delegated this task, and finish. " +
	"The result argument is the ONLY thing the delegating agent is guaranteed to receive: write the " +
	"answer, the root-cause analysis, the proposed change, or the specific reason you stopped, with " +
	"the evidence behind it. A status line like \"investigated the issue and found the cause\" forces " +
	"the delegating agent to redo the work, because it cannot see what you did."

// ReturnToolConfig replaces the driver's lifecycle-style done tool
// with a result-style one: a single `result` argument that is the
// value handed back to the caller, plus any number of alias names
// that funnel to the same signal.
//
// Why this exists (#728). The stock done tool is a lifecycle status
// emitter — `report_done` was introduced in docs/autonomous-plan.md as
// one of the "report_done / set_status lifecycle tools", and its
// payload field is called `detail` because pkg/tools.NewLifecycleTool
// documents it as "an optional short human-readable note". That
// framing is right for a status emission and wrong for a delegation's
// return value, and it produced exactly the failure it describes:
// content-free reports the delegating agent had to re-derive.
//
// Aliases exist because a subagent's namespace accumulated three
// near-synonymous names — report_done, report_completed and (on the
// parent only) mark_task_done — of which one terminated the loop, one
// did not, and one was not registered at all. A model that reaches for
// any of them should succeed rather than have to guess which is real.
type ReturnToolConfig struct {
	// Name is the primary tool name. Empty means
	// DefaultReturnToolName.
	Name string

	// Aliases are additional tool names wired to the same signal.
	// Duplicates of Name, and of each other, are dropped. Empty
	// entries are dropped.
	Aliases []string

	// Description overrides the prose shown to the model on every
	// registered name. Empty falls back to a default that states the
	// return contract.
	Description string
}

// WithReturnTool switches the driver from the lifecycle-style done
// tool (`report_done(state, detail)`) to a result-style return tool
// (`return_result(result)`) plus optional aliases.
//
// Opt-in: consumers that don't set it keep the lifecycle done tool
// unchanged, so WithDoneToolName / WithDoneToolDescription and every
// existing `report_done` prompt keep working exactly as before. The
// in-tree background subagent path sets it; nothing else does.
func WithReturnTool(rc ReturnToolConfig) Option {
	return func(c *autoConfig) {
		cp := rc
		cp.Aliases = append([]string(nil), rc.Aliases...)
		c.returnTool = &cp
	}
}

// returnArgs is the JSON shape the PRIMARY return tool presents: one
// field, which is the value handed back.
type returnArgs struct {
	Result string `json:"result" jsonschema:"your findings: the answer, analysis, or proposed change, with the evidence behind it"`
}

// legacyReturnArgs is the shape the ALIAS names present. It additionally
// accepts the lifecycle-shaped `report_done(state:"done", detail:"...")`
// call, because that is what every prompt, doc and example written
// against the old tool tells a model to emit — and ADK's function tools
// validate with additionalProperties:false, so an unexpected `state`
// key is a hard validation error, not an ignored field. An alias that
// rejected the exact call shape it exists to catch would be worse than
// no alias at all.
//
// Detail is read only when Result is empty.
type legacyReturnArgs struct {
	Result string `json:"result,omitempty" jsonschema:"your findings: the answer, analysis, or proposed change, with the evidence behind it"`
	Detail string `json:"detail,omitempty" jsonschema:"deprecated alias for result; prefer result"`
	State  string `json:"state,omitempty" jsonschema:"deprecated and ignored; present only so older report_done-shaped calls validate"`
}

type returnAck struct {
	Ack string `json:"ack"`
}

const emptyReturnAck = "rejected: result is required — write your actual findings, not a status line"

// buildDoneTools builds the tools whose invocation ends the run,
// signalling through doneCh. Shared by Run and Resume so a resumed run
// offers the model the identical termination gesture (they drifted
// apart once already).
//
// Returns the lifecycle-style done tool unless WithReturnTool was set,
// in which case it returns the result-style return tool followed by
// one tool per alias.
func buildDoneTools(cfg *autoConfig, doneCh chan string) ([]tool.Tool, error) {
	signal := func(detail string) {
		select {
		case doneCh <- detail:
		default:
			// Already signalled; treat the second call as a no-op
			// rather than blocking the tool handler.
		}
	}

	// A bounded delegation terminates by running out of work, so there
	// is no done tool to register and nothing for the model to choose
	// between (#730).
	if cfg.stopOnNaturalEnd {
		return nil, nil
	}

	if cfg.returnTool == nil {
		t, err := coretools.NewLifecycleTool(coretools.LifecycleOptions{
			Name:          cfg.doneToolName,
			Description:   cfg.doneToolDescription,
			AllowedStates: []string{"done"},
			Handler: func(_ context.Context, ev coretools.LifecycleEvent) error {
				signal(ev.Detail)
				return nil
			},
		})
		if err != nil {
			return nil, err
		}
		return []tool.Tool{t}, nil
	}

	desc := strings.TrimSpace(cfg.returnTool.Description)
	if desc == "" {
		desc = defaultReturnToolDescription
	}
	// deliver signals when result is non-empty. An empty return is the
	// failure this tool exists to prevent, and the caller receives
	// nothing either way — so spend one tool call telling the model
	// what is missing rather than ending the run empty-handed. Bounded
	// by the run's turn and step budgets, and it mirrors
	// NewLifecycleTool's existing "rejected: ..." handling of a missing
	// required field.
	deliver := func(result string) returnAck {
		if result = strings.TrimSpace(result); result == "" {
			return returnAck{Ack: emptyReturnAck}
		}
		signal(result)
		return returnAck{Ack: "ok"}
	}
	primaryFn := func(_ tool.Context, args returnArgs) (returnAck, error) {
		return deliver(args.Result), nil
	}
	aliasFn := func(_ tool.Context, args legacyReturnArgs) (returnAck, error) {
		if strings.TrimSpace(args.Result) != "" {
			return deliver(args.Result), nil
		}
		return deliver(args.Detail), nil
	}

	primary := cfg.returnToolName()
	out := make([]tool.Tool, 0, 1+len(cfg.returnTool.Aliases))
	seen := make(map[string]struct{}, 1+len(cfg.returnTool.Aliases))
	for _, name := range append([]string{primary}, cfg.returnTool.Aliases...) {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		var (
			t   tool.Tool
			err error
		)
		cfgt := functiontool.Config{Name: name, Description: desc}
		if name == primary {
			t, err = functiontool.New(cfgt, primaryFn)
		} else {
			t, err = functiontool.New(cfgt, aliasFn)
		}
		if err != nil {
			return nil, fmt.Errorf("build return tool %q: %w", name, err)
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("build return tool: no usable tool name")
	}
	return out, nil
}

// doneToolNames lists every name the model can end the run through:
// the lifecycle tool's single name on the legacy path, or the primary
// plus every alias on the result path. Used by the in-turn cost bound
// to recognize the one call it must not cut off (#729).
func (c *autoConfig) doneToolNames() []string {
	if c.stopOnNaturalEnd {
		// No done tool is registered, so no call can be one.
		return nil
	}
	if c.returnTool == nil {
		return []string{c.doneToolName}
	}
	out := []string{c.returnToolName()}
	for _, a := range c.returnTool.Aliases {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// returnToolName resolves the primary return-tool name, preferring an
// explicit WithDoneToolName override so the existing collision escape
// hatch keeps working on the result-style path too.
func (c *autoConfig) returnToolName() string {
	if c.returnTool != nil && strings.TrimSpace(c.returnTool.Name) != "" {
		return strings.TrimSpace(c.returnTool.Name)
	}
	if c.doneToolNameExplicit {
		return c.doneToolName
	}
	return DefaultReturnToolName
}
