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

// Originally derived from go-steer/mast@d239f89 (internal/transcript/schedule.go).

package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

const (
	scheduleKeyPrefix = "mast_schedule:"
	scheduleSessionID = "mast-scheduler" + reservedOpsSuffix
	scheduleUserID    = "system"
)

// ScheduleRecord is one workload's persisted cadence state.
//
// The anchor is the instant the cadence is counted from: the moment the
// daemon first saw this workload's schedule. Every fire lands on
// Anchor + k*Interval, so a daemon returning from a restart preserves the
// workload's phase instead of re-anchoring to process boot time.
type ScheduleRecord struct {
	// Workload names the bundle or recipe this cadence belongs to.
	Workload string `json:"workload"`

	// Interval is the configured cadence duration at the time of the write.
	Interval time.Duration `json:"interval,omitempty"`

	// Anchor is the instant the cadence is counted from.
	Anchor time.Time `json:"anchor"`

	// LastTick is the most recent lattice point accounted for — fired,
	// or coalesced away as a missed tick.
	LastTick time.Time `json:"last_tick,omitempty"`

	// LastFire is when the last fire actually started (tick + jitter),
	// and Fires counts the fires this schedule has driven since the
	// anchor was set.
	LastFire time.Time `json:"last_fire,omitempty"`
	Fires    int64     `json:"fires,omitempty"`
}

// Schedule reads a workload's persisted cadence state, or (nil, nil) when
// no valid record is present.
//
// Missing sessions, blanked keys, corrupt JSON, and anchor-less records all
// return (nil, nil) so the caller re-anchors on now rather than wedging the
// scheduled trigger.
func (h *Handle) Schedule(ctx context.Context, appName, workload string) (*ScheduleRecord, error) {
	if h == nil || h.Service == nil {
		return nil, errors.New("eventlog: Schedule: handle service is required")
	}
	if appName == "" || workload == "" {
		return nil, nil
	}
	resp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName:   appName,
		UserID:    scheduleUserID,
		SessionID: scheduleSessionID,
	})
	if err != nil {
		if isNotFoundErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("eventlog: get schedule session: %w", err)
	}
	if resp == nil || resp.Session == nil {
		return nil, nil
	}
	v, err := resp.Session.State().Get(scheduleKeyPrefix + workload)
	if err != nil {
		return nil, nil
	}
	raw, ok := v.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var rec ScheduleRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil, nil
	}
	if rec.Anchor.IsZero() {
		return nil, nil
	}
	return &rec, nil
}

// SaveSchedule writes the workload's cadence state to the reserved
// "mast-scheduler:mast-ops" session, replacing any previous record for
// rec.Workload.
func (h *Handle) SaveSchedule(ctx context.Context, appName string, rec ScheduleRecord) error {
	if h == nil || h.Service == nil {
		return errors.New("eventlog: SaveSchedule: handle service is required")
	}
	if appName == "" {
		return errors.New("eventlog: SaveSchedule: appName is required")
	}
	if rec.Workload == "" {
		return errors.New("eventlog: SaveSchedule: workload name is required")
	}
	blob, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("eventlog: marshal schedule for workload %q: %w", rec.Workload, err)
	}
	sess, err := h.getOrCreateOpsSession(ctx, appName, scheduleUserID, scheduleSessionID)
	if err != nil {
		return err
	}
	ev := session.NewEvent(ctx, "schedule")
	ev.Author = "daemon"
	ev.Content = genai.NewContentFromText(
		fmt.Sprintf("scheduled trigger for %q anchored at %s", rec.Workload, rec.Anchor.UTC().Format(time.RFC3339Nano)),
		genai.RoleUser,
	)
	ev.Actions.StateDelta = map[string]any{
		scheduleKeyPrefix + rec.Workload: string(blob),
	}
	if err := h.Service.AppendEvent(ctx, sess, ev); err != nil {
		return fmt.Errorf("eventlog: append schedule event for workload %q: %w", rec.Workload, err)
	}
	return nil
}

func (h *Handle) getOrCreateOpsSession(ctx context.Context, appName, userID, opsSessionID string) (session.Session, error) {
	if resp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName:   appName,
		UserID:    userID,
		SessionID: opsSessionID,
	}); err == nil && resp != nil && resp.Session != nil {
		return resp.Session, nil
	}
	created, cerr := h.Service.Create(ctx, &session.CreateRequest{
		AppName:   appName,
		UserID:    userID,
		SessionID: opsSessionID,
	})
	if cerr == nil && created != nil && created.Session != nil {
		return created.Session, nil
	}
	retry, gerr := h.Service.Get(ctx, &session.GetRequest{
		AppName:   appName,
		UserID:    userID,
		SessionID: opsSessionID,
	})
	if gerr != nil || retry == nil || retry.Session == nil {
		return nil, fmt.Errorf("eventlog: open ops session %q: %w", opsSessionID, cerr)
	}
	return retry.Session, nil
}

func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	if IsSessionNotFound(err) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}
