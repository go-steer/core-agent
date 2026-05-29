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

// Package telemetry initializes OpenTelemetry for the agent loop.
//
// Telemetry is off by default — no exporter is configured — so a fresh
// invocation makes zero outbound network calls. Consumers opt in by
// setting one of:
//
//   - "console" — writes spans to stderr; useful for local debug
//   - "otlp"    — honors standard OTEL env vars
//     (OTEL_EXPORTER_OTLP_ENDPOINT, etc.) to ship to a collector
//   - "none"    — the default; no spans leave
//
// ADK's telemetry.New constructs providers but does NOT install them as
// OTEL globals; you must call SetGlobalOtelProviders explicitly or ADK's
// instrumentation will run against the noop tracer. This package handles
// that.
package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	adktelemetry "google.golang.org/adk/telemetry"
)

// Mode names recognized by Setup.
const (
	ModeNone    = "none"    // default; no spans exported
	ModeConsole = "console" // stdout exporter; for local dev
	ModeOTLP    = "otlp"    // honors OTEL_EXPORTER_OTLP_ENDPOINT etc.
)

// Setup configures OpenTelemetry. Returns a shutdown function the
// caller MUST call (typically deferred) so buffered spans get flushed.
//
// When mode is "" or "none", no providers are constructed and the
// shutdown returns nil — call sites stay clean either way.
func Setup(ctx context.Context, mode string) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }
	switch mode {
	case "", ModeNone:
		return noop, nil
	case ModeConsole, ModeOTLP:
		// fall through
	default:
		return noop, fmt.Errorf("telemetry: unknown mode %q (want console/otlp/none)", mode)
	}

	var opts []adktelemetry.Option
	if mode == ModeConsole {
		exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return noop, fmt.Errorf("telemetry: console exporter: %w", err)
		}
		opts = append(opts, adktelemetry.WithSpanProcessors(sdktrace.NewBatchSpanProcessor(exp)))
	}
	// For mode==otlp we let ADK's telemetry.New honor the standard
	// OTEL_EXPORTER_OTLP_* env vars without explicit option overrides.

	providers, err := adktelemetry.New(ctx, opts...)
	if err != nil {
		return noop, fmt.Errorf("telemetry: init: %w", err)
	}
	providers.SetGlobalOtelProviders()
	return providers.Shutdown, nil
}
