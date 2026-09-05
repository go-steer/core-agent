# OTel enablement components

Two components, one axis. `otel/` is the portable half — two env vars per
binary, nothing Google-specific. `../otel-gke/` is `otel/` plus the GKE
`Instrumentation` CR that makes those env vars mean something on a GKE cluster
with Managed OpenTelemetry turned on.

Unlike the rest of this recipe, tracing is **on by default**:
`set-up-demo.sh` probes the cluster for the `telemetry.googleapis.com`
CRD and applies the `-otel` overlay when it finds it. See
[Four overlays, two axes](#four-overlays-two-axes) below.

## What `otel/` does

Patches the daemon Deployment's `core-agent` container and the watcher
Deployment's `watcher` container — note that the watcher Deployment is named
`lookout-watch` and its container is not — with exactly two variables:

| Var | Value | Purpose |
|---|---|---|
| `OTEL_TRACES_EXPORTER` | `otlp` | Overrides `otel.exporter` from the packaged `config.json`, whose default is `none`. |
| `OTEL_SERVICE_NAME` | `core-agent` / `lookout-watch` | GKE Managed OTel's `Instrumentation` CR does **not** auto-inject this. Without it the daemon shows up in Cloud Trace as `unknown_service:<binary>`. lookout has defaulted its own spans to `service.name=lookout` since v0.21.0, so on the watcher this is an override rather than a fix — it makes the Cloud Trace service name match the Deployment name an operator would grep for. |

Everything else — endpoint, sampler, metric export interval, `k8s.*` resource
attributes — rides the standard OTel SDK env vars, injected by the CR on GKE
or set operator-side anywhere else.

`GOOGLE_CLOUD_PROJECT`, which Cloud Trace needs for the `gcp.project_id`
resource attribute, is already on the daemon via the base's `envFrom` reference
to the `core-agent-gcp-env` ConfigMap. The watcher has no such `envFrom` — see
[Watcher spans](#watcher-spans-a-known-open-question).

### Why env vars and not config

This recipe ships its `config.json` **inside the content OCI image**. Turning
tracing on there would mean rebuilding and re-pushing content and bumping
`CONTENT_TAG` in `prereqs.sh` — a content release to change a runtime knob.
`OTEL_TRACES_EXPORTER` beats the config file (`pkg/telemetry/otel.go`), so the
env-var route flips tracing with a redeploy and leaves the content image alone.
That is a stronger argument here than in recipes that mount config from a
ConfigMap.

## Four overlays, two axes

This recipe has two orthogonal deployment decisions, so it has 2 × 2 overlays:

| | tracing off | tracing on (default) |
|---|---|---|
| **image volume** (K8s ≥ 1.33) | `overlays/example` | `overlays/example-otel` |
| **initContainer copy** (older) | `overlays/initcontainer-copy` | `overlays/initcontainer-copy-otel` |

Content delivery is forced by the cluster's Kubernetes version; tracing is
forced by whether Managed OpenTelemetry is enabled. `set-up-demo.sh` probes for
both and picks one of the four. Override tracing with `OTEL=0` (off) or
`OTEL=1` (on, and fail loudly if the CRD is missing).

The `-otel` overlays are deliberately thin: they add
`../../components/otel-gke` over their delivery sibling and declare **no
`images:` block and no patches**. `set-up-demo.sh` writes the per-cluster pins
and values into the *delivery* overlay, and they flow up through the
composition. An outer `images:` block would run over the already-transformed
inner output and silently win, overriding whatever the operator configured in
`prereqs.sh`.

## Composing by hand

```yaml
resources:
  - ../example          # or ../initcontainer-copy
components:
  - ../../components/otel-gke
```

Off GKE, compose `../../components/otel` instead and supply the endpoint
yourself:

```yaml
resources:
  - ../example
components:
  - ../../components/otel
patches:
  - target: {kind: Deployment, name: core-agent}
    patch: |-
      - op: add
        path: /spec/template/spec/containers/1/env/-
        value: {name: OTEL_EXPORTER_OTLP_ENDPOINT, value: "http://otel-collector.observability.svc:4318"}
```

Mind the container index: the daemon Pod has an `install-users-json`
initContainer, but JSON-patch paths into `containers` are indexed separately
from `initContainers`, so `containers/0` is `core-agent`. Prefer a strategic
merge patch keyed by container *name* (which is what `otel/daemon-env.yaml`
does) over an index, exactly to avoid this.

## GKE prereqs

One-time, before the overlay applies:

    gcloud services enable cloudtrace.googleapis.com telemetry.googleapis.com \
      --project="${PROJECT_ID}"

    gcloud container clusters update "${CLUSTER_NAME}" --region="${REGION}" \
      --managed-otel-scope=COLLECTION_AND_INSTRUMENTATION_COMPONENTS \
      --project="${PROJECT_ID}"

    # BOTH service accounts — see "Bind both service accounts" below.
    for ksa in core-agent-daemon lookout-watch; do
      gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
        --role="roles/cloudtrace.user" \
        --member="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/gke-platform-agent/sa/${ksa}"
    done

Needs GKE control plane `1.34.1-gke.2178000` or later and gcloud `551.0.0` or
later. `set-up-demo.sh` prints these commands verbatim when it finds the CRD
missing, so you do not have to come back here.

The IAM member above is a Workload Identity Federation **direct binding** on
the KSA principal, matching how this recipe grants every other role — see
`deploy/base/10-serviceaccount-daemon.yaml`. There is no Google Service Account
to impersonate.

## Bind both service accounts

Two Deployments are instrumented, so two KSAs need `roles/cloudtrace.user`.
Granting only the daemon is the easy mistake, and it produces a trace that
looks complete: the turn, its tool calls, the subagent delegation — all present,
with only the watcher's inject span missing. Nothing errors. The watcher log
says nothing. You just get a story that starts one step too late.

The watcher is the one that gets forgotten because, unlike the daemon, it holds
no other GCP role — there is no existing binding to amend and nothing else
breaks to tip you off. `deploy/base/11-serviceaccount-watcher.yaml` says so at
the point where someone would go looking, and `set-up-demo.sh` checks both
principals on the tracing path and prints the missing command.

This is the house pattern, not a quirk of this recipe: `gke-troubleshoot-agent`'s
`scripts/setup-wif.sh` binds its watcher the same way.

`roles/cloudtrace.user` remains the *only* GCP role the watcher KSA needs.
Everything else it does — reading the k8s API, posting to the daemon over pod
networking with a bearer token — uses no cloud identity, and that is worth
preserving.

## The Instrumentation CR's namespace

`../otel-gke/instrumentation.yaml` spells out `namespace:
gke-platform-agent` rather than inheriting it. The base uses a
`NamespaceTransformer` with `unsetOnly: true` (required so the kube-system
capacity Role/RoleBinding are not clobbered), and resources contributed by a
component at overlay level are not reached by it. An unnamespaced CR would land
in `default` and match nothing.

## Sampling

Defaults to `AlwaysOn` — every span exported, which is what you want for a demo
you are reading traces from. Dial down on the `Instrumentation` CR:

    OTEL_TRACES_SAMPLER=parentbased_traceidratio
    OTEL_TRACES_SAMPLER_ARG=0.05     # 5%

## Applying to a running deployment

Switching a running namespace from a plain overlay to its `-otel` sibling
changes the Pod template — two new env vars — so `kubectl apply -k` rolls both
Deployments on its own. No explicit restart needed for that part.

The part that can still miss is the *injection*, and on a first-time enable it
usually does. GKE's webhook stamps `OTEL_EXPORTER_OTLP_ENDPOINT` and friends at
Pod admission, and one `apply -k` submits the `Instrumentation` CR and the
Deployment update together — so the new Pods get admitted before GKE has
reconciled the CR. The symptom is a Pod carrying this component's two vars and
none of the injected ones, and a daemon log line reading:

    core-agent: telemetry: OTLP HTTP exporter (via ADK) → (default localhost:4318)

Observed on the 2026-08-19 deploy, so treat it as the expected first-run
behavior rather than an edge case. A restart fixes it permanently — the CR
exists by then:

    kubectl rollout restart deployment/core-agent deployment/lookout-watch \
      -n gke-platform-agent

`set-up-demo.sh` detects this and **does the restart for you**, then re-checks.
If the endpoint is still absent afterwards it is not the race — the CR exists
and nothing is acting on it, which means the cluster's `--managed-otel-scope`
covers collection but not `INSTRUMENTATION_COMPONENTS`. See
[Verifying](#verifying).

## Verifying

`set-up-demo.sh` checks, on the tracing path, that
`OTEL_EXPORTER_OTLP_ENDPOINT` is present on the daemon container, restarts once
if it is not, and re-checks. That variable is injected by GKE's webhook, not by
these manifests, so its presence is the real signal that the CR took effect.
Absent, the two env vars this component sets are still there and the SDK falls
back to `localhost:4318` — spans go nowhere, quietly.

The cheapest independent confirmation is the daemon's own boot line, which
prints the resolved endpoint:

    kubectl -n gke-platform-agent logs deploy/core-agent | grep telemetry
    core-agent: telemetry: OTLP HTTP exporter (via ADK) → http://opentelemetry-collector.gke-managed-otel.svc.cluster.local:4318

lookout prints the equivalent. Either showing `(default localhost:4318)` means
tracing is not working no matter what the manifests say.

Then look in Cloud Trace. Nothing emits spans until something exercises the
daemon — an empty trace list right after deploy is expected, not a failure.

**Do not query by service name.** The Cloud Trace v1 API's `+service_name:`
filter keys off the legacy per-trace service concept, not the OTel `service.name`
resource attribute, so `+service_name:core-agent` returns zero traces on a
cluster that is exporting perfectly. Filter on a resource attribute the OTel SDK
actually sets:

    filter=k8s.namespace.name:gke-platform-agent

Verified against this cluster on 2026-08-19: `+service_name:` → 0 traces,
`k8s.namespace.name:` → 532. The spans themselves carry the right
`service.name` (`core-agent` / `lookout-watch`); it is only that filter that
cannot see it.

### What a healthy incident looks like

Two traces, and both matter:

1. **The delivery hop** — two spans across both services: lookout-watch's
   `HTTP POST` client span parenting core-agent's
   `POST /sessions/<sid>/inject`. This is the proof that trace context crosses
   the watcher → daemon boundary, and it is the span you lose if only the
   daemon KSA holds `roles/cloudtrace.user`.
2. **The turn** — rooted at `invoke_agent core_agent`, ~60 spans, containing
   `generate_content <model>` (with `gen_ai.usage.*` token counts and
   `cache_read.input_tokens`), `execute_tool record_plan`,
   `execute_tool spawn_agent`, a nested `invoke_agent cluster-1` for the
   delegated subagent, its `execute_tool gke_*` calls, and
   `execute_tool return_result`.

Each `gke_*` tool call produces a pair: core-agent's `mcp.tool_call` and, beneath
its HTTP client span, a `tools/call <name>` span emitted by Google's managed GKE
MCP service (`gcp.server.service: container.googleapis.com`). Those remote spans
show `service.name` as unset — that is Google's service not setting it, not a
gap in this recipe. Seeing them at all means the trace reaches past the cluster.

### Trace-list noise

With the default `AlwaysOn` sampler, an attached TUI dominates the trace list:
the background-agent SSE stream reconnects roughly every 18s and each
reconnect is its own root trace (`GET /sessions/<sid>/agents/<branch>/events`).
On the 2026-08-19 run that was 481 of 532 traces. Harmless, but if you are
demoing from the Cloud Trace console, filter by span name or detach the TUI
first rather than reaching for a sampler — dialling sampling down would thin the
incident traces you came to look at along with the noise.
