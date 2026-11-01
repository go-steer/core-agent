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

// Originally derived from go-steer/mast@d239f89 (internal/cli/autoresume.go).

package compose

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/internal/effects"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

const (
	// AutoResumeMaxAttempts is the maximum number of auto-resume attempts
	// allowed for a single session within AutoResumeAttemptWindow before the
	// M2 crash-loop breaker trips.
	AutoResumeMaxAttempts = 3

	// AutoResumeAttemptWindow is the rolling window for per-session M2
	// crash-loop attempt counting.
	AutoResumeAttemptWindow = 10 * time.Minute

	// AutoResumeBootCap bounds the number of sessions a single boot pass will
	// auto-resume, ordered oldest-first.
	AutoResumeBootCap = 50

	autoResumeOpsSuffix   = ":mast-ops"
	autoResumeSubInfix    = ":sub:"
	autoResumeAttemptsKey = "mast_autoresume_attempts"
	autoResumeLastKey     = "mast_autoresume_last_attempt"
)

// AutoResumeFunc drives one resumed continuation turn for sessionID.
// When repair is non-nil, it carries the synthetic FunctionResponse parts
// repairing dangling read-only tool calls from the trailing event.
type AutoResumeFunc func(ctx context.Context, userID, sessionID string, repair *genai.Content) error

// AutoResumeConfig configures a boot-time AutoResumer pass.
type AutoResumeConfig struct {
	Handle          *eventlog.Handle
	AppName         string
	UserID          string
	IsReadOnly      effects.ReadOnlyPredicate
	SubAgentNames   map[string]bool
	FreshnessWindow time.Duration
	MaxAttempts     int
	AttemptWindow   time.Duration
	MaxPerBoot      int
	LeaseLost       <-chan struct{}
	Logger          *slog.Logger
	Resume          AutoResumeFunc
	// BeforeResume is an optional test hook invoked after scan and before the
	// M1 supersession check on each candidate.
	BeforeResume func(sessionID string)
}

// AutoResumer scans durable sessions on boot and drives continuation turns for
// eligible interrupted sessions behind the fleet scheduling lease.
type AutoResumer struct {
	cfg AutoResumeConfig
	now func() time.Time
}

type autoResumeCandidate struct {
	userID        string
	sessionID     string
	interruptedAt time.Time
	lastEventTime time.Time
	eventCount    int
	repairable    []effects.DanglingIntent
}

// NewAutoResumer constructs a boot-time AutoResumer.
func NewAutoResumer(cfg AutoResumeConfig) *AutoResumer {
	if cfg.AppName == "" {
		cfg.AppName = "core-agent"
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = AutoResumeMaxAttempts
	}
	if cfg.AttemptWindow <= 0 {
		cfg.AttemptWindow = AutoResumeAttemptWindow
	}
	if cfg.MaxPerBoot <= 0 {
		cfg.MaxPerBoot = AutoResumeBootCap
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &AutoResumer{
		cfg: cfg,
		now: time.Now,
	}
}

// Run executes one sequential boot auto-resume pass and returns the number of
// sessions resumed. It stops immediately if ctx is cancelled or LeaseLost fires.
func (a *AutoResumer) Run(ctx context.Context) int {
	if a.cfg.Handle == nil || a.cfg.Handle.Service == nil || a.cfg.Resume == nil {
		return 0
	}
	if a.stopped(ctx) {
		return 0
	}
	candidates, err := a.scanCandidates(ctx)
	if err != nil {
		a.cfg.Logger.Error("auto-resume boot scan failed", "error", err.Error())
		return 0
	}
	if len(candidates) == 0 {
		return 0
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].interruptedAt.Equal(candidates[j].interruptedAt) {
			return candidates[i].interruptedAt.Before(candidates[j].interruptedAt)
		}
		return candidates[i].sessionID < candidates[j].sessionID
	})
	if len(candidates) > a.cfg.MaxPerBoot {
		candidates = candidates[:a.cfg.MaxPerBoot]
	}

	resumed := 0
	for _, c := range candidates {
		if a.stopped(ctx) {
			a.cfg.Logger.Info("auto-resume pass stopped by context or lease loss")
			break
		}
		if a.resumeCandidate(ctx, c) {
			resumed++
		}
	}
	return resumed
}

func (a *AutoResumer) stopped(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	select {
	case <-a.cfg.LeaseLost:
		return true
	default:
		return false
	}
}

func (a *AutoResumer) scanCandidates(ctx context.Context) ([]autoResumeCandidate, error) {
	resp, err := a.cfg.Handle.Service.List(ctx, &session.ListRequest{
		AppName: a.cfg.AppName,
		UserID:  a.cfg.UserID,
	})
	if err != nil {
		return nil, err
	}
	var out []autoResumeCandidate
	for _, s := range resp.Sessions {
		if a.stopped(ctx) {
			break
		}
		if isSkippedSessionID(s.ID()) {
			continue
		}
		if c, ok := a.inspectSession(ctx, s.UserID(), s.ID()); ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func isSkippedSessionID(sessionID string) bool {
	return strings.HasSuffix(sessionID, autoResumeOpsSuffix) ||
		strings.Contains(sessionID, autoResumeSubInfix) ||
		sessionID == schedulingLeaseSessionID
}

func (a *AutoResumer) inspectSession(ctx context.Context, userID, sessionID string) (autoResumeCandidate, bool) {
	full, err := a.cfg.Handle.Service.Get(ctx, &session.GetRequest{
		AppName:   a.cfg.AppName,
		UserID:    userID,
		SessionID: sessionID,
	})
	if err != nil || full == nil || full.Session == nil {
		return autoResumeCandidate{}, false
	}
	events := full.Session.Events()
	if len(eventlog.ScanPendingParks(events)) > 0 {
		return autoResumeCandidate{}, false
	}
	lastEv, unhandled := lastEventIsUnhandledCall(events, a.cfg.SubAgentNames)
	if !unhandled {
		return autoResumeCandidate{}, false
	}
	interruptedAt := lastEv.Timestamp
	if interruptedAt.IsZero() {
		interruptedAt = full.Session.LastUpdateTime()
	}
	if a.cfg.FreshnessWindow > 0 && !interruptedAt.IsZero() && a.now().Sub(interruptedAt) > a.cfg.FreshnessWindow {
		return autoResumeCandidate{}, false
	}
	scan := effects.ScanDangling(events, a.cfg.IsReadOnly, a.cfg.SubAgentNames)
	if len(scan.Mutating) > 0 {
		ackedAt, ok := a.cfg.Handle.EffectsAckedAt(ctx, a.cfg.AppName, userID, sessionID)
		if !ok || len(scan.Unacked(ackedAt)) > 0 {
			a.cfg.Logger.Warn("auto-resume skipped: unacknowledged mutating tool call (ambiguous effect)",
				"session", sessionID, "dangling_mutating", len(scan.Mutating))
			return autoResumeCandidate{}, false
		}
	}
	if len(scan.Deferred) > 0 {
		return autoResumeCandidate{}, false
	}
	for _, d := range scan.Repairable {
		if d.EventIndex != scan.LastCallEventIndex {
			return autoResumeCandidate{}, false
		}
	}
	count, lastTime := snapshotCountAndTime(events)
	return autoResumeCandidate{
		userID:        userID,
		sessionID:     sessionID,
		interruptedAt: interruptedAt,
		lastEventTime: lastTime,
		eventCount:    count,
		repairable:    scan.Repairable,
	}, true
}

func (a *AutoResumer) resumeCandidate(ctx context.Context, c autoResumeCandidate) bool {
	if a.cfg.BeforeResume != nil {
		a.cfg.BeforeResume(c.sessionID)
	}
	if a.stopped(ctx) {
		return false
	}
	attempts, lastAttempt := a.attempts(ctx, c.userID, c.sessionID)
	if attempts >= a.cfg.MaxAttempts && a.now().Sub(lastAttempt) < a.cfg.AttemptWindow {
		a.cfg.Logger.Warn("auto-resume skipped: restart-loop breaker tripped",
			"session", c.sessionID, "attempts", attempts)
		return false
	}
	if sessionUpdatedSince(ctx, a.cfg.Handle, a.cfg.AppName, c.userID, c.sessionID, c.eventCount, c.lastEventTime) {
		a.cfg.Logger.Info("auto-resume skipped: session superseded since scan", "session", c.sessionID)
		return false
	}
	if err := a.recordAttempt(ctx, c.userID, c.sessionID); err != nil {
		a.cfg.Logger.Error("auto-resume: failed to record attempt", "session", c.sessionID, "error", err.Error())
		return false
	}
	var repair *genai.Content
	if len(c.repairable) > 0 {
		repair = repairContent(c.repairable)
		if err := a.appendRepair(ctx, c.userID, c.sessionID, repair); err != nil {
			a.cfg.Logger.Error("auto-resume: failed to append repair content", "session", c.sessionID, "error", err.Error())
			return false
		}
	}
	if err := a.cfg.Resume(ctx, c.userID, c.sessionID, repair); err != nil {
		a.cfg.Logger.Error("auto-resume turn failed", "session", c.sessionID, "error", err.Error())
		return false
	}
	return true
}

func (a *AutoResumer) appendRepair(ctx context.Context, userID, sessionID string, repair *genai.Content) error {
	resp, err := a.cfg.Handle.Service.Get(ctx, &session.GetRequest{
		AppName:   a.cfg.AppName,
		UserID:    userID,
		SessionID: sessionID,
	})
	if err != nil {
		return err
	}
	ev := session.NewEvent(ctx, "autoresume-repair")
	ev.Author = "user"
	ev.Timestamp = a.now().UTC()
	ev.Content = repair
	return a.cfg.Handle.Service.AppendEvent(ctx, resp.Session, ev)
}

func (a *AutoResumer) attempts(ctx context.Context, userID, sessionID string) (int, time.Time) {
	opsID := sessionID + autoResumeOpsSuffix
	resp, err := a.cfg.Handle.Service.Get(ctx, &session.GetRequest{
		AppName:   a.cfg.AppName,
		UserID:    userID,
		SessionID: opsID,
	})
	if err != nil || resp == nil || resp.Session == nil {
		return 0, time.Time{}
	}
	var count int
	if v, err := resp.Session.State().Get(autoResumeAttemptsKey); err == nil {
		if s, ok := v.(string); ok && s != "" {
			_, _ = fmt.Sscanf(s, "%d", &count)
		}
	}
	var last time.Time
	if v, err := resp.Session.State().Get(autoResumeLastKey); err == nil {
		if s, ok := v.(string); ok && s != "" {
			if parsed, perr := time.Parse(time.RFC3339Nano, s); perr == nil {
				last = parsed
			}
		}
	}
	return count, last
}

func (a *AutoResumer) recordAttempt(ctx context.Context, userID, sessionID string) error {
	now := a.now().UTC()
	count, last := a.attempts(ctx, userID, sessionID)
	if !last.IsZero() && now.Sub(last) >= a.cfg.AttemptWindow {
		count = 0
	}
	count++
	opsID := sessionID + autoResumeOpsSuffix
	var sess session.Session
	if resp, err := a.cfg.Handle.Service.Get(ctx, &session.GetRequest{
		AppName:   a.cfg.AppName,
		UserID:    userID,
		SessionID: opsID,
	}); err == nil && resp != nil && resp.Session != nil {
		sess = resp.Session
	} else {
		created, cerr := a.cfg.Handle.Service.Create(ctx, &session.CreateRequest{
			AppName:   a.cfg.AppName,
			UserID:    userID,
			SessionID: opsID,
		})
		if cerr != nil {
			return cerr
		}
		sess = created.Session
	}
	ev := session.NewEvent(ctx, "autoresume-attempt")
	ev.Author = "daemon"
	ev.Timestamp = now
	ev.Content = genai.NewContentFromText(fmt.Sprintf("boot-time auto-resume attempt %d", count), genai.RoleUser)
	ev.Actions.StateDelta = map[string]any{
		autoResumeAttemptsKey: fmt.Sprintf("%d", count),
		autoResumeLastKey:     now.Format(time.RFC3339Nano),
	}
	return a.cfg.Handle.Service.AppendEvent(ctx, sess, ev)
}

// lastEventIsUnhandledCall reports whether the trailing visible event in events
// represents an unfinished turn (a user turn/tool response awaiting a model
// reply, or a model turn containing a FunctionCall), excluding sub-agent turns.
func lastEventIsUnhandledCall(events session.Events, subAgentNames map[string]bool) (*session.Event, bool) {
	if events == nil {
		return nil, false
	}
	var last *session.Event
	for ev := range events.All() {
		if ev == nil || ev.Content == nil || ev.Content.Role == "" {
			continue
		}
		last = ev
	}
	if last == nil {
		return nil, false
	}
	if subAgentNames[last.Author] {
		return last, false
	}
	if last.Content.Role == genai.RoleUser {
		return last, true
	}
	if last.Content.Role == genai.RoleModel {
		for _, p := range last.Content.Parts {
			if p != nil && p.FunctionCall != nil {
				return last, true
			}
		}
	}
	return last, false
}

func snapshotCountAndTime(events session.Events) (int, time.Time) {
	if events == nil {
		return 0, time.Time{}
	}
	count := 0
	var lastTime time.Time
	for ev := range events.All() {
		if ev == nil {
			continue
		}
		count++
		if !ev.Timestamp.IsZero() {
			lastTime = ev.Timestamp
		}
	}
	return count, lastTime
}

// sessionUpdatedSince reports whether sessionID has advanced (or acquired a
// pending park) since the scan snapshot (wantCount, wantLast).
func sessionUpdatedSince(ctx context.Context, h *eventlog.Handle, appName, userID, sessionID string, wantCount int, wantLast time.Time) bool {
	if h == nil || h.Service == nil {
		return true
	}
	full, err := h.Service.Get(ctx, &session.GetRequest{
		AppName:   appName,
		UserID:    userID,
		SessionID: sessionID,
	})
	if err != nil || full == nil || full.Session == nil {
		return true
	}
	events := full.Session.Events()
	if len(eventlog.ScanPendingParks(events)) > 0 {
		return true
	}
	gotCount, gotLast := snapshotCountAndTime(events)
	return gotCount != wantCount || !gotLast.Equal(wantLast)
}

// repairContent builds the H3 repair message: one synthetic error
// FunctionResponse per dangling read-only call in the last function-call event.
func repairContent(repairable []effects.DanglingIntent) *genai.Content {
	parts := make([]*genai.Part, 0, len(repairable))
	for _, d := range repairable {
		p := genai.NewPartFromFunctionResponse(d.ToolName, map[string]any{
			"error": "interrupted before completion",
		})
		p.FunctionResponse.ID = d.CallID
		parts = append(parts, p)
	}
	return &genai.Content{Role: genai.RoleUser, Parts: parts}
}
