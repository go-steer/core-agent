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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/internal/monitor"
	"github.com/go-steer/core-agent/v3/internal/notify"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

const (
	notifyOpsSessionID   = "mast-notify:mast-ops"
	notifyOpsUserID      = "mast-ops"
	notifyStateKeyPrefix = "mast_notify:"
	timelineSeparator    = "\n\n"
)

// NotifierSender is the outbound switchboard client interface used by Notifier
// (#1406).
type NotifierSender interface {
	Post(ctx context.Context, text, sessionID string) (notify.Ref, error)
	Append(ctx context.Context, parent notify.Ref, text string) (notify.Ref, error)
	Replace(ctx context.Context, ref notify.Ref, text string) (notify.Ref, error)
}

// NotifierConfig configures a Notifier (#1406).
type NotifierConfig struct {
	WorkloadName string
	AppName      string
	Notify       *config.MonitorNotifyConfig
	WakeOn       []string
	Client       NotifierSender
	EventLog     *eventlog.Handle
	Logger       *slog.Logger
	Now          func() time.Time
}

type notifyState struct {
	Ref            notify.Ref        `json:"ref"`
	Text           string            `json:"text,omitempty"`
	LastSpokeAt    time.Time         `json:"last_spoke_at,omitzero"`
	LastRemindedAt map[string]string `json:"last_reminded_at,omitempty"`
	ActiveSubjects map[string]string `json:"active_subjects,omitempty"`
	Degraded       bool              `json:"degraded,omitempty"`
}

// Notifier manages outbound monitoring notifications, timeline/replace state,
// remind_after and digest_after timers, and edge-triggered degraded notices
// persisted on the reserved "mast-notify:mast-ops" session (#1406).
type Notifier struct {
	workloadName string
	appName      string
	channel      string
	updateMode   string
	remindAfter  time.Duration
	digestAfter  time.Duration
	wakeOn       []string
	client       NotifierSender
	eventLog     *eventlog.Handle
	logger       *slog.Logger
	now          func() time.Time

	mu     sync.Mutex
	state  notifyState
	loaded bool
}

// NewNotifier constructs a Notifier from cfg (#1406). Returns (nil, nil) when
// cfg.Notify is nil.
func NewNotifier(cfg NotifierConfig) (*Notifier, error) {
	if cfg.Notify == nil {
		return nil, nil
	}
	channel := strings.TrimSpace(cfg.Notify.Channel)
	if channel == "" {
		return nil, errors.New("compose: monitor.notify.channel is required")
	}
	appName := strings.TrimSpace(cfg.AppName)
	if appName == "" {
		appName = "core-agent"
	}
	workloadName := strings.TrimSpace(cfg.WorkloadName)
	if workloadName == "" {
		workloadName = appName
	}
	updateMode := strings.TrimSpace(cfg.Notify.Update)
	if updateMode == "" {
		updateMode = config.MonitorNotifyUpdateTimeline
	}
	if updateMode != config.MonitorNotifyUpdateTimeline && updateMode != config.MonitorNotifyUpdateReplace {
		return nil, fmt.Errorf("compose: invalid monitor.notify.update %q", updateMode)
	}
	client := cfg.Client
	if client == nil {
		endpoint := strings.TrimSpace(cfg.Notify.Endpoint)
		tokenEnv := strings.TrimSpace(cfg.Notify.TokenEnv)
		token := strings.TrimSpace(os.Getenv(tokenEnv))
		c, err := notify.New(notify.Config{
			Endpoint: endpoint,
			Channel:  channel,
			Token:    token,
		})
		if err != nil {
			return nil, err
		}
		client = c
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	now := nowFn()
	return &Notifier{
		workloadName: workloadName,
		appName:      appName,
		channel:      channel,
		updateMode:   updateMode,
		remindAfter:  cfg.Notify.ResolvedRemindAfter(),
		digestAfter:  cfg.Notify.ResolvedDigestAfter(),
		wakeOn:       slices.Clone(cfg.WakeOn),
		client:       client,
		eventLog:     cfg.EventLog,
		logger:       cfg.Logger,
		now:          nowFn,
		state: notifyState{
			LastSpokeAt:    now,
			LastRemindedAt: map[string]string{},
			ActiveSubjects: map[string]string{},
		},
	}, nil
}

// AfterCycle processes the outcome of a monitoring cycle, updating durable
// state on "mast-notify:mast-ops" and dispatching outbound notifications when
// appropriate (#1406).
func (n *Notifier) AfterCycle(ctx context.Context, parsed *monitor.Set, turnOutput string, cycleFailed bool) error {
	if n == nil || n.client == nil || n.channel == "" {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()

	n.ensureStateLoaded(ctx)
	now := n.now().UTC()

	if cycleFailed || (parsed != nil && parsed.Degraded) {
		n.handleFailedCycle(ctx, now, turnOutput)
		return nil
	}
	n.handleRecoveryIfDegraded(ctx, now)

	shouldSpeak := n.evaluateCycleTransitions(now, parsed)
	if !shouldSpeak {
		n.state.Ref = notify.Ref{}
		n.state.Text = ""
		n.saveState(ctx)
		return nil
	}

	text := strings.TrimSpace(turnOutput)
	if text == "" {
		return errors.New("the cycle had something to report and the turn produced no text; nothing was sent")
	}

	if err := n.sendUpdate(ctx, text); err != nil {
		// Never replay failed notifications on subsequent cycles; close the
		// open timeline so the next speaking cycle starts fresh.
		n.state.Ref = notify.Ref{}
		n.state.Text = ""
		n.saveState(ctx)
		return err
	}
	n.state.LastSpokeAt = now
	n.saveState(ctx)
	return nil
}

func (n *Notifier) handleFailedCycle(ctx context.Context, now time.Time, cause string) {
	if n.state.Degraded {
		return
	}
	n.state.Degraded = true
	n.state.Ref = notify.Ref{}
	n.state.Text = ""
	n.saveState(ctx)

	if !n.allowsDegradedNotice() {
		return
	}
	msg := fmt.Sprintf("monitoring cycle for %s degraded at %s", n.workloadName, now.Format(time.RFC3339))
	if trimmed := strings.TrimSpace(cause); trimmed != "" {
		msg += ": " + trimmed
	}
	_, _ = n.client.Post(ctx, msg, "")
}

func (n *Notifier) handleRecoveryIfDegraded(ctx context.Context, now time.Time) {
	if !n.state.Degraded {
		return
	}
	n.state.Degraded = false
	n.saveState(ctx)

	if !n.allowsDegradedNotice() {
		return
	}
	msg := fmt.Sprintf("monitoring cycle for %s recovered at %s. Monitoring has recovered.", n.workloadName, now.Format(time.RFC3339))
	_, _ = n.client.Post(ctx, msg, "")
}

func (n *Notifier) allowsDegradedNotice() bool {
	return len(n.wakeOn) == 0 || slices.Contains(n.wakeOn, "degraded")
}

func (n *Notifier) evaluateCycleTransitions(now time.Time, parsed *monitor.Set) bool {
	if parsed == nil {
		return true
	}
	if n.state.LastRemindedAt == nil {
		n.state.LastRemindedAt = map[string]string{}
	}
	if n.state.ActiveSubjects == nil {
		n.state.ActiveSubjects = map[string]string{}
	}

	shouldSpeak := false
	for _, tr := range parsed.Transitions {
		if tr.Edge == "closed" {
			delete(n.state.ActiveSubjects, tr.SubjectKey)
			delete(n.state.LastRemindedAt, tr.SubjectKey)
			shouldSpeak = true
			continue
		}
		if tr.Edge == "remained" {
			if n.checkAndAdvanceRemind(now, tr.SubjectKey) {
				shouldSpeak = true
			}
			continue
		}
		// opened / re_opened or custom transition edges.
		n.state.ActiveSubjects[tr.SubjectKey] = now.Format(time.RFC3339)
		n.state.LastRemindedAt[tr.SubjectKey] = now.Format(time.RFC3339)
		shouldSpeak = true
	}
	if shouldSpeak {
		return true
	}
	if n.digestAfter > 0 && !now.Before(n.state.LastSpokeAt.Add(n.digestAfter)) {
		return true
	}
	return false
}

func (n *Notifier) checkAndAdvanceRemind(now time.Time, subject string) bool {
	if n.remindAfter <= 0 {
		return false
	}
	lastRaw, ok := n.state.LastRemindedAt[subject]
	if !ok {
		n.state.ActiveSubjects[subject] = now.Format(time.RFC3339)
		n.state.LastRemindedAt[subject] = now.Format(time.RFC3339)
		return false
	}
	lastAt, err := time.Parse(time.RFC3339Nano, lastRaw)
	if err != nil {
		lastAt, err = time.Parse(time.RFC3339, lastRaw)
	}
	if err != nil || !now.Before(lastAt.Add(n.remindAfter)) {
		n.state.LastRemindedAt[subject] = now.Format(time.RFC3339)
		return true
	}
	return false
}

func (n *Notifier) isRemindDue(ctx context.Context, subject string, now time.Time) bool {
	if n == nil || n.remindAfter <= 0 {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureStateLoaded(ctx)
	if n.state.LastRemindedAt == nil {
		n.state.LastRemindedAt = map[string]string{}
	}
	if n.state.ActiveSubjects == nil {
		n.state.ActiveSubjects = map[string]string{}
	}
	lastRaw, ok := n.state.LastRemindedAt[subject]
	if !ok {
		nowStr := now.UTC().Format(time.RFC3339)
		n.state.ActiveSubjects[subject] = nowStr
		n.state.LastRemindedAt[subject] = nowStr
		n.saveState(ctx)
		return false
	}
	lastAt, err := time.Parse(time.RFC3339Nano, lastRaw)
	if err != nil {
		lastAt, err = time.Parse(time.RFC3339, lastRaw)
	}
	if err != nil {
		return true
	}
	return !now.UTC().Before(lastAt.Add(n.remindAfter))
}

func (n *Notifier) isDigestDue(ctx context.Context, now time.Time) bool {
	if n == nil || n.digestAfter <= 0 {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureStateLoaded(ctx)
	return !now.UTC().Before(n.state.LastSpokeAt.Add(n.digestAfter))
}

func isZeroRef(ref notify.Ref) bool {
	return strings.TrimSpace(ref.ID) == "" && strings.TrimSpace(ref.Channel) == ""
}

func (n *Notifier) sendUpdate(ctx context.Context, text string) error {
	if n.updateMode == config.MonitorNotifyUpdateReplace {
		return n.sendReplace(ctx, text)
	}
	return n.sendTimeline(ctx, text)
}

func (n *Notifier) sendReplace(ctx context.Context, text string) error {
	if isZeroRef(n.state.Ref) {
		ref, err := n.client.Post(ctx, text, "")
		if err != nil {
			return err
		}
		n.state.Ref = ref
		n.state.Text = text
		return nil
	}
	ref, err := n.client.Replace(ctx, n.state.Ref, text)
	if err == nil {
		n.state.Ref = ref
		n.state.Text = text
		return nil
	}
	if !errors.Is(err, notify.ErrNoSuchMessage) && !errors.Is(err, notify.ErrEditUnsupported) && !errors.Is(err, notify.ErrSendFullText) {
		return err
	}
	freshRef, postErr := n.client.Post(ctx, text, "")
	if postErr != nil {
		return postErr
	}
	n.state.Ref = freshRef
	n.state.Text = text
	return nil
}

func (n *Notifier) sendTimeline(ctx context.Context, text string) error {
	if isZeroRef(n.state.Ref) {
		ref, err := n.client.Post(ctx, text, "")
		if err != nil {
			return err
		}
		n.state.Ref = ref
		n.state.Text = text
		return nil
	}
	ref, err := n.client.Append(ctx, n.state.Ref, text)
	if err == nil {
		if ref.ID != "" && ref != n.state.Ref {
			n.state.Ref = ref
			n.state.Text = text
		} else {
			n.state.Text += timelineSeparator + text
		}
		return nil
	}
	if !errors.Is(err, notify.ErrSendFullText) && !errors.Is(err, notify.ErrNoSuchMessage) {
		return err
	}
	full := n.state.Text + timelineSeparator + text
	if errors.Is(err, notify.ErrSendFullText) {
		rRef, rerr := n.client.Replace(ctx, n.state.Ref, full)
		if rerr == nil {
			n.state.Ref = rRef
			n.state.Text = full
			return nil
		}
		if !errors.Is(rerr, notify.ErrEditUnsupported) && !errors.Is(rerr, notify.ErrNoSuchMessage) && !errors.Is(rerr, notify.ErrSendFullText) {
			return rerr
		}
	}
	freshRef, postErr := n.client.Post(ctx, full, "")
	if postErr != nil {
		return postErr
	}
	n.state.Ref = freshRef
	n.state.Text = full
	return nil
}

func (n *Notifier) stateKey() string {
	return notifyStateKeyPrefix + n.workloadName
}

func (n *Notifier) ensureStateLoaded(ctx context.Context) {
	if n.loaded || n.eventLog == nil || n.eventLog.Service == nil {
		n.loaded = true
		return
	}
	n.loaded = true
	resp, err := n.eventLog.Service.Get(ctx, &session.GetRequest{
		AppName:   n.appName,
		UserID:    notifyOpsUserID,
		SessionID: notifyOpsSessionID,
	})
	if err != nil || resp == nil || resp.Session == nil || resp.Session.State() == nil {
		return
	}
	val, err := resp.Session.State().Get(n.stateKey())
	if err != nil {
		return
	}
	raw, ok := val.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return
	}
	var loaded notifyState
	if err := json.Unmarshal([]byte(raw), &loaded); err != nil {
		return
	}
	if loaded.LastRemindedAt == nil {
		loaded.LastRemindedAt = map[string]string{}
	}
	if loaded.ActiveSubjects == nil {
		loaded.ActiveSubjects = map[string]string{}
	}
	if loaded.LastSpokeAt.IsZero() {
		loaded.LastSpokeAt = n.now().UTC()
	}
	n.state = loaded
}

func (n *Notifier) saveState(ctx context.Context) {
	if n.eventLog == nil || n.eventLog.Service == nil {
		return
	}
	sess := n.ensureNotifyOpsSession(ctx)
	if sess == nil {
		return
	}
	raw, err := json.Marshal(n.state)
	if err != nil {
		return
	}
	ev := session.NewEvent(ctx, "")
	ev.Author = "system"
	ev.Timestamp = n.now().UTC()
	ev.Actions.StateDelta = map[string]any{
		n.stateKey(): string(raw),
	}
	_ = n.eventLog.Service.AppendEvent(ctx, sess, ev)
}

func (n *Notifier) ensureNotifyOpsSession(ctx context.Context) session.Session {
	resp, err := n.eventLog.Service.Get(ctx, &session.GetRequest{
		AppName:   n.appName,
		UserID:    notifyOpsUserID,
		SessionID: notifyOpsSessionID,
	})
	if err == nil && resp != nil && resp.Session != nil {
		return resp.Session
	}
	created, err := n.eventLog.Service.Create(ctx, &session.CreateRequest{
		AppName:   n.appName,
		UserID:    notifyOpsUserID,
		SessionID: notifyOpsSessionID,
	})
	if err == nil && created != nil && created.Session != nil {
		return created.Session
	}
	resp, err = n.eventLog.Service.Get(ctx, &session.GetRequest{
		AppName:   n.appName,
		UserID:    notifyOpsUserID,
		SessionID: notifyOpsSessionID,
	})
	if err == nil && resp != nil {
		return resp.Session
	}
	return nil
}
