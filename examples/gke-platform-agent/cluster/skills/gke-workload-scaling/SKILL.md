---
name: gke-workload-scaling
description: Diagnose scaling behavior of a GKE workload — replica counts, HPA and VPA configuration, resource requests, cluster autoscaler headroom. Read-only; produces a finding, the evidence for it, and a proposed scaling change the parent applies.
---

# GKE workload scaling

This is a **method**, not a mission. It tells you *how* to work out why a
workload is not scaling the way it should. It never tells you *what* to look at
or *where* — that came with your goal, and your goal outranks everything in this
file.

Two very different questions land here. Establish which one you were asked before
reading anything:

1. **It is not scaling** — replicas are pinned, or an autoscaler exists and is not
   acting.
2. **It scaled and that made things worse** — thrash, eviction churn, or pods that
   scale up and cannot be placed.

## Reads — there is no shell

Everything here is read-only, through the `gke` MCP. There is **no** `kubectl` and
**no** `gcloud`: you cannot scale, autoscale, or apply anything. Where this skill
says "propose", it means *write it into your report* for the platform agent to
act on.

| What you need | Read |
|---|---|
| Deployment replicas + resource requests | `gke_get_k8s_resource` (`outputFormat: "YAML"`) |
| HPA state (current/desired, conditions) | `gke_describe_k8s_resource` on `horizontalpodautoscaler` |
| VPA recommendations and mode | `gke_describe_k8s_resource` on `verticalpodautoscaler` |
| Cluster-level VPA, autoscaling profile | `gke_get_cluster` |
| Node-pool autoscaling bounds | `gke_get_node_pool`, `gke_list_node_pools` |
| Scale-up failures, evictions, `FailedScheduling` | `gke_list_k8s_events` |

**Never invent a tool name.** You cannot query a metrics backend; you read the
autoscaler's own reported state and reason from that.

## Step 1 — read the workload before the autoscaler

`spec.replicas`, and `resources.requests` on every container.

**Missing CPU/memory requests is the single most common cause of "HPA does not
work."** A utilization-target HPA computes a percentage of the request; with no
request there is no denominator, the HPA reports `unknown` for the metric, and it
never acts. If requests are absent, you have the finding — stop and report it.

## Step 2 — read the autoscaler's own account of itself

Describe the HPA and read its `status.conditions` and `status.currentMetrics`
rather than inferring from replica counts:

- `AbleToScale: False` — usually the stabilization window (5 minutes by default),
  or a `scaleTargetRef` that names a workload that does not exist.
- `ScalingActive: False` — the metric cannot be read: no request set, metrics
  server unavailable, or a custom/external metric with no collector behind it.
- `ScalingLimited: True` — it wants more replicas than `maxReplicas` allows. The
  autoscaler is working; the ceiling is the problem.
- **At `minReplicas` with load** or **at `maxReplicas` continuously** — both are
  configuration findings, not failures.

For VPA, read `status.recommendation` against the current requests, and the
update mode: `Off` recommends only, `Initial` applies at pod creation, `Auto`
evicts to resize, `InPlaceOrRecreate` resizes in place where the cluster version
supports it. A VPA in `Off` mode is a very common "why did nothing change."

**HPA and VPA on the same resource thrash each other.** If both target CPU, that
is your answer.

## Step 3 — if pods scaled but did not run

Scale-up that produces `Pending` pods is a placement problem, not a scaling one.
Read the events: `Insufficient cpu`/`memory` means the request does not fit any
node; a taint or affinity message means it fits nowhere it is allowed. Check the
node pool's `autoscaling` bounds — a `maxNodeCount` already reached caps the
workload no matter what the HPA wants. On Autopilot there are no node pools to
read; placement is the platform's problem and the request shape is yours.

## Step 4 — report and propose

You are **read-only by construction**. You do not scale, enable, or apply
anything; the platform agent owns the change.

Report: **the constraint**, **the field or condition that establishes it**, and
**the concrete change** — with the number you are proposing and why that number.
"Raise `maxReplicas`" is not a proposal; "raise `maxReplicas` from 5 to 12, since
the HPA has been `ScalingLimited` at 5 with CPU at 91% against a 50% target" is.

Workload-level fixes are manifests — hand back the YAML:

- Explicit `resources.requests` on every container, which is the prerequisite for
  everything else here.
- An HPA — `assets/hpa-example.yaml` is the template; fill in the target,
  metric, and bounds for this workload.
- A VPA — `assets/vpa-example.yaml`; propose `Off` mode first when the goal is
  right-sizing evidence rather than automatic change, and note that `Auto` evicts
  pods and needs a PDB and graceful `SIGTERM` handling.
- A fixed `spec.replicas` change, when the honest answer is that this workload
  should not autoscale.

Cluster-level fixes are settings for the platform agent to apply through the
cluster-update path or IaC: enabling VPA on a Standard cluster
(`verticalPodAutoscaling.enabled`), or widening a node pool's autoscaling
`minNodeCount`/`maxNodeCount`.

Say what the change will cost — added replicas consume quota, VPA `Auto` restarts
pods, and a wider node pool raises the bill. Do not end with a bare status line
like "reviewed scaling."
