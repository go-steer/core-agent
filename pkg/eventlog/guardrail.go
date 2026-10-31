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
// tripped across restarts and RestoreGuardrails can read current state
// in O(1) without hydrating the full session history (#1360).
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

// Load returns the current GuardrailState for the session, or a zero
// GuardrailState when no row has been recorded.
func (s *GuardrailStore) Load(ctx context.Context, appName, userID, sessionID string) (GuardrailState, error) {
	st, _, err := s.LoadWithLegacyCheck(ctx, appName, userID, sessionID)
	return st, err
}

// LoadWithLegacyCheck returns the session's GuardrailState from
// agent_guardrail_state. When no row exists in agent_guardrail_state,
// it performs an O(1) indexed check on agent_eventlog to report whether
// the session has pre-upgrade guardrail events that need a one-time
// fold and backfill (#1360).
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
	return GuardrailState{}, seq > 0, nil
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
