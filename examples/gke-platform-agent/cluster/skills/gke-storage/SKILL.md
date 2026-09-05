---
name: gke-storage
description: Diagnose GKE storage problems — unbound PVCs, mount failures, exhausted volumes, wrong access modes or disk types. Read-only; produces a root cause, the evidence for it, and a proposed manifest change the parent applies.
---

# GKE storage

This is a **method**, not a mission. It tells you *how* to diagnose a storage
problem on the workload you were asked about. It never tells you *what* to look
at or *where* — that came with your goal.

## Reads — there is no shell

Everything here is read-only, through the `gke` MCP. There is **no** `kubectl` and
**no** `gcloud`; you cannot create, resize, or apply anything. Where this skill
says "propose", it means *write it into your report* for the platform agent to
act on.

| What you need | Read |
|---|---|
| PVC phase, requested size, class, bound volume | `gke_get_k8s_resource` on `persistentvolumeclaim` |
| PV backing a bound claim | `gke_get_k8s_resource` on `persistentvolume` |
| StorageClass provisioner and options | `gke_get_k8s_resource` on `storageclass` |
| Pod volume mounts and scheduling state | `gke_describe_k8s_resource` on the pod |
| `FailedMount`, `ProvisioningFailed`, `FailedAttachVolume` | `gke_list_k8s_events` |

**Never invent a tool name.** You cannot inspect a filesystem or read disk usage
from inside a container; where the answer needs that, say so and propose the
metric or check that would produce it.

## Step 1 — classify from the pod's state

- **`Pending`, and the events name a volume** → provisioning or binding. Step 2.
- **`ContainerCreating` with `FailedMount` / `FailedAttachVolume`** → the volume
  exists but is not reaching this pod. Step 3.
- **Running, but the application reports "no space left on device"** → capacity.
  Step 4.

## Step 2 — provisioning and binding

Read the PVC. `Pending` has three usual causes, and the events distinguish them:

- **No matching StorageClass** — the named class does not exist, or no default
  class is set and the PVC names none.
- **`WaitForFirstConsumer`** — this is *normal*. The class deliberately delays
  binding until a pod is scheduled; a PVC pending with no pod consuming it is not
  a fault. Do not report it as one.
- **`ProvisioningFailed`** — quota, an unsupported disk type in the zone, or a
  size below the provisioner's minimum. The event message names which.

Access mode is the other frequent answer: `ReadWriteOnce` on a Persistent Disk
binds to one node at a time, so a multi-replica Deployment mounting one RWO claim
will strand replicas. `ReadWriteMany` needs Filestore (`standard-rwm`) or another
NFS-backed class, not `pd.csi.storage.gke.io`.

## Step 3 — attach and mount failures

- **Zone mismatch** — a zonal PD can only attach to a node in its own zone. On a
  regional cluster this appears as a pod that schedules somewhere the disk cannot
  follow; `regional-pd` replication or a zone-constrained scheduler is the fix.
- **Still attached elsewhere** — a `Multi-Attach` error means the previous pod's
  node has not released it. Common after a node becomes `NotReady`; it usually
  clears, so check whether it is still happening before proposing anything.
- **Missing referenced object** — `FailedMount` for a `Secret` or `ConfigMap`
  volume names exactly what is absent. Confirm it is genuinely missing before
  proposing that it be created.

## Step 4 — capacity

Compare the PVC's request against what the application is doing. Volume expansion
is only possible when the StorageClass sets `allowVolumeExpansion: true` — read
that field first. It is one of the few mutable fields on a StorageClass, so if it
is false the proposal is a two-step change (enable it on the class, then raise
the PVC request), not an impossible one. The genuinely immutable fields are
`provisioner`, `parameters`, `reclaimPolicy`, and `volumeBindingMode`: a wrong
disk type or access mode means a new class and a data migration. Say which of
those two you are proposing.

## Step 5 — report and propose

You are **read-only by construction**. You do not resize, create, or apply
anything.

Report: **the root cause**, **the field or event that establishes it**, and **the
concrete change**.

Manifest proposals — hand back the YAML:

- A `StorageClass` with the right provisioner and options, for example
  `pd.csi.storage.gke.io` with `type: pd-ssd`, `volumeBindingMode:
  WaitForFirstConsumer`, and `allowVolumeExpansion: true`, which every production
  class should set from the start.
- A corrected PVC — access mode, class, or an increased `resources.requests.storage`
  where expansion is permitted.
- The right CSI driver for the access pattern: Persistent Disk for block,
  Filestore for `ReadWriteMany`, Cloud Storage FUSE for object data.

Name the disruption: a class change means a new volume and a data migration, and
expansion is online but not instant. Do not end with a bare status line like
"looked at storage."
