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
	"net/http"
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

// Park kinds reported by GET /parks and GET /sessions/{sid}/parks.
const (
	// ParkKindApproval is a write-gate park: a mutating call waiting to
	// be approved, rejected or edited. Change is set.
	ParkKindApproval = "approval"
	// ParkKindInput is a question the agent asked —
	// request_operator_input, or an ADK RequestedInput. Change is nil.
	ParkKindInput = "input"

	captureStateKeyPrefix = "mast_effect_capture_"
	ackEffectsMaxBytes    = 4 * 1024
)

// ParksRequest asks for the parks on one session, or on every parked
// session when SessionID is empty.
type ParksRequest struct {
	SessionID string `json:"session_id,omitempty"`
}

// ParksResult is the body of GET /parks, GET /parks/{session}, and
// GET /sessions/{sid}/parks.
type ParksResult struct {
	Sessions []SessionParks `json:"sessions"`
}

// SessionParks is one session's parks projection.
type SessionParks struct {
	SessionID string        `json:"session_id"`
	State     string        `json:"state"`
	Awaiting  string        `json:"awaiting,omitempty"`
	Parks     []Park        `json:"parks,omitempty"`
	Hold      *Hold         `json:"hold,omitempty"`
	Applied   []AppliedCall `json:"applied,omitempty"`
}

// Park is one open interrupt on a session.
type Park struct {
	InterruptID string        `json:"interrupt_id"`
	Kind        string        `json:"kind"`
	Question    string        `json:"question,omitempty"`
	Author      string        `json:"author,omitempty"`
	RaisedAt    time.Time     `json:"raised_at"`
	Change      *ProposedCall `json:"change,omitempty"`
}

// ProposedCall is a tool call waiting on a verdict.
type ProposedCall struct {
	CallID        string         `json:"call_id,omitempty"`
	Tool          string         `json:"tool"`
	Args          map[string]any `json:"args,omitempty"`
	Key           string         `json:"key,omitempty"`
	Policy        string         `json:"policy,omitempty"`
	Agent         string         `json:"agent,omitempty"`
	Stale         string         `json:"stale,omitempty"`
	VerdictFormat map[string]any `json:"verdict_format,omitempty"`
	ChangeSet     *ChangeSet     `json:"change_set,omitempty"`
}

// ChangeSet is the approved-as-a-unit set a parked call belongs to.
type ChangeSet struct {
	Specialist    string            `json:"specialist,omitempty"`
	Changes       []Call            `json:"changes,omitempty"`
	Grantable     bool              `json:"grantable"`
	Ungrantable   string            `json:"ungrantable,omitempty"`
	TTLSeconds    int               `json:"ttl_seconds,omitempty"`
	Preconditions map[string]string `json:"preconditions,omitempty"`
}

// Call is a tool call named without a verdict attached to it.
type Call struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args,omitempty"`
}

// Hold is an active operator-placed gate pause.
type Hold struct {
	Reason    string    `json:"reason,omitempty"`
	Message   string    `json:"message,omitempty"`
	Token     string    `json:"token,omitempty"`
	MintedAt  time.Time `json:"minted_at,omitzero"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	ResumeAt  time.Time `json:"resume_at,omitzero"`
}

// AppliedCall is a mutating call that already ran and that the gate
// captured prior state for.
type AppliedCall struct {
	CallID      string         `json:"call_id,omitempty"`
	Tool        string         `json:"tool"`
	Args        map[string]any `json:"args,omitempty"`
	Key         string         `json:"key,omitempty"`
	CapturedAt  time.Time      `json:"captured_at,omitzero"`
	Read        string         `json:"read,omitempty"`
	Digest      string         `json:"digest,omitempty"`
	PriorFields []string       `json:"prior_fields,omitempty"`
	Revert      *Call          `json:"revert,omitempty"`
}

// AckEffectsRequest records an operator's acknowledgement of ambiguous
// prior effects on a session.
type AckEffectsRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// AckEffectsResponse is the 200 body of POST /ack-effects and
// POST /sessions/{sid}/ack-effects.
type AckEffectsResponse struct {
	Session string    `json:"session"`
	AckedAt time.Time `json:"acked_at"`
	By      string    `json:"by,omitempty"`
	Reason  string    `json:"reason,omitempty"`
}

// ParksProvider is an optional Registrant capability that returns the
// session's parks projection directly. When absent, the server projects
// parks from the registrant's EventLog.
type ParksProvider interface {
	AttachParks(ctx context.Context) (SessionParks, error)
}

// EffectsAcker is an optional Registrant capability that records an
// operator acknowledgement of ambiguous prior effects. When absent, the
// server records the watermark on the registrant's EventLog.
type EffectsAcker interface {
	AttachAckEffects(ctx context.Context, by, reason string) (time.Time, error)
}

// detachedTurnContext detaches a turn-driving context from the HTTP request's
// cancellation while preserving request-scoped values (authenticated Caller,
// OTel trace span) and bounding execution by the daemon's lifecycle context
// when wired (mast #562). Because it delegates Done/Err/Deadline to parent without
// spawning a child WithCancel or AfterFunc, it never leaks goroutines and
// remains live after the HTTP handler returns.
type detachedTurnContext struct {
	context.Context
	values context.Context
}

func (d detachedTurnContext) Value(key any) any {
	return d.values.Value(key)
}

func (h *handlers) turnContext(reqCtx context.Context) context.Context {
	detached := context.WithoutCancel(reqCtx)
	if h.daemonCtx == nil {
		return detached
	}
	return detachedTurnContext{
		Context: h.daemonCtx,
		values:  detached,
	}
}

func (h *handlers) registerParks(mux *http.ServeMux) {
	h.routeSessionDrainGated(mux, "GET", "parks", auth.ActionSessionRead, h.doSessionParks)
	h.routeSessionDrainGated(mux, "POST", "ack-effects", auth.ActionSessionWrite, h.doSessionAckEffects)
	mux.HandleFunc("GET /parks", h.doListParks)
	mux.HandleFunc("GET /parks/{sid}", h.doGetParksBySession)
	mux.HandleFunc("POST /ack-effects", h.doTopLevelAckEffects)
}

func (h *handlers) doSessionParks(w http.ResponseWriter, r *http.Request, entry *Entry) {
	sp, err := projectEntryParks(r.Context(), entry)
	if err != nil {
		http.Error(w, "park read failed", http.StatusInternalServerError)
		return
	}
	writeParksResult(w, ParksResult{Sessions: []SessionParks{sp}})
}

func (h *handlers) doGetParksBySession(w http.ResponseWriter, r *http.Request) {
	if h.draining() {
		h.rejectDraining(w)
		return
	}
	sid := r.PathValue("sid")
	if sid == "" {
		http.Error(w, "parks: session id is required", http.StatusBadRequest)
		return
	}
	entry, ok := h.lookupShortcutAuth(w, r, auth.ActionSessionRead)
	if !ok {
		return
	}
	h.doSessionParks(w, r, entry)
}

func (h *handlers) doListParks(w http.ResponseWriter, r *http.Request) {
	if h.draining() {
		h.rejectDraining(w)
		return
	}
	entries := h.reg.List()
	if h.enforceACL {
		c, _ := auth.CallerFromContext(r.Context())
		entries = h.reg.ListAuthorized(c)
	}
	sessions := make([]SessionParks, 0)
	for _, entry := range entries {
		sp, err := projectEntryParks(r.Context(), entry)
		if err != nil {
			http.Error(w, "park read failed", http.StatusInternalServerError)
			return
		}
		if len(sp.Parks) == 0 && sp.Hold == nil {
			continue
		}
		sessions = append(sessions, sp)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].SessionID < sessions[j].SessionID
	})
	writeParksResult(w, ParksResult{Sessions: sessions})
}

func writeParksResult(w http.ResponseWriter, out ParksResult) {
	if out.Sessions == nil {
		out.Sessions = []SessionParks{}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func (h *handlers) doSessionAckEffects(w http.ResponseWriter, r *http.Request, entry *Entry) {
	var req AckEffectsRequest
	if r.ContentLength > 0 {
		if err := readJSON(r, &req, ackEffectsMaxBytes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if h.rejectDraining(w) {
		return
	}
	h.executeAckEffects(w, r, entry, strings.TrimSpace(req.Reason))
}

func (h *handlers) doTopLevelAckEffects(w http.ResponseWriter, r *http.Request) {
	if h.rejectDraining(w) {
		return
	}
	var req AckEffectsRequest
	if err := readJSON(r, &req, ackEffectsMaxBytes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	if req.SessionID == "" {
		http.Error(w, "ack-effects: session_id is required", http.StatusBadRequest)
		return
	}
	entry, err := h.reg.lookupSingleGated(r.Context(), req.SessionID, h.resumeGateFor(r))
	if err != nil {
		writeLookupError(w, err)
		return
	}
	if !h.authorize(w, r, entry, auth.ActionSessionWrite) {
		return
	}
	if h.rejectDraining(w) {
		return
	}
	h.executeAckEffects(w, r, entry, strings.TrimSpace(req.Reason))
}

func (h *handlers) executeAckEffects(w http.ResponseWriter, r *http.Request, entry *Entry, reason string) {
	ctx := h.turnContext(r.Context())

	by := verifiedApprover(ctx)
	if by == "" {
		c, _ := auth.CallerFromContext(ctx)
		by = c.Identity
	}
	if acker, ok := entry.Agent.(EffectsAcker); ok {
		at, err := acker.AttachAckEffects(ctx, by, reason)
		if err != nil {
			http.Error(w, fmt.Sprintf("ack-effects: %v", err), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, AckEffectsResponse{
			Session: entry.SessionID,
			AckedAt: at,
			By:      by,
			Reason:  reason,
		})
		return
	}
	hLog := entry.Agent.EventLog()
	if hLog == nil {
		http.Error(w, "ack-effects: this session has no durable eventlog", http.StatusNotImplemented)
		return
	}
	at, err := hLog.AckEffects(ctx, entry.AppName, entry.UserID, entry.SessionID, by, reason)
	if err != nil {
		http.Error(w, fmt.Sprintf("ack-effects: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, AckEffectsResponse{
		Session: entry.SessionID,
		AckedAt: at,
		By:      by,
		Reason:  reason,
	})
}

func projectEntryParks(ctx context.Context, entry *Entry) (SessionParks, error) {
	if entry == nil || entry.Agent == nil {
		return SessionParks{}, errors.New("attach: nil session entry")
	}
	if pp, ok := entry.Agent.(ParksProvider); ok {
		return pp.AttachParks(ctx)
	}
	sp := SessionParks{
		SessionID: entry.SessionID,
		State:     AgentStateIdle,
	}
	if pc, ok := entry.Agent.(PauseController); ok {
		st := pc.AttachPauseState()
		if st.Paused {
			sp.State = AgentStatePaused
			sp.Hold = &Hold{
				Reason:   st.Reason,
				MintedAt: st.Since,
			}
		}
	}
	hLog := entry.Agent.EventLog()
	if hLog == nil || hLog.Service == nil {
		return sp, nil
	}
	resp, err := hLog.Service.Get(ctx, &session.GetRequest{
		AppName:   entry.AppName,
		UserID:    entry.UserID,
		SessionID: entry.SessionID,
	})
	if err != nil {
		if eventlog.IsSessionNotFound(err) {
			return sp, nil
		}
		return SessionParks{}, err
	}
	pending := eventlog.ScanPendingParks(resp.Session.Events())
	for _, p := range pending {
		park := toAttachPark(p)
		sp.Parks = append(sp.Parks, park)
		if park.Kind == ParkKindApproval {
			sp.Awaiting = ParkKindApproval
		} else if sp.Awaiting == "" {
			sp.Awaiting = ParkKindInput
		}
	}
	if len(sp.Parks) > 0 {
		sp.State = AgentStatePaused
	}
	sp.Applied = scanAppliedCallsFromEvents(resp.Session.Events())
	return sp, nil
}

func toAttachPark(p eventlog.ParkedCall) Park {
	kind := ParkKindInput
	if p.Awaiting == eventlog.AwaitingApproval {
		kind = ParkKindApproval
	}
	park := Park{
		InterruptID: p.InterruptID,
		Kind:        kind,
		Question:    p.Message,
		Author:      p.Author,
		RaisedAt:    p.RaisedAt,
	}
	if kind == ParkKindApproval {
		park.Change = &ProposedCall{
			Tool:  p.ProposedTool,
			Args:  p.ProposedArgs,
			Key:   p.Subject,
			Agent: p.Author,
		}
	}
	return park
}

type wireCaptureRecord struct {
	Tool           string            `json:"tool"`
	Key            string            `json:"key"`
	Arguments      map[string]any    `json:"arguments"`
	CapturedAt     time.Time         `json:"captured_at"`
	Read           string            `json:"read"`
	Prior          map[string]any    `json:"prior"`
	PriorFields    []string          `json:"prior_fields,omitempty"`
	Digest         string            `json:"digest"`
	Revert         *wireRevertChange `json:"revert,omitempty"`
	FunctionCallID string            `json:"function_call_id,omitempty"`
}

type wireRevertChange struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

func decodeWireCapture(raw any) (wireCaptureRecord, bool) {
	var data []byte
	switch v := raw.(type) {
	case string:
		data = []byte(v)
	case []byte:
		data = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return wireCaptureRecord{}, false
		}
		data = b
	}
	var rec wireCaptureRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return wireCaptureRecord{}, false
	}
	return rec, true
}

func scanAppliedCallsFromEvents(events session.Events) []AppliedCall {
	if events == nil {
		return nil
	}
	var out []AppliedCall
	for ev := range events.All() {
		if ev == nil || len(ev.Actions.StateDelta) == 0 {
			continue
		}
		keys := make([]string, 0, len(ev.Actions.StateDelta))
		for k := range ev.Actions.StateDelta {
			if strings.HasPrefix(k, captureStateKeyPrefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			rec, ok := decodeWireCapture(ev.Actions.StateDelta[k])
			if !ok {
				continue
			}
			priorFields := rec.PriorFields
			if len(priorFields) == 0 {
				priorFields = sortedMapKeys(rec.Prior)
			}
			ac := AppliedCall{
				CallID:      rec.FunctionCallID,
				Tool:        rec.Tool,
				Args:        rec.Arguments,
				Key:         rec.Key,
				CapturedAt:  rec.CapturedAt,
				Read:        rec.Read,
				Digest:      rec.Digest,
				PriorFields: priorFields,
			}
			if rec.Revert != nil {
				ac.Revert = &Call{
					Tool: rec.Revert.Tool,
					Args: rec.Revert.Arguments,
				}
			}
			out = append(out, ac)
		}
	}
	return out
}

func sortedMapKeys(m map[string]any) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
