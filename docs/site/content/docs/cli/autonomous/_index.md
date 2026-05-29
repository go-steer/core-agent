---
title: Autonomous (headless)
weight: 2
---

The agent runs unattended against a goal you defined up front. Typically long-lived monitors, scheduled jobs, or remediation workers — anything where no operator is at the keyboard. Budgets (turns, tokens, cost, wallclock) bound the run; the session log is the audit trail.

## In this section

- **[Quickstart]({{< relref "/docs/cli/autonomous/quickstart.md" >}})** — first 15 minutes: a working monitor with budgets, headless permission posture, durable session log.
- **[Operations]({{< relref "/docs/cli/autonomous/operations.md" >}})** — the depth reference: budgets, lifecycle tool, crash-resume, failure policy, audit log, subagent composition. *(Was `autonomous.md` pre-v2.)*
- **[GKE multi-agent scenario]({{< relref "/docs/cli/autonomous/gke-team-scenario.md" >}})** — multi-agent worked example based on [gke-labs/kube-agents](https://github.com/gke-labs/kube-agents): a parent platform agent plus operator + devteam specialists, each with their own MCP servers and skills, interacting around a real GKE rollout/SLO scenario.

## Common references

- [Sessions and event log]({{< relref "/docs/reference/sessions.md" >}}) — durable session storage + crash-resume
- [Context management]({{< relref "/docs/reference/context-management.md" >}}) — compaction + checkpoints make long unattended runs viable
- [Attach mode]({{< relref "/docs/reference/attach-tui.md" >}}) — let an operator drop into an unattended agent mid-run
- [Agent design]({{< relref "/docs/agent-design/_index.md" >}}) — prescriptive patterns; goal-following is stricter without an operator in the loop
