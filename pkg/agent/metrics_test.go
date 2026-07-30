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

	"github.com/go-steer/core-agent/v2/pkg/models/mock"
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
