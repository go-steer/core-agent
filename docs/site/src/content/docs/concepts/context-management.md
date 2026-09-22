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
| Task-boundary checkpoints | on | `--checkpoint=off` | `/done [note]` (alias `/checkpoint`) |
| Agentic tool wrappers (subtasks) | on | `--agentic-tools=false` | (model-driven via `agentic_*` tools) |

A fourth — `/context` (alias `/boundaries`) — is an observation surface, not a mechanism: it reports the shape of what the others have done this session.

---

## Compaction (Mechanism A)

**The reactive backstop.** When the context window fills past a per-model-tier threshold (default `0.85` for frontier, `0.65` for mid, `0.35` for small-tier models since v2.5), the agent automatically compacts the conversation into a "teammate handover" summary and slices the pre-summary history out of future requests. The full audit log is preserved on disk — only the live LLM request is sliced.

### How it fires

- **Automatic:** post-turn hook checks utilization against the configured per-tier threshold; when over, the next `Run` drains a `compactionPending` flag by writing the summary before its actual work. The operator-visible turn boundary stays clean — no surprise latency cliff after the assistant finishes.
- **Manual:** `/compact [focus]` runs the same summarizer immediately. The optional `focus` argument biases the summary toward a particular thread when you want to preserve specific context.

### Growth inside a turn (since v2.10)

The post-turn hook above used to be the *only* place context was ever evaluated, and the thing that overflows a window does not wait for a turn boundary ([#975](https://github.com/go-steer/core-agent/issues/975)). Inside one agentic turn the sequence is: a model call completes and commits its usage, a tool runs and returns a payload, the next model call is built with that payload in it. Nothing looked at context between the second and third steps, so a single large tool result — a full pod log, a wide `kubectl get -o json`, an MCP response over a big cluster — went into the next request while the utilization figure both compaction and `/context` read still described the comfortable state before it arrived. On a Kubernetes session large read results are the normal case rather than an edge case.

Utilization is now the last measured input-token count **plus an estimate of everything appended since it**. Tool results are measured in bytes as they land and converted at three bytes per token — deliberately below the usual four-byte prose rule of thumb, because what is being estimated is JSON, YAML and log output, which tokenize worse, and because the error directions are not symmetric: under-counting means the threshold never fires and the overflow happens anyway, while over-counting means one compaction runs early, which you can see and price. The estimate covers only the unmeasured tail and is discarded the moment the next model call reports a real number. Model text is not counted this way — the provider's own usage record for the call that produced it is a better number than anything measured locally.

Two things act on that estimate, both inside the turn:

- **Compaction is marked pending as soon as the threshold is crossed**, rather than at the end of an agentic turn that may still have a dozen tool calls to run. The threshold itself is unchanged; only the moment it is read.
- **Past 95% of the window the turn is cut** (`agent.ContextBudgetHardCeiling`). Compaction is already pending when this happens, so the next turn reduces context and carries on — the session heals itself and there is nothing to reset. The alternative is not a turn that succeeds: it is the provider rejecting an oversized request with an error that classifies badly and points nowhere near compaction. The cut is announced as a `context-reduction-degraded` row with `operation: turn-cut`, carrying the estimate, the window, and how many of the bytes were unmeasured.

`ContextWindowUsedEstimated()` on the embedded agent returns both the number and whether any of it is an estimate, so an embedding surface can say which of the two it is rendering. The bundled TUI's context segment does not draw that distinction yet.

### Per-tier thresholds (since v2.5)

A single 0.85 threshold worked for frontier-tier models (Opus, Pro) but fired far too late for small-tier models (Flash, Haiku) — reasoning quality on those tiers degrades well before they reach 85% context utilization. The per-tier defaults trigger earlier on smaller models so the session stays inside its effective working range:

| Tier | Default trigger | Examples |
|---|---|---|
| `frontier` | `0.85` (unchanged) | `claude-opus-4-*`, `gemini-3.x-pro`, `gemini-3.8-flash`, `gemini-3.7-flash`, `gemini-3.6-flash` |
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

### When the summarizer comes back empty (since v2.9)

Compaction and task-boundary checkpoints share one summarizer call, so they fail the same way, and one of those ways is the model returning **no text at all** — a safety block, a `MAX_TOKENS` cutoff, a candidate carrying only a thought signature ([#908](https://github.com/go-steer/core-agent/issues/908)).

Three things happen:

- **The reason is reported.** The provider's own explanation is carried into the error in the same vocabulary `/slash/btw` uses: `agent: compaction: model returned no summary text (finish_reason=MAX_TOKENS) after 2 attempts`. When the provider offered none, the message says so — that absence is itself the signal.
- **It is retried once, but only when a retry could help.** An empty response the provider *explained* with a terminal reason (the safety family, `MAX_TOKENS`, a malformed tool call) is a property of the input: an identical retry produces an identical answer, so none is made. An unexplained empty response — or one carrying a reason this version doesn't recognise — is retried exactly once. The retry is capped inside the single call, so nothing loops.
- **A failed *automatic* run is visible to attached clients**, not just in the daemon log. It appends a durable `context-reduction-failed` event (`Author=agent/context-reduction`) carrying `operation` (`compaction` or `checkpoint`), the verbatim `reason`, and, for compaction, `consecutive_failures` and `cooldown_turns`. Tail `GET /sessions/{app}/{sid}/events` or the attach event stream to see it. A manual `/compact` or `/done` reports the failure to you directly and writes no row.

A failed automatic compaction still clears its pending flag and backs off exponentially rather than re-attempting every turn. That is deliberate — a summarizer that is persistently empty would otherwise burn the most expensive call in the session on every turn. Since v2.10 the backoff is no longer the *only* answer: two consecutive failures escalate to mechanical truncation (below), so history stays bounded even while the summarizer is down. Cutting a manual `/compact` (optionally with a `focus` to shrink the ask) is still the fastest way back to a real summary; a `MAX_TOKENS` reason specifically points at the summarizer's own output cap.

### When compaction itself is degraded (since v2.10)

Compaction has two dependencies a long unattended run can lose, and until [#974](https://github.com/go-steer/core-agent/issues/974) it lost both silently. Each now has a fallback that is *worse than the real thing and much better than nothing*, and each says so on the event log as a `context-reduction-degraded` row (same author as the failure row, different event name — see [the attach reference](/reference/attach-http/#degraded-context-reduction-v2100-dev)).

**An uncatalogued model no longer disables compaction.** The threshold check needs the model's context window, and for a model the pricing catalogue has never heard of — a Vertex publication name, a proxy id, a model newer than this build — `ContextWindowSize()` returns 0. That used to divide out to "not full yet", so compaction never fired at all: absent, silent, and observable only when the provider hard-failed on an oversized request thousands of turns later. The window is now assumed to be **128,000 tokens** (`agent.AssumedContextWindowSize`) and the substitution is announced once, naming the model id. The number is chosen to be wrong in the safe direction: guessing too large kills the session at the provider's wall, while guessing too small only buys extra compactions you can see and price. Pin a catalogued model id if you want the exact threshold back. Note the assumption lives in the compactor and *not* in the usage tracker — `/context` still reports an unknown window as unknown rather than rendering a fabricated percentage as though it were measured.

**A dead summarizer no longer means unbounded history.** Compaction summarizes by calling the model, so the same 429 storm that is breaking turns breaks compaction too; the backoff then grows to 32 turns while history grows the whole time. After **two** consecutive failures (one is often a transient the next attempt clears), compaction falls back to **mechanical truncation**: no model call, a deterministic digest of the dropped window — a ledger of the tool calls that ran, with their arguments but not their results, most recent first — plus the tail of the conversation verbatim within a fixed character budget. The digest opens by telling the model in plain terms that it is reading a truncation, that detail *has* been lost, and that it should re-run a tool rather than guess at what the dropped result said. It writes an ordinary compaction boundary (`CustomMetadata["compaction"] = "summary"`), so nothing on the read side changes; the extra key `compaction_mechanical` marks it for a post-mortem that needs to know which kind of boundary it is looking at. A successful fallback clears the cooldown but keeps the failure count, which leaves exactly one summarizer attempt per context-refill cycle — that attempt is both the cost and the recovery probe. If the mechanical path fails too, *that* is a `context-reduction-failed` row: both strategies are down.

Mechanical truncation is lossy in a way a summary is not, and it is meant to be the thing that keeps an unattended run alive until someone looks at it — not a steady state. A degraded row on a session you care about is a reason to intervene.

---

## Task class (since v2.5)

`--task=<class>` is a single flag that picks a coherent bundle of defaults tuned for the kind of work the operator is sitting down to do. Operator-declared (not LLM-inferred) — the operator knows whether they're debugging or chatting; asking them to type one flag is cheaper and more predictable than any classifier we could build.

Five classes ship today:

| Class | Default model tier | Compaction threshold | Ask mode | Tools | Plan-first | When to use |
|---|---|---|---|---|---|---|
| `debug` | frontier (e.g. `claude-opus-5`, `gemini-3.7-flash`) | `0.65` | `auto` | built-ins − `bash` | on | Bug hunts, root-cause investigations, multi-file traces |
| `implement` | frontier | `0.70` | `auto` | built-ins | off | Feature work, multi-file refactors |
| `chat` | mid (e.g. `claude-sonnet-5`, `gemini-3.5-flash`) | `0.85` | `auto` | built-ins | off | Q&A, pairing, lightweight design discussion |
| `research` | mid | `0.65` | `allow` | built-ins − `bash` | on | Read-heavy codebase exploration; `allow` keeps the ask-mode noise out of the way |
| `review` | frontier | `0.75` | `auto` | built-ins − `bash` | on | PR / diff review |

Resolution per-provider:

| Tier | Gemini / Vertex | Anthropic |
|---|---|---|
| frontier | `gemini-3.7-flash` | `claude-opus-5` |
| mid | `gemini-3.5-flash` | `claude-sonnet-5` |
| small | `gemini-3.5-flash-lite` | `claude-haiku-4-5` |

Explicit per-knob flags always win over the class defaults:

- `--model` (long-form alias of `-m`) pins the model — e.g. `--task=debug --model=gemini-3.7-flash` uses debug-mode defaults but a specific model. A model set in the config file (`model.name`) is likewise respected; `--task` only fills in the tier model when neither `--model` nor a config-file model is set.
- `--compaction-threshold=<0..1>` pins the post-turn compaction trigger, overriding both the config-file `compaction.threshold` and the class default.
- `--ask=off|stdin|auto` pins the ask-user mode; left unset, the class default applies.
- `--enable-tools=<names>` adds back a built-in the class dropped — `--task=debug --enable-tools=bash` gives you the shell under debug defaults.
- `--plan-mode=off|advisory|required` pins plan mode; `--task=debug --plan-mode=off` keeps the reduced tool set without the plan requirement, and `--plan-mode=advisory` keeps the plan artifact without the gate. (`--plan-first[=false]` is the deprecated two-state spelling.)

### Tools and plan-first (since v2.9)

The three investigation-shaped classes — `debug`, `research`, `review` — drop `bash` from the built-in set and turn on [plan-first gating](/reference/configuration/#plan-mode-v29--plan_mode) (`plan_mode: "required"`). Both defaults come from the same measured session ([#160](https://github.com/go-steer/core-agent/issues/160)): the model reached for `bash $ grep -rn` on its first tool call with the native `grep` tool sitting in the schema, and emitted zero plan sentences before acting. `implement` keeps the shell because edit-then-test cycles need it and plan-first would gate the very edits the class exists to make; `chat` isn't investigation-shaped at all.

Dropping `bash` is the blunt version of the [bash search gate](/concepts/tools/#the-bash-search-gate), which refuses only the search-shaped subset. They compose: `--task=debug --enable-tools=bash` puts the shell back and the search gate still refuses `bash grep`.

Three things to know:

- **`--enable-tools` cancels the profile, not your config.** It cannot re-enable a tool you turned off in `tools.disable` or `--disable-tools` — asking for both is a startup error rather than a silent win for either side. Naming a tool the class never dropped is a harmless no-op.
- **Subagents inherit the reduction.** A declarative subagent draws from the parent's already-gated catalog, so `--task=debug` hardens the parent and its subagents together.
- **Plan-first gates `fetch_url` and `spawn_agent` too**, not just writes. Under `--task=research` the model records a plan before its first fetch. That is the intended discipline, but it is a real behavior change for scripted research runs — `--plan-mode=off` opts out, and `--plan-mode=advisory` keeps the recorded plan while unblocking the run.

Plan-first needs somewhere to write plans. If no `.agents/` directory was resolved, `--no-builtin-tools` is set, or `record_plan` is disabled, the class default is **suppressed** with a startup line saying which of those it was — a plan-first gate with no `record_plan` denies every mutating call for the life of the session and nothing can clear it (`/replan` only revokes a plan; it can't grant one). An explicit `--plan-mode=required` (or the deprecated `--plan-first` / `require_plan_artifact: true`) is still honored in that situation, but startup warns. Advisory mode can't deadlock — it arms no gate — but it goes inert under the same conditions, since there is no `record_plan` to write the artifact.

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
  model for the parent — e.g. --model gemini-3.7-flash --agentic-small-model
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

Compaction and watchdog signals catch *behavioral* runaway (context fill, repeated tool calls). They don't bound the *outcome* — a model can produce many tool calls in a single turn. The cost ceiling is the dollar-denominated guard for that case, and since [#720](https://github.com/go-steer/core-agent/issues/720) it is checked *during* the turn rather than only after it.

Two bounds, both optional, both off by default:

| Bound | CLI flag | Config field | What it caps |
|---|---|---|---|
| Per-turn | `--max-turn-cost-usd=<N>` | `agent.max_turn_cost_usd` | Cumulative spend of a single conversation turn (every model call + subtask between one operator inject and agent-done state) |
| Per-session | `--max-session-cost-usd=<N>` | `agent.max_session_cost_usd` | Cumulative spend across all turns since the agent started |

### What happens when a ceiling trips

1. Session cost (from the usage tracker) and per-turn delta (against a snapshot taken at turn start) are computed **as the turn runs**, on each event, and again at the turn boundary.
2. If either configured bound is met or exceeded, the agent emits a structured [`guardrail-trip`](/reference/attach-http/#guardrail-trips-protocol-1130) event with `guardrail=cost_ceiling` and a reason describing the spend, the bound, and how to clear it. The event is *non-terminal*: it reports a guardrail firing, not a turn ending ([#891](https://github.com/go-steer/core-agent/issues/891)).
3. The turn in flight is cancelled. If it was in the middle of generating, it reports `turn-error` / `kind=canceled` as its one terminal frame, and the trip from step 2 — which carries `halted_turn: true` — is what explains it. Before protocol 1.13.0 the trip *was* the terminal frame and the cancel was suppressed to keep the count at one ([#818](https://github.com/go-steer/core-agent/issues/818); [frame details](/reference/attach-http/#turn-error-kinds)).
4. **How far the stop reaches depends on which bound tripped**, and this is the whole difference between them ([#1049](https://github.com/go-steer/core-agent/issues/1049)). A **per-session** trip halts the session: a flag is set, the next `Run` returns the same error without invoking the model, and only an operator reset clears it. A **per-turn** trip ends its turn and nothing else — no flag, nothing to reset, and the next turn starts from a fresh per-turn baseline.
5. To resume a halted session — `/guardrail reset` in the TUI, `POST /sessions/{id}/guardrails/reset` over attach ([#666](https://github.com/go-steer/core-agent/issues/666)), or `Agent.ResetCostCeiling()` when embedding the library.

Checking in-turn is what makes this a backstop rather than a receipt. A runaway is a loop *inside* one turn — model, tool, model, tool — and the tracker grows on every model call within it. Through v2.9.0-dev.0 enforcement ran only at turn boundaries (the post-turn hook, plus [#362](https://github.com/go-steer/core-agent/issues/362)'s settle-time re-check at the top of the *next* turn), so a single runaway turn was capped only after it had finished spending, and a turn that never terminated was never capped at all. Note the consequence: crossing a ceiling now kills the turn in progress and discards its partial answer, the same as an operator `/interrupt`. That is the intended trade for a bound whose whole job is to stop spending.

#### The two bounds stop different things

The names are the contract. `--max-session-cost-usd` bounds a session, so tripping it halts the session. `--max-turn-cost-usd` bounds a turn, so tripping it ends that turn — one expensive turn in an eight-hour unattended run is not a reason to end the run, and until [#1049](https://github.com/go-steer/core-agent/issues/1049) it was: the per-turn trip set the same session-wide flag, wrote the same durable halt row, and so survived a pod roll as a permanent stop that no amount of restarting cleared.

What a turn-scoped bound cannot do on its own is stop a driver that simply re-drives. A wake loop or auto-continue will happily start turn after turn, each one spending up to the ceiling and being cut, forever — a hard stop turned into an unbounded spend loop, which is worse than the halt it replaced. So the *bound* stays turn-scoped and the *runaway* gets a session-scoped answer: **three turns in a row that each end in a per-turn trip halt the session**, with a reason that says so rather than blaming the last turn. One turn that finishes inside its bound clears the count, so an agent that occasionally runs hot never reaches it. Every trip is reported on the stream either way — an attached operator watches the pattern build, not just its conclusion.

A **per-session** trip needs more than a bare reset. The accumulator is already at or past the ceiling, so clearing the flag alone re-trips on the very next turn — the reset surface refuses that case outright (HTTP **409**) and asks for `additional_budget_usd`, which RAISES the ceiling. It never zeroes the accumulator or restarts a spend window: `/usage`, the eventlog-derived cost, and the ceiling check all keep counting the same dollars, so a session that spent $12 still reports $12 after the operator hands it another $5 of runway. A per-turn **escalation** needs no budget — the bound it broke was per-turn, so a bare reset is enough, and the reset clears the consecutive-trip count along with the halt.

#### Without an attach listener, the cut is in the log

`guardrail-trip` reaches a client that subscribed to one. A plain `core-agent -p '…'` never did: the operator-event seam is a no-op until the attach adapter registers an emitter, so through v2.10.0-dev.1 an unattended cut produced no operator-visible report at all and the entire output was the model client's own `context canceled`, surfaced two layers down and indistinguishable from a provider failure. That is the shape that most needs it — unattended runs default to `watchdog=enforce` and a `$10.00` session ceiling, so they are at once the likeliest to trip a guardrail and the likeliest to have nobody listening ([#1131](https://github.com/go-steer/core-agent/issues/1131)).

Every guardrail that cuts a turn — cost ceiling, watchdog, and the approval gate's [refusal-storm cut](/concepts/permissions/#in-session-decisions) — now writes one line naming itself, carrying its own reason, and disclaiming the cancellation that follows:

```text
agent: [session default] cost_ceiling guardrail cut the turn in flight — the cancellation error that follows is this cut, not a provider failure: per-turn cost ceiling exceeded: this turn cost $0.0512, ceiling is $0.0500. The turn was stopped with its work unfinished; the session is NOT halted, and a further turn would start from a fresh per-turn budget. Nothing starts that turn on its own, though: under a driver (TUI, attach, auto-continue) the run goes on, and a one-shot (-p) run ends here with exit 1. 1 in a row now — at 3 the session halts and needs an operator reset.
```

The last two sentences are the one-shot reader's ([#1140](https://github.com/go-steer/core-agent/issues/1140)). A turn-scoped cut leaves the session available, and the line used to report that as "the next turn starts clean" — a present-tense promise about a turn that nothing inside the agent will ever start. Under a driver it was true; in a `-p` one-shot there is no next turn, the process exits 1, and the only line explaining the stop said the opposite of what happened to the reader most likely to be reading it, since a one-shot is precisely the run with no client attached. The wording is the fix rather than a signal plumbed down from the caller: a per-turn trip deliberately does not raise the session fence, so the agent has nothing to consult, and a sentence true in both worlds costs nothing.

**All four in-turn cuts** carry it, not just the two guardrails. The [refusal-storm cut](/concepts/permissions/#in-session-decisions) is the sharpest case — it withholds its `guardrail-trip` event on purpose, so its log line is the *only* live record of the stop, and it was telling a process about to exit 1 that the next turn starts with an empty refusal map. The [context-budget cut](#growth-inside-a-turn-since-v210) promised a remedy rather than a scope ("compaction will run first on the next turn"), which a one-shot never runs; there the subjunctive goes into the text itself, because that text is also the durable degraded row and a stale promise is wrong in a stored row too. The one-shot sentence stays on the log line only — a client reading the row queried a session, so it has a caller by construction.

The name is the same token `error.type` carries on `gen_ai.agent.invocation.duration`, so the log and the metrics backend describe one halt in one vocabulary. A trip at the turn *boundary* logs `guardrail tripped` instead of `guardrail cut the turn in flight`, because that turn completed and produced an answer — the same distinction `halted_turn` draws on the wire.

The `[session …]` tag ([#1136](https://github.com/go-steer/core-agent/issues/1136)) is what makes the line actionable on a daemon, where several sessions interleave into one log and the operator's next move is `POST /sessions/{id}/guardrails/reset` — a request that takes the id. A single-session run is not special-cased: its id is the `default` that the `--no-repl` banner already prints, and that is the id its reset takes too.

**Every operator line the agent logs carries it** ([#1137](https://github.com/go-steer/core-agent/issues/1137)), not only the four that cut a turn — the fourth of those being the [context-budget cut](#growth-inside-a-turn-since-v210), which is not a guardrail and needs no reset, but ends a turn with the same `context canceled` and is read out of the same multiplexed log. Also: the compaction and checkpoint failures, the degraded-mode notices, the summarizer's empty-response retries, a dropped inbox message, and the wake-fence notice — whose sentence, like the cost ceiling's, ends by pointing at a reset that takes an id. The tag always sits in the same place, directly after the `agent:` prefix, so `grep '\[session s-7\]'` finds every `agent:` line one session wrote and the message bodies stay greppable as whole phrases. (One notice stands outside that family and outside the grep: the tail-repair warning writes to stderr under the CLI's own `core-agent:` prefix and names its session inline as `for session s-7`.) Where a line's text is *also* stored as a durable degraded or failure row, the id goes on the log line only — the row is already filed against its session, and a second copy inside its text would be one to keep in sync. Logging is unconditional rather than conditional on being headless: an attached client gets both, exactly as it has for [context-budget turn cuts](#growth-inside-a-turn-since-v210) since v2.10. Carrying the *whole* typed operator-event stream to stderr on headless runs is [#1132](https://github.com/go-steer/core-agent/issues/1132); this line stands on its own because the refusal-storm cut deliberately emits no event for such a stream to carry.

### Resetting a tripped guardrail

Both backstops share one recovery surface ([#666](https://github.com/go-steer/core-agent/issues/666)):

| Surface | Read state | Reset |
|---|---|---|
| In-process TUI | `/guardrail` | `/guardrail reset [watchdog\|cost_ceiling\|all] [+<usd>]` |
| Attach HTTP | `GET /sessions/{id}/guardrails` | `POST /sessions/{id}/guardrails/reset` (body optional) |
| Library | `Agent.WatchdogTripped()` / `Agent.CostCeilingTripped()` | `Agent.ResetWatchdog()` / `Agent.ResetCostCeiling()` + `Agent.AddSessionCostBudget(usd)` |

`/guardrail` with no arguments prints what is armed, what tripped, why, and — when a bare reset would re-trip — how much budget to add. The reset is `SessionWrite`, not admin: the next thing an operator does after clearing a halt is `POST /inject`, which is itself `SessionWrite`, so gating the reset harder would buy no safety.

#### A halted session goes quiet, and nothing is lost while it waits

A tripped guardrail refuses turns *before* the agent reads its inbox, so anything sent to a halted session is queued, not delivered — and as of [#1040](https://github.com/go-steer/core-agent/issues/1040) it no longer wakes the agent to be refused again either. The session simply goes inert until an operator resets it, at which point the queued messages drive the first turn. Sending to a halted session is therefore safe and does nothing visible: `POST /inject` still returns 200, an attached TUI still shows the message arriving, and auto-continue stands down instead of piling continuation notes into a queue nobody can drain. Before this, a halted session woke on every arriving message, refused every time (no model spend, but a log line every few minutes), and — because the inbox is bounded and drops the *oldest* message when full — could silently discard an operator's earliest instructions if the halt stood long enough.

The one thing a halt does not currently do is tell you: a refused turn emits no frame, so the evidence is in the daemon log rather than in your client. `GET /sessions/{id}/guardrails` and `/guardrail` are how you ask ([#891](https://github.com/go-steer/core-agent/issues/891) tracks surfacing it unprompted).

#### Halts survive a restart

A halt that a restart clears is not a halt. Since v2.9.0-dev ([#643](https://github.com/go-steer/core-agent/issues/643)) both trips — and the operator resets that clear them — are written to the eventlog and folded forward by the next process over the same session. A crash, an OOM kill, or a pod roll no longer hands a runaway loop a fresh budget, which matters most for exactly the unattended deployments [#642](https://github.com/go-steer/core-agent/issues/642) turned these backstops on for. Budget an operator granted before the restart is preserved too, so a resumed session doesn't re-halt at the old bar.

Restored state never overrules live configuration: a daemon restarted with `--watchdog=warn` does not resurrect an enforce-mode halt, and granted budget is not applied to a per-session ceiling that is no longer configured. Restore also fails *open* — if the guardrail history can't be read, the session runs rather than being bricked by a transient database error. Durability requires a session store (`--session-db` / `WithEventLog`); with no eventlog, behavior is unchanged.

### Why "stop, get attention" instead of throttle

A cost-ceiling trip almost always means *something is wrong* — a tool-call loop ([#144](https://github.com/go-steer/core-agent/issues/144)), a model going off the rails, an unexpectedly expensive prompt. Auto-resume would just continue burning budget. The explicit operator reset forces a human look-in.

That reasoning is about a *session* halt, which is why a per-turn trip does not produce one. Cutting the turn already stopped the spending the bound was written to stop, and demanding a reset for it would have an unattended run sit halted until morning over one expensive turn — a human look-in nobody asked for, at the cost of the autonomy the run exists for. The look-in is reserved for the cases that earn it: the session bound, and the three-in-a-row pattern that says the stopping is not working.

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

Compaction caps the *context* dimension. The cost ceiling caps the *dollar* dimension. The watchdog catches the *behavioral* dimension — a session going off-rails (an agent stuck calling `read_file` on the same path five times in a row, the [#144](https://github.com/go-steer/core-agent/issues/144) pattern, or cycling between two calls forever) before the dollar count gets large enough to trip the cost ceiling.

### Modes

The modes are a ladder — each one includes everything the mode above it in this table does.

| Mode | What it does |
|---|---|
| `off` | No observation. |
| `warn` | Observes the tool-call stream. When a signal trips, logs a structured alert to the operator via the normal status channel (`send()` callback for CLI; future SSE event for attach-mode). Does NOT pause the turn, and does not tell the model anything. |
| `feedback` | Warn, plus the observation is injected into the **model's** next-turn context as a `[watchdog]` block ([#159](https://github.com/go-steer/core-agent/issues/159)). A correction, not a backstop — nothing halts a model that reads the block and loops anyway. |
| `enforce` | Feedback, plus a Critical runaway signal (today: `repeated-tool-call`, `alternating-tool-cycle`, `dominant-tool-call` or `no-op-streak` — *not* the Warn-level `repeated-tool-name`, `tool-failure-streak`, `no-new-state` or `tools-without-text`) **halts the agent**: it emits a non-terminal [`guardrail-trip`](/reference/attach-http/#guardrail-trips-protocol-1130) (`guardrail=watchdog`, `halted_turn: true`) carrying the reason, cancels the turn in flight — which then reports `turn-error` / `kind=canceled` as its one terminal frame ([#891](https://github.com/go-steer/core-agent/issues/891); through protocol 1.12.0 the trip was itself that frame and the cancel was suppressed, [#818](https://github.com/go-steer/core-agent/issues/818)) — and, for a **session-scoped** signal, refuses new turns until the operator clears it (`/guardrail reset`, `POST /sessions/{id}/guardrails/reset`, or `Agent.ResetWatchdog` when embedding). This is the hard behavioral backstop — an auto-continue re-drive of the interrupted turn is refused at pre-flight instead of re-issuing the looping call. A **turn-scoped** Critical (today: `no-op-streak` alone) cuts the turn and stops there; see [Two scopes](#two-scopes-what-a-critical-is-entitled-to-stop). |

### Feedback: telling the model what it is doing

`warn` and `enforce` both route the observation to an operator — a log line, or a halt that waits for one. Neither tells the party actually choosing the next tool call. Under `feedback` and above, the next turn's prompt is prefixed with a block like:

```text
[watchdog] Automated observation about your own previous turn — this is not a message from the user, and the user cannot see it.
- repeated-tool-call: You called read_file 5 times in a row with byte-identical arguments ({"path":"a.txt"}). The same call with the same arguments returns the same result, so repeating it cannot make progress. Change the arguments, use a different tool, or — if you have no next step that differs — stop calling tools and say what you are stuck on. Do not repeat this call unchanged.
Adjust your approach on this turn accordingly.
```

Notes on the contract:

- **`enforce` implies `feedback`.** An enforce halt is cleared by an operator reset, and the reset resumes a model whose context still ends in the loop it was halted for. Without the injection, the reset is a treadmill: the same five calls, the same halt, one operator round-trip later. `ResetWatchdog` therefore clears the halt but keeps the queued observation, and a halt [restored from the eventlog](#halts-survive-a-restart) after a restart re-synthesizes it from the persisted reason.
- **`warn` is unchanged** and injects nothing. Feedback is its own rung precisely so turning it on is a decision, not a silent rewrite of the context every existing `warn` operator is already running.
- **Two readers, two texts.** `watchdog.Alert.Reason` is operator-facing and may name operator controls (`/interrupt`, `--max-turn-cost-usd`); `Alert.Guidance` is model-facing and names none of them. A custom `Signal` that sets no `Guidance` falls back to `Reason`, so a third-party detector is never silently inert under feedback.
- **Not a trust boundary.** The block is framed as an automated observation, but a user prompt can contain the literal string `[watchdog]`, exactly as it can contain `[Inbox]`. Treat it as steering, not authentication.
- The queue is bounded (4 alerts, oldest dropped), and nothing is queued while the mode is below `feedback` — flipping the mode later can't deliver a stale backlog.

### Choosing the mode

Precedence is `--watchdog` > `safety.watchdog` > a mode-dependent default, mirroring `--small-tier-parent` / `safety.small_tier_parent`:

```json
{ "safety": { "watchdog": "enforce" } }
```

The config field ([#660](https://github.com/go-steer/core-agent/issues/660)) exists so a recipe is a self-contained unit — before it, `--watchdog` was CLI-only, so a hardened recipe still depended on every deploy manifest and every `core-agent -c ...` invocation remembering the flag by hand.

With neither source set, the default is **`enforce` for unattended runs** (`-p`, `--no-repl`, or a non-TTY stdin) and **`warn` for interactive REPL/TUI runs** ([#642](https://github.com/go-steer/core-agent/issues/642)). The split is about who reads the alert: an interactive operator sees the warning and can hit Ctrl-C, so halting on their behalf is presumptuous. Nobody reads a daemon's warn-mode log in time, which made warn indistinguishable from off exactly where the backstop mattered. Pass `--watchdog=warn` (or set the config field) to restore observe-only on an unattended run.

The resolved mode applies to every agent the process hosts, including the sessions a multi-session daemon creates through `POST /sessions`. The startup line names the source it came from and what the mode actually does, e.g. `watchdog: enforce mode [unattended default] (…; injects the observation into the model's next turn; halts the turn in flight and refuses new ones until cleared with /guardrail reset, …)`.

`feedback` is the mode for a run where you want the agent to self-correct without a halt — an interactive session, or an autonomous job where stopping is more expensive than a few wasted turns. It is *weaker* than the unattended default, so an unattended run only gets it by asking for it explicitly.

Enforce mode mirrors the cost ceiling's halt contract (above): a trip sets a flag, the next `Run` refuses at pre-flight, and recovery is operator-driven (`/guardrail reset`, which also resets the signal's run-length state). There is no automatic reset — a tripped watchdog is a "stop, get human attention" signal, not a throttle. Only Critical signals halt; a hypothetical future low-severity signal would stay advisory even under enforce.

#### Two scopes: what a Critical is entitled to stop

Severity says how sure a signal is that something is wrong. **Scope** says what stopping it costs, and the two are not the same question ([#1090](https://github.com/go-steer/core-agent/issues/1090)):

- **Session-scoped** (the default, and every signal but one): the halt above. The turn is cut *and* every later turn is refused until an operator resets. Right when the evidence spans turns — a re-driven loop is not disposed of by ending the turn it happened to be in.
- **Turn-scoped** (`no-op-streak`): the turn is cut, a `guardrail-trip` says so, and nothing else changes. No flag, no durable row, no reset needed; the next turn runs. Right when the evidence *is* a loop inside one turn, because the model cannot keep calling a tool inside a turn that is over.

What a turn cut gives up is protection against the model restarting the loop next turn, and that is covered by an escalation rather than by widening the scope: **three consecutive turns cut by the same kind of signal halt the session**, with a reason that names the pattern. One turn that ends without a cut clears the count, so an agent that trips occasionally over an eight-hour run never accumulates its way into a halt — exactly the [per-turn cost ceiling's](#the-two-bounds-stop-different-things) escalation, for the same reason. An operator reset clears the count too, so a reset is not a treadmill one cut from re-halting.

The narrowing came out of the cluster. On the approval-gate drill, an operator denied one `alert`; the model correctly stopped re-issuing it and recorded the denial with four `mark_task_done` calls, three of which came back `no_op: true` because [the checkpoint fires once per turn](/concepts/tools/#mark_task_done); the daemon halted and the drill's remaining legs never ran. The model's mistake was stylistic — it should have ended the turn with a sentence — and the consequence was the agent's working life. A cut turn is the proportionate answer to a turn's worth of wasted calls, and it stops the loop exactly as dead.

A custom `Signal` gets the safe default for free: `watchdog.Alert.Scope`'s zero value is `ScopeSession`, so a detector written before scopes existed, or by a third party, keeps enforce's original contract without saying anything.

The run-length state has exactly one other way to be cleared, and it never clears a halt ([#1086](https://github.com/go-steer/core-agent/issues/1086)). When the [permission gate ends a turn](/concepts/permissions/#in-session-decisions) because the model kept re-issuing a call the operator refused, the calls it cut come off the signals too. The watchdog is reading the same tool stream, and a run length that survives a turn somebody else already ended means the next turn's first identical call is counted as the fifth in a row — a session halt for a turn that has done nothing wrong. Nothing about the tripped flag changes: a watchdog that genuinely halted stays halted and still waits for the operator, because an agent that could clear its own guardrail is not one.

Detection is **in-turn**, not just at the turn boundary ([#705](https://github.com/go-steer/core-agent/issues/705)). A tool loop is a loop *inside* one turn — model, tool, model, tool — and through v2.9.0-dev.0 the signals were only drained after the turn returned, so the halt arrived for a turn that had already finished burning budget. A turn that loops forever never reaches that boundary at all, which made enforce a no-op against precisely the shape it exists to catch. Under `enforce` the alerts are now drained as each tool call is observed and a Critical trip cancels the in-flight turn immediately (the same cancellation path as an operator `/interrupt`, but recorded as a watchdog halt, not an interrupt). `warn` and `feedback` keep post-turn timing — neither halts anything, and moving their log line or their injection earlier would only change when the operator reads it.

Future modes — `prompt` (pause turn + ask operator via the existing permissions prompter) and `auto` (call `Agent.SwapModel` to escalate to a frontier model without operator interaction) — are designed but deferred, along with an operator `/escalate` slash for manual model swaps. The designed *signals* are all resolved: `no-new-state` shipped the semantic-loop line ("these two calls ask the same question differently"), `tools-without-text` shipped in v3.0, and the remaining three are closed rather than deferred — see [Signals we decided not to build](#signals-we-decided-not-to-build).

### Signals

Eight signals ship. Four are `Critical` — they stop the agent under `enforce` (three of them the session, `no-op-streak` [only the turn](#two-scopes-what-a-critical-is-entitled-to-stop)) and reach the model under `feedback`. Four are `Warn`: they never halt, in any mode. The line between them is what the detector can *prove*. Three of the Critical detectors compare arguments, so when they fire the agent is demonstrably learning nothing; the fourth, `no-op-streak`, compares nothing at all — it reads a claim the tool itself made about its own result. `repeated-tool-name` is a loop detector too, but it keys on the name alone and therefore cannot tell a loop from a sweep; halting a working agent is worse than being slow to stop a stuck one, so it warns. `no-new-state` is the same trade moved from the call to the result: an identical answer cannot tell a stall from a poll. `tools-without-text` is that trade at its limit — it compares nothing at all, so a long legitimate sweep and a runaway are the same observation to it.

| Signal | Severity | Trips when | Catches |
|---|---|---|---|
| `repeated-tool-call` | Critical | The same tool is called 5 times in a row with equivalent args | The `read_file` loop from [#144](https://github.com/go-steer/core-agent/issues/144) |
| `alternating-tool-cycle` | Critical | The same sequence of 2–4 calls repeats 3 times with identical args each lap | The `list_agents → check_agent` loop that survived `stop` *and* `/interrupt` in the [PR #622](https://github.com/go-steer/core-agent/pull/622) GKE UAT |
| `dominant-tool-call` | Critical | One call accounts for 8 of the last 12 tool calls, without ever forming a run long enough for the repeat detector | A loop with occasional other calls wedged in, which resets the repeat detector's run count and reads to the cycle detector as the repeat detector's job ([#702](https://github.com/go-steer/core-agent/issues/702)) |
| `repeated-tool-name` | Warn | The same tool **name** is called 15 times in a row, whatever the arguments | A loop whose arguments are model-authored free text, so every rephrasing defeats the three args-keyed detectors above |
| `tool-failure-streak` | Warn | 3 tool calls in a row all return errors, with none succeeding in between | An agent with no tool-verified evidence about anything — the state it was in when it reported an incident "fully resolved" in the same UAT ([#639](https://github.com/go-steer/core-agent/issues/639)) |
| `no-op-streak` | Critical (turn) | 3 tool calls in a row each come back saying they changed nothing | The `mark_task_done` loop `repeated-tool-name` was built as a backstop for and could not reach: 13 rejected calls, split 7 + 6 by one interleave ([#907](https://github.com/go-steer/core-agent/issues/907)) |
| `no-new-state` | Warn | 6 successful results in a row all return a payload the turn has already seen | The reworded loop against a tool that never declares a no-op — different calls, identical answers ([#655](https://github.com/go-steer/core-agent/issues/655)) |
| `tools-without-text` | Warn | 12 tool calls in a row with no assistant text between them | Grinding in silence — the one property every recorded runaway in this project shares, and the only signal with no evasion but talking ([#655](https://github.com/go-steer/core-agent/issues/655)) |

The cycle detector was added because the v1 detector documented its own evasions ([#649](https://github.com/go-steer/core-agent/issues/649)): "consecutive" means a run of one call, so wedging a second call into the loop hid it, and literal-string arg comparison meant `main.go` and `/workspace/main.go` read as two different calls.

- **Args are path-canonicalized, narrowly.** Values under path-shaped keys (`path`, `file_path`, `dir`, `target`, …) are cleaned, so `main.go`, `./main.go` and `dir/../main.go` are one call; the consecutive detector additionally treats `/workspace/main.go` and `main.go` as one, since a genuine path suffix on a component boundary is the same file. `a/doc.go` and `b/doc.go` stay distinct — a basename match would false-positive on every repo with repeated filenames. Non-path values (a `grep` pattern, a `bash` command) are never normalized even when they look like paths.
- **One alert per stuck pattern**, not one per call past the threshold — including across rotations, so `a → b → a → b` doesn't alert twice for presenting as `b → a` on the next call.
- **One behavior, one alert — across detectors too.** The cycle detector skips blocks made of a single repeated call; the density detector stands down both for a run long enough to be the repeat detector's (5) and for a window that is a clean 2–4 call cycle; the name detector stands down when the tail of its run is args-identical, which is again the repeat detector's. So `a → a → a → a → a → a` is `repeated-tool-call` alone and `a → a → b → a → a → b …` is `alternating-tool-cycle` alone. Under `feedback` a duplicate alert is duplicated prompt text, not just a duplicated log line. **`no-op-streak` is the one deliberate exception**, and it is structural: it reads results, the four detectors above read calls, and a call-reading detector cannot know a result came back inert, so it has nothing to stand down *on*. Five identical no-op calls therefore raise both it and `repeated-tool-call`. Under `enforce` that costs nothing — the no-op streak trips at 3, two calls before the repeat detector, and the halt is idempotent, so there is one halt carrying the better attribution. Under `warn`/`feedback` it is genuinely two entries for one behavior, which is the price of reading the one piece of evidence the other four cannot see.
- **Two laps is not a cycle.** Read-grep-read-grep is ordinary exploration. Three laps with byte-identical arguments each time is not: nothing in the inputs changed, so nothing in the results can have.
- **The known false positive is a hand-rolled polling loop** written as alternating tool calls. That is what [`wait_and_verify`](/concepts/tools/#wait_and_verify-v29--bounded-poll-until-condition) exists for; an embedder who wants the pattern anyway can construct `watchdog.DefaultWatchdog` with their own signal list. `no-new-state` is the detector most exposed to this — a poll is *defined* by reading the same answer until it changes — which is why it is `Warn` and why `wait_and_verify` is exempt from it outright: flagging the bounded tool that exists to replace hand-rolled polling would be flagging the fix.

#### `dominant-tool-call` (v2.9+)

The two loop detectors above divide the space cleanly on paper. The shape that falls between them is *mostly one repeated call, with occasional other calls interleaved*: the repeat detector resets its run length on any non-matching call, so an interleave restarts the count from zero, and the cycle detector sees a nearly-uniform block and hands it back to the repeat detector.

This was a convergence gap rather than a miss, and the distinction is what set the tuning. In one GKE UAT session the backstop did fire — 22 byte-identical `gke_list_clusters` calls over two minutes and twenty seconds, halted by `repeated-tool-call` at call 22 — but the loop was visibly degenerate from about the fourth call, and the interleaves were what stretched a threshold of 5 into 22 calls and a large share of a $0.77 session. Density over a sliding window reaches the same verdict inside the first full window, because it does not care *where* the interleaves fall, only that one call dominates recent activity.

- **8 of the last 12**, with args canonicalized the same way the cycle detector canonicalizes them.
- **It stands down for the other two detectors** (see the bullet above), so adding it did not turn any existing loop into two alerts.
- **A legitimate poll is not materially more halt-prone than before.** Fitting 8 identical calls into a 12-call window already trips `repeated-tool-call` at 5 consecutive unless something interleaves. As with the cycle detector, [`wait_and_verify`](/concepts/tools/#wait_and_verify-v29--bounded-poll-until-condition) is the supported way to wait, and an embedder can construct `watchdog.DefaultWatchdog` with their own signal list.

#### `repeated-tool-name` (v2.9+)

The three detectors above all key on `(name, canonicalized args)`. That is the right key when the arguments name what the call operates on — re-reading one file, re-listing one cluster. It is the wrong key when an argument is **model-authored free text**, because the model rewords it every iteration and each call hashes differently while doing exactly the same nothing.

Observed live in a GKE demo session: asked a question after an incident was already closed, an agent called `mark_task_done` nine times in a single invocation, each with a freshly-worded one-paragraph `detail` about the same finished work. Nine distinct args hashes, so `repeated-tool-call`'s run length reset to 1 on every call and `dominant-tool-call` saw nine singleton keys. Mode was `enforce`; the watchdog reported no trip throughout, and the loop ended only because an operator hit interrupt. Cost ceilings didn't reach it either — the whole episode was about $0.23 of a $5 session budget.

- **Warn, not Critical — and the severity is what sets the threshold.** These are one decision, not two. Dropping args from the key means this signal cannot distinguish a stuck agent from a productive one working through a list, so it needs a false-positive budget. At `Critical` that budget is expensive: a false positive under `enforce` halts a working agent, so the threshold has to clear every plausible sweep, which puts it around 20 — high enough that the detector has no demonstrated catch, a guardrail priced so as never to fire. At `Warn` a false positive costs a log line and, under `feedback`, a paragraph of next-turn context the model is free to disregard. That buys a threshold low enough to be useful.
- **15 in a row, name only.** Where those meet. 12 was the intuitive number — it matches the density detector's window — but this page's own guidance is that 12 `read_file` calls over 12 distinct paths is work, and a detector whose first act is to contradict a sibling's false-positive case is one operators switch off. 15 clears it with margin and still fires before a grind has run for 20 calls.
- **It is a backstop, not the fix for that session.** Nine is under 15, and no threshold low enough to catch nine survives the budget above. The `mark_task_done` loop is fixed at the tool layer instead: a second call in the same turn now reports that the checkpoint was already recorded and that repeating cannot do anything further, rather than replying "acknowledged" again. A tool that cannot act twice in a turn should say so itself. That fix shipped and worked, and the loop continued anyway — the model kept calling and kept being told no. Watching the rejections is [`no-op-streak`](#no-op-streak-v29), below.
- **It stands down for the repeat detector** when the tail of the run is args-identical — that stretch is already `repeated-tool-call`'s, at a much lower threshold, and that alert is the stronger one: `Critical`, and able to prove the calls were redundant.
- **The alert text does not claim the calls are identical**, because here they genuinely are not. It says nothing else has happened for a long time, and asks the model either to name the list it is working through or to say what is actually blocking it.
- **Known false positive, now survivable: a genuine sweep longer than 15 calls** with no other tool interleaved. It costs noise, not a halt. An embedder who runs such workloads and wants silence can construct `watchdog.DefaultWatchdog` with their own signal list or a higher threshold.

#### `tool-failure-streak` (v2.9+)

Every other signal reads tool *calls*. This one reads outcomes, because the failure it exists for is invisible from calls alone: in the [PR #622](https://github.com/go-steer/core-agent/pull/622) GKE UAT an agent that could not reach its cluster at all reported the incident resolved — "everything is in tip-top shape" — with nothing having verified anything. The calls looked normal; the results were the story.

What it detects is deliberately narrow and objective — a run of calls that all came back as errors, with none succeeding in between. No prose is inspected. A detector that tried to recognize an over-confident *claim* would be a heuristic about English wearing the costume of a runtime guarantee, which is the defect class this release exists to remove. So this closes the **evidence** half of [#639](https://github.com/go-steer/core-agent/issues/639), not the honesty half: it tells a model that has been failing every call that it has verified nothing, at the point where it is most likely to start narrating instead of reporting. It cannot detect a confident conclusion drawn from tools that all succeeded and said nothing useful.

- **Warn, never Critical.** Under `enforce` — the unattended default since [#642](https://github.com/go-steer/core-agent/issues/642) — a Critical alert halts the agent. Halting three denials into a legitimate RBAC probe would make the backstop the outage. A failure streak is an evidence problem, so it goes to the operator log and, under `feedback`, to the model's own next turn. Provable runaway *behavior* is Critical via the three args-keyed loop detectors; `repeated-tool-name` reaches the same conclusion this bullet does, for the same reason.
- **One success resets the run**, and re-arms the alert. One success is evidence, and evidence is the thing being counted.
- **One alert per streak**, like the loop detectors — under `feedback` a re-emitting signal is a prompt leak.
- **Success and failure follow ADK's convention**: a reserved `error` key inside the function response. The agent flattens it at the bridge, so the watchdog never has to know a provider's response shape.
- **Tool outcomes are an optional observation.** A custom `Watchdog` sees them only if it implements `watchdog.ToolResultObserver`; the `Watchdog` interface itself is unchanged, so a third-party implementation doesn't break to gain a signal it may not want.

#### `no-op-streak` (v2.9+)

`repeated-tool-name` was added as a backstop for the free-text `mark_task_done` loop described above, and its own bullet concedes it cannot reach the observed one: nine calls is under fifteen, and no threshold low enough to catch nine survives the false-positive budget that section argues. A later GKE session made the gap concrete — sixteen `mark_task_done` calls, **thirteen of them answered "already recorded for this turn"**, split 7 + 6 by a single interleaved read. Every default signal stayed silent, and the tool-layer fix from [#857](https://github.com/go-steer/core-agent/pull/857) was already in place and working: the rejections were the tool doing its job. Nothing was watching the rejections.

So this signal reads what the tool said about its own result rather than what the model asked for. That is not a retuning of the loop detectors — it removes the inference entirely, and with it the precision/recall trade that kept `repeated-tool-name` at Warn:

- **`no_op` is a claim, not a guess.** A tool opts in by setting a reserved `no_op` key at the top level of its function response, exactly as `error` marks a failure; the agent flattens both at the bridge (`agent.ToolResultNoOpKey`). Nothing matches on prose. Two built-ins set it. `mark_task_done` sets it on the repeat branch — a second call in one turn cannot record a second checkpoint, and now it says so in a way a detector can read. `record_plan` sets it when the plan you sent is byte-for-byte the plan already on disk, which writes nothing at all ([#918](https://github.com/go-steer/core-agent/issues/918)); a *revision* in the same turn overwrites the artifact and is work, so it does not. Two limits worth knowing if you write tools: **it is a core-agent key, not an ADK one**, so an embedder whose in-process tool already returns a `no_op` field starts feeding this signal (rename the field, or run `DefaultWatchdog` with your own list); and **an MCP tool cannot set it**, because ADK nests MCP results under `output` and the bridge reads the top level only.
- **A runtime fault is not a no-op.** `mark_task_done` against an unbound agent looks identical from outside — nothing happened — and deliberately does *not* set the key. "Nothing needed doing" and "something needed doing and silently didn't get done" are opposites, and halting the agent for the second one would punish the model for the runtime's bug while telling it to stop doing the only thing still trying.
- **Critical, unlike `tool-failure-streak`.** The two both read outcomes, and the severity split is about what a halt costs. A failure streak may be a legitimate RBAC probe collecting denials, so halting three in would make the backstop the outage. A no-op streak has nothing in flight: every call in it has reported, itself, that it changed nothing.
- **3 in a row, and a failure is not a no-op.** The two claims are independent — a call that errored did not "do nothing", it failed to do something, and that streak has its own signal. Any productive result resets the run.
- **Any tool counts toward the run.** Three inert calls is three inert calls; a streak that alternates two idempotent tools is not less stuck for being spread across them.
- **One alert per streak**, re-armed by the next productive call. The observed 7 + 6 trace therefore raises two — the interleave genuinely ended one loop and the model genuinely started another.
- **Threshold is clamped to at least 2.** At 1, every idempotent write in the process is an alert.
- **The known false positive is a deliberately idempotent workflow** — three converge-to-desired-state calls in a row that all find nothing to do. It is a real shape, and it is why the tool has to opt in: a tool whose no-ops are ordinary should simply not set the key. `delete_file` is the in-tree example — deleting an already-absent file is a no-op success on purpose, and sweeping three missing scratch files in a row is a productive cleanup, so it stays out.
- **This is an availability change for `--checkpoint=model` + `--watchdog=enforce`, and both are defaults on an unattended run.** `mark_task_done` is the most over-called tool in the product by this page's own evidence, and three redundant calls in one turn stop the agent where before the same three cost a few cents. That is the intended trade and it is not free: a model that fires three inert checkpoints and *then* answers the operator loses the answer. It is priced as Critical anyway because the failure it replaces is worse and unbounded — the observed loop ran to an operator interrupt, and Warn would not have stopped it either. Deployments that would rather absorb the loop can set `--watchdog=feedback`, which still delivers the correction to the only party that can act on it, or take the tool away entirely with `--checkpoint=operator`.
- **[Turn-scoped](#two-scopes-what-a-critical-is-entitled-to-stop) since v3.0 — the only shipped signal that is.** v2.9 shipped this Critical at the default session scope, so those three redundant checkpoints halted a headless daemon until a human ran `/guardrail reset`. A GKE approval-gate drill priced that: an operator denying one alert is answered by the model recording the denial, repeatedly, in reworded calls, and the reset became the *modal* ending of an ordinary refusal — a halt earned on a turn where the model had already stopped doing the thing anyone wanted stopped. The evidence never justified more than the turn, either: a no-op streak resets on any productive result, so the calls it counts are consecutive and come from one turn, and ending that turn ends the loop as completely as halting the session would. Three turns in a row cut this way still halt the session ([#1090](https://github.com/go-steer/core-agent/issues/1090)) — a model that restarts the loop on every fresh turn is the shape a turn-scoped cut genuinely cannot answer.

#### `no-new-state` (v3.0+)

Every detector above `no-op-streak` keys on the **call** — the name, the canonicalized arguments, the shape of a window. All of them share one evasion, and `repeated-tool-name`'s own section concedes it: a model that rewords its arguments produces a different key on every iteration while asking the identical question. `no-op-streak` answered that by reading a claim the tool makes about itself, and it only reaches tools that opted in. A `read_file` returning the same file for the sixth time is not a no-op — it did exactly what it was asked, successfully, and has no idea you already had those bytes.

This signal reads the **answer** instead. Two calls that ask the same question differently get the same payload back, so a digest of the result catches the rewording without having to understand it — no semantic comparison, no embeddings, no second model. It is the deferred *"these two calls ask the same question differently"* from [#655](https://github.com/go-steer/core-agent/issues/655), solved by not comparing the questions.

- **6 successful results in a row carrying a payload the turn already saw.** Between `no-op-streak`'s 3 and `repeated-tool-name`'s 15, for the reason both of those use: a no-op is a confession and needs no margin; a tool name is almost no evidence and needs a lot; an identical payload is strong evidence with one confound, below.
- **Warn, so it never halts.** A repeated answer cannot tell a stalled agent from one polling a cluster that has not converged yet — and on the Kubernetes work this project is aimed at, polling is routine. Under `feedback` the guidance still reaches the model, which is the only party that can stop making the call.
- **`wait_and_verify` is exempt.** Its contract *is* "call me until the state changes", and it bounds its own waiting. A hand-rolled poll — the same `read_file` or cluster read six times — is exactly what should be caught, because nothing bounds it. The guidance names `wait_and_verify` as the alternative rather than just reporting the stall.
- **It keys on the payload, not the tool.** Two different tools returning the identical answer share a digest, on purpose: that is what a model rewording its way from one tool into another looks like, and folding the name into the hash would blind the detector to it.
- **A result it cannot fingerprint resets the run.** Absence of evidence is not evidence of a stall. Every failure to digest costs recall and none of it costs precision — which is the right direction for a detector whose alert is prompt text under `feedback`.
- **Failures and no-ops are skipped, not counted as progress.** They belong to `tool-failure-streak` and `no-op-streak`; three detectors alerting on one behaviour teaches an operator less than one. But they do not *reset* the run either — otherwise a stalled agent that errors once every five calls stays under the threshold forever.
- **Memory is bounded at the last 64 distinct payloads,** and an evicted payload reads as new information again. That is the honest reading: the claim is "this turn already had it", and past the bound the detector no longer knows that it did.
- **"Already seen" is scoped to the turn**, and this is the one place the watchdog's state is not session-lived. Everything else here is a consecutive run, which self-limits: the next call that breaks it clears it whether or not a turn ended in between. A remembered payload does not self-limit, and a watchdog is cleared only by an operator reset — so without a boundary, a daemon woken every ten minutes to read six unchanged resources would produce six already-seen results on its *second* wake and be told it was stalling, once per wake, forever. That agent is monitoring correctly. A loop that survives a turn boundary is therefore not this detector's to catch; that is re-driven work, which the auto-continue bound and the cost ceilings already cover. A refused turn is not a boundary either — it never ran, so nothing in it could have made progress, and counting it would let a re-drive launder a stall one refusal at a time. Embedders get the boundary for free: `Agent.Run` calls it, and a custom `watchdog.Watchdog` opts in by implementing the optional `watchdog.TurnObserver`, exactly as `ToolResultObserver` works.

#### `tools-without-text` (v3.0+)

Every detector above asks a question *about the calls*: is this one identical to the last, does the recent window have a period, does one name dominate it, did they all fail, did the tools say they did nothing, is the data coming back data we already had. Each is a comparison, and each has an evasion — which is why there are seven of them. A model that rewords its arguments beats the first four; one whose calls all succeed beats the fifth; a tool that never opts into `no_op` beats the sixth; genuinely-different answers beat the seventh.

This one compares nothing. It counts how many tool calls went by without the model saying a word, and the only way to evade it is to talk — which is the behaviour we wanted. Silence is the one property every recorded runaway in this project shares: the `read_file` loop, the alternation, the dominance case, the thirteen `mark_task_done` calls all ground away without a word of assistant text, and none of them had to.

- **12 calls in a row, and the threshold was measured rather than guessed.** The design doc proposed 15 in a table headed "initial guess". Sixty-four archived GKE-drill sessions — real runs against a live cluster, all judged good at the time — put the longest textless run at **10**, the 95th percentile at 5 and the median at 2. Twelve clears the observed ceiling by a fifth and sits under the [#905](https://github.com/go-steer/core-agent/issues/905) loop's fifteen. The corpus is committed and the margin is a test, not a comment: `TestDefaultToolsWithoutTextClearsTheRecordedCorpus` fails if a future extraction contains a good session that would trip this.
- **Warn, and this is the least close call in the package.** All it knows is that a number got large. A deep but legitimate sweep and a runaway are identical to it by construction, so it never halts; under `feedback` the note to say what you have found costs a working agent one paragraph and gives a stuck one the only nudge that has ever stopped one of these without a human.
- **One non-whitespace sentence from the model clears the run completely.** That is not a loophole. A model narrating between sweeps is doing the thing the signal wants, and one that narrates its way through a genuine loop is caught by the six detectors that read the calls.
- **Only the agent's own words count.** Not tool results (the runtime talking), not injected inbox messages or the watchdog's own feedback block (us talking), and not the model's private reasoning where the provider marks it — a trace nobody is shown does not answer "has anyone been told anything". An agent grinding in silence must not have its run cleared because somebody talked *to* it.
- **Raise it if you run long sweeps.** Every session in the corpus is Kubernetes triage. A twelve-call silent sweep through a codebase is plausible in a way a twelve-call silent sweep through a cluster is not; an operator who sees this fire on one should raise the threshold rather than treat it as a finding. That it only ever costs a log line is what makes the tighter number affordable.

### Signals we decided not to build

The design doc's signal table listed three more. All three are closed rather than deferred, because each is already covered by something that ships and *acts* rather than warns:

| Proposed | Why not | What covers it |
|---|---|---|
| `files-not-touched` ("turns since a new file path was read") | Fires whenever a read-only cluster agent does its job correctly, and assumes the workload has a filesystem — most of this project's agents do not | `no-new-state`, which asks the same question (is new information arriving) without the assumption |
| `context-growth-rate` | Warning an operator that a context is filling, next to a component whose job is to empty it, is decoration | [Per-tier compaction](#per-tier-thresholds-since-v25) ([#119](https://github.com/go-steer/core-agent/issues/119)) |
| `cost-burn-rate` | Strictly weaker than a ceiling, on evidence the watchdog would have to be handed rather than observe | The session cost ceiling ([#145](https://github.com/go-steer/core-agent/issues/145)) and the per-turn ceiling ([#1049](https://github.com/go-steer/core-agent/issues/1049)), both of which stop |

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

The `Watchdog` interface lets you plug in a custom implementation (same composability pattern as `Compactor` / `Checkpointer`). For most operators the default — `NewDefaultWatchdog()`, which wires all six signals in the table above — is sufficient.

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
core-agent --model claude-opus-5 --agentic-small-model claude-opus-5

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

Checkpoints run the same summarizer compaction does, so a model-driven one that comes back empty behaves the same way — see [When the summarizer comes back empty](#when-the-summarizer-comes-back-empty-since-v29). A lost checkpoint is the cheaper of the two failures: re-cut it with `/done`.

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

### Choosing who declares a boundary

Three parties can declare one: the model (via the `mark_task_done` tool), the operator (via `/done`), and the runtime (via the `Checkpointer` heuristic, off in the default implementation). `--checkpoint` picks which of them are live — config-file equivalent `checkpoint.mode`, so a recipe can ship its posture instead of relying on every invocation and deploy manifest remembering a flag.

| Mode | `mark_task_done` | `/done` | Heuristic | Use when |
|---|---|---|---|---|
| `model` (default) | ✅ | ✅ | ✅ | Interactive sessions with recognizable task boundaries |
| `operator` | ❌ | ✅ | ✅ | Long-lived services — see below |
| `off` | ❌ | ❌ | ❌ | Debugging, where auto-slicing complicates reproduction |

```json
{ "checkpoint": { "mode": "operator" } }
```

**Why `operator` exists.** `mark_task_done` is a model-facing tool, and its description is prompt text the model reads at the moment it decides what to call. That description used to instruct the model to use the tool "generously at natural task boundaries", and its `detail` argument asked for a completion summary. Both were written for an interactive coding session.

A daemon consuming machine signals has no conversation about to shift to a new task, so it sees a boundary everywhere: one live deployment produced sixteen `mark_task_done` calls in a single session, thirteen of them rejected as no-ops, and answered unrelated operator questions with completion reports instead of answers. The recipe's own instructions forbade exactly that and lost — a tool description outranks the persona at the point of decision, and a recipe author cannot edit it. `operator` mode removes the affordance instead of arguing with it.

The description and arg schema were also rewritten, so `model` mode is better behaved than it was; `operator` is the posture for a deployment where the model should never declare its own boundary at all.

**Checkpointing off is not context reduction off.** Compaction is a separate mechanism that fires on context-window utilization with no model involvement, and none of these modes touch it. Use `--no-compact` for that.

`--no-checkpoint` still works as a deprecated alias for `--checkpoint=off`. Its old help text promised only that the model would stop self-signalling completion, but it also removed `/done` and the heuristic — which is exactly the split `operator` mode now makes available. Passing it prints a deprecation notice; passing it alongside a contradicting `--checkpoint` is a config error rather than a silent precedence rule.

`/help` and `/tools` reflect whichever mode is active.

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
