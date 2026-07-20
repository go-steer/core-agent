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

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

func TestSetupMetrics_None(t *testing.T) {
	t.Parallel()
	shutdown, err := SetupMetrics(context.Background(), config.OTELMetricsConfig{}, MetricsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("noop shutdown errored: %v", err)
	}
}

func TestSetupMetrics_UnknownMode(t *testing.T) {
	t.Parallel()
	_, err := SetupMetrics(context.Background(), config.OTELMetricsConfig{Exporter: "smoke-signals"}, MetricsOptions{})
	if err == nil || !strings.Contains(err.Error(), "unknown metrics mode") {
		t.Fatalf("expected unknown-mode error, got %v", err)
	}
}

// TestSetupMetrics_EnvVarOverridesMode pins the OTEL_METRICS_EXPORTER
// override — same load-bearing role as OTEL_TRACES_EXPORTER for
// multi-Pod K8s Deployments with a shared ConfigMap.
//
// Not t.Parallel: mutates process env + OTel global MeterProvider.
func TestSetupMetrics_EnvVarOverridesMode(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv(MetricsExporterEnvVar, MetricsModePrometheus)
	shutdown, err := SetupMetrics(
		context.Background(),
		config.OTELMetricsConfig{Exporter: MetricsModeNone, PrometheusAddr: addr},
		MetricsOptions{},
	)
	if err != nil {
		t.Fatalf("SetupMetrics: %v (env should have overridden config=none)", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// A successful Prometheus mode means the env override took effect
	// — ModeNone would have short-circuited to the noop path and never
	// bound the scrape port. Confirm by scraping the endpoint.
	waitForListener(t, addr)
	assertScrapeOK(t, "http://"+addr+"/metrics")
}

// TestSetupMetrics_EmptyEnvVarLeavesConfigMode pins that an unset env
// var doesn't override a non-none config value. Env-unset ≠ "none".
func TestSetupMetrics_EmptyEnvVarLeavesConfigMode(t *testing.T) {
	addr := freeAddr(t)
	t.Setenv(MetricsExporterEnvVar, "")
	shutdown, err := SetupMetrics(
		context.Background(),
		config.OTELMetricsConfig{Exporter: MetricsModePrometheus, PrometheusAddr: addr},
		MetricsOptions{},
	)
	if err != nil {
		t.Fatalf("SetupMetrics: %v (config-mode should have applied when env is empty)", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	waitForListener(t, addr)
	assertScrapeOK(t, "http://"+addr+"/metrics")
}

// TestSetupMetrics_EnvVarInvalidValueSurfacesSameError pins that an
// invalid env-var value produces the same "unknown metrics mode"
// error as an invalid config value — no silent fallthrough that could
// mask an operator typo in a K8s manifest.
func TestSetupMetrics_EnvVarInvalidValueSurfacesSameError(t *testing.T) {
	t.Setenv(MetricsExporterEnvVar, "smoke-signals")
	_, err := SetupMetrics(context.Background(), config.OTELMetricsConfig{Exporter: MetricsModeNone}, MetricsOptions{})
	if err == nil || !strings.Contains(err.Error(), "unknown metrics mode") {
		t.Fatalf("expected unknown-mode error for invalid env value, got %v", err)
	}
}

// TestSetupMetrics_Prometheus_ServesScrape verifies the scrape
// endpoint returns a 200 with the Prometheus text-format content
// type. Doesn't inspect payload contents — that's the observer
// tests' job.
func TestSetupMetrics_Prometheus_ServesScrape(t *testing.T) {
	addr := freeAddr(t)
	shutdown, err := SetupMetrics(
		context.Background(),
		config.OTELMetricsConfig{Exporter: MetricsModePrometheus, PrometheusAddr: addr},
		MetricsOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	waitForListener(t, addr)
	assertScrapeOK(t, "http://"+addr+"/metrics")

	// /healthz probes the scrape server without touching the metric
	// registry — useful for K8s liveness / readiness.
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", resp.StatusCode)
	}
}

// TestSetupMetrics_Prometheus_DefaultAddr verifies the default bind
// address gets used when neither cfg.PrometheusAddr nor
// opts.PrometheusAddr is set.
//
// Skipped when :9464 is already bound (common in shared CI runners).
func TestSetupMetrics_Prometheus_DefaultAddr(t *testing.T) {
	ln, err := net.Listen("tcp", DefaultPrometheusAddr)
	if err != nil {
		t.Skipf("default port %s already bound: %v", DefaultPrometheusAddr, err)
	}
	_ = ln.Close()

	shutdown, err := SetupMetrics(
		context.Background(),
		config.OTELMetricsConfig{Exporter: MetricsModePrometheus},
		MetricsOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	waitForListener(t, DefaultPrometheusAddr)
}

// TestSetupMetrics_Prometheus_BindFailureSurfaces pins fail-loudly
// behavior. If the scrape port can't bind (in use), the daemon must
// see an error at boot rather than silently ship without metrics.
func TestSetupMetrics_Prometheus_BindFailureSurfaces(t *testing.T) {
	// Hold a port so the SetupMetrics bind fails deterministically.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold-port listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	_, err = SetupMetrics(
		context.Background(),
		config.OTELMetricsConfig{
			Exporter:       MetricsModePrometheus,
			PrometheusAddr: ln.Addr().String(),
		},
		MetricsOptions{},
	)
	if err == nil {
		t.Fatal("expected bind-failure error, got nil")
	}
	if !strings.Contains(err.Error(), "prometheus scrape endpoint") {
		t.Errorf("expected prometheus-scrape error prefix, got %q", err.Error())
	}
}

// TestSetupMetrics_OptsAddrWinsOverCfg pins the precedence rule:
// opts.PrometheusAddr (from --metrics-addr) overrides
// cfg.PrometheusAddr (from config.json). Matches Unix
// explicit-over-implicit convention.
func TestSetupMetrics_OptsAddrWinsOverCfg(t *testing.T) {
	cfgAddr := freeAddr(t)
	optsAddr := freeAddr(t)
	shutdown, err := SetupMetrics(
		context.Background(),
		config.OTELMetricsConfig{Exporter: MetricsModePrometheus, PrometheusAddr: cfgAddr},
		MetricsOptions{PrometheusAddr: optsAddr},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	waitForListener(t, optsAddr)
	assertScrapeOK(t, "http://"+optsAddr+"/metrics")

	// cfgAddr must NOT have been bound.
	if _, err := http.Get("http://" + cfgAddr + "/metrics"); err == nil {
		t.Errorf("cfg addr %s was bound; opts should have won", cfgAddr)
	}
}

// freeAddr grabs and releases a port to get a likely-free address.
// Small race window, acceptable for local tests.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeAddr listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// waitForListener polls the given address until Dial succeeds or the
// deadline hits. Compensates for the goroutine gap between
// SetupMetrics returning and srv.Serve accepting.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("listener at %s did not come up within deadline", addr)
}

func assertScrapeOK(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s status = %d, body=%s", url, resp.StatusCode, body)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") && !strings.HasPrefix(ct, "application/openmetrics-text") {
		t.Errorf("unexpected content-type %q for %s", ct, url)
	}
}
