# kube-agents Platform Agent, on core-agent

This recipe runs the [kube-agents](https://github.com/gke-labs/kube-agents)
**Platform Agent** — its persona, governance playbooks, and all 18 skills — on
core-agent's v2 runtime instead of Hermes. It is Phase 0 of the "one contract,
many companions" work (`docs/hermes-replacement-design.md`, epic #589): prove the
v2 instruction loader can consume an *unmodified* snapshot of the upstream
content, wired to the GKE MCP, with core-agent's plan-first gate and attach hub.

Nothing here is added to the kube-agents tree. The loader-consumed content under
`upstream/` is a faithful, unmodified snapshot (see `upstream/PROVENANCE.md`);
everything Hermes-specific is either translated into core-agent config or
documented as out of scope below.

## What you get

- The full Platform Agent persona (`SOUL.md` + `AGENTS.md`), `@include`d verbatim
  and reconciled to core-agent by a small overlay in `AGENTS.md`.
- All 18 skills discovered by the v2 skills loader.
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
  AGENTS.md              # persona (@include upstream/) + core-agent runtime overlay
  AGENTS.d/
    50-governance.md     # on-demand index of the governance SOPs
  upstream/              # faithful, UNMODIFIED snapshot (see PROVENANCE.md)
    SOUL.md AGENTS.md CAPABILITIES.md
    governance/          # 10 SOPs + inventory.md
    docs/                # glossary, gcp-console-links, session_management
    cluster/             # read-only Cluster Agent persona (@include'd by the subagent)
    LICENSE PROVENANCE.md
  .agents/
    config.json          # local REPL: model, plan-first policy, cluster subagent
    config.hub.json      # same + attach.multi_session (shared daemon)
    mcp.json             # gke + gke-readonly + developer_knowledge (native HTTP)
    skills/              # 18 skills (copied here; see PROVENANCE.md)
  recipe_test.go         # loader-only validation (no creds, no cluster)
```

The instruction files live at the recipe **root**, not under `.agents/`: the
loader confines `@include` to the including file's scope root, and only a
root-scoped `AGENTS.md` can reach the sibling `upstream/` snapshot. `.agents/`
holds the config, MCP, and skills the loader keys to `<agentsDir>`.

## Component mapping (kube-agents → core-agent)

| Upstream component | Verdict | Where it goes |
|---|---|---|
| `SOUL.md`, `AGENTS.md`, `CAPABILITIES.md` | **vendor** | `@include`d by `AGENTS.md` |
| `governance/` (10 SOPs + `inventory.md`) | **vendor** | on-demand via `AGENTS.d/50-governance.md` |
| 18 skills | **vendor** | `.agents/skills/` (script caveat below) |
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
  of the platform's fleet/provisioning skills. (Its own domain diagnostic skills —
  `gke-reliability`, `gke-storage`, … — live in `agents/cluster/skills/` upstream;
  vendoring that second skill tree waits for the external-content-root increment.)
- **`tools` (omitted → inherit)** — it inherits the parent's built-ins, which
  already have `bash` disabled and carry the same plan-first permission gate, so
  it cannot escalate.

The persona is the **unmodified** upstream `cluster/SOUL.md`, `@include`d and then
reconciled to core-agent by a short inline overlay (there is no kanban dispatcher
or bash preflight here — the subagent returns its RCA directly in its reply). See
[Reference → Declarative subagents](https://go-steer.github.io/core-agent/reference/configuration/#declarative-subagents-v29)
for the full `subagents[]` schema and the nil/list/empty scoping contract.

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
non-loopback listener refuses to start without authentication. The Kubernetes
deployment (namespace, KSA + Workload Identity, hub `Deployment`/`Service`) is a
follow-on increment; this recipe is the runnable config it wraps.

## Verify

`recipe_test.go` is a hermetic, credential-free check that the loader can consume
the bundle — persona assembly (the `@include` chain resolves), all 10 governance
SOPs discoverable and indexed, all 18 skills loaded, the `gke` +
`developer_knowledge` MCP servers parsed (and `platform_control` / `agent_common`
absent), and the plan-first policy set. It runs in CI's `test-unit` presubmit and
standalone:

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
