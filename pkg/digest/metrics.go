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
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metric export for the digest subsystem (#338 Phase 3). Async
// observables over the package-level Telemetry() snapshot — the
// counters are process-global (see the telemetry.go doc comment), so
// there are no per-session attributes here; the per-session cost
// surface lives in pkg/usage (core_agent.digest.subagent.cost_usd,
// backed by Tracker.DigestSavings).
//
// Deliberately absent: the design doc's store entries/bytes gauges —
// the production binary wires LazyStore→EventlogStore, neither of
// which tracks occupancy; FilesystemStore's Len/Bytes are test
// helpers.

const (
	// MetricDigestCalls counts Process calls per digest method.
	MetricDigestCalls = "core_agent.digest.calls"
	// MetricDigestBytesSaved is the cumulative (raw - digest) byte
	// reduction per method. Passthrough contributes 0 by definition.
	MetricDigestBytesSaved = "core_agent.digest.bytes_saved"

	// AttrDigestMethod carries the digest path taken:
	// passthrough | structural_json | llm_fallback.
	AttrDigestMethod = "digest.method"
)

// RegisterMetrics wires the digest observers against mp. Call once at
// boot with the process-global MeterProvider; the callback snapshots
// Telemetry() on every export interval.
func RegisterMetrics(mp metric.MeterProvider) (metric.Registration, error) {
	if mp == nil {
		return nil, fmt.Errorf("digest.RegisterMetrics: nil MeterProvider")
	}
	meter := mp.Meter("github.com/go-steer/core-agent/v2/pkg/digest")

	calls, err := meter.Int64ObservableCounter(
		MetricDigestCalls,
		metric.WithDescription("Cumulative MCP digest calls, by digest method."),
		metric.WithUnit("{call}"),
	)
	if err != nil {
		return nil, fmt.Errorf("digest: calls instrument: %w", err)
	}
	bytesSaved, err := meter.Int64ObservableCounter(
		MetricDigestBytesSaved,
		metric.WithDescription("Cumulative bytes removed from tool output by the digest, by method."),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, fmt.Errorf("digest: bytes_saved instrument: %w", err)
	}

	callback := func(_ context.Context, o metric.Observer) error {
		snap := Telemetry()
		for method, n := range snap.MethodCounts {
			if n == 0 {
				continue
			}
			o.ObserveInt64(calls, n, metric.WithAttributes(attribute.String(AttrDigestMethod, method)))
		}
		for method, b := range snap.BytesSaved {
			if b == 0 {
				continue
			}
			o.ObserveInt64(bytesSaved, b, metric.WithAttributes(attribute.String(AttrDigestMethod, method)))
		}
		return nil
	}
	return meter.RegisterCallback(callback, calls, bytesSaved)
}
