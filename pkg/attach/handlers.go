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

package attach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// injectMaxBytes caps the size of the POST /inject body. 8 KiB is
// plenty for operator nudges; larger payloads are probably misuse
// and we want to fail fast.
const injectMaxBytes = 8 * 1024

// wakeMaxBytes caps the size of the POST /wake body. Wake bodies are
// tiny (optional target + optional prompt); 8 KiB matches /inject.
const wakeMaxBytes = 8 * 1024

// handlers bundles the dependencies the HTTP handlers need. Construct
// via newHandlers; the server wires it onto a *http.ServeMux.
//
// enforceACL turns on per-session Authorize checks in
// session-scoped handlers. Set by the server from
// Options.MultiSessionEnabled; default false preserves single-user
// behavior (every request passes the auth gate).
type handlers struct {
	reg        *SessionRegistry
	pool       *broadcasterPool
	enforceACL bool
	// factory, when non-nil, enables the POST /sessions endpoint.
	// Set from Options.SessionFactory by the Server constructor.
	factory SessionFactory
	// costLimit is the per-caller token bucket applied to the
	// cost-bearing endpoints (slash ops, POST /sessions,
	// pricing/refresh). Set from Options.CostRateLimit by the Server
	// constructor; nil (the default for bare newHandlers, and for
	// CostRateLimit.Disabled) means no enforcement. See rate_limit.go.
	costLimit *costRateLimiter
	// closing is closed by Server.Close (before srv.Shutdown) to wake
	// long-lived streaming handlers that do NOT ride the broadcaster
	// pool — today the /perms/stream prompt feed. Without it those
	// streams held Shutdown hostage for the full ShutdownTimeout,
	// exactly like /events did before its pool.Close reorder (#488).
	// /events itself needs no select on this: closing the pool closes
	// its subscriber channels.
	closing chan struct{}
	// daemonCtx is the daemon's lifetime context (Options.DaemonCtx);
	// consulted by draining(). Nil = shutdown gating disabled.
	daemonCtx context.Context
}

func newHandlers(reg *SessionRegistry, pool *broadcasterPool) *handlers {
	return &handlers{reg: reg, pool: pool, closing: make(chan struct{})}
}

// draining reports whether the daemon's lifetime context has been
// cancelled (SIGTERM has fired). The window between ctx-cancel and
// Server.Close is where wake loops are already dead but the listener
// still accepts requests — a message queued there lands in the
// IN-MEMORY inbox and dies with the process, after the client got a
// 200 (#564). Mutating intake endpoints check this and 503 instead so
// clients redeliver after the restart. nil daemonCtx (bare
// newHandlers, tests, embedders that don't wire it) never drains.
func (h *handlers) draining() bool {
	if h.daemonCtx == nil {
		return false
	}
	select {
	case <-h.daemonCtx.Done():
		return true
	default:
		return false
	}
}

// rejectDraining writes the 503 + Retry-After response for intake
// during shutdown. Returns true when the request was rejected.
func (h *handlers) rejectDraining(w http.ResponseWriter) bool {
	if !h.draining() {
		return false
	}
	w.Header().Set("Retry-After", "5")
	http.Error(w, "daemon is shutting down; queued messages would be lost — retry after restart", http.StatusServiceUnavailable)
	return true
}

// authorize checks the request's Caller against entry.ACL for the
// given action. Returns true when allowed; returns false and writes a
// 404 (NOT 403, intentionally — hiding session existence from
// unauthorized callers prevents activity-pattern enumeration) on
// deny.
//
// Always returns true when enforceACL is false — the no-enforcement
// posture preserves single-user behavior.
func (h *handlers) authorize(w http.ResponseWriter, r *http.Request, entry *Entry, action auth.Action) bool {
	if !h.enforceACL {
		return true
	}
	c, _ := auth.CallerFromContext(r.Context())
	if auth.Authorize(c, action, entry.ACL) {
		return true
	}
	// Same body the lookup-not-found path uses so a 404 from the auth
	// gate is indistinguishable from a 404 for a session that genuinely
	// doesn't exist. Operator-side audit logs (with caller identity)
	// are how a denied access is investigated.
	http.Error(w, "session not found", http.StatusNotFound)
	return false
}

// resumeGateFor returns the pre-resume authorization gate for this
// request, or nil when ACL enforcement is off. A Lookup miss lazily
// RESUMES the session — constructing a full agent, replaying its
// eventlog, spawning a wake loop — so the caller must be checked
// against the persisted ACL row BEFORE that work runs, not only by
// the post-lookup authorize (#484). The gate uses ActionSessionRead
// as the resume bar (the weakest session action — anyone the ACL
// admits at all may cause a resume); the per-action authorize still
// runs on the resumed entry afterwards, exactly as before.
//
// A deny returns the same not-found-shaped error a genuine miss
// produces, so writeLookupError emits an indistinguishable 404 — no
// session-existence oracle (mirrors authorize's 404-not-403 choice).
func (h *handlers) resumeGateFor(r *http.Request) resumeGate {
	if !h.enforceACL {
		return nil
	}
	c, _ := auth.CallerFromContext(r.Context())
	return func(row SessionACLRow) error {
		if auth.Authorize(c, auth.ActionSessionRead, row.ACL()) {
			return nil
		}
		return fmt.Errorf("%w: %s/%s", ErrSessionNotFound, row.AppName, row.SessionID)
	}
}

// lookupQualifiedAuth resolves a /sessions/{app}/{sid}/... handler's
// target entry AND runs the per-action authorization check in one
// call. Returns (entry, true) on success; writes the appropriate
// error response and returns (nil, false) otherwise.
func (h *handlers) lookupQualifiedAuth(w http.ResponseWriter, r *http.Request, action auth.Action) (*Entry, bool) {
	app := r.PathValue("app")
	sid := r.PathValue("sid")
	entry, err := h.reg.lookupGated(r.Context(), app, sid, h.resumeGateFor(r))
	if err != nil {
		writeLookupError(w, err)
		return nil, false
	}
	if !h.authorize(w, r, entry, action) {
		return nil, false
	}
	return entry, true
}

// lookupShortcutAuth is the single-segment counterpart to
// lookupQualifiedAuth — used by handlers wired to the
// /sessions/{sid}/... shortcut routes.
func (h *handlers) lookupShortcutAuth(w http.ResponseWriter, r *http.Request, action auth.Action) (*Entry, bool) {
	sid := r.PathValue("sid")
	entry, err := h.reg.lookupSingleGated(r.Context(), sid, h.resumeGateFor(r))
	if err != nil {
		writeLookupError(w, err)
		return nil, false
	}
	if !h.authorize(w, r, entry, action) {
		return nil, false
	}
	return entry, true
}

// routeSession registers one session-scoped endpoint under BOTH URL
// forms — the qualified /sessions/{app}/{sid} route and the
// /sessions/{sid} shortcut (which resolves when the SessionID is
// unambiguous across registered apps; 409 otherwise) — wiring entry
// lookup plus the per-action authorization check in front of fn.
//
// method is the HTTP verb; suffix is the path below the session
// segment ("events", "perms/allow", ...) or "" for the bare session
// URL (DELETE /sessions/...). Every fn runs with lookup + auth
// already done, exactly like the former per-endpoint Qualified /
// Shortcut wrapper pairs this helper replaced.
func (h *handlers) routeSession(mux *http.ServeMux, method, suffix string, action auth.Action, fn func(http.ResponseWriter, *http.Request, *Entry)) {
	tail := ""
	if suffix != "" {
		tail = "/" + suffix
	}
	mux.HandleFunc(method+" /sessions/{app}/{sid}"+tail, func(w http.ResponseWriter, r *http.Request) {
		if entry, ok := h.lookupQualifiedAuth(w, r, action); ok {
			fn(w, r, entry)
		}
	})
	mux.HandleFunc(method+" /sessions/{sid}"+tail, func(w http.ResponseWriter, r *http.Request) {
		if entry, ok := h.lookupShortcutAuth(w, r, action); ok {
			fn(w, r, entry)
		}
	})
}

// routeSessionDrainGated is routeSession with the shutdown drain gate
// run BEFORE entry lookup (#564 review). Same rationale as
// routeSessionLimited/#484: a Lookup miss lazily resumes the session
// — full agent construction, tracker replay, a wake loop spawned off
// the already-cancelled daemon ctx — which is pure waste when the
// response is going to be 503, exactly while the process races its
// termination grace period.
func (h *handlers) routeSessionDrainGated(mux *http.ServeMux, method, suffix string, action auth.Action, fn func(http.ResponseWriter, *http.Request, *Entry)) {
	tail := ""
	if suffix != "" {
		tail = "/" + suffix
	}
	mux.HandleFunc(method+" /sessions/{app}/{sid}"+tail, func(w http.ResponseWriter, r *http.Request) {
		if h.rejectDraining(w) {
			return
		}
		if entry, ok := h.lookupQualifiedAuth(w, r, action); ok {
			fn(w, r, entry)
		}
	})
	mux.HandleFunc(method+" /sessions/{sid}"+tail, func(w http.ResponseWriter, r *http.Request) {
		if h.rejectDraining(w) {
			return
		}
		if entry, ok := h.lookupShortcutAuth(w, r, action); ok {
			fn(w, r, entry)
		}
	})
}

// routeSessionLimited is routeSession with the per-caller cost
// limiter run BEFORE entry lookup. The order matters (#484): a
// Lookup miss lazily resumes the session — constructing a full
// agent, replaying its eventlog into the tracker, spawning a wake
// loop — which is exactly the work the limiter exists to bound.
// The pre-#484 limitCost wrapper ran after lookup, so a caller who
// was about to be 429'd forced that work anyway on every call.
//
// Consequence: an over-limit caller gets 429 before the 404-vs-200
// lookup outcome is computed. Deliberate — it also closes the mild
// existence/cost oracle the old order exposed.
//
// Always POST + ActionSessionWrite — every cost-bearing endpoint
// mutates or drives model work, so unlike routeSession there are no
// method/action parameters.
func (h *handlers) routeSessionLimited(mux *http.ServeMux, suffix string, fn func(http.ResponseWriter, *http.Request, *Entry)) {
	const method = "POST"
	const action = auth.ActionSessionWrite
	tail := ""
	if suffix != "" {
		tail = "/" + suffix
	}
	mux.HandleFunc(method+" /sessions/{app}/{sid}"+tail, func(w http.ResponseWriter, r *http.Request) {
		if !h.allowCost(w, r) {
			return
		}
		if entry, ok := h.lookupQualifiedAuth(w, r, action); ok {
			fn(w, r, entry)
		}
	})
	mux.HandleFunc(method+" /sessions/{sid}"+tail, func(w http.ResponseWriter, r *http.Request) {
		if !h.allowCost(w, r) {
			return
		}
		if entry, ok := h.lookupShortcutAuth(w, r, action); ok {
			fn(w, r, entry)
		}
	})
}

// register wires the handler set onto a mux. Routes use Go 1.22+
// pattern matching so {app}/{sid} is a clean two-segment match.
func (h *handlers) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /sessions", h.listSessions)
	// POST /sessions creates a new owned session via the
	// SessionFactory closure (cmd-level wiring). 501 when the
	// factory is nil, so older deployments behave as today.
	// Cost-limited (#463): each create constructs a fresh agent.
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		if !h.allowCost(w, r) {
			return
		}
		h.createSession(w, r)
	})

	// Session-scoped endpoints — each registered under both the
	// qualified and shortcut URL forms via routeSession.
	h.routeSession(mux, "GET", "events", auth.ActionSessionRead, h.streamEvents)
	h.routeSessionDrainGated(mux, "POST", "inject", auth.ActionSessionWrite, h.doInject)
	h.routeSessionDrainGated(mux, "POST", "wake", auth.ActionSessionWrite, h.doWake)
	h.routeSession(mux, "POST", "interrupt", auth.ActionSessionWrite, h.doInterrupt)
	// v1.5.0 hold surface — /pause, /resume, /agents/{name}/stop;
	// see handlers_pause.go.
	h.registerPause(mux)

	// Read-only state endpoints — feed the TUI's /tools, /status, and
	// subagent surfaces. Pure projections over in-memory state; safe for
	// ReadOnly mode (the read-only flag gates POSTs only). Two subagent
	// reads, deliberately distinct: /agents is "what's running" (live
	// spawned instances), /subagents is "what's configured" (the roster
	// the daemon loaded, spawnable by reference — #627).
	h.routeSession(mux, "GET", "tools", auth.ActionSessionRead, h.doTools)
	h.routeSession(mux, "GET", "agents", auth.ActionSessionRead, h.doAgents)
	h.routeSession(mux, "GET", "subagents", auth.ActionSessionRead, h.doSubagents)
	h.routeSession(mux, "GET", "status", auth.ActionSessionRead, h.doStatus)
	// A subagent's persisted inner turns (#638) — the read path for
	// "why did that subagent loop?", which /agents (status only) and
	// the parent-scoped /events SSE stream both leave unanswerable.
	// Not in pollingReadRe: this is a one-shot diagnostic, not a
	// status-bar poll, so it keeps its traces.
	h.routeSession(mux, "GET", "agents/{name}/events", auth.ActionSessionRead, h.doSubagentEvents)

	// Operator-state read endpoints (usage / context / memory /
	// skills / mcp / pricing); see handlers_operator.go.
	h.registerOperatorState(mux)

	// PR D — HTTP-driven permission prompts (/perms/stream SSE +
	// /perms/respond POST); see handlers_prompts.go.
	h.registerPrompts(mux)

	// GET /whoami — session-agnostic caller-identity echo
	// (SSE spec v1.4.0 companion, see handlers_whoami.go).
	h.registerWhoAmI(mux)
}

// sessionDescriptor is one row in the GET /sessions response.
type sessionDescriptor struct {
	AppName   string `json:"app"`
	UserID    string `json:"user"`
	SessionID string `json:"sessionID"`
	// HasEventLog reports whether the agent was wired with an
	// eventlog; live-tail only works for sessions where this is
	// true. Surface explicitly so a client doesn't try /events
	// against a session that has no log.
	HasEventLog bool `json:"has_event_log"`
	// Status is "active" for entries live in the in-memory
	// registry and "idle" for entries known only from the
	// persisted ACL store (evicted or post-restart). Requesting
	// /events on an idle session triggers a lazy resume via the
	// SessionResumer; the operator UX should render the
	// distinction so the click-in latency is expected.
	Status string `json:"status"`
	// LastTouchedAt is the last activity timestamp. For active
	// sessions this comes from the in-memory Entry (bumped by
	// broadcaster.pump + Lookup hits). For idle sessions it
	// comes from the persisted ACL row. Zero when neither
	// source has it.
	LastTouchedAt time.Time `json:"last_touched_at,omitempty"`
}

const (
	sessionStatusActive = "active"
	sessionStatusIdle   = "idle"
)

func (h *handlers) listSessions(w http.ResponseWriter, r *http.Request) {
	// In-memory half — sessions currently registered. This is
	// the pre-v2.5 behavior in isolation; the union with
	// persisted rows below is what turns "list" into a full
	// operator inventory (docs/session-resume-design.md OQ #1).
	memEntries := h.reg.List()
	c, _ := auth.CallerFromContext(r.Context())
	if h.enforceACL {
		// Filter to sessions the caller may read; hide the others'
		// existence per the design's "no leaking activity patterns"
		// invariant.
		memEntries = h.reg.ListAuthorized(c)
	}
	// Build descriptors keyed by (app, sid) so the persisted-only
	// half can dedup against them (in-memory wins).
	seen := make(map[[2]string]struct{}, len(memEntries))
	out := make([]sessionDescriptor, 0, len(memEntries))
	for _, e := range memEntries {
		key := [2]string{e.AppName, e.SessionID}
		seen[key] = struct{}{}
		out = append(out, sessionDescriptor{
			AppName:       e.AppName,
			UserID:        e.UserID,
			SessionID:     e.SessionID,
			HasEventLog:   e.Agent.EventLog() != nil,
			Status:        sessionStatusActive,
			LastTouchedAt: e.LastTouchedAt(),
		})
	}
	// Persisted-only half — sessions the caller can read that
	// aren't in the in-memory registry (evicted, or post-restart
	// pre-resume). Only populated when ACL enforcement is on
	// AND the registry has a store; otherwise skip cleanly.
	if h.enforceACL {
		if store, ok := h.reg.aclStoreForList(); ok {
			rows, err := store.ListVisibleTo(r.Context(), c)
			if err == nil { // best-effort — a store hiccup mustn't zero out the list
				for _, row := range rows {
					key := [2]string{row.AppName, row.SessionID}
					if _, dup := seen[key]; dup {
						continue // already reflected as "active"
					}
					out = append(out, sessionDescriptor{
						AppName:       row.AppName,
						UserID:        row.UserID,
						SessionID:     row.SessionID,
						HasEventLog:   true, // sessions in the ACL store were created with eventlog wired
						Status:        sessionStatusIdle,
						LastTouchedAt: row.LastTouchedAt,
					})
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// streamEvents is the core SSE handler. Subscribes to the broadcaster,
// writes each frame as `event: agent` + JSON payload, flushes after
// every write. Returns when the client disconnects or the subscriber
// is dropped (slow).
func (h *handlers) streamEvents(w http.ResponseWriter, r *http.Request, entry *Entry) {
	// Reject a protocol-incompatible client cleanly before opening the
	// SSE stream: a major-version skew means the wire shape differs and
	// the client would silently mis-render (#389). Always echoes the
	// server's version back via the X-Attach-Protocol-Version header.
	if !negotiateProtocolVersion(w, r) {
		return
	}
	since := parseSince(r.URL.Query().Get("since"))
	if entry.Agent.EventLog() == nil {
		http.Error(w, "this session has no event log; attach requires --session-db", http.StatusPreconditionFailed)
		return
	}
	bcast, err := h.pool.For(entry)
	if err != nil {
		http.Error(w, fmt.Sprintf("broadcaster init failed: %v", err), http.StatusInternalServerError)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "server does not support streaming (no http.Flusher)", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx-style proxy buffering
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	debugf("/events subscribe %s/%s since=%d", entry.AppName, entry.SessionID, since)
	ch := bcast.Subscribe(r.Context(), since)
	for frame := range ch {
		// Two frame shapes, distinguished by Type. Typed frames carry
		// a protocol event (capabilities / status-update / etc.) and
		// marshal TypedData directly. Legacy frames carry an eventlog
		// seq + ADK session.Event, marshal the whole Frame, and use
		// the back-compat `event: agent` name.
		eventName := EventAgent
		var payload any = frame
		if frame.Type != "" {
			eventName = frame.Type
			payload = frame.TypedData
		}
		buf, jerr := json.Marshal(payload)
		if jerr != nil {
			// Skip a frame we couldn't marshal rather than killing
			// the stream; surface in server logs but keep the wire
			// flowing for everything else.
			debugf("/events %s/%s type=%s seq=%d marshal error: %v",
				entry.AppName, entry.SessionID, eventName, frame.Seq, jerr)
			continue
		}
		// SSE framing: event type + data block + blank line.
		if _, werr := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, buf); werr != nil {
			// Client disconnected mid-write. The ctx cancel from
			// r.Context() should already be propagating; just exit.
			debugf("/events %s/%s write error (client gone): %v",
				entry.AppName, entry.SessionID, werr)
			return
		}
		flusher.Flush()
		debugf("/events %s/%s wrote type=%s seq=%d (%d bytes)",
			entry.AppName, entry.SessionID, eventName, frame.Seq, len(buf))
	}
	debugf("/events %s/%s channel closed (subscriber dropped or ctx done)", entry.AppName, entry.SessionID)
}

type injectRequest struct {
	Message string `json:"message"`
}

func (h *handlers) doInject(w http.ResponseWriter, r *http.Request, entry *Entry) {
	var req injectRequest
	if err := readJSON(r, &req, injectMaxBytes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Message == "" {
		http.Error(w, "inject: message is required", http.StatusBadRequest)
		return
	}
	// Second drain check, AFTER the body read: the route-level gate
	// runs before an unbounded body read (no ReadTimeout on the
	// listener), so a request that passed it pre-SIGTERM can trickle
	// its body across the cancel instant. Re-check here so the 503
	// lands instead of an acknowledged-then-lost message; a duplicate
	// on client redelivery beats a silent loss (#564 review P1).
	if h.rejectDraining(w) {
		return
	}
	caller, _ := auth.CallerFromContext(r.Context())
	if err := entry.Agent.InjectAs(req.Message, caller); err != nil {
		http.Error(w, fmt.Sprintf("inject: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"injected": req.Message,
		"session":  entry.SessionID,
	})
}

type wakeRequest struct {
	// Target is reserved for the future multi-subagent wake shape
	// described in attach-mode-design.md. v1 always wakes the
	// session's primary agent; non-empty Target returns 501.
	Target string `json:"target,omitempty"`
	// Prompt, when supplied, is also injected into the inbox before
	// wake fires (equivalent to a paired inject + wake from the
	// operator). Empty just wakes without queuing a message.
	Prompt string `json:"prompt,omitempty"`
}

func (h *handlers) doWake(w http.ResponseWriter, r *http.Request, entry *Entry) {
	var req wakeRequest
	// Body is optional for /wake (unlike /inject); accept empty.
	if r.ContentLength > 0 {
		if err := readJSON(r, &req, wakeMaxBytes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if req.Target != "" {
		http.Error(w, "wake: per-subagent target is not yet implemented; omit 'target' to wake the session", http.StatusNotImplemented)
		return
	}
	// Second drain check after the (optional, unbounded) body read —
	// same TOCTOU closure as doInject (#564 review P1). A bare wake
	// also matters: 200 promises a turn the dead wake loop never runs.
	if h.rejectDraining(w) {
		return
	}
	if req.Prompt != "" {
		caller, _ := auth.CallerFromContext(r.Context())
		if err := entry.Agent.InjectAs(req.Prompt, caller); err != nil {
			http.Error(w, fmt.Sprintf("wake: inject prompt: %v", err), http.StatusInternalServerError)
			return
		}
	}
	entry.Agent.RequestWake()
	writeJSON(w, http.StatusOK, map[string]any{
		"woken":  entry.SessionID,
		"prompt": req.Prompt,
	})
}

// --- /interrupt — stop the in-flight turn, and hold the loop ------------
//
// Operator-driven stop of whatever the agent is doing right now. Used
// by the TUI's ESC keybinding and by scripted operators via curl. The
// agent's session, event log, registered subagents, and attach
// registration all survive — only the in-flight model call is
// interrupted.
//
// As of protocol v1.5.0 an interrupt also PARKS the loop by default
// (body `{"hold": false}` opts out, restoring the pre-v1.5.0
// cancel-and-carry-on behavior). Holding is what makes the operator's
// stop stick: without it the wake loop, the autonomous scheduler, or
// auto-continue would start a fresh turn moments later and the
// operator would watch the thing they just killed resume itself. The
// parked loop waits for POST /resume — or for any operator /inject,
// which resumes implicitly so the long-standing interrupt-then-inject
// client pattern behaves exactly as it did before.
//
// Response (InterruptResponse):
//   - 200 OK `{interrupted, paused, running_subagents, ...}` — see
//     pause.go. interrupted=false + header
//     `X-Interrupted: nothing-in-flight` when the agent was idle; with
//     hold that is still a state change, so paused is true.
//   - 412 Precondition Failed — agent implements neither
//     PauseController nor InterruptProvider (older runtime; nothing to
//     cancel from the server's perspective).
//   - 403 Forbidden — when --attach-readonly is set; gated at the
//     middleware layer along with /inject and /wake.
//
// Audit: each successful cancel (interrupted=true) emits an
// eventlog row with Author="attach/interrupt" and
// CustomMetadata={source:"operator"} so the operator's intent is
// captured in the audit trail alongside the agent's own
// ctx.Canceled response.

func (h *handlers) doInterrupt(w http.ResponseWriter, r *http.Request, entry *Entry) {
	var req InterruptRequest
	// Body is optional; an empty POST is the v1.5.0 default (hold).
	if r.ContentLength > 0 {
		if err := readJSON(r, &req, pauseMaxBytes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	hold := req.Hold == nil || *req.Hold

	pc, hasPause := entry.Agent.(PauseController)
	ip, hasInterrupt := entry.Agent.(InterruptProvider)
	if !hasPause && !hasInterrupt {
		http.Error(w, "interrupt: this agent does not implement InterruptProvider (older runtime?)", http.StatusPreconditionFailed)
		return
	}

	// heldViaController: the hold path armed the audit row itself.
	heldViaController := hold && hasPause
	var canceled, paused bool
	switch {
	case heldViaController:
		// One call so the cancel and the hold are atomic with respect
		// to each other: a wake racing the interrupt can't slip a fresh
		// turn in between them.
		canceled, paused = pc.AttachInterruptHold("")
	case hold:
		// Registrant predates PauseController. Cancel anyway rather
		// than 501 — a stop that half-lands beats no stop — but say so
		// on the wire so a client rendering "paused" doesn't lie.
		w.Header().Set("X-Hold", "unsupported")
		canceled = ip.AttachInterrupt()
	default:
		canceled = ip.AttachInterrupt()
	}
	// Report the gate's ACTUAL state rather than this branch's intent.
	// An agent parked by an earlier /pause is still parked after a
	// {"hold": false} cancel — that flag declines to ADD a hold, it
	// doesn't lift one — and a client rendering its banner from this
	// response would otherwise clear it and show a running agent whose
	// loop is stopped.
	if hasPause {
		paused = pc.AttachPauseState().Paused
	}

	if canceled {
		// Best-effort audit row. Don't fail the request if the
		// emission errors — the cancel already fired.
		//
		// Prefer a self-auditing registrant (#565): it appends the
		// audit from inside its own turn loop, after the interrupted
		// turn unwinds, so the write can't race the runner's in-flight
		// session handle and get mislabeled as a stale-session error.
		// Fall back to the out-of-band append for older registrants
		// that don't implement InterruptSelfAuditor.
		//
		// AttachInterruptHold arms the pending row itself, before it
		// fires the cancel — the ordering matters (see
		// Agent.InterruptAndHold), so don't re-arm it here.
		if sa, ok := entry.Agent.(InterruptSelfAuditor); ok {
			if !heldViaController {
				sa.MarkInterruptPending()
			}
		} else {
			appendInterruptAudit(r.Context(), entry)
		}
	} else {
		w.Header().Set("X-Interrupted", "nothing-in-flight")
	}

	resp := InterruptResponse{
		Session:     entry.SessionID,
		Interrupted: canceled,
		Paused:      paused,
	}
	if req.StopSubagents {
		resp.StoppedSubagents = stopAllSubagents(entry)
	}
	resp.RunningSubagents = runningSubagents(entry)
	writeJSON(w, http.StatusOK, resp)
}

// stopAllSubagents stops every running background subagent, for
// /interrupt's `stop_subagents` flag. Best-effort per subagent: one
// that errors (or finished between the list and the stop) is skipped
// rather than failing the whole interrupt — the parent cancel has
// already landed and the response's running_subagents shows whatever
// survived.
func stopAllSubagents(entry *Entry) []RunningSubagent {
	st, ok := entry.Agent.(AgentStopper)
	if !ok {
		return nil
	}
	var out []RunningSubagent
	for _, sa := range runningSubagents(entry) {
		stopped, err := st.AttachStopAgent(sa.Name)
		if err != nil || !stopped {
			continue
		}
		out = append(out, sa)
	}
	return out
}

// appendInterruptAudit writes one event row recording the operator's
// interrupt intent. Author + CustomMetadata identify the source so a
// later audit query (or attach /events tail) can distinguish
// operator-initiated cancels from any other ctx.Canceled flowing
// through the agent loop. Best-effort: an eventlog write failure
// is logged-only — never fails the HTTP request.
func appendInterruptAudit(ctx context.Context, entry *Entry) {
	log := entry.Agent.EventLog()
	if log == nil {
		return
	}
	getResp, err := log.Service.Get(ctx, &session.GetRequest{
		AppName:   entry.AppName,
		UserID:    entry.UserID,
		SessionID: entry.SessionID,
	})
	if err != nil {
		return
	}
	_ = log.Service.AppendEvent(ctx, getResp.Session, NewInterruptAuditEvent())
}

// NewInterruptAuditEvent builds the eventlog row that records an
// operator-initiated interrupt. Author + CustomMetadata identify the
// source so a later audit query (or attach /events tail) can
// distinguish operator cancels from any other ctx.Canceled flowing
// through the agent loop. Shared by the handler's out-of-band fallback
// (appendInterruptAudit) and by self-auditing registrants
// (InterruptSelfAuditor) so the two paths can never drift (#565).
func NewInterruptAuditEvent() *session.Event {
	ev := session.NewEvent("attach-interrupt")
	ev.Author = "attach/interrupt"
	ev.CustomMetadata = map[string]any{"source": "operator"}
	return ev
}

// readJSON decodes JSON into v with a size cap. Returns an error
// usable as an HTTP body.
func readJSON(r *http.Request, v any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBytes)
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return fmt.Errorf("request body too large (max %d bytes)", maxBytes)
		}
		return fmt.Errorf("read body: %w", err)
	}
	if len(body) == 0 {
		return errors.New("request body is empty")
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("malformed JSON: %w", err)
	}
	return nil
}

// writeJSON writes status + JSON-marshaled payload. Best-effort —
// errors here are logged at the layer above (caller's already given
// up if the marshal fails, and the network write isn't recoverable).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeLookupError maps registry errors onto HTTP statuses.
func writeLookupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrSessionNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrAmbiguousSession):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

// parseSince extracts the ?since=N query parameter. Invalid /
// missing values return 0 (replay from the start, which is also the
// "no prior cursor" default).
func parseSince(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// --- /tools, /agents, /status — read-only state projections --------------
//
// Each handler looks up the Entry, then type-asserts against the
// matching optional provider interface (ToolsProvider /
// AgentsProvider / StatusProvider). When the agent doesn't implement
// the provider, the response is the zero shape (empty list / zero
// struct) — never 501, so a TUI that fans these out at startup
// against mixed-vintage agents doesn't have to special-case errors.

func (h *handlers) doTools(w http.ResponseWriter, _ *http.Request, entry *Entry) {
	out := []ToolInfo{}
	if p, ok := entry.Agent.(ToolsProvider); ok {
		if list := p.AttachTools(); list != nil {
			out = list
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": out})
}

func (h *handlers) doAgents(w http.ResponseWriter, _ *http.Request, entry *Entry) {
	out := []AgentInfo{}
	if p, ok := entry.Agent.(AgentsProvider); ok {
		if list := p.AttachAgents(); list != nil {
			out = list
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

// doSubagents projects the CONFIGURED subagent roster (#627) — declarative
// templates + predefined catalog specs the daemon loaded — as opposed to
// doAgents' live/spawned instances. Empty list (never 501) when the agent
// implements no SubagentCatalogProvider or no manager is wired.
func (h *handlers) doSubagents(w http.ResponseWriter, _ *http.Request, entry *Entry) {
	out := []SubagentCatalogInfo{}
	if p, ok := entry.Agent.(SubagentCatalogProvider); ok {
		if list := p.AttachSubagentCatalog(); list != nil {
			out = list
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"subagents": out})
}

func (h *handlers) doStatus(w http.ResponseWriter, _ *http.Request, entry *Entry) {
	var out StatusInfo
	if p, ok := entry.Agent.(StatusProvider); ok {
		out = p.AttachStatus()
	}
	// Ensure State is always populated so consumers don't have to
	// special-case "missing" vs "idle".
	if out.State == "" {
		out.State = AgentStateIdle
	}
	// Pause is projected here rather than left to each StatusProvider:
	// the gate lives on the agent, so folding it in centrally means a
	// registrant can't report "idle" while the loop is actually parked
	// (which is what a paused agent looked like before v1.5.0). A
	// StatusProvider that already reported paused keeps its own fields.
	if pc, ok := entry.Agent.(PauseController); ok && out.State != AgentStatePaused {
		if st := pc.AttachPauseState(); st.Paused {
			out.State = AgentStatePaused
			out.PausedSince = st.Since
			out.PauseReason = st.Reason
			out.Interrupted = st.Interrupted
		}
	}
	writeJSON(w, http.StatusOK, out)
}
