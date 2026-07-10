# FailedMount

Kubelet can't mount a volume the pod requires. Blocks pod startup.

## Budget

- Max turns: 6
- Max wall time: 8 min

## Diagnose

1. Get the specific mount failure from events:
   `kubectl -n {namespace} describe pod {name}` → Events section. Look for `MountVolume.SetUp failed` or `Unable to attach or mount volumes`.

2. Extract the volume name and type from the pod spec:
   `kubectl -n {namespace} get pod {name} -o jsonpath='{.spec.volumes[*]}'`

3. Classify by volume type:
   - **PVC** (`persistentVolumeClaim`) — PV binding, storage class, provisioner, node zone affinity.
   - **Secret** — Secret exists in namespace? RBAC on the SA?
   - **ConfigMap** — ConfigMap exists in namespace?
   - **CSI** (custom driver) — driver installed? node ready?
   - **hostPath** — path exists on the node? Permissions?

## Common fixes

| Symptom | Fix | Verify (interval → check) |
|---|---|---|
| PVC in `Pending` (no PV bound) | Check the StorageClass exists: `kubectl get sc`. Confirm the provisioner is running. If GCE PD: check the zone matches the node's zone (`failure-domain.beta.kubernetes.io/zone` on the PVC vs. the node). | 2m → PVC transitions to `Bound`; pod mounts |
| Secret / ConfigMap missing | Create it: `kubectl -n {namespace} create secret ...` / `kubectl create configmap ...`. Or, if a namespace-scoped RBAC issue: verify the pod's SA has `get`/`list` on the resource. | 90s → pod mounts + `Running` |
| RWO PVC bound to another pod on a different node | The PVC has ReadWriteOnce access mode; only one node can attach it. If the prior pod is still running elsewhere: delete/reschedule it. If the node is unreachable: force-delete the pod on the dead node. | 3m → PVC re-attaches; new pod mounts |
| CSI driver not installed on the node | Install the driver (or move pod to a node pool where it is). This is infra work; may exceed budget. | Coordinate with platform team. |
| GKE + Filestore: subvolume permissions | Confirm the mounted path has UID/GID matching the pod's `fsGroup`. `kubectl exec` into a debug pod on the same volume and `ls -la`. | 3m → app can read/write; no `EACCES` in logs |
| PVC zone mismatch (regional pod, zonal PD) | Migrate the app to use a regional PD, or pin the pod to the PD's zone via node affinity. | 5m → pod schedules on correct zone + mounts |

## When to escalate

- CSI driver install requires cluster admin.
- Storage class doesn't exist and you can't create one (RBAC).
- PVC bound to a dead node whose kubelet won't respond (may need to force-detach at cloud-provider layer).
- Data safety concern (deleting a PV / PVC could lose data — never do this from triage).
