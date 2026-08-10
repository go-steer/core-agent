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
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/genai"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/tools"
	"github.com/go-steer/core-agent/v2/pkg/usage"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// DefaultAppName tags this process in the ADK runner. Telemetry and
// session stores key off this; override with WithAppName when embedding
// in a host that wants its own identity.
const DefaultAppName = "core-agent"

// The system prompt is assembled from ordered LAYERS, stable →
// volatile (docs/system-prompt-layering-design.md, #459):
//
//	1. CoreInstruction        — always (harness contract + unenforced safety invariants)
//	2. provider quirks        — selected from the model identifier; WithoutProviderQuirks suppresses
//	3. mode overlay           — InteractiveOverlay (default) or AutonomousOverlay via WithMode
//	4. user memory            — WithUserInstruction (the pkg/instruction loader's output)
//	5. consumer/operator text — WithExtraInstruction (repeatable), config append_system_prompt, flags
//
// Later layers are more specific and, by ordinary instruction-following
// convention, win on conflict — a project's AGENTS.md can override the
// interactive overlay's communication defaults, but nothing silently
// overrides the compaction contract short of a WithInstruction full
// replace. Stable-first ordering is also the prompt-cache-friendly
// ordering: editing project memory never invalidates the cached core.
//
// Admission test for CoreInstruction (do not add lines that fail it):
// (1) harness contract the model cannot discover by looking, or
// (2) a safety invariant the runtime does not yet enforce, carrying a
// marked exit path for when it does. Everything else goes in quirks,
// overlays, tool descriptions, or user layers.

// CoreInstruction is layer 1 — the always-on core. Two items, both
// harness contract: the dispatch fact (read-only tools run
// concurrently; the runtime serializes state-mutating tools — the
// #460 enforcement whose landing DELETED the old edit-sequencing
// prompt rule, exactly per its marked exit path) and the
// compaction/handover contract (the paragraph spawned subagents
// were silently losing pre-#459).
const CoreInstruction = `Independent tool calls issued in the same response may execute concurrently; the runtime serializes state-mutating tools so writes cannot race. A call that depends on another call's result must go in a later response — results are never visible to sibling calls in the same response.

Earlier conversation may have been summarized into context for you in one of two shapes: "[Conversation compacted…]" framing (we hit the context wall mid-task and the prior turns were condensed), or "[The prior task is complete…]" framing (the prior task closed cleanly and a handover record replaces its history). Both arrive wrapped at the start of your context, both are authoritative shared history. Read FROM them when the user references prior work — what was discussed, what files were touched, what was decided — rather than re-running tools to rediscover what's already recorded there. The conversation continues in both cases; treat the framing as picking up an in-progress session, not as a fresh start.`

// GeminiParallelismQuirk is a layer-2 provider quirk applied to
// Gemini-family models (model identifier containing "gemini").
//
// Probe evidence (dev/parallel-probe/): Gemini-3.1-pro-preview-
// customtools without this mandate never batched across 65 search
// turns; Claude models are "less affected, marginal benefit" and get
// no quirks. Retire this when a probe rerun shows the provider no
// longer needs the exhortation.
const GeminiParallelismQuirk = `Execute multiple independent tool calls in parallel when feasible — searching, reading files, independent shell commands, or editing different files. When investigating code, if you need to read multiple files or grep multiple directories, issue all the tool calls in a single response; do not execute them one by one.`

// InteractiveOverlay is layer 3a — the default mode overlay.
// Disposition only: a present user can redirect cheaply, so narrate
// before non-trivial work and ask focused questions when genuinely
// blocked. No tool names (tool mechanics live in tool descriptions).
const InteractiveOverlay = `A user is present. Before starting non-trivial work — multi-file edits, architectural choices, asks with multiple valid approaches — say what you're about to do in a sentence or two so they can redirect cheaply; skip the preamble for trivial asks. When blocked on a decision only the user can make, ask one focused question rather than guessing. Report outcomes plainly, including failures and steps you skipped.`

// AutonomousOverlay is layer 3b — selected by WithMode(ModeAutonomous).
// The narrate-before-acting line is deliberately here: the
// eventlog/OTel trace is a runtime property of every autonomous
// deployment, and stated intent is what makes a burst of tool calls
// legible in that record. The no-clarification line is scoped to
// questions in OUTPUT TEXT; a deliberately installed ask channel
// (tools.NewAskUserTool with a live prompter) carries its own
// exception in its tool description.
const AutonomousOverlay = `You are operating autonomously: no human reads your output in real time, and questions posed in it go unanswered — do not ask for clarification in your responses. Proceed on reversible actions that follow from the goal; gather missing information with your tools instead of asking, and prefer a recorded reasonable assumption over stalling. Before each multi-step or consequential series of actions, state in a sentence or two what you are about to do and why — nobody will approve it; your output is the audit record of the run. Verify your work before declaring it done: run the checks that exist rather than asserting success. End your turn only when the goal is complete or blocked on something no tool can resolve, and say which.`

// DefaultInstruction is the pre-#459 monolithic prompt, retained as
// a compositional alias through the v2.8.x series (deleted at the
// next breaking window alongside WithSystemInstructionPrefix). Close
// to today's semantics minus the persona and plan-sketch lines — see
// the disposition table in docs/system-prompt-layering-design.md.
//
// Deprecated: build with the layer options (WithMode,
// WithExtraInstruction, WithUserInstruction) instead of composing
// against this constant.
const DefaultInstruction = CoreInstruction + "\n\n" + InteractiveOverlay

// Mode selects the layer-3 overlay: how the agent should carry
// itself given who (if anyone) is watching. Set where the agent is
// built — drivers never mutate a caller-supplied agent (the
// autonomous driver warns when it sees an interactive-mode agent;
// see autonomous.Run).
type Mode int

const (
	// ModeInteractive (the default): a user is present and can
	// redirect / answer questions.
	ModeInteractive Mode = iota
	// ModeAutonomous: nobody reads output in real time; narrate for
	// the audit record and never ask questions in output text.
	ModeAutonomous
)

// assembleInstruction builds the layered system prompt for modelName
// (docs/system-prompt-layering-design.md): core → provider quirks →
// mode overlay → user memory → extras, blank-line joined, empty
// layers omitted. Shared by agent.New and RunSubtask (which builds
// its llmagent directly).
func assembleInstruction(modelName string, mode Mode, noQuirks bool, userInstruction string, extras []string) string {
	layers := make([]string, 0, 4+len(extras))
	layers = append(layers, CoreInstruction)
	if !noQuirks {
		layers = append(layers, providerQuirks(modelName)...)
	}
	switch mode {
	case ModeAutonomous:
		layers = append(layers, AutonomousOverlay)
	default:
		layers = append(layers, InteractiveOverlay)
	}
	if userInstruction != "" {
		layers = append(layers, userInstruction)
	}
	layers = append(layers, extras...)
	return joinLayers(layers)
}

// providerQuirks selects the layer-2 workarounds for a model
// identifier. Quirk admission requires probe evidence cited on the
// quirk const, and each quirk names its models so it can be retired
// when the provider improves. Claude models get no quirks today.
func providerQuirks(modelName string) []string {
	if strings.Contains(strings.ToLower(modelName), "gemini") {
		return []string{GeminiParallelismQuirk}
	}
	return nil
}

// joinLayers blank-line-joins the non-empty layers — no headers, no
// placeholders (the instruction loader emits its own scope headers
// inside layer 4).
func joinLayers(layers []string) string {
	out := make([]string, 0, len(layers))
	for _, l := range layers {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n\n")
}

// DefaultSchedulingInstruction is the composable system-instruction
// constant for autonomous loops that have a tools.Scheduler installed
// (via RunAutonomous's WithScheduler option, or per-subagent via
// BackgroundAgentManager). It covers the cross-cutting cadence and
// state-persistence guidance that doesn't fit in the schedule_next_turn
// tool's per-call description.
//
// Opt-in by composition — the autonomous driver does NOT inject this
// automatically. Recommended consumer usage:
//
//	agent.New(m,
//	    agent.WithInstruction(
//	        agent.DefaultInstruction + "\n\n" +
//	        agent.DefaultSchedulingInstruction + "\n\n" +
//	        myConsumerInstruction,
//	    ),
//	    agent.WithTools(...),
//	)
//
// See docs/scheduled-monitoring-design.md for the design rationale
// and the matching tool-description text (Layer 1 of the steering
// pattern).
const DefaultSchedulingInstruction = `When running a paced loop with schedule_next_turn:

1. Default to slow cadences. Most monitoring tasks tolerate 5-15 minute gaps; some tolerate hours. Cost scales linearly with wake frequency — start slow and tighten only when you observe active anomalies.

2. Adaptive cadence is encouraged. When you see anomalies in flight, shorten the cadence for the next few turns to track resolution. When the system has been quiet for several cycles, lengthen the cadence again.

3. State does not survive a defer except in the eventlog. The conversation context resets between turns; only files you wrote and todo entries you created persist. To carry a baseline ("deployments I saw last scan", "error counts at last poll") across turns, write it to a file or todo entry on this turn and read it back on the next.

4. The next_prompt is a hook, not a full restatement. Keep it short and action-oriented ("rescan and diff vs baseline.json"). The original goal and your system instructions are already in the next turn's context.

5. Don't call schedule_next_turn and report_done in the same turn. If you do, report_done wins and the loop exits.`

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
	// invocationHist + toolInstrumenter are the #338 gen_ai.*
	// instruments; both are non-nil after New (noop-backed when
	// metrics are off; nil only on hand-constructed Agents, which
	// the record sites guard against). toolInstrumenter is reused
	// by RunSubtask so subtask tool calls land in the same
	// histogram. metricAgentName is the low-cardinality
	// gen_ai.agent.name attribute value (see WithMetricAgentName).
	invocationHist   metric.Float64Histogram
	toolInstrumenter *tools.DurationInstrumenter
	metricAgentName  string
	// compactionsDone / checkpointsDone are process-lifetime metric
	// counters (#338), incremented on successful Compact/Checkpoint.
	// Deliberately not derived from ContextStats: the eventlog scan
	// is O(events) per read and its count survives restarts — the
	// wrong shape for an ObservableCounter.
	compactionsDone atomic.Int64
	checkpointsDone atomic.Int64
	// pendingInterruptAudit is set by MarkInterruptPending (the agent
	// side of attach.InterruptSelfAuditor) when an operator interrupt
	// fires, and drained in the post-turn cleanup once the interrupted
	// turn has fully unwound — the one window with no live runner handle
	// racing the audit write (#565). Atomic: set from the /interrupt
	// handler goroutine, read/cleared from the turn goroutine.
	pendingInterruptAudit atomic.Bool
	// watchdogAlertCounter is the sync core_agent.watchdog.alerts
	// instrument; counted in drainWatchdogAlerts.
	watchdogAlertCounter metric.Int64Counter
	description          string
	userID               string
	sessionID            string
	model                adkmodel.LLM
	modelName            string
	mode                 Mode
	gate                 *permissions.Gate
	bgMgr                SubagentManager
	// subagentMaxDepth is this agent's recursion cap when wrapped as a
	// subagent tool (set via WithSubagentMaxDepth). Read by a parent's
	// WithSubagents resolution; 0 = substrate default.
	subagentMaxDepth int
	inbox            *inbox
	wake             *wakeSignal
	tracker          *usage.Tracker
	compactor        Compactor
	checkpointer     Checkpointer

	// operatorEmit is the typed operator-event callback set by the
	// broadcaster on first subscribe (see the attach package's broadcaster Subscribe).
	// Nil when no SSE client is connected — the emit() helper drops
	// events to the floor in that case (no consumer = no work).
	// Guarded by emitMu so the broadcaster can swap or clear it
	// without racing the agent's per-turn emit calls.
	emitMu       sync.Mutex
	operatorEmit func(eventType string, payload any)

	// mu guards cancelInFlight + compactionPending + checkpoint
	// flags + subtask counters. Held only across short store-and-
	// clear operations; never across an LLM call.
	mu                    sync.Mutex
	cancelInFlight        context.CancelFunc
	cancelInFlightGen     uint64 // generation of the currently-registered cancel (0 = none)
	cancelSeq             uint64 // monotonic issuer for cancel generations (#359)
	compactionPending     bool
	compactionFailures    int    // consecutive failed auto-compactions; drives backoff (#356)
	compactionCooldown    int    // turns to skip before the next auto-compaction attempt (#356)
	checkpointRequested   bool   // flipped by mark_task_done tool handler during a turn
	checkpointPending     bool   // promoted from checkpointRequested by post-turn hook
	pendingCheckpointNote string // detail from the mark_task_done call (or /done arg)
	// Subtask counters surface through ContextStats so /context can
	// show how much of the parent's reported cost came from
	// Mechanism-B subtasks vs parent turns. usage.Tracker bundles
	// both into one totals view because pricing per-turn doesn't
	// know whether the turn came from a subtask; these counters
	// give us the breakdown without touching the tracker.
	subtaskCount        int
	subtaskInputTokens  int
	subtaskOutputTokens int
	subtaskCostUSD      float64

	// Cost-ceiling enforcement (#145). costCeiling is the configured
	// caps (zero = disabled). turnStartCost snapshots the session's
	// cumulative cost at each turn's start so the post-turn hook can
	// compute the delta. costCeilingExceeded blocks new Run calls
	// until the operator calls ResetCostCeiling. See cost_ceiling.go
	// for the full enforcement contract.
	costCeiling         CostCeiling
	turnStartCost       float64
	costCeilingExceeded bool
	costCeilingReason   string

	// Watchdog (#123 PR 2). Optional behavioral observer; nil when
	// not wired. onWatchdogAlert is called for each alert returned by
	// watchdog.Check in the post-turn hook; default nil = collect-only
	// (alerts accumulate but never surface — useful for tests).
	//
	// watchdogEnforce (#623) turns a Critical alert into a hard halt:
	// watchdogTripped blocks new Run calls (via preflightWatchdog)
	// until the operator calls ResetWatchdog, and watchdogReason holds
	// the operator-facing explanation for /stats-style surfaces. This
	// mirrors the cost-ceiling kill switch (see watchdog.go +
	// cost_ceiling.go for the shared contract).
	watchdog        watchdog.Watchdog
	onWatchdogAlert func(watchdog.Alert)
	watchdogEnforce bool
	watchdogTripped bool
	watchdogReason  string

	// Event hook (WithEventHook). Optional callbacks that observe
	// session events as they stream (onEvent) and once per turn from
	// the post-turn cleanup (onTurnEnd). Both nil = no-op. The
	// pkg/hooks Dispatcher plugs its methods in here to fan events
	// out to operator-configured shell commands.
	onEvent   func(*session.Event)
	onTurnEnd func()
}

// Option mutates Agent construction. Use the With* helpers below.
type Option func(*options)

type options struct {
	appName     string
	name        string
	description string
	instruction string
	// instructionExplicit records that WithInstruction (or the
	// deprecated prefix path) supplied a full replacement — layers
	// 1–3 are skipped; layers 4–5 still append (#459).
	instructionExplicit bool
	mode                Mode
	userInstruction     string
	extraInstructions   []string
	noQuirks            bool
	streaming           adkagent.StreamingMode
	userID              string
	sessionID           string
	tools               []tool.Tool
	toolsets            []tool.Toolset
	sessionService      session.Service
	eventLog            *eventlog.Handle
	subagents           []*Agent
	// subagentMaxDepth is this agent's own recursion cap when it is
	// wrapped as a subagent tool (WithSubagentMaxDepth). Read by the
	// PARENT's WithSubagents resolution, not used when this agent runs
	// as a parent. 0 = substrate default.
	subagentMaxDepth int
	bgMgr            SubagentManager
	gate             *permissions.Gate
	tracker          *usage.Tracker
	compactor        Compactor
	checkpointer     Checkpointer
	costCeiling      CostCeiling
	watchdog         watchdog.Watchdog
	onWatchdogAlert  func(watchdog.Alert)
	watchdogEnforce  bool
	onEvent          func(*session.Event)
	onTurnEnd        func()
	postConstruct    func(*Agent)
	meterProvider    metric.MeterProvider
	metricAgentName  string
}

func defaultOptions() options {
	return options{
		appName:     DefaultAppName,
		name:        "core_agent",
		description: "core-agent conversational agent",
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

// WithInstruction replaces the assembled system instruction wholesale
// — the full-replace escape hatch. Layers 1–3 (core, provider quirks,
// mode overlay) are skipped ENTIRELY; you take on the harness
// contract yourself (compaction summaries, parallel-dispatch rules —
// tool-use degradation is on you). Layers 4–5 (WithUserInstruction /
// WithExtraInstruction) still append after the replacement so a
// custom base can compose with operator appends.
func WithInstruction(s string) Option {
	return func(o *options) {
		o.instruction = s
		o.instructionExplicit = true
	}
}

// WithMode selects the layer-3 overlay (default ModeInteractive).
// Autonomous consumers — anything driving the agent with no human
// reading output in real time — should set ModeAutonomous where they
// build the agent; the in-tree spawn paths (background subagents,
// RunSubtask, remote spawn) do.
func WithMode(m Mode) Option { return func(o *options) { o.mode = m } }

// WithExtraInstruction appends s as a layer-5 block (repeatable —
// each call appends another blank-line-separated block, in call
// order). The encouraged customization path: the harness contract
// and mode overlay stay intact underneath. Empty strings are
// dropped.
func WithExtraInstruction(s string) Option {
	return func(o *options) {
		if s == "" {
			return
		}
		o.extraInstructions = append(o.extraInstructions, s)
	}
}

// WithUserInstruction installs the pkg/instruction loader's output
// as layer 4 (user memory: AGENTS.md and friends). Deliberately
// AFTER the core/overlay layers — user instructions take precedence
// over our defaults by ordinary instruction-following convention,
// and the stable-first ordering keeps the cached core prefix intact
// across memory edits (this inverts the deprecated
// WithSystemInstructionPrefix arrangement on purpose).
func WithUserInstruction(s string) Option {
	return func(o *options) { o.userInstruction = s }
}

// WithoutProviderQuirks suppresses layer 2 — for consumers that have
// measured their model doesn't need the workarounds, or that are
// running their own probes.
func WithoutProviderQuirks() Option { return func(o *options) { o.noQuirks = true } }

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
// Each subagent's tool name comes from its own WithName value, and its
// recursion depth cap from its own WithSubagentMaxDepth value (default 2).
// Use NewSubagentTool directly for the remaining per-subagent overrides
// (custom tool name, description, branch label).
//
// Resolved at the end of New() so that the parent's session.Service
// and session triple — set by other With* options — are captured
// at the point the subagent tools are constructed.
func WithSubagents(agents []*Agent) Option {
	return func(o *options) { o.subagents = append(o.subagents, agents...) }
}

// WithSubagentMaxDepth sets the recursion depth cap applied when THIS
// agent is exposed as a subagent tool via WithSubagents — the value
// forwarded to SubagentOptions.MaxDepth at the parent's construction.
// A subagent at depth >= this cap that is invoked from another subagent
// gets an error result rather than being allowed to recurse.
//
// 0 (the default) means "use the substrate default" (NewSubagentTool's
// defaultSubagentMaxDepth, currently 2). Declarative subagents thread
// their config `max_depth` through this option so an operator-set value
// is honored rather than silently dropped; a plain WithSubagents caller
// who never sets it keeps the default.
func WithSubagentMaxDepth(n int) Option {
	return func(o *options) { o.subagentMaxDepth = n }
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
func WithBackgroundManager(mgr SubagentManager) Option {
	return func(o *options) { o.bgMgr = mgr }
}

// the agent's metadata, so it can be surfaced over the attach-mode
// /tools endpoint (each tool gets a pre-flight `gate_state` field —
// "allowed" / "denied" / "prompted" / "denied-allow-mode" — without
// actually consulting the gate at request time). Optional; without
// it, the /tools endpoint reports an empty gate_state per tool and
// the TUI's auditing column is blank.
//
// This is metadata-only — the gate that actually mediates tool calls
// is still the one wired into the tool constructors themselves. The
// agent does not call this gate; it just exposes a read-only view.
func WithGate(g *permissions.Gate) Option {
	return func(o *options) { o.gate = g }
}

// WithSystemInstructionPrefix prepends prefix to the agent's
// instruction with the pre-#459 semantics: the result is a full
// replacement (prefix + whatever instruction was set, defaulting to
// the DefaultInstruction alias), so layer assembly is skipped.
//
// Deprecated: memory belongs AFTER the core, not before it — use
// WithUserInstruction (layer 4). This survives through v2.8.x for
// consumers that composed against the old prefix arrangement and is
// deleted at the next breaking window together with
// DefaultInstruction.
func WithSystemInstructionPrefix(prefix string) Option {
	return func(o *options) {
		if prefix == "" {
			return
		}
		base := o.instruction
		if !o.instructionExplicit {
			base = DefaultInstruction
		}
		if base == "" {
			o.instruction = prefix
		} else {
			o.instruction = prefix + "\n\n" + base
		}
		o.instructionExplicit = true
	}
}

// WithUsageTracker wires a shared *usage.Tracker into the agent so
// agent-level code (the compactor's threshold check, future per-turn
// rollups) can read context-window state without the consumer
// reaching in. The same tracker can be shared with a TUI host that
// already keeps one for /stats — both populate via usage.Append and
// read the same totals.
//
// Optional. Nil-safe: components that read the tracker check first
// and degrade gracefully ("don't trigger threshold-based compaction
// if we don't know how full the window is").
func WithUsageTracker(t *usage.Tracker) Option {
	return func(o *options) { o.tracker = t }
}

// WithCompactor wires a Compactor implementation that drives
// context-window compaction (Mechanism A of
// docs/context-management-design.md). When wired, the post-turn
// hook in Run checks Compactor.ShouldCompact(); if true, the next
// Run call fires Compact() before its actual work, replacing the
// pre-summary history with a single summary event.
//
// Pass agent.NewDefaultCompactor() for the package default
// (threshold 0.85, five-section handover prompt). Custom Compactor
// implementations let consumers swap in a different prompt or
// trigger logic.
//
// Optional. When nil, Agent.Compact returns ErrNoCompactor and the
// post-turn hook is a no-op — compaction has to be wired in
// explicitly.
func WithCompactor(c Compactor) Option {
	return func(o *options) { o.compactor = c }
}

// WithCheckpointer wires a Checkpointer implementation that drives
// task-boundary checkpoints (Mechanism C of
// docs/context-management-design.md). When wired, the agent
// automatically registers the mark_task_done built-in tool — the
// model can call it to signal task completion, and the post-turn
// hook in Run promotes that into a pending checkpoint the next
// Run drains by writing a richer handover record. The TUI's
// /done slash drives the same path manually.
//
// Pass agent.NewDefaultCheckpointer() for the package default
// (heuristic off; mark_task_done + /done are the trigger paths;
// six-section completion-record prompt). Custom Checkpointer
// implementations let consumers swap in a different prompt or
// heuristic.
//
// Optional. When nil, Agent.Checkpoint returns ErrNoCheckpointer
// and the mark_task_done tool is not registered.
func WithCheckpointer(c Checkpointer) Option {
	return func(o *options) { o.checkpointer = c }
}

// WithPostConstruct registers a callback invoked once the *Agent
// is fully built (right before New returns). Useful for late-
// binding patterns where the caller needs the agent pointer to
// wire something they registered earlier — e.g., an externally-
// constructed tool whose handler closure captured a *Agent
// placeholder. The hook fires on the happy path only; if New
// returns an error the hook is not called.
//
// One hook per agent. Calling WithPostConstruct twice keeps the
// last one (Option-pattern overwrite semantics, same as other
// scalar With* options).
func WithPostConstruct(f func(*Agent)) Option {
	return func(o *options) { o.postConstruct = f }
}

// WithMeterProvider overrides the OTel MeterProvider backing the
// agent's metric instruments (gen_ai.agent.invocation.duration and
// the per-tool gen_ai.tool.execution.duration wrapper). Defaults to
// otel.GetMeterProvider() resolved at construction time — the daemon
// installs its provider via telemetry.SetupMetrics before any agent
// is built, and when metrics are disabled the global is the noop
// provider, so recording costs nothing. Embedders wanting metrics
// must likewise install their provider (or pass one here) before
// calling New. Background subagents always bind to the global
// provider (the spawn path doesn't thread this option). Primarily
// useful for tests injecting a ManualReader.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.meterProvider = mp }
}

// WithMetricAgentName overrides the gen_ai.agent.name attribute value
// on the agent's metric instruments without changing the agent's
// actual name (WithName). Metric attribute values must stay
// low-cardinality: the spawn path names background subagents with
// MODEL-CHOSEN strings, and stamping those on a histogram would
// accrete one series per invented name on a long-lived daemon — it
// passes a fixed class-level value here instead. Defaults to the
// WithName value, which is operator-configured and bounded.
func WithMetricAgentName(name string) Option {
	return func(o *options) { o.metricAgentName = name }
}

// New constructs an Agent backed by model. Returns a clear error if the
// underlying ADK constructors reject the configuration.
func New(model adkmodel.LLM, opts ...Option) (*Agent, error) {
	if model == nil {
		return nil, fmt.Errorf("agent: model is required")
	}
	// Strip role-less / invalid-role Content from every request before it
	// reaches the provider (#614). Wrapped here so the ONE model value is
	// sanitized on both the main-turn runner (llmagent below) and the
	// internal summarizer/side-question paths (a.model). Outermost of any
	// provider wrapper. See rolesanitize.go.
	model = newRoleSanitizingLLM(model)
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
			MaxDepth:        sa.subagentMaxDepth, // 0 → NewSubagentTool default
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

	// Register the mark_task_done built-in BEFORE llmagent.New —
	// llmagent snapshots its tool list at construction time, so
	// adding the tool after would mean the model never sees it.
	// The handler needs to mutate the constructed Agent's
	// checkpoint flags; we resolve the agent pointer via late
	// binding (declared here, populated after the struct is
	// built below). See NewMarkTaskDoneTool docs for the
	// late-binding contract.
	var agentRef *Agent
	if o.checkpointer != nil {
		o.tools = append(o.tools, NewMarkTaskDoneTool(func() *Agent { return agentRef }))
	}

	// Mutating-tool serialization (#460): read-only tools keep ADK's
	// concurrent dispatch; state-mutating tools share one per-agent
	// lock so parallel writes can never race and corrupt state — the
	// runtime enforcement that retired CoreInstruction's old
	// edit-sequencing prompt rule. Wrapped last so every registered
	// tool (consumer tools, internal mark_task_done, spawn extras)
	// is covered by the same serializer.
	var mutationMu tools.MutationSerializer
	o.tools = tools.SerializeMutating(o.tools, &mutationMu)
	for i, ts := range o.toolsets {
		o.toolsets[i] = tools.SerializeMutatingToolset(ts, &mutationMu)
	}

	// Per-tool duration metrics (#338): wrap OUTSIDE the serializer
	// so the recorded latency includes mutation-lock wait (and, for
	// MCP tools gated inside mcp.Build, permission-prompt wait) —
	// the latency the model actually observes. When metrics are
	// disabled the resolved global provider is the noop one and
	// recording costs nothing.
	mp := o.meterProvider
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	toolInstrumenter, err := tools.NewDurationInstrumenter(mp)
	if err != nil {
		return nil, fmt.Errorf("agent: tool duration instrumenter: %w", err)
	}
	o.tools = toolInstrumenter.Instrument(o.tools)
	for i, ts := range o.toolsets {
		o.toolsets[i] = toolInstrumenter.InstrumentToolset(ts)
	}
	invocationHist, err := newInvocationHistogram(mp)
	if err != nil {
		return nil, fmt.Errorf("agent: invocation histogram: %w", err)
	}
	watchdogAlerts, err := newWatchdogAlertCounter(mp)
	if err != nil {
		return nil, fmt.Errorf("agent: watchdog alert counter: %w", err)
	}

	// Layer assembly (#459). WithInstruction / the deprecated prefix
	// path replace layers 1–3 wholesale; layers 4–5 append in both
	// arrangements so operator appends compose with a custom base.
	instruction := o.instruction
	if o.instructionExplicit {
		tail := make([]string, 0, 1+len(o.extraInstructions))
		if o.userInstruction != "" {
			tail = append(tail, o.userInstruction)
		}
		tail = append(tail, o.extraInstructions...)
		instruction = joinLayers(append([]string{instruction}, tail...))
	} else {
		instruction = assembleInstruction(model.Name(), o.mode, o.noQuirks, o.userInstruction, o.extraInstructions)
	}

	inner, err := llmagent.New(llmagent.Config{
		Name:        o.name,
		Model:       model,
		Description: o.description,
		Instruction: instruction,
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

	// Wrap the runner's view of the session.Service so the compactor
	// can slice history at the latest summary event before ADK builds
	// the LLM request. Other callers (direct Compact, AskSideQuestion,
	// subagent path) keep using the unwrapped svc on the Agent so
	// they see the full audit log. When no compactor is wired, the
	// wrapping is a no-op pass-through.
	runnerSvc := svc
	if o.compactor != nil {
		runnerSvc = &compactingService{inner: svc}
	}

	r, err := runner.New(runner.Config{
		AppName:           o.appName,
		Agent:             inner,
		SessionService:    runnerSvc,
		AutoCreateSession: true,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: build runner: %w", err)
	}

	a := &Agent{
		inner:                inner,
		runner:               r,
		sessionService:       svc,
		eventLog:             o.eventLog,
		tools:                o.tools,
		streaming:            o.streaming,
		appName:              o.appName,
		agentName:            o.name,
		description:          o.description,
		userID:               o.userID,
		sessionID:            o.sessionID,
		model:                model,
		modelName:            model.Name(),
		mode:                 o.mode,
		subagentMaxDepth:     o.subagentMaxDepth,
		invocationHist:       invocationHist,
		toolInstrumenter:     toolInstrumenter,
		metricAgentName:      cmp.Or(o.metricAgentName, o.name),
		watchdogAlertCounter: watchdogAlerts,
		gate:                 o.gate,
		bgMgr:                o.bgMgr,
		inbox:                newInbox(),
		wake:                 newWakeSignal(),
		tracker:              o.tracker,
		compactor:            o.compactor,
		checkpointer:         o.checkpointer,
		costCeiling:          o.costCeiling,
		watchdog:             o.watchdog,
		onWatchdogAlert:      o.onWatchdogAlert,
		watchdogEnforce:      o.watchdogEnforce,
		onEvent:              o.onEvent,
		onTurnEnd:            o.onTurnEnd,
	}
	if a.bgMgr != nil {
		a.bgMgr.AttachParent(a)
	}
	// Late-bind the agent pointer so the mark_task_done tool
	// (registered above before llmagent.New) can resolve *Agent
	// when the model calls it. See NewMarkTaskDoneTool docs.
	agentRef = a
	if o.postConstruct != nil {
		o.postConstruct(a)
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

// Description returns the one-line description set via WithDescription.
// Empty when unset. Satisfies attach.DescriptionProvider — the
// /.well-known/agent-card.json handler falls back to this when no
// explicit AgentCardConfig.Description override is supplied.
func (a *Agent) Description() string { return a.description }

// Compile-time check: *Agent satisfies attach.DescriptionProvider.
// If this assertion ever fails, the card endpoint's automatic
// description fallback would silently stop working in production.
var _ attach.DescriptionProvider = (*Agent)(nil)

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

// Streaming returns the ADK streaming mode the agent was constructed
// with. Part of the read-only accessor seam the split-out subagent
// packages (pkg/agent/background) use to build child agents that
// inherit the parent's streaming behavior without reaching the
// unexported field directly.
func (a *Agent) Streaming() adkagent.StreamingMode {
	if a == nil {
		var zero adkagent.StreamingMode
		return zero
	}
	return a.streaming
}

// Tracker returns the usage.Tracker the agent was constructed with via
// WithTracker, or nil when none was wired. Part of the read-only
// accessor seam pkg/agent/background uses to roll background subagent
// turns into the parent's usage totals.
func (a *Agent) Tracker() *usage.Tracker {
	if a == nil {
		return nil
	}
	return a.tracker
}

// Gate returns the permissions gate wired via WithGate, or nil when
// none was configured. Read-only seam for the split-out packages
// (pkg/attachadapter projects gate state onto the attach wire
// format); mutations go through the gate's own methods.
func (a *Agent) Gate() *permissions.Gate {
	if a == nil {
		return nil
	}
	return a.gate
}

// Inner returns the underlying ADK agent the turn loop drives. It is
// the read-only seam the split-out driver package (pkg/agent/autonomous,
// see docs/agent-package-split-design.md) uses instead of reaching the
// unexported field directly; returns nil if the agent is nil or was
// constructed without an inner agent.
func (a *Agent) Inner() adkagent.Agent {
	if a == nil {
		return nil
	}
	return a.inner
}

// SetOperatorEventEmitter installs (or clears, when f is nil) the
// callback the agent uses to push typed operator events —
// status-update, usage-update, turn-complete, turn-error, inbox —
// to whatever transport is listening. The attach SSE broadcaster is
// today's only consumer (it wires its Emit on first subscriber and
// clears it when the last disconnects), but the seam itself is
// transport-neutral (#506; formerly SetAttachEmitter, which baked
// one transport's name into the frozen core surface).
//
// When a tracker is wired via WithUsageTracker, this
// also installs (or clears) a tracker.SetOnAppend callback that
// emits a usage-update event with cumulative + per-model totals
// after every Append. That's what carries the "running cost" the
// spec describes for the usage-update event type — the
// turn-complete event reports 0 for cost because the agent itself
// has no pricing reference (pricing lives in the harness).
//
// Optional. When no callback is installed, all agent-side emit calls
// are no-ops — events are dropped to the floor since no consumer
// can see them. This matches the protocol's design intent: typed
// events are operator-visible signals, not audit log entries; if
// there's no operator, there's nothing to signal.
//
// Safe to call concurrently with the agent's own emit path; the
// internal mutex serializes the swap and any in-flight emit reads.
func (a *Agent) SetOperatorEventEmitter(f func(eventType string, payload any)) {
	if a == nil {
		return
	}
	a.emitMu.Lock()
	a.operatorEmit = f
	a.emitMu.Unlock()

	if a.tracker == nil {
		return
	}
	if f == nil {
		a.tracker.SetOnAppend(nil)
		return
	}
	a.tracker.SetOnAppend(func() {
		totals := a.tracker.Totals()
		update := attach.UsageUpdate{
			TokensInTotal:  totals.InputTokens,
			TokensOutTotal: totals.OutputTokens,
			CostUSDTotal:   totals.CostUSD,
			TurnsTotal:     totals.Turns,
		}
		if byModel := a.tracker.TotalsByModel(); len(byModel) > 0 {
			update.ByModel = make(map[string]attach.UsageByModel, len(byModel))
			for model, t := range byModel {
				update.ByModel[model] = attach.UsageByModel{
					TokensIn:  t.InputTokens,
					TokensOut: t.OutputTokens,
					CostUSD:   t.CostUSD,
					Turns:     t.Turns,
				}
			}
		}
		// The onAppend callback fires after tracker.Append lands the
		// turn, so Last() gives the just-committed record. Ship it
		// inline as LastTurn so remote surfaces can render authoritative
		// per-turn cost (with cache discount + operator pricing
		// overrides applied server-side) without the client having to
		// duplicate the pricing catalog lookup.
		if last, ok := a.tracker.Last(); ok {
			update.LastTurn = &attach.UsageLastTurn{
				TokensIn:       last.InputTokens,
				TokensInCached: last.CachedInputTokens,
				TokensOut:      last.OutputTokens,
				CostUSD:        last.CostUSD,
				Model:          last.Model,
			}
		}
		a.emit(attach.EventUsageUpdate, update)
	})
}

// emit pushes one typed operator event to the installed emitter.
// No-op when none is wired (no operator listening, nothing to
// signal).
//
// The lock is held only across the callback read, not the call
// itself, so a fan-out-heavy emitter (the attach broadcaster's
// Emit) doesn't block agent progress and a SetOperatorEventEmitter
// swap can't race a long-running fan-out.
func (a *Agent) emit(eventType string, payload any) {
	if a == nil {
		return
	}
	a.emitMu.Lock()
	cb := a.operatorEmit
	a.emitMu.Unlock()
	if cb == nil {
		return
	}
	cb(eventType, payload)
}

// Emit is the exported seam over emit() for the packages that will
// split out of pkg/agent (pkg/agent/background's inbox lives outside
// the core type — see docs/agent-package-split-design.md). It pushes a
// typed event onto the attach SSE stream, or is a no-op when no
// subscriber is connected. Safe on a nil receiver.
func (a *Agent) Emit(eventType string, payload any) {
	a.emit(eventType, payload)
}

// HasCompactor reports whether a Compactor was wired via
// WithCompactor. Hosts use this to gate operator-facing surfaces:
// don't list `/compact` in `/help` when there's nothing to invoke.
// Same idea as nil-checking a.compactor directly, but exported so
// adapters living outside the agent package don't need a
// reflection trick.
func (a *Agent) HasCompactor() bool {
	if a == nil {
		return false
	}
	return a.compactor != nil
}

// HasCheckpointer reports whether a Checkpointer was wired via
// WithCheckpointer. Hosts use this to gate `/done` (and the
// `/checkpoint` alias) out of `/help` and the slash palette when
// --no-checkpoint was passed. Same shape as HasCompactor.
func (a *Agent) HasCheckpointer() bool {
	if a == nil {
		return false
	}
	return a.checkpointer != nil
}

// ModelName returns the name of the LLM the agent was constructed
// with (sourced from model.Name() at New() time). Used by the
// attach-mode /status endpoint so the TUI usage panel can label the
// in/out/cost figures with the model in use.
func (a *Agent) ModelName() string {
	if a == nil {
		return ""
	}
	return a.modelName
}

// Mode reports the layer-3 overlay mode the agent was built with
// (#459). The autonomous driver consults this to warn when an
// interactive-mode agent is driven autonomously. Note a
// WithInstruction full-replace skips the overlay entirely; Mode
// still reports whatever WithMode set (default ModeInteractive) —
// the warning is advisory, and full-replace consumers know what
// they're doing.
func (a *Agent) Mode() Mode {
	if a == nil {
		return ModeInteractive
	}
	return a.mode
}

// Model returns the LLM the agent was constructed with (#510).
// Exposes the value New received so drivers that accept a pre-built
// Agent — runner.Run in particular — can derive the streaming model
// from the agent instead of demanding it be passed alongside (the
// agent/model mismatch hazard #492 flagged). Nil-safe.
func (a *Agent) Model() adkmodel.LLM {
	if a == nil {
		return nil
	}
	return a.model
}

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
	// Settle-time cost-ceiling enforcement (#362). The prior turn's
	// post-turn hook already ran maybeEnforceCostCeiling, but in
	// harness-driven deployments the harness calls tracker.Append for
	// that turn's main-model cost AFTER the cleanup hook (see the
	// tapped/UsageMetadata comment below and the turn-complete emit
	// path) — so the post-turn delta saw only in-turn internal appends
	// (subtasks, summarizer) and missed the main-model spend entirely.
	// A single runaway turn (the #144 read-file-loop) could therefore
	// never trip the per-turn cap. Re-run enforcement here, at the top
	// of the next Run, now that the prior turn is fully settled in the
	// tracker: a.turnStartCost still holds the prior turn's baseline
	// (snapshotTurnStartCost below hasn't reset it yet), so the delta is
	// the prior turn's true cost. Idempotent + a no-op when no ceiling
	// is configured, so this is cheap on the common path.
	a.maybeEnforceCostCeiling()
	// Cost-ceiling pre-flight (#145). If a prior turn tripped the
	// configured per-turn / per-session spend cap, refuse this turn
	// at the very top — before any tracker writes, model calls, or
	// pending-cleanup work. Operator must call ResetCostCeiling to
	// resume. Returning the error via the iterator (rather than
	// panicking or silently no-op'ing) lets the host surface a clear
	// failure mode that matches the structured turn-error event we
	// emitted when the ceiling first tripped.
	if err := a.preflightCostCeiling(); err != nil {
		return func(yield func(*session.Event, error) bool) {
			// Still a refused turn the caller observes — record it
			// (#338), or the invocation histogram goes dark exactly
			// during a spend-cap incident, with no error.type series
			// to alert on. Duration ~0 is accurate: the turn was
			// refused before any work.
			a.recordInvocation(0, err)
			yield(nil, err)
		}
	}
	// Watchdog pre-flight (#623). If a prior turn tripped a Critical
	// runaway signal under --watchdog=enforce, refuse this turn at the
	// top — same structural refusal as the cost ceiling. This is what
	// actually breaks a tool-call loop: an auto-continue re-drive of the
	// interrupted turn calls Run again and is refused here instead of
	// re-issuing the looping call. Operator resumes via ResetWatchdog.
	if err := a.preflightWatchdog(); err != nil {
		return func(yield func(*session.Event, error) bool) {
			a.recordInvocation(0, err)
			yield(nil, err)
		}
	}
	// Tail repair (#537): heal a history whose previous turn died
	// between a persisted functionCall and its functionResponse —
	// crash mid-tool, or any mid-tool cancellation (the runner
	// appends the tool's error response with the already-cancelled
	// turn ctx, so the write fails and the call is orphaned durably).
	// Providers reject an unanswered call, so without this the
	// session is poisoned for every subsequent turn. Must run before
	// the checkpoint/compaction drains below: both invoke the
	// summarizer over this same history and would trip on the
	// dangling tail themselves. See tail_repair.go.
	a.repairDanglingToolCalls(ctx)
	// Pre-turn: drain any pending cleanups from the prior turn's
	// post-hook so the runner builds its request against a slimmed
	// history. Checkpoint runs before compaction — a checkpoint
	// subsumes the slicing baseline, making any pending compaction
	// redundant for the same span. Errors are swallowed inside
	// (the operator can /done or /compact manually if it
	// persistently fails); pending flags are always cleared to
	// prevent retry loops.
	a.runPendingCheckpoint(ctx)
	a.runPendingCompaction(ctx)
	// Snapshot the session's cumulative cost so the post-turn hook
	// can compute the per-turn delta. No-op when no ceiling is
	// configured.
	a.snapshotTurnStartCost()
	if a.bgMgr != nil {
		prompt = a.bgMgr.PrependPendingAlerts(prompt)
	}
	// drainInboxFull emits `inbox`/dequeued events for each message
	// (same side effect as the public DrainInbox) and surfaces the
	// turn originator from the drained batch. Routing through this
	// helper keeps the SSE event stream consistent with what /inject
	// produced on the way in AND lets us thread the caller identity
	// into the turn context below.
	inboxTexts, inboxOriginator := a.drainInboxFull()
	prompt = prependInboxMessages(prompt, inboxTexts)
	msg := genai.NewContentFromText(prompt, genai.RoleUser)

	// Per-turn correlation handle: fresh prompt_id assigned at turn
	// start, threaded into the terminal turn-complete / turn-error
	// event so SSE consumers can correlate the terminal event back
	// to whatever (operator prompt, inbox message) triggered the turn.
	promptID := newPromptID()
	started := time.Now()

	// Announce the turn entering the streaming state. Only fields
	// that change since the last emission need to be present
	// (spec merge semantics); turn_state is always required.
	a.emit(attach.EventStatusUpdate, attach.StatusUpdate{
		Model:     a.modelName,
		TurnState: attach.TurnStateStreaming,
	})

	// Track the cancel func so Interrupt() can fire it during the
	// turn. Wrap the iterator so the cancel is cleared when the
	// consumer is done draining events (cleanly or via early
	// return) — otherwise a second Interrupt() call after the
	// turn ended would invoke a no-op cancel against the wrong
	// context.
	runCtx, cancel := context.WithCancel(ctx)
	// Thread the turn originator (most-recent caller in the drained
	// inbox batch) onto the turn context so the eventlog metadata
	// extractor, the MCP outbound path, and any other caller-aware
	// substrate sees the identity that triggered this turn. Zero
	// originator (legacy / single-user / out-of-band Run callers)
	// leaves runCtx unwrapped: any caller already on the parent ctx
	// propagates via context-value inheritance, so the no-inbox-
	// originator + ctx-caller case (an attach handler calling Run
	// directly with the request context) Just Works.
	if inboxOriginator.Identity != "" {
		runCtx = auth.WithCaller(runCtx, inboxOriginator)
	}
	// Thread the per-session gate onto runCtx so tool wrappers
	// constructed against the daemon-wide template gate (every MCP
	// toolset and the daemon-startup built-in tool registry) route
	// their permission checks through THIS session's sub-gate
	// instead of the template's. The receiver is pkg/tools.gatedTool
	// which prefers permissions.SessionGateFromContext(ctx) over
	// gt.gate when present. Without this, every multi-session tool
	// call's prompt would go to the daemon's startup PromptBroker
	// (no per-session subscriber → hang forever).
	//
	// a.gate may be nil for hand-constructed Agent values in tests;
	// permissions.WithSessionGate(nil) is a no-op so the guard is
	// covered by the helper.
	runCtx = permissions.WithSessionGate(runCtx, a.gate)
	cancelGen := a.setCancelInFlight(cancel)
	inner := a.runner.Run(runCtx, a.userID, a.sessionID, msg, adkagent.RunConfig{
		StreamingMode: a.streaming,
	})

	// Tap UsageMetadata + error state as events flow so the post-turn
	// emit can carry per-turn token totals without depending on the
	// harness's tracker.Append timing (which happens AFTER cleanup).
	var (
		promptTokens, completionTokens int
		turnErr                        error
	)
	// Per-turn dedup for watchdog observations (#363): ADK's streaming
	// aggregator can re-emit the same FunctionCall part on an
	// intermediate aggregate plus the final event, which double-counted
	// each real call and tripped the repeated-tool-call signal at ~half
	// the configured threshold. Scoped to this turn — cross-turn
	// repeats are exactly the signal the watchdog exists to count.
	watchdogSeen := map[string]struct{}{}
	tapped := func(yield func(*session.Event, error) bool) {
		for ev, err := range inner {
			if ev != nil && ev.UsageMetadata != nil {
				promptTokens = int(ev.UsageMetadata.PromptTokenCount)
				completionTokens = int(ev.UsageMetadata.CandidatesTokenCount)
			}
			if err != nil {
				turnErr = err
			}
			// Watchdog observation (#123 PR 2). Extract tool calls
			// (FunctionCall parts) from this event and feed them to
			// the watchdog so its signals can fire on the post-turn
			// hook. No-op when no watchdog is wired.
			if a.watchdog != nil && ev != nil {
				a.observeToolCallsForWatchdog(ev, watchdogSeen)
			}
			// Digest-savings observation. Walk FunctionResponse
			// parts for the `savings` sidecar the MCP digest wrap
			// stamps on every wrapped tool response, and append to
			// the session-scoped tracker. Fixes the multi-session
			// gap where the process-level `DigestOptions.OnResult`
			// callback wired in main.go never reached per-session
			// trackers (see tool_savings_observer.go's file
			// docstring for the full rationale).
			a.observeToolSavings(ev)
			// Event-hook observation (WithEventHook). Fires alongside
			// watchdog observation so both observers see the same
			// event stream in the same order. Callback is expected
			// to be synchronous and quick (pkg/hooks bounds its
			// subprocess calls with per-hook timeouts).
			if a.onEvent != nil && ev != nil {
				a.onEvent(ev)
			}
			if !yield(ev, err) {
				return
			}
		}
	}

	return wrapWithCleanup(tapped, func() {
		// Always release the per-turn context. Only Interrupt() used
		// to call cancel(); an uninterrupted turn leaked a live
		// cancellable child of the process-lifetime parent ctx every
		// turn (classic lostcancel — thousands accrue in a long-lived
		// daemon). Safe here: cleanup runs after the event stream has
		// fully drained, so nothing still depends on runCtx (#359).
		cancel()
		a.clearCancelInFlight(cancelGen)
		// Post-turn hooks. Order matters: mark_task_done flag
		// promotion first (it's the operator-visible signal); then
		// the threshold check. Either can flag a pending cleanup
		// that the next Run call drains before its own work.
		a.maybeMarkCheckpointPending()
		a.maybeMarkCompactionPending()
		a.maybeEnforceCostCeiling()
		a.drainWatchdogAlerts()
		// Operator-interrupt audit (#565): if a /interrupt fired during
		// this turn, append its audit row now — after the event stream
		// drained and runCtx was cancelled above, so the write can't
		// race the runner's in-flight session handle. No-op when nothing
		// is pending.
		a.drainInterruptAudit()
		if a.onTurnEnd != nil {
			a.onTurnEnd()
		}

		// Terminal event per spec: exactly one turn-complete OR
		// turn-error fires per turn. usage-update fires separately
		// from the tracker.Append callback wired in SetAttachEmitter,
		// which lands AFTER turn-complete (matching the spec's
		// "turn-complete → status-update idle → usage-update" order
		// because the harness calls Append after this cleanup runs).
		// gen_ai.agent.invocation.duration (#338): one point per
		// turn, error.type only on failed turns (stable classifier
		// kinds, not raw error text). Recorded against
		// context.Background() because runCtx is already cancelled
		// here — exemplar linkage is lost for this instrument;
		// acceptable, the terminal SSE event carries prompt_id for
		// correlation.
		a.recordInvocation(time.Since(started).Seconds(), turnErr)

		if turnErr != nil {
			a.emit(attach.EventTurnError, attach.ClassifyTurnError(turnErr))
		} else {
			a.emit(attach.EventTurnComplete, attach.TurnComplete{
				PromptID:  promptID,
				Model:     a.modelName,
				TokensIn:  promptTokens,
				TokensOut: completionTokens,
				// cost_usd intentionally omitted (nil *float64 +
				// omitempty): the agent has no pricing reference
				// (pricing lives in the harness's config). The
				// "cost deferred" signal is explicit on the wire
				// per spec v1.1.0 §2.5 — the immediately-following
				// usage-update (fired from the tracker.Append
				// callback where pricing has already been applied)
				// carries authoritative cost.
				LatencyMs: time.Since(started).Milliseconds(),
			})
		}
		a.emit(attach.EventStatusUpdate, attach.StatusUpdate{
			TurnState: attach.TurnStateIdle,
		})
	})
}

// BackgroundManager returns the SubagentManager the agent was
// constructed with via WithBackgroundManager, or nil when none was
// wired. Used by spawn tools + the runner's REPL alert display to
// reach the manager without keeping a separate reference. Callers that
// need the concrete *background.Manager recover it with
// background.ManagerOf.
func (a *Agent) BackgroundManager() SubagentManager {
	if a == nil {
		return nil
	}
	return a.bgMgr
}

// RunWithContents drives one agent turn from a pre-built conversation
// history (genai.Contents) instead of a single prompt string. The
// trailing message is treated as the new user input; everything before
// it is pre-populated into a fresh session as history events.
//
// Each call uses a fresh sessionID so prior calls don't accumulate
// state — the caller-supplied history is authoritative. Use this when
// integrating with a runtime (the AX adapter is the motivating
// example) that supplies the full conversation history per turn
// rather than relying on a session-managed prompt.
//
// The last content's Role must be genai.RoleUser; non-user trailing
// messages return an error. Empty contents return an error.
func (a *Agent) RunWithContents(ctx context.Context, contents []*genai.Content) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		if len(contents) == 0 {
			yield(nil, fmt.Errorf("agent: RunWithContents: contents is empty"))
			return
		}
		last := contents[len(contents)-1]
		if last == nil || last.Role != genai.RoleUser {
			role := ""
			if last != nil {
				role = last.Role
			}
			yield(nil, fmt.Errorf("agent: RunWithContents: last content must be a user message, got role=%q", role))
			return
		}
		history := contents[:len(contents)-1]

		// gen_ai.agent.invocation.duration (#338) for the
		// RunWithContents path too (the AX adapter drives turns
		// exclusively through here). Timing starts after argument
		// validation — a contract violation isn't a turn. turnErr
		// tracks the last error yielded so the deferred record
		// carries error.type on failed turns.
		started := time.Now()
		var turnErr error
		defer func() { a.recordInvocation(time.Since(started).Seconds(), turnErr) }()

		sessionID, err := freshSessionID()
		if err != nil {
			turnErr = err
			yield(nil, err)
			return
		}

		createResp, err := a.sessionService.Create(ctx, &session.CreateRequest{
			AppName:   a.appName,
			UserID:    a.userID,
			SessionID: sessionID,
		})
		if err != nil {
			turnErr = fmt.Errorf("agent: RunWithContents: create session: %w", err)
			yield(nil, turnErr)
			return
		}
		sess := createResp.Session
		// This call created the session, so this call deletes it once
		// the iterator finishes (success, error, or early caller stop).
		// Without the cleanup every RunWithContents turn left one
		// durable session row behind — the AX adapter calls this
		// per-turn, so rows grew without bound. Best-effort and
		// detached from ctx so an interrupted turn still cleans up;
		// a failed delete just leaves one orphan row (the pre-fix
		// behavior for every row), so the error is dropped.
		defer func() {
			_ = a.sessionService.Delete(context.WithoutCancel(ctx), &session.DeleteRequest{
				AppName:   a.appName,
				UserID:    a.userID,
				SessionID: sessionID,
			})
		}()

		for i, c := range history {
			if c == nil {
				continue
			}
			ev := session.NewEvent(fmt.Sprintf("rwc-history-%d", i))
			ev.Author = authorFor(c.Role, a.agentName)
			ev.LLMResponse = adkmodel.LLMResponse{Content: c}
			if err := a.sessionService.AppendEvent(ctx, sess, ev); err != nil {
				turnErr = fmt.Errorf("agent: RunWithContents: append history event %d: %w", i, err)
				yield(nil, turnErr)
				return
			}
		}

		// Track the cancel func so Interrupt() can fire it during the
		// turn — mirrors Run(). Clearing happens via defer here
		// since we're already inside the closure.
		runCtx, cancel := context.WithCancel(ctx)
		// defer cancel() releases the per-turn context on return —
		// without it every RunWithContents turn leaked a live
		// cancellable child ctx (#359). clearCancelInFlight is keyed
		// by generation so a late defer can't clobber a newer turn.
		cancelGen := a.setCancelInFlight(cancel)
		defer cancel()
		defer a.clearCancelInFlight(cancelGen)
		for ev, err := range a.runner.Run(runCtx, a.userID, sessionID, last, adkagent.RunConfig{
			StreamingMode: a.streaming,
		}) {
			if err != nil {
				turnErr = err
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// authorFor maps a genai role to the ADK Event.Author convention used
// by the runner: user messages → "user"; everything else (model, tool
// responses) → the agent's name.
func authorFor(role string, agentName string) string {
	if role == genai.RoleUser {
		return "user"
	}
	return agentName
}

// freshSessionID generates a unique session ID for one RunWithContents
// call. Uses crypto/rand so concurrent callers don't collide.
func freshSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("agent: generate session id: %w", err)
	}
	return "rwc-" + hex.EncodeToString(b[:]), nil
}

// Interrupt cancels the in-flight turn (if any) by invoking the
// stored cancel func. Returns true if there was something to cancel
// (a turn was in flight when called), false if the agent was idle
// (no-op). Safe for concurrent callers; the cancel is single-shot
// per turn — a second Interrupt during the same turn is a no-op.
//
// Cancellation propagates through context.Canceled to the in-flight
// model call. The agent's tools (bash, fetch_url, etc.) cancel
// their I/O when they see the cancel; the model call returns
// immediately with a partial response; the run loop emits any
// already-accumulated content and exits. Sessions, the event log,
// background subagents, and the attach registry all survive
// untouched.
func (a *Agent) Interrupt() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	cancel := a.cancelInFlight
	a.cancelInFlight = nil
	a.cancelInFlightGen = 0
	a.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// setCancelInFlight stores the cancel func for the current turn and
// returns a generation token identifying this registration. The token
// is passed back to clearCancelInFlight so a late-firing older-turn
// cleanup can't clobber a newer turn's cancel (#359). Replaces any
// prior value — concurrent Run() calls on the same Agent are not
// supported (the agent's session ID is per-Agent, so a parallel Run
// would interleave events on the same session anyway).
func (a *Agent) setCancelInFlight(cancel context.CancelFunc) uint64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelSeq++
	a.cancelInFlight = cancel
	a.cancelInFlightGen = a.cancelSeq
	return a.cancelInFlightGen
}

// turnInFlight reports whether a Run turn is currently executing on
// this agent. A turn registers its cancel func via setCancelInFlight
// once it starts driving the runner and clears it on cleanup, so a
// non-nil cancelInFlight is the in-flight signal. Used by Compact /
// Checkpoint to refuse mid-turn boundary writes (#355).
func (a *Agent) turnInFlight() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cancelInFlight != nil
}

// clearCancelInFlight clears the stored cancel func only when gen
// matches the currently-registered generation. Avoids clobbering a
// newer turn's cancel when an older turn's cleanup runs late (the
// iter.Seq2 wrapper's defer might fire after the consumer has already
// started a follow-up turn — though see the no-concurrent-Run-per-
// Agent rule).
//
// Generation matching replaces the old pointer-identity check
// (reflect.Value.Pointer() on a context.CancelFunc returns the shared
// code pointer for every context.WithCancel cancel, so the guard was
// always true and a stale cleanup could silently clobber a live
// turn's cancel — making Interrupt a no-op; see #359).
func (a *Agent) clearCancelInFlight(gen uint64) {
	if a == nil || gen == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancelInFlightGen == gen {
		a.cancelInFlight = nil
		a.cancelInFlightGen = 0
	}
}

// wrapWithCleanup wraps a session.Event iterator so cleanup runs
// when the consumer is done draining (cleanly or via early return).
// Used by Run() / RunWithContents to clear cancelInFlight when a
// turn ends.
func wrapWithCleanup(seq iter.Seq2[*session.Event, error], cleanup func()) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		defer cleanup()
		for ev, err := range seq {
			if !yield(ev, err) {
				return
			}
		}
	}
}

// SetAttachEmitter is the pre-#506 name of SetOperatorEventEmitter —
// the typed operator-event seam is transport-neutral; only its
// first consumer was attach.
//
// Deprecated: use SetOperatorEventEmitter.
func (a *Agent) SetAttachEmitter(f func(eventType string, payload any)) {
	a.SetOperatorEventEmitter(f)
}
