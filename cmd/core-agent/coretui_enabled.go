// Copyright 2026 The go-steer team
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
	"iter"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/permissions"
)

// pkgCoreElicitor mirrors pkgElicitor (the internal/tui variant) for
// the core-tui code path. Set lazily by makeMCPElicitor when
// CORE_AGENT_TUI=core-tui is active; consumed by launchTUIv2 so the
// same instance the MCP servers were wired against actually receives
// each elicit through the TUI.
var pkgCoreElicitor coretui.Elicitor

// availableModelIDs is the hardcoded Gemini 3.x candidate list the
// /model picker surfaces. Mirrors internal/tui/model_picker.go's
// availableModels() — kept duplicate here rather than promoted to a
// public function on agent.Agent because it's pure UI policy. When
// cogo grows a real model catalog we'll promote.
func availableModelIDs() []string {
	return []string{
		"gemini-3.1-pro-preview-customtools",
		"gemini-3.1-pro-preview",
		"gemini-3.5-flash",
		"gemini-3-flash-preview",
		"gemini-3.1-flash-lite-preview",
		"gemini-3.1-flash-image-preview",
		"gemini-2.5-pro",
		"gemini-2.5-flash",
	}
}

// launchTUIv2 is the core-tui-backed alternative to launchTUI. Same
// inputs, same return contract; differs only in which TUI library
// drives the operator surface. Picked at runtime by CORE_AGENT_TUI=
// core-tui (see main.go). While both code paths coexist (PRs 6-9 of
// docs/core-tui-adapter-design.md), this lets operators A/B the two
// and stick on either until the migration settles.
func launchTUIv2(ctx context.Context, deps tuiDeps) (didRun bool, exitCode int, err error) {
	a, err := agent.New(deps.Model, deps.AgentOpts...)
	if err != nil {
		return false, 0, fmt.Errorf("agent.New: %w", err)
	}

	prompter := coretui.NewPrompter()
	// Wrap so the gate sees a permissions.Prompter (its expected
	// interface) while the TUI drains a coretui.PermissionPrompter.
	deps.Gate.SetPrompter(&gatePrompterBridge{inner: prompter})

	// pkgCoreElicitor was set by makeMCPElicitor (called earlier in
	// main.go before mcp.Build). When unset (CORE_AGENT_TUI wasn't
	// set at MCP-build time) construct an unwired one so the TUI
	// still has something to drain; elicits land in the bit-bucket.
	elicitor := pkgCoreElicitor
	if elicitor == nil {
		elicitor = coretui.NewElicitor()
	}

	wrapped := &coreAgentAdapter{
		inner:    a,
		deps:     deps,
		ctxBuild: ctx,
	}

	opts := coretui.Options{
		Agent:    wrapped,
		Prompter: prompter,
		Elicitor: elicitor,
		PermissionMode: coretui.PermissionModeWiring{
			Initial: translateMode(deps.Gate.Mode()),
			Set: func(m coretui.PermissionMode) error {
				deps.Gate.SetMode(translateModeBack(m))
				return nil
			},
		},
		// Default queueing mode matches core-agent's existing UX —
		// operator types during streaming, inbox auto-continues at
		// turn end. Flip to QueueForNext for the buffer-only flow.
		MidTurnInjectionMode: coretui.InjectIntoCurrent,
	}

	if err := coretui.Run(ctx, opts); err != nil {
		return true, 1, err
	}
	return true, 0, nil
}

// coreAgentAdapter wraps *agent.Agent so it satisfies core-tui's
// tui.Agent plus every optional capability interface core-agent can
// support. Built incrementally — capability methods are listed
// below in spec order.
type coreAgentAdapter struct {
	inner    *agent.Agent
	deps     tuiDeps
	ctxBuild context.Context
}

// Run satisfies coretui.Agent. Translates each *session.Event from
// the ADK iterator into a coretui.Event.
func (a *coreAgentAdapter) Run(ctx context.Context, prompt string) iter.Seq2[coretui.Event, error] {
	return func(yield func(coretui.Event, error) bool) {
		for ev, err := range a.inner.Run(ctx, prompt) {
			if err != nil {
				yield(coretui.Event{}, err)
				return
			}
			te := coretui.Event{Partial: ev.Partial}
			if ev.UsageMetadata != nil {
				te.Usage = &coretui.Usage{
					InputTokens:  int(ev.UsageMetadata.PromptTokenCount),
					OutputTokens: int(ev.UsageMetadata.CandidatesTokenCount),
				}
			}
			if ev.Content != nil {
				for _, p := range ev.Content.Parts {
					if p.FunctionCall != nil {
						te.ToolCalls = append(te.ToolCalls, coretui.ToolCall{
							ID:   p.FunctionCall.ID,
							Name: p.FunctionCall.Name,
							Args: p.FunctionCall.Args,
						})
					}
					if p.Text != "" {
						te.Text += p.Text
					}
				}
			}
			if !yield(te, nil) {
				return
			}
		}
	}
}

// Interrupt satisfies coretui.Interruptible.
func (a *coreAgentAdapter) Interrupt() bool { return a.inner.Interrupt() }

// Inject satisfies coretui.InjectableAgent (R-CHAT-11).
func (a *coreAgentAdapter) Inject(message string) error { return a.inner.Inject(message) }

// WakeRequested satisfies coretui.WakeRequester (R-WAKE-1).
func (a *coreAgentAdapter) WakeRequested() <-chan struct{} { return a.inner.WakeRequested() }

// AvailableModels satisfies coretui.ModelSwapper. Returns the
// hardcoded Gemini 3.x catalog (see availableModelIDs comment).
func (a *coreAgentAdapter) AvailableModels() []coretui.ModelInfo {
	ids := availableModelIDs()
	out := make([]coretui.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, coretui.ModelInfo{ID: id, Display: id})
	}
	return out
}

// SwitchModel satisfies coretui.ModelSwapper. Resolves the new
// model through the host's provider and rebuilds the agent with
// the same agent opts.
func (a *coreAgentAdapter) SwitchModel(modelID string) (coretui.Agent, error) {
	newLLM, err := a.deps.Provider.Model(a.ctxBuild, modelID)
	if err != nil {
		return nil, err
	}
	newAgent, err := agent.New(newLLM, a.deps.AgentOpts...)
	if err != nil {
		return nil, err
	}
	return &coreAgentAdapter{
		inner:    newAgent,
		deps:     a.deps,
		ctxBuild: a.ctxBuild,
	}, nil
}

// SessionApprovals satisfies coretui.PermissionController. Maps the
// gate's ApprovalLog slice 1:1 into the core-tui shape.
func (a *coreAgentAdapter) SessionApprovals() []coretui.ApprovalLog {
	src := a.deps.Gate.Approvals()
	out := make([]coretui.ApprovalLog, 0, len(src))
	for _, ap := range src {
		out = append(out, coretui.ApprovalLog{
			Tool:     ap.Tool,
			Key:      ap.Key,
			Decision: ap.Decision.String(),
		})
	}
	return out
}

// AddAllowPatterns satisfies coretui.PermissionController.
func (a *coreAgentAdapter) AddAllowPatterns(patterns []string) error {
	return a.deps.Gate.AddAllowPatterns(patterns)
}

// AddDenyPatterns satisfies coretui.PermissionController.
func (a *coreAgentAdapter) AddDenyPatterns(patterns []string) error {
	return a.deps.Gate.AddDenyPatterns(patterns)
}

// AddBuiltinAllowExtra satisfies coretui.PermissionController.
func (a *coreAgentAdapter) AddBuiltinAllowExtra(bundleName string) error {
	entries, ok := permissions.Bundles[bundleName]
	if !ok {
		return fmt.Errorf("unknown bundle %q", bundleName)
	}
	return a.deps.Gate.AddAllowPatterns(entries)
}

// SlashCommands satisfies coretui.SlashProvider. Surfaces /btw and
// /subagent to the palette + /help.
func (a *coreAgentAdapter) SlashCommands() []coretui.SlashCommandSpec {
	return []coretui.SlashCommandSpec{
		{
			Name:        "btw",
			Aliases:     []string{"by-the-way"},
			Description: "ask a side question (modal, no tool, doesn't land in history)",
		},
		{
			Name:        "subagent",
			Aliases:     []string{"sub"},
			Description: "spawn a background sub-agent: /subagent <goal> [--name=X --tools=Y --max-turns=N]",
		},
	}
}

// InvokeSlash satisfies coretui.SlashProvider. /btw calls
// AskSideQuestion + surfaces the answer through a SideAnswer modal;
// /subagent parses flags and spawns through BackgroundManager.
func (a *coreAgentAdapter) InvokeSlash(ctx context.Context, name, args string) (coretui.SlashResult, error) {
	switch name {
	case "btw", "by-the-way":
		answer, err := a.inner.AskSideQuestion(ctx, args)
		if err != nil {
			return coretui.SlashResult{
				ModalAnswer: &coretui.SideAnswer{Question: args, Err: err},
			}, nil
		}
		return coretui.SlashResult{
			ModalAnswer: &coretui.SideAnswer{Question: args, Answer: answer},
		}, nil
	case "subagent", "sub":
		// Full flag parsing lives in the original /subagent handler
		// (internal/tui/subagent.go); a follow-up PR lifts that logic
		// here. For now, point the operator at the in-process flow.
		return coretui.SlashResult{
			SystemMessage: "/subagent requires the internal/tui flag parser — not yet lifted into the core-tui adapter. Use CORE_AGENT_TUI=internal to drive subagent spawn for now.",
		}, nil
	}
	return coretui.SlashResult{}, fmt.Errorf("unknown slash: %s", name)
}

// gatePrompterBridge adapts a core-tui PermissionPrompter so it
// satisfies permissions.Prompter (the gate's expected interface).
// Translates PromptKind / Decision values across the two enum
// vocabularies.
type gatePrompterBridge struct {
	inner coretui.PermissionPrompter
}

// AskApproval implements permissions.Prompter by delegating to the
// core-tui prompter after translating the request shape.
func (g *gatePrompterBridge) AskApproval(ctx context.Context, req permissions.PromptRequest) (permissions.Decision, error) {
	cReq := coretui.PermissionRequest{
		Kind:        translateKind(req.Kind),
		ToolName:    req.ToolName,
		Detail:      req.Detail,
		DetailKind:  translateDetailKind(req.Kind),
		Verb:        req.Verb,
		Source:      req.Source,
		PersistTool: req.PersistTool,
		PersistKey:  req.PersistKey,
	}
	cDec, err := g.inner.AskApproval(ctx, cReq)
	if err != nil {
		return permissions.DecisionDeny, err
	}
	return translateDecision(cDec), nil
}

// translateKind maps permissions.PromptKind → coretui.PermissionKind.
// Four-to-four mapping with PathScope folded into Edit (both are
// file-access events from the operator's perspective).
func translateKind(k permissions.PromptKind) coretui.PermissionKind {
	switch k {
	case permissions.PromptKindBash:
		return coretui.PermissionKindBash
	case permissions.PromptKindFileWrite, permissions.PromptKindPathScope:
		return coretui.PermissionKindEdit
	default:
		return coretui.PermissionKindOther
	}
}

// translateDetailKind picks the right Glamour code-fence language
// tag for the modal body based on the request Kind. The host has
// already rendered req.Detail; this is just the styling hint.
func translateDetailKind(k permissions.PromptKind) coretui.DetailKind {
	switch k {
	case permissions.PromptKindBash:
		return coretui.DetailShell
	case permissions.PromptKindFileWrite:
		return coretui.DetailDiff
	default:
		return coretui.DetailPlain
	}
}

// translateDecision maps coretui.PermissionDecision → permissions.Decision.
// One-to-one because the spec for both adopted the same R-PERM-2
// vocabulary.
func translateDecision(d coretui.PermissionDecision) permissions.Decision {
	switch d {
	case coretui.DecisionAllowOnce:
		return permissions.DecisionAllowOnce
	case coretui.DecisionAllowSession:
		return permissions.DecisionAllowSession
	case coretui.DecisionAllowSessionVerb:
		return permissions.DecisionAllowSessionVerb
	case coretui.DecisionAllowSessionTool:
		return permissions.DecisionAllowSessionTool
	case coretui.DecisionAllowAlways:
		return permissions.DecisionAllowAlways
	default:
		return permissions.DecisionDeny
	}
}

// translateMode / translateModeBack bridge the gate's three-valued
// string Mode (ask / allow / yolo) and core-tui's four-valued
// PermissionMode enum (default / acceptEdits / plan / bypass).
//
// Mappings:
//
//   ask    ↔ default
//   allow  ↔ acceptEdits  (closest semantic — gate auto-allows
//                          everything not explicitly denied; core-
//                          tui's acceptEdits is "edit tools auto-
//                          allow, everything else still asks")
//   yolo   ↔ bypass       (one-to-one)
//
// core-tui's `plan` has no core-agent equivalent — the chip can
// display it, but flipping to plan leaves the gate on `ask`. A
// future core-agent ModePlan would close the gap.
func translateMode(m permissions.Mode) coretui.PermissionMode {
	switch m {
	case permissions.ModeAllow:
		return coretui.PermissionModeAcceptEdits
	case permissions.ModeYolo:
		return coretui.PermissionModeBypass
	default:
		return coretui.PermissionModeDefault
	}
}

func translateModeBack(m coretui.PermissionMode) permissions.Mode {
	switch m {
	case coretui.PermissionModeAcceptEdits:
		return permissions.ModeAllow
	case coretui.PermissionModeBypass:
		return permissions.ModeYolo
	default:
		return permissions.ModeAsk
	}
}

// coreMCPElicitor wraps a coretui.Elicitor as an mcp.ElicitorFn so
// the MCP servers can route their elicit requests through the
// shared core-tui modal. Translates between the MCP SDK's JSON-
// schema-shaped request and core-tui's flat field list.
type coreMCPElicitor struct {
	inner coretui.Elicitor
}

// elicit implements mcp.ElicitorFn.
func (c *coreMCPElicitor) elicit(ctx context.Context, serverName string, req *mcpsdk.ElicitRequest) (*mcpsdk.ElicitResult, error) {
	params := req.Params
	if params == nil {
		return &mcpsdk.ElicitResult{Action: "decline"}, nil
	}
	cReq, ok := translateMCPSchemaToElicitRequest(params)
	if !ok {
		return &mcpsdk.ElicitResult{Action: "decline"}, nil
	}
	result, err := c.inner.Elicit(ctx, serverName, cReq)
	if err != nil {
		return &mcpsdk.ElicitResult{Action: "cancel"}, err
	}
	out := &mcpsdk.ElicitResult{
		Action: translateElicitAction(result.Action),
	}
	if result.Action == coretui.ElicitActionSubmit {
		out.Content = result.Values
	}
	return out, nil
}

// translateMCPSchemaToElicitRequest flattens the SDK's JSON schema
// into core-tui's []ElicitField. Supports primitive types
// (string/number/integer/boolean) + enums; nested objects are
// declined (R-ELIC-3 — the second-return-false path drops the
// request server-side instead of opening a broken modal).
func translateMCPSchemaToElicitRequest(p *mcpsdk.ElicitParams) (coretui.ElicitRequest, bool) {
	out := coretui.ElicitRequest{
		Mode:        coretui.ElicitFormMode,
		Title:       p.Message,
		Description: p.Message,
	}
	schema, ok := p.RequestedSchema.(map[string]any)
	if !ok {
		return out, false
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return out, false
	}
	requiredSet := map[string]bool{}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				requiredSet[s] = true
			}
		}
	}
	for name, raw := range props {
		propMap, ok := raw.(map[string]any)
		if !ok {
			return out, false
		}
		typeName, _ := propMap["type"].(string)
		field := coretui.ElicitField{
			Name:        name,
			Description: stringOf(propMap, "description"),
			Required:    requiredSet[name],
		}
		switch typeName {
		case "string":
			if enum, ok := propMap["enum"].([]any); ok {
				field.Type = coretui.ElicitFieldEnum
				for _, e := range enum {
					if s, ok := e.(string); ok {
						field.EnumChoices = append(field.EnumChoices, s)
					}
				}
			} else {
				field.Type = coretui.ElicitFieldString
			}
		case "number":
			field.Type = coretui.ElicitFieldNumber
		case "integer":
			field.Type = coretui.ElicitFieldInteger
		case "boolean":
			field.Type = coretui.ElicitFieldBoolean
		default:
			return out, false // unsupported type
		}
		if d, ok := propMap["default"]; ok {
			field.Default = d
		}
		out.Fields = append(out.Fields, field)
	}
	return out, true
}

// stringOf is a tiny helper for pulling optional string fields out
// of a schema map — returns "" when missing or non-string.
func stringOf(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// translateElicitAction maps core-tui's ElicitAction back to the
// MCP SDK's stringy action vocabulary.
func translateElicitAction(a coretui.ElicitAction) string {
	switch a {
	case coretui.ElicitActionSubmit:
		return "accept"
	case coretui.ElicitActionDecline:
		return "decline"
	default:
		return "cancel"
	}
}

// (Touching `time` keeps the import live until the SubagentLister
// adapter lands; it uses StartedAt time.Time on entries.)
var _ = time.Now
