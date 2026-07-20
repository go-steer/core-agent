---
title: OpenTelemetry
---


`core-agent` emits [OpenTelemetry](https://opentelemetry.io) traces via ADK's built-in instrumentation plus a small set of custom spans around MCP tool calls and the [structural pruner](/concepts/mcp/#agentic-wrap). Traces let you attribute cost, latency, and errors across model calls, tool invocations, and pruning passes without adding a logging middleware.

Configuration lives in `.agents/config.json` under the `otel:` key, with standard OpenTelemetry SDK env vars available as per-process overrides. The daemon speaks OTLP over HTTP or gRPC — point it at any OTLP-compatible collector (self-hosted OpenTelemetry Collector, [GKE Managed OpenTelemetry](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/managed-otel-gke), Jaeger, Honeycomb, etc.).

---

## Enabling

### Config file

```json
{
  "otel": {
    "exporter": "otlp"
  }
}
```

Values for `exporter`:

| Value | Behavior |
|---|---|
| `none` | Default. No exporter registered; spans are recorded but dropped. Zero overhead in hot paths. |
| `console` | Prints span JSON to stderr. Local development only — noisy. |
| `otlp` | OTLP exporter. Reads `OTEL_EXPORTER_OTLP_ENDPOINT` and related env vars for target + auth. |

### Env-var override

The `OTEL_TRACES_EXPORTER` env var overrides `otel.exporter` from the config file (added in v2.7.0-dev.4). This is the load-bearing knob for multi-daemon Kubernetes deployments where a shared ConfigMap can't carry per-Pod exporter targets:

```bash
export OTEL_TRACES_EXPORTER=otlp
export OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.observability.svc:4318
export OTEL_SERVICE_NAME=core-agent
export OTEL_RESOURCE_ATTRIBUTES="deployment.environment=prod,team=sre"
```

All standard OpenTelemetry SDK env vars work — sampling (`OTEL_TRACES_SAMPLER`), headers (`OTEL_EXPORTER_OTLP_HEADERS`), protocol (`OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf` or `grpc`), etc.

---

## Span tree

A typical tool call from a session produces this hierarchy:

```
adk.invoke_agent                        (root, from ADK)
├── adk.call_llm                        (planner LLM call)
└── mcp.tool_call                       {tool.name, tool.server, tool.call_id}
    ├── mcp.http_call                   (otelhttp on the MCP transport, HTTP servers only)
    └── digest.process                  {digest.strategy, digest.input_bytes, digest.output_bytes}
          └── subagent.llm_call         (agentic strategy only)
                {model, input_tokens, output_tokens, savings.tokens_dropped}
```

Key attributes:

| Attribute | Where | Meaning |
|---|---|---|
| `tool.name` | `mcp.tool_call` | Fully-qualified tool name, e.g. `gke.list_clusters`. |
| `tool.server` | `mcp.tool_call` | The MCP server namespace. |
| `digest.strategy` | `digest.process` | `structural` \| `agentic` \| `passthrough`. |
| `digest.input_bytes` | `digest.process` | Response size before pruning. |
| `digest.output_bytes` | `digest.process` | Response size after pruning. |
| `savings.tokens_dropped` | `subagent.llm_call` | Tokens the LLM summarizer dropped from the raw response — the "savings" number shown in `/stats`. |
| `model` | `subagent.llm_call` | The sub-agent model used (usually cheaper than the planner). |

---

## Common queries

**Attribute cost to a specific MCP server.** Group `subagent.llm_call` by parent `mcp.tool_call.tool.server` and sum `input_tokens + output_tokens`. Answers "which MCP server is driving the LLM bill this week?"

**Find pruning regressions.** Filter `digest.process` where `digest.output_bytes > digest.input_bytes * 0.5` and `digest.strategy = "structural"` — pruner is failing to compress. Common cause: JSON-in-string that the pruner can't see through (see [PR #302](https://github.com/go-steer/core-agent/pull/302) for the fix history).

**Track tool-call tail latency.** Percentile query on `mcp.tool_call` duration, grouped by `tool.name`. The MCP layer is often the biggest driver of session wall-clock time.

**Confirm agentic wrap is active.** Presence of `subagent.llm_call` under `mcp.tool_call` proves the agentic path fired. If it's missing, the daemon is running the structural pruner instead — check `--mcp-agentic-wrap-llm` on the daemon args.

---

## Distributed tracing across binaries

When several core-agent binaries run alongside each other — daemon + `k8s-event-watcher` sidecar + `core-agent-tui` client, or daemon + peer daemons — a single incident produces spans that live in different processes. Stitching them into one trace requires two things: the [W3C Trace Context](https://www.w3.org/TR/trace-context/) `traceparent` header propagating across HTTP hops, and the HTTP clients / servers on each hop being instrumented to extract + re-inject it.

`core-agent` uses OpenTelemetry's standard TextMapPropagator and [`otelhttp`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp) middleware to make this transparent:

- **Propagator registered globally** at daemon startup — supports both `traceparent` and `tracestate` (`pkg/telemetry/otel.go`). Every span the daemon emits carries the current trace's IDs.
- **Attach server** wraps the router in `otelhttp.NewHandler` (`pkg/attach/server.go`) — every inbound HTTP request extracts `traceparent` if present, becomes a root or child span, and the trace context flows into every downstream operation the request touches.
- **MCP client** wraps the outbound transport in `otelhttp.NewTransport` (`pkg/mcp/lifecycle.go`) — the `mcp.http_call` span you see in the span tree above rides on that transport, and MCP servers that speak OTel see the parent trace.
- **`k8s-event-watcher`** initializes the same OTel SDK at startup (`cmd/k8s-event-watcher/main.go`) and wraps its outbound HTTP client (`injector.go`) so a `POST /sessions/{sid}/inject` from the sidecar starts a trace on the watcher, propagates via `traceparent`, and the daemon's `otelhttp.Handler` extracts it into the request context. The inject → session-turn → tool-call → MCP-call chain becomes one trace across two processes.

### End-to-end span tree

A full triage inject on GKE with the OTel overlay applied produces roughly:

```
POST /sessions                              (root — watcher creating session, otelhttp client span)
POST /sessions/{sid}/inject                 (root — watcher's inject call)
invoke_agent core_agent                     (root — daemon's ADK-emitted turn span)
├── HTTP POST                               (otelhttp on outbound calls)
├── generate_content <model>                (ADK-emitted LLM call, e.g. "generate_content gemini-3.5-flash")
│   └── HTTP POST                           (otelhttp on genai transport → Vertex / Gemini)
├── execute_tool <tool_name>                (per tool call, e.g. "execute_tool gke_get_k8s_resource")
├── mcp.tool_call                           (our custom span wrapping the MCP round-trip)
│   ├── mcp.http_call                       (otelhttp on MCP HTTP transport)
│   └── digest.process                      (response digest / prune)
│         └── subagent.llm_call             (agentic wrap only — --mcp-agentic-wrap-llm=true)
└── tools/call <tool_name>                  (MCP server's own instrumentation; may appear as separate root)
```

Attach-server spans use the `METHOD PATH` naming convention (from `otelhttp.WithSpanNameFormatter` in `pkg/attach/server.go`), so what shows up in your backend is literally `POST /sessions/019f8075.../inject` — not a semantic-name like `attach.inject`. Filter / query by path prefix if you want to isolate all inject events, or by the `http.route` attribute if your backend surfaces it.

### Verifying it works

In Cloud Trace, filter by `service.name = "core-agent"` and open one trace. You should see:

- Root spans from the watcher (`POST /sessions/...`) and daemon (`invoke_agent core_agent`, `POST /sessions/{sid}/inject`).
- Cross-binary stitching: `traceparent` propagation from watcher → daemon should make the watcher's `POST /sessions/{sid}/inject` a child of the corresponding daemon-side span (or vice versa depending on which side started the trace).
- `HTTP POST` spans under `generate_content` show the outbound Vertex calls are instrumented.

If daemon spans appear on separate traces from the watcher's, `traceparent` isn't propagating — likely causes:

- Watcher didn't have `OTEL_TRACES_EXPORTER=otlp` set (spans get recorded but dropped).
- A reverse proxy or load balancer between the two is stripping `traceparent` (rare — most cloud LBs pass it through).
- The daemon's attach listener isn't going through `otelhttp.NewHandler` (only happens if `attach.listen` is disabled — the wrap is unconditional otherwise).

---

## Deploying on Kubernetes

The GKE troubleshooting recipe ships a reusable kustomize component and a canonical overlay that wire OTel export in two composable pieces:

- **[`components/otel`](https://github.com/go-steer/core-agent/tree/main/examples/gke-troubleshoot-agent/deploy/components/otel)** — one-env-var component that flips the daemon's exporter from `none` to `otlp` via `OTEL_TRACES_EXPORTER`. Environment-agnostic; the same component works on and off GKE.
- **Endpoint + service + resource attrs** — supplied by the runtime environment via standard OTel SDK env vars. Where those come from depends on where you're deploying.

### On GKE (Managed OpenTelemetry)

The [`example-otel`](https://github.com/go-steer/core-agent/tree/main/examples/gke-troubleshoot-agent/deploy/overlays/example-otel) overlay composes the component + an `Instrumentation` CR. [GKE Managed OpenTelemetry](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/managed-otel-gke) auto-injects a subset of standard OTel SDK env vars into every Pod matched by the CR's selector — specifically `OTEL_EXPORTER_OTLP_ENDPOINT` (in-cluster managed collector), `OTEL_TRACES_EXPORTER=otlp` / `OTEL_METRICS_EXPORTER` / `OTEL_LOGS_EXPORTER`, `OTEL_TRACES_SAMPLER` + `_ARG`, `OTEL_METRIC_EXPORT_INTERVAL`, and `K8S_POD_UID` + `OTEL_RESOURCE_ATTRIBUTES` with `k8s.pod.uid` (the collector's k8s-attributes processor then attaches `k8s.namespace.name` etc.). Notably **`OTEL_SERVICE_NAME` is NOT auto-injected** — set it yourself in the daemon Pod's env (the recipe's component patch does). Spans land in Cloud Trace with no self-managed collector to run.

Cluster prereqs (one-time):

```bash
gcloud services enable cloudtrace.googleapis.com telemetry.googleapis.com
gcloud container clusters update <CLUSTER> --location=<REGION> \
  --managed-otel-scope=COLLECTION_AND_INSTRUMENTATION_COMPONENTS
gcloud projects add-iam-policy-binding <PROJECT> \
  --member="serviceAccount:<POD-SA>" \
  --role="roles/cloudtrace.user"
```

Then:

```bash
kubectl apply -k examples/gke-troubleshoot-agent/deploy/overlays/example-otel/
```

### Anywhere else (self-managed Collector, Docker, systemd, ...)

Same component; supply the endpoint yourself. In kustomize:

```yaml
resources:
  - ../../base
components:
  - ../../components/otel
patches:
  - target: {kind: Deployment, name: core-agent}
    patch: |-
      - op: add
        path: /spec/template/spec/containers/0/env/-
        value: {name: OTEL_EXPORTER_OTLP_ENDPOINT, value: "http://otel-collector.observability.svc:4318"}
      - op: add
        path: /spec/template/spec/containers/0/env/-
        value: {name: OTEL_SERVICE_NAME, value: core-agent}
```

In Docker: `-e OTEL_TRACES_EXPORTER=otlp -e OTEL_EXPORTER_OTLP_ENDPOINT=http://...`. In systemd: `Environment=OTEL_TRACES_EXPORTER=otlp` etc. All standard OTel SDK env vars are honored by ADK-go's underlying SDK directly — no core-agent-side plumbing.

---

## Pitfalls

- **Cloud Trace requires `gcp.project_id` on every span's resource.** Without it, the managed collector's Cloud Trace exporter drops entire batches with `InvalidArgument: Resource is missing required attribute "gcp.project_id"`. Set `GOOGLE_CLOUD_PROJECT` in the daemon Pod's env (Vertex needs it anyway); `pkg/telemetry.Setup` reads it and passes to ADK via `WithGcpResourceProject`. Alternative: `OTEL_RESOURCE_ATTRIBUTES=gcp.project_id=<project>,...` — but note ADK's resource merge overrides it with `cfg.gcpResourceProject` (empty by default), so `GOOGLE_CLOUD_PROJECT` is the reliable path.
- **Silent export failures.** OTel SDK's default diag + error handlers are noop — export failures (unreachable collector, TLS mismatch, wrong port, permission-denied) go nowhere. `pkg/telemetry.Setup` installs stderr handlers (`otel-diag ...` + `otel-export: ...` prefixes) — grep the daemon log after any "no spans in the backend" symptom.
- **Set `OTEL_TRACES_EXPORTER` if config.json says `none`.** The env var is an override, not an additive setting. `otel.exporter: "none"` + `OTEL_TRACES_EXPORTER=otlp` → OTLP wins; but `OTEL_TRACES_EXPORTER=""` (empty) doesn't override.
- **HTTP vs gRPC endpoint ports.** OTLP HTTP is `:4318`, gRPC is `:4317`. GKE Managed OTel exposes HTTP only. Mismatch shows as `dial tcp: connection refused` in daemon logs.
- **Env vars need a Pod restart.** SDK reads env at process start. After changing `OTEL_*` on a running daemon, `kubectl rollout restart deployment/core-agent`.
- **Sampling defaults to `AlwaysOn`.** In production, set `OTEL_TRACES_SAMPLER=parentbased_traceidratio` + `OTEL_TRACES_SAMPLER_ARG=0.05` (5%) to keep collector load manageable.
- **`subagent.llm_call` requires the agentic wrap.** Without `--mcp-agentic-wrap-llm=true`, digest runs the structural pruner and no sub-agent span appears. This is a common cause of "cost dashboards look wrong" when the wrap is toggled off silently.
- **Polling reads on the attach listener don't trace.** The remote TUI polls `/status`, `/usage`, `/tools`, `/agents`, `/context`, `/memory`, `/skills`, `/mcp`, `/pricing`, `/perms` every 1-2s for status-bar rendering. Those GETs are excluded from tracing (via `otelhttp.WithFilter` on the attach handler) so they don't flood Cloud Trace with noise. Writes, SSE streams, and admin ops (DELETE) still trace normally.
- **Metrics aren't emitted yet.** Only traces (and internally-scoped logs) flow via OTLP today. ADK-go's telemetry package has a TODO for meter provider; tracked as [#338](https://github.com/go-steer/core-agent/issues/338) for our own wiring plus alignment with ADK's `gen_ai.*` histogram schema.
