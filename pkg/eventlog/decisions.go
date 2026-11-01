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

package eventlog

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/go-steer/core-agent/v3/pkg/approval"
)

const (
	// EffectsAckTimeKey is the session-state key under which AckEffects records
	// the operator's acknowledgement watermark for ambiguous prior effects.
	EffectsAckTimeKey = "mast_effects_ack_time"
	// EffectsAckReasonKey is the session-state key under which AckEffects records
	// the operator's acknowledgement reason.
	EffectsAckReasonKey = "mast_effects_ack_reason"
	// EffectsAckByKey is the session-state key under which AckEffects records
	// the operator identity.
	EffectsAckByKey = "mast_effects_ack_by"
	// RedactionApproverDigest is the default export mode: human approvers are
	// replaced with approval.RedactApprover's SHA-256 prefix, and machine
	// identities pass through in the clear.
	RedactionApproverDigest = "approver-sha256"
	// RedactionNone marks an export written with IncludeApprover: true.
	RedactionNone = "none"

	exportArgsWarning = "proposed_args and executed_args are exported verbatim and may contain resource names, namespaces, or command flags from the target environment; review before sharing outside the operator boundary"

	reservedOpsSuffix = ":mast-ops"

	// AwaitingApproval indicates a session is parked on a write-gate confirmation.
	AwaitingApproval = "approval"
	// AwaitingInput indicates a session is parked on an operator input question.
	AwaitingInput = "input"
)

// agentApprovalDecisionRow is the GORM model for agent_approval_decisions.
// Persisting decisions synchronously from approval.WithDecisionSink before a
// mutating tool executes closes the crash window between tool dispatch and
// ADK's post-tool AppendEvent (#1400, mast #561).
type agentApprovalDecisionRow struct {
	ID             int64     `gorm:"primaryKey;autoIncrement"`
	SessionID      string    `gorm:"not null;index:idx_approval_decisions_session;uniqueIndex:idx_approval_decisions_call,priority:1"`
	InvocationID   string    `gorm:"index:idx_approval_decisions_invocation"`
	FunctionCallID string    `gorm:"not null;uniqueIndex:idx_approval_decisions_call,priority:2"`
	Tool           string    `gorm:"not null"`
	Outcome        string    `gorm:"not null"`
	Scope          string    `gorm:"not null"`
	Disposition    string    `gorm:"not null"`
	Approver       string    `gorm:"not null"`
	DecidedAt      time.Time `gorm:"not null;index:idx_approval_decisions_decided_at"`
	PayloadJSON    string    `gorm:"type:text;not null"`
}

// TableName pins the SQL table name.
func (agentApprovalDecisionRow) TableName() string { return "agent_approval_decisions" }

// DecisionStore persists write-gate Decision records in agent_approval_decisions.
// A nil *DecisionStore is a safe no-op.
type DecisionStore struct {
	db *gorm.DB
}

// NewDecisionStore constructs a DecisionStore over an existing GORM connection
// and AutoMigrates agent_approval_decisions.
func NewDecisionStore(ctx context.Context, db *gorm.DB) (*DecisionStore, error) {
	if db == nil {
		return nil, errors.New("eventlog: NewDecisionStore: db is required")
	}
	if err := db.WithContext(ctx).AutoMigrate(&agentApprovalDecisionRow{}); err != nil {
		return nil, fmt.Errorf("eventlog: AutoMigrate agent_approval_decisions: %w", err)
	}
	return &DecisionStore{db: db}, nil
}

// RecordDecision persists a write-gate Decision record before tool execution.
func (s *DecisionStore) RecordDecision(ctx context.Context, d approval.Decision) error {
	if s == nil || s.db == nil {
		return nil
	}
	if d.Session == "" {
		return errors.New("eventlog: DecisionStore.RecordDecision: session is required")
	}
	raw, err := approval.EncodeDecision(d)
	if err != nil {
		return fmt.Errorf("eventlog: DecisionStore.RecordDecision encode: %w", err)
	}
	callID := d.FunctionCallID
	if callID == "" {
		callID = fmt.Sprintf("%s:%d", d.Tool, d.DecidedAt.UnixNano())
	}
	decidedAt := d.DecidedAt
	if decidedAt.IsZero() {
		decidedAt = time.Now().UTC()
	}
	row := agentApprovalDecisionRow{
		SessionID:      d.Session,
		InvocationID:   d.Invocation,
		FunctionCallID: callID,
		Tool:           d.Tool,
		Outcome:        string(d.Outcome),
		Scope:          string(d.Scope),
		Disposition:    string(d.Disposition),
		Approver:       d.Approver,
		DecidedAt:      decidedAt,
		PayloadJSON:    raw,
	}
	err = s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "session_id"}, {Name: "function_call_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"invocation_id", "tool", "outcome", "scope", "disposition", "approver", "decided_at", "payload_json"}),
		}).
		Create(&row).Error
	if err != nil {
		return fmt.Errorf("eventlog: DecisionStore.RecordDecision: %w", err)
	}
	return nil
}

// ListDecisions returns the durable decisions recorded in SQL for sessionID
// (or across all sessions when sessionID is empty), oldest first.
func (s *DecisionStore) ListDecisions(ctx context.Context, sessionID string) ([]approval.Decision, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	tx := s.db.WithContext(ctx).Model(&agentApprovalDecisionRow{})
	if sessionID != "" {
		tx = tx.Where("session_id = ?", sessionID)
	}
	var rows []agentApprovalDecisionRow
	if err := tx.Order("decided_at ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("eventlog: DecisionStore.ListDecisions: %w", err)
	}
	out := make([]approval.Decision, 0, len(rows))
	for _, r := range rows {
		d, err := approval.DecodeDecision(r.PayloadJSON)
		if err != nil {
			continue
		}
		if d.FunctionCallID == "" {
			d.FunctionCallID = r.FunctionCallID
		}
		out = append(out, d)
	}
	return out, nil
}

// HasDecision reports whether a Decision record for functionCallID in
// sessionID exists in agent_approval_decisions. It matches
// approval.DecisionLookup so write-gate plugins can detect pre-crash
// dispatches across process restarts (#1400, mast #561).
func (s *DecisionStore) HasDecision(ctx context.Context, sessionID, functionCallID string) (bool, error) {
	if s == nil || s.db == nil || sessionID == "" || functionCallID == "" {
		return false, nil
	}
	var count int64
	if err := s.db.WithContext(ctx).
		Model(&agentApprovalDecisionRow{}).
		Where("session_id = ? AND function_call_id = ?", sessionID, functionCallID).
		Count(&count).Error; err != nil {
		return false, fmt.Errorf("eventlog: DecisionStore.HasDecision: %w", err)
	}
	return count > 0, nil
}

func (s *DecisionStore) deleteSession(ctx context.Context, appName, userID, sessionID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	// Do not delete rows if another session under a different (app_name, user_id)
	// still owns the same session_id in ADK's sessions table.
	var remaining int64
	if err := s.db.WithContext(ctx).
		Table("sessions").
		Where("id = ? AND NOT (app_name = ? AND user_id = ?)", sessionID, appName, userID).
		Count(&remaining).Error; err == nil && remaining > 0 {
		return nil
	}
	if err := s.db.WithContext(ctx).
		Where("session_id = ?", sessionID).
		Delete(&agentApprovalDecisionRow{}).Error; err != nil {
		return fmt.Errorf("eventlog: delete approval decisions for session %q: %w", sessionID, err)
	}
	return nil
}

// HasDecision delegates to Handle.Decisions.HasDecision.
func (h *Handle) HasDecision(ctx context.Context, sessionID, functionCallID string) (bool, error) {
	if h == nil || h.Decisions == nil {
		return false, nil
	}
	return h.Decisions.HasDecision(ctx, sessionID, functionCallID)
}

// RecordDecision delegates to Handle.Decisions.RecordDecision.
func (h *Handle) RecordDecision(ctx context.Context, d approval.Decision) error {
	if h == nil || h.Decisions == nil {
		return nil
	}
	return h.Decisions.RecordDecision(ctx, d)
}

// ExportOptions scopes a decision export.
type ExportOptions struct {
	AppName         string
	UserID          string
	SessionID       string
	Workload        string
	Since           time.Time
	Until           time.Time
	IncludeApprover bool
	Source          string
	Tool            string
}

// ExportMeta is the provenance object on the first line of an export.
type ExportMeta struct {
	Schema     string     `json:"schema"`
	Tool       string     `json:"tool"`
	ExportedAt time.Time  `json:"exported_at"`
	Source     string     `json:"source,omitempty"`
	Session    string     `json:"session,omitempty"`
	Workload   string     `json:"workload,omitempty"`
	Since      *time.Time `json:"since,omitempty"`
	Until      *time.Time `json:"until,omitempty"`
	Redaction  string     `json:"redaction"`
	Warning    string     `json:"warning"`
	Records    int        `json:"records"`
}

type metaLine struct {
	Meta ExportMeta `json:"_meta"`
}

// SessionDecisions returns the write-gate Decision records for one session in
// the order the calls were adjudicated, merging records from the SQL
// DecisionStore and the session's event state deltas.
func (h *Handle) SessionDecisions(ctx context.Context, appName, userID, sessionID string) ([]approval.Decision, error) {
	if h == nil || h.Service == nil {
		return nil, errors.New("eventlog: SessionDecisions: handle service is required")
	}
	resolvedUser, err := h.resolveSessionUser(ctx, appName, userID, sessionID)
	if err != nil {
		return nil, err
	}
	resp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName:   appName,
		UserID:    resolvedUser,
		SessionID: sessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("eventlog: get session %q: %w", sessionID, err)
	}
	fromEvents := scanEventDecisions(resp.Session.Events())
	if h.Decisions == nil {
		return fromEvents, nil
	}
	fromSQL, err := h.Decisions.ListDecisions(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return mergeDecisions(fromEvents, fromSQL), nil
}

func scanEventDecisions(events session.Events) []approval.Decision {
	var out []approval.Decision
	for ev := range events.All() {
		if ev == nil {
			continue
		}
		delta := ev.Actions.StateDelta
		if len(delta) == 0 {
			continue
		}
		keys := make([]string, 0, len(delta))
		for k := range delta {
			if strings.HasPrefix(k, approval.DecisionStateKeyPrefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			d, err := approval.DecodeDecision(delta[k])
			if err != nil {
				continue
			}
			if d.FunctionCallID == "" {
				d.FunctionCallID = strings.TrimPrefix(k, approval.DecisionStateKeyPrefix)
			}
			out = append(out, d)
		}
	}
	return out
}

func mergeDecisions(fromEvents, fromSQL []approval.Decision) []approval.Decision {
	if len(fromSQL) == 0 {
		return fromEvents
	}
	if len(fromEvents) == 0 {
		return fromSQL
	}
	seen := make(map[string]bool, len(fromEvents))
	out := make([]approval.Decision, 0, len(fromEvents)+len(fromSQL))
	for _, d := range fromEvents {
		if d.FunctionCallID != "" {
			seen[d.FunctionCallID] = true
		}
		out = append(out, d)
	}
	for _, d := range fromSQL {
		if d.FunctionCallID != "" && seen[d.FunctionCallID] {
			continue
		}
		out = append(out, d)
	}
	slices.SortStableFunc(out, func(a, b approval.Decision) int {
		return cmp.Compare(a.DecidedAt.UnixNano(), b.DecidedAt.UnixNano())
	})
	return out
}

// ExportDecisions writes the store's write-gate Decision records to w as
// JSON Lines, preceded by one ExportMeta provenance line, and returns the
// number of decision rows written.
func (h *Handle) ExportDecisions(ctx context.Context, w io.Writer, opts ExportOptions) (int, error) {
	if h == nil || h.Service == nil {
		return 0, errors.New("eventlog: ExportDecisions: handle service is required")
	}
	decisions, err := h.collectExportDecisions(ctx, opts)
	if err != nil {
		return 0, err
	}
	rows := filterAndRedactDecisions(decisions, opts)
	if err := writeExportMeta(w, opts, len(rows)); err != nil {
		return 0, err
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for i, d := range rows {
		if err := enc.Encode(d); err != nil {
			return i, fmt.Errorf("write decision row: %w", err)
		}
	}
	return len(rows), nil
}

func (h *Handle) collectExportDecisions(ctx context.Context, opts ExportOptions) ([]approval.Decision, error) {
	if opts.SessionID != "" {
		return h.SessionDecisions(ctx, opts.AppName, opts.UserID, opts.SessionID)
	}
	resp, err := h.Service.List(ctx, &session.ListRequest{
		AppName: opts.AppName,
		UserID:  opts.UserID,
	})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	ids := make([]string, 0, len(resp.Sessions))
	users := make(map[string]string, len(resp.Sessions))
	apps := make(map[string]string, len(resp.Sessions))
	for _, s := range resp.Sessions {
		if strings.HasSuffix(s.ID(), reservedOpsSuffix) {
			continue
		}
		ids = append(ids, s.ID())
		users[s.ID()] = s.UserID()
		apps[s.ID()] = s.AppName()
	}
	sort.Strings(ids)
	var all []approval.Decision
	for _, id := range ids {
		ds, err := h.SessionDecisions(ctx, apps[id], users[id], id)
		if err != nil {
			return nil, err
		}
		all = append(all, ds...)
	}
	return all, nil
}

func filterAndRedactDecisions(decisions []approval.Decision, opts ExportOptions) []approval.Decision {
	var rows []approval.Decision
	for _, d := range decisions {
		if opts.Workload != "" && d.Workload != opts.Workload {
			continue
		}
		if !opts.Since.IsZero() && d.DecidedAt.Before(opts.Since) {
			continue
		}
		if !opts.Until.IsZero() && !d.DecidedAt.Before(opts.Until) {
			continue
		}
		if !opts.IncludeApprover {
			d = d.Redacted()
		}
		rows = append(rows, d)
	}
	return rows
}

func writeExportMeta(w io.Writer, opts ExportOptions, count int) error {
	redaction := RedactionApproverDigest
	if opts.IncludeApprover {
		redaction = RedactionNone
	}
	toolName := opts.Tool
	if toolName == "" {
		toolName = "core-agent"
	}
	meta := ExportMeta{
		Schema:     approval.DecisionSchema,
		Tool:       toolName,
		ExportedAt: time.Now().UTC(),
		Source:     opts.Source,
		Session:    opts.SessionID,
		Workload:   opts.Workload,
		Redaction:  redaction,
		Warning:    exportArgsWarning,
		Records:    count,
	}
	if !opts.Since.IsZero() {
		t := opts.Since.UTC()
		meta.Since = &t
	}
	if !opts.Until.IsZero() {
		t := opts.Until.UTC()
		meta.Until = &t
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(metaLine{Meta: meta}); err != nil {
		return fmt.Errorf("write export header: %w", err)
	}
	return nil
}

// AckEffects appends an operator acknowledgement watermark for ambiguous prior
// effects on sessionID. Dangling mutating tool calls at or before the returned
// timestamp no longer trip the outbox's ambiguous-effect refusal mode.
func (h *Handle) AckEffects(ctx context.Context, appName, userID, sessionID, by, reason string) (time.Time, error) {
	if h == nil || h.Service == nil {
		return time.Time{}, errors.New("eventlog: AckEffects: handle service is required")
	}
	if strings.HasSuffix(sessionID, reservedOpsSuffix) {
		return time.Time{}, fmt.Errorf("eventlog: AckEffects: reserved session ID %q", sessionID)
	}
	resolvedUser, err := h.resolveSessionUser(ctx, appName, userID, sessionID)
	if err != nil {
		return time.Time{}, err
	}
	resp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName:   appName,
		UserID:    resolvedUser,
		SessionID: sessionID,
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("eventlog: get session %q: %w", sessionID, err)
	}
	now := time.Now().UTC()
	author := by
	if author == "" {
		author = "operator"
	}
	ev := session.NewEvent(ctx, "effects-ack")
	ev.Author = author
	ev.Content = genai.NewContentFromText(
		fmt.Sprintf("operator acknowledged ambiguous prior effects: %s", reason),
		genai.RoleUser,
	)
	ev.Actions.StateDelta = map[string]any{
		EffectsAckTimeKey:   now.Format(time.RFC3339Nano),
		EffectsAckReasonKey: reason,
		EffectsAckByKey:     author,
	}
	if err := h.Service.AppendEvent(ctx, resp.Session, ev); err != nil {
		return time.Time{}, fmt.Errorf("eventlog: append effects-ack event to session %q: %w", sessionID, err)
	}
	return now, nil
}

// EffectsAckedAt returns the session's effects-acknowledgement watermark, if
// one was recorded. Read failures and missing markers report false.
func (h *Handle) EffectsAckedAt(ctx context.Context, appName, userID, sessionID string) (time.Time, bool) {
	if h == nil || h.Service == nil || strings.HasSuffix(sessionID, reservedOpsSuffix) {
		return time.Time{}, false
	}
	resolvedUser, err := h.resolveSessionUser(ctx, appName, userID, sessionID)
	if err != nil {
		return time.Time{}, false
	}
	resp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName:   appName,
		UserID:    resolvedUser,
		SessionID: sessionID,
	})
	if err != nil || resp == nil || resp.Session == nil {
		return time.Time{}, false
	}
	v, err := resp.Session.State().Get(EffectsAckTimeKey)
	if err != nil {
		return time.Time{}, false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// ParkedCall is one open interrupt or long-running tool park on a session.
type ParkedCall struct {
	InterruptID    string             `json:"interrupt_id"`
	ToolName       string             `json:"tool_name,omitempty"`
	Awaiting       string             `json:"awaiting,omitempty"`
	Message        string             `json:"message,omitempty"`
	ProposedTool   string             `json:"proposed_tool,omitempty"`
	ProposedArgs   map[string]any     `json:"proposed_args,omitempty"`
	Subject        string             `json:"subject,omitempty"`
	Author         string             `json:"author,omitempty"`
	RaisedAt       time.Time          `json:"raised_at"`
	LongRunning    bool               `json:"long_running,omitempty"`
	ResponseSchema *jsonschema.Schema `json:"response_schema,omitempty"`
	Payload        any                `json:"payload,omitempty"`
}

// SessionParkedCalls groups a session's open parks with its awaiting classification.
type SessionParkedCalls struct {
	SessionID string       `json:"session_id"`
	UserID    string       `json:"user_id,omitempty"`
	Awaiting  string       `json:"awaiting"`
	Pending   []ParkedCall `json:"pending"`
}

// ListParks returns the open parks for one session (when sessionID is
// non-empty) or across all sessions under appName. When sessionID is empty,
// only sessions with at least one open park are returned.
func (h *Handle) ListParks(ctx context.Context, appName, userID, sessionID string) ([]SessionParkedCalls, error) {
	if h == nil || h.Service == nil {
		return nil, errors.New("eventlog: ListParks: handle service is required")
	}
	if sessionID != "" {
		if strings.HasSuffix(sessionID, reservedOpsSuffix) {
			return nil, fmt.Errorf("eventlog: session %q not found: %w", sessionID, gorm.ErrRecordNotFound)
		}
		resolvedUser, err := h.resolveSessionUser(ctx, appName, userID, sessionID)
		if err != nil {
			return nil, err
		}
		resp, err := h.Service.Get(ctx, &session.GetRequest{
			AppName:   appName,
			UserID:    resolvedUser,
			SessionID: sessionID,
		})
		if err != nil {
			return nil, err
		}
		pending := ScanPendingParks(resp.Session.Events())
		if len(pending) == 0 {
			return []SessionParkedCalls{}, nil
		}
		return []SessionParkedCalls{{
			SessionID: sessionID,
			UserID:    resolvedUser,
			Awaiting:  classifyAwaiting(pending),
			Pending:   pending,
		}}, nil
	}

	resp, err := h.Service.List(ctx, &session.ListRequest{
		AppName: appName,
		UserID:  userID,
	})
	if err != nil {
		return nil, fmt.Errorf("eventlog: list sessions: %w", err)
	}
	out := make([]SessionParkedCalls, 0)
	for _, s := range resp.Sessions {
		if strings.HasSuffix(s.ID(), reservedOpsSuffix) {
			continue
		}
		full, err := h.Service.Get(ctx, &session.GetRequest{
			AppName:   s.AppName(),
			UserID:    s.UserID(),
			SessionID: s.ID(),
		})
		if err != nil {
			return nil, err
		}
		pending := ScanPendingParks(full.Session.Events())
		if len(pending) == 0 {
			continue
		}
		out = append(out, SessionParkedCalls{
			SessionID: s.ID(),
			UserID:    s.UserID(),
			Awaiting:  classifyAwaiting(pending),
			Pending:   pending,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SessionID < out[j].SessionID
	})
	return out, nil
}

// ScanPendingParks walks a session's event log in order and returns any
// RequestedInput or LongRunningToolIDs parks that have not been resolved by a
// later FunctionResponse.
func ScanPendingParks(events session.Events) []ParkedCall {
	if events == nil {
		return nil
	}
	var order []string
	byID := make(map[string]ParkedCall)
	for ev := range events.All() {
		if ev == nil {
			continue
		}
		recordRequestedInputPark(ev, &order, byID)
		recordLongRunningParks(ev, &order, byID)
		clearResolvedParks(ev, byID)
	}
	var pending []ParkedCall
	for _, id := range order {
		if p, ok := byID[id]; ok {
			pending = append(pending, p)
			delete(byID, id)
		}
	}
	return pending
}

func recordRequestedInputPark(ev *session.Event, order *[]string, byID map[string]ParkedCall) {
	ri := ev.RequestedInput
	if ri == nil || ri.InterruptID == "" {
		return
	}
	if _, seen := byID[ri.InterruptID]; !seen {
		*order = append(*order, ri.InterruptID)
	}
	byID[ri.InterruptID] = ParkedCall{
		InterruptID:    ri.InterruptID,
		Awaiting:       AwaitingInput,
		Message:        ri.Message,
		Author:         ev.Author,
		RaisedAt:       ev.Timestamp,
		ResponseSchema: ri.ResponseSchema,
		Payload:        ri.Payload,
	}
}

func recordLongRunningParks(ev *session.Event, order *[]string, byID map[string]ParkedCall) {
	for _, id := range ev.LongRunningToolIDs {
		if id == "" {
			continue
		}
		if _, seen := byID[id]; seen {
			continue
		}
		*order = append(*order, id)
		p := ParkedCall{
			InterruptID: id,
			Awaiting:    AwaitingInput,
			Author:      ev.Author,
			RaisedAt:    ev.Timestamp,
			LongRunning: true,
		}
		enrichLongRunningPark(ev, id, &p)
		byID[id] = p
	}
}

func enrichLongRunningPark(ev *session.Event, id string, p *ParkedCall) {
	if ev.Content == nil {
		return
	}
	for _, part := range ev.Content.Parts {
		if part == nil || part.FunctionCall == nil || part.FunctionCall.ID != id {
			continue
		}
		p.ToolName = part.FunctionCall.Name
		if m, ok := part.FunctionCall.Args["message"].(string); ok {
			p.Message = m
		}
		if part.FunctionCall.Name == toolconfirmation.FunctionCallName {
			d := approval.DescribeConfirmation(part.FunctionCall.Args)
			p.Awaiting = AwaitingApproval
			p.Message = d.Summary()
			p.ProposedTool = d.Tool
			p.ProposedArgs = d.Args
			if d.Request != nil {
				p.Subject, _ = d.Request["subject"].(string)
			}
			p.Payload = d.Request
			p.ResponseSchema = approval.VerdictSchema()
		}
	}
}

func clearResolvedParks(ev *session.Event, byID map[string]ParkedCall) {
	if ev.Content == nil {
		return
	}
	for _, part := range ev.Content.Parts {
		if part == nil || part.FunctionResponse == nil {
			continue
		}
		delete(byID, part.FunctionResponse.ID)
	}
}

func classifyAwaiting(pending []ParkedCall) string {
	out := ""
	for _, p := range pending {
		if p.ToolName == toolconfirmation.FunctionCallName || p.Awaiting == AwaitingApproval {
			return AwaitingApproval
		}
		out = AwaitingInput
	}
	return out
}

func (h *Handle) resolveSessionUser(ctx context.Context, appName, userID, sessionID string) (string, error) {
	if userID != "" {
		return userID, nil
	}
	resp, err := h.Service.List(ctx, &session.ListRequest{AppName: appName})
	if err != nil {
		return "", fmt.Errorf("eventlog: list sessions: %w", err)
	}
	for _, s := range resp.Sessions {
		if strings.HasSuffix(s.ID(), reservedOpsSuffix) {
			continue
		}
		if s.ID() == sessionID {
			return s.UserID(), nil
		}
	}
	return "", fmt.Errorf("eventlog: session %q not found: %w", sessionID, gorm.ErrRecordNotFound)
}
