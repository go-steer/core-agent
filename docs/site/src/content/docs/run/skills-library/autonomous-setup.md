---
title: autonomous-setup skill
---

The `autonomous-setup` skill walks a user through configuring an unattended `core-agent` — single-agent monitor or multi-agent team. Bundled in [`SKILLS/autonomous-setup/`](https://github.com/go-steer/core-agent/tree/main/SKILLS/autonomous-setup).

## What it covers

A 9-step runbook:

1. Decide if autonomous actually fits (vs interactive + attach)
2. Define the goal crisply
3. Pick single-agent or multi-agent shape
4. Bound the run with budget caps
5. Write `AGENTS.md` with unattended discipline
6. Set headless permission posture
7. Wire durable sessions
8. Write the Go driver
9. Test end-to-end before deployment

The runbook is non-linear; the agent picks references based on which path the user is on.

## Triggers

The skill matches phrasings like:

- "set up an autonomous agent"
- "run core-agent unattended"
- "configure a monitor"
- "build a multi-agent team"
- "set up a long-running agent"
- "watch the deploy / SLO / queue"

Any phrasing implying unattended execution OR mentioning concrete monitoring/coordination tasks.

## References

Three reference files; the agent fetches based on the user's path:

- **[`single-agent-monitor.md`](https://github.com/go-steer/core-agent/blob/main/SKILLS/autonomous-setup/references/single-agent-monitor.md)** — single-agent monitor pattern with a full worked example (Kubernetes deployment watcher). Covers goal-sentence shape, `AGENTS.md`, `config.json`, the Go driver, attach-mode observation, variations (cadence, multi-cluster), common failure modes. Read for ~80% of autonomous setups.
- **[`multi-agent-decomposition.md`](https://github.com/go-steer/core-agent/blob/main/SKILLS/autonomous-setup/references/multi-agent-decomposition.md)** — parent + specialists pattern (from gke-labs/kube-agents). Covers when to split, scope separation via tool surface, parent / specialist AGENTS.md patterns, cost model, anti-patterns. Read when the workload genuinely decomposes (multi-tenant, multi-domain).
- **[`budgets-and-recovery.md`](https://github.com/go-steer/core-agent/blob/main/SKILLS/autonomous-setup/references/budgets-and-recovery.md)** — sizing the four budget caps, per-agent vs shared budgets, failure policy, crash-resume via `ResumeAutonomous`, audit-log queries, diagnosing runaway cost. Read when sizing or troubleshooting.

## When to invoke vs read the docs

| You want | Use |
|---|---|
| Agent to walk you through autonomous setup | Install + invoke `autonomous-setup` |
| First-time 15-minute autonomous walkthrough | [Autonomous quickstart](/run/autonomous/quickstart/) |
| Multi-agent reference (GKE team-shaped) | [GKE multi-agent scenario](/use-cases/k8s-triage/) |
| Budget / lifecycle reference | [Autonomous → Operations](/run/autonomous/operations/) |

The skill IS the docs in workflow form.

## Installing

```bash
cp -r /path/to/core-agent/SKILLS/autonomous-setup .agents/skills/
```

See [Skills library → Installing](/run/skills-library/) for global install options.

## Adapting

Common adaptations:

**Org-specific budgets.** Edit the skill body's "picking values" section to hardcode your org's defaults:

```markdown
For all production agents at Acme, use these defaults:
- WithMaxWallclock(24*time.Hour)
- WithMaxCost(20.00)
- WithPerTurnTimeout(60*time.Second)
Cost overruns require approval; smaller budgets are encouraged.
```

**Pre-defined templates.** Replace the generic Go driver with one that points at your team's template repository:

```markdown
For Acme autonomous monitors, start from our template:
git clone github.com/acme/agent-template my-monitor
Then customize cmd/main.go's goal string.
```

**Cloud-specific patterns.** Add a `references/aws-deployment-patterns.md` (or GCP, Azure) covering your cloud's runtime quirks — service account setup, log forwarding, etc.

## Where to go next

- **[Autonomous quickstart](/run/autonomous/quickstart/)** — single-agent worked example
- **[GKE multi-agent scenario](/use-cases/k8s-triage/)** — multi-agent worked example
- **[Subagents and wrappers](/agent-design/subagents-and-wrappers/)** — choreography patterns the skill references
- **[Cost efficiency](/agent-design/cost-efficiency/)** — model-selection decisions for autonomous runs
