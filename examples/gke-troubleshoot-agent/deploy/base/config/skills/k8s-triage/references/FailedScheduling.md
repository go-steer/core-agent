# FailedScheduling

Scheduler couldn't place the pod on any node. Pod stays in `Pending`.

## Budget

- Max turns: 6
- Max wall time: 6 min

## Diagnose (read-only)

1. Get the scheduler's reason:
   `gke_list_k8s_events` scoped to `{namespace}` / `{name}` (or
   `gke_describe_k8s_resource` on the pod). The last `FailedScheduling`
   line names the constraint(s) the scheduler couldn't satisfy.

2. Common patterns in the message:
   - `Insufficient cpu` / `Insufficient memory` — no node has room.
   - `node(s) had untolerated taint {...}` — nodes are tainted and the pod lacks matching tolerations.
   - `node(s) didn't match Pod's node affinity/selector` — the pod's `nodeSelector` or `nodeAffinity` matches zero nodes.
   - `node(s) didn't have free ports` — hostPort conflict.
   - `pod has unbound immediate PersistentVolumeClaims` → chain to `references/FailedMount.md`.
   - `X nodes are available, Y filtered ... Z is not schedulable` — nodes cordoned or under pressure.

3. Get the current node situation:
   `gke_list_k8s_api_resources` (kind `Node`) for which are `Ready`, and
   `gke_describe_k8s_resource` on a candidate node for its allocatable vs
   allocated capacity. Also `gke_get_node_pool` / `gke_list_node_pools`
   for autoscaling bounds — a node pool already at `maxNodeCount` will
   never grow.

4. Get the pod's own constraints from `gke_get_k8s_resource` (kind `Pod`):
   `spec.nodeSelector`, `spec.affinity`, `spec.tolerations`,
   `spec.containers[].resources.requests`.

## Convergence check

Cluster Autoscaler adds a node in ~1–3 minutes when the pool has
headroom, so a `Pending` pod is often a wait, not a fault:

```
wait_and_verify(
  tool:            "gke_get_k8s_resource",
  args_json:       "{\"kind\": \"Pod\", \"namespace\": \"{namespace}\", \"name\": \"{name}\"}",
  expect_jq:       ".status.phase != \"Pending\"",
  interval_seconds: 20,
  timeout_seconds: 240
)
```

If the pool is already at its max (step 3), skip the wait — say why —
and go straight to a proposal.

## Remediation proposals

| Evidence | Proposed change | Verify (interval → check) |
|---|---|---|
| Insufficient CPU/memory across all nodes | Lower the pod's resource requests (name the current and proposed values), or raise the node pool's `maxNodeCount` / add capacity | 2m → pod schedules |
| Wrong `nodeSelector` (matches no nodes) | Fix the label selector on `<controller>` — quote a real node label from step 3 that the workload should target | 90s → pod schedules |
| Taint without toleration | Add the matching toleration to the pod spec, or remove the taint if it was set by mistake (say which, and why) | 90s → pod schedules |
| hostPort conflict | Change the hostPort, or move to a NodePort/LoadBalancer Service if hostPort isn't required | 90s → pod schedules |
| All nodes cordoned | Uncordon a healthy node and find out why they were cordoned (an upgrade in flight is a wait, not a fix) | 3m → pod schedules |
| PVC unbound | Load `references/FailedMount.md` and follow it | See FailedMount |

## When to escalate

- The node pool needs a bigger machine type (infra change).
- A custom scheduler is in use and is misconfigured.
- Cluster Autoscaler should be adding nodes and isn't — check `gke_list_k8s_events` in `kube-system` for cluster-autoscaler messages, then escalate with them.
- A namespace `ResourceQuota` is the blocker (`gke_get_k8s_resource`, kind `ResourceQuota`) — quota changes are a platform decision.
