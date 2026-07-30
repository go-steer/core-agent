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

package agent

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// Agent-level metrics (#338 Phase 3). Names and attributes match
// ADK's cross-language metrics schema (adk.dev/observability/metrics)
// verbatim so dashboards built for ADK Python / Kotlin agents work
// unchanged. ADK-Go doesn't emit these yet (upstream TODO(#479)).
//
// The schema's remaining histograms — gen_ai.agent.request.size,
// gen_ai.agent.response.size, gen_ai.agent.workflow.steps — are
// deliberately NOT emitted: their units and semantics are
// undocumented upstream (absent even from adk-python's source), and
// emitting a guess would create false cross-language compatibility.
// Revisit when ADK defines them.

const (
	// MetricGenAIInvocationDuration is the ADK-schema histogram of
	// end-to-end prompt→response turn latency.
	MetricGenAIInvocationDuration = "gen_ai.agent.invocation.duration"

	// AttrGenAIAgentName carries the agent's configured name
	// (WithName; "core_agent" by default) — distinguishes the daemon
	// root agent from spawned subagents in shared dashboards.
	AttrGenAIAgentName = "gen_ai.agent.name"

	// AttrErrorType marks failed turns. Only present on error; the
	// values are the stable attach.ClassifyTurnError kind enum
	// (config_error, auth_error, rate_limited, ...), never raw
	// error text.
	AttrErrorType = "error.type"
)

// meterName is the instrumentation scope for all pkg/agent
// instruments, per the module-path convention (see pkg/usage).
const meterName = "github.com/go-steer/core-agent/v2/pkg/agent"

// core_agent.* instruments for agent-lifecycle state (#338 Phase 3).
const (
	MetricAgentCompactions  = "core_agent.agent.compactions"
	MetricAgentCheckpoints  = "core_agent.agent.checkpoints"
	MetricAgentSubtasks     = "core_agent.agent.subtasks"
	MetricAgentInboxPending = "core_agent.agent.inbox_pending"
	MetricWatchdogAlerts    = "core_agent.watchdog.alerts"

	// AttrMetricSessionID mirrors pkg/usage's session.id key (kept
	// local to avoid an import for one string).
	AttrMetricSessionID  = "session.id"
	AttrWatchdogSignal   = "signal"
	AttrWatchdogSeverity = "severity"
)

// newWatchdogAlertCounter builds the sync alert counter recorded in
// drainWatchdogAlerts.
func newWatchdogAlertCounter(mp metric.MeterProvider) (metric.Int64Counter, error) {
	return mp.Meter(meterName).Int64Counter(
		MetricWatchdogAlerts,
		metric.WithDescription("Watchdog alerts raised, by signal and severity."),
		metric.WithUnit("{alert}"),
	)
}

// AgentSource enumerates live agents for RegisterMetrics to sample on
// each export interval. Implementations must be cheap and
// thread-safe; nil agents in the slice are skipped.
type AgentSource interface {
	Agents() []*Agent
}

// RegisterMetrics wires the per-agent lifecycle observers against mp:
// compactions, checkpoints, subtasks (counters) and inbox_pending
// (gauge), each dimensioned by session.id. Call once at boot with the
// process-global MeterProvider.
//
// Counter sources are the in-memory per-process fields on Agent — NOT
// the eventlog-derived ContextStats, which is an O(events) scan per
// call and resumes across restarts (a restart would step an
// ObservableCounter backward or forward arbitrarily).
func RegisterMetrics(mp metric.MeterProvider, src AgentSource) (metric.Registration, error) {
	if mp == nil {
		return nil, fmt.Errorf("agent.RegisterMetrics: nil MeterProvider")
	}
	if src == nil {
		return nil, fmt.Errorf("agent.RegisterMetrics: nil AgentSource")
	}
	meter := mp.Meter(meterName)

	compactions, err := meter.Int64ObservableCounter(
		MetricAgentCompactions,
		metric.WithDescription("Successful context compactions this process."),
		metric.WithUnit("{compaction}"),
	)
	if err != nil {
		return nil, fmt.Errorf("agent: compactions instrument: %w", err)
	}
	checkpoints, err := meter.Int64ObservableCounter(
		MetricAgentCheckpoints,
		metric.WithDescription("Successful checkpoints this process."),
		metric.WithUnit("{checkpoint}"),
	)
	if err != nil {
		return nil, fmt.Errorf("agent: checkpoints instrument: %w", err)
	}
	subtasks, err := meter.Int64ObservableCounter(
		MetricAgentSubtasks,
		metric.WithDescription("Agentic subtasks run this process."),
		metric.WithUnit("{subtask}"),
	)
	if err != nil {
		return nil, fmt.Errorf("agent: subtasks instrument: %w", err)
	}
	inbox, err := meter.Int64ObservableGauge(
		MetricAgentInboxPending,
		metric.WithDescription("Messages waiting in the agent inbox."),
		metric.WithUnit("{message}"),
	)
	if err != nil {
		return nil, fmt.Errorf("agent: inbox instrument: %w", err)
	}

	callback := func(_ context.Context, o metric.Observer) error {
		for _, a := range src.Agents() {
			if a == nil {
				continue
			}
			attrs := metric.WithAttributes(attribute.String(AttrMetricSessionID, a.SessionID()))
			if v := a.compactionsDone.Load(); v > 0 {
				o.ObserveInt64(compactions, v, attrs)
			}
			if v := a.checkpointsDone.Load(); v > 0 {
				o.ObserveInt64(checkpoints, v, attrs)
			}
			a.mu.Lock()
			st := int64(a.subtaskCount)
			a.mu.Unlock()
			if st > 0 {
				o.ObserveInt64(subtasks, st, attrs)
			}
			o.ObserveInt64(inbox, int64(a.PendingInboxCount()), attrs)
		}
		return nil
	}
	return meter.RegisterCallback(callback, compactions, checkpoints, subtasks, inbox)
}

// recordInvocation lands one gen_ai.agent.invocation.duration point.
// error.type (the stable ClassifyTurnError kind) rides only on failed
// turns. Nil-guarded: hand-constructed Agents (tests) have no
// histogram and degrade to a no-op, matching the rest of Run's
// nil-field posture.
func (a *Agent) recordInvocation(seconds float64, turnErr error) {
	if a.invocationHist == nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.String(AttrGenAIAgentName, a.metricAgentName)}
	if turnErr != nil {
		attrs = append(attrs, attribute.String(AttrErrorType, attach.ClassifyTurnError(turnErr).Kind))
	}
	a.invocationHist.Record(context.Background(), seconds, metric.WithAttributes(attrs...))
}

// newInvocationHistogram builds the turn-duration histogram on mp.
// Bucket boundaries span real turn shapes: a single-model-call turn
// lands in seconds, an agentic multi-tool turn in minutes, an
// autonomous long turn up to an hour — the SDK defaults top out at
// 10s and would flatten everything beyond a trivial turn into one
// bucket.
func newInvocationHistogram(mp metric.MeterProvider) (metric.Float64Histogram, error) {
	return mp.Meter(meterName).Float64Histogram(
		MetricGenAIInvocationDuration,
		metric.WithDescription("End-to-end duration of one agent turn (prompt to terminal event)."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(1, 2, 5, 10, 30, 60, 120, 300, 600, 1200, 1800, 3600),
	)
}
