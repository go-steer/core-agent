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
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/adk/v2/session"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	guardrailTripAuthor   = "agent/guardrail-trip"
	guardrailResetAuthor  = "attach/guardrail-reset"
	guardrailKindWatchdog = "watchdog"
	guardrailKindCost     = "cost_ceiling"
	guardrailMetaBudget   = "budget_added_usd"
)

// GuardrailState is the durable row in agent_guardrail_state tracking
// whether a session's watchdog or cost ceiling has tripped and any
// cumulative operator-granted budget extensions (#1360).
type GuardrailState struct {
	AppName            string    `gorm:"primaryKey;not null"`
	UserID             string    `gorm:"primaryKey;not null"`
	SessionID          string    `gorm:"primaryKey;not null"`
	WatchdogTripped    bool      `gorm:"not null;default:false"`
	WatchdogReason     string    `gorm:"type:text"`
	CostTripped        bool      `gorm:"not null;default:false"`
	CostReason         string    `gorm:"type:text"`
	BudgetExtensionUSD float64   `gorm:"not null;default:0"`
	LastEventID        string    `gorm:"type:text"`
	UpdatedAt          time.Time `gorm:"autoUpdateTime"`
}

// TableName pins the GORM table name across database drivers.
func (GuardrailState) TableName() string { return "agent_guardrail_state" }

// GuardrailStore persists guardrail trip, reset, and budget-extension
// state synchronously to the database so a tripped session remains
// tripped across restarts and RestoreGuardrails reads current state by
// primary key without hydrating the session history (#1360, #1438).
// Sessions created through eventlog.Handle.Service seed a zero state
// row on creation; pre-upgrade sessions without a state row perform a
// one-time indexed check on agent_eventlog and backfill or seed the row
// so subsequent reads are O(1) primary-key lookups.
type GuardrailStore struct {
	db *gorm.DB
}

// NewGuardrailStore auto-migrates agent_guardrail_state on db and
// returns a ready GuardrailStore.
func NewGuardrailStore(ctx context.Context, db *gorm.DB) (*GuardrailStore, error) {
	if db == nil {
		return nil, errors.New("eventlog: GuardrailStore: db is nil")
	}
	if err := db.WithContext(ctx).AutoMigrate(&GuardrailState{}); err != nil {
		return nil, fmt.Errorf("eventlog: AutoMigrate agent_guardrail_state: %w", err)
	}
	return &GuardrailStore{db: db}, nil
}

// TripWatchdog records a durable watchdog trip for the session.
func (s *GuardrailStore) TripWatchdog(ctx context.Context, appName, userID, sessionID, reason string) error {
	if s == nil || s.db == nil {
		return errors.New("eventlog: GuardrailStore is nil")
	}
	return tripWatchdogOnDB(s.db.WithContext(ctx), appName, userID, sessionID, reason, "")
}

func tripWatchdogOnDB(db *gorm.DB, appName, userID, sessionID, reason, eventID string) error {
	now := time.Now().UTC()
	row := GuardrailState{
		AppName:         appName,
		UserID:          userID,
		SessionID:       sessionID,
		WatchdogTripped: true,
		WatchdogReason:  reason,
		LastEventID:     eventID,
		UpdatedAt:       now,
	}
	updates := map[string]any{
		"watchdog_tripped": true,
		"watchdog_reason":  reason,
		"updated_at":       now,
	}
	if eventID != "" {
		updates["last_event_id"] = eventID
	}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "app_name"}, {Name: "user_id"}, {Name: "session_id"}},
		DoUpdates: clause.Assignments(updates),
	}).Create(&row).Error; err != nil {
		return fmt.Errorf("eventlog: trip watchdog: %w", err)
	}
	return nil
}

// TripCost records a durable cost-ceiling trip for the session.
func (s *GuardrailStore) TripCost(ctx context.Context, appName, userID, sessionID, reason string) error {
	if s == nil || s.db == nil {
		return errors.New("eventlog: GuardrailStore is nil")
	}
	return tripCostOnDB(s.db.WithContext(ctx), appName, userID, sessionID, reason, "")
}

func tripCostOnDB(db *gorm.DB, appName, userID, sessionID, reason, eventID string) error {
	now := time.Now().UTC()
	row := GuardrailState{
		AppName:     appName,
		UserID:      userID,
		SessionID:   sessionID,
		CostTripped: true,
		CostReason:  reason,
		LastEventID: eventID,
		UpdatedAt:   now,
	}
	updates := map[string]any{
		"cost_tripped": true,
		"cost_reason":  reason,
		"updated_at":   now,
	}
	if eventID != "" {
		updates["last_event_id"] = eventID
	}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "app_name"}, {Name: "user_id"}, {Name: "session_id"}},
		DoUpdates: clause.Assignments(updates),
	}).Create(&row).Error; err != nil {
		return fmt.Errorf("eventlog: trip cost: %w", err)
	}
	return nil
}

// ClearWatchdog clears a durable watchdog trip for the session.
func (s *GuardrailStore) ClearWatchdog(ctx context.Context, appName, userID, sessionID string) error {
	if s == nil || s.db == nil {
		return errors.New("eventlog: GuardrailStore is nil")
	}
	return clearWatchdogOnDB(s.db.WithContext(ctx), appName, userID, sessionID, "")
}

func clearWatchdogOnDB(db *gorm.DB, appName, userID, sessionID, eventID string) error {
	now := time.Now().UTC()
	row := GuardrailState{
		AppName:     appName,
		UserID:      userID,
		SessionID:   sessionID,
		LastEventID: eventID,
		UpdatedAt:   now,
	}
	updates := map[string]any{
		"watchdog_tripped": false,
		"watchdog_reason":  "",
		"updated_at":       now,
	}
	if eventID != "" {
		updates["last_event_id"] = eventID
	}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "app_name"}, {Name: "user_id"}, {Name: "session_id"}},
		DoUpdates: clause.Assignments(updates),
	}).Create(&row).Error; err != nil {
		return fmt.Errorf("eventlog: clear watchdog: %w", err)
	}
	return nil
}

// ClearCost clears a durable cost-ceiling trip for the session.
func (s *GuardrailStore) ClearCost(ctx context.Context, appName, userID, sessionID string) error {
	if s == nil || s.db == nil {
		return errors.New("eventlog: GuardrailStore is nil")
	}
	return clearCostOnDB(s.db.WithContext(ctx), appName, userID, sessionID, "")
}

func clearCostOnDB(db *gorm.DB, appName, userID, sessionID, eventID string) error {
	now := time.Now().UTC()
	row := GuardrailState{
		AppName:     appName,
		UserID:      userID,
		SessionID:   sessionID,
		LastEventID: eventID,
		UpdatedAt:   now,
	}
	updates := map[string]any{
		"cost_tripped": false,
		"cost_reason":  "",
		"updated_at":   now,
	}
	if eventID != "" {
		updates["last_event_id"] = eventID
	}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "app_name"}, {Name: "user_id"}, {Name: "session_id"}},
		DoUpdates: clause.Assignments(updates),
	}).Create(&row).Error; err != nil {
		return fmt.Errorf("eventlog: clear cost: %w", err)
	}
	return nil
}

// AddBudgetExtension adds a positive USD budget extension to the
// session's durable guardrail state. Non-positive addUSD is a no-op.
func (s *GuardrailStore) AddBudgetExtension(ctx context.Context, appName, userID, sessionID string, addUSD float64) error {
	if s == nil || s.db == nil {
		return errors.New("eventlog: GuardrailStore is nil")
	}
	if addUSD <= 0 {
		return nil
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return addBudgetExtensionOnDB(tx, appName, userID, sessionID, addUSD, "")
	})
}

func addBudgetExtensionOnDB(db *gorm.DB, appName, userID, sessionID string, addUSD float64, eventID string) error {
	if addUSD <= 0 {
		return nil
	}
	var cur GuardrailState
	err := db.Where("app_name = ? AND user_id = ? AND session_id = ?", appName, userID, sessionID).
		First(&cur).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("eventlog: load guardrail state for budget extension: %w", err)
	}
	if eventID != "" && cur.LastEventID == eventID {
		return nil
	}
	next := cur.BudgetExtensionUSD + addUSD
	now := time.Now().UTC()
	row := GuardrailState{
		AppName:            appName,
		UserID:             userID,
		SessionID:          sessionID,
		BudgetExtensionUSD: next,
		LastEventID:        eventID,
		UpdatedAt:          now,
	}
	updates := map[string]any{
		"budget_extension_usd": next,
		"updated_at":           now,
	}
	if eventID != "" {
		updates["last_event_id"] = eventID
	}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "app_name"}, {Name: "user_id"}, {Name: "session_id"}},
		DoUpdates: clause.Assignments(updates),
	}).Create(&row).Error; err != nil {
		return fmt.Errorf("eventlog: add guardrail budget extension: %w", err)
	}
	return nil
}

// SaveState writes a full GuardrailState snapshot for the session. Used
// by RestoreGuardrails when backfilling a pre-upgrade session whose
// guardrail events predated agent_guardrail_state (#1360).
func (s *GuardrailStore) SaveState(ctx context.Context, st GuardrailState) error {
	if s == nil || s.db == nil {
		return errors.New("eventlog: GuardrailStore is nil")
	}
	now := time.Now().UTC()
	st.UpdatedAt = now
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "app_name"}, {Name: "user_id"}, {Name: "session_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"watchdog_tripped":     st.WatchdogTripped,
			"watchdog_reason":      st.WatchdogReason,
			"cost_tripped":         st.CostTripped,
			"cost_reason":          st.CostReason,
			"budget_extension_usd": st.BudgetExtensionUSD,
			"updated_at":           now,
		}),
	}).Create(&st).Error; err != nil {
		return fmt.Errorf("eventlog: save guardrail state: %w", err)
	}
	return nil
}

// Load returns the current GuardrailState for the session in O(1) by
// primary key, or a zero GuardrailState when no row has been recorded.
func (s *GuardrailStore) Load(ctx context.Context, appName, userID, sessionID string) (GuardrailState, error) {
	if s == nil || s.db == nil {
		return GuardrailState{}, errors.New("eventlog: GuardrailStore is nil")
	}
	var row GuardrailState
	err := s.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ?", appName, userID, sessionID).
		First(&row).Error
	if err == nil {
		return row, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return GuardrailState{AppName: appName, UserID: userID, SessionID: sessionID}, nil
	}
	return GuardrailState{}, fmt.Errorf("eventlog: load guardrail state: %w", err)
}

// LoadWithLegacyCheck returns the session's GuardrailState from
// agent_guardrail_state in O(1) by primary key when a state or sentinel
// row is present. When no row exists yet (e.g. a pre-upgrade session),
// it probes agent_eventlog via idx_agent_eventlog_session_author in
// O(log N) to report whether pre-upgrade guardrail events need a
// one-time fold and backfill; when none exist and the session is
// present in storage, it seeds a zero sentinel row so future calls hit
// the primary key directly (#1360, #1438).
func (s *GuardrailStore) LoadWithLegacyCheck(ctx context.Context, appName, userID, sessionID string) (GuardrailState, bool, error) {
	if s == nil || s.db == nil {
		return GuardrailState{}, false, errors.New("eventlog: GuardrailStore is nil")
	}
	var row GuardrailState
	err := s.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ?", appName, userID, sessionID).
		First(&row).Error
	if err == nil {
		return row, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return GuardrailState{}, false, fmt.Errorf("eventlog: load guardrail state: %w", err)
	}
	var seq int64
	if qErr := s.db.WithContext(ctx).
		Model(&agentEventRow{}).
		Select("seq").
		Where("app_name = ? AND user_id = ? AND session_id = ? AND author IN ?",
			appName, userID, sessionID, []string{guardrailTripAuthor, guardrailResetAuthor}).
		Limit(1).
		Scan(&seq).Error; qErr != nil {
		return GuardrailState{}, false, fmt.Errorf("eventlog: check legacy guardrail rows: %w", qErr)
	}
	if seq == 0 {
		_ = s.seedZeroSentinelIfSessionExists(ctx, appName, userID, sessionID)
	}
	return GuardrailState{}, seq > 0, nil
}

// ensureState inserts a zero sentinel GuardrailState row when none exists yet,
// leaving any existing row untouched (ON CONFLICT DO NOTHING) so concurrent
// trip or budget writes can never be overwritten (#1438).
func (s *GuardrailStore) ensureState(ctx context.Context, appName, userID, sessionID string) error {
	if s == nil || s.db == nil || appName == "" || userID == "" || sessionID == "" {
		return nil
	}
	return ensureStateOnDB(s.db.WithContext(ctx), appName, userID, sessionID)
}

func ensureStateOnDB(db *gorm.DB, appName, userID, sessionID string) error {
	row := GuardrailState{
		AppName:   appName,
		UserID:    userID,
		SessionID: sessionID,
		UpdatedAt: time.Now().UTC(),
	}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "app_name"}, {Name: "user_id"}, {Name: "session_id"}},
		DoNothing: true,
	}).Create(&row).Error; err != nil {
		return fmt.Errorf("eventlog: ensure guardrail state: %w", err)
	}
	return nil
}

// seedZeroSentinelIfSessionExists checks whether the session is persisted in
// ADK's sessions table or has rows in agent_eventlog inside a transaction and
// seeds the zero sentinel row only when present (#1438), avoiding orphan rows
// when callers query arbitrary nonexistent or concurrently deleted session IDs.
func (s *GuardrailStore) seedZeroSentinelIfSessionExists(ctx context.Context, appName, userID, sessionID string) error {
	if s == nil || s.db == nil || appName == "" || userID == "" || sessionID == "" {
		return nil
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if !sessionExistsOnDB(tx, appName, userID, sessionID) {
			return nil
		}
		return ensureStateOnDB(tx, appName, userID, sessionID)
	})
}

func sessionExistsOnDB(db *gorm.DB, appName, userID, sessionID string) bool {
	var exists int
	if err := db.Session(&gorm.Session{NewDB: true}).
		Table("sessions").
		Select("1").
		Where("app_name = ? AND user_id = ? AND id = ?", appName, userID, sessionID).
		Limit(1).
		Scan(&exists).Error; err == nil && exists == 1 {
		return true
	}
	if err := db.Session(&gorm.Session{NewDB: true}).
		Model(&agentEventRow{}).
		Select("1").
		Where("app_name = ? AND user_id = ? AND session_id = ?", appName, userID, sessionID).
		Limit(1).
		Scan(&exists).Error; err == nil && exists == 1 {
		return true
	}
	return false
}

// deleteSession removes any guardrail state row for the deleted session.
func (s *GuardrailStore) deleteSession(ctx context.Context, appName, userID, sessionID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := s.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ?", appName, userID, sessionID).
		Delete(&GuardrailState{}).Error; err != nil {
		return fmt.Errorf("eventlog: delete guardrail state for session %q: %w", sessionID, err)
	}
	return nil
}

// applyEvent synchronizes a guardrail-trip or guardrail-reset session
// event into agent_guardrail_state on the fallback (non-overlayInTx)
// path.
func (s *GuardrailStore) applyEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	if s == nil || s.db == nil || sess == nil || ev == nil || ev.Partial {
		return nil
	}
	return applyGuardrailEventOnDB(s.db.WithContext(ctx), sess, ev)
}

// applyGuardrailEventOnDB applies a guardrail-trip or guardrail-reset
// event against db (which may be ADK's in-flight transaction in
// overlayAfterCreate or the overlay pool on the fallback path).
func applyGuardrailEventOnDB(db *gorm.DB, sess session.Session, ev *session.Event) error {
	if db == nil || sess == nil || ev == nil || ev.Partial {
		return nil
	}
	appName, userID, sessionID := sess.AppName(), sess.UserID(), sess.ID()
	switch ev.Author {
	case guardrailTripAuthor:
		g, _ := ev.CustomMetadata["guardrail"].(string)
		reason, _ := ev.CustomMetadata["reason"].(string)
		switch g {
		case guardrailKindWatchdog:
			return tripWatchdogOnDB(db.Session(&gorm.Session{NewDB: true}), appName, userID, sessionID, reason, "")
		case guardrailKindCost:
			return tripCostOnDB(db.Session(&gorm.Session{NewDB: true}), appName, userID, sessionID, reason, "")
		}
	case guardrailResetAuthor:
		for _, g := range guardrailResetTargets(ev.CustomMetadata["reset"]) {
			switch g {
			case guardrailKindWatchdog:
				if err := clearWatchdogOnDB(db.Session(&gorm.Session{NewDB: true}), appName, userID, sessionID, ""); err != nil {
					return err
				}
			case guardrailKindCost:
				if err := clearCostOnDB(db.Session(&gorm.Session{NewDB: true}), appName, userID, sessionID, ""); err != nil {
					return err
				}
			}
		}
		if add := guardrailFloatMeta(ev.CustomMetadata[guardrailMetaBudget]); add > 0 {
			return addBudgetExtensionOnDB(db.Session(&gorm.Session{NewDB: true}), appName, userID, sessionID, add, ev.ID)
		}
	}
	return nil
}

func guardrailResetTargets(v any) []string {
	switch xs := v.(type) {
	case []string:
		return xs
	case []any:
		out := make([]string, 0, len(xs))
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func guardrailFloatMeta(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		return 0
	}
}
