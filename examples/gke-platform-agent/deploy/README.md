# `deploy/` — Kubernetes manifests for `gke-platform-agent`

Plain kustomize. No Helm, no operator, no CRDs of our own. `kubectl apply
-k overlays/<one>` is the whole install.

If you want to *run* this, start at [`../DEMO.md`](../DEMO.md) —
`scripts/set-up-demo.sh` picks the right overlay, fills in your
coordinates and verifies the result. This file is for reading the
manifests: what each object is for, and which decisions are load-bearing.

```
base/            one namespace, two Deployments, the RBAC they need
components/      composable add-ons (tracing)
overlays/        2 × 2 — content delivery × tracing
content.Dockerfile
```

## `base/`

Numbered by dependency order, so a reader meets each object before the
thing that references it.

| | |
| --- | --- |
| `00-namespace.yaml` | `gke-platform-agent` |
| `10-serviceaccount-daemon.yaml` | `core-agent-daemon` — the Vertex identity. Needs `roles/aiplatform.user` via Workload Identity; the file documents the binding. |
| `11-serviceaccount-watcher.yaml` | `lookout-watch` — needs no GCP role at all, except `roles/cloudtrace.user` on the traced path. |
| `12/13-clusterrole*-watcher.yaml` | Cluster-wide **read** for the watcher's enrichment sources. Vendored from lookout; note it includes `secrets: list`, which is why 16 exists. |
| `14/15-*-watcher-capacity.yaml` | A Role/RoleBinding in **`kube-system`**, for the `cluster-autoscaler-status` ConfigMap the `capacity` source reads. |
| `16-networkpolicy-watcher.yaml` | Default-deny **ingress** to the watcher, admitting only `:9090` scrapers from this namespace. Egress is deliberately unrestricted. |
| `20-secrets-placeholder.md` | Not a manifest. The two Secrets are created out-of-band; this says how. |
| `40-pvc.yaml` | Session DB. |
| `50-deployment-daemon.yaml` | The hub. |
| `51-deployment-watcher.yaml` | The watcher. |
| `60-service.yaml` | ClusterIP `:7777`. |

The base is not applyable on its own: it carries placeholder coordinates
and no content image, so the daemon would come up with nothing to read.

### Four things in here that are not obvious

**Namespacing goes through a `NamespaceTransformer` with `unsetOnly:
true`, not kustomize's `namespace:` shorthand.** The shorthand rewrites
the namespace of *every* namespaced object, including the capacity
Role/RoleBinding — which must stay in `kube-system`. Moved, they bind
nothing, and the watcher's `capacity` source 403s silently: no error, one
enrichment source quietly missing from every incident.
`namespace-transformer.yaml` carries the full rationale.

**Cluster-scoped names are namespace-suffixed** (`lookout-watch-<ns>`).
Two deployments of this recipe on one cluster would otherwise fight over
one ClusterRoleBinding, and tearing down either would break the other.

**The daemon has no HTTP health endpoint, so the probes are bare TCP
connects on `:7777`.** Every route requires a bearer token, and a probe
cannot hold one. A TCP probe proves the listener is up, which is what a
probe can honestly prove here.

**An initContainer stages `users.json` rather than mounting the Secret
directly.** `pkg/auth/users.go` rejects a bearer table with any group or
other mode bits set, and the pod's `fsGroup: 65532` makes a `0400` Secret
arrive as `0440`. The init container copies it to `0400` owned by 65532
in an emptyDir. Mounting the Secret straight in looks correct and fails
at boot with a permissions error about a file nobody wrote.

## `content.Dockerfile`

The recipe content ships as a container image: `AGENTS.md`, `.agents/`,
and the `cluster/` subagent root, copied into an image root that
reproduces the recipe directory. A ConfigMap cannot hold it (~1.3 MiB,
over the limit), and an image is already a thing registries distribute.

One `ARG BASE` selects the base, because the two delivery paths need
different ones:

- `scratch` — for the OCI **image volume**. Nothing executes; the kubelet
  mounts the image's filesystem read-only.
- `cgr.dev/chainguard/busybox` — for the **initContainer copy** fallback,
  which needs a `cp`.

`.agents/plans/.gitkeep` is pre-baked deliberately. The daemon writes
plans, the image volume is read-only, so a writable emptyDir is nested at
`<mount>/.agents/plans` — and a nested mount needs an existing mount
point inside the read-only parent. Without the `.gitkeep`, the directory
does not exist in the image and the pod fails to start.

## `overlays/` — two axes, four directories

Two independent decisions, neither of which is a preference:

|                               | tracing off          | tracing on (default)      |
| ----------------------------- | -------------------- | ------------------------- |
| **image volume** (K8s ≥ 1.33) | `example`            | `example-otel`            |
| **initContainer copy**        | `initcontainer-copy` | `initcontainer-copy-otel` |

**Content delivery** is forced by the cluster's Kubernetes version. Image
volumes are beta from 1.33 and enabled on GKE 1.35+; below that,
`initcontainer-copy` pulls the same content as an ordinary image and
copies it into an `emptyDir`.

**Tracing** is forced by whether GKE Managed OpenTelemetry is enabled —
the `telemetry.googleapis.com` `Instrumentation` CRD is the marker.

The two `*-otel` directories are thin composers: they reference their
delivery sibling and add `components/otel-gke`, and they declare **no
`images:` block of their own**. That absence is load-bearing.
`set-up-demo.sh` writes image pins into the *delivery* overlay, and an
outer `images:` block in the composer would override them — you would pin
a tag and deploy a different one.

`components/otel/` holds the tracing wiring and its own
[README](components/otel/README.md), including the GKE prerequisites and
the two IAM bindings that fail silently when missing.

## Deploying by hand

If you are not using `scripts/`:

1. Build and push the content image from the **recipe root** — the build
   context is that directory, not this one, because the Dockerfile
   `COPY`s `.agents/`, `AGENTS.md` and `cluster/`:
   `docker build -f deploy/content.Dockerfile -t <ref> .`
   Add `--build-arg BASE=cgr.dev/chainguard/busybox` for the
   `initcontainer-copy` path.
2. Create the two Secrets — see
   [`base/20-secrets-placeholder.md`](base/20-secrets-placeholder.md).
3. Copy `overlays/example/` (or `overlays/initcontainer-copy/`), edit the
   values its [README](overlays/example/README.md) lists, and
   `kubectl apply -k` it.

Read `kubectl kustomize <overlay>` before applying and grep it for
`your-`. Every placeholder in this tree is spelled that way so that one
grep catches an unfilled value before the cluster does.
