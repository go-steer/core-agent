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
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/models/mock"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

func invocationPoints(t *testing.T, rm metricdata.ResourceMetrics) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != MetricGenAIInvocationDuration {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("metric %s is %T, want Histogram[float64]", m.Name, m.Data)
			}
			if m.Unit != "s" {
				t.Errorf("unit = %q, want s", m.Unit)
			}
			return h.DataPoints
		}
	}
	return nil
}

func invAttr(set attribute.Set, key string) (string, bool) {
	v, ok := set.Value(attribute.Key(key))
	if !ok {
		return "", false
	}
	return v.AsString(), true
}

// TestRun_RecordsInvocationDuration pins the ADK-schema turn
// histogram: one point per completed turn, gen_ai.agent.name set,
// error.type absent on success.
func TestRun_RecordsInvocationDuration(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	prov := mock.NewEcho()
	llm, err := prov.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("provider.Model: %v", err)
	}
	a, err := New(llm, WithName("uat_agent"), WithMeterProvider(mp))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, err := range a.Run(context.Background(), "ping") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	pts := invocationPoints(t, rm)
	if len(pts) != 1 {
		t.Fatalf("got %d invocation points, want 1", len(pts))
	}
	if pts[0].Count != 1 {
		t.Errorf("count = %d, want 1", pts[0].Count)
	}
	if name, _ := invAttr(pts[0].Attributes, AttrGenAIAgentName); name != "uat_agent" {
		t.Errorf("%s = %q, want uat_agent", AttrGenAIAgentName, name)
	}
	if _, present := invAttr(pts[0].Attributes, AttrErrorType); present {
		t.Errorf("error.type present on successful turn")
	}
}

// TestRun_RecordsInvocationErrorType pins the error-turn shape:
// error.type carries a stable ClassifyTurnError kind, never raw
// error text.
func TestRun_RecordsInvocationErrorType(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	// minimalLLM (agent_test.go) errors on any GenerateContent call,
	// producing a turn error through the full Run path.
	a, err := New(minimalLLM{}, WithMeterProvider(mp))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sawErr := false
	for _, err := range a.Run(context.Background(), "ping") {
		if err != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("expected the turn to error")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	pts := invocationPoints(t, rm)
	if len(pts) != 1 {
		t.Fatalf("got %d invocation points, want 1", len(pts))
	}
	et, present := invAttr(pts[0].Attributes, AttrErrorType)
	if !present {
		t.Fatal("error.type missing on failed turn")
	}
	// The exact kind depends on the classifier's string matching; the
	// contract is "a stable enum value", pinned loosely here: short,
	// no spaces (raw error text would contain them).
	if et == "" || len(et) > 32 {
		t.Errorf("error.type = %q, want a short stable kind", et)
	}
}

type staticAgentSource []*Agent

func (s staticAgentSource) Agents() []*Agent { return s }

// TestAgentRegisterMetrics_ObservesLifecycle pins the per-agent
// observer: counters read the in-memory fields, gauges read live
// state, zero-valued counters are suppressed, and every series
// carries the agent's session.id.
func TestAgentRegisterMetrics_ObservesLifecycle(t *testing.T) {
	t.Parallel()
	prov := mock.NewEcho()
	llm, err := prov.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("mock model: %v", err)
	}
	busy, err := New(llm, WithSession("u", "sess-busy"))
	if err != nil {
		t.Fatalf("New busy: %v", err)
	}
	busy.compactionsDone.Add(2)
	busy.checkpointsDone.Add(1)
	if err := busy.Inject("pending message"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	idle, err := New(llm, WithSession("u", "sess-idle"))
	if err != nil {
		t.Fatalf("New idle: %v", err)
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if _, err := RegisterMetrics(mp, staticAgentSource{busy, idle, nil}); err != nil {
		t.Fatalf("RegisterMetrics: %v", err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	byName := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			byName[m.Name] = m
		}
	}

	comp := byName[MetricAgentCompactions].Data.(metricdata.Sum[int64])
	if len(comp.DataPoints) != 1 {
		t.Fatalf("compactions: want 1 point (idle agent suppressed), got %d", len(comp.DataPoints))
	}
	if comp.DataPoints[0].Value != 2 {
		t.Errorf("compactions = %d, want 2", comp.DataPoints[0].Value)
	}
	if sid, _ := invAttr(comp.DataPoints[0].Attributes, AttrMetricSessionID); sid != "sess-busy" {
		t.Errorf("compactions session.id = %q, want sess-busy", sid)
	}

	inbox := byName[MetricAgentInboxPending].Data.(metricdata.Gauge[int64])
	if len(inbox.DataPoints) != 2 {
		t.Fatalf("inbox_pending: want 2 points (gauge not suppressed), got %d", len(inbox.DataPoints))
	}
	for _, dp := range inbox.DataPoints {
		sid, _ := invAttr(dp.Attributes, AttrMetricSessionID)
		want := int64(0)
		if sid == "sess-busy" {
			want = 1
		}
		if dp.Value != want {
			t.Errorf("inbox_pending{%s} = %d, want %d", sid, dp.Value, want)
		}
	}
}

// TestCompact_IncrementsMetricCounter pins the increment site: a
// successful Compact bumps the in-memory counter the observer reads.
func TestCompact_IncrementsMetricCounter(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "SUMMARY"}
	a, err := New(llm, WithCompactor(NewDefaultCompactor()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "hello")
	plantEvent(t, a, genai.RoleModel, "world")
	if _, err := a.Compact(context.Background(), ""); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := a.compactionsDone.Load(); got != 1 {
		t.Errorf("compactionsDone = %d, want 1", got)
	}
}

// TestDrainWatchdogAlerts_CountsWithoutCallback pins that alerts are
// counted even when no host callback is wired — the internal buffer
// drains on Check, so this is the only counting opportunity.
func TestDrainWatchdogAlerts_CountsWithoutCallback(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	prov := mock.NewEcho()
	llm, err := prov.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("mock model: %v", err)
	}
	w := &fakeWatchdog{pending: []watchdog.Alert{
		{Signal: "repeated_tool_call", Severity: watchdog.SeverityWarn, Reason: "x3"},
		{Signal: "repeated_tool_call", Severity: watchdog.SeverityWarn, Reason: "x4"},
	}}
	// NO onAlert callback — the counter must still see both alerts.
	a, err := New(llm, WithMeterProvider(mp), WithWatchdog(w, nil), WithSession("u", "sess-wd"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.drainWatchdogAlerts()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != MetricWatchdogAlerts {
				continue
			}
			sum := m.Data.(metricdata.Sum[int64])
			if len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 2 {
				t.Fatalf("watchdog alerts = %+v, want single point of 2", sum.DataPoints)
			}
			if sig, _ := invAttr(sum.DataPoints[0].Attributes, AttrWatchdogSignal); sig != "repeated_tool_call" {
				t.Errorf("signal = %q", sig)
			}
			return
		}
	}
	t.Fatal("watchdog alerts metric not found")
}
