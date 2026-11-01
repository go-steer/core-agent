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

// Originally derived from go-steer/mast@d239f89 (internal/cli/pauserescan_test.go).

package compose

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

type listFailingService struct {
	adksession.Service
	down atomic.Bool
}

func (s *listFailingService) List(ctx context.Context, req *adksession.ListRequest) (*adksession.ListResponse, error) {
	if s.down.Load() {
		return nil, errors.New("session store is unreachable")
	}
	return s.Service.List(ctx, req)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForLog(t *testing.T, buf *syncBuffer, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), substr) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in logs:\n%s", substr, buf.String())
}

func seedPauseSession(t *testing.T, h *eventlog.Handle, sid string) {
	t.Helper()
	if _, err := h.Service.Create(context.Background(), &adksession.CreateRequest{
		AppName: "core-agent", UserID: "system", SessionID: sid,
	}); err != nil {
		t.Fatalf("Create(%q): %v", sid, err)
	}
}

func TestRescanFiresATimerMintedOnAnotherInstance(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	seedPauseSession(t, h, "s-rescan")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fired := make(chan string, 4)
	sched := NewPauseScheduler(PauseSchedulerConfig{
		Handle:  h,
		AppName: "core-agent",
		Fire: func(_ context.Context, rec *eventlog.PauseRecord) error {
			fired <- rec.Token
			_, err := h.ConsumeToken(ctx, "core-agent", rec.Token, "timer")
			return err
		},
	})
	sched.rescanEvery = 5 * time.Millisecond

	if err := sched.Seed(ctx); err != nil {
		t.Fatalf("boot scan: %v", err)
	}
	go sched.Run(ctx)
	go sched.RunRescan(ctx)

	handle, _, err := h.PauseGate(ctx, "core-agent", "", "s-rescan", eventlog.PauseSpec{
		Reason:   eventlog.ReasonRateLimitBackoff,
		ResumeAt: time.Now().UTC().Add(-time.Second),
	})
	if err != nil {
		t.Fatalf("PauseGate on the other instance: %v", err)
	}

	select {
	case tok := <-fired:
		if tok != handle.Token {
			t.Fatalf("fired %q, want the token minted elsewhere %q", tok, handle.Token)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a timed pause minted on another instance never fired; the rescan did not pick it up")
	}

	select {
	case tok := <-fired:
		t.Errorf("the rescan re-fired a consumed token %q", tok)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRescanIsIdempotentAgainstAPendingTimer(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	seedPauseSession(t, h, "s-rescan-idem")
	ctx := context.Background()

	sched := NewPauseScheduler(PauseSchedulerConfig{
		Handle:  h,
		AppName: "core-agent",
		Fire: func(context.Context, *eventlog.PauseRecord) error {
			return nil
		},
	})
	handle, _, err := h.PauseGate(ctx, "core-agent", "", "s-rescan-idem", eventlog.PauseSpec{
		Reason:   eventlog.ReasonMaintenanceWindow,
		ResumeAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("PauseGate: %v", err)
	}

	for i := range 3 {
		n, err := sched.Scan(ctx)
		if err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
		if n != 1 {
			t.Fatalf("scan %d armed %d timers, want the 1 active record", i, n)
		}
	}
	sched.mu.Lock()
	entries := len(sched.entries)
	_, present := sched.entries[handle.Token]
	sched.mu.Unlock()
	if entries != 1 || !present {
		t.Errorf("three scans left %d entries (token present: %v), want exactly 1", entries, present)
	}
}

func TestRescanLogsOnTransitionNotOnEveryTick(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	seedPauseSession(t, h, "s-rescan-log")
	svc := &listFailingService{Service: h.Service}
	h.Service = svc

	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sched := NewPauseScheduler(PauseSchedulerConfig{
		Handle:  h,
		AppName: "core-agent",
		Logger:  logger,
		Fire: func(context.Context, *eventlog.PauseRecord) error {
			return nil
		},
	})
	sched.rescanEvery = 2 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.down.Store(true)
	go sched.RunRescan(ctx)

	waitForLog(t, buf, "rescan failed")

	time.Sleep(50 * time.Millisecond)
	if n := strings.Count(buf.String(), "rescan failed"); n != 1 {
		t.Errorf("a store that stayed down logged %d times; want 1 line per transition", n)
	}

	svc.down.Store(false)
	waitForLog(t, buf, "rescan recovered")
	time.Sleep(50 * time.Millisecond)
	if n := strings.Count(buf.String(), "rescan recovered"); n != 1 {
		t.Errorf("recovery logged %d times; want 1 line per transition", n)
	}
}

func TestRescanStopsWithItsContext(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	seedPauseSession(t, h, "s-rescan-stop")
	sched := NewPauseScheduler(PauseSchedulerConfig{
		Handle:  h,
		AppName: "core-agent",
		Fire: func(context.Context, *eventlog.PauseRecord) error {
			return nil
		},
	})
	sched.rescanEvery = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sched.RunRescan(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunRescan outlived its context; losing the lease would not stop it")
	}
}

func TestRescanStopsOnLeaseLost(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	seedPauseSession(t, h, "s-rescan-lease")
	leaseLost := make(chan struct{})
	sched := NewPauseScheduler(PauseSchedulerConfig{
		Handle:    h,
		AppName:   "core-agent",
		LeaseLost: leaseLost,
		Fire: func(context.Context, *eventlog.PauseRecord) error {
			return nil
		},
	})
	sched.rescanEvery = time.Millisecond

	ctx := context.Background()
	runDone := make(chan struct{})
	rescanDone := make(chan struct{})
	go func() {
		defer close(runDone)
		sched.Run(ctx)
	}()
	go func() {
		defer close(rescanDone)
		sched.RunRescan(ctx)
	}()

	close(leaseLost)
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit on LeaseLost")
	}
	select {
	case <-rescanDone:
	case <-time.After(5 * time.Second):
		t.Fatal("RunRescan did not exit on LeaseLost")
	}
}

func TestRescanPreservesInFlightAndRetryBackoff(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	seedPauseSession(t, h, "s-rescan-backoff")
	ctx := context.Background()

	handle, _, err := h.PauseGate(ctx, "core-agent", "", "s-rescan-backoff", eventlog.PauseSpec{
		Reason:   eventlog.ReasonRateLimitBackoff,
		ResumeAt: time.Now().UTC().Add(-time.Second),
	})
	if err != nil {
		t.Fatalf("PauseGate: %v", err)
	}

	inFire := make(chan struct{})
	releaseFire := make(chan struct{})
	sched := NewPauseScheduler(PauseSchedulerConfig{
		Handle:  h,
		AppName: "core-agent",
		Fire: func(context.Context, *eventlog.PauseRecord) error {
			close(inFire)
			<-releaseFire
			return errors.New("temporary failure")
		},
	})

	fireDone := make(chan struct{})
	go func() {
		defer close(fireDone)
		sched.fireDue(ctx, handle.Token)
	}()

	<-inFire
	// While fireDue is in flight, Scan must not re-insert the token.
	if _, err := sched.Scan(ctx); err != nil {
		t.Fatalf("Scan while in flight: %v", err)
	}
	sched.mu.Lock()
	_, queuedWhileInFlight := sched.entries[handle.Token]
	sched.mu.Unlock()
	if queuedWhileInFlight {
		t.Error("Scan re-inserted an in-flight token into entries")
	}

	close(releaseFire)
	<-fireDone

	// fireDue failed and requeued with pauseRequeueDelay; Scan must not shorten that backoff.
	sched.mu.Lock()
	backoffAt := sched.entries[handle.Token]
	sched.mu.Unlock()
	if _, err := sched.Scan(ctx); err != nil {
		t.Fatalf("Scan after backoff: %v", err)
	}
	sched.mu.Lock()
	afterScanAt := sched.entries[handle.Token]
	sched.mu.Unlock()
	if !afterScanAt.Equal(backoffAt) {
		t.Errorf("Scan overwrote retry backoff: got %v, want %v", afterScanAt, backoffAt)
	}
}
