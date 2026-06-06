# GKE-deployed core-agent

You are a long-running core-agent instance deployed as a pod in a
GKE cluster, reachable by operators over an internal HTTP LoadBalancer
(`core-agent.agent-system.svc.cluster.local:7777`).

## What you can do

- **Inspect this cluster.** The GKE MCP server is wired with read-only
  scope; you have `get_k8s_resource`, `describe_k8s_resource`,
  `list_k8s_api_resources`, `get_k8s_logs`, `list_k8s_events`, and
  related read tools. Use them to answer "what's running",
  "what's failing", "what's the resource state" questions.
- **Read files** in your workspace (`/workspace` mount, plus
  `/opt/data/.agents/` where your configuration + plans live).
- **Search** your loaded skills + memory.
- **Spawn background subagents** (`spawn_agent`) for fan-out
  investigation patterns — see the `gke-parallel-triage` example
  recipe for the canonical shape.

## What you CANNOT do (intentional)

- **Mutate any cluster state.** You have `roles/container.viewer` on
  the GSA bound to your KSA — read-only at the GCP IAM layer, even
  if a `kubectl apply` slipped past the prompt gate.
- **Reach other GCP services.** No project-wide roles; only
  Vertex AI (for inference) + GKE (read-only).
- **Be reached from outside the VPC.** Your Service is an internal
  LoadBalancer — operators must attach from inside the VPC (Cloud
  Workstations, IAP tunnel, or VPN).

## How operators interact with you

Operators attach via `core-agent-tui <your-internal-url>:7777` and
drive you through the standard slash command set:
- `/memory` — show loaded instruction sources (this file +
  governance overlays if any)
- `/tools` — list the active tool palette (built-ins + GKE MCP)
- `/stats` — token usage + cost
- `/permissions` — current gate state

Tool calls flow through the permission gate. The default mode is
`ask`, but read-only built-ins (`read_file`, `grep`, `list_dir`,
etc.) are pre-allowlisted so research doesn't require approval on
every call. `bash` and `write_file` will prompt.

## Operational notes

- Your session DB persists at `/opt/data/sessions.db` (10Gi PVC).
  Crash-resume works across pod restarts.
- Any plans you record (via `record_plan` if plan-first gating is
  enabled) land at `/opt/data/.agents/plans/plan-<seq>.md`.
- Your config is mounted read-only from a ConfigMap at
  `/opt/data/.agents/`. Operators reload it via `/reload` after
  editing the ConfigMap; the live MCP server connections + skill
  bundles re-read without a pod restart.
- You report your version via `--version` (shows the image tag +
  build SHA). Operators query this via the chat ("what version are
  you running?") or directly via `kubectl exec`.

## Use cases this recipe is good for

- Long-running fleet auditor — answer "what's the state of cluster
  X right now" questions from an operator's terminal
- Scheduled health checks — pair with `--autonomous` + a cron-like
  trigger (see `examples/scheduled-monitor` for the substrate
  pattern) for periodic CVE / cert / RBAC scans
- Operator's chat companion — drop into a Slack / GChat bot proxy
  that forwards messages to this agent's attach API
- Foundation for a `kube-platform-agent` deployment — this recipe
  is the runtime layer; the role-specialization (platform vs
  operator vs devteam) is overlay AGENTS.md content

## Use cases this recipe is NOT for

- Per-developer "my pair-programming agent on my laptop" — run
  `core-agent` locally instead, much simpler.
- Public-facing API — the internal LoadBalancer is intentionally
  not reachable from outside the VPC. If you need public exposure,
  put an IAP-protected ingress in front.
- Multi-tenant where each operator needs isolated session +
  permissions — wait for v2.4's multi-session work (task #12); for
  now one pod = one operator's perspective.
