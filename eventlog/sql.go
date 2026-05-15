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
	"iter"
	"sync/atomic"
	"time"

	"google.golang.org/adk/session"
	adkdatabase "google.golang.org/adk/session/database"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// agentEventRow is the overlay table that gives every event a
// monotonic seq alongside ADK's events table. event_id is a logical
// foreign key to events.id (we do not declare a constraint to avoid
// coupling our migration to ADK's schema evolution).
type agentEventRow struct {
	Seq          int64  `gorm:"primaryKey;autoIncrement"`
	AppName      string `gorm:"not null;index:idx_agent_eventlog_session,priority:1"`
	UserID       string `gorm:"not null;index:idx_agent_eventlog_session,priority:2"`
	SessionID    string `gorm:"not null;index:idx_agent_eventlog_session,priority:3"`
	EventID      string `gorm:"not null;uniqueIndex:idx_agent_eventlog_event"`
	Branch       string `gorm:"index:idx_agent_eventlog_branch"`
	Author       string `gorm:"index:idx_agent_eventlog_author"`
	Timestamp    time.Time
	InvocationID string
}

// TableName pins the table name independent of GORM's pluralization
// rules so cross-driver behavior is predictable.
func (agentEventRow) TableName() string { return "agent_eventlog" }

// Open constructs a Handle backed by the supplied GORM dialector.
// Pass any standard dialector (sqlite.Open, postgres.Open, mysql.Open).
//
// Open does several things:
//
//   - Constructs ADK's database.SessionService against the dialector
//     and runs its AutoMigrate so the events / sessions / state
//     tables exist.
//   - Opens a second GORM connection for our overlay table and
//     AutoMigrates agent_eventlog.
//   - For SQLite (detected via the dialector's Name()), enables WAL
//     journal mode so concurrent readers can run alongside the
//     writer. Disable with WithSkipWAL.
//   - Wraps the ADK service so AppendEvent writes to both layers.
func Open(ctx context.Context, dialector gorm.Dialector, opts ...Option) (*Handle, error) {
	o := defaultOpenOpts()
	for _, opt := range opts {
		opt(&o)
	}

	// 1) ADK's session service does the heavy schema lifting (events,
	// sessions, app/user state).
	adkSvc, err := adkdatabase.NewSessionService(dialector, defaultGormOpts(o)...)
	if err != nil {
		return nil, fmt.Errorf("eventlog: open ADK session service: %w", err)
	}
	if err := adkdatabase.AutoMigrate(adkSvc); err != nil {
		return nil, fmt.Errorf("eventlog: ADK AutoMigrate: %w", err)
	}

	// 2) Our overlay connection. We open a fresh dialector instance
	// rather than trying to share ADK's connection — GORM's API
	// doesn't expose a *gorm.DB from session/database, and we'd
	// rather not depend on reflection. SQLite handles concurrent
	// connections cleanly (especially in WAL); other drivers
	// likewise tolerate multiple connections to the same DSN.
	gormCfg := o.gormConfig
	if gormCfg == nil {
		gormCfg = &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
		}
	}
	db, err := gorm.Open(dialector, gormCfg)
	if err != nil {
		return nil, fmt.Errorf("eventlog: open overlay db: %w", err)
	}

	// 3) WAL for SQLite. Best-effort: a failure here is logged via
	// the gorm logger but doesn't abort Open — the database is still
	// usable, just with the default journal mode (slower concurrent
	// reads).
	if !o.skipWAL && isSQLite(dialector) {
		if err := db.WithContext(ctx).Exec("PRAGMA journal_mode=WAL").Error; err != nil {
			// Don't fail Open; some SQLite distributions (notably
			// :memory:) reject WAL.
			_ = err
		}
	}

	// 4) AutoMigrate the overlay table.
	if err := db.WithContext(ctx).AutoMigrate(&agentEventRow{}); err != nil {
		return nil, fmt.Errorf("eventlog: AutoMigrate overlay: %w", err)
	}

	stream := &gormStream{
		db:            db,
		adkSvc:        adkSvc,
		watchInterval: o.watchInterval,
	}
	svc := &service{inner: adkSvc, stream: stream}
	return &Handle{Stream: stream, Service: svc, db: db}, nil
}

// defaultGormOpts returns the gorm.Option list passed to ADK's
// NewSessionService. We mirror our own logger setting so ADK doesn't
// dump SQL to stderr in tests; if the caller supplied a gormConfig
// via WithGORMConfig we honor its logger choice instead.
func defaultGormOpts(o openOpts) []gorm.Option {
	if o.gormConfig != nil && o.gormConfig.Logger != nil {
		return []gorm.Option{&gorm.Config{Logger: o.gormConfig.Logger}}
	}
	return []gorm.Option{&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}}
}

// isSQLite recognizes glebarez/sqlite (dialector.Name() == "sqlite")
// and the gorm.io/driver/sqlite (cgo) variant. Used to scope the WAL
// PRAGMA so we don't try to send it to Postgres/MySQL.
func isSQLite(d gorm.Dialector) bool {
	if d == nil {
		return false
	}
	return d.Name() == "sqlite" || d.Name() == "sqlite3"
}

// gormStream implements Stream backed by the agent_eventlog table.
type gormStream struct {
	db            *gorm.DB
	adkSvc        session.Service
	watchInterval time.Duration

	closed atomic.Bool
}

// Append writes the overlay row for ev. The caller (typically our
// session.Service wrapper) is responsible for first writing the
// underlying event via ADK's AppendEvent so the event row exists for
// our overlay row to reference.
//
// Returns the assigned seq number from the autoincrement primary key.
func (s *gormStream) Append(ctx context.Context, sess session.Session, ev *session.Event) (int64, error) {
	if s.closed.Load() {
		return 0, ErrClosed
	}
	if sess == nil {
		return 0, errors.New("eventlog: Append: session is required")
	}
	if ev == nil {
		return 0, errors.New("eventlog: Append: event is required")
	}
	row := &agentEventRow{
		AppName:      sess.AppName(),
		UserID:       sess.UserID(),
		SessionID:    sess.ID(),
		EventID:      ev.ID,
		Branch:       ev.Branch,
		Author:       ev.Author,
		Timestamp:    ev.Timestamp,
		InvocationID: ev.InvocationID,
	}
	if row.Timestamp.IsZero() {
		row.Timestamp = time.Now()
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		return 0, fmt.Errorf("eventlog: insert overlay row: %w", err)
	}
	return row.Seq, nil
}

// Since returns events with seq > fromSeq, in seq order, bounded by
// current end-of-log.
func (s *gormStream) Since(ctx context.Context, fromSeq int64, opts ...QueryOption) iter.Seq2[Entry, error] {
	q := queryOpts{}
	for _, o := range opts {
		o(&q)
	}
	return func(yield func(Entry, error) bool) {
		if s.closed.Load() {
			yield(Entry{}, ErrClosed)
			return
		}
		s.iterateOnce(ctx, fromSeq, q, yield)
	}
}

// Watch returns events with seq > fromSeq, in seq order, blocking for
// new events until ctx is cancelled. Implementation polls the table
// at watchInterval; reset to a per-session sleep loop the moment
// caught up.
func (s *gormStream) Watch(ctx context.Context, fromSeq int64, opts ...QueryOption) iter.Seq2[Entry, error] {
	q := queryOpts{}
	for _, o := range opts {
		o(&q)
	}
	return func(yield func(Entry, error) bool) {
		cursor := fromSeq
		for {
			if s.closed.Load() {
				yield(Entry{}, ErrClosed)
				return
			}
			if err := ctx.Err(); err != nil {
				return
			}
			advanced := false
			ok := s.iterateOnceFunc(ctx, cursor, q, func(e Entry, err error) bool {
				if err != nil {
					return yield(e, err)
				}
				if e.Seq > cursor {
					cursor = e.Seq
					advanced = true
				}
				return yield(e, nil)
			})
			if !ok {
				return
			}
			if advanced {
				// Drain again immediately — fast path for bursts.
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.watchInterval):
			}
		}
	}
}

// iterateOnce yields all rows currently visible (seq > fromSeq) one
// at a time. Used by Since (returns when caught up) and indirectly by
// Watch (loops on top).
func (s *gormStream) iterateOnce(ctx context.Context, fromSeq int64, q queryOpts, yield func(Entry, error) bool) {
	s.iterateOnceFunc(ctx, fromSeq, q, yield)
}

// iterateOnceFunc returns false if the consumer signaled stop via
// yield. Splitting this out from iterateOnce lets Watch reuse the
// same query pipeline with its own yield wrapper that updates the
// cursor.
func (s *gormStream) iterateOnceFunc(ctx context.Context, fromSeq int64, q queryOpts, yield func(Entry, error) bool) bool {
	rows, err := s.queryRows(ctx, fromSeq, q)
	if err != nil {
		return yield(Entry{}, err)
	}
	for _, r := range rows {
		ev, err := s.loadEvent(ctx, r)
		if err != nil {
			if !yield(Entry{Seq: r.Seq}, err) {
				return false
			}
			continue
		}
		if !yield(Entry{Seq: r.Seq, Event: ev}, nil) {
			return false
		}
	}
	return true
}

// queryRows runs the SELECT against agent_eventlog with all filters
// applied. Returns rows in seq order.
func (s *gormStream) queryRows(ctx context.Context, fromSeq int64, q queryOpts) ([]agentEventRow, error) {
	tx := s.db.WithContext(ctx).Model(&agentEventRow{}).Where("seq > ?", fromSeq)
	// WithSessionTree wins over ForSession when both are set —
	// the tree query already implies the (app, user) pair.
	if q.treeParentID != "" {
		tx = tx.Where("app_name = ? AND user_id = ?", q.treeAppName, q.treeUserID).
			Where("session_id = ? OR session_id LIKE ?", q.treeParentID, q.treeParentID+":sub:%")
	} else {
		if q.appName != "" {
			tx = tx.Where("app_name = ?", q.appName)
		}
		if q.userID != "" {
			tx = tx.Where("user_id = ?", q.userID)
		}
		if q.sessionID != "" {
			tx = tx.Where("session_id = ?", q.sessionID)
		}
	}
	if q.branchPrefix != "" {
		// Match exact prefix or prefix followed by separator. ADK
		// uses '.' for branch separators (per its docstring:
		// agent_1.agent_2.agent_3); accept either join character so
		// callers passing "parent" match "parent.child" as well as
		// "parent/child".
		tx = tx.Where(
			"branch = ? OR branch LIKE ? OR branch LIKE ?",
			q.branchPrefix,
			q.branchPrefix+".%",
			q.branchPrefix+"/%",
		)
	}
	if q.author != "" {
		tx = tx.Where("author = ?", q.author)
	}
	if q.authorSuffix != "" {
		tx = tx.Where("author LIKE ?", "%"+q.authorSuffix)
	}
	tx = tx.Order("seq ASC")
	if q.limit > 0 {
		tx = tx.Limit(q.limit)
	}
	var rows []agentEventRow
	if err := tx.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("eventlog: query overlay rows: %w", err)
	}
	return rows, nil
}

// loadEvent hydrates a session.Event for a row by re-fetching it from
// ADK's events table via the session.Service. We deliberately go
// through the session.Service interface rather than reaching into
// ADK's schema directly so we stay decoupled from ADK's row layout.
func (s *gormStream) loadEvent(ctx context.Context, r agentEventRow) (*session.Event, error) {
	resp, err := s.adkSvc.Get(ctx, &session.GetRequest{
		AppName:   r.AppName,
		UserID:    r.UserID,
		SessionID: r.SessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("eventlog: load session %q: %w", r.SessionID, err)
	}
	if resp == nil || resp.Session == nil {
		return nil, fmt.Errorf("eventlog: session %q not found", r.SessionID)
	}
	for ev := range resp.Session.Events().All() {
		if ev != nil && ev.ID == r.EventID {
			return ev, nil
		}
	}
	return nil, fmt.Errorf("eventlog: event %q not found in session %q", r.EventID, r.SessionID)
}

// Close idempotently shuts down the stream. The underlying *gorm.DB
// connection is owned by the Handle, not the Stream — Handle.Close
// releases the connection.
func (s *gormStream) Close() error {
	s.closed.Store(true)
	return nil
}

// Compile-time interface checks.
var _ Stream = (*gormStream)(nil)
var _ session.Service = (*service)(nil)
