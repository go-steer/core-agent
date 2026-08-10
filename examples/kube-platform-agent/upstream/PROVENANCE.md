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
- **`cluster/SOUL.md`** is `@include`d by the `cluster` declarative subagent
  (subagent `@include`s are project-scoped, so they cannot reach a content root).

| Upstream (`agents/platform/…`) | Here | Notes |
|---|---|---|
| `AGENTS.md` (workspace) | `upstream/AGENTS.md` | loaded from the content root, unmodified |
| `skills/` (18 skills) | `upstream/skills/` | loaded from the content root, unmodified — no copy under `.agents/` |
| `SOUL.md` | `upstream/SOUL.md` | `@include`d by the project-root `AGENTS.md` (see above) |
| `CAPABILITIES.md` | `upstream/CAPABILITIES.md` | faithful reference snapshot (not currently loaded; upstream's `AGENTS.md` does not `@include` it) |
| `governance/` (10 SOPs + `inventory.md`) | `upstream/governance/` | indexed on-demand by `AGENTS.d/50-governance.md` |
| `docs/` (glossary, gcp-console-links, session_management) | `upstream/docs/` | reference, read on demand |
| `../cluster/{SOUL,AGENTS,CAPABILITIES}.md` | `upstream/cluster/` | the read-only Cluster Agent persona. `SOUL.md` is `@include`d by the `cluster` declarative subagent in `.agents/config.json` and reconciled to core-agent's runtime by the subagent's inline overlay (no kanban dispatcher / bash preflight here); `AGENTS.md` and `CAPABILITIES.md` are vendored as faithful reference snapshots (the subagent's routing blurb is condensed from `CAPABILITIES.md` into its `description`). All three unmodified. |

## Cluster Agent domain skills (`.agents/skills/`)

The `cluster` subagent's six domain diagnostic skills are vendored **unmodified**
from `agents/cluster/skills/` (same repo/commit as above) — but into
`.agents/skills/`, **not** under `upstream/`:

| Upstream (`agents/cluster/skills/…`) | Here |
|---|---|
| `gke-workload-troubleshooting`, `gke-observability`, `gke-reliability`, `gke-storage`, `gke-workload-scaling`, `gke-workload-security` | `.agents/skills/<name>/` |

Why `.agents/skills/` and not the `upstream/` content root:

- **The framework requires the parent to load any skill a subagent uses.** A
  declarative subagent's `skills:` is a *name-scoped subset of the parent's
  loaded set* (`surface.skills.Scoped`) — there is no independent per-subagent
  skill load — so these six must be in the parent's loaded skills for the
  `cluster` subagent to reference them. `.agents/skills/` is the project-scope
  source, loaded in both vendored and live modes.
- **`upstream/` must stay a faithful `agents/platform/` snapshot.** Dropping
  cluster skills into `upstream/skills/` would corrupt that invariant and break
  live-mode parity (pointing the content root at a real `agents/platform`
  checkout would then differ from the vendored tree).
- **The content-root coupling is the reason they can't come from a second root.**
  A content root couples skills *with instructions*: declaring `agents/cluster`
  as a root would also concatenate `cluster/AGENTS.md` ("one cluster only; never
  reason about the fleet") into the **platform parent's** prompt, contradicting
  its fleet mandate. Project-scope placement loads only the skills, not that
  persona. The residual trade — the parent also sees these six as invokable
  tools — is acceptable: they are troubleshooting SOPs, and the overlay
  `AGENTS.md` instructs the parent to delegate single-cluster diagnosis to
  `cluster` rather than run them itself.

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
  `.agents/config.hub.json` (model, MCP scope, description). Not vendored as a
  file. (Its six domain diagnostic skills **are** now vendored — see the Cluster
  Agent domain skills note below.)

Skill-local `scripts/*.py` (in `fleet-audit`, `github-issue-resolver`,
`kube-agents-observability`, `submit-suggestion`) **are** carried along under
`upstream/skills/` so each skill's `SKILL.md` and references stay intact and
discoverable, but they cannot execute in the distroless brain image — see the
README's script caveat.

## Re-syncing

Re-copy the files above from the source path at a newer commit, update the
commit SHA in this file, and re-run the recipe's loader test
(`dev/tools/e2e-recipe-kube-platform-agent`, or `go test
./examples/kube-platform-agent/...`). Do not hand-edit vendored files; reconcile
runtime differences in the project-root `AGENTS.md` (the overlay) instead.
