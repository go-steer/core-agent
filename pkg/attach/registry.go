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

// Package attach implements live-tail + inject over HTTP/SSE for
// headless core-agent deployments. See docs/attach-mode-design.md.
//
// Server side (agent process) — the agent is bridged onto the attach
// surface by pkg/attachadapter, registered, and served (compiling
// excerpt of examples/attach-daemon, the canonical embedding):
//
//	a, _ := agent.New(llm,
//	    agent.WithSession("operator", "main"),
//	    agent.WithEventLog(handle), // attach requires an eventlog
//	)
//	ad := attachadapter.New(a) // + optional attachadapter.With* capability providers
//	reg := attach.NewSessionRegistry()
//	reg.Register(ad)
//	srv, _ := attach.NewServer(attach.Options{
//	    Registry: reg,
//	    Addr:     ":7777",
//	    Auth: attach.AuthConfig{
//	        BearerToken:  os.Getenv("ATTACH_TOKEN"),  // and/or…
//	        TLSCertFile:  "/etc/certs/server.crt",    // TLS
//	        TLSKeyFile:   "/etc/certs/server.key",
//	        ClientCAFile: "/etc/certs/ca.crt", // mTLS
//	    },
//	})
//	go srv.ListenAndServe()
//	go runner.WakeLoop(ctx, a, runner.WakeLoopOptions{}) // drain injects into turns
//
// Client side (operator on a laptop, or another binary):
//
//	core-agent ls https://pod-ip:7777
//	core-agent attach https://pod-ip:7777/sessions/<app>/<sid>
//
// The protocol is HTTP + Server-Sent Events. The core endpoints:
//
//	GET  /sessions                                   list active sessions
//	GET  /sessions/<app>/<sid>/events?since=N        SSE event stream
//	POST /sessions/<app>/<sid>/inject                queue an inbox message
//	POST /sessions/<app>/<sid>/wake                  wake a deferred subagent
//
// plus read-only state projections (/tools, /status, /usage, …),
// operator mutations (/interrupt, /perms/*, /slash/*), and the
// HTTP-driven permission-prompt stream (/perms/stream + /perms/respond)
// — see the SSE event-stream protocol spec on the docs site for the
// full surface.
//
// URL shortcut: /sessions/<sid> works when <sid> is unambiguous across
// registered apps. Returns 409 with a helpful message on collision.
package attach

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// Registrant is the subset of *agent.Agent the registry needs. Defined
// as an interface so attach/ doesn't import agent/ (avoids an import
// cycle — agent/ needs to call into attach to register itself).
type Registrant interface {
	// AppName / UserID / SessionID together form the ADK session
	// key. The registry uses (AppName, SessionID) for URL lookup
	// (userID is per-process today; see attach-mode-design.md) but
	// stores all three so /sessions can expose full triples.
	AppName() string
	UserID() string
	SessionID() string

	// EventLog returns the agent's event log handle, or nil if the
	// agent was constructed without WithEventLog. Attach requires a
	// non-nil eventlog for live-tail (broadcaster pumps from
	// Stream.Watch); the server returns a clear error for sessions
	// without one.
	EventLog() *eventlog.Handle

	// Inject queues an inbox message that the next turn drains.
	// Also fires the wake signal internally (see Agent.Inject).
	Inject(message string) error

	// InjectAs is Inject with a per-message originator identity. The
	// caller is the auth.Caller resolved from the inbound HTTP
	// request (see callerMiddleware). When the agent loop drains the
	// inbox, the last non-empty caller becomes the turn originator
	// stamped onto eventlog metadata and outbound MCP context.
	//
	// In single-user / pre-multi-session deployments, this method is
	// called with a zero-value Caller and behaves identically to
	// Inject.
	InjectAs(message string, caller auth.Caller) error

	// RequestWake fires the agent's wake signal without queuing a
	// message. Used by POST /wake.
	//
	// Implementations SHOULD also publish an EventWake (WakeEvent) to
	// the operator-event transport so attached clients see the wake;
	// *agent.Agent does, and that is what makes protocol 1.7.0's `wake`
	// frame appear. A registrant that doesn't publish still returns 200
	// from /wake — it just looks, to every attached operator, exactly
	// like a pre-1.7.0 daemon.
	RequestWake()
}

// ContextInjector is the optional capability a registrant implements
// when it can carry the injecting request's context — specifically its
// OpenTelemetry span context — onto the queued inbox message.
//
// The write handlers (POST /inject, POST /wake with a prompt) prefer
// it over Registrant.InjectAs so that the turn which eventually drains
// the message can link back to the request that queued it. Without it
// the trace ends at the handler: the handler returns as soon as the
// message is queued and its span closes, while the turn starts minutes
// later on the agent loop with no parent to inherit.
//
// Optional rather than a method on Registrant on purpose. Registrant
// is exported API; widening it would break every out-of-tree
// implementation for a telemetry improvement. Registrants that don't
// implement it fall back to InjectAs and simply produce no link —
// exactly the pre-existing behavior.
type ContextInjector interface {
	InjectAsContext(ctx context.Context, message string, caller auth.Caller) error
}

// injectWithContext queues message on reg, preferring the
// context-carrying path when the registrant supports it. Shared by the
// /inject and /wake handlers so both operator write paths produce the
// same trace linkage.
func injectWithContext(ctx context.Context, reg Registrant, message string, caller auth.Caller) error {
	if ci, ok := reg.(ContextInjector); ok {
		return ci.InjectAsContext(ctx, message, caller)
	}
	return reg.InjectAs(message, caller)
}

// SessionRegistry holds every session exposed over attach — hosts
// wrap their *agent.Agent in attachadapter.New and Register the
// adapter (agent.WithSessionRegistry was removed in #443). Keys by the full
// (AppName, UserID, SessionID) triple but exposes a single-segment
// SessionID shortcut for the unambiguous case.
//
// Optional aclStore (set via NewSessionRegistryWithStore) persists
// the ACL of every RegisterOwned call to disk so sessions survive
// daemon restart. Nil store keeps the legacy in-memory-only
// behavior — backward compatible for single-user and pre-resume
// deployments.
type SessionRegistry struct {
	mu sync.RWMutex
	// Keyed by the full triple; (app, user, sid) is the ADK identity.
	byTriple map[tripleKey]*Entry
	// aclStore, when non-nil, persists ACL rows on RegisterOwned
	// and deletes them on Unregister. Phase 1 of session-resume
	// (docs/session-resume-design.md): persistence only.
	aclStore SessionACLStore
	// resumer, when non-nil, reconstructs sessions on Lookup miss
	// using the persisted ACL row + the daemon's factory shape.
	// Phase 2 of session-resume. Set via WithResumer; nil falls
	// back to the legacy "miss = ErrSessionNotFound" behavior.
	resumer SessionResumer
	// resumeFlight collapses concurrent Lookup misses for the same
	// (app, sid) into a single resumer call — two TUIs reconnecting
	// simultaneously after restart shouldn't race two agent
	// constructions + double-register. Singleflight keys by
	// "app/sid" so resumes for different sessions still run in
	// parallel.
	resumeFlight singleflight.Group
	// regSeq is a monotonically increasing stamp assigned (under mu)
	// to every Entry this registry creates. It gives "which of two
	// Entries for the same triple is newer" a defined answer — the
	// broadcaster pool's staleness check needs a DIRECTION, not just
	// inequality: a handler that resolved its Entry before a
	// re-registration must not be able to unseat the current
	// broadcaster with its stale pointer (#486).
	regSeq uint64
	// evictHook, when non-nil, is invoked (outside the registry lock)
	// once per entry removed by EvictBefore. The attach server wires
	// it to retire the entry's broadcaster from the pool (#486): the
	// pool keys by triple, so after evict + lazy resume the resumed
	// Entry would otherwise inherit the OLD broadcaster — its
	// snapshots and emitter bound to the dead agent, and its pump
	// touch()ing the stale entry so the resumed session looks idle
	// to the sweep while actively streaming.
	evictHook func(*Entry)
}

// SetEvictHook wires a callback invoked for each entry the idle
// sweep evicts, after the entry's cancelOnEvict fired. Called once at
// server construction; last-wins if called again (a registry shared
// by multiple servers keeps the most recent server's hook).
func (r *SessionRegistry) SetEvictHook(fn func(*Entry)) {
	r.mu.Lock()
	r.evictHook = fn
	r.mu.Unlock()
}

// Entry is one registered session as the registry sees it.
type Entry struct {
	AppName   string
	UserID    string
	SessionID string

	// Agent is the live registrant. The registry holds it by
	// reference; lifetime is the registrant's, not the registry's.
	Agent Registrant

	// acl governs which Callers may interact with this session in a
	// multi-session deployment. Zero value (empty Owner / nil slices)
	// means "no owner" — only Admin Callers may access it, which is
	// the documented behavior for legacy sessions registered via
	// Register (vs. RegisterOwned). See
	// docs/multi-session-design.md §"Migration story".
	//
	// Read through CurrentACL, written through setACL, and held as an
	// atomic pointer rather than a plain field because PATCH
	// /sessions/.../acl amends it on a live session (#797) while every
	// in-flight request is reading it to authorize itself. It used to
	// be an exported field, safe only because nothing ever wrote it
	// after registration; making it mutable without the indirection
	// would be a data race on the authorization decision itself.
	//
	// Never mutate a stored value in place — setACL always stores a
	// freshly built ACL, which is what lets readers hold the loaded
	// value without copying it.
	acl atomic.Pointer[auth.SessionACL]

	// lastTouchedNs is Unix nanoseconds of the last "activity" on
	// this entry: a memory-hit Lookup, an event pumped by the
	// broadcaster (including autonomous agent work — a busy agent
	// is never idle), or the entry's initial registration.
	//
	// Read/written via sync/atomic so the touch path is lock-free —
	// broadcast is the hot loop and we don't want to contend with
	// the registry's mu on every event.
	//
	// Consumed by the idle eviction sweep: entries whose
	// lastTouchedNs is older than (now - idleAfter) are candidates
	// for eviction. Not persisted on every touch (write
	// amplification); the sweep persists it once, at evict time.
	lastTouchedNs atomic.Int64

	// regSeq orders Entries for the same triple across
	// re-registrations (evict + lazy resume, agent restart).
	// Assigned by the registry under its mutex; 0 only for Entries
	// constructed outside the registry (white-box tests). The
	// broadcaster pool compares regSeq to decide whether an incoming
	// Entry may replace the pooled broadcaster's (#486).
	regSeq uint64

	// cancelOnEvict is invoked by the registry just before removing
	// the entry — from the idle sweep, from a future DELETE
	// /sessions, or from Unregister when a cancel was supplied.
	// Typically it cancels the ctx driving the per-session wake
	// loop so the loop exits cleanly and doesn't leak.
	//
	// Nil when the caller didn't supply one (legacy Register, tests
	// registering a bare stubRegistrant, resumer's registerResumed
	// path when cancel wasn't threaded).
	cancelOnEvict context.CancelFunc
}

// CurrentACL returns the entry's ACL as of this instant. Safe for
// concurrent use, and the only correct way to read the ACL of a live
// session: PATCH /sessions/.../acl can replace it under a concurrent
// request (#797).
//
// The identity lists are copied, so a caller may keep or sort the
// result without disturbing the entry. Zero value when the session
// was registered unowned (legacy Register) — which grants nothing to
// any non-Admin Caller, per Authorize.
func (e *Entry) CurrentACL() auth.SessionACL {
	acl := e.acl.Load()
	if acl == nil {
		return auth.SessionACL{}
	}
	return auth.SessionACL{
		Owner:        acl.Owner,
		Viewers:      append([]string(nil), acl.Viewers...),
		Contributors: append([]string(nil), acl.Contributors...),
	}
}

// setACL replaces the entry's ACL. Unexported: an ACL change has to
// reach the persisted row too, or it silently un-does itself at the
// next daemon restart, so SessionRegistry.AmendACL is the entrypoint
// and this is its in-memory half.
//
// Stores a normalized copy rather than the caller's value — readers
// hold the loaded ACL without copying it, so the stored value must
// never be reachable for mutation from anywhere else.
func (e *Entry) setACL(acl auth.SessionACL) {
	normalized := acl.Normalized()
	e.acl.Store(&normalized)
}

// LastTouchedAt returns the entry's last-touched wall time. Safe for
// concurrent access. Zero time if the entry has never been touched
// (which shouldn't happen — registration always seeds it).
func (e *Entry) LastTouchedAt() time.Time {
	ns := e.lastTouchedNs.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// touch bumps the entry's last-touched timestamp to now. Called by
// the registry and the broadcaster; lock-free.
func (e *Entry) touch() {
	e.lastTouchedNs.Store(time.Now().UnixNano())
}

type tripleKey struct {
	App, User, SID string
}

// NewSessionRegistry returns an empty registry. Sessions registered
// via RegisterOwned are in-memory only — they do NOT survive
// daemon restart. Use NewSessionRegistryWithStore to opt into
// disk-backed ACL persistence (Phase 1 of session resume).
func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{byTriple: make(map[tripleKey]*Entry)}
}

// NewSessionRegistryWithStore returns a registry wired to persist
// every RegisterOwned ACL to the supplied store. The store
// upserts on Register; the Phase 2 SessionResumer reads it back
// on Lookup miss to reconstruct evicted sessions (when wired via
// WithResumer).
//
// Passing nil for the store is equivalent to NewSessionRegistry
// (no persistence). Callers wire this in their daemon startup
// once the eventlog handle exposes its DB connection
// (eventlog.Handle.DB).
func NewSessionRegistryWithStore(store SessionACLStore) *SessionRegistry {
	return &SessionRegistry{
		byTriple: make(map[tripleKey]*Entry),
		aclStore: store,
	}
}

// WithResumer wires a SessionResumer onto an existing registry.
// Subsequent Lookup / LookupSingle misses consult the resumer to
// reconstruct sessions that exist on disk but not in memory
// (e.g. after a daemon restart, or after Phase 3's eviction
// sweep). Returns the same registry for chaining.
//
// The resumer is configured separately from the store because the
// store is daemon-startup data (a SQL connection) while the
// resumer is a closure capturing the full compose.SessionFactoryDeps —
// the two have different construction sites and lifetimes.
// Calling WithResumer(nil) is a no-op (preserves the existing
// resumer); to disable, construct a fresh registry.
func (r *SessionRegistry) WithResumer(resumer SessionResumer) *SessionRegistry {
	if resumer == nil {
		return r
	}
	r.mu.Lock()
	r.resumer = resumer
	r.mu.Unlock()
	return r
}

// ErrSessionExists is returned by Register when the (app, user, sid)
// triple is already registered.
var ErrSessionExists = errors.New("attach: session already registered")

// ErrSessionNotFound is returned by Lookup / Unregister when no
// matching entry exists.
var ErrSessionNotFound = errors.New("attach: session not found")

// ErrACLOwnerNotTransferable is returned by AmendACL when the amended
// ACL names a different Owner. Ownership transfer is a decision with
// no undo from the losing side, so it is refused rather than performed
// — see AmendACL for the reasoning, and note that the HTTP surface
// maps this to 400 rather than 500.
var ErrACLOwnerNotTransferable = errors.New("attach: session owner is not transferable")

// ErrAmbiguousSession is returned by LookupSingle when more than one
// registered session shares the same SessionID across different
// apps — the caller must use the qualified two-segment form.
var ErrAmbiguousSession = errors.New("attach: session id is ambiguous across registered apps; use the /sessions/<app>/<sessionID> form")

// Register adds a session with no owner — legacy single-user behavior.
// Returns ErrSessionExists if the triple is already present (caller
// should not silently overwrite, since a double-register usually means
// an Agent construction race).
//
// In a multi-session deployment, sessions registered via Register are
// admin-only-accessible — Authorize denies non-Admin Callers because
// the ACL.Owner is empty. New code that knows the owning identity
// should use RegisterOwned instead.
func (r *SessionRegistry) Register(ag Registrant) (*Entry, error) {
	return r.registerWithACL(ag, auth.SessionACL{}, nil)
}

// RegisterOwned adds a session and stamps the supplied caller as the
// Owner of the ACL. Use from session creation paths that know the
// originating Caller (the typical multi-session attach setup: the
// daemon resolves the caller from the credential, then creates the
// session under that identity).
//
// Owner must be non-empty — pass Register if you intentionally want an
// unowned (admin-only-accessible) session.
func (r *SessionRegistry) RegisterOwned(ag Registrant, owner string) (*Entry, error) {
	return r.RegisterOwnedWithCancel(ag, owner, nil)
}

// RegisterOwnedWithCancel is RegisterOwned with a cancel func the
// registry invokes when the entry is evicted (idle sweep,
// DELETE /sessions via HardDelete, or explicit Unregister).
// The cancel typically shuts down the session's wake-loop goroutine
// so it exits cleanly instead of leaking past the session's
// lifetime.
//
// Passing nil is equivalent to RegisterOwned — no cancel invoked on
// evict. In-process consumers with no long-lived goroutines behind
// the Registrant (e.g., tests, admin-only utility sessions) can
// safely pass nil.
func (r *SessionRegistry) RegisterOwnedWithCancel(ag Registrant, owner string, cancelOnEvict context.CancelFunc) (*Entry, error) {
	return r.RegisterOwnedWithACL(ag, auth.SessionACL{Owner: owner}, cancelOnEvict)
}

// RegisterOwnedWithACL is RegisterOwnedWithCancel for a caller that
// knows more than the owner at creation time — a session opened on
// behalf of an incident channel whose responders should be able to
// answer it, say (#797). Viewers get read; Contributors get read +
// write; only the Owner (and any Admin) may amend the ACL afterwards.
//
// The identity lists are normalized (trimmed, de-duplicated, empties
// dropped) before they are stored or persisted, so the ACL that
// authorizes later requests is the one a GET /sessions/.../acl reports
// rather than whatever spelling arrived.
//
// Owner must be non-empty — pass Register if you intentionally want an
// unowned (admin-only-accessible) session.
func (r *SessionRegistry) RegisterOwnedWithACL(ag Registrant, acl auth.SessionACL, cancelOnEvict context.CancelFunc) (*Entry, error) {
	if acl.Owner == "" {
		return nil, errors.New("attach: RegisterOwned: owner identity is required (use Register for legacy unowned sessions)")
	}
	return r.registerWithACL(ag, acl.Normalized(), cancelOnEvict)
}

func (r *SessionRegistry) registerWithACL(ag Registrant, acl auth.SessionACL, cancelOnEvict context.CancelFunc) (*Entry, error) {
	if ag == nil {
		return nil, errors.New("attach: Register: nil Registrant")
	}
	key := tripleKey{App: ag.AppName(), User: ag.UserID(), SID: ag.SessionID()}
	if key.App == "" || key.SID == "" {
		return nil, fmt.Errorf("attach: Register: AppName and SessionID are required (got app=%q sid=%q)", key.App, key.SID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byTriple[key]; dup {
		return nil, fmt.Errorf("%w: %s/%s/%s", ErrSessionExists, key.App, key.User, key.SID)
	}
	r.regSeq++
	e := &Entry{
		AppName:       key.App,
		UserID:        key.User,
		SessionID:     key.SID,
		Agent:         ag,
		regSeq:        r.regSeq,
		cancelOnEvict: cancelOnEvict,
	}
	e.setACL(acl)
	e.touch() // seed lastTouchedNs so the very first sweep doesn't fire on a brand-new entry
	r.byTriple[key] = e
	// Persist the ACL row when a store is wired AND the ACL has
	// an Owner (legacy Register without owner stays in-memory
	// only — matches the "ACL row exists ⟺ session is resumable"
	// invariant from the design doc OQ #7).
	//
	// The persistence happens inside the registry mutex so a
	// concurrent Lookup that races with Register can't see an
	// in-memory entry without its persisted ACL row. The store
	// call is bounded — typical sub-ms against SQLite.
	if r.aclStore != nil && acl.Owner != "" {
		if err := r.aclStore.Put(context.Background(), SessionACLRow{
			AppName:      key.App,
			UserID:       key.User,
			SessionID:    key.SID,
			Owner:        acl.Owner,
			Viewers:      acl.Viewers,
			Contributors: acl.Contributors,
		}); err != nil {
			// Roll back the in-memory insert so we don't end up
			// with an unresumable session that an operator
			// thinks is durable. Surface the error so the
			// session-creation endpoint can return 500.
			delete(r.byTriple, key)
			return nil, fmt.Errorf("attach: persist session ACL: %w", err)
		}
	}
	return e, nil
}

// Unregister removes a session by its full triple. No-op (returns nil)
// when the entry doesn't exist — keeps shutdown paths idempotent.
//
// Does NOT delete the persisted ACL row when a store is wired —
// Unregister is about removing the in-memory entry (agent shutdown,
// eviction sweep). The ACL row stays on disk so the next Lookup
// miss can lazily resume the session (Phase 2). For hard removal
// that also clears the persisted ACL — as the DELETE /sessions
// endpoint requires — use HardDelete.
func (r *SessionRegistry) Unregister(appName, userID, sessionID string) {
	r.mu.Lock()
	key := tripleKey{App: appName, User: userID, SID: sessionID}
	entry := r.byTriple[key]
	delete(r.byTriple, key)
	r.mu.Unlock()
	// Fire the cancel outside the lock — it typically triggers a
	// wake-loop goroutine's exit, which may itself take other
	// locks. Nil-safe.
	if entry != nil && entry.cancelOnEvict != nil {
		entry.cancelOnEvict()
	}
}

// HardDelete removes the in-memory entry (like Unregister) AND
// deletes its persisted ACL row when a store is wired, so a
// subsequent Lookup miss can't lazy-resume the session from
// persistence. Idempotent — no error when either the in-memory
// entry or the ACL row is absent.
//
// Not cleaned by HardDelete (out of scope; the registry owns
// session identity, not session data):
//   - Underlying ADK/SQLite session records (see pkg/eventlog).
//     They become unreachable via the registry but stay on disk.
//   - broadcaster subscribers — the caller must Close the per-
//     entry broadcaster via broadcasterPool.Remove.
//
// The wake-loop cancel (cancelOnEvict) fires exactly as with
// Unregister, so goroutines tied to the session exit cleanly.
func (r *SessionRegistry) HardDelete(ctx context.Context, appName, userID, sessionID string) error {
	r.mu.Lock()
	key := tripleKey{App: appName, User: userID, SID: sessionID}
	entry := r.byTriple[key]
	delete(r.byTriple, key)
	aclStore := r.aclStore
	r.mu.Unlock()

	// Fire cancel outside the lock (same rationale as Unregister).
	if entry != nil && entry.cancelOnEvict != nil {
		entry.cancelOnEvict()
	}

	if aclStore != nil {
		if err := aclStore.Delete(ctx, appName, userID, sessionID); err != nil {
			return fmt.Errorf("attach: hard-delete session ACL: %w", err)
		}
	}
	return nil
}

// AmendACL rewrites the ACL of a registered session, in memory and on
// disk, and returns what was stored (normalized: trimmed,
// de-duplicated, empties dropped). Backs PATCH
// /sessions/{app}/{sid}/acl (#797), whose caller has already passed
// ActionSessionAdmin against the ACL being amended.
//
// The amendment is expressed as a function rather than a value because
// PATCH is a read-modify-write: an omitted list means "leave that one
// alone", so the new ACL is a function of the current one. Handing the
// registry a pre-computed value would put the read outside the lock,
// and two concurrent PATCHes — one setting viewers, one setting
// contributors — would each carry the other's field forward from a
// stale snapshot and silently drop one of the two edits. On an
// authorization decision that is not an acceptable race, so amend runs
// under r.mu against the live ACL.
//
// amend must therefore be pure and fast: no locking, no I/O, no
// re-entry into the registry. It receives a copy, so mutating the
// slices it is handed is safe.
//
// Owner is not transferable here — amend must return the session's
// current owner (or the zero value, which is read as "unchanged").
// A different one is ErrACLOwnerNotTransferable rather than a silent
// no-op, because handing a session to someone else is a decision with
// no undo from the losing side (the old owner immediately fails
// ActionSessionAdmin and can't take it back), and because the
// persisted row's owner index is what GET /sessions resolves an idle
// session's visibility from. If ownership transfer ever ships it
// should be its own verb.
//
// The in-memory swap happens first and is rolled back if persistence
// fails, mirroring registerWithACL: an ACL that authorizes live
// requests but vanishes at the next restart is worse than a failed
// PATCH, because nothing surfaces the divergence until the session
// resumes and a responder is silently 404'd.
//
// A session with no owner (legacy Register) has no persisted row by
// design, so its amended ACL stays in memory only — the same
// "ACL row exists ⟺ session is resumable" invariant registerWithACL
// keeps.
//
// Returns ErrSessionNotFound when the triple isn't registered.
func (r *SessionRegistry) AmendACL(ctx context.Context, appName, userID, sessionID string, amend func(auth.SessionACL) auth.SessionACL) (auth.SessionACL, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := tripleKey{App: appName, User: userID, SID: sessionID}
	e, ok := r.byTriple[key]
	if !ok {
		return auth.SessionACL{}, fmt.Errorf("%w: %s/%s/%s", ErrSessionNotFound, appName, userID, sessionID)
	}
	prev := e.CurrentACL()
	next := amend(prev).Normalized()
	if next.Owner == "" {
		next.Owner = prev.Owner
	}
	if next.Owner != prev.Owner {
		return auth.SessionACL{}, fmt.Errorf("%w: session owner is %q, got %q — send the current owner or omit the field", ErrACLOwnerNotTransferable, prev.Owner, next.Owner)
	}
	e.setACL(next)
	if r.aclStore == nil || next.Owner == "" {
		return next, nil
	}
	// Carry the existing row's CreatedAt / LastTouchedAt forward:
	// Put is a whole-row upsert that stamps now() over a zero value,
	// so writing without them would make every ACL edit look like a
	// brand-new session and re-sort the operator's GET /sessions.
	// Title is left zero on purpose — Put reads empty as "don't
	// touch", which is exactly right here.
	row := SessionACLRow{AppName: key.App, UserID: key.User, SessionID: key.SID}
	if existing, err := r.aclStore.Get(ctx, key.App, key.User, key.SID); err == nil {
		row.CreatedAt = existing.CreatedAt
		row.LastTouchedAt = existing.LastTouchedAt
	}
	row.Owner = next.Owner
	row.Viewers = next.Viewers
	row.Contributors = next.Contributors
	if err := r.aclStore.Put(ctx, row); err != nil {
		e.setACL(prev)
		return auth.SessionACL{}, fmt.Errorf("attach: persist session ACL: %w", err)
	}
	return next, nil
}

// Lookup returns the entry for the qualified (appName, sessionID) form.
// userID is not required for lookup — the registry searches across all
// registered userIDs for the (app, sid) pair.
//
// On miss, when a SessionResumer is wired (Phase 2 of session-resume),
// Lookup attempts to reconstruct the session from its persisted ACL
// row and registers the resulting entry before returning it.
// Concurrent misses for the same (app, sid) collapse via singleflight
// to a single resumer call.
//
// Returns ErrSessionNotFound when the session is neither in memory
// nor reconstructable (no persisted row, or no resumer configured).
// Returns the resumer's error verbatim for any other failure (factory
// error, store I/O error) so the handler can surface a 500 with the
// underlying cause.
func (r *SessionRegistry) Lookup(ctx context.Context, appName, sessionID string) (*Entry, error) {
	return r.lookupGated(ctx, appName, sessionID, nil)
}

// resumeGate is a per-request pre-authorization hook consulted on a
// Lookup miss BEFORE the SessionResumer runs. Resume is expensive —
// it constructs a full agent, loads instructions, replays the
// session's entire eventlog into the usage tracker, and spawns a wake
// loop — so a caller the ACL would reject must be turned away from
// the persisted row alone, not after the daemon already did the work
// (#484). The gate receives the persisted ACL row and returns nil to
// proceed; any error aborts the lookup and is returned verbatim (the
// handlers pass a not-found-shaped error so a deny stays
// indistinguishable from a genuine miss).
type resumeGate func(SessionACLRow) error

// lookupGated is Lookup with an optional pre-resume authorization
// gate. Internal — the HTTP handlers thread their per-request gate
// through here; the exported Lookup keeps the ungated signature.
func (r *SessionRegistry) lookupGated(ctx context.Context, appName, sessionID string, gate resumeGate) (*Entry, error) {
	if appName == "" || sessionID == "" {
		return nil, fmt.Errorf("attach: Lookup: appName and sessionID are required")
	}
	// Fast path: check the in-memory map. The gate deliberately does
	// NOT run here — in-memory hits are cheap and the caller's own
	// per-action authorize covers them (as it always has).
	r.mu.RLock()
	for k, e := range r.byTriple {
		if k.App == appName && k.SID == sessionID {
			r.mu.RUnlock()
			e.touch() // memory hit counts as activity — keep the sweep from evicting active sessions
			return e, nil
		}
	}
	resumer := r.resumer
	r.mu.RUnlock()

	if resumer == nil {
		return nil, fmt.Errorf("%w: %s/%s", ErrSessionNotFound, appName, sessionID)
	}
	return r.resumeAndRegister(ctx, appName, sessionID, resumer, gate)
}

// LookupSingle resolves the /sessions/<sessionID> shortcut. Returns
// ErrAmbiguousSession if the SessionID is registered against multiple
// apps — the caller should then use the qualified form and surface
// the helpful error to the client.
//
// On miss, behaves the same as Lookup w.r.t. resume — the resumer
// receives a fixed app name ("core-agent" by default; see
// resumerDefaultApp) since the shortcut form can't carry it. In
// practice every session in scope for resume comes from the
// same single app per daemon, so this is the right behavior.
func (r *SessionRegistry) LookupSingle(ctx context.Context, sessionID string) (*Entry, error) {
	return r.lookupSingleGated(ctx, sessionID, nil)
}

// lookupSingleGated is LookupSingle with an optional pre-resume
// authorization gate — see lookupGated.
func (r *SessionRegistry) lookupSingleGated(ctx context.Context, sessionID string, gate resumeGate) (*Entry, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("attach: LookupSingle: sessionID is required")
	}
	r.mu.RLock()
	var found *Entry
	for k, e := range r.byTriple {
		if k.SID == sessionID {
			if found != nil {
				r.mu.RUnlock()
				return nil, fmt.Errorf("%w: %s", ErrAmbiguousSession, sessionID)
			}
			found = e
		}
	}
	resumer := r.resumer
	r.mu.RUnlock()
	if found != nil {
		found.touch()
		return found, nil
	}
	if resumer == nil {
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}
	return r.resumeAndRegister(ctx, resumerDefaultApp, sessionID, resumer, gate)
}

// resumerDefaultApp is the AppName passed to SessionResumer.Resume
// when LookupSingle resolves the shortcut form (which carries no
// app). Matches the single-app-per-daemon assumption documented in
// docs/attach-mode-design.md — there's no production deployment
// today with multiple apps sharing one daemon.
const resumerDefaultApp = "core-agent"

// resumeAndRegister calls the resumer (deduped via singleflight) and
// registers the result under its own ACL. Returns the new Entry on
// success; ErrSessionNotFound when the resumer reports no persisted
// row; the underlying error otherwise.
//
// When a gate is supplied it is evaluated against the persisted ACL
// row BEFORE any resume work — and, critically, OUTSIDE the
// singleflight: flights are keyed by (app, sid) and shared across
// callers, so an unauthorized caller's deny must never become the
// shared flight result an authorized concurrent caller receives.
// The extra row read is one indexed point query. When no aclStore is
// wired the gate is skipped (nothing to authorize against — matches
// the pre-gate behavior, and production resume deployments always
// wire the store the resumer itself reads).
func (r *SessionRegistry) resumeAndRegister(ctx context.Context, app, sid string, resumer SessionResumer, gate resumeGate) (*Entry, error) {
	if gate != nil {
		r.mu.RLock()
		store := r.aclStore
		r.mu.RUnlock()
		if store != nil {
			row, err := store.FindByAppSID(ctx, app, sid)
			if err != nil {
				if errors.Is(err, ErrSessionACLNotFound) {
					return nil, fmt.Errorf("%w: %s/%s", ErrSessionNotFound, app, sid)
				}
				return nil, err
			}
			if err := gate(row); err != nil {
				return nil, err
			}
		}
	}
	key := app + "/" + sid
	v, err, _ := r.resumeFlight.Do(key, func() (any, error) {
		// Recheck the map under the lock — another goroutine may
		// have completed a parallel resume between our RUnlock and
		// the Singleflight entry. The singleflight gives us the
		// "exactly one Resume call" guarantee; we still need to
		// avoid double-registering when the racing goroutine wasn't
		// part of our flight.
		r.mu.RLock()
		for k, e := range r.byTriple {
			if k.App == app && k.SID == sid {
				r.mu.RUnlock()
				return e, nil
			}
		}
		r.mu.RUnlock()

		ag, acl, cancelOnEvict, resumeErr := resumer.Resume(ctx, app, sid)
		if resumeErr != nil {
			// The resumer surfaces ErrSessionACLNotFound when no
			// persisted row exists; translate to ErrSessionNotFound
			// so handlers map it to 404 uniformly with the in-memory
			// "no such session" case.
			if errors.Is(resumeErr, ErrSessionACLNotFound) {
				return nil, fmt.Errorf("%w: %s/%s", ErrSessionNotFound, app, sid)
			}
			return nil, resumeErr
		}
		entry, err := r.registerResumed(ag, acl, cancelOnEvict)
		if err != nil {
			// The resumer already spawned its wake loop; on
			// registration failure we must cancel to avoid leaking
			// the goroutine. Rare path (would need a triple
			// collision from a racing legacy Register, which the
			// design excludes for resumable sessions).
			if cancelOnEvict != nil {
				cancelOnEvict()
			}
			return nil, fmt.Errorf("attach: resume: register: %w", err)
		}
		return entry, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*Entry), nil
}

// registerResumed adds a resumer-constructed Registrant to the
// in-memory registry under its persisted ACL. Skips the aclStore
// Put — the row is already on disk (that's how the resumer
// learned about the session in the first place). Used only by
// resumeAndRegister; not exported.
//
// If the triple races and is already registered (concurrent
// resume + concurrent legacy Register, vanishingly unlikely),
// returns the existing entry rather than ErrSessionExists so the
// caller sees a successful lookup either way.
func (r *SessionRegistry) registerResumed(ag Registrant, acl auth.SessionACL, cancelOnEvict context.CancelFunc) (*Entry, error) {
	if ag == nil {
		return nil, errors.New("attach: registerResumed: nil Registrant")
	}
	key := tripleKey{App: ag.AppName(), User: ag.UserID(), SID: ag.SessionID()}
	if key.App == "" || key.SID == "" {
		return nil, fmt.Errorf("attach: registerResumed: AppName and SessionID are required (got app=%q sid=%q)", key.App, key.SID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byTriple[key]; ok {
		return existing, nil
	}
	r.regSeq++
	e := &Entry{
		AppName:       key.App,
		UserID:        key.User,
		SessionID:     key.SID,
		Agent:         ag,
		regSeq:        r.regSeq,
		cancelOnEvict: cancelOnEvict,
	}
	e.setACL(acl)
	e.touch() // seed lastTouchedNs — matches the initial-touch behavior of registerWithACL
	r.byTriple[key] = e
	// Persist a fresh LastTouchedAt on resume so GET /sessions
	// ordering reflects that this session is back in memory. The
	// aclStore row itself is unchanged (row was already written at
	// original creation); Touch only bumps LastTouchedAt.
	if r.aclStore != nil && acl.Owner != "" {
		// Best-effort — a store hiccup here shouldn't fail the
		// resume. The next sweep-time Touch will backfill.
		_ = r.aclStore.Touch(context.Background(), key.App, key.User, key.SID, time.Now())
	}
	return e, nil
}

// List returns a snapshot of every registered entry, sorted by
// (AppName, UserID, SessionID) for stable output. Used by GET /sessions
// so the operator sees a deterministic ordering across requests.
func (r *SessionRegistry) List() []*Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Entry, 0, len(r.byTriple))
	for _, e := range r.byTriple {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AppName != out[j].AppName {
			return out[i].AppName < out[j].AppName
		}
		if out[i].UserID != out[j].UserID {
			return out[i].UserID < out[j].UserID
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

// Len returns the number of registered sessions. Useful for tests +
// the listener's startup log.
func (r *SessionRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byTriple)
}

// ListAuthorized returns the subset of List() that c may
// auth.ActionSessionRead per each Entry's ACL. Used by the GET
// /sessions handler in multi-session mode so a Caller only sees
// sessions they have access to — hiding unauthorized session
// existence prevents leaking activity patterns.
//
// An Admin Caller sees everything (same as List); an anonymous Caller
// (Identity == "") sees nothing unless an Entry's ACL has an empty
// Owner AND empty Viewers/Contributors (which Authorize also rejects
// — so practically nothing).
func (r *SessionRegistry) ListAuthorized(c auth.Caller) []*Entry {
	all := r.List()
	out := make([]*Entry, 0, len(all))
	for _, e := range all {
		if auth.Authorize(c, auth.ActionSessionRead, e.CurrentACL()) {
			out = append(out, e)
		}
	}
	return out
}

// aclStoreForList exposes the store for the listSessions handler's
// union path. Internal — the handler needs the store to include
// evicted/post-restart sessions in GET /sessions results, but the
// store shouldn't be a public field on the registry. Returns
// (store, true) when a store is wired; (nil, false) otherwise so
// the handler can skip the persisted-half query cleanly on
// registries without one.
func (r *SessionRegistry) aclStoreForList() (SessionACLStore, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.aclStore == nil {
		return nil, false
	}
	return r.aclStore, true
}

// TouchEntry marks the entry for (app, sid) as active-right-now.
// The broadcaster calls this on every event pumped through the
// session so autonomous agent work (long-running tool calls,
// background compaction, etc.) counts as activity. Silent no-op
// on miss — the entry may have been evicted between the caller's
// Lookup and Touch, and we don't want to error on a benign race.
//
// Cheap (single atomic store, no mutex on the entry itself). Safe
// for concurrent callers.
func (r *SessionRegistry) TouchEntry(appName, sessionID string) {
	if appName == "" || sessionID == "" {
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k, e := range r.byTriple {
		if k.App == appName && k.SID == sessionID {
			e.touch()
			return
		}
	}
}

// EvictBefore removes every in-memory entry whose lastTouchedNs is
// older than cutoff and returns how many were removed. The evicted
// entries' cancelOnEvict funcs (if any) are invoked outside the
// registry lock so wake-loop shutdown doesn't contend on lookups.
//
// When aclStore is wired, the last-touched value at eviction time
// is persisted to the ACL row so GET /sessions and future resume
// see an accurate "last active" timestamp. The session title goes
// with it, if the registrant has one: an evicted session is
// answered from the row from here on, so this is the last point
// at which the title is still readable. Persistence is
// best-effort: a store error is logged upstream but doesn't block
// eviction (the in-memory removal is the correctness-critical
// half; the DB row is metadata).
//
// Callers: the sweep goroutine started by SweepIdle. Test code
// may call directly to exercise the eviction path without the
// ticker overhead.
func (r *SessionRegistry) EvictBefore(cutoff time.Time) int {
	cutoffNs := cutoff.UnixNano()
	// Two-phase to keep the critical section short. Phase 1:
	// snapshot candidates + remove from map under the write lock.
	// Phase 2: fire cancels + best-effort store Touch outside the
	// lock so slow cancels / DB writes don't block Lookup.
	type candidate struct {
		key           tripleKey
		entry         *Entry
		cancel        context.CancelFunc
		lastTouchedNs int64
	}
	var evicted []candidate
	r.mu.Lock()
	for k, e := range r.byTriple {
		if e.lastTouchedNs.Load() < cutoffNs {
			evicted = append(evicted, candidate{key: k, entry: e, cancel: e.cancelOnEvict, lastTouchedNs: e.lastTouchedNs.Load()})
			delete(r.byTriple, k)
		}
	}
	store := r.aclStore
	hook := r.evictHook
	r.mu.Unlock()

	for _, c := range evicted {
		if c.cancel != nil {
			c.cancel()
		}
		if hook != nil {
			// Retire per-entry server state bound to this Entry —
			// in production, the pool broadcaster (#486). Runs
			// outside the registry lock: the hook Closes the
			// broadcaster, which blocks on its goroutines draining.
			hook(c.entry)
		}
		if store != nil {
			// Persist the last-touched time we removed. The row
			// stays; only LastTouchedAt bumps. Silent on error —
			// the in-memory eviction already succeeded.
			_ = store.Touch(context.Background(), c.key.App, c.key.User, c.key.SID, time.Unix(0, c.lastTouchedNs))
			// And the title (#808), for the same reason: this is
			// the moment the session stops being answerable from
			// memory, so it is the last chance to write down what
			// the row could not otherwise know. Skipped when the
			// registrant has no title — SetTitle's "" clears, and
			// an untitled eviction must not wipe a title an
			// operator set by hand earlier.
			//
			// ErrSessionACLNotFound is the expected answer for a
			// session registered without an owner (in-memory only,
			// no row to write to), hence the discard.
			if title := entryTitle(c.entry); title != "" {
				_ = store.SetTitle(context.Background(), c.key.App, c.key.User, c.key.SID, title)
			}
		}
	}
	return len(evicted)
}

// SweepIdle runs the eviction sweep on a ticker until ctx is done.
// Every idleAfter/4 tick, evicts entries idle longer than
// idleAfter. Blocks until ctx cancels — call as a goroutine.
//
// idleAfter <= 0 disables the sweep (returns immediately). Callers
// wire the operator's session_idle_timeout config value here; the
// "0s → disabled" contract lives at the config parser, so a caller
// that passes 0 explicitly (rather than relying on defaults) means
// it.
//
// Ticker fires at 1/4 the idle window — coarse enough that the
// wake-up cost is negligible against the eviction budget, fine
// enough that an evictable session isn't held for more than ~25%
// of the idle window past its idle threshold. Under 60s tick
// intervals are floored to 60s so tests with tiny idleAfter don't
// spin the CPU.
func (r *SessionRegistry) SweepIdle(ctx context.Context, idleAfter time.Duration) {
	if idleAfter <= 0 {
		return
	}
	tick := idleAfter / 4
	if tick < time.Minute {
		// Enforce a floor in production. Tests that want a
		// tight loop should call EvictBefore directly rather
		// than piggy-backing on the ticker.
		tick = time.Minute
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			r.EvictBefore(now.Add(-idleAfter))
		}
	}
}
