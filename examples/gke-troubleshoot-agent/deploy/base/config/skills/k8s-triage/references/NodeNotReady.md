# NodeNotReady

The node hosting this pod stopped reporting Ready. Pod-level Events
usually surface this via `NodeNotReady`; node-level Events are the
source of truth (`gke_list_k8s_events` filtered to the Node object).

## Budget

- Max turns: 4
- Max wall time: 5 min

## Diagnose (read-only)

1. Get the node's status:
   `gke_describe_k8s_resource` (kind `Node`, name `{context.node}`).
   `Conditions.Ready` should be `True`. If `False` or `Unknown`, note the
   reason (`KubeletNotReady`, `NetworkUnavailable`, `KernelDeadlock`,
   `NodeStatusUnknown`).

2. Check when the node last reported: `LastHeartbeatTime` on the Ready
   condition. More than ~5m ago means kubelet is dead or unreachable.

3. Cluster-level blast radius:
   `gke_list_k8s_api_resources` (kind `Node`) — how many are NotReady?
   More than one → cluster-wide issue, escalate immediately.

4. Is GKE already acting on it?
   `gke_list_operations` / `gke_get_operation` for the cluster — a node
   auto-repair or an upgrade in flight shows up here, and changes the
   answer from "propose a fix" to "wait for the platform".

## Convergence check

Node auto-repair and transient kubelet restarts recover on their own,
usually well inside ten minutes. Watch the pod, not the node — a
reschedule is a good outcome too:

```
wait_and_verify(
  tool:            "gke_get_k8s_resource",
  args_json:       "{\"kind\": \"Pod\", \"namespace\": \"{namespace}\", \"name\": \"{name}\"}",
  expect_jq:       ".status.phase == \"Running\"",
  interval_seconds: 30,
  timeout_seconds: 300
)
```

## Remediation proposals

| Evidence | Proposed change | Verify (interval → check) |
|---|---|---|
| Single node down; the rest healthy | Cordon and drain `{context.node}` (`--ignore-daemonsets`) so the scheduler moves the affected pods, then replace the node | 3m → affected pods reschedule and reach `Ready` |
| GKE node auto-repair already in flight (visible in step 4) | Nothing to apply — wait. Report the operation ID and its ETA | 5m+ (usually beyond this budget) |
| Kubelet OOM on the node (system-reserved too small) | Cordon, drain, delete the VM so GKE recreates it. Long-term: raise `system-reserved` in the node pool config | Coordinate with platform team |
| Multiple nodes NotReady at once | Cluster-wide event (rolling upgrade, control-plane issue, network outage). Not per-pod triage | Escalate immediately |

## When to escalate

- Multi-node scope.
- Node auto-repair isn't kicking in and the node has been down for more than ten minutes.
- Suspected control-plane issue (surfaces in Cloud Logging for GKE, which you cannot read from here).
- Data-loss risk (pods with local storage on the failed node).
