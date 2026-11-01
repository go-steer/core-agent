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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agent/internal/toolcalls"
)

// SubagentMode selects the ADK execution contract for a subagent (#1401).
type SubagentMode string

const (
	// SubagentModeChat is the default multi-turn conversational mode.
	SubagentModeChat SubagentMode = "chat"

	// SubagentModeTask runs the subagent in ADK's llmagent.ModeTask: a
	// multi-turn tool loop that terminates by calling the runtime-injected
	// finish_task tool and returns a structured (or string) output without
	// transferring control away from the parent.
	SubagentModeTask SubagentMode = "task"

	// SubagentModeSingleTurn runs the subagent in ADK's
	// llmagent.ModeSingleTurn: one LLM call with no tools, no subagents,
	// and optional OutputSchema validation on the reply.
	SubagentModeSingleTurn SubagentMode = "single_turn"
)

// WithSubagentMode sets the ADK execution mode (chat, task, single_turn)
// for this agent. Unset or SubagentModeChat preserves the default chat
// contract.
func WithSubagentMode(m SubagentMode) Option {
	return func(o *options) { o.subagentMode = m }
}

// WithOutputSchema constrains a SubagentModeTask or SubagentModeSingleTurn
// agent's terminal output to s. Passing a non-nil schema on a chat-mode
// agent returns an error from New.
func WithOutputSchema(s *genai.Schema) Option {
	return func(o *options) { o.outputSchema = s }
}

// WithBeforeModelCallbacks registers ADK BeforeModelCallbacks on the
// underlying llmagent.
func WithBeforeModelCallbacks(cbs ...llmagent.BeforeModelCallback) Option {
	return func(o *options) { o.beforeModelCallbacks = append(o.beforeModelCallbacks, cbs...) }
}

// WithAfterModelCallbacks registers ADK AfterModelCallbacks on the
// underlying llmagent.
func WithAfterModelCallbacks(cbs ...llmagent.AfterModelCallback) Option {
	return func(o *options) { o.afterModelCallbacks = append(o.afterModelCallbacks, cbs...) }
}

// WithDisallowTransfer configures DisallowTransferToParent and
// DisallowTransferToPeers on the underlying llmagent. SubagentModeTask and
// SubagentModeSingleTurn force both true automatically.
func WithDisallowTransfer(toParent, toPeers bool) Option {
	return func(o *options) {
		o.disallowTransferSet = true
		o.disallowTransferToParent = toParent
		o.disallowTransferToPeers = toPeers
	}
}

// SubagentMode reports the ADK execution contract configured via
// WithSubagentMode (default SubagentModeChat).
func (a *Agent) SubagentMode() SubagentMode {
	if a == nil || a.subagentMode == "" {
		return SubagentModeChat
	}
	return a.subagentMode
}

// OutputSchema returns the output schema configured via WithOutputSchema,
// or nil when unset.
func (a *Agent) OutputSchema() *genai.Schema {
	if a == nil {
		return nil
	}
	return a.outputSchema
}

// DefaultChatInstruction is the fallback system prompt for a Chat-mode
// coordinator when its caller leaves Instruction empty.
const DefaultChatInstruction = "You are a helpful AI assistant. Answer the user's request clearly and concisely. " +
	"When specialist sub-agents or tools are available and the request falls in their domain, delegate to them and synthesize their findings."

// DefaultTaskInstruction is the fallback system prompt for a Task-mode
// specialist when its caller leaves Instruction empty.
const DefaultTaskInstruction = "Complete the delegated task using the available tools, then call finish_task with a clear, self-contained result for the caller."

// DefaultSingleTurnInstruction is the fallback system prompt for a
// SingleTurn-mode specialist when its caller leaves Instruction empty.
const DefaultSingleTurnInstruction = "Respond to the request in a single reply."

func modeInstructionProvider(explicit, fallback string) llmagent.InstructionProvider {
	text := explicit
	if strings.TrimSpace(text) == "" {
		text = fallback
	}
	return func(_ adkagent.ReadonlyContext) (string, error) {
		return text, nil
	}
}

// TaskAgentConfig configures a ModeTask specialist — a bounded sub-agent
// that runs a multi-turn tool loop, reports its result through the
// auto-installed finish_task tool, and never takes over the conversation.
type TaskAgentConfig struct {
	Name        string
	Description string
	Instruction string
	Model       adkmodel.LLM

	Tools        []tool.Tool
	Toolsets     []tool.Toolset
	SubAgents    []adkagent.Agent
	OutputSchema *genai.Schema
	OutputKey    string

	BeforeModelCallbacks []llmagent.BeforeModelCallback
	AfterModelCallbacks  []llmagent.AfterModelCallback
}

// NewTaskAgent builds an llmagent in ModeTask with transfers disabled.
func NewTaskAgent(cfg TaskAgentConfig) (adkagent.Agent, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("agent: TaskAgent %q requires a Model", cfg.Name)
	}
	return llmagent.New(llmagent.Config{
		Name:                     cfg.Name,
		Description:              cfg.Description,
		Model:                    cfg.Model,
		Mode:                     llmagent.ModeTask,
		InstructionProvider:      modeInstructionProvider(cfg.Instruction, DefaultTaskInstruction),
		Tools:                    cfg.Tools,
		Toolsets:                 cfg.Toolsets,
		SubAgents:                cfg.SubAgents,
		OutputSchema:             cfg.OutputSchema,
		OutputKey:                cfg.OutputKey,
		DisallowTransferToParent: true,
		DisallowTransferToPeers:  true,
		BeforeModelCallbacks:     gated(cfg.Name, cfg.OutputSchema, true, cfg.BeforeModelCallbacks),
		AfterModelCallbacks:      cfg.AfterModelCallbacks,
	})
}

// SingleTurnAgentConfig configures a ModeSingleTurn specialist — a pure
// one-shot classifier, extractor or router with no tools, no sub-agents
// and no iteration loop.
type SingleTurnAgentConfig struct {
	Name        string
	Description string
	Instruction string
	Model       adkmodel.LLM

	OutputSchema *genai.Schema
	OutputKey    string

	BeforeModelCallbacks []llmagent.BeforeModelCallback
	AfterModelCallbacks  []llmagent.AfterModelCallback
}

// NewSingleTurnAgent builds an llmagent in ModeSingleTurn with transfers
// disabled.
func NewSingleTurnAgent(cfg SingleTurnAgentConfig) (adkagent.Agent, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("agent: SingleTurnAgent %q requires a Model", cfg.Name)
	}
	return llmagent.New(llmagent.Config{
		Name:                     cfg.Name,
		Description:              cfg.Description,
		Model:                    cfg.Model,
		Mode:                     llmagent.ModeSingleTurn,
		InstructionProvider:      modeInstructionProvider(cfg.Instruction, DefaultSingleTurnInstruction),
		OutputSchema:             cfg.OutputSchema,
		OutputKey:                cfg.OutputKey,
		DisallowTransferToParent: true,
		DisallowTransferToPeers:  true,
		BeforeModelCallbacks:     gated(cfg.Name, cfg.OutputSchema, false, cfg.BeforeModelCallbacks),
		AfterModelCallbacks:      cfg.AfterModelCallbacks,
	})
}

// CoordinatorConfig configures the root Chat-mode agent that owns the user
// conversation and delegates bounded work to ModeTask / ModeSingleTurn
// sub-agents.
type CoordinatorConfig struct {
	Name        string
	Description string
	Instruction string
	Model       adkmodel.LLM

	Tools     []tool.Tool
	Toolsets  []tool.Toolset
	SubAgents []adkagent.Agent

	BeforeModelCallbacks []llmagent.BeforeModelCallback
	AfterModelCallbacks  []llmagent.AfterModelCallback
}

// NewCoordinator builds the root Chat-mode llmagent.
func NewCoordinator(cfg CoordinatorConfig) (adkagent.Agent, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("agent: Coordinator %q requires a Model", cfg.Name)
	}
	return llmagent.New(llmagent.Config{
		Name:                 cfg.Name,
		Description:          cfg.Description,
		Model:                cfg.Model,
		Mode:                 llmagent.ModeChat,
		InstructionProvider:  modeInstructionProvider(cfg.Instruction, DefaultChatInstruction),
		Tools:                cfg.Tools,
		Toolsets:             cfg.Toolsets,
		SubAgents:            cfg.SubAgents,
		BeforeModelCallbacks: gated(cfg.Name, nil, false, cfg.BeforeModelCallbacks),
		AfterModelCallbacks:  cfg.AfterModelCallbacks,
	})
}

// FinishTaskToolName is the name under which ADK's ModeTask installs its
// completion tool.
const FinishTaskToolName = "finish_task"

// StallMarker prefixes every synthetic completion FinishOnStall writes.
const StallMarker = "[stalled: specialist ended its turn without calling finish_task]"

// StallInstruction is the parent-facing guidance that accompanies a
// stalled delegation result.
const StallInstruction = "Treat this delegation as incomplete. Do not re-invoke this specialist with the same " +
	"request; either answer the question below from other evidence, delegate a narrower check to a different " +
	"specialist, or note the gap in your response."

// Stalled reports whether s carries StallMarker.
func Stalled(s string) bool {
	return strings.Contains(s, StallMarker)
}

// StallPayload builds the finish_task arguments FinishOnStall injects when
// agentName ends a turn with no tool call.
type StallPayload func(agentName, lastWords string) map[string]any

// DefaultStallPayload is the StallPayload for a Task agent with no
// OutputSchema, where ADK's finish_task takes `{"result": <string>}`.
func DefaultStallPayload(agentName, lastWords string) map[string]any {
	return map[string]any{"result": StallText(agentName, lastWords)}
}

// StallText formats the stall notice for agentName around lastWords.
func StallText(agentName, lastWords string) string {
	if lastWords == "" {
		return fmt.Sprintf("%s\n\n%s\n\nSpecialist %q produced no final report and made no tool call on its last turn.",
			StallMarker, StallInstruction, agentName)
	}
	return fmt.Sprintf("%s\n\n%s\n\nSpecialist %q stopped without reporting and said:\n%s",
		StallMarker, StallInstruction, agentName, lastWords)
}

// FinishOnStall returns an AfterModelCallback that closes the delegation when
// a ModeTask agent ends a turn with no function call. If payload is nil,
// DefaultStallPayload is used.
func FinishOnStall(agentName string, payload StallPayload) llmagent.AfterModelCallback {
	if payload == nil {
		payload = DefaultStallPayload
	}
	return func(_ adkagent.Context, resp *adkmodel.LLMResponse, respErr error) (*adkmodel.LLMResponse, error) {
		if respErr != nil || resp == nil || resp.Partial || resp.Content == nil {
			return nil, nil
		}
		if resp.TurnComplete && hasFunctionCall(resp.Content) {
			return nil, nil
		}
		if !resp.TurnComplete && resp.FinishReason != genai.FinishReasonStop {
			return nil, nil
		}
		if hasFunctionCall(resp.Content) {
			return nil, nil
		}

		args := payload(agentName, nonThoughtText(resp.Content))
		parts := make([]*genai.Part, 0, len(resp.Content.Parts)+1)
		parts = append(parts, resp.Content.Parts...)
		parts = append(parts, genai.NewPartFromFunctionCall(FinishTaskToolName, args))

		out := *resp
		out.Content = &genai.Content{
			Role:  resp.Content.Role,
			Parts: parts,
		}
		return &out, nil
	}
}

func hasFunctionCall(c *genai.Content) bool {
	for _, p := range c.Parts {
		if p != nil && p.FunctionCall != nil {
			return true
		}
	}
	return false
}

func nonThoughtText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p == nil || p.Thought || p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p.Text)
	}
	return strings.TrimSpace(b.String())
}

// RefusalMarker prefixes every synthetic response RefuseOnGate writes.
const RefusalMarker = "[refused: model call blocked before reaching the provider]"

// RefusalInstruction is the caller-facing guidance that accompanies a
// refused delegation.
const RefusalInstruction = "This agent was stopped at its call boundary before the request was sent. " +
	"Do not re-delegate to it; synthesize from the findings already in hand or note the gap."

// Refused reports whether s carries RefusalMarker.
func Refused(s string) bool {
	return strings.Contains(s, RefusalMarker)
}

// RefusalText formats the refusal notice for agentName around reason.
func RefusalText(agentName, reason string) string {
	return fmt.Sprintf("%s\n\n%s\n\nAgent %q was not called: %s",
		RefusalMarker, RefusalInstruction, agentName, reason)
}

// CallGate decides whether an agent may make its next model call.
type CallGate interface {
	Allow(agentName string) error
}

type callGateKey struct{}

// WithCallGate attaches gate to ctx so every agent constructed with
// NewCoordinator, NewTaskAgent, NewSingleTurnAgent, or New consults it
// before each model call. Passing a nil gate returns ctx unchanged.
func WithCallGate(ctx context.Context, gate CallGate) context.Context {
	if gate == nil {
		return ctx
	}
	return context.WithValue(ctx, callGateKey{}, gate)
}

// CallGateFrom returns the CallGate attached to ctx, or nil if none was set.
func CallGateFrom(ctx context.Context) CallGate {
	if ctx == nil {
		return nil
	}
	g, _ := ctx.Value(callGateKey{}).(CallGate)
	return g
}

// RefusalPayload builds the finish_task arguments RefuseOnGate injects when a
// ModeTask agent is refused before its model call.
type RefusalPayload func(agentName, reason string) map[string]any

// DefaultRefusalPayload is the RefusalPayload for a ModeTask agent with no
// OutputSchema, where ADK's finish_task takes `{"result": <string>}`.
func DefaultRefusalPayload(agentName, reason string) map[string]any {
	return map[string]any{"result": RefusalText(agentName, reason)}
}

// UnreportableRefusal is the RefusalPayload for a ModeTask agent whose
// OutputSchema cannot honestly represent a refusal.
func UnreportableRefusal(_, _ string) map[string]any { return nil }

// RefuseOnGate returns a BeforeModelCallback that consults CallGateFrom(ctx)
// and short-circuits the model call when the gate refuses.
func RefuseOnGate(agentName string, taskMode bool, payload RefusalPayload) llmagent.BeforeModelCallback {
	if payload == nil {
		payload = DefaultRefusalPayload
	}
	return func(ctx adkagent.Context, req *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
		gate := CallGateFrom(ctx)
		if gate == nil {
			return nil, nil
		}
		err := gate.Allow(agentName)
		if err == nil {
			return nil, nil
		}
		reason := err.Error()

		if taskMode {
			if frg, ok := gate.(FinalReportGate); ok && frg.AllowFinalReport(agentName) {
				stripToFinalReport(req, reason)
				return nil, nil
			}
			if args := payload(agentName, reason); args != nil {
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{genai.NewPartFromFunctionCall(FinishTaskToolName, args)},
					},
					TurnComplete: true,
					FinishReason: genai.FinishReasonStop,
				}, nil
			}
		}

		return &adkmodel.LLMResponse{
			Content:      genai.NewContentFromText(RefusalText(agentName, reason), genai.RoleModel),
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}, nil
	}
}

func gated(agentName string, schema *genai.Schema, taskMode bool, caller []llmagent.BeforeModelCallback) []llmagent.BeforeModelCallback {
	var payload RefusalPayload
	if schema != nil {
		payload = UnreportableRefusal
	}
	out := make([]llmagent.BeforeModelCallback, 0, 1+len(caller))
	out = append(out, RefuseOnGate(agentName, taskMode, payload))
	out = append(out, caller...)
	return out
}

// FinalReportInstruction is the user-role nudge appended to a refused
// specialist's last request when the gate grants it one more call to report.
const FinalReportInstruction = "You have reached your call limit for this delegation (%s) and your investigation " +
	"tools have been removed. Call " + FinishTaskToolName + " now to report what you have already found from the " +
	"evidence above. If an aspect of the request was not yet checked, say so in the report rather than guessing."

// FinalReportGate is an optional extension of CallGate for ModeTask
// specialists that have already gathered evidence when the ceiling hits.
type FinalReportGate interface {
	CallGate
	AllowFinalReport(agentName string) bool
}

func stripToFinalReport(req *adkmodel.LLMRequest, reason string) {
	if req == nil {
		return
	}
	if req.Tools != nil {
		if ft, ok := req.Tools[FinishTaskToolName]; ok {
			req.Tools = map[string]any{FinishTaskToolName: ft}
		} else {
			req.Tools = nil
		}
	}
	if req.Config != nil {
		var kept []*genai.FunctionDeclaration
		for _, t := range req.Config.Tools {
			if t == nil {
				continue
			}
			for _, fd := range t.FunctionDeclarations {
				if fd != nil && fd.Name == FinishTaskToolName {
					kept = append(kept, fd)
				}
			}
		}
		if len(kept) > 0 {
			req.Config.Tools = []*genai.Tool{{FunctionDeclarations: kept}}
		} else {
			req.Config.Tools = nil
		}
	}
	nudge := fmt.Sprintf(FinalReportInstruction, reason)
	req.Contents = append(req.Contents, genai.NewContentFromText(nudge, genai.RoleUser))
}

// newSubagentDispatcher wraps a ModeTask or ModeSingleTurn adkagent.Agent
// in a single-node workflowagent so ADK's runner.New accepts it as a
// runner root. ADK's runner requires a root LlmAgent to be in ModeChat,
// whereas workflowagent.New has no such restriction and delegates directly
// to workflow.RunNode.
//
// Passing nil as the node input causes RunLLMAgentAsNode to consume the
// user message runner.Run already appended to the session, avoiding a
// duplicate user event in the subsession.
func newSubagentDispatcher(name string, sp adkagent.Agent) (adkagent.Agent, error) {
	if sp == nil {
		return nil, fmt.Errorf("agent: subagent %q is nil", name)
	}
	spNode, err := workflow.NewAgentNode(sp, workflow.NodeConfig{})
	if err != nil {
		return nil, err
	}
	dyn := workflow.NewDynamicNode[any, any](name+"_dispatch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (any, error) {
			return workflow.RunNode[any](ctx, spNode, nil)
		}, workflow.NodeConfig{})
	return workflowagent.New(workflowagent.Config{
		Name:        name + "_runner",
		Description: "Single-subagent workflow wrapper for " + name + ".",
		Edges:       workflow.Chain(workflow.Start, dyn),
		SubAgents:   []adkagent.Agent{sp},
	})
}

func buildInnerLLMAgent(o *options, model adkmodel.LLM, instruction string) (inner, runnerRoot adkagent.Agent, err error) {
	switch o.subagentMode {
	case "", SubagentModeChat:
		if o.outputSchema != nil {
			return nil, nil, fmt.Errorf("agent: OutputSchema requires SubagentModeTask or SubagentModeSingleTurn")
		}
	case SubagentModeTask:
	case SubagentModeSingleTurn:
		if len(o.tools) > 0 || len(o.toolsets) > 0 || len(o.subagents) > 0 {
			return nil, nil, fmt.Errorf("agent: SubagentModeSingleTurn allows no tools, toolsets, or subagents")
		}
	default:
		return nil, nil, fmt.Errorf("agent: unknown SubagentMode %q", o.subagentMode)
	}

	cfg := llmagent.Config{
		Name:                o.name,
		Model:               model,
		Description:         o.description,
		Tools:               o.tools,
		Toolsets:            o.toolsets,
		OutputSchema:        o.outputSchema,
		AfterModelCallbacks: o.afterModelCallbacks,
	}
	switch o.subagentMode {
	case SubagentModeTask:
		cfg.Mode = llmagent.ModeTask
		cfg.DisallowTransferToParent = true
		cfg.DisallowTransferToPeers = true
		cfg.BeforeModelCallbacks = gated(o.name, o.outputSchema, true, o.beforeModelCallbacks)
		if o.outputSchema == nil && len(o.afterModelCallbacks) == 0 {
			cfg.AfterModelCallbacks = []llmagent.AfterModelCallback{FinishOnStall(o.name, nil)}
		}
	case SubagentModeSingleTurn:
		cfg.Mode = llmagent.ModeSingleTurn
		cfg.DisallowTransferToParent = true
		cfg.DisallowTransferToPeers = true
		cfg.BeforeModelCallbacks = gated(o.name, o.outputSchema, false, o.beforeModelCallbacks)
	default:
		cfg.BeforeModelCallbacks = o.beforeModelCallbacks
		if o.disallowTransferSet {
			cfg.DisallowTransferToParent = o.disallowTransferToParent
			cfg.DisallowTransferToPeers = o.disallowTransferToPeers
		}
	}
	// Left nil when there is no prompt, which is not the same as a
	// provider that returns "". ADK's Instruction branch skips an empty
	// string; the provider branch does not, so setting it
	// unconditionally would append an empty system part where a
	// consumer previously got no system instruction at all — reachable
	// via WithInstruction("") or a whitespace-only --system-prompt-file.
	// The Anthropic adapter drops empty parts; Gemini forwards them.
	if instruction != "" {
		cfg.InstructionProvider = literalInstruction(instruction)
	}
	inner, err = llmagent.New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("agent: build llmagent: %w", err)
	}
	runnerRoot = inner
	if o.subagentMode == SubagentModeTask || o.subagentMode == SubagentModeSingleTurn {
		runnerRoot, err = newSubagentDispatcher(o.name, inner)
		if err != nil {
			return nil, nil, fmt.Errorf("agent: wrap subagent dispatcher: %w", err)
		}
	}
	return inner, runnerRoot, nil
}

// subagentTaskResult is the tool return value for a SubagentModeTask or
// SubagentModeSingleTurn subagent tool call (#1401). When the subagent
// produces a structured object (via OutputSchema), Output carries the
// validated map and Result carries its JSON serialization for callers
// that read Result.
type subagentTaskResult struct {
	Result         string           `json:"result"`
	Output         map[string]any   `json:"output,omitempty"`
	Calls          []toolcalls.Call `json:"calls,omitempty"`
	CallsTruncated int              `json:"calls_truncated,omitempty"`
	CallsNote      string           `json:"calls_note,omitempty"`
}

func (r subagentTaskResult) withCalls(rec *toolcalls.Recorder) subagentTaskResult {
	r.Calls, r.CallsTruncated = rec.Calls(), rec.Dropped()
	r.CallsNote = toolcalls.Note(len(r.Calls), r.CallsTruncated)
	return r
}

func formatTaskOutput(lastOutput any, fallbackText string, hasSchema bool) (string, map[string]any) {
	switch v := lastOutput.(type) {
	case map[string]any:
		raw, err := json.Marshal(v)
		if err != nil {
			return fallbackText, v
		}
		return string(raw), v
	case string:
		if hasSchema {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(v), &parsed); err == nil && parsed != nil {
				return v, parsed
			}
		}
		return v, nil
	default:
		if lastOutput != nil {
			if raw, err := json.Marshal(lastOutput); err == nil {
				var parsed map[string]any
				if json.Unmarshal(raw, &parsed) == nil && parsed != nil {
					return string(raw), parsed
				}
				return string(raw), nil
			}
		}
		return fallbackText, nil
	}
}

var supportedSchemaTypes = map[genai.Type]bool{
	genai.TypeString:  true,
	genai.TypeInteger: true,
	genai.TypeNumber:  true,
	genai.TypeBoolean: true,
	genai.TypeArray:   true,
	genai.TypeObject:  true,
}

// ParseOutputSchema parses an inline JSON Schema string ("{...}") or loads
// a .json schema file resolved against baseDirs, normalizes type names to
// uppercase genai.Type constants, and validates that every node is supported
// by ADK's output validator (#1401).
func ParseOutputSchema(raw string, baseDirs ...string) (*genai.Schema, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	var data []byte
	if strings.HasPrefix(trimmed, "{") {
		data = []byte(trimmed)
	} else {
		path := resolveSchemaPath(trimmed, baseDirs)
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("output_schema %q: %w", raw, err)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var schema genai.Schema
	if err := dec.Decode(&schema); err != nil {
		return nil, fmt.Errorf("output_schema: decode: %w", err)
	}
	normalizeOutputSchema(&schema)
	if err := checkOutputSchema(&schema, "$"); err != nil {
		return nil, fmt.Errorf("output_schema: %w", err)
	}
	return &schema, nil
}

func resolveSchemaPath(ref string, baseDirs []string) string {
	if filepath.IsAbs(ref) {
		return ref
	}
	fromSlash := filepath.FromSlash(ref)
	for _, base := range baseDirs {
		if base == "" {
			continue
		}
		cand := filepath.Join(base, fromSlash)
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
		if filepath.Base(base) == "subagents" && !strings.HasPrefix(filepath.ToSlash(ref), "../") {
			alt := filepath.Join(filepath.Dir(base), fromSlash)
			if _, err := os.Stat(alt); err == nil {
				return alt
			}
		}
	}
	if len(baseDirs) > 0 && baseDirs[0] != "" {
		return filepath.Join(baseDirs[0], fromSlash)
	}
	return fromSlash
}

func normalizeOutputSchema(s *genai.Schema) {
	if s == nil {
		return
	}
	s.Type = genai.Type(strings.ToUpper(string(s.Type)))
	normalizeOutputSchema(s.Items)
	for _, p := range s.Properties {
		normalizeOutputSchema(p)
	}
	for _, a := range s.AnyOf {
		normalizeOutputSchema(a)
	}
}

func checkOutputSchema(s *genai.Schema, path string) error {
	if s == nil {
		return fmt.Errorf("%s: null schema", path)
	}
	if s.Type == "" {
		return fmt.Errorf("%s: missing type", path)
	}
	if !supportedSchemaTypes[s.Type] {
		return fmt.Errorf("%s: type %q is not one of object, array, string, integer, number, boolean", path, s.Type)
	}
	if path == "$" && s.Type != genai.TypeObject {
		return fmt.Errorf("$: top-level type must be object, got %q", s.Type)
	}
	switch s.Type {
	case genai.TypeArray:
		if s.Items == nil {
			return fmt.Errorf("%s: array has no items schema", path)
		}
		if err := checkOutputSchema(s.Items, path+"[]"); err != nil {
			return err
		}
	case genai.TypeObject:
		if len(s.Properties) == 0 {
			return fmt.Errorf("%s: object has no properties", path)
		}
		keys := make([]string, 0, len(s.Properties))
		for k := range s.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, name := range keys {
			if err := checkOutputSchema(s.Properties[name], path+"."+name); err != nil {
				return err
			}
		}
		for _, req := range s.Required {
			if _, ok := s.Properties[req]; !ok {
				return fmt.Errorf("%s: required property %q is not in properties (%s)", path, req, strings.Join(keys, ", "))
			}
		}
	}
	return nil
}
