# FailedScheduling

Scheduler couldn't place the pod on any node. Pod stays in `Pending`.

## Budget

- Max turns: 6
- Max wall time: 6 min

## Diagnose

1. Get the scheduler's reason:
   `kubectl -n {namespace} describe pod {name}` → Events section. Look for the last `FailedScheduling` line — it names the constraint(s) the scheduler couldn't satisfy.

2. Common patterns in the message:
   - `Insufficient cpu` / `Insufficient memory` — no node has room.
   - `node(s) had untolerated taint {...}` — nodes are tainted and pod lacks matching tolerations.
   - `node(s) didn't match Pod's node affinity/selector` — pod's `nodeSelector` or `nodeAffinity` matches zero nodes.
   - `node(s) didn't have free ports` — hostPort conflict.
   - `pod has unbound immediate PersistentVolumeClaims` → chain to `references/FailedMount.md` (PVC not bound).
   - `X nodes are available, Y filtered ... Z is not schedulable` — nodes cordoned or under pressure.

3. Get the current node situation:
   `kubectl get nodes -o wide` (which ones are Ready?)
   `kubectl top nodes` (which have headroom?)

4. Get the pod's spec constraints:
   `kubectl -n {namespace} get pod {name} -o jsonpath='{.spec.nodeSelector}{.spec.nodeAffinity}{.spec.tolerations}{.spec.containers[*].resources.requests}'`

## Common fixes

| Symptom | Fix | Verify (interval → check) |
|---|---|---|
| Insufficient CPU/memory across all nodes | Reduce pod's resource requests (if it can tolerate less) OR scale up node pool (`gcloud container clusters resize` or Cluster Autoscaler if enabled) | 2m → pod schedules |
| Wrong nodeSelector (matches no nodes) | Fix the label selector — usually a typo or a stale label name. `kubectl edit deployment <controller>` | 90s → pod schedules |
| Taint without toleration | Add matching toleration to the pod OR remove the taint from nodes if it was set by mistake | 90s → pod schedules |
| hostPort conflict | Change the hostPort, or move to a NodePort/LoadBalancer Service if hostPort isn't required | 90s → pod schedules |
| Node pool at zero capacity (all cordoned) | `kubectl uncordon <node>` for a healthy one; investigate why they were cordoned | 3m → pod schedules |
| PVC unbound (chain to FailedMount) | Load `references/FailedMount.md` and follow that. | See FailedMount. |

## When to escalate

- Node pool needs vertical scale (bigger machine type) — infra change.
- Custom scheduler in use and it's misconfigured.
- Cluster autoscaler is stuck (would need to add nodes but isn't) — check `kubectl get events -n kube-system` for cluster-autoscaler messages; may need platform team.
- Pod's constraints are correct but there's a resource-quota block at the namespace level (`kubectl -n {namespace} describe resourcequota`).
