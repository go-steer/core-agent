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

// Originally derived from go-steer/mast@d2f45c7 (internal/cli/a2a.go, internal/a2a/server.go).

package compose

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/approval"
	"github.com/go-steer/core-agent/v3/pkg/attach"
	"github.com/go-steer/core-agent/v3/pkg/attachadapter"
	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

const (
	// DefaultA2AMaxTasks is the default LRU bound on terminal/idle tasks
	// retained in the in-memory A2A task registry (mast assessment M9).
	DefaultA2AMaxTasks = 1024
	// DefaultA2ATaskTTL is the default TTL for terminal/idle tasks in the
	// in-memory A2A task registry (mast assessment M9).
	DefaultA2ATaskTTL = 24 * time.Hour

	a2aContextStateKey = "a2a_context_id"
	a2aTaskStateKey    = "a2a_task_id"
)

// AgentProvider is implemented by attach.Registrant values that expose their
// underlying *agent.Agent (such as *attachadapter.Adapter).
type AgentProvider interface {
	Agent() *agent.Agent
}

// A2ABackendOptions configures a session-backed A2ABackend.
type A2ABackendOptions struct {
	// DaemonCtx bounds background execution for non-streaming message/send
	// turns so an HTTP client disconnect does not abort an in-flight turn
	// (mast #562). Defaults to context.Background().
	DaemonCtx context.Context
	// Registry is the daemon's session registry. Optional when FactoryDeps.Registry
	// is populated.
	Registry *attach.SessionRegistry
	// FactoryDeps constructs new per-contextId sessions with explicit session IDs
	// via ReproduceAgent. Either FactoryDeps or SessionFactory may be supplied.
	FactoryDeps *SessionFactoryDeps
	// SessionFactory is an optional fallback factory for hosts that construct
	// sessions via a custom attach.SessionFactory closure.
	SessionFactory attach.SessionFactory
	// Resumer reconstructs evicted/restarted sessions on lookup miss. Defaults
	// to BuildSessionResumer(*FactoryDeps) when FactoryDeps is non-nil.
	Resumer attach.SessionResumer
	// EventlogHandle is the durable session store used to detect P2 write-gate
	// and input parks and to recover tasks across daemon restarts.
	EventlogHandle *eventlog.Handle
	// ACLStore is the persistent ACL backing store used for cross-restart
	// authorization checks. Defaults to FactoryDeps.ACLStore when FactoryDeps is set.
	ACLStore attach.SessionACLStore
	// EnforceACL enables per-caller ACL checks (auth.Authorize) on SendMessage,
	// PreflightTask, GetTask, and CancelTask.
	EnforceACL bool
	// MaxTasks caps the number of terminal/idle tasks kept in memory (LRU).
	// In-flight tasks (submitted, working) are pinned and never evicted.
	// Defaults to DefaultA2AMaxTasks (1024).
	MaxTasks int
	// TaskTTL is the time-to-live for terminal/idle tasks in memory.
	// Defaults to DefaultA2ATaskTTL (24h).
	TaskTTL time.Duration
	// Now is an optional clock for deterministic TTL/LRU testing.
	Now func() time.Time
}

// A2ABackend bridges the attach A2A v0.3 JSON-RPC handler onto core-agent
// sessions (1 A2A contextId <-> 1 core-agent session), with P2 durable park
// state mapping and a bounded task registry.
type A2ABackend struct {
	daemonCtx      context.Context
	registry       *attach.SessionRegistry
	factoryDeps    *SessionFactoryDeps
	sessionFactory attach.SessionFactory
	resumer        attach.SessionResumer
	eventlogHandle *eventlog.Handle
	aclStore       attach.SessionACLStore
	enforceACL     bool
	maxTasks       int
	taskTTL        time.Duration
	now            func() time.Time

	resolveMu       sync.Mutex
	mu              sync.Mutex
	tasks           map[string]*a2aTaskRecord
	contextSessions map[string]string
	sessionLocks    map[string]*sync.Mutex
}

type a2aTaskRecord struct {
	outcome      attach.A2ATaskOutcome
	sessionID    string
	appName      string
	owner        string
	cancel       context.CancelFunc
	updatedAt    time.Time
	lastAccessed time.Time
}

var (
	_ attach.A2ABackend     = (*A2ABackend)(nil)
	_ attach.A2APreflighter = (*A2ABackend)(nil)
)

// BuildA2AOptions translates config.Config.A2A and an A2ABackend into the
// attach.A2AOptions consumed by attach.NewServer.
func BuildA2AOptions(cfg *config.Config, backend attach.A2ABackend) attach.A2AOptions {
	if cfg == nil || !cfg.A2A.Expose {
		return attach.A2AOptions{}
	}
	skillDesc := strings.TrimSpace(cfg.A2A.SkillDescription)
	if skillDesc == "" {
		skillDesc = strings.TrimSpace(cfg.Agent.Description)
	}
	return attach.A2AOptions{
		Expose:           true,
		SkillName:        strings.TrimSpace(cfg.A2A.SkillName),
		SkillDescription: skillDesc,
		InputSchema:      cfg.A2A.InputSchema,
		OutputSchema:     cfg.A2A.OutputSchema,
		Auth: attach.A2AAuthOptions{
			Required: cfg.A2A.Auth.Required,
			Scopes:   cfg.A2A.Auth.Scopes,
		},
		Backend: backend,
	}
}

// NewA2ABackend constructs a session-backed A2ABackend.
func NewA2ABackend(opts A2ABackendOptions) *A2ABackend {
	daemonCtx := opts.DaemonCtx
	if daemonCtx == nil && opts.FactoryDeps != nil {
		daemonCtx = opts.FactoryDeps.DaemonCtx
	}
	if daemonCtx == nil {
		daemonCtx = context.Background()
	}
	reg := opts.Registry
	if reg == nil && opts.FactoryDeps != nil {
		reg = opts.FactoryDeps.Registry
	}
	if reg == nil {
		reg = attach.NewSessionRegistry()
	}
	evHandle := opts.EventlogHandle
	if evHandle == nil && opts.FactoryDeps != nil {
		evHandle = opts.FactoryDeps.EventlogHandle
	}
	aclStore := opts.ACLStore
	if aclStore == nil && opts.FactoryDeps != nil {
		aclStore = opts.FactoryDeps.ACLStore
	}
	resumer := opts.Resumer
	if resumer == nil && opts.FactoryDeps != nil {
		resumer = BuildSessionResumer(*opts.FactoryDeps)
	}
	maxTasks := opts.MaxTasks
	if maxTasks <= 0 {
		maxTasks = DefaultA2AMaxTasks
	}
	taskTTL := opts.TaskTTL
	if taskTTL <= 0 {
		taskTTL = DefaultA2ATaskTTL
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &A2ABackend{
		daemonCtx:       daemonCtx,
		registry:        reg,
		factoryDeps:     opts.FactoryDeps,
		sessionFactory:  opts.SessionFactory,
		resumer:         resumer,
		eventlogHandle:  evHandle,
		aclStore:        aclStore,
		enforceACL:      opts.EnforceACL,
		maxTasks:        maxTasks,
		taskTTL:         taskTTL,
		now:             nowFn,
		tasks:           make(map[string]*a2aTaskRecord),
		contextSessions: make(map[string]string),
		sessionLocks:    make(map[string]*sync.Mutex),
	}
}

func (b *A2ABackend) sessionLock(sid string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessionLocks == nil {
		b.sessionLocks = make(map[string]*sync.Mutex)
	}
	m, ok := b.sessionLocks[sid]
	if !ok {
		m = &sync.Mutex{}
		b.sessionLocks[sid] = m
	}
	return m
}

// PreflightTask validates caller permissions and session availability before
// message/stream commits HTTP 200 text/event-stream headers.
func (b *A2ABackend) PreflightTask(ctx context.Context, req attach.A2ATaskRequest) error {
	caller := resolveA2ACaller(ctx, req.Caller)
	if strings.TrimSpace(req.TaskID) != "" {
		if err := b.checkExistingTaskAccess(ctx, caller, strings.TrimSpace(req.TaskID), auth.ActionSessionWrite); err != nil {
			return err
		}
	}
	if strings.TrimSpace(req.ContextID) != "" {
		if err := b.checkContextAccess(ctx, caller, strings.TrimSpace(req.ContextID), auth.ActionSessionWrite); err != nil {
			return err
		}
	}
	if b.factoryDeps == nil && b.sessionFactory == nil && len(b.registry.List()) == 0 {
		return errors.New("a2a: no session factory or registered session available")
	}
	return nil
}

// SendMessage executes or resumes an A2A task turn against the core-agent
// session mapped to req.ContextID.
func (b *A2ABackend) SendMessage(ctx context.Context, req attach.A2ATaskRequest) (attach.A2ATaskOutcome, error) {
	caller := resolveA2ACaller(ctx, req.Caller)
	if strings.TrimSpace(req.TaskID) != "" {
		if err := b.checkExistingTaskAccess(ctx, caller, strings.TrimSpace(req.TaskID), auth.ActionSessionWrite); err != nil {
			return attach.A2ATaskOutcome{}, err
		}
	}
	if strings.TrimSpace(req.ContextID) != "" {
		if err := b.checkContextAccess(ctx, caller, strings.TrimSpace(req.ContextID), auth.ActionSessionWrite); err != nil {
			return attach.A2ATaskOutcome{}, err
		}
	}
	taskID, contextID := b.normalizeTaskAndContextIDs(req.TaskID, req.ContextID)

	ag, err := b.resolveOrCreateSession(ctx, caller, taskID, contextID)
	if err != nil {
		return attach.A2ATaskOutcome{}, err
	}

	sessMu := b.sessionLock(ag.SessionID())
	sessMu.Lock()
	defer sessMu.Unlock()

	turnCtx, cancelTurn := b.buildTurnContext(ctx, caller, req.OnProgress == nil)
	defer cancelTurn()

	started, err := b.beginTaskTurn(taskID, contextID, ag, caller, cancelTurn)
	if err != nil {
		return attach.A2ATaskOutcome{}, err
	}
	if !started {
		// Task was already canceled before the turn started; honor cancel-wins.
		b.mu.Lock()
		out := cloneA2AOutcome(b.tasks[taskID].outcome)
		b.mu.Unlock()
		return out, nil
	}

	b.persistSessionTaskCorrelation(turnCtx, ag, taskID, contextID)

	seq, err := b.startAgentTurn(turnCtx, ag, req, caller)
	if err != nil {
		return b.recordTurnFailure(taskID, contextID, err)
	}

	fullText, turnErr := consumeA2ATurnEvents(seq, req.OnProgress)
	b.persistSessionTaskCorrelation(turnCtx, ag, taskID, contextID)
	return b.finalizeTurnOutcome(turnCtx, ag, taskID, contextID, fullText, turnErr)
}

// GetTask returns the current state of task id from the bounded in-memory
// registry or recovers it from the durable eventlog across a daemon restart.
func (b *A2ABackend) GetTask(ctx context.Context, id string) (attach.A2ATaskOutcome, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return attach.A2ATaskOutcome{}, false, nil
	}
	caller := resolveA2ACaller(ctx, auth.Caller{})
	now := b.now().UTC()

	b.mu.Lock()
	b.evictLocked(now)
	rec, ok := b.tasks[id]
	if ok {
		owner := rec.owner
		sid := rec.sessionID
		appName := rec.appName
		rec.lastAccessed = now
		out := cloneA2AOutcome(rec.outcome)
		b.mu.Unlock()
		if err := b.authorizeSessionOrOwner(ctx, caller, appName, sid, owner, auth.ActionSessionRead); err != nil {
			return attach.A2ATaskOutcome{}, false, err
		}
		return out, true, nil
	}
	b.mu.Unlock()

	return b.recoverTaskFromStore(ctx, caller, id)
}

// CancelTask cancels an in-flight or parked task and transitions it to
// TaskStateCanceled, preserving the cancel-wins invariant against racing turn
// completions.
func (b *A2ABackend) CancelTask(ctx context.Context, id string) (attach.A2ATaskOutcome, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return attach.A2ATaskOutcome{}, false, nil
	}
	caller := resolveA2ACaller(ctx, auth.Caller{})
	now := b.now().UTC()

	b.mu.Lock()
	b.evictLocked(now)
	rec, ok := b.tasks[id]
	if !ok {
		b.mu.Unlock()
		recovered, found, err := b.recoverTaskFromStore(ctx, caller, id)
		if err != nil || !found {
			return attach.A2ATaskOutcome{}, found, err
		}
		b.mu.Lock()
		rec, ok = b.tasks[id]
		if !ok {
			b.mu.Unlock()
			return recovered, true, nil
		}
	}
	owner := rec.owner
	sid := rec.sessionID
	appName := rec.appName
	b.mu.Unlock()

	if err := b.authorizeSessionOrOwner(ctx, caller, appName, sid, owner, auth.ActionSessionWrite); err != nil {
		return attach.A2ATaskOutcome{}, false, err
	}

	return b.applyCancelLocked(id, now), true, nil
}

func (b *A2ABackend) applyCancelLocked(id string, now time.Time) attach.A2ATaskOutcome {
	b.mu.Lock()
	rec, ok := b.tasks[id]
	if !ok {
		b.mu.Unlock()
		return attach.A2ATaskOutcome{TaskID: id, State: attach.TaskStateCanceled}
	}
	cancel := rec.cancel
	rec.cancel = nil
	if rec.outcome.State != attach.TaskStateCompleted && rec.outcome.State != attach.TaskStateFailed {
		rec.outcome.State = attach.TaskStateCanceled
		rec.outcome.Output = ""
		rec.outcome.Data = nil
		rec.updatedAt = now
	}
	rec.lastAccessed = now
	out := cloneA2AOutcome(rec.outcome)
	b.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return out
}

func (b *A2ABackend) normalizeTaskAndContextIDs(taskID, contextID string) (string, string) {
	taskID = strings.TrimSpace(taskID)
	contextID = strings.TrimSpace(contextID)

	b.mu.Lock()
	defer b.mu.Unlock()
	if taskID != "" {
		if rec, ok := b.tasks[taskID]; ok && rec.outcome.ContextID != "" {
			if contextID == "" {
				contextID = rec.outcome.ContextID
			}
		}
	}
	if taskID == "" {
		taskID = "a2a-" + uuid.NewString()
	}
	if contextID == "" {
		contextID = "ctx-" + uuid.NewString()
	}
	return taskID, contextID
}

func (b *A2ABackend) beginTaskTurn(taskID, contextID string, ag *agent.Agent, caller auth.Caller, cancel context.CancelFunc) (bool, error) {
	now := b.now().UTC()
	b.mu.Lock()
	defer b.mu.Unlock()

	b.evictLocked(now)
	if existing, ok := b.tasks[taskID]; ok {
		if existing.outcome.State == attach.TaskStateCanceled {
			return false, nil
		}
		if isA2ATaskInFlight(existing.outcome.State) {
			return false, fmt.Errorf("a2a: task %q is already in progress (%s)", taskID, existing.outcome.State)
		}
		if existing.outcome.State != attach.TaskStateInputRequired {
			return false, fmt.Errorf("a2a: task %q is in terminal state %s and cannot be resumed", taskID, existing.outcome.State)
		}
	}
	owner := caller.Identity
	if existing, ok := b.tasks[taskID]; ok && existing.owner != "" {
		owner = existing.owner
	}
	b.tasks[taskID] = &a2aTaskRecord{
		outcome: attach.A2ATaskOutcome{
			TaskID:    taskID,
			ContextID: contextID,
			State:     attach.TaskStateWorking,
		},
		sessionID:    ag.SessionID(),
		appName:      ag.AppName(),
		owner:        owner,
		cancel:       cancel,
		updatedAt:    now,
		lastAccessed: now,
	}
	b.contextSessions[contextID] = ag.SessionID()
	return true, nil
}

func (b *A2ABackend) startAgentTurn(
	turnCtx context.Context,
	ag *agent.Agent,
	req attach.A2ATaskRequest,
	caller auth.Caller,
) (iter.Seq2[*session.Event, error], error) {
	if pending := b.pendingParks(turnCtx, ag); len(pending) > 0 {
		resumeContent, err := buildParkResumeContent(pending[0], req, caller)
		if err != nil {
			return nil, err
		}
		return ag.RunContent(turnCtx, resumeContent), nil
	}
	prompt := buildA2APromptText(req)
	return ag.Run(turnCtx, prompt), nil
}

func consumeA2ATurnEvents(
	seq iter.Seq2[*session.Event, error],
	onProgress func(state attach.A2ATaskState, statusText string),
) (string, error) {
	var nonPartialBuilder strings.Builder
	var partialBuilder strings.Builder
	for ev, err := range seq {
		if err != nil {
			return firstNonEmptyString(nonPartialBuilder.String(), partialBuilder.String()), err
		}
		if ev == nil || ev.Content == nil || ev.Content.Role != genai.RoleModel {
			continue
		}
		text := extractContentText(ev.Content)
		if text == "" {
			continue
		}
		if ev.Partial {
			// mast #408: skip partial streaming deltas on SSE progress
			// frames while retaining a fallback buffer for providers that
			// emit text only on partial events.
			partialBuilder.WriteString(text)
			continue
		}
		nonPartialBuilder.WriteString(text)
		if onProgress != nil {
			onProgress(attach.TaskStateWorking, text)
		}
	}
	return firstNonEmptyString(nonPartialBuilder.String(), partialBuilder.String()), nil
}

func (b *A2ABackend) finalizeTurnOutcome(
	turnCtx context.Context,
	ag *agent.Agent,
	taskID, contextID, fullText string,
	turnErr error,
) (attach.A2ATaskOutcome, error) {
	now := b.now().UTC()
	b.mu.Lock()
	if rec, ok := b.tasks[taskID]; ok && rec.outcome.State == attach.TaskStateCanceled {
		rec.cancel = nil
		rec.lastAccessed = now
		out := cloneA2AOutcome(rec.outcome)
		b.mu.Unlock()
		return out, nil
	}
	b.mu.Unlock()

	if turnErr != nil {
		return b.recordTurnFailure(taskID, contextID, turnErr)
	}

	outcome := attach.A2ATaskOutcome{
		TaskID:    taskID,
		ContextID: contextID,
		State:     attach.TaskStateCompleted,
		Output:    fullText,
	}
	if pending := b.pendingParks(turnCtx, ag); len(pending) > 0 {
		outcome.State = attach.TaskStateInputRequired
		outcome.Output = ""
		outcome.StatusMessage = pending[0].Message
		outcome.Metadata = parkToMetadata(pending[0])
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if rec, ok := b.tasks[taskID]; ok {
		rec.cancel = nil
		// Cancel-wins invariant: if CancelTask raced while we checked
		// pendingParks, keep TaskStateCanceled.
		if rec.outcome.State == attach.TaskStateCanceled {
			rec.lastAccessed = now
			return cloneA2AOutcome(rec.outcome), nil
		}
		rec.outcome = cloneA2AOutcome(outcome)
		rec.updatedAt = now
		rec.lastAccessed = now
	}
	b.evictLocked(now)
	return outcome, nil
}

func (b *A2ABackend) recordTurnFailure(taskID, contextID string, turnErr error) (attach.A2ATaskOutcome, error) {
	now := b.now().UTC()
	b.mu.Lock()
	defer b.mu.Unlock()
	if rec, ok := b.tasks[taskID]; ok {
		rec.cancel = nil
		if rec.outcome.State == attach.TaskStateCanceled {
			rec.lastAccessed = now
			return cloneA2AOutcome(rec.outcome), nil
		}
		rec.outcome = attach.A2ATaskOutcome{
			TaskID:        taskID,
			ContextID:     contextID,
			State:         attach.TaskStateFailed,
			StatusMessage: turnErr.Error(),
		}
		rec.updatedAt = now
		rec.lastAccessed = now
		b.evictLocked(now)
		return cloneA2AOutcome(rec.outcome), turnErr
	}
	return attach.A2ATaskOutcome{
		TaskID:        taskID,
		ContextID:     contextID,
		State:         attach.TaskStateFailed,
		StatusMessage: turnErr.Error(),
	}, turnErr
}

func (b *A2ABackend) resolveOrCreateSession(
	ctx context.Context,
	caller auth.Caller,
	taskID, contextID string,
) (*agent.Agent, error) {
	b.resolveMu.Lock()
	defer b.resolveMu.Unlock()

	sid := b.lookupMappedSessionID(taskID, contextID)
	if sid != "" {
		if ag, err := b.lookupExistingAgent(ctx, caller, sid, auth.ActionSessionWrite); err != nil || ag != nil {
			if ag != nil {
				b.mu.Lock()
				b.contextSessions[contextID] = ag.SessionID()
				b.mu.Unlock()
			}
			return ag, err
		}
	}
	for _, candidate := range []string{contextID, taskID} {
		if candidate == "" || candidate == sid {
			continue
		}
		ag, err := b.lookupExistingAgent(ctx, caller, candidate, auth.ActionSessionWrite)
		if err != nil {
			return nil, err
		}
		if ag != nil {
			b.mu.Lock()
			b.contextSessions[contextID] = ag.SessionID()
			b.mu.Unlock()
			return ag, nil
		}
	}
	return b.createSessionForContext(ctx, caller, taskID, contextID)
}

func (b *A2ABackend) lookupMappedSessionID(taskID, contextID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if taskID != "" {
		if rec, ok := b.tasks[taskID]; ok && rec.sessionID != "" {
			return rec.sessionID
		}
	}
	if contextID != "" {
		if sid, ok := b.contextSessions[contextID]; ok && sid != "" {
			return sid
		}
	}
	return ""
}

func (b *A2ABackend) lookupInMemoryEntry(appName, sid string) *attach.Entry {
	if b.registry == nil || sid == "" {
		return nil
	}
	for _, entry := range b.registry.List() {
		if entry.SessionID == sid && (appName == "" || entry.AppName == appName) {
			return entry
		}
	}
	return nil
}

func (b *A2ABackend) lookupExistingAgent(
	ctx context.Context,
	caller auth.Caller,
	candidateID string,
	action auth.Action,
) (*agent.Agent, error) {
	if entry := b.lookupInMemoryEntry("", candidateID); entry != nil {
		if b.enforceACL && !auth.Authorize(caller, action, entry.CurrentACL()) {
			return nil, attach.ErrA2AForbidden
		}
		if ag := agentFromRegistrant(entry.Agent); ag != nil {
			return ag, nil
		}
	}
	if (b.factoryDeps == nil && b.resumer == nil) || b.eventlogHandle == nil || b.eventlogHandle.Service == nil {
		return nil, nil
	}
	sess, err := b.findPersistedSession(ctx, candidateID)
	if err != nil || sess == nil {
		return nil, err
	}
	if entry := b.lookupInMemoryEntry(sess.AppName(), sess.ID()); entry != nil {
		if b.enforceACL && !auth.Authorize(caller, action, entry.CurrentACL()) {
			return nil, attach.ErrA2AForbidden
		}
		if ag := agentFromRegistrant(entry.Agent); ag != nil {
			return ag, nil
		}
	}
	if err := b.authorizePersistedSession(ctx, caller, sess.AppName(), sess.ID(), sess.UserID(), action); err != nil {
		return nil, err
	}
	return b.resumePersistedAgent(ctx, caller, sess)
}

func (b *A2ABackend) resumePersistedAgent(ctx context.Context, caller auth.Caller, sess session.Session) (*agent.Agent, error) {
	resumeCaller := caller
	if sess.UserID() != "" {
		resumeCaller = auth.Caller{Identity: sess.UserID()}
	}
	if b.factoryDeps != nil {
		ad, cancelOnEvict, err := ReproduceAgent(*b.factoryDeps, resumeCaller, sess.ID(), "resumed")
		if err != nil {
			return nil, err
		}
		b.registerAgentEntry(ad, resumeCaller.Identity, cancelOnEvict)
		return ad.Agent(), nil
	}
	if b.resumer != nil {
		reg, acl, cancelOnEvict, err := b.resumer.Resume(ctx, sess.AppName(), sess.ID())
		if err != nil {
			return nil, err
		}
		b.registerAgentEntry(reg, cmp.Or(acl.Owner, resumeCaller.Identity), cancelOnEvict)
		return agentFromRegistrant(reg), nil
	}
	return nil, nil
}

func (b *A2ABackend) createSessionForContext(
	ctx context.Context,
	caller auth.Caller,
	taskID, contextID string,
) (*agent.Agent, error) {
	if b.factoryDeps != nil {
		sid := cmp.Or(contextID, taskID, newSessionID())
		ad, cancelOnEvict, err := ReproduceAgent(*b.factoryDeps, caller, sid, "created")
		if err != nil {
			return nil, err
		}
		b.registerAgentEntry(ad, caller.Identity, cancelOnEvict)
		b.mu.Lock()
		b.contextSessions[contextID] = sid
		b.mu.Unlock()
		return ad.Agent(), nil
	}
	if b.sessionFactory != nil {
		reg, cancelOnEvict, err := b.sessionFactory(ctx, caller)
		if err != nil {
			return nil, err
		}
		b.registerAgentEntry(reg, caller.Identity, cancelOnEvict)
		ag := agentFromRegistrant(reg)
		if ag == nil {
			return nil, errors.New("a2a: session factory returned registrant without *agent.Agent")
		}
		b.mu.Lock()
		b.contextSessions[contextID] = ag.SessionID()
		b.mu.Unlock()
		return ag, nil
	}
	entries := b.registry.List()
	if len(entries) == 1 {
		if b.enforceACL && !auth.Authorize(caller, auth.ActionSessionWrite, entries[0].CurrentACL()) {
			return nil, attach.ErrA2AForbidden
		}
		if ag := agentFromRegistrant(entries[0].Agent); ag != nil {
			b.mu.Lock()
			b.contextSessions[contextID] = ag.SessionID()
			b.mu.Unlock()
			return ag, nil
		}
	}
	return nil, errors.New("a2a: unable to resolve or create session")
}

func (b *A2ABackend) registerAgentEntry(reg attach.Registrant, owner string, cancel context.CancelFunc) {
	if b.registry == nil || reg == nil {
		if cancel != nil {
			cancel()
		}
		return
	}
	if b.lookupInMemoryEntry(reg.AppName(), reg.SessionID()) != nil {
		if cancel != nil {
			cancel()
		}
		return
	}
	resolvedOwner := cmp.Or(strings.TrimSpace(owner), auth.Anonymous.Identity)
	if _, err := b.registry.RegisterOwnedWithCancel(reg, resolvedOwner, cancel); err != nil && cancel != nil {
		cancel()
	}
}

func agentFromRegistrant(r attach.Registrant) *agent.Agent {
	if r == nil {
		return nil
	}
	if ap, ok := r.(AgentProvider); ok {
		return ap.Agent()
	}
	if ad, ok := r.(*attachadapter.Adapter); ok {
		return ad.Agent()
	}
	if ag, ok := r.(*agent.Agent); ok {
		return ag
	}
	return nil
}

// a2aDetachedTurnContext detaches a turn-driving context from the HTTP
// request's cancellation while preserving request-scoped values and bounding
// execution by the daemon's lifecycle context (mast #562).
type a2aDetachedTurnContext struct {
	context.Context
	values context.Context
}

func (d a2aDetachedTurnContext) Value(key any) any {
	return d.values.Value(key)
}

func (b *A2ABackend) buildTurnContext(reqCtx context.Context, caller auth.Caller, detachFromHTTP bool) (context.Context, context.CancelFunc) {
	withCaller := auth.WithCaller(reqCtx, caller)
	base := withCaller
	if detachFromHTTP {
		detached := context.WithoutCancel(withCaller)
		parent := b.daemonCtx
		if parent == nil {
			parent = context.Background()
		}
		base = a2aDetachedTurnContext{
			Context: parent,
			values:  detached,
		}
	}
	return context.WithCancel(base)
}

func (b *A2ABackend) pendingParks(ctx context.Context, ag *agent.Agent) []eventlog.ParkedCall {
	if ag == nil {
		return nil
	}
	svc := ag.SessionService()
	if svc == nil && b.eventlogHandle != nil {
		svc = b.eventlogHandle.Service
	}
	if svc == nil {
		return nil
	}
	resp, err := svc.Get(ctx, &session.GetRequest{
		AppName:   ag.AppName(),
		UserID:    ag.UserID(),
		SessionID: ag.SessionID(),
	})
	if err != nil || resp == nil || resp.Session == nil {
		return nil
	}
	return eventlog.ScanPendingParks(resp.Session.Events())
}

func (b *A2ABackend) persistSessionTaskCorrelation(ctx context.Context, ag *agent.Agent, taskID, contextID string) {
	if ag == nil {
		return
	}
	svc := ag.SessionService()
	if svc == nil && b.eventlogHandle != nil {
		svc = b.eventlogHandle.Service
	}
	if svc == nil {
		return
	}
	resp, err := svc.Get(ctx, &session.GetRequest{
		AppName:   ag.AppName(),
		UserID:    ag.UserID(),
		SessionID: ag.SessionID(),
	})
	if err != nil || resp == nil || resp.Session == nil {
		return
	}
	ev := session.NewEvent(ctx, "a2a-meta")
	ev.Author = cmp.Or(ag.AgentName(), ag.AppName(), "core_agent")
	ev.Actions.StateDelta = map[string]any{
		a2aTaskStateKey:    taskID,
		a2aContextStateKey: contextID,
	}
	_ = svc.AppendEvent(ctx, resp.Session, ev)
}

func (b *A2ABackend) recoverTaskFromStore(
	ctx context.Context,
	caller auth.Caller,
	id string,
) (attach.A2ATaskOutcome, bool, error) {
	sess, err := b.findPersistedSession(ctx, id)
	if err != nil || sess == nil {
		return attach.A2ATaskOutcome{}, false, err
	}
	if err := b.authorizePersistedSession(ctx, caller, sess.AppName(), sess.ID(), sess.UserID(), auth.ActionSessionRead); err != nil {
		return attach.A2ATaskOutcome{}, false, err
	}
	ctxID := sessionA2AContextID(sess)
	events := sess.Events()
	pending := eventlog.ScanPendingParks(events)
	outcome := attach.A2ATaskOutcome{
		TaskID:    id,
		ContextID: ctxID,
		State:     attach.TaskStateCompleted,
		Output:    lastAssistantTextFromEvents(events),
	}
	if len(pending) > 0 && isLatestSessionTask(sess, id) {
		outcome.State = attach.TaskStateInputRequired
		outcome.Output = ""
		outcome.StatusMessage = pending[0].Message
		outcome.Metadata = parkToMetadata(pending[0])
	}

	now := b.now().UTC()
	b.mu.Lock()
	b.evictLocked(now)
	b.tasks[id] = &a2aTaskRecord{
		outcome:      cloneA2AOutcome(outcome),
		sessionID:    sess.ID(),
		appName:      sess.AppName(),
		owner:        sess.UserID(),
		updatedAt:    now,
		lastAccessed: now,
	}
	b.contextSessions[ctxID] = sess.ID()
	b.mu.Unlock()
	return outcome, true, nil
}

func sessionA2AContextID(sess session.Session) string {
	if v, err := sess.State().Get(a2aContextStateKey); err == nil {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	if events := sess.Events(); events != nil {
		var last string
		for ev := range events.All() {
			if ev == nil || ev.Actions.StateDelta == nil {
				continue
			}
			if s, ok := ev.Actions.StateDelta[a2aContextStateKey].(string); ok && s != "" {
				last = s
			}
		}
		if last != "" {
			return last
		}
	}
	return sess.ID()
}

func isLatestSessionTask(sess session.Session, taskID string) bool {
	if v, err := sess.State().Get(a2aTaskStateKey); err == nil {
		if s, ok := v.(string); ok && s != "" {
			return s == taskID
		}
	}
	if events := sess.Events(); events != nil {
		var last string
		for ev := range events.All() {
			if ev == nil || ev.Actions.StateDelta == nil {
				continue
			}
			if s, ok := ev.Actions.StateDelta[a2aTaskStateKey].(string); ok && s != "" {
				last = s
			}
		}
		if last != "" {
			return last == taskID
		}
	}
	return sess.ID() == taskID
}

func sessionHasA2ATaskID(sess session.Session, taskID string) bool {
	if v, err := sess.State().Get(a2aTaskStateKey); err == nil {
		if s, ok := v.(string); ok && s == taskID {
			return true
		}
	}
	if events := sess.Events(); events != nil {
		for ev := range events.All() {
			if ev == nil || ev.Actions.StateDelta == nil {
				continue
			}
			if s, ok := ev.Actions.StateDelta[a2aTaskStateKey].(string); ok && s == taskID {
				return true
			}
		}
	}
	return false
}

func sessionMatchesA2AID(sess session.Session, id string) bool {
	if sess.ID() == id || sessionHasA2ATaskID(sess, id) {
		return true
	}
	if v, err := sess.State().Get(a2aContextStateKey); err == nil {
		if s, ok := v.(string); ok && s == id {
			return true
		}
	}
	if events := sess.Events(); events != nil {
		for ev := range events.All() {
			if ev == nil || ev.Actions.StateDelta == nil {
				continue
			}
			if s, ok := ev.Actions.StateDelta[a2aContextStateKey].(string); ok && s == id {
				return true
			}
		}
	}
	return false
}

func (b *A2ABackend) defaultAppName() string {
	if b.registry != nil {
		if entries := b.registry.List(); len(entries) > 0 && entries[0].AppName != "" {
			return entries[0].AppName
		}
	}
	return "core-agent"
}

func (b *A2ABackend) findPersistedSession(ctx context.Context, id string) (session.Session, error) {
	if entry := b.lookupInMemoryEntry("", id); entry != nil {
		if ag := agentFromRegistrant(entry.Agent); ag != nil && ag.SessionService() != nil {
			resp, gErr := ag.SessionService().Get(ctx, &session.GetRequest{
				AppName:   ag.AppName(),
				UserID:    ag.UserID(),
				SessionID: ag.SessionID(),
			})
			if gErr == nil && resp != nil && resp.Session != nil {
				return resp.Session, nil
			}
		}
	}
	if b.eventlogHandle == nil || b.eventlogHandle.Service == nil {
		return nil, nil
	}
	listResp, err := b.eventlogHandle.Service.List(ctx, &session.ListRequest{
		AppName: b.defaultAppName(),
	})
	if err != nil || listResp == nil {
		return nil, err
	}
	var fallback session.Session
	for _, s := range listResp.Sessions {
		if strings.HasSuffix(s.ID(), ":mast-ops") {
			continue
		}
		full, gErr := b.eventlogHandle.Service.Get(ctx, &session.GetRequest{
			AppName:   s.AppName(),
			UserID:    s.UserID(),
			SessionID: s.ID(),
		})
		if gErr != nil || full == nil || full.Session == nil {
			continue
		}
		if s.ID() == id {
			return full.Session, nil
		}
		if sessionMatchesA2AID(full.Session, id) {
			fallback = full.Session
		}
	}
	return fallback, nil
}

func (b *A2ABackend) findPersistedSessionByTaskID(ctx context.Context, taskID string) (session.Session, error) {
	if b.eventlogHandle == nil || b.eventlogHandle.Service == nil {
		return nil, nil
	}
	listResp, err := b.eventlogHandle.Service.List(ctx, &session.ListRequest{
		AppName: b.defaultAppName(),
	})
	if err != nil || listResp == nil {
		return nil, err
	}
	for _, s := range listResp.Sessions {
		if strings.HasSuffix(s.ID(), ":mast-ops") {
			continue
		}
		full, gErr := b.eventlogHandle.Service.Get(ctx, &session.GetRequest{
			AppName:   s.AppName(),
			UserID:    s.UserID(),
			SessionID: s.ID(),
		})
		if gErr != nil || full == nil || full.Session == nil {
			continue
		}
		if sessionHasA2ATaskID(full.Session, taskID) {
			return full.Session, nil
		}
	}
	return nil, nil
}

func (b *A2ABackend) checkExistingTaskAccess(ctx context.Context, caller auth.Caller, taskID string, action auth.Action) error {
	b.mu.Lock()
	rec, ok := b.tasks[taskID]
	var appName, sid, owner string
	var state attach.A2ATaskState
	if ok {
		appName, sid, owner = rec.appName, rec.sessionID, rec.owner
		state = rec.outcome.State
	}
	b.mu.Unlock()
	if ok {
		if err := b.authorizeSessionOrOwner(ctx, caller, appName, sid, owner, action); err != nil {
			return err
		}
		if isA2ATaskInFlight(state) {
			return fmt.Errorf("a2a: task %q is already in progress (%s)", taskID, state)
		}
		if state != attach.TaskStateInputRequired {
			return fmt.Errorf("a2a: task %q is in terminal state %s and cannot be resumed", taskID, state)
		}
		return nil
	}
	sess, err := b.findPersistedSessionByTaskID(ctx, taskID)
	if err != nil || sess == nil {
		return err
	}
	if err := b.authorizePersistedSession(ctx, caller, sess.AppName(), sess.ID(), sess.UserID(), action); err != nil {
		return err
	}
	if !isLatestSessionTask(sess, taskID) || len(eventlog.ScanPendingParks(sess.Events())) == 0 {
		return fmt.Errorf("a2a: task %q is in terminal state %s and cannot be resumed", taskID, attach.TaskStateCompleted)
	}
	return nil
}

func (b *A2ABackend) checkContextAccess(ctx context.Context, caller auth.Caller, contextID string, action auth.Action) error {
	b.mu.Lock()
	sid := b.contextSessions[contextID]
	b.mu.Unlock()
	if sid != "" {
		return b.authorizeSessionOrOwner(ctx, caller, "", sid, "", action)
	}
	sess, err := b.findPersistedSession(ctx, contextID)
	if err != nil || sess == nil {
		return err
	}
	return b.authorizePersistedSession(ctx, caller, sess.AppName(), sess.ID(), sess.UserID(), action)
}

func (b *A2ABackend) authorizeSessionOrOwner(
	ctx context.Context,
	caller auth.Caller,
	appName, sid, owner string,
	action auth.Action,
) error {
	if !b.enforceACL {
		return nil
	}
	if entry := b.lookupInMemoryEntry(appName, sid); entry != nil {
		if acl := entry.CurrentACL(); acl.Owner != "" || len(acl.Viewers) > 0 || len(acl.Contributors) > 0 {
			if !auth.Authorize(caller, action, acl) {
				return attach.ErrA2AForbidden
			}
			return nil
		}
	}
	return b.authorizePersistedSession(ctx, caller, appName, sid, owner, action)
}

func (b *A2ABackend) authorizePersistedSession(
	ctx context.Context,
	caller auth.Caller,
	appName, sid, owner string,
	action auth.Action,
) error {
	if !b.enforceACL {
		return nil
	}
	if b.aclStore != nil && appName != "" && sid != "" {
		if row, err := b.aclStore.FindByAppSID(ctx, appName, sid); err == nil {
			if !auth.Authorize(caller, action, row.ACL()) {
				return attach.ErrA2AForbidden
			}
			return nil
		}
	}
	if owner != "" {
		if !auth.Authorize(caller, action, auth.SessionACL{Owner: owner}) {
			return attach.ErrA2AForbidden
		}
	}
	return nil
}

// evictLocked enforces TaskTTL and MaxTasks (LRU) on b.tasks while pinning
// in-flight tasks (submitted, working). Caller must hold b.mu.
func (b *A2ABackend) evictLocked(now time.Time) {
	if b.taskTTL > 0 {
		for id, rec := range b.tasks {
			if isA2ATaskInFlight(rec.outcome.State) {
				continue
			}
			if now.Sub(rec.updatedAt) > b.taskTTL {
				delete(b.tasks, id)
			}
		}
	}
	for b.maxTasks > 0 && len(b.tasks) > b.maxTasks {
		victimID := b.selectLRUVictimLocked()
		if victimID == "" {
			break
		}
		delete(b.tasks, victimID)
	}
}

func (b *A2ABackend) selectLRUVictimLocked() string {
	var victimID string
	var victimRec *a2aTaskRecord
	for id, rec := range b.tasks {
		if isA2ATaskInFlight(rec.outcome.State) {
			continue
		}
		if victimRec == nil || compareLRUVictim(id, rec, victimID, victimRec) < 0 {
			victimID = id
			victimRec = rec
		}
	}
	return victimID
}

func compareLRUVictim(idA string, a *a2aTaskRecord, idB string, b *a2aTaskRecord) int {
	// Prefer evicting terminal tasks before input-required tasks.
	prioA := evictionPriority(a.outcome.State)
	prioB := evictionPriority(b.outcome.State)
	if prioA != prioB {
		return cmp.Compare(prioA, prioB)
	}
	if !a.lastAccessed.Equal(b.lastAccessed) {
		return a.lastAccessed.Compare(b.lastAccessed)
	}
	if !a.updatedAt.Equal(b.updatedAt) {
		return a.updatedAt.Compare(b.updatedAt)
	}
	return cmp.Compare(idA, idB)
}

func evictionPriority(state attach.A2ATaskState) int {
	if state == attach.TaskStateInputRequired {
		return 1
	}
	return 0
}

func isA2ATaskInFlight(state attach.A2ATaskState) bool {
	return state == attach.TaskStateSubmitted || state == attach.TaskStateWorking
}

func buildParkResumeContent(park eventlog.ParkedCall, req attach.A2ATaskRequest, caller auth.Caller) (*genai.Content, error) {
	if park.ToolName == toolconfirmation.FunctionCallName || park.Awaiting == eventlog.AwaitingApproval {
		verdict, err := parseA2AApprovalVerdict(req, caller)
		if err != nil {
			return nil, err
		}
		part := genai.NewPartFromFunctionResponse(toolconfirmation.FunctionCallName, approval.ConfirmationResponse(verdict))
		part.FunctionResponse.ID = park.InterruptID
		return &genai.Content{
			Role:  genai.RoleUser,
			Parts: []*genai.Part{part},
		}, nil
	}
	toolName := cmp.Or(park.ToolName, "adk_request_input")
	respPayload := map[string]any{"response": req.Text}
	if len(req.Data) > 0 {
		if _, hasResp := req.Data["response"]; hasResp {
			respPayload = req.Data
		} else {
			respPayload = map[string]any{"response": req.Data}
		}
	}
	part := genai.NewPartFromFunctionResponse(toolName, respPayload)
	part.FunctionResponse.ID = park.InterruptID
	return &genai.Content{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{part},
	}, nil
}

func parseA2AApprovalVerdict(req attach.A2ATaskRequest, caller auth.Caller) (approval.Verdict, error) {
	approver := strings.TrimSpace(caller.Identity)
	if approver == auth.Anonymous.Identity {
		approver = ""
	}
	data := req.Data
	if len(data) == 0 {
		trimmed := strings.TrimSpace(req.Text)
		if strings.HasPrefix(trimmed, "{") {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
				data = parsed
			}
		}
	}
	if len(data) > 0 {
		v, err := verdictFromMap(data)
		if err != nil {
			return approval.Verdict{}, err
		}
		if payloadApprover := strings.TrimSpace(v.Approver); payloadApprover != "" && payloadApprover != approver {
			return approval.Verdict{}, fmt.Errorf("a2a: approval verdict approver %q does not match authenticated caller %q", payloadApprover, approver)
		}
		v.Approver = approver
		if _, err := v.Decision(); err != nil {
			return approval.Verdict{}, err
		}
		return v, nil
	}
	word := strings.ToLower(strings.TrimSpace(req.Text))
	switch word {
	case "approve", "approved", "yes", "y", "confirm", "confirmed", "ok":
		return approval.Verdict{Verdict: approval.OutcomeApprove, Approver: approver}, nil
	case "reject", "rejected", "deny", "denied", "no", "n", "cancel":
		return approval.Verdict{Verdict: approval.OutcomeReject, Note: strings.TrimSpace(req.Text), Approver: approver}, nil
	default:
		return approval.Verdict{}, fmt.Errorf("a2a: session is parked awaiting approval for a mutating tool call; supply a verdict (approve, reject, or edit)")
	}
}

func verdictFromMap(data map[string]any) (approval.Verdict, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return approval.Verdict{}, err
	}
	var v approval.Verdict
	if err := json.Unmarshal(raw, &v); err != nil {
		return approval.Verdict{}, fmt.Errorf("a2a: decode approval verdict: %w", err)
	}
	if v.Args == nil {
		if mod, ok := data["modified_args"].(map[string]any); ok {
			v.Args = mod
		}
	}
	if v.Note == "" {
		if r, ok := data["reason"].(string); ok {
			v.Note = r
		}
	}
	if v.Verdict == "" {
		if confirmed, ok := data["confirmed"].(bool); ok {
			if confirmed {
				v.Verdict = approval.OutcomeApprove
			} else {
				v.Verdict = approval.OutcomeReject
			}
		} else if approved, ok := data["approved"].(bool); ok {
			if approved {
				v.Verdict = approval.OutcomeApprove
			} else {
				v.Verdict = approval.OutcomeReject
			}
		}
	}
	switch v.Verdict {
	case approval.OutcomeApprove, approval.OutcomeReject, approval.OutcomeEdit:
		return v, nil
	default:
		return approval.Verdict{}, fmt.Errorf("a2a: unknown approval verdict %q (want approve, reject, or edit)", v.Verdict)
	}
}

func parkToMetadata(p eventlog.ParkedCall) map[string]any {
	meta := map[string]any{
		"awaiting":     cmp.Or(p.Awaiting, eventlog.AwaitingInput),
		"interrupt_id": p.InterruptID,
	}
	if p.ProposedTool != "" {
		meta["proposed_tool"] = p.ProposedTool
	}
	if len(p.ProposedArgs) > 0 {
		meta["proposed_args"] = p.ProposedArgs
	}
	if p.Subject != "" {
		meta["subject"] = p.Subject
	}
	return meta
}

func buildA2APromptText(req attach.A2ATaskRequest) string {
	if strings.TrimSpace(req.Text) != "" {
		return req.Text
	}
	if len(req.Data) > 0 {
		if b, err := json.Marshal(req.Data); err == nil {
			return string(b)
		}
	}
	return ""
}

func extractContentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range c.Parts {
		if p != nil && p.Text != "" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func lastAssistantTextFromEvents(events session.Events) string {
	if events == nil {
		return ""
	}
	var last string
	for ev := range events.All() {
		if ev == nil || ev.Partial || ev.Content == nil || ev.Content.Role != genai.RoleModel {
			continue
		}
		if t := extractContentText(ev.Content); t != "" {
			last = t
		}
	}
	return last
}

func resolveA2ACaller(ctx context.Context, fallback auth.Caller) auth.Caller {
	if fallback.Identity != "" || fallback.Admin || len(fallback.Labels) > 0 {
		return fallback
	}
	if c, ok := auth.CallerFromContext(ctx); ok && (c.Identity != "" || c.Admin || len(c.Labels) > 0) {
		return c
	}
	return auth.Anonymous
}

func cloneA2AOutcome(in attach.A2ATaskOutcome) attach.A2ATaskOutcome {
	out := in
	if in.Data != nil {
		out.Data = make(map[string]any, len(in.Data))
		for k, v := range in.Data {
			out.Data[k] = v
		}
	}
	if in.Metadata != nil {
		out.Metadata = make(map[string]any, len(in.Metadata))
		for k, v := range in.Metadata {
			out.Metadata[k] = v
		}
	}
	return out
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
