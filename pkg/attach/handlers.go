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

	"google.golang.org/adk/session"
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
type handlers struct {
	reg  *SessionRegistry
	pool *BroadcasterPool
}

func newHandlers(reg *SessionRegistry, pool *BroadcasterPool) *handlers {
	return &handlers{reg: reg, pool: pool}
}

// register wires the handler set onto a mux. Routes use Go 1.22+
// pattern matching so {app}/{sid} is a clean two-segment match.
func (h *handlers) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /sessions", h.listSessions)

	// Qualified two-segment form: /sessions/<app>/<sid>/...
	mux.HandleFunc("GET /sessions/{app}/{sid}/events", h.eventsQualified)
	mux.HandleFunc("POST /sessions/{app}/{sid}/inject", h.injectQualified)
	mux.HandleFunc("POST /sessions/{app}/{sid}/wake", h.wakeQualified)
	mux.HandleFunc("POST /sessions/{app}/{sid}/interrupt", h.interruptQualified)

	// Read-only state endpoints — feed the TUI's /tools, /subagents,
	// /status slash commands. Pure projections over in-memory state;
	// safe for ReadOnly mode (the read-only flag gates POSTs only).
	mux.HandleFunc("GET /sessions/{app}/{sid}/tools", h.toolsQualified)
	mux.HandleFunc("GET /sessions/{app}/{sid}/agents", h.agentsQualified)
	mux.HandleFunc("GET /sessions/{app}/{sid}/status", h.statusQualified)

	// Single-segment shortcut: /sessions/<sid>/... — resolves when
	// SessionID is unambiguous across registered apps; 409 otherwise.
	// Registered after the qualified patterns so Go's routing prefers
	// the longer match.
	mux.HandleFunc("GET /sessions/{sid}/events", h.eventsShortcut)
	mux.HandleFunc("POST /sessions/{sid}/inject", h.injectShortcut)
	mux.HandleFunc("POST /sessions/{sid}/wake", h.wakeShortcut)
	mux.HandleFunc("POST /sessions/{sid}/interrupt", h.interruptShortcut)
	mux.HandleFunc("GET /sessions/{sid}/tools", h.toolsShortcut)
	mux.HandleFunc("GET /sessions/{sid}/agents", h.agentsShortcut)
	mux.HandleFunc("GET /sessions/{sid}/status", h.statusShortcut)

	// Operator-state read endpoints (usage / context / memory /
	// skills / mcp / pricing); see handlers_operator.go.
	h.registerOperatorState(mux)

	// PR D — HTTP-driven permission prompts (/perms/stream SSE +
	// /perms/respond POST); see handlers_prompts.go.
	h.registerPrompts(mux)
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
}

func (h *handlers) listSessions(w http.ResponseWriter, _ *http.Request) {
	out := make([]sessionDescriptor, 0, h.reg.Len())
	for _, e := range h.reg.List() {
		out = append(out, sessionDescriptor{
			AppName:     e.AppName,
			UserID:      e.UserID,
			SessionID:   e.SessionID,
			HasEventLog: e.Agent.EventLog() != nil,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (h *handlers) eventsQualified(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	sid := r.PathValue("sid")
	entry, err := h.reg.Lookup(app, sid)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	h.streamEvents(w, r, entry)
}

func (h *handlers) eventsShortcut(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	entry, err := h.reg.LookupSingle(sid)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	h.streamEvents(w, r, entry)
}

// streamEvents is the core SSE handler. Subscribes to the broadcaster,
// writes each frame as `event: agent` + JSON payload, flushes after
// every write. Returns when the client disconnects or the subscriber
// is dropped (slow).
func (h *handlers) streamEvents(w http.ResponseWriter, r *http.Request, entry *Entry) {
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
		buf, jerr := json.Marshal(frame)
		if jerr != nil {
			// Skip a frame we couldn't marshal rather than killing
			// the stream; surface in server logs but keep the wire
			// flowing for everything else.
			debugf("/events %s/%s seq=%d marshal error: %v", entry.AppName, entry.SessionID, frame.Seq, jerr)
			continue
		}
		// SSE framing: event type + data block + blank line.
		if _, werr := fmt.Fprintf(w, "event: agent\ndata: %s\n\n", buf); werr != nil {
			// Client disconnected mid-write. The ctx cancel from
			// r.Context() should already be propagating; just exit.
			debugf("/events %s/%s write error (client gone): %v", entry.AppName, entry.SessionID, werr)
			return
		}
		flusher.Flush()
		debugf("/events %s/%s wrote seq=%d (%d bytes)", entry.AppName, entry.SessionID, frame.Seq, len(buf))
	}
	debugf("/events %s/%s channel closed (subscriber dropped or ctx done)", entry.AppName, entry.SessionID)
}

type injectRequest struct {
	Message string `json:"message"`
}

func (h *handlers) injectQualified(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	sid := r.PathValue("sid")
	entry, err := h.reg.Lookup(app, sid)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	h.doInject(w, r, entry)
}

func (h *handlers) injectShortcut(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	entry, err := h.reg.LookupSingle(sid)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	h.doInject(w, r, entry)
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
	if err := entry.Agent.Inject(req.Message); err != nil {
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

func (h *handlers) wakeQualified(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	sid := r.PathValue("sid")
	entry, err := h.reg.Lookup(app, sid)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	h.doWake(w, r, entry)
}

func (h *handlers) wakeShortcut(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	entry, err := h.reg.LookupSingle(sid)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	h.doWake(w, r, entry)
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
	if req.Prompt != "" {
		if err := entry.Agent.Inject(req.Prompt); err != nil {
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

// --- /interrupt — cancel the in-flight turn -----------------------------
//
// Operator-driven cancel of whatever the agent is doing right now.
// Used by the TUI's ESC keybinding (when input is empty + a turn is
// in flight) and by scripted operators via curl. The agent's session,
// event log, registered subagents, and attach registration all
// survive the cancel — only the in-flight model call is interrupted.
//
// Response:
//   - 200 OK with `{interrupted: true, session: <sid>}` — there was
//     something in flight and the cancel fired.
//   - 200 OK with `{interrupted: false, session: <sid>}` + header
//     `X-Interrupted: nothing-in-flight` — agent is idle; no-op.
//   - 412 Precondition Failed — agent doesn't implement
//     InterruptProvider (older runtime; nothing to cancel from
//     the server's perspective).
//   - 403 Forbidden — when --attach-readonly is set; gated at the
//     middleware layer along with /inject and /wake.
//
// Audit: each successful cancel (interrupted=true) emits an
// eventlog row with Author="attach/interrupt" and
// CustomMetadata={source:"operator"} so the operator's intent is
// captured in the audit trail alongside the agent's own
// ctx.Canceled response.

func (h *handlers) interruptQualified(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	sid := r.PathValue("sid")
	entry, err := h.reg.Lookup(app, sid)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	h.doInterrupt(w, r, entry)
}

func (h *handlers) interruptShortcut(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	entry, err := h.reg.LookupSingle(sid)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	h.doInterrupt(w, r, entry)
}

func (h *handlers) doInterrupt(w http.ResponseWriter, r *http.Request, entry *Entry) {
	ip, ok := entry.Agent.(InterruptProvider)
	if !ok {
		http.Error(w, "interrupt: this agent does not implement InterruptProvider (older runtime?)", http.StatusPreconditionFailed)
		return
	}
	canceled := ip.AttachInterrupt()
	if canceled {
		// Best-effort audit row. Don't fail the request if the
		// emission errors — the cancel already fired.
		appendInterruptAudit(r.Context(), entry)
	} else {
		w.Header().Set("X-Interrupted", "nothing-in-flight")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"interrupted": canceled,
		"session":     entry.SessionID,
	})
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
	ev := session.NewEvent("attach-interrupt")
	ev.Author = "attach/interrupt"
	ev.CustomMetadata = map[string]any{"source": "operator"}
	_ = log.Service.AppendEvent(ctx, getResp.Session, ev)
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

func (h *handlers) toolsQualified(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	sid := r.PathValue("sid")
	entry, err := h.reg.Lookup(app, sid)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	h.doTools(w, entry)
}

func (h *handlers) toolsShortcut(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	entry, err := h.reg.LookupSingle(sid)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	h.doTools(w, entry)
}

func (h *handlers) doTools(w http.ResponseWriter, entry *Entry) {
	out := []ToolInfo{}
	if p, ok := entry.Agent.(ToolsProvider); ok {
		if list := p.AttachTools(); list != nil {
			out = list
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": out})
}

func (h *handlers) agentsQualified(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	sid := r.PathValue("sid")
	entry, err := h.reg.Lookup(app, sid)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	h.doAgents(w, entry)
}

func (h *handlers) agentsShortcut(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	entry, err := h.reg.LookupSingle(sid)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	h.doAgents(w, entry)
}

func (h *handlers) doAgents(w http.ResponseWriter, entry *Entry) {
	out := []AgentInfo{}
	if p, ok := entry.Agent.(AgentsProvider); ok {
		if list := p.AttachAgents(); list != nil {
			out = list
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

func (h *handlers) statusQualified(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	sid := r.PathValue("sid")
	entry, err := h.reg.Lookup(app, sid)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	h.doStatus(w, entry)
}

func (h *handlers) statusShortcut(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	entry, err := h.reg.LookupSingle(sid)
	if err != nil {
		writeLookupError(w, err)
		return
	}
	h.doStatus(w, entry)
}

func (h *handlers) doStatus(w http.ResponseWriter, entry *Entry) {
	var out StatusInfo
	if p, ok := entry.Agent.(StatusProvider); ok {
		out = p.AttachStatus()
	}
	// Ensure State is always populated so consumers don't have to
	// special-case "missing" vs "idle".
	if out.State == "" {
		out.State = AgentStateIdle
	}
	writeJSON(w, http.StatusOK, out)
}
