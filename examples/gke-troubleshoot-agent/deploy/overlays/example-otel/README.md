# `example-otel` overlay

The [`example`](../example/) overlay + OpenTelemetry tracing to [Google Cloud Managed OpenTelemetry for GKE](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/managed-otel-gke). Spans land in [Cloud Trace](https://console.cloud.google.com/traces) — no collector to deploy, no CRDs to install.

Same shape as `example/`:
- Composes the `example` overlay as-is (base + agents ConfigMap + watcher cluster-name patch).
- Adds the [`components/otel`](../../components/otel/) component, which patches the daemon container's `env:` with the four standard OTel SDK env vars.
- Pins images to `2.7.0-dev.4` — the first release that includes the `OTEL_TRACES_EXPORTER` env override ([PR #315](https://github.com/go-steer/core-agent/pull/315)).

For the mechanics of what the OTel component does, see [`components/otel/README.md`](../../components/otel/README.md). This file only covers what an operator does *around* applying this overlay.

## GKE prerequisites (one-time, cluster-wide)

Managed OpenTelemetry is a cluster-wide toggle. Run these once against the target cluster before applying this overlay:

    # 1. Enable the required Google Cloud APIs
    gcloud services enable \
      cloudtrace.googleapis.com \
      telemetry.googleapis.com \
      --project=<PROJECT>

    # 2. Enable managed OTel on the cluster. This provisions the
    #    opentelemetry-collector Deployment + Service in the
    #    `gke-managed-otel` namespace.
    gcloud container clusters update <CLUSTER> \
      --location=<REGION> \
      --managed-otel-scope=COLLECTION_AND_INSTRUMENTATION_COMPONENTS

    # 3. Grant Cloud Trace user role to the daemon Pod's identity.
    #    For clusters using the default Compute Engine SA:
    gcloud projects add-iam-policy-binding <PROJECT> \
      --member="serviceAccount:$(gcloud projects describe <PROJECT> \
        --format='value(projectNumber)')-compute@developer.gserviceaccount.com" \
      --role="roles/cloudtrace.user"

    # (Workload Identity: grant to the WI-bound Google SA the KSA
    # `core-agent` in namespace `agent-triage` impersonates instead.)

Requirements: GKE control plane `1.34.1-gke.2178000` or later, gcloud `551.0.0` or later.

Verify the collector is running:

    kubectl get pods -n gke-managed-otel

You should see `opentelemetry-collector-*` Pods `Ready 1/1`.

## Apply the overlay

From the repo root:

    kubectl apply -k examples/gke-troubleshoot-agent/deploy/overlays/example-otel/

If the daemon was already running (e.g. you migrated from the plain `example` overlay), restart it so the new env vars take effect:

    kubectl rollout restart deployment/core-agent -n agent-triage

## Verify spans reach Cloud Trace

Trigger any tool call. The simplest is:

    ./bin/core-agent-cli --daemon-url=$(kubectl get svc core-agent -n agent-triage -o jsonpath='{...}') \
      "list clusters in the current project"

Then open [Cloud Trace Explorer](https://console.cloud.google.com/traces), filter by service `core-agent`, and you should see a trace with the span tree documented in [`docs/reference/otel.md`](../../../../../docs/site/content/docs/reference/otel.md):

    mcp.tool_call
    ├── mcp.http_call
    └── digest.process
          └── subagent.llm_call    (agentic path only)

## Customizing

**Sampling.** By default every span is exported. To sample, patch the daemon env in your own overlay that composes this one:

```yaml
patches:
  - target:
      kind: Deployment
      name: core-agent
    patch: |-
      - op: add
        path: /spec/template/spec/containers/0/env/-
        value:
          name: OTEL_TRACES_SAMPLER
          value: parentbased_traceidratio
      - op: add
        path: /spec/template/spec/containers/0/env/-
        value:
          name: OTEL_TRACES_SAMPLER_ARG
          value: "0.05"
```

**Different environment / cluster labels.** Patch `OTEL_RESOURCE_ATTRIBUTES`:

```yaml
- op: replace
  path: /spec/template/spec/containers/0/env/3/value
  value: "deployment.environment=prod,team=sre"
```

(The index `3` matches the order in `components/otel/daemon-env.yaml`; keep in sync if that file changes.)

**Non-GKE / self-managed collector.** Skip the GKE prerequisites and override the endpoint. See [`components/otel/README.md` § Tuning](../../components/otel/README.md#tuning).

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| No traces in Cloud Trace after 2–3 minutes | (1) Pod not restarted after apply, (2) IAM: `roles/cloudtrace.user` not granted, (3) collector not running (`kubectl get pods -n gke-managed-otel`). |
| Daemon logs `OTLP export failed: dial tcp: ... connection refused` | Managed OTel not enabled on the cluster — re-run the `gcloud container clusters update --managed-otel-scope=...` command. |
| Traces show but span tree stops at `mcp.tool_call` | Agentic wrap disabled. Set `--mcp-agentic-wrap-llm=true` on the daemon (or in the base's config) to see the `subagent.llm_call` child span. |
| `k8s.namespace.name` / `k8s.pod.name` attributes missing | Managed OTel's k8s-attributes processor requires the collector to have access to Pod metadata — this is the default, but Workload Identity + restrictive IAM can strip it. Check the collector logs. |
