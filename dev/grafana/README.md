# Grafana dashboards

Starter dashboards for a `core-agent` daemon scraped by Prometheus.

| File | Dashboard | UID |
| --- | --- | --- |
| `core-agent-overview.json` | Core Agent — Overview | `core-agent-overview` |

## Prerequisites

The daemon has to be exporting metrics, which is a **separate switch from
tracing** — `OTEL_TRACES_EXPORTER` gives you spans and nothing else:

```json
{
  "otel": {
    "metrics": {
      "exporter": "prometheus",
      "prometheus_addr": ":9464"
    }
  }
}
```

or `OTEL_METRICS_EXPORTER=prometheus`. That opens a dedicated listener
serving `/metrics`; point a Prometheus scrape job at it.

## Import

Grafana → Dashboards → **New** → **Import** → upload the JSON. There are
no import-time inputs to fill in; the datasource is chosen from the
dashboard itself, via two picker variables at the top:

- **Datasource** — any Prometheus datasource. Every panel reads
  `${datasource}`, so switching it repoints the whole dashboard.
- **Job** — populated by `label_values(go_goroutine_count, job)` against
  that datasource, multi-select with an *All* option. Every panel filters
  on it, so a fleet is one import: tick the daemons you want.

If **Job** comes up empty, the datasource isn't scraping a `core-agent`
daemon with metrics enabled (`go_goroutine_count` is the Go runtime
series every metrics-enabled daemon exports).

## What's in the overview

Five rows — **Spend**, **Tokens**, **Latency**, **Context and digest**,
**Health** — and fifteen content panels: spend rate by model,
priced-vs-lower-bound 24h spend, subagent spend, token throughput by
type, prompt-cache ROI, tool latency quantiles, slowest tools, turn
latency by agent, digest bytes saved, context events, MCP server health,
watchdog alerts by signal, autonomous runs by stop reason, the attach
listener, and Go runtime liveness.

## The spec of record

The panels are derived from, not the source of, the instrument
inventory. For what each series means, its unit, its attributes, the
cardinality controls, and the caveats that make a couple of these
panels narrower than they look:

<https://go-steer.github.io/core-agent/concepts/metrics/>

`pkg/telemetry/dashboard_test.go` enforces the chain — every metric a
panel queries must appear on that page, and every metric that page
names must exist in the Go tree. A rename that skips either artifact
fails the build.
