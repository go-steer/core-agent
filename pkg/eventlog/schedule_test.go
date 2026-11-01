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

// Originally derived from go-steer/mast@d239f89 (internal/transcript/schedule_test.go).

package eventlog

import (
	"bytes"
	"context"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestScheduleRoundTrip(t *testing.T) {
	h, _ := openTestHandle(t)
	ctx := context.Background()
	anchor := time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC)

	got, err := h.Schedule(ctx, "app", "gke-triage")
	if err != nil {
		t.Fatalf("Schedule on fresh store: %v", err)
	}
	if got != nil {
		t.Fatalf("Schedule on fresh store = %+v, want nil", got)
	}

	rec := ScheduleRecord{
		Workload: "gke-triage",
		Interval: time.Hour,
		Anchor:   anchor,
		LastTick: anchor.Add(2 * time.Hour),
		LastFire: anchor.Add(2*time.Hour + 12*time.Second),
		Fires:    2,
	}
	if err := h.SaveSchedule(ctx, "app", rec); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}
	got, err = h.Schedule(ctx, "app", "gke-triage")
	if err != nil {
		t.Fatalf("Schedule after save: %v", err)
	}
	if got == nil {
		t.Fatal("Schedule returned nil for a record just written")
	}
	if !got.Anchor.Equal(anchor) || !got.LastTick.Equal(rec.LastTick) || !got.LastFire.Equal(rec.LastFire) || got.Interval != time.Hour || got.Fires != 2 {
		t.Errorf("Schedule = %+v, want %+v", got, rec)
	}

	// Last write wins in place.
	rec.LastTick = anchor.Add(3 * time.Hour)
	rec.Fires = 3
	if err := h.SaveSchedule(ctx, "app", rec); err != nil {
		t.Fatalf("SaveSchedule (update): %v", err)
	}
	got, err = h.Schedule(ctx, "app", "gke-triage")
	if err != nil {
		t.Fatalf("Schedule after update: %v", err)
	}
	if got == nil || got.Fires != 3 || !got.LastTick.Equal(rec.LastTick) {
		t.Errorf("after update, Schedule = %+v, want the second write", got)
	}

	// One record per workload: two daemons over one store do not re-anchor each other.
	other := ScheduleRecord{
		Workload: "cost-sweep",
		Interval: 30 * time.Minute,
		Anchor:   anchor.Add(time.Minute),
	}
	if err := h.SaveSchedule(ctx, "app", other); err != nil {
		t.Fatalf("SaveSchedule (second workload): %v", err)
	}
	gotFirst, err := h.Schedule(ctx, "app", "gke-triage")
	if err != nil || gotFirst == nil || !gotFirst.Anchor.Equal(anchor) {
		t.Errorf("first workload anchor changed: got=%+v err=%v", gotFirst, err)
	}
	gotSecond, err := h.Schedule(ctx, "app", "cost-sweep")
	if err != nil || gotSecond == nil || !gotSecond.Anchor.Equal(anchor.Add(time.Minute)) {
		t.Errorf("second workload anchor = %+v (err=%v), want its own", gotSecond, err)
	}
}

func TestScheduleIsInvisibleToParksDecisionsAndAck(t *testing.T) {
	h, _ := openTestHandle(t)
	ctx := context.Background()

	if err := h.SaveSchedule(ctx, "app", ScheduleRecord{
		Workload: "gke-triage",
		Interval: time.Hour,
		Anchor:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveSchedule: %v", err)
	}

	parks, err := h.ListParks(ctx, "app", "", "")
	if err != nil {
		t.Fatalf("ListParks: %v", err)
	}
	if len(parks) != 0 {
		t.Errorf("ListParks = %+v, want schedule ops session to be ignored", parks)
	}

	var buf bytes.Buffer
	n, err := h.ExportDecisions(ctx, &buf, ExportOptions{AppName: "app"})
	if err != nil {
		t.Fatalf("ExportDecisions: %v", err)
	}
	if n != 0 {
		t.Errorf("ExportDecisions count = %d, want 0", n)
	}

	if _, err := h.AckEffects(ctx, "app", "system", scheduleSessionID, "op", "test"); err == nil {
		t.Error("AckEffects accepted reserved scheduleSessionID")
	}
}

func TestScheduleUnreadableRecordsReadAsAbsent(t *testing.T) {
	h, _ := openTestHandle(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		blob string
	}{
		{"not JSON", "{{"},
		{"no anchor", `{"workload":"gke-triage"}`},
		{"blanked", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, err := h.getOrCreateOpsSession(ctx, "app", scheduleUserID, scheduleSessionID)
			if err != nil {
				t.Fatalf("getOrCreateOpsSession: %v", err)
			}
			ev := session.NewEvent(ctx, "schedule-corrupt")
			ev.Author = "daemon"
			ev.Content = genai.NewContentFromText("corrupt", genai.RoleUser)
			ev.Actions.StateDelta = map[string]any{
				scheduleKeyPrefix + "gke-triage": tc.blob,
			}
			if err := h.Service.AppendEvent(ctx, sess, ev); err != nil {
				t.Fatalf("AppendEvent: %v", err)
			}
			got, err := h.Schedule(ctx, "app", "gke-triage")
			if err != nil {
				t.Fatalf("Schedule error: %v", err)
			}
			if got != nil {
				t.Errorf("Schedule = %+v, want nil for %s", got, tc.name)
			}
		})
	}
}

func TestSaveScheduleValidation(t *testing.T) {
	h, _ := openTestHandle(t)
	ctx := context.Background()

	if err := h.SaveSchedule(ctx, "", ScheduleRecord{
		Workload: "gke-triage",
		Anchor:   time.Now().UTC(),
	}); err == nil {
		t.Error("SaveSchedule accepted empty appName")
	}
	if err := h.SaveSchedule(ctx, "app", ScheduleRecord{
		Anchor: time.Now().UTC(),
	}); err == nil {
		t.Error("SaveSchedule accepted empty Workload")
	}
	var nilHandle *Handle
	if err := nilHandle.SaveSchedule(ctx, "app", ScheduleRecord{
		Workload: "gke-triage",
		Anchor:   time.Now().UTC(),
	}); err == nil {
		t.Error("nil Handle SaveSchedule should fail")
	}
	if _, err := nilHandle.Schedule(ctx, "app", "gke-triage"); err == nil {
		t.Error("nil Handle Schedule should fail")
	}
}
