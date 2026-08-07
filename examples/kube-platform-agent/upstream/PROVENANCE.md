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

The `@include` directive and `AGENTS.d/` scan are confined to the **scope root**
of the file that uses them — for a file loaded from `<project-root>/.agents/`
that root is `.agents/` itself, so a `.agents/AGENTS.md` cannot reach a sibling
`upstream/`. The recipe therefore puts its instruction files at the **project
root** (`AGENTS.md` + `AGENTS.d/`), where the scope root is the project root and
`@include upstream/…` resolves — the root-`AGENTS.md` layout the loader
explicitly supports (Cursor / Antigravity / Hermes convention). Skills are read
from `<agentsDir>/skills/` and cannot come from `upstream/`. So the snapshot is
split:

| Upstream (`agents/platform/…`) | Here | Notes |
|---|---|---|
| `SOUL.md`, `AGENTS.md`, `CAPABILITIES.md` | `upstream/` | `@include`d by the project-root `AGENTS.md` |
| `governance/` (10 SOPs + `inventory.md`) | `upstream/governance/` | indexed on-demand by `AGENTS.d/50-governance.md` |
| `docs/` (glossary, gcp-console-links, session_management) | `upstream/docs/` | reference, read on demand |
| `skills/` (18 skills) | `.agents/skills/` | copied here, not to `upstream/`, because `skills.Load` is keyed to `<agentsDir>` and cannot reach `upstream/`. This is the one place the config-only recipe diverges from a single-tree snapshot; the external-content-root increment removes the copy. |
| `../cluster/{SOUL,AGENTS,CAPABILITIES}.md` | `upstream/cluster/` | the read-only Cluster Agent persona. `SOUL.md` is `@include`d by the `cluster` declarative subagent in `.agents/config.json` and reconciled to core-agent's runtime by the subagent's inline overlay (no kanban dispatcher / bash preflight here); `AGENTS.md` and `CAPABILITIES.md` are vendored as faithful reference snapshots (the subagent's routing blurb is condensed from `CAPABILITIES.md` into its `description`). All three unmodified. |

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
- `agents/cluster/config.yaml`, `agents/cluster/skills/` — the Cluster Agent's
  Hermes config (translated into the `cluster` subagent block in
  `.agents/config.json`) and its domain diagnostic skills (the config-only
  recipe scopes the subagent's skills to none rather than vendoring a second
  skill tree; the external-content-root increment can add them).

Skill-local `scripts/*.py` (in `fleet-audit`, `github-issue-resolver`,
`kube-agents-observability`, `submit-suggestion`) **are** carried along so each
skill's `SKILL.md` and references stay intact and discoverable, but they cannot
execute in the distroless brain image — see the README's script caveat.

## Re-syncing

Re-copy the files above from the source path at a newer commit, update the
commit SHA in this file, and re-run the recipe's loader test
(`dev/tools/e2e-recipe-kube-platform-agent`, or `go test
./examples/kube-platform-agent/...`). Do not hand-edit vendored files; reconcile
runtime differences in the project-root `AGENTS.md` (the overlay) instead.
