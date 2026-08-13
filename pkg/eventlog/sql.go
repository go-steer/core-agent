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
	"reflect"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"google.golang.org/adk/session"
	adkdatabase "google.golang.org/adk/session/database"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// sqliteBusyTimeoutMs is how long every new SQLite connection waits
// for the write lock before returning SQLITE_BUSY. Five seconds is
// long enough to ride out a parent's checkpoint write while a child
// subagent tries to create its session row, and short enough that a
// genuinely deadlocked DB surfaces visibly rather than hanging.
const sqliteBusyTimeoutMs = 5000

// agentEventRow is the overlay table that gives every event a
// monotonic seq alongside ADK's events table. event_id is a logical
// foreign key to events.id (we do not declare a constraint to avoid
// coupling our migration to ADK's schema evolution).
//
// Metadata holds the JSON-encoded sidecar map populated by the
// MetadataExtractor (typically the per-request auth.Caller identity
// and proxy attribution). Empty string represents "no sidecar" — rows
// persisted before this column shipped are read back with an empty
// string and surface as Entry.Metadata = nil.
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
	Metadata     string `gorm:"type:text"`
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

	// 0) For SQLite, tune the DSN before any connection is opened.
	// Both settings apply to every connection in the ADK and overlay
	// pools (each opens its own gorm.DB against this dialector).
	//
	//   - busy_timeout(N): a writer that finds the write lock held
	//     waits up to N ms instead of failing immediately with
	//     SQLITE_BUSY.
	//   - _txlock=immediate: begin every read-write transaction with
	//     BEGIN IMMEDIATE so the write lock is taken up front. This is
	//     load-bearing, not belt-and-suspenders: ADK's AppendEvent
	//     reads the session row and then writes state + the event row
	//     inside one transaction. Under the default deferred BEGIN the
	//     read takes a snapshot and the first write tries to upgrade it
	//     to the write lock — and SQLite refuses that upgrade with an
	//     *immediate* SQLITE_BUSY when another connection holds the
	//     lock, because busy_timeout deliberately never retries a
	//     snapshot upgrade (doing so could deadlock two upgraders). So
	//     busy_timeout alone does nothing for the case that actually
	//     bites us: the boot-scan/lock/bootlog writes on the overlay
	//     pool holding the write lock while a continuation turn's
	//     AppendEvent on the ADK pool tries to commit (#575), and the
	//     parent-checkpoint vs. subagent-create race (#230). IMMEDIATE
	//     turns both into an ordinary busy_timeout wait. Reads (Get,
	//     List) use no explicit transaction, so they stay lock-free
	//     under WAL — IMMEDIATE only affects read-write transactions.
	// No-op for non-SQLite drivers, which carry their own semantics.
	if isSQLite(dialector) {
		injectSQLitePragma(dialector, fmt.Sprintf("busy_timeout(%d)", sqliteBusyTimeoutMs))
		injectSQLiteDSNParam(dialector, "_txlock", "immediate")
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
		db:                db,
		adkSvc:            adkSvc,
		watchInterval:     o.watchInterval,
		metadataExtractor: o.metadataExtractor,
	}
	svc := &service{inner: adkSvc, stream: stream}
	return &Handle{Stream: stream, Service: svc, db: db, DB: db, adkDB: adkGormDB(adkSvc)}, nil
}

// adkGormDB reaches the *gorm.DB that adkdatabase.NewSessionService
// opens internally so Handle.Close can shut down its connection pool.
// ADK's databaseService keeps the handle in an unexported `db` field
// with no accessor, so we read it reflectively (mirroring the reflection
// injectSQLitePragma already relies on for the dialector's DSN field).
// Returns nil if the service isn't the shape we expect — Close then
// simply skips it, preserving today's behavior rather than panicking.
func adkGormDB(svc session.Service) *gorm.DB {
	if svc == nil {
		return nil
	}
	v := reflect.ValueOf(svc)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	f := v.FieldByName("db")
	if !f.IsValid() || f.Kind() != reflect.Pointer || !f.CanAddr() {
		return nil
	}
	// The field is unexported, so read it through its address. The
	// value is only ever read (never mutated) and lives for the
	// lifetime of the Handle.
	// #nosec G103 -- unexported-field read of ADK's own *gorm.DB; the only way to close its pool.
	f = reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
	db, ok := f.Interface().(*gorm.DB)
	if !ok {
		return nil
	}
	return db
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

// injectSQLitePragma appends `_pragma=<spec>` (e.g. "busy_timeout(5000)")
// to the SQLite dialector's DSN so every subsequent connection picks
// up the pragma at open time. A DSN may carry several `_pragma=` params,
// so double-injection is detected per pragma name (a caller-supplied
// busy_timeout with a different value is left untouched).
func injectSQLitePragma(d gorm.Dialector, spec string) {
	mutateSQLiteDSN(d, func(dsn string) string {
		// Don't double-inject — caller may have set this in their DSN.
		if strings.Contains(dsn, "_pragma="+pragmaName(spec)) {
			return dsn
		}
		return appendDSNParam(dsn, "_pragma="+spec)
	})
}

// injectSQLiteDSNParam appends a plain `key=value` query parameter to
// the SQLite dialector's DSN (e.g. "_txlock", "immediate"). Unlike
// _pragma these are driver-level DSN options rather than SQL PRAGMAs,
// so they can't ride the _pragma channel. No-op when the key is already
// present, so a caller-supplied value in their own DSN wins.
func injectSQLiteDSNParam(d gorm.Dialector, key, value string) {
	mutateSQLiteDSN(d, func(dsn string) string {
		if strings.Contains(dsn, key+"=") {
			return dsn
		}
		return appendDSNParam(dsn, key+"="+value)
	})
}

// mutateSQLiteDSN reads the dialector's exported string `DSN` field and
// replaces it with fn(dsn). Both `github.com/glebarez/sqlite` and
// `gorm.io/driver/sqlite` expose that field, so reflection works
// without hard-coding either driver here. No-op when the field isn't
// present (unknown SQLite driver shape — caller can still set options
// in their own DSN). Centralizes the one reflective access the pragma
// and raw-param injectors share.
func mutateSQLiteDSN(d gorm.Dialector, fn func(string) string) {
	if d == nil {
		return
	}
	v := reflect.ValueOf(d)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}
	f := v.FieldByName("DSN")
	if !f.IsValid() || f.Kind() != reflect.String || !f.CanSet() {
		return
	}
	f.SetString(fn(f.String()))
}

// appendDSNParam appends a `key=value` (or `_pragma=spec`) query
// parameter to a DSN, choosing `?` or `&` as the separator.
func appendDSNParam(dsn, param string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + param
}

// pragmaName extracts the pragma name from a spec like
// "busy_timeout(5000)" → "busy_timeout". Used to detect "already set"
// in injectSQLitePragma so a caller-supplied value (different timeout)
// isn't overwritten.
func pragmaName(spec string) string {
	if i := strings.Index(spec, "("); i > 0 {
		return spec[:i]
	}
	return spec
}

// gormStream implements Stream backed by the agent_eventlog table.
type gormStream struct {
	db                *gorm.DB
	adkSvc            session.Service
	watchInterval     time.Duration
	metadataExtractor MetadataExtractor

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
	if s.metadataExtractor != nil {
		if md := s.metadataExtractor(ctx); len(md) > 0 {
			encoded, mdErr := encodeMetadata(md)
			if mdErr != nil {
				return 0, fmt.Errorf("eventlog: encode metadata: %w", mdErr)
			}
			row.Metadata = encoded
		}
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		return 0, fmt.Errorf("eventlog: insert overlay row: %w", err)
	}
	return row.Seq, nil
}

// deleteSession removes every overlay row for a session. The service
// wrapper calls this after ADK deletes its own rows so we don't leave
// orphaned overlay rows behind. An orphaned row is poison: hydration
// re-fetches the (now missing) session and returns "session not found"
// forever, and because the row's seq is real, an unfiltered Watch/Since
// re-queries and re-errors it on every poll — one deletion would
// otherwise break live-tail/replay for every consumer on the log.
func (s *gormStream) deleteSession(ctx context.Context, appName, userID, sessionID string) error {
	if err := s.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ?", appName, userID, sessionID).
		Delete(&agentEventRow{}).Error; err != nil {
		return fmt.Errorf("eventlog: delete overlay rows for session %q: %w", sessionID, err)
	}
	return nil
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
				// Advance the cursor past this row unconditionally —
				// including when hydration failed. iterateOnceFunc
				// still populates e.Seq on error, so a permanently
				// unhydratable row (e.g. its session was deleted out
				// from under us) is surfaced once and then skipped,
				// instead of being re-queried and re-errored on every
				// poll interval forever.
				if e.Seq > cursor {
					cursor = e.Seq
					advanced = true
				}
				return yield(e, err)
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
	h := newSessionHydrator(s.adkSvc)
	for _, r := range rows {
		md := decodeMetadata(r.Metadata)
		ev, err := h.hydrate(ctx, r)
		if err != nil {
			if !yield(Entry{Seq: r.Seq, Metadata: md}, err) {
				return false
			}
			continue
		}
		if !yield(Entry{Seq: r.Seq, Event: ev, Metadata: md}, nil) {
			return false
		}
	}
	return true
}

// LatestSeq returns the highest seq currently visible under the same
// filters Since/Watch honor, or 0 when no matching rows exist. A
// single indexed MAX(seq) query — used by pkg/attach to clamp an
// unbounded ?since=0 replay to a bounded tail (#385) without scanning
// (or hydrating) the full table. Optional Stream extension: callers
// discover it by type assertion.
func (s *gormStream) LatestSeq(ctx context.Context, opts ...QueryOption) (int64, error) {
	if s.closed.Load() {
		return 0, ErrClosed
	}
	q := queryOpts{}
	for _, o := range opts {
		o(&q)
	}
	tx := applyQueryFilters(s.db.WithContext(ctx).Model(&agentEventRow{}), q)
	var maxSeq int64
	if err := tx.Select("COALESCE(MAX(seq), 0)").Scan(&maxSeq).Error; err != nil {
		return 0, fmt.Errorf("eventlog: query max seq: %w", err)
	}
	return maxSeq, nil
}

// Branches implements BranchLister: the distinct branch labels
// visible under the same filters Since/Watch honor, sorted, with the
// empty branch (the session's own turns) left out.
//
// A single indexed SELECT DISTINCT over the branch column — no event
// hydration, which is what makes it usable on the request path where
// scanning a long session's rows to collect its branch labels would
// not be. WithLimit caps the number of labels returned.
func (s *gormStream) Branches(ctx context.Context, opts ...QueryOption) ([]string, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	q := queryOpts{}
	for _, o := range opts {
		o(&q)
	}
	tx := applyQueryFilters(s.db.WithContext(ctx).Model(&agentEventRow{}), q).
		Where("branch <> ''").
		Distinct("branch").
		Order("branch ASC")
	if q.limit > 0 {
		tx = tx.Limit(q.limit)
	}
	var branches []string
	if err := tx.Pluck("branch", &branches).Error; err != nil {
		return nil, fmt.Errorf("eventlog: query distinct branches: %w", err)
	}
	return branches, nil
}

// NthNewestSeq returns the seq of the row `offset` places behind the
// newest row visible under the same filters Since/Watch honor
// (offset 0 = the newest row), or 0 when fewer than offset+1 rows
// match. A single indexed ORDER BY seq DESC LIMIT 1 OFFSET n query.
//
// Used by pkg/attach to clamp an unbounded ?since=0 replay to the
// session's own newest N events (#481): seq values are global across
// sessions (one autoincrement for the whole table), so a floor
// computed as MAX(seq)-N silently truncates a quiet session's history
// whenever a busy sibling session has advanced the global counter.
// The floor must come from the filtered row set itself. Optional
// Stream extension: callers discover it by type assertion.
func (s *gormStream) NthNewestSeq(ctx context.Context, offset int64, opts ...QueryOption) (int64, error) {
	if s.closed.Load() {
		return 0, ErrClosed
	}
	if offset < 0 {
		return 0, fmt.Errorf("eventlog: NthNewestSeq: negative offset %d", offset)
	}
	q := queryOpts{}
	for _, o := range opts {
		o(&q)
	}
	tx := applyQueryFilters(s.db.WithContext(ctx).Model(&agentEventRow{}), q)
	var seqs []int64
	if err := tx.Order("seq DESC").Offset(int(offset)).Limit(1).Pluck("seq", &seqs).Error; err != nil {
		return 0, fmt.Errorf("eventlog: query nth-newest seq: %w", err)
	}
	if len(seqs) == 0 {
		return 0, nil
	}
	return seqs[0], nil
}

// applyQueryFilters translates queryOpts into WHERE clauses. Shared
// by queryRows, LatestSeq, and NthNewestSeq so all see the exact same
// visibility rules. Does NOT apply the seq cursor, ordering, or limit
// — those belong to the row query only.
func applyQueryFilters(tx *gorm.DB, q queryOpts) *gorm.DB {
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
	if len(q.branchPrefixes) > 0 {
		sql, args := branchPrefixClause(q.branchPrefixes)
		tx = tx.Where(sql, args...)
	}
	if q.author != "" {
		tx = tx.Where("author = ?", q.author)
	}
	if q.authorSuffix != "" {
		tx = tx.Where("author LIKE ?", "%"+q.authorSuffix)
	}
	return tx
}

// branchPrefixEscape is the LIKE escape character for the branch
// filter. Deliberately NOT the conventional backslash: the escape
// char is embedded in the SQL text as a string literal, and backslash
// is itself special inside string literals on MySQL (which needs
// ESCAPE '\\') but not on Postgres with standard_conforming_strings
// (which rejects '\\' as a two-character escape). '!' has no special
// meaning in a string literal on any driver we open, so one spelling
// works everywhere. escapeLike doubles it when it appears in the
// prefix itself.
const branchPrefixEscape = "!"

// escapeLike neutralizes the LIKE metacharacters in a literal so it
// matches itself and nothing else. Without it a subagent named
// "gke_cluster" would match branches like "gkeXcluster.child" ('_' is
// LIKE's single-character wildcard), and a caller who passed "%" would
// silently widen the query to every branch in the session tree.
func escapeLike(s string) string {
	r := strings.NewReplacer(
		branchPrefixEscape, branchPrefixEscape+branchPrefixEscape,
		"%", branchPrefixEscape+"%",
		"_", branchPrefixEscape+"_",
	)
	return r.Replace(s)
}

// branchPrefixClause builds the OR group for WithAnyBranchPrefix: for
// each prefix, an exact-branch match plus the two separator-joined
// descendant forms. ADK uses '.' for branch separators (per its
// docstring: agent_1.agent_2.agent_3); '/' is accepted too so a caller
// passing "parent" matches "parent/child" as well as "parent.child".
//
// Returns the parenthesized SQL and its bound args. GORM does wrap an
// OR-bearing Where fragment for us today, but the clause is AND'd with
// the session and author filters and an unwrapped chain of ORs would
// swallow them — too sharp an edge to leave resting on a detail of the
// query builder.
func branchPrefixClause(prefixes []string) (string, []any) {
	clauses := make([]string, 0, len(prefixes)*3)
	args := make([]any, 0, len(prefixes)*3)
	for _, p := range prefixes {
		esc := escapeLike(p)
		clauses = append(clauses,
			"branch = ?",
			"branch LIKE ? ESCAPE '"+branchPrefixEscape+"'",
			"branch LIKE ? ESCAPE '"+branchPrefixEscape+"'",
		)
		args = append(args, p, esc+".%", esc+"/%")
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args
}

// queryRows runs the SELECT against agent_eventlog with all filters
// applied. Returns rows in seq order.
func (s *gormStream) queryRows(ctx context.Context, fromSeq int64, q queryOpts) ([]agentEventRow, error) {
	tx := applyQueryFilters(s.db.WithContext(ctx).Model(&agentEventRow{}).Where("seq > ?", fromSeq), q)
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

// sessionKey identifies a session for the hydration cache.
type sessionKey struct {
	app, user, session string
}

// sessionIndex is a loaded session's events indexed by event ID (or the
// error from trying to load it). Building it once turns per-row
// hydration into an O(1) map lookup.
type sessionIndex struct {
	events map[string]*session.Event
	err    error
}

// sessionHydrator hydrates overlay rows into their session.Events for a
// single Since/Watch pass. It loads each distinct session at most once
// through the session.Service and indexes that session's events by ID,
// so hydrating a row is a map lookup rather than a full-session Get plus
// linear scan per row. This is what keeps replay linear: previously
// loadEvent re-fetched and re-scanned the whole session for every one
// of its N rows, so a Since/Watch over an N-event session did N
// full-session Gets — O(N^2). We still go through the session.Service
// interface rather than reaching into ADK's schema, so the decoupling
// from ADK's row layout is preserved.
type sessionHydrator struct {
	svc   session.Service
	cache map[sessionKey]*sessionIndex
}

func newSessionHydrator(svc session.Service) *sessionHydrator {
	return &sessionHydrator{svc: svc, cache: make(map[sessionKey]*sessionIndex)}
}

// hydrate returns the session.Event for row r, loading and indexing r's
// session on first use and serving subsequent rows of the same session
// (including a failed load) from the cache.
func (h *sessionHydrator) hydrate(ctx context.Context, r agentEventRow) (*session.Event, error) {
	key := sessionKey{app: r.AppName, user: r.UserID, session: r.SessionID}
	idx, ok := h.cache[key]
	if !ok {
		idx = h.buildIndex(ctx, key)
		h.cache[key] = idx
	}
	if idx.err != nil {
		return nil, idx.err
	}
	ev := idx.events[r.EventID]
	if ev == nil {
		return nil, fmt.Errorf("eventlog: event %q not found in session %q", r.EventID, r.SessionID)
	}
	return ev, nil
}

// buildIndex loads a session once and indexes its events by ID. A load
// failure (or missing session) is captured in the returned index and
// cached so we don't re-Get a missing session for each of its rows.
func (h *sessionHydrator) buildIndex(ctx context.Context, key sessionKey) *sessionIndex {
	resp, err := h.svc.Get(ctx, &session.GetRequest{
		AppName:   key.app,
		UserID:    key.user,
		SessionID: key.session,
	})
	if err != nil {
		return &sessionIndex{err: fmt.Errorf("eventlog: load session %q: %w", key.session, err)}
	}
	if resp == nil || resp.Session == nil {
		return &sessionIndex{err: fmt.Errorf("eventlog: session %q not found", key.session)}
	}
	events := make(map[string]*session.Event)
	for ev := range resp.Session.Events().All() {
		if ev != nil {
			events[ev.ID] = ev
		}
	}
	return &sessionIndex{events: events}
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
