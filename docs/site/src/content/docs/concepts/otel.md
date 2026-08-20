---
title: OpenTelemetry
---


`core-agent` emits [OpenTelemetry](https://opentelemetry.io) traces via ADK's built-in instrumentation plus a small set of custom spans — one per agent turn, and around MCP tool calls and the [structural pruner](/concepts/mcp/#agentic-wrap). Traces let you attribute cost, latency, and errors across model calls, tool invocations, and pruning passes without adding a logging middleware.

Configuration lives in `.agents/config.json` under the `otel:` key, with standard OpenTelemetry SDK env vars available as per-process overrides. The daemon speaks OTLP over HTTP or gRPC — point it at any OTLP-compatible collector (self-hosted OpenTelemetry Collector, [GKE Managed OpenTelemetry](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/managed-otel-gke), Jaeger, Honeycomb, etc.).

This page is about **traces**. [Metrics](/concepts/metrics/) — token/cost meters, latency histograms, subsystem gauges, Go runtime metrics — run on their own pipeline behind their own switch; enabling one does not enable the other.

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
agent.turn                              (root — core-agent's own turn span)
└── invoke_agent <agent>                (ADK's agent span, e.g. "invoke_agent core_agent")
    ├── generate_content <model>        (planner LLM call)
    └── mcp.tool_call                   {tool.name, tool.server, tool.call_id}
        ├── mcp.http_call               (otelhttp on the MCP transport, HTTP servers only)
        └── digest.process              {digest.strategy, digest.input_bytes, digest.output_bytes}
              └── subagent.llm_call     (agentic strategy only)
                    {model, input_tokens, output_tokens, savings.tokens_dropped}
```

`agent.turn` is core-agent's own span around one turn of `Agent.Run`. It exists because ADK's `invoke_agent` inherits whatever parent is on the context it's handed — on the daemon's wake loop that context has no span, so before `agent.turn` every turn started a fresh, parentless trace with nothing tying it to whatever asked for it. Opening our own span on the per-turn context makes ADK's the child, gives the turn a stable name to query on, and provides somewhere to hang the inject links described under [distributed tracing](#distributed-tracing-across-binaries).

Key attributes:

| Attribute | Where | Meaning |
|---|---|---|
| `gen_ai.conversation.id` | `agent.turn`, `invoke_agent` | Session ID. The one attribute that selects every span of a session — see [common queries](#common-queries). |
| `gen_ai.agent.name` | `agent.turn`, `invoke_agent` | The agent's name. |
| `core_agent.inbox.linked_injects` | `agent.turn` | How many injects this turn drained that carried a trace context. Absent when none did. Counts before the 32-link cap, so it can exceed the number of links on the span. |
| `tool.name` | `mcp.tool_call` | Fully-qualified tool name, e.g. `gke.list_clusters`. |
| `tool.server` | `mcp.tool_call` | The MCP server namespace. |
| `digest.strategy` | `digest.process` | `structural` \| `agentic` \| `passthrough`. |
| `digest.input_bytes` | `digest.process` | Response size before pruning. |
| `digest.output_bytes` | `digest.process` | Response size after pruning. |
| `savings.tokens_dropped` | `subagent.llm_call` | Tokens the LLM summarizer dropped from the raw response — the "savings" number shown in `/stats`. |
| `model` | `subagent.llm_call` | The sub-agent model used (usually cheaper than the planner). |

---

## Common queries

**Pull every span for one session.** Filter on `gen_ai.conversation.id = <session id>`. Both `agent.turn` and ADK's `invoke_agent` carry it, so this is the query that reconstructs "what did this session actually do", across however many separate turn traces it produced.

**Attribute cost to a specific MCP server.** Group `subagent.llm_call` by parent `mcp.tool_call.tool.server` and sum `input_tokens + output_tokens`. Answers "which MCP server is driving the LLM bill this week?"

**Find pruning regressions.** Filter `digest.process` where `digest.output_bytes > digest.input_bytes * 0.5` and `digest.strategy = "structural"` — pruner is failing to compress. Common cause: JSON-in-string that the pruner can't see through (see [PR #302](https://github.com/go-steer/core-agent/pull/302) for the fix history).

**Track tool-call tail latency.** Percentile query on `mcp.tool_call` duration, grouped by `tool.name`. The MCP layer is often the biggest driver of session wall-clock time.

**Confirm agentic wrap is active.** Presence of `subagent.llm_call` under `mcp.tool_call` proves the agentic path fired. If it's missing, the daemon is running the structural pruner instead — check `--mcp-agentic-wrap-llm` on the daemon args.

---

## Distributed tracing across binaries

When several agent binaries run alongside each other — daemon + event-watcher sidecar ([k8s-lookout](https://github.com/go-steer/k8s-lookout)'s `lookout watch`) + `core-agent-tui` client, or daemon + peer daemons — a single incident produces spans that live in different processes. Stitching them into one trace requires two things: the [W3C Trace Context](https://www.w3.org/TR/trace-context/) `traceparent` header propagating across HTTP hops, and the HTTP clients / servers on each hop being instrumented to extract + re-inject it.

`core-agent` uses OpenTelemetry's standard TextMapPropagator and [`otelhttp`](https://pkg.go.dev/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp) middleware to make this transparent:

- **Propagator registered globally** at daemon startup — supports both `traceparent` and `tracestate` (`pkg/telemetry/otel.go`). Every span the daemon emits carries the current trace's IDs.
- **Attach server** wraps the router in `otelhttp.NewHandler` (`pkg/attach/server.go`) — every inbound HTTP request extracts `traceparent` if present, becomes a root or child span, and the trace context flows into every downstream operation the request touches.
- **MCP client** wraps the outbound transport in `otelhttp.NewTransport` (`pkg/mcp/lifecycle.go`) — the `mcp.http_call` span you see in the span tree above rides on that transport, and MCP servers that speak OTel see the parent trace.
- **LLM calls** are instrumented on both Gemini-family backends, by different mechanisms. On Vertex AI, the genai SDK builds its HTTP client through `cloud.google.com/go/auth/httptransport`, whose default telemetry already wraps the transport in `otelhttp` — the daemon adds nothing. On the direct Gemini API (API-key auth), genai would fall back to an untraced client, so the provider supplies an `otelhttp`-wrapped one explicitly (`pkg/models/gemini/gemini.go`). Either way the outbound `generateContent` call shows up as an `HTTP POST` client span under `generate_content <model>` and carries `traceparent`.
- **The event-watcher sidecar** (`lookout watch`, shipped from [go-steer/k8s-lookout](https://github.com/go-steer/k8s-lookout) as `ghcr.io/go-steer/lookout` and deployed under the `lookout-watch` name) initializes the same OTel SDK at startup and wraps its outbound HTTP client so a `POST /sessions/{sid}/inject` from the sidecar starts a trace on the watcher, propagates via `traceparent`, and the daemon's `otelhttp.Handler` extracts it into the request context. The inject hop is one trace across two processes: the daemon's `POST /sessions/{sid}/inject` server span is a genuine child of the watcher's client span.

### An inject and the turn it causes are two traces, joined by a link

This is the part that surprises people, so it's worth stating plainly: **the turn is not on the inject's trace, and that is correct.**

An inject is asynchronous. `POST /inject` queues the message on the agent's inbox and returns `200` immediately — the handler's span ends there, typically milliseconds later. The turn that answers the message starts whenever the agent loop next drains the inbox, which may be seconds or minutes later, on a different goroutine, driven by the daemon's own loop context. There is no live span left to be a child of. Worse, injects **batch**: an agent that is mid-turn accumulates every inject that arrives, and the next turn drains them all at once. A parent-child edge would have to pick one of them and misattribute the whole turn to it.

So core-agent uses [span links](https://opentelemetry.io/docs/concepts/signals/traces/#span-links), which exist for exactly this asynchronous fan-in shape. The turn opens its own trace rooted at `agent.turn`, and attaches **one link per drained inject** that arrived with a valid trace context. Following a link in your backend jumps from the turn to the watcher-side request that asked for it, and every inject in a batch keeps its own edge.

Details worth knowing:

- Injects that carry no trace context — the CLI, library callers, `core-agent-tui`, auto-continue's own notes, anything queued while tracing was off — contribute no link. The turn span is still there; it just has none.
- Links are capped at **32 per turn** (the inbox itself holds up to 256). The most recent injects win, since the last caller in a batch is the turn's originator. `core_agent.inbox.linked_injects` records the pre-cap count.
- `POST /wake` with a `prompt` is an inject and behaves identically. `POST /resume` with a steer message does **not** link today — its API takes no context.

### End-to-end span tree

A full triage inject on GKE with the OTel overlay applied produces roughly this — note the three separate traces:

```
── trace A (watcher) ──────────────────────────────────────────
HTTP POST                                   (root — watcher's inject call, service.name=lookout-watch)
└── POST /sessions/{sid}/inject             (daemon's attach server span, service.name=core-agent)

── trace B (the turn) ─────────────────────────────────────────
agent.turn                                  (root — core-agent's turn span)
│     ↳ link → the "POST /sessions/{sid}/inject" span in trace A (one per drained inject)
└── invoke_agent core_agent                 (ADK-emitted agent span)
    ├── generate_content <model>            (ADK-emitted LLM call, e.g. "generate_content gemini-3.7-flash")
    │   └── HTTP POST                       (otelhttp on the genai HTTP client → Vertex / Gemini)
    ├── execute_tool <tool_name>            (per tool call, e.g. "execute_tool gke_get_k8s_resource")
    ├── invoke_agent <subagent>             (a spawned subagent's turn, nested in-process)
    ├── mcp.tool_call                       (our custom span wrapping the MCP round-trip)
    │   ├── mcp.http_call                   (otelhttp on MCP HTTP transport)
    │   └── digest.process                  (response digest / prune)
    │         └── subagent.llm_call         (agentic wrap only — --mcp-agentic-wrap-llm=true)
    └── tools/call <tool_name>              (MCP server's own instrumentation; may appear as a separate root)

── trace C (session creation, if the watcher created one) ─────
HTTP POST → POST /sessions                  (watcher creating the session, same two-span shape as A)
```

Attach-server spans use the `METHOD PATH` naming convention (from `otelhttp.WithSpanNameFormatter` in `pkg/attach/server.go`), so what shows up in your backend is literally `POST /sessions/019f8075.../inject` — not a semantic-name like `attach.inject`. Filter / query by path prefix if you want to isolate all inject events, or by the `http.route` attribute if your backend surfaces it.

### Verifying it works

Everything below is verified against live data from a GKE cluster running the OTel overlay.

**Do not filter by service name on the Cloud Trace v1 REST API.** `filter=%2Bservice_name:core-agent` returns zero traces even on a cluster that is exporting correctly — that filter keys off Cloud Trace's legacy per-trace service concept, not the OTel `service.name` resource attribute the collector writes. (This caveat is about the v1 REST API specifically; the console's own service facet is untested here.) Filter on span attributes instead:

| Goal | Filter |
|---|---|
| The inject hops | `url.path:/sessions/<sid>/inject` |
| The turns for one session | `gen_ai.conversation.id:<sid>` |
| Everything from one namespace | `k8s.namespace.name:<ns>` |

What you should see:

- Two spans on the inject trace: the watcher's client span (`service.name=lookout-watch`) with the daemon's `POST /sessions/{sid}/inject` as its **child** (`service.name=core-agent`). That parent-child edge across processes is the propagation check — if it's missing, `traceparent` really isn't getting through.
- A separate trace rooted at `agent.turn`, carrying a **link** to that inject span, with `invoke_agent core_agent` and the whole tool/model waterfall beneath it. Separate is expected; see the section above.
- `HTTP POST` spans under `generate_content` show the outbound Vertex calls are instrumented.

Troubleshooting, in the order worth checking:

- **`agent.turn` and the watcher's spans are on different traces.** Normal and correct — that's the async inject boundary, not a bug. Check the turn span's links, not its parent.
- **The turn span has no links.** The inject reached the daemon without a trace context. Either the watcher isn't exporting (`OTEL_TRACES_EXPORTER=otlp` unset in the sidecar — spans get recorded and dropped), something between the two is stripping `traceparent` (rare; most cloud LBs pass it through), or the message was injected by something that isn't the watcher (TUI, CLI, auto-continue), which never had a trace context to pass.
- **`POST /sessions/{sid}/inject` is a root instead of the watcher's child.** Now `traceparent` really is being lost on the wire. Same two causes as above, minus the "it wasn't the watcher" case.
- **No daemon spans at all.** The attach listener isn't going through `otelhttp.NewHandler` (only happens if `attach.listen` is disabled — the wrap is unconditional otherwise), or the daemon's exporter is off / misconfigured. Check the daemon log for `otel-export:` lines; see [Pitfalls](#pitfalls).

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
- **Metrics are a separate pipeline with a separate switch.** `OTEL_TRACES_EXPORTER` turns on spans and nothing else; metrics need `otel.metrics.exporter` or `OTEL_METRICS_EXPORTER`. ADK-go has no MeterProvider (upstream TODO), so the daemon builds its own. Full instrument inventory, PromQL samples, cardinality controls, and pitfalls: [Metrics](/concepts/metrics/).
