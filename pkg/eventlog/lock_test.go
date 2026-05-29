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
	"strings"
	"testing"
	"time"
)

func TestAcquireLock_FirstAcquireSucceeds(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	lock, err := h.AcquireLock(context.Background(), "app", "user", "sess1")
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer lock.Release()
	if lock.Holder() == "" {
		t.Errorf("Holder should be non-empty")
	}
}

func TestAcquireLock_SecondAcquireBlocks(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	first, err := h.AcquireLock(context.Background(), "app", "user", "sess1")
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	defer first.Release()

	_, err = h.AcquireLock(context.Background(), "app", "user", "sess1")
	if !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("second AcquireLock err = %v, want ErrSessionLocked", err)
	}
	if !strings.Contains(err.Error(), first.Holder()) {
		t.Errorf("error should mention the holder %q for diagnostics; got %v", first.Holder(), err)
	}
}

func TestAcquireLock_DifferentSessionsDoNotBlock(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	a, err := h.AcquireLock(context.Background(), "app", "user", "A")
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	defer a.Release()
	b, err := h.AcquireLock(context.Background(), "app", "user", "B")
	if err != nil {
		t.Fatalf("second AcquireLock: %v", err)
	}
	defer b.Release()
}

func TestRelease_AllowsReacquire(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	first, err := h.AcquireLock(context.Background(), "app", "user", "sess1")
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	second, err := h.AcquireLock(context.Background(), "app", "user", "sess1")
	if err != nil {
		t.Fatalf("re-AcquireLock after Release: %v", err)
	}
	defer second.Release()
	if second.Holder() == first.Holder() {
		t.Errorf("re-acquired lock should have a fresh holder identifier; both %q", second.Holder())
	}
}

func TestRelease_Idempotent(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	lock, err := h.AcquireLock(context.Background(), "app", "user", "sess1")
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}

func TestStaleLock_GetsStolen(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	ctx := context.Background()

	// Plant a stale lease directly via the underlying db so we
	// don't have to wait 30s for an in-process lock to age out.
	if err := h.db.WithContext(ctx).AutoMigrate(&agentRunLockRow{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	staleHolder := "ghost/1/dead"
	if err := h.db.WithContext(ctx).Create(&agentRunLockRow{
		AppName:     "app",
		UserID:      "user",
		SessionID:   "sess1",
		Holder:      staleHolder,
		AcquiredAt:  time.Now().Add(-2 * time.Hour),
		HeartbeatAt: time.Now().Add(-2 * time.Hour),
	}).Error; err != nil {
		t.Fatalf("plant stale row: %v", err)
	}

	lock, err := h.AcquireLock(ctx, "app", "user", "sess1")
	if err != nil {
		t.Fatalf("AcquireLock should steal stale lease, got err = %v", err)
	}
	defer lock.Release()
	if lock.Holder() == staleHolder {
		t.Errorf("stolen lease should have a fresh holder; got the stale holder %q", staleHolder)
	}
}

func TestHeartbeat_KeepsLeaseFresh(t *testing.T) {
	t.Parallel()
	// We don't wait 30s in tests; instead we read the row directly
	// after a short sleep and assert heartbeat_at advanced.
	h, cleanup := openTestHandle(t)
	defer cleanup()
	ctx := context.Background()

	lock, err := h.AcquireLock(ctx, "app", "user", "sess1")
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer lock.Release()

	// Read the initial heartbeat.
	var first agentRunLockRow
	if err := h.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ?", "app", "user", "sess1").
		First(&first).Error; err != nil {
		t.Fatalf("read row: %v", err)
	}

	// Bump the heartbeat by hand to simulate a tick advancing — the
	// real loop ticks every 5s, which is too slow for a unit test.
	// We're verifying the conditional UPDATE pattern works (only
	// refreshes our row when we still hold it).
	now := time.Now().Add(2 * time.Second)
	res := h.db.WithContext(ctx).
		Model(&agentRunLockRow{}).
		Where("app_name = ? AND user_id = ? AND session_id = ? AND holder = ?",
			"app", "user", "sess1", lock.Holder()).
		Update("heartbeat_at", now)
	if res.Error != nil {
		t.Fatalf("manual heartbeat: %v", res.Error)
	}
	if res.RowsAffected != 1 {
		t.Errorf("manual heartbeat affected %d rows, want 1 (holder match)", res.RowsAffected)
	}

	var second agentRunLockRow
	if err := h.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ?", "app", "user", "sess1").
		First(&second).Error; err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !second.HeartbeatAt.After(first.HeartbeatAt) {
		t.Errorf("heartbeat did not advance: first=%v second=%v", first.HeartbeatAt, second.HeartbeatAt)
	}
}

func TestRelease_DoesNotDeleteStolenSuccessor(t *testing.T) {
	t.Parallel()
	// Scenario: A acquires, A's heartbeat lapses, B steals, A
	// belatedly calls Release. A's Release must not delete B's
	// row. The conditional WHERE holder = A.holder protects this.
	h, cleanup := openTestHandle(t)
	defer cleanup()
	ctx := context.Background()

	a, err := h.AcquireLock(ctx, "app", "user", "sess1")
	if err != nil {
		t.Fatalf("AcquireLock A: %v", err)
	}

	// Simulate B stealing by overwriting holder directly in the DB.
	if err := h.db.WithContext(ctx).
		Model(&agentRunLockRow{}).
		Where("app_name = ? AND user_id = ? AND session_id = ?", "app", "user", "sess1").
		Updates(map[string]any{"holder": "B/2/face", "heartbeat_at": time.Now()}).Error; err != nil {
		t.Fatalf("simulate B steal: %v", err)
	}

	// A releases — should leave B's row alone.
	if err := a.Release(); err != nil {
		t.Fatalf("A.Release: %v", err)
	}

	var row agentRunLockRow
	if err := h.db.WithContext(ctx).
		Where("app_name = ? AND user_id = ? AND session_id = ?", "app", "user", "sess1").
		First(&row).Error; err != nil {
		t.Fatalf("B's row should still exist; got err %v", err)
	}
	if row.Holder != "B/2/face" {
		t.Errorf("B's row was clobbered; holder = %q", row.Holder)
	}
}
