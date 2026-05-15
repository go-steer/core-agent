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
	streaming      adkagent.StreamingMode
	appName        string
	userID         string
	sessionID      string
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

	// TODO(subagents): a future WithSubagents([]*Agent) Option will
	// register each subagent as a synthetic tool whose handler invokes
	// the subagent's runner. Plumb through here when that lands.
}

func defaultOptions() options {
	return options{
		appName:     DefaultAppName,
		name:        "core_agent",
		description: "core-agent conversational agent",
		instruction: "You are a helpful assistant. Be concise and accurate.",
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

	svc := o.sessionService
	if svc == nil {
		svc = session.InMemoryService()
	}
	r, err := runner.New(runner.Config{
		AppName:           o.appName,
		Agent:             inner,
		SessionService:    svc,
		AutoCreateSession: true,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: build runner: %w", err)
	}

	return &Agent{
		inner:          inner,
		runner:         r,
		sessionService: svc,
		eventLog:       o.eventLog,
		streaming:      o.streaming,
		appName:        o.appName,
		userID:         o.userID,
		sessionID:      o.sessionID,
	}, nil
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
func (a *Agent) Run(ctx context.Context, prompt string) iter.Seq2[*session.Event, error] {
	msg := genai.NewContentFromText(prompt, genai.RoleUser)
	return a.runner.Run(ctx, a.userID, a.sessionID, msg, adkagent.RunConfig{
		StreamingMode: a.streaming,
	})
}
