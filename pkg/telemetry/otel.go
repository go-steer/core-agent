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
	"log"
	"os"

	"github.com/go-logr/stdr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
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

	// Route OTel SDK internal diag messages + span-export errors to
	// stderr so exporter failures (unreachable collector, TLS mismatch,
	// wrong port, wrong protocol) surface loudly instead of silently
	// dropping spans. Default handlers are noop; without these two
	// hooks, "no spans in the backend" is indistinguishable from
	// "backend rejecting them silently." Verbosity gates via
	// OTEL_LOG_LEVEL — 0=fatal, 1=error (default), higher = more.
	logLevel := 1
	if lvl := os.Getenv("OTEL_LOG_LEVEL"); lvl == "debug" {
		logLevel = 8
	}
	otel.SetLogger(stdr.New(log.New(os.Stderr, "otel-diag ", log.LstdFlags)).V(logLevel))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Printf("otel-export: %v", err)
	}))

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
	switch mode {
	case ModeConsole:
		exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return noop, fmt.Errorf("telemetry: console exporter: %w", err)
		}
		opts = append(opts, adktelemetry.WithSpanProcessors(sdktrace.NewBatchSpanProcessor(exp)))
	case ModeOTLP:
		// Construct the OTLP HTTP exporter explicitly instead of relying
		// on ADK's implicit env-var wiring. Two reasons:
		//   1. Ownership — a construction error surfaces via
		//      telemetry.Setup's returned error, not silently
		//      swallowed inside ADK's configure() path.
		//   2. Log-visible endpoint — we log the resolved endpoint at
		//      construction, so operators can grep the daemon boot
		//      log to confirm the exporter is talking to the right
		//      collector. Without this line, "no spans arrive" is
		//      indistinguishable from "wrong endpoint".
		//
		// otlptracehttp.New honors OTEL_EXPORTER_OTLP_ENDPOINT +
		// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT +
		// OTEL_EXPORTER_OTLP_HEADERS + OTEL_EXPORTER_OTLP_INSECURE
		// internally, so operators still use the standard env vars.
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return noop, fmt.Errorf("telemetry: otlp http exporter: %w", err)
		}
		endpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
		if endpoint == "" {
			endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
		}
		if endpoint == "" {
			endpoint = "(default localhost:4318)"
		}
		log.Printf("core-agent: telemetry: OTLP HTTP exporter wired → %s", endpoint)
		opts = append(opts, adktelemetry.WithSpanProcessors(sdktrace.NewBatchSpanProcessor(exp)))
	}

	providers, err := adktelemetry.New(ctx, opts...)
	if err != nil {
		return noop, fmt.Errorf("telemetry: init: %w", err)
	}
	providers.SetGlobalOtelProviders()
	return providers.Shutdown, nil
}
