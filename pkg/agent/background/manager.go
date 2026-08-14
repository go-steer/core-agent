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

package background

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/agent/autonomous"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/models"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
)

// defaultMaxDepth caps how deep the background subagent tree can go by
// default. Mirrors the parallel-tool-call subagent depth cap in pkg/agent.
const defaultMaxDepth = 2

// Manager owns the lifecycle of in-process background
// subagents that the parent agent's model decides to spawn at runtime
// via the spawn_agent tool family (see background_tools.go).
//
// One manager backs one parent agent. The manager:
//
//   - constructs each spawned subagent against the parent's session
//     service (with branch isolation), the parent's permissions gate
//     (inherited wholesale), and a fresh model.LLM (one client per
//     spawn, see docs/background-subagents-design.md);
//
//   - runs each subagent in its own goroutine via RunAutonomous with
//     per-subagent budgets;
//
//   - multiplexes alert + completion messages from every running
//     subagent onto a single channel the parent's run loop drains
//     before each turn (see Agent.Run);
//
//   - enforces a configurable max-concurrent cap on top of the
//     subagent depth cap (the existing CurrentSubagentDepth check
//     from subagent.go) so a runaway model can't spawn unboundedly.
//
// Construction order is intentional: the manager is built first
// (without a parent reference), the spawn-related tools are built
// against the manager, the parent agent.New is called with those
// tools registered and the manager wired via WithBackgroundManager.
// agent.New stamps the parent back-reference onto the manager during
// construction so Spawn can read parent.SessionService / AppName /
// UserID / SessionID without the consumer plumbing them twice.
type Manager struct {
	mu sync.Mutex

	// Set by WithBackgroundManager when the parent agent is built.
	parent *agent.Agent

	// Required at construction.
	provider models.Provider
	modelID  string

	// Required for the autonomous deadlock guard and (transitively)
	// for tools that read it via the chain established at parent
	// construction. Inherited by every spawned subagent.
	gate *permissions.Gate

	// Catalog of tools the model may list in spawn_agent.tools /
	// spawn_agent.extras. Lookup is by Name(). The manager always
	// adds report_alert + report_completed regardless of what the
	// model requested.
	catalog map[string]tool.Tool

	maxDepth         int
	maxConcurrent    int
	defaultBudgets   Budgets
	defaultScheduler coretools.Scheduler

	// predefined is the operator-curated roster the model can spawn by
	// reference (spawn_agent {agent: "<name>"}), keyed by spec name.
	// Templates: each carries a persona, tool grant, model, and budgets
	// a reference spawn may only narrow. Read-only after construction.
	predefined map[string]Spec
	// templates is the richer roster of DECLARATIVE subagents — those with
	// pre-resolved instruction + model factory + toolsets (MCP + skills),
	// including rooted subagents with their own content root. Keyed by
	// name; disjoint from predefined. Populated by the declarative-subagent
	// builder via SetSubagentTemplates so the same subagent the parent can
	// call synchronously (agent.WithSubagents) is also spawnable async by
	// reference (#626, option B).
	templates map[string]SubagentTemplate
	// allowAdhoc gates inline-persona (ad-hoc) spawns. Off by default so
	// an unattended daemon's model can only spawn operator-vetted specs.
	allowAdhoc bool
	// smallModelID backs the "small" model override; empty means the
	// small tier isn't configured and "small" spawns are rejected.
	smallModelID string
	// syncWaitTimeout bounds how long a synchronous spawn (spawn_agent
	// {wait: true}, #626/D5) may hold the parent turn open before the tool
	// returns a partial/timeout result. Distinct from — and typically
	// tighter than — a subagent's own fire-and-continue wall-clock budget:
	// the subagent keeps running (its result later pushed), only the parent's
	// wait is capped. Zero means wait indefinitely (until the subagent's own
	// budget or the parent ctx ends it).
	syncWaitTimeout time.Duration
	// instanceSeq gives each predefined spec a monotonic instance
	// counter for auto-derived names ("cluster-1", "cluster-2", ...).
	instanceSeq map[string]int

	agents  map[string]*Handle
	alerts  chan Alert
	onAlert func(Alert) // optional synchronous hook, set via OnAlert
	closed  bool
}

// OnAlert installs a synchronous hook called from pushAlert before
// the channel send. Useful for surfacing alerts to side channels
// (e.g. the REPL's inline display) without competing with the model-
// context drain on Alerts() / PrependPendingAlerts. Pass nil to
// clear.
//
// The hook runs in whichever goroutine triggered the alert
// (typically a subagent's goroutine for report_alert, the Spawn
// goroutine for completion). Hooks should not block.
func (m *Manager) OnAlert(h func(Alert)) {
	m.mu.Lock()
	m.onAlert = h
	m.mu.Unlock()
}

// Handle is the lifecycle record for one spawned subagent.
// Exposed read-only via Manager.List / Manager.Get so operator
// surfaces (attach hub, TUI) and the stop_agent tool can introspect
// status without reaching into internal state.
type Handle struct {
	Name      string
	Branch    string
	StartedAt time.Time

	mu     sync.Mutex
	status Status
	result *autonomous.RunResult
	err    error
	cancel context.CancelFunc
	done   chan struct{}
	sync   syncClaim
}

// syncClaim tracks whether a spawn_agent {wait: true} caller is going
// to consume this subagent's completion inline, so the completion
// goroutine can skip the redundant terminal alert (#646). Without it a
// successful synchronous spawn surfaces its result twice: once as the
// tool result, and again as a "[Background reports]" line on the
// parent's next turn.
//
// The three states exist to resolve the race between the waiter timing
// out and the goroutine finishing. Both transitions happen under
// Handle.mu, so exactly one of them wins:
//
//   - the goroutine wins → syncConsumed, alert suppressed, and a waiter
//     that was mid-timeout learns it must deliver inline after all;
//   - the waiter wins → syncNone, and the goroutine pushes the alert as
//     it would for a fire-and-continue spawn.
type syncClaim int

const (
	// syncNone: no synchronous waiter; terminal alerts go to the parent
	// inbox as usual.
	syncNone syncClaim = iota
	// syncClaimed: a wait:true caller intends to consume the completion
	// inline and has not yet given up.
	syncClaimed
	// syncConsumed: the completion goroutine honored a claim and skipped
	// the terminal alert. The waiter MUST deliver the result inline.
	syncConsumed
)

// claimSync registers a synchronous waiter's intent to consume this
// subagent's completion inline. Returns false when the handle is
// already terminal — the completion goroutine has passed the
// suppression check, so its alert is already on its way and the waiter
// must not assume it was suppressed.
func (h *Handle) claimSync() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.done:
		return false
	default:
	}
	if h.sync != syncNone {
		// A second concurrent waiter on the same handle can't happen via
		// spawn_agent (one tool call owns one fresh handle), but SpawnRef
		// hands handles to operator surfaces too. Leave the first claim.
		return false
	}
	h.sync = syncClaimed
	return true
}

// takeSyncClaim is called by the completion goroutine just before it
// would push the terminal alert. It reports whether a synchronous
// waiter claimed this completion, in which case the alert is skipped
// and the claim is marked consumed so a racing timeout can tell that
// the result was suppressed on its behalf.
func (h *Handle) takeSyncClaim() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sync != syncClaimed {
		return false
	}
	h.sync = syncConsumed
	return true
}

// releaseSync is called by a synchronous waiter that is giving up
// (sync-wait timeout or parent cancellation). It returns true when the
// claim was still outstanding — the completion, whenever it lands, will
// alert normally. It returns false when the goroutine already consumed
// the claim in the race window, meaning the alert was suppressed and
// the waiter must deliver the result inline instead of reporting
// "still running".
func (h *Handle) releaseSync() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sync == syncClaimed {
		h.sync = syncNone
		return true
	}
	return false
}

// Status is the lifecycle state of a background subagent.
type Status int

const (
	// StatusRunning — goroutine alive, RunAutonomous loop active.
	StatusRunning Status = iota
	// StatusCompleted — RunAutonomous returned with Reason==Completed.
	StatusCompleted
	// StatusFailed — RunAutonomous returned with a non-Completed
	// terminal reason (MaxTurns, MaxCost, error, etc.).
	StatusFailed
	// StatusStopped — explicit Stop() canceled the run.
	StatusStopped
	// StatusDeferred — RunAutonomous cleanly deferred or hit a budget cap.
	StatusDeferred
)

// String renders the status for tool results and diagnostics.
func (s Status) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	case StatusStopped:
		return "stopped"
	case StatusDeferred:
		return "deferred"
	default:
		return "?"
	}
}

// Status returns the current status (safe for concurrent callers).
func (h *Handle) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

// Result returns the terminal RunResult if the subagent has finished,
// or nil if it's still running.
func (h *Handle) Result() *autonomous.RunResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.result == nil {
		return nil
	}
	r := *h.result
	return &r
}

// Err returns the terminal error if the subagent's RunAutonomous
// returned one. Nil while running or on clean completion.
func (h *Handle) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// Done returns a channel that closes when the subagent's goroutine
// exits. Use to wait for completion from the parent without polling.
func (h *Handle) Done() <-chan struct{} {
	return h.done
}

// Budgets bounds a single spawned subagent's run. Zero
// values mean no cap for that dimension. The manager's
// WithDefaultBudgets supplies the defaults; per-spawn
// overrides come from the spawn_agent tool args.
type Budgets struct {
	MaxTurns       int
	MaxCost        float64
	MaxWallclock   time.Duration
	PerTurnTimeout time.Duration
}

// Alert is one report message a spawned subagent (or the manager
// itself on completion) emitted upward to the parent.
type Alert struct {
	From      string
	Text      string
	Timestamp time.Time
	Kind      string // "alert" (default) | "completed" | "failed" | "stopped"
}

// Spec is the request shape a single Spawn call expects.
// Built from the spawn_agent tool args by the tool handler.
type Spec struct {
	Name string
	// Description is a one-line summary of what this subagent is for.
	// Surfaced to the operator catalog (#627) and — for a predefined
	// spec, which the parent can reference by name — into the
	// spawn_agent schema the model routes from (#640). Optional, but a
	// spec without one is harder for the parent to route to.
	Description string
	// SystemPrompt is the subagent's task-specific instruction.
	// Since #459 it COMPOSES: the built agent gets the layered
	// baseline (agent.CoreInstruction + provider quirks +
	// agent.AutonomousOverlay) with this text appended as a layer-5
	// block — so spawned subagents keep the compaction contract and
	// edit-safety rules they previously lost to the full replace.
	// Set ReplaceSystemPrompt for the old bare-prompt behavior.
	SystemPrompt string
	// ReplaceSystemPrompt, when true, restores the pre-#459
	// semantics: SystemPrompt fully replaces the layered baseline
	// (agent.WithInstruction). The subagent then carries NO harness
	// contract — compaction summaries arrive unexplained — so use
	// only when you are supplying your own complete prompt.
	ReplaceSystemPrompt bool
	Goal                string
	Tools               []string
	Extras              []string
	Budgets             Budgets
	// ModelID selects the model this subagent runs on. Empty means
	// "inherit the manager's model" (m.modelID — typically the parent's).
	// A predefined spec carries its operator-configured model here; a
	// per-spawn "small" override rewrites it to the manager's small-tier
	// model id (see resolvePredefinedSpec / WithSmallModelID, #626).
	ModelID string
	// Scheduler selects the between-turn scheduler the subagent's
	// RunAutonomous loop honors. Valid values: "" or "default" (use
	// the manager's WithDefaultScheduler — may itself be
	// nil), "sleep" (in-process goroutine sleep), "exit_on_defer"
	// (orchestrator-managed exit), "none" (no scheduler — the
	// schedule_next_turn tool won't be registered for this subagent).
	Scheduler string
	// Mode selects how the subagent's loop terminates. Empty derives
	// it from Scheduler; see Mode.
	Mode Mode
	// Ref is the predefined-spec name this spec was resolved from, set
	// by the reference path (resolvePredefinedSpec) and empty for an
	// ad-hoc, inline-authored one. Name is rewritten to a per-instance
	// name before the spawn ("triage-2"), so Ref is what survives to
	// say WHICH configured subagent is running — the self-spawn guard
	// matches on it (#732).
	Ref string
}

// Mode distinguishes the two things "background subagent" has always
// meant, which want opposite termination rules (#730).
//
// A BOUNDED delegation is handed one task and is done when it stops
// working: the first turn that ends without the model asking for
// another tool ends the run, and its last message is the deliverable.
// No done tool is registered — there is one way out and the model
// cannot forget to take it.
//
// A STANDING worker is a loop that watches something. A turn that
// produces only text is a status report, so the driver feeds it the
// continuation prompt and keeps going until a budget fires, the
// scheduler defers it, or it calls the return tool. This is the
// pre-#730 behavior, unchanged.
type Mode string

const (
	// ModeAuto derives the mode: standing when a scheduler is
	// installed (an agent that asks to be re-run later is by
	// definition not finished when it stops talking), bounded
	// otherwise. This is the zero value, and the default for every
	// spawn that doesn't say.
	ModeAuto Mode = ""
	// ModeBounded forces the one-task delegation contract.
	ModeBounded Mode = "bounded"
	// ModeStanding forces the watch-loop contract.
	ModeStanding Mode = "standing"
)

// ManagerOption configures NewManager.
type ManagerOption func(*bgMgrConfig)

type bgMgrConfig struct {
	provider         models.Provider
	modelID          string
	gate             *permissions.Gate
	catalog          []tool.Tool
	maxDepth         int
	maxConcurrent    int
	defaultBudgets   Budgets
	defaultScheduler coretools.Scheduler
	alertBuffer      int
	predefined       []Spec
	templates        []SubagentTemplate
	allowAdhoc       bool
	smallModelID     string
	syncWaitTimeout  time.Duration
}

// WithProvider wires the model provider + model ID used to
// build a fresh LLM client per spawn. Required.
func WithProvider(p models.Provider, modelID string) ManagerOption {
	return func(c *bgMgrConfig) { c.provider = p; c.modelID = modelID }
}

// WithGate wires the permissions gate that spawned
// subagents inherit (by reference; same instance). Required when
// running in ask/allow mode; the manager rejects spawn requests when
// the gate is in ask-mode without a prompter (same deadlock guard as
// RunAutonomous).
func WithGate(g *permissions.Gate) ManagerOption {
	return func(c *bgMgrConfig) { c.gate = g }
}

// WithCatalog registers the tool instances spawn_agent
// arguments can refer to by name. Pass the parent's already-gated
// tool list (typically tools.Default() plus any MCP/skill tools
// flattened to a single slice); the manager looks up each requested
// tool by Tool.Name(). Tools not listed here can't be requested.
func WithCatalog(tools []tool.Tool) ManagerOption {
	return func(c *bgMgrConfig) { c.catalog = tools }
}

// WithMaxDepth caps how deep the subagent tree can go.
// A spawn from a context already at depth>=N returns an error result
// instead of nesting further. Default 2.
func WithMaxDepth(n int) ManagerOption {
	return func(c *bgMgrConfig) { c.maxDepth = n }
}

// WithMaxConcurrent caps how many subagents can be Running
// at once. Spawn calls that would exceed this return a clean tool-
// result error the model can adapt to. Default 8.
func WithMaxConcurrent(n int) ManagerOption {
	return func(c *bgMgrConfig) { c.maxConcurrent = n }
}

// WithDefaultBudgets sets the budgets a spawn request
// inherits when its own per-call args don't override. Default:
// 50 turns / $1.00 / 10 minutes, no per-turn timeout.
func WithDefaultBudgets(b Budgets) ManagerOption {
	return func(c *bgMgrConfig) { c.defaultBudgets = b }
}

// WithDefaultScheduler sets the tools.Scheduler that spawned
// subagents inherit when the per-spawn Spec.Scheduler is
// empty or "default". Pass tools.SleepScheduler() for the canonical
// in-process supervisor topology where the parent runs as a long-lived
// daemon and children sleep between scans. Pass tools.ExitOnDeferScheduler()
// for orchestrator-managed deployments. Pass nil (or leave unset) to
// run subagents without between-turn pacing — the schedule_next_turn
// tool is then unavailable to those subagents.
//
// Per-spawn overrides via Spec.Scheduler win when supplied;
// see Spawn / NewSpawnAgentTool.
func WithDefaultScheduler(s coretools.Scheduler) ManagerOption {
	return func(c *bgMgrConfig) { c.defaultScheduler = s }
}

// WithAlertBuffer sets the alert channel buffer. When full,
// the oldest pending alert is dropped to make room (with a warning
// logged). Default 256.
func WithAlertBuffer(n int) ManagerOption {
	return func(c *bgMgrConfig) { c.alertBuffer = n }
}

// WithPredefinedSpecs registers the operator-curated subagent roster
// the parent's model can spawn by reference (spawn_agent {agent:
// "<name>"}, #626). Each spec is a template: its SystemPrompt, tool
// grant, model (Spec.ModelID), and budgets are what a reference spawn
// inherits and may only narrow. Names must be unique and non-empty;
// each spec needs a SystemPrompt (its persona). Goal may be empty — the
// parent supplies the task per spawn. Duplicate or invalid specs make
// NewManager return an error.
func WithPredefinedSpecs(specs []Spec) ManagerOption {
	return func(c *bgMgrConfig) { c.predefined = specs }
}

// WithAllowAdhoc permits inline-persona (ad-hoc) spawns — the parent's
// model authoring a fresh system_prompt at spawn time rather than
// referencing a predefined spec. Off by default (the daemon posture):
// an unattended daemon should only spawn operator-vetted specs. Turn it
// on for interactive/dev sessions where a human is steering.
func WithAllowAdhoc(allow bool) ManagerOption {
	return func(c *bgMgrConfig) { c.allowAdhoc = allow }
}

// WithSmallModelID sets the model id the "small" per-spawn model
// override resolves to (D2). Without it, spawns requesting model:
// "small" are rejected with ErrNoSmallModel.
func WithSmallModelID(id string) ManagerOption {
	return func(c *bgMgrConfig) { c.smallModelID = id }
}

// WithSyncWaitTimeout bounds how long a synchronous spawn (spawn_agent
// {wait: true}, #626) holds the parent turn open before returning a
// partial/timeout result. The subagent keeps running in the background
// past the timeout (its result later pushed via [Background reports]); only
// the parent's blocking wait is capped. Zero (the default) waits until the
// subagent finishes on its own budget or the parent context is canceled.
func WithSyncWaitTimeout(d time.Duration) ManagerOption {
	return func(c *bgMgrConfig) { c.syncWaitTimeout = d }
}

// NewManager builds a manager from the supplied
// options. Required: provider + modelID (WithProvider).
// The parent agent reference is established later by
// WithBackgroundManager when the parent is constructed via agent.New
// — until that wiring happens, Spawn returns ErrNoParent.
func NewManager(opts ...ManagerOption) (*Manager, error) {
	cfg := bgMgrConfig{
		maxDepth:      defaultMaxDepth,
		maxConcurrent: 8,
		alertBuffer:   256,
		defaultBudgets: Budgets{
			MaxTurns:     50,
			MaxCost:      1.0,
			MaxWallclock: 10 * time.Minute,
		},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.provider == nil {
		return nil, errors.New("background: WithProvider is required")
	}
	if cfg.modelID == "" {
		return nil, errors.New("background: WithProvider needs a non-empty modelID")
	}
	catalog := make(map[string]tool.Tool, len(cfg.catalog))
	for _, t := range cfg.catalog {
		if t == nil {
			continue
		}
		catalog[t.Name()] = t
	}
	predefined := make(map[string]Spec, len(cfg.predefined))
	for _, s := range cfg.predefined {
		if err := validatePredefinedSpec(s); err != nil {
			return nil, err
		}
		if _, dup := predefined[s.Name]; dup {
			return nil, fmt.Errorf("background: duplicate predefined subagent name %q", s.Name)
		}
		predefined[s.Name] = s
	}
	m := &Manager{
		provider:         cfg.provider,
		modelID:          cfg.modelID,
		gate:             cfg.gate,
		catalog:          catalog,
		maxDepth:         cfg.maxDepth,
		maxConcurrent:    cfg.maxConcurrent,
		defaultBudgets:   cfg.defaultBudgets,
		defaultScheduler: cfg.defaultScheduler,
		predefined:       predefined,
		templates:        make(map[string]SubagentTemplate),
		allowAdhoc:       cfg.allowAdhoc,
		smallModelID:     cfg.smallModelID,
		syncWaitTimeout:  cfg.syncWaitTimeout,
		instanceSeq:      make(map[string]int),
		agents:           make(map[string]*Handle),
		alerts:           make(chan Alert, cfg.alertBuffer),
	}
	// Declarative-subagent templates passed at construction (rare — most
	// callers use SetSubagentTemplates because the builder runs after the
	// manager exists). Validated + collision-checked the same way.
	if len(cfg.templates) > 0 {
		if err := m.SetSubagentTemplates(cfg.templates); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// validatePredefinedSpec checks a roster entry at construction time.
// Unlike validateSpec (the per-spawn check), Goal may be empty here — a
// template spec typically has its goal supplied per spawn — but Name
// and SystemPrompt are required, and Name must be branch-safe since it
// seeds auto-derived instance names.
func validatePredefinedSpec(spec Spec) error {
	if err := validateSpawnName(spec.Name); err != nil {
		return err
	}
	if strings.TrimSpace(spec.SystemPrompt) == "" {
		return fmt.Errorf("background: predefined spec %q needs a SystemPrompt", spec.Name)
	}
	return nil
}

// ErrUnknownScheduler is wrapped and returned by Spawn when a
// spec.Scheduler value isn't one of the recognized choices.
var ErrUnknownScheduler = errors.New("background: unknown scheduler choice")

// resolveScheduler maps a Spec.Scheduler string to a
// tools.Scheduler instance. Recognized values: "" / "default" / "sleep"
// / "exit_on_defer" / "none". Returns ErrUnknownScheduler for
// anything else.
func (m *Manager) resolveScheduler(choice string) (coretools.Scheduler, error) {
	switch choice {
	case "", "default":
		return m.defaultScheduler, nil
	case "sleep":
		return coretools.SleepScheduler(), nil
	case "exit_on_defer":
		return coretools.ExitOnDeferScheduler(), nil
	case "none":
		return nil, nil
	default:
		return nil, fmt.Errorf("%w: %q (allowed: default, sleep, exit_on_defer, none)", ErrUnknownScheduler, choice)
	}
}

// ErrNoParent is returned by Spawn when the manager hasn't been
// attached to an agent yet (i.e. agent.New(... WithBackgroundManager
// ...) hasn't run).
var ErrNoParent = errors.New("background: parent agent not wired (use agent.WithBackgroundManager)")

// ErrSubagentExists is returned by Spawn when a RUNNING subagent with
// the requested name is already registered. Names must be unique among
// live subagents within a manager; a handle in a terminal state
// (completed / failed / stopped / deferred) is evicted by the next
// Spawn of the same name, so names become reusable once their previous
// run has finished.
var ErrSubagentExists = errors.New("background: subagent with this name already exists")

// ErrDepthExceeded is returned by Spawn when the calling context is
// already at the max subagent depth.
var ErrDepthExceeded = errors.New("background: max subagent depth exceeded")

// ErrSelfSpawn is returned by Spawn when a subagent tries to spawn a
// subagent it is itself an instance of. Recursion at depth 1 sits
// inside any sensible depth cap, so the cap can't see it: what stops it
// is the declared-name lineage the spawn context carries (#732).
var ErrSelfSpawn = errors.New("background: a subagent may not spawn itself")

// ErrTooManyConcurrent is returned by Spawn when the manager already
// has MaxConcurrent running subagents.
var ErrTooManyConcurrent = errors.New("background: max concurrent subagents reached")

// ErrManagerClosed is returned by Spawn after Close has been called.
var ErrManagerClosed = errors.New("background: closed")

// ErrUnknownTool is wrapped and returned by Spawn when a spec.Tools
// or spec.Extras entry isn't present in the catalog.
var ErrUnknownTool = errors.New("background: unknown tool")

// AttachParent records the parent agent on the manager. Called by
// agent.New when WithBackgroundManager is set (via the
// agent.SubagentManager seam). Safe to call once; subsequent calls
// overwrite (last-writer-wins so re-construction in tests works
// cleanly).
func (m *Manager) AttachParent(a *agent.Agent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.parent = a
}

// Parent returns the agent the manager is attached to, or nil if no
// agent.New has wired it yet. Exposed for tests + diagnostics.
func (m *Manager) Parent() *agent.Agent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.parent
}

// Alerts returns the channel external consumers (the runner's REPL
// alert display goroutine, library consumers building their own UIs)
// drain to surface alerts as they arrive. The pre-turn drain inside
// Agent.Run uses PrependPendingAlerts instead — that path uses a
// non-blocking drain so it doesn't compete with this channel.
//
// Note: a single alert lands on this channel exactly once. Consumers
// must agree on who drains it; today the runner.WriteEvents alert-
// display goroutine drains for REPL display, and Agent.Run drains
// for pre-turn injection. They're separated by which path is active
// (REPL vs headless vs autonomous).
func (m *Manager) Alerts() <-chan Alert { return m.alerts }

// pushAlert enqueues a non-blocking with drop-oldest backpressure.
// When the channel is full, the oldest pending alert is dropped (and
// the drop is logged) so a stuck consumer can't deadlock a runaway
// spawner. Calls any installed OnAlert hook synchronously before the
// channel send so side-channel display consumers see every alert.
func (m *Manager) pushAlert(a Alert) {
	m.mu.Lock()
	hook := m.onAlert
	m.mu.Unlock()
	if hook != nil {
		hook(a)
	}
	for {
		select {
		case m.alerts <- a:
			return
		default:
			// Drop oldest, retry once.
			select {
			case dropped := <-m.alerts:
				log.Printf("Manager: alert buffer full, dropped: from=%q kind=%q",
					dropped.From, dropped.Kind)
			default:
				// Channel emptied between the failed send and our
				// drop attempt — try the send again.
			}
		}
	}
}

// List returns all currently-tracked handles, sorted by start time.
// Terminal handles remain in the list until Close (so operator
// surfaces can still report final status). Defensive copy of slice.
func (m *Manager) List() []*Handle {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Handle, 0, len(m.agents))
	for _, h := range m.agents {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

// Get returns the handle for the named subagent. ok=false when the
// name isn't registered.
func (m *Manager) Get(name string) (*Handle, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.agents[name]
	return h, ok
}

// Stop cancels the named subagent's context. The goroutine exits at
// the next ctx-aware checkpoint inside RunAutonomous. Returns nil
// even when the subagent is already terminal; surfaces "not found"
// when the name isn't registered.
func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	h, ok := m.agents[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("background: no subagent named %q", name)
	}
	h.mu.Lock()
	cancel := h.cancel
	if h.status == StatusRunning {
		h.status = StatusStopped
	}
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// closeDrainTimeout bounds how long Close waits for cancelled
// subagent goroutines to observe their ctx and exit. Unbounded, a
// single wedged subagent (a tool stuck in uninterruptible I/O) held
// daemon teardown hostage until the supervisor's SIGKILL (#538);
// bounded, teardown latency stays predictable and inside K8s' default
// 30s termination grace period. Per-event persistence means an
// abandoned goroutine loses nothing already committed — it dies with
// the process moments later.
// Var, not const, so tests can shrink it.
var closeDrainTimeout = 5 * time.Second

// Close stops every running subagent and prevents new spawns. Blocks
// until each goroutine has exited or closeDrainTimeout elapses —
// stragglers are abandoned (their contexts stay cancelled) and
// reported in the returned error so the caller can log them.
// Idempotent.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	handles := make([]*Handle, 0, len(m.agents))
	for _, h := range m.agents {
		handles = append(handles, h)
	}
	m.mu.Unlock()

	for _, h := range handles {
		h.mu.Lock()
		cancel := h.cancel
		h.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	drained := make(chan struct{})
	go func() {
		for _, h := range handles {
			<-h.done
		}
		close(drained)
	}()
	select {
	case <-drained:
		return nil
	case <-time.After(closeDrainTimeout):
		stuck := 0
		for _, h := range handles {
			select {
			case <-h.done:
			default:
				stuck++
			}
		}
		if stuck == 0 {
			// The last handle drained in the window between the timer
			// firing and the recount (or the select picked the timer
			// arm with both cases ready). Clean drain, not an error.
			return nil
		}
		return fmt.Errorf("background: %d subagent(s) still running after %s close drain; abandoning wait (their contexts remain cancelled)", stuck, closeDrainTimeout)
	}
}

// runningCount returns the number of handles in StatusRunning.
// Caller holds m.mu.
func (m *Manager) runningCount() int {
	n := 0
	for _, h := range m.agents {
		if h.Status() == StatusRunning {
			n++
		}
	}
	return n
}

// autoWiredSubagentTools are tool names the autonomous driver and the
// background manager wire automatically into every spawned subagent
// when applicable — the model sometimes lists them in spec.Tools
// anyway because it doesn't know they're auto-wired. Silently skipping
// them in resolveTools means a well-intentioned-but-confused request
// doesn't fail spawn with ErrUnknownTool.
//
//   - schedule_next_turn: registered by RunAutonomous whenever
//     WithScheduler is set on the child (which it is, by default,
//     when WithDefaultScheduler is configured).
//   - return_result, plus its report_done / report_completed /
//     mark_task_done aliases: registered by RunAutonomous always (the
//     loop's termination signal, #728).
//   - report_alert: registered by the manager in spawn.go so the child
//     can push back to the parent mid-run.
var autoWiredSubagentTools = func() map[string]struct{} {
	m := map[string]struct{}{
		"schedule_next_turn":             {},
		autonomous.DefaultReturnToolName: {},
		"report_alert":                   {},
	}
	for _, alias := range subagentReturnToolAliases {
		m[alias] = struct{}{}
	}
	return m
}()

// resolveTools maps spec.Tools + spec.Extras to actual tool.Tool
// instances by Name() lookup in the catalog. Unknown names return
// ErrUnknownTool. The two slices are concatenated; duplicates are
// preserved by lookup result (i.e. same instance returned twice).
// Names in autoWiredSubagentTools are silently dropped from the
// returned slice — the manager / autonomous driver register their
// real implementations elsewhere.
func (m *Manager) resolveTools(names []string) ([]tool.Tool, error) {
	out := make([]tool.Tool, 0, len(names))
	for _, n := range names {
		if _, autoWired := autoWiredSubagentTools[n]; autoWired {
			continue
		}
		t, ok := m.catalog[n]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownTool, n)
		}
		out = append(out, t)
	}
	return out, nil
}

// Compile-time check that *Manager satisfies the core seam.
var _ agent.SubagentManager = (*Manager)(nil)

// ListSubagents implements agent.SubagentManager. Returns attach-facing
// metadata for the manager's live subagents; backs Agent.AttachAgents.
func (m *Manager) ListSubagents() []attach.AgentInfo {
	if m == nil {
		return nil
	}
	parentSessionID := ""
	if p := m.Parent(); p != nil {
		parentSessionID = p.SessionID()
	}
	handles := m.List()
	out := make([]attach.AgentInfo, 0, len(handles))
	for _, h := range handles {
		ai := attach.AgentInfo{
			ID:              h.Name, // Handle keys by name
			Name:            h.Name,
			Status:          h.Status().String(),
			StartedAt:       h.StartedAt,
			ParentSessionID: parentSessionID,
		}
		if r := h.Result(); r != nil && r.FinalText != "" {
			ai.LastReport = r.FinalText
		}
		out = append(out, ai)
	}
	return out
}

// SpawnSubagent implements agent.SubagentManager. Translates an attach
// spec into a background Spec and delegates to Spawn; backs
// attachadapter.AttachSpawnSubagent.
func (m *Manager) SpawnSubagent(ctx context.Context, spec attach.SubagentSpec) (attach.SubagentSpawnResponse, error) {
	handle, err := m.Spawn(ctx, "" /* parentBranch */, Spec{
		Name:         spec.Name,
		SystemPrompt: spec.SystemPrompt,
		Goal:         spec.Goal,
		Tools:        spec.Tools,
		Extras:       spec.Extras,
		Budgets: Budgets{
			MaxTurns:     spec.Budgets.MaxTurns,
			MaxCost:      spec.Budgets.MaxCostUSD,
			MaxWallclock: time.Duration(spec.Budgets.MaxWallClockS) * time.Second,
		},
		Scheduler: spec.Scheduler,
	})
	if err != nil {
		return attach.SubagentSpawnResponse{}, err
	}
	return attach.SubagentSpawnResponse{Name: handle.Name, StartedAt: handle.StartedAt}, nil
}

// ManagerOf recovers the concrete *Manager from an agent's
// SubagentManager seam, or nil when the agent has no manager wired (or a
// different implementation). Rich callers (the runner's REPL, the
// embedded TUI) use this to reach Manager methods beyond the
// agent.SubagentManager interface (List, Get, Stop, Alerts, OnAlert).
func ManagerOf(a *agent.Agent) *Manager {
	if a == nil {
		return nil
	}
	m, _ := a.BackgroundManager().(*Manager)
	return m
}
