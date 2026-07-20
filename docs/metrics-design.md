# Metrics: OTel MeterProvider (primary) + Prometheus scrape (secondary)

Design doc for closing the metrics gap. Untracked sibling to
[`shared-memory-design.md`](shared-memory-design.md),
[`scheduled-monitoring-design.md`](scheduled-monitoring-design.md),
[`peer-registration-design.md`](peer-registration-design.md),
[`attach-mode-design.md`](attach-mode-design.md). The traces side of
telemetry is documented in
[`docs/site/src/content/docs/concepts/otel.md`](site/src/content/docs/concepts/otel.md).
Written 2026-07-20 after auditing the codebase and confirming that no
OTel metrics flow out of any core-agent binary today.

## Context

We have a working OTel *traces* pipeline (`pkg/telemetry/otel.go`
delegates to ADK's `adktelemetry.New`, spans ship via OTLP; recent
fixes #333–#337 stabilized it). We have Prometheus counters in exactly
one place — the `cmd/k8s-event-watcher` sidecar
(`cmd/k8s-event-watcher/metrics.go`) — and only when `--metrics-addr`
is explicitly set.

We have **no OTel metrics anywhere**. Grep confirms zero references to
`otel/metric`, `MeterProvider`, `SetMeterProvider`, `WithMeterProvider`,
counters, or histograms across `pkg/`, `cmd/`, `internal/`. The three
`otelhttp.NewHandler` / `otelhttp.NewTransport` sites
(`pkg/attach/server.go`, `pkg/mcp/lifecycle.go`,
`cmd/k8s-event-watcher/injector.go`) *would* record HTTP metrics if a
MeterProvider were installed globally — but none is, so those
instruments fall into the noop meter and vanish.

**Google's ADK is not filling this gap.** `google.golang.org/adk` v1.2.0
(and still v1.5.0, and v2.0.0) contains a literal
`// TODO(#479) init meter provider` in `telemetry/setup_otel.go`. Its
`Providers` struct exposes only `TracerProvider` and `LoggerProvider`.
The [Google Cloud AI-agent-ADK observability
doc](https://docs.cloud.google.com/stackdriver/docs/instrumentation/ai-agent-adk)
is Python-only and explicitly states *"Metric data isn't collected"* —
even the Python path only ships traces + logs to Cloud
Trace / Cloud Logging, not to Cloud Monitoring.

Meanwhile we already **count and measure a lot internally** — the numbers
that would make the most useful metrics already live in memory. The gap
is exposure, not instrumentation.

### What we already track (free metrics if we expose them)

| Subsystem | Values | Source |
|---|---|---|
| Usage tracker | turns, input/cached/output/thoughts/tool-use tokens, cost USD, session duration; per-model breakdown | `pkg/usage/tracker.go` |
| Context window | used, size | `pkg/usage/context_window.go` |
| Digest wrap | `MethodCounts[method]`, `BytesSaved[method]` (structural_json / llm_fallback / passthrough) | `pkg/digest/telemetry.go` |
| Digest store | entry count, total bytes | `pkg/digest/store.go` |
| Agent context stats | compactions, checkpoints, subtask count / tokens / cost | `pkg/agent/context_stats.go`, `pkg/agent/agent.go` |
| Autonomous runs | turns, tokens, cost, duration, `StopReason` enum | `pkg/agent/autonomous.go` |
| Attach registry | active sessions, per-session idle age | `pkg/attach/registry.go` |
| Broadcaster | subscriber count, slow-subscriber drops | `pkg/attach/broadcaster.go` |
| Peer registry | active peer count | `pkg/attach/peers.go` |
| MCP lifecycle | per-server status (running/starting/failed/stopped) | `pkg/mcp/lifecycle.go` |
| Watchdog | repeated-tool-call run length, alert counts | `pkg/watchdog/watchdog.go` |

### Gaps (need call-site instrumentation, defer to v2)

- Per-tool invocation count / duration / error rate — `pkg/tools/` has no counters
- Per-turn latency and time-to-first-token — `usage.Turn.At` is only completion time
- MCP per-tool call duration histograms
- Model API retry / error counters (currently just log lines in `pkg/models/gemini/builtins.go:388,529`)
- Cache hit ratio as a first-class metric (we have cached-input tokens per turn, no separate hit/miss)

### Settled decisions (do not relitigate)

- **OTel MeterProvider is the primary surface. Prometheus scrape is a
  co-equal secondary reader on the same MeterProvider.** OTel wins on
  ecosystem alignment with our existing trace pipeline and on the
  future GenAI semconv (see below). But Prometheus is what
  most operators already scrape, and the k8s-event-watcher already
  ships a `/metrics` endpoint — refusing to expose one on the daemon
  would strand every existing Prometheus consumer. The trick that
  makes both cheap: OTel Go's Prometheus *reader* bridges a
  MeterProvider to a `promhttp` handler. One meter registration → both
  wire formats. No parallel instrument definitions.
- **New package `pkg/telemetry/metrics.go`** alongside `otel.go`. Same
  `Setup` boot pattern (returns a shutdown closure). Not a
  merge-into-otel.go: the OTel *traces* code delegates to ADK, but
  ADK doesn't know how to build a MeterProvider; metrics init runs
  independently. Keeping the files parallel keeps the "off by default,
  env-var override, standard OTel env vars honored" pattern legible.
- **Off by default.** Fresh invocations still make zero outbound network
  calls. Consumers opt in via `otel.metrics_exporter` = `otlp` /
  `prometheus` / `both` / `none`, mirroring the existing
  `otel.exporter` (traces) shape. `OTEL_METRICS_EXPORTER` env var
  overrides the config value, following the same convention as
  `OTEL_TRACES_EXPORTER` (#315).
- **v1 uses async observable instruments only, driven by the existing
  in-process counters.** No call-site changes. A single `RegisterCallback`
  per subsystem observes the counter fields listed in the table above.
  Zero risk of double-counting or drift because the tracker is still
  the source of truth. v2 adds sync instruments at call sites (per-tool
  histograms, retry counters) once we have real consumers asking.
- **GenAI semantic conventions where they exist.** The OTel GenAI SIG
  has stabilized names for token usage (`gen_ai.client.token.usage`) and
  op duration (`gen_ai.client.operation.duration`). We adopt those for
  the token/cost metrics so cloud vendor dashboards (GCM, Datadog,
  Honeycomb) that already render GenAI panels light up for free.
  Custom metrics use the `core_agent.*` namespace to keep them
  distinguishable.
- **Cost is a metric, not just a log line.** Per-turn cost in USD gets
  its own instrument (`core_agent.session.cost_usd`) with `model` as an
  attribute. Cost dashboards are the single most-requested "why is our
  bill spiking" surface and everything else in this doc is downstream
  of solving that.
- **Per-agent identity via `core_agent.peer.id` when peer-registration
  is active.** OTel resource attrs (`service.name`,
  `service.instance.id`, `service.namespace`) cover per-process
  pivoting for free, but Pod restarts churn `service.instance.id`
  and lose the logical identity operators want ("this triage agent"
  vs "that autoscaled replica of it"). When the daemon has
  registered against a peer-registration hub, its
  `PeerRegistry.RegistrationID` is stamped as a resource attribute
  so per-agent aggregation survives Pod churn. Absent a hub, the
  attribute is not set and per-agent queries fall back to
  `service.instance.id`.
- **Prometheus scrape endpoint served from the attach listener when
  possible.** Adds a `GET /metrics` handler to `pkg/attach/server.go`.
  Attach listeners already have TLS + auth wiring; we don't want two
  separately-secured HTTP surfaces per daemon. When attach is disabled,
  the metrics endpoint uses a standalone listener at
  `--metrics-addr` (matches k8s-event-watcher's shape).
- **`k8s-event-watcher` migrates its six Prometheus counters onto the
  same MeterProvider substrate.** The sidecar becomes a consumer of
  `pkg/telemetry/metrics` instead of running a parallel Prometheus
  registry. Same `/metrics` output shape (backward compatible), no
  operator-visible break. Removes a whole class of "which telemetry
  library is this binary using" confusion.
- **No metrics-side pipeline for logs.** ADK opportunistically wires a
  `LoggerProvider` if `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` is set; we
  leave that alone. Metrics and logs are separate signals and we're not
  in the log-shipping business.

## Package shape

```go
// package telemetry (extends the existing package)

// MetricsMode names the metrics exporter surface. Empty and ModeNone
// disable metrics entirely.
const (
    MetricsModeNone       = "none"       // default; no metrics exported
    MetricsModeOTLP       = "otlp"       // push via OTLP; honors OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
    MetricsModePrometheus = "prometheus" // pull; served at Config.Prometheus.Endpoint
    MetricsModeBoth       = "both"       // OTLP push + Prometheus pull, one MeterProvider
)

// MetricsExporterEnvVar overrides the config-file mode. Matches the
// shape of TracesExporterEnvVar (#315).
const MetricsExporterEnvVar = "OTEL_METRICS_EXPORTER"

// SetupMetrics installs a global MeterProvider. Returns a shutdown
// function the caller MUST call (typically deferred). Safe to call
// after SetupTraces; the two pipelines share nothing at runtime
// beyond the OTel SDK's global registry.
//
// When mode is "" or "none", no provider is constructed and the
// shutdown returns nil.
func SetupMetrics(ctx context.Context, mode string, opts MetricsOptions) (shutdown func(context.Context) error, err error)

// MetricsOptions carries the Prometheus endpoint address and any
// observers to register. Observers are the sole v1 mechanism for
// exposing existing in-process counters — see "Observer registration"
// below.
type MetricsOptions struct {
    PrometheusAddr string       // e.g. ":9464" or "127.0.0.1:9464"; ignored unless mode is prometheus/both
    Observers      []Observer   // registered in order after MeterProvider is up
    ResourceAttrs  []attribute.KeyValue // extra resource attrs merged with defaults
}

// Observer is a subsystem's contribution to the metrics surface. It
// creates its own instruments against the passed Meter and registers
// a callback that reads live values from the subsystem. Called once
// during SetupMetrics.
type Observer interface {
    RegisterMetrics(meter metric.Meter) error
}
```

### Config

```go
// OTELConfig gains a Metrics block. Existing fields unchanged.
type OTELConfig struct {
    Exporter string        `json:"exporter,omitempty"` // traces (existing)
    Endpoint string        `json:"endpoint,omitempty"` // traces (existing)
    Metrics  MetricsConfig `json:"metrics,omitempty"`
}

type MetricsConfig struct {
    Exporter       string `json:"exporter,omitempty"`        // "none" | "otlp" | "prometheus" | "both"
    PrometheusAddr string `json:"prometheus_addr,omitempty"` // scrape endpoint bind address
}
```

CLI flag: `--metrics-addr` on `core-agent` and `core-agent-tui` mirrors
the existing k8s-event-watcher flag. When set, implies
`OTEL_METRICS_EXPORTER=prometheus` unless already overridden.

## v1 instrument catalog

Async / observable. Attribute keys follow OTel GenAI semconv where they
exist; everything else uses `core_agent.*`.

### GenAI semconv (mapped verbatim from `usage.Tracker`)

| Metric | Type | Unit | Attributes | Source |
|---|---|---|---|---|
| `gen_ai.client.token.usage` | ObservableCounter | `{token}` | `gen_ai.token.type` = input\|output\|cached\|thoughts\|tool_use, `gen_ai.request.model` | `Tracker.TotalsByModel()` |
| `gen_ai.client.operation.duration` | ObservableGauge | `s` | `gen_ai.request.model` | derived from `Tracker.Duration()` per session (v1: session-level; v2: per-turn as sync histogram) |

### core-agent-specific — session + cost

| Metric | Type | Unit | Attributes | Source |
|---|---|---|---|---|
| `core_agent.session.turns` | ObservableCounter | `{turn}` | `session.id`, `gen_ai.request.model` | `Tracker.Totals().Turns` |
| `core_agent.session.cost_usd` | ObservableCounter | `USD` | `session.id`, `gen_ai.request.model` | `Tracker.TotalsByModel()` |
| `core_agent.session.duration` | ObservableGauge | `s` | `session.id` | `Tracker.Duration()` |
| `core_agent.context.window_used` | ObservableGauge | `{token}` | `session.id`, `gen_ai.request.model` | `usage.ContextWindowUsed()` |
| `core_agent.context.window_size` | ObservableGauge | `{token}` | `session.id`, `gen_ai.request.model` | `usage.ContextWindowSize()` |

### core-agent-specific — digest / MCP wrap

| Metric | Type | Unit | Attributes | Source |
|---|---|---|---|---|
| `core_agent.digest.calls` | ObservableCounter | `{call}` | `digest.method` = structural_json\|llm_fallback\|passthrough | `digest.telemetry.MethodCounts` |
| `core_agent.digest.bytes_saved` | ObservableCounter | `By` | `digest.method` | `digest.telemetry.BytesSaved` |
| `core_agent.digest.store.entries` | ObservableGauge | `{entry}` | none | `FilesystemStore.Len()` |
| `core_agent.digest.store.bytes` | ObservableGauge | `By` | none | `FilesystemStore.Bytes()` |
| `core_agent.digest.subagent.cost_usd` | ObservableCounter | `USD` | none | `Tracker.DigestSavings().AgenticSubagentCostUSD` |

### core-agent-specific — agent / autonomous

| Metric | Type | Unit | Attributes | Source |
|---|---|---|---|---|
| `core_agent.agent.compactions` | ObservableCounter | `{compaction}` | `session.id` | `ContextStats.CompactionCount` |
| `core_agent.agent.checkpoints` | ObservableCounter | `{checkpoint}` | `session.id` | `ContextStats.CheckpointCount` |
| `core_agent.agent.subtasks` | ObservableCounter | `{subtask}` | `session.id` | `ContextStats.SubtaskCount` |
| `core_agent.autonomous.runs` | ObservableCounter | `{run}` | `stop_reason` (completed\|max_turns\|max_tokens\|max_cost\|wallclock\|context_cancelled\|retry_aborted\|deferred) | incremented on `RunResult.StopReason` at run end (thin sync counter; the only v1 exception to the async-only rule because `RunResult` doesn't accumulate) |
| `core_agent.agent.inbox_pending` | ObservableGauge | `{message}` | `session.id` | `Agent.PendingInboxCount()` |

### core-agent-specific — attach / peers / MCP

| Metric | Type | Unit | Attributes | Source |
|---|---|---|---|---|
| `core_agent.attach.sessions.active` | ObservableGauge | `{session}` | none | `SessionRegistry.Len()` |
| `core_agent.attach.subscribers` | ObservableGauge | `{subscriber}` | `session.id` | broadcaster subscriber count |
| `core_agent.attach.subscriber_drops` | ObservableCounter | `{drop}` | `session.id`, `reason` = slow\|full | broadcaster drop sites |
| `core_agent.attach.peers.active` | ObservableGauge | `{peer}` | none | `PeerRegistry.Len()` |
| `core_agent.mcp.server.status` | ObservableGauge | `{server}` (0/1 per label combo) | `mcp.server`, `mcp.status` = running\|starting\|failed\|stopped | `Server.Status` |
| `core_agent.watchdog.alerts` | ObservableCounter | `{alert}` | `session.id`, `signal` (e.g. repeated_tool_call) | `Watchdog.alerts` |

### k8s-event-watcher (migrated from Prometheus)

Existing metric names preserved exactly for scrape backward
compatibility — Prometheus consumers scraping the sidecar today keep
working. Under the hood these become OTel counters emitted through the
same MeterProvider.

| Metric | Type | Preserved from |
|---|---|---|
| `k8s_event_watcher_events_seen_total` | ObservableCounter | `cmd/k8s-event-watcher/metrics.go:47` |
| `k8s_event_watcher_events_injected_total` | ObservableCounter | same |
| `k8s_event_watcher_events_deduped_total` | ObservableCounter | same |
| `k8s_event_watcher_inject_errors_total` | ObservableCounter | same |
| `k8s_event_watcher_session_creates_total` | ObservableCounter | same |
| `k8s_event_watcher_active_incidents` | ObservableGauge | same |

## How to look at metrics

Four tiers of granularity. Each is answered by a different mechanism —
metrics don't cover all of them, and trying to force per-turn detail
into the metrics pipeline is the wrong shape.

| You want to see… | Use | Cost |
|---|---|---|
| Fleet totals, rates, error budgets | Metrics, no attribute filter | Free day 1 |
| "Which agent / process is misbehaving" | Metrics filtered by `service.instance.id` or `core_agent.peer.id` | Free day 1 |
| "Which session hit its cost ceiling" | Metrics filtered by `session.id` (small fleets) OR trace search by `session.id` (large fleets) | Config flag (`otel.metrics.session_labels`) |
| Individual turn tokens, latency, tool tree | Traces (`session.id` attribute on spans) | Already works today |
| "Alert fired — show me the offending turn" | Exemplars linking metric → trace | Depends on open question #4 |

### Aggregate (fleet-wide)

Free. Every metric supports it — strip attribute filters, sum the
series. `sum(core_agent_session_cost_usd)` gives total spend; `rate(gen_ai_client_token_usage_total[5m])`
gives fleet token/sec. Works from Prometheus PromQL directly, from
OTLP via the operator's collector / Grafana / GCM / Datadog / Honeycomb
query language.

### Per agent (per process / per peer)

Free via OTel resource attributes plus one custom attribute:

- `service.name` — separates binaries (`core-agent` vs
  `k8s-event-watcher` vs `core-agent-tui`).
- `service.instance.id` — one series per process (uuid at boot, or
  `HOSTNAME` in K8s).
- `service.namespace` — deployment cohort (per-cluster, per-tenant).
- `core_agent.peer.id` = `PeerRegistry.RegistrationID` — logical
  identity that survives Pod restarts. Emitted only when the daemon
  registered against a hub.

Typical query: `sum by (peer_id) (core_agent_session_cost_usd)` →
cost bar chart per agent. This is the tier operators actually want
for multi-agent fleets ("which agent in my triage pool is burning
budget"). No per-session drill-down needed for this question.

### Per session

Supported via a `session.id` attribute on every session-scoped metric,
**gated behind a config flag** (`otel.metrics.session_labels`, default
off — see open question #2). Reasoning:

- **Cardinality risk is real.** Long-running daemons with attach-mode
  and k8s-event-watcher inject create sessions on the order of
  hundreds/hour. Emitting `session.id` on every counter grows the
  Prometheus TSDB unboundedly and inflates OTLP push payloads.
- **When off:** metrics aggregate across sessions per-process. Totals
  and rates still work; you lose "which session was expensive."
  Adequate for most fleets. Per-session investigation shifts to
  traces (which are naturally session-scoped).
- **When on:** full per-session drill-down. Small fleets flip it on.
- **Exception — inherently per-session gauges** (`core_agent.context.window_used`,
  `core_agent.agent.inbox_pending`) emit `session.id` regardless.
  A single "active session's context fill" gauge without the label
  is meaningless.

### Per turn — wrong tool: use traces

Metrics are aggregates by design; a single turn is one data point.
The turn-shaped questions belong in the tracing surface, which we
already have:

- **"How many turns / how many tokens?"** → cumulative metric
  counters (`core_agent.session.turns`, `gen_ai.client.token.usage`),
  differenced across scrape intervals. This is an aggregate rate, not
  an individual turn.
- **"Show me this turn's timing, model calls, tool tree."** → the
  existing `adk.invoke_agent` → `adk.call_llm` → `mcp.tool_call`
  span tree. Filter by `session.id` span attribute to see every turn
  in one session as a waterfall.
- **"An alert just fired — take me to the exact turn that caused it."**
  → OTel **exemplars** (open question #4). An exemplar attaches a
  sampled trace ID to a metric data point so click-through works:
  metric spike → exemplar → span → root cause. This is the one
  bridge worth reconsidering before PR #A ships; it's cheap and it's
  exactly the aggregate→instance workflow operators reach for. If
  we skip it in v1, per-turn attribution requires manual
  session-id-to-trace correlation.

### Backends that render these tiers well out of the box

- **Prometheus + Grafana** — PromQL covers aggregate / per-agent /
  per-session natively; no special support needed.
- **GCM (Cloud Monitoring)** — GenAI semconv panels render token /
  cost metrics automatically. `service.*` resource attrs pivot per
  agent.
- **Datadog / Honeycomb / Tempo + Prometheus** — same shape; native
  exemplar support in Grafana + Tempo makes the metric→trace
  click-through work with zero config once exemplars are on.

## Observer registration

Each subsystem contributes an observer. The composition happens at boot
in `cmd/core-agent/main.go`, alongside the existing `telemetry.Setup`
call.

```go
// In cmd/core-agent/main.go, after tracker/registry/etc. are built.
metricsShutdown, err := telemetry.SetupMetrics(ctx, cfg.OTEL.Metrics.Exporter, telemetry.MetricsOptions{
    PrometheusAddr: cfg.OTEL.Metrics.PrometheusAddr,
    Observers: []telemetry.Observer{
        usage.NewMetricsObserver(trackerRegistry),   // pkg/usage exposes per-session tracker snapshots
        digest.NewMetricsObserver(digestTelemetry),
        agent.NewMetricsObserver(agentRegistry),
        attach.NewMetricsObserver(sessionRegistry, peerRegistry, broadcaster),
        mcp.NewMetricsObserver(mcpLifecycle),
        watchdog.NewMetricsObserver(watchdog),
    },
})
if err != nil { return fmt.Errorf("metrics init: %w", err) }
defer metricsShutdown(ctx)
```

Each `NewMetricsObserver` returns an `Observer` value carrying the
minimum interface it needs from the subsystem — no package-level
globals, no init-order dependencies. Observers live in the same
package as the subsystem they observe (matches how tests import them).

The alternative — a single `metrics.go` file that reaches into every
package via getter functions — was considered and rejected. Every
subsystem's counters are already private state protected by a mutex or
`atomic`; exposing them through a getter for a metrics package would
force us to widen those APIs. Local observers keep the widening local.

## Composition with existing primitives

| Primitive | Interaction |
|---|---|
| **`pkg/telemetry/otel.go`** (traces) | Independent init; shares the OTel SDK's global registry but no runtime code. Both installed at daemon startup in `cmd/core-agent/main.go`. |
| **`otelhttp` wrappers** (`pkg/attach/server.go`, `pkg/mcp/lifecycle.go`, `cmd/k8s-event-watcher/injector.go`) | Start emitting HTTP metrics *automatically* the moment the MeterProvider is installed — zero code change at the wrapper sites. This is a free-side-effect win. |
| **`attach-mode`** | Serves `GET /metrics` when the attach listener is enabled and Prometheus mode is on. When attach is off, a standalone listener at `--metrics-addr` mirrors the k8s-event-watcher shape. |
| **`peer-registration`** | Peers can report their own metrics endpoint (or their own OTLP target) — no coordination needed; each peer independently opts in. A future hub-side aggregation MCP tool that fans queries across peer `/metrics` endpoints is thinkable but out of v1 scope. |
| **`permissions.Gate`** | The `/metrics` endpoint is read-only, gate-free (matches Prometheus norms). Callers wanting auth on the endpoint use attach-mode's existing auth + TLS wiring. |
| **`Scheduler`** | Untouched. Async observers run on the MeterProvider's export interval; no scheduler entries. |

## Implementation sketch

About **900–1200 LoC + tests** total, split across three PRs.

### PR #A — `pkg/telemetry/metrics.go` + core observers

- `pkg/telemetry/metrics.go` — `SetupMetrics`, `MetricsOptions`,
  `Observer` interface, mode switch, Prometheus reader wiring, OTLP
  metrics exporter wiring (~200 LoC).
- `pkg/telemetry/metrics_test.go` — mode matrix, env-var override,
  no-op path, both-mode reader composition (~150 LoC).
- `pkg/usage/metrics.go` — `NewMetricsObserver` reading from a
  registry of session trackers; emits GenAI semconv + `core_agent.session.*`
  (~120 LoC).
- `pkg/digest/metrics.go` — observer over `MethodCounts`, `BytesSaved`,
  `FilesystemStore.Len/Bytes` (~80 LoC).
- `pkg/attach/metrics.go` — observer over `SessionRegistry`,
  `PeerRegistry`, broadcaster; plus the `/metrics` HTTP handler on the
  attach listener (~120 LoC).
- `pkg/mcp/metrics.go` — observer over `Server.Status` (~50 LoC).
- `pkg/agent/metrics.go` — observer over `ContextStats`; sync
  `core_agent.autonomous.runs` counter incremented in `autonomous.go`
  at run completion (~100 LoC).
- `pkg/watchdog/metrics.go` — observer over alert accumulator (~40 LoC).
- Package tests for each observer verifying instrument names, units,
  and one-shot value observation (~200 LoC).
- `pkg/config/config.go` — `MetricsConfig` block (~15 LoC).
- `cmd/core-agent/main.go` — wire `SetupMetrics` alongside
  `telemetry.Setup`; add `--metrics-addr` flag (~40 LoC).
- CHANGELOG entry under `[Unreleased]`.

### PR #B — `docs/site/` + operator recipes

- `docs/site/src/content/docs/concepts/metrics.md` — new page mirroring
  the existing `otel.md` shape: what's exposed, how to enable, sample
  queries (Prom + PromQL, OTLP + generic).
- Cross-links from `otel.md` to `metrics.md` and vice versa.
- Update `docs/site/src/content/docs/reference/configuration.md` with
  the new `otel.metrics.*` fields.
- `examples/gke-troubleshoot-agent/deploy/components/otel/` — add a
  Prometheus scrape annotation + metrics endpoint to the pod spec so
  GKE's managed Prometheus finds it (or the OTLP metrics
  export goes through the managed collector — one env var flip).
- Sample Grafana dashboard JSON in `dev/grafana/` covering the
  cost, token, digest-savings, and MCP-status panels.

### PR #C — `k8s-event-watcher` migration + first sync instruments

- `cmd/k8s-event-watcher/metrics.go` — replace `prometheus.NewRegistry`
  with a `telemetry.Observer` over the same six counters. Same
  scrape output; the `--metrics-addr` flag stays. Delete the
  hand-rolled `/metrics` handler in favor of the shared one (~-80 net LoC).
- `pkg/mcp/lifecycle.go` — sync histogram
  `core_agent.mcp.tool_call.duration` around each tool invocation
  (first taste of call-site instrumentation; small and self-contained)
  (~40 LoC).
- CHANGELOG entry under `[Unreleased]`: note the k8s-event-watcher
  metrics substrate change is backward compatible; scrape output
  unchanged.

## Open questions

1. **Prometheus endpoint auth.** The k8s-event-watcher endpoint is
   currently unauthenticated (Prometheus convention; assumes the
   endpoint is only exposed on a cluster-internal address). When the
   attach listener is TLS-authenticated, do we require the same for
   `/metrics`? Lean: **no, follow Prometheus norms** — operators
   wanting auth reverse-proxy in front. Cheap to revisit.
2. **Per-session cardinality.** Emitting `session.id` as an attribute
   on every per-session metric explodes cardinality on long-running
   daemons handling thousands of sessions. Lean: **make it configurable**
   — a `otel.metrics.session_labels` bool (default `false`) that,
   when off, aggregates across sessions. Consumers running <100
   sessions/day flip it on for the drill-down; big-fleet operators
   leave it off. Worth deciding before PR #A freezes attribute keys.
3. **GenAI semconv version pin.** The GenAI semconv is still moving.
   Do we pin to the version stable at PR #A time, or track upstream
   and accept breakage? Lean: **pin, and bump in a dedicated PR** with
   a CHANGELOG entry each time.
4. **Exemplars linking metrics → traces.** OTel supports attaching a
   sampled trace ID to a metric data point so an alert on a metric
   spike click-throughs to the exact trace that contributed to it.
   Originally deferred to v2, but the "How to look at metrics"
   section's per-turn story leans on this — without exemplars, going
   from "cost alert fired" to "which turn caused it" requires manual
   `session.id` correlation between the metric label and the trace
   attribute. The cost is small (OTel Go's SDK provides an
   `AlwaysOn` exemplar filter out of the box; Grafana + Tempo
   render them natively). Lean: **include in PR #A** if it doesn't
   balloon the PR; otherwise ship as an immediate PR #A.1.
5. **Cost as a metric vs. a log line.** We emit per-turn cost through
   the event log today. A metric duplicates the signal. Lean: **both**
   — metrics for dashboards / alerts, event log for audit. The doc's
   settled section commits to this; flagged here in case an operator
   asks why they see cost twice.
6. **Prometheus vs. `otel.exporter.prometheus` naming.** OTel-standard
   env vars for Prometheus don't exist (Prometheus is pull, OTel env
   vars are for push exporters). We invent `OTEL_METRICS_EXPORTER=prometheus`
   as a core-agent extension. Any risk of clashing with a future OTel
   spec extension? Lean: **low** — the spec explicitly enumerates
   push exporters; `prometheus` as a mode value is our shorthand for
   "install a Prometheus reader," not a standard exporter name.
7. **Cost of always-on observer callbacks.** Async callbacks fire on
   every export interval (default 60s). Reading `TotalsByModel()`
   takes a lock on every session tracker. At >1000 concurrent
   sessions that's a real cost. Lean: **fine for v1** (measure at
   the first operator hitting the ceiling); consider a cached
   snapshot fed by the eventlog for v2.
8. **Should `SetupMetrics` and `Setup` (traces) merge?** They share
   env-var override style and shutdown shape. Merging into
   `telemetry.Setup(ctx, cfg.OTEL)` returning one shutdown reads
   nicer. Argument against: traces delegate to ADK; metrics don't.
   Merging means the metrics init path has to route around ADK,
   which means the file grows the "ADK doesn't know about metrics"
   caveat inside a function whose name says "OTel." Lean: **keep
   separate for v1**; revisit if / when ADK ships a MeterProvider
   (#479 upstream).

## Out of scope (v1)

- **Log export via OTel `LoggerProvider`.** ADK already opportunistically
  wires this if the OTLP logs endpoint env var is set. Not our
  substrate to design.
- **Sync per-tool histograms across `pkg/tools/*`.** Would require
  wrapping every tool's `Run` with a duration histogram + error
  counter. Belongs in v2, once we have consumers whose dashboards
  need it. PR #C ships one taste (`mcp.tool_call.duration`) so the
  pattern is in-tree.
- **Per-turn latency + time-to-first-token.** Same reason — needs
  call-site work in the model adapters. v2.
- **Aggregation-across-peers hub metric.** A hub that scrapes every
  peer's `/metrics` and republishes a fleet-scoped view is possible
  but adds coordination we don't need yet. Each peer exports
  independently; operators aggregate at their scrape / OTLP
  collector.
- **Cardinality-managed relabeling / recording rules.** Consumer
  responsibility. We ship raw metrics with reasonable attribute sets;
  Prometheus operators use `relabel_config`, OTLP operators use
  Collector processors.
- **Statsd, DogStatsD, or vendor-specific push protocols.** OTLP
  covers the modern set. Vendors that only speak statsd run an
  OTel Collector in front.

## Why this puts us in a useful position

The headline claim, once shipped:

> **core-agent is a Go agent runtime that emits OTel metrics + traces
> today** — not "planned" or "TODO(#479)" or "Python-only" the way
> ADK-Go and the Google Cloud AI-agent-ADK guide currently are.

Concretely, the unique combination:

1. **Both wire formats from one meter definition.** Prometheus
   operators scrape `/metrics`. OTLP operators point at their
   collector. Neither side compromises; the code path stays single.
2. **GenAI semconv adoption.** Cloud vendor dashboards that already
   render GenAI panels (GCM, Datadog) work against core-agent with
   zero custom-metric plumbing.
3. **Cost is first-class.** Per-model, per-session, streaming to
   whatever backend the operator already runs. The "why is our LLM
   bill spiking" question gets a dashboard, not a grep.
4. **Free k8s-event-watcher migration** — existing Prometheus
   scrapes keep working; the sidecar and daemon speak the same
   substrate. One less "which telemetry library" answer to give.
5. **Traces + metrics on the same OTel resource.** `service.name`,
   `gcp.project_id`, `deployment.environment` — one set of resource
   attrs, one story for correlating a latency spike (metric) to the
   trace that caused it.

The audit-derived-memory doc's headline was uniqueness *versus other
memory systems*. This doc's headline is much simpler: **we ship
metrics.** In this category, that alone is currently a distinguishing
property.
