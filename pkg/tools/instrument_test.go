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
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// timedFakeTool is a runnable fake whose Run behavior is scripted.
type timedFakeTool struct {
	name  string
	sleep time.Duration
	err   error
}

func (f *timedFakeTool) Name() string                            { return f.name }
func (f *timedFakeTool) Description() string                     { return "fake" }
func (f *timedFakeTool) IsLongRunning() bool                     { return false }
func (f *timedFakeTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: f.name}
}
func (f *timedFakeTool) Run(_ adktool.Context, _ any) (map[string]any, error) {
	if f.sleep > 0 {
		time.Sleep(f.sleep)
	}
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"ok": true}, nil
}

// notRunnable satisfies adktool.Tool but not runnableTool.
type notRunnable struct{}

func (notRunnable) Name() string        { return "not_runnable" }
func (notRunnable) Description() string { return "n/a" }
func (notRunnable) IsLongRunning() bool { return false }

func setupInstrumenter(t *testing.T) (*DurationInstrumenter, func() metricdata.ResourceMetrics) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	di, err := NewDurationInstrumenter(mp)
	if err != nil {
		t.Fatalf("NewDurationInstrumenter: %v", err)
	}
	return di, func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		return rm
	}
}

// toolHistPoints digs the tool-duration histogram points out of a
// collection, keyed by their full attribute set.
func toolHistPoints(t *testing.T, rm metricdata.ResourceMetrics) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != MetricGenAIToolDuration {
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

func attrString(t *testing.T, set attribute.Set, key string) (string, bool) {
	t.Helper()
	v, ok := set.Value(attribute.Key(key))
	if !ok {
		return "", false
	}
	return v.AsString(), true
}

func TestDurationInstrumenter_RecordsSuccess(t *testing.T) {
	t.Parallel()
	di, collect := setupInstrumenter(t)
	wrapped := di.Instrument([]adktool.Tool{&timedFakeTool{name: "read_file"}})
	if _, err := wrapped[0].(runnableTool).Run(nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	pts := toolHistPoints(t, collect())
	if len(pts) != 1 {
		t.Fatalf("got %d histogram points, want 1", len(pts))
	}
	if pts[0].Count != 1 {
		t.Errorf("count = %d, want 1", pts[0].Count)
	}
	if name, _ := attrString(t, pts[0].Attributes, AttrGenAIToolName); name != "read_file" {
		t.Errorf("%s = %q, want read_file", AttrGenAIToolName, name)
	}
	// error.type must be ABSENT on success — clean success series.
	if _, present := attrString(t, pts[0].Attributes, AttrErrorType); present {
		t.Errorf("error.type present on successful call")
	}
}

func TestDurationInstrumenter_ErrorTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want string
	}{
		{context.Canceled, ToolErrorCanceled},
		{context.DeadlineExceeded, ToolErrorTimeout},
		{errors.New("exploded"), ToolErrorOther},
	}
	for _, tc := range cases {
		di, collect := setupInstrumenter(t)
		wrapped := di.Instrument([]adktool.Tool{&timedFakeTool{name: "bash", err: tc.err}})
		if _, err := wrapped[0].(runnableTool).Run(nil, nil); err == nil {
			t.Fatal("Run should propagate the error")
		}
		pts := toolHistPoints(t, collect())
		if len(pts) != 1 {
			t.Fatalf("%v: got %d points, want 1", tc.err, len(pts))
		}
		if got, _ := attrString(t, pts[0].Attributes, AttrErrorType); got != tc.want {
			t.Errorf("%v: error.type = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// TestDurationInstrumenter_IncludesLockWait pins the deliberate
// timing posture: the timer wraps OUTSIDE the #460 serializer, so a
// mutating tool's recorded duration includes the wait for the
// per-agent mutation lock.
func TestDurationInstrumenter_IncludesLockWait(t *testing.T) {
	t.Parallel()
	di, collect := setupInstrumenter(t)
	var mu MutationSerializer

	// bash is classified mutating, so serializeOne wraps it.
	serialized := SerializeMutating([]adktool.Tool{&timedFakeTool{name: "bash"}}, &mu)
	timed := di.Instrument(serialized)

	const hold = 60 * time.Millisecond
	mu.Lock()
	release := make(chan struct{})
	go func() {
		time.Sleep(hold)
		mu.Unlock()
		close(release)
	}()
	if _, err := timed[0].(runnableTool).Run(nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	<-release

	pts := toolHistPoints(t, collect())
	if len(pts) != 1 {
		t.Fatalf("got %d points, want 1", len(pts))
	}
	if pts[0].Sum < hold.Seconds() {
		t.Errorf("recorded duration %.4fs < lock hold %.4fs; timer must wrap outside the serializer", pts[0].Sum, hold.Seconds())
	}
}

// TestTimedTool_ProcessRequest_PacksWrapperNotInner mirrors the gate
// and serializer contract: dispatch must route through the timer.
func TestTimedTool_ProcessRequest_PacksWrapperNotInner(t *testing.T) {
	t.Parallel()
	di, _ := setupInstrumenter(t)
	inner := &timedFakeTool{name: "list_clusters"}
	wrapped := di.Instrument([]adktool.Tool{inner})

	req := &model.LLMRequest{}
	tt, ok := wrapped[0].(*timedTool)
	if !ok {
		t.Fatalf("wrapped tool is %T, want *timedTool", wrapped[0])
	}
	if err := tt.ProcessRequest(nil, req); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}
	got, ok := req.Tools["list_clusters"]
	if !ok {
		t.Fatalf("req.Tools missing entry; have %v", req.Tools)
	}
	if got != tt {
		t.Errorf("req.Tools[list_clusters] = %T, want the *timedTool wrapper", got)
	}
	if tt.Declaration() == nil || tt.Declaration().Name != "list_clusters" {
		t.Errorf("Declaration not forwarded from inner tool")
	}
}

func TestDurationInstrumenter_PassThroughs(t *testing.T) {
	t.Parallel()
	di, _ := setupInstrumenter(t)

	// Non-runnable tools pass through unwrapped.
	nr := notRunnable{}
	out := di.Instrument([]adktool.Tool{nr})
	if out[0] != adktool.Tool(nr) {
		t.Errorf("non-runnable tool was wrapped: %T", out[0])
	}

	// Nil instrumenter passes everything through (failed-constructor
	// degrade path).
	var nilDI *DurationInstrumenter
	in := []adktool.Tool{&timedFakeTool{name: "x"}}
	if got := nilDI.Instrument(in); &got[0] != &in[0] && got[0] != in[0] {
		t.Errorf("nil instrumenter must pass tools through")
	}
	if got := nilDI.InstrumentToolset(nil); got != nil {
		t.Errorf("nil toolset must pass through as nil")
	}
}

// fakeToolset yields a fixed tool list.
type fakeToolset struct{ tools []adktool.Tool }

func (s *fakeToolset) Name() string { return "static" }
func (s *fakeToolset) Tools(_ adkagent.ReadonlyContext) ([]adktool.Tool, error) {
	return s.tools, nil
}

func TestDurationInstrumenter_ToolsetWrapsLazily(t *testing.T) {
	t.Parallel()
	di, collect := setupInstrumenter(t)
	inner := &timedFakeTool{name: "mcp_thing", err: errors.New("boom")}
	ts := di.InstrumentToolset(&fakeToolset{tools: []adktool.Tool{inner}})

	got, err := ts.Tools(nil)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d tools, want 1", len(got))
	}
	if _, err := got[0].(runnableTool).Run(nil, nil); err == nil {
		t.Fatal("Run should propagate error")
	}
	pts := toolHistPoints(t, collect())
	if len(pts) != 1 {
		t.Fatalf("got %d points, want 1 (toolset-yielded tool not timed)", len(pts))
	}
}
