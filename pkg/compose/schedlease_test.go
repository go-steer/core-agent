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

// Originally derived from go-steer/mast@d239f89 (internal/cli/schedlease_test.go).

package compose

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

func openSchedLeaseHandle(t *testing.T, opts ...eventlog.Option) *eventlog.Handle {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(path), opts...)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func capturingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func acquireNow(ctx context.Context, h *eventlog.Handle, appName string, logger *slog.Logger) (*SchedulingLease, bool) {
	return acquireSchedulingLeaseWithin(ctx, h, appName, logger, 0, time.Millisecond)
}

func TestAcquireSchedulingLease_SecondInstanceIsPassive(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	ctx := context.Background()

	firstLog, firstBuf := capturingLogger()
	first, firstActive := AcquireSchedulingLease(ctx, h, "nightly", firstLog)
	defer first.Release()
	if !firstActive || first == nil {
		t.Fatalf("the first instance must drive the work; log was %s", firstBuf.String())
	}

	secondLog, secondBuf := capturingLogger()
	second, secondActive := acquireNow(ctx, h, "nightly", secondLog)
	defer second.Release()
	if secondActive || second != nil {
		t.Fatalf("the second instance must be passive; log was %s", secondBuf.String())
	}

	got := secondBuf.String()
	if !strings.Contains(got, "level=ERROR") {
		t.Errorf("the refusal should be ERROR, not a line that scrolls past; got %s", got)
	}
	for _, want := range []string{"scheduled trigger", "timed-pause resume", "auto-resume"} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not mention %q; got %s", want, got)
		}
	}
}

func TestAcquireSchedulingLease_ReleaseHandsTheRoleOver(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	ctx := context.Background()
	logger, _ := capturingLogger()

	first, firstActive := AcquireSchedulingLease(ctx, h, "nightly", logger)
	if !firstActive {
		t.Fatal("first instance should be active")
	}
	first.Release()

	second, secondActive := AcquireSchedulingLease(ctx, h, "nightly", logger)
	defer second.Release()
	if !secondActive {
		t.Error("after a clean release the successor should take the role immediately")
	}
}

func TestAcquireSchedulingLease_DifferentWorkloadsBothDrive(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	ctx := context.Background()
	logger, _ := capturingLogger()

	a, aActive := AcquireSchedulingLease(ctx, h, "nightly", logger)
	defer a.Release()
	b, bActive := AcquireSchedulingLease(ctx, h, "hourly", logger)
	defer b.Release()
	if !aActive || !bActive {
		t.Errorf("different workloads should both drive their own schedules; nightly=%v hourly=%v", aActive, bActive)
	}
}

func TestAcquireSchedulingLease_NoStoreStillDrivesAndWarns(t *testing.T) {
	t.Parallel()
	logger, buf := capturingLogger()

	lease, active := AcquireSchedulingLease(context.Background(), nil, "nightly", logger)
	defer lease.Release()
	if !active {
		t.Fatal("without a session store the daemon must still fire its own schedule")
	}
	got := buf.String()
	if !strings.Contains(got, "--session-db") {
		t.Errorf("the warning should name the flag that would fix it; got %s", got)
	}
}

func TestAcquireSchedulingLease_AStoreThatCannotAnswerFailsOpen(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	sqlDB, err := h.DB.DB()
	if err != nil {
		t.Fatalf("h.DB.DB(): %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close the pool out from under gorm: %v", err)
	}

	logger, buf := capturingLogger()
	lease, active := AcquireSchedulingLease(context.Background(), h, "nightly", logger)
	defer lease.Release()
	if !active {
		t.Fatalf("a broken store must not stop scheduled work; log was %s", buf.String())
	}
	got := buf.String()
	if !strings.Contains(got, "level=ERROR") {
		t.Errorf("failing open is a decision worth an ERROR; got %s", got)
	}
	if !strings.Contains(got, "duplicates") {
		t.Errorf("the line should say what the fail-open risks; got %s", got)
	}
}

func plantAbandonedLease(t *testing.T, h *eventlog.Handle, appName string, staleFor time.Duration) {
	t.Helper()
	logger, _ := capturingLogger()
	real, active := AcquireSchedulingLease(context.Background(), h, appName, logger)
	if !active {
		t.Fatal("setup: could not take the lease to learn its key")
	}
	var key struct {
		AppName   string
		UserID    string
		SessionID string
	}
	if err := h.DB.Table("agent_run_lock").Select("app_name", "user_id", "session_id").Take(&key).Error; err != nil {
		t.Fatalf("setup: read the planted key: %v", err)
	}
	real.Release()
	now := time.Now().UTC()
	err := h.DB.Exec(
		"INSERT INTO agent_run_lock (app_name, user_id, session_id, holder, acquired_at, heartbeat_at) VALUES (?, ?, ?, ?, ?, ?)",
		key.AppName, key.UserID, key.SessionID, "killed-instance/1/deadbeef", now.Add(-time.Hour), now.Add(-staleFor),
	).Error
	if err != nil {
		t.Fatalf("setup: plant the abandoned lease: %v", err)
	}
}

func TestAcquireSchedulingLease_BootAfterAKillTakesTheAbandonedLease(t *testing.T) {
	t.Parallel()
	staleWindow := 1 * time.Second
	h := openSchedLeaseHandle(t, eventlog.WithLockHeartbeat(200*time.Millisecond, staleWindow))
	plantAbandonedLease(t, h, "nightly", staleWindow-300*time.Millisecond)

	refusedLog, refusedBuf := capturingLogger()
	_, refusedActive := acquireNow(context.Background(), h, "nightly", refusedLog)
	if refusedActive {
		t.Fatalf("setup is not testing anything: the planted lease was already stale; log was %s", refusedBuf.String())
	}

	logger, buf := capturingLogger()
	start := time.Now()
	lease, active := acquireSchedulingLeaseWithin(context.Background(), h, "nightly", logger,
		staleWindow+2*time.Second, 50*time.Millisecond)
	defer lease.Release()

	if !active {
		t.Fatalf("an instance booting after a kill must take the abandoned lease; waited %s, log was %s",
			time.Since(start).Round(time.Millisecond), buf.String())
	}
	got := buf.String()
	if !strings.Contains(got, "waiting in case its holder is gone") {
		t.Errorf("the wait should say why boot paused; got %s", got)
	}
	if !strings.Contains(got, "stopped heartbeating") {
		t.Errorf("taking over should say it took over; got %s", got)
	}
}

func TestAcquireSchedulingLease_ALiveHolderIsStillRefusedAfterTheWindow(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	ctx := context.Background()
	logger, _ := capturingLogger()

	first, _ := AcquireSchedulingLease(ctx, h, "nightly", logger)
	defer first.Release()

	secondLog, secondBuf := capturingLogger()
	start := time.Now()
	second, secondActive := acquireSchedulingLeaseWithin(ctx, h, "nightly", secondLog, 400*time.Millisecond, 50*time.Millisecond)
	defer second.Release()

	if secondActive {
		t.Fatalf("a live holder must keep the role; log was %s", secondBuf.String())
	}
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Errorf("gave up after %s without waiting out the window", waited)
	}
	if got := secondBuf.String(); !strings.Contains(got, "level=ERROR") {
		t.Errorf("the eventual refusal is still an ERROR; got %s", got)
	}
}

func TestAcquireSchedulingLease_RetryStopsWhenBootIsCancelled(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	logger, _ := capturingLogger()

	first, _ := AcquireSchedulingLease(context.Background(), h, "nightly", logger)
	defer first.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	second, secondActive := acquireSchedulingLeaseWithin(ctx, h, "nightly", logger, time.Minute, 50*time.Millisecond)
	defer second.Release()
	if secondActive {
		t.Error("a cancelled boot should not conclude it drives the work")
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("a cancelled boot waited %s for the retry window", waited)
	}
}

func TestSchedulingLeaseRetryWindowOutlastsDefaultStaleness(t *testing.T) {
	t.Parallel()
	if SchedulingLeaseRetryWindow <= 30*time.Second {
		t.Errorf("SchedulingLeaseRetryWindow %s does not outlast the 30s default staleness window", SchedulingLeaseRetryWindow)
	}
}

func TestSchedulingLease_NeverHeldNeverReportsLoss(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	ctx := context.Background()
	logger, _ := capturingLogger()

	first, _ := AcquireSchedulingLease(ctx, h, "nightly", logger)
	defer first.Release()
	second, _ := acquireNow(ctx, h, "nightly", logger)
	defer second.Release()

	select {
	case <-second.Lost():
		t.Error("a passive replica reported losing a lease it never held")
	default:
	}

	nilStoreLease, _ := AcquireSchedulingLease(ctx, nil, "nightly", logger)
	select {
	case <-nilStoreLease.Lost():
		t.Error("a daemon with no store reported losing a lease that does not exist")
	default:
	}
}

func TestSchedulingLease_NilIsUsable(t *testing.T) {
	t.Parallel()
	var s *SchedulingLease
	s.Release()
	if s.Holder() != "" {
		t.Errorf("nil lease Holder() = %q, want empty", s.Holder())
	}
	select {
	case <-s.Lost():
		t.Error("a nil lease reported loss")
	default:
	}
}
