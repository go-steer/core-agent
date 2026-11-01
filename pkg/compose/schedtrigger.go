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

// Originally derived from go-steer/mast@d239f89 (internal/cli/schedtrigger.go).

package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

// SchedulerIdentity is the caller identity stamped on contexts for
// unrequested scheduled runs.
const SchedulerIdentity = "mast:scheduler"

const scheduleStoreWriteTimeout = 5 * time.Second

// SchedulerContext stamps SchedulerIdentity onto ctx as the authenticated
// caller so audit logs and write-gate parks attribute scheduled turns to
// "mast:scheduler".
func SchedulerContext(ctx context.Context) context.Context {
	return auth.WithCaller(ctx, auth.Caller{Identity: SchedulerIdentity})
}

// ScheduledFire describes a single cadence tick delivered to ScheduledFireFunc.
type ScheduledFire struct {
	SessionID string
	Workload  string
	Tick      time.Time
	FireNum   int64
	Skipped   int
	Payload   string
}

// ScheduledFireFunc executes one scheduled turn for fire.
type ScheduledFireFunc func(ctx context.Context, fire ScheduledFire) error

// ScheduledPreFireFunc runs before the LLM turn on each due tick.
// When ok is false, the LLM turn is skipped while LastTick still advances in
// the durable schedule record. When ok is true and payloadOverride is
// non-empty, payloadOverride replaces the default ScheduledPayload.
type ScheduledPreFireFunc func(ctx context.Context, tick time.Time, fireNum int64) (payloadOverride string, ok bool)

// ScheduledTriggerConfig configures a ScheduledTrigger.
type ScheduledTriggerConfig struct {
	Handle     *eventlog.Handle
	AppName    string
	Workload   string
	Interval   time.Duration
	Jitter     time.Duration
	Prompt     string
	Fire       ScheduledFireFunc
	PreFire    ScheduledPreFireFunc
	LeaseLost  <-chan struct{}
	IsDraining func() bool
	Logger     *slog.Logger
	Now        func() time.Time
	JitterFrac func() float64
}

type scheduledEnvelope struct {
	Kind     string `json:"kind"`
	Workload string `json:"workload"`
	Tick     string `json:"tick"`
	Fire     int64  `json:"fire,omitempty"`
	Skipped  int    `json:"skipped,omitempty"`
}

// ScheduledSessionID returns the deterministic per-tick session ID for a
// scheduled fire.
func ScheduledSessionID(workload string, tick time.Time) string {
	return fmt.Sprintf("scheduled-%s-%s", workload, tick.UTC().Format("20060102T150405Z"))
}

// ScheduledPayload builds the user-turn message for a scheduled fire.
func ScheduledPayload(workload string, tick time.Time, fireNum int64, skipped int, prompt string) string {
	body, err := json.Marshal(scheduledEnvelope{
		Kind:     "scheduled",
		Workload: workload,
		Tick:     tick.UTC().Format(time.RFC3339),
		Fire:     fireNum,
		Skipped:  skipped,
	})
	if err != nil {
		body = []byte(`{"kind":"scheduled"}`)
	}
	text := fmt.Sprintf("SCHEDULED %s", string(body))
	if strings.TrimSpace(prompt) != "" {
		text += "\n\n" + prompt
	}
	return text
}

// nextTick returns the first cadence point strictly after `after`:
// anchor + k*interval for the smallest such k. An `after` before the
// anchor yields the anchor itself.
func nextTick(anchor time.Time, interval time.Duration, after time.Time) time.Time {
	if interval <= 0 {
		return time.Time{}
	}
	elapsed := after.Sub(anchor)
	if elapsed < 0 {
		return anchor
	}
	k := elapsed/interval + 1
	return anchor.Add(k * interval)
}

// ticksSkipped counts the cadence points in (last, now] and returns the
// latest of them.
func ticksSkipped(last time.Time, interval time.Duration, now time.Time) (int, time.Time) {
	if interval <= 0 || last.IsZero() || !now.After(last) {
		return 0, last
	}
	n := int(now.Sub(last) / interval)
	if n <= 0 {
		return 0, last
	}
	return n, last.Add(time.Duration(n) * interval)
}

type tickPlan struct {
	Tick           time.Time
	FireAt         time.Time
	Skipped        int
	SkippedFrom    time.Time
	SkippedThrough time.Time
}

// ScheduledTrigger drives one workload's anchored periodic cadence.
type ScheduledTrigger struct {
	handle     *eventlog.Handle
	appName    string
	workload   string
	interval   time.Duration
	jitter     time.Duration
	prompt     string
	fire       ScheduledFireFunc
	preFire    ScheduledPreFireFunc
	leaseLost  <-chan struct{}
	isDraining func() bool
	logger     *slog.Logger

	now        func() time.Time
	jitterFrac func() float64

	mu     sync.Mutex
	anchor time.Time
	last   time.Time
	fires  int64

	running  atomic.Bool
	stopOnce sync.Once
	stopping chan struct{}
}

// NewScheduledTrigger validates cfg, clamps Jitter to <= Interval/2, and
// constructs a ScheduledTrigger.
func NewScheduledTrigger(cfg ScheduledTriggerConfig) (*ScheduledTrigger, error) {
	if cfg.Interval <= 0 {
		return nil, errors.New("compose: NewScheduledTrigger: interval must be > 0")
	}
	if strings.TrimSpace(cfg.Workload) == "" {
		return nil, errors.New("compose: NewScheduledTrigger: workload name is required")
	}
	jitter := cfg.Jitter
	if jitter < 0 {
		jitter = 0
	}
	if maxJitter := cfg.Interval / 2; jitter > maxJitter {
		jitter = maxJitter
	}
	appName := cfg.AppName
	if appName == "" {
		appName = cfg.Workload
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	jitterFn := cfg.JitterFrac
	if jitterFn == nil {
		jitterFn = rand.Float64
	}
	return &ScheduledTrigger{
		handle:     cfg.Handle,
		appName:    appName,
		workload:   cfg.Workload,
		interval:   cfg.Interval,
		jitter:     jitter,
		prompt:     cfg.Prompt,
		fire:       cfg.Fire,
		preFire:    cfg.PreFire,
		leaseLost:  cfg.LeaseLost,
		isDraining: cfg.IsDraining,
		logger:     logger,
		now:        nowFn,
		jitterFrac: jitterFn,
		stopping:   make(chan struct{}),
	}, nil
}

// Seed loads the persisted cadence from the durable eventlog or anchors a new
// cadence at t.now() and writes it before the first fire.
func (t *ScheduledTrigger) Seed(ctx context.Context) error {
	return t.seed(ctx)
}

func (t *ScheduledTrigger) seed(ctx context.Context) error {
	if t.handle != nil {
		rec, err := t.handle.Schedule(ctx, t.appName, t.workload)
		if err != nil {
			t.logger.Warn("scheduled trigger could not read persisted cadence; re-anchoring",
				"workload", t.workload, "error", err.Error())
		} else if rec != nil {
			t.mu.Lock()
			t.anchor = rec.Anchor.UTC()
			t.last = rec.LastTick.UTC()
			t.fires = rec.Fires
			if t.last.IsZero() {
				t.last = t.anchor
			}
			last := t.last
			anchor := t.anchor
			t.mu.Unlock()
			t.logger.Info("scheduled trigger resumed its persisted cadence",
				"workload", t.workload, "anchor", anchor.Format(time.RFC3339Nano),
				"interval", t.interval.String(), "jitter", t.jitter.String(),
				"last_tick", last.Format(time.RFC3339Nano), "fires", rec.Fires)
			return nil
		}
	}
	now := t.now().UTC()
	t.mu.Lock()
	t.anchor = now
	t.last = now
	t.fires = 0
	t.mu.Unlock()
	t.logger.Info("scheduled trigger anchored",
		"workload", t.workload, "anchor", now.Format(time.RFC3339Nano),
		"interval", t.interval.String(), "jitter", t.jitter.String(),
		"next_tick", nextTick(now, t.interval, now).Format(time.RFC3339Nano))
	return t.persist(ctx, time.Time{}, 0)
}

func (t *ScheduledTrigger) plan(now time.Time) tickPlan {
	t.mu.Lock()
	defer t.mu.Unlock()
	var p tickPlan
	after := now.UTC()
	if n, through := ticksSkipped(t.last, t.interval, after); n > 0 {
		p.Skipped, p.SkippedFrom, p.SkippedThrough = n, t.last.Add(t.interval), through
		t.last = through
	}
	if t.last.After(after) {
		after = t.last
	}
	p.Tick = nextTick(t.anchor, t.interval, after)
	p.FireAt = p.Tick.Add(t.jitterOffset())
	return p
}

func (t *ScheduledTrigger) jitterOffset() time.Duration {
	if t.jitter <= 0 {
		return 0
	}
	return time.Duration(t.jitterFrac() * float64(t.jitter))
}

// Run seeds the cadence and runs the periodic trigger loop until ctx is
// cancelled, LeaseLost closes, or Stop is called.
func (t *ScheduledTrigger) Run(ctx context.Context) {
	t.mu.Lock()
	unanchored := t.anchor.IsZero()
	t.mu.Unlock()
	if unanchored {
		if err := t.seed(ctx); err != nil {
			t.logger.Warn("scheduled trigger could not persist initial anchor",
				"workload", t.workload, "error", err.Error())
		}
	}
	for {
		p := t.plan(t.now())
		if p.Skipped > 0 {
			t.logger.Info("scheduled ticks skipped rather than caught up",
				"workload", t.workload, "ticks", p.Skipped,
				"from", p.SkippedFrom.Format(time.RFC3339Nano),
				"through", p.SkippedThrough.Format(time.RFC3339Nano),
				"next_tick", p.Tick.Format(time.RFC3339Nano))
		}
		delay := p.FireAt.Sub(t.now())
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-t.leaseLost:
			timer.Stop()
			t.logger.Error("scheduling lease lost; stopping scheduled trigger", "workload", t.workload)
			return
		case <-t.stopping:
			timer.Stop()
			return
		case <-timer.C:
		}
		t.fireDue(ctx, p.Tick, p.Skipped)
	}
}

// Stop signals the trigger loop to exit without waiting for the current timer.
func (t *ScheduledTrigger) Stop() {
	t.stopOnce.Do(func() { close(t.stopping) })
}

func (t *ScheduledTrigger) fireDue(ctx context.Context, tick time.Time, skipped int) {
	log := t.logger.With("workload", t.workload, "tick", tick.Format(time.RFC3339Nano))
	if ctx.Err() != nil || t.aborted() || (t.isDraining != nil && t.isDraining()) {
		log.Info("scheduled fire skipped; the daemon is draining or lost the lease")
		return
	}
	if !t.running.CompareAndSwap(false, true) {
		t.mu.Lock()
		if tick.After(t.last) {
			t.last = tick
		}
		t.mu.Unlock()
		log.Warn("scheduled fire skipped; previous fire still in flight")
		return
	}
	defer t.running.Store(false)

	t.mu.Lock()
	t.last = tick
	fireNum := t.fires + 1
	t.mu.Unlock()
	sessionID := ScheduledSessionID(t.workload, tick)
	payload := ScheduledPayload(t.workload, tick, fireNum, skipped, t.prompt)

	turnCtx, cancelTurn := t.turnContext(SchedulerContext(ctx))
	defer cancelTurn()

	if t.preFire != nil {
		override, ok := t.preFire(turnCtx, tick, fireNum)
		if !ok {
			if ctx.Err() != nil || t.aborted() {
				return
			}
			log.Info("scheduled fire suppressed by pre-fire hook", "session", sessionID)
			if perr := t.persist(ctx, time.Time{}, 0); perr != nil {
				log.Warn("scheduled trigger could not persist cadence after suppressed tick",
					"error", perr.Error())
			}
			return
		}
		if override != "" {
			payload = override
		}
	}

	start := t.now()
	var err error
	if t.fire != nil {
		err = t.fire(turnCtx, ScheduledFire{
			SessionID: sessionID,
			Workload:  t.workload,
			Tick:      tick,
			FireNum:   fireNum,
			Skipped:   skipped,
			Payload:   payload,
		})
	}
	if ctx.Err() != nil || t.aborted() {
		log.Warn("scheduled fire aborted on lease loss or stop; skipping cadence persistence",
			"session", sessionID)
		return
	}
	if err != nil {
		log.Error("scheduled fire failed; the cadence continues",
			"session", sessionID, "error", err.Error())
	} else {
		log.Info("scheduled trigger fired",
			"session", sessionID,
			"caller", SchedulerIdentity,
			"took", t.now().Sub(start).String())
	}
	if perr := t.persist(ctx, start, 1); perr != nil {
		log.Warn("scheduled trigger could not persist its cadence; a restart may re-phase it",
			"error", perr.Error())
	}
}

func (t *ScheduledTrigger) turnContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
		case <-t.leaseLost:
			cancel()
		case <-t.stopping:
			cancel()
		}
	}()
	return ctx, func() {
		close(done)
		cancel()
	}
}

func (t *ScheduledTrigger) aborted() bool {
	if t.leaseHasBeenLost() {
		return true
	}
	select {
	case <-t.stopping:
		return true
	default:
		return false
	}
}

func (t *ScheduledTrigger) leaseHasBeenLost() bool {
	if t.leaseLost == nil {
		return false
	}
	select {
	case <-t.leaseLost:
		return true
	default:
		return false
	}
}

func (t *ScheduledTrigger) persist(ctx context.Context, lastFire time.Time, fired int64) error {
	t.mu.Lock()
	if t.handle == nil {
		t.fires += fired
		t.mu.Unlock()
		return nil
	}
	rec := eventlog.ScheduleRecord{
		Workload: t.workload,
		Interval: t.interval,
		Anchor:   t.anchor,
		LastTick: t.last,
		LastFire: lastFire,
	}
	baseFires := t.fires
	t.mu.Unlock()

	if prev, err := t.handle.Schedule(ctx, t.appName, t.workload); err == nil && prev != nil {
		rec.Fires = prev.Fires
		if lastFire.IsZero() {
			rec.LastFire = prev.LastFire
		}
	} else {
		rec.Fires = baseFires
	}
	rec.Fires += fired
	t.mu.Lock()
	t.fires = rec.Fires
	t.mu.Unlock()

	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduleStoreWriteTimeout)
	defer cancel()
	return t.handle.SaveSchedule(wctx, t.appName, rec)
}
