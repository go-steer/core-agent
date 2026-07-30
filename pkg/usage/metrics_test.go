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

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type staticProvider []TrackedSession

func (s staticProvider) Trackers() []TrackedSession { return s }

// setupReader wires a ManualReader-backed MeterProvider and returns a
// collector func that snapshots current metric state. Callers register
// their observer against mp, then call collect() to inspect what would
// have been exported.
func setupReader(t *testing.T) (*sdkmetric.MeterProvider, func() metricdata.ResourceMetrics) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	collect := func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("reader.Collect: %v", err)
		}
		return rm
	}
	return mp, collect
}

func TestRegisterMetrics_NilArgs(t *testing.T) {
	t.Parallel()
	if _, err := RegisterMetrics(nil, staticProvider{}); err == nil {
		t.Error("expected error for nil MeterProvider")
	}
	mp, _ := setupReader(t)
	if _, err := RegisterMetrics(mp, nil); err == nil {
		t.Error("expected error for nil TrackerProvider")
	}
}

// TestRegisterMetrics_EmitsInstruments pins the four instrument names,
// their units, and the session/model attribute shape. A tracker with
// two turns on two different models produces per-model series.
func TestRegisterMetrics_EmitsInstruments(t *testing.T) {
	t.Parallel()
	tracker := NewTracker()
	pricing := Pricing{InputPerMTok: 1.0, OutputPerMTok: 2.0}
	tracker.AppendUsage("gemini-2.5-pro", TurnUsage{InputTokens: 1000, OutputTokens: 500, CachedInputTokens: 300, ThoughtsTokens: 100, ToolUseTokens: 50}, pricing)
	tracker.AppendUsage("gemini-2.5-flash", TurnUsage{InputTokens: 200, OutputTokens: 80}, pricing)

	mp, collect := setupReader(t)
	if _, err := RegisterMetrics(mp, staticProvider{{Tracker: tracker, SessionID: "sess-A"}}); err != nil {
		t.Fatalf("RegisterMetrics: %v", err)
	}

	rm := collect()
	if len(rm.ScopeMetrics) == 0 {
		t.Fatal("no scope metrics emitted")
	}

	byName := indexByName(rm)
	want := []struct {
		name string
		unit string
	}{
		{MetricGenAITokenUsage, "{token}"},
		{MetricSessionTurns, "{turn}"},
		{MetricSessionCost, "USD"},
		{MetricSessionDuration, "s"},
	}
	for _, w := range want {
		m, ok := byName[w.name]
		if !ok {
			t.Errorf("missing metric %s", w.name)
			continue
		}
		if m.Unit != w.unit {
			t.Errorf("%s unit = %q, want %q", w.name, m.Unit, w.unit)
		}
	}

	// Per-model breakout: turns should have one series per model, each
	// with session.id="sess-A" and gen_ai.request.model=<model>.
	turnsData, ok := byName[MetricSessionTurns].Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("turns metric wrong shape: %T", byName[MetricSessionTurns].Data)
	}
	if len(turnsData.DataPoints) != 2 {
		t.Errorf("turns data points = %d, want 2 (one per model)", len(turnsData.DataPoints))
	}
	for _, dp := range turnsData.DataPoints {
		sid, _ := dp.Attributes.Value(AttrSessionID)
		if sid.AsString() != "sess-A" {
			t.Errorf("expected session.id=sess-A on turns data point, got %q", sid.AsString())
		}
		model, ok := dp.Attributes.Value(AttrGenAIModel)
		if !ok {
			t.Error("turns data point missing gen_ai.request.model attr")
		}
		if dp.Value != 1 {
			t.Errorf("turns count for model %s = %d, want 1", model.AsString(), dp.Value)
		}
	}
}

// TestRegisterMetrics_TokenTypeBreakdown pins that each populated
// token type produces its own series and zero-valued types are
// suppressed (keeps low-cardinality series clean).
func TestRegisterMetrics_TokenTypeBreakdown(t *testing.T) {
	t.Parallel()
	tracker := NewTracker()
	tracker.AppendUsage("m", TurnUsage{
		InputTokens: 100, OutputTokens: 50, CachedInputTokens: 30,
		// ThoughtsTokens + ToolUseTokens deliberately zero — those
		// series must NOT appear.
	}, Pricing{})

	mp, collect := setupReader(t)
	if _, err := RegisterMetrics(mp, staticProvider{{Tracker: tracker, SessionID: "sess-A"}}); err != nil {
		t.Fatalf("RegisterMetrics: %v", err)
	}

	rm := collect()
	tokens, ok := indexByName(rm)[MetricGenAITokenUsage].Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatal("token metric missing or wrong shape")
	}

	seen := map[string]int64{}
	for _, dp := range tokens.DataPoints {
		tt, ok := dp.Attributes.Value(AttrGenAITokenType)
		if !ok {
			t.Error("token data point missing token-type attr")
			continue
		}
		seen[tt.AsString()] = dp.Value
	}

	wantSeen := map[string]int64{
		TokenTypeInput:  100,
		TokenTypeOutput: 50,
		TokenTypeCached: 30,
	}
	for k, v := range wantSeen {
		if got := seen[k]; got != v {
			t.Errorf("token[%s] = %d, want %d", k, got, v)
		}
	}
	if _, present := seen[TokenTypeThought]; present {
		t.Error("thoughts token series present despite zero value")
	}
	if _, present := seen[TokenTypeToolUse]; present {
		t.Error("tool_use token series present despite zero value")
	}
}

// TestRegisterMetrics_MultipleSessions pins that per-session series
// stay separate — the drill-down story from the design doc's "How to
// look at metrics" section only works if session.id is stamped
// distinctly per tracker.
func TestRegisterMetrics_MultipleSessions(t *testing.T) {
	t.Parallel()
	a := NewTracker()
	a.AppendUsage("m", TurnUsage{InputTokens: 10, OutputTokens: 5}, Pricing{})
	b := NewTracker()
	b.AppendUsage("m", TurnUsage{InputTokens: 100, OutputTokens: 50}, Pricing{})

	mp, collect := setupReader(t)
	_, err := RegisterMetrics(mp, staticProvider{
		{Tracker: a, SessionID: "sess-A"},
		{Tracker: b, SessionID: "sess-B"},
	})
	if err != nil {
		t.Fatalf("RegisterMetrics: %v", err)
	}

	rm := collect()
	tokens, ok := indexByName(rm)[MetricGenAITokenUsage].Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatal("token metric missing")
	}

	perSession := map[string]int64{}
	for _, dp := range tokens.DataPoints {
		tt, _ := dp.Attributes.Value(AttrGenAITokenType)
		if tt.AsString() != TokenTypeInput {
			continue
		}
		sid, _ := dp.Attributes.Value(AttrSessionID)
		perSession[sid.AsString()] = dp.Value
	}
	if perSession["sess-A"] != 10 || perSession["sess-B"] != 100 {
		t.Errorf("per-session input tokens = %v, want sess-A=10 sess-B=100", perSession)
	}
}

// TestRegisterMetrics_OptionalAttributes pins that empty AppName /
// UserID don't leak as "" labels — the design doc calls for dropping
// them so the resulting series carry only meaningful identity.
func TestRegisterMetrics_OptionalAttributes(t *testing.T) {
	t.Parallel()
	tracker := NewTracker()
	tracker.AppendUsage("m", TurnUsage{InputTokens: 10, OutputTokens: 5}, Pricing{})

	mp, collect := setupReader(t)
	_, err := RegisterMetrics(mp, staticProvider{{
		Tracker: tracker, SessionID: "sess-A", AppName: "", UserID: "",
	}})
	if err != nil {
		t.Fatalf("RegisterMetrics: %v", err)
	}

	rm := collect()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			for _, dp := range dataPointAttributes(m) {
				if _, ok := dp.Value(AttrAppName); ok {
					t.Errorf("%s: unexpected app.name attribute for empty AppName", m.Name)
				}
				if _, ok := dp.Value(AttrUserID); ok {
					t.Errorf("%s: unexpected user.id attribute for empty UserID", m.Name)
				}
			}
		}
	}
}

// TestSingleTracker_AdaptsPrimary pins the SingleTracker convenience
// path used by non-attach daemons.
func TestSingleTracker_AdaptsPrimary(t *testing.T) {
	t.Parallel()
	if got := (SingleTracker{}).Trackers(); got != nil {
		t.Errorf("nil-tracker SingleTracker returned %v, want nil slice", got)
	}
	tr := NewTracker()
	st := SingleTracker{Tracker: tr, SessionID: "primary"}
	if got := st.Trackers(); len(got) != 1 || got[0].Tracker != tr || got[0].SessionID != "primary" {
		t.Errorf("SingleTracker adapter mismatch: %+v", got)
	}
}

func indexByName(rm metricdata.ResourceMetrics) map[string]metricdata.Metrics {
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func dataPointAttributes(m metricdata.Metrics) []attribute.Set {
	var out []attribute.Set
	switch d := m.Data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			out = append(out, dp.Attributes)
		}
	case metricdata.Sum[float64]:
		for _, dp := range d.DataPoints {
			out = append(out, dp.Attributes)
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			out = append(out, dp.Attributes)
		}
	case metricdata.Gauge[int64]:
		for _, dp := range d.DataPoints {
			out = append(out, dp.Attributes)
		}
	}
	return out
}

// TestRegisterMetrics_WithoutSessionLabels_Aggregates pins the
// labels-off contract: totals merge across sessions BEFORE observing
// (identical-attribute observations are last-wins in the SDK, so
// stripping labels without aggregating silently drops sessions), no
// session identity attrs anywhere, priced is the AND across sessions,
// and the per-session duration gauge disappears entirely.
func TestRegisterMetrics_WithoutSessionLabels_Aggregates(t *testing.T) {
	t.Parallel()
	a := NewTracker()
	a.AppendUsage("m", TurnUsage{InputTokens: 10, OutputTokens: 5}, Pricing{})
	a.AppendDigestSavings(DigestSavingsRecord{Path: "llm_fallback", SubagentCostUSD: 0.25})
	b := NewTracker()
	b.AppendUsage("m", TurnUsage{InputTokens: 30, OutputTokens: 7}, Pricing{Unpriced: true})
	b.AppendDigestSavings(DigestSavingsRecord{Path: "llm_fallback", SubagentCostUSD: 0.5})

	mp, collect := setupReader(t)
	_, err := RegisterMetrics(mp, staticProvider{
		{Tracker: a, SessionID: "s1", AppName: "app", UserID: "u"},
		{Tracker: b, SessionID: "s2", AppName: "app", UserID: "u"},
	}, WithoutSessionLabels())
	if err != nil {
		t.Fatalf("RegisterMetrics: %v", err)
	}
	byName := indexByName(collect())

	if _, ok := byName[MetricSessionDuration]; ok {
		t.Errorf("%s present in labels-off mode; aggregated wall-clock is meaningless", MetricSessionDuration)
	}

	turns, ok := byName[MetricSessionTurns].Data.(metricdata.Sum[int64])
	if !ok || len(turns.DataPoints) != 1 {
		t.Fatalf("turns: want exactly 1 aggregated point, got %+v", byName[MetricSessionTurns])
	}
	if turns.DataPoints[0].Value != 2 {
		t.Errorf("turns = %d, want 2 (1+1 across sessions)", turns.DataPoints[0].Value)
	}

	tokens := byName[MetricGenAITokenUsage].Data.(metricdata.Sum[int64])
	var sawInput bool
	for _, dp := range tokens.DataPoints {
		if v, _ := dp.Attributes.Value(attribute.Key(AttrGenAITokenType)); v.AsString() == TokenTypeInput {
			sawInput = true
			if dp.Value != 40 {
				t.Errorf("input tokens = %d, want 40 (10+30)", dp.Value)
			}
		}
	}
	if !sawInput {
		t.Error("no input token point in labels-off mode")
	}

	cost := byName[MetricSessionCost].Data.(metricdata.Sum[float64])
	if len(cost.DataPoints) != 1 {
		t.Fatalf("cost: want 1 aggregated point, got %d", len(cost.DataPoints))
	}
	if v, _ := cost.DataPoints[0].Attributes.Value(attribute.Key(AttrPriced)); v.AsBool() {
		t.Error("priced = true; one session had unpriced turns, AND-merge must yield false")
	}

	digest := byName[MetricDigestSubagentCost].Data.(metricdata.Sum[float64])
	if len(digest.DataPoints) != 1 || digest.DataPoints[0].Value != 0.75 {
		t.Errorf("digest subagent cost: want single 0.75 point, got %+v", digest.DataPoints)
	}

	// No series anywhere may carry session identity.
	for name, m := range byName {
		for _, set := range dataPointAttributes(m) {
			for _, key := range []string{AttrSessionID, AttrAppName, AttrUserID} {
				if _, ok := set.Value(attribute.Key(key)); ok {
					t.Errorf("%s carries %s in labels-off mode", name, key)
				}
			}
		}
	}
}

// TestRegisterMetrics_DigestSubagentCost_PerSession pins the new
// digest-cost series in default (labels-on) mode: one point per
// session that actually spent, zero-spend sessions suppressed.
func TestRegisterMetrics_DigestSubagentCost_PerSession(t *testing.T) {
	t.Parallel()
	spender := NewTracker()
	spender.AppendDigestSavings(DigestSavingsRecord{Path: "llm_fallback", SubagentCostUSD: 0.4})
	idle := NewTracker()

	mp, collect := setupReader(t)
	if _, err := RegisterMetrics(mp, staticProvider{
		{Tracker: spender, SessionID: "s1"},
		{Tracker: idle, SessionID: "s2"},
	}); err != nil {
		t.Fatalf("RegisterMetrics: %v", err)
	}
	byName := indexByName(collect())
	digest, ok := byName[MetricDigestSubagentCost].Data.(metricdata.Sum[float64])
	if !ok || len(digest.DataPoints) != 1 {
		t.Fatalf("digest cost: want 1 point (zero-spend suppressed), got %+v", byName[MetricDigestSubagentCost])
	}
	if v, _ := digest.DataPoints[0].Attributes.Value(attribute.Key(AttrSessionID)); v.AsString() != "s1" {
		t.Errorf("digest cost session = %q, want s1", v.AsString())
	}
}
