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
	"errors"
	"fmt"
	"strings"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

// SubagentOptions configures NewSubagentTool. Inner is required;
// everything else has sensible defaults.
type SubagentOptions struct {
	// Inner is the *agent.Agent to expose as a tool the parent's
	// model can call. The tool's function name comes from
	// Inner.AgentName() (set via agent.WithName), unless overridden
	// via Name. The tool's description comes from Inner's
	// llmagent.Description, unless overridden via Description.
	Inner *Agent

	// Name overrides the function name shown to the parent's model.
	// Empty falls back to Inner.AgentName().
	Name string

	// Description overrides the function description shown to the
	// parent's model. Empty falls back to Inner's Description (or a
	// generic fallback when that's also empty).
	Description string

	// MaxDepth caps recursion depth. A subagent at depth >= MaxDepth
	// that is invoked from another subagent gets an error result
	// rather than being allowed to recurse. Default 2; pass a
	// larger value if your agent topology genuinely needs deeper
	// nesting.
	MaxDepth int

	// Branch overrides the branch label appended to the parent's
	// branch on the subagent's events. Defaults to the tool name
	// (which is Inner.AgentName() unless Name overrides it). The
	// resulting branch is "<parent_branch>.<this>".
	Branch string

	// ParentService, when non-nil, overrides the session.Service
	// the subagent's runner uses. The agent.WithSubagents
	// convenience option fills this in automatically with the
	// parent agent's service so subagent events land in the
	// parent's audit log without any consumer plumbing.
	//
	// When nil, NewSubagentTool falls back to Inner.SessionService()
	// — which is fine for callers who construct subagents
	// pre-wired against the same Handle.
	ParentService session.Service

	// ParentAppName, ParentUserID, ParentSessionID identify the
	// parent's session triple. When set, the subagent runs through
	// the parent's session row (with branch isolation) so cross-
	// session audit queries find both. Empty values fall back to
	// Inner's own AppName/UserID/SessionID. Set automatically by
	// agent.WithSubagents.
	ParentAppName   string
	ParentUserID    string
	ParentSessionID string
}

const (
	defaultSubagentMaxDepth = 2
	defaultSubagentDesc     = "Run a focused subagent and return its result. Pass the request as a single string."
	branchSeparator         = "."
)

// subagentDepthKey carries the current subagent recursion depth
// through the context chain. Top-level callers see depth 0; each
// nested subagent invocation increments by one.
type subagentDepthKey struct{}

// CurrentSubagentDepth returns the recursion depth of the current
// subagent invocation. Zero when we're not inside a subagent (i.e.
// the parent's top-level turn).
func CurrentSubagentDepth(ctx context.Context) int {
	v, _ := ctx.Value(subagentDepthKey{}).(int)
	return v
}

// subagentArgs is the JSON shape the parent's model sees on every
// subagent tool call: a single "request" string carrying the task
// for the subagent.
type subagentArgs struct {
	Request string `json:"request" jsonschema:"the task for the subagent in plain language"`
}

// subagentResult is what comes back to the parent's model: the
// joined final text from the subagent's run, plus any error
// surfaced from the subagent runner.
type subagentResult struct {
	Result string `json:"result"`
}

// NewSubagentTool wraps an *agent.Agent as a tool the parent's
// model can call. The subagent runs through ADK's runner using the
// parent's session.Service (so its events stream live into the same
// audit log as the parent), with session.Event.Branch set to
// "<parent_branch>.<this>" so the audit log stays distinguishable
// and ADK's contents-processor branch filter keeps the subagent's
// events from leaking into the parent's next-turn LLM request.
//
// The parent's session.Service is captured from Inner — Inner is
// expected to have been constructed with the same WithEventLog (or
// WithSessionService) the parent uses. The agent.WithSubagents
// convenience option handles this wiring automatically; consumers
// who construct subagent tools directly via NewSubagentTool need to
// share the session.Service themselves.
func NewSubagentTool(opts SubagentOptions) (tool.Tool, error) {
	if opts.Inner == nil {
		return nil, errors.New("agent: NewSubagentTool: Inner is required")
	}
	if opts.Inner.inner == nil {
		return nil, errors.New("agent: NewSubagentTool: Inner has no underlying ADK agent")
	}

	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = opts.Inner.AgentName()
	}
	if name == "" {
		return nil, errors.New("agent: NewSubagentTool: subagent has no name (set via agent.WithName or SubagentOptions.Name)")
	}

	desc := strings.TrimSpace(opts.Description)
	if desc == "" {
		desc = opts.Inner.inner.Description()
	}
	if desc == "" {
		desc = defaultSubagentDesc
	}

	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultSubagentMaxDepth
	}

	branch := strings.TrimSpace(opts.Branch)
	if branch == "" {
		branch = name
	}

	parentService := opts.ParentService
	if parentService == nil {
		parentService = opts.Inner.SessionService()
	}
	if parentService == nil {
		return nil, errors.New("agent: NewSubagentTool: no session.Service available (set SubagentOptions.ParentService or construct Inner with WithEventLog / WithSessionService)")
	}
	innerAgent := opts.Inner.inner
	innerAppName := firstNonEmpty(opts.ParentAppName, opts.Inner.AppName())
	innerUserID := firstNonEmpty(opts.ParentUserID, opts.Inner.UserID())
	// The subagent runs in its own session row derived from the
	// parent's so two concurrent runners don't trip ADK's
	// stale-session optimistic-concurrency check. Events still land
	// in the same database — audit queries find the subagent via
	// WithBranchPrefix(branch) across sessions, or via the derived
	// session ID directly.
	parentSessionID := firstNonEmpty(opts.ParentSessionID, opts.Inner.SessionID())
	subagentSessionID := deriveSubagentSessionID(parentSessionID, branch)

	handler := func(toolCtx tool.Context, args subagentArgs) (subagentResult, error) {
		// tool.Context embeds agent.ReadonlyContext which embeds
		// context.Context, so we can read context values and pass
		// it to runner.Run directly.
		if depth := CurrentSubagentDepth(toolCtx); depth >= maxDepth {
			return subagentResult{
				Result: fmt.Sprintf("subagent %q refused: depth limit reached (%d)", name, maxDepth),
			}, nil
		}
		// Wrap the parent's session.Service so every event the
		// inner runner appends gets the right Branch before
		// landing in storage. ADK's contents-processor uses Branch
		// to decide which events show up in the LLM request — see
		// internal/llminternal/contents_processor.go in ADK.
		parentBranch := toolCtx.Branch()
		fullBranch := composeBranch(parentBranch, branch)
		wrapped := &branchInjectingService{
			inner:  parentService,
			branch: fullBranch,
		}

		// Build a fresh runner per invocation so concurrent
		// subagent calls (ADK dispatches function calls in
		// parallel goroutines) don't share mutable runner state.
		// The runner reads from the wrapped service, which
		// transparently writes to the parent's storage with our
		// Branch tag.
		r, err := runner.New(runner.Config{
			AppName:           innerAppName,
			Agent:             innerAgent,
			SessionService:    wrapped,
			AutoCreateSession: true,
		})
		if err != nil {
			return subagentResult{}, fmt.Errorf("subagent %q: build runner: %w", name, err)
		}

		// Push the new depth into the context value chain so any
		// further subagent calls from inside this one see the
		// incremented count.
		childCtx := context.WithValue(toolCtx, subagentDepthKey{}, CurrentSubagentDepth(toolCtx)+1)

		msg := genai.NewContentFromText(args.Request, genai.RoleUser)
		var sb strings.Builder
		for ev, err := range r.Run(childCtx, innerUserID, subagentSessionID, msg, adkagent.RunConfig{
			StreamingMode: opts.Inner.streaming,
		}) {
			if err != nil {
				return subagentResult{}, fmt.Errorf("subagent %q: run: %w", name, err)
			}
			collectFinalText(&sb, ev)
		}
		return subagentResult{Result: sb.String()}, nil
	}

	return functiontool.New(functiontool.Config{
		Name:        name,
		Description: desc,
	}, handler)
}

// composeBranch builds the full branch path for a subagent call:
// the parent's branch (possibly empty for top-level), joined with
// the subagent's own branch label by ADK's "." separator.
func composeBranch(parent, this string) string {
	parent = strings.TrimSpace(parent)
	this = strings.TrimSpace(this)
	switch {
	case parent == "" && this == "":
		return ""
	case parent == "":
		return this
	case this == "":
		return parent
	default:
		return parent + branchSeparator + this
	}
}

// collectFinalText walks one event's content and appends any final
// (non-partial) text parts to sb. We deliberately ignore partial
// text streams to avoid double-counting tokens — ADK emits both
// streaming partials and a consolidated TurnComplete event.
func collectFinalText(sb *strings.Builder, ev *session.Event) {
	if ev == nil || ev.Content == nil || ev.Partial {
		return
	}
	for _, p := range ev.Content.Parts {
		if p == nil || p.Text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(p.Text)
	}
}

// branchInjectingService wraps a session.Service so every appended
// event picks up our Branch label before landing in storage. The
// CRUD methods pass through unchanged. This is how a subagent's
// events end up tagged for the audit log without requiring the
// subagent's runner to know anything about branching.
type branchInjectingService struct {
	inner  session.Service
	branch string
}

func (s *branchInjectingService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	return s.inner.Create(ctx, req)
}

func (s *branchInjectingService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	return s.inner.Get(ctx, req)
}

func (s *branchInjectingService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	return s.inner.List(ctx, req)
}

func (s *branchInjectingService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	return s.inner.Delete(ctx, req)
}

// AppendEvent stamps Branch on the event before delegating. We only
// override an empty Branch — events that already carry one (e.g.,
// nested subagent invocations) keep their existing label so the
// branch hierarchy stays accurate.
func (s *branchInjectingService) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	if ev != nil && ev.Branch == "" {
		ev.Branch = s.branch
	}
	return s.inner.AppendEvent(ctx, sess, ev)
}

// AgentName returns the configured agent name (the WithName value)
// stored on construction. Used by NewSubagentTool to derive a
// default tool name.
func (a *Agent) AgentName() string {
	if a == nil {
		return ""
	}
	return a.agentName
}

// firstNonEmpty returns the first non-empty string from its
// arguments. Used in NewSubagentTool to pick parent-supplied
// session info over the subagent's own when WithSubagents wires
// the override.
func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// deriveSubagentSessionID composes the session ID the subagent's
// runner uses. Lives in the same database as the parent's session
// so audit queries can find both, but as a separate session row so
// ADK's per-session optimistic-concurrency check doesn't trip when
// the parent's outer runner resumes after the subagent finishes.
//
// Format: "<parent>:sub:<branch>". When parent is empty (consumer
// constructed NewSubagentTool standalone), the prefix is dropped.
func deriveSubagentSessionID(parent, branch string) string {
	if parent == "" {
		return "sub:" + branch
	}
	return parent + ":sub:" + branch
}
