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

package memory

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationName = "github.com/go-steer/core-agent/v3/pkg/memory"

type instruments struct {
	once          sync.Once
	opsCounter    metric.Int64Counter
	durationHist  metric.Float64Histogram
	itemsReturned metric.Int64Histogram
}

var memInstruments instruments

func getInstruments() *instruments {
	memInstruments.once.Do(func() {
		meter := otel.Meter(instrumentationName)
		memInstruments.opsCounter, _ = meter.Int64Counter(
			"core_agent.memory.operations",
			metric.WithDescription("Total memory router operations executed."),
		)
		memInstruments.durationHist, _ = meter.Float64Histogram(
			"core_agent.memory.duration_ms",
			metric.WithDescription("Latency of memory router operations in milliseconds."),
			metric.WithUnit("ms"),
		)
		memInstruments.itemsReturned, _ = meter.Int64Histogram(
			"core_agent.memory.items_returned",
			metric.WithDescription("Number of memory items returned per recall operation."),
		)
	})
	return &memInstruments
}

func startOpSpan(ctx context.Context, opName, backendName, mount string) (context.Context, func(items int, err error)) {
	start := time.Now()
	attrs := []attribute.KeyValue{
		attribute.String("memory.operation", opName),
		attribute.String("memory.backend", backendName),
		attribute.String("memory.mount", mount),
	}
	ctx, span := otel.Tracer(instrumentationName).Start(ctx, "core_agent.memory."+opName, trace.WithAttributes(attrs...))
	return ctx, func(items int, err error) {
		status := "ok"
		if err != nil {
			status = "error"
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.SetAttributes(attribute.Int("memory.items", items))
		span.End()

		inst := getInstruments()
		mAttrs := metric.WithAttributes(
			attribute.String("operation", opName),
			attribute.String("backend", backendName),
			attribute.String("mount", mount),
			attribute.String("status", status),
		)
		elapsedMS := float64(time.Since(start).Microseconds()) / 1000.0
		if inst.opsCounter != nil {
			inst.opsCounter.Add(ctx, 1, mAttrs)
		}
		if inst.durationHist != nil {
			inst.durationHist.Record(ctx, elapsedMS, mAttrs)
		}
		if opName == "recall" && inst.itemsReturned != nil {
			inst.itemsReturned.Record(ctx, int64(items), mAttrs)
		}
	}
}
