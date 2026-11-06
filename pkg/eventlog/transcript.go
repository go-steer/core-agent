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
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/adk/v2/session"
	"gorm.io/gorm"
)

// Transcript is the store a session's ordered event record lives in:
// ADK's session.Service plus the seq-ordered read-back every Since and
// Watch consumer relies on. It is the first of the four store concerns
// in docs/external-state-design.md (#1192) to get a seam; control,
// index and the per-session state stores stay on the local SQL
// database for now.
//
// There is one implementation per backend. The SQL one is this
// package's (OpenSQLTranscript); pkg/statestore holds the others. The
// conformance suite in pkg/statestore/conformance is the contract:
// every backend runs it.
//
// What Service() must do beyond plain ADK semantics:
//
//   - AppendEvent assigns Entry.Seq, unique and increasing within the
//     event's root session tree (RootOf), and persists the sidecar
//     found by EntryMetadataFrom(ctx) with the event. It drops partial
//     events without writing anything, as ADK's own services do.
//   - Get reports a missing session with an error wrapping
//     session.ErrNotFound, so IsSessionNotFound classifies it. Delete
//     of a missing session is a no-op, as ADK's contract says.
//   - Create honours CreateRequest.SessionID.
type Transcript interface {
	// Service is the ADK session.Service for this backend.
	Service() session.Service

	// Since yields entries with seq > fromSeq in seq order, bounded by
	// the current end of the record, under the same QueryOption
	// semantics as Stream.Since. A backend that cannot enumerate its
	// whole store refuses a query whose ResolveQuery(opts).Scope() is
	// not ok with an error wrapping ErrUnscopedQuery.
	Since(ctx context.Context, fromSeq int64, opts ...QueryOption) iter.Seq2[Entry, error]

	// HighestSeq is the last seq assigned in the root session tree
	// (appName, userID, root), or 0 when it has no events.
	HighestSeq(ctx context.Context, appName, userID, root string) (int64, error)

	// Subscribe returns a channel that receives a wake-up after every
	// event this process appends to the tree (appName, userID, root),
	// and a cancel func that releases the subscription. The channel
	// carries no data: a reader runs Since from its cursor to catch up,
	// so a slow reader coalesces wake-ups instead of lagging. Wake-ups
	// from other processes are not promised; a backend whose store is
	// shared across processes (SQL over a file) is read by polling.
	Subscribe(appName, userID, root string) (wake <-chan struct{}, cancel func())

	// Close releases the backend's resources. Safe to call twice.
	Close() error
}

// ErrAppendViaService is returned by Stream.Append on a Handle whose
// Transcript assigns seq inside its own Service().AppendEvent. There
// is no separate overlay to write: append through Handle.Service.
var ErrAppendViaService = errors.New("eventlog: this Stream has no separate overlay; append through Handle.Service")

type entryMetadataKey struct{}

// ContextWithEntryMetadata attaches the sidecar map for the event about
// to be appended, for a Transcript backend to persist (see
// EntryMetadataFrom). Handle.Service does this itself from the
// MetadataExtractor; callers appending straight through a Transcript's
// Service, and the conformance suite, use it directly.
func ContextWithEntryMetadata(ctx context.Context, md map[string]string) context.Context {
	if len(md) == 0 {
		return ctx
	}
	return context.WithValue(ctx, entryMetadataKey{}, md)
}

// EntryMetadataFrom returns the sidecar metadata the Handle's
// MetadataExtractor produced for the event being appended, or nil. A
// Transcript backend reads it inside Service().AppendEvent and stores
// it so Since returns it as Entry.Metadata.
func EntryMetadataFrom(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	md, _ := ctx.Value(entryMetadataKey{}).(map[string]string)
	return md
}

// Subscribers is the in-process wake-up registry a Transcript backend
// uses to implement Subscribe. Notify never blocks: each subscriber's
// channel holds at most one pending wake-up, and a second one before
// the reader wakes is coalesced into it. The zero value is ready.
type Subscribers struct {
	mu   sync.Mutex
	subs map[treeKey]map[*subscriber]struct{}
}

type treeKey struct{ app, user, root string }

type subscriber struct{ ch chan struct{} }

// Subscribe registers a wake-up channel for the tree.
func (s *Subscribers) Subscribe(appName, userID, root string) (<-chan struct{}, func()) {
	k := treeKey{appName, userID, root}
	sub := &subscriber{ch: make(chan struct{}, 1)}
	s.mu.Lock()
	if s.subs == nil {
		s.subs = make(map[treeKey]map[*subscriber]struct{})
	}
	if s.subs[k] == nil {
		s.subs[k] = make(map[*subscriber]struct{})
	}
	s.subs[k][sub] = struct{}{}
	s.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			if m := s.subs[k]; m != nil {
				delete(m, sub)
				if len(m) == 0 {
					delete(s.subs, k)
				}
			}
			s.mu.Unlock()
		})
	}
	return sub.ch, cancel
}

// Notify wakes every subscriber of the tree without blocking.
func (s *Subscribers) Notify(appName, userID, root string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs[treeKey{appName, userID, root}] {
		select {
		case sub.ch <- struct{}{}:
		default:
		}
	}
}

// OpenTranscript constructs a Handle whose transcript lives in t and
// whose control, index and per-session state stores (run lock, claims,
// boot log, guardrail state, spend ledger, approval decisions, pauses,
// schedules) live on the local database behind dialector — in the
// shipped CLI, the --session-db SQLite file. Handle.DB is that local
// database.
//
// Handle.Service appends through t.Service() (which assigns seq) and
// then applies guardrail state locally. Handle.Stream reads t: Since
// is t.Since; Watch replays with Since and then waits on t.Subscribe
// for the next wake-up, so live tail costs no polling; Append returns
// ErrAppendViaService. Options that only mean something for the SQL
// transcript (WithWatchInterval, WithSkipWAL for the transcript side)
// apply to the local database where they apply at all.
func OpenTranscript(ctx context.Context, t Transcript, dialector gorm.Dialector, opts ...Option) (*Handle, error) {
	if t == nil {
		return nil, errors.New("eventlog: OpenTranscript: transcript is required")
	}
	o := defaultOpenOpts()
	for _, opt := range opts {
		opt(&o)
	}
	db, err := openLocalDB(ctx, dialector, o)
	if err != nil {
		return nil, err
	}
	// The overlay table is migrated even though nothing writes it: Ping
	// reads it, and a tool that opens the file to inspect it should find
	// the schema it expects, just empty.
	if err := db.WithContext(ctx).AutoMigrate(&agentEventRow{}); err != nil {
		_ = closeGormDB(db)
		return nil, fmt.Errorf("eventlog: AutoMigrate overlay: %w", err)
	}
	stores, err := openStateStores(ctx, db)
	if err != nil {
		_ = closeGormDB(db)
		return nil, err
	}
	stream := &transcriptStream{t: t, retryInterval: o.watchInterval, done: make(chan struct{})}
	svc := &service{
		inner:             t.Service(),
		guardrail:         stores.guardrail,
		spend:             stores.spend,
		decisions:         stores.decisions,
		metadataExtractor: o.metadataExtractor,
	}
	h := &Handle{
		Stream:                stream,
		Service:               svc,
		Guardrail:             stores.guardrail,
		Spend:                 stores.spend,
		Decisions:             stores.decisions,
		db:                    db,
		DB:                    db,
		lockHeartbeatInterval: o.lockHeartbeatInterval,
		lockStaleAfter:        o.lockStaleAfter,
	}
	if o.residencyLease {
		var invalidate func(string, string, string)
		if inv, ok := t.(TranscriptInvalidator); ok {
			invalidate = inv.Invalidate
		}
		h.residency = newResidency(h, o.residencyLost, invalidate)
		svc.residency = h.residency
	}
	return h, nil
}

// transcriptStream adapts a Transcript to the Stream interface for a
// Handle built by OpenTranscript.
type transcriptStream struct {
	t Transcript
	// retryInterval is how long Watch waits after a failed catch-up
	// pass before reading again when no wake-up arrives first. It is
	// Open's watch interval: on this path it is a retry backoff, not a
	// poll.
	retryInterval time.Duration

	closed atomic.Bool
	// done is closed by Close so a Watch parked on a wake-up returns.
	done chan struct{}
}

func (s *transcriptStream) Append(context.Context, session.Session, *session.Event) (int64, error) {
	if s.closed.Load() {
		return 0, ErrClosed
	}
	return 0, ErrAppendViaService
}

func (s *transcriptStream) Since(ctx context.Context, fromSeq int64, opts ...QueryOption) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		if s.closed.Load() {
			yield(Entry{}, ErrClosed)
			return
		}
		for e, err := range s.t.Since(ctx, fromSeq, opts...) {
			if !yield(e, err) {
				return
			}
		}
	}
}

// Watch subscribes first and replays second, so an event appended
// between the end of the replay and the wait is already a pending
// wake-up rather than a lost one. Each wake-up is answered with a Since
// from the cursor, which is also how a reader that fell behind catches
// up: the wake-up carries no data, so there is nothing to lag on.
//
// Two details keep it equivalent to the SQL poller. A pass that moved
// the cursor is followed by another pass at once, so a WithLimit that
// cut a backlog short drains the rest without waiting for an append.
// A pass that failed is retried after retryInterval even if no wake-up
// arrives, so a transient read error does not park the reader until
// the next event.
func (s *transcriptStream) Watch(ctx context.Context, fromSeq int64, opts ...QueryOption) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		if s.closed.Load() {
			yield(Entry{}, ErrClosed)
			return
		}
		app, user, root, ok := ResolveQuery(opts...).Scope()
		if !ok {
			yield(Entry{}, fmt.Errorf("eventlog: Watch: %w", ErrUnscopedQuery))
			return
		}
		wake, cancel := s.t.Subscribe(app, user, root)
		defer cancel()
		cursor := fromSeq
		for {
			if s.closed.Load() {
				yield(Entry{}, ErrClosed)
				return
			}
			if ctx.Err() != nil {
				return
			}
			advanced, failed := false, false
			for e, err := range s.t.Since(ctx, cursor, opts...) {
				if e.Seq > cursor {
					cursor = e.Seq
					advanced = true
				}
				if err != nil {
					failed = true
				}
				if !yield(e, err) {
					return
				}
			}
			if advanced {
				continue
			}
			var retry <-chan time.Time
			if failed && s.retryInterval > 0 {
				retry = time.After(s.retryInterval)
			}
			select {
			case <-ctx.Done():
				return
			case <-s.done:
			case <-wake:
			case <-retry:
			}
		}
	}
}

func (s *transcriptStream) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	if s.done != nil {
		close(s.done)
	}
	return s.t.Close()
}

// OpenSQLTranscript opens this package's own SQL-backed Transcript
// against dialector: ADK's database session service plus the
// agent_eventlog overlay, exactly what Open builds, without the control
// and state stores a Handle adds. It exists so the SQL backend can run
// the conformance suite and be composed like any other backend; the
// shipped CLI keeps using Open, whose Handle reads the same tables by
// polling so that a second process watching the file still sees new
// rows.
func OpenSQLTranscript(ctx context.Context, dialector gorm.Dialector, opts ...Option) (Transcript, error) {
	o := defaultOpenOpts()
	for _, opt := range opts {
		opt(&o)
	}
	core, err := openSQLCore(ctx, dialector, o)
	if err != nil {
		return nil, err
	}
	return &sqlTranscript{
		core: core,
		svc:  &service{inner: core.adkSvc, stream: core.stream},
	}, nil
}

// sqlTranscript is the SQL backend behind the Transcript interface.
type sqlTranscript struct {
	core *sqlCore
	svc  *service

	closeMu sync.Mutex
}

func (t *sqlTranscript) Service() session.Service { return t.svc }

func (t *sqlTranscript) Since(ctx context.Context, fromSeq int64, opts ...QueryOption) iter.Seq2[Entry, error] {
	return t.core.stream.Since(ctx, fromSeq, opts...)
}

func (t *sqlTranscript) HighestSeq(ctx context.Context, appName, userID, root string) (int64, error) {
	return t.core.stream.LatestSeq(ctx, WithSessionTree(appName, userID, root))
}

func (t *sqlTranscript) Subscribe(appName, userID, root string) (<-chan struct{}, func()) {
	return t.core.stream.subs.Subscribe(appName, userID, root)
}

// Close is safe to call more than once, including concurrently: a
// Handle closes the Transcript it was given, and the caller may too.
func (t *sqlTranscript) Close() error {
	t.closeMu.Lock()
	defer t.closeMu.Unlock()
	var firstErr error
	if err := t.core.stream.Close(); err != nil {
		firstErr = err
	}
	if t.core.db != nil {
		if err := closeGormDB(t.core.db); err != nil && firstErr == nil {
			firstErr = err
		}
		t.core.db = nil
	}
	return firstErr
}
