# FailedMount

Kubelet can't mount a volume the pod requires. Blocks pod startup.

## Budget

- Max turns: 6
- Max wall time: 8 min

## Diagnose (read-only)

1. Get the specific mount failure from events:
   `gke_list_k8s_events` scoped to `{namespace}` / `{name}` (or
   `gke_describe_k8s_resource` on the pod). Look for
   `MountVolume.SetUp failed` or `Unable to attach or mount volumes`.

2. Extract the volume name and type from the pod spec:
   `gke_get_k8s_resource` (kind `Pod`) → `spec.volumes[]`.

3. Classify by volume type, and read the referenced object directly —
   `gke_get_k8s_resource` works for PVC, Secret, ConfigMap and
   StorageClass too:
   - **PVC** (`persistentVolumeClaim`) — is it `Bound`? Which StorageClass? Which zone?
   - **Secret** — does it exist in `{namespace}`?
   - **ConfigMap** — does it exist in `{namespace}`?
   - **CSI** (custom driver) — is the driver's DaemonSet running on `{context.node}`?
   - **hostPath** — you cannot inspect node filesystems from here; report it as unverifiable.

## Convergence check

Attach/detach races (RWO volume still attached to a terminating pod)
resolve on their own within a couple of minutes:

```
wait_and_verify(
  tool:            "gke_get_k8s_resource",
  args_json:       "{\"kind\": \"Pod\", \"namespace\": \"{namespace}\", \"name\": \"{name}\"}",
  expect_jq:       ".status.phase == \"Running\"",
  interval_seconds: 20,
  timeout_seconds: 180
)
```

If the PVC itself is the blocked object, poll it instead
(`expect_contains: "Bound"`) — a `Pending` PVC that binds is the real
signal.

## Remediation proposals

| Evidence | Proposed change | Verify (interval → check) |
|---|---|---|
| PVC `Pending` (no PV bound) | Name the missing piece: StorageClass absent, provisioner not running, or a zone mismatch between the PVC's zone label and the node's. The change is on the PVC/StorageClass, not the pod | 2m → PVC `Bound`; pod mounts |
| Secret / ConfigMap missing | Create the named object in `{namespace}` (say which keys the pod expects), or fix the reference if the name is a typo | 90s → pod mounts + `Running` |
| RWO PVC attached to another pod on a different node | Only one node can attach a ReadWriteOnce PVC. Name the holding pod and propose deleting/rescheduling it; if its node is unreachable, that's a force-detach a human must do | 3m → PVC re-attaches; new pod mounts |
| CSI driver not installed on the node | Install the driver, or move the workload to a node pool that has it. Infra work | Coordinate with platform team |
| GKE + Filestore: subvolume permissions | The mounted path's UID/GID must match the pod's `fsGroup`. You cannot exec into the volume from here — propose the `fsGroup` you read from the spec and ask for confirmation | 3m → app reads/writes; no `EACCES` in logs |
| PVC zone mismatch (regional workload, zonal PD) | Move to a regional PD, or pin the pod to the PD's zone via node affinity | 5m → pod schedules in the right zone + mounts |

## When to escalate

- CSI driver install requires cluster admin.
- StorageClass doesn't exist and creating one is a platform decision.
- PVC bound to a dead node whose kubelet won't respond (may need a cloud-provider force-detach).
- Any proposal that would delete a PV or PVC. Never propose that — escalate with the data-loss risk spelled out.
