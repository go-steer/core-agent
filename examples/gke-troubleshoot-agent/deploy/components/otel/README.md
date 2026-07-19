# OTel enablement component

Opt-in kustomize component that turns on OpenTelemetry OTLP export from the core-agent daemon. Defaults target [Google Cloud Managed OpenTelemetry for GKE](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/managed-otel-gke) — spans land in Cloud Trace, no self-managed collector to run.

## What it does

Patches the daemon Deployment's `env:` to add four standard OpenTelemetry SDK vars:

| Var | Default | Purpose |
|---|---|---|
| `OTEL_TRACES_EXPORTER` | `otlp` | Overrides `cfg.OTEL.Exporter` (see PR #315). |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://opentelemetry-collector.gke-managed-otel.svc.cluster.local:4318` | GKE Managed OTel collector's HTTP OTLP endpoint. |
| `OTEL_SERVICE_NAME` | `core-agent` | Service name in Cloud Trace / Jaeger / Honeycomb filters. |
| `OTEL_RESOURCE_ATTRIBUTES` | `deployment.environment=demo` | Resource-level attributes stamped on every span. |

No collector Deployment. No CRDs. No secrets. Just the env-var patch — GKE handles the rest.

## Prerequisites (cluster-side, one-time)

Managed OpenTelemetry is a **cluster-wide toggle** on GKE. Before applying an overlay that composes this component:

    # 1. Enable the required APIs (idempotent)
    gcloud services enable \
      cloudtrace.googleapis.com \
      telemetry.googleapis.com \
      --project=<PROJECT>

    # 2. Enable managed OTel on the cluster
    gcloud container clusters update <CLUSTER> \
      --location=<REGION> \
      --managed-otel-scope=COLLECTION_AND_INSTRUMENTATION_COMPONENTS

    # 3. Grant Cloud Trace user to the Pod's identity. For the default
    #    Compute Engine default SA (most demo clusters):
    gcloud projects add-iam-policy-binding <PROJECT> \
      --member="serviceAccount:$(gcloud projects describe <PROJECT> \
        --format='value(projectNumber)')-compute@developer.gserviceaccount.com" \
      --role="roles/cloudtrace.user"

    # (If you use Workload Identity, grant the role to the WI-bound
    # Google SA that the core-agent Pod's KSA impersonates instead.)

Requires GKE control-plane version `1.34.1-gke.2178000` or later and gcloud `551.0.0` or later.

GKE deploys the collector into the `gke-managed-otel` namespace on cluster update. Verify with:

    kubectl get pods -n gke-managed-otel

## Composing

Add to any overlay's `kustomization.yaml`:

```yaml
resources:
  - ../../base
components:
  - ../../components/otel
```

For the canonical example see [`deploy/overlays/example-otel/`](../../overlays/example-otel/).

## Tuning

### Endpoint (non-GKE / self-managed collector)

Override the endpoint via a small patch in your overlay:

```yaml
patches:
  - target:
      kind: Deployment
      name: core-agent
    patch: |-
      - op: replace
        path: /spec/template/spec/containers/0/env/1/value
        value: "http://my-collector.observability.svc:4318"
```

Or use `patchesStrategicMerge` with the same shape as `daemon-env.yaml` here.

### Sampling

Defaults to 100% (every span exported). For production, dial down via extra env vars:

```yaml
env:
  - name: OTEL_TRACES_SAMPLER
    value: "parentbased_traceidratio"
  - name: OTEL_TRACES_SAMPLER_ARG
    value: "0.05"   # 5%
```

Same standard OTel SDK env vars ADK's underlying SDK honors — no code change required.

### Resource attributes

Extend `OTEL_RESOURCE_ATTRIBUTES` in your overlay to add cluster / region / team labels:

    "deployment.environment=prod,k8s.cluster.name=prod-us-central1,team=sre"

Comma-separated `key=value` pairs. GKE Managed OTel's k8s-attributes processor already stamps `k8s.namespace.name` / `k8s.pod.name` / `k8s.container.name` automatically — you don't need to duplicate those.

## Applying to an existing deployment

Env vars inject only at Pod creation. After `kubectl apply -k` on an already-running daemon, restart it:

    kubectl rollout restart deployment/core-agent -n agent-triage

## Verifying spans reach Cloud Trace

After a triage inject fires (or any tool call), visit [Cloud Trace Explorer](https://console.cloud.google.com/traces) and filter by service name `core-agent`. You should see traces with the span hierarchy documented in [`docs/site/content/docs/reference/otel.md`](../../../../docs/site/content/docs/reference/otel.md):

    mcp.tool_call
    ├── mcp.http_call            (from otelhttp on the MCP transport)
    └── digest.process           (structural / agentic / passthrough)
          └── subagent.llm_call  (agentic path only — requires --mcp-agentic-wrap-llm=true)

If no traces appear:

- Confirm cluster has managed OTel enabled: `kubectl get pods -n gke-managed-otel`
- Confirm IAM: the Pod's SA has `roles/cloudtrace.user`
- Confirm the daemon actually restarted after the overlay landed
- Check collector logs: `kubectl logs -n gke-managed-otel -l app=opentelemetry-collector -c opentelemetry-collector`

## Alternatives (not this component)

- **`Instrumentation` CRD auto-injection**: GKE Managed OTel also supports a namespace-scoped `Instrumentation` CR that label-selects Pods and injects the env vars automatically. Equivalent outcome; requires cluster-level CRD access. This component uses the env-patch path so overlays without CRD permissions still work.
- **Self-managed OpenTelemetry Collector**: Deploy your own collector Deployment + config to route to Jaeger / Honeycomb / self-hosted backends. Just point `OTEL_EXPORTER_OTLP_ENDPOINT` at it (see "Tuning" above); everything else stays the same.
