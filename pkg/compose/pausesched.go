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

// Originally derived from go-steer/mast@d239f89 (internal/cli/pausesched.go).

package compose

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

const (
	pauseRescanInterval = time.Minute
	pauseRequeueDelay   = time.Minute
)

// PauseFireFunc is invoked when a timed pause reaches its ResumeAt deadline.
type PauseFireFunc func(ctx context.Context, rec *eventlog.PauseRecord) error

// PauseSchedulerConfig configures a PauseScheduler.
type PauseSchedulerConfig struct {
	Handle         *eventlog.Handle
	AppName        string
	Fire           PauseFireFunc
	LeaseLost      <-chan struct{}
	RescanInterval time.Duration
	Logger         *slog.Logger
}

// PauseScheduler is the single-instance timed-pause scheduler gated behind the
// fleet scheduling lease. It arms timers from the boot ops-row scan, accepts
// direct pushes via Schedule, and periodically rescans the durable store so
// timed pauses minted on passive replicas are picked up within
// pauseRescanInterval.
type PauseScheduler struct {
	handle      *eventlog.Handle
	appName     string
	logger      *slog.Logger
	fire        PauseFireFunc
	leaseLost   <-chan struct{}
	rescanEvery time.Duration

	mu       sync.Mutex
	entries  map[string]time.Time
	inFlight map[string]struct{}
	wake     chan struct{}
	stopOnce sync.Once
	stopping chan struct{}
}

// NewPauseScheduler constructs a PauseScheduler.
func NewPauseScheduler(cfg PauseSchedulerConfig) *PauseScheduler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	every := cfg.RescanInterval
	if every <= 0 {
		every = pauseRescanInterval
	}
	appName := cfg.AppName
	if appName == "" {
		appName = "core-agent"
	}
	return &PauseScheduler{
		handle:      cfg.Handle,
		appName:     appName,
		logger:      logger,
		fire:        cfg.Fire,
		leaseLost:   cfg.LeaseLost,
		rescanEvery: every,
		entries:     make(map[string]time.Time),
		inFlight:    make(map[string]struct{}),
		wake:        make(chan struct{}, 1),
		stopping:    make(chan struct{}),
	}
}

// Schedule arms (or re-arms) a resume token's fire time and wakes the loop.
func (ps *PauseScheduler) Schedule(token string, at time.Time) {
	if token == "" || at.IsZero() {
		return
	}
	ps.mu.Lock()
	ps.entries[token] = at
	ps.mu.Unlock()
	select {
	case ps.wake <- struct{}{}:
	default:
	}
}

func (ps *PauseScheduler) scheduleFromScan(token string, at time.Time) {
	if token == "" || at.IsZero() {
		return
	}
	ps.mu.Lock()
	if _, busy := ps.inFlight[token]; busy {
		ps.mu.Unlock()
		return
	}
	if existing, ok := ps.entries[token]; ok && existing.After(at) {
		ps.mu.Unlock()
		return
	}
	ps.entries[token] = at
	ps.mu.Unlock()
	select {
	case ps.wake <- struct{}{}:
	default:
	}
}

// Seed loads every active pause record with a non-zero ResumeAt from the
// durable store.
func (ps *PauseScheduler) Seed(ctx context.Context) error {
	n, err := ps.Scan(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		ps.logger.Info("timed-pause scheduler seeded from boot scan", "timers", n)
	}
	return nil
}

// Scan arms every active pause record carrying a ResumeAt and returns the
// count. Idempotent: re-arming an already-pending token updates its fire time
// in place without queueing a duplicate fire, and preserves any in-flight
// execution or later retry backoff.
func (ps *PauseScheduler) Scan(ctx context.Context) (int, error) {
	if ps.handle == nil {
		return 0, nil
	}
	records, err := ps.handle.ScanPauses(ctx, ps.appName)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rec := range records {
		if !rec.ResumeAt.IsZero() {
			ps.scheduleFromScan(rec.Token, rec.ResumeAt)
			n++
		}
	}
	return n, nil
}

// RunRescan periodically re-scans the durable store for timed pauses minted on
// passive replicas. It exits when ctx is cancelled, LeaseLost closes, or Stop
// is called.
func (ps *PauseScheduler) RunRescan(ctx context.Context) {
	every := ps.rescanEvery
	if every <= 0 {
		every = pauseRescanInterval
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ps.leaseLost:
			ps.logger.Error("scheduling lease lost; stopping timed-pause rescan")
			return
		case <-ps.stopping:
			return
		case <-ticker.C:
			if _, err := ps.Scan(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				if !failing {
					failing = true
					ps.logger.Warn("timed-pause rescan failed; timers minted on another instance will not fire until it succeeds", "error", err.Error())
				}
				continue
			}
			if failing {
				failing = false
				ps.logger.Info("timed-pause rescan recovered")
			}
		}
	}
}

// Run drives the timed-pause timer loop until ctx is cancelled, LeaseLost
// closes, or Stop is called.
func (ps *PauseScheduler) Run(ctx context.Context) {
	for {
		token, at, ok := ps.earliest()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-ps.leaseLost:
				ps.logger.Error("scheduling lease lost; stopping timed-pause scheduler")
				return
			case <-ps.stopping:
				return
			case <-ps.wake:
				continue
			}
		}
		delay := time.Until(at)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-ps.leaseLost:
			timer.Stop()
			ps.logger.Error("scheduling lease lost; stopping timed-pause scheduler")
			return
		case <-ps.stopping:
			timer.Stop()
			return
		case <-ps.wake:
			timer.Stop()
			continue
		case <-timer.C:
			ps.fireDue(ctx, token)
		}
	}
}

// Stop signals both Run and RunRescan to exit immediately.
func (ps *PauseScheduler) Stop() {
	ps.stopOnce.Do(func() { close(ps.stopping) })
}

func (ps *PauseScheduler) earliest() (string, time.Time, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	var (
		token string
		at    time.Time
		found bool
	)
	for tok, t := range ps.entries {
		if !found || t.Before(at) {
			token, at, found = tok, t, true
		}
	}
	return token, at, found
}

func (ps *PauseScheduler) fireDue(ctx context.Context, token string) {
	if ctx.Err() != nil || ps.aborted() {
		return
	}
	ps.mu.Lock()
	delete(ps.entries, token)
	ps.inFlight[token] = struct{}{}
	ps.mu.Unlock()
	defer func() {
		ps.mu.Lock()
		delete(ps.inFlight, token)
		ps.mu.Unlock()
	}()

	if ps.handle == nil {
		return
	}
	turnCtx, cancelTurn := ps.turnContext(ctx)
	defer cancelTurn()

	rec, err := ps.handle.FindToken(turnCtx, ps.appName, token)
	if errors.Is(err, eventlog.ErrTokenNotFound) {
		return
	}
	if err != nil {
		if ctx.Err() != nil || ps.aborted() {
			return
		}
		ps.logger.Warn("timed-pause record fetch failed; requeued",
			"retry_in", pauseRequeueDelay.String(), "error", err.Error())
		ps.Schedule(token, time.Now().Add(pauseRequeueDelay))
		return
	}
	if !rec.Active() {
		return
	}
	if now := time.Now().UTC(); rec.ResumeAt.After(now) {
		ps.Schedule(token, rec.ResumeAt)
		return
	}
	if ps.fire != nil {
		if err := ps.fire(turnCtx, rec); err != nil {
			if ctx.Err() != nil || ps.aborted() {
				return
			}
			ps.logger.Warn("timed resume refused; requeued",
				"session", rec.SessionID, "plane", rec.Plane, "reason", string(rec.Reason),
				"retry_in", pauseRequeueDelay.String(), "error", err.Error())
			ps.Schedule(token, time.Now().Add(pauseRequeueDelay))
			return
		}
	}
	ps.logger.Info("timed resume fired",
		"session", rec.SessionID, "plane", rec.Plane, "reason", string(rec.Reason))
}

func (ps *PauseScheduler) turnContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
		case <-ps.leaseLost:
			cancel()
		case <-ps.stopping:
			cancel()
		}
	}()
	return ctx, func() {
		close(done)
		cancel()
	}
}

func (ps *PauseScheduler) aborted() bool {
	if ps.leaseLost != nil {
		select {
		case <-ps.leaseLost:
			return true
		default:
		}
	}
	select {
	case <-ps.stopping:
		return true
	default:
		return false
	}
}
