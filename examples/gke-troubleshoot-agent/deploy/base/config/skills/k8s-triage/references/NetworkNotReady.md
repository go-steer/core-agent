# NetworkNotReady

Kubelet reports the pod's network isn't ready. Usually a CNI plugin
issue, node-level networking config, or (on GKE) a Dataplane V2 hiccup.

## Budget

- Max turns: 4
- Max wall time: 5 min

## Diagnose

1. Check the node's status:
   `kubectl describe node {context.node}` — look at `Conditions` (NetworkUnavailable?), `Addresses`, `System Info`.

2. Check the CNI pods on the node:
   `kubectl -n kube-system get pods -o wide --field-selector spec.nodeName={context.node}` — look for the CNI DaemonSet (calico-node, cilium-agent, or on GKE: `netd-*`, `anetd-*` for Dataplane V2).

3. If CNI pods are Ready but the pod's still NetworkNotReady, check the kubelet log on the node:
   `gcloud compute ssh {context.node} -- 'sudo journalctl -u kubelet -n 100'` (GKE-specific; other providers vary).

4. If this event fires cluster-wide (many pods affected), it's a cluster-level issue not a per-pod triage — escalate immediately.

## Common fixes

| Symptom | Fix | Verify |
|---|---|---|
| CNI pod on this node is CrashLooping | Restart it: `kubectl -n kube-system delete pod <cni-pod>` (DaemonSet will recreate) | 2m → new CNI pod Ready; NetworkNotReady clears |
| Node ran out of IPs in its pod CIDR | Migrate pod off the node (`kubectl drain --ignore-daemonsets`) and let the cluster reschedule. Longer-term: enlarge node's pod CIDR (GKE cluster-level) | 3m → pod moves + Ready |
| GKE Dataplane V2 upgrade in progress | Wait — GKE's rolling upgrade completes on its own. Verify via `gcloud container operations list --filter="TYPE=UPGRADE_MASTER"`. | 5m+ (out of budget usually; escalate) |
| CNI config file corrupted / missing on node | Cordon and drain the node; escalate to infra team for node replacement | Coordinate with platform team. |

## When to escalate

- Multi-node scope (cluster-level).
- CNI DaemonSet not running at all (cluster misconfig).
- Node is unhealthy (would need replacement — infra work).
