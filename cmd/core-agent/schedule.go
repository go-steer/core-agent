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

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/go-steer/core-agent/v3/internal/effects"
	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/compose"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

func buildDaemonEffectControls(cfg *config.Config) (effects.ReadOnlyPredicate, map[string]bool) {
	if cfg == nil {
		return effects.NewPredicate(nil), nil
	}
	var overrides map[string]bool
	if len(cfg.Tools.Policies) > 0 {
		overrides = make(map[string]bool, len(cfg.Tools.Policies))
		for _, p := range cfg.Tools.Policies {
			if p.Name != "" && p.Mutating != nil {
				overrides[p.Name] = *p.Mutating
			}
		}
	}
	var subNames map[string]bool
	if len(cfg.Subagents) > 0 {
		subNames = make(map[string]bool, len(cfg.Subagents))
		for _, sa := range cfg.Subagents {
			if sa.Name != "" {
				subNames[sa.Name] = true
			}
		}
	}
	return effects.NewPredicate(overrides), subNames
}

func startSingleSessionScheduleAndAutoContinue(
	ctx, retryCtx context.Context,
	wg *sync.WaitGroup,
	cfg *config.Config,
	eventlogHandle *eventlog.Handle,
	a *agent.Agent,
	autoContinueEnabled, autoContinueRetry bool,
	autoContinueFreshness, autoContinueRetryInterval time.Duration,
) (*daemonScheduleHandle, error) {
	var schedHandle *daemonScheduleHandle
	var leaseLost <-chan struct{}
	if cfg != nil && cfg.Schedule != nil {
		h, err := startDaemonSchedule(ctx, daemonScheduleDeps{
			EventLog: eventlogHandle,
			AppName:  a.AppName(),
			Workload: a.AppName(),
			Schedule: cfg.Schedule,
			Fire: func(_ context.Context, fire compose.ScheduledFire) error {
				return a.InjectAs(fire.Payload, auth.Caller{Identity: compose.SchedulerIdentity})
			},
		})
		if err != nil {
			return nil, fmt.Errorf("schedule: %w", err)
		}
		schedHandle = h
		if !h.Drives {
			fmt.Fprintln(os.Stderr, "core-agent: schedule: another replica holds the scheduling lease; standing down passively")
			return schedHandle, nil
		}
		leaseLost = h.Lost()
	}
	if autoContinueEnabled && (cfg == nil || !cfg.Attach.MultiSession.Enabled) {
		isReadOnly, subNames := buildDaemonEffectControls(cfg)
		acCfg := compose.AutoContinueConfig{
			EventLog:      eventlogHandle,
			Freshness:     autoContinueFreshness,
			IsReadOnly:    isReadOnly,
			SubAgentNames: subNames,
			LeaseLost:     leaseLost,
		}
		compose.AutoContinueStartupSessionWithConfig(ctx, acCfg, a)
		if autoContinueRetry {
			startupAgent := a
			wg.Add(1)
			go func() {
				defer wg.Done()
				compose.AutoContinueRetryLoop(retryCtx, autoContinueRetryInterval, func() {
					compose.AutoContinueStartupRetryWithConfig(retryCtx, acCfg, startupAgent)
				}, leaseLost)
			}()
		}
	}
	return schedHandle, nil
}

// daemonScheduleDeps wires the fleet scheduling lease and the in-process
// scheduled trigger for a daemon configured with cfg.Schedule (#1405).
type daemonScheduleDeps struct {
	EventLog *eventlog.Handle
	AppName  string
	Workload string
	Schedule *config.ScheduleConfig
	Logger   *slog.Logger
	Fire     compose.ScheduledFireFunc
	PreFire  compose.ScheduledPreFireFunc
}

// daemonScheduleHandle holds the active scheduling lease and optional
// scheduled trigger started by startDaemonSchedule.
type daemonScheduleHandle struct {
	Lease   *compose.SchedulingLease
	Drives  bool
	Trigger *compose.ScheduledTrigger
	done    chan struct{}
}

// Lost returns a channel that closes if the fleet scheduling lease is lost,
// or nil when no lease is held.
func (h *daemonScheduleHandle) Lost() <-chan struct{} {
	if h == nil || h.Lease == nil {
		return nil
	}
	return h.Lease.Lost()
}

// Stop halts the scheduled trigger loop (if running), waits for it to exit,
// and releases the fleet scheduling lease.
func (h *daemonScheduleHandle) Stop() {
	if h == nil {
		return
	}
	if h.Trigger != nil {
		h.Trigger.Stop()
	}
	if h.done != nil {
		<-h.done
	}
	if h.Lease != nil {
		h.Lease.Release()
	}
}

// startDaemonSchedule acquires the fleet scheduling lease and, when this
// replica holds the lease and a schedule is configured, seeds and starts the
// in-process scheduled trigger (#1405).
func startDaemonSchedule(ctx context.Context, deps daemonScheduleDeps) (*daemonScheduleHandle, error) {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	appName := deps.AppName
	if appName == "" {
		appName = "core-agent"
	}
	workload := deps.Workload
	if workload == "" {
		workload = appName
	}

	lease, drives := compose.AcquireSchedulingLease(ctx, deps.EventLog, appName, logger)
	handle := &daemonScheduleHandle{
		Lease:  lease,
		Drives: drives,
	}
	if !drives || deps.Schedule == nil {
		return handle, nil
	}

	interval, ierr := deps.Schedule.EffectiveInterval()
	jitter, jerr := deps.Schedule.EffectiveJitter()
	if ierr != nil || jerr != nil {
		lease.Release()
		return nil, errors.Join(ierr, jerr)
	}

	var lost <-chan struct{}
	if lease != nil {
		lost = lease.Lost()
	}
	trigger, err := compose.NewScheduledTrigger(compose.ScheduledTriggerConfig{
		Handle:    deps.EventLog,
		AppName:   appName,
		Workload:  workload,
		Interval:  interval,
		Jitter:    jitter,
		Prompt:    deps.Schedule.Prompt,
		LeaseLost: lost,
		Logger:    logger,
		PreFire:   deps.PreFire,
		Fire:      deps.Fire,
	})
	if err != nil {
		lease.Release()
		return nil, err
	}
	if err := trigger.Seed(ctx); err != nil {
		logger.Warn("scheduled trigger could not persist its initial anchor", "workload", workload, "error", err.Error())
	}
	handle.Trigger = trigger
	if deps.Fire != nil {
		handle.done = make(chan struct{})
		go func() {
			defer close(handle.done)
			trigger.Run(ctx)
		}()
	}
	return handle, nil
}
