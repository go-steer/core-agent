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
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// ScheduleEvent is what the schedule_next_turn tool emits when the
// model calls it. The autonomous driver consumes one event per turn
// from the channel returned by NewScheduleTool and feeds it to the
// configured Scheduler.
type ScheduleEvent struct {
	// WakeAt is the absolute time the next turn should run, resolved
	// from either an absolute time or a relative duration in the tool
	// args before this event is emitted.
	WakeAt time.Time

	// NextPrompt is the continuation message the next turn should
	// receive. Empty string means the driver should fall back to its
	// configured continuation prompt (typically "continue").
	NextPrompt string

	// Detail is the freeform one-liner the model passed for telemetry
	// / operator visibility (e.g. "polling cluster-A on 10m cadence").
	// Surfaces on the chat-style streaming UI and in the eventlog.
	Detail string

	// Time is the moment the tool handler received the call.
	Time time.Time
}

// ScheduleOptions configures NewScheduleTool.
type ScheduleOptions struct {
	// Name overrides the tool's function name. Defaults to
	// "schedule_next_turn" when empty.
	Name string

	// Description overrides the model-facing tool description. Defaults
	// to a baseline that includes the report_done distinction, the
	// cadence ladder (30s fast-changing state, 5-15m steady-state,
	// 1h+ slow-changing infra), good-vs-bad next_prompt examples, and
	// the state-persistence reminder.
	Description string

	// MaxDefer caps how far in the future a single call may schedule.
	// Zero means no cap. A call with wake_in_sec or wake_at past the
	// cap returns a tool-result error to the model so it can adapt;
	// the channel sees no event for that rejected call.
	MaxDefer time.Duration

	// BufferSize sets the schedule channel buffer. Default 1 — exactly
	// one schedule emission is consumed per turn by the autonomous
	// driver, so anything past 1 is overflow that would indicate a
	// bug. Exposed for tests that want to inspect multiple emissions
	// without a driver in the loop.
	BufferSize int
}

const (
	defaultScheduleName        = "schedule_next_turn"
	defaultScheduleDescription = `Pause the autonomous loop and resume later with a new prompt.

Use this instead of report_done when there is more periodic work to do — report_done exits the loop permanently and will not resume.

Cadence ladder (pick the largest interval that meets the goal — cost scales linearly with wake frequency):
- 30s: fast-changing state (pod restarts, queue depths, in-flight error counts during active investigation)
- 5-15m: steady-state monitoring (deployment drift, error-rate baselines, namespace inventory)
- 1h+: slow-changing infra (cluster autoscaling, IAM, quota)

next_prompt should be short and action-oriented. The original goal and system instructions are re-presented on the next turn, so don't restate them. Good: "rescan and diff vs baseline.json". Bad: "continue your task" (too vague), or restating the full monitoring brief (wasteful).

State doesn't survive the defer. The conversation context resets between turns — only files you wrote and todo entries you created persist. If you need a baseline ("deployments I saw last scan", "error counts at last poll") on the next turn, write it to a file or todo entry NOW, then read it back after wake.

Provide either wake_at (RFC3339 absolute time) or wake_in_sec (relative seconds from now); exactly one is required.

Don't call this in the same turn as report_done — if both are called, report_done wins and the loop exits.`
)

type scheduleArgs struct {
	WakeAt     string `json:"wake_at,omitempty" jsonschema:"absolute wake time in RFC3339 format (e.g. 2026-05-20T15:30:00Z). Provide exactly one of wake_at or wake_in_sec."`
	WakeInSec  int    `json:"wake_in_sec,omitempty" jsonschema:"relative wake delay in seconds from now (must be positive — use wake_at for an immediate wake). Provide exactly one of wake_at or wake_in_sec."`
	NextPrompt string `json:"next_prompt,omitempty" jsonschema:"short action-oriented prompt for the next turn (e.g. 'rescan and diff vs baseline.json'). Empty falls back to the loop's default continuation prompt."`
	Detail     string `json:"detail,omitempty" jsonschema:"optional one-line telemetry detail (e.g. 'polling cluster-A on 10m cadence')"`
}

type scheduleResult struct {
	Ack    string `json:"ack"`
	WakeAt string `json:"wake_at,omitempty"`
}

// NewScheduleTool returns the schedule_next_turn tool plus a channel
// the autonomous driver consumes after each turn. The tool emits one
// event per successful call; rejected calls (validation failures,
// cap violations) return a tool-result error to the model and emit
// nothing on the channel.
//
// The channel is buffered (default 1); a non-blocking send is used so
// a single misbehaving turn that calls the tool multiple times won't
// deadlock the goroutine. The autonomous driver drains the channel
// non-blockingly after each turn and consults the configured Scheduler
// only if an event was received.
//
// Returns an error only when opts is invalid (currently: never; all
// options have sensible zero-value defaults).
func NewScheduleTool(opts ScheduleOptions) (tool.Tool, <-chan ScheduleEvent, error) {
	name := opts.Name
	if name == "" {
		name = defaultScheduleName
	}
	desc := opts.Description
	if desc == "" {
		desc = defaultScheduleDescription
	}
	bufSize := opts.BufferSize
	if bufSize <= 0 {
		bufSize = 1
	}
	ch := make(chan ScheduleEvent, bufSize)
	t, err := functiontool.New(
		functiontool.Config{Name: name, Description: desc},
		scheduleFunc(ch, opts.MaxDefer),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("tools: NewScheduleTool: %w", err)
	}
	return t, ch, nil
}

// scheduleFunc builds the handler invoked by the wrapped function
// tool. Extracted so tests can drive it directly without going through
// ADK's functiontool wrapper.
func scheduleFunc(ch chan<- ScheduleEvent, maxDefer time.Duration) functiontool.Func[scheduleArgs, scheduleResult] {
	return func(_ tool.Context, in scheduleArgs) (scheduleResult, error) {
		now := time.Now()
		wakeAt, rerr := resolveWakeAt(in, now)
		if rerr != "" {
			return scheduleResult{Ack: "rejected: " + rerr}, nil
		}
		if maxDefer > 0 {
			cap := now.Add(maxDefer)
			if wakeAt.After(cap) {
				return scheduleResult{
					Ack: fmt.Sprintf("rejected: wake-time exceeds MaxDefer of %s; use a shorter interval", maxDefer),
				}, nil
			}
		}
		ev := ScheduleEvent{
			WakeAt:     wakeAt,
			NextPrompt: strings.TrimSpace(in.NextPrompt),
			Detail:     strings.TrimSpace(in.Detail),
			Time:       now,
		}
		// Non-blocking send: the autonomous driver consumes exactly
		// one event per turn. A second call in the same turn (which
		// the model shouldn't make) gets dropped on the floor at the
		// channel and the driver sees only the first.
		select {
		case ch <- ev:
		default:
		}
		return scheduleResult{
			Ack:    fmt.Sprintf("deferred until %s", wakeAt.UTC().Format(time.RFC3339)),
			WakeAt: wakeAt.UTC().Format(time.RFC3339),
		}, nil
	}
}

// resolveWakeAt parses the model's wake_at / wake_in_sec args into an
// absolute time. Returns a non-empty reason string when the args are
// invalid (exactly-one rule, malformed RFC3339, negative duration).
func resolveWakeAt(in scheduleArgs, now time.Time) (time.Time, string) {
	hasAbs := strings.TrimSpace(in.WakeAt) != ""
	hasRel := in.WakeInSec != 0
	if hasAbs && hasRel {
		return time.Time{}, "provide exactly one of wake_at or wake_in_sec, not both"
	}
	if !hasAbs && !hasRel {
		return time.Time{}, "provide exactly one of wake_at or wake_in_sec"
	}
	if hasAbs {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(in.WakeAt))
		if err != nil {
			return time.Time{}, fmt.Sprintf("wake_at is not valid RFC3339: %v", err)
		}
		return t, ""
	}
	if in.WakeInSec < 0 {
		return time.Time{}, "wake_in_sec must be non-negative"
	}
	return now.Add(time.Duration(in.WakeInSec) * time.Second), ""
}
