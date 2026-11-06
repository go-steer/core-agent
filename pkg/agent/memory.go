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

// Long-term memory on the agent loop (#948, docs/memory-service-design.md
// "Model-facing tools, preload, and governance"):
//
//   - WithMemory registers recall_memory / persist_memory / forget_memory
//     (pkg/memory/memtools) and bridges the Router to ADK's
//     runner.Config.MemoryService.
//   - Preload recalls once per user turn, in the pre-turn pipeline, and
//     prepends a <memory_context> block to the USER message. The system
//     instruction is never touched, so prompt caches stay warm.
//   - Consolidation is opt-in: "turn_end" starts in the background
//     after each successful turn, over that turn's events; "session_end"
//     runs when the host calls EndSession, which also waits for any
//     turn_end consolidation still running.
//   - Every completed write appends a content-less memory/* event to the
//     session transcript, so the audit trail is durable and the model
//     never sees it as history.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/memory"
	"github.com/go-steer/core-agent/v3/pkg/memory/memtools"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

// Memory preload modes (MemorySettings.Preload).
const (
	MemoryPreloadOff          = "off"
	MemoryPreloadSessionStart = "session_start"
	MemoryPreloadTurnStart    = "turn_start"
)

// Memory consolidation modes (MemorySettings.Consolidate).
const (
	MemoryConsolidateOff        = "off"
	MemoryConsolidateTurnEnd    = "turn_end"
	MemoryConsolidateSessionEnd = "session_end"
)

const (
	// memoryPreloadTimeout bounds the pre-turn recall. A slow memory
	// service delays the turn by at most this much, then the turn runs
	// without the block.
	memoryPreloadTimeout = 20 * time.Second
	// memoryConsolidateTimeout bounds one turn_end or session_end
	// consolidation.
	memoryConsolidateTimeout = 2 * time.Minute
)

// MemorySettings configures how an agent uses its memory Router.
type MemorySettings struct {
	// Preload is MemoryPreloadOff (default), MemoryPreloadSessionStart
	// (the first turn of a session only) or MemoryPreloadTurnStart
	// (every user turn).
	Preload string
	// PreloadTopK bounds the preload recall and is the recall_memory
	// default. Zero uses the Router's default.
	PreloadTopK int
	// Consolidate is MemoryConsolidateOff (default),
	// MemoryConsolidateTurnEnd or MemoryConsolidateSessionEnd.
	Consolidate string
	// ConsolidateMount is the writable mount consolidation writes to.
	// Empty means "agent".
	ConsolidateMount string
	// DisableTools keeps the Router (preload, consolidation, the ADK
	// MemoryService bridge) but registers no model-facing memory tools.
	DisableTools bool
	// DisabledTools lists individual memory tool names (for example from
	// config.Tools.Disable) to withhold when registering memory tools.
	DisabledTools []string
}

// WithMemory gives the agent long-term memory backed by r. A nil r is a
// no-op. Tools are registered against the agent's gate (WithGate); the
// gate is resolved after every option is applied, so option order does
// not matter.
func WithMemory(r *memory.Router, s MemorySettings) Option {
	return func(o *options) {
		o.memoryRouter = r
		o.memorySettings = s
	}
}

// agentMemory is the per-Agent memory state.
type agentMemory struct {
	router   *memory.Router
	settings MemorySettings

	mu           sync.Mutex
	turnStart    time.Time
	turnUser     string
	preloadDone  bool
	pendingAudit []queuedOutOfBandEvent
	drainMu      sync.Mutex
	// consolidating tracks turn_end consolidations running in the
	// background; EndSession waits for them.
	consolidating sync.WaitGroup
}

type subagentMemoryAuditKey struct{}

// wireMemory resolves WithMemory inside New: it binds the transcript
// audit recorder (late, through ref, because the Agent does not exist
// yet) and appends the memory tools to o.tools. Called before the
// serializer and instrumenter wrap the tool list.
func wireMemory(o *options, ref func() *Agent) (*agentMemory, error) {
	if o.memoryRouter == nil {
		return nil, nil
	}
	if err := validateMemorySettings(o.memorySettings); err != nil {
		return nil, err
	}
	router := o.memoryRouter.With(memory.AppendAuditRecorder(func(ctx context.Context, author string, payload map[string]any) error {
		if fn, ok := ctx.Value(subagentMemoryAuditKey{}).(func(string, map[string]any)); ok && fn != nil {
			fn(author, payload)
			return nil
		}
		if a := ref(); a != nil {
			a.queueMemoryAudit(author, payload)
		}
		return nil
	}))
	o.memoryRouter = router
	if !o.memorySettings.DisableTools {
		ts, err := memtools.New(router, memtools.Options{Gate: o.gate, DefaultTopK: o.memorySettings.PreloadTopK})
		if err != nil {
			return nil, fmt.Errorf("agent: memory tools: %w", err)
		}
		for _, t := range ts {
			if t != nil && !slices.Contains(o.memorySettings.DisabledTools, t.Name()) {
				o.tools = append(o.tools, t)
			}
		}
	}
	return &agentMemory{router: router, settings: o.memorySettings}, nil
}

func validateMemorySettings(s MemorySettings) error {
	switch s.Preload {
	case "", MemoryPreloadOff, MemoryPreloadSessionStart, MemoryPreloadTurnStart:
	default:
		return fmt.Errorf("agent: unknown memory preload %q", s.Preload)
	}
	switch s.Consolidate {
	case "", MemoryConsolidateOff, MemoryConsolidateTurnEnd, MemoryConsolidateSessionEnd:
	default:
		return fmt.Errorf("agent: unknown memory consolidate %q", s.Consolidate)
	}
	return nil
}

// memoryService is runner.Config.MemoryService: the Router bridged to
// ADK so a third-party tool calling toolCtx.SearchMemory reads the same
// mounted scopes. Nil when memory is off.
func memoryService(o *options) adkmemory.Service {
	if o.memoryRouter == nil {
		return nil
	}
	mount := o.memorySettings.ConsolidateMount
	if o.memorySettings.Consolidate == "" || o.memorySettings.Consolidate == MemoryConsolidateOff {
		mount = memory.SubagentScopeOff
	}
	return memory.AsADKService(o.memoryRouter, mount)
}

func (a *Agent) memoryRunnerService() adkmemory.Service {
	if a == nil || a.memory == nil {
		return nil
	}
	mount := a.memory.settings.ConsolidateMount
	if a.memory.settings.Consolidate == "" || a.memory.settings.Consolidate == MemoryConsolidateOff {
		mount = memory.SubagentScopeOff
	}
	return memory.AsADKService(a.memory.router, mount)
}

// MemoryRouter returns the Router this agent uses (with its transcript
// audit recorder attached), or nil when memory is off.
func (a *Agent) MemoryRouter() *memory.Router {
	if a == nil || a.memory == nil {
		return nil
	}
	return a.memory.router
}

// MemorySettings returns the MemorySettings this agent was constructed
// with, or the zero value when memory is off.
func (a *Agent) MemorySettings() MemorySettings {
	if a == nil || a.memory == nil {
		return MemorySettings{}
	}
	return a.memory.settings
}

// memoryUser resolves the user_id for partition_by_user mounts: an id
// the host already stamped wins, then the turn's originator, then the
// verified caller on ctx, then the session user.
func (a *Agent) memoryUser(ctx context.Context, originator string) context.Context {
	if memory.UserIDFromContext(ctx) != "" {
		return ctx
	}
	uid := originator
	if uid == "" {
		if c, ok := auth.CallerFromContext(ctx); ok {
			uid = c.Identity
		}
	}
	if uid == "" {
		uid = a.userID
	}
	if uid == "" {
		return ctx
	}
	return memory.WithUserID(ctx, uid)
}

// memoryPreloadStep is the pre-turn pipeline stage. On every turn,
// including a RunContent resume, it waits for any prior turn_end
// consolidation to finish and records the turn start and the turn's
// memory user, which turn_end consolidation reads. On a text turn it
// then prepends the recalled block.
func (a *Agent) memoryPreloadStep(tp *turnPrep) error {
	m := a.memory
	if m == nil {
		return nil
	}
	if err := a.waitConsolidating(tp.ctx); err != nil {
		return err
	}
	ctx := a.memoryUser(tp.ctx, tp.drained.originator.Identity)
	m.mu.Lock()
	m.turnStart = time.Now()
	m.turnUser = memory.UserIDFromContext(ctx)
	m.mu.Unlock()
	if tp.hasContent {
		return nil
	}
	query := strings.TrimSpace(tp.rawPrompt)
	if query == "" {
		query = strings.TrimSpace(strings.Join(tp.drained.texts, "\n"))
	}
	if query == "" || !a.shouldPreload(ctx) {
		return nil
	}
	// Preload honours the same per-mount deny rules recall_memory does.
	mounts := memtools.PermittedRecallMounts(ctx, a.gate, m.router, query)
	if len(mounts) == 0 {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, memoryPreloadTimeout)
	defer cancel()
	items, _, err := m.router.Recall(rctx, query, mounts, nil, m.settings.PreloadTopK)
	if err != nil {
		slog.Warn("core-agent: memory preload failed; running the turn without recalled memories",
			"err", err, "session", a.sessionID)
		return nil
	}
	a.markPreloaded()
	if block := memory.FormatContext(items); block != "" {
		tp.prompt = block + "\n\n" + tp.prompt
	}
	return nil
}

// shouldPreload reports whether this turn preloads. session_start fires
// once per Agent, and only when the session has no model-visible
// history yet, so a process resuming a long session does not preload on
// its first turn. The once-only flag is set by markPreloaded after a
// recall actually ran, so an empty first turn or a failed recall does
// not use it up.
func (a *Agent) shouldPreload(ctx context.Context) bool {
	m := a.memory
	switch m.settings.Preload {
	case MemoryPreloadTurnStart:
		return true
	case MemoryPreloadSessionStart:
	default:
		return false
	}
	m.mu.Lock()
	done := m.preloadDone
	m.mu.Unlock()
	if done {
		return false
	}
	sess, err := a.fetchMemorySession(ctx)
	if err != nil {
		// No session yet (AutoCreateSession makes it on the first
		// turn) is the session_start case itself.
		return true
	}
	for ev := range sess.Events().All() {
		if ev != nil && ev.Content != nil && ev.Content.Role != "" {
			a.markPreloaded()
			return false
		}
	}
	return true
}

func (a *Agent) markPreloaded() {
	a.memory.mu.Lock()
	a.memory.preloadDone = true
	a.memory.mu.Unlock()
}

// finishMemoryTurn runs from the post-turn cleanup, after the
// out-of-band drain. It flushes the memory/* rows the turn queued, then
// starts turn_end consolidation over this turn's events in the
// background, so the turn's terminal frame does not wait on the memory
// service. A failed turn is not consolidated. EndSession waits for it.
func (a *Agent) finishMemoryTurn(turnErr error) {
	m := a.memory
	if m == nil {
		return
	}
	a.drainMemoryAudit()
	if turnErr != nil || m.settings.Consolidate != MemoryConsolidateTurnEnd {
		return
	}
	m.mu.Lock()
	since, uid := m.turnStart, m.turnUser
	m.mu.Unlock()
	var sincePtr *time.Time
	if !since.IsZero() {
		sincePtr = &since
	}
	ctx := context.Background()
	if uid != "" {
		ctx = memory.WithUserID(ctx, uid)
	}
	m.consolidating.Add(1)
	go func() {
		defer m.consolidating.Done()
		if err := a.consolidateMemory(ctx, sincePtr); err != nil {
			slog.Warn("core-agent: memory turn_end consolidation failed", "err", err, "session", a.sessionID)
		}
	}()
}

// EndSession finishes the session's memory work. It waits (bounded by
// ctx) for any turn_end consolidation still running, runs session_end
// consolidation over the whole session when memory is configured with
// MemoryConsolidateSessionEnd, and flushes queued memory audit rows
// unless a turn is in flight (that turn's cleanup flushes them). Hosts
// call it before a process exits or a session is evicted. The user for
// partition_by_user mounts is the one on ctx (memory.WithUserID), else
// the agent's session user. A nil Agent or one without memory is a
// no-op.
func (a *Agent) EndSession(ctx context.Context) error {
	if a == nil || a.memory == nil {
		return nil
	}
	if err := a.waitConsolidating(ctx); err != nil {
		return err
	}
	var err error
	if a.memory.settings.Consolidate == MemoryConsolidateSessionEnd {
		err = a.consolidateMemory(ctx, nil)
	}
	if !a.turnInFlight() {
		a.drainMemoryAudit()
	}
	return err
}

func (a *Agent) waitConsolidating(ctx context.Context) error {
	waited := make(chan struct{})
	go func() {
		a.memory.consolidating.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Agent) consolidateMemory(ctx context.Context, since *time.Time) error {
	ctx, cancel := context.WithTimeout(a.memoryUser(ctx, ""), memoryConsolidateTimeout)
	defer cancel()
	sess, err := a.fetchMemorySession(ctx)
	if err != nil {
		return err
	}
	_, err = a.memory.router.ConsolidateSession(ctx, a.memory.settings.ConsolidateMount, sess, since)
	if errors.Is(err, memory.ErrAuditFailed) {
		slog.Warn("core-agent: memory consolidated but its audit row failed", "err", err, "session", a.sessionID)
		return nil
	}
	return err
}

// fetchMemorySession reads the full, unwrapped session (the audit view,
// not the compacted one), which is what consolidation extracts from.
func (a *Agent) fetchMemorySession(ctx context.Context) (session.Session, error) {
	resp, err := a.sessionService.Get(ctx, &session.GetRequest{
		AppName:   a.appName,
		UserID:    a.userID,
		SessionID: a.sessionID,
	})
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Session == nil {
		return nil, session.ErrNotFound
	}
	return resp.Session, nil
}

// queueMemoryAudit is the Router's transcript audit recorder. The row is
// content-less (author memory/<op>, payload in CustomMetadata), which
// ADK's contents processor skips, so it never becomes model history.
//
// With an event log it rides the shared out-of-band queue. Without one
// it goes through the agent's own session.Service on a queue with the
// same write window, so the trail lands in whichever store is wired
// (docs/memory-service-design.md "Alignment seam", item 4).
func (a *Agent) queueMemoryAudit(author string, payload map[string]any) {
	ev := session.NewEvent(context.Background(), "memory-audit")
	ev.Author = author
	ev.CustomMetadata = payload
	if a.eventLog != nil {
		a.queueOutOfBandEvent(ev)
		return
	}
	m := a.memory
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pendingAudit = append(m.pendingAudit, queuedOutOfBandEvent{ev: ev})
	m.mu.Unlock()
	if !a.turnInFlight() {
		a.drainMemoryAudit()
	}
}

// drainMemoryAudit writes queued memory/* rows through the session
// service when no event log is wired. Rows that fail are retried on the
// next drain up to maxOutOfBandEventAttempts, then logged and dropped.
func (a *Agent) drainMemoryAudit() {
	m := a.memory
	if m == nil || a.sessionService == nil {
		return
	}
	m.drainMu.Lock()
	defer m.drainMu.Unlock()
	m.mu.Lock()
	pending := m.pendingAudit
	m.pendingAudit = nil
	m.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	ctx := context.Background()
	for i := range pending {
		err := a.appendMemoryAudit(ctx, pending[i].ev)
		if err == nil {
			continue
		}
		pending[i].attempts++
		if pending[i].attempts < maxOutOfBandEventAttempts {
			m.mu.Lock()
			m.pendingAudit = append(append([]queuedOutOfBandEvent(nil), pending[i:]...), m.pendingAudit...)
			m.mu.Unlock()
			slog.Warn("core-agent: memory audit row not written; will retry", "err", err, "session", a.sessionID)
			return
		}
		slog.Error("core-agent: memory audit row dropped after retries", "err", err, "author", pending[i].ev.Author, "session", a.sessionID)
	}
}

func (a *Agent) appendMemoryAudit(ctx context.Context, ev *session.Event) error {
	return appendSessionAudit(ctx, a.sessionService, a.appName, a.userID, a.sessionID, ev)
}

func appendSessionAudit(ctx context.Context, svc session.Service, appName, userID, sessionID string, ev *session.Event) error {
	resp, err := svc.Get(ctx, &session.GetRequest{
		AppName: appName, UserID: userID, SessionID: sessionID, NumRecentEvents: 1,
	})
	if err != nil {
		return err
	}
	if resp == nil || resp.Session == nil {
		return session.ErrNotFound
	}
	if last := resp.Session.LastUpdateTime(); !last.IsZero() && ev.Timestamp.Before(last) {
		ev.Timestamp = last
	}
	return svc.AppendEvent(ctx, resp.Session, ev)
}

// prepareSubagentMemory sets up per-invocation memory context, audit
// queueing, preload, and post-invocation consolidation/flush for a
// synchronous NewSubagentTool delegation. Because a subagent *Agent may
// be invoked concurrently or shared across sessions in multi-session
// mode, all state lives on the returned ctx and finish closure rather
// than mutating a.
func (a *Agent) prepareSubagentMemory(
	ctx context.Context,
	svc session.Service,
	appName, userID, sessionID, reqText string,
	gate *permissions.Gate,
) (context.Context, string, func(error)) {
	if a == nil || a.memory == nil {
		return ctx, reqText, func(error) {}
	}
	m := a.memory
	ctx = a.memoryUser(ctx, userID)

	var (
		auditMu sync.Mutex
		pending []*session.Event
	)
	ctx = context.WithValue(ctx, subagentMemoryAuditKey{}, func(author string, payload map[string]any) {
		ev := session.NewEvent(context.Background(), "memory-audit")
		ev.Author = author
		ev.CustomMetadata = payload
		auditMu.Lock()
		pending = append(pending, ev)
		auditMu.Unlock()
	})

	// NewSubagentTool (subagent.go) allocates a fresh invocation-unique
	// subSessionID on every synchronous subagent tool call and runs a single
	// runner.Run turn inside that child session, so session_start and
	// turn_start preload semantics are identical on this path.
	query := strings.TrimSpace(reqText)
	if query != "" && (m.settings.Preload == MemoryPreloadTurnStart || m.settings.Preload == MemoryPreloadSessionStart) {
		g := gate
		if g == nil {
			g = a.gate
		}
		if mounts := memtools.PermittedRecallMounts(ctx, g, m.router, query); len(mounts) > 0 {
			rctx, cancel := context.WithTimeout(ctx, memoryPreloadTimeout)
			items, _, err := m.router.Recall(rctx, query, mounts, nil, m.settings.PreloadTopK)
			cancel()
			if err != nil {
				slog.Warn("core-agent: subagent memory preload failed; running without recalled memories",
					"err", err, "subagent", a.agentName, "session", sessionID)
			} else if block := memory.FormatContext(items); block != "" {
				reqText = block + "\n\n" + reqText
			}
		}
	}

	flushAudit := func() {
		auditMu.Lock()
		batch := pending
		pending = nil
		auditMu.Unlock()
		bg := context.Background()
		for _, ev := range batch {
			if err := appendSessionAudit(bg, svc, appName, userID, sessionID, ev); err != nil {
				slog.Warn("core-agent: subagent memory audit row failed", "err", err, "subagent", a.agentName, "session", sessionID)
			}
		}
	}

	finish := func(turnErr error) {
		flushAudit()
		if turnErr != nil {
			return
		}
		if m.settings.Consolidate != MemoryConsolidateTurnEnd && m.settings.Consolidate != MemoryConsolidateSessionEnd {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, memoryConsolidateTimeout)
		defer cancel()
		resp, err := svc.Get(cctx, &session.GetRequest{
			AppName: appName, UserID: userID, SessionID: sessionID,
		})
		if err != nil || resp == nil || resp.Session == nil {
			return
		}
		if _, err := m.router.ConsolidateSession(cctx, m.settings.ConsolidateMount, resp.Session, nil); err != nil && !errors.Is(err, memory.ErrAuditFailed) {
			slog.Warn("core-agent: subagent memory consolidation failed", "err", err, "subagent", a.agentName, "session", sessionID)
		}
		flushAudit()
	}
	return ctx, reqText, finish
}

// OpenConfigMemoryRouter opens a memory.Router and resolves MemorySettings
// from cfg.Memory and cfg.AgentEngine (#948). When memory is unconfigured,
// it returns (nil, MemorySettings{}, noopClose, nil).
func OpenConfigMemoryRouter(
	ctx context.Context,
	cfg *config.Config,
	defaultAgentID string,
	getenv func(string) string,
) (*memory.Router, MemorySettings, func(), error) {
	noop := func() {}
	if cfg == nil || cfg.Memory == nil {
		return nil, MemorySettings{}, noop, nil
	}
	ref, err := cfg.ResolveMemoryRef(getenv)
	if err != nil {
		return nil, MemorySettings{}, noop, err
	}
	if ref == "" {
		return nil, MemorySettings{}, noop, nil
	}
	m := cfg.Memory
	ns := m.ResolveNamespace(getenv)
	agentID := strings.TrimSpace(defaultAgentID)
	if agentID == "" {
		agentID = "default"
	}
	var mounts []memory.MountTemplate
	agentMode := config.MemoryModeRW
	if m.Agent != nil {
		if id := strings.TrimSpace(m.Agent.ID); id != "" {
			agentID = id
		}
		if m.Agent.Mode != "" {
			agentMode = m.Agent.Mode
		}
	}
	if agentMode != config.MemoryModeOff {
		mt := memory.AgentMount(ns, agentID, memory.AccessMode(agentMode))
		if m.Agent != nil {
			mt.PartitionByUser = m.Agent.PartitionByUser
			if m.Agent.WriteMode != "" {
				mt.WriteMode = memory.WriteMode(m.Agent.WriteMode)
			}
			ttl, err := m.Agent.ResolvedTTL()
			if err != nil {
				return nil, MemorySettings{}, noop, err
			}
			mt.DefaultTTL = ttl
		}
		mounts = append(mounts, mt)
	}
	for _, sh := range m.Shared {
		mt, err := sharedMountFromConfig(ns, sh)
		if err != nil {
			return nil, MemorySettings{}, noop, err
		}
		mounts = append(mounts, mt)
	}
	var rOpts []memory.RouterOption
	if m.TopK > 0 {
		rOpts = append(rOpts, memory.WithDefaultTopK(m.TopK))
	}
	if m.StrictRecall {
		rOpts = append(rOpts, memory.WithStrictRecall(true))
	}
	router, backend, err := memory.OpenRouter(ctx, memory.BindingSpec{
		Ref:       ref,
		Namespace: ns,
		Mounts:    mounts,
	}, rOpts...)
	if err != nil {
		return nil, MemorySettings{}, noop, err
	}
	settings := MemorySettings{
		Preload:          m.Preload,
		PreloadTopK:      m.TopK,
		Consolidate:      m.Consolidate,
		ConsolidateMount: m.ConsolidateMount,
		DisabledTools:    slices.Clone(cfg.Tools.Disable),
	}
	if !CanConsolidateMount(router, settings.ConsolidateMount) {
		settings.Consolidate = MemoryConsolidateOff
	}
	return router, settings, func() { _ = backend.Close() }, nil
}

// ResolveSubagentMemory resolves the memory.Router and MemorySettings for a
// declarative subagent (#948), handling both inline subagents and rooted
// subagent bundles (spec.Root != "") whose own config.json may declare a
// standalone memory backend or mount overrides.
func ResolveSubagentMemory(
	ctx context.Context,
	parent *memory.Router,
	parentSettings MemorySettings,
	spec config.SubagentSpec,
	rootDir string,
	getenv func(string) string,
) (*memory.Router, MemorySettings, func(), error) {
	noop := func() {}
	subCfg := spec.Memory
	if rootDir != "" {
		if rootLoaded, err := loadRootedSubagentConfig(rootDir); err != nil {
			return nil, MemorySettings{}, noop, err
		} else if rootLoaded != nil && rootLoaded.Memory != nil {
			if ref, rErr := rootLoaded.ResolveMemoryRef(getenv); rErr != nil {
				return nil, MemorySettings{}, noop, rErr
			} else if ref != "" {
				r, s, closeFn, oErr := OpenConfigMemoryRouter(ctx, rootLoaded, spec.Name, getenv)
				if oErr != nil {
					return nil, MemorySettings{}, noop, oErr
				}
				if spec.EffectiveMode() == string(SubagentModeSingleTurn) {
					s.DisableTools = true
				}
				return r, s, closeFn, nil
			}
			if subCfg == nil {
				subCfg = subagentMemoryFromRootConfig(rootLoaded.Memory)
			}
		}
	}
	r, s, err := DeriveSubagentMemory(
		parent,
		parentSettings,
		spec.Name,
		spec.EffectiveMode(),
		spec.Capability,
		false,
		subCfg,
	)
	return r, s, noop, err
}

func loadRootedSubagentConfig(rootDir string) (*config.Config, error) {
	for _, dir := range []string{rootDir, filepath.Join(rootDir, config.AgentsDirName)} {
		if _, err := os.Stat(filepath.Join(dir, config.ConfigFileName)); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		return config.Load(dir)
	}
	return nil, nil
}

func subagentMemoryFromRootConfig(m *config.MemoryConfig) *config.SubagentMemoryConfig {
	if m == nil {
		return nil
	}
	out := &config.SubagentMemoryConfig{
		Shared:  slices.Clone(m.Shared),
		Preload: m.Preload,
	}
	if m.Agent != nil {
		out.Agent = m.Agent.Mode
		out.AgentID = m.Agent.ID
		out.PartitionByUser = m.Agent.PartitionByUser
		out.WriteMode = m.Agent.WriteMode
	}
	return out
}

// IsMemoryToolName reports whether name is one of the model-facing memory
// tools registered by WithMemory.
func IsMemoryToolName(name string) bool {
	return name == memtools.RecallToolName ||
		name == memtools.PersistToolName ||
		name == memtools.ForgetToolName
}

// DeriveSubagentMemory derives a subagent's memory.Router and MemorySettings
// from parent and subCfg (#948).
func DeriveSubagentMemory(
	parent *memory.Router,
	parentSettings MemorySettings,
	subName, subMode, capability string,
	adHoc bool,
	subCfg *config.SubagentMemoryConfig,
) (*memory.Router, MemorySettings, error) {
	if parent == nil {
		return nil, MemorySettings{}, nil
	}
	dc := memory.SubagentDeriveConfig{
		SubagentName: subName,
		Mode:         subMode,
		Capability:   capability,
		AdHoc:        adHoc,
	}
	if subCfg != nil {
		dc.AgentMode = subCfg.Agent
		dc.AgentIDOverride = subCfg.AgentID
		dc.AgentPartitionByUser = subCfg.PartitionByUser
		if subCfg.WriteMode != "" {
			dc.AgentWriteMode = memory.WriteMode(subCfg.WriteMode)
		}
		dc.InheritShared = subCfg.InheritShared
		ns := parent.Namespace()
		for _, sh := range subCfg.Shared {
			mt, err := sharedMountFromConfig(ns, sh)
			if err != nil {
				return nil, MemorySettings{}, err
			}
			dc.ExtraShared = append(dc.ExtraShared, mt)
		}
	}
	r, err := parent.DeriveForSubagent(dc)
	if err != nil || r == nil {
		return nil, MemorySettings{}, err
	}
	s := parentSettings
	if subCfg != nil && subCfg.Preload != "" {
		s.Preload = subCfg.Preload
	}
	if subMode == string(SubagentModeSingleTurn) {
		s.DisableTools = true
	}
	if !CanConsolidateMount(r, s.ConsolidateMount) {
		s.Consolidate = MemoryConsolidateOff
	}
	return r, s, nil
}

// CanConsolidateMount reports whether r has the target consolidation mount
// (defaulting to "agent" when mount is empty) mounted read-write.
func CanConsolidateMount(r *memory.Router, mount string) bool {
	if r == nil {
		return false
	}
	if mount == "" {
		mount = memory.MountAgent
	}
	for _, m := range r.Mounts() {
		if m.Name == mount && m.Mode == memory.AccessReadWrite {
			return true
		}
	}
	return false
}

func sharedMountFromConfig(ns string, sh config.SharedMemoryConfig) (memory.MountTemplate, error) {
	mt := memory.SharedMount(ns, sh.Pool, memory.AccessMode(sh.EffectiveMode()))
	mt.PartitionByUser = sh.PartitionByUser
	if sh.WriteMode != "" {
		mt.WriteMode = memory.WriteMode(sh.WriteMode)
	}
	ttl, err := sh.ResolvedTTL()
	if err != nil {
		return memory.MountTemplate{}, err
	}
	mt.DefaultTTL = ttl
	if len(sh.ScopeAttrs) > 0 {
		mt.BaseScope = maps.Clone(sh.ScopeAttrs)
	}
	return mt, nil
}
