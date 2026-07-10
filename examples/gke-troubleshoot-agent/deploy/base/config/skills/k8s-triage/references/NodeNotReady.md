# NodeNotReady

The node hosting this pod stopped reporting Ready. Pod-level Events
usually surface this via `NodeNotReady` when the pod's status is
affected. Node-level Events (`kubectl get events --field-selector
involvedObject.kind=Node`) are the source of truth.

## Budget

- Max turns: 4
- Max wall time: 5 min

## Diagnose

1. Get the node's status:
   `kubectl describe node {context.node}`
   Look at `Conditions` — `Ready` should be `True`. If `False` or `Unknown`, note the reason (`KubeletNotReady`, `NetworkUnavailable`, `KernelDeadlock`, `NodeStatusUnknown`).

2. Check when the node last reported:
   Look at `LastHeartbeatTime` in the Ready condition. If >5m ago, kubelet may be dead or unreachable.

3. Cluster-level: how many nodes are NotReady?
   `kubectl get nodes` — count NotReady. If more than one → cluster-wide issue.

4. On GKE, check Cloud Console for the underlying VM's status:
   `gcloud compute instances describe <node-name> --zone=<zone>` — is it TERMINATED, REPAIRING, running?

## Common fixes

| Symptom | Fix | Verify |
|---|---|---|
| Single node down; other nodes healthy | Cordon + drain the node (`kubectl drain {node} --ignore-daemonsets --delete-emptydir-data`) — kube-scheduler will reschedule affected pods. Then investigate/replace the node. | 3m → affected pods reschedule on other nodes; new pods Ready |
| GKE node auto-repair pending | Wait — GKE auto-repair kicks in for unhealthy nodes within 10m. Check with `gcloud container operations list`. Force-repair: `gcloud container clusters repair`. | 5m+ (out of budget; escalate) |
| Kubelet OOM on the node (system-reserved insufficient) | Cordon + drain + delete the VM (GKE recreates it). Long-term: raise system-reserved via node pool spec. | Coordinate with platform team. |
| Multi-node NotReady simultaneously | Cluster-wide event (rolling upgrade, control-plane issue, network outage). Escalate immediately — this isn't per-pod triage. | Escalate. |

## When to escalate

- Multi-node scope.
- Node auto-repair isn't kicking in.
- Suspected control-plane issue (would surface in Cloud Logging for GKE).
- Data-loss risk (pods with local storage on the failed node).
