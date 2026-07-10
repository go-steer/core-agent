# Evicted

Kubelet evicted the pod. Usually node pressure (memory, disk, PIDs)
triggered by the node's soft/hard eviction thresholds.

## Budget

- Max turns: 5
- Max wall time: 6 min

## Diagnose

1. Get the pod's status message (kubelet writes the reason here):
   `kubectl -n {namespace} get pod {name} -o jsonpath='{.status.reason} {.status.message}'`
   Common values: `Evicted` + a message like `The node was low on resource: memory` or `... disk-pressure`.

2. Check the node's conditions:
   `kubectl describe node {context.node}` — look at `Conditions` for `MemoryPressure`, `DiskPressure`, `PIDPressure`.

3. Check what else is on the node:
   `kubectl -n {namespace} get pods --all-namespaces -o wide --field-selector spec.nodeName={context.node}` — is one pod the noisy neighbor?

4. Check the evicted pod's QoS class:
   `kubectl -n {namespace} get pod {name} -o jsonpath='{.status.qosClass}'`
   - `Guaranteed` — evicted only under extreme pressure.
   - `Burstable` — the common evictee.
   - `BestEffort` — first in the pecking order.

## Common fixes

| Symptom | Fix | Verify |
|---|---|---|
| BestEffort pod evicted under memory pressure | Add resource requests to the pod's controller (`kubectl edit deployment <controller>` → `resources.requests.memory`); moves the pod to Burstable QoS. Longer-term: set limits too. | 3m → new pod schedules + stays Ready |
| Disk pressure on node (image cache, ephemeral storage) | Prune node's image cache (GKE auto-manages; other providers may need manual). Or move pod to a node with more disk. | 3m → new pod stays Ready |
| Noisy neighbor pod evicting yours | Set proper resource requests on the noisy neighbor so scheduler avoids co-location. Requires knowing its workload. | Coordinate with the neighbor's owner. |
| Chronic eviction (same pod, multiple times/day) | Vertical Pod Autoscaler + right-sizing, OR migrate to a bigger node pool. | 24h steady-state observation. |
| Node consistently under pressure | Cluster-level capacity issue. Cluster autoscaler should add nodes; if it doesn't, escalate. | Escalate. |

## When to escalate

- Chronic evictions across multiple pods on the same node (node under-provisioned).
- Cluster autoscaler not adding capacity when it should.
- Suspected data loss on eviction (evicted pods with emptyDir carrying important state).
