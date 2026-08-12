# Evicted

Kubelet evicted the pod. Usually node pressure (memory, disk, PIDs)
triggered by the node's soft/hard eviction thresholds.

## Budget

- Max turns: 5
- Max wall time: 6 min

## Diagnose (read-only)

1. Get the pod's status message — kubelet writes the reason there:
   `gke_get_k8s_resource` (kind `Pod`) → `status.reason` + `status.message`.
   Common: `Evicted` with `The node was low on resource: memory` or
   `... ephemeral-storage`.

2. Check the node's conditions:
   `gke_describe_k8s_resource` (kind `Node`, name `{context.node}`) —
   `MemoryPressure`, `DiskPressure`, `PIDPressure`.

3. See what else is on the node:
   `gke_list_k8s_api_resources` (kind `Pod`, filtered to
   `spec.nodeName={context.node}`) — is one workload the noisy neighbor?

4. Check the evicted pod's QoS class:
   `status.qosClass` from step 1.
   - `Guaranteed` — evicted only under extreme pressure.
   - `Burstable` — the common evictee.
   - `BestEffort` — first in the pecking order.

## Convergence check

An evicted pod's controller normally reschedules it within a minute.
The question is whether the replacement is stable:

```
wait_and_verify(
  tool:            "gke_get_k8s_resource",
  args_json:       "{\"kind\": \"Deployment\", \"namespace\": \"{namespace}\", \"name\": \"<controller>\"}",
  expect_jq:       ".status.readyReplicas == .spec.replicas",
  interval_seconds: 20,
  timeout_seconds: 180
)
```

`verified: true` → `RESOLVED`, but say in the summary that the node
pressure that caused the eviction is unchanged.

## Remediation proposals

| Evidence | Proposed change | Verify (interval → check) |
|---|---|---|
| BestEffort pod evicted under memory pressure | Add `resources.requests.memory` (and limits) to `<controller>`, moving the pod to Burstable QoS. Give the values | 3m → new pod schedules and stays `Ready` |
| Disk pressure on the node (image cache, ephemeral storage) | Reduce the workload's ephemeral-storage usage or move it to a node with more disk. On GKE, image-cache GC is automatic — don't propose pruning it | 3m → new pod stays `Ready` |
| A noisy neighbor is driving the pressure | Name the neighbor from step 3 and propose requests/limits on IT, so the scheduler stops co-locating | Coordinate with the neighbor's owner |
| Chronic eviction (same pod, several times a day) | Right-size via VPA recommendations, or move to a larger node pool. Include the eviction timestamps you found | 24h steady-state observation |
| The node is consistently under pressure | Cluster-level capacity issue; Cluster Autoscaler should be adding nodes | Escalate |

## When to escalate

- Chronic evictions across multiple pods on the same node (node under-provisioned).
- Cluster Autoscaler not adding capacity when it should.
- Suspected data loss on eviction (evicted pods with `emptyDir` carrying state).
