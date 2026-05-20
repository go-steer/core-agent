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

package tools

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/tool"
)

func TestNewScheduleTool_Defaults(t *testing.T) {
	t.Parallel()
	tl, ch, err := NewScheduleTool(ScheduleOptions{})
	if err != nil {
		t.Fatalf("NewScheduleTool: %v", err)
	}
	if tl.Name() != "schedule_next_turn" {
		t.Errorf("default name = %q, want schedule_next_turn", tl.Name())
	}
	desc := tl.Description()
	for _, want := range []string{"report_done", "Cadence", "30s", "1h+", "State doesn't survive"} {
		if !strings.Contains(desc, want) {
			t.Errorf("default description missing %q; got %q", want, desc)
		}
	}
	if cap(ch) != 1 {
		t.Errorf("default channel buffer = %d, want 1", cap(ch))
	}
}

func TestNewScheduleTool_NameAndDescriptionOverrides(t *testing.T) {
	t.Parallel()
	tl, _, err := NewScheduleTool(ScheduleOptions{
		Name:        "wake_me_later",
		Description: "custom desc",
	})
	if err != nil {
		t.Fatalf("NewScheduleTool: %v", err)
	}
	if tl.Name() != "wake_me_later" {
		t.Errorf("name override didn't take, got %q", tl.Name())
	}
	if tl.Description() != "custom desc" {
		t.Errorf("description override didn't take, got %q", tl.Description())
	}
}

func TestScheduleFunc_AcceptsWakeInSec(t *testing.T) {
	t.Parallel()
	ch := make(chan ScheduleEvent, 1)
	fn := scheduleFunc(ch, 0)
	res, err := fn(tool.Context(nil), scheduleArgs{
		WakeInSec:  60,
		NextPrompt: "rescan",
		Detail:     "10m cadence",
	})
	if err != nil {
		t.Fatalf("fn: %v", err)
	}
	if !strings.HasPrefix(res.Ack, "deferred until ") {
		t.Errorf("expected ack to start with 'deferred until', got %q", res.Ack)
	}
	select {
	case ev := <-ch:
		if ev.NextPrompt != "rescan" {
			t.Errorf("NextPrompt = %q, want rescan", ev.NextPrompt)
		}
		if ev.Detail != "10m cadence" {
			t.Errorf("Detail = %q, want '10m cadence'", ev.Detail)
		}
		if ev.WakeAt.IsZero() {
			t.Errorf("WakeAt should be set")
		}
		if ev.Time.IsZero() {
			t.Errorf("Time should be set")
		}
		// WakeAt should be roughly now+60s (allow generous slack for CI).
		if delta := time.Until(ev.WakeAt); delta < 50*time.Second || delta > 70*time.Second {
			t.Errorf("WakeAt delta = %v, want ~60s", delta)
		}
	default:
		t.Fatalf("expected event on channel")
	}
}

func TestScheduleFunc_AcceptsWakeAtAbsolute(t *testing.T) {
	t.Parallel()
	ch := make(chan ScheduleEvent, 1)
	fn := scheduleFunc(ch, 0)
	target := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second)
	res, err := fn(tool.Context(nil), scheduleArgs{
		WakeAt: target.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("fn: %v", err)
	}
	if !strings.HasPrefix(res.Ack, "deferred until ") {
		t.Errorf("ack = %q", res.Ack)
	}
	ev := <-ch
	if !ev.WakeAt.Equal(target) {
		t.Errorf("WakeAt = %v, want %v", ev.WakeAt, target)
	}
}

func TestScheduleFunc_RejectsBothArgs(t *testing.T) {
	t.Parallel()
	ch := make(chan ScheduleEvent, 1)
	fn := scheduleFunc(ch, 0)
	res, err := fn(tool.Context(nil), scheduleArgs{
		WakeAt:    time.Now().Format(time.RFC3339),
		WakeInSec: 30,
	})
	if err != nil {
		t.Fatalf("fn: %v", err)
	}
	if !strings.Contains(res.Ack, "rejected") || !strings.Contains(res.Ack, "exactly one") {
		t.Errorf("expected rejection for both args, got %q", res.Ack)
	}
	select {
	case ev := <-ch:
		t.Errorf("rejected call should not emit; got %+v", ev)
	default:
	}
}

func TestScheduleFunc_RejectsNeitherArg(t *testing.T) {
	t.Parallel()
	ch := make(chan ScheduleEvent, 1)
	fn := scheduleFunc(ch, 0)
	res, err := fn(tool.Context(nil), scheduleArgs{})
	if err != nil {
		t.Fatalf("fn: %v", err)
	}
	if !strings.Contains(res.Ack, "rejected") {
		t.Errorf("expected rejection for missing args, got %q", res.Ack)
	}
	select {
	case <-ch:
		t.Errorf("rejected call should not emit")
	default:
	}
}

func TestScheduleFunc_RejectsMalformedAbsolute(t *testing.T) {
	t.Parallel()
	ch := make(chan ScheduleEvent, 1)
	fn := scheduleFunc(ch, 0)
	res, err := fn(tool.Context(nil), scheduleArgs{WakeAt: "not-a-time"})
	if err != nil {
		t.Fatalf("fn: %v", err)
	}
	if !strings.Contains(res.Ack, "rejected") || !strings.Contains(res.Ack, "RFC3339") {
		t.Errorf("expected RFC3339 rejection, got %q", res.Ack)
	}
}

func TestScheduleFunc_RejectsNegativeRelative(t *testing.T) {
	t.Parallel()
	ch := make(chan ScheduleEvent, 1)
	fn := scheduleFunc(ch, 0)
	res, err := fn(tool.Context(nil), scheduleArgs{WakeInSec: -10})
	if err != nil {
		t.Fatalf("fn: %v", err)
	}
	if !strings.Contains(res.Ack, "rejected") || !strings.Contains(res.Ack, "non-negative") {
		t.Errorf("expected non-negative rejection, got %q", res.Ack)
	}
}

func TestScheduleFunc_EnforcesMaxDefer(t *testing.T) {
	t.Parallel()
	ch := make(chan ScheduleEvent, 1)
	fn := scheduleFunc(ch, 10*time.Minute) // cap at 10m

	// Within the cap: accepted.
	res, err := fn(tool.Context(nil), scheduleArgs{WakeInSec: 300})
	if err != nil {
		t.Fatalf("fn (within cap): %v", err)
	}
	if !strings.HasPrefix(res.Ack, "deferred") {
		t.Errorf("within-cap ack = %q", res.Ack)
	}
	<-ch // drain

	// Past the cap: rejected.
	res, err = fn(tool.Context(nil), scheduleArgs{WakeInSec: 3600})
	if err != nil {
		t.Fatalf("fn (past cap): %v", err)
	}
	if !strings.Contains(res.Ack, "rejected") || !strings.Contains(res.Ack, "MaxDefer") {
		t.Errorf("expected MaxDefer rejection, got %q", res.Ack)
	}
	select {
	case <-ch:
		t.Errorf("past-cap call should not emit")
	default:
	}
}

func TestScheduleFunc_ZeroMaxDeferMeansNoCap(t *testing.T) {
	t.Parallel()
	ch := make(chan ScheduleEvent, 1)
	fn := scheduleFunc(ch, 0)
	res, err := fn(tool.Context(nil), scheduleArgs{WakeInSec: 31536000}) // one year
	if err != nil {
		t.Fatalf("fn: %v", err)
	}
	if !strings.HasPrefix(res.Ack, "deferred") {
		t.Errorf("zero-cap should accept arbitrary defer, got %q", res.Ack)
	}
	<-ch
}

func TestScheduleFunc_TrimsStringArgs(t *testing.T) {
	t.Parallel()
	ch := make(chan ScheduleEvent, 1)
	fn := scheduleFunc(ch, 0)
	if _, err := fn(tool.Context(nil), scheduleArgs{
		WakeInSec:  10,
		NextPrompt: "  rescan  ",
		Detail:     "  10m cadence  ",
	}); err != nil {
		t.Fatalf("fn: %v", err)
	}
	ev := <-ch
	if ev.NextPrompt != "rescan" {
		t.Errorf("NextPrompt = %q, want trimmed", ev.NextPrompt)
	}
	if ev.Detail != "10m cadence" {
		t.Errorf("Detail = %q, want trimmed", ev.Detail)
	}
}

func TestScheduleFunc_NonBlockingOverflow(t *testing.T) {
	t.Parallel()
	ch := make(chan ScheduleEvent, 1)
	fn := scheduleFunc(ch, 0)

	// First call lands.
	if _, err := fn(tool.Context(nil), scheduleArgs{WakeInSec: 10}); err != nil {
		t.Fatalf("fn (first): %v", err)
	}
	// Second call would block on a buffered=1 channel — must drop instead.
	res, err := fn(tool.Context(nil), scheduleArgs{WakeInSec: 20})
	if err != nil {
		t.Fatalf("fn (second): %v", err)
	}
	if !strings.HasPrefix(res.Ack, "deferred") {
		// We still tell the model we accepted — the driver just won't
		// see the second event. That's the intended non-blocking shape.
		t.Errorf("second call ack = %q, expected acceptance", res.Ack)
	}
}
