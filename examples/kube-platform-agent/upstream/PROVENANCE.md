# Provenance

The content under this recipe is a faithful, **unmodified** snapshot of the
kube-agents Platform Agent, vendored so core-agent can run it via the v2
instruction loader without a live kube-agents checkout.

| | |
|---|---|
| Source repo | https://github.com/gke-labs/kube-agents |
| Source path | `agents/platform/` |
| Source commit | `2a5815d71e32cb622b3d118ee5b0c82eb579fd91` |
| License | Apache-2.0 (see `LICENSE`) |

## What is vendored, and where

`upstream/` is a single-tree snapshot of `agents/platform/`, consumed as a
**content root** (`content_roots: ["../upstream"]` in `.agents/config.json`):
the loader reads the workspace `AGENTS.md` and the whole `skills/` tree from it
directly, unmodified. Pointing that content root (or `--agents-content-dir`) at a
real, unmodified kube-agents checkout is the recipe's "live" mode — this snapshot
is the credential-free, CI-testable default.

Two files are handled outside the content root, because of how the loaders scope:

- **`SOUL.md`** is `@include`d by the project-root `AGENTS.md` rather than loaded
  from the content root. A content root auto-assembles only the workspace
  `AGENTS.md` (plus its `AGENTS.d/`), and upstream splits its persona across
  `SOUL.md` / `AGENTS.md` / `CAPABILITIES.md` — so `SOUL.md` is the one persona
  file the recipe vendors and includes directly. (`@include`/`AGENTS.d/` are
  confined to the including file's scope root; the recipe's instruction files
  live at the **project root** so `@include upstream/…` resolves.)
- **The `cluster` subagent's persona** loads from its **own** content root
  (`"root": "../cluster"` in `.agents/config.json`), not from this `upstream/`
  snapshot. The sibling `cluster/` tree — persona, skills, and MCP — is derived
  from `agents/cluster/` and documented below; `upstream/cluster/` remains the
  faithful, unmodified `agents/cluster/` reference snapshot.

| Upstream (`agents/platform/…`) | Here | Notes |
|---|---|---|
| `AGENTS.md` (workspace) | `upstream/AGENTS.md` | loaded from the content root, unmodified |
| `skills/` (18 skills) | `upstream/skills/` | loaded from the content root, unmodified — no copy under `.agents/` |
| `SOUL.md` | `upstream/SOUL.md` | `@include`d by the project-root `AGENTS.md` (see above) |
| `CAPABILITIES.md` | `upstream/CAPABILITIES.md` | faithful reference snapshot (not currently loaded; upstream's `AGENTS.md` does not `@include` it) |
| `governance/` (10 SOPs + `inventory.md`) | `upstream/governance/` | indexed on-demand by `AGENTS.d/50-governance.md` |
| `docs/` (glossary, gcp-console-links, session_management) | `upstream/docs/` | reference, read on demand |
| `agents/cluster/{SOUL,AGENTS,CAPABILITIES}.md` | `upstream/cluster/` | faithful, **unmodified** reference snapshot of the Cluster Agent persona (not loaded at runtime; the runtime persona lives in the sibling `cluster/` tree — see "Cluster Agent content root" below). |

## Cluster Agent content root (`../cluster/`)

The `cluster` subagent has its **own** content root — the sibling `cluster/`
tree, referenced by `"root": "../cluster"` in `.agents/config.json` /
`.agents/config.hub.json`. With a per-subagent root, the subagent auto-assembles
its persona from `cluster/AGENTS.md`, loads its skills from `cluster/skills/`,
and loads its MCP servers from `cluster/mcp.json` — all **independently of the
platform parent** (`root` support landed in core-agent #619; the recipe adopts
it in #621). This decouples the Cluster Agent entirely: the parent no longer has
to load a single cluster skill or persona line.

| Source | Here | Notes |
|---|---|---|
| `agents/cluster/SOUL.md` | `cluster/SOUL.md` | unmodified copy of the persona; `@include`d by `cluster/AGENTS.md`. |
| — | `cluster/AGENTS.md` | `@include SOUL.md` + a core-agent **runtime overlay** reconciling the Hermes persona (no kanban dispatcher, no `cluster_preflight.sh`, no bash/GitOps; return the RCA directly; reads via the read-only `gke` + `developer_knowledge` MCP). |
| `agents/cluster/config.yaml` (MCP block) | `cluster/mcp.json` | translated to core-agent's native HTTP MCP: read-only `gke` (`…/mcp/read-only`) + `developer_knowledge`, `agentic_wrap_llm: true`. |
| `agents/cluster/skills/` (6 skills) | `cluster/skills/<name>/` | unmodified: `gke-workload-troubleshooting`, `gke-observability`, `gke-reliability`, `gke-storage`, `gke-workload-scaling`, `gke-workload-security`. |

Why a dedicated `cluster/` tree rather than reusing `upstream/`:

- **A per-subagent `root` couples skills *with* a persona** — which is exactly
  what the Cluster Agent needs (its own persona + its own six skills), but that
  persona must be the **core-agent-reconciled** one, not the raw Hermes
  `agents/cluster/AGENTS.md`. So the root points at `cluster/`, which carries the
  overlay-augmented `AGENTS.md`, and not at `upstream/cluster/` (the faithful raw
  snapshot).
- **`upstream/` must stay a faithful `agents/platform/` snapshot**, so cluster
  content cannot live there without corrupting live-mode parity (pointing the
  content root at a real `agents/platform` checkout would then differ).
- **The root scope is confined** to `cluster/` — a subagent root's `@include`
  cannot reach `../upstream`, so `cluster/SOUL.md` is a copy rather than an
  include of `upstream/cluster/SOUL.md`. Re-sync both from the same commit.

Earlier (recipe #617) these six skills were vendored into the parent's
`.agents/skills/` and name-scoped onto the subagent, because a declarative
subagent's `skills:` was then only a subset of the *parent's* loaded set. The
per-subagent `root` removes that coupling: `.agents/skills/` is now empty and the
parent is back to its 18 platform skills.

`gke-workload-security` carries a `scripts/audit_cluster.sh`; like the platform
skills' scripts (below) it is discoverable but cannot execute in the distroless
brain image (bash is disabled).

## What is deliberately NOT vendored

These are Hermes-runtime-specific and have no analog in core-agent's runtime;
their capabilities are mapped in `../README.md` (see "Component mapping"):

- `config.yaml` — translated into `.agents/config.json` + `.agents/mcp.json`.
  The `platform_control` and `agent_common` stdio MCP servers are dropped (see
  the README's per-tool decomposition); `gke` and `developer_knowledge` are
  translated from node `mcp-remote` proxies to core-agent's native HTTP MCP.
- `cron/` — scheduled jobs; a separate core-agent increment.
- `plugins/` — the Hermes hook bus (incident_context, multiuser_memory).
- top-level `scripts/` (credential-proxy, relay-patches, token-broker,
  gitops-clone, session_kv, `platform_mcp_server.py`, `agent_common_server.py`) —
  moot under core-agent's distroless brain image.
- `agents/chat/` — the Chat Agent companion (a separate `go-steer/switchboard`
  workstream).
- `agents/cluster/config.yaml` — the Cluster Agent's Hermes config, translated
  into the `cluster` subagent block in `.agents/config.json` /
  `.agents/config.hub.json` (model, `"root": "../cluster"`, description) and its
  MCP block into `cluster/mcp.json`. Not vendored as a file. (Its persona and
  six domain skills **are** derived into the sibling `cluster/` content root —
  see "Cluster Agent content root" above.)

Skill-local `scripts/*.py` (in `fleet-audit`, `github-issue-resolver`,
`kube-agents-observability`, `submit-suggestion`) **are** carried along under
`upstream/skills/` so each skill's `SKILL.md` and references stay intact and
discoverable, but they cannot execute in the distroless brain image — see the
README's script caveat.

## Re-syncing

Re-copy the files above from the source path at a newer commit, update the
commit SHA in this file, and re-run the recipe's loader test
(`dev/tools/e2e-recipe-kube-platform-agent`, or `go test
./examples/kube-platform-agent/...`). This includes the sibling `cluster/` tree
(`SOUL.md` and `skills/` track `agents/cluster/` at the same commit). Do not
hand-edit vendored files; reconcile runtime differences in the overlay files
instead — the project-root `AGENTS.md` for the platform parent, and
`cluster/AGENTS.md`'s "Runtime overlay" section for the Cluster Agent.
