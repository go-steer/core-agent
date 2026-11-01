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

// Durable budget spend ledger (#1359).
//
// A session cost ceiling that resets on process restart bounds spend
// per process rather than per session. SpendStore maintains an
// append-only ledger (agent_budget_spend) on the same database
// connection as the eventlog overlay and guardrail store: one row per
// priced model call at the rate in force when the call was made,
// foldable in O(1) via an indexed GROUP BY author aggregate.

package eventlog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SpendRecord is one priced model call recorded in agent_budget_spend.
//
// Author is the event's author or model attribution verbatim. CostUSD
// is what the call cost when it was made, priced by the catalog in
// force at the time. Unpriced marks a call the catalog could not price,
// so a restored figure preserves the same unpriced-turn signal a live
// tracker carries.
//
// The remaining fields (#1433) carry the per-call shape a usage.Tracker
// needs to be restored from the ledger without re-pricing: the model
// and backend the call was billed on, the token buckets, whether it
// was a side call, and the event it was committed on.
type SpendRecord struct {
	Author   string
	Tokens   int64
	CostUSD  float64
	Unpriced bool
	// At is the wall-clock time of the call. Zero means "now".
	At time.Time

	// EventID is the ledger's idempotency key within a session: a
	// second Append with the same (app, user, session, EventID) is a
	// no-op. Empty makes no idempotency claim — the row is stored
	// with a NULL key and never deduplicated.
	EventID string
	// Model and Backend are what the call was priced on
	// (usage.Turn.Model / usage.Turn.Backend).
	Model   string
	Backend string
	// Side marks a call recorded with usage.Tracker.AppendSideUsage.
	Side bool
	// Token buckets, as on usage.Turn. Tokens above is their sum for
	// the Fold aggregate; these are what a restored Turn is built from.
	InputTokens              int64
	CachedInputTokens        int64
	CacheCreationInputTokens int64
	OutputTokens             int64
	ThoughtsTokens           int64
	ToolUseTokens            int64
}

// SpendTotals is one accumulator's folded form.
type SpendTotals struct {
	Tokens  int64
	CostUSD float64
	Calls   int
}

// SpendState is what folding a session's ledger produces: the session's
// total, and the same rows broken out by author so per-subagent or
// per-model ceilings come back too.
//
// Session includes every call ByAuthor also accounts for — the two are
// projections of the same rows, computed in the same query.
type SpendState struct {
	Session  SpendTotals
	ByAuthor map[string]SpendTotals
	// Unpriced counts calls the catalog could not price.
	Unpriced int
}

// budgetSpendRow is the GORM model for the agent_budget_spend table.
// Append-only: one row per priced model call, ordered by Seq.
//
// EventID is nullable on purpose (#1433): rows written before the
// column existed, and rows whose caller named no event, carry NULL,
// which every supported engine (SQLite, Postgres, MySQL) treats as
// distinct under the unique index — so the index can be created over
// a populated table and a key-less row is never mistaken for a
// duplicate. The idempotency guarantee holds for rows that do name
// their event, which is every row the pkg/compose writer produces.
type budgetSpendRow struct {
	Seq       int64  `gorm:"primaryKey;autoIncrement"`
	AppName   string `gorm:"not null;index:idx_budget_spend_session,priority:1;uniqueIndex:idx_budget_spend_event,priority:1"`
	UserID    string `gorm:"not null;index:idx_budget_spend_session,priority:2;uniqueIndex:idx_budget_spend_event,priority:2"`
	SessionID string `gorm:"not null;index:idx_budget_spend_session,priority:3;uniqueIndex:idx_budget_spend_event,priority:3"`
	Author    string
	Tokens    int64
	CostUSD   float64
	Unpriced  bool
	At        time.Time `gorm:"not null"`

	// Added by #1433. AutoMigrate adds the columns to a table created
	// by #1430; see TestSpendStoreMigratesAPreExistingTable.
	// size:191 keeps MySQL InnoDB utf8mb4 index keys bounded instead of
	// falling back to an unindexable LONGTEXT column.
	EventID                  *string `gorm:"size:191;uniqueIndex:idx_budget_spend_event,priority:4"`
	Model                    string
	Backend                  string
	Side                     bool
	InputTokens              int64
	CachedInputTokens        int64
	CacheCreationInputTokens int64
	OutputTokens             int64
	ThoughtsTokens           int64
	ToolUseTokens            int64
}

// TableName pins the table name independent of GORM's pluralization,
// matching agent_eventlog, agent_run_lock, and agent_guardrail_state.
func (budgetSpendRow) TableName() string { return "agent_budget_spend" }

// SpendStore persists and folds a session's per-call spend (#1359).
//
// A nil *SpendStore is usable and does nothing: Append is a no-op and
// Fold reports empty state — the "no durable store configured" case at
// the call sites, same shape as GuardrailStore.
//
// Safe for concurrent use.
type SpendStore struct {
	db *gorm.DB
}

// NewSpendStore constructs a store over an existing GORM connection —
// typically Handle.DB, the connection the guardrail store and
// pkg/attach's session ACL store already share. AutoMigrates
// agent_budget_spend; safe against an existing database.
func NewSpendStore(ctx context.Context, db *gorm.DB) (*SpendStore, error) {
	if db == nil {
		return nil, errors.New("eventlog: NewSpendStore: db is required")
	}
	if err := db.WithContext(ctx).AutoMigrate(&budgetSpendRow{}); err != nil {
		return nil, fmt.Errorf("eventlog: AutoMigrate agent_budget_spend: %w", err)
	}
	return &SpendStore{db: db}, nil
}

func newBudgetSpendRow(app, user, sid string, rec SpendRecord) budgetSpendRow {
	at := rec.At
	if at.IsZero() {
		at = time.Now()
	}
	row := budgetSpendRow{
		AppName:                  app,
		UserID:                   user,
		SessionID:                sid,
		Author:                   rec.Author,
		Tokens:                   rec.Tokens,
		CostUSD:                  rec.CostUSD,
		Unpriced:                 rec.Unpriced,
		At:                       at,
		Model:                    rec.Model,
		Backend:                  rec.Backend,
		Side:                     rec.Side,
		InputTokens:              rec.InputTokens,
		CachedInputTokens:        rec.CachedInputTokens,
		CacheCreationInputTokens: rec.CacheCreationInputTokens,
		OutputTokens:             rec.OutputTokens,
		ThoughtsTokens:           rec.ThoughtsTokens,
		ToolUseTokens:            rec.ToolUseTokens,
	}
	if rec.EventID != "" {
		id := rec.EventID
		row.EventID = &id
	}
	return row
}

// Append writes one call's spend.
//
// Idempotent on rec.EventID (#1433): a row already present for
// (app, user, sid, EventID) is left alone and no error is returned —
// `ON CONFLICT DO NOTHING` on SQLite and Postgres, which GORM's MySQL
// dialector renders as `ON DUPLICATE KEY UPDATE` of the key to itself.
// An empty EventID is stored as NULL and is never deduplicated.
func (s *SpendStore) Append(ctx context.Context, app, user, sid string, rec SpendRecord) error {
	if s == nil || s.db == nil {
		return nil
	}
	if app == "" || sid == "" {
		return fmt.Errorf("eventlog: SpendStore.Append: app and session are required (got app=%q sid=%q)", app, sid)
	}
	row := newBudgetSpendRow(app, user, sid, rec)
	if err := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&row).Error; err != nil {
		return fmt.Errorf("eventlog: SpendStore.Append: %w", err)
	}
	return nil
}

// AppendBatch writes multiple spend rows atomically in a single
// transaction, with the same EventID idempotency semantics as Append
// (#1433). Used when seeding a pre-ledger session's historical turns so
// a transient mid-replay error never commits a partial ledger.
func (s *SpendStore) AppendBatch(ctx context.Context, app, user, sid string, recs []SpendRecord) error {
	if s == nil || s.db == nil || len(recs) == 0 {
		return nil
	}
	if app == "" || sid == "" {
		return fmt.Errorf("eventlog: SpendStore.AppendBatch: app and session are required (got app=%q sid=%q)", app, sid)
	}
	rows := make([]budgetSpendRow, len(recs))
	for i, rec := range recs {
		rows[i] = newBudgetSpendRow(app, user, sid, rec)
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i := range rows {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows[i]).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("eventlog: SpendStore.AppendBatch: %w", err)
	}
	return nil
}

// spendAgg is one author's aggregate, as the fold query returns it.
type spendAgg struct {
	Author   string
	Tokens   int64
	CostUSD  float64
	Calls    int
	Unpriced int
}

// Fold reconstructs the session's spend from its ledger.
//
// One GROUP BY author, summed in the database rather than in Go: the
// ledger is one row per model call, and a long-running session is the
// case this exists for.
//
// A session with no rows folds to the zero value and no error.
func (s *SpendStore) Fold(ctx context.Context, app, user, sid string) (SpendState, error) {
	if s == nil || s.db == nil {
		return SpendState{}, nil
	}
	tx := s.db.WithContext(ctx).
		Model(&budgetSpendRow{}).
		Where("app_name = ? AND user_id = ? AND session_id = ?", app, user, sid)
	return foldSpendQuery(tx)
}

// FoldTree reconstructs the spend across a root session and every
// derived "<parent>:sub:..." sub-session in its tree, matching the
// scoping of WithSessionTree (#1359).
func (s *SpendStore) FoldTree(ctx context.Context, app, user, parentSID string) (SpendState, error) {
	if s == nil || s.db == nil {
		return SpendState{}, nil
	}
	tx := s.db.WithContext(ctx).
		Model(&budgetSpendRow{}).
		Where("app_name = ? AND user_id = ?", app, user).
		Where("session_id = ? OR session_id LIKE ? ESCAPE '"+branchPrefixEscape+"'",
			parentSID, escapeLike(parentSID)+":sub:%")
	return foldSpendQuery(tx)
}

func foldSpendQuery(tx *gorm.DB) (SpendState, error) {
	var st SpendState
	var aggs []spendAgg
	err := tx.
		Select("author AS author, "+
			"COALESCE(SUM(tokens), 0) AS tokens, "+
			"COALESCE(SUM(cost_usd), 0) AS cost_usd, "+
			"COUNT(*) AS calls, "+
			"COALESCE(SUM(CASE WHEN unpriced = ? THEN 1 ELSE 0 END), 0) AS unpriced", true).
		Group("author").
		Scan(&aggs).Error
	if err != nil {
		return SpendState{}, fmt.Errorf("eventlog: SpendStore.Fold: %w", err)
	}
	if len(aggs) == 0 {
		return st, nil
	}
	st.ByAuthor = make(map[string]SpendTotals, len(aggs))
	for _, a := range aggs {
		st.Session.Tokens += a.Tokens
		st.Session.CostUSD += a.CostUSD
		st.Session.Calls += a.Calls
		st.Unpriced += a.Unpriced
		st.ByAuthor[a.Author] = SpendTotals{Tokens: a.Tokens, CostUSD: a.CostUSD, Calls: a.Calls}
	}
	return st, nil
}

// History returns the session's spend rows oldest-first — the per-call
// cost trail behind a folded total.
func (s *SpendStore) History(ctx context.Context, app, user, sid string) ([]SpendRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	return historyQuery(s.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ?", app, user, sid))
}

// HistoryTree is History over a root session and every derived
// "<parent>:sub:..." sub-session in its tree, oldest-first across the
// whole tree — the rows a resumed session's usage.Tracker is restored
// from (#1433). Empty, with no error, for a session with no rows.
func (s *SpendStore) HistoryTree(ctx context.Context, app, user, parentSID string) ([]SpendRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	return historyQuery(s.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ?", app, user).
		Where("session_id = ? OR session_id LIKE ? ESCAPE '"+branchPrefixEscape+"'",
			parentSID, escapeLike(parentSID)+":sub:%"))
}

func historyQuery(tx *gorm.DB) ([]SpendRecord, error) {
	var rows []budgetSpendRow
	if err := tx.Order("seq ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("eventlog: SpendStore.History: %w", err)
	}
	out := make([]SpendRecord, 0, len(rows))
	for _, r := range rows {
		rec := SpendRecord{
			Author:                   r.Author,
			Tokens:                   r.Tokens,
			CostUSD:                  r.CostUSD,
			Unpriced:                 r.Unpriced,
			At:                       r.At,
			Model:                    r.Model,
			Backend:                  r.Backend,
			Side:                     r.Side,
			InputTokens:              r.InputTokens,
			CachedInputTokens:        r.CachedInputTokens,
			CacheCreationInputTokens: r.CacheCreationInputTokens,
			OutputTokens:             r.OutputTokens,
			ThoughtsTokens:           r.ThoughtsTokens,
			ToolUseTokens:            r.ToolUseTokens,
		}
		if r.EventID != nil {
			rec.EventID = *r.EventID
		}
		out = append(out, rec)
	}
	return out, nil
}

// deleteSession removes any spend rows for the deleted session.
func (s *SpendStore) deleteSession(ctx context.Context, appName, userID, sessionID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := s.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ?", appName, userID, sessionID).
		Delete(&budgetSpendRow{}).Error; err != nil {
		return fmt.Errorf("eventlog: delete spend rows for session %q: %w", sessionID, err)
	}
	return nil
}
