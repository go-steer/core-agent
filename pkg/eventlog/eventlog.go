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

// Package eventlog is the durable, append-only audit log that backs
// agent.Agent's session.Service. Each event the ADK runner appends to
// a session is persisted to the underlying database (SQLite, MySQL, or
// Postgres via GORM) and assigned a monotonic seq number. Subscribers
// can replay history with Since(fromSeq) or live-tail with
// Watch(fromSeq).
//
// The package layers on top of ADK's session/database service: ADK
// owns the events / sessions / state tables, and we add a thin
// agent_eventlog overlay table whose rows reference ADK's events by
// id and add the seq column. Two GORM connections (ADK's and ours)
// share the same database file/DSN — atomic-across-tables writes are
// not provided in v1; the AppendEvent path writes ADK first, then
// the overlay, and surfaces overlay-write errors so callers can
// retry (event_id is unique-indexed for safe idempotency).
//
// See docs/eventlog-plan.md and docs/eventlog-decisions.md for the
// design rationale and milestone breakdown.
package eventlog

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"google.golang.org/adk/session"
	"gorm.io/gorm"
)

// Stream is the append-only event log primitive. Implementations are
// expected to be safe for concurrent use.
type Stream interface {
	// Append writes ev to the log under sess. Returns the assigned
	// seq number. The event itself is also expected to be persisted
	// via the paired session.Service.AppendEvent — Stream.Append
	// only writes the overlay row that carries the seq.
	//
	// Most callers don't invoke this directly; agent.Run drives the
	// session.Service which in turn calls Append internally.
	Append(ctx context.Context, sess session.Session, ev *session.Event) (seq int64, err error)

	// Since returns events with seq > fromSeq, in seq order. Bounded
	// by current end-of-log; returns when caught up. Apply filters
	// via QueryOption (ForSession, WithBranchPrefix, WithAuthor,
	// WithLimit).
	Since(ctx context.Context, fromSeq int64, opts ...QueryOption) iter.Seq2[Entry, error]

	// Watch returns events with seq > fromSeq, in seq order, blocking
	// for new events as they're appended. Cancel ctx to stop. Same
	// QueryOptions as Since.
	//
	// The default poll interval is 200ms, configurable via Open's
	// WithWatchInterval option.
	Watch(ctx context.Context, fromSeq int64, opts ...QueryOption) iter.Seq2[Entry, error]

	// Close releases resources held by the Stream (typically the
	// underlying gorm.DB connection pool). Safe to call multiple
	// times.
	Close() error
}

// BranchLister is an OPTIONAL Stream extension: report the distinct
// branch labels that exist under a query's filters, without hydrating
// a single event.
//
// It answers "what ran here?", which the Since/Watch pair can only
// answer by reading every row and looking at the branch column. The
// caller that needs it is the attach layer's subagent-events endpoint:
// a subagent declared as "cluster" writes its turns under the branch
// the runner minted for the instance ("bg.cluster-1"), so resolving a
// name to its branches means asking the log which branches are there
// (go-steer/core-agent#694).
//
// Not part of Stream itself — adding a method to a published interface
// breaks every implementation outside this repo. Type-assert for it
// and degrade when it's absent:
//
//	if bl, ok := stream.(eventlog.BranchLister); ok { ... }
//
// Honors the same QueryOptions as Since, including WithLimit (a cap on
// the number of DISTINCT labels returned, not on rows scanned). Labels
// come back sorted, and the empty branch — the parent session's own
// turns — is omitted.
type BranchLister interface {
	Branches(ctx context.Context, opts ...QueryOption) ([]string, error)
}

// Entry is one row from the event log: the assigned seq plus the
// underlying ADK session.Event (loaded via the paired
// session.Service).
//
// Metadata is an optional sidecar map populated by a MetadataExtractor
// (see WithMetadataExtractor). The eventlog package itself is agnostic
// to the keys — agent.Agent wires an extractor that pulls
// auth.Caller.Identity (key "caller") and proxy attribution
// (key "proxy_by") from the request context. Rows persisted before
// the sidecar column shipped read back as a nil Metadata map.
type Entry struct {
	Seq      int64
	Event    *session.Event
	Metadata map[string]string
}

// MetadataExtractor pulls the per-event sidecar metadata from the
// context at Append time. Return nil (or empty map) to skip — empty
// maps round-trip as nil on the read side. Callers wire an extractor
// via WithMetadataExtractor; the default is no-op (preserves the
// pre-sidecar shape on disk).
//
// The function is called inside Append's request context, so it can
// safely fetch request-scoped values without spawning goroutines.
type MetadataExtractor func(ctx context.Context) map[string]string

// Handle bundles the Stream with the session.Service that writes to
// the same database. agent.WithEventLog(handle) wires both into an
// agent.Agent in one call.
type Handle struct {
	// Stream is the seq + replay + watch primitive.
	Stream Stream
	// Service is the session.Service backed by the same database.
	// Pass to agent.WithSessionService (or use the
	// agent.WithEventLog convenience that does both at once).
	Service session.Service
	// DB exposes the overlay-table connection so adjacent
	// substrates (e.g., pkg/attach.SessionACLStore) can share the
	// same database without re-opening it. Read-only access from
	// outside pkg/eventlog — mutations happen via the typed
	// stores. Nil before Open returns and after Close.
	DB *gorm.DB
	// db is the same value as DB, kept as an unexported alias for
	// the original lifecycle code in Close (so the cleanup nil-out
	// stays correct).
	db *gorm.DB
	// adkDB is ADK's own gorm.DB — a second connection pool opened
	// inside adkdatabase.NewSessionService against the same DSN. We
	// retain it so Close can shut it down too; otherwise every
	// Open/Close cycle leaks the ADK pool's connections and file
	// handles. Nil if it couldn't be retrieved (Close then skips it).
	adkDB *gorm.DB
}

// Close releases all resources held by the Handle (Stream + the
// underlying database connection). Safe to call multiple times.
func (h *Handle) Close() error {
	if h == nil {
		return nil
	}
	var firstErr error
	if h.Stream != nil {
		if err := h.Stream.Close(); err != nil {
			firstErr = err
		}
	}
	if h.db != nil {
		if err := closeGormDB(h.db); err != nil && firstErr == nil {
			firstErr = err
		}
		h.db = nil
	}
	// Close ADK's separate connection pool too. Skipping it leaked a
	// pool per Open/Close cycle (tests, multi-daemon session churn).
	if h.adkDB != nil {
		if err := closeGormDB(h.adkDB); err != nil && firstErr == nil {
			firstErr = err
		}
		h.adkDB = nil
	}
	return firstErr
}

// closeGormDB closes the underlying *sql.DB connection pool behind a
// gorm.DB. Shared by Handle.Close for both the overlay and ADK pools.
func closeGormDB(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// Option configures Open.
type Option func(*openOpts)

type openOpts struct {
	watchInterval     time.Duration
	gormConfig        *gorm.Config
	skipWAL           bool
	metadataExtractor MetadataExtractor
}

func defaultOpenOpts() openOpts {
	return openOpts{
		watchInterval: 200 * time.Millisecond,
	}
}

// WithWatchInterval sets the polling interval Watch uses to check for
// new rows. Default is 200ms. Smaller values reduce subscriber latency
// at the cost of database load; larger values do the opposite.
func WithWatchInterval(d time.Duration) Option {
	return func(o *openOpts) {
		if d > 0 {
			o.watchInterval = d
		}
	}
}

// WithGORMConfig overrides the gorm.Config used for the overlay
// connection. Useful for silencing the default logger in tests
// (gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}).
func WithGORMConfig(c *gorm.Config) Option {
	return func(o *openOpts) { o.gormConfig = c }
}

// WithSkipWAL disables the automatic PRAGMA journal_mode=WAL set on
// SQLite databases at Open time. WAL is on by default because it
// permits concurrent readers alongside a writer; turn it off for
// in-memory databases or read-only setups where WAL adds no value.
func WithSkipWAL() Option {
	return func(o *openOpts) { o.skipWAL = true }
}

// WithMetadataExtractor wires a function that produces sidecar
// metadata for each appended event. The map (JSON-encoded) is stored
// in the overlay row's metadata column and surfaces on Entry.Metadata
// at read time. Nil disables the sidecar (default).
//
// agent.New wires an extractor that pulls the per-request caller +
// proxy attribution from the context so multi-session audit logs
// carry who triggered each event without coupling pkg/eventlog to
// pkg/auth.
func WithMetadataExtractor(fn MetadataExtractor) Option {
	return func(o *openOpts) { o.metadataExtractor = fn }
}

// QueryOption filters Since/Watch results.
type QueryOption func(*queryOpts)

type queryOpts struct {
	appName, userID, sessionID string
	treeAppName, treeUserID    string
	treeParentID               string
	branchPrefixes             []string
	author                     string
	authorSuffix               string
	limit                      int
}

// ForSession restricts results to one session triple. Without it,
// queries scan across every session in the database — useful for
// audit dashboards, dangerous for high-volume reads.
func ForSession(appName, userID, sessionID string) QueryOption {
	return func(q *queryOpts) {
		q.appName = appName
		q.userID = userID
		q.sessionID = sessionID
	}
}

// WithSessionTree restricts results to the parent session ID and any
// derived sub-session IDs. The subagent runner names its session
// "<parent>:sub:<branch>" by convention; this option's underlying SQL
// matches the parent + every "<parent>:sub:%" descendant in one query
// so an audit can pull the whole tree without a follow-up join.
//
// When set, takes precedence over the (App, User, Session) triple
// from ForSession — the two are mutually exclusive in practice
// because WithSessionTree implies the (app, user) pair already.
// Mutually composable with the other QueryOptions (WithBranchPrefix,
// WithAuthor, WithAuthorSuffix, WithLimit).
func WithSessionTree(appName, userID, parentSessionID string) QueryOption {
	return func(q *queryOpts) {
		q.treeAppName = appName
		q.treeUserID = userID
		q.treeParentID = parentSessionID
	}
}

// WithBranchPrefix matches events whose Branch field begins with
// prefix. Use to scope queries to a subagent subtree once Phase 4 of
// the eventlog plan ships subagent runners that set Branch.
//
// Repeated calls accumulate and are OR'd together — see
// WithAnyBranchPrefix, which this delegates to.
func WithBranchPrefix(prefix string) QueryOption {
	return WithAnyBranchPrefix(prefix)
}

// WithAnyBranchPrefix matches events whose Branch begins with ANY of
// the supplied prefixes (an OR group, AND'd with the other filters).
//
// One subagent is reachable under several branch spellings depending
// on how it was launched — a sync subagent tool tags its events with
// the bare name, the background runner with "bg.<name>", RunSubtask
// with "sub.<name>", the remote runner with "remote.<name>" — so an
// operator asking "what did subagent X do?" needs the union, not a
// single prefix (#638).
//
// Empty prefixes are dropped: a caller building the list from
// user input can't accidentally widen the query to everything by
// passing "". LIKE metacharacters ('%', '_') in a prefix are escaped,
// so a prefix matches literally and only the intended subtree.
func WithAnyBranchPrefix(prefixes ...string) QueryOption {
	return func(q *queryOpts) {
		for _, p := range prefixes {
			if p != "" {
				q.branchPrefixes = append(q.branchPrefixes, p)
			}
		}
	}
}

// WithAuthor matches events emitted by a specific author. The
// autonomous driver uses Author="<binary>/autonomous" for checkpoint
// events; consumer-supplied authors work the same way.
func WithAuthor(name string) QueryOption {
	return func(q *queryOpts) { q.author = name }
}

// WithAuthorSuffix matches events whose Author ends with the supplied
// suffix. Used by ResumeAutonomous to find checkpoint events
// regardless of which binary emitted them — checkpoints land with
// Author="<binary>/autonomous", so suffix "/autonomous" matches
// checkpoints from any core-agent-family process. Empty suffix is a
// no-op (matches everything).
func WithAuthorSuffix(suffix string) QueryOption {
	return func(q *queryOpts) { q.authorSuffix = suffix }
}

// WithLimit caps the number of entries returned. Zero or negative is
// treated as no limit.
func WithLimit(n int) QueryOption {
	return func(q *queryOpts) {
		if n > 0 {
			q.limit = n
		}
	}
}

// ErrClosed is returned by Stream methods invoked after Close.
var ErrClosed = errors.New("eventlog: stream is closed")

// IsSessionNotFound reports whether err means "that session is not in
// the log", as opposed to a real failure to read it.
//
// ADK's database session service wraps GORM's ErrRecordNotFound with
// %w behind the text "database error while fetching session", which
// reads like a database fault and is not one — a session that has
// never been written is the ordinary state of every session before its
// first event. Callers that treat any non-nil error as a problem log a
// scary line on a healthy cold boot; this is the check that tells the
// two apart without matching on message text.
//
// It lives here rather than in the callers because the GORM coupling
// is this package's to own.
func IsSessionNotFound(err error) bool {
	return err != nil && errors.Is(err, gorm.ErrRecordNotFound)
}

// Ping reports whether the event log is still readable, for use by a
// readiness probe (#946).
//
// It issues a real bounded read against the overlay table rather than
// pinging the connection pool. On SQLite a pool ping proves almost
// nothing: the pool hands back a live *sql.DB whose file may since
// have been deleted, replaced, or locked by another writer, and
// database/sql only discovers that on a statement. One indexed row
// read is the cheapest query that actually touches the file.
//
// A closed Handle reports ErrClosed rather than panicking, so a probe
// racing shutdown fails cleanly instead of taking the process with it.
func (h *Handle) Ping(ctx context.Context) error {
	if h == nil || h.DB == nil {
		return ErrClosed
	}
	var seq int64
	// Scan (not First) so an empty log is success, not
	// ErrRecordNotFound — a daemon that has served no events yet is
	// ready, and the probe must not say otherwise on a cold boot.
	if err := h.DB.WithContext(ctx).
		Model(&agentEventRow{}).
		Select("seq").
		Limit(1).
		Scan(&seq).Error; err != nil {
		return fmt.Errorf("eventlog: ping: %w", err)
	}
	return nil
}
