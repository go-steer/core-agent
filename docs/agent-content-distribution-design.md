# Agent content distribution

**Status:** proposed (2026-08-08). Target: **v2.9**. Tracking issue: [#611](https://github.com/go-steer/core-agent/issues/611). Unblocks the `kube-platform-agent` hub+watcher deploy increment (PR F) named in `docs/hermes-replacement-design.md` (epic [#589](https://github.com/go-steer/core-agent/issues/589)).

## Motivation

`core-agent` ships as a distroless, **content-free** brain image
(`ghcr.io/go-steer/core-agent`). A recipe's content — `config.json`, `mcp.json`,
the project `AGENTS.md` overlay, `AGENTS.d/`, and (for content-heavy recipes) a
whole `content_roots` tree of workspace instructions + skills + governance +
docs — lives *outside* the image, in the operator's repo. Deploying a recipe to
Kubernetes therefore reduces to one problem: **get the recipe directory onto the
pod's filesystem, read-only, at a mount path.**

The existing `gke-troubleshoot-agent` deploy
(`examples/gke-troubleshoot-agent/deploy/`) solves this with a single flat
**ConfigMap**: `configMapGenerator` folds every file into `_`-flattened keys, and
a projected volume's `items:` list reconstitutes the tree at
`/etc/core-agent/.agents` (`kustomization.yaml:94-112`,
`50-deployment-daemon.yaml:231-268`). That works for its ~14-file, single-skill
recipe. It does **not** scale to a content-heavy recipe:

1. **ConfigMaps have a 1 MiB total-size ceiling.** The `kube-platform-agent`
   recipe's `upstream/` tree is **1.3 MiB** on disk (~692 KiB of the
   loader-consumed subset). A single ConfigMap cannot hold it; splitting across
   several ConfigMaps multiplies the `items:` bookkeeping without lifting the
   per-object limit meaningfully.
2. **ConfigMap keys can't contain `/`.** Every file becomes a hand-maintained
   `flat_key=path/in/tree` pair in *two* places (the generator and the volume
   `items:`). For 18 skills with `references/` subtrees plus 10 governance SOPs
   plus workspace docs, that is 100+ hand-written entries — unmaintainable and a
   silent-drift hazard.

We also do **not** want to solve this by embedding the recipe content in the
`core-agent` image. That would couple the generic brain to one recipe and force
an image rebuild on every content change — the opposite of the "content-free
brain, content-in-the-recipe" split the whole v2 loader is built around.

This doc establishes a **sanctioned content-distribution pattern** — the general
"how does a recipe's directory reach a distroless pod" answer — and picks a
primary mechanism.

## Conceptual model

The v2 loader is **path-relative and self-contained.** Given the recipe
directory materialized at *any* mount path, `core-agent -c <mount>/.agents/config.json`
reproduces local behavior exactly:

- `-c <file>` sets `agentsDir = filepath.Dir(cfgPath)` and
  `projectRoot = filepath.Dir(agentsDir)` (`cmd/core-agent/main.go:2033-2035`,
  `main.go:682-685`).
- `content_roots: ["../upstream"]` resolves relative to `agentsDir`
  (`cmd/core-agent/content_roots.go`) → `<mount>/upstream`.
- `@include upstream/SOUL.md` in the project `AGENTS.md` resolves relative to
  `projectRoot`, inside scope.
- On-demand `read_file upstream/governance/<sop>.md` is a `projectRoot`-relative
  read.

The recipe's own loader test (`examples/kube-platform-agent/recipe_test.go`)
already pins this self-consistency with no cluster and no credentials.

So **content distribution = "materialize the whole recipe directory, read-only,
at a mount path."** The only free variable is the *delivery mechanism*. Once the
directory is present, nothing about the daemon changes — this is a
deploy/packaging concern, not a runtime feature (**no `core-agent` code change**).

### Why the recipe directory must be mounted *whole*

The tempting optimization — put the small `.agents/` files in a ConfigMap and
mount the big `upstream/` tree separately — **breaks the loader**, for two
reasons that are load-bearing, not incidental:

- **`@include` is scope-confined.** `ensureWithinScope`
  (`pkg/instruction/load.go:648-667`) rejects any include target that escapes
  the including file's scope root. If `.agents/` and `upstream/` are separate
  mounts under a non-mount parent, `AGENTS.md`'s `@include upstream/SOUL.md`
  canonicalizes across a mount boundary the loader was never told to trust, and
  the include is refused. (`content_roots` grants trust to a *declared root* —
  it does not make an arbitrary split safe.)
- **Governance is read `projectRoot`-relative on demand.**
  `AGENTS.d/50-governance.md` indexes `upstream/governance/*.md` as
  `read_file` targets relative to `projectRoot`. Split the tree and every SOP
  path 404s at read time.

Mounting the whole recipe directory at one path sidesteps both: `projectRoot` is
the mount, `upstream/` is a real subdirectory of it, and every include and
on-demand read resolves exactly as it does locally.

### Layering: immutable content vs. mutable state vs. secrets

The content mount is **immutable and read-only**. Three things the daemon needs
are *not* content and get their own volumes layered in:

| Concern | Volume | Path |
|---|---|---|
| record_plan artifacts (scratch) | `emptyDir` | `<agentsDir>/plans` (nested inside the read-only content mount) |
| session eventlog + ACL (state) | PVC (RWO) | `/var/lib/core-agent` |
| bearer-table `users.json` (secret) | Secret → initContainer → `emptyDir` | separate dir, e.g. `/etc/core-agent/users.json` |

The `plans` nesting and the `users.json` initContainer staging are both proven in
the gke-troubleshoot deploy (`50-deployment-daemon.yaml:80-104,193-202`) — the
same techniques carry over unchanged. Keeping the content mount at a *distinct*
path (see the concrete layout below) means the `users.json` path stays free and
the auth volume never collides with the content volume.

## Goals

- A sanctioned, documented pattern for delivering a recipe directory to a
  distroless pod without embedding content in the `core-agent` image and without
  the ConfigMap size/flattening limits.
- **Start with OCI image volume** as the reference mechanism — content packaged
  as a `FROM scratch` OCI artifact, mounted read-only via `volumes[].image`. It
  is the recommended default, *not* an exclusive choice: every mechanism below
  realizes the same "mount the recipe directory whole" pattern and is fully
  supported. The reference recipe leads with the image volume and documents the
  others as drop-in overlays, so an operator picks the one that fits their
  cluster.
- Documented alternatives with when-to-use tradeoffs (ConfigMap, gcsfuse CSI,
  derived recipe image, initContainer copy). A `FROM scratch` **content image**
  is the shared artifact behind two of them (image volume *and* initContainer
  copy), so choosing a delivery mechanism rarely means rebuilding the content.
- Zero `core-agent` code change; zero change to the recipe's *local* behavior
  (the same `config.json` runs on a laptop and in-cluster).
- A reference implementation on the `kube-platform-agent` recipe (PR F).

## Non-goals (v2.9)

- No new `core-agent` runtime feature. (If verification surfaces a genuine gap —
  e.g. the absence of an unauthenticated `/healthz` endpoint, already noted in
  the gke-troubleshoot probe comment — that is tracked separately, not here.)
- Not changing gke-troubleshoot's ConfigMap deploy. It is correct for a small
  recipe; this doc documents the crossover point rather than churning it.
- Remote/URL content roots stay out of scope (security, per
  `docs/instruction-loader-v2-design.md`).
- HA / multi-replica (needs RWM storage) stays out of scope, as in
  gke-troubleshoot.

## Mechanisms

| Mechanism | How | Size limit | Image build? | Cluster floor | When to use |
|---|---|---|---|---|---|
| **ConfigMap** (status quo) | flatten tree into `_`-keys, reconstitute via projected `items:` | **1 MiB**, keys no `/` | no | any | small recipes (≲ tens of files, < ~900 KiB) |
| **OCI image volume** *(starting point)* | `FROM scratch` + `COPY` recipe dir; `volumes[].image` mounts it read-only | none (image) | yes (content-only) | k8s **1.33** beta / **GKE 1.35+** | content-heavy recipes; declarative + versioned + independent lifecycle |
| **initContainer copy** | run the **same** `FROM scratch` content image as an initContainer that `cp -a`'s its files into a shared `emptyDir` the daemon then mounts | none | reuses the content image | **any** | clusters below the image-volume floor; wants the same content artifact + no `core-agent` coupling and no runtime egress |
| **gcsfuse CSI** | push recipe dir to a GCS bucket; mount via the GCS Fuse CSI driver | none | no | GKE (addon) | GCP-native, no image registry step; accept fuse perf/consistency + IAM setup |
| **Derived recipe image** | `FROM core-agent` + `COPY` recipe dir; run that image | none (image) | yes (couples to base tag) | any | want a single runnable image; accept coupling content to the `core-agent` base tag |
| **initContainer git-clone** | initContainer clones the recipe repo into a shared `emptyDir` | none | no | any (needs egress) | specifically want an *unmodified upstream checkout* pulled at boot; accept runtime GitHub dep + git image |

All of these deliver the same outcome — the recipe directory, whole, at a mount
path — so they are interchangeable per cluster; the sections below detail the
starting point and the most useful fallback (initContainer copy).

### Starting point: OCI image volume

We lead with the OCI image volume because it is the most declarative option and
its content artifact is reused by the initContainer-copy fallback — but it is a
default, not a lock-in. Kubernetes
[image volumes](https://kubernetes.io/docs/tasks/configure-pod-container/image-volumes/)
(KEP-4639) mount the filesystem of an OCI image directly into a pod, **read-only**,
with no running container for it. Beta and on-by-default since k8s **1.33**;
available on **GKE 1.35+**. The pod spec is fully declarative:

```yaml
spec:
  containers:
    - name: core-agent
      volumeMounts:
        - name: recipe-content
          mountPath: /opt/kube-platform-agent   # image volumes are always read-only
  volumes:
    - name: recipe-content
      image:
        reference: ghcr.io/go-steer/kube-platform-agent-content:<tag-or-digest>
        pullPolicy: IfNotPresent
```

Why it wins for a content-heavy recipe:

- **No size or key limits.** The 1.3 MiB tree (or a 100 MiB one) is just image
  layers.
- **Declarative and versioned.** The content is an addressable artifact pinned
  by tag/digest in the overlay — it fits the "declarative-like config" the user
  asked for, and content and brain have **independent lifecycles** (bump the
  content image without touching `core-agent`, and vice versa).
- **Never touches the `core-agent` image.** The content image is
  `FROM scratch` — pure files, no base, no coupling. This is the property that
  distinguishes it from the derived-image fallback.
- **subPath + pull-secret support** work like any volume, so the `plans`
  emptyDir still nests and private registries still authenticate.

Version-floor mitigation: for clusters below GKE 1.35 the preferred fallback is
**initContainer copy** (next section) — it reuses the *identical* `FROM scratch`
content image, so the content artifact is unchanged and there is still no
coupling to the `core-agent` base tag; only the plumbing differs (an `emptyDir`
the initContainer fills, instead of an image volume). The **derived recipe
image** (`FROM ghcr.io/go-steer/core-agent + COPY`) remains a documented option
for operators who specifically want a single runnable image and accept coupling
the content to a base-image tag. Either way the image-volume path is the *base*
overlay and the fallback is an opt-out overlay, so the floor is never a hard gate.

### Fallback: initContainer copy (same content image, any cluster)

On clusters below the image-volume floor, run the **same** content image as an
initContainer whose only job is to copy its files into a shared `emptyDir` the
daemon then mounts read-only at the content path:

```yaml
spec:
  initContainers:
    - name: install-content
      image: ghcr.io/go-steer/kube-platform-agent-content:<tag-or-digest>
      command: ["cp", "-a", "/.", "/content/"]   # image root -> shared volume
      volumeMounts:
        - name: recipe-content
          mountPath: /content
  containers:
    - name: core-agent
      volumeMounts:
        - name: recipe-content
          mountPath: /opt/kube-platform-agent
          readOnly: true
  volumes:
    - name: recipe-content
      emptyDir: {}
```

This keeps the content image as the single source of truth — the operator builds
one artifact and picks the delivery mechanism per cluster.

Caveat: the copy step needs a `cp` binary, which a `FROM scratch` image doesn't
have. The pragmatic form is a **two-flavor content image** built from one
`content.Dockerfile` (byte-identical `COPY` layers): the `FROM scratch` flavor
for image-volume clusters, and a **minimal-base flavor that provides `cp`** for
the copy fallback. For that base, prefer **Chainguard busybox**
(`cgr.dev/chainguard/busybox`) over upstream `busybox`: it is Wolfi-based,
continuously rebuilt to typically **zero known CVEs**, similar in size (~1–4 MB),
and ships the `cp` applet. Distroless is *not* an option here — `distroless/static`
and `distroless/base` carry no `cp`/shell at all, and the `distroless/*:debug`
tags only add one by embedding busybox, so they are strictly larger with no
security gain. (The copy image is also short-lived — it runs only as an
initContainer and never sits alongside the daemon — so its attack surface is
already minimal; Chainguard busybox is simply the strictly-better base at the
same size.) Pin it by digest, same as the content and `core-agent` images.

### Building the content image

`deploy/content.Dockerfile` — one file, two flavors selected by a build arg. The
`COPY` layers (the actual content) are identical; only the base differs, so the
delivered files are byte-for-byte the same whichever flavor a cluster uses:

```dockerfile
# Content-only OCI artifact for the kube-platform-agent recipe.
#   BASE=scratch                  -> image-volume flavor (no base, no coupling)
#   BASE=cgr.dev/chainguard/busybox -> initContainer-copy flavor (provides cp)
ARG BASE=scratch
FROM ${BASE}
# The image root reproduces the recipe directory verbatim, so a pod that
# mounts it at <mount> can run `core-agent -c <mount>/.agents/config.json`.
COPY .agents/   /.agents/
COPY AGENTS.md  /AGENTS.md
COPY AGENTS.d/  /AGENTS.d/
COPY upstream/  /upstream/
```

`docker build --build-arg BASE=scratch` for the image-volume path;
`--build-arg BASE=cgr.dev/chainguard/busybox` (hardened, ~busybox-sized, has
`cp`) for the initContainer-copy fallback.

Notes:

- Ship the **loader-consumed set**. The carried-but-unexecutable skill
  `scripts/` (4 of the 18 platform skills carry `*.py`; one of the six
  `cluster/` skills carries a `*.sh`) can't run under distroless anyway
  (documented gap in the recipe README); whether to prune them from the content
  image is a size optimization, not a correctness question — default to
  *shipping them* so the image is a faithful mirror of the recipe dir the loader
  test validates.
- Pin by **digest** in the production overlay; use a moving tag only in the
  example overlay.
- Built in CI alongside the recipe, or by the operator; a
  `dev/tools/build-recipe-content-image` helper is an open question below.

### Concrete layout — kube-platform-agent

Mount the content image at a **dedicated path** (not `/etc/core-agent`, which the
auth volume uses):

```
/opt/kube-platform-agent/              <- content mount (image volume or copied emptyDir), read-only
├── .agents/
│   ├── config.json  (or config.hub.json via overlay)
│   ├── mcp.json
│   └── plans/                         <- emptyDir nested here (writable)
├── AGENTS.md                          (@include upstream/SOUL.md)
├── AGENTS.d/50-governance.md          (on-demand SOP index)
└── upstream/                          (content_roots: ["../upstream"])
```

Daemon args: `-c /opt/kube-platform-agent/.agents/config.json --no-repl
--session-db --session-db-path=/var/lib/core-agent/session.db
--small-tier-parent=allow`.

Path resolution, all satisfied by the single whole-tree mount:

- `agentsDir = /opt/kube-platform-agent/.agents`,
  `projectRoot = /opt/kube-platform-agent`.
- `content_roots: ["../upstream"]` → `/opt/kube-platform-agent/upstream` ✓
- `@include upstream/SOUL.md` → under `projectRoot`, in scope ✓
- `read_file upstream/governance/*.md` → `projectRoot`-relative ✓
- record_plan writes `/opt/kube-platform-agent/.agents/plans` → covered by the
  nested writable `emptyDir` ✓
- `config.hub.json` `auth.table_file: /etc/core-agent/users.json` → a *separate*
  path the content mount never touches; the `users.json` Secret is staged there
  by the same busybox initContainer pattern gke-troubleshoot uses ✓

### Watcher (event-driven path)

Reused verbatim from gke-troubleshoot, config swapped: the **lookout** watcher
(`ghcr.io/go-steer/lookout`, `51-deployment-watcher.yaml` — that manifest is the
pin of record, since the two recipes now track different lookout releases) runs as a
separate Deployment with its own SA + ClusterRole + ClusterRoleBinding, POSTs
matched K8s Events to the daemon Service `:7777` with a `WATCHER_TOKEN` Secret.
Nothing about the watcher depends on how the *daemon's* content is delivered, so
it drops in unchanged with the kube-platform owner/cluster-name args.

## Verification

- **Loader self-consistency** is already pinned hermetically by
  `recipe_test.go` (persona present, all 10 governance SOPs resolve, all 18
  skills discovered, cluster subagent declared, config policy). That test is the
  contract the content image must preserve.
- **Content-image completeness:** add a check that the file set `COPY`d into the
  content image equals the loader-consumed set — so nothing the loader needs is
  missing from the artifact. Cheapest form: a `dev/tools` script that builds the
  image, extracts it, and runs the existing loader test against the extracted
  tree.
- **In-cluster boot** on real GKE 1.35+ is manual UAT (documented in the recipe
  README), as with every GKE recipe — no credentials in CI.

## Rollout / follow-on

1. **This doc** + tracking issue [#611] (design-first; present for review before
   manifests).
2. **kube-platform-agent `deploy/`** (PR F, deploy-manifests-only scope): adapt
   gke-troubleshoot's `deploy/base/` — keep the lookout watcher Deployment + SAs
   + ClusterRole/Binding + Service + session-db PVC + users.json initContainer;
   **replace** the content ConfigMap with the OCI image volume; add
   `deploy/content.Dockerfile`; base overlay = image-volume, a documented
   `overlays/derived-image` fallback for pre-1.35 clusters. Use
   `config.hub.json` (multi-session bearer-table hub). Add a README deploy
   section; update `docs/site`.
3. **Generalize (doc-only):** note in the gke-troubleshoot README that its
   ConfigMap is the small-recipe floor and that content-heavy recipes should
   follow this pattern — without churning its working manifests.

The cluster-as-remote-peer half of PR F (W6 [#595]) remains deferred; the
`cluster` profile stays an in-process subagent for this increment.

## Open questions

1. **Content-image build helper.** Ship `dev/tools/build-recipe-content-image`
   (build both flavors + tag + optional push) or leave it to per-recipe
   `content.Dockerfile` + a README `docker build`? Lean: a thin shared helper,
   since every content-heavy recipe wants the same two-flavor build.
2. **Prune unexecutable skill scripts from the content image?** Faithful mirror
   (ship them) vs. minimal image (drop the `~6` `scripts/*.py`). Lean: ship them
   — the size delta is small and the image should equal what the loader test
   validates.
3. **Do we publish both content flavors by default, or build the Chainguard-base
   copy flavor only on demand?** Both are cheap; leaning toward publishing both so the
   image-volume base overlay and the initContainer-copy fallback overlay each
   reference a ready tag. The base overlay uses image-volume, the fallback
   overlay uses initContainer-copy, and derived-image stays a third documented
   overlay — none is a hard gate.
