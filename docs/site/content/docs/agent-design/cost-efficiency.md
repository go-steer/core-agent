---
title: Cost efficiency
weight: 4
---

What actually moves the cost needle on `core-agent` sessions, in rough order of impact. Built-in price tracking and per-model breakdowns make the tradeoffs measurable rather than guesswork; this page covers how to read those signals and the patterns that consistently reduce cost without sacrificing capability.

For the mechanism details see [Context management]({{< relref "reference/context-management.md" >}}) and [Configuration → pricing]({{< relref "reference/configuration.md" >}}).

---

## The cost hierarchy

A rough sense of where dollars go on a typical coding session:

| Driver | Typical share of session cost | Lever |
|---|---|---|
| Input tokens (cumulative across turns) | 70-90% | Compaction, checkpoints, agentic wrappers |
| Output tokens | 10-25% | Tighter `AGENTS.md` ("be concise"), structured output formats |
| Per-turn model rate | Multiplier on both | Model selection (Pro vs Flash for subtasks) |
| Subtask + compaction LLM calls | 2-10% | Bounded by infrastructure, not operator-tunable |

**The input-token share is the killer.** Every turn re-sends the entire conversation history to the model. By turn 30, you're paying input-token prices for everything that came before, plus the new turn. The single biggest lever on cost is preventing that input-token total from growing without bound — which is exactly what compaction, checkpoints, and agentic wrappers do.

---

## Lever 1 — Model selection (biggest impact)

Frontier models (Gemini Pro, Claude Opus) cost 5-15x more per token than Flash/Haiku-tier models. The "use Pro for everything" pattern is the most common source of accidentally expensive sessions.

### Pro+Flash split via agentic wrappers

The intended cost model for tool-heavy work: **parent on Pro/Opus for reasoning, subtasks on Flash/Haiku for content digestion.**

```bash
core-agent --agentic-tools --agentic-small-model gemini-2.5-flash
```

Real numbers from a smoke session — single user prompt, `agentic_read_file` call:

```
Models:  gemini-3.1-pro-preview-customtools  (5 turns, 30822 in / 558 out, $0.0683)
       + gemini-2.5-flash                    (2 turns, 16520 in / 206 out, $0.0055)
```

- Pro: $0.0137/turn. Flash: $0.0028/turn. **~5x cheaper per turn.**
- Flash absorbed the heavy file content (16k input tokens) at a fraction of what Pro would have charged for the same read.

On a 50-turn session with 20 tool-heavy reads, the same workflow costs roughly $1.50 with the split vs $4.50 without. The savings compound on longer sessions because the parent's context stays smaller (lower input-token growth, higher cache-hit rate — see Lever 2 below).

### When NOT to split

- **Trivial tool calls.** A `read_file` on a 50-line file isn't worth a subtask hop; the overhead exceeds the savings. The agentic wrappers' descriptions account for this — the model only routes to them for "might be large" cases.
- **Precision-sensitive tool use.** Flash hallucinates on cross-corpus search ([issue #60](https://github.com/go-steer/core-agent/issues/60)). For `agentic_grep`-style workflows where citation accuracy matters, either use a more capable subtask model or spot-check results in the parent.
- **Single-model affinity.** If your `AGENTS.md` is tuned to a specific model's behavior, mixing models complicates the tuning. Test cross-model interactions before committing.

### Picking the parent model

If cost matters more than capability:

| Workload | Recommendation |
|---|---|
| Heavy code analysis, multi-step debugging | Pro / Opus |
| Lightweight Q&A, doc explanation, simple edits | Flash / Haiku as the parent (no split needed) |
| Long autonomous monitor that's mostly polling | Flash as parent, with `--ask=auto` |
| Mixed: code work + bulk reads | Pro parent + `--agentic-small-model=flash` |

The price-per-million-tokens delta between Pro and Flash is so large that the right answer is almost never "Pro everywhere." Look at `/stats` after a representative session and compare what you're actually using.

---

## Lever 2 — Context management (sustained impact)

The three context-management mechanisms (compaction, checkpoints, agentic wrappers) compound each other:

- **Agentic wrappers** prevent bloat from entering the parent's context in the first place. Less to compact later; less to send on every turn.
- **Checkpoints** carve the session into focused chunks at natural task boundaries. Each new task starts with a clean working set.
- **Compaction** catches the residual growth between boundaries. Reactive backstop.

### What each costs

| Mechanism | Cost per fire | Frequency | Net savings |
|---|---|---|---|
| Agentic wrapper subtask | $0.005-0.05 per call (Flash) | Per tool call | Replaces a $0.02-0.20 Pro read |
| Checkpoint summary | $0.01-0.05 (single LLM call on the parent model) | Per `/done` or `mark_task_done` | Slices ~10-50k tokens out of subsequent prompts |
| Compaction summary | $0.01-0.05 (same shape) | Auto: per 85% utilization trigger; Manual: per `/compact` | Slices history-to-date out of subsequent prompts |

All three were cost-instrumented in v2.0 ([fix #61](https://github.com/go-steer/core-agent/issues/61)) — the summarizer calls show up in `/stats` and `/context` so you can measure the actual overhead.

### When the savings pay off

**Compaction breaks even after ~5 additional turns past the boundary.** A 30k-token compaction summary that replaces 50k tokens of pre-summary history costs ~$0.05 to produce; the savings are ~$0.0033/turn on Pro thereafter. So 5+ post-summary turns and you're ahead.

**Checkpoints break even after ~3 turns of the next task.** Smaller win per fire than compaction, but you also avoid the model getting confused by stale context — quality dividend on top of the cost dividend.

**Agentic wrappers break even on every call** when the file is larger than ~500 lines. Smaller than that, the bare tool is cheaper.

### Disable when they wouldn't fire

For short headless one-shots (`core-agent -p "..."`) compaction and checkpoints would never fire — the session ends before either threshold is crossed. The flags `--no-compact` and `--no-checkpoint` skip the wiring entirely; saves a few KB of context budget that's otherwise spent on the auto-wired `mark_task_done` tool description.

---

## Lever 3 — Prompt caching

Both Anthropic and Google support prompt caching: stable prefix content is hashed and a cached version reduces input-token billing for subsequent turns. The agent's system instruction + the early turns of a session are usually the cache hot path.

### What helps cache hits

- **Stable `AGENTS.md`.** Every word counts as cache material. Editing `AGENTS.md` mid-session invalidates the cache.
- **Consistent skill descriptions.** Same caveat — they're part of the system context the cache spans.
- **Compaction summaries land as `RoleUser`** which means they sit BEFORE the latest turn but AFTER the cached system prefix. The cache hit rate stays high because the prefix doesn't move; only the user/model turn pair changes.
- **`tools.disable`** for tools the agent doesn't need — reduces the system-prompt-side schemas, leaving more budget for cache-warm prefix.

### What hurts cache hits

- **Switching models mid-session** (e.g., `/model gemini-2.5-flash` after starting on Pro). The cache is per-model.
- **Adding/removing MCP servers via `/reload`.** Tool schemas change → cache invalidates.
- **`/compact`** triggers a model call that DOES warm the cache for the next turn. Just be aware: the compaction itself isn't cached (first time the model sees the summarization prompt + full history), but subsequent turns hit the post-summary cache cleanly.

### What's measurable

`/stats` and `/context` don't currently break out cache-hit vs cache-miss tokens — that detail lives in the provider's response metadata. For now, the proxy is "input-token totals grew linearly with turn count" (no caching) vs "grew sub-linearly" (caching working). Direct cache-hit accounting is queued behind provider-side support.

---

## Lever 4 — Output token shape

Output tokens are usually 10-25% of total cost, but they're 4-6x more expensive per-token than input on most models. Tightening output is the cheap fix when the model is being unnecessarily verbose.

### `AGENTS.md` patterns that reduce output

```markdown
## Output style

- Default to concise. Plain prose, no bullet lists unless explicitly asked.
- Cite file:line; don't paste more than 10 lines verbatim unless asked.
- One-paragraph explanations for "how does X work" questions. Expand on request.
- Skip social niceties ("Sure, here's...", "I hope this helps!").
```

### Structured output formats

If your workflow produces predictable output (a review, a checklist, a status report), specify the format:

```markdown
Output a structured review with these three sections:

✅ Things done well (1-2 lines each, max 5)
⚠️ Issues to fix before merging (file:line + suggested change)
💡 Optional improvements (separate section, don't block on these)
```

The model defaults to natural-language verbosity; a fixed format clips it down.

### Avoid open-ended "explain X" requests

"Explain how authentication works" produces ~500 output tokens. "What file handles authentication and which function is the entry point?" produces ~50. Both are useful queries; the latter is 10x cheaper.

---

## Reading `/context` for cost decisions

`/context` is the operator surface for "where's my budget going":

```text
Context-management activity:
  Compactions:  1 (last 4m12s ago, focus: auth module)
  Checkpoints:  3 (last 51s ago, note: finished surveying messageKinds)
  Summarized:   8420 chars across all boundaries
  Subtasks:     2 (32919 in / 338 out tokens, $0.0107 rolled up to /stats total)
  Models:       gemini-3.1-pro-preview-customtools (5 turns, 30822 in / 558 out, $0.0683)
              + gemini-2.5-flash (2 turns, 16520 in / 206 out, $0.0055)
```

**Things to look for:**

- **Models row shows the Pro+Flash split is working** if Flash has a meaningful turn share. If Flash is 0 turns but you passed `--agentic-tools`, the model isn't routing through the wrappers — strengthen the `AGENTS.md` framing (see [Subagents and wrappers]({{< relref "agent-design/subagents-and-wrappers.md" >}})).
- **Subtasks row vs. Models row Flash entry should match** — both come from the same accounting; they're cross-checks on each other.
- **Compactions count vs. session length:** if you've been running for an hour with no compactions, either your context isn't large enough to need one (good) or compaction isn't wired (`--no-compact` was passed).
- **Summarized chars total** tells you how much history-collapse is in play. Low number on a long session = either no compactions or no checkpoints fired.

---

## Common cost mistakes

| Mistake | What it costs | Fix |
|---|---|---|
| Default model is Pro/Opus, no `--agentic-small-model` | 30-50% over-spend on tool-heavy work | `--agentic-tools --agentic-small-model gemini-2.5-flash` |
| Long `AGENTS.md` repeating default instruction guidance | 5-10% over-spend across all turns + worse model attention | Strip rules covered by `agent.DefaultInstruction` |
| Tools enabled the agent never uses | 2-5% over-spend on every turn (schema in prompt) | `tools.disable` the unused ones |
| Switching models mid-session | Cache invalidation, ~10-30% input-token over-spend per turn after switch | Pin the model in `config.json`; switch only when intentional |
| Output verbose by default | 5-15% over-spend on outputs | `AGENTS.md` rule: "default to concise" |
| Disabling compaction "just in case" | Long sessions hit context wall ($BIG cost or session death) | Leave compaction on; disable only for known-short runs |
| Re-reading files the agent already digested via agentic_* | Pro-priced redundant reads | `AGENTS.md` rule: "trust digests; don't verify with bare read_file" |

---

## A rough decision tree

**"My session is more expensive than I expected."** Run `/context`. Check:

1. Is the Models row showing Flash? If not, the wrappers aren't being used → fix the prompt or restart with `--agentic-tools`.
2. Is the Compactions count zero on a long session? Either the session isn't large enough (probably fine) or you have `--no-compact` somewhere (fix the config).
3. Is the per-turn input growing without bound? Use `/compact` manually; consider whether a `/done` boundary is appropriate.

**"I'm starting a new project and want to minimize cost from day 1."**

1. Pin a model in `config.json` — don't rely on auto-detection
2. Default to `Pro+Flash split`: `--agentic-tools --agentic-small-model gemini-2.5-flash` (or `claude-haiku-4-5` on Anthropic)
3. Write a concise `AGENTS.md` with an output-style section ("default to concise")
4. Leave compaction + checkpoints on (default-on)
5. Disable tools the agent doesn't need with `tools.disable`

**"I'm running an autonomous long-running monitor."**

1. Parent on Flash, not Pro — monitors don't need frontier reasoning
2. Tight budgets: `WithMaxCost`, `WithMaxWallclock`, `WithPerTurnTimeout`
3. `--ask=auto` so the agent can refuse cleanly when it would otherwise block on a prompt
4. Read-only tool surface where possible (`tools.disable` + permission allow patterns)
5. Skip `--agentic-tools` — monitors do small reads, not bulk content digestion. The wrapper overhead doesn't pay off.

---

## Where to go next

- **[Context management]({{< relref "reference/context-management.md" >}})** — the underlying mechanisms; library API for `WithCompactor` / `WithCheckpointer`
- **[Subagents and wrappers]({{< relref "agent-design/subagents-and-wrappers.md" >}})** — design patterns for `agentic_*` + background subagents
- **[System instructions]({{< relref "agent-design/system-instructions.md" >}})** — `AGENTS.md` patterns including output-style rules
- **[Configuration → pricing]({{< relref "reference/configuration.md" >}})** — `pricing.refresh`, `pricing.source`, per-model override; the layered catalog
- **[Providers]({{< relref "reference/providers.md" >}})** — model IDs, per-backend pricing notes
