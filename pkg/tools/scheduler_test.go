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

package tools

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSleepScheduler_HonorsWakeAt(t *testing.T) {
	t.Parallel()
	s := SleepScheduler()
	wake := time.Now().Add(50 * time.Millisecond)
	start := time.Now()
	if err := s.BeforeNextTurn(context.Background(), ScheduleEvent{WakeAt: wake}); err != nil {
		t.Fatalf("BeforeNextTurn: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("returned too early: %v", elapsed)
	}
}

func TestSleepScheduler_PastWakeAtReturnsImmediately(t *testing.T) {
	t.Parallel()
	s := SleepScheduler()
	start := time.Now()
	if err := s.BeforeNextTurn(context.Background(), ScheduleEvent{
		WakeAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("BeforeNextTurn: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("past wake-at should return immediately, took %v", elapsed)
	}
}

func TestSleepScheduler_ContextCancellation(t *testing.T) {
	t.Parallel()
	s := SleepScheduler()
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay; the scheduler should unblock immediately.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := s.BeforeNextTurn(ctx, ScheduleEvent{
		WakeAt: time.Now().Add(time.Hour),
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("should have unblocked on cancel quickly, took %v", elapsed)
	}
}

func TestSleepScheduler_ExternalWakeUnblocks(t *testing.T) {
	t.Parallel()
	s := SleepScheduler()
	wake := make(chan struct{}, 1)
	ctx := ContextWithWake(context.Background(), wake)
	// Fire wake after a short delay; SleepScheduler should return
	// nil promptly (not ctx.Err()) and not wait out the hour.
	go func() {
		time.Sleep(20 * time.Millisecond)
		wake <- struct{}{}
	}()
	start := time.Now()
	err := s.BeforeNextTurn(ctx, ScheduleEvent{
		WakeAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Errorf("expected nil (external wake), got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("should have unblocked on external wake quickly, took %v", elapsed)
	}
}

func TestSleepScheduler_NilWakeChannelBlocksOnTimerOnly(t *testing.T) {
	t.Parallel()
	// No wake channel attached — scheduler should behave exactly as
	// before (wait for the timer, respect ctx).
	s := SleepScheduler()
	start := time.Now()
	if err := s.BeforeNextTurn(context.Background(), ScheduleEvent{
		WakeAt: time.Now().Add(40 * time.Millisecond),
	}); err != nil {
		t.Fatalf("BeforeNextTurn: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("returned too early (nil wakeCh should not fire): %v", elapsed)
	}
}

func TestContextWithWake_NilChannelIsNoOp(t *testing.T) {
	t.Parallel()
	parent := context.Background()
	got := ContextWithWake(parent, nil)
	if got != parent {
		t.Errorf("ContextWithWake(_, nil) should return the parent ctx unchanged")
	}
}

func TestExitOnDeferScheduler_AlwaysReturnsDeferSentinel(t *testing.T) {
	t.Parallel()
	s := ExitOnDeferScheduler()
	err := s.BeforeNextTurn(context.Background(), ScheduleEvent{
		WakeAt:     time.Now().Add(time.Hour),
		NextPrompt: "rescan",
	})
	if !errors.Is(err, ErrSchedulerDefer) {
		t.Errorf("expected ErrSchedulerDefer, got %v", err)
	}
}

func TestSchedulerFunc_AdaptsFunctionToInterface(t *testing.T) {
	t.Parallel()
	var got ScheduleEvent
	s := SchedulerFunc(func(_ context.Context, ev ScheduleEvent) error {
		got = ev
		return nil
	})
	want := ScheduleEvent{
		WakeAt:     time.Now().Add(time.Minute),
		NextPrompt: "rescan",
	}
	if err := s.BeforeNextTurn(context.Background(), want); err != nil {
		t.Fatalf("BeforeNextTurn: %v", err)
	}
	if !got.WakeAt.Equal(want.WakeAt) || got.NextPrompt != want.NextPrompt {
		t.Errorf("event mismatch: got %+v want %+v", got, want)
	}
}
