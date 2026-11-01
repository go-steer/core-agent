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

// Originally derived from go-steer/mast@d239f89 (internal/cli/schedtrigger_test.go).

package compose

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

var anchorTime = time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC)

func TestNextTickIsAnchoredNotReset(t *testing.T) {
	t.Parallel()
	const interval = time.Hour
	for _, tc := range []struct {
		name  string
		after time.Time
		want  time.Time
	}{
		{"at the anchor, the first tick is one interval out", anchorTime, anchorTime.Add(time.Hour)},
		{"mid-interval", anchorTime.Add(17 * time.Minute), anchorTime.Add(time.Hour)},
		{"exactly on a tick, the NEXT one", anchorTime.Add(time.Hour), anchorTime.Add(2 * time.Hour)},
		{"a hair before a tick", anchorTime.Add(time.Hour - time.Nanosecond), anchorTime.Add(time.Hour)},
		{"three days later, still on the same phase", anchorTime.Add(72*time.Hour + 47*time.Minute), anchorTime.Add(73 * time.Hour)},
		{"before the anchor: the anchor itself", anchorTime.Add(-5 * time.Minute), anchorTime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextTick(anchorTime, interval, tc.after); !got.Equal(tc.want) {
				t.Errorf("nextTick(after=%v) = %v, want %v", tc.after, got, tc.want)
			}
		})
	}
}

func TestTicksSkippedCounts(t *testing.T) {
	t.Parallel()
	const interval = time.Hour
	for _, tc := range []struct {
		name    string
		last    time.Time
		now     time.Time
		want    int
		through time.Time
	}{
		{"nothing missed", anchorTime, anchorTime.Add(30 * time.Minute), 0, anchorTime},
		{"one, exactly on the tick", anchorTime, anchorTime.Add(time.Hour), 1, anchorTime.Add(time.Hour)},
		{"a weekend down", anchorTime, anchorTime.Add(50 * time.Hour), 50, anchorTime.Add(50 * time.Hour)},
		{"partial intervals do not count", anchorTime, anchorTime.Add(3*time.Hour + 59*time.Minute), 3, anchorTime.Add(3 * time.Hour)},
		{"a clock that moved backwards misses nothing", anchorTime, anchorTime.Add(-time.Hour), 0, anchorTime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, through := ticksSkipped(tc.last, interval, tc.now)
			if n != tc.want || !through.Equal(tc.through) {
				t.Errorf("ticksSkipped(last=%v, now=%v) = %d, %v; want %d, %v",
					tc.last, tc.now, n, through, tc.want, tc.through)
			}
		})
	}
}

func TestScheduledCadenceSurvivesARestartWithoutCatchingUp(t *testing.T) {
	t.Parallel()
	const interval = time.Hour
	h := openSchedLeaseHandle(t)
	ctx := context.Background()

	first, _ := newTestTrigger(t, h, interval, 0)
	first.now = clockAt(anchorTime)
	if err := first.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first.fireDue(ctx, anchorTime.Add(interval), 0)

	second, fired := newTestTrigger(t, h, interval, 0)
	second.now = clockAt(anchorTime.Add(3*interval + 30*time.Minute))
	if err := second.seed(ctx); err != nil {
		t.Fatalf("seed after restart: %v", err)
	}
	if !second.anchor.Equal(anchorTime) {
		t.Fatalf("anchor after restart = %v, want %v", second.anchor, anchorTime)
	}

	p := second.plan(second.now())
	if p.Skipped != 2 {
		t.Errorf("skipped = %d, want 2", p.Skipped)
	}
	if want := anchorTime.Add(2 * interval); !p.SkippedFrom.Equal(want) {
		t.Errorf("skipped from = %v, want %v", p.SkippedFrom, want)
	}
	if want := anchorTime.Add(3 * interval); !p.SkippedThrough.Equal(want) {
		t.Errorf("skipped through = %v, want %v", p.SkippedThrough, want)
	}
	if want := anchorTime.Add(4 * interval); !p.Tick.Equal(want) {
		t.Errorf("next tick = %v, want %v", p.Tick, want)
	}
	if got := len(*fired); got != 0 {
		t.Errorf("the restarted process fired %d times while planning", got)
	}

	if again := second.plan(second.now()); again.Skipped != 0 {
		t.Errorf("second plan reported %d skipped ticks again", again.Skipped)
	}

	// When the next tick actually fires, it carries the collapsed skipped count.
	second.fireDue(ctx, p.Tick, p.Skipped)
	if len(*fired) != 1 || (*fired)[0].Skipped != 2 {
		t.Errorf("fired = %+v, want 1 fire with Skipped=2", *fired)
	}
	if !strings.Contains((*fired)[0].Payload, `"skipped":2`) {
		t.Errorf("payload %q missing skipped=2", (*fired)[0].Payload)
	}
}

func TestScheduledJitterIsBoundedAndDoesNotAccumulate(t *testing.T) {
	t.Parallel()
	const (
		interval = time.Hour
		jitter   = 5 * time.Minute
	)
	h := openSchedLeaseHandle(t)
	tr, _ := newTestTrigger(t, h, interval, jitter)
	tr.now = clockAt(anchorTime)
	tr.jitterFrac = func() float64 { return 0.999 }
	if err := tr.seed(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for k := 1; k <= 10; k++ {
		want := anchorTime.Add(time.Duration(k) * interval)
		p := tr.plan(tr.now())
		if !p.Tick.Equal(want) {
			t.Fatalf("tick %d = %v, want %v", k, p.Tick, want)
		}
		if p.FireAt.Before(p.Tick) || !p.FireAt.Before(p.Tick.Add(jitter)) {
			t.Fatalf("fire %d at %v, want it inside [%v, %v)", k, p.FireAt, p.Tick, p.Tick.Add(jitter))
		}
		tr.last = p.Tick
		tr.now = clockAt(p.FireAt)
	}
}

func TestScheduledJitterClampedToHalfInterval(t *testing.T) {
	t.Parallel()
	tr, err := NewScheduledTrigger(ScheduledTriggerConfig{
		Workload: "uat-sched",
		Interval: 10 * time.Minute,
		Jitter:   8 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewScheduledTrigger: %v", err)
	}
	if tr.jitter != 5*time.Minute {
		t.Errorf("jitter = %v, want clamped to 5m (interval/2)", tr.jitter)
	}
}

func TestScheduledJitterZeroFiresOnTheTick(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	tr, _ := newTestTrigger(t, h, time.Hour, 0)
	tr.now = clockAt(anchorTime)
	tr.jitterFrac = func() float64 { t.Fatal("jitterFrac consulted for a zero jitter"); return 0 }
	if err := tr.seed(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := tr.plan(tr.now())
	if !p.FireAt.Equal(p.Tick) {
		t.Errorf("fire at %v, want it on the tick %v", p.FireAt, p.Tick)
	}
}

func TestScheduledFireSpendsTheTickWhateverHappens(t *testing.T) {
	t.Parallel()
	const interval = time.Hour
	h := openSchedLeaseHandle(t)
	ctx := context.Background()

	tr, fired := newTestTrigger(t, h, interval, 0)
	tr.now = clockAt(anchorTime)
	tr.fire = func(_ context.Context, f ScheduledFire) error {
		*fired = append(*fired, f)
		return errors.New("the model was unreachable")
	}
	if err := tr.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tick := anchorTime.Add(interval)
	tr.fireDue(ctx, tick, 0)

	if len(*fired) != 1 {
		t.Fatalf("fires = %d, want 1", len(*fired))
	}
	rec, err := h.Schedule(ctx, "uat-sched", tr.workload)
	if err != nil || rec == nil {
		t.Fatalf("no schedule record after a failed fire (err=%v)", err)
	}
	if !rec.LastTick.Equal(tick) {
		t.Errorf("last_tick = %v, want the spent tick %v", rec.LastTick, tick)
	}
	if p := tr.plan(tick.Add(time.Minute)); !p.Tick.Equal(tick.Add(interval)) {
		t.Errorf("next tick after a failure = %v, want %v", p.Tick, tick.Add(interval))
	}
}

func TestScheduledFireRefusedWhileDraining(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	ctx := context.Background()

	tr, fired := newTestTrigger(t, h, time.Hour, 0)
	tr.now = clockAt(anchorTime)
	tr.isDraining = func() bool { return true }
	if err := tr.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tr.fireDue(ctx, anchorTime.Add(time.Hour), 0)

	if len(*fired) != 0 {
		t.Errorf("the scheduler started a turn during a drain (%d fires)", len(*fired))
	}
}

func TestScheduledRunIdentifiesItselfAsTheScheduler(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if caller, ok := auth.CallerFromContext(ctx); ok && caller.Identity == SchedulerIdentity {
		t.Fatalf("a bare context already reports %q", SchedulerIdentity)
	}
	caller, ok := auth.CallerFromContext(SchedulerContext(ctx))
	if !ok || caller.Identity != SchedulerIdentity {
		t.Errorf("caller on scheduled fire = %+v (ok=%v), want %q", caller, ok, SchedulerIdentity)
	}
}

func TestScheduledSessionIDIsLegible(t *testing.T) {
	t.Parallel()
	sid := ScheduledSessionID("gke-triage", anchorTime.Add(90*time.Minute))
	if want := "scheduled-gke-triage-20260816T043000Z"; sid != want {
		t.Errorf("session ID = %q, want %q", sid, want)
	}
	if strings.HasSuffix(sid, ":mast-ops") {
		t.Errorf("session ID %q lands in the reserved ops-row namespace", sid)
	}
	if a, b := ScheduledSessionID("wl", anchorTime), ScheduledSessionID("wl", anchorTime.Add(time.Second)); a == b {
		t.Errorf("two ticks share the session ID %q", a)
	}
}

func TestScheduledAnchorIsPersistedBeforeTheFirstFire(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	ctx := context.Background()

	tr, _ := newTestTrigger(t, h, 24*time.Hour, 0)
	tr.now = clockAt(anchorTime)
	if err := tr.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec, err := h.Schedule(ctx, "uat-sched", tr.workload)
	if err != nil || rec == nil {
		t.Fatalf("no schedule record after seeding (err=%v)", err)
	}
	if !rec.Anchor.Equal(anchorTime) {
		t.Errorf("persisted anchor = %v, want %v", rec.Anchor, anchorTime)
	}
	if rec.Fires != 0 {
		t.Errorf("fires = %d before anything fired", rec.Fires)
	}

	next, _ := newTestTrigger(t, h, 24*time.Hour, 0)
	next.now = clockAt(anchorTime.Add(time.Hour))
	if err := next.seed(ctx); err != nil {
		t.Fatalf("seed after restart: %v", err)
	}
	if !next.anchor.Equal(anchorTime) {
		t.Errorf("anchor after restart = %v, want %v", next.anchor, anchorTime)
	}
}

func TestScheduledFireCountAccumulatesAcrossProcesses(t *testing.T) {
	t.Parallel()
	const interval = time.Hour
	h := openSchedLeaseHandle(t)
	ctx := context.Background()

	first, _ := newTestTrigger(t, h, interval, 0)
	first.now = clockAt(anchorTime)
	if err := first.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first.fireDue(ctx, anchorTime.Add(interval), 0)
	first.fireDue(ctx, anchorTime.Add(2*interval), 0)

	second, _ := newTestTrigger(t, h, interval, 0)
	second.now = clockAt(anchorTime.Add(3 * interval))
	if err := second.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	second.fireDue(ctx, anchorTime.Add(3*interval), 0)

	rec, err := h.Schedule(ctx, "uat-sched", "uat-sched")
	if err != nil || rec == nil {
		t.Fatalf("no schedule record (err=%v)", err)
	}
	if rec.Fires != 3 {
		t.Errorf("fires = %d, want 3 across both processes", rec.Fires)
	}
}

func TestScheduledPreFireHook_SkipsTurnAndAdvancesLastTick(t *testing.T) {
	t.Parallel()
	const interval = time.Hour
	h := openSchedLeaseHandle(t)
	ctx := context.Background()

	tr, fired := newTestTrigger(t, h, interval, 0)
	tr.now = clockAt(anchorTime)
	if err := tr.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Suppressed tick: PreFire returns ok=false.
	tr.preFire = func(_ context.Context, _ time.Time, _ int64) (string, bool) {
		return "", false
	}
	tick1 := anchorTime.Add(interval)
	tr.fireDue(ctx, tick1, 0)
	if len(*fired) != 0 {
		t.Fatalf("Fire called despite PreFire ok=false: %+v", *fired)
	}
	rec, err := h.Schedule(ctx, "uat-sched", tr.workload)
	if err != nil || rec == nil {
		t.Fatalf("Schedule after suppressed tick: %v", err)
	}
	if !rec.LastTick.Equal(tick1) {
		t.Errorf("LastTick = %v, want %v", rec.LastTick, tick1)
	}
	if rec.Fires != 0 {
		t.Errorf("Fires = %d, want 0 after suppressed tick", rec.Fires)
	}

	// Allowed tick with payload override.
	tr.preFire = func(_ context.Context, _ time.Time, _ int64) (string, bool) {
		return "CUSTOM PAYLOAD", true
	}
	tick2 := anchorTime.Add(2 * interval)
	tr.fireDue(ctx, tick2, 0)
	if len(*fired) != 1 || (*fired)[0].Payload != "CUSTOM PAYLOAD" {
		t.Errorf("fired = %+v, want 1 fire with custom payload", *fired)
	}
}

func TestScheduledSingleFlightSkipsOverlap(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	ctx := context.Background()

	tr, fired := newTestTrigger(t, h, time.Hour, 0)
	tr.now = clockAt(anchorTime)
	if err := tr.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tr.running.Store(true)
	tr.fireDue(ctx, anchorTime.Add(time.Hour), 0)
	if len(*fired) != 0 {
		t.Errorf("overlapping fire should have been skipped, got %d fires", len(*fired))
	}
}

func TestScheduledTriggerStopsOnLeaseLost(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	leaseLost := make(chan struct{})
	logger, _ := capturingLogger()

	tr, err := NewScheduledTrigger(ScheduledTriggerConfig{
		Handle:    h,
		AppName:   "uat-sched",
		Workload:  "uat-sched",
		Interval:  time.Hour,
		LeaseLost: leaseLost,
		Logger:    logger,
		Now:       clockAt(anchorTime),
	})
	if err != nil {
		t.Fatalf("NewScheduledTrigger: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.Run(context.Background())
	}()

	close(leaseLost)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ScheduledTrigger.Run did not exit promptly after LeaseLost closed")
	}
}

func TestScheduledTriggerLeaseLostMidTurnCancelsFireAndSkipsPersist(t *testing.T) {
	t.Parallel()
	h := openSchedLeaseHandle(t)
	ctx := context.Background()
	leaseLost := make(chan struct{})
	logger, _ := capturingLogger()

	inTurn := make(chan struct{})
	tr, err := NewScheduledTrigger(ScheduledTriggerConfig{
		Handle:    h,
		AppName:   "uat-sched",
		Workload:  "uat-sched",
		Interval:  time.Hour,
		LeaseLost: leaseLost,
		Logger:    logger,
		Now:       clockAt(anchorTime),
		Fire: func(turnCtx context.Context, _ ScheduledFire) error {
			close(inTurn)
			<-turnCtx.Done()
			return turnCtx.Err()
		},
	})
	if err != nil {
		t.Fatalf("NewScheduledTrigger: %v", err)
	}
	if err := tr.seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.fireDue(ctx, anchorTime.Add(time.Hour), 0)
	}()

	<-inTurn
	close(leaseLost)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fireDue did not abort in-flight turn when LeaseLost closed")
	}

	rec, err := h.Schedule(ctx, "uat-sched", "uat-sched")
	if err != nil || rec == nil {
		t.Fatalf("Schedule: %v", err)
	}
	if rec.Fires != 0 {
		t.Errorf("Fires = %d, want 0 (deposed replica must not clobber ScheduleRecord)", rec.Fires)
	}
}

func newTestTrigger(t *testing.T, h *eventlog.Handle, interval, jitter time.Duration) (*ScheduledTrigger, *[]ScheduledFire) {
	t.Helper()
	fired := &[]ScheduledFire{}
	logger, _ := capturingLogger()
	tr, err := NewScheduledTrigger(ScheduledTriggerConfig{
		Handle:   h,
		AppName:  "uat-sched",
		Workload: "uat-sched",
		Interval: interval,
		Jitter:   jitter,
		Prompt:   "Check cluster health.",
		Logger:   logger,
		Fire: func(_ context.Context, f ScheduledFire) error {
			*fired = append(*fired, f)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewScheduledTrigger: %v", err)
	}
	return tr, fired
}

func clockAt(at time.Time) func() time.Time {
	return func() time.Time { return at }
}
