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
const (
	defaultHeartbeatInterval = 5 * time.Second
	defaultStaleAfter        = 30 * time.Second
)

// ErrSessionLocked is returned by AcquireLock when another live
// process already holds the lease. The error message includes the
// holder identifier so operators can diagnose contention.
var ErrSessionLocked = errors.New("eventlog: session is locked by another process")

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
	staleAfter        time.Duration

	mu       sync.Mutex
	released bool
	stop     chan struct{}
	done     chan struct{}
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
		if time.Since(existing.HeartbeatAt) <= defaultStaleAfter {
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
		heartbeatInterval: defaultHeartbeatInterval,
		staleAfter:        defaultStaleAfter,
		stop:              make(chan struct{}),
		done:              make(chan struct{}),
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

	// Conditional delete: only remove the row if we still hold it
	// (i.e. nobody stole it after a stale-window elapsed during a
	// long pause). Avoids accidentally deleting a successor's
	// lease.
	res := l.db.
		Where("app_name = ? AND user_id = ? AND session_id = ? AND holder = ?",
			l.app, l.user, l.session, l.holder).
		Delete(&agentRunLockRow{})
	if res.Error != nil {
		return fmt.Errorf("eventlog: SessionLock.Release: %w", res.Error)
	}
	return nil
}

// Holder returns the identifier we registered when acquiring the
// lock. Useful for diagnostics + for tests that need to assert the
// row content.
func (l *SessionLock) Holder() string { return l.holder }

func (l *SessionLock) heartbeatLoop() {
	defer close(l.done)
	ticker := time.NewTicker(l.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			now := time.Now()
			// Conditional UPDATE — only refresh if we still own
			// the row. If another process stole the lease while
			// we paused, the WHERE clause matches zero rows and
			// we silently degrade until Release.
			_ = l.db.
				Model(&agentRunLockRow{}).
				Where("app_name = ? AND user_id = ? AND session_id = ? AND holder = ?",
					l.app, l.user, l.session, l.holder).
				Update("heartbeat_at", now)
		}
	}
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
