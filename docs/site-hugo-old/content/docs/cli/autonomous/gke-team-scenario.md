---
title: GKE multi-agent scenario
weight: 3
---

A worked multi-agent example for running unattended `core-agent` agents against a GKE fleet. Three coordinating agents — `platform`, `operator`, `devteam` — each with their own role, MCP servers, skills, and budget envelope. The scenario at the end shows them interacting around a real production-style workflow: a rollout that breaches an SLO and gets investigated and remediated through the team's normal escalation path.

The team shape mirrors [gke-labs/kube-agents](https://github.com/gke-labs/kube-agents). The MCP wiring, skills, and scenario are concrete to `core-agent`'s capabilities — you can adapt them to your fleet.

If you haven't done the [Autonomous quickstart]({{< relref "/docs/cli/autonomous/quickstart.md" >}}) yet, do that first. This page assumes you understand how a single autonomous agent runs; the new shape here is multi-agent coordination.

---

## What this example shows

- Three autonomous agents running concurrently with different roles + tool surfaces
- A parent (`platform`) that spawns specialized children (`operator`, `devteam`) at runtime with scoped permissions
- Per-agent MCP servers — the same [GKE MCP server](https://docs.cloud.google.com/kubernetes-engine/docs/reference/mcp) wired with different endpoints (read-only vs full) per role
- Skills from [google/skills](https://github.com/google/skills/tree/main/skills/cloud/gke-basics) and [GoogleCloudPlatform/gke-mcp](https://github.com/GoogleCloudPlatform/gke-mcp/tree/main/skills) composed per role
- Cost-efficient operation via per-agent `--agentic-small-model` routing
- Escalation flow: `devteam` detects an app-level issue, investigates, escalates infra-side concerns to `operator`, which decides on cluster-side action

---

## The team

From the [gke-labs/kube-agents](https://github.com/gke-labs/kube-agents) README:

| Agent | Role | Scope | Spawned by |
|---|---|---|---|
| **`platform`** | Master custodian + agent architect. Multi-tenancy governance, RBAC boundaries, provisions specialized subagents at runtime. | Top-level | Operator (you) at startup |
| **`operator`** | Autonomous custodian of the infrastructure. Multi-cluster balancing, capacity, upgrades, platform security policies. Scheduled: health patrols, CVE scans, log rotations, certificate scans. | Infrastructure-wide | `platform` |
| **`devteam`** | Production-safety coach + application workload custodian. Schema validation, resource requests/limits, NetworkPolicies. Scheduled: rollout watches, error-rate monitors, SLO checks. | Per-application / per-namespace | `platform` |

`platform` runs continuously; it dispatches `operator` and `devteam` agents as needed (one per cluster, one per tenant, one per app, etc.).

---

## Repository layout

```
gke-team/
├── .agents/
│   ├── config.json                              # shared model + pricing defaults
│   ├── agents/
│   │   ├── platform/
│   │   │   ├── AGENTS.md
│   │   │   ├── config.json
│   │   │   └── templates/                       # subagent templates for operator + devteam
│   │   ├── operator/
│   │   │   ├── AGENTS.md
│   │   │   ├── config.json
│   │   │   ├── mcp.json                         # gke-mcp full endpoint
│   │   │   └── skills/
│   │   │       ├── cve-scan/SKILL.md
│   │   │       ├── certificate-rotation/SKILL.md
│   │   │       └── capacity-rebalancing/SKILL.md
│   │   └── devteam/
│   │       ├── AGENTS.md
│   │       ├── config.json
│   │       ├── mcp.json                         # gke-mcp read-only endpoint
│   │       └── skills/
│   │           ├── rollout-investigation/SKILL.md
│   │           ├── slo-monitoring/SKILL.md
│   │           └── error-rate-investigation/SKILL.md
└── cmd/
    └── gke-team/
        └── main.go                              # Go driver that spawns the team
```

Top-level `.agents/config.json` sets fleet-wide defaults (pricing source, default provider). Each agent's directory has its own `AGENTS.md` and `config.json` overlaying the role-specific bits.

---

## Per-agent configuration

### `platform` — the parent

The orchestrator. Doesn't do GKE work directly; spawns and monitors the specialists.

**`agents/platform/AGENTS.md`:**

```markdown
You are the platform custodian for the Acme GKE fleet. You don't touch
infrastructure or applications directly — you provision specialized agents
that do, and you watch what they do.

## Your responsibilities

- On startup: spawn one `operator` agent per cluster in the fleet, and one
  `devteam` agent per registered application namespace.
- On alert escalation: receive `report_alert` from child agents. If the
  alert is application-level, ack and let the originating `devteam` agent
  continue. If it's infrastructure-level (cluster, node, network, IAM),
  forward it to the relevant `operator` and ask for a remediation plan.
- On budget exhaustion or unhealthy subagent: stop the failing subagent
  with `stop_agent` and spawn a fresh one with a wider envelope.

## How you spawn agents

Use `spawn_agent` with these templates:

- operator: load `templates/operator.yaml`, pass `cluster=<name>` in the
  goal. Budget: 4h wallclock, 200 turns, $5 cost.
- devteam: load `templates/devteam.yaml`, pass `namespace=<name>` in the
  goal. Budget: 2h wallclock, 100 turns, $2 cost.

## What NOT to do

- Don't make GKE API calls yourself. You have no MCP wiring — that's
  intentional. Delegate to operator or devteam.
- Don't escalate operator alerts to devteam or vice versa. They have
  different scopes.
- Don't restart healthy subagents. The default replacement on budget
  exhaustion is fine; aggressive restarts churn audit logs.

## When to use report_done

When all child agents have completed and no new dispatches are needed —
typically only on shutdown.
```

**`agents/platform/config.json`:**

```json
{
  "version": 1,
  "model": {
    "provider": "gemini",
    "name": "gemini-3.1-pro-preview-customtools"
  },
  "permissions": {
    "mode": "allow",
    "allow": [
      "spawn_agent:*",
      "list_agents:*",
      "check_agent:*",
      "stop_agent:*"
    ]
  }
}
```

`platform` only has the spawn-tooling allowed. It can't touch the filesystem, can't shell out, can't make API calls. The strict surface keeps the parent's blast radius minimal.

### `operator` — infrastructure-wide

The specialist for cluster-side concerns. Wires the GKE MCP server's full endpoint (read + mutate) and the gke-basics skill for cluster-design guidance.

**`agents/operator/AGENTS.md`:**

```markdown
You are the infrastructure custodian for one GKE cluster (named in your goal).

## Your responsibilities

- Scheduled health patrols every 15 minutes: cluster status, node pool
  capacity, control-plane version, certificate expiry.
- CVE scans every 6 hours: scan running images against the public CVE feed,
  alert on high+ severity findings.
- Capacity rebalancing weekly: assess node pool utilization, propose changes.
- On escalation from a `devteam` agent: investigate the infra-side concern,
  return a one-paragraph remediation plan via `report_alert` to the
  platform.

## Tool surface

You have access to the full GKE MCP endpoint (cluster + node-pool + k8s
resource operations). You can mutate cluster state (apply manifests, patch
resources, scale node pools) — use this power judiciously.

## What NOT to do

- Don't take destructive action without explicit approval. Use the
  `/mcp/read-only` endpoint to investigate; only switch to the full endpoint
  when you have a concrete fix to apply AND the platform has acked your plan.
- Don't operate on application workloads. That's devteam's scope. If you
  notice an app-level issue, tell platform; let it route to the right devteam.
- Don't run `delete_cluster` or `delete_node_pool` ever. The `/mcp/delete-tools`
  endpoint is not wired into your tool surface — see your mcp.json.

## When to use report_done

You're a long-lived monitor — almost never. Only on platform-initiated
shutdown.

## When to use report_alert

- High+ CVE detected on a running image
- Certificate expiring within 7 days
- Node pool below capacity thresholds (free capacity < 10%)
- Control-plane upgrade available
- Escalated devteam concern that crosses into infra territory

Format: one paragraph, with the specific resource(s) named and a proposed
remediation. Don't post status updates when everything is healthy.
```

**`agents/operator/mcp.json`:**

```json
{
  "version": 1,
  "servers": {
    "gke": {
      "transport": "http",
      "url":       "https://container.googleapis.com/mcp",
      "headers":   { "Authorization": "Bearer ${env:GCP_TOKEN}" }
    },
    "gke-readonly": {
      "transport": "http",
      "url":       "https://container.googleapis.com/mcp/read-only",
      "headers":   { "Authorization": "Bearer ${env:GCP_TOKEN}" }
    }
  }
}
```

Two endpoints from the same MCP server. The `gke-readonly` endpoint is the default for investigation; switch to `gke` only when applying a remediation. The `/mcp/delete-tools` endpoint is intentionally not wired — destructive cluster deletion is too easy to do by mistake and should always require a human.

**`agents/operator/config.json`:**

```json
{
  "version": 1,
  "model": {
    "provider": "gemini",
    "name": "gemini-3.1-pro-preview-customtools"
  },
  "permissions": {
    "mode": "allow",
    "allow": [
      "gke_*",
      "gke-readonly_*",
      "schedule_next_turn:*",
      "report_alert:*",
      "report_done:*"
    ]
  },
  "agentic": {
    "enabled": true,
    "small_model": "gemini-2.5-flash"
  }
}
```

(The `agentic` config block is the file equivalent of `--agentic-tools --agentic-small-model`.)

**Skills wired in** (one example):

`agents/operator/skills/cve-scan/SKILL.md`:

```markdown
---
name: cve-scan
description: Scan running container images on this cluster for known CVEs. Use during scheduled patrols every 6 hours, or when the user asks for a security review. Composes the gke-readonly_get_k8s_resource (Pods) with public CVE feed lookups.
---

When invoked:

1. List all Pods in the cluster: `gke-readonly_get_k8s_resource` with
   kind=Pod, all namespaces.
2. Extract the unique image references (excluding system namespaces:
   kube-system, gmp-system, gke-managed-*).
3. For each unique image, query the CVE feed via `agentic_fetch_url` against
   https://api.osv.dev/v1/query — pass the image's package name + version.
4. Aggregate findings by severity (HIGH, CRITICAL).
5. If any HIGH+ findings: call `report_alert` with the affected images +
   pod counts + suggested remediation (image upgrade path).
6. Otherwise: silent return.

## When NOT to use

- Outside scheduled patrols, unless explicitly asked. Don't fire on every
  config change.
- Against system namespaces. Those are managed by Google and don't follow
  the same vulnerability response process.

## Cost note

This skill is the highest-cost operation the operator runs. Use the
agentic_fetch_url wrapper (routed via the cluster's --agentic-small-model)
so each CVE lookup runs on Flash, not Pro.
```

### `devteam` — app-side

The specialist for application workload concerns. Wires the GKE MCP server's read-only endpoint (it should never mutate cluster state on its own) and skills for rollout investigation, SLO monitoring, error-rate triage.

**`agents/devteam/AGENTS.md`:**

```markdown
You are the application-safety custodian for one namespace
(named in your goal) in the Acme GKE fleet.

## Your responsibilities

- Watch every deployment in your namespace. On any rollout (Deployment.spec
  change observed via the gke-mcp event stream), monitor the rollout for
  the next 30 minutes:
  - Pod readiness (every 30s for the first 5 min, then every 2 min)
  - Container restart counts
  - SLO error budget burn rate
  - Recent Warning events at the deployment + pod level
- Periodic SLO checks every 5 minutes for all watched deployments.
- On anomaly detection: investigate using the gke-readonly endpoint and the
  `slo-monitoring` / `rollout-investigation` skills. If the investigation
  reveals app-level cause: post `report_alert` to platform with findings.
- If investigation reveals infra-level cause (node pressure, image-pull
  failure, network policy block): post `report_alert` to platform asking
  for operator escalation.

## Tool surface

You have READ-ONLY access to the GKE MCP server. You cannot mutate cluster
state, scale deployments, or restart pods. This is intentional — your job
is to observe and alert, not act.

## What NOT to do

- Don't propose Kubernetes manifest changes directly. Surface the symptom
  and let the operator (via platform escalation) decide what to apply.
- Don't run kubectl apply / patch / delete commands. You have no access to
  the gke-mcp `/mcp` endpoint, only `/mcp/read-only`. Attempted writes
  will fail; don't waste turns retrying.
- Don't post status updates when everything is healthy. Silence = good.
- Don't restart investigations for the same incident. If you already
  reported an alert about Pod X CrashLooping 10 minutes ago, don't fire
  the same alert again unless the state has changed.

## When to use report_done

You're a long-lived monitor. Almost never call this; the platform shuts
you down via stop_agent when needed.

## When to use report_alert

- SLO error budget burn rate > 2x normal for > 5 min
- Pod in CrashLoopBackOff / ImagePullBackOff / Error for > 2 min
- Rollout stuck (kubectl rollout status timeout)
- Recent (last 5 min) Warning events at deployment/pod level
- Container restart count increasing across consecutive scans

Format the alert with: severity (P0-P3), affected resources (named),
investigation findings, suspected cause (app-level or infra-level),
recommended escalation path (devteam-side fix vs operator-side fix).
```

**`agents/devteam/mcp.json`:**

```json
{
  "version": 1,
  "servers": {
    "gke-readonly": {
      "transport": "http",
      "url":       "https://container.googleapis.com/mcp/read-only",
      "headers":   { "Authorization": "Bearer ${env:GCP_TOKEN}" }
    }
  }
}
```

Only the read-only endpoint. Devteam structurally cannot mutate the cluster — even if its AGENTS.md somehow drifted to suggest a mutation, the tool wouldn't exist in its surface.

**`agents/devteam/config.json`:**

```json
{
  "version": 1,
  "model": {
    "provider": "gemini",
    "name": "gemini-3.1-pro-preview-customtools"
  },
  "permissions": {
    "mode": "allow",
    "allow": [
      "gke-readonly_*",
      "schedule_next_turn:*",
      "report_alert:*"
    ]
  },
  "agentic": {
    "enabled": true,
    "small_model": "gemini-2.5-flash"
  }
}
```

---

## The Go driver

The driver that ties everything together. `platform` runs via `RunAutonomous`; its children are spawned dynamically via `spawn_agent`.

**`cmd/gke-team/main.go`:**

```go
package main

import (
    "context"
    "log"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/go-steer/core-agent/pkg/agent"
    "github.com/go-steer/core-agent/pkg/models"
    _ "github.com/go-steer/core-agent/pkg/models/gemini"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(),
        syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    // The platform agent's config lives at .agents/agents/platform/
    // — set CORE_AGENT_CONFIG_DIR or chdir to point at it before resolving.
    if err := os.Chdir(".agents/agents/platform"); err != nil {
        log.Fatalf("chdir: %v", err)
    }

    provider, err := models.Resolve(nil)
    if err != nil {
        log.Fatalf("resolve provider: %v", err)
    }
    model, err := provider.Model(ctx, "gemini-3.1-pro-preview-customtools")
    if err != nil {
        log.Fatalf("get model: %v", err)
    }

    // List of clusters + namespaces to manage. In a real deployment this
    // comes from a service registry; for the example, hardcoded.
    goal := `Bootstrap the GKE management team:

    Clusters: prod-us-central1, prod-europe-west1, staging-us-central1
    Namespaces: acme-api, acme-billing, acme-workers (in each prod cluster)

    Spawn one operator per cluster and one devteam per (cluster, namespace).
    Then monitor: when subagents report alerts, route per your routing rules.
    Replace failed subagents within 60s of failure detection.`

    res, err := agent.RunAutonomous(ctx, model, goal,
        // Generous wallclock — the platform runs as long as the operator
        // wants the fleet managed.
        agent.WithMaxWallclock(24*time.Hour),
        // Cost ceiling per day.
        agent.WithMaxCost(50.00),
        // Per-turn timeout — the platform's individual turns should be fast.
        agent.WithPerTurnTimeout(60*time.Second),
        // No subagent inheritance — each child has its own config + budget.
        // See devteam.yaml / operator.yaml in the templates/ directory.
    )
    if err != nil {
        log.Fatalf("platform autonomous run: %v", err)
    }
    log.Printf("platform stop: %s (turns=%d, cost=$%.4f)",
        res.StopReason, res.Turns, res.CostUSD)
}
```

Run it:

```bash
go build -o gke-team ./cmd/gke-team && GCP_TOKEN=$(gcloud auth print-access-token) ./gke-team
```

---

## The scenario: SLO breach during a rollout

A walkthrough of what happens in a real production incident.

### T+0:00 — Routine state

- `platform` has been running for 8 hours. It spawned 9 subagents (3 operators + 6 devteams across 3 clusters × 2 namespaces).
- `devteam-prod-us-central1-acme-api` is watching the `acme-api` namespace. Its last 5 SLO checks returned healthy (error budget burn rate 0.3x normal).

### T+0:01 — Deployment update lands

A CI/CD pipeline applies a new `acme-api` Deployment spec. `devteam-prod-us-central1-acme-api`'s next scheduled wake observes the change via `gke-readonly_list_k8s_events` and the `Deployment.spec` update.

The agent's `rollout-investigation` skill fires (description matches "rollout observed"):

```text
[devteam-prod-us-central1-acme-api turn]
> calling skill rollout-investigation
> calling agentic_read_file on the new Deployment.spec via gke-readonly_get_k8s_resource (small model: Flash)
> "Rollout target: 6 replicas, image acme-api:v2.4.7. Previous: v2.4.6."
> calling schedule_next_turn (30s) — first rollout check
```

### T+0:01:30 — First rollout check

```text
[devteam wakes]
> calling gke-readonly_get_k8s_rollout_status for acme-api deployment
> "1/6 ready, 5 pending image pull"
> calling schedule_next_turn (30s)
```

Normal early state. Devteam doesn't alert yet.

### T+0:03:30 — Three minutes in, still slow

```text
[devteam wakes]
> calling gke-readonly_get_k8s_rollout_status
> "3/6 ready, 1 ImagePullBackOff, 2 pending"
> ImagePullBackOff is anomalous. Investigating.
> calling gke-readonly_describe_k8s_resource on the failing pod
> "Failed to pull image acme-api:v2.4.7: ErrImagePull: rpc error: code = NotFound desc = Image not found"
```

The image doesn't exist in the registry. Devteam consults its skill book — this is application-level (CI/CD didn't push the image) AND potentially infra-level (registry connectivity).

### T+0:04:00 — Devteam alerts

```text
[devteam calls report_alert]
> Alert: P1
> Resource: deployment/acme-api in prod-us-central1/acme-api
> Finding: New rollout (v2.4.6 -> v2.4.7) stuck. Image acme-api:v2.4.7 NOT FOUND
>          in registry. 1 of 6 pods in ImagePullBackOff; 2 stuck pending.
>          Existing v2.4.6 pods serving traffic — SLO holding for now.
> Cause:   Application-level (CI/CD push failure) is most likely. Infra-level
>          (registry connectivity) is less likely — other deployments are
>          pulling cleanly.
> Escalation: devteam-side first (alert CI/CD pipeline owner). If image push
>             is confirmed but pull still failing, escalate to operator for
>             registry investigation.
```

The alert flows back to `platform`'s inbox.

### T+0:04:05 — Platform routes

```text
[platform receives alert via inbox]
> Alert is app-level per devteam classification. Acking devteam, no
> operator escalation needed yet.
> calling check_agent for devteam-prod-us-central1-acme-api — still healthy.
> No further action.
```

In a real deployment, `platform` would also POST the alert to PagerDuty / Slack / wherever. The skeleton above shows the routing decision; the actual alert delivery is a `report_alert` body the operator sees + a custom MCP tool would post externally.

### T+0:08:00 — Devteam continues monitoring

```text
[devteam wakes]
> calling gke-readonly_get_k8s_rollout_status
> "Still 3/6 ready, 3 pods now in ImagePullBackOff"
> SLO error budget burn rate now 1.4x normal (gradual increase as old
> pods absorb full traffic).
> Not yet anomalous enough to re-alert.
> calling schedule_next_turn (2 min)
```

### T+0:12:00 — CI/CD pipeline fixes the image push

A human (or the CI/CD pipeline) detects the push failure separately, re-pushes the image. Devteam's next wake observes:

```text
[devteam wakes]
> calling gke-readonly_get_k8s_rollout_status
> "6/6 ready, rollout complete in 2m43s"
> SLO error budget burn rate back to 0.3x normal.
> No further action. Returning to scheduled cadence.
```

### Why this scenario is realistic

- The failure was **app-level** (bad CI/CD push), not infra-level (cluster fine). Devteam's job was to detect, investigate, and escalate appropriately — not to fix the issue itself.
- `platform`'s routing decision was trivial here but matters for the harder cases (infra-level escalations going to operator, cross-cutting concerns going to both).
- `operator` was never woken for this — its role is infra-stewardship, not app rollouts. The structural separation (devteam read-only, operator full + read-only) means there's no accidental scope creep.
- The cost-efficiency wiring kept the bulk of the read load on Flash. Each devteam's scheduled wakes are cheap; the parent does almost no LLM work between alerts.

---

## What this demonstrates

**Multi-agent coordination via spawn + report.** `platform` spawns specialists via `spawn_agent`; specialists alert via `report_alert`; alerts flow back through the parent's inbox; the parent routes per its rules.

**Scope separation via tool surface.** `devteam` literally cannot mutate cluster state — its MCP wiring excludes the mutating endpoint. This is stronger than an `AGENTS.md` "don't do X" rule, because the model can't propose what the tool doesn't expose.

**Per-agent cost-efficiency.** Each child wired with `--agentic-tools --agentic-small-model gemini-2.5-flash`. The heavy reads (CVE lookups, rollout investigations) absorb to Flash; the parent reasoning stays on Pro.

**Same skill schema across roles.** The `cve-scan`, `rollout-investigation`, `slo-monitoring` skills are SKILL.md bundles with the same shape — just wired to different agents based on whose responsibility they are.

**Structural blast-radius limits.** `platform` has no GKE tooling at all — it can't accidentally take action even if its model went off-script. The blast radius for "the parent agent did something wrong" is bounded to "spawned a bad subagent" which `stop_agent` can correct.

---

## How to adapt

| Your situation | Adaptation |
|---|---|
| Single-cluster, single-tenant | Skip `platform` entirely. Run `operator` and `devteam` as two `RunAutonomous` calls in the same Go binary, with shared session storage but separate `agent.New` instances. |
| Multi-cloud (GKE + EKS + AKS) | Add cloud-specific MCP servers per `operator`. Devteam stays cloud-agnostic if your workloads are. |
| Different specialist split | The 3-agent shape is a starting point. A `security` agent for IAM + audit-log review is a common 4th specialist. |
| Want operator approval before remediation | Remove `operator`'s access to the full `/mcp` endpoint; have it propose remediations as `report_alert` to platform, which a human approves before running `kubectl apply` manually. |
| Want to extend to drift detection | Add a `drift-detector` skill to `operator` that runs daily, comparing cluster state to a desired-state YAML repo. Alert on diffs. |
| Want to add cost analysis | Wire the [gke-cost-optimization](https://github.com/GoogleCloudPlatform/gke-mcp/tree/main/skills/gke-cost-optimization) skill into `operator`. |

---

## What's outside this example

- **Multi-tenancy isolation between devteams.** Each devteam in this setup sees only its namespace via the MCP server's `--namespace` filter (set in the MCP server config, not shown). For stronger isolation, run separate GKE MCP server instances per-tenant.
- **State coordination across the team.** Devteam doesn't know what other devteams are doing. For workflows that need cross-tenant awareness (e.g., "all devteams are seeing higher error rates simultaneously — likely infra issue"), you'd add a `coordinator` agent that aggregates devteam alerts and emits cross-tenant signals.
- **Persistent memory across agent restarts.** When a subagent restarts (budget exhaustion, crash), it starts fresh — the prior agent's session data is in the event log but the new instance doesn't auto-load it. v2.1's [memory tools]({{< relref "/docs/reference/context-management.md" >}}) (PR IV) will close this gap.
- **Real alert delivery.** This example uses `report_alert` for in-team flow. To actually wake a human, wire a custom MCP tool for PagerDuty / Slack / your incident system; the `platform` agent's alert routing should call it.

---

## Where to go next

- **[Autonomous quickstart]({{< relref "/docs/cli/autonomous/quickstart.md" >}})** — single-agent version with budgets + headless permission posture
- **[Operations]({{< relref "/docs/cli/autonomous/operations.md" >}})** — the depth reference: budgets, lifecycle, crash-resume, failure policy
- **[Subagents and wrappers]({{< relref "/docs/agent-design/subagents-and-wrappers.md" >}})** — choreography patterns for the parent + child agent relationship
- **[System instructions]({{< relref "/docs/agent-design/system-instructions.md" >}})** — `AGENTS.md` patterns for autonomous use (crisp success criteria, explicit don't-do lists)
- **[Cost efficiency]({{< relref "/docs/agent-design/cost-efficiency.md" >}})** — Pro+Flash split economics, decision tree for cost-bounded fleet operations
- **[MCP servers]({{< relref "/docs/reference/mcp.md" >}})** — the schema for the `mcp.json` files shown above
- **[GKE MCP server reference](https://docs.cloud.google.com/kubernetes-engine/docs/reference/mcp)** — the upstream API: cluster ops, node-pool ops, k8s resource ops; read-only / full / delete-tools endpoints
- **[gke-labs/kube-agents](https://github.com/gke-labs/kube-agents)** — the upstream project this example mirrors; SOUL.md persona pattern + workspace integration
- **[google/skills GKE basics](https://github.com/google/skills/tree/main/skills/cloud/gke-basics)** — general GKE skill library
- **[GoogleCloudPlatform/gke-mcp skills](https://github.com/GoogleCloudPlatform/gke-mcp/tree/main/skills)** — workload + lifecycle + AI-troubleshooting skills for the `operator` and `devteam` agents
