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
- All 18 skills discovered by the v2 skills loader **from the content root** — no
  copied skill tree under `.agents/` (that is what `content_roots` buys: the same
  config runs the vendored snapshot or a live checkout unchanged).
- The 10 governance SOPs + the bootstrap inventory scan, vendored and indexed for
  on-demand reading (not injected into every turn).
- The remote Google MCPs — `gke` (read-write, for the platform agent),
  `gke-readonly` (the read-only endpoint the cluster subagent is scoped to), and
  `developer_knowledge` — translated from Hermes' node `mcp-remote` proxies to
  core-agent's native HTTP MCP transport.
- A read-only **`cluster` subagent** the platform agent delegates single-cluster
  diagnostics to — Hermes' per-cluster Cluster Agent profile, mapped to a
  declarative subagent scoped to a strictly narrower tool surface (see below).
- Plan-first safety: every mutation (including *all* MCP calls) is gated behind
  `record_plan`, and `bash` is disabled.

## Layout

```
kube-platform-agent/
  AGENTS.md              # SOUL persona (@include) + core-agent runtime overlay
  AGENTS.d/
    50-governance.md     # on-demand index of the governance SOPs
  upstream/              # faithful, UNMODIFIED snapshot = the default content root
    SOUL.md AGENTS.md CAPABILITIES.md
    skills/              # 18 skills (loaded via content_roots, not copied)
    governance/          # 10 SOPs + inventory.md
    docs/                # glossary, gcp-console-links, session_management
    cluster/             # read-only Cluster Agent persona (@include'd by the subagent)
    LICENSE PROVENANCE.md
  .agents/
    config.json          # content_roots + model + plan-first policy + cluster subagent
    config.hub.json      # same + attach.multi_session (shared daemon)
    mcp.json             # gke + gke-readonly + developer_knowledge (native HTTP)
  recipe_test.go         # loader-only validation (no creds, no cluster)
```

Two scoping rules shape this layout:

- **Content root vs. project scope.** The workspace `AGENTS.md` and the whole
  `skills/` tree are read from the content root (`content_roots: ["../upstream"]`),
  so `.agents/` holds no copied skills — the same config runs the snapshot or a
  live checkout. MCP is *not* auto-loaded from a content root, so `mcp.json` stays
  in `.agents/` (the recipe translates the two Google MCPs once).
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
| `agents/cluster/` (read-only Cluster Agent profile) | **vendor + subagent** | `cluster` declarative subagent in `.agents/config.json` (persona from `upstream/cluster/`) |
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

## The `cluster` subagent

Hermes scaffolds a per-cluster **Cluster Agent** profile — a read-only SRE pinned
to one cluster that the Platform Agent delegates deep diagnostics to via the
kanban board. core-agent maps that to a **declarative subagent**: a fixed roster
entry in `.agents/config.json` (`subagents[]`) that becomes a `cluster` tool the
platform agent can call by name.

```jsonc
"subagents": [
  {
    "name": "cluster",
    "description": "Read-only SRE scoped to exactly one named GKE cluster…",
    "instructions": "@include upstream/cluster/SOUL.md\n\n## Runtime overlay (core-agent)\n…",
    "model": { "provider": "vertex", "name": "gemini-3.5-flash" },
    "mcp": ["gke-readonly", "developer_knowledge"],  // scoped: NOT the read-write gke
    "skills": []                                     // grant none of the fleet skills
  }
]
```

The point is **least privilege**, enforced by config rather than by persona alone:

- **`mcp` (list → scope)** — the subagent sees only `gke-readonly` and
  `developer_knowledge`, never the read-write `gke` the platform agent itself
  uses. Its cluster reads physically cannot mutate.
- **`skills` (empty → grant none)** — a single-cluster read-only SRE inherits none
  of the platform's fleet/provisioning skills. Its own domain diagnostic skills
  (`gke-reliability`, `gke-storage`, …) live in `agents/cluster/skills/` upstream
  and stay unwired — see [Cluster domain skills](#cluster-domain-skills) for the
  content-root coupling that blocks them.
- **`tools` (omitted → inherit)** — it inherits the parent's built-ins, which
  already have `bash` disabled and carry the same plan-first permission gate, so
  it cannot escalate.

The persona is the **unmodified** upstream `cluster/SOUL.md`, `@include`d and then
reconciled to core-agent by a short inline overlay (there is no kanban dispatcher
or bash preflight here — the subagent returns its RCA directly in its reply). See
[Reference → Declarative subagents](https://go-steer.github.io/core-agent/reference/configuration/#declarative-subagents-v29)
for the full `subagents[]` schema and the nil/list/empty scoping contract.

### Cluster domain skills

The Cluster Agent ships its own diagnostic skills under `agents/cluster/skills/`
(`gke-reliability`, `gke-storage`, `gke-workload-troubleshooting`, …). A subagent
draws its skills by *scoping the parent's* loaded set, so to grant these the
platform parent would first have to load them — which means declaring
`agents/cluster` as a second content root. That does not work cleanly today: a
content root couples skills with instructions, so adding `agents/cluster` would
also fold its workspace `AGENTS.md` — *"one cluster only; never query or reason
about other clusters or the fleet"* — into the **fleet** platform agent's own
instructions, directly contradicting its mandate. Until a content root can
contribute skills without its instructions (or subagents can load skills from a
dedicated scope), the `cluster` subagent stays scoped to `skills: []` and relies
on its persona plus the read-only MCP surface. This is a real limitation of the
current content-root model, recorded in `docs/external-content-root-design.md`.

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
  base/                           # namespace, SAs, RBAC, PVC, daemon, watcher, service
  overlays/example/               # image-volume delivery (GKE 1.35+)  ← start here
  overlays/initcontainer-copy/    # fallback for clusters below the image-volume floor
```

**1. Build + push the content image.** The daemon mounts the recipe directory
from a `FROM scratch` OCI artifact, so content and the core-agent brain image
have independent lifecycles (nothing recipe-specific is baked into
`core-agent`). From this directory:

```bash
docker build -f deploy/content.Dockerfile \
  -t ghcr.io/<you>/kube-platform-agent-content:v1 .
docker push ghcr.io/<you>/kube-platform-agent-content:v1
```

For clusters below GKE 1.35 (no image-volume support), also build the
busybox flavor for the initContainer-copy overlay:

```bash
docker build -f deploy/content.Dockerfile \
  --build-arg BASE=cgr.dev/chainguard/busybox \
  -t ghcr.io/<you>/kube-platform-agent-content:v1-copy .
```

**2. Create the two Secrets** (`core-agent-users`, `k8s-event-watcher-token`)
out-of-band — see [`deploy/base/20-secrets-placeholder.md`](deploy/base/20-secrets-placeholder.md).
The `users.json` identities must match `config.hub.json`: the watcher's
`sa:k8s-event-watcher` is a `proxy_identity` that asserts the admin owner
`platform-oncall@example.com`.

**3. Bind Workload Identity** for the daemon KSA — see
[`deploy/base/10-serviceaccount-daemon.yaml`](deploy/base/10-serviceaccount-daemon.yaml)
for the roles (`aiplatform.user`, `mcp.toolUser`, `container.viewer`,
`iam.serviceAccountUser` on the node SA).

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
indexed, all 18 skills loaded from the content root (and *no* copied
`.agents/skills/` tree), a live-checkout content root honored via a fixture, the
`gke` + `developer_knowledge` MCP servers parsed (and `platform_control` /
`agent_common` absent), and the plan-first policy set. It runs in CI's
`test-unit` presubmit and standalone:

```bash
dev/tools/e2e-recipe-kube-platform-agent          # or:
go test ./examples/kube-platform-agent/...
```

A live GKE run is manual UAT — bring your own project and clusters.

## Customizing

- **Model.** Edit the `model` object in `.agents/config.json` (any core-agent
  provider). Keep it in sync with `config.hub.json` if you run the hub too.
- **Read-only fleet.** The `gke-readonly` server already points at
  `https://container.googleapis.com/mcp/read-only`; to make the *platform* agent
  investigate-only too, scope its own surface to it (or repoint `gke`) in
  `mcp.json`.
- **Tune the cluster subagent.** Widen or narrow its `mcp` / `skills` / `tools`
  scope in `.agents/config.json`, or give it its own model. Add more roster
  entries the same way (e.g. a `cost` or `security` delegate).
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
