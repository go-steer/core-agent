---
title: Using the CLI
weight: 3
---

The `core-agent` binary supports two interaction modes. Pick the one that matches what you're trying to do:

## [Interactive]({{< relref "/docs/cli/interactive/_index.md" >}})

You're driving the agent yourself from a terminal. The Bubble Tea TUI takes over, conversation history is preserved across turns, slash commands surface session controls. Best for code review, exploration, ad-hoc tasks, anything where you want to see the model's response and react.

Start with the [interactive quickstart]({{< relref "/docs/cli/interactive/quickstart.md" >}}) for the first-15-minutes path.

## [Autonomous]({{< relref "/docs/cli/autonomous/_index.md" >}})

The agent runs unattended against a goal you defined up front — typically a long-lived monitor, scheduled job, or remediation worker. Budgets cap turns/tokens/cost/wallclock; the session log is the audit trail. Best for headless deployments where no operator is at the keyboard.

Start with the [autonomous quickstart]({{< relref "/docs/cli/autonomous/quickstart.md" >}}) for the first-15-minutes path.

---

## Library users

If you're embedding `core-agent` inside your own Go binary — building a coding assistant, web service, or custom autonomous worker — see [Using the library]({{< relref "/docs/library/_index.md" >}}) instead. The CLI guides above describe how the bundled binary behaves; the library guide covers the Go API you'd use to construct your own.
