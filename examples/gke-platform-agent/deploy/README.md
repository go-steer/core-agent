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

**Both probes are `httpGet /healthz`, not TCP connects.** They used to be
bare TCP on `:7777`, because every route required a bearer token and a
probe cannot hold one — an HTTP probe got a 401, which kubelet reads as
failure. [#946](https://github.com/go-steer/core-agent/issues/946) added
an unauthenticated `GET /healthz`, served ahead of auth, that reports
whether the session store is actually queryable. TCP cannot: a socket
keeps accepting connections long after the database behind it has stopped
answering, so a hub that could not serve a single inject still looked
Ready. This needs an image carrying #946 — against an older tag `/healthz`
404s and the pod never goes Ready.

**The `users.json` Secret is mounted directly, with no initContainer.**
There used to be one. `pkg/auth` rejected a bearer table with any group or
other mode bits set, and the pod's `fsGroup: 65532` turns a `0400` Secret
into `0440` on disk, so mounting the Secret straight in looked correct and
failed at boot with a permissions error about a file nobody wrote. The
workaround was an `install-users-json` initContainer that copied it into
an emptyDir at `0400` — about 20 lines of YAML and, more to the point, a
`runAsUser: 0` container in an otherwise non-root pod, present only to
run one `chmod`.

[#944](https://github.com/go-steer/core-agent/issues/944) accepts `0440`
when the file's owning group is one the process belongs to — exactly what
`fsGroup` produces — and still fails closed on any other group, so this is
the check learning what `fsGroup` means rather than a relaxation of it.
Both changes landed together in
[#986](https://github.com/go-steer/core-agent/issues/986), with the image
bump they required.

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
