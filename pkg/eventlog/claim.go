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

// Durable continuation claims (#977).
//
// agent_run_lock stops two daemons from *deciding* to continue a session
// at the same instant. It does not stop them from deciding one after the
// other, because the lock is released before the continuation turn runs
// — the turn executes asynchronously in the session's wake loop and that
// path has no turn-end hook to hold a lease across. So the second daemon
// acquires the lock cleanly, reads a tail that still looks interrupted
// (the first daemon's turn has committed nothing yet), and injects a
// second continuation into the same session.
//
// The window the lock leaves open is therefore not "two turns overlap".
// It is "two daemons read the same interrupted tail and both act on it",
// and that is what a claim closes: a durable row saying *this specific
// interruption has already been continued by somebody*. It needs no new
// lease semantics and no hook that does not exist, because it is not a
// lock — it is a fact, and it stays true after the writer has gone away.
//
// Holding agent_run_lock across the turn instead would read like
// session-level mutual exclusion without being it. Ordinary turns take
// no lock at all — not attach-driven turns, not wake-loop turns, not the
// local REPL — so a lock held across the continuation excludes another
// continuation and nothing else. A reader would infer an invariant the
// code does not have, which is worse than the narrow scope being visible.
package eventlog

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

// continuationClaimTTL bounds how long a claim suppresses a re-attempt
// at the same interruption.
//
// A claim with no expiry is a deadlock with extra steps. The continuation
// note goes onto an IN-MEMORY inbox and only becomes a committed event
// when the wake loop drains it; a daemon killed in that gap loses the
// note, leaves the tail exactly as it was, and — with a permanent claim —
// would never continue that session again, because nothing will ever
// produce a newer interruption for a session nobody is talking to. That
// is precisely the multi-hour-unattended failure this work exists to
// prevent, so the claim has to age out.
//
// Ten minutes is generous against what it has to cover: the gap between
// injecting the note and that note being committed as an event, which is
// a wake plus a model round trip. Once the note commits, the tail has
// moved and the claim is moot regardless. And it costs nothing on the
// boot-scan path, where breakerWindow already skips a session attempted
// inside the same ten minutes.
const continuationClaimTTL = 10 * time.Minute

// continuationClaimRow is one session's most recently claimed
// interruption. One row per (app, user, session), overwritten in place
// as the session is interrupted again — the same cardinality as the ACL
// table, not one row per event.
//
// Both instants are stored as Unix nanoseconds rather than time.Time
// because both are compared in SQL, inside the conditional upsert below,
// and an integer comparison means the same thing on every driver. Two
// daemons classifying the same tail derive InterruptedAtUnixNano from
// the same committed event, so they derive the same number.
type continuationClaimRow struct {
	AppName               string `gorm:"primaryKey"`
	UserID                string `gorm:"primaryKey"`
	SessionID             string `gorm:"primaryKey"`
	InterruptedAtUnixNano int64
	ClaimedAtUnixNano     int64
	Holder                string
}

// TableName pins the table name independent of GORM's pluralization.
func (continuationClaimRow) TableName() string { return "agent_continuation_claim" }

// ClaimContinuation records that the caller is about to inject a
// continuation for the interruption at interruptedAt, and reports
// whether the claim is the caller's to act on.
//
// false means the interruption at that exact instant has already been
// claimed — by this daemon on an earlier pass, or by a peer sharing the
// database — and the caller must stand down rather than inject a second
// continuation for the same interruption. true means the row now names
// the caller.
//
// A *later* interruption is a different fact and always claimable: the
// row carries one instant, not a flag, so a session interrupted again
// after a continuation has run gets continued again. That is the whole
// point of keying on the interruption rather than on the session. A
// claim older than continuationClaimTTL is also re-claimable; see that
// constant for why an eternal claim would be a deadlock.
//
// Atomic in one statement rather than read-then-write. Every in-tree
// caller already holds the session run lock, so the compare-and-set is
// belt and braces — but a claim whose correctness depends on a lock
// taken somewhere else is exactly the kind of invariant that quietly
// stops being true, and one conditional upsert costs nothing to make
// honest.
func (h *Handle) ClaimContinuation(ctx context.Context, app, user, session string, interruptedAt time.Time, holder string) (bool, error) {
	return h.claimContinuationAt(ctx, app, user, session, interruptedAt, holder, time.Now())
}

// claimContinuationAt is ClaimContinuation with the clock injected, so a
// test can age a claim past continuationClaimTTL without sleeping for
// ten minutes.
func (h *Handle) claimContinuationAt(ctx context.Context, app, user, session string, interruptedAt time.Time, holder string, now time.Time) (bool, error) {
	if h == nil || h.DB == nil {
		return false, fmt.Errorf("eventlog: ClaimContinuation: no database")
	}
	if err := h.DB.WithContext(ctx).AutoMigrate(&continuationClaimRow{}); err != nil {
		return false, fmt.Errorf("eventlog: migrate agent_continuation_claim: %w", err)
	}
	nanos := interruptedAt.UnixNano()
	staleBefore := now.Add(-continuationClaimTTL).UnixNano()
	row := continuationClaimRow{
		AppName:               app,
		UserID:                user,
		SessionID:             session,
		InterruptedAtUnixNano: nanos,
		ClaimedAtUnixNano:     now.UnixNano(),
		Holder:                holder,
	}
	// INSERT, and on a pre-existing row for this session UPDATE it only
	// if it names a DIFFERENT interruption or has aged out. A live row
	// already naming this one leaves RowsAffected at zero, which is the
	// stand-down signal.
	const table = "agent_continuation_claim"
	res := h.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "app_name"}, {Name: "user_id"}, {Name: "session_id"}},
		Where: clause.Where{Exprs: []clause.Expression{
			clause.Neq{Column: clause.Column{Table: table, Name: "interrupted_at_unix_nano"}, Value: nanos},
			clause.Or(clause.Lt{Column: clause.Column{Table: table, Name: "claimed_at_unix_nano"}, Value: staleBefore}),
		}},
		DoUpdates: clause.AssignmentColumns([]string{"interrupted_at_unix_nano", "claimed_at_unix_nano", "holder"}),
	}).Create(&row)
	if res.Error != nil {
		return false, fmt.Errorf("eventlog: claim continuation: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// ReleaseContinuationClaim gives back a claim whose injection did not
// happen, so the interruption can be claimed again immediately — by the
// retry driver on its next tick, or by a peer daemon — rather than after
// continuationClaimTTL.
//
// Conditional on the claim still naming this interruption AND this
// holder, so a caller unwinding late cannot delete a successor's claim
// for a newer interruption.
//
// Note the asymmetry with the claim itself: a claim that is never
// released is a continuation delayed by the TTL, and a release that
// never runs is the same. Both failure directions cost latency, and
// neither produces the duplicate this file exists to prevent — which is
// why callers log a failed release rather than retrying it.
func (h *Handle) ReleaseContinuationClaim(ctx context.Context, app, user, session string, interruptedAt time.Time, holder string) error {
	if h == nil || h.DB == nil {
		return fmt.Errorf("eventlog: ReleaseContinuationClaim: no database")
	}
	res := h.DB.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ? AND interrupted_at_unix_nano = ? AND holder = ?",
			app, user, session, interruptedAt.UnixNano(), holder).
		Delete(&continuationClaimRow{})
	if res.Error != nil {
		return fmt.Errorf("eventlog: release continuation claim: %w", res.Error)
	}
	return nil
}
