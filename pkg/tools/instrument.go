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
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// Per-tool duration metrics (#338 Phase 3). The instrument name and
// attribute keys match ADK's cross-language metrics schema
// (adk.dev/observability/metrics) verbatim, so dashboards built for
// ADK Python / Kotlin agents work unchanged against a Go daemon.
// ADK-Go itself doesn't emit these yet (upstream TODO(#479)); when it
// does, this wrapper can be dropped if the schema still matches.

const (
	// MetricGenAIToolDuration is the ADK-schema histogram of
	// individual tool-call latency.
	MetricGenAIToolDuration = "gen_ai.tool.execution.duration"

	// AttrGenAIToolName carries the tool's registered name.
	AttrGenAIToolName = "gen_ai.tool.name"

	// AttrErrorType marks failed calls. Only present on error —
	// success series stay clean, matching the OTel semconv
	// convention for error.type.
	AttrErrorType = "error.type"
)

// Closed error.type enum for tool calls. Never raw err.Error() —
// that would be unbounded-cardinality.
const (
	ToolErrorCanceled = "canceled"
	ToolErrorTimeout  = "timeout"
	ToolErrorOther    = "_OTHER"
)

// DurationInstrumenter wraps tools so every Run records
// gen_ai.tool.execution.duration. Construct once per agent (the
// instrument is created here, not per wrapped tool; the OTel SDK
// dedups identical instruments per meter anyway, but one histogram
// handle keeps the hot path allocation-free).
//
// The recorded duration is wall-clock across the outermost Run —
// deliberately INCLUDING the #460 mutation-lock wait and, for gated
// tools, any permission-prompt wait: that is the latency the model
// (and the operator watching the session) actually observes. Headless
// deployments — the observability target — have no prompts, so the
// ask-mode skew is a documented trade, not a bug.
type DurationInstrumenter struct {
	hist metric.Float64Histogram
}

// NewDurationInstrumenter builds the instrumenter against mp. A noop
// MeterProvider (metrics disabled) yields a working instrumenter
// whose Record calls are cheap no-ops — callers don't gate on the
// metrics mode.
func NewDurationInstrumenter(mp metric.MeterProvider) (*DurationInstrumenter, error) {
	if mp == nil {
		return nil, errors.New("tools: NewDurationInstrumenter requires a MeterProvider")
	}
	meter := mp.Meter("github.com/go-steer/core-agent/v2/pkg/tools")
	hist, err := meter.Float64Histogram(
		MetricGenAIToolDuration,
		metric.WithDescription("Duration of individual tool executions."),
		metric.WithUnit("s"),
		// Sub-second file reads through multi-minute bash / MCP
		// calls; the SDK default buckets top out at 10s and would
		// flatten the long tail this histogram exists to expose.
		metric.WithExplicitBucketBoundaries(0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300),
	)
	if err != nil {
		return nil, err
	}
	return &DurationInstrumenter{hist: hist}, nil
}

// Instrument wraps every runnable tool in ts with a duration timer.
// Non-runnable tools pass through untouched (same posture as the
// serializer and gate wrappers). A nil instrumenter passes everything
// through, so a failed constructor degrades to no metrics rather
// than no tools.
func (di *DurationInstrumenter) Instrument(ts []adktool.Tool) []adktool.Tool {
	if di == nil || di.hist == nil {
		return ts
	}
	out := make([]adktool.Tool, len(ts))
	for i, t := range ts {
		out[i] = di.instrumentOne(t)
	}
	return out
}

// InstrumentToolset wraps a toolset so every tool it yields is timed.
// Tools resolve lazily (MCP toolsets fetch on demand), so wrapping
// happens per Tools() call — the per-call cost is one thin wrapper
// allocation per tool; the histogram handle is shared.
func (di *DurationInstrumenter) InstrumentToolset(ts adktool.Toolset) adktool.Toolset {
	if di == nil || di.hist == nil || ts == nil {
		return ts
	}
	return &timedToolset{inner: ts, di: di}
}

func (di *DurationInstrumenter) instrumentOne(t adktool.Tool) adktool.Tool {
	if t == nil {
		return t
	}
	if _, ok := t.(runnableTool); !ok {
		return t
	}
	return &timedTool{inner: t, di: di}
}

type timedToolset struct {
	inner adktool.Toolset
	di    *DurationInstrumenter
}

func (s *timedToolset) Name() string { return s.inner.Name() }

func (s *timedToolset) Tools(ctx agent.ReadonlyContext) ([]adktool.Tool, error) {
	upstream, err := s.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	return s.di.Instrument(upstream), nil
}

// timedTool times the outermost Run. Same wrapping shape as
// serializedTool / gatedTool.
type timedTool struct {
	inner adktool.Tool
	di    *DurationInstrumenter
}

func (tt *timedTool) Name() string        { return tt.inner.Name() }
func (tt *timedTool) Description() string { return tt.inner.Description() }
func (tt *timedTool) IsLongRunning() bool { return tt.inner.IsLongRunning() }

func (tt *timedTool) Declaration() *genai.FunctionDeclaration {
	if rn, ok := tt.inner.(runnableTool); ok {
		return rn.Declaration()
	}
	return nil
}

// ProcessRequest packs tt — the wrapper — so ADK dispatch routes
// through the timer instead of bypassing it. Same shape as
// serializedTool.ProcessRequest.
func (tt *timedTool) ProcessRequest(ctx adktool.Context, req *model.LLMRequest) error {
	return PackTool(req, tt)
}

func (tt *timedTool) Run(ctx adktool.Context, args any) (map[string]any, error) {
	rn, ok := tt.inner.(runnableTool)
	if !ok {
		return nil, nil
	}
	start := time.Now()
	out, err := rn.Run(ctx, args)
	attrs := []attribute.KeyValue{attribute.String(AttrGenAIToolName, tt.inner.Name())}
	if err != nil {
		attrs = append(attrs, attribute.String(AttrErrorType, classifyToolError(err)))
	}
	// ctx (adktool.Context embeds context.Context) carries the live
	// mcp.tool_call / turn span, so exemplar linkage works when a
	// span is recording. Record ignores ctx cancellation. Nil guard:
	// ADK always passes a real Context, but direct callers (tests,
	// other wrappers) may not.
	rctx := context.Context(context.Background())
	if ctx != nil {
		rctx = ctx
	}
	tt.di.hist.Record(rctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
	return out, err
}

// classifyToolError maps a tool error to the closed error.type enum.
func classifyToolError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return ToolErrorCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return ToolErrorTimeout
	default:
		return ToolErrorOther
	}
}
