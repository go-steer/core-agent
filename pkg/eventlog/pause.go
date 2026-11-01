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

// Originally derived from go-steer/mast@d239f89 (internal/transcript/pause.go).

package eventlog

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
	"gorm.io/gorm"
)

const (
	pauseGateKey       = "mast_pause_gate"
	pauseIntrKeyPrefix = "mast_pause_intr:"
	tokenPrefix        = "mrt_"

	// DefaultTokenTTL is the default resume-token lifetime.
	DefaultTokenTTL = 7 * 24 * time.Hour

	// PlaneGate and PlaneInterrupt identify the two pause planes recorded in PauseRecord.Plane.
	PlaneGate      = "gate"
	PlaneInterrupt = "interrupt"
)

// Reason is the pause-reason taxonomy.
type Reason string

const (
	ReasonBudgetExhaustion  Reason = "budget_exhaustion"
	ReasonWatchdogAnomaly   Reason = "watchdog_anomaly"
	ReasonCostCoolDown      Reason = "cost_cool_down"
	ReasonMaintenanceWindow Reason = "maintenance_window"
	ReasonRateLimitBackoff  Reason = "rate_limit_backoff"
	ReasonAmbiguity         Reason = "ambiguity"
	ReasonOperator          Reason = "operator"
	ReasonA2ATaskPending    Reason = "a2a_task_pending"
	ReasonOther             Reason = "other"
)

var validReasons = map[Reason]bool{
	ReasonBudgetExhaustion:  true,
	ReasonWatchdogAnomaly:   true,
	ReasonCostCoolDown:      true,
	ReasonMaintenanceWindow: true,
	ReasonRateLimitBackoff:  true,
	ReasonAmbiguity:         true,
	ReasonOperator:          true,
	ReasonA2ATaskPending:    true,
	ReasonOther:             true,
}

// ValidReasons lists the accepted Reason values.
func ValidReasons() []string {
	return []string{
		string(ReasonBudgetExhaustion),
		string(ReasonWatchdogAnomaly),
		string(ReasonCostCoolDown),
		string(ReasonMaintenanceWindow),
		string(ReasonRateLimitBackoff),
		string(ReasonAmbiguity),
		string(ReasonOperator),
		string(ReasonA2ATaskPending),
		string(ReasonOther),
	}
}

// Pause/token sentinel errors.
var (
	ErrTokenNotFound  = fmt.Errorf("eventlog: resume token not found: %w", gorm.ErrRecordNotFound)
	ErrTokenExpired   = errors.New("eventlog: resume token expired")
	ErrAlreadyResumed = errors.New("eventlog: pause already resumed")
)

// PauseSpec is the caller-facing pause request.
type PauseSpec struct {
	Reason    Reason
	Message   string
	Metadata  map[string]any
	ResumeAt  time.Time
	Interrupt bool
	TokenTTL  time.Duration
}

func (spec *PauseSpec) validate() error {
	if !validReasons[spec.Reason] {
		return fmt.Errorf("eventlog: unknown pause reason %q (want one of %s)", spec.Reason, strings.Join(ValidReasons(), ", "))
	}
	if spec.TokenTTL < 0 {
		return errors.New("eventlog: token TTL must not be negative")
	}
	if spec.TokenTTL > DefaultTokenTTL {
		return fmt.Errorf("eventlog: token TTL %s exceeds the %s default", spec.TokenTTL, DefaultTokenTTL)
	}
	return nil
}

func (spec *PauseSpec) ttl() time.Duration {
	if spec.TokenTTL > 0 {
		return spec.TokenTTL
	}
	return DefaultTokenTTL
}

// PauseRecord is the durable pause record and resume token stored on a
// session's companion ops row.
type PauseRecord struct {
	Token       string         `json:"token"`
	Plane       string         `json:"plane"`
	InterruptID string         `json:"interrupt_id,omitempty"`
	App         string         `json:"app"`
	User        string         `json:"user"`
	SessionID   string         `json:"session_id"`
	Reason      Reason         `json:"reason"`
	Message     string         `json:"message,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	MintedAt    time.Time      `json:"minted_at"`
	ExpiresAt   time.Time      `json:"expires_at"`
	ResumeAt    time.Time      `json:"resume_at,omitzero"`
	ConsumedAt  time.Time      `json:"consumed_at,omitzero"`
	ConsumedBy  string         `json:"consumed_by,omitempty"`
}

// Active reports whether the record still gates or awaits a resume.
func (r *PauseRecord) Active() bool { return r != nil && r.ConsumedAt.IsZero() }

// Expired reports whether the record's token has passed its TTL.
func (r *PauseRecord) Expired(now time.Time) bool { return now.After(r.ExpiresAt) }

// PauseHandle is returned when a pause record is minted or updated.
type PauseHandle struct {
	Token     string    `json:"token"`
	SessionID string    `json:"session_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func mintToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("eventlog: mint resume token: %w", err)
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// PauseGate writes or updates a session's plane-B gate pause.
func (h *Handle) PauseGate(ctx context.Context, appName, userID, sessionID string, spec PauseSpec) (PauseHandle, bool, error) {
	if h == nil || h.Service == nil {
		return PauseHandle{}, false, errors.New("eventlog: PauseGate: handle service is required")
	}
	if strings.HasSuffix(sessionID, reservedOpsSuffix) {
		return PauseHandle{}, false, fmt.Errorf("eventlog: PauseGate: reserved session ID %q", sessionID)
	}
	if err := spec.validate(); err != nil {
		return PauseHandle{}, false, err
	}
	resolvedUser, err := h.resolveSessionUser(ctx, appName, userID, sessionID)
	if err != nil {
		return PauseHandle{}, false, err
	}
	if _, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: appName, UserID: resolvedUser, SessionID: sessionID,
	}); err != nil {
		return PauseHandle{}, false, fmt.Errorf("eventlog: get session %q: %w", sessionID, err)
	}

	now := time.Now().UTC()
	rec := h.opsPauseRecords(ctx, appName, resolvedUser, sessionID)[pauseGateKey]
	action := "gate pause"
	created := !rec.Active()
	if rec.Active() {
		action = "gate pause updated"
		rec.Reason, rec.Message, rec.Metadata, rec.ResumeAt = spec.Reason, spec.Message, spec.Metadata, spec.ResumeAt
	} else {
		token, err := mintToken()
		if err != nil {
			return PauseHandle{}, false, err
		}
		rec = &PauseRecord{
			Token: token, Plane: PlaneGate,
			App: appName, User: resolvedUser, SessionID: sessionID,
			Reason: spec.Reason, Message: spec.Message, Metadata: spec.Metadata,
			MintedAt: now, ExpiresAt: now.Add(spec.ttl()), ResumeAt: spec.ResumeAt,
		}
	}
	if err := h.writePauseRecord(ctx, appName, resolvedUser, sessionID, pauseGateKey, rec,
		fmt.Sprintf("%s (%s): %s", action, rec.Reason, rec.Message)); err != nil {
		return PauseHandle{}, false, err
	}
	return PauseHandle{Token: rec.Token, SessionID: sessionID, ExpiresAt: rec.ExpiresAt}, created, nil
}

// PauseInterrupt mints the plane-A pause record for a parked interrupt.
func (h *Handle) PauseInterrupt(ctx context.Context, appName, userID, sessionID, interruptID string, spec PauseSpec) (PauseHandle, error) {
	if h == nil || h.Service == nil {
		return PauseHandle{}, errors.New("eventlog: PauseInterrupt: handle service is required")
	}
	if strings.HasSuffix(sessionID, reservedOpsSuffix) {
		return PauseHandle{}, fmt.Errorf("eventlog: PauseInterrupt: reserved session ID %q", sessionID)
	}
	if interruptID == "" {
		return PauseHandle{}, errors.New("eventlog: PauseInterrupt: interrupt ID must be non-empty")
	}
	if err := spec.validate(); err != nil {
		return PauseHandle{}, err
	}
	resolvedUser, err := h.resolveSessionUser(ctx, appName, userID, sessionID)
	if err != nil {
		return PauseHandle{}, err
	}
	token, err := mintToken()
	if err != nil {
		return PauseHandle{}, err
	}
	now := time.Now().UTC()
	rec := &PauseRecord{
		Token: token, Plane: PlaneInterrupt, InterruptID: interruptID,
		App: appName, User: resolvedUser, SessionID: sessionID,
		Reason: spec.Reason, Message: spec.Message, Metadata: spec.Metadata,
		MintedAt: now, ExpiresAt: now.Add(spec.ttl()), ResumeAt: spec.ResumeAt,
	}
	if err := h.writePauseRecord(ctx, appName, resolvedUser, sessionID, pauseIntrKeyPrefix+interruptID, rec,
		fmt.Sprintf("interrupt pause (%s) for %s: %s", rec.Reason, interruptID, rec.Message)); err != nil {
		return PauseHandle{}, err
	}
	return PauseHandle{Token: token, SessionID: sessionID, ExpiresAt: rec.ExpiresAt}, nil
}

// GatePause returns the session's active gate-pause record, if any.
func (h *Handle) GatePause(ctx context.Context, appName, userID, sessionID string) *PauseRecord {
	if h == nil || h.Service == nil || strings.HasSuffix(sessionID, reservedOpsSuffix) {
		return nil
	}
	resolvedUser, err := h.resolveSessionUser(ctx, appName, userID, sessionID)
	if err != nil {
		return nil
	}
	rec := h.opsPauseRecords(ctx, appName, resolvedUser, sessionID)[pauseGateKey]
	if !rec.Active() {
		return nil
	}
	return rec
}

// PauseRecords returns all pause records for one session (active and consumed).
func (h *Handle) PauseRecords(ctx context.Context, appName, userID, sessionID string) (map[string]*PauseRecord, error) {
	if h == nil || h.Service == nil {
		return nil, errors.New("eventlog: PauseRecords: handle service is required")
	}
	if strings.HasSuffix(sessionID, reservedOpsSuffix) {
		return nil, fmt.Errorf("eventlog: PauseRecords: reserved session ID %q: %w", sessionID, gorm.ErrRecordNotFound)
	}
	resolvedUser, err := h.resolveSessionUser(ctx, appName, userID, sessionID)
	if err != nil {
		return nil, err
	}
	return h.opsPauseRecords(ctx, appName, resolvedUser, sessionID), nil
}

// ScanPauses walks every session's ops row under appName and returns all
// active pause records.
func (h *Handle) ScanPauses(ctx context.Context, appName string) ([]*PauseRecord, error) {
	if h == nil || h.Service == nil {
		return nil, errors.New("eventlog: ScanPauses: handle service is required")
	}
	resp, err := h.Service.List(ctx, &session.ListRequest{AppName: appName})
	if err != nil {
		return nil, fmt.Errorf("eventlog: scan pauses: list sessions: %w", err)
	}
	var out []*PauseRecord
	for _, sess := range resp.Sessions {
		if !strings.HasSuffix(sess.ID(), reservedOpsSuffix) {
			continue
		}
		full, gerr := h.Service.Get(ctx, &session.GetRequest{
			AppName: appName, UserID: sess.UserID(), SessionID: sess.ID(),
		})
		if gerr != nil || full == nil || full.Session == nil {
			continue
		}
		primary := strings.TrimSuffix(sess.ID(), reservedOpsSuffix)
		for _, rec := range parsePauseRecords(full.Session, primary) {
			if rec.Active() {
				out = append(out, rec)
			}
		}
	}
	return out, nil
}

// FindToken resolves a resume token to its pause record (active or consumed).
func (h *Handle) FindToken(ctx context.Context, appName, token string) (*PauseRecord, error) {
	if h == nil || h.Service == nil {
		return nil, errors.New("eventlog: FindToken: handle service is required")
	}
	if token == "" || !strings.HasPrefix(token, tokenPrefix) {
		return nil, fmt.Errorf("token %q: %w", token, ErrTokenNotFound)
	}
	resp, err := h.Service.List(ctx, &session.ListRequest{AppName: appName})
	if err != nil {
		return nil, fmt.Errorf("eventlog: find token: list sessions: %w", err)
	}
	for _, sess := range resp.Sessions {
		if !strings.HasSuffix(sess.ID(), reservedOpsSuffix) {
			continue
		}
		full, gerr := h.Service.Get(ctx, &session.GetRequest{
			AppName: appName, UserID: sess.UserID(), SessionID: sess.ID(),
		})
		if gerr != nil || full == nil || full.Session == nil {
			continue
		}
		primary := strings.TrimSuffix(sess.ID(), reservedOpsSuffix)
		for _, rec := range parsePauseRecords(full.Session, primary) {
			if rec.Token == token {
				return rec, nil
			}
		}
	}
	return nil, ErrTokenNotFound
}

// ConsumeToken marks the token's pause record consumed, enforcing token TTL.
func (h *Handle) ConsumeToken(ctx context.Context, appName, token, by string) (*PauseRecord, error) {
	return h.consumeToken(ctx, appName, token, by, true)
}

// ConsumeScheduled consumes a token on behalf of the timed-pause scheduler
// without enforcing token TTL.
func (h *Handle) ConsumeScheduled(ctx context.Context, appName, token, by string) (*PauseRecord, error) {
	return h.consumeToken(ctx, appName, token, by, false)
}

func (h *Handle) consumeToken(ctx context.Context, appName, token, by string, enforceExpiry bool) (*PauseRecord, error) {
	rec, err := h.FindToken(ctx, appName, token)
	if err != nil {
		return nil, err
	}
	if appName != "" && rec.App != "" && rec.App != appName {
		return nil, ErrTokenNotFound
	}
	if !rec.ConsumedAt.IsZero() {
		return rec, fmt.Errorf("token %s (session %q, consumed %s by %s): %w",
			token, rec.SessionID, rec.ConsumedAt.Format(time.RFC3339), rec.ConsumedBy, ErrAlreadyResumed)
	}
	if enforceExpiry && rec.Expired(time.Now().UTC()) {
		return rec, fmt.Errorf("token %s (session %q, expired %s): %w",
			token, rec.SessionID, rec.ExpiresAt.Format(time.RFC3339), ErrTokenExpired)
	}
	rec.ConsumedAt = time.Now().UTC()
	rec.ConsumedBy = by
	key := pauseGateKey
	if rec.Plane == PlaneInterrupt {
		key = pauseIntrKeyPrefix + rec.InterruptID
	}
	if err := h.writePauseRecord(ctx, rec.App, rec.User, rec.SessionID, key, rec,
		fmt.Sprintf("pause resumed (%s) by %s", rec.Reason, by)); err != nil {
		return nil, err
	}
	return rec, nil
}

// ExtendToken moves an active token's expiry to now + ttl.
func (h *Handle) ExtendToken(ctx context.Context, appName, token string, ttl time.Duration) (*PauseRecord, error) {
	if ttl <= 0 {
		return nil, errors.New("eventlog: extend token: TTL must be positive")
	}
	rec, err := h.FindToken(ctx, appName, token)
	if err != nil {
		return nil, err
	}
	if appName != "" && rec.App != "" && rec.App != appName {
		return nil, ErrTokenNotFound
	}
	if !rec.ConsumedAt.IsZero() {
		return rec, fmt.Errorf("token %s: %w", token, ErrAlreadyResumed)
	}
	rec.ExpiresAt = time.Now().UTC().Add(ttl)
	key := pauseGateKey
	if rec.Plane == PlaneInterrupt {
		key = pauseIntrKeyPrefix + rec.InterruptID
	}
	if err := h.writePauseRecord(ctx, rec.App, rec.User, rec.SessionID, key, rec,
		fmt.Sprintf("resume token extended to %s (operator action)", rec.ExpiresAt.Format(time.RFC3339))); err != nil {
		return nil, err
	}
	return rec, nil
}

func (h *Handle) writePauseRecord(ctx context.Context, appName, userID, sessionID, key string, rec *PauseRecord, text string) error {
	blob, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("eventlog: encode pause record: %w", err)
	}
	opsID := sessionID + reservedOpsSuffix
	sess, err := h.getOrCreateOpsSession(ctx, appName, userID, opsID)
	if err != nil {
		return err
	}
	ev := session.NewEvent(ctx, "pause")
	ev.Author = "operator"
	ev.Content = genai.NewContentFromText(text, genai.RoleUser)
	ev.Actions.StateDelta = map[string]any{key: string(blob)}
	if err := h.Service.AppendEvent(ctx, sess, ev); err != nil {
		return fmt.Errorf("eventlog: append pause event for session %q: %w", sessionID, err)
	}
	return nil
}

func (h *Handle) opsPauseRecords(ctx context.Context, appName, userID, sessionID string) map[string]*PauseRecord {
	resp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: appName, UserID: userID, SessionID: sessionID + reservedOpsSuffix,
	})
	if err != nil || resp == nil || resp.Session == nil {
		return nil
	}
	return parsePauseRecords(resp.Session, sessionID)
}

func parsePauseRecords(sess session.Session, sessionID string) map[string]*PauseRecord {
	out := make(map[string]*PauseRecord)
	for k, v := range sess.State().All() {
		if k != pauseGateKey && !strings.HasPrefix(k, pauseIntrKeyPrefix) {
			continue
		}
		blob, ok := v.(string)
		if !ok || blob == "" {
			continue
		}
		var rec PauseRecord
		if err := json.Unmarshal([]byte(blob), &rec); err != nil {
			continue
		}
		if rec.SessionID == "" {
			rec.SessionID = sessionID
		}
		out[k] = &rec
	}
	return out
}
