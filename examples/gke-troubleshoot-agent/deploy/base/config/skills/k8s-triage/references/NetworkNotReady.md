# NetworkNotReady

Kubelet reports the pod's network isn't ready. Usually a CNI plugin
issue, node-level networking config, or (on GKE) a Dataplane V2 hiccup.

## Budget

- Max turns: 4
- Max wall time: 5 min

## Diagnose (read-only)

1. Check the node:
   `gke_describe_k8s_resource` (kind `Node`, name `{context.node}`) —
   `Conditions.NetworkUnavailable`, `Addresses`, `System Info`.

2. Check the CNI pods on that node:
   `gke_list_k8s_api_resources` (kind `Pod`, namespace `kube-system`,
   filtered to `spec.nodeName={context.node}`) — look for the CNI
   DaemonSet (`calico-node`, `cilium-agent`, or on GKE `netd-*` /
   `anetd-*` for Dataplane V2). If one is not `Ready`, read its logs with
   `gke_get_k8s_logs`.

3. You cannot reach the node's kubelet journal — no shell, no SSH. If
   the CNI pods look healthy and the pod still isn't, say the node-level
   cause is unverifiable from here and escalate with what you have.

4. Blast radius: `gke_list_k8s_events` cluster-wide for the same reason.
   Many pods affected → cluster-level issue, escalate immediately.

5. Is the platform already changing something?
   `gke_list_operations` — a Dataplane V2 or node-pool upgrade in flight
   explains a transient NetworkNotReady.

## Convergence check

CNI restarts and rolling upgrades clear on their own; this reason is
transient more often than not:

```
wait_and_verify(
  tool:            "gke_get_k8s_resource",
  args_json:       "{\"kind\": \"Pod\", \"namespace\": \"{namespace}\", \"name\": \"{name}\"}",
  expect_jq:       ".status.phase == \"Running\"",
  interval_seconds: 20,
  timeout_seconds: 180
)
```

## Remediation proposals

| Evidence | Proposed change | Verify (interval → check) |
|---|---|---|
| The CNI pod on this node is CrashLooping | Delete it so the DaemonSet recreates it; include the CNI pod's name and the log lines that show the crash | 2m → new CNI pod `Ready`; NetworkNotReady clears |
| Node ran out of IPs in its pod CIDR | Drain the node so the workload reschedules; long-term, enlarge the cluster's pod CIDR (a cluster-level change) | 3m → pod moves and reaches `Ready` |
| GKE Dataplane V2 upgrade in progress (from step 5) | Nothing to apply — wait for the operation. Report its ID | 5m+ (usually beyond budget) |
| CNI config missing/corrupt on the node | Cordon and drain; the node needs replacement. Infra work | Coordinate with platform team |

## When to escalate

- Multi-node scope (cluster-level).
- CNI DaemonSet not running at all (cluster misconfiguration).
- The node is unhealthy and needs replacement.
