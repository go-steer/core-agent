---
title: Metrics
---


`core-agent` exports OpenTelemetry **metrics** — token and cost meters, two ADK-schema latency histograms, a set of `core_agent.*` subsystem meters, and Go runtime metrics — over OTLP, a Prometheus scrape endpoint, or both. They answer the questions traces are the wrong shape for: what is this fleet spending per hour, which tool is slow at p99, is an MCP server down, is the watchdog tripping.

Metrics are **off by default** and run on a **separate pipeline from [traces](/concepts/otel/)**. ADK-go has no `MeterProvider` (upstream TODO), so the daemon builds its own SDK provider in `pkg/telemetry.SetupMetrics` rather than going through ADK. Consequences worth knowing up front: `otel.exporter` (traces) and `otel.metrics.exporter` are independent switches, and the metrics pipeline honors `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` in addition to the generic `OTEL_EXPORTER_OTLP_ENDPOINT`.

---

## Enabling

### Config file

```json
{
  "otel": {
    "metrics": {
      "exporter": "prometheus",
      "prometheus_addr": ":9464",
      "session_labels": true
    }
  }
}
```

| Value for `exporter` | Behavior |
|---|---|
| `none` | Default. No `MeterProvider` installed; every instrument is a no-op. |
| `otlp` | OTLP **HTTP** exporter on a periodic reader. Target comes from `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`, then `OTEL_EXPORTER_OTLP_ENDPOINT`, then the SDK default `localhost:4318`. |
| `prometheus` | Serves `/metrics` on `prometheus_addr` (default `:9464`) for a scraper to pull. |
| `both` | Push and pull at the same time — the OTLP reader and the scrape endpoint share one provider. |

The Prometheus endpoint is a **dedicated listener**, not a route on the [attach listener](/reference/attach-http/). It is unauthenticated, by Prometheus convention — bind it to a cluster-internal address, or put a reverse proxy in front.

### Env-var override

`OTEL_METRICS_EXPORTER` overrides `otel.metrics.exporter` from the config file, the same way `OTEL_TRACES_EXPORTER` overrides `otel.exporter`. This is the knob for multi-Pod Kubernetes deployments where one shared ConfigMap can't carry a per-Pod exporter choice:

```bash
export OTEL_METRICS_EXPORTER=otlp
export OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.observability.svc:4318
export OTEL_SERVICE_NAME=core-agent
export OTEL_METRIC_EXPORT_INTERVAL=60000   # ms; SDK default
```

It is an override, not an additive setting: an empty value doesn't override, and `none` in the env turns metrics off even if the config file asked for them.

### CLI flag

`--metrics-addr :9464` overrides `otel.metrics.prometheus_addr`. It is **ignored unless** the exporter mode selects `prometheus` or `both` — passing it alone does not turn metrics on.

### Confirming it's up

The daemon prints one line per reader at startup:

```
core-agent: telemetry: metrics OTLP HTTP exporter → http://collector.observability.svc:4318
core-agent: telemetry: metrics Prometheus scrape → http://:9464/metrics
```

A bind failure on the scrape port is a **boot error**, not a background goroutine crash — the daemon refuses to start rather than run silently blind.

---

## What's emitted

Instrument names follow [OTel GenAI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/) where a stable upstream name exists, and `core_agent.*` otherwise. The two `gen_ai.*.duration` histograms match [ADK's cross-language metrics schema](https://adk.dev/observability/metrics) verbatim, so a dashboard built for an ADK Python or Kotlin agent works unchanged against a Go daemon.

The **Prometheus name** column is what the OTel Prometheus exporter actually publishes, unit suffixes and `_total` included — verified against a live scrape, not derived from the naming rules. Dots become underscores in both metric names and label keys (`session.id` → `session_id`).

### Cost and usage

| Metric | Prometheus name | Type | Unit | Attributes |
|---|---|---|---|---|
| `gen_ai.client.token.usage` | `gen_ai_client_token_usage_total` | Counter (async) | `{token}` | session identity, `gen_ai.request.model`, `gen_ai.token.type` |
| `core_agent.session.cost_usd` | `core_agent_session_cost_usd_USD_total` | Counter (async) | `USD` | session identity, `gen_ai.request.model`, `priced` |
| `core_agent.session.turns` | `core_agent_session_turns_total` | Counter (async) | `{turn}` | session identity, `gen_ai.request.model` |
| `core_agent.session.duration` | `core_agent_session_duration_seconds` | Gauge (async) | `s` | session identity |
| `core_agent.digest.subagent.cost_usd` | `core_agent_digest_subagent_cost_usd_USD_total` | Counter (async) | `USD` | session identity |

`gen_ai.token.type` is one of `input`, `output`, `cached`, `cache_write`, `thoughts`, `tool_use`. `cache_write` is disjoint from both `input` and `cached` and is billed at a premium — netting it against the `cached` discount is how you compute [prompt-caching](/agent-design/cost-efficiency/) ROI. Zero-valued types produce no series at all, so a session that never used a cache never grows a `cached` dimension.

`priced=false` means the cost figure is a **lower bound**: at least one turn used a model with no catalog rate and was billed as $0. Filter it out of "exact spend" panels rather than under-reporting.

### Latency

| Metric | Prometheus name | Type | Unit | Attributes |
|---|---|---|---|---|
| `gen_ai.agent.invocation.duration` | `gen_ai_agent_invocation_duration_seconds` | Histogram | `s` | `gen_ai.agent.name`, `error.type` (on failure only) |
| `gen_ai.tool.execution.duration` | `gen_ai_tool_execution_duration_seconds` | Histogram | `s` | `gen_ai.tool.name`, `error.type` (on failure only) |

One invocation point per agent turn, **including subagent turns**. Async and background subagents report under the bounded name `gen_ai.agent.name=background_subagent` rather than their own — an operator-authored roster could otherwise grow the label set without bound.

`error.type` is present only on failures, matching the semconv convention that success series stay clean. For tools it is a closed enum — `canceled`, `timeout`, `_OTHER` — never a raw error string.

Tool buckets run `0.01s … 300s` (the SDK default tops out at 10s and would flatten exactly the long tail this histogram exists to show).

### Subsystems

| Metric | Prometheus name | Type | Unit | Attributes |
|---|---|---|---|---|
| `core_agent.digest.calls` | `core_agent_digest_calls_total` | Counter (async) | `{call}` | `digest.method` |
| `core_agent.digest.bytes_saved` | `core_agent_digest_bytes_saved_total` | Counter (async) | `By` | `digest.method` |
| `core_agent.agent.compactions` | `core_agent_agent_compactions_total` | Counter (async) | `{compaction}` | `session.id` |
| `core_agent.agent.checkpoints` | `core_agent_agent_checkpoints_total` | Counter (async) | `{checkpoint}` | `session.id` |
| `core_agent.agent.subtasks` | `core_agent_agent_subtasks_total` | Counter (async) | `{subtask}` | `session.id` |
| `core_agent.agent.inbox_pending` | `core_agent_agent_inbox_pending` | Gauge (async) | `{message}` | `session.id` |
| `core_agent.watchdog.alerts` | `core_agent_watchdog_alerts_total` | Counter | `{alert}` | `signal`, `severity` |
| `core_agent.autonomous.runs` | `core_agent_autonomous_runs_total` | Counter | `{run}` | `stop_reason` |
| `core_agent.mcp.server.status` | `core_agent_mcp_server_status` | Gauge (async) | `{server}` | `mcp.server`, `mcp.status` |
| `core_agent.attach.sessions.active` | `core_agent_attach_sessions_active` | Gauge (async) | `{session}` | — |
| `core_agent.attach.subscribers` | `core_agent_attach_subscribers` | Gauge (async) | `{subscriber}` | `session.id` |
| `core_agent.attach.subscriber_drops` | `core_agent_attach_subscriber_drops_total` | Counter (async) | `{drop}` | — |
| `core_agent.attach.peers.active` | `core_agent_attach_peers_active` | Gauge (async) | `{peer}` | — |

Attribute value sets:

- `digest.method` — `passthrough` | `structural_json` | `llm_fallback`. Passthrough contributes `0` to `bytes_saved` by definition.
- `signal` — `repeated-tool-call` | `alternating-tool-cycle` | `dominant-tool-call` | `repeated-tool-name` | `tool-failure-streak`. `severity` — `warn` | `critical`. See [autonomous operations](/run/autonomous/operations/) for what the watchdog does when it trips.
- `stop_reason` — `completed`, `max_turns_exceeded`, `max_tokens_exceeded`, `max_cost_exceeded`, `wallclock_exceeded`, `context_cancelled`, `retry_policy_aborted`, `deferred`, plus `error` for failures that never reached a stop reason.
- `mcp.status` — `ok` | `error`, set once when the server starts and never transitioned. `core_agent.mcp.server.status` is a **presence gauge**: the value is always `1`, and the signal lives entirely in the `mcp.status` dimension.

### Go runtime

Enabling metrics also starts [`contrib/instrumentation/runtime`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/runtime) on the same provider — heap, GC, and goroutine visibility with no agent-loop instrumentation:

```
go_memory_used_bytes          go_memory_allocated_bytes_total
go_memory_gc_goal_bytes       go_memory_allocations_total
go_goroutine_count            go_processor_limit
go_config_gogc_percent
```

They double as a liveness check on the pipeline itself: if a scrape or export interval shows the `go_*` series and nothing else, the pipeline is fine and the agent simply hasn't done the thing you're measuring yet.

---

## Cardinality: `session_labels`

By default every usage metric carries `session.id`, `app.name`, and `user.id`. That is right for a workstation or a handful of long-lived sessions, and wrong for a fleet daemon churning thousands of short sessions across several models — series count is sessions × models × token types.

Set `otel.metrics.session_labels: false` to aggregate across sessions **before** observing. Two consequences, both deliberate:

- `core_agent.session.duration` is **not emitted at all**. A wall-clock duration aggregated across sessions is meaningless.
- `priced` becomes the AND across sessions: `false` if *any* session had an unpriced turn for that model.

Aggregation rather than mere label-stripping is load-bearing. Two `Observe` calls with identical attribute sets in one callback are last-wins in the OTel SDK, so stripping alone would silently report one session's totals as if they were the fleet's.

---

## Sample queries

PromQL, against the scrape endpoint:

**Spend rate, by model.** The one panel most operators want first.

```text
sum by (gen_ai_request_model) (rate(core_agent_session_cost_usd_USD_total[5m])) * 3600
```

**Spend you can actually trust.** Drop the lower-bound series.

```text
sum(rate(core_agent_session_cost_usd_USD_total{priced="true"}[5m])) * 3600
```

**Cache ROI.** Cached reads against the writes that paid for them.

```text
sum(rate(gen_ai_client_token_usage_total{gen_ai_token_type="cached"}[1h]))
  /
sum(rate(gen_ai_client_token_usage_total{gen_ai_token_type="cache_write"}[1h]))
```

**Tool p99, by tool.** The MCP layer is usually the biggest driver of session wall-clock.

```text
histogram_quantile(0.99,
  sum by (le, gen_ai_tool_name) (rate(gen_ai_tool_execution_duration_seconds_bucket[5m])))
```

**An MCP server is down.** Alert on the dimension, not the value.

```text
core_agent_mcp_server_status{mcp_status="error"} == 1
```

**The agent is looping.** Watchdog criticals are the cheap early signal; pair with a spend-rate alert.

```text
sum by (signal) (increase(core_agent_watchdog_alerts_total{severity="critical"}[15m])) > 0
```

**Autonomous runs hitting a ceiling** rather than finishing.

```text
sum by (stop_reason) (increase(core_agent_autonomous_runs_total{stop_reason!="completed"}[1h]))
```

**Digest is earning its keep.**

```text
sum by (digest_method) (rate(core_agent_digest_bytes_saved_total[30m]))
```

**SSE subscribers are being dropped** — a slow or wedged TUI client falling behind the broadcaster.

```text
increase(core_agent_attach_subscriber_drops_total[10m]) > 0
```

On a non-Prometheus backend the same queries hold with the original dotted names and no `_total` / unit suffix — e.g. in Cloud Monitoring MQL or a Grafana OTLP datasource, `core_agent.session.cost_usd` with a `gen_ai.request.model` group-by.

### Grafana

[`dev/grafana/core-agent-overview.json`](https://github.com/go-steer/core-agent/blob/main/dev/grafana/core-agent-overview.json) is an importable starter dashboard against a Prometheus datasource: spend rate by model, token throughput by type, digest bytes saved, tool-latency p50/p95/p99, MCP server status, and watchdog alerts. Import it, pick your datasource, and treat it as a starting point rather than a spec — the panels are exactly the queries above.

---

## Caveats

These are properties of the design, not bugs — worth knowing before you write an alert on top of them.

- **Tool duration includes wait, not just work.** `gen_ai.tool.execution.duration` is wall-clock across the outermost `Run`, deliberately including the mutation-lock wait and, for gated tools, the permission-prompt wait. That is the latency the model and the operator actually observe. Headless deployments — the observability target — have no prompts, so the interactive skew is a documented trade.
- **Per-session series restart on evict + lazy resume.** A resumed session rebuilds its tracker, so `core_agent.session.duration` measures the *current incarnation*, not the session's full life, and the per-session counters restart at zero. `session_labels: false` compensates via a retirement baseline that keeps the aggregate monotonic when sessions leave; per-session mode does not.
- **`core_agent.agent.compactions` counts this process, not this session's history.** It is an in-memory counter, deliberately not the eventlog-derived `ContextStats.CompactionCount` (an O(events) scan per read, and one that survives restarts — the wrong shape for a process-lifetime counter).
- **Async instruments carry no exemplars.** The provider installs an always-on exemplar filter, but exemplars attach to *sync* instruments recorded inside a live span. Today only the two histograms qualify; the async observers run outside any span, so trace-to-metric jumps work for tool and turn latency and not for the counters.
- **`core_agent.mcp.server.status` never transitions.** It reflects `Server.Status` as set when the server started. A server that dies later still reads `ok`.
- **Cost is a catalog estimate.** It is computed from the [pricing catalog](/reference/configuration/#pricing-top-level), not from a provider invoice. `priced` tells you when even the estimate is incomplete.

---

## Deploying on Kubernetes

### GKE, via Managed OpenTelemetry (OTLP)

The [`example-otel`](https://github.com/go-steer/core-agent/tree/main/examples/gke-troubleshoot-agent/deploy/overlays/example-otel) overlay of the GKE troubleshooting recipe wires metrics alongside traces. The [`components/otel`](https://github.com/go-steer/core-agent/tree/main/examples/gke-troubleshoot-agent/deploy/components/otel) component sets `OTEL_METRICS_EXPORTER=otlp` on the daemon; the `Instrumentation` CR supplies `OTEL_EXPORTER_OTLP_ENDPOINT` (the in-cluster managed collector) and `OTEL_METRIC_EXPORT_INTERVAL`, and the managed collector forwards to Cloud Monitoring.

Two prerequisites beyond the trace ones:

```bash
gcloud services enable monitoring.googleapis.com
gcloud projects add-iam-policy-binding <PROJECT> \
  --member="<POD-IDENTITY>" --role="roles/monitoring.metricWriter"
```

:::caution
The trace half of this overlay is verified end-to-end against a live GKE cluster. The **metrics half is not yet** — the local Prometheus path is covered by tests and manual UAT, but OTLP metrics landing in Cloud Monitoring from a real cluster has not been eyeballed. Treat the IAM and API prerequisites above as the documented-but-unconfirmed path, and check the collector logs if metrics don't appear ([#554](https://github.com/go-steer/core-agent/issues/554)).
:::

### Anywhere, via a Prometheus scrape

The pull path has no cloud dependency. Turn the endpoint on and let a scraper find it:

```yaml
spec:
  template:
    metadata:
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9464"
        prometheus.io/path: "/metrics"
    spec:
      containers:
        - name: core-agent
          env:
            - name: OTEL_METRICS_EXPORTER
              value: "prometheus"
          ports:
            - name: metrics
              containerPort: 9464
```

`:9464` is the OTel-conventional reader port, which is why it is the default — kube-prometheus `PodMonitor` selectors and Google Managed Prometheus both find it without extra configuration.

---

## Pitfalls

- **Metrics being off doesn't disable the code paths.** With `exporter: none` the global `MeterProvider` is a no-op and every `Record` / `Observe` is cheap and silent — call sites never gate on the mode. So "no data" and "not enabled" look identical from inside the process; check the boot lines.
- **`OTEL_METRICS_EXPORTER` and `OTEL_TRACES_EXPORTER` are separate switches.** Setting only the traces one — the common mistake, because the [OTel page](/concepts/otel/) is where most people start — gives you spans and no metrics.
- **Cloud Monitoring needs `gcp.project_id` on the resource.** Same requirement as Cloud Trace, and the metrics pipeline doesn't go through ADK, so `pkg/telemetry` stamps it from `GOOGLE_CLOUD_PROJECT` directly. Unset, and the receiver rejects whole batches.
- **`--metrics-addr` alone does nothing.** It only overrides the bind address; the exporter mode still has to select `prometheus` or `both`.
- **The scrape port must be free at boot.** A bind failure fails startup by design. In a `hostNetwork` Pod or alongside another `:9464` exporter, move it.
- **Push and pull disagree by design.** In `both` mode the OTLP reader exports on an interval while the scraper pulls on its own schedule, so the two see snapshots taken at different instants. Counters converge; gauges won't match point-for-point.
- **`USD` shows up in Prometheus metric names.** `core_agent_session_cost_usd_USD_total` is not a typo — the exporter appends the unit verbatim for units it doesn't have a canonical suffix for. Copy the name from a real scrape rather than typing it.

---

See also: [OpenTelemetry](/concepts/otel/) for traces, the span tree, and distributed propagation; the [`otel.metrics` configuration reference](/reference/configuration/#otelmetrics); and [`docs/metrics-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/metrics-design.md) for why the surface is shaped this way.
