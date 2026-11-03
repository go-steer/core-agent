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
	"iter"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/session"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/internal/effects"
	"github.com/go-steer/core-agent/v3/internal/monitor"
	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/attach"
	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/compose"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/mcp"
	"github.com/go-steer/core-agent/v3/pkg/tools"
)

func buildMonitorAckHandler(cfg *config.Config, eventlogHandle *eventlog.Handle, toolsets []adktool.Toolset, hasMCPServers bool) (attach.MonitorAckHandler, error) {
	if cfg == nil || cfg.Monitor == nil {
		return nil, nil
	}
	if err := config.CheckMonitorCollectSurface(cfg, hasMCPServers); err != nil {
		return nil, err
	}
	if cfg.Monitor.Ack == nil && strings.TrimSpace(cfg.Monitor.TransitionsFrom) == "" {
		return nil, nil
	}
	workload := cfg.Agent.DisplayName
	if workload == "" {
		workload = "core-agent"
	}
	return compose.NewMonitorAcker(compose.MonitorAckerConfig{
		WorkloadName: workload,
		AppName:      "core-agent",
		Monitor:      cfg.Monitor,
		Toolsets:     toolsets,
		EventLog:     eventlogHandle,
	}).Ack, nil
}

func buildDaemonEffectControls(cfg *config.Config, toolsets ...adktool.Toolset) (effects.ReadOnlyPredicate, map[string]bool) {
	if cfg == nil && len(toolsets) == 0 {
		return effects.NewPredicate(nil), nil
	}
	var overrides map[string]bool
	var subNames map[string]bool
	if cfg != nil {
		if len(cfg.Tools.Policies) > 0 {
			overrides = make(map[string]bool, len(cfg.Tools.Policies))
			for _, p := range cfg.Tools.Policies {
				if p.Name != "" && p.Mutating != nil {
					overrides[p.Name] = *p.Mutating
				}
			}
		}
		if len(cfg.Subagents) > 0 {
			subNames = make(map[string]bool, len(cfg.Subagents))
			for _, sa := range cfg.Subagents {
				if sa.Name != "" {
					subNames[sa.Name] = true
				}
			}
		}
	}
	var (
		hintOnce    sync.Once
		readOnlyMap map[string]bool
	)
	var hint effects.ReadOnlyHint
	if len(toolsets) > 0 {
		hint = func(toolName string) (bool, bool) {
			hintOnce.Do(func() {
				readOnlyMap = make(map[string]bool)
				for _, ts := range toolsets {
					if ts == nil {
						continue
					}
					list, err := mcp.ListTools(context.Background(), ts)
					if err != nil {
						continue
					}
					for _, t := range list {
						if t != nil && tools.IsReadOnlyTool(t) {
							readOnlyMap[t.Name()] = true
						}
					}
				}
			})
			if readOnlyMap[toolName] {
				return true, true
			}
			return false, false
		}
	}
	basePred := effects.NewPredicateWithHints(overrides, hint)
	if len(overrides) == 0 {
		return basePred, subNames
	}
	return func(name string) effects.Class {
		if _, ok := overrides[name]; ok || effects.IsBuiltinOrControl(name) {
			return basePred(name)
		}
		bestLen := 0
		var bestMutating bool
		for policyName, mutating := range overrides {
			if len(policyName) > bestLen && strings.HasSuffix(name, "_"+policyName) {
				bestLen = len(policyName)
				bestMutating = mutating
			}
		}
		if bestLen > 0 {
			if bestMutating {
				return effects.ClassMutating
			}
			return effects.ClassReadOnly
		}
		return basePred(name)
	}, subNames
}

func startMultiSessionAutoContinue(
	retryCtx context.Context,
	wg *sync.WaitGroup,
	cfg *config.Config,
	deps *compose.SessionFactoryDeps,
	enabled, retry bool,
	retryInterval time.Duration,
) {
	if !enabled || deps == nil || cfg == nil || !cfg.Attach.MultiSession.Enabled {
		return
	}
	maxPerBoot := 0
	if ac := cfg.Agent.AutoContinue; ac != nil {
		maxPerBoot = ac.MaxPerBoot
	}
	snapshot := *deps
	go compose.AutoContinueBootScan(snapshot, maxPerBoot)
	if retry {
		wg.Add(1)
		go func() {
			defer wg.Done()
			compose.AutoContinueRetryLoop(retryCtx, retryInterval, func() {
				compose.AutoContinueRetryScan(snapshot, maxPerBoot)
			}, snapshot.LeaseLost)
		}()
	}
}

func hasMCPToolsets(toolsets []adktool.Toolset) bool {
	for _, ts := range toolsets {
		if ts != nil && ts.Name() != "skills" {
			return true
		}
	}
	return false
}

func startSingleSessionScheduleAndAutoContinue(
	ctx, retryCtx context.Context,
	wg *sync.WaitGroup,
	cfg *config.Config,
	eventlogHandle *eventlog.Handle,
	a *agent.Agent,
	toolsets []adktool.Toolset,
	factoryDeps *compose.SessionFactoryDeps,
	autoContinueEnabled, autoContinueRetry bool,
	autoContinueFreshness, autoContinueRetryInterval time.Duration,
) (*daemonScheduleHandle, error) {
	var schedHandle *daemonScheduleHandle
	var leaseLost <-chan struct{}
	if cfg != nil && cfg.Schedule != nil {
		workload := cfg.Agent.DisplayName
		if workload == "" {
			workload = a.AppName()
		}
		var preFire compose.ScheduledPreFireFunc
		var notifier *compose.Notifier
		var collector *compose.MonitorCollector
		if cfg.Monitor != nil {
			if err := config.CheckMonitorCollectSurface(cfg, hasMCPToolsets(toolsets)); err != nil {
				return nil, err
			}
			n, err := compose.NewNotifier(compose.NotifierConfig{
				WorkloadName: workload,
				AppName:      a.AppName(),
				Notify:       cfg.Monitor.Notify,
				WakeOn:       cfg.Monitor.WakeOn,
				EventLog:     eventlogHandle,
			})
			if err != nil {
				return nil, fmt.Errorf("monitor notify: %w", err)
			}
			notifier = n
			if cfg.Monitor.Collect != nil {
				collector = compose.NewMonitorCollector(compose.MonitorCollectorConfig{
					WorkloadName: workload,
					AppName:      a.AppName(),
					UserID:       a.UserID(),
					Monitor:      cfg.Monitor,
					BasePrompt:   cfg.Schedule.Prompt,
					Toolsets:     toolsets,
					EventLog:     eventlogHandle,
					Notifier:     notifier,
				})
				preFire = collector.PreFire
			}
		}
		var localDeps compose.SessionFactoryDeps
		if factoryDeps != nil {
			localDeps = *factoryDeps
		}
		waiter := &scheduledTurnWaiter{}
		fireFn := func(fireCtx context.Context, fire compose.ScheduledFire) error {
			if cfg.Attach.MultiSession.Enabled && factoryDeps != nil {
				return runMultiSessionScheduledTurn(fireCtx, localDeps, fire, collector, notifier)
			}
			return waiter.runScheduledTurn(fireCtx, a, fire, collector, notifier)
		}
		h, err := startDaemonSchedule(ctx, daemonScheduleDeps{
			EventLog: eventlogHandle,
			AppName:  a.AppName(),
			Workload: workload,
			Schedule: cfg.Schedule,
			PreFire:  preFire,
			Fire:     fireFn,
		})
		if err != nil {
			return nil, fmt.Errorf("schedule: %w", err)
		}
		h.waiter = waiter
		schedHandle = h
		if !h.Drives {
			fmt.Fprintln(os.Stderr, "core-agent: schedule: another replica holds the scheduling lease; standing down passively")
			return schedHandle, nil
		}
		leaseLost = h.Lost()
	}
	if cfg != nil && cfg.Attach.MultiSession.Enabled {
		if factoryDeps != nil {
			depsCopy := *factoryDeps
			depsCopy.LeaseLost = leaseLost
			startMultiSessionAutoContinue(retryCtx, wg, cfg, &depsCopy, autoContinueEnabled, autoContinueRetry, autoContinueRetryInterval)
		}
		return schedHandle, nil
	}
	if autoContinueEnabled {
		isReadOnly, subNames := buildDaemonEffectControls(cfg, toolsets...)
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

func runMultiSessionScheduledTurn(
	fireCtx context.Context,
	deps compose.SessionFactoryDeps,
	fire compose.ScheduledFire,
	collector *compose.MonitorCollector,
	notifier *compose.Notifier,
) error {
	caller := auth.Caller{Identity: compose.SchedulerIdentity}
	sid := strings.TrimSpace(fire.SessionID)
	if sid == "" {
		sid = compose.ScheduledSessionID(fire.Workload, fire.Tick)
	}
	var ag *agent.Agent
	if deps.Registry != nil {
		for _, entry := range deps.Registry.List() {
			if entry.SessionID == sid {
				if ap, ok := entry.Agent.(compose.AgentProvider); ok {
					ag = ap.Agent()
				}
				break
			}
		}
	}
	if ag == nil {
		ad, cancelOnEvict, err := compose.ReproduceAgent(deps, caller, sid, "created")
		if err != nil {
			return err
		}
		if deps.Registry != nil {
			if _, rerr := deps.Registry.RegisterOwnedWithCancel(ad, caller.Identity, cancelOnEvict); rerr != nil && cancelOnEvict != nil {
				cancelOnEvict()
			}
		} else if cancelOnEvict != nil {
			defer cancelOnEvict()
		}
		ag = ad.Agent()
	}
	fullText, turnErr := consumeScheduledTurnEvents(ag.Run(fireCtx, fire.Payload))
	if notifier != nil {
		var parsed *monitor.Set
		if collector != nil {
			parsed, _ = collector.LastCycle()
		}
		_ = notifier.AfterCycle(fireCtx, parsed, fullText, turnErr != nil)
	}
	return turnErr
}

func consumeScheduledTurnEvents(seq iter.Seq2[*session.Event, error]) (string, error) {
	var nonPartial strings.Builder
	var partial strings.Builder
	for ev, err := range seq {
		if err != nil {
			if nonPartial.Len() > 0 {
				return nonPartial.String(), err
			}
			return partial.String(), err
		}
		if ev == nil || ev.Content == nil || ev.Content.Role != genai.RoleModel {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.Text != "" {
				if ev.Partial {
					partial.WriteString(p.Text)
				} else {
					nonPartial.WriteString(p.Text)
				}
			}
		}
	}
	if nonPartial.Len() > 0 {
		return nonPartial.String(), nil
	}
	return partial.String(), nil
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

type scheduledTurnResult struct {
	text string
	err  error
}

type scheduledTurnWaiter struct {
	mu       sync.Mutex
	out      chan scheduledTurnResult
	promptID string
	agent    *agent.Agent
}

func (w *scheduledTurnWaiter) runScheduledTurn(
	fireCtx context.Context,
	a *agent.Agent,
	fire compose.ScheduledFire,
	collector *compose.MonitorCollector,
	notifier *compose.Notifier,
) error {
	ch := make(chan scheduledTurnResult, 1)
	w.mu.Lock()
	promptID, err := a.InjectAsContextWithID(fireCtx, fire.Payload, auth.Caller{Identity: compose.SchedulerIdentity})
	if err != nil {
		w.mu.Unlock()
		return err
	}
	w.out = ch
	w.promptID = promptID
	w.agent = a
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if w.out == ch {
			w.out = nil
			w.promptID = ""
			w.agent = nil
		}
		w.mu.Unlock()
	}()
	select {
	case res := <-ch:
		if notifier != nil {
			var parsed *monitor.Set
			if collector != nil {
				parsed, _ = collector.LastCycle()
			}
			notifyCtx := fireCtx
			var cancel context.CancelFunc
			if notifyCtx.Err() != nil {
				notifyCtx, cancel = context.WithTimeout(context.WithoutCancel(fireCtx), 10*time.Second)
				defer cancel()
			}
			_ = notifier.AfterCycle(notifyCtx, parsed, res.text, res.err != nil)
		}
		return res.err
	case <-fireCtx.Done():
		if !a.RemoveInboxID(promptID) {
			a.Interrupt()
		}
		if notifier != nil {
			notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(fireCtx), 10*time.Second)
			defer cancel()
			var parsed *monitor.Set
			if collector != nil {
				parsed, _ = collector.LastCycle()
			}
			_ = notifier.AfterCycle(notifyCtx, parsed, fireCtx.Err().Error(), true)
		}
		return fireCtx.Err()
	}
}

// daemonScheduleHandle holds the active scheduling lease and optional
// scheduled trigger started by startDaemonSchedule.
type daemonScheduleHandle struct {
	Lease   *compose.SchedulingLease
	Drives  bool
	Trigger *compose.ScheduledTrigger
	done    chan struct{}
	waiter  *scheduledTurnWaiter
}

// AfterTurn delivers the completed wake-loop turn outcome to any waiting
// scheduled fire (#1406).
func (h *daemonScheduleHandle) AfterTurn(text string, turnErr error) {
	if h == nil || h.waiter == nil {
		return
	}
	h.waiter.mu.Lock()
	ch := h.waiter.out
	if ch == nil {
		h.waiter.mu.Unlock()
		return
	}
	ag := h.waiter.agent
	pid := h.waiter.promptID
	if ag != nil && pid != "" && !ag.LastTurnDrainedID(pid) && ag.InboxContainsID(pid) && (turnErr == nil || ag.LastTurnReachedDrain()) {
		h.waiter.mu.Unlock()
		return
	}
	h.waiter.out = nil
	h.waiter.promptID = ""
	h.waiter.agent = nil
	h.waiter.mu.Unlock()
	select {
	case ch <- scheduledTurnResult{text: text, err: turnErr}:
	default:
	}
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
	timeout, terr := deps.Schedule.EffectiveTimeout()
	if ierr != nil || jerr != nil || terr != nil {
		lease.Release()
		return nil, errors.Join(ierr, jerr, terr)
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
		MaxFires:  deps.Schedule.MaxFires,
		Timeout:   timeout,
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

func servesSessionFactory(cfg *config.Config, token string) bool {
	return cfg.Attach.MultiSession.Enabled || cfg.A2A.Expose || token != ""
}

func servesSessionCreation(cfg *config.Config, token string) bool {
	return cfg.Attach.MultiSession.Enabled || token != ""
}
