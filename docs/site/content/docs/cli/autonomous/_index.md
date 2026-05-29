---
title: Autonomous (headless)
weight: 2
---

The agent runs unattended against a goal you defined up front. Typically long-lived monitors, scheduled jobs, or remediation workers — anything where no operator is at the keyboard. Budgets (turns, tokens, cost, wallclock) bound the run; the session log is the audit trail.

> **The `quickstart` and `gke-team-scenario` pages below are landing in Phases 2 and 4 of the v2.0 docs redesign.** [Operations]({{< relref "cli/autonomous/operations.md" >}}) is the existing depth-oriented reference and is current.

## In this section

- **[Quickstart]({{< relref "cli/autonomous/quickstart.md" >}})** *(coming Phase 2)* — first 15 minutes: a working monitor that runs against a concrete scenario.
- **[Operations]({{< relref "cli/autonomous/operations.md" >}})** — the depth reference: budgets, lifecycle, crash-resume, failure policy, audit log. *(Was `autonomous.md` pre-v2.)*
- **[GKE team scenario]({{< relref "cli/autonomous/gke-team-scenario.md" >}})** *(coming Phase 4)* — multi-agent worked example based on [gke-labs/kube-agents](https://github.com/gke-labs/kube-agents): a parent platform agent plus operator + devteam specialists, each with their own MCP servers and skills, interacting around a real GKE workflow.

## Common references

- [Sessions and event log]({{< relref "reference/sessions.md" >}}) — durable session storage + crash-resume
- [Context management]({{< relref "reference/context-management.md" >}}) — compaction + checkpoints make long unattended runs viable
- [Attach mode]({{< relref "reference/attach-tui.md" >}}) — connect an operator TUI to an unattended agent
