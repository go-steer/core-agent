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
// Server side (agent process):
//
//	reg := attach.NewSessionRegistry()
//	ag, _ := agent.New(m, agent.WithSessionRegistry(reg), ...)
//	srv, _ := attach.NewServer(reg, attach.Options{
//	    Addr:     ":7777",
//	    TLSCert:  "/etc/certs/server.crt",
//	    TLSKey:   "/etc/certs/server.key",
//	    ClientCA: "/etc/certs/ca.crt",  // mTLS
//	    ReadOnly: false,
//	})
//	go srv.ListenAndServe()
//
// Client side (operator on a laptop, or another binary):
//
//	core-agent ls https://pod-ip:7777
//	core-agent attach https://pod-ip:7777/sessions/<app>/<sid>
//
// The protocol is HTTP + Server-Sent Events. Four endpoints:
//
//	GET  /sessions                                   list active sessions
//	GET  /sessions/<app>/<sid>/events?since=N        SSE event stream
//	POST /sessions/<app>/<sid>/inject                queue an inbox message
//	POST /sessions/<app>/<sid>/wake                  wake a deferred subagent
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

	"github.com/go-steer/core-agent/pkg/auth"
	"github.com/go-steer/core-agent/pkg/eventlog"
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
	RequestWake()
}

// SessionRegistry holds every Agent that opted into attach-mode by
// calling agent.WithSessionRegistry at construction. Keys by the full
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
	// (docs/session-resume-design.md): persistence only; lazy
	// resume on Lookup miss lands in Phase 2 (separate field +
	// wiring on the registry).
	aclStore SessionACLStore
}

// Entry is one registered session as the registry sees it.
type Entry struct {
	AppName   string
	UserID    string
	SessionID string

	// Agent is the live registrant. The registry holds it by
	// reference; lifetime is the registrant's, not the registry's.
	Agent Registrant

	// ACL governs which Callers may interact with this session in a
	// multi-session deployment. Zero value (empty Owner / nil slices)
	// means "no owner" — only Admin Callers may access it, which is
	// the documented behavior for legacy sessions registered via
	// Register (vs. RegisterOwned). See
	// docs/multi-session-design.md §"Migration story".
	ACL auth.SessionACL
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
// upserts on Register; the future Phase 2 SessionResumer reads it
// back on Lookup miss to reconstruct evicted sessions.
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

// ErrSessionExists is returned by Register when the (app, user, sid)
// triple is already registered.
var ErrSessionExists = errors.New("attach: session already registered")

// ErrSessionNotFound is returned by Lookup / Unregister when no
// matching entry exists.
var ErrSessionNotFound = errors.New("attach: session not found")

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
	return r.registerWithACL(ag, auth.SessionACL{})
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
	if owner == "" {
		return nil, errors.New("attach: RegisterOwned: owner identity is required (use Register for legacy unowned sessions)")
	}
	return r.registerWithACL(ag, auth.SessionACL{Owner: owner})
}

func (r *SessionRegistry) registerWithACL(ag Registrant, acl auth.SessionACL) (*Entry, error) {
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
	e := &Entry{
		AppName:   key.App,
		UserID:    key.User,
		SessionID: key.SID,
		Agent:     ag,
		ACL:       acl,
	}
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
// future eviction sweep). The ACL row stays on disk so the next
// Lookup miss can lazily resume the session (Phase 2). For hard
// removal that also clears the persisted ACL, use a future
// DELETE /sessions endpoint (out of scope here).
func (r *SessionRegistry) Unregister(appName, userID, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byTriple, tripleKey{App: appName, User: userID, SID: sessionID})
}

// Lookup returns the entry for the qualified (appName, sessionID) form.
// userID is not required for lookup — the registry searches across all
// registered userIDs for the (app, sid) pair. Returns ErrSessionNotFound
// when there's no match. The full-triple form is what's stored
// internally; this just searches.
func (r *SessionRegistry) Lookup(appName, sessionID string) (*Entry, error) {
	if appName == "" || sessionID == "" {
		return nil, fmt.Errorf("attach: Lookup: appName and sessionID are required")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k, e := range r.byTriple {
		if k.App == appName && k.SID == sessionID {
			return e, nil
		}
	}
	return nil, fmt.Errorf("%w: %s/%s", ErrSessionNotFound, appName, sessionID)
}

// LookupSingle resolves the /sessions/<sessionID> shortcut. Returns
// ErrAmbiguousSession if the SessionID is registered against multiple
// apps — the caller should then use the qualified form and surface
// the helpful error to the client.
func (r *SessionRegistry) LookupSingle(sessionID string) (*Entry, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("attach: LookupSingle: sessionID is required")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var found *Entry
	for k, e := range r.byTriple {
		if k.SID == sessionID {
			if found != nil {
				return nil, fmt.Errorf("%w: %s", ErrAmbiguousSession, sessionID)
			}
			found = e
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}
	return found, nil
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
		if auth.Authorize(c, auth.ActionSessionRead, e.ACL) {
			out = append(out, e)
		}
	}
	return out
}
