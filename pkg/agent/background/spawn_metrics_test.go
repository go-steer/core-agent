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

package background

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// TestSpawn_InheritsParentMeterProvider pins the embedder-propagation
// fix: a background subagent's gen_ai.agent.invocation.duration must
// land on the SAME MeterProvider the parent resolved at construction,
// not the process global. An embedder that hands core-agent a
// non-global provider would otherwise see every background subagent's
// turns vanish from its pipeline. Fails on pre-change code (the
// subagent's agent.New fell back to otel.GetMeterProvider(), so this
// reader collects zero points).
func TestSpawn_InheritsParentMeterProvider(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	// completingLLM reaches StopReasonCompleted in two turns (report_done
	// then plain text), so the subagent runs to completion and records
	// exactly one invocation.
	prov := &recordingProvider{llm: &completingLLM{detail: "ok"}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
	}}, WithDefaultBudgets(Budgets{MaxTurns: 2}))
	defer mgr.Close()

	// Wire a parent carrying our non-global provider; WithBackgroundManager
	// attaches it as the manager's parent, which spawn.go reads for the
	// provider to inherit.
	parentLLM, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("parent Model: %v", err)
	}
	if _, err := agent.New(parentLLM,
		agent.WithBackgroundManager(mgr),
		agent.WithMeterProvider(mp),
	); err != nil {
		t.Fatalf("agent.New parent: %v", err)
	}

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("subagent goroutine didn't finish")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var pts []metricdata.HistogramDataPoint[float64]
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != agent.MetricGenAIInvocationDuration {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("metric %s is %T, want Histogram[float64]", m.Name, m.Data)
			}
			pts = h.DataPoints
		}
	}
	if len(pts) == 0 {
		t.Fatal("no invocation points on the parent's provider — background subagent did not inherit it")
	}
	// The subagent's series carries the bounded class-level name, never
	// its (possibly model-chosen) instance name.
	var found bool
	for _, dp := range pts {
		if v, ok := dp.Attributes.Value(attribute.Key(agent.AttrGenAIAgentName)); ok && v.AsString() == "background_subagent" {
			found = true
		}
	}
	if !found {
		t.Errorf("no %s=background_subagent series on the parent's provider; points=%d", agent.AttrGenAIAgentName, len(pts))
	}
}
