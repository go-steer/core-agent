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

package telemetry

// The OTel metrics pipeline is entirely separate from the traces
// pipeline in otel.go. ADK constructs a TracerProvider (and optionally
// a LoggerProvider) but has an open TODO(#479) upstream for
// MeterProvider — so metrics init runs on its own, without going
// through ADK. See docs/metrics-design.md for the full design.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// Metric mode names accepted by SetupMetrics. See OTELMetricsConfig
// in pkg/config for the config-file surface.
const (
	MetricsModeNone       = "none"
	MetricsModeOTLP       = "otlp"
	MetricsModePrometheus = "prometheus"
	MetricsModeBoth       = "both"
)

// MetricsExporterEnvVar overrides cfg.OTEL.Metrics.Exporter when set.
// Matches the OTEL_TRACES_EXPORTER convention (#315) — operators
// running multi-Pod K8s Deployments with a shared ConfigMap flip the
// metrics surface per-Pod via env instead of duplicating config.json.
const MetricsExporterEnvVar = "OTEL_METRICS_EXPORTER"

// DefaultPrometheusAddr is used when Prometheus mode is selected but
// neither cfg.PrometheusAddr nor opts.PrometheusAddr is set. Matches
// the OTel-conventional Prometheus reader port so tooling that
// auto-discovers it (kube-prometheus PodMonitor selectors, etc.)
// works out of the box.
const DefaultPrometheusAddr = ":9464"

// MetricsOptions carries deployment overrides. All fields may be zero
// — SetupMetrics fills in defaults from cfg + env.
type MetricsOptions struct {
	// PrometheusAddr overrides cfg.PrometheusAddr when non-empty.
	// Threaded from --metrics-addr in the CLI so operators can
	// override the config-file bind without editing JSON.
	PrometheusAddr string

	// ServiceName is stamped as the OTel service.name resource
	// attribute. Empty falls back to whatever OTEL_SERVICE_NAME
	// contributes via resource.Default(), then to "core-agent".
	ServiceName string
}

// SetupMetrics installs a global MeterProvider based on cfg. Returns a
// shutdown function the caller MUST call (typically deferred) so
// buffered metric points get flushed and the Prometheus listener stops
// cleanly.
//
// When cfg.Exporter is "" or "none" (and OTEL_METRICS_EXPORTER is
// unset), no provider is installed and shutdown returns nil — call
// sites stay clean either way.
//
// Errors are returned, not swallowed. The daemon entrypoint fails
// loudly on init failure to catch misconfigurations in dev rather
// than silently shipping a binary that emits no metrics. Callers
// preferring graceful degradation can inspect and continue.
func SetupMetrics(ctx context.Context, cfg config.OTELMetricsConfig, opts MetricsOptions) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }

	// Env-var override wins over config-file mode. Empty env leaves
	// config intact; matches OTEL_TRACES_EXPORTER handling in otel.go.
	mode := cfg.Exporter
	if envMode := os.Getenv(MetricsExporterEnvVar); envMode != "" {
		mode = envMode
	}

	switch mode {
	case "", MetricsModeNone:
		return noop, nil
	case MetricsModeOTLP, MetricsModePrometheus, MetricsModeBoth:
		// fall through
	default:
		return noop, fmt.Errorf("telemetry: unknown metrics mode %q (want none/otlp/prometheus/both)", mode)
	}

	res, err := buildResource(opts.ServiceName)
	if err != nil {
		return noop, fmt.Errorf("telemetry: metrics resource: %w", err)
	}

	var (
		readers   []sdkmetric.Reader
		shutdowns []func(context.Context) error
	)

	if mode == MetricsModeOTLP || mode == MetricsModeBoth {
		exp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return noop, fmt.Errorf("telemetry: otlp metric exporter: %w", err)
		}
		readers = append(readers, sdkmetric.NewPeriodicReader(exp))
		fmt.Fprintf(os.Stderr, "core-agent: telemetry: metrics OTLP HTTP exporter → %s\n", resolvedOTLPEndpoint())
	}

	if mode == MetricsModePrometheus || mode == MetricsModeBoth {
		// Dedicated registry (not prometheus.DefaultRegisterer) so
		// scrape output is isolated from any other Prometheus code
		// a consumer might register in the same process.
		reg := prometheus.NewRegistry()
		promReader, err := promexporter.New(
			promexporter.WithRegisterer(reg),
			promexporter.WithoutTargetInfo(),
			promexporter.WithoutScopeInfo(),
		)
		if err != nil {
			return noop, fmt.Errorf("telemetry: prometheus reader: %w", err)
		}
		readers = append(readers, promReader)

		addr := opts.PrometheusAddr
		if addr == "" {
			addr = cfg.PrometheusAddr
		}
		if addr == "" {
			addr = DefaultPrometheusAddr
		}
		stop, err := servePrometheus(addr, reg)
		if err != nil {
			return noop, fmt.Errorf("telemetry: prometheus scrape endpoint on %s: %w", addr, err)
		}
		shutdowns = append(shutdowns, stop)
		fmt.Fprintf(os.Stderr, "core-agent: telemetry: metrics Prometheus scrape → http://%s/metrics\n", addr)
	}

	providerOpts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		// AlwaysOnFilter records exemplars whenever a sync metric
		// is updated inside a live span context. Async observers
		// (our v1 default) don't run inside spans and so won't
		// attach exemplars until sync instruments land in PR #C
		// (mcp.tool_call.duration) — the wiring is in place now so
		// v2 gets it for free.
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOnFilter),
	}
	for _, r := range readers {
		providerOpts = append(providerOpts, sdkmetric.WithReader(r))
	}
	mp := sdkmetric.NewMeterProvider(providerOpts...)
	otel.SetMeterProvider(mp)

	shutdown = func(ctx context.Context) error {
		var errs []error
		if err := mp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("meter provider: %w", err))
		}
		for _, stop := range shutdowns {
			if err := stop(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	return shutdown, nil
}

// buildResource assembles the metrics-side OTel resource. Pulls
// env-supplied attrs first (OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES,
// process/OS attrs) via resource.Default(), then layers our defaults on
// top for keys the env didn't provide.
func buildResource(serviceName string) (*resource.Resource, error) {
	overlay := []attribute.KeyValue{}
	if serviceName != "" {
		overlay = append(overlay, attribute.String("service.name", serviceName))
	} else if os.Getenv("OTEL_SERVICE_NAME") == "" {
		overlay = append(overlay, attribute.String("service.name", "core-agent"))
	}
	// gcp.project_id — Cloud Monitoring's OTLP-receiver ingress
	// requires it and rejects entire batches missing it. Traces path
	// handles this via ADK; metrics path doesn't go through ADK, so
	// we stamp it here from the same GOOGLE_CLOUD_PROJECT env var.
	if p := os.Getenv("GOOGLE_CLOUD_PROJECT"); p != "" {
		overlay = append(overlay, attribute.String("gcp.project_id", p))
	}
	return resource.Merge(resource.Default(), resource.NewSchemaless(overlay...))
}

// resolvedOTLPEndpoint returns a string suitable for a boot-log
// diagnostic. Honors the metrics-specific env var first, then the
// generic OTLP env var, then falls back to the SDK default so
// operators can grep boot logs to confirm the export target.
func resolvedOTLPEndpoint() string {
	if e := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"); e != "" {
		return e
	}
	if e := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); e != "" {
		return e
	}
	return "(default localhost:4318)"
}

// servePrometheus starts an HTTP server on addr serving /metrics from
// the given registry. Uses net.Listen synchronously so bind failures
// (address in use, permission denied) surface as errors on the boot
// path rather than as background goroutine crashes.
//
// The handler is unauthenticated by design — matches Prometheus
// scrape norms and the k8s event watcher's endpoint (`lookout watch`
// in go-steer/k8s-lookout, formerly cmd/k8s-event-watcher). Operators
// wanting auth put a reverse proxy in front or bind to a
// cluster-internal address.
func servePrometheus(addr string, reg *prometheus.Registry) (func(context.Context) error, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "core-agent: telemetry: prometheus scrape server: %v\n", err)
		}
	}()
	return srv.Shutdown, nil
}
