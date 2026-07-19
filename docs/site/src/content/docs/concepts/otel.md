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
watcher.inject                          (root — watcher process)
└── http.POST /sessions/{sid}/inject    (otelhttp on watcher's client)
    └── attach.inject                   (attach server on daemon; extracted from traceparent)
        └── adk.invoke_agent            (session turn)
            ├── adk.call_llm            (planner)
            └── mcp.tool_call
                ├── mcp.http_call       (otelhttp on MCP transport)
                └── digest.process
                      └── subagent.llm_call
```

The watcher's root span and the daemon's `attach.inject` span share a trace ID. Jaeger / Cloud Trace / Tempo will render them as one waterfall.

### Verifying it works

In Cloud Trace, filter by `service.name = "k8s-event-watcher"` and open one trace. You should see a child span with `service.name = "core-agent"` on the same trace ID. If the daemon's spans appear on a separate trace, `traceparent` isn't being propagated — likely causes:

- Watcher didn't have `OTEL_TRACES_EXPORTER=otlp` set (spans get recorded but dropped).
- A reverse proxy or load balancer between the two is stripping `traceparent` (rare — most cloud LBs pass it through).
- The daemon's attach listener isn't going through `otelhttp.NewHandler` (only happens if `attach.listen` is disabled — the wrap is unconditional otherwise).

### Known gap: Vertex / Gemini calls

**The Vertex / Gemini genai client is not currently wrapped in `otelhttp`.** `adk.call_llm` and `subagent.llm_call` spans exist (ADK emits them internally), but the outbound HTTPS request to `generativelanguage.googleapis.com` / `aiplatform.googleapis.com` produces no `http.POST vertex.generate`-shaped span, and no `traceparent` header is sent to Google. Traces stop at the LLM boundary — you can attribute cost + latency at the ADK layer but can't stitch the model's internal timing (if Google ever exposes it) back to the caller's trace.

Wrapping `genai.ClientConfig.HTTPClient` with `otelhttp.NewTransport` closes this gap. Tracked as follow-up work; ADK-Go's client-injection points at `pkg/models/gemini/gemini.go` are the natural hook.

---

## Deploying on Kubernetes

The GKE troubleshooting recipe ships a reusable kustomize component and a canonical overlay that wire OTel export in two composable pieces:

- **[`components/otel`](https://github.com/go-steer/core-agent/tree/main/examples/gke-troubleshoot-agent/deploy/components/otel)** — one-env-var component that flips the daemon's exporter from `none` to `otlp` via `OTEL_TRACES_EXPORTER`. Environment-agnostic; the same component works on and off GKE.
- **Endpoint + service + resource attrs** — supplied by the runtime environment via standard OTel SDK env vars. Where those come from depends on where you're deploying.

### On GKE (Managed OpenTelemetry)

The [`example-otel`](https://github.com/go-steer/core-agent/tree/main/examples/gke-troubleshoot-agent/deploy/overlays/example-otel) overlay composes the component + an `Instrumentation` CR. [GKE Managed OpenTelemetry](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/managed-otel-gke) auto-injects the standard OTel SDK env vars (endpoint targeting the in-cluster managed collector, service name, resource attrs with `k8s.*`) into every Pod matched by the CR's selector. Spans land in Cloud Trace with no self-managed collector to run.

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

- **Set `OTEL_TRACES_EXPORTER` if config.json says `none`.** The env var is an override, not an additive setting. `otel.exporter: "none"` + `OTEL_TRACES_EXPORTER=otlp` → OTLP wins; but `OTEL_TRACES_EXPORTER=""` (empty) doesn't override.
- **HTTP vs gRPC endpoint ports.** OTLP HTTP is `:4318`, gRPC is `:4317`. GKE Managed OTel exposes HTTP only. Mismatch shows as `dial tcp: connection refused` in daemon logs.
- **Env vars need a Pod restart.** SDK reads env at process start. After changing `OTEL_*` on a running daemon, `kubectl rollout restart deployment/core-agent`.
- **Sampling defaults to `AlwaysOn`.** In production, set `OTEL_TRACES_SAMPLER=parentbased_traceidratio` + `OTEL_TRACES_SAMPLER_ARG=0.05` (5%) to keep collector load manageable.
- **`subagent.llm_call` requires the agentic wrap.** Without `--mcp-agentic-wrap-llm=true`, digest runs the structural pruner and no sub-agent span appears. This is a common cause of "cost dashboards look wrong" when the wrap is toggled off silently.
