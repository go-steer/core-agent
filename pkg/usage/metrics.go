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

package usage

// Metric export for the usage subsystem. See docs/metrics-design.md
// for the full instrument catalog and semantic-convention rationale.
//
// This file wires async observable instruments over Tracker
// snapshots. The Tracker itself is unchanged — RegisterMetrics reads
// via TotalsByModel() and Duration() during each MeterProvider export
// interval. No call-site instrumentation and no double-counting risk.

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// TrackedSession pairs a Tracker with the session-identity fields we
// stamp as metric attributes. Populated by the daemon at observer
// registration time; a session's identity does not change over its
// lifetime so callers can snapshot once.
//
// AppName and UserID are optional. Empty values are dropped from the
// attribute set — the resulting series carry SessionID only, which
// is the load-bearing identity for the "per-session drill-down"
// story.
type TrackedSession struct {
	Tracker   *Tracker
	SessionID string
	AppName   string
	UserID    string
}

// TrackerProvider enumerates live sessions for the metrics observer to
// sample on each export interval. Implementations are responsible for
// thread-safe access to the underlying session registry.
//
// The interface is deliberately narrow so callers can supply either
// an attach-mode SessionRegistry adapter (multi-session daemons) or a
// SingleTracker wrapper (non-attach daemons with a single primary
// tracker) without pulling pkg/attach into this package's imports.
type TrackerProvider interface {
	Trackers() []TrackedSession
}

// SingleTracker adapts a single (tracker, sessionID) pair to
// TrackerProvider. Used by daemons running without attach-mode where
// there is exactly one session and no registry to iterate.
type SingleTracker struct {
	Tracker   *Tracker
	SessionID string
	AppName   string
	UserID    string
}

// Trackers implements TrackerProvider. Returns a single-element slice
// (or empty if Tracker is nil).
func (s SingleTracker) Trackers() []TrackedSession {
	if s.Tracker == nil {
		return nil
	}
	return []TrackedSession{{
		Tracker:   s.Tracker,
		SessionID: s.SessionID,
		AppName:   s.AppName,
		UserID:    s.UserID,
	}}
}

// Metric names — GenAI semconv where a stable name exists, otherwise
// core_agent.* for our own surface. See docs/metrics-design.md for
// the full rationale on why we adopt GenAI semconv (cloud vendor
// dashboards render token / cost panels automatically).
const (
	// #nosec G101 -- OTel GenAI semconv metric name, not a credential.
	MetricGenAITokenUsage = "gen_ai.client.token.usage"
	MetricSessionTurns    = "core_agent.session.turns"
	MetricSessionCost     = "core_agent.session.cost_usd"
	MetricSessionDuration = "core_agent.session.duration"
	// MetricDigestSubagentCost lives here rather than pkg/digest
	// because the accumulator is Tracker.DigestSavings() — the digest
	// package's own TelemetrySnapshot carries no cost field.
	MetricDigestSubagentCost = "core_agent.digest.subagent.cost_usd"
)

// Attribute keys. GenAI semconv keys use the exact upstream spelling
// so consumers with dashboards keyed on gen_ai.* work out of the
// box. Session identity uses session.id (semconv-adjacent — the
// GenAI SIG has proposed but not finalized a gen_ai.session.id).
const (
	AttrGenAIModel = "gen_ai.request.model"
	// #nosec G101 -- OTel GenAI semconv attribute key, not a credential.
	AttrGenAITokenType = "gen_ai.token.type"
	AttrSessionID      = "session.id"
	AttrAppName        = "app.name"
	AttrUserID         = "user.id"
	// AttrPriced tags the cost series with whether every turn for the
	// model had a known catalog rate. priced=false means CostUSD is a
	// lower bound because at least one turn was unpriced (rate unknown,
	// billed as $0) — dashboards can filter it out of "exact spend"
	// panels rather than under-reporting. See #368.
	AttrPriced = "priced"
)

// Token-type attribute values. Match the Turn breakdown fields.
const (
	TokenTypeInput   = "input"
	TokenTypeOutput  = "output"
	TokenTypeCached  = "cached"
	TokenTypeThought = "thoughts"
	TokenTypeToolUse = "tool_use"
)

// RegisterOption tunes RegisterMetrics.
type RegisterOption func(*registerOptions)

type registerOptions struct {
	sessionLabels bool
}

// WithoutSessionLabels drops the per-session identity attributes
// (session.id, app.name, user.id) from every usage metric and
// aggregates values across sessions before observing. For fleet
// operators where per-session labels would blow up series
// cardinality. Two consequences of aggregation:
//
//   - core_agent.session.duration is not emitted at all — a
//     wall-clock duration aggregated across sessions is meaningless.
//   - `priced` on the cost series is the AND across sessions: false
//     if ANY session had an unpriced turn for that model.
//
// Aggregation (rather than merely stripping attributes) is load-
// bearing: two Observe calls with identical attribute sets in one
// callback are last-wins in the OTel SDK, so stripping alone would
// silently report only one session's totals.
func WithoutSessionLabels() RegisterOption {
	return func(o *registerOptions) { o.sessionLabels = false }
}

// RegisterMetrics wires the async usage observers against mp. Callers
// pass the process-global MeterProvider (obtained via otel.GetMeterProvider())
// so metric points flow into whichever reader(s) telemetry.SetupMetrics
// installed. tp is called on every export interval — implementations
// must be cheap and thread-safe.
//
// Returns the registered Registration so the caller can Unregister
// on shutdown if desired; typical usage discards it (the MeterProvider
// shutdown cleans up).
func RegisterMetrics(mp metric.MeterProvider, tp TrackerProvider, opts ...RegisterOption) (metric.Registration, error) {
	if mp == nil {
		return nil, fmt.Errorf("usage.RegisterMetrics: nil MeterProvider")
	}
	if tp == nil {
		return nil, fmt.Errorf("usage.RegisterMetrics: nil TrackerProvider")
	}
	ro := registerOptions{sessionLabels: true}
	for _, opt := range opts {
		opt(&ro)
	}

	meter := mp.Meter("github.com/go-steer/core-agent/v2/pkg/usage")

	tokens, err := meter.Int64ObservableCounter(
		MetricGenAITokenUsage,
		metric.WithDescription("Cumulative GenAI tokens consumed, broken down by type and model."),
		metric.WithUnit("{token}"),
	)
	if err != nil {
		return nil, fmt.Errorf("usage: token instrument: %w", err)
	}

	turns, err := meter.Int64ObservableCounter(
		MetricSessionTurns,
		metric.WithDescription("Cumulative model turns per session, broken down by model."),
		metric.WithUnit("{turn}"),
	)
	if err != nil {
		return nil, fmt.Errorf("usage: turn instrument: %w", err)
	}

	cost, err := meter.Float64ObservableCounter(
		MetricSessionCost,
		metric.WithDescription("Cumulative cost in USD per session, broken down by model."),
		metric.WithUnit("USD"),
	)
	if err != nil {
		return nil, fmt.Errorf("usage: cost instrument: %w", err)
	}

	duration, err := meter.Float64ObservableGauge(
		MetricSessionDuration,
		metric.WithDescription("Wall-clock duration of the session in seconds."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("usage: duration instrument: %w", err)
	}

	digestCost, err := meter.Float64ObservableCounter(
		MetricDigestSubagentCost,
		metric.WithDescription("Cumulative cost in USD of agentic digest subagent calls."),
		metric.WithUnit("USD"),
	)
	if err != nil {
		return nil, fmt.Errorf("usage: digest subagent cost instrument: %w", err)
	}

	// observeModel lands the per-model series (turns, cost+priced,
	// token breakdown) under the given base attribute set.
	observeModel := func(o metric.Observer, base []attribute.KeyValue, model string, totals Totals) {
		// Explicit copy avoids append() mutating base's backing
		// array — the same slice is reused across every model, so an
		// aliased append would silently overwrite the previous
		// model's attribute.
		modelAttrs := make([]attribute.KeyValue, 0, len(base)+1)
		modelAttrs = append(modelAttrs, base...)
		modelAttrs = append(modelAttrs, attribute.String(AttrGenAIModel, model))
		o.ObserveInt64(turns, int64(totals.Turns), metric.WithAttributes(modelAttrs...))

		// Cost carries an extra `priced` dimension so a total
		// that includes unpriced (rate-unknown) turns is
		// distinguishable from an exact one — see #368. Kept
		// off the turn/token series to hold their cardinality
		// down; cost is the only surface where "$0 because
		// unknown" is misleading.
		costAttrs := make([]attribute.KeyValue, 0, len(modelAttrs)+1)
		costAttrs = append(costAttrs, modelAttrs...)
		costAttrs = append(costAttrs, attribute.Bool(AttrPriced, totals.UnpricedTurns == 0))
		o.ObserveFloat64(cost, totals.CostUSD, metric.WithAttributes(costAttrs...))

		// One token observation per token-type dimension.
		// Skip zero-valued fields to keep low-cardinality
		// series clean — a session that never consumed
		// cached tokens shouldn't produce a `type=cached`
		// series at all.
		observeToken(o, tokens, modelAttrs, TokenTypeInput, totals.InputTokens)
		observeToken(o, tokens, modelAttrs, TokenTypeOutput, totals.OutputTokens)
		observeToken(o, tokens, modelAttrs, TokenTypeCached, totals.CachedInputTokens)
		observeToken(o, tokens, modelAttrs, TokenTypeThought, totals.ThoughtsTokens)
		observeToken(o, tokens, modelAttrs, TokenTypeToolUse, totals.ToolUseTokens)
	}

	perSession := func(_ context.Context, o metric.Observer) error {
		for _, ts := range tp.Trackers() {
			if ts.Tracker == nil {
				continue
			}
			sessionAttrs := sessionAttributes(ts)
			o.ObserveFloat64(duration, ts.Tracker.Duration().Seconds(), metric.WithAttributes(sessionAttrs...))
			if dc := ts.Tracker.DigestSavings().AgenticSubagentCostUSD; dc > 0 {
				o.ObserveFloat64(digestCost, dc, metric.WithAttributes(sessionAttrs...))
			}
			for model, totals := range ts.Tracker.TotalsByModel() {
				observeModel(o, sessionAttrs, model, totals)
			}
		}
		return nil
	}

	// aggregated merges every tracker's totals per model BEFORE
	// observing (see WithoutSessionLabels: identical-attribute
	// observations are last-wins, so stripping labels without
	// aggregating would drop all but one session).
	aggregated := func(_ context.Context, o metric.Observer) error {
		byModel := map[string]Totals{}
		var digestUSD float64
		for _, ts := range tp.Trackers() {
			if ts.Tracker == nil {
				continue
			}
			digestUSD += ts.Tracker.DigestSavings().AgenticSubagentCostUSD
			for model, totals := range ts.Tracker.TotalsByModel() {
				agg := byModel[model]
				agg.Turns += totals.Turns
				agg.InputTokens += totals.InputTokens
				agg.CachedInputTokens += totals.CachedInputTokens
				agg.OutputTokens += totals.OutputTokens
				agg.ThoughtsTokens += totals.ThoughtsTokens
				agg.ToolUseTokens += totals.ToolUseTokens
				agg.CostUSD += totals.CostUSD
				agg.UnpricedTurns += totals.UnpricedTurns
				byModel[model] = agg
			}
		}
		if digestUSD > 0 {
			o.ObserveFloat64(digestCost, digestUSD)
		}
		for model, totals := range byModel {
			observeModel(o, nil, model, totals)
		}
		// core_agent.session.duration deliberately not observed: a
		// wall-clock duration aggregated across sessions has no
		// meaning.
		return nil
	}

	callback := perSession
	if !ro.sessionLabels {
		callback = aggregated
	}
	return meter.RegisterCallback(callback, tokens, turns, cost, duration, digestCost)
}

// sessionAttributes builds the identity attribute set for a
// TrackedSession, dropping optional fields that are empty so the
// series don't sprout meaningless "" labels.
func sessionAttributes(ts TrackedSession) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.String(AttrSessionID, ts.SessionID)}
	if ts.AppName != "" {
		attrs = append(attrs, attribute.String(AttrAppName, ts.AppName))
	}
	if ts.UserID != "" {
		attrs = append(attrs, attribute.String(AttrUserID, ts.UserID))
	}
	return attrs
}

func observeToken(o metric.Observer, inst metric.Int64ObservableCounter, base []attribute.KeyValue, tokenType string, v int) {
	if v == 0 {
		return
	}
	attrs := make([]attribute.KeyValue, 0, len(base)+1)
	attrs = append(attrs, base...)
	attrs = append(attrs, attribute.String(AttrGenAITokenType, tokenType))
	o.ObserveInt64(inst, int64(v), metric.WithAttributes(attrs...))
}
