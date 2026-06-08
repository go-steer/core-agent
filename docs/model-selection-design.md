# Task-class model selection: operator hint + watchdog escalation

Design doc for two composing mechanisms that get the right model
class onto a given session: a `--task` flag that maps an operator-
declared task class to sensible defaults, and an out-of-band
watchdog that detects sessions going off the rails and triggers
escalation.

**Status:** proposed (2026-06-08). Awaiting approval before
implementation. v2.5 candidate.

**Tracking issue:** [#123](https://github.com/go-steer/core-agent/issues/123)

## Motivation

Real session from 2026-06-08 (logs in `.agents/sessions/2026-06-08T14-37-49Z.json`):

- Model: `gemini-3.5-flash` (small-tier, 1M context)
- Task: find a text-wrapping bug in `core-tui`
- 142 turns over 2h32m, $19.45
- Crashed at turn 142 on `400 INVALID_ARGUMENT` (context overflow)
- **Bug not found.**

Companion sessions on the same day burned another ~$60 on
post-mortems and follow-up attempts, also on Flash. The same bug
was found by an Opus-tier session in a handful of turns when
handed the same problem.

**The dominant variable was model class.** Other mitigations
shipped or in flight — agentic-tools default-on (#118), lower
compaction threshold for small models (#119), small-tier startup
warning (#121), transcript fidelity (#120) — all help around the
edges. None of them change the fact that Flash should not have
been the parent for an open-ended debug, and there's no convenient
way today to express that.

Two structural gaps:

1. **No way to express task class at session start.** The
   operator picks a model and starts typing. Task class —
   "debugging vs. implementing vs. chatting vs. researching" —
   is in the operator's head, not in any flag. The model picked
   for "chat with me about the code" is the same model picked
   for "find a subtle render bug across three files." Different
   tasks want different model tiers, different compaction
   thresholds, and different tool defaults.

2. **No detection when a session is going off the rails.** Once
   started, a session either completes or runs out of context
   or budget. There's no observer watching "this run has done
   80 turns without touching a new file" or "the model has called
   `grep` with the same pattern 7 times." The 142-turn session
   *was* going off the rails by turn 30; nobody noticed until
   turn 142.

A pure LLM-based pre-flight router (Flash classifies the first
prompt, routes to a model tier) is **explicitly rejected as the
primary mechanism** — see Non-goals. The two pieces below are
chosen because they don't require the routing layer to be smart
about difficulty estimation, which is exactly what small models
are bad at.

## Goals

- **Express task class at session start.** Single flag, predictable
  mapping to (model, compaction threshold, agentic-tools default,
  ask-mode default). Operator-declared, not LLM-inferred.
- **Detect off-rails sessions.** Pure-heuristic watchdog over turn
  telemetry — no extra LLM calls. Fires before the session reaches
  the context wall, not after.
- **Recover, don't just warn.** Watchdog can prompt the operator
  to escalate to a frontier model; the session continues with the
  same context on the new model.
- **Compose with existing flags.** `--task` sets defaults; explicit
  `--model` / `--agentic-small-model` / `--no-agentic-tools` always
  win. No magic.
- **Observable.** Both pieces emit telemetry so the operator can see
  what was decided and why.

## Non-goals (v1)

- **LLM-based pre-flight routing.** Tempting and well-precedented
  (RouteLLM, semantic routers, LiteLLM routing primitives), but
  rejected as the *primary* mechanism for one reason: **difficulty
  estimation requires the same reasoning depth as the task itself**.
  A Flash router will systematically under-estimate the gnarly
  cases — the 142-turn debug session was prompted as "find the
  render glitch in the TUI," which sounds easy. Any Flash classifier
  would have routed it to Flash. The misclassification is silent
  (operator can't tell they got the worse outcome) and biased
  toward exactly the cases that cost the most. Pre-flight routing
  may revisit as v2 once #2 + #4 are operating and we have telemetry
  on what task classes get misjudged.
- **Auto-escalation without operator consent.** Watchdog *suggests*;
  the operator (or a config knob explicitly opting in) decides.
  Silently upgrading a session from Flash to Opus mid-run is a
  cost surprise.
- **Cross-provider routing.** Stay within the operator's configured
  provider for v1 — escalation `flash → pro` within Gemini, or
  `haiku → opus` within Anthropic. Cross-provider (e.g. Flash → Opus)
  adds auth + pricing-catalog questions that aren't the point.
- **Per-subagent task class.** Spawned subagents inherit the parent's
  task class for v1. Per-spawn overrides come later if telemetry shows
  the parent's class is wrong for the subagent's work.
- **A formal "task ontology."** Five task classes (see below) cover
  observed usage. Resist the urge to model the whole space.

## Proposed design

Two independent pieces. Either ships without the other; both
together close the loop.

### Piece 1: `--task` flag + task-class defaults table

A new CLI flag and config field. Maps a task-class string to a
bundle of defaults that get applied before agent construction.

```bash
core-agent --task=debug                     # defaults for deep bug hunting
core-agent --task=implement                 # defaults for feature work
core-agent --task=chat                      # defaults for Q&A / pairing
core-agent --task=research                  # defaults for codebase exploration
core-agent --task=review                    # defaults for PR / diff review
```

Each task class maps to a tuple:

| Task class | Default model | Compaction threshold | Agentic tools | Ask mode |
|---|---|---|---|---|
| `debug` | frontier (e.g. `claude-opus-4-7`, `gemini-3.5-pro`) | 0.65 | on (with `agentic-small-model`) | `auto` |
| `implement` | frontier | 0.7 | on (with `agentic-small-model`) | `auto` |
| `chat` | mid (e.g. `claude-sonnet-4-6`, `gemini-2.5-pro`) | 0.85 | on (no small-model split) | `auto` |
| `research` | mid | 0.65 | on (with `agentic-small-model`) | `allow` |
| `review` | frontier | 0.75 | on (with `agentic-small-model`) | `auto` |
| (unset) | provider default | 0.85 (`DefaultCompactionThreshold`) | on (PR #118) | provider default |

The "frontier" / "mid" resolution comes from the pricing catalog's
existing tier metadata (or a small built-in classifier if the
pricing catalog doesn't tier — see Open Questions). The operator
can always override individual knobs:

```bash
# Use debug defaults, but pin the model
core-agent --task=debug --model=gemini-3.5-pro

# Use research defaults, but no agentic-tools
core-agent --task=research --agentic-tools=false
```

**Why operator-declared, not inferred:** the operator knows what
they're sitting down to do. Asking them to type `--task=debug`
once is cheaper, more predictable, and less error-prone than any
classifier we could build. Defaults still apply when the flag
isn't set — this is *additive*, not a regression.

**Config-file equivalent.** Same knobs settable in `.agents/config.json`:

```json
{
  "session": {
    "task_class": "debug"
  }
}
```

Useful for project-local defaults (`.agents/config.json` in a
codebase known to be debug-heavy, e.g. an infra repo).

**Surface in `/stats` and `/context`.** Both slashes show the
active task class so the operator can confirm what's in effect.

### Piece 2: watchdog auto-escalation

An out-of-band observer over per-turn telemetry. Pure heuristic;
no LLM calls. Runs in the same process, fires between turns.

**What it watches:**

| Signal | Threshold (initial guess) | Why |
|---|---|---|
| Turns since last new file path read | ≥ 25 | Real progress usually touches new files |
| Identical tool call args repeated | ≥ 5 | Re-running the same grep is a stuck signal |
| Tool calls without intervening assistant text | ≥ 15 | Loop hunting, no output |
| Context utilization growth rate | > 5% / turn for 5 turns | Heading for the wall fast |
| Cost burn rate | > $1 / 5 min for 15 min | Diverging from any reasonable trajectory |

When any signal trips, the watchdog emits a `WatchdogAlert` event.
Default behavior (configurable):

1. **Warn mode** (default): log to stderr + `/stats`, no action.
   ```
   ⚠ watchdog: session has not touched a new file in 30 turns.
     This session may be stuck. Consider escalating to a frontier
     model (current: gemini-3.5-flash). Type /escalate to upgrade.
   ```

2. **Prompt mode** (`watchdog.action: prompt`): pause the run,
   ask the operator y/n to escalate, resume on either path.

3. **Auto mode** (`watchdog.action: auto`): swap the model
   underneath, log the swap, keep going.

**Escalation mechanics:** same session, same context, model handle
swapped. Possible because `pkg/agent` already takes a `models.LLM`
at construction; we add a `SwapModel(newModel)` entrypoint that
transitions atomically between turns. New model receives full
conversation history (compacted or not, per current state).

**Auto mode is opt-in.** Default ships in warn mode. Auto mode
is for headless / autonomous deployments where there's no operator
to prompt and the cost ceiling is bounded by `--max-cost`.

**Slash equivalents.**

- `/escalate [model]` — operator-driven model swap, same mechanic
  as watchdog auto mode. Useful even without watchdog firing.
- `/watchdog` — show what the watchdog is seeing this session
  (counts per signal, last trip).

### How the two pieces compose

- `--task` sets the initial trajectory. Right model, right
  thresholds, right tool defaults from turn 1.
- Watchdog is the safety net for when `--task` wasn't passed,
  or was passed wrong, or the task evolves mid-session (chat
  that turned into a debug).

Most sessions never need the watchdog because `--task` got the
initial pick right. The watchdog matters for the long tail —
sessions where the operator under-specified, or where the
problem turned out to be harder than expected.

## Implementation sketch

### Piece 1

- New CLI flag in `cmd/core-agent/main.go`: `flag.String("task", "", "...")`.
- New config field: `config.Session.TaskClass`.
- Task-class table lives in a new `pkg/taskclass/` package. Each
  class is a `Profile{Model, CompactionThreshold, AgenticTools,
  AgenticSmallModel, AskMode}`. Lookup is map-keyed by class string.
- "Frontier" / "mid" model resolution either reads a tier field
  from the pricing catalog (`pkg/pricing/`) or, if absent, falls
  back to a small built-in `model-id → tier` table.
- Resolution happens in `main.go` after `--model` / `--ask` /
  `--agentic-*` flags are parsed but before agent construction.
  Explicit flags win; task-class defaults fill the unspecified.
- `/stats` and `/context` slash handlers read the resolved task
  class from agent metadata and surface it.

### Piece 2

- New `pkg/watchdog/` package. `Watchdog` interface with `Observe(TurnTelemetry)`
  and `Check() []Alert`. Pluggable so consumers can disable or
  swap strategies.
- Default watchdog implementation tracks the five signals above
  in a rolling window. Stateless across sessions; per-session
  state lives in the agent.
- Hook point: `agent.Agent` calls `watchdog.Observe(...)` at the
  end of each turn (post-tool-call, post-summary). `watchdog.Check()`
  runs synchronously between turns — no goroutine churn.
- New `Agent.SwapModel(newModel models.LLM) error` method. Atomic
  between turns. Subagent inheritance unchanged (subagents take
  the parent's *current* model at spawn time).
- New `/escalate` slash handler in `cmd/core-agent/coretui_enabled.go`
  + the line-mode REPL.
- Config block:
  ```json
  {
    "watchdog": {
      "action": "warn|prompt|auto",
      "thresholds": {
        "stuck_turns": 25,
        "repeat_calls": 5
      }
    }
  }
  ```

## Telemetry

Without good telemetry on this, we can't tell whether the task-class
table is well-tuned or the watchdog thresholds are right. So:

- **Per-session event:** which task class was active, whether it
  came from `--task`, config, or default. Land in the session log
  alongside model + cost.
- **Per-session event:** every watchdog alert (signal name, turn
  number, action taken). Land in the session log.
- **Per-escalation event:** model before, model after, trigger
  (operator or watchdog), turn number, context utilization at swap.

After a few weeks of dogfooding, this data tells us:

- Which task classes are getting picked.
- Which task classes have the wrong default model (operator
  consistently overrides).
- Which watchdog signals fire most often, and whether they
  correlate with sessions the operator deemed off-rails.

Without #120 (transcript fidelity), watchdog signal counts may be
the *only* way to retrospectively diagnose a session — so this
telemetry partially compensates for #120 until that's fixed.

## Open questions

1. **Tier classification source of truth.** Pricing catalog
   (`pkg/pricing/`) has per-model price metadata. Does it have a
   tier field? If yes, reuse it. If no, where does the
   frontier/mid/small classification live? A small package-level
   table is the obvious answer for v1; a richer source can come
   later. Prefer not to fork a new metadata file.
2. **Should `--task` change `permissions.mode`?** Today `--ask=auto`
   selects ask vs. allow based on stdin shape. The task class
   table proposes ask defaults — does that override `--ask`, or
   only fill it when unset? Proposal: only fill when unset.
3. **What's the right `--task` default?** Today there's no default.
   Setting one (e.g. `chat`) would change behavior for every
   user who never passes the flag. Proposal: keep "unset"
   distinct from any class — no flag = today's behavior. Reduces
   blast radius of landing the feature.
4. **Watchdog model swap and compaction interaction.** When the
   watchdog escalates `flash → pro`, the new model has a *different*
   context window and (per #119) a different compaction threshold.
   Re-evaluate compaction immediately after the swap, or wait for
   next turn? Proposal: re-evaluate immediately; this is the same
   logic that runs after any model-context change.
5. **Cost ceiling vs. watchdog escalation.** If the watchdog auto-
   escalates from a $1/Mtok model to a $15/Mtok model mid-session,
   should it also tighten or check the cost ceiling? Plausibly yes
   — escalation is the moment to surface the new per-turn cost
   projection.
6. **Should the watchdog signals also feed `/done`?** A session
   that's been stuck for 25 turns is probably a session that
   *should* checkpoint and start fresh. Maybe `/done` is the
   right action sometimes, not `/escalate`. Out of scope for v1
   but worth flagging.

## Out of scope (revisit later)

- LLM-based pre-flight routing (see Non-goals — revisit as v2
  with telemetry from v1 in hand).
- Per-subagent task class.
- Cross-provider escalation (`gemini-flash → claude-opus`).
- Multi-step approval / "approve escalation but stay on this
  context window cap."
- An automatic `--task` *inference* mode that watches the first
  N turns and recommends a class — easy to imagine, hard to make
  reliable without an LLM in the loop.

## References

- #118 — agentic-tools default on (shipped, complementary)
- #119 — per-model compaction threshold (this design assumes #119
  lands; the task-class table sets per-class thresholds that
  presuppose the per-model knob exists)
- #120 — empty tool text in transcripts (this design partially
  works around #120 via watchdog telemetry, but #120 should be
  fixed independently)
- #121 — small-tier startup warning (this design supersedes #121:
  if `--task` is passed, the warning is unnecessary; if `--task`
  is not passed and the model is small-tier, the warning still
  fires)
- `docs/context-management-design.md` — sets the substrate this
  composes against (compactor, subtasks, checkpoints, memory)
- `docs/plan-first-design.md` — different problem (enforcing the
  workflow shape), same flavor of "operator-declared mode that
  influences agent behavior"
