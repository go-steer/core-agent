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
	"time"

	"gorm.io/gorm"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// SessionACLStore persists per-session ACL state across daemon
// restarts. Backs Phase 1 of the session-resume-on-restart design
// (docs/session-resume-design.md): RegisterOwned writes a row at
// session-creation time; the future Phase 2 SessionResumer reads it
// to reconstruct evicted sessions on Lookup miss.
//
// Implementations are expected to be safe for concurrent use.
// The on-disk shape is operator-visible (SQLite database file),
// so the column layout MUST stay stable across releases.
type SessionACLStore interface {
	// Put upserts the ACL row for (app, user, sid). Called by
	// RegisterOwned at session-creation time. Idempotent — re-Put
	// of the same triple updates LastTouchedAt and any changed
	// ACL fields.
	Put(ctx context.Context, row SessionACLRow) error

	// Get returns the persisted ACL row for the triple. Returns
	// ErrSessionACLNotFound when no row exists (typical case for
	// sessions that were registered before this store was wired in,
	// or for fresh deployments with no prior sessions).
	Get(ctx context.Context, app, user, sid string) (SessionACLRow, error)

	// FindByAppSID returns the first persisted row matching the
	// (app, sid) pair, scanning across users. Used by the resumer:
	// SessionRegistry.Lookup is keyed by (app, sid) (no userID;
	// userID is per-process today, see registry doc), so the
	// resumer must locate the matching row without knowing user.
	// Returns ErrSessionACLNotFound when no row exists.
	//
	// In practice SessionIDs are unique across users, so the
	// "first match" behavior is unambiguous. The deterministic
	// scan-order isn't specified — callers that need stable order
	// across multi-user-same-sid rows (shouldn't happen) sort
	// post-fetch.
	FindByAppSID(ctx context.Context, app, sid string) (SessionACLRow, error)

	// Delete removes the ACL row for the triple. Idempotent — no
	// error when the row doesn't exist. Used by future
	// DELETE /sessions hard-delete; NOT called by the eviction
	// sweep (eviction is in-memory only).
	Delete(ctx context.Context, app, user, sid string) error

	// Touch updates the row's LastTouchedAt without rewriting the
	// rest of the row. Called by the registry on every Lookup hit
	// and every event broadcast so the eviction sweep has a
	// recent timestamp to compare against.
	Touch(ctx context.Context, app, user, sid string, when time.Time) error

	// ListByOwner returns every ACL row where the owner matches.
	// Used by future operator UX ("show me all sessions I own").
	// Order is unspecified; callers that need stable order should
	// sort by their preferred field (typically LastTouchedAt desc).
	ListByOwner(ctx context.Context, owner string) ([]SessionACLRow, error)

	// ListVisibleTo returns every ACL row the caller may
	// SessionRead per the Authorize matrix: Owner, Viewer,
	// Contributor, or Admin-sees-all. Powers the persisted half
	// of GET /sessions (the in-memory half comes from
	// Registry.List(); the handler unions + dedupes the two).
	//
	// An Admin caller gets every row in the table. A zero-identity
	// caller gets nothing (matches Authorize's empty-identity
	// deny-by-default).
	ListVisibleTo(ctx context.Context, caller auth.Caller) ([]SessionACLRow, error)
}

// SessionACLRow is the public shape returned by SessionACLStore.
// The wire-format JSON fields on ViewersJSON / ContributorsJSON are
// only an implementation detail of the GORM-backed store; callers
// access the strongly-typed Viewers / Contributors slices directly
// via the ACL() helper.
type SessionACLRow struct {
	AppName       string
	UserID        string
	SessionID     string
	Owner         string
	Viewers       []string
	Contributors  []string
	CreatedAt     time.Time
	LastTouchedAt time.Time
}

// ACL returns the auth.SessionACL view of the row — what the
// Authorize matrix consumes when deciding whether a caller may
// access this session.
func (r SessionACLRow) ACL() auth.SessionACL {
	return auth.SessionACL{
		Owner:        r.Owner,
		Viewers:      append([]string(nil), r.Viewers...),
		Contributors: append([]string(nil), r.Contributors...),
	}
}

// ErrSessionACLNotFound is returned by SessionACLStore.Get when no
// row exists for the triple. Distinct from a SQL error so callers
// can branch cleanly (404 vs 500 in the resumer).
var ErrSessionACLNotFound = errors.New("attach: session ACL not found")

// ===== GORM-backed implementation =====

// sessionACLRow is the GORM model for the agent_session_acl table.
// Internal — exported callers go through SessionACLStore and the
// public SessionACLRow value type. The composite primary key
// (AppName, UserID, SessionID) matches the SessionRegistry's
// triple shape so resume + ACL stay in lockstep.
//
// ViewersJSON / ContributorsJSON are JSON-encoded []string. Keeps
// the table primary-key shape simple and queryable without a join
// table — slice sizes in practice are single-digit identities, so
// deserialize cost is trivial. Schema can grow without invalidating
// existing rows (new fields default to zero values).
type sessionACLRow struct {
	AppName          string    `gorm:"not null;primaryKey"`
	UserID           string    `gorm:"not null;primaryKey"`
	SessionID        string    `gorm:"not null;primaryKey"`
	Owner            string    `gorm:"not null;index:idx_session_acl_owner"`
	ViewersJSON      string    `gorm:"type:text"`
	ContributorsJSON string    `gorm:"type:text"`
	CreatedAt        time.Time `gorm:"not null"`
	LastTouchedAt    time.Time `gorm:"not null;index:idx_session_acl_last_touched"`
}

func (sessionACLRow) TableName() string { return "agent_session_acl" }

// gormSessionACLStore implements SessionACLStore against the
// eventlog's GORM connection. The same *gorm.DB is shared with
// the eventlog's overlay table — no separate connection pool.
type gormSessionACLStore struct {
	db *gorm.DB
}

// NewSessionACLStore constructs a SessionACLStore backed by the
// supplied GORM connection (typically the one returned by
// eventlog.Open, exposed via Handle.SessionACL). AutoMigrates the
// agent_session_acl table; safe to call against an existing
// database — AutoMigrate is idempotent for additive schema changes.
//
// Returns a constructed store on success. On AutoMigrate failure,
// returns the underlying GORM error wrapped with package context.
func NewSessionACLStore(ctx context.Context, db *gorm.DB) (SessionACLStore, error) {
	if db == nil {
		return nil, errors.New("attach: NewSessionACLStore: db is required")
	}
	if err := db.WithContext(ctx).AutoMigrate(&sessionACLRow{}); err != nil {
		return nil, fmt.Errorf("attach: AutoMigrate agent_session_acl: %w", err)
	}
	return &gormSessionACLStore{db: db}, nil
}

func (s *gormSessionACLStore) Put(ctx context.Context, row SessionACLRow) error {
	if row.AppName == "" || row.SessionID == "" {
		return fmt.Errorf("attach: SessionACLStore.Put: AppName and SessionID are required (got app=%q sid=%q)", row.AppName, row.SessionID)
	}
	if row.Owner == "" {
		return errors.New("attach: SessionACLStore.Put: Owner is required (rows without owners aren't persisted — use the in-memory Register path)")
	}
	now := time.Now()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	if row.LastTouchedAt.IsZero() {
		row.LastTouchedAt = now
	}
	viewers, err := encodeStringSlice(row.Viewers)
	if err != nil {
		return fmt.Errorf("attach: SessionACLStore.Put: encode viewers: %w", err)
	}
	contributors, err := encodeStringSlice(row.Contributors)
	if err != nil {
		return fmt.Errorf("attach: SessionACLStore.Put: encode contributors: %w", err)
	}
	internal := sessionACLRow{
		AppName:          row.AppName,
		UserID:           row.UserID,
		SessionID:        row.SessionID,
		Owner:            row.Owner,
		ViewersJSON:      viewers,
		ContributorsJSON: contributors,
		CreatedAt:        row.CreatedAt,
		LastTouchedAt:    row.LastTouchedAt,
	}
	// GORM's Save upserts when the primary key is set — handles
	// both first-insert and re-Put-of-same-triple cleanly.
	if err := s.db.WithContext(ctx).Save(&internal).Error; err != nil {
		return fmt.Errorf("attach: SessionACLStore.Put: %w", err)
	}
	return nil
}

func (s *gormSessionACLStore) Get(ctx context.Context, app, user, sid string) (SessionACLRow, error) {
	var internal sessionACLRow
	err := s.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ?", app, user, sid).
		First(&internal).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return SessionACLRow{}, ErrSessionACLNotFound
	}
	if err != nil {
		return SessionACLRow{}, fmt.Errorf("attach: SessionACLStore.Get: %w", err)
	}
	return rowFromInternal(internal), nil
}

func (s *gormSessionACLStore) FindByAppSID(ctx context.Context, app, sid string) (SessionACLRow, error) {
	if app == "" || sid == "" {
		return SessionACLRow{}, fmt.Errorf("attach: SessionACLStore.FindByAppSID: app and sid are required (got app=%q sid=%q)", app, sid)
	}
	var internal sessionACLRow
	err := s.db.WithContext(ctx).
		Where("app_name = ? AND session_id = ?", app, sid).
		First(&internal).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return SessionACLRow{}, ErrSessionACLNotFound
	}
	if err != nil {
		return SessionACLRow{}, fmt.Errorf("attach: SessionACLStore.FindByAppSID: %w", err)
	}
	return rowFromInternal(internal), nil
}

func (s *gormSessionACLStore) Delete(ctx context.Context, app, user, sid string) error {
	err := s.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ?", app, user, sid).
		Delete(&sessionACLRow{}).Error
	if err != nil {
		return fmt.Errorf("attach: SessionACLStore.Delete: %w", err)
	}
	return nil
}

func (s *gormSessionACLStore) Touch(ctx context.Context, app, user, sid string, when time.Time) error {
	if when.IsZero() {
		when = time.Now()
	}
	res := s.db.WithContext(ctx).
		Model(&sessionACLRow{}).
		Where("app_name = ? AND user_id = ? AND session_id = ?", app, user, sid).
		Update("last_touched_at", when)
	if res.Error != nil {
		return fmt.Errorf("attach: SessionACLStore.Touch: %w", res.Error)
	}
	// No row affected ≠ error: the session might exist in the
	// in-memory registry but not (yet) in the ACL table — typical
	// for legacy / pre-multi-session sessions. Silently skip.
	return nil
}

func (s *gormSessionACLStore) ListByOwner(ctx context.Context, owner string) ([]SessionACLRow, error) {
	var rows []sessionACLRow
	err := s.db.WithContext(ctx).
		Where("owner = ?", owner).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("attach: SessionACLStore.ListByOwner: %w", err)
	}
	out := make([]SessionACLRow, len(rows))
	for i := range rows {
		out[i] = rowFromInternal(rows[i])
	}
	return out, nil
}

func (s *gormSessionACLStore) ListVisibleTo(ctx context.Context, caller auth.Caller) ([]SessionACLRow, error) {
	// Empty-identity caller sees nothing — matches Authorize's
	// deny-by-default for zero-value Callers.
	if caller.Identity == "" && !caller.Admin {
		return nil, nil
	}
	// Admin sees every row. The full-table scan is acceptable —
	// administrative listing isn't on a hot path, and realistic
	// scale (thousands of sessions, not millions) makes the cost
	// sub-millisecond on any backend.
	if caller.Admin {
		var rows []sessionACLRow
		err := s.db.WithContext(ctx).Find(&rows).Error
		if err != nil {
			return nil, fmt.Errorf("attach: SessionACLStore.ListVisibleTo (admin): %w", err)
		}
		out := make([]SessionACLRow, len(rows))
		for i := range rows {
			out[i] = rowFromInternal(rows[i])
		}
		return out, nil
	}
	// Non-admin: rows where caller is Owner, OR appears in
	// ViewersJSON, OR appears in ContributorsJSON. Owner check is
	// indexed; the JSON membership check is a substring match.
	//
	// The substring match is technically a false-positive risk —
	// an identity that's a prefix of another identity's
	// JSON-quoted value could match. We mitigate by wrapping the
	// identity in quote characters (`"alice@example.com"`) which
	// is how it appears verbatim in the JSON array. False
	// positives are still theoretically possible across pathological
	// identity strings; second-stage filter in Go ensures
	// correctness regardless.
	quoted := `"` + escapeJSONString(caller.Identity) + `"`
	var rows []sessionACLRow
	err := s.db.WithContext(ctx).
		Where("owner = ? OR viewers_json LIKE ? OR contributors_json LIKE ?",
			caller.Identity, "%"+quoted+"%", "%"+quoted+"%").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("attach: SessionACLStore.ListVisibleTo: %w", err)
	}
	// Second-stage filter: SQL LIKE is a candidate set, not a
	// guaranteed match. Re-check each row with the real
	// authorization predicate.
	out := make([]SessionACLRow, 0, len(rows))
	for _, r := range rows {
		row := rowFromInternal(r)
		if auth.Authorize(caller, auth.ActionSessionRead, row.ACL()) {
			out = append(out, row)
		}
	}
	return out, nil
}

// ===== helpers =====

func rowFromInternal(r sessionACLRow) SessionACLRow {
	return SessionACLRow{
		AppName:       r.AppName,
		UserID:        r.UserID,
		SessionID:     r.SessionID,
		Owner:         r.Owner,
		Viewers:       decodeStringSlice(r.ViewersJSON),
		Contributors:  decodeStringSlice(r.ContributorsJSON),
		CreatedAt:     r.CreatedAt,
		LastTouchedAt: r.LastTouchedAt,
	}
}

func encodeStringSlice(xs []string) (string, error) {
	if len(xs) == 0 {
		// Store "" rather than "[]" so the empty case takes zero
		// row space and round-trips through decodeStringSlice as
		// nil — matches the Go-side zero-value convention.
		return "", nil
	}
	b, err := json.Marshal(xs)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeStringSlice(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		// Defensive: malformed JSON returns nil rather than
		// panicking. The store is best-effort by design (lossy
		// if the column is corrupted, never stops a read).
		return nil
	}
	return out
}

// escapeJSONString escapes the small set of characters that JSON
// encoding would change in a string. Used to construct the quoted
// LIKE pattern in ListVisibleTo so the substring match maps to the
// identity's actual on-disk byte sequence. Conservative — escapes
// quotes and backslashes; anything more exotic in an identity
// would have failed identity validation upstream anyway.
func escapeJSONString(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' {
			out = append(out, '\\')
		}
		out = append(out, c)
	}
	return string(out)
}
