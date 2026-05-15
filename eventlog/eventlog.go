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

// Entry is one row from the event log: the assigned seq plus the
// underlying ADK session.Event (loaded via the paired
// session.Service).
type Entry struct {
	Seq   int64
	Event *session.Event
}

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
	// db is our overlay-table connection. Closed by Handle.Close.
	db *gorm.DB
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
		sqlDB, err := h.db.DB()
		if err == nil {
			if cerr := sqlDB.Close(); cerr != nil && firstErr == nil {
				firstErr = cerr
			}
		} else if firstErr == nil {
			firstErr = err
		}
		h.db = nil
	}
	return firstErr
}

// Option configures Open.
type Option func(*openOpts)

type openOpts struct {
	watchInterval time.Duration
	gormConfig    *gorm.Config
	skipWAL       bool
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

// QueryOption filters Since/Watch results.
type QueryOption func(*queryOpts)

type queryOpts struct {
	appName, userID, sessionID string
	branchPrefix               string
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

// WithBranchPrefix matches events whose Branch field begins with
// prefix. Use to scope queries to a subagent subtree once Phase 4 of
// the eventlog plan ships subagent runners that set Branch.
func WithBranchPrefix(prefix string) QueryOption {
	return func(q *queryOpts) { q.branchPrefix = prefix }
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
