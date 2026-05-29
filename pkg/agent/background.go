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
	"log"
	"sort"
	"sync"
	"time"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/pkg/models"
	"github.com/go-steer/core-agent/pkg/permissions"
	coretools "github.com/go-steer/core-agent/pkg/tools"
)

// BackgroundAgentManager owns the lifecycle of in-process background
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
type BackgroundAgentManager struct {
	mu sync.Mutex

	// Set by WithBackgroundManager when the parent agent is built.
	parent *Agent

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
	defaultBudgets   BackgroundBudgets
	defaultScheduler coretools.Scheduler

	agents  map[string]*BackgroundHandle
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
func (m *BackgroundAgentManager) OnAlert(h func(Alert)) {
	m.mu.Lock()
	m.onAlert = h
	m.mu.Unlock()
}

// BackgroundHandle is the lifecycle record for one spawned subagent.
// Exposed read-only via Manager.List / Manager.Get so the parent
// model's check_agent tool can introspect status without reaching
// into internal state.
type BackgroundHandle struct {
	Name      string
	Branch    string
	StartedAt time.Time

	mu     sync.Mutex
	status BackgroundStatus
	result *RunResult
	err    error
	cancel context.CancelFunc
	done   chan struct{}
}

// BackgroundStatus is the lifecycle state of a background subagent.
type BackgroundStatus int

const (
	// StatusRunning — goroutine alive, RunAutonomous loop active.
	StatusRunning BackgroundStatus = iota
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
func (s BackgroundStatus) String() string {
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
func (h *BackgroundHandle) Status() BackgroundStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

// Result returns the terminal RunResult if the subagent has finished,
// or nil if it's still running.
func (h *BackgroundHandle) Result() *RunResult {
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
func (h *BackgroundHandle) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// Done returns a channel that closes when the subagent's goroutine
// exits. Use to wait for completion from the parent without polling.
func (h *BackgroundHandle) Done() <-chan struct{} {
	return h.done
}

// BackgroundBudgets bounds a single spawned subagent's run. Zero
// values mean no cap for that dimension. The manager's
// WithBackgroundDefaultBudgets supplies the defaults; per-spawn
// overrides come from the spawn_agent tool args.
type BackgroundBudgets struct {
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

// BackgroundSpec is the request shape a single Spawn call expects.
// Built from the spawn_agent tool args by the tool handler.
type BackgroundSpec struct {
	Name         string
	SystemPrompt string
	Goal         string
	Tools        []string
	Extras       []string
	Budgets      BackgroundBudgets
	// Scheduler selects the between-turn scheduler the subagent's
	// RunAutonomous loop honors. Valid values: "" or "default" (use
	// the manager's WithBackgroundDefaultScheduler — may itself be
	// nil), "sleep" (in-process goroutine sleep), "exit_on_defer"
	// (orchestrator-managed exit), "none" (no scheduler — the
	// schedule_next_turn tool won't be registered for this subagent).
	Scheduler string
}

// BackgroundManagerOption configures NewBackgroundAgentManager.
type BackgroundManagerOption func(*bgMgrConfig)

type bgMgrConfig struct {
	provider         models.Provider
	modelID          string
	gate             *permissions.Gate
	catalog          []tool.Tool
	maxDepth         int
	maxConcurrent    int
	defaultBudgets   BackgroundBudgets
	defaultScheduler coretools.Scheduler
	alertBuffer      int
}

// WithBackgroundProvider wires the model provider + model ID used to
// build a fresh LLM client per spawn. Required.
func WithBackgroundProvider(p models.Provider, modelID string) BackgroundManagerOption {
	return func(c *bgMgrConfig) { c.provider = p; c.modelID = modelID }
}

// WithBackgroundGate wires the permissions gate that spawned
// subagents inherit (by reference; same instance). Required when
// running in ask/allow mode; the manager rejects spawn requests when
// the gate is in ask-mode without a prompter (same deadlock guard as
// RunAutonomous).
func WithBackgroundGate(g *permissions.Gate) BackgroundManagerOption {
	return func(c *bgMgrConfig) { c.gate = g }
}

// WithBackgroundCatalog registers the tool instances spawn_agent
// arguments can refer to by name. Pass the parent's already-gated
// tool list (typically tools.Default() plus any MCP/skill tools
// flattened to a single slice); the manager looks up each requested
// tool by Tool.Name(). Tools not listed here can't be requested.
func WithBackgroundCatalog(tools []tool.Tool) BackgroundManagerOption {
	return func(c *bgMgrConfig) { c.catalog = tools }
}

// WithBackgroundMaxDepth caps how deep the subagent tree can go.
// A spawn from a context already at depth>=N returns an error result
// instead of nesting further. Default 2.
func WithBackgroundMaxDepth(n int) BackgroundManagerOption {
	return func(c *bgMgrConfig) { c.maxDepth = n }
}

// WithBackgroundMaxConcurrent caps how many subagents can be Running
// at once. Spawn calls that would exceed this return a clean tool-
// result error the model can adapt to. Default 8.
func WithBackgroundMaxConcurrent(n int) BackgroundManagerOption {
	return func(c *bgMgrConfig) { c.maxConcurrent = n }
}

// WithBackgroundDefaultBudgets sets the budgets a spawn request
// inherits when its own per-call args don't override. Default:
// 50 turns / $1.00 / 10 minutes, no per-turn timeout.
func WithBackgroundDefaultBudgets(b BackgroundBudgets) BackgroundManagerOption {
	return func(c *bgMgrConfig) { c.defaultBudgets = b }
}

// WithBackgroundDefaultScheduler sets the tools.Scheduler that spawned
// subagents inherit when the per-spawn BackgroundSpec.Scheduler is
// empty or "default". Pass tools.SleepScheduler() for the canonical
// in-process supervisor topology where the parent runs as a long-lived
// daemon and children sleep between scans. Pass tools.ExitOnDeferScheduler()
// for orchestrator-managed deployments. Pass nil (or leave unset) to
// run subagents without between-turn pacing — the schedule_next_turn
// tool is then unavailable to those subagents.
//
// Per-spawn overrides via BackgroundSpec.Scheduler win when supplied;
// see Spawn / NewSpawnAgentTool.
func WithBackgroundDefaultScheduler(s coretools.Scheduler) BackgroundManagerOption {
	return func(c *bgMgrConfig) { c.defaultScheduler = s }
}

// WithBackgroundAlertBuffer sets the alert channel buffer. When full,
// the oldest pending alert is dropped to make room (with a warning
// logged). Default 256.
func WithBackgroundAlertBuffer(n int) BackgroundManagerOption {
	return func(c *bgMgrConfig) { c.alertBuffer = n }
}

// NewBackgroundAgentManager builds a manager from the supplied
// options. Required: provider + modelID (WithBackgroundProvider).
// The parent agent reference is established later by
// WithBackgroundManager when the parent is constructed via agent.New
// — until that wiring happens, Spawn returns ErrNoParent.
func NewBackgroundAgentManager(opts ...BackgroundManagerOption) (*BackgroundAgentManager, error) {
	cfg := bgMgrConfig{
		maxDepth:      defaultSubagentMaxDepth,
		maxConcurrent: 8,
		alertBuffer:   256,
		defaultBudgets: BackgroundBudgets{
			MaxTurns:     50,
			MaxCost:      1.0,
			MaxWallclock: 10 * time.Minute,
		},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.provider == nil {
		return nil, errors.New("agent: BackgroundAgentManager: WithBackgroundProvider is required")
	}
	if cfg.modelID == "" {
		return nil, errors.New("agent: BackgroundAgentManager: WithBackgroundProvider needs a non-empty modelID")
	}
	catalog := make(map[string]tool.Tool, len(cfg.catalog))
	for _, t := range cfg.catalog {
		if t == nil {
			continue
		}
		catalog[t.Name()] = t
	}
	return &BackgroundAgentManager{
		provider:         cfg.provider,
		modelID:          cfg.modelID,
		gate:             cfg.gate,
		catalog:          catalog,
		maxDepth:         cfg.maxDepth,
		maxConcurrent:    cfg.maxConcurrent,
		defaultBudgets:   cfg.defaultBudgets,
		defaultScheduler: cfg.defaultScheduler,
		agents:           make(map[string]*BackgroundHandle),
		alerts:           make(chan Alert, cfg.alertBuffer),
	}, nil
}

// ErrUnknownScheduler is wrapped and returned by Spawn when a
// spec.Scheduler value isn't one of the recognized choices.
var ErrUnknownScheduler = errors.New("agent: BackgroundAgentManager: unknown scheduler choice")

// resolveScheduler maps a BackgroundSpec.Scheduler string to a
// tools.Scheduler instance. Recognized values: "" / "default" / "sleep"
// / "exit_on_defer" / "none". Returns ErrUnknownScheduler for
// anything else.
func (m *BackgroundAgentManager) resolveScheduler(choice string) (coretools.Scheduler, error) {
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
var ErrNoParent = errors.New("agent: BackgroundAgentManager: parent agent not wired (use agent.WithBackgroundManager)")

// ErrSubagentExists is returned by Spawn when a subagent with the
// requested name is already registered (running or terminal). Names
// must be unique within a manager.
var ErrSubagentExists = errors.New("agent: BackgroundAgentManager: subagent with this name already exists")

// ErrDepthExceeded is returned by Spawn when the calling context is
// already at the max subagent depth.
var ErrDepthExceeded = errors.New("agent: BackgroundAgentManager: max subagent depth exceeded")

// ErrTooManyConcurrent is returned by Spawn when the manager already
// has MaxConcurrent running subagents.
var ErrTooManyConcurrent = errors.New("agent: BackgroundAgentManager: max concurrent subagents reached")

// ErrManagerClosed is returned by Spawn after Close has been called.
var ErrManagerClosed = errors.New("agent: BackgroundAgentManager: closed")

// ErrUnknownTool is wrapped and returned by Spawn when a spec.Tools
// or spec.Extras entry isn't present in the catalog.
var ErrUnknownTool = errors.New("agent: BackgroundAgentManager: unknown tool")

// attachParent records the parent agent on the manager. Called by
// agent.New when WithBackgroundManager is set. Safe to call once;
// subsequent calls overwrite (last-writer-wins so re-construction in
// tests works cleanly).
func (m *BackgroundAgentManager) attachParent(a *Agent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.parent = a
}

// Parent returns the agent the manager is attached to, or nil if no
// agent.New has wired it yet. Exposed for tests + diagnostics.
func (m *BackgroundAgentManager) Parent() *Agent {
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
func (m *BackgroundAgentManager) Alerts() <-chan Alert { return m.alerts }

// pushAlert enqueues a non-blocking with drop-oldest backpressure.
// When the channel is full, the oldest pending alert is dropped (and
// the drop is logged) so a stuck consumer can't deadlock a runaway
// spawner. Calls any installed OnAlert hook synchronously before the
// channel send so side-channel display consumers see every alert.
func (m *BackgroundAgentManager) pushAlert(a Alert) {
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
				log.Printf("BackgroundAgentManager: alert buffer full, dropped: from=%q kind=%q",
					dropped.From, dropped.Kind)
			default:
				// Channel emptied between the failed send and our
				// drop attempt — try the send again.
			}
		}
	}
}

// List returns all currently-tracked handles, sorted by start time.
// Terminal handles remain in the list until Close (so check_agent
// can return final status). Defensive copy of slice.
func (m *BackgroundAgentManager) List() []*BackgroundHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*BackgroundHandle, 0, len(m.agents))
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
func (m *BackgroundAgentManager) Get(name string) (*BackgroundHandle, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.agents[name]
	return h, ok
}

// Stop cancels the named subagent's context. The goroutine exits at
// the next ctx-aware checkpoint inside RunAutonomous. Returns nil
// even when the subagent is already terminal; surfaces "not found"
// when the name isn't registered.
func (m *BackgroundAgentManager) Stop(name string) error {
	m.mu.Lock()
	h, ok := m.agents[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("agent: BackgroundAgentManager: no subagent named %q", name)
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

// Close stops every running subagent and prevents new spawns. Blocks
// until each goroutine has exited so callers don't race with shutdown.
// Idempotent.
func (m *BackgroundAgentManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	handles := make([]*BackgroundHandle, 0, len(m.agents))
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
	for _, h := range handles {
		<-h.done
	}
	return nil
}

// runningCount returns the number of handles in StatusRunning.
// Caller holds m.mu.
func (m *BackgroundAgentManager) runningCount() int {
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
//     when WithBackgroundDefaultScheduler is configured).
//   - report_done: registered by RunAutonomous always (the loop's
//     termination signal).
//   - report_alert / report_completed: registered by the manager in
//     background_spawn.go so the child can push back to the parent.
var autoWiredSubagentTools = map[string]struct{}{
	"schedule_next_turn": {},
	"report_done":        {},
	"report_alert":       {},
	"report_completed":   {},
}

// resolveTools maps spec.Tools + spec.Extras to actual tool.Tool
// instances by Name() lookup in the catalog. Unknown names return
// ErrUnknownTool. The two slices are concatenated; duplicates are
// preserved by lookup result (i.e. same instance returned twice).
// Names in autoWiredSubagentTools are silently dropped from the
// returned slice — the manager / autonomous driver register their
// real implementations elsewhere.
func (m *BackgroundAgentManager) resolveTools(names []string) ([]tool.Tool, error) {
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
