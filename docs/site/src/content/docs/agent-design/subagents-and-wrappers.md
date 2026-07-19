---
title: Subagents and wrappers
---

Two ways to push work off the parent agent:

- **Agentic tool wrappers** (`agentic_read_file`, `agentic_grep`, `agentic_research`, `agentic_fetch_url`) — synchronous, bounded, single-purpose. The parent calls them like any other tool; under the hood they spawn a focused subtask on a (typically cheaper) model and return only the digest. Raw tool output never enters the parent's context.
- **Background subagents** (`spawn_agent`, `list_agents`, `check_agent`, `stop_agent`) — asynchronous, longer-running, multi-turn. The parent dispatches a goal; the subagent works in its own session until done; alerts and completion summaries land back in the parent's chat.

This page covers when to use each, how to actually get the model to use them (the model-side adoption story is non-trivial), and the failure modes worth designing around.

For the mechanisms themselves see [Context management → Agentic tool wrappers](/concepts/context-management/) and the [Reference → Background subagents](/reference/configuration/) section.

---

## When to use which

| Question | Answer |
|---|---|
| Will it finish in a few seconds and return a discrete result? | Agentic wrapper |
| Does it need to span minutes/hours and report progress over time? | Background subagent |
| Is it a tool call where you care about the digest but not the raw output? | Agentic wrapper |
| Do you want it to make autonomous decisions in parallel with the parent? | Background subagent |
| Will the parent block on the result? | Agentic wrapper (it's synchronous) |
| Does the subtask need its own tools the parent doesn't have? | Background subagent |
| Does the model need to use it many times per turn? | Agentic wrapper (cheaper per call) |
| Is the goal "fan out N independent tasks, collate results"? | Background subagents (N of them, in parallel) |

**Rule of thumb:** wrappers replace bare tool calls; subagents replace handing off a multi-step task. If you're asking "should this be a tool or a subprocess," the answer is wrapper. If you're asking "should this be inline reasoning or a delegated task," the answer is subagent.

---

## Agentic wrappers: getting the model to actually use them

This is the non-obvious part. The wrappers register by default; getting the model to consistently prefer them over the bare tool calls (which are also still registered) is a separate problem.

### The default behavior

With `--agentic-small-model gemini-2.5-flash` set (the wrappers themselves are on by default), the model sees both `read_file` and `agentic_read_file`. Their descriptions explicitly tell it when to prefer the wrapper:

> Read a file and return a focused excerpt or summary. **Use INSTEAD OF read_file** when the file might be large and you only need a specific section...

Pro/Opus-tier models will generally route to the wrapper for large files. But two failure patterns are worth knowing about.

### Failure pattern 1 — verify-with-bare-tool

**Symptom:** the model calls `agentic_read_file`, gets back a digest, then calls bare `read_file` on the same file to "verify" the digest by reading the source directly.

**Cause:** Frontier models double-check digests by reading raw data when enumeration precision matters. The digest is correct; the model just doesn't trust it.

**Impact:** The agentic wrapper's cost-efficiency win is partly defeated because the parent's context absorbs the raw read anyway. The Flash subtask still ran cheaply on the scan, but the parent did a redundant Pro-priced read on top.

**Mitigations (in increasing strength):**

1. **`AGENTS.md` rule:**
   ```markdown
   ## When using agentic_* tools

   The agentic_read_file / agentic_grep / agentic_research wrappers route
   reads through a subtask so the raw content stays out of your context.
   Don't re-read the same path/pattern with bare read_file or grep to
   spot-check — that re-introduces the raw content you were trying to
   avoid. If a digest is missing something specific, call the wrapper
   again with a narrower question instead.
   ```

   The tool descriptions now ship with this same guidance baked in (v2.1+), so this `AGENTS.md` rule is reinforcement rather than the primary signal.

2. **Restrict the bare tool's permission:** allow `agentic_*` freely; require approval for bare `read_file` on large files.

3. **Use bare tools as escape hatches only:** disable the bare tools that have agentic counterparts via `tools.disable`. The model can't fall back to what isn't registered.

Option 1 is the gentlest; Option 3 is the hardest constraint. Match the strictness to your tolerance for the redundant cost.

Tracked as [issue #59](https://github.com/go-steer/core-agent/issues/59) — description tightening across all four wrappers is queued for v2.1.

### Failure pattern 2 — Flash subtask hallucination

**Symptom:** the subtask returns a digest with one or two fabricated file:line citations alongside the correct ones. Pro accepts the result without re-verification and surfaces the bad data to the operator.

**Cause:** Smaller models (Flash, Haiku) struggle with cross-corpus extraction (`agentic_grep`, `agentic_research`). They're fine at summarizing a single document you handed them (`agentic_read_file`, `agentic_fetch_url`), but the multi-step "search, rank, cite, summarize" workflow exceeds their precision budget after a few turns of internal exploration.

**Impact:** Bad data flows through to the operator. Depending on what they do with it, real downstream errors.

**Mitigations:**

1. **Tighten the subtask budget for grep/research.** The default `MaxTurns` for `agentic_grep` is 3; for `agentic_research` it's 5. Drop to 2 and 3 respectively if you observe hallucinations — fewer turns = less room to confabulate.

2. **Route the noisy wrapper to a more capable model.** `--agentic-small-model gemini-2.5-flash` is global today, but a v2.1 enhancement may add per-wrapper overrides. In library use, you can construct different `AgenticToolOpts` per wrapper.

3. **Add an `AGENTS.md` rule for the parent to spot-check:**
   ```markdown
   ## When using agentic_grep results

   The agentic_grep wrapper returns ranked file:line citations. Spot-check
   1-2 cited locations with bare read_file before acting on critical claims.
   Citations are advisory; verify when precision matters (e.g., proposing
   an edit).
   ```

Tracked as [issue #60](https://github.com/go-steer/core-agent/issues/60).

### Worked example: the cost-efficiency math

A real session from the 2026-05-29 smoke. Parent on `gemini-3.1-pro-preview-customtools`, subtasks on `gemini-2.5-flash`. Single user prompt: "use agentic_read_file to read internal/tui/update.go and tell me what message types it handles":

```
Session stats:
  Turns:      7
  Tokens:     47342 in / 764 out
  Cost:       $0.0738
  Models:     gemini-3.1-pro-preview-customtools (5 turns, 30822 in / 558 out, $0.0683)
            + gemini-2.5-flash (2 turns, 16520 in / 206 out, $0.0055)
```

- **Per-turn cost:** Pro = $0.0137, Flash = $0.0028. **~5x cheaper per turn on Flash.**
- **Subtask absorbed the heavy read** (16k input tokens), parent did the synthesis (5 turns of reasoning at 6k each).
- **Without `--agentic-small-model`:** the same workflow would have all 7 turns at Pro pricing — roughly $0.10 instead of $0.07. ~30% savings on a single tool-call-heavy request.

The savings compound on long sessions. A 50-turn debugging session that does 20 file reads via `agentic_read_file` instead of bare `read_file` saves significantly more — the parent's context stays smaller, prompt-cache hit rate stays higher, and the per-read cost is on Flash, not Pro.

See [Cost efficiency](/agent-design/cost-efficiency/) for more detailed cost-model breakdowns.

---

## Background subagents: choreography patterns

Background subagents are spawned via `spawn_agent` (the model can call it directly) or `/subagent <goal>` (operator-driven from the TUI). The parent gets back a subagent ID; the subagent runs in its own session; alerts and completion summaries flow back through the inbox.

### Pattern 1 — worker

One subagent against one task. The parent dispatches and continues; the subagent reports back when done.

**When:** the task is long-running and the parent has other work to do in parallel.

**Example:** "spawn a subagent to run the test suite and tell me if anything breaks; I'll keep working on the refactor."

**`AGENTS.md` framing:**
```markdown
## Background subagents

When the user asks for something that takes more than ~30 seconds to run
and produces a discrete result, spawn a background subagent for it rather
than blocking on the result yourself. Use spawn_agent with a focused goal.
```

### Pattern 2 — fan-out + collate

N subagents in parallel against related tasks. The parent collects their reports and synthesizes.

**When:** N independent items each need their own focused investigation, and you want them to run in parallel.

**Example:** "for each open PR, spawn a subagent to review it against our house style; when all reports come in, give me a ranked list of which need attention first."

**Choreography:**
- Parent spawns N subagents with `spawn_agent`, capturing each subagent ID.
- Parent uses `check_agent <id>` to poll; OR waits passively and processes `report_alert` events as they arrive in the inbox.
- After all N complete, parent synthesizes the reports into the operator-facing result.

**Failure modes:** if any single subagent goes off-script, its budget cap stops it independently. The other subagents keep running. The parent collates whatever did succeed.

### Pattern 3 — manager (recursive)

A subagent that itself spawns subagents. Often called "manager" or "coordinator."

**When:** the goal is high-level enough that decomposition itself is the work. Example: "investigate why our staging environment has degraded over the past week" — the subagent figures out what subtasks to spawn (look at deploys, look at infra changes, look at error rates, etc.).

**Caveats:** depth tracking is the operator's responsibility. Subagent A spawning subagent B spawning subagent C means three nested budget envelopes; you can run into cost-blowout situations if each level has generous budgets. Mitigations:

- Set tight budgets on the manager subagent. It shouldn't reason for 10 minutes before spawning its first child.
- Use the `--max-turns` and `--max-cost` flags on `spawn_agent` to bound each level.
- Audit the spawn tree via `list_agents` after the run.

### Pattern 4 — scheduled monitor

A subagent that wakes periodically to check something, posts an alert if it sees a problem, then defers until the next cycle.

**When:** monitoring tasks. "Watch the deploy queue every 5 minutes; alert if anything's stuck."

**Choreography:**
- Parent spawns the subagent with `--scheduler=default` (the default).
- The subagent's body uses the `schedule_next_turn` tool to defer until its next wake time.
- Each wake produces a brief turn that checks the thing and decides whether to alert.
- Alerts come through `report_alert` to the parent's inbox.

See the [Autonomous quickstart](/run/autonomous/quickstart/) for a worked example.

---

## Composition: agentic wrappers + subagents

The two mechanisms compose. A subagent can use the agentic wrappers internally; the agentic wrapper machinery doesn't care whether the caller is the parent or a subagent.

**Example:** a "code-review" background subagent that uses `agentic_read_file` to digest large files cheaply, then composes its review in its own context using the digests. Parent kicks it off with `spawn_agent`; the subagent's session uses `--agentic-tools --agentic-small-model gemini-2.5-flash` so its sub-subtasks run cheaply too.

The composition keeps the parent's context tiny (it just sees "spawned subagent, awaiting completion") while the subagent absorbs the bulk of the work at the best per-token price.

---

## Anti-patterns

| Pattern | Why it fails | Fix |
|---|---|---|
| Using `agentic_read_file` for small files (~< 200 lines) | Subtask overhead exceeds savings | Bare `read_file` for small files; the agentic wrapper pays off on bulk content |
| Spawning a subagent for a 5-second task | The async overhead exceeds the work | Inline it; subagents are for tasks measured in minutes |
| Letting the parent re-verify agentic_* digests by re-reading source | Defeats the wrapper's whole purpose | `AGENTS.md` rule to trust digests; see issue #59 |
| Using `agentic_grep` on a cheap model for code precision tasks | Flash hallucinates citations; see #60 | Use a more capable model for grep/research; tighten turn budget |
| Manager subagent with generous budgets at every level | Cost blowout; nested envelopes multiply | Tight budgets per level; audit with `list_agents` |
| Spawning N subagents without budget caps | One runaway can consume the entire session budget | Always `--max-turns` + `--max-cost` per spawn |
| Using subagents because they sound advanced | Adds complexity for no payoff if the task fits in the parent | Default to inline; subagents only when the use case justifies it |

---

## Where to go next

- **[Cost efficiency](/agent-design/cost-efficiency/)** — the per-turn cost models for wrappers + subagents; when the savings pay off
- **[Built-in tools](/concepts/tools/)** — the bare tools the wrappers are wrapping; description-text patterns you can mirror in your own tools
- **[Context management → Agentic wrappers](/concepts/context-management/)** — the mechanism + the four built-in wrappers
- **[Autonomous quickstart](/run/autonomous/quickstart/)** — background subagents + scheduling in unattended runs
- **[Autonomous → Operations](/run/autonomous/operations/)** — `RunAutonomous`, budgets, lifecycle tool, the spawn tools
- **[Issue #59](https://github.com/go-steer/core-agent/issues/59)** — agentic_* description tightening (v2.1 polish)
- **[Issue #60](https://github.com/go-steer/core-agent/issues/60)** — Flash hallucination on agentic_grep (v2.1 polish)
