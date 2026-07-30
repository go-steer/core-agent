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

package digest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// NOT t.Parallel(): the digest telemetry counters are package-global
// and this test resets them.
func TestRegisterMetrics_EmitsPerMethodSeries(t *testing.T) {
	ResetTelemetry()
	t.Cleanup(ResetTelemetry)

	recordTelemetry(MethodStructuralJSON, 1000, 300) // 700 saved
	recordTelemetry(MethodStructuralJSON, 500, 400)  // 100 saved
	recordTelemetry(MethodPassthrough, 200, 200)     // 0 saved

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if _, err := RegisterMetrics(mp); err != nil {
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

	calls, ok := byName[MetricDigestCalls].Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("calls metric missing or wrong type: %+v", byName[MetricDigestCalls])
	}
	got := map[string]int64{}
	for _, dp := range calls.DataPoints {
		v, _ := dp.Attributes.Value(attribute.Key(AttrDigestMethod))
		got[v.AsString()] = dp.Value
	}
	if got[MethodStructuralJSON] != 2 || got[MethodPassthrough] != 1 {
		t.Errorf("calls = %v, want structural_json=2 passthrough=1", got)
	}
	if _, present := got[MethodLLMFallback]; present {
		t.Error("llm_fallback series present with zero calls (zero suppression broken)")
	}

	saved, ok := byName[MetricDigestBytesSaved].Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("bytes_saved metric missing or wrong type")
	}
	for _, dp := range saved.DataPoints {
		v, _ := dp.Attributes.Value(attribute.Key(AttrDigestMethod))
		switch v.AsString() {
		case MethodStructuralJSON:
			if dp.Value != 800 {
				t.Errorf("structural bytes_saved = %d, want 800", dp.Value)
			}
		case MethodPassthrough:
			t.Error("passthrough bytes_saved series present (always 0; zero suppression broken)")
		}
	}
}

func TestRegisterMetrics_NilProvider(t *testing.T) {
	if _, err := RegisterMetrics(nil); err == nil {
		t.Error("expected error for nil MeterProvider")
	}
}
