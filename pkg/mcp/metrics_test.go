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

package mcp

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestRegisterMetrics_StatusGauge(t *testing.T) {
	t.Parallel()
	servers := []*Server{
		{Name: "gke", Status: StatusOK},
		{Name: "broken", Status: StatusError},
		nil, // tolerated
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if _, err := RegisterMetrics(mp, servers); err != nil {
		t.Fatalf("RegisterMetrics: %v", err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	var pts []metricdata.DataPoint[int64]
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == MetricServerStatus {
				pts = m.Data.(metricdata.Gauge[int64]).DataPoints
			}
		}
	}
	if len(pts) != 2 {
		t.Fatalf("got %d status points, want 2", len(pts))
	}
	statuses := map[string]string{}
	for _, dp := range pts {
		if dp.Value != 1 {
			t.Errorf("status gauge value = %d, want 1", dp.Value)
		}
		name, _ := dp.Attributes.Value(attribute.Key(AttrMCPServer))
		status, _ := dp.Attributes.Value(attribute.Key(AttrMCPStatus))
		statuses[name.AsString()] = status.AsString()
	}
	if statuses["gke"] != StatusOK || statuses["broken"] != StatusError {
		t.Errorf("statuses = %v, want gke=ok broken=error", statuses)
	}
}
