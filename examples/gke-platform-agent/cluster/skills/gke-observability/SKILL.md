---
name: gke-observability
description: Audit whether a GKE cluster and its workloads are observable — logging, metrics, managed Prometheus, control-plane metrics, tracing. Read-only; produces findings, the evidence for them, and proposed changes the parent applies.
---

# GKE observability

This is a **method**, not a mission. It tells you *how* to assess the
observability of the cluster and workload you were asked about. It never tells
you *what* to assess or *where* — that came with your goal, and your goal
outranks everything in this file.

Reach for this skill when the investigation stalls because the telemetry that
would answer it does not exist. "The signal you need was never collected" is a
legitimate root cause, and reporting it is more useful than a guess.

## Reads — there is no shell

Everything here is read-only, through the `gke` MCP. There is **no** `kubectl`
and **no** `gcloud`; you cannot enable a service, and you cannot run a Cloud
Logging query. Where this skill says "propose", it means *write it into your
report* for the platform agent to act on.

| What you need | Read |
|---|---|
| Logging / monitoring / managed-Prometheus config | `gke_get_cluster` |
| Collector pods (e.g. `gmp-system`) | `gke_get_k8s_resource` |
| Workload log output | `gke_get_k8s_logs` |
| Collector or scrape failures | `gke_list_k8s_events` |

**Never invent a tool name.** You have no Cloud Monitoring or Cloud Logging query
tool: you can read a workload's logs through `gke_get_k8s_logs`, but you cannot
run an LQL search or evaluate a metric. If a check needs one, report it as
unavailable and say what you would have queried.

## Step 1 — is the cluster collecting anything

From `gke_get_cluster`:

- `loggingConfig.componentConfig.enableComponents` — `SYSTEM_COMPONENTS` and
  `WORKLOADS`. Workload logging off means application stdout never reaches Cloud
  Logging, and nobody has noticed.
- `monitoringConfig.componentConfig.enableComponents` — `SYSTEM_COMPONENTS`, plus
  `APISERVER` / `SCHEDULER` / `CONTROLLER_MANAGER` on Standard clusters. Missing
  control-plane components means API-server latency and scheduler pressure are
  invisible.
- `monitoringConfig.managedPrometheusConfig.enabled` — off means custom
  application metrics are not being collected at all, which also disables any HPA
  that scales on them.
- `monitoringConfig.advancedDatapathObservabilityConfig` — Dataplane V2 flow
  visibility, the only in-cluster source for L4 connectivity evidence.

An empty or `NONE` component list is the finding; do not soften it.

## Step 2 — is the workload observable

- **Is it logging at all?** `gke_get_k8s_logs` on the workload. Nothing on stdout
  means Cloud Logging has nothing to collect, however the cluster is configured.
- **Is the output structured?** Free-text lines cannot be filtered by field, so
  an incident is a text search. JSON on stdout with `severity` is the fix, and it
  is an application change, not a cluster one — say so.
- **Are metrics exposed?** Look for a scrape annotation or a `PodMonitoring`
  resource. An application that exposes no metrics endpoint cannot be scraped no
  matter what is enabled cluster-side.
- **Is the collector healthy?** If managed Prometheus is on, read the
  `gmp-system` pods and the namespace events; a crash-looping collector looks
  identical to "no metrics" from the outside, and the distinction changes the fix.

## Step 3 — report and propose

You are **read-only by construction**. You cannot enable telemetry; you report
what is missing and what to turn on.

Structure the report as: **the gap**, **the field or read that establishes it**,
and **the specific change**. Tie each gap to the investigation that hit it —
"there is no request-latency signal, which is why the latency question could not
be answered" is worth far more than a generic checklist.

Cluster-side proposals are settings, applied by the platform agent through the
cluster-update path or the cluster's IaC:

| Gap | Setting to propose |
|---|---|
| Workload logs not collected | add `WORKLOADS` to `loggingConfig` components |
| Control plane blind (Standard) | add `APISERVER`, `SCHEDULER`, `CONTROLLER_MANAGER` to `monitoringConfig` |
| No custom metrics | enable managed Prometheus |
| No L4 flow visibility (Dataplane V2) | enable advanced datapath observability |

Application-side proposals: structured JSON logging on stdout; an OpenTelemetry
exporter to Cloud Trace for cross-service latency; the Cloud Profiler agent for
CPU and allocation hotspots. Name the signal each one would have produced for
*this* incident.

Where a dashboard or alerting policy is the right answer, propose the concrete
query the operator should use — for example, `resource.type="k8s_container"` with
`resource.labels.container_name` and `severity>=ERROR` — and say plainly that you
could not run it yourself.

Do not end with a bare status line like "reviewed observability."
