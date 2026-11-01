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
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"

	"github.com/go-steer/core-agent/v3/pkg/compose"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

func TestStartDaemonSchedule_SeedsAndRunsBehindLease(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "sched.db")
	h, err := eventlog.Open(ctx, sqlite.Open(dbPath))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	defer func() { _ = h.Close() }()

	fired := make(chan compose.ScheduledFire, 1)
	sh, err := startDaemonSchedule(ctx, daemonScheduleDeps{
		EventLog: h,
		AppName:  "core-agent",
		Workload: "nightly-audit",
		Schedule: &config.ScheduleConfig{
			Every:  "1s",
			Jitter: "0s",
			Prompt: "Run audit.",
		},
		Fire: func(_ context.Context, f compose.ScheduledFire) error {
			select {
			case fired <- f:
			default:
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("startDaemonSchedule: %v", err)
	}
	defer sh.Stop()

	if !sh.Drives || sh.Trigger == nil {
		t.Fatalf("handle = %+v, want Drives=true and non-nil Trigger", sh)
	}
	rec, err := h.Schedule(ctx, "core-agent", "nightly-audit")
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if rec == nil || rec.Interval != time.Second {
		t.Fatalf("Schedule record = %+v, want seeded 1s interval", rec)
	}

	select {
	case f := <-fired:
		if f.FireNum != 1 {
			t.Errorf("FireNum = %d, want 1", f.FireNum)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for scheduled fire")
	}
}
