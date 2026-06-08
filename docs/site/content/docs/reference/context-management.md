---
title: Context management
weight: 6
---

Long agent sessions hit two failure modes the model can't recover from on its own:

1. **The context window fills up.** Every turn appends to the prompt; eventually the next turn errors out with "context window exceeded."
2. **Raw tool output bloats the parent.** A 5,000-line file read, a 200KB URL fetch, a grep with hundreds of matches — each dumps that volume into the parent's window even while it's still working, slowing every subsequent turn and crowding out the actual task.

`core-agent` ships three mechanisms — designed together, deployed independently — to keep long sessions alive. All three are on by default. See [`docs/context-management-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/context-management-design.md) for the full design rationale.

| Mechanism | Default | CLI flag to disable | Slash command |
|---|---|---|---|
| Compaction | on | `--no-compact` | `/compact [focus]` (alias `/summarize`) |
| Task-boundary checkpoints | on | `--no-checkpoint` | `/done [note]` (alias `/checkpoint`) |
| Agentic tool wrappers (subtasks) | on | `--agentic-tools=false` | (model-driven via `agentic_*` tools) |

A fourth — `/context` (alias `/boundaries`) — is an observation surface, not a mechanism: it reports the shape of what the others have done this session.

---

## Compaction (Mechanism A)

**The reactive backstop.** When the context window fills past ~85% utilization, the agent automatically compacts the conversation into a "teammate handover" summary and slices the pre-summary history out of future requests. The full audit log is preserved on disk — only the live LLM request is sliced.

### How it fires

- **Automatic:** post-turn hook checks utilization against the configured threshold (default `0.85`); when over, the next `Run` drains a `compactionPending` flag by writing the summary before its actual work. The operator-visible turn boundary stays clean — no surprise latency cliff after the assistant finishes.
- **Manual:** `/compact [focus]` runs the same summarizer immediately. The optional `focus` argument biases the summary toward a particular thread when you want to preserve specific context.

### What the summary contains

A five-section "teammate handover":

```
# Current state
The exact user request. What's been completed. What's actively in progress. What's specifically remaining.

# Files & changes
Files modified, read, or analyzed. Critical code locations with line numbers when known.

# Technical context
Architectural decisions made and why. Patterns adopted. Commands that worked or failed.

# Strategy & approach
The strategy chosen. Alternatives considered and rejected. Gotchas. Blockers.

# Exact next steps
A concrete numbered list of the next developer-style actions.
```

### When to disable

Pass `--no-compact` for short headless one-shots where compaction would never fire anyway, or when debugging issues where you don't want history rewrites in play. `/compact` remains available as a manual command regardless of the flag.

---

## Agentic tool wrappers (Mechanism B)

**The proactive bloat prevention.** Compaction and checkpoints are *reactive* — they clean up after raw tool output has already landed in the parent's context. Agentic wrappers are *proactive* — they route the underlying tool call through a single-purpose subtask on a (typically cheaper) model so only the digest reaches the parent. The raw 5,000-line read never enters the parent's context.

On by default since v2.1. Pass `--agentic-tools=false` to register only the bare tools.

### Configuring

```bash
# Default — wrappers register; subtasks inherit the parent's model
core-agent

# Recommended: route subtasks to a cheaper model for the cost-efficiency win
core-agent --agentic-small-model gemini-2.5-flash

# Opt out — register only the bare tools
core-agent --agentic-tools=false
```

### The four wrappers

| Wrapper | Inner tools | Replaces |
|---|---|---|
| `agentic_read_file` | `read_file` | bare `read_file` for large files |
| `agentic_fetch_url` | `fetch_url` | bare `fetch_url` for long pages |
| `agentic_grep` | `grep` + `read_file` | bare `grep` when matches will be many |
| `agentic_research` | `read_file` + `grep` + `list_dir` + `glob` | open-ended investigation |

Tool descriptions tell the model when to prefer the wrapper ("Use INSTEAD OF read_file when the file might be large and you only need a specific section..."). The wrappers share the parent's permission gate and per-tool output caps — the subtask isn't a security boundary, it's a *context isolation* boundary.

### Cost efficiency

The wrappers' point is the model-selection asymmetry: parent on a frontier model (Pro, Opus) does the reasoning; subtasks on a cheap tier (Flash, Haiku) do the *content digestion*. A subtask reading a 5,000-line file is ~95% prompt-context cost; offloading that to a model ~10x cheaper per-token routinely cuts session cost by 30-50% on long sessions.

### Fresh-context invariant

Each subtask sees ONLY its `SystemPrompt` + `UserMessage`. The parent's history never reaches it. This is load-bearing: the subtask gets the full attention budget for one narrow question, and the parent's prior turns can't leak into a subtask's work. The subtask's events land in a parent-prefixed session row (`<parent>:sub:<branch>`) so the audit log stays correlated without polluting the parent's session.

---

## Task-boundary checkpoints (Mechanism C)

**The proactive task-slicing.** Where compaction triggers on context pressure, checkpoints trigger on *task completion* — the model self-signals "this task is done" and a richer six-section completion record gets written, slicing the prior task's exploration out of future requests so the next task starts with a clean working set.

### How it fires

- **Model-driven:** at natural task boundaries the model calls the built-in `mark_task_done(detail)` tool. The handler stashes the detail and flips a pending flag; the next `Run` drains it by writing the checkpoint.
- **Operator-driven:** `/done [note]` slash (alias `/checkpoint`) does the same thing manually — useful when the model didn't notice the boundary or when you want to force one before switching topics.

### What the checkpoint contains

A six-section completion record:

```
# Task
What was the task? What's the headline outcome?

# Files & changes
Files modified, read, or analyzed. Files considered and NOT changed (with why).

# Technical context
Architectural decisions, patterns, commands that worked or failed.

# Strategy & approach
Strategy chosen, alternatives rejected, gotchas, lessons.

# Verification & next steps
What's been verified, what's known-good but unverified, follow-up work queued.

# Where we are
Status framed as "what the operator and I both know right now."
```

### Why checkpoints help (vs. compaction alone)

Compaction triggers on token pressure — it might fire mid-task and the summary will reflect mid-task state. Checkpoints fire on natural boundaries the model recognizes, so the summary is *task-complete-state* rather than *whatever-state-we-happened-to-be-in*. Both write the same kind of slicing boundary event under the hood (`session.Event.CustomMetadata["compaction"] = "checkpoint"` vs `"summary"`); the differences are the trigger condition and the prompt that shapes the summary.

### When to disable

Pass `--no-checkpoint` for runs where the model shouldn't self-signal task completion, or when debugging where auto-slicing complicates reproduction. Both `/done` and the `mark_task_done` model-facing tool are removed when this flag is set; `/help` and `/tools` reflect that.

---

## Observing the shape — `/context`

`/context` (alias `/boundaries`) reports what the three mechanisms have done this session. Companion to `/stats`: where `/stats` shows token totals + cost, `/context` shows the *shape* of the conversation.

```text
Context-management activity:
  Compactions:  1 (last 4m12s ago, focus: auth module)
  Checkpoints:  3 (last 51s ago, note: finished surveying messageKinds for the v3 design)
  Summarized:   8420 chars across all boundaries
  Subtasks:     2 (32919 in / 338 out tokens, $0.0107 rolled up to /stats total)
  Models:       gemini-3.1-pro-preview-customtools (5 turns, 30822 in / 558 out, $0.0683)
              + gemini-2.5-flash (2 turns, 16520 in / 206 out, $0.0055)
```

The **Models** row only appears when more than one model has been used this session (typical for `--agentic-tools --agentic-small-model`). Sorted by descending cost so the priciest model leads. The same breakdown also surfaces in `/stats` directly when multiple models are in play.

---

## How they layer together

The three mechanisms are designed to compose:

- **Agentic wrappers** prevent bloat from entering the parent in the first place (proactive).
- **Checkpoints** carve the session into focused task chunks at natural boundaries (semi-proactive).
- **Compaction** cleans up whatever still accumulates between boundaries (reactive backstop).

For a long autonomous run that needs to survive across many tasks, default-on compaction + default-on checkpoints + `--agentic-tools --agentic-small-model` is the recommended setup. Each layer makes the others more effective:

- The cheaper subtask cost makes compaction summaries less expensive (less raw output to summarize).
- Checkpoints between tasks mean compaction has less work to do (history is already mostly sliced).
- Compaction catches the case where you forget to `/done` or the model misses a natural boundary.

---

## Library usage

From your own Go code:

```go
import (
    "github.com/go-steer/core-agent/pkg/agent"
    "github.com/go-steer/core-agent/pkg/tools/agentic"
)

a, err := agent.New(model,
    agent.WithCompactor(agent.NewDefaultCompactor()),
    agent.WithCheckpointer(agent.NewDefaultCheckpointer()),
    agent.WithUsageTracker(tracker),
)
```

For the agentic wrappers, use `tools/agentic.AgenticReadFile`, `AgenticFetchURL`, `AgenticGrep`, `AgenticResearch`. They take an `AgenticToolOpts` with `AgentGetter` (a late-binding closure — see `agent.WithPostConstruct`), `Provider`, `SmallModelID`, and `InnerTools` (the bare tools the subtask is allowed to call). See [Library API → Context management]({{< relref "/docs/library/api.md" >}}) for full signatures.

Direct programmatic access:

- `Agent.Compact(ctx, focus) (CompactionResult, error)` — runs the summarizer synchronously.
- `Agent.CompactIfNeeded(ctx, focus) (CompactionResult, error)` — threshold-gated variant.
- `Agent.Checkpoint(ctx, taskNote) (CheckpointResult, error)` — writes a task-boundary checkpoint.
- `Agent.RunSubtask(ctx, SubtaskSpec) (SubtaskResult, error)` — the primitive the agentic wrappers are built on.
- `Agent.ContextStats() ContextStats` — snapshot the same data `/context` shows.
- `Agent.HasCompactor() bool` / `Agent.HasCheckpointer() bool` — predicates for host adapters gating slash commands.

---

## Where to go next

- [Interactive workflows]({{< relref "/docs/cli/interactive/workflows.md" >}}) — operator-side workflow context
- [Library API]({{< relref "/docs/library/api.md" >}}) — full signatures + extension points
- [Autonomous runs]({{< relref "/docs/cli/autonomous/operations.md" >}}) — compaction makes long unattended runs viable
- [Sessions and event log]({{< relref "/docs/reference/sessions.md" >}}) — how boundary events show up in the audit log
- [`docs/context-management-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/context-management-design.md) — full design rationale, alternatives considered, future roadmap (memory tools)
