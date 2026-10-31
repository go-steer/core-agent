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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"gorm.io/gorm"
)

// agentRunLockRow is the persistent lease that prevents two
// processes from simultaneously running RunAutonomous /
// ResumeAutonomous against the same (app, user, session). One row
// per locked session; presence == lease held; staleness is detected
// via heartbeat_at.
type agentRunLockRow struct {
	AppName     string `gorm:"primaryKey"`
	UserID      string `gorm:"primaryKey"`
	SessionID   string `gorm:"primaryKey"`
	Holder      string
	AcquiredAt  time.Time
	HeartbeatAt time.Time
}

// TableName pins the table name independent of GORM's pluralization.
func (agentRunLockRow) TableName() string { return "agent_run_lock" }

// Lock-tuning constants. Heartbeat is best-effort: a 5s interval
// against a 30s staleness window leaves plenty of headroom for
// short hiccups (DB pause, GC stall) without false-sharing the
// lock.
//
// defaultHeartbeatTimeout bounds each heartbeat UPDATE so a wedged
// database can't hang the heartbeat goroutine (and, via Release's
// wait on the goroutine, Release itself) indefinitely. It comfortably
// exceeds SQLite's 5s busy_timeout so a heartbeat isn't spuriously
// cancelled while merely waiting on the write lock.
const (
	defaultHeartbeatInterval = 5 * time.Second
	defaultStaleAfter        = 30 * time.Second
	defaultHeartbeatTimeout  = 10 * time.Second
)

// ErrSessionLocked is returned by AcquireLock when another live
// process already holds the lease. The error message includes the
// holder identifier so operators can diagnose contention.
var ErrSessionLocked = errors.New("eventlog: session is locked by another process")

// ErrLeaseLost is returned by consumers of SessionLock.Lost (such as
// autonomous.Resume) when a held session lease is stolen by another
// process or its heartbeat fails past the staleness window (#1361).
var ErrLeaseLost = errors.New("eventlog: session lease lost")

// SessionLock is the lease returned by Handle.AcquireLock. It runs a
// background goroutine that refreshes heartbeat_at every
// heartbeatInterval until Release is called. Safe to call Release
// multiple times.
type SessionLock struct {
	db        *gorm.DB
	app, user string
	session   string
	holder    string

	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration
	staleAfter        time.Duration
	lastHeartbeat     time.Time

	mu       sync.Mutex
	released bool
	stop     chan struct{}
	done     chan struct{}
	// lost is closed by the heartbeat loop if it discovers the lease
	// was stolen out from under us (its conditional UPDATE matched zero
	// rows) or if heartbeats have failed for at least lossWindow().
	// Callers running work under the lock select on Lost() to abort
	// before committing split-brain writes.
	lost chan struct{}
	// stolen records that lost was closed because the conditional
	// UPDATE matched zero rows — the row is now someone else's — as
	// opposed to a heartbeat stall. Written by the heartbeat goroutine
	// before it closes done; read by Release after <-done.
	stolen bool
}

// lossWindow is how long heartbeats may fail before the holder declares
// its own lease lost. It is one heartbeatInterval SHORTER than
// staleAfter, the point at which AcquireLock lets another process steal
// the row: a fast-failing database is noticed only on the next tick, so
// without the margin a holder could believe it owned the lease for up to
// one interval after a successor had taken it (#1361 review). Floored at
// staleAfter/2 so a configuration with interval >= staleAfter cannot
// drive the window to zero.
func (l *SessionLock) lossWindow() time.Duration {
	if l.staleAfter <= 0 {
		return 0
	}
	w := l.staleAfter - l.heartbeatInterval
	if floor := l.staleAfter / 2; w < floor {
		w = floor
	}
	return w
}

func (h *Handle) lockIntervals() (interval, staleAfter time.Duration) {
	interval = defaultHeartbeatInterval
	if h.lockHeartbeatInterval > 0 {
		interval = h.lockHeartbeatInterval
	}
	staleAfter = defaultStaleAfter
	if h.lockStaleAfter > 0 {
		staleAfter = h.lockStaleAfter
	}
	return interval, staleAfter
}

// AcquireLock takes an exclusive lease on (app, user, session) for
// the lifetime of the returned *SessionLock. Returns ErrSessionLocked
// if another process holds a fresh lease (heartbeat within
// staleAfter); steals the lease if the existing holder's heartbeat
// is older than staleAfter (indicating a crashed process).
//
// The lease is heartbeated automatically until SessionLock.Release
// is called; Release is idempotent and safe to defer.
func (h *Handle) AcquireLock(ctx context.Context, app, user, session string) (*SessionLock, error) {
	if h == nil || h.db == nil {
		return nil, errors.New("eventlog: AcquireLock called on nil Handle")
	}
	if err := h.db.WithContext(ctx).AutoMigrate(&agentRunLockRow{}); err != nil {
		return nil, fmt.Errorf("eventlog: AutoMigrate agent_run_lock: %w", err)
	}
	heartbeatInterval, staleAfter := h.lockIntervals()
	holder := newHolderID()
	now := time.Now()
	row := &agentRunLockRow{
		AppName:     app,
		UserID:      user,
		SessionID:   session,
		Holder:      holder,
		AcquiredAt:  now,
		HeartbeatAt: now,
	}

	// Try to insert. If the row already exists, GORM surfaces the
	// unique-constraint violation; we then check for staleness.
	err := h.db.WithContext(ctx).Create(row).Error
	if err != nil {
		// Slow path: existing row. Check whether it's stale.
		var existing agentRunLockRow
		if lookupErr := h.db.WithContext(ctx).
			Where("app_name = ? AND user_id = ? AND session_id = ?", app, user, session).
			First(&existing).Error; lookupErr != nil {
			// Couldn't even read the row; surface the original
			// insert error.
			return nil, fmt.Errorf("eventlog: AcquireLock: %w", err)
		}
		if time.Since(existing.HeartbeatAt) <= staleAfter {
			return nil, fmt.Errorf("%w (held by %s, last heartbeat %s ago)",
				ErrSessionLocked, existing.Holder, time.Since(existing.HeartbeatAt).Round(time.Second))
		}
		// Steal: overwrite the stale lease with our identity in a
		// single UPDATE. Predicate keeps the operation safe under
		// concurrent stealers — only one update succeeds, the
		// other rebounds to the locked path on its next attempt.
		res := h.db.WithContext(ctx).
			Model(&agentRunLockRow{}).
			Where("app_name = ? AND user_id = ? AND session_id = ? AND holder = ?",
				app, user, session, existing.Holder).
			Updates(map[string]any{
				"holder":       holder,
				"acquired_at":  now,
				"heartbeat_at": now,
			})
		if res.Error != nil {
			return nil, fmt.Errorf("eventlog: AcquireLock: steal stale lease: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			// Someone else stole first.
			return nil, fmt.Errorf("%w (lost the steal race)", ErrSessionLocked)
		}
	}

	lock := &SessionLock{
		db:                h.db,
		app:               app,
		user:              user,
		session:           session,
		holder:            holder,
		heartbeatInterval: heartbeatInterval,
		heartbeatTimeout:  defaultHeartbeatTimeout,
		staleAfter:        staleAfter,
		lastHeartbeat:     now,
		stop:              make(chan struct{}),
		done:              make(chan struct{}),
		lost:              make(chan struct{}),
	}
	go lock.heartbeatLoop()
	return lock, nil
}

// Release ends the lease and stops the heartbeat goroutine.
// Idempotent; safe to defer.
func (l *SessionLock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return nil
	}
	l.released = true
	close(l.stop)
	l.mu.Unlock()

	// Wait for the heartbeat goroutine to exit before deleting the
	// row so we don't race a heartbeat against the delete.
	<-l.done

	// A stolen row is a successor's lease; the conditional predicate
	// below would match nothing, and the query is one more round trip
	// against a database that may be the reason we are here.
	if l.stolen {
		return nil
	}

	// Conditional delete: only remove the row if we still hold it
	// (i.e. nobody stole it after a stale-window elapsed during a
	// long pause). Avoids accidentally deleting a successor's
	// lease. Bounded: on the DB-stall path that closes Lost(), an
	// unbounded DELETE here would hang Release — and with it the
	// caller's exit-and-restart — against the very database that
	// stalled (#1361 review).
	ctx, cancel := context.WithTimeout(context.Background(), l.heartbeatTimeout)
	defer cancel()
	res := l.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ? AND holder = ?",
			l.app, l.user, l.session, l.holder).
		Delete(&agentRunLockRow{})
	if res.Error != nil {
		return fmt.Errorf("eventlog: SessionLock.Release: %w", res.Error)
	}
	return nil
}

// Reaffirm re-proves ownership with one conditional heartbeat UPDATE
// and reports whether this holder still owns the row. It exists for the
// moment after Lost() has closed because heartbeats STALLED (not because
// the row was stolen): in a single-replica deployment nobody took the
// lease during the outage, and a caller that can prove that may still
// write its final checkpoint. A stolen lease reports false without
// touching the database. The query is bounded by heartbeatTimeout.
func (l *SessionLock) Reaffirm(ctx context.Context) (bool, error) {
	if l == nil {
		return false, nil
	}
	l.mu.Lock()
	released := l.released
	l.mu.Unlock()
	if released {
		return false, nil
	}
	select {
	case <-l.done:
		if l.stolen {
			return false, nil
		}
	default:
	}
	ctx, cancel := context.WithTimeout(ctx, l.heartbeatTimeout)
	defer cancel()
	res := l.db.WithContext(ctx).
		Model(&agentRunLockRow{}).
		Where("app_name = ? AND user_id = ? AND session_id = ? AND holder = ?",
			l.app, l.user, l.session, l.holder).
		Update("heartbeat_at", time.Now())
	if res.Error != nil {
		return false, fmt.Errorf("eventlog: SessionLock.Reaffirm: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// Holder returns the identifier we registered when acquiring the
// lock. Useful for diagnostics + for tests that need to assert the
// row content.
func (l *SessionLock) Holder() string { return l.holder }

// Lost returns a channel that is closed if the lease is lost while it is
// held: either the heartbeat's conditional UPDATE matched zero rows —
// another process reclaimed the lease after our heartbeat lapsed past
// the staleness window (a long GC pause, sleep, or DB stall) — or
// heartbeats have failed to reach the database for the loss window
// (staleAfter minus one heartbeat interval), after which we can no
// longer prove exclusivity. A caller running work under the lock — e.g.
// the autonomous run loop — must select on this channel and abort
// promptly, otherwise both processes run against the same session: the
// exact split-brain the lock exists to prevent. Reaffirm tells the two
// causes apart after the fact. The channel is never closed for a lock
// that is cleanly Released while still held.
func (l *SessionLock) Lost() <-chan struct{} { return l.lost }

func (l *SessionLock) heartbeatLoop() {
	defer close(l.done)
	if l.lastHeartbeat.IsZero() {
		l.lastHeartbeat = time.Now()
	}
	ticker := time.NewTicker(l.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			lost, stolen := l.beat()
			if lost {
				// Lease lost. Signal any consumer running under the
				// lock so it can abort, then stop heartbeating —
				// continuing to refresh a row we no longer own is
				// pointless.
				l.stolen = stolen
				close(l.lost)
				return
			}
		}
	}
}

// beat refreshes heartbeat_at for the row we still own, bounded by
// beatTimeout() so a wedged database neither hangs the loop forever nor
// outlives the loss window (#1361). It returns lost=true when the lease
// is gone: stolen=true if the conditional UPDATE succeeded and matched
// zero rows (another process owns the row), stolen=false if heartbeats
// have failed for at least lossWindow() so exclusivity can no longer be
// guaranteed. A transient error inside the window returns false and
// retries on the next tick.
func (l *SessionLock) beat() (lost, stolen bool) {
	ctx, cancel := context.WithTimeout(context.Background(), l.beatTimeout())
	defer cancel()
	now := time.Now()
	res := l.db.WithContext(ctx).
		Model(&agentRunLockRow{}).
		Where("app_name = ? AND user_id = ? AND session_id = ? AND holder = ?",
			l.app, l.user, l.session, l.holder).
		Update("heartbeat_at", now)
	if res.Error != nil {
		w := l.lossWindow()
		return w > 0 && time.Since(l.lastHeartbeat) >= w, false
	}
	if res.RowsAffected == 0 {
		return true, true
	}
	l.lastHeartbeat = now
	return false, false
}

// beatTimeout bounds each heartbeat query by the lesser of
// heartbeatTimeout and the remaining loss window from lastHeartbeat so a
// hung database unblocks right at the loss boundary instead of lingering
// up to defaultHeartbeatTimeout (10s) after another process is already
// eligible to steal the lease (#1361).
func (l *SessionLock) beatTimeout() time.Duration {
	timeout := l.heartbeatTimeout
	window := l.lossWindow()
	if window <= 0 {
		return timeout
	}
	rem := window - time.Since(l.lastHeartbeat)
	if rem > 0 && rem < timeout {
		return rem
	}
	if rem <= 0 && window < timeout {
		return window
	}
	return timeout
}

// newHolderID builds a per-acquisition identifier string of the
// form "<host>/<pid>/<rand>" so logs and diagnostic messages can
// trace the holding process across the cluster.
func newHolderID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}
