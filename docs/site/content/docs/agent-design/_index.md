---
title: Agent design
weight: 5
---

How to write the `AGENTS.md`, skills, and tool descriptions that make your agent actually behave the way you want. Prescriptive content rather than reference — what patterns work, what failure modes to watch for, how to make the model use subagents and `agentic_*` wrappers efficiently.

The reference material these pages draw from is in the [Reference]({{< relref "reference/_index.md" >}}) section: [Skills]({{< relref "reference/skills.md" >}}), [Permissions]({{< relref "reference/permissions.md" >}}), [Context management]({{< relref "reference/context-management.md" >}}).

## In this section

- **[System instructions]({{< relref "agent-design/system-instructions.md" >}})** — `AGENTS.md` patterns: role framing, do/don't lists, model-specific quirks, iteration approach. Includes the post-checkpoint loop case study from v2.0 development.
- **[Skills]({{< relref "agent-design/skills.md" >}})** — `SKILL.md` design: when to write a skill vs. an `AGENTS.md` rule, how to write a description that triggers, body structure, composability with `references/`.
- **[Subagents and wrappers]({{< relref "agent-design/subagents-and-wrappers.md" >}})** — getting the model to actually use `agentic_*` wrappers + background subagents; the verify-with-bare-tool failure mode; Flash hallucination on cross-corpus search; choreography patterns (worker, fan-out, manager, scheduled monitor).
- **[Cost efficiency]({{< relref "agent-design/cost-efficiency.md" >}})** — what moves the cost needle: model selection (Pro+Flash split), context management compounding, prompt caching, output shape. Includes a decision tree for "my session is more expensive than I expected."

## Reading order

If you're new to `core-agent`, walk these in order — each builds on the prior. Operators who've used `core-agent` for a while can pick whichever covers the specific friction they're hitting; the pages cross-link generously.
