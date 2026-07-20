# `example-otel` overlay

The [`example`](../example/) overlay + OpenTelemetry tracing to [Google Cloud Managed OpenTelemetry for GKE](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/managed-otel-gke). Spans land in [Cloud Trace](https://console.cloud.google.com/traces) — no collector to deploy, no Deployment to maintain.

## How it's assembled

Three composable pieces:

1. **`../example`** — the plain overlay (base + agents ConfigMap + watcher cluster-name patch).
2. **[`../../components/otel`](../../components/otel/)** — one-env-var component that flips `pkg/telemetry`'s exporter from `none` to `otlp` via `OTEL_TRACES_EXPORTER`. That's the only core-agent-specific knob.
3. **[`instrumentation.yaml`](instrumentation.yaml)** — a GKE Managed OpenTelemetry `Instrumentation` CR. Empty selector, so it targets all Pods in the `agent-triage` namespace. GKE auto-injects a subset of standard OTel SDK env vars: `OTEL_EXPORTER_OTLP_ENDPOINT` (in-cluster managed collector), `OTEL_TRACES_EXPORTER`, `OTEL_METRIC_EXPORT_INTERVAL`, `K8S_POD_UID`, sampler config, and `OTEL_RESOURCE_ATTRIBUTES` with `k8s.pod.uid` (collector then attaches `k8s.namespace.name` etc. server-side). **`OTEL_SERVICE_NAME` is NOT auto-injected** — the component sets it explicitly on the daemon + watcher deployments.

Images are pinned to `2.7.0-dev.4`, the first release carrying the env override ([PR #315](https://github.com/go-steer/core-agent/pull/315)).

Off-GKE deployments use the same component with a different endpoint source — see [`components/otel/README.md` § Non-GKE](../../components/otel/README.md#non-gke-self-managed-collector-docker-etc).

## GKE prerequisites (one-time, cluster-wide)

Managed OpenTelemetry is a cluster-wide toggle, and the CR shipped by this overlay requires the CRD it installs. Run these once against the target cluster before applying:

    # 1. Enable the required Google Cloud APIs
    gcloud services enable \
      cloudtrace.googleapis.com \
      telemetry.googleapis.com \
      --project=<PROJECT>

    # 2. Enable managed OTel on the cluster. Provisions the
    #    opentelemetry-collector Deployment + Service in the
    #    `gke-managed-otel` namespace AND installs the
    #    `Instrumentation` CRD this overlay applies.
    gcloud container clusters update <CLUSTER> \
      --location=<REGION> \
      --managed-otel-scope=COLLECTION_AND_INSTRUMENTATION_COMPONENTS

    # 3. Grant Cloud Trace user role to the daemon Pod's identity.
    #    Default Compute Engine SA path:
    gcloud projects add-iam-policy-binding <PROJECT> \
      --member="serviceAccount:$(gcloud projects describe <PROJECT> \
        --format='value(projectNumber)')-compute@developer.gserviceaccount.com" \
      --role="roles/cloudtrace.user"

    # (Workload Identity: grant to the WI-bound Google SA the KSA
    # `core-agent` in namespace `agent-triage` impersonates instead.)

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

Trigger any tool call (kill a Pod to fire the watcher, or use `core-agent-cli` against the daemon), then open [Cloud Trace Explorer](https://console.cloud.google.com/traces), filter by service `core-agent`. Expected span tree (documented at [`docs/reference/otel.md`](../../../../../docs/site/content/docs/reference/otel.md)):

    mcp.tool_call
    ├── mcp.http_call
    └── digest.process
          └── subagent.llm_call    (agentic path only)

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
