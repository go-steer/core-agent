# `example-otel` overlay

The [`example`](../example/) overlay + OpenTelemetry to [Google Cloud Managed OpenTelemetry for GKE](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/managed-otel-gke). Spans land in [Cloud Trace](https://console.cloud.google.com/traces) and metrics in [Cloud Monitoring](https://console.cloud.google.com/monitoring) — no collector to deploy, no Deployment to maintain.

## How it's assembled

Three composable pieces:

1. **`../example`** — the plain overlay (base + agents ConfigMap + watcher cluster-name patch).
2. **[`../../components/otel`](../../components/otel/)** — the component that flips `pkg/telemetry`'s exporters from `none` to `otlp` via `OTEL_TRACES_EXPORTER` and `OTEL_METRICS_EXPORTER`. Those are the only core-agent-specific knobs. Two vars, not one, because traces and metrics are separate pipelines with separate switches — ADK-go has no `MeterProvider`, so the daemon builds its own and reads its own var.
3. **[`instrumentation.yaml`](instrumentation.yaml)** — a GKE Managed OpenTelemetry `Instrumentation` CR. Empty selector, so it targets all Pods in the `agent-triage` namespace. GKE auto-injects a subset of standard OTel SDK env vars: `OTEL_EXPORTER_OTLP_ENDPOINT` (in-cluster managed collector), `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, `OTEL_METRIC_EXPORT_INTERVAL`, `K8S_POD_UID`, sampler config, and `OTEL_RESOURCE_ATTRIBUTES` with `k8s.pod.uid` (collector then attaches `k8s.namespace.name` etc. server-side). **`OTEL_SERVICE_NAME` is NOT auto-injected** — the component sets it explicitly on the daemon + watcher deployments.

Images are pinned to `2.9.0-dev.1`. That is a floor, not a preference: the recipe's `config.json` uses `alerts` and `tools.wait_and_verify`, and an older daemon does not reject that config — `pkg/config` ignores unknown keys, so it boots clean, drops both blocks, and registers neither tool ([#680](https://github.com/go-steer/core-agent/issues/680)). The `OTEL_TRACES_EXPORTER` env override this overlay relies on first shipped in `2.7.0-dev.4` ([PR #315](https://github.com/go-steer/core-agent/pull/315)); 2.8.0 added the full metrics pipeline + Go runtime instrumentation ([#325](https://github.com/go-steer/core-agent/issues/325) / [#338](https://github.com/go-steer/core-agent/issues/338)).

Off-GKE deployments use the same component with a different endpoint source — see [`components/otel/README.md` § Non-GKE](../../components/otel/README.md#non-gke-self-managed-collector-docker-etc).

## GKE prerequisites (one-time, cluster-wide)

Managed OpenTelemetry is a cluster-wide toggle, and the CR shipped by this overlay requires the CRD it installs. Run these once against the target cluster before applying:

    # 1. Enable the required Google Cloud APIs
    gcloud services enable \
      cloudtrace.googleapis.com \
      monitoring.googleapis.com \
      telemetry.googleapis.com \
      --project=<PROJECT>

    # 2. Enable managed OTel on the cluster. Provisions the
    #    opentelemetry-collector Deployment + Service in the
    #    `gke-managed-otel` namespace AND installs the
    #    `Instrumentation` CRD this overlay applies.
    gcloud container clusters update <CLUSTER> \
      --location=<REGION> \
      --managed-otel-scope=COLLECTION_AND_INSTRUMENTATION_COMPONENTS

    # 3. Grant the telemetry writer roles to the daemon Pod's identity
    #    — Cloud Trace for spans, Cloud Monitoring for metrics.
    #    Default Compute Engine SA path:
    SA="serviceAccount:$(gcloud projects describe <PROJECT> \
      --format='value(projectNumber)')-compute@developer.gserviceaccount.com"
    gcloud projects add-iam-policy-binding <PROJECT> \
      --member="$SA" --role="roles/cloudtrace.user"
    gcloud projects add-iam-policy-binding <PROJECT> \
      --member="$SA" --role="roles/monitoring.metricWriter"

    # (Workload Identity: grant to the WI-bound Google SA the KSA
    # `core-agent` in namespace `agent-triage` impersonates instead.
    # `scripts/setup-wif.sh` does all of this for the WIF path.)

Requires GKE control plane `1.34.1-gke.2178000` or later, gcloud `551.0.0` or later.

Verify the collector Pods are running:

    kubectl get pods -n gke-managed-otel

You should see `opentelemetry-collector-*` Pods `Ready 1/1`.

## Apply

From the repo root:

    kubectl apply -k examples/gke-troubleshoot-agent/deploy/overlays/example-otel/

If the daemon was already running (e.g. migrating from the plain `example` overlay), restart it so the injected env vars take effect:

    kubectl rollout restart deployment/core-agent -n agent-triage

## Verify

### Traces

Trigger any tool call (kill a Pod to fire the watcher, or use `core-agent-cli` against the daemon), then open [Cloud Trace Explorer](https://console.cloud.google.com/traces), filter by service `core-agent`. Expected span tree (documented at [Concepts → OpenTelemetry](https://go-steer.github.io/core-agent/concepts/otel/)):

    mcp.tool_call
    ├── mcp.http_call
    └── digest.process
          └── subagent.llm_call    (agentic path only)

### Metrics

⚠️ **Not yet verified against a live cluster.** The Prometheus path is covered by unit tests and local UAT; OTLP metrics landing in Cloud Monitoring from a real GKE cluster has not been observed, so treat this section as the expected shape rather than a confirmed one ([#554](https://github.com/go-steer/core-agent/issues/554)).

After the same trigger, [Metrics Explorer](https://console.cloud.google.com/monitoring/metrics-explorer) should carry the daemon's instruments under the `workload.googleapis.com/` prefix that the managed collector writes OTLP metrics to — start with `gen_ai.client.token.usage` (grouped by `gen_ai.request.model`) and `core_agent.session.cost_usd`, which move on every turn. `go_goroutine_count` and the other `go_*` runtime series are the liveness check: if they appear and nothing else does, the pipeline is healthy and the agent simply hasn't run a turn.

The full inventory, the per-instrument attributes, and PromQL for the pull path are on [Concepts → Metrics](https://go-steer.github.io/core-agent/concepts/metrics/).

## Customizing

**Sampling / resource attrs / prompt+response capture.** Extend `instrumentation.yaml` — that's what the CR is for. Sampling ratio, metric interval, `promptsResponses.uploadBasePath` for prompt/response capture, all live on the CR. See the [GKE Managed OTel docs](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/managed-otel-gke) for the CR schema.

**Selecting a subset of Pods.** If you're layering this overlay into a larger namespace and want the CR to target only core-agent, replace `selector: {}` with a label match:

    spec:
      selector:
        matchLabels:
          app.kubernetes.io/name: core-agent

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `kubectl apply -k` errors: `no matches for kind "Instrumentation"` | Cluster doesn't have managed OTel enabled — re-run the `gcloud container clusters update --managed-otel-scope=...` command from prereqs. |
| No traces in Cloud Trace + collector logs `InvalidArgument: Resource is missing required attribute "gcp.project_id"` | The daemon isn't stamping the `gcp.project_id` resource attribute Cloud Trace requires. Verify `GOOGLE_CLOUD_PROJECT` is set in the daemon Pod env — the base recipe wires it via `envFrom: core-agent-gcp-env`. `kubectl describe pod` on the daemon; if absent, the ConfigMap wasn't populated for your cluster. `pkg/telemetry.Setup` reads `GOOGLE_CLOUD_PROJECT` and passes it to ADK via `WithGcpResourceProject` so the resource stamp is non-empty. |
| No traces in Cloud Trace after 2–3 minutes (no collector-side error) | (1) Pod not restarted after apply, (2) IAM: `roles/cloudtrace.user` not granted, (3) collector not running (`kubectl get pods -n gke-managed-otel`). |
| Daemon logs `otel-export: ...` or `otel-diag ...` | The visibility hooks in `pkg/telemetry.Setup` — export failure surfaces (unreachable collector, TLS, protocol mismatch). Read the specific error to diagnose. |
| Daemon logs `OTLP export failed: dial tcp: ... connection refused` | Managed OTel enabled but the `Instrumentation` CR didn't reach the daemon Pod — check `kubectl describe pod` for injected env vars; if absent, verify the CR is in the same namespace. |
| Traces show but span tree stops at `mcp.tool_call` | Agentic wrap disabled. Set `--mcp-agentic-wrap-llm=true` on the daemon (or in the base config) to see the `subagent.llm_call` child span. |
| `k8s.namespace.name` / `k8s.pod.name` missing on spans | Managed OTel's k8s-attributes processor needs Pod-metadata access; usually the default, but restrictive Workload Identity setups can strip it. Check collector logs. |
| Traces appear but no metrics | The two pipelines have separate switches. `kubectl exec` into the daemon and check `OTEL_METRICS_EXPORTER` is `otlp` — the component sets it, but only for Pods created after the apply. Then check the daemon's boot log for the `telemetry: metrics OTLP HTTP exporter → ...` line: absent means the switch never took. |
| Metrics rejected: `PermissionDenied` in the collector log | `roles/monitoring.metricWriter` not granted to the Pod identity (prereq 3), or `monitoring.googleapis.com` not enabled (prereq 1). |
| Metrics rejected: `Resource is missing required attribute "gcp.project_id"` | Same cause and fix as the trace-side row above — `GOOGLE_CLOUD_PROJECT` in the daemon Pod env. The metrics pipeline doesn't go through ADK, so `pkg/telemetry.SetupMetrics` stamps the attribute itself from that same var. |
