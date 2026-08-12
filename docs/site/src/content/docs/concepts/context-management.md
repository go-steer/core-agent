---
title: Context management
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

**The reactive backstop.** When the context window fills past a per-model-tier threshold (default `0.85` for frontier, `0.65` for mid, `0.35` for small-tier models since v2.5), the agent automatically compacts the conversation into a "teammate handover" summary and slices the pre-summary history out of future requests. The full audit log is preserved on disk — only the live LLM request is sliced.

### How it fires

- **Automatic:** post-turn hook checks utilization against the configured per-tier threshold; when over, the next `Run` drains a `compactionPending` flag by writing the summary before its actual work. The operator-visible turn boundary stays clean — no surprise latency cliff after the assistant finishes.
- **Manual:** `/compact [focus]` runs the same summarizer immediately. The optional `focus` argument biases the summary toward a particular thread when you want to preserve specific context.

### Per-tier thresholds (since v2.5)

A single 0.85 threshold worked for frontier-tier models (Opus, Pro) but fired far too late for small-tier models (Flash, Haiku) — reasoning quality on those tiers degrades well before they reach 85% context utilization. The per-tier defaults trigger earlier on smaller models so the session stays inside its effective working range:

| Tier | Default trigger | Examples |
|---|---|---|
| `frontier` | `0.85` (unchanged) | `claude-opus-4-*`, `gemini-3.x-pro`, `gemini-3.6-flash` |
| `mid` | `0.65` | `claude-sonnet-4-*`, `gemini-3.5-flash`, `gemini-2.5-pro` |
| `small` | `0.35` | `claude-haiku-4-*`, `gemini-3.5-flash-lite`, `gemini-3.1-flash`, `gemini-2.5-flash` |

Tier classification is by substring match against the model ID — see `pkg/modeltier`. Unknown models fall back to the single `compaction.threshold` setting (default `0.85`).

Override per-tier defaults in `.agents/config.json`:

```json
{
  "compaction": {
    "threshold": 0.85,
    "threshold_by_tier": {
      "small": 0.30
    }
  }
}
```

Only set tiers you want to override; the rest take their substrate defaults.

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

## Task class (since v2.5)

`--task=<class>` is a single flag that picks a coherent bundle of defaults tuned for the kind of work the operator is sitting down to do. Operator-declared (not LLM-inferred) — the operator knows whether they're debugging or chatting; asking them to type one flag is cheaper and more predictable than any classifier we could build.

Five classes ship today:

| Class | Default model tier | Compaction threshold | Ask mode | When to use |
|---|---|---|---|---|
| `debug` | frontier (e.g. `claude-opus-4-7`, `gemini-3.6-flash`) | `0.65` | `auto` | Bug hunts, root-cause investigations, multi-file traces |
| `implement` | frontier | `0.70` | `auto` | Feature work, multi-file refactors |
| `chat` | mid (e.g. `claude-sonnet-4-6`, `gemini-3.5-flash`) | `0.85` | `auto` | Q&A, pairing, lightweight design discussion |
| `research` | mid | `0.65` | `allow` | Read-heavy codebase exploration; `allow` keeps the ask-mode noise out of the way |
| `review` | frontier | `0.75` | `auto` | PR / diff review |

Resolution per-provider:

| Tier | Gemini / Vertex | Anthropic |
|---|---|---|
| frontier | `gemini-3.6-flash` | `claude-opus-4-7` |
| mid | `gemini-3.5-flash` | `claude-sonnet-4-6` |
| small | `gemini-3.5-flash-lite` | `claude-haiku-4-5` |

Explicit per-knob flags always win over the class defaults:

- `--model` (long-form alias of `-m`) pins the model — e.g. `--task=debug --model=gemini-3.6-flash` uses debug-mode defaults but a specific model. A model set in the config file (`model.name`) is likewise respected; `--task` only fills in the tier model when neither `--model` nor a config-file model is set.
- `--compaction-threshold=<0..1>` pins the post-turn compaction trigger, overriding both the config-file `compaction.threshold` and the class default.
- `--ask=off|stdin|auto` pins the ask-user mode; left unset, the class default applies.

Config-file equivalent:

```json
{ "session": { "task_class": "debug" } }
```

Useful for project-local defaults (an infra repo where debugging is the typical workload sets `task_class: debug` once and operators don't have to remember).

### What `--task` does NOT change

- **Agentic-tools** — already on by default since v2.1; every task class wants it on.
- **`--agentic-small-model`** — per-provider default already picked by [#122](https://github.com/go-steer/core-agent/issues/122).
- **Per-tier compaction thresholds** in `compaction.threshold_by_tier` config — those still win for their specific tier even when a task class sets the fallback `Threshold`. Operators who've carefully tuned per-tier thresholds keep them.

### Small-tier-parent guard (since v2.5)

The `--task` flag picks a sensible model tier for each class, but explicit `--model` always wins. When the operator's explicit choice (or their config-file default) lands on a small-tier model (Flash, Haiku, etc.) for the *parent*, a startup-time guard fires by default ([#121](https://github.com/go-steer/core-agent/issues/121)):

```
core-agent: small-tier parent: gemini-2.5-flash is a small-tier model. Small-tier
  models work well as subtask workers (--agentic-small-model) but loop and stall
  as the parent for long interactive sessions. Consider a frontier or mid-tier
  model for the parent — e.g. --model gemini-3.6-flash --agentic-small-model
  gemini-2.5-flash. Pass --small-tier-parent=allow to suppress this notice.
```

Modes:

| `--small-tier-parent` | Behavior |
|---|---|
| `warn` (default) | Logs the notice and proceeds. |
| `refuse` | Exits with config-error code. Useful for supervised deploys. |
| `allow` | Suppresses the check entirely. |

Skipped regardless when `-p` (one-shot — operator may be scripting Flash on purpose), `--yolo` (trust-the-operator), or the resolved model's tier doesn't classify (unknown / future model).

Config-file equivalent: `safety.small_tier_parent`. CLI overrides config; default is `warn`.

The 2026-06-08 smoke that motivated this guard burned ~$80 across three sessions on `gemini-3.5-flash` as the parent — the same bug an Opus-tier session found in a handful of turns.

---

## Cost ceiling (kill switch — since v2.5)

Compaction and watchdog signals catch *behavioral* runaway (context fill, repeated tool calls). They don't bound the *outcome* — a model can produce many tool calls in a single turn before any post-turn check fires. The cost ceiling is the dollar-denominated guard for that case.

Two bounds, both optional, both off by default:

| Bound | CLI flag | Config field | What it caps |
|---|---|---|---|
| Per-turn | `--max-turn-cost-usd=<N>` | `agent.max_turn_cost_usd` | Cumulative spend of a single conversation turn (every model call + subtask between one operator inject and agent-done state) |
| Per-session | `--max-session-cost-usd=<N>` | `agent.max_session_cost_usd` | Cumulative spend across all turns since the agent started |

### What happens when a ceiling trips

1. The post-turn hook computes session cost (from the usage tracker) and per-turn delta (against a snapshot taken at turn start).
2. If either configured bound is met or exceeded, the agent emits a structured `turn-error` event with `kind=cost_ceiling`, message describing the spend + bound, and `retryable=false`.
3. A flag is set; the next `Run` call returns the same error immediately without invoking the model.
4. The operator clears the flag to resume — `/guardrail reset` in the TUI, `POST /sessions/{id}/guardrails/reset` over attach ([#666](https://github.com/go-steer/core-agent/issues/666)), or `Agent.ResetCostCeiling()` when embedding the library.

A **per-session** trip needs more than a bare reset. The accumulator is already at or past the ceiling, so clearing the flag alone re-trips on the very next turn — the reset surface refuses that case outright (HTTP **409**) and asks for `additional_budget_usd`, which RAISES the ceiling. It never zeroes the accumulator or restarts a spend window: `/usage`, the eventlog-derived cost, and the ceiling check all keep counting the same dollars, so a session that spent $12 still reports $12 after the operator hands it another $5 of runway. A **per-turn** trip needs no budget — the next turn starts from a fresh baseline.

### Resetting a tripped guardrail

Both backstops share one recovery surface ([#666](https://github.com/go-steer/core-agent/issues/666)):

| Surface | Read state | Reset |
|---|---|---|
| In-process TUI | `/guardrail` | `/guardrail reset [watchdog\|cost_ceiling\|all] [+<usd>]` |
| Attach HTTP | `GET /sessions/{id}/guardrails` | `POST /sessions/{id}/guardrails/reset` (body optional) |
| Library | `Agent.WatchdogTripped()` / `Agent.CostCeilingTripped()` | `Agent.ResetWatchdog()` / `Agent.ResetCostCeiling()` + `Agent.AddSessionCostBudget(usd)` |

`/guardrail` with no arguments prints what is armed, what tripped, why, and — when a bare reset would re-trip — how much budget to add. The reset is `SessionWrite`, not admin: the next thing an operator does after clearing a halt is `POST /inject`, which is itself `SessionWrite`, so gating the reset harder would buy no safety.

#### Halts survive a restart

A halt that a restart clears is not a halt. Since v2.9.0-dev ([#643](https://github.com/go-steer/core-agent/issues/643)) both trips — and the operator resets that clear them — are written to the eventlog and folded forward by the next process over the same session. A crash, an OOM kill, or a pod roll no longer hands a runaway loop a fresh budget, which matters most for exactly the unattended deployments [#642](https://github.com/go-steer/core-agent/issues/642) turned these backstops on for. Budget an operator granted before the restart is preserved too, so a resumed session doesn't re-halt at the old bar.

Restored state never overrules live configuration: a daemon restarted with `--watchdog=warn` does not resurrect an enforce-mode halt, and granted budget is not applied to a per-session ceiling that is no longer configured. Restore also fails *open* — if the guardrail history can't be read, the session runs rather than being bricked by a transient database error. Durability requires a session store (`--session-db` / `WithEventLog`); with no eventlog, behavior is unchanged.

### Why "stop, get attention" instead of throttle

A cost-ceiling trip almost always means *something is wrong* — a tool-call loop ([#144](https://github.com/go-steer/core-agent/issues/144)), a model going off the rails, an unexpectedly expensive prompt. Auto-resume would just continue burning budget. The explicit operator reset forces a human look-in.

### Defaults and posture

The per-turn bound is **off by default**. The per-session bound is **off for interactive runs and `$10.00` for unattended runs** — `-p` one-shot, a `--no-repl` daemon, or any run whose stdin isn't a TTY ([#642](https://github.com/go-steer/core-agent/issues/642)). An unattended agent has nobody watching the spend, so "off until configured" meant every deploy that forgot the flag ran unbounded.

To opt an unattended run back out, say so explicitly — an explicit `0` from either source beats the default:

```bash
core-agent -p "..." --max-session-cost-usd=0        # flag
# or  "agent": { "max_session_cost_usd": 0 }        # config
```

Two recommended starting postures:

```bash
# Interactive desktop / dev — bound a single turn so a runaway can't
# burn more than a coffee's worth before refusing
core-agent --max-turn-cost-usd=0.50

# Long-running autonomous deploy — bound the whole session so a slow
# burn over hours doesn't quietly exceed the deploy's budget
core-agent --no-repl --attach-listen=127.0.0.1:7777 \
  --max-turn-cost-usd=1.00 --max-session-cost-usd=20.00
```

Tune from your own usage — `/stats` shows current session cost; pick bounds at ~5x your normal turn / session spend so genuine work doesn't trip.

### Composition with the other mechanisms

- **Compaction** (above) caps context not money.
- **Cost ceiling** caps money regardless of why.
- **Watchdog** (below) catches behavioral patterns (repeated identical tool calls) without waiting for the dollar count to add up.

All three are complementary. Both the session cost ceiling and the watchdog resolve their default by mode: active backstops when unattended, advisory when an operator is at the keyboard.

---

## Watchdog (behavioral observer — since v2.5)

Compaction caps the *context* dimension. The cost ceiling caps the *dollar* dimension. The watchdog catches the *behavioral* dimension — a session going off-rails (e.g. an agent stuck calling `read_file` on the same path five times in a row, the [#144](https://github.com/go-steer/core-agent/issues/144) pattern) before the dollar count gets large enough to trip the cost ceiling.

### Modes

| Mode | What it does |
|---|---|
| `warn` | Observes the tool-call stream. When a signal trips, logs a structured alert to the operator via the normal status channel (`send()` callback for CLI; future SSE event for attach-mode). Does NOT pause the turn. |
| `enforce` | Same detection as warn, but a Critical runaway signal (today: `repeated-tool-call`) **halts the agent**: it emits a `turn-error` (`kind=watchdog`, non-retryable) and refuses new turns until the operator clears it (`/guardrail reset`, `POST /sessions/{id}/guardrails/reset`, or `Agent.ResetWatchdog` when embedding). This is the hard behavioral backstop — an auto-continue re-drive of the interrupted turn is refused at pre-flight instead of re-issuing the looping call. |
| `off` | No observation. |

### Choosing the mode

Precedence is `--watchdog` > `safety.watchdog` > a mode-dependent default, mirroring `--small-tier-parent` / `safety.small_tier_parent`:

```json
{ "safety": { "watchdog": "enforce" } }
```

The config field ([#660](https://github.com/go-steer/core-agent/issues/660)) exists so a recipe is a self-contained unit — before it, `--watchdog` was CLI-only, so a hardened recipe still depended on every deploy manifest and every `core-agent -c ...` invocation remembering the flag by hand.

With neither source set, the default is **`enforce` for unattended runs** (`-p`, `--no-repl`, or a non-TTY stdin) and **`warn` for interactive REPL/TUI runs** ([#642](https://github.com/go-steer/core-agent/issues/642)). The split is about who reads the alert: an interactive operator sees the warning and can hit Ctrl-C, so halting on their behalf is presumptuous. Nobody reads a daemon's warn-mode log in time, which made warn indistinguishable from off exactly where the backstop mattered. Pass `--watchdog=warn` (or set the config field) to restore observe-only on an unattended run.

The resolved mode applies to every agent the process hosts, including the sessions a multi-session daemon creates through `POST /sessions`. The startup line names the source it came from, e.g. `watchdog: enforce mode [unattended default]`.

Enforce mode mirrors the cost ceiling's halt contract (above): post-turn detection sets a flag, the next `Run` refuses at pre-flight, and recovery is operator-driven (`/guardrail reset`, which also resets the signal's run-length state). There is no automatic reset — a tripped watchdog is a "stop, get human attention" signal, not a throttle. Only Critical signals halt; a hypothetical future low-severity signal would stay advisory even under enforce.

Future modes — `prompt` (pause turn + ask operator via the existing permissions prompter) and `auto` (call `Agent.SwapModel` to escalate to a frontier model without operator interaction) — are designed but deferred. Same for additional signals (tools-without-text, files-not-touched, context-growth-rate, cost-burn-rate) and an operator `/escalate` slash for manual model swaps.

### v1 signal: repeated identical tool calls

One signal ships in v1: `repeated-tool-call`, threshold 5. Trips when the same `(tool name, JSON-serialized args)` pair appears five times in a row. Catches the runaway loop pattern from [#144](https://github.com/go-steer/core-agent/issues/144) and similar shapes.

- "Consecutive" is the keyword — `a → b → a → b → a` doesn't trip (no run of identical calls); `a → a → a → a → a` does.
- Args comparison is literal-string. Calls with semantically-equivalent but textually-different args (e.g. `"main.go"` vs `"/workspace/main.go"`) aren't detected as repeats. Tool-specific canonicalization is a future enhancement.
- One alert per stuck pattern, not one per tool call past the threshold — operators get a single notice, not flood.

### Composition

The watchdog is the *behavioral signal layer*. Paired with:

- **Per-tier compaction thresholds** ([#119](https://github.com/go-steer/core-agent/issues/119)) — the context signal.
- **Cost ceiling** ([#145](https://github.com/go-steer/core-agent/issues/145)) — the dollar signal. The hard backstop when behavioral signals miss.
- **Task class** ([#123 PR 1](https://github.com/go-steer/core-agent/issues/123)) — the operator-declared posture layer (different signal, set up-front rather than detected at runtime).

### Library usage

```go
import (
    "github.com/go-steer/core-agent/v2/pkg/agent"
    "github.com/go-steer/core-agent/v2/pkg/watchdog"
)

w := watchdog.NewDefaultWatchdog()
a, err := agent.New(model,
    agent.WithWatchdog(w, func(alert watchdog.Alert) {
        log.Printf("watchdog: %s", alert)
    }),
    agent.WithWatchdogEnforce(), // optional: halt on Critical runaway
    // ... other options
)
```

The `Watchdog` interface lets you plug in a custom implementation (same composability pattern as `Compactor` / `Checkpointer`). For most operators the default — `NewDefaultWatchdog()` with `RepeatedToolCallSignal(threshold=5)` wired in — is sufficient.

Add `agent.WithWatchdogEnforce()` to promote it from observe-only to a kill switch: a Critical alert then trips `Agent`, and subsequent `Run` calls return an error satisfying `agent.IsWatchdogTripped(err)` until it is reset (`a.ResetWatchdog()` in-process; `/guardrail reset` or `POST /sessions/{id}/guardrails/reset` for operators). Query `a.WatchdogTripped()` for the `(bool, reason)` to surface in a `/stats`-style view.

---

## Agentic tool wrappers (Mechanism B)

**The proactive bloat prevention.** Compaction and checkpoints are *reactive* — they clean up after raw tool output has already landed in the parent's context. Agentic wrappers are *proactive* — they route the underlying tool call through a single-purpose subtask on a (typically cheaper) model so only the digest reaches the parent. The raw 5,000-line read never enters the parent's context.

On by default since v2.1. Pass `--agentic-tools=false` to register only the bare tools.

### Configuring

```bash
# Default — wrappers register; subtasks auto-route to the provider's
# cheap-tier model (gemini-3.5-flash-lite on Gemini/Vertex, claude-haiku-4-5
# on Anthropic). The cost-efficiency win activates without extra config.
core-agent

# Pin a specific small model (cross-provider, custom tier, etc.)
core-agent --agentic-small-model gemini-2.5-flash

# Pin subtasks to the parent's model (disable the cheap-tier default)
core-agent --model claude-opus-4-7 --agentic-small-model claude-opus-4-7

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

Tool descriptions tell the model when to prefer the wrapper ("Use INSTEAD OF read_file (NOT IN ADDITION TO) when the file might be large..."). They also explicitly forbid the verify-with-bare-tool fallback ("Treat the digest as authoritative; DO NOT re-read with bare read_file to spot-check"). The framing pushes a model that wants to double-check toward refining the agentic call with a narrower question rather than re-fetching the raw content — defeats of the wrapper otherwise (see [#59](https://github.com/go-steer/core-agent/issues/59) for the smoke that motivated the wording). The wrappers share the parent's permission gate and per-tool output caps — the subtask isn't a security boundary, it's a *context isolation* boundary.

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
    "github.com/go-steer/core-agent/v2/pkg/agent"
    "github.com/go-steer/core-agent/v2/pkg/tools/agentic"
)

a, err := agent.New(model,
    agent.WithCompactor(agent.NewDefaultCompactor()),
    agent.WithCheckpointer(agent.NewDefaultCheckpointer()),
    agent.WithUsageTracker(tracker),
)
```

For the agentic wrappers, use `tools/agentic.AgenticReadFile`, `AgenticFetchURL`, `AgenticGrep`, `AgenticResearch`. They take an `AgenticToolOpts` with `AgentGetter` (a late-binding closure — see `agent.WithPostConstruct`), `Provider`, `SmallModelID`, and `InnerTools` (the bare tools the subtask is allowed to call). See [Library API → Context management](/embed/api/) for full signatures.

Direct programmatic access:

- `Agent.Compact(ctx, focus) (CompactionResult, error)` — runs the summarizer synchronously.
- `Agent.CompactIfNeeded(ctx, focus) (CompactionResult, error)` — threshold-gated variant.
- `Agent.Checkpoint(ctx, taskNote) (CheckpointResult, error)` — writes a task-boundary checkpoint.
- `Agent.RunSubtask(ctx, SubtaskSpec) (SubtaskResult, error)` — the primitive the agentic wrappers are built on.
- `Agent.ContextStats() ContextStats` — snapshot the same data `/context` shows.
- `Agent.HasCompactor() bool` / `Agent.HasCheckpointer() bool` — predicates for host adapters gating slash commands.

---

## Where to go next

- [Interactive workflows](/run/interactive/workflows/) — operator-side workflow context
- [Library API](/embed/api/) — full signatures + extension points
- [Autonomous runs](/run/autonomous/operations/) — compaction makes long unattended runs viable
- [Sessions and event log](/concepts/sessions/) — how boundary events show up in the audit log
- [`docs/context-management-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/context-management-design.md) — full design rationale, alternatives considered, future roadmap (memory tools)
