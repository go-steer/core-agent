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

// Originally derived from go-steer/mast.

package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-agent/v3/internal/monitor"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

// MonitorCollectorConfig configures a MonitorCollector (#1406).
type MonitorCollectorConfig struct {
	WorkloadName string
	AppName      string
	UserID       string
	Monitor      *config.MonitorConfig
	BasePrompt   string
	Toolsets     []tool.Toolset
	RunTool      func(ctx adkagent.Context, name string, args map[string]any) (map[string]any, error)
	EventLog     *eventlog.Handle
	Notifier     *Notifier
	Logger       *slog.Logger
	Now          func() time.Time
}

// MonitorCollector executes deterministic pre-turn collection and transition
// gating before each scheduled fire (#1406).
type MonitorCollector struct {
	workloadName string
	appName      string
	userID       string
	mon          *config.MonitorConfig
	basePrompt   string
	toolsets     []tool.Toolset
	runTool      func(ctx adkagent.Context, name string, args map[string]any) (map[string]any, error)
	eventLog     *eventlog.Handle
	notifier     *Notifier
	logger       *slog.Logger
	now          func() time.Time

	mu         sync.Mutex
	lastParsed *monitor.Set
	lastFailed bool
}

// NewMonitorCollector constructs a MonitorCollector from cfg (#1406).
func NewMonitorCollector(cfg MonitorCollectorConfig) *MonitorCollector {
	appName := strings.TrimSpace(cfg.AppName)
	if appName == "" {
		appName = "core-agent"
	}
	userID := strings.TrimSpace(cfg.UserID)
	if userID == "" {
		userID = "daemon"
	}
	workloadName := strings.TrimSpace(cfg.WorkloadName)
	if workloadName == "" {
		workloadName = appName
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	runFn := cfg.RunTool
	if runFn == nil {
		toolsets := cfg.Toolsets
		runFn = func(cctx adkagent.Context, name string, args map[string]any) (map[string]any, error) {
			return runOwnBehalf(cctx, toolsets, name, args)
		}
	}
	return &MonitorCollector{
		workloadName: workloadName,
		appName:      appName,
		userID:       userID,
		mon:          cfg.Monitor,
		basePrompt:   cfg.BasePrompt,
		toolsets:     cfg.Toolsets,
		runTool:      runFn,
		eventLog:     cfg.EventLog,
		notifier:     cfg.Notifier,
		logger:       cfg.Logger,
		now:          nowFn,
	}
}

// LastCycle returns the parsed transition set and failure status recorded by
// the most recent PreFire invocation.
func (c *MonitorCollector) LastCycle() (*monitor.Set, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastParsed, c.lastFailed
}

func (c *MonitorCollector) recordCycle(parsed *monitor.Set, failed bool) {
	c.mu.Lock()
	c.lastParsed = parsed
	c.lastFailed = failed
	c.mu.Unlock()
}

// PreFire executes the configured monitor.collect tool and evaluates
// transitions_from / wake_on gating (#1406).
//
// When PreFire returns ("", false), the caller must skip the scheduled LLM turn
// completely (spending zero LLM calls).
func (c *MonitorCollector) PreFire(ctx context.Context, tick time.Time, fireNum int64) (string, bool) {
	if c == nil || c.mon == nil || c.mon.Collect == nil || strings.TrimSpace(c.mon.Collect.Tool) == "" {
		if c == nil {
			return "", true
		}
		c.recordCycle(nil, false)
		return c.basePrompt, true
	}

	if err := ctx.Err(); err != nil {
		c.failCycle(ctx, tick, err)
		return "", false
	}

	sessionID := fmt.Sprintf("sched-%d-%s", fireNum, tick.UTC().Format(time.RFC3339))
	cctx := newCollectContext(ctx, c.appName, c.userID, sessionID)
	toolName := strings.TrimSpace(c.mon.Collect.Tool)
	res, err := c.runTool(cctx, toolName, c.mon.Collect.Args)
	if err != nil {
		c.failCycle(ctx, tick, err)
		return "", false
	}

	if strings.TrimSpace(c.mon.TransitionsFrom) == "" {
		text, err := extractToolText(res)
		if err != nil {
			c.failCycle(ctx, tick, err)
			return "", false
		}
		c.recordCycle(nil, false)
		return c.formatRawPrompt(toolName, text, tick), true
	}

	parsed, err := monitor.ParseResult(res)
	if err != nil {
		c.failCycle(ctx, tick, fmt.Errorf("monitor.transitions_from %q: %w", c.mon.TransitionsFrom, err))
		return "", false
	}

	if parsed.Degraded {
		c.failCycle(ctx, tick, fmt.Errorf("collect tool %q reported degraded=true", toolName))
		return "", false
	}

	filtered := c.filterTransitions(ctx, tick, parsed)
	c.recordCycle(&filtered, false)

	if len(filtered.Transitions) == 0 {
		if c.notifier != nil && c.notifier.isDigestDue(ctx, tick) {
			return c.formatTransitionsPrompt(filtered, tick), true
		}
		if c.notifier != nil {
			_ = c.notifier.AfterCycle(ctx, &filtered, "", false)
		}
		if c.logger != nil {
			c.logger.Info("monitoring cycle stayed quiet; skipping model turn",
				"session", sessionID)
		}
		return "", false
	}

	if c.logger != nil {
		c.logger.Info("monitoring cycle classified transitions",
			"session", sessionID,
			"transitions", len(filtered.Transitions))
	}
	return c.formatTransitionsPrompt(filtered, tick), true
}

func (c *MonitorCollector) failCycle(ctx context.Context, tick time.Time, err error) {
	c.recordCycle(&monitor.Set{Transitions: []monitor.Transition{}, Degraded: true}, true)
	if c.logger != nil {
		c.logger.Error("monitoring cycle collection failed",
			"workload", c.workloadName,
			"tick", tick.UTC().Format(time.RFC3339),
			"error", err.Error())
	}
	if c.notifier != nil {
		_ = c.notifier.AfterCycle(ctx, nil, err.Error(), true)
	}
}

func (c *MonitorCollector) filterTransitions(ctx context.Context, tick time.Time, set monitor.Set) monitor.Set {
	acked := c.loadAckedSubjects(ctx)
	out := monitor.Set{
		Transitions: []monitor.Transition{},
		Degraded:    set.Degraded,
	}
	for _, tr := range set.Transitions {
		if acked[tr.SubjectKey] {
			continue
		}
		if !c.matchesWakeOn(ctx, tick, tr) {
			continue
		}
		out.Transitions = append(out.Transitions, tr)
	}
	return out
}

func (c *MonitorCollector) loadAckedSubjects(ctx context.Context) map[string]bool {
	if c.eventLog == nil {
		return nil
	}
	recs, err := c.eventLog.Acks(ctx, c.appName, c.workloadName)
	if err != nil || len(recs) == 0 {
		return nil
	}
	out := make(map[string]bool, len(recs))
	for _, r := range recs {
		out[r.Subject] = true
	}
	return out
}

func (c *MonitorCollector) matchesWakeOn(ctx context.Context, tick time.Time, tr monitor.Transition) bool {
	if tr.Edge == "remained" && c.notifier != nil && c.notifier.remindAfter > 0 {
		if !c.notifier.isRemindDue(ctx, tr.SubjectKey, tick) {
			return false
		}
	}
	if len(c.mon.WakeOn) == 0 {
		return tr.Edge != "remained" || (c.notifier != nil && c.notifier.remindAfter > 0)
	}
	return slices.Contains(c.mon.WakeOn, tr.Edge)
}

func (c *MonitorCollector) formatRawPrompt(toolName, text string, tick time.Time) string {
	var b strings.Builder
	if strings.TrimSpace(c.basePrompt) != "" {
		b.WriteString(strings.TrimSpace(c.basePrompt))
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Collected output from %s at %s:\n%s", toolName, tick.UTC().Format(time.RFC3339), strings.TrimSpace(text))
	return b.String()
}

func (c *MonitorCollector) formatTransitionsPrompt(set monitor.Set, tick time.Time) string {
	payload := map[string]any{
		"kind":        "scheduled",
		"workload":    c.workloadName,
		"tick":        tick.UTC().Format(time.RFC3339),
		"transitions": set,
	}
	raw, _ := json.Marshal(payload)
	if strings.TrimSpace(c.basePrompt) == "" {
		return string(raw)
	}
	return strings.TrimSpace(c.basePrompt) + "\n\n" + string(raw)
}
