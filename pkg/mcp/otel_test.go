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
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// installRecorderMCP swaps in an in-memory tracer provider so tests
// can assert what spans + attributes the MCP wrap emits. Restores
// the prior global provider on cleanup.
//
// Not t.Parallel-safe: OTel's global provider is process-scoped;
// callers must serialize with other tracing tests.
func installRecorderMCP(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	// Re-resolve the package tracer against the new provider —
	// otel.Tracer memoizes per name.
	tracer = tp.Tracer("core-agent/mcp")
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		tracer = prev.Tracer("core-agent/mcp")
	})
	return rec
}

// readOnlySpanStub thinly wraps sdktrace.ReadOnlySpan so tests can
// pass a pointer without depending on the concrete provider type.
type readOnlySpanStub struct {
	sdktrace.ReadOnlySpan
}
