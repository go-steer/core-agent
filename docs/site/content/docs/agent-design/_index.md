---
title: Agent design
weight: 5
---

How to write the `AGENTS.md`, skills, and tool descriptions that make your agent actually behave the way you want. Prescriptive content rather than reference — what patterns work, what failure modes to watch for, how to make the model use subagents and `agentic_*` wrappers efficiently.

> **All pages in this section are landing in Phase 3 of the v2.0 docs redesign.** The reference material they draw from is already current — see [Skills]({{< relref "reference/skills.md" >}}), [Permissions]({{< relref "reference/permissions.md" >}}), [Context management]({{< relref "reference/context-management.md" >}}).

## In this section

- **[System instructions]({{< relref "agent-design/system-instructions.md" >}})** *(coming Phase 3)* — `AGENTS.md` patterns: role framing, do/don't lists, fallback chain, why specific instructions matter for instruction-following-weak models.
- **[Skills]({{< relref "agent-design/skills.md" >}})** *(coming Phase 3)* — `SKILL.md` design for autonomous use: when to write a skill vs an `AGENTS.md` rule, how to scope triggers, references vs inline content.
- **[Subagents and wrappers]({{< relref "agent-design/subagents-and-wrappers.md" >}})** *(coming Phase 3)* — getting the model to actually use `agentic_read_file` / `agentic_grep` / `agentic_research` instead of falling back to bare tools. Spawning background subagents. Coordination patterns.
- **[Cost efficiency]({{< relref "agent-design/cost-efficiency.md" >}})** *(coming Phase 3)* — the Pro+Flash model split, why `/context` matters, prompt-caching considerations, when compaction + checkpoints pay for themselves.
