---
name: gke-reliability
description: Assess the availability posture of a GKE workload — disruption budgets, health probes, zone spreading, graceful shutdown, maintenance windows. Read-only; produces findings, the evidence for them, and proposed manifest changes the parent applies.
---

# GKE reliability

This is a **method**, not a mission. It tells you *how* to assess whether the
workload you were asked about survives the disruptions it will actually meet:
node upgrades, preemption, zone loss, and its own restarts. It never tells you
*what* to assess or *where* — that came with your goal.

Reach for this skill when an incident's real cause is that a *routine* event was
not survivable — a drain during an upgrade, a Spot reclaim, a rollout that never
became ready.

## Reads — there is no shell

Everything here is read-only, through the `gke` MCP. There is **no** `kubectl` and
**no** `gcloud`; you cannot apply a manifest or change a cluster setting. Where
this skill says "propose", it means *write it into your report* for the platform
agent to act on.

| What you need | Read |
|---|---|
| Regionality, maintenance policy, release channel | `gke_get_cluster` |
| Node-pool zones, Spot/preemptible, autorepair | `gke_get_node_pool`, `gke_list_node_pools` |
| Workload spec: probes, spread, grace period | `gke_get_k8s_resource` (`outputFormat: "YAML"`) |
| Disruption budgets in the namespace | `gke_get_k8s_resource` on `poddisruptionbudget` |
| Rollout progress | `gke_get_k8s_rollout_status` |
| Evictions, drains, upgrade activity | `gke_list_k8s_events`, `gke_list_operations` |

**Never invent a tool name.** If a check needs a capability you do not have,
report it as unavailable.

## Step 1 — what the cluster survives

From `gke_get_cluster`:

- `location` is a region (`us-central1`) → regional control plane; a zone
  (`us-central1-a`) → the control plane is a single point of failure.
- `locations` with several entries → nodes span zones. One entry means a zone
  outage takes the whole workload, however many replicas it has.
- `maintenancePolicy` — absent means GKE may upgrade nodes at peak hours.
- `releaseChannel` — governs how upgrades arrive at all.

Per node pool: the zones it spans, `config.spot` / `config.preemptible` (nodes
reclaimed with ~30 seconds' notice), and `management.autoRepair`.

## Step 2 — what the workload survives

- **Disruption budget.** No PDB covering these pods means a drain can evict every
  replica at once. Also check the inverse: a PDB with `minAvailable` equal to the
  replica count blocks drains entirely and stalls node upgrades — a real and
  under-reported failure mode.
- **Probes.** Read every container. No readiness probe means traffic arrives
  before the app can serve it; no liveness probe means a wedged process is never
  restarted. A liveness probe that is too aggressive — short `initialDelaySeconds`
  on a slow starter — causes restart loops on its own, and a startup probe is the
  fix rather than a longer liveness delay.
- **Zone spread.** `topologySpreadConstraints` with
  `topologyKey: topology.kubernetes.io/zone`, or their absence. Without them the
  scheduler may place every replica in one zone even on a regional cluster.
  `DoNotSchedule` enforces the spread and can leave pods `Pending`;
  `ScheduleAnyway` degrades gracefully. Which is right depends on the workload —
  say which you mean and why.
- **Graceful shutdown.** `terminationGracePeriodSeconds` (30s default) against
  what the application actually needs to drain, plus a `preStop` hook where
  connection draining matters.
- **Replica count and anti-affinity.** A single replica has no availability story
  at all; that is the finding, and no PDB can rescue it.

## Step 3 — was this incident a disruption

Correlate before concluding. `gke_list_k8s_events` for `Evicted`,
`Preempted`, `NodeNotReady`, or drain messages, and `gke_list_operations` for an
upgrade or repair overlapping the incident window. A workload that "failed
randomly" during a node upgrade did not fail randomly.

## Step 4 — report and propose

You are **read-only by construction**. You do not apply manifests or change
cluster settings.

Report: **the disruption that is not survived**, **the field or event that
establishes it**, and **the concrete change**. Order by what would actually have
prevented *this* incident — a generic reliability checklist is worth much less
than "this was a Spot reclaim, and one replica with no PDB is why it became an
outage."

Manifest proposals — hand back the YAML:

- A `PodDisruptionBudget` (`policy/v1`) with `minAvailable` set below the replica
  count, selecting this workload's labels.
- Readiness/liveness/startup probes on containers that lack them, with the delays
  the app's own startup time justifies.
- `topologySpreadConstraints` with `maxSkew: 1` across
  `topology.kubernetes.io/zone`.
- A longer `terminationGracePeriodSeconds`, or a `preStop` hook.

Cluster-level proposals are settings the platform agent applies through the
cluster-update path or IaC: a maintenance window or exclusion covering peak
hours, additional node-pool zones, or a non-Spot pool for workloads that cannot
absorb reclaims.

Say what each change costs — more replicas and more zones cost money, and a tight
PDB trades upgrade velocity for availability. Do not end with a bare status line
like "reviewed reliability."
