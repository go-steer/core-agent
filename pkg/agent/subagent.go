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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent/internal/subsession"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/usage"
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

	// Gate, when non-nil, is consulted before the subagent runs — so
	// plan-first, the allow/deny policy and the ask prompt apply to
	// the act of delegating and not only to what the delegate does
	// once it is running (#758).
	//
	// The lookup is deliberately made under the SAME policy bucket the
	// asynchronous door uses: a declarative subagent is reachable both
	// as this tool and as `spawn_agent {agent: "<name>"}`, and an
	// operator who wrote `deny: ["spawn_agent:cluster"]` meant that
	// `cluster` does not run — not that it does not run through one of
	// the two doors. So the rule is matched as
	// `spawn_agent:<subagent-name>` whichever door the model picked.
	//
	// agent.WithSubagents fills this in from the parent's
	// agent.WithGate. Consumers calling NewSubagentTool directly pass
	// their own; nil leaves the tool ungated, which is what a host that
	// wired no gate anywhere already has everywhere else.
	Gate *permissions.Gate

	// ParentTracker is the usage.Tracker the DELEGATING agent bills
	// to. Every model turn the subagent takes is appended to it,
	// priced by the subagent's own model — so a delegated turn lands
	// in /usage, in /stats, and, most importantly, under the session
	// and per-turn cost ceilings.
	//
	// It has to be the parent's rather than Inner's, and it has to be
	// passed in rather than read off Inner, because a subagent
	// assembled declaratively is constructed with no tracker at all
	// (cmd/core-agent/subagents.go wires a model, a persona and a tool
	// surface, nothing else). Inner.Tracker() is nil in every
	// in-tree deployment.
	//
	// Why the tool has to do this itself: in library mode *Agent.Run
	// does not append usage — the harness consuming its event iterator
	// does (pkg/runner/headless.go, pkg/runner/wakeloop.go). A
	// subagent invoked as a tool runs on its OWN ADK runner inside
	// this handler, so its events never reach that iterator and no
	// harness can see them. The asynchronous door solves the same
	// problem the same way, one layer up
	// (pkg/agent/background/spawn.go rolls a spawned run into
	// parent.Tracker()).
	//
	// agent.WithSubagents fills this in from the parent's
	// agent.WithUsageTracker. Nil leaves the spend unaccounted, which
	// is what a host that wired no tracker anywhere already has
	// everywhere else.
	//
	// The roll-up is one level: it resolves at construction, so a
	// subagent that was itself built with WithSubagents bills ITS
	// subagents to whatever tracker IT held at the time — nil, for a
	// child assembled without one. That gap is library-only; the
	// declarative roster is flat (config.SubagentSpec has no nested
	// subagents, and a subagent is denied the spawn tool), so every
	// in-tree deployment is a single level.
	ParentTracker *usage.Tracker
}

const (
	defaultSubagentMaxDepth = 2
	defaultSubagentDesc     = "Run a focused subagent and return its result. Pass the request as a single string."

	// subagentGateBucket is the policy bucket delegation is matched
	// under, for both this synchronous door and background's
	// spawn_agent. Duplicated as a literal rather than imported from
	// pkg/agent/background, which imports this package. See
	// SubagentOptions.Gate for why the two doors share one bucket.
	subagentGateBucket = "spawn_agent"
)

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
	// The model name the delegated turns are priced and attributed
	// under. Captured once: Agent.model is set by New and never
	// reassigned, and a declarative subagent commonly runs a cheaper
	// tier than its parent, so per-model attribution in /usage only
	// stays honest if the SUBAGENT's model labels the row.
	//
	// Empty (an Agent built with a nil model) disables the roll-up
	// rather than filing the spend under "": a nameless bucket in
	// /usage prices at zero and reads as a bug in the ledger. Such an
	// agent cannot serve a turn anyway — the runner below fails first.
	var innerModelName string
	if m := opts.Inner.Model(); m != nil {
		innerModelName = m.Name()
	}
	// The parent's tracker wins; Inner's is the fallback for a
	// consumer who called NewSubagentTool directly and wired the
	// tracker onto the subagent instead. Both nil is the honest "no
	// ledger anywhere" case and the roll-up is skipped.
	billTo := opts.ParentTracker
	if billTo == nil {
		billTo = opts.Inner.Tracker()
	}
	// The subagent runs in its own session row derived from the
	// parent's so two concurrent runners don't trip ADK's
	// stale-session optimistic-concurrency check. Events still land
	// in the same database — audit queries find the subagent via
	// WithBranchPrefix(branch) across sessions.
	//
	// The derived session ID is computed per invocation (below, in
	// the handler) with an invocation-unique component: DefaultInstruction
	// urges parallel tool calls, so two concurrent invocations of the
	// same subagent would otherwise share one deterministic session
	// row — interleaving each other's in-flight history and racing
	// ADK's optimistic-concurrency check — and sequential invocations
	// would silently accumulate history across independent requests
	// (#364).
	parentSessionID := firstNonEmpty(opts.ParentSessionID, opts.Inner.SessionID())

	handler := func(toolCtx tool.Context, args subagentArgs) (subagentResult, error) {
		// tool.Context embeds agent.ReadonlyContext which embeds
		// context.Context, so we can read context values and pass
		// it to runner.Run directly.
		//
		// Depth is checked before the gate deliberately: it is the
		// cheaper refusal and the one the caller cannot approve away,
		// so asking a human to authorize a delegation the next line
		// would refuse anyway is pure prompt noise.
		if depth := subsession.CurrentDepth(toolCtx); depth >= maxDepth {
			return subagentResult{
				Result: fmt.Sprintf("subagent %q refused: depth limit reached (%d)", name, maxDepth),
			}, nil
		}
		// Gate the delegation itself (#758). Unlike the depth refusal
		// above — a runtime condition the model is expected to route
		// around, so it reads better as a result — a permission denial
		// propagates as a Go error, which is what every other gated
		// tool in the tree does (see pkg/tools/fetch.go) and what makes
		// the flow trace, the watchdog and both TUIs treat it as the
		// failed call it is.
		if opts.Gate != nil {
			if err := opts.Gate.CheckToolCall(toolCtx, subagentGateBucket, name, name); err != nil {
				return subagentResult{}, err
			}
		}
		// Wrap the parent's session.Service so every event the
		// inner runner appends gets the right Branch before
		// landing in storage. ADK's contents-processor uses Branch
		// to decide which events show up in the LLM request — see
		// internal/llminternal/contents_processor.go in ADK.
		parentBranch := toolCtx.Branch()
		fullBranch := subsession.ComposeBranch(parentBranch, branch)
		wrapped := &subsession.BranchInjectingService{
			Inner:  parentService,
			Branch: fullBranch,
		}

		// Derive a per-invocation session row. The invocation-unique
		// component keeps concurrent and sequential invocations of the
		// same subagent isolated from one another (#364).
		subagentSessionID := subsession.DeriveSessionID(parentSessionID, branch, subagentInvocationID(toolCtx.FunctionCallID()))

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
		childCtx := subsession.WithDepth(toolCtx, subsession.CurrentDepth(toolCtx)+1)
		// Record which subagent we're inside so a spawn_agent call made
		// from in here can't re-launch this same subagent asynchronously
		// (#732). A declarative subagent is reachable both ways — as a
		// parent tool call and by reference — under one name, so the
		// guard has to see the synchronous half of the stack too.
		childCtx = subsession.WithLineage(childCtx, name)

		msg := genai.NewContentFromText(args.Request, genai.RoleUser)
		var sb strings.Builder
		// gen_ai.agent.invocation.duration (#338): the sync subagent
		// drives the inner ADK runner directly here, bypassing the inner
		// *Agent.Run wrapper that owns recordInvocation — so without this
		// a synchronously-invoked subagent's turns never land in the
		// histogram, unlike the async/background path (which runs through
		// autonomous.Run → *Agent.Run). Record against the inner agent's
		// own instrument (its metricAgentName, bounded by config), timed
		// over the run only — the depth refusal and runner-build failure
		// above are pre-turn, matching RunWithContents' validation-first
		// posture. turnErr carries error.type on a failed run.
		started := time.Now()
		var turnErr error
		defer func() { opts.Inner.recordInvocation(time.Since(started).Seconds(), turnErr) }()
		// Bill the delegated turns to the parent (see
		// SubagentOptions.ParentTracker). TurnTap is the shared
		// discipline for this: Gemini's UsageMetadata is cumulative
		// across the chunks of one model turn, so appending per event
		// would both double-count tokens and inflate the turn count
		// (#353). Observe every event, commit once on TurnComplete.
		var tap usage.TurnTap
		for ev, err := range r.Run(childCtx, innerUserID, subagentSessionID, msg, adkagent.RunConfig{
			StreamingMode: opts.Inner.streaming,
		}) {
			if err != nil {
				turnErr = fmt.Errorf("subagent %q: run: %w", name, err)
				return subagentResult{}, turnErr
			}
			collectFinalText(&sb, ev)
			tap.Observe(ev)
			if u, ok := tap.Commit(ev); ok && billTo != nil && innerModelName != "" {
				billTo.AppendUsage(innerModelName, u, usage.PriceFor(innerModelName, nil))
			}
		}
		return subagentResult{Result: sb.String()}, nil
	}

	return functiontool.New(functiontool.Config{
		Name:        name,
		Description: desc,
	}, handler)
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

// subagentInvocationID returns a per-invocation unique component for
// the derived subagent session ID. It prefers ADK's FunctionCallID
// (stable, ties the derived row to the triggering tool call for audit)
// and falls back to a fresh UUID when that's empty — some non-Gemini
// or synthetic invocation paths don't populate it, and an empty
// component would collapse concurrent invocations back onto one shared
// row (#364).
func subagentInvocationID(functionCallID string) string {
	if id := strings.TrimSpace(functionCallID); id != "" {
		return id
	}
	return uuid.NewString()
}
