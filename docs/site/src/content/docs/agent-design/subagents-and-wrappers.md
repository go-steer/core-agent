---
title: Subagents and wrappers
---

Three ways to push work off the parent agent:

- **Agentic tool wrappers** (`agentic_read_file`, `agentic_grep`, `agentic_research`, `agentic_fetch_url`) — synchronous, bounded, single-purpose. The parent calls them like any other tool; under the hood they spawn a focused subtask on a (typically cheaper) model and return only the digest. Raw tool output never enters the parent's context.
- **Background subagents** (`spawn_agent`, `stop_agent`) — asynchronous, longer-running, multi-turn, decided at runtime. The parent dispatches a goal; the subagent works in its own session until done; alerts and completion summaries are **pushed** back into the parent's chat (the `[Background reports]` block on its next turn), so the parent doesn't poll. Need to block on a result inline? Use `spawn_agent { wait: true }`.
- **Declarative subagents** (`subagents[]` in `config.json`, v2.9+) — a fixed roster of named delegates authored ahead of time. Each becomes a named tool on the parent, with its own persona, model, and a name-scoped slice of the parent's tool/MCP/skill surface. Same in-process substrate as `spawn_agent`, but the roster ships in the config rather than being invented at runtime — so it deploys as one ConfigMap.

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

Background subagents are spawned via `spawn_agent` (the model can call it directly) or `/subagent <name> <goal>` (operator-driven from the TUI, referencing a configured subagent). The parent gets back a subagent ID; the subagent runs in its own session; alerts and completion summaries flow back through the inbox.

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
- Parent spawns N subagents with `spawn_agent`, capturing each subagent name.
- Parent waits passively; each subagent's alerts and completion summary are pushed into the parent's next turn (the `[Background reports]` block) as they finish — no polling. To block on a single result inline, spawn it with `wait: true`.
- After all N complete, parent synthesizes the reports into the operator-facing result.

**Failure modes:** if any single subagent goes off-script, its budget cap stops it independently. The other subagents keep running. The parent collates whatever did succeed.

### Pattern 3 — manager (recursive)

A subagent that itself spawns subagents. Often called "manager" or "coordinator."

**When:** the goal is high-level enough that decomposition itself is the work. Example: "investigate why our staging environment has degraded over the past week" — the subagent figures out what subtasks to spawn (look at deploys, look at infra changes, look at error rates, etc.).

**Caveats:** depth tracking is the operator's responsibility. Subagent A spawning subagent B spawning subagent C means three nested budget envelopes; you can run into cost-blowout situations if each level has generous budgets. Mitigations:

- A subagent cannot spawn *itself*. `spawn_agent` refuses a reference to any configured subagent already running as an ancestor of the call and tells the model why, so it reroutes onto doing the work rather than retrying. The match is on the configured name, not the instance name — `cluster-1` asking for another `cluster` is refused. This is a separate bound from the depth cap, which by definition can't see recursion that stays shallow: a subagent respawning itself sits at depth 1 under any cap.

- Set tight budgets on the manager subagent. It shouldn't reason for 10 minutes before spawning its first child.
- Use the `--max-turns` and `--max-cost` flags on `spawn_agent` to bound each level.
- Audit the spawn tree out-of-band via the attach hub's `GET .../agents` endpoint or the TUI (operator surfaces), not a model tool.
- When one of them misbehaves, read its turns: `GET .../agents/{name}/events` returns that subagent's persisted inner turns, nested descendants included. `/agents` tells you a subagent is running and what it last reported; this is how you see *why* it looped. In either TUI, `/subagents <name>` opens the same turn log without the curl — and while a *sync* subagent's call is in flight, its tool row tails the newest turns inline.

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

## Declarative subagents: a fixed roster in config

Background subagents are decided at runtime — the parent (or operator) picks a goal and spawns a worker on the fly. **Declarative subagents** invert that: you author a fixed roster in `config.json`, and each entry becomes a named tool the parent delegates to by name. Same in-process substrate (`agent.WithSubagents`); the difference is *when* the roster is decided.

```jsonc
{
  "model": { "provider": "vertex", "name": "gemini-3.5-flash" },
  "subagents": [
    {
      "name": "cluster",
      "description": "Read-only cluster investigator. Delegate GKE reads here.",
      "instructions": "@include ./personas/cluster.md",
      "mcp": ["gke-readonly"],
      "skills": ["fleet-audit"]
    }
  ]
}
```

**When to reach for it over `spawn_agent`:**

| Question | Answer |
|---|---|
| Is the set of delegates known ahead of time and stable? | Declarative roster |
| Does the deployment need to be a single reproducible artifact (ConfigMap, image)? | Declarative roster |
| Does each delegate need a *narrower* tool surface than the parent (least privilege)? | Declarative roster — scope its `tools`/`mcp`/`skills` |
| Does the parent decide *at runtime* what to delegate, or spawn N-of-something? | `spawn_agent` |
| Is it a one-off, ad-hoc task? | `spawn_agent` |

**Least-privilege scoping.** Each of `tools`, `mcp`, and `skills` narrows one dimension of the parent's surface, following a nil / list / empty contract: **omit** the field to inherit the parent's full set, give a **non-empty list** to scope to exactly those names, or give an **empty list** (`[]`) to grant none of that dimension. This is the main design payoff over inheriting everything — a read-only `cluster` delegate can see `gke-readonly` while the parent keeps the read-write `gke` server, without a second MCP process or a nested config tree. Scoping reuses the parent's already-started MCP toolsets and a filtered view of its loaded skills (no re-walk), and every inherited tool keeps the parent's permission gate, so a subagent cannot escalate.

Inheritance has exactly one carve-out, and it is about *delegation* rather than privilege: `tools: <omitted>` grants the parent's registry minus `spawn_agent` and `stop_agent` (v2.9+). The gate is what makes inheriting a tool safe, and it has no opinion about a subagent starting another agent — so a delegate that inherits everything can quietly become a fleet parent. Name the spawn tools in `tools:` to build a deliberate orchestrator subagent; otherwise the boot line reports `spawn=withheld` and the delegate does its own work.

**Independent content with `root`.** Inline `tools`/`mcp`/`skills` can only *narrow* the parent's surface — a subagent can't hold a skill or server the parent doesn't also load. When a delegate needs its **own** persona, skills, or MCP servers that the parent must **not** have, point it at a content root:

```jsonc
{
  "subagents": [
    {
      "name": "cluster",
      "description": "Read-only investigation of a single GKE cluster.",
      "tools": ["read_file", "grep"],   // built-ins — always inline
      "root": "../cluster"              // own scope: AGENTS.md + skills/ + mcp.json
    }
  ]
}
```

With `root` set, the subagent loads its persona from `<root>/AGENTS.md` (an inline `instructions` still overrides it), its skills from `<root>/skills/`, and its MCP servers from `<root>/mcp.json` — none of which the parent loads. The same nil / list / empty contract still applies, but now it filters **within the root**: omit `skills` to get all of the root's skills, or list a subset. Built-in `tools` stay inline (they live in the binary, not a directory). A relative `root` resolves like a [content root](/reference/configuration/) (against the agents dir, else the cwd); a missing directory is a loud startup error. This is what lets sibling recipe trees — `agents/platform/` (the fleet parent) and `agents/cluster/` (a read-only specialist) — ship under one mounted image with cleanly separated personas and skills.

**Invoke it sync *or* async (v2.9+).** A declarative subagent isn't sync-only. Every roster entry — including a rooted one with its own MCP + skills — is also spawnable by reference through the unified `spawn_agent` surface, so the parent can choose per call:

```jsonc
spawn_agent { agent: "cluster", goal: "<a quick triage plan>" }             // async: fire-and-continue, report pushed later
spawn_agent { agent: "cluster", goal: "...", wait: true }                   // sync: block this turn, return the result inline
spawn_agent { agent: "cluster", goal: "triage pool A" }                     // fan out the same spec N times with different goals
spawn_agent { agent: "cluster", goal: "...", model: "small" }              // narrow-only override: downshift the tier
```

`wait: true` reproduces the classic named-tool delegation (block, get the answer), capped by a tighter sync wall-clock so a slow subagent can't hang the parent turn — on timeout the subagent keeps running and its result is pushed later. A wait that succeeds delivers the result **once**, inline: the redundant `[Background reports]` echo on the next turn is suppressed (v2.9+). Omitting `wait` is fire-and-continue: the parent handles other work and receives the subagent's report on a subsequent turn.

The inline result carries the subagent's completion report as `output`, plus its last assistant text as `final_text` when that text says something the report doesn't. "Last" here means the last *substantive* turn — the last one that both said something and used a tool (v2.9+) — so a subagent that answered early and then idled returns its findings rather than its idling. A wait that **times out** delivers the same two pieces through the pushed report instead, with the last assistant text appended under the same `final_text` label — the two delivery paths carry the same content, so a subagent that ran long doesn't hand the parent less than a fast one would. Spawned subagents are told that the report is the deliverable — write the findings, not "investigated the issue and found the cause" — because the parent cannot see the subagent's work and a status line forces it to redo the task. That contract lives in the subagent's **system instruction**, not only in a tool description, because on a budget cap, a watchdog halt or a natural stop no completion tool is called at all and the last assistant text is what the parent gets. Both delegation paths install it (`agent.SubagentReturnContract`), so the same declared subagent isn't told two different things depending on whether it was reached as a tool or spawned — the rendering names the return tool only where one exists, and otherwise points at the last message. Overrides are **narrowing-only** — you can drop the tier to `small` or tighten budgets, never widen the grant or name a different model (configure a dedicated spec for that). Operators can spawn the same roster by name from the TUI with `/subagent <name> <goal>`. See [Unified subagent invocation](https://github.com/go-steer/core-agent/blob/main/docs/unified-subagent-invocation-design.md) for the design.

### How a delegation ends (v2.9+)

A spawned subagent is one of two things, and they want opposite termination rules.

A **bounded delegation** is handed one task. `agent.Run` is already a terminating loop — model, tool, model, until the model stops asking for tools — and for a subagent with a deliverable, that *is* completion. So the run ends there and the turn's last message is the deliverable. This is the default.

A **standing worker** is a loop that watches something. A turn with no tool calls means *idle*, not *finished*, so the driver injects the continuation prompt and keeps going until a budget fires, the scheduler defers it, or the model calls the return tool.

Both get `return_result(result)` — registered under the aliases `report_done`, `report_completed` and `mark_task_done` so any name the model reaches for both delivers the payload and ends the run. An empty result is refused with a corrective ack rather than terminating the delegation with nothing in hand. For a standing worker the tool is the only way out short of a budget; for a bounded one it is the *preferred* way out, ranked above the natural end, so a model that calls it returns a curated result instead of whatever text it happened to stop on. A bounded delegation that never calls it still ends — nothing can hang by being forgotten.

Bounded briefly shipped without the return tool, on the argument that one exit is simpler than two. Because bounded is the default, that left the alias net covering only the path models rarely take: a GKE triage subagent finished its analysis, called `mark_task_done`, and was told it had hallucinated the tool. Two exits are fine when they are ordered.

Which one you get is derived from the scheduler: a subagent that can ask to be re-run later (`scheduler: "sleep"`, `"exit_on_defer"`, or a manager-level default scheduler) is standing; everything else is bounded. Embedders can override it explicitly with `Spec.Mode` / `SubagentTemplate.Mode` (`background.ModeBounded` / `background.ModeStanding`).

Before v2.9 every spawn ran the standing loop, so a delegation that had already answered was re-driven with `"continue"` until its turn cap — running past its own answer and overwriting it each time. A GKE triage subagent produced a correct root cause and patch on turn 1, then spent six more turns scope-creeping and returned "standing by in a healthy, inactive state" to its parent, at ten times the cost of the correct answer.

Because a bounded delegation is no longer re-driven, one that runs out of room hands back a **partial** — which is the right contract, since the parent holds the goal and can re-ask with specifics, where a blind `"continue"` inside the subagent knows nothing about what is missing. The result carries a machine-readable `stop_reason` so the parent can tell the two apart: `natural` (finished), `max_steps` / `budget` (ran out of room — re-ask or raise the cap), `deferred` (it will resume on its own), `stopped`, or `error`. A pushed report spells the reason out too, but only when it isn't `natural` — a report that completed without one is a finished result, and a trailer on the common case would be noise on every bullet in the parent's next prompt.

If you force `ModeBounded` onto a subagent that *does* have a scheduler, an explicit `schedule_next_turn` still wins: asking to be woken again is a choice the model made, where the natural end is inferred from a choice it didn't make.

**The parent discovers the roster from the schema, not from its persona (v2.9+).** `spawn_agent`'s description lists every configured subagent as `name — description`, and `agent` is constrained to an enum of exactly those names (the enum is dropped when the operator enabled ad-hoc spawns, since an ad-hoc spawn leaves `agent` empty). So a parent persona that never mentions `cluster` can still route a cluster-scoped task to it, and a persona reused across fleets picks up each fleet's roster without editing. Write each `description` as *when to delegate here* — it is the only routing signal the parent gets.

See [Reference → Declarative subagents](/reference/configuration/#declarative-subagents-v29) for the full field schema, and [`examples/kube-platform-agent/`](https://github.com/go-steer/core-agent/tree/main/examples/kube-platform-agent) for a recipe that delegates GKE reads to a scoped `cluster` subagent.

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
| Manager subagent with generous budgets at every level | Cost blowout; nested envelopes multiply | Tight budgets per level; audit the spawn tree via the attach hub / TUI |
| Spawning N subagents without budget caps | One runaway can consume the entire session budget | Always `--max-turns` + `--max-cost` per spawn |
| Using subagents because they sound advanced | Adds complexity for no payoff if the task fits in the parent | Default to inline; subagents only when the use case justifies it |

---

## Where to go next

- **[Cost efficiency](/agent-design/cost-efficiency/)** — the per-turn cost models for wrappers + subagents; when the savings pay off
- **[Built-in tools](/concepts/tools/)** — the bare tools the wrappers are wrapping; description-text patterns you can mirror in your own tools
- **[Context management → Agentic wrappers](/concepts/context-management/)** — the mechanism + the four built-in wrappers
- **[Autonomous quickstart](/run/autonomous/quickstart/)** — background subagents + scheduling in unattended runs
- **[Autonomous → Operations](/run/autonomous/operations/)** — `autonomous.Run`, budgets, lifecycle tool, the spawn tools
- **[Issue #59](https://github.com/go-steer/core-agent/issues/59)** — agentic_* description tightening (v2.1 polish)
- **[Issue #60](https://github.com/go-steer/core-agent/issues/60)** — Flash hallucination on agentic_grep (v2.1 polish)
