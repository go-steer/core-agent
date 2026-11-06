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
	"fmt"
	"strings"

	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agent/internal/subsession"
	"github.com/go-steer/core-agent/v3/pkg/models"
)

// SubagentAttachInput is the attach / inherit item name that appends the
// parent turn's triggering message verbatim to a delegated subagent's
// request or goal without re-typing it (#1499).
const SubagentAttachInput = "input"

type parentInputKey struct{}

// WithParentInput returns a derived context carrying the parent turn's
// triggering message so synchronous and asynchronous subagent calls made
// during the turn can splice it verbatim without the parent model
// transcribing it into tool arguments (#1499).
func WithParentInput(ctx context.Context, input string) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, parentInputKey{}, strings.TrimSpace(input))
}

// ParentInputFromContext returns the parent turn's triggering message
// stamped on ctx via WithParentInput, or "" when none is present.
func ParentInputFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(parentInputKey{}).(string)
	return v
}

// SpliceParentInput appends parentInput verbatim to prompt, separated by a
// blank line when both are non-empty. When parentInput is empty or
// whitespace-only, prompt is returned unchanged; when prompt is empty or
// whitespace-only, parentInput is returned directly.
func SpliceParentInput(prompt, parentInput string) string {
	parentInput = strings.TrimSpace(parentInput)
	if parentInput == "" {
		return prompt
	}
	if strings.TrimSpace(prompt) == "" {
		return parentInput
	}
	return strings.TrimRight(prompt, "\n") + "\n\n" + parentInput
}

// ValidateSubagentAttach validates the per-call attach slice on a subagent
// tool or spawn_agent call and reports whether SubagentAttachInput ("input")
// was requested (#1499). Any unrecognized item returns an error.
func ValidateSubagentAttach(attach []string) (includeInput bool, err error) {
	for _, raw := range attach {
		item := strings.TrimSpace(raw)
		switch item {
		case SubagentAttachInput:
			includeInput = true
		default:
			return false, fmt.Errorf("unknown attach item %q (want %q)", raw, SubagentAttachInput)
		}
	}
	return includeInput, nil
}

// WithSubagentInheritInput configures whether this agent, when exposed as a
// subagent tool via WithSubagents, automatically appends the parent turn's
// triggering message verbatim to every request (#1499).
func WithSubagentInheritInput(inherit bool) Option {
	return func(o *options) { o.subagentInheritInput = inherit }
}

// SubagentInheritInput reports whether this agent was configured with
// WithSubagentInheritInput(true). Nil-safe.
func (a *Agent) SubagentInheritInput() bool {
	return a != nil && a.subagentInheritInput
}

// turnTriggeringInput extracts the triggering user message for a turn from
// the pre-turn pipeline state (before alerts and watchdog feedback are
// prepended) or from an explicit *genai.Content message.
func turnTriggeringInput(tp *turnPrep, msg *genai.Content) string {
	if tp != nil && !tp.hasContent {
		var parts []string
		for i, m := range tp.drained.texts {
			if i < len(tp.drained.senders) && tp.drained.senders[i] == AutoContinueOriginator {
				continue
			}
			if t := strings.TrimSpace(m); t != "" {
				parts = append(parts, t)
			}
		}
		if raw := strings.TrimSpace(tp.rawPrompt); raw != "" {
			parts = append(parts, raw)
		}
		return strings.Join(parts, "\n\n")
	}
	if msg != nil && msg.Role == genai.RoleUser {
		var parts []string
		for _, p := range msg.Parts {
			if p == nil || p.Thought {
				continue
			}
			if t := strings.TrimSpace(p.Text); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// withTurnParentInput stamps the current turn's triggering message onto ctx
// (falling back to the last non-empty triggering message on wake-only
// continuation turns) so subagents invoked during the turn can attach it.
func (a *Agent) withTurnParentInput(ctx context.Context, tp *turnPrep, msg *genai.Content) context.Context {
	in := turnTriggeringInput(tp, msg)
	if a != nil {
		a.lastTriggerInputMu.Lock()
		if in != "" {
			a.lastTriggerInput = in
		} else {
			in = a.lastTriggerInput
		}
		a.lastTriggerInputMu.Unlock()
	}
	if in != "" {
		return WithParentInput(ctx, in)
	}
	return ctx
}

func resolveSubagentRequest(ctx context.Context, name string, args subagentArgs, opts SubagentOptions) (string, error) {
	attachInput, err := ValidateSubagentAttach(args.Attach)
	if err != nil {
		return "", fmt.Errorf("subagent %q: %w", name, err)
	}
	if attachInput || args.IncludeInput || opts.InheritInput || opts.Inner.SubagentInheritInput() {
		return SpliceParentInput(args.Request, ParentInputFromContext(ctx)), nil
	}
	return args.Request, nil
}

func buildSubagentChildContext(toolCtx context.Context, name, request string) (context.Context, *models.PriorSuccess) {
	// Push the new depth into the context value chain so any further
	// subagent calls from inside this one see the incremented count.
	childCtx := subsession.WithDepth(toolCtx, subsession.CurrentDepth(toolCtx)+1)
	// Record which subagent we're inside so a spawn_agent call made
	// from in here can't re-launch this same subagent asynchronously (#732).
	childCtx = subsession.WithLineage(childCtx, name)
	// Stamp this subagent's own request as the triggering input for any
	// nested delegation it makes (#1499).
	childCtx = WithParentInput(childCtx, request)
	// The parent's served calls say nothing about this subagent's own
	// instruction and tools, so the delegation gets its own record (#1247).
	served := models.NewPriorSuccess()
	childCtx = models.WithPriorSuccess(childCtx, served)
	return childCtx, served
}
