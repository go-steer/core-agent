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
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metric export for MCP server health (#338 Phase 3).

const (
	// MetricServerStatus is a presence gauge: one point of value 1
	// per configured MCP server, dimensioned by status. Alert on
	// `core_agent.mcp.server.status{mcp.status="error"} == 1`.
	MetricServerStatus = "core_agent.mcp.server.status"

	AttrMCPServer = "mcp.server"
	// AttrMCPStatus carries the ACTUAL Server.Status enum: "ok" or
	// "error", set once when Build starts the server and never
	// transitioned afterward. No invented lifecycle states — if
	// richer status ever ships on Server, the gauge picks it up
	// automatically.
	AttrMCPStatus = "mcp.status"
)

// RegisterMetrics wires the MCP status gauge against mp. servers is
// the slice mcp.Build returned — written once at startup and
// read-only afterward, so the callback can walk it without locking.
func RegisterMetrics(mp metric.MeterProvider, servers []*Server) (metric.Registration, error) {
	if mp == nil {
		return nil, fmt.Errorf("mcp.RegisterMetrics: nil MeterProvider")
	}
	meter := mp.Meter("github.com/go-steer/core-agent/v2/pkg/mcp")

	status, err := meter.Int64ObservableGauge(
		MetricServerStatus,
		metric.WithDescription("Configured MCP servers by startup status (value is always 1; alert on the status dimension)."),
		metric.WithUnit("{server}"),
	)
	if err != nil {
		return nil, fmt.Errorf("mcp: status instrument: %w", err)
	}

	callback := func(_ context.Context, o metric.Observer) error {
		for _, s := range servers {
			if s == nil {
				continue
			}
			o.ObserveInt64(status, 1, metric.WithAttributes(
				attribute.String(AttrMCPServer, s.Name),
				attribute.String(AttrMCPStatus, s.Status),
			))
		}
		return nil
	}
	return meter.RegisterCallback(callback, status)
}
