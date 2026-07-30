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

package autonomous

import (
	"go.opentelemetry.io/otel/metric"
)

// Autonomous-run metrics (#338 Phase 3). One sync counter increment
// per completed Run, dimensioned by the ACTUAL StopReason string
// constants (plus "error" for runs that abort before a reason is
// assigned).
//
// Scope note for dashboard builders: the daemon binary does not call
// autonomous.Run — its consumers are background subagent spawns
// (pkg/agent/background) and library embedders driving the loop
// directly. A daemon with no autonomous spawns legitimately shows
// zero here.
const (
	MetricAutonomousRuns = "core_agent.autonomous.runs"

	// AttrStopReason carries the StopReason constant that ended the
	// run: completed, max_turns_exceeded, max_tokens_exceeded,
	// max_cost_exceeded, wallclock_exceeded, context_cancelled,
	// retry_policy_aborted, deferred — or "error" when the run
	// failed before any reason was assigned.
	AttrStopReason = "stop_reason"

	// StopReasonErrorFallback is the AttrStopReason value for
	// error-return paths that never assigned a StopReason.
	StopReasonErrorFallback = "error"
)

const meterName = "github.com/go-steer/core-agent/v2/pkg/agent/autonomous"

func newRunsCounter(mp metric.MeterProvider) (metric.Int64Counter, error) {
	return mp.Meter(meterName).Int64Counter(
		MetricAutonomousRuns,
		metric.WithDescription("Autonomous runs completed, by stop reason."),
		metric.WithUnit("{run}"),
	)
}
