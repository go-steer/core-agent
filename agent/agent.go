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

// Package agent wraps the Google ADK runner with sensible defaults
// (streaming mode, in-memory session service, app name) so consumers
// hit the same shape regardless of whether they're driving the agent
// from a one-shot CLI, a REPL, or an HTTP handler.
//
// Multi-turn conversation history is preserved automatically when
// Run() is called repeatedly with the same userID + sessionID — by
// default ADK's session.InMemoryService accumulates events. Pass
// WithSessionService to plug in a durable backend (e.g. an
// eventlog-backed Service for SQLite/Postgres persistence + audit
// log + crash-resume).
package agent

import (
	"context"
	"fmt"
	"iter"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/eventlog"
)

// DefaultAppName tags this process in the ADK runner. Telemetry and
// session stores key off this; override with WithAppName when embedding
// in a host that wants its own identity.
const DefaultAppName = "core-agent"

// DefaultInstruction is the system instruction applied to every agent
// that doesn't override it via WithInstruction. Comprises a baseline
// helpfulness/concision directive plus a parallelism mandate adapted
// from google-gemini/gemini-cli's prompt patterns
// (packages/core/src/prompts/snippets.ts).
//
// The parallelism mandate is load-bearing for Gemini, which otherwise
// emits one tool call per assistant turn even when independent
// operations are obviously batchable. Direct measurement
// (dev/parallel-probe/) shows Gemini-3.1-pro-preview-customtools
// without this instruction never batched across 65 search turns;
// Claude is less affected but still benefits marginally.
//
// Exported so consumers building on WithInstruction can reuse the
// baseline + mandate verbatim: `agent.WithInstruction(agent.DefaultInstruction + "\n\n" + extra)`.
const DefaultInstruction = `You are a helpful assistant. Be concise and accurate.

Tools execute in parallel by default. Execute multiple independent tool calls in parallel when feasible — searching, reading files, independent shell commands, or editing different files. When investigating code, if you need to read multiple files or grep multiple directories, issue all the tool calls in a single response; do not execute them one by one.`

const (
	defaultUserID    = "local"
	defaultSessionID = "default"
)

// Agent is the wrapper around an ADK llmagent + runner. One Agent
// represents one configured LLM-driven role.
type Agent struct {
	inner          adkagent.Agent
	runner         *runner.Runner
	sessionService session.Service
	eventLog       *eventlog.Handle
	tools          []tool.Tool
	streaming      adkagent.StreamingMode
	appName        string
	agentName      string
	userID         string
	sessionID      string
	bgMgr          *BackgroundAgentManager
	inbox          *inbox
}

// Option mutates Agent construction. Use the With* helpers below.
type Option func(*options)

type options struct {
	appName        string
	name           string
	description    string
	instruction    string
	streaming      adkagent.StreamingMode
	userID         string
	sessionID      string
	tools          []tool.Tool
	toolsets       []tool.Toolset
	sessionService session.Service
	eventLog       *eventlog.Handle
	subagents      []*Agent
	bgMgr          *BackgroundAgentManager
}

func defaultOptions() options {
	return options{
		appName:     DefaultAppName,
		name:        "core_agent",
		description: "core-agent conversational agent",
		instruction: DefaultInstruction,
		streaming:   adkagent.StreamingModeSSE,
		userID:      defaultUserID,
		sessionID:   defaultSessionID,
	}
}

// WithAppName overrides the AppName handed to the ADK runner. Useful
// when embedding so telemetry and session stores can distinguish
// multiple agents inside one binary.
func WithAppName(s string) Option { return func(o *options) { o.appName = s } }

// WithName overrides the agent's display name (visible in OTEL spans).
func WithName(s string) Option { return func(o *options) { o.name = s } }

// WithDescription overrides the agent's description.
func WithDescription(s string) Option { return func(o *options) { o.description = s } }

// WithInstruction overrides the system instruction.
func WithInstruction(s string) Option { return func(o *options) { o.instruction = s } }

// WithStreaming overrides the streaming mode. Default is StreamingModeSSE
// (required to receive Partial events).
func WithStreaming(m adkagent.StreamingMode) Option {
	return func(o *options) { o.streaming = m }
}

// WithSession overrides the user/session IDs handed to the ADK runner.
// Reuse the same pair across Run() calls to preserve conversation history.
func WithSession(userID, sessionID string) Option {
	return func(o *options) { o.userID = userID; o.sessionID = sessionID }
}

// WithTools registers a set of tools the agent may call. Order is
// preserved but immaterial; ADK keys tools by Name.
func WithTools(ts []tool.Tool) Option {
	return func(o *options) { o.tools = append(o.tools, ts...) }
}

// WithToolsets registers groups of tools (MCP servers, skills, etc.).
// Each Toolset implements google.golang.org/adk/tool.Toolset and is
// passed to llmagent.Config.Toolsets.
func WithToolsets(ts []tool.Toolset) Option {
	return func(o *options) { o.toolsets = append(o.toolsets, ts...) }
}

// WithSessionService overrides the session.Service handed to the ADK
// runner. The default is session.InMemoryService(), which loses all
// state when the process exits. Pass a durable Service (typically the
// one returned by eventlog.Open(...).Service when wiring the audit
// log + crash-resume substrate) to persist sessions across runs.
//
// The supplied Service is also exposed via Agent.SessionService() so
// callers can query session state directly without keeping their own
// reference. Passing nil restores the default.
func WithSessionService(s session.Service) Option {
	return func(o *options) { o.sessionService = s }
}

// WithEventLog wires an eventlog.Handle into the agent — the Handle's
// Service becomes the agent's session.Service (so every event lands
// in the durable log), and the Handle is stored on the agent so
// callers can reach back to it for replay/watch via
// Agent.EventLog().
//
// Equivalent to WithSessionService(h.Service) plus a stash of the
// Handle for later access; passing nil is a no-op.
func WithEventLog(h *eventlog.Handle) Option {
	return func(o *options) {
		if h == nil {
			return
		}
		o.sessionService = h.Service
		o.eventLog = h
	}
}

// WithSubagents registers each agent as a callable tool the parent's
// model can invoke by name. The subagent runs through ADK's runner
// using the parent's session.Service (so its events stream live into
// the same audit log) with session.Event.Branch set to
// "<parent_branch>.<subagent_name>" — ADK's contents-processor
// branch filter then keeps the subagent's events from leaking back
// into the parent's next-turn LLM request, which preserves context
// isolation while keeping the audit log unified.
//
// Each subagent's tool name comes from its own WithName value. Use
// NewSubagentTool directly for per-subagent overrides (custom name,
// description, depth cap, branch label).
//
// Resolved at the end of New() so that the parent's session.Service
// and session triple — set by other With* options — are captured
// at the point the subagent tools are constructed.
func WithSubagents(agents []*Agent) Option {
	return func(o *options) { o.subagents = append(o.subagents, agents...) }
}

// WithBackgroundManager attaches a BackgroundAgentManager to the
// agent. The manager's parent back-reference is set during
// construction so its Spawn calls can read the agent's session
// triple + session.Service without the consumer plumbing them twice.
//
// Each turn of Agent.Run drains pending alerts from the manager's
// channel (non-blocking) and prepends them to the prompt the
// underlying ADK runner sees, so the parent's model is aware of
// what its background subagents have reported since the last turn.
//
// Pass nil to clear (e.g. for tests that re-construct an agent).
func WithBackgroundManager(mgr *BackgroundAgentManager) Option {
	return func(o *options) { o.bgMgr = mgr }
}

// WithSystemInstructionPrefix prepends prefix to the agent's default
// instruction. Used for memory loading: AGENTS.md / CLAUDE.md /
// GEMINI.md project memory becomes part of the system prompt rather
// than the user's first message.
func WithSystemInstructionPrefix(prefix string) Option {
	return func(o *options) {
		if prefix == "" {
			return
		}
		if o.instruction == "" {
			o.instruction = prefix
			return
		}
		o.instruction = prefix + "\n\n" + o.instruction
	}
}

// New constructs an Agent backed by model. Returns a clear error if the
// underlying ADK constructors reject the configuration.
func New(model adkmodel.LLM, opts ...Option) (*Agent, error) {
	if model == nil {
		return nil, fmt.Errorf("agent: model is required")
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}

	// Resolve subagents into tools. Done after all options are
	// applied so each subagent tool captures the parent's final
	// session.Service + (app, user, session) triple — the values
	// the parent will be constructed with on the next line. The
	// subagent's events then land in the parent's session row,
	// branch-isolated.
	parentSvc := o.sessionService
	if parentSvc == nil {
		parentSvc = session.InMemoryService()
		o.sessionService = parentSvc
	}
	for _, sa := range o.subagents {
		if sa == nil {
			continue
		}
		st, err := NewSubagentTool(SubagentOptions{
			Inner:           sa,
			ParentService:   parentSvc,
			ParentAppName:   o.appName,
			ParentUserID:    o.userID,
			ParentSessionID: o.sessionID,
		})
		if err != nil {
			return nil, fmt.Errorf("agent: WithSubagents: %w", err)
		}
		o.tools = append(o.tools, st)
	}

	inner, err := llmagent.New(llmagent.Config{
		Name:        o.name,
		Model:       model,
		Description: o.description,
		Instruction: o.instruction,
		Tools:       o.tools,
		Toolsets:    o.toolsets,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: build llmagent: %w", err)
	}

	// o.sessionService was guaranteed non-nil by the subagent
	// resolution block above (which materializes the default
	// in-memory service when no other was wired).
	svc := o.sessionService
	r, err := runner.New(runner.Config{
		AppName:           o.appName,
		Agent:             inner,
		SessionService:    svc,
		AutoCreateSession: true,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: build runner: %w", err)
	}

	a := &Agent{
		inner:          inner,
		runner:         r,
		sessionService: svc,
		eventLog:       o.eventLog,
		tools:          o.tools,
		streaming:      o.streaming,
		appName:        o.appName,
		agentName:      o.name,
		userID:         o.userID,
		sessionID:      o.sessionID,
		bgMgr:          o.bgMgr,
		inbox:          newInbox(),
	}
	if a.bgMgr != nil {
		a.bgMgr.attachParent(a)
	}
	return a, nil
}

// Tools returns the resolved tool list the agent was constructed
// with — including any subagent tools materialized by WithSubagents.
// Useful for diagnostics ("does my parent know about the research
// subagent?") without introspecting ADK internals.
func (a *Agent) Tools() []tool.Tool {
	if a == nil {
		return nil
	}
	out := make([]tool.Tool, len(a.tools))
	copy(out, a.tools)
	return out
}

// AppName returns the AppName the agent was constructed with (the
// value passed to runner.Config). Used by callers that need to
// identify the session triple (app, user, session) for queries
// against the event log or session.Service.
func (a *Agent) AppName() string { return a.appName }

// UserID returns the user identifier the agent was constructed with.
func (a *Agent) UserID() string { return a.userID }

// SessionID returns the session identifier the agent was constructed
// with. Combined with AppName + UserID this is the key the event log
// uses to scope ForSession queries.
func (a *Agent) SessionID() string { return a.sessionID }

// SessionService returns the session.Service backing this agent. When
// no WithSessionService option was passed at construction this is the
// default in-memory service. Useful for callers that want to query
// session state directly (e.g. listing prior events) without keeping
// their own reference to the Service they passed in.
func (a *Agent) SessionService() session.Service { return a.sessionService }

// EventLog returns the *eventlog.Handle the agent was constructed
// with via WithEventLog, or nil when no event log was wired. Use to
// reach back to Stream.Since / Stream.Watch for replay or live tail
// without keeping a separate reference.
func (a *Agent) EventLog() *eventlog.Handle { return a.eventLog }

// Run executes one turn of the agent against prompt and returns the event
// iterator straight from ADK's runner. Callers are expected to range over
// the returned iter.Seq2 and consume events as they arrive — partial text
// chunks, tool calls, and the final TurnComplete event.
//
// Multi-turn use: call Run() repeatedly on the same Agent. The configured
// session ID is reused across calls, so the ADK accumulates conversation
// history automatically.
//
// When a BackgroundAgentManager is wired via WithBackgroundManager,
// any alerts background subagents have emitted since the last turn
// are drained (non-blocking) and prepended to the prompt so the
// parent's model sees them before deciding what to do next.
//
// Inbox messages queued via Agent.Inject from external callers
// (harness, orchestrator, HTTP handler) are also drained and
// prepended, sibling to the alerts block. Ordering: alerts go
// first (internal state changes); inbox goes second (external
// input, closer to the prompt logically); then the original prompt.
func (a *Agent) Run(ctx context.Context, prompt string) iter.Seq2[*session.Event, error] {
	if a.bgMgr != nil {
		prompt = a.bgMgr.PrependPendingAlerts(prompt)
	}
	if a.inbox != nil {
		prompt = prependInboxMessages(prompt, a.inbox.drain())
	}
	msg := genai.NewContentFromText(prompt, genai.RoleUser)
	return a.runner.Run(ctx, a.userID, a.sessionID, msg, adkagent.RunConfig{
		StreamingMode: a.streaming,
	})
}

// BackgroundManager returns the BackgroundAgentManager the agent was
// constructed with via WithBackgroundManager, or nil when none was
// wired. Used by spawn tools + the runner's REPL alert display to
// reach the manager without keeping a separate reference.
func (a *Agent) BackgroundManager() *BackgroundAgentManager {
	if a == nil {
		return nil
	}
	return a.bgMgr
}
