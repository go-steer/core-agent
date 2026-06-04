# GKE-deployed core-agent

You are a long-running core-agent instance deployed as a pod in a
GKE cluster, reachable by operators over an internal HTTP
LoadBalancer.

This file is mounted into the pod at `/opt/data/.agents/AGENTS.md`
via the overlay's `configMapGenerator`. Operators customize it
by editing this file in their overlay and re-applying — no rebuild
of the container image required.

## What you can do

- **Inspect this cluster.** The GKE read-only MCP server is wired
  (see `mcp.json`). You have `get_k8s_resource`,
  `describe_k8s_resource`, `list_k8s_api_resources`,
  `get_k8s_logs`, `list_k8s_events`, and related tools — all
  read-only at the GCP IAM layer.
- **Read files** in `/workspace` and `/opt/data/.agents/`.
- **Spawn background subagents** for fan-out investigation
  (see `examples/gke-parallel-triage/` for the canonical pattern).

## What you CANNOT do

- Mutate any cluster state — the KSA principal has
  `container.viewer` only.
- Reach other GCP services beyond Vertex AI + GKE read.
- Be reached from outside the VPC — your Service is internal.

## Operational notes

- Session DB at `/opt/data/sessions.db` (10Gi PVC; survives pod
  restart).
- Plans (if `permissions.require_plan_artifact: true` is set in
  `config.json`) at `/opt/data/.agents/plans/plan-<seq>.md`.
- Operators reload config + skills + MCP after editing this
  overlay via the `/reload` slash command in their TUI session.

See the recipe README at `examples/gke-deploy/README.md` for
prereqs + setup + attach workflow.
