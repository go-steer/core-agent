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
// The mode is normally set via cfg.OTEL.Exporter in .agents/config.json,
// but the standard OpenTelemetry SDK env var `OTEL_TRACES_EXPORTER`
// overrides when set (matches the OTel spec's env-var-wins convention).
// This is the load-bearing knob for multi-daemon k8s deployments where
// the base ConfigMap is shared across Pods but each Pod's exporter
// target differs — operators wire it via a per-Deployment env-var
// patch instead of duplicating config.json per daemon.
//
// ADK's telemetry.New constructs providers but does NOT install them as
// OTEL globals; you must call SetGlobalOtelProviders explicitly or ADK's
// instrumentation will run against the noop tracer. This package handles
// that.
package telemetry

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	adktelemetry "google.golang.org/adk/telemetry"
)

// Mode names recognized by Setup.
const (
	ModeNone    = "none"    // default; no spans exported
	ModeConsole = "console" // stdout exporter; for local dev
	ModeOTLP    = "otlp"    // honors OTEL_EXPORTER_OTLP_ENDPOINT etc.
)

// TracesExporterEnvVar names the OTel-standard env var that overrides
// the config-file exporter mode. Same shape as the mode arg: "none",
// "console", or "otlp". Unknown values fall through to the mode
// switch below and return the same error the config-file path does.
const TracesExporterEnvVar = "OTEL_TRACES_EXPORTER"

// Setup configures OpenTelemetry. Returns a shutdown function the
// caller MUST call (typically deferred) so buffered spans get flushed.
//
// When mode is "" or "none", no providers are constructed and the
// shutdown returns nil — call sites stay clean either way.
//
// The OTel-standard env var `OTEL_TRACES_EXPORTER` overrides the
// passed mode when set. This is the load-bearing knob for k8s
// deployments with shared ConfigMaps but per-Pod exporter targets:
// operators patch the Deployment env instead of forking config.json.
// The override runs before the mode-validation switch, so an invalid
// env-var value produces the same clear error as an invalid config
// value.
func Setup(ctx context.Context, mode string) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }

	// Env-var override wins over the config-file mode. Empty string
	// leaves the config value intact (env-unset ≠ "select none").
	// Matches the OTel SDK spec convention where env vars override
	// in-process defaults.
	if envMode := os.Getenv(TracesExporterEnvVar); envMode != "" {
		mode = envMode
	}

	// Register the W3C TextMapPropagator globally REGARDLESS of the
	// exporter mode. Even with no exporter, downstream code that
	// starts spans against the noop tracer will produce contexts;
	// otelhttp middleware needs a propagator to inject/extract
	// traceparent headers so distributed-trace continuity works the
	// moment an operator flips exporter to otlp. Registering a
	// composite of TraceContext (traceparent) + Baggage covers the
	// W3C shape every OTel-instrumented downstream expects. See #217.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

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
