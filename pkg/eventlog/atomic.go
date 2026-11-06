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

// One event, one transaction (#976).
//
// AppendEvent used to be two independent writes: ADK persisted its
// events row and then we inserted the overlay row that gives the event
// its monotonic seq. A crash, OOM kill or node eviction landing between
// them left an event that ADK has and the overlay does not — and the
// overlay is the index every eventlog consumer reads. Since and Watch
// query overlay rows; an event with no row is not late, it is absent,
// permanently, from live tail, attach replay and every transcript built
// on top of them. The window is small and the workload is a daemon
// running for hours on a cluster that evicts pods, which is precisely
// the case `--session-db` exists to survive.
//
// The two writes cannot simply be wrapped from outside: ADK's
// databaseService exposes no transaction hook, and its AppendEvent
// opens and commits its own transaction. (When this shipped, ADK also
// opened its own connection pool; it now shares ours via
// NewSessionServiceFromDB, which is what lets the callback below be
// registered on a handle we own rather than one reached by reflection.)
//
// So we do not try to join ADK's transaction from outside — we get
// invited in. ADK's applyEvent runs `s.db.WithContext(ctx).Transaction`,
// and inside it `tx.Create(storageEv)`. GORM runs registered create
// callbacks on that same `tx`, so a callback registered on the shared
// *gorm.DB fires with the transaction in hand. The overlay row is
// inserted there, inside ADK's transaction: the two rows now commit
// together or not at all, and an error from the overlay insert aborts
// the event write rather than orphaning it.
//
// Three things make this less clever than it sounds:
//
//   - The only ADK internal it depends on is the table name "events".
//     Everything else the row needs travels down from AppendEvent
//     through the context, because ADK passes the caller's ctx into
//     Transaction and GORM hangs it on tx.Statement.Context. No
//     reflection over ADK's row struct, no assumption about its
//     columns.
//   - It degrades rather than fails. If GORM ever refuses the callback
//     registration, AppendEvent falls back to the two-write path it
//     has always used. A missing atomicity guarantee is the old
//     behaviour, and the old behaviour is not a crash.
//   - It cannot recurse. The callback fires for every create on the
//     shared pool — ADK's rows, our own overlay and state-store
//     inserts, and the overlay insert it makes itself; the table
//     filter stops everything but the ADK events row, and the pending
//     record is consumed once.
//
// Not in scope here: overlay rows orphaned by a crash *before* this
// change. Backfilling them at Open would hand each a fresh seq at the
// end of the log, so a replay would deliver a pre-crash event after
// every event written since — ordering damage in exchange for presence,
// on rows nobody can distinguish from a session ADK pruned. The fix
// stops new orphans; existing ones stay invisible, which is what they
// already are.

package eventlog

import (
	"context"

	"google.golang.org/adk/v2/session"
	"gorm.io/gorm"
)

// adkEventsTable is the table ADK's storageEvent declares via TableName.
// The one ADK internal this file depends on; asserted in the tests so a
// dependency bump that renames it fails there rather than silently
// turning the atomic write back off.
const adkEventsTable = "events"

// overlayCallbackName is the GORM callback registration key. Namespaced
// so it cannot collide with ADK's own callbacks or a host's.
const overlayCallbackName = "core-agent:eventlog:overlay"

// overlayPending is the hand-off from service.AppendEvent to the
// in-transaction callback: what to write on the way down, what happened
// on the way back up.
//
// Not concurrency-safe, and it does not need to be. It is created per
// AppendEvent call, and AppendEvent holds the service's write mutex for
// its whole duration, so exactly one goroutine ever touches a given
// instance.
type overlayPending struct {
	sess session.Session
	ev   *session.Event

	seq  int64
	done bool
	err  error
}

type overlayPendingKey struct{}

// withOverlayPending attaches p to ctx so the callback can find it once
// ADK carries the context into its transaction.
func withOverlayPending(ctx context.Context, p *overlayPending) context.Context {
	return context.WithValue(ctx, overlayPendingKey{}, p)
}

// overlayPendingFrom recovers the record, or nil when this create is not
// one of ours — every write ADK makes outside AppendEvent lands here
// too, and must be left alone.
func overlayPendingFrom(ctx context.Context) *overlayPending {
	if ctx == nil {
		return nil
	}
	p, _ := ctx.Value(overlayPendingKey{}).(*overlayPending)
	return p
}

// registerOverlayCallback wires the after-create hook onto the shared
// *gorm.DB that ADK's AppendEvent transaction runs on. Returns false
// when there is no DB to register on or GORM rejects the registration
// — the caller then keeps the two-write path.
func (s *gormStream) registerOverlayCallback(db *gorm.DB) bool {
	if db == nil {
		return false
	}
	if err := db.Callback().Create().After("gorm:create").
		Register(overlayCallbackName, s.overlayAfterCreate); err != nil {
		return false
	}
	return true
}

// overlayAfterCreate inserts the overlay row inside the transaction that
// just wrote the ADK events row.
//
// An error here is added to the statement, which is what makes the two
// rows atomic in both directions: ADK checks tx.Create(...).Error,
// returns it from the transaction function, and GORM rolls back. An
// event whose overlay row cannot be written is not persisted at all,
// which is the correct trade — the event is recoverable from the model
// on a retry, while an invisible event is not recoverable by anyone.
func (s *gormStream) overlayAfterCreate(tx *gorm.DB) {
	if tx == nil || tx.Statement == nil || tx.Error != nil {
		return
	}
	if tx.Statement.Table != adkEventsTable {
		return
	}
	p := overlayPendingFrom(tx.Statement.Context)
	if p == nil || p.done {
		return
	}
	row, err := s.buildOverlayRow(tx.Statement.Context, p.sess, p.ev)
	if err != nil {
		p.err = err
		_ = tx.AddError(err)
		return
	}
	// NewDB gives the insert a clean statement while keeping the
	// transaction's connection, so it commits with the event row. The
	// table filter above is what stops this create from re-entering the
	// callback.
	if err := tx.Session(&gorm.Session{NewDB: true}).Create(row).Error; err != nil {
		p.err = err
		_ = tx.AddError(err)
		return
	}
	if err := applyGuardrailEventOnDB(tx.Session(&gorm.Session{NewDB: true}), p.sess, p.ev); err != nil {
		p.err = err
		_ = tx.AddError(err)
		return
	}
	p.seq = row.Seq
	p.done = true
}
