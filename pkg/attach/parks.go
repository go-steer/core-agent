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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	h.routeSession(mux, "GET", "parks", auth.ActionSessionRead, h.doSessionParks)
	h.routeSessionDrainGated(mux, "ack-effects", h.doSessionAckEffects)
	mux.HandleFunc("GET /parks", h.doListParks)
	mux.HandleFunc("GET /parks/{sid}", h.doGetParksBySession)
	mux.HandleFunc("GET /parks/{app}/{sid}", h.doGetParksByAppSession)
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

func (h *handlers) doGetParksByAppSession(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.lookupQualifiedAuth(w, r, auth.ActionSessionRead)
	if !ok {
		return
	}
	h.doSessionParks(w, r, entry)
}

func (h *handlers) doListParks(w http.ResponseWriter, r *http.Request) {
	allEntries := h.reg.List()
	entries := allEntries
	c, _ := auth.CallerFromContext(r.Context())
	if h.enforceACL {
		entries = h.reg.ListAuthorized(c)
	}
	seen := make(map[[2]string]struct{}, len(allEntries))
	sharedLog, knownApps := h.reg.eventLogForList()
	for _, entry := range allEntries {
		seen[[2]string{entry.AppName, entry.SessionID}] = struct{}{}
		if sharedLog == nil && entry.Agent != nil {
			sharedLog = entry.Agent.EventLog()
		}
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
	sessions = h.appendPersistedParks(r.Context(), c, sharedLog, knownApps, seen, sessions)
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].SessionID < sessions[j].SessionID
	})
	writeParksResult(w, ParksResult{Sessions: sessions})
}

func (h *handlers) appendPersistedParks(ctx context.Context, c auth.Caller, sharedLog *eventlog.Handle, knownApps []string, seen map[[2]string]struct{}, sessions []SessionParks) []SessionParks {
	if sharedLog == nil || sharedLog.Service == nil {
		return sessions
	}
	if h.enforceACL {
		store, ok := h.reg.aclStoreForList()
		if !ok {
			return sessions
		}
		rows, err := store.ListVisibleTo(ctx, c)
		if err != nil {
			return sessions
		}
		for _, row := range rows {
			if strings.HasSuffix(row.SessionID, ":mast-ops") {
				continue
			}
			key := [2]string{row.AppName, row.SessionID}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			sp, err := projectStoredSessionParks(ctx, sharedLog, row.AppName, row.UserID, row.SessionID)
			if err != nil || (len(sp.Parks) == 0 && sp.Hold == nil) {
				continue
			}
			sessions = append(sessions, sp)
		}
		return sessions
	}
	for _, appName := range knownApps {
		listResp, err := sharedLog.Service.List(ctx, &session.ListRequest{AppName: appName})
		if err != nil || listResp == nil {
			continue
		}
		for _, s := range listResp.Sessions {
			if s == nil || strings.HasSuffix(s.ID(), ":mast-ops") {
				continue
			}
			key := [2]string{s.AppName(), s.ID()}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			sp, err := projectStoredSessionParks(ctx, sharedLog, s.AppName(), s.UserID(), s.ID())
			if err != nil || (len(sp.Parks) == 0 && sp.Hold == nil) {
				continue
			}
			sessions = append(sessions, sp)
		}
	}
	return sessions
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
	if err := readOptionalJSON(r, &req, ackEffectsMaxBytes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.rejectDraining(w) {
		return
	}
	h.executeAckEffects(w, r, entry, strings.TrimSpace(req.Reason))
}

func readOptionalJSON(r *http.Request, v any, maxBytes int64) error {
	if r == nil || r.Body == nil {
		return nil
	}
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
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("malformed JSON: %w", err)
	}
	return nil
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
	if sp, ok := entry.Agent.(StatusProvider); ok {
		st := sp.AttachStatus()
		if st.TurnInFlight || st.State == AgentStateRunning {
			http.Error(w, "ack-effects: cannot acknowledge effects while a turn is active on this session", http.StatusConflict)
			return
		}
	}
	ctx := h.turnContext(r.Context())

	by := verifiedApprover(ctx)
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
	return populateSessionParksFromLog(ctx, hLog, entry.AppName, entry.UserID, entry.SessionID, sp)
}

func projectStoredSessionParks(ctx context.Context, hLog *eventlog.Handle, appName, userID, sessionID string) (SessionParks, error) {
	sp := SessionParks{
		SessionID: sessionID,
		State:     AgentStateIdle,
	}
	return populateSessionParksFromLog(ctx, hLog, appName, userID, sessionID, sp)
}

func populateSessionParksFromLog(ctx context.Context, hLog *eventlog.Handle, appName, userID, sessionID string, sp SessionParks) (SessionParks, error) {
	resp, err := hLog.Service.Get(ctx, &session.GetRequest{
		AppName:   appName,
		UserID:    userID,
		SessionID: sessionID,
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
		change := &ProposedCall{
			CallID: p.CallID,
			Tool:   p.ProposedTool,
			Args:   p.ProposedArgs,
			Key:    p.Subject,
			Agent:  p.Author,
		}
		populateProposedCallFromPayload(change, p.Payload)
		park.Change = change
	}
	return park
}

func populateProposedCallFromPayload(change *ProposedCall, payload any) {
	req, ok := payload.(map[string]any)
	if !ok || len(req) == 0 {
		return
	}
	if change.Key == "" {
		if k, ok := req["key"].(string); ok && k != "" {
			change.Key = k
		}
	}
	if policy, ok := req["policy"].(string); ok && policy != "" {
		change.Policy = policy
	}
	if a, ok := req["agent"].(string); ok && a != "" && change.Agent == "" {
		change.Agent = a
	}
	if stale, ok := req["stale"].(string); ok && stale != "" {
		change.Stale = stale
	}
	if vf, ok := req["verdict_format"].(map[string]any); ok && len(vf) > 0 {
		change.VerdictFormat = vf
	}
	if rawCS, ok := req["change_set"]; ok && rawCS != nil {
		if b, err := json.Marshal(rawCS); err == nil {
			var cs ChangeSet
			if json.Unmarshal(b, &cs) == nil {
				change.ChangeSet = &cs
			}
		}
	}
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
