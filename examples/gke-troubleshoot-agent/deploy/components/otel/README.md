# OTel enablement component

Opt-in kustomize component that switches core-agent's exporters from `none` to `otlp` and sets the service name for both the daemon and watcher containers. Deliberately narrow — a handful of env vars, none of them auto-injected off GKE.

## What it does

Patches the daemon's `core-agent` container and the watcher Deployment's `watcher` container (the Deployment is named `lookout-watch`; its container is not) with:

| Var | Value | Purpose |
|---|---|---|
| `OTEL_TRACES_EXPORTER` | `otlp` | Overrides `otel.exporter` from the base `config.json` (default `none`). See [PR #315](https://github.com/go-steer/core-agent/pull/315). |
| `OTEL_METRICS_EXPORTER` | `otlp` (daemon only) | Overrides `otel.metrics.exporter` (default `none`). **Metrics are a separate pipeline with a separate switch** — ADK-go has no `MeterProvider`, so `pkg/telemetry.SetupMetrics` builds its own and reads its own var. Setting only `OTEL_TRACES_EXPORTER` gets you spans and no metrics. Not set on the watcher: lookout publishes `lookout_*` through its own Prometheus endpoint (`--metrics-addr`), not OTLP. |
| `OTEL_SERVICE_NAME` | `core-agent` / `lookout-watch` | GKE Managed OTel's `Instrumentation` CR does NOT auto-inject this ([docs](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/managed-otel-gke#environment_variables)). For the **daemon**, without it spans surface as `unknown_service:<binary>` in Cloud Trace. For the **watcher**, lookout has defaulted its own spans to `service.name=lookout` since v0.21.0, so this is not a fix for a broken name — it is a deliberate override, so the service name in Cloud Trace matches the Deployment an operator greps for rather than the binary. |

What the daemon then exports: per-session GenAI token/cost meters, two ADK-schema latency histograms, the `core_agent.*` subsystem meters, and Go runtime metrics. Full inventory, PromQL samples, and caveats: [Metrics](https://go-steer.github.io/core-agent/concepts/metrics/).

**GOOGLE_CLOUD_PROJECT** (for the `gcp.project_id` resource attribute Cloud Trace requires) is already wired via the base's `envFrom` reference to the `core-agent-gcp-env` ConfigMap. `pkg/telemetry.Setup` reads it and passes to ADK via `WithGcpResourceProject` so the resource attribute is stamped correctly.

Endpoint discovery, sampling ratio, metric export interval, and k8s.* resource attributes ride the standard OTel SDK env vars — auto-injected by the `Instrumentation` CR on GKE, or set operator-side off-GKE (see "Non-GKE" below).

## Composing

```yaml
resources:
  - ../../base
components:
  - ../../components/otel
```

Then supply the endpoint via one of the paths below.

## On GKE (Managed OpenTelemetry)

Compose this component **plus** ship an `Instrumentation` CR in your overlay. The CR auto-injects `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_METRIC_EXPORT_INTERVAL`, `K8S_POD_UID`, sampler config, and `OTEL_RESOURCE_ATTRIBUTES` (with `k8s.pod.uid`) into every Pod matched by its selector. Notably absent from the auto-inject list: `OTEL_SERVICE_NAME` — which is exactly why this component sets it.

See [`overlays/example-otel/`](../../overlays/example-otel/) for the canonical shape.

Cluster prereqs (one-time, before the overlay applies):

    gcloud services enable cloudtrace.googleapis.com monitoring.googleapis.com \
      telemetry.googleapis.com
    gcloud container clusters update <CLUSTER> --location=<REGION> \
      --managed-otel-scope=COLLECTION_AND_INSTRUMENTATION_COMPONENTS
    gcloud projects add-iam-policy-binding <PROJECT> \
      --member="serviceAccount:<POD-SA>" \
      --role="roles/cloudtrace.user"
    gcloud projects add-iam-policy-binding <PROJECT> \
      --member="serviceAccount:<POD-SA>" \
      --role="roles/monitoring.metricWriter"

`monitoring.googleapis.com` + `roles/monitoring.metricWriter` are the metrics half; `cloudtrace.googleapis.com` + `roles/cloudtrace.user` are the traces half. Both are inert if the corresponding exporter is off. `scripts/setup-wif.sh` applies all four for you.

Requires GKE control plane `1.34.1-gke.2178000` or later, gcloud `551.0.0` or later.

> **Not yet verified live.** The traces path is confirmed end-to-end against a real GKE cluster. The metrics path is confirmed locally (Prometheus scrape + unit tests) but **OTLP metrics reaching Cloud Monitoring from a live cluster has not been observed** — the prerequisites above are documented from the Cloud Monitoring API's requirements, not from a run. Tracked in [#554](https://github.com/go-steer/core-agent/issues/554).

## Non-GKE (self-managed Collector, Docker, etc.)

Compose the component + patch the endpoint (and anything else) into the daemon Deployment yourself:

```yaml
resources:
  - ../../base
components:
  - ../../components/otel
patches:
  - target:
      kind: Deployment
      name: core-agent
    patch: |-
      - op: add
        path: /spec/template/spec/containers/0/env/-
        value: {name: OTEL_EXPORTER_OTLP_ENDPOINT, value: "http://otel-collector.observability.svc:4318"}
      - op: add
        path: /spec/template/spec/containers/0/env/-
        value: {name: OTEL_SERVICE_NAME, value: core-agent}
      - op: add
        path: /spec/template/spec/containers/0/env/-
        value: {name: OTEL_RESOURCE_ATTRIBUTES, value: "deployment.environment=prod,team=sre"}
```

Same standard OTel SDK env vars work in any environment — Compose (`environment:`), systemd (`Environment=`), plain `docker run -e`, whatever.

## Sampling

Defaults to `AlwaysOn` (every span exported). Dial down in production via the standard sampler env vars — set them on the `Instrumentation` CR (on GKE) or add to your Deployment patch (off GKE):

    OTEL_TRACES_SAMPLER=parentbased_traceidratio
    OTEL_TRACES_SAMPLER_ARG=0.05     # 5%

## Applying to an existing deployment

Env vars inject only at Pod creation. After `kubectl apply -k` on an already-running daemon, restart it:

    kubectl rollout restart deployment/core-agent -n agent-triage

## Alternatives (not this component)

- **`Instrumentation` CRD alone (no component)**: possible if you flip `pkg/telemetry`'s config-file default from `none` to `otlp`, but that changes default behavior for non-GKE deployments — any operator without an endpoint gets a fail-loud SDK on startup. This component keeps `pkg/telemetry`'s default safe and makes exporter enablement an explicit, per-overlay opt-in.
- **OpenTelemetry Operator (OSS)**: also ships an `Instrumentation` CRD, but its schema and purpose are different — it side-loads auto-instrumentation agents into uninstrumented apps. Doesn't compose here; core-agent is already instrumented via ADK-go's OTel SDK. Non-GKE operators use env vars directly.
