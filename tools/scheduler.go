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
	"time"
)

// Scheduler decides what RunAutonomous should do between turns when
// the prior turn emitted a schedule intent via the schedule_next_turn
// tool. Loops whose model never emits a schedule intent never consult
// the scheduler — zero overhead by default.
//
// Bundled implementations:
//
//   - SleepScheduler — long-lived daemon: sleeps the goroutine until
//     WakeAt, then returns nil so the loop continues.
//   - ExitOnDeferScheduler — orchestrator-managed (k8s CronJob, AX,
//     external queue): returns ErrSchedulerDefer so the loop exits
//     cleanly with StopReasonDeferred + RunResult.NextWakeAt
//     populated. A subsequent ResumeAutonomous picks up at the
//     persisted wake-time.
//
// Consumers ship their own Scheduler for distributed shapes (NATS
// queue, custom orchestrator). The schedulerFunc adapter is exported
// for one-off function-shaped implementations.
type Scheduler interface {
	// BeforeNextTurn is consulted by RunAutonomous after a turn that
	// emitted a schedule intent, after the per-turn checkpoint is
	// written. Return nil to let the loop continue at ev.WakeAt with
	// prompt=ev.NextPrompt. Return ErrSchedulerDefer to exit the loop
	// with StopReasonDeferred. Any other error aborts the run.
	BeforeNextTurn(ctx context.Context, ev ScheduleEvent) error
}

// ErrSchedulerDefer is the sentinel a Scheduler returns to exit the
// autonomous loop with StopReasonDeferred — handing the wake-time off
// to whatever orchestrator restarts the process or invokes
// ResumeAutonomous.
var ErrSchedulerDefer = errors.New("scheduler: defer to next process")

// SchedulerFunc adapts a plain function to the Scheduler interface
// for callers that don't want to declare a type just to wire one
// behavior.
type SchedulerFunc func(ctx context.Context, ev ScheduleEvent) error

// BeforeNextTurn implements Scheduler.
func (f SchedulerFunc) BeforeNextTurn(ctx context.Context, ev ScheduleEvent) error {
	return f(ctx, ev)
}

// SleepScheduler returns a Scheduler that sleeps the calling goroutine
// until ev.WakeAt, respecting context cancellation. Returns nil on
// wake; returns ctx.Err() when the context is cancelled mid-sleep.
//
// Use when the agent runs as a long-lived daemon that should retain
// in-memory state between scan cycles (warm prompt cache, supervisor
// subagent tree, etc.). For deployments where the process exits
// between cycles, use ExitOnDeferScheduler instead.
//
// A WakeAt already in the past returns immediately.
func SleepScheduler() Scheduler {
	return SchedulerFunc(func(ctx context.Context, ev ScheduleEvent) error {
		wait := time.Until(ev.WakeAt)
		if wait <= 0 {
			return nil
		}
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

// ExitOnDeferScheduler returns a Scheduler that exits the autonomous
// loop on every schedule emission via ErrSchedulerDefer. The driver
// reports StopReasonDeferred with RunResult.NextWakeAt populated from
// the schedule event; whatever orchestrator wraps the process
// (k8s CronJob, supervisord, AX) restarts it at or after that
// wake-time and a fresh ResumeAutonomous call picks up where this
// run left off (the deferred checkpoint persists the wake-time to
// the eventlog).
//
// Use when external scheduling infrastructure already exists and the
// operator prefers short-lived processes over a long-lived daemon.
// For most monitoring shapes SleepScheduler is the cleaner default;
// see docs/scheduled-monitoring-design.md.
func ExitOnDeferScheduler() Scheduler {
	return SchedulerFunc(func(_ context.Context, _ ScheduleEvent) error {
		return ErrSchedulerDefer
	})
}
