# kube-agents Platform Agent, on core-agent

This recipe runs the [kube-agents](https://github.com/gke-labs/kube-agents)
**Platform Agent** — its persona, governance playbooks, and all 18 skills — on
core-agent's v2 runtime instead of Hermes. It is Phase 0 of the "one contract,
many companions" work (`docs/hermes-replacement-design.md`, epic #589): prove the
v2 instruction loader can consume an *unmodified* snapshot of the upstream
content, wired to the GKE MCP, with core-agent's plan-first gate and attach hub.

Nothing here is added to the kube-agents tree. The Platform Agent's workspace
instructions and all 18 skills are loaded **from a content root** — the vendored
`upstream/` snapshot by default (a faithful, unmodified copy; see
`upstream/PROVENANCE.md`), or a real kube-agents checkout when you point
`content_roots` at one (see [Running against a live checkout](#running-against-a-live-checkout)).
Everything Hermes-specific is either translated into core-agent config or
documented as out of scope below.

## What you get

- The Platform Agent workspace instructions loaded verbatim from the content root
  and the `SOUL.md` persona `@include`d by the recipe's `AGENTS.md`, reconciled to
  core-agent by a small overlay in the same file.
- All 18 platform skills discovered by the v2 skills loader **from the content
  root** — no copied platform-skill tree under `.agents/` (that is what
  `content_roots` buys: the same config runs the vendored snapshot or a live
  checkout unchanged). The six *cluster* domain-diagnostic skills the `cluster`
  subagent carries live in that subagent's **own** content root (`cluster/`), not
  the parent's skill set — see [The `cluster` subagent](#the-cluster-subagent).
- The 10 governance SOPs + the bootstrap inventory scan, vendored and indexed for
  on-demand reading (not injected into every turn).
- The remote Google MCPs — a single **read-only** `gke` and `developer_knowledge`
  — translated from Hermes' node `mcp-remote` proxies to core-agent's native HTTP
  MCP transport. There is no read-write GKE endpoint: the platform agent is
  **propose-only by construction**, not just by persona.
- A read-only **`cluster` subagent** the platform agent delegates single-cluster
  diagnostics to — Hermes' per-cluster Cluster Agent profile, mapped to a
  declarative subagent that loads its **own** persona, six GKE domain-diagnostic
  skills, and read-only MCP surface from a dedicated content root (`cluster/`),
  independent of the platform parent (see below).
- Plan-first safety: every mutation (including *all* MCP calls) is gated behind
  `record_plan`, and `bash` is disabled.

## Layout

```
kube-platform-agent/
  AGENTS.md              # SOUL persona (@include) + core-agent runtime overlay
  AGENTS.d/
    50-governance.md     # on-demand index of the governance SOPs
  upstream/              # faithful, UNMODIFIED agents/platform snapshot = the default content root
    SOUL.md AGENTS.md CAPABILITIES.md
    skills/              # 18 skills (loaded via content_roots, not copied)
    governance/          # 10 SOPs + inventory.md
    docs/                # glossary, gcp-console-links, session_management
    cluster/             # faithful, UNMODIFIED agents/cluster snapshot (reference only)
    LICENSE PROVENANCE.md
  cluster/               # the `cluster` subagent's OWN content root ("root": "../cluster")
    AGENTS.md            # @include SOUL.md + core-agent runtime overlay
    SOUL.md              # Cluster Agent persona (copy of upstream/cluster/SOUL.md)
    mcp.json             # read-only gke + developer_knowledge (native HTTP)
    skills/              # 6 GKE domain-diagnostic skills
  .agents/
    config.json          # content_roots + model + plan-first policy + cluster subagent
    config.hub.json      # same + attach.multi_session (shared daemon)
    mcp.json             # read-only gke + developer_knowledge (native HTTP)
  recipe_test.go         # loader-only validation (no creds, no cluster)
```

Two scoping rules shape this layout:

- **Content root vs. project scope.** The workspace `AGENTS.md` and the whole
  platform `skills/` tree are read from the content root
  (`content_roots: ["../upstream"]`), so `.agents/` holds no copied *platform*
  skills — the same config runs the snapshot or a live checkout. The `cluster`
  subagent gets its **own** content root (`"root": "../cluster"`), a self-contained
  tree with its persona, six skills, and `mcp.json` — decoupled from the parent
  entirely (the parent loads none of the cluster's skills or persona). See
  [The `cluster` subagent](#the-cluster-subagent). MCP for the parent is *not*
  auto-loaded from `content_roots`, so the parent's `mcp.json` stays in `.agents/`
  (the recipe translates the two Google MCPs once).
- **Project-root instruction files.** `@include` and the `AGENTS.d/` scan are
  confined to the including file's scope root, so the recipe's `AGENTS.md` +
  `AGENTS.d/` live at the project **root** — that is the only scope from which
  `@include upstream/SOUL.md` resolves. `SOUL.md` is `@include`d rather than read
  from the content root because a content root auto-assembles only the workspace
  `AGENTS.md`, and upstream splits its persona across `SOUL.md` / `AGENTS.md` /
  `CAPABILITIES.md`.

## Component mapping (kube-agents → core-agent)

| Upstream component | Verdict | Where it goes |
|---|---|---|
| `AGENTS.md` (workspace) | **content root** | loaded from `content_roots` (snapshot or live checkout) |
| `SOUL.md` | **vendor** | `@include`d by `AGENTS.md` (content roots auto-load only the workspace file) |
| `governance/` (10 SOPs + `inventory.md`) | **vendor** | on-demand via `AGENTS.d/50-governance.md` |
| 18 skills | **content root** | loaded from `content_roots`, not copied (script caveat below) |
| MCP `gke`, `developer_knowledge` | **translate** | `.agents/mcp.json` native HTTP |
| `agents/cluster/` (read-only Cluster Agent profile) | **vendor + subagent** | `cluster` declarative subagent in `.agents/config.json` with `"root": "../cluster"` — its own persona, six domain skills, and read-only MCP loaded from the sibling `cluster/` content root |
| MCP `platform_control` (stdio Python) | **drop** | decomposed per-tool ↓ |
| MCP `agent_common` / `call_agent` (A2A) | **companion** | a peer/`call_peer` increment |
| `cron/jobs.json` | **companion** | a scheduled-jobs increment |
| `plugins/` (hook bus) | **n/a** | no hook bus in core-agent |
| top-level `scripts/` (credential-proxy, token-broker, gitops-clone, …) | **n/a** | moot under the distroless brain image |

### Why dropping `platform_control` is safe

- `verify_gke_cluster` → redundant with the `gke` MCP.
- `send_notification` → a chat/alert companion (out of scope here).
- `list_cc_pods`, `list_cc_healthchecks`, `get_cc_operator_status`,
  `get_cc_pod_diagnostics`, `audit_log_searcher` → **genuinely lost for now**:
  they shell out to `kubectl` / `gcloud logging` against `krmapihosting-system`
  and Config-Connector CRDs. The `gke` MCP doesn't cover Config Connector, and
  the distroless brain has no `kubectl` / `gcloud`. A future
  Config-Connector-diagnostics MCP (or a `../k8s-lookout` query path) closes this.

### Skill-script caveat

Four skills (`fleet-audit`, `github-issue-resolver`, `kube-agents-observability`,
`submit-suggestion`) carry `scripts/*.py`. The loader discovers and validates
their `SKILL.md` fine, but the scripts **cannot execute** in the distroless brain
image. That is a routing decision, not a dead end: read-only fleet/observability
work moves to `../k8s-lookout` (the dataplane-intelligence service with cluster
access), and the GitOps write path moves to a GitHub MCP +
the write-path increment. The validation test asserts skill *discovery*, not
script execution.

### Known gaps off Hermes

Config-Connector / KCC in-container diagnostics and audit-log search (above); the
plugin hook bus; the kanban delegation board (dropped by design — subagents and
peers replace it); the script-first cron path (until the scheduled-jobs
increment); and the byte-compatible `/v1/chat/completions` A2A wire shape (until a
compat adapter). None block Phase 0.

### Live-UAT observations (2026-08)

A first end-to-end incident run (a `FailedMount` from a mistyped Secret name,
`gemini-3.5-flash` parent) surfaced four behaviors. The first confirmed a design
choice to keep; the other three drove the fixes now shipped in this recipe:

- **Project/cluster addressing works — keep the env overlay.** With
  `.agents/env.yaml` declaring `GOOGLE_CLOUD_PROJECT` and the "Addressing GKE
  resources" block in `AGENTS.md`, the agent resolved the cluster's location in a
  **single** `list_clusters` call scoped to `projects/<project>/locations/-` and
  fully-qualified every subsequent call — no project guessing, no `projects/-`
  wildcards, no asking the operator. This eliminates the project-discovery thrash
  that otherwise burns opening turns.
- **Opening-turn filesystem scanning → fixed by positive-only framing.**
  `gemini-3.5-flash` initially spent roughly half its calls scanning the container
  filesystem (`list_dir /`, `/opt`, `/workspace`, …) and probing for Hermes
  startup artifacts — *including the exact paths an earlier overlay named as
  off-limits*. A **negative** "do not probe X" instruction reads as a to-do list
  to this model, so the overlay's "Start on the task" bullet was rewritten to a
  purely **positive** framing (everything you need is in this prompt; act on the
  task, don't look around first) with the enumerated off-limits paths removed. A
  stronger parent model or a tool-level guardrail remains the more robust option
  if scanning recurs.
- **The `cluster` subagent went unused → fixed by wiring its domain skills.** The
  platform parent self-diagnosed and even read `upstream/cluster/{SOUL,AGENTS}.md`
  by hand rather than delegating, partly because the subagent then carried
  `skills: []` — no troubleshooting playbooks, so little upside to delegating. The
  subagent now loads six GKE domain-diagnostic skills (and its own persona + MCP)
  from a dedicated `cluster/` content root (`"root": "../cluster"`), and the
  overlay's delegation bullet was sharpened to make delegation the cheaper path.
  (These skills were first vendored into the parent's `.agents/skills/` in #617;
  #621 moved them into the subagent's own root once per-subagent `root` shipped.)
  See [The `cluster` subagent](#the-cluster-subagent).
- **Propose-only was persona-only → now enforced by the transport.** The platform
  agent previously held a read-write `gke` MCP and, after recording a plan,
  **directly patched the live Deployment**. The recipe now points the single `gke`
  server at `https://container.googleapis.com/mcp/read-only`, so the mutating
  verbs (`gke_patch_*`, `gke_create_*`, …) are not in the tool surface at all —
  propose-only is enforced by construction, not persona. The GitOps write path
  (proposing changes as a PR) lands in a later increment.

Enrichment note (resolved, [#618](https://github.com/go-steer/core-agent/issues/618)):
earlier revisions shipped a minimal watcher ClusterRole (events + `pods:get`),
so injects carried **no enrichment** (`enrichment_error stage=resolve` — the
watcher SA couldn't `list` anything) and the agent had to gather all context
itself via the `gke` MCP. The deploy tree now vendors lookout's **enrichment-complete**
ClusterRole verbatim (`deploy/base/12-clusterrole-watcher.yaml`), so incident
injects arrive pre-warmed with the correlated bundle. That role includes a
cluster-wide `secrets: list` grant (the expiry source's §11 tradeoff); it is
`list`-only, paired with a default-deny `NetworkPolicy`
(`deploy/base/16-networkpolicy-watcher.yaml`), and on the pinned `lookout:v0.17.0`
image a narrower operator copy degrades to a `skipped=` partial rather than
hard-failing ([k8s-lookout#192](https://github.com/go-steer/k8s-lookout/issues/192)).
See [Deploy to GKE](#deploy-to-gke) for the RBAC breakdown and how to narrow it.

## The `cluster` subagent

Hermes scaffolds a per-cluster **Cluster Agent** profile — a read-only SRE pinned
to one cluster that the Platform Agent delegates deep diagnostics to via the
kanban board. core-agent maps that to a **declarative subagent** with its **own
content root**: a fixed roster entry in `.agents/config.json` (`subagents[]`) that
becomes a `cluster` tool the platform agent can call by name, and whose persona,
skills, and MCP all load from a self-contained `cluster/` tree.

```jsonc
"subagents": [
  {
    "name": "cluster",
    "description": "Read-only SRE scoped to exactly one named GKE cluster…",
    "model": { "provider": "vertex", "name": "gemini-3.5-flash" },
    "root": "../cluster"   // resolves against the agents dir → the sibling cluster/ tree
  }
]
```

With `root` set (a core-agent v2.9 feature, #619), the subagent loads
**independently of the platform parent**:

- **Persona** auto-assembles from `cluster/AGENTS.md` (which `@include`s
  `cluster/SOUL.md` — the unmodified upstream persona — then adds a short
  core-agent runtime overlay: no kanban dispatcher or bash preflight; the subagent
  returns its RCA directly in its reply).
- **Skills** load from `cluster/skills/` — exactly the six GKE domain-diagnostic
  skills (`gke-workload-troubleshooting`, `gke-observability`, `gke-reliability`,
  `gke-storage`, `gke-workload-scaling`, `gke-workload-security`) and none of the
  platform's fleet/provisioning skills. The platform parent never loads them.
- **MCP** loads from `cluster/mcp.json` — the same single **read-only** `gke`
  (`…/mcp/read-only`) + `developer_knowledge`, so the subagent has no write path.

The point is **least privilege**, enforced by config rather than by persona alone,
and by *construction* rather than by subsetting: the subagent's surface is
whatever its own root contains, so it can never reference a parent skill or a
mutating MCP verb it was never given.

- **`tools` (omitted → inherit)** — it still inherits the parent's built-ins,
  which already have `bash` disabled and carry the same plan-first permission gate,
  so it cannot escalate.

See [Reference → Declarative subagents](https://go-steer.github.io/core-agent/reference/configuration/#declarative-subagents-v29)
for the full `subagents[]` schema, including the per-subagent `root` contract.

### The `cluster/` content root

The Cluster Agent's tree is a small, self-contained content root derived from
`agents/cluster/` (same repo/commit as the platform snapshot):

| Source | In `cluster/` | Notes |
|---|---|---|
| `agents/cluster/SOUL.md` | `SOUL.md` | unmodified copy of the persona |
| — | `AGENTS.md` | `@include SOUL.md` + core-agent runtime overlay |
| `agents/cluster/config.yaml` (MCP block) | `mcp.json` | read-only `gke` + `developer_knowledge`, native HTTP |
| `agents/cluster/skills/` (6) | `skills/` | unmodified |

Why a dedicated `cluster/` tree rather than reusing `upstream/`:

- **A per-subagent `root` bundles skills *with* a persona** — exactly what the
  Cluster Agent needs, but that persona must be the **core-agent-reconciled** one
  (the runtime overlay), not raw Hermes `agents/cluster/AGENTS.md`. So the root
  points at `cluster/`, which carries the overlay-augmented `AGENTS.md`.
- **`upstream/` must stay a faithful `agents/platform/` snapshot.** Folding cluster
  content into it would corrupt that invariant and break live-mode parity.
- **A subagent root's `@include` is scope-confined** to the root, so it cannot
  reach `../upstream`; `cluster/SOUL.md` is therefore a copy (re-sync both from the
  same commit — see `upstream/PROVENANCE.md`).

Earlier (#617) these six skills were vendored into the parent's `.agents/skills/`
and name-scoped onto the subagent, because a declarative subagent's `skills:` was
then only a *subset of the parent's* loaded set — which also meant the parent saw
them as invokable tools. The per-subagent `root` (#621) removes that coupling
entirely: `.agents/skills/` is now empty and the parent is back to its 18 platform
skills.

## Run it locally

```bash
# From this directory. Uses your ADC for the GKE + developer_knowledge MCPs.
gcloud auth application-default login
core-agent -c .agents/config.json
```

`.agents/config.json` is the interactive-REPL config — no attach listener, so it
drops you straight into a session. The parent model defaults to Vertex
`gemini-3.5-flash`; set your project with `GOOGLE_CLOUD_PROJECT` (and
`GOOGLE_CLOUD_LOCATION`) or your ADC's active project. Ask it a read-only fleet
question first (e.g. "list the clusters in this project") — it will investigate
freely, then require a recorded plan before any mutation.

### As an attach hub

`.agents/config.hub.json` is the same recipe with an `attach.multi_session` block
(bearer-table auth, admin + proxy identities) so the agent runs as a shared
daemon that companions attach to:

```bash
core-agent -c .agents/config.hub.json \
  --no-repl \
  --session-db --session-db-path /tmp/kube-platform-agent/sessions.db
```

`--session-db` is a boolean that turns session persistence on; `--session-db-path`
sets where it lands (and self-enables persistence). The listen address comes from
the config's `attach.listen`. Before exposing the listener, populate the bearer
table at the path named in the config (`/etc/core-agent/users.json`) — a
non-loopback listener refuses to start without authentication.

## Deploy to GKE

`deploy/` is a kustomize tree that runs the hub daemon plus the event watcher in
a cluster. It reuses the [`gke-troubleshoot-agent`](../gke-troubleshoot-agent/)
deploy shape — namespace, daemon `Deployment`/`Service`, the
[lookout](https://github.com/go-steer/k8s-lookout) watcher + its RBAC, a
session-db PVC, and the `users.json` initContainer — with one substitution: the
recipe's content (~1.3 MiB of workspace + 18 skills + governance) is too large
for a ConfigMap, so it ships as an **OCI image volume** instead of a flattened
ConfigMap. See [`docs/agent-content-distribution-design.md`](../../docs/agent-content-distribution-design.md)
for the full pattern.

```
deploy/
  content.Dockerfile              # FROM-scratch content image (two flavors)
  base/                           # namespace, SAs, watcher RBAC (+capacity Role, NetworkPolicy), PVC, daemon, watcher, service
  overlays/example/               # image-volume delivery (GKE 1.35+)  ← start here
  overlays/initcontainer-copy/    # fallback for clusters below the image-volume floor
```

**1. Build + push the content image.** The daemon mounts the recipe directory
from a `FROM scratch` OCI artifact, so content and the core-agent brain image
have independent lifecycles (nothing recipe-specific is baked into
`core-agent`). From this directory:

```bash
docker build -f deploy/content.Dockerfile \
  -t ghcr.io/<you>/kube-platform-agent-content:v2 .
docker push ghcr.io/<you>/kube-platform-agent-content:v2
```

For clusters below GKE 1.35 (no image-volume support), also build the
busybox flavor for the initContainer-copy overlay:

```bash
docker build -f deploy/content.Dockerfile \
  --build-arg BASE=cgr.dev/chainguard/busybox \
  -t ghcr.io/<you>/kube-platform-agent-content:v2-copy .
```

**2. Create the two Secrets** (`core-agent-users`, `lookout-watch-token`)
out-of-band — see [`deploy/base/20-secrets-placeholder.md`](deploy/base/20-secrets-placeholder.md).
The `users.json` identities must match `config.hub.json`: the watcher's
`sa:lookout-watch` is a `proxy_identity` that asserts the admin owner
`platform-oncall@example.com`.

**3. Bind Workload Identity** for the daemon KSA — see
[`deploy/base/10-serviceaccount-daemon.yaml`](deploy/base/10-serviceaccount-daemon.yaml)
for the roles (`aiplatform.user`, `mcp.toolUser`, `container.viewer`,
`iam.serviceAccountUser` on the node SA).

**Watcher RBAC.** `deploy/base/12-clusterrole-watcher.yaml` is lookout's
**enrichment-complete** ClusterRole, vendored verbatim from
[k8s-lookout](https://github.com/go-steer/k8s-lookout) `deploy/12` @ `v0.17.0`
(only the object name is suffixed `-kube-platform` so it coexists with
gke-troubleshoot-agent's copy). It is read-only (`get`/`list`/`watch` only, no
write verbs) and grants exactly the reads lookout's enrichment + `--sources=auto`
detection perform. Two grants are worth calling out:

- **Cluster-wide `secrets: list`** — the expiry source's certificate/token scan
  (§11 tradeoff; the broadest grant). It is `list`-only and paired with a
  default-deny `NetworkPolicy` (`16-networkpolicy-watcher.yaml`). To avoid it,
  narrow your copy per the comments in `12-*.yaml` (namespace-tier `Role` +
  `--expiry-namespaces`, or drop the secrets/serviceaccounts rules and run
  `--enrich=off`) — since `v0.14.0` enrichment degrades to a `skipped=` partial
  instead of failing.
- **kube-system capacity `Role`** (`14`/`15`) — a name-scoped `get` on the
  `cluster-autoscaler-status` ConfigMap for the capacity source; inert when that
  source is off.

**4. Copy `overlays/example/`**, edit the `images:` pins (including the content
image you pushed), the `core-agent-gcp-env` project/location, and the watcher's
`--cluster-name`, then apply:

```bash
kubectl apply -k deploy/overlays/example
```

Operators (and the chat gateway companion) attach over the `core-agent` Service
on `:7777`; expose it via internal LoadBalancer, IAP, or `kubectl port-forward`.
Deploy a watcher into each additional cluster you want covered, pointing
`--daemon-url` at this hub. The `cluster` subagent stays in-process in this
increment; promoting it to a remote peer is a later step (W6).

## Running against a live checkout

By default the recipe loads its content from the vendored `upstream/` snapshot
(`content_roots: ["../upstream"]`), so it runs with no external dependency. To run
the Platform Agent's workspace instructions and skills from a **real, unmodified
kube-agents checkout** instead — the whole point of the content-root model —
**replace** the vendored root in the config with the checkout:

```jsonc
// .agents/config.json
"content_roots": ["/path/to/kube-agents/agents/platform"],
```

Replace, don't append: `--agents-content-dir` (and additional `content_roots`
entries) *layer after* the config's list — they don't override it — and skills
resolve first-declarer-wins in listed order. A real checkout carries the same 18
skill names as `upstream/`, so leaving `../upstream` ahead of it would shadow the
checkout's skills entirely and load both workspace `AGENTS.md` files. Point the
single content root at the checkout (drop `../upstream`) and the snapshot is out
of the picture. Use `--agents-content-dir` when you want to *add* a root whose
content does **not** collide with the snapshot — e.g. a scratch overlay layered
on top — not to switch runtimes:

```bash
# adds a non-colliding extra root on top of the config's roots
core-agent -c .agents/config.json --agents-content-dir /path/to/extra-overlay
```

The external tree is read **unmodified**: nothing is written into it, and the
recipe adds nothing to it. Two things still come from this recipe rather than the
checkout, by design:

- **`SOUL.md`** — `@include`d from `upstream/SOUL.md` (a content root auto-loads
  only the workspace `AGENTS.md`; keep the vendored `SOUL.md` in sync via
  `upstream/PROVENANCE.md`, or drop it into your checkout's `AGENTS.md` as an
  `@include` there).
- **`governance/` and `docs/`** — read on demand by path relative to the recipe;
  the on-demand `read_file` calls resolve against `upstream/`, not the external
  checkout (a sandbox reads within the project, not an arbitrary external tree).

Skills come entirely from the content root, so a live checkout's skill edits are
picked up on the next load with no re-vendoring.

## Verify

`recipe_test.go` is a hermetic, credential-free check that the loader can consume
the bundle — persona assembly (the workspace file loads from the content root and
the `SOUL.md` `@include` resolves), all 10 governance SOPs discoverable and
indexed, the 18 platform skills loaded from the content root (with `.agents/skills/`
empty — no cluster skill leaks into the parent), a live-checkout content root
honored via a fixture, the single read-only `gke` + `developer_knowledge` MCP
servers parsed (and the old `gke-readonly` / `platform_control` / `agent_common`
absent), the `cluster` subagent declared with `"root": "../cluster"` and its own
root loading exactly the six domain skills, the read-only MCP, and a persona
carrying the Read-Only Boundary + runtime overlay, and the plan-first policy set.
It runs in CI's `test-unit` presubmit and standalone:

```bash
dev/tools/e2e-recipe-kube-platform-agent          # or:
go test ./examples/kube-platform-agent/...
```

A live GKE run is manual UAT — bring your own project and clusters.

## Customizing

- **Model.** Edit the `model` object in `.agents/config.json` (any core-agent
  provider). Keep it in sync with `config.hub.json` if you run the hub too.
- **Read-only by default.** The single `gke` server points at
  `https://container.googleapis.com/mcp/read-only`, so both the platform agent and
  the `cluster` subagent are investigate-only. If you ever need a write path, add a
  *separate* read-write `gke` server in `mcp.json` and scope it to the parent only
  (never the `cluster` subagent) — but prefer the GitOps-PR write path when it
  lands.
- **Tune the cluster subagent.** Edit its content root (`cluster/`) — add or
  remove skills under `cluster/skills/`, adjust `cluster/mcp.json`, or refine the
  runtime overlay in `cluster/AGENTS.md` — or change its model in
  `.agents/config.json`. Add more roster entries the same way (e.g. a `cost` or
  `security` delegate, each with its own `root`).
- **Add companions.** Promoting the in-process `cluster` subagent to a remote
  peer, and adding chat/alert companions, arrives in later increments (peers,
  `call_peer`).

## Re-syncing the snapshot

See `upstream/PROVENANCE.md` — re-copy from the pinned source path at a newer
commit, update the SHA, and re-run the loader test. Reconcile runtime differences
in `AGENTS.md` (the overlay), never by editing vendored files.

## Related

- `docs/hermes-replacement-design.md` — the design this recipe validates.
- `../gke-troubleshoot-agent/` — a from-scratch GKE daemon recipe (kustomize deploy).
- `../gke-parallel-triage/` — a minimal config-only GKE recipe.
