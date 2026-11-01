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

// Originally derived from go-steer/mast@d239f89 (internal/cli/schedlease.go).

package compose

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

const (
	// SchedulingLeaseRetryWindow is the boot retry window for a contested
	// scheduling lease: 30s default staleness window in pkg/eventlog + 2s
	// margin so a process restarting after SIGKILL/OOM reclaims its own
	// abandoned lease instead of standing down permanently.
	SchedulingLeaseRetryWindow = 32 * time.Second

	schedulingLeaseRetryEvery = 500 * time.Millisecond
	schedulingLeaseUserID     = "_system"
	schedulingLeaseSessionID  = "__mast_scheduling__"
)

// SchedulingLease wraps the single fleet-wide *eventlog.SessionLock that
// gates all three unrequested-turn drivers on a replica: the scheduled
// trigger, the timed-pause scheduler, and boot auto-resume / auto-continue.
type SchedulingLease struct {
	lock *eventlog.SessionLock
}

// Lost returns a channel that closes if the underlying session lock loses
// ownership mid-flight (heartbeat failure or stolen row). Nil-safe: a nil
// lease (passive replica or fail-open without a store) returns a nil channel,
// which blocks forever in a select.
func (s *SchedulingLease) Lost() <-chan struct{} {
	if s == nil || s.lock == nil {
		return nil
	}
	return s.lock.Lost()
}

// Release drops the underlying session lock so a successor replica can take
// over immediately without waiting out the staleness window. Idempotent and
// nil-safe.
func (s *SchedulingLease) Release() {
	if s == nil || s.lock == nil {
		return
	}
	_ = s.lock.Release()
}

// Holder returns the lock holder identity string, or "" when no lock is held.
func (s *SchedulingLease) Holder() string {
	if s == nil || s.lock == nil {
		return ""
	}
	return s.lock.Holder()
}

// AcquireSchedulingLease decides whether this replica drives unrequested
// background work (scheduled triggers, timed-pause resumes, and boot
// auto-resume), returning the lease handle and active=true when this replica
// should drive scheduled work.
//
// Three outcomes:
//   - No durable store (h == nil or h.DB == nil): fails open (nil, true) with
//     a warning naming --session-db.
//   - Lease held by a live holder after SchedulingLeaseRetryWindow: stands
//     down passively (nil, false) and logs at ERROR.
//   - Unexpected store error while ctx is still live: fails open (nil, true)
//     and logs at ERROR so a transient DB fault at boot does not silently
//     disable scheduled work in a single-replica deployment.
func AcquireSchedulingLease(ctx context.Context, h *eventlog.Handle, appName string, logger *slog.Logger) (*SchedulingLease, bool) {
	return acquireSchedulingLeaseWithin(ctx, h, appName, logger, SchedulingLeaseRetryWindow, schedulingLeaseRetryEvery)
}

func acquireSchedulingLeaseWithin(
	ctx context.Context,
	h *eventlog.Handle,
	appName string,
	logger *slog.Logger,
	retryFor, retryEvery time.Duration,
) (*SchedulingLease, bool) {
	if logger == nil {
		logger = slog.Default()
	}
	if appName == "" {
		appName = "core-agent"
	}
	if h == nil || h.DB == nil {
		logger.Warn("scheduled work is not coordinated between instances (--session-db is empty); run a single instance for this workload",
			"workload", appName)
		return nil, true
	}

	deadline := time.Now().Add(retryFor)
	waited := false
	for {
		lock, err := h.AcquireLock(ctx, appName, schedulingLeaseUserID, schedulingLeaseSessionID)
		switch {
		case err == nil:
			if waited {
				logger.Info("took the scheduling lease from an instance that stopped heartbeating; scheduled work resumes here",
					"workload", appName, "lease", schedulingLeaseSessionID, "holder", lock.Holder())
			} else {
				logger.Info("this instance drives scheduled work for the workload",
					"workload", appName, "lease", schedulingLeaseSessionID, "holder", lock.Holder())
			}
			return &SchedulingLease{lock: lock}, true

		case errors.Is(err, eventlog.ErrSessionLocked):
			if remaining := time.Until(deadline); remaining > 0 && ctx.Err() == nil {
				if !waited {
					waited = true
					logger.Info("the scheduling lease is held; waiting in case its holder is gone",
						"workload", appName, "lease", schedulingLeaseSessionID,
						"waiting_up_to", remaining.Round(time.Second).String(), "error", err.Error())
				}
				select {
				case <-ctx.Done():
				case <-time.After(retryEvery):
				}
				continue
			}
			logger.Error("another instance already drives scheduled work against this session store; this instance will serve requests but will not fire scheduled triggers, timed-pause resumes or boot auto-resume",
				"workload", appName, "lease", schedulingLeaseSessionID, "error", err.Error())
			return nil, false

		default:
			if ctx.Err() != nil {
				return nil, false
			}
			logger.Error("could not take the scheduling lease; proceeding as if this instance drives scheduled work, which duplicates it if another instance is running",
				"workload", appName, "lease", schedulingLeaseSessionID, "error", err.Error())
			return nil, true
		}
	}
}
