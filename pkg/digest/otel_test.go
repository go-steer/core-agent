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
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// installRecorder swaps in an in-memory tracer provider for the
// duration of the test, so we can assert Process emits the expected
// digest.process span with its attribute set. Restores the prior
// global provider (typically noop) on t.Cleanup.
//
// Not t.Parallel-safe: OTel's global provider is process-scoped, so
// tests that install a recorder cannot share the process with other
// tracing tests. Callers add t.Parallel selectively.
func installRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	// Reset the package-level tracer so the new provider takes effect
	// (otel.Tracer memoizes the returned Tracer per name; overwriting
	// the global provider doesn't retroactively update captured Tracers).
	tracer = tp.Tracer("core-agent/digest")
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		tracer = prev.Tracer("core-agent/digest")
	})
	return rec
}

// TestProcess_EmitsDigestProcessSpan pins the #223 Phase 5 contract:
// each Process call emits a digest.process span with core_agent.*
// attributes capturing the router's decision + savings math. This is
// the span OTel dashboards slice on to rank tools by savings.
func TestProcess_EmitsDigestProcessSpan(t *testing.T) {
	rec := installRecorder(t)

	// Under-threshold → passthrough path. Simplest to assert against.
	payload := []byte(`{"k":"v"}`)
	if _, err := Process(context.Background(), payload, Options{Threshold: 4096}); err != nil {
		t.Fatalf("Process: %v", err)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d: %+v", len(spans), spans)
	}
	sp := spans[0]
	if sp.Name() != "digest.process" {
		t.Errorf("span name = %q, want digest.process", sp.Name())
	}
	got := map[string]any{}
	for _, kv := range sp.Attributes() {
		got[string(kv.Key)] = kv.Value.AsInterface()
	}
	if got["core_agent.digest.path"] != "passthrough" {
		t.Errorf("path attr = %v, want passthrough", got["core_agent.digest.path"])
	}
	if got["core_agent.digest.original_bytes"] != int64(len(payload)) {
		t.Errorf("original_bytes attr = %v, want %d", got["core_agent.digest.original_bytes"], len(payload))
	}
	// Savings tokens estimated via 4-char heuristic; both original
	// and digest are the same (passthrough → digest == payload), so
	// savings_tokens_est should be 0. But the attribute must exist.
	if _, ok := got["core_agent.digest.savings_tokens_est"]; !ok {
		t.Errorf("savings_tokens_est attr missing: %v", got)
	}
}

// TestProcess_SpanMarksStructuralPath pins that the structural path
// stamps path=structural_json rather than defaulting to passthrough
// on a payload the pruner actually reduces.
func TestProcess_SpanMarksStructuralPath(t *testing.T) {
	rec := installRecorder(t)
	payload := []byte(`{"k":"` + strings.Repeat("x", 2000) + `"}`) // over threshold → pruner truncates
	if _, err := Process(context.Background(), payload, Options{Threshold: 100}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	for _, kv := range spans[0].Attributes() {
		if string(kv.Key) == "core_agent.digest.path" && kv.Value.AsString() != "structural_json" {
			t.Errorf("path attr = %q, want structural_json", kv.Value.AsString())
		}
	}
}
