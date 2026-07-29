# System prompt layering: minimal core, provider quirks, mode overlays, user layers

> **Status (2026-07-29):** SHIPPED — #459 implemented this design
> (PR #522), and the #460 follow-up landed the executor serialization
> of mutating tools, taking the edit-sequencing paragraph's marked
> exit path: that paragraph is now DELETED from `CoreInstruction` and
> the dispatch-fact sentence states the serialized contract. The
> quoted layer-1 text below is the as-designed version; `pkg/agent`
> is the living source.

Design doc for restructuring how core-agent assembles system
instructions. Sibling to `docs/instruction-loader-v2-design.md` (the
user-layer loader this composes with), `docs/scheduled-monitoring-design.md`
(whose instruction/tool-description split this generalizes), and
`docs/coding-agent-instructions.md` (the survey of Crush's and Claude
Code's prompt surfaces that fed this design).

## Context

### What exists today

The full instruction surface, bottom to top:

- **`agent.DefaultInstruction`** (`pkg/agent/agent.go:74-82`, ~350
  words) — one exported const mixing five unrelated concerns: a
  persona line, a plan-sketch rule, a parallelism mandate (load-bearing
  for Gemini per the measurement in `dev/parallel-probe/`), an
  edit-sequencing safety rule, and the compaction/handover
  interpretation contract.
- **`agent.DefaultSchedulingInstruction`** (`agent.go:106-116`) —
  opt-in by composition for paced loops. Unchanged by this design.
- **User memory** — the `pkg/instruction` loader (AGENTS.md /
  CLAUDE.md / GEMINI.md, `.agents/`, `AGENTS.d/*.md`, `@include`)
  assembled per scope and installed as a *prefix* via
  `agent.WithSystemInstructionPrefix` (`cmd/core-agent/main.go:1022-1040`).
- **Mode** — nothing. Autonomous and interactive runs send the same
  system prompt. The autonomous driver (`pkg/agent/autonomous/`)
  differs only in tools (`report_done`), the goal-as-first-prompt
  loop, and continuation prompts. Guidance like "You are an
  autonomous worker…" lives only in `examples/autonomous/main.go:88-96`
  and `docs/autonomous.md:47-53` — every consumer re-types it, or
  forgets to.
- **Subagents** — `BackgroundAgentManager` requires a per-spec
  `SystemPrompt` (`pkg/agent/background/spawn.go:299-300`) passed to
  `agent.WithInstruction`, which is a **full replace**. Spawned agents
  silently lose the compaction contract and the edit-safety rule.
  `RunSubtask` (`pkg/agent/subtask.go`) and the remote-TUI spawn path
  (`internal/coretuiremote/capabilities.go:1243`) have the same shape.
- **Operator override** — none. No `--system-prompt` /
  `--append-system-prompt` equivalent and no config field, despite
  `docs/coding-agent-instructions.md:133-138` cataloging exactly that
  surface in Claude Code as the industry-benchmark customization
  model.

### Why restructure now

Anthropic's Claude-5-generation context-engineering guidance (and the
80%+ system-prompt reduction they shipped in Claude Code with no eval
regression) lands on three findings that map directly onto our
current problems:

1. **Rules written for older models are now dead weight, and
   conflicting directives measurably cost deliberation.** Our persona
   and plan-sketch lines are exactly the category they deleted. Worse,
   because user memory is *prepended*, a project's AGENTS.md that says
   "always explain your plan in detail" now conflicts with our
   plan-sketch line that says "1-3 sentences" — and the model has to
   spend attention reconciling text we control.
2. **Tool guidance belongs in tool descriptions, stated once.** We
   already follow this for `schedule_next_turn` and `report_done`
   (the "Layer 1" pattern in `docs/scheduled-monitoring-design.md`).
   This design extends that posture to the whole prompt: the system
   prompt carries only what no tool description can.
3. **Prompt-cache economics reward stable-prefix ordering.** Content
   that never changes should come first so a per-project memory edit
   doesn't invalidate the cached core. Today's ordering
   (memory-as-prefix) is exactly backwards, for both caching and
   precedence: the immutable core currently comes *last*, where it
   reads as overriding the user's own instructions.

The forcing function is real use: every serious deployment customizes
prompts (per-caller overlays already exist for multi-session; Scion
and AX supply their own instructions), and today each of them either
composes `DefaultInstruction` by hand or replaces it wholesale and
loses the harness contract without knowing it.

## The layer model

### Admission test for the core

A line earns a place in the always-on core only if it is one of:

1. **Harness contract** — a fact about this runtime the model cannot
   discover by looking: how compaction/handover summaries arrive, how
   the runtime dispatches tool calls. Irreducible.
2. **Safety invariant the runtime does not (yet) enforce** — prompt
   text standing in for a missing executor guarantee. Every such line
   carries a marked exit path: when the runtime enforces it, the line
   is deleted, not softened.
3. Nothing else. Provider workarounds go in the quirks layer; taste
   and disposition go in mode overlays or user layers; tool mechanics
   go in tool descriptions.

### The stack

Assembled top-to-bottom, **stable → volatile**:

| # | Layer | Source | Changes when |
|---|-------|--------|--------------|
| 1 | Core contract | `agent.CoreInstruction` | core-agent release |
| 2 | Provider quirks | selected by model at `agent.New` | model changes |
| 3 | Mode overlay | `agent.WithMode` (default interactive) | mode changes |
| 4 | User memory | `pkg/instruction` loader (user → project → per-caller) | project/session |
| 5 | Consumer/operator append | `WithExtraInstruction` / config / flag | deployment |

Later layers are more specific and — by ordinary instruction-following
convention — win when they conflict with earlier ones. That is the
correct precedence: a project's AGENTS.md *should* be able to override
our interactive overlay's communication defaults; nothing should be
able to silently override the compaction contract except an explicit
full replace.

`WithInstruction` survives unchanged as the full-replace escape hatch
(layers 1–3 skipped entirely, caveat emptor — same contract as Claude
Code's `--system-prompt`).

## Proposed texts

Word counts are the point; each is quoted in full. The existing
~350-word monolith becomes a ~150-word core plus small optional
layers.

### Layer 1 — `agent.CoreInstruction`

```
Independent tool calls issued in the same response execute in
parallel; a call that depends on another call's result must go in a
later response.

Do not issue multiple `edit_file` or `write_file` calls targeting the
same path in one response — those must run sequentially across turns
so each edit sees the prior result; parallel writes to the same file
race and corrupt state. If you are unsure whether two operations are
independent, run them sequentially.

Earlier conversation may have been summarized into context for you in
one of two shapes: "[Conversation compacted…]" framing (we hit the
context wall mid-task and the prior turns were condensed), or "[The
prior task is complete…]" framing (the prior task closed cleanly and
a handover record replaces its history). Both arrive wrapped at the
start of your context, both are authoritative shared history. Read
FROM them when the user references prior work — what was discussed,
what files were touched, what was decided — rather than re-running
tools to rediscover what's already recorded there. The conversation
continues in both cases; treat the framing as picking up an
in-progress session, not as a fresh start.
```

Three items, each passing the admission test:

- **Parallel dispatch fact** — harness contract. Note this is the
  *fact* only; the exhortation to batch moves to the Gemini quirk.
- **Edit sequencing** — safety invariant, category 2. Exit path: a
  follow-up (out of scope here) serializes mutating tools in the
  executor the way the Claude Agent SDK does (read-only tools run
  concurrently, state-mutating tools run sequentially), after which
  this paragraph is deleted.
- **Compaction contract** — harness contract, kept verbatim from
  today's text. This is the paragraph subagents are silently losing.

### Layer 2 — provider quirks

One const per measured workaround, selected automatically in
`agent.New` from the model identifier (same resolution surface as
`docs/model-selection-design.md`), suppressible via
`WithoutProviderQuirks()`.

`agent.GeminiParallelismQuirk` (applied to Gemini models):

```
Execute multiple independent tool calls in parallel when feasible —
searching, reading files, independent shell commands, or editing
different files. When investigating code, if you need to read
multiple files or grep multiple directories, issue all the tool
calls in a single response; do not execute them one by one.
```

The `dev/parallel-probe/` measurement (65 search turns, zero batching
without the mandate on Gemini-3.1-pro; Claude "less affected,
marginal benefit") is exactly the evidence standard for this layer:
**no quirk ships without a probe result in its doc comment**, and
each quirk names the models it applies to so it ages out when a
provider fixes the behavior. Claude models get no quirks today.

### Layer 3a — `agent.InteractiveOverlay` (default)

```
A user is present. Before starting non-trivial work — multi-file
edits, architectural choices, asks with multiple valid approaches —
say what you're about to do in a sentence or two so they can
redirect cheaply; skip the preamble for trivial asks. When blocked
on a decision only the user can make, ask one focused question
rather than guessing. Report outcomes plainly, including failures
and steps you skipped.
```

The plan-sketch rule survives in both overlays rather than the core,
with different rationales: here, because a present user can redirect
cheaply before work is done; in the autonomous overlay, because the
narration is the audit record of the run. The persona line ("You are a helpful assistant. Be concise and accurate.")
is deleted entirely: identity already enters the prompt via ADK's
description mechanism (`WithDescription` → "you are an agent named X"),
and concision/helpfulness are judgment current models have.

### Layer 3b — `agent.AutonomousOverlay`

```
You are operating autonomously: no human reads your output in real
time, and questions posed in it go unanswered — do not ask for
clarification in your responses. Proceed on reversible actions that
follow from the goal; gather missing information with your tools
instead of asking, and prefer a recorded reasonable assumption over
stalling. Before each multi-step
or consequential series of actions, state in a sentence or two what
you are about to do and why — nobody will approve it; your output is
the audit record of the run. Verify your work before declaring it
done: run the checks that exist rather than asserting success. End
your turn only when the goal is complete or blocked on something no
tool can resolve, and say which.
```

The narrate-before-acting line is deliberately here and not left to
AGENTS.md: the eventlog/OTel trace is a runtime property of every
autonomous deployment, and stated intent is what makes a burst of
tool calls legible in that record after the fact. It's still
disposition (no approval implied), so it lives in the overlay, not
the core.

The no-clarification line is deliberately scoped to *questions in
output text* — those genuinely go unanswered. It does not say "never
ask through any channel," because autonomous deployments legitimately
install ask channels (`ask_user` with a live prompter, the Scion
status-and-new-turn pattern — `docs/autonomous.md:70-124`). When such
a tool is present, its description carries the exception; see the
`NewAskUserTool` change under API changes.

Deliberately mechanics-free: `report_done`, `schedule_next_turn`, and
`ask_user` semantics stay in their tool descriptions, where they are
stated once and only load when the tool is present. The overlay
carries *disposition* — the piece that is true regardless of which
lifecycle tools happen to be installed. `DefaultSchedulingInstruction`
remains a separate opt-in composed alongside this overlay when a
scheduler is installed, exactly as today.

### Disposition of the current `DefaultInstruction`, line by line

| Current text | Disposition |
|---|---|
| "You are a helpful assistant. Be concise and accurate." | Deleted (identity comes from `WithDescription`; judgment, not contract) |
| Plan-sketch paragraph | → both overlays, softened (interactive: cheap redirection; autonomous: audit/log record) |
| "Tools execute in parallel by default." | → Core (fact) |
| "Execute multiple independent tool calls in parallel…" | → Gemini quirk (measured exhortation) |
| Edit-sequencing paragraph | → Core, with marked exit path (executor serialization) |
| Compaction/handover paragraph | → Core, verbatim |

## API changes

### `pkg/agent`

```go
// New exported consts (DefaultInstruction is retired; see migration).
const CoreInstruction = `…`
const InteractiveOverlay = `…`
const AutonomousOverlay = `…`
const GeminiParallelismQuirk = `…`

type Mode int
const (
    ModeInteractive Mode = iota // default
    ModeAutonomous
)

func WithMode(m Mode) Option              // selects layer 3
func WithExtraInstruction(s string) Option // layer 5 append slot (repeatable)
func WithoutProviderQuirks() Option        // suppress layer 2
// WithInstruction(s) unchanged: full replace, skips layers 1–3.
// WithSystemInstructionPrefix deprecated → memory now enters as layer 4
// via a dedicated option the CLI uses:
func WithUserInstruction(s string) Option  // layer 4 (loader output)
```

`agent.New` assembles layers 1→5 joined by blank lines, in the fixed
order above, unless `WithInstruction` is present. Each layer that is
empty is simply omitted — no headers, no placeholders. (The loader
already emits its own `# <Scope> memory (<path>)` headers inside
layer 4.)

### Mode selection is consumer-side, with in-tree consumers fixed

The earlier instinct was to have `RunAutonomous` auto-inject the
overlay. It can't, cleanly: the driver takes a
`build func(extras []tool.Tool)` closure and never constructs the
agent itself, and we've already settled (background-subagents design)
that drivers don't mutate caller-supplied agents. So:

- `RunAutonomous` **documents** `WithMode(ModeAutonomous)` as required
  in the build closure, and its examples/docs are updated to use it in
  place of the hand-typed "You are an autonomous worker…" text.
- If the built agent exposes `Mode() == ModeInteractive`, the driver
  logs a one-line warning at start ("autonomous run with interactive
  overlay; did you mean WithMode(ModeAutonomous)?"). Warning, not
  error — a consumer replacing the whole prompt via `WithInstruction`
  knows what they're doing.
- The in-tree autonomous consumers — `BackgroundAgentManager`
  (`background/spawn.go`), `RunSubtask`, and the remote-TUI spawn
  path — set `WithMode(ModeAutonomous)` themselves. These are where
  the footgun actually fires today, and we own them.

### Subagent prompts compose instead of replace

`background.Spec.SystemPrompt` (and the `spawn_subagent` /
remote-spawn tool `system_prompt` args) reroute from
`WithInstruction` to `WithExtraInstruction`: the subagent gets
core + quirks + autonomous overlay + its task-specific instruction.
A new `Spec.ReplaceSystemPrompt bool` (default false) preserves the
old full-replace behavior for the rare consumer that wants a bare
prompt. Same change in `RunSubtask` and
`internal/coretuiremote/capabilities.go`.

This quietly fixes a live bug: spawned subagents compact like any
other agent but currently have no idea what a compaction summary is.

### `tools.NewAskUserTool` description

The autonomous overlay's no-clarification line is scoped to output
text, so the sanctioned ask channel carries its own exception where
it belongs — in the tool description. `NewAskUserTool`'s description
gains one line: *"If this tool is available, a human is reachable:
use it for decisions that genuinely block progress, instead of
guessing or asking in your response text."* Deployments using
`RefusePrompter` are unaffected — the model still gets the in-band
`(no user available: …)` refusal and adapts, exactly as today
(`docs/autonomous.md:112-115`).

### CLI / config

```yaml
agent:
  append_system_prompt: |        # → WithExtraInstruction (layer 5)
    ...
  system_prompt_file: path.md    # → WithInstruction (full replace)
```

Plus `--append-system-prompt <text|@file>` and
`--system-prompt-file <path>` flags mirroring the config, with flag
beating config. Append is the documented, encouraged path; full
replace gets the same warning Claude Code's docs give theirs (you
lose the harness contract; tool-use degradation is on you). The
`/memory` attach endpoint (`main.go:1049-1061`) grows a line
reporting which layers are active so operators can see the assembled
shape without reading code.

## Settled design decisions (do not relitigate — design around them)

- **One core + small overlays, not two prompts.** Autonomous and
  interactive share layers 1–2 verbatim; the mode delta is
  disposition only (~70–110 words) and never mechanics. Precedent:
  Claude Code appends an autonomy section to one base prompt rather
  than maintaining two prompts.
- **The admission test is the spec for layer 1.** Anything that is
  not harness contract or an unenforced safety invariant does not go
  in the core, no matter how good the advice is. Advice goes in
  overlays, skills, or user layers.
- **Tool mechanics live in tool descriptions.** Extends the
  scheduled-monitoring "Layer 1" pattern to the entire prompt
  surface. No tool name may appear in a mode overlay.
- **Quirks require measurement.** A provider quirk ships with a
  probe result (à la `dev/parallel-probe/`) cited in its doc comment
  and an explicit model list. No "probably helps" text.
- **Stable → volatile ordering.** Core first, user layers after —
  for prompt-cache economics and so user instructions naturally take
  precedence over our defaults. This inverts today's
  memory-as-prefix arrangement on purpose.
- **`WithInstruction` stays a full replace.** Changing its semantics
  under existing consumers is worse than adding
  `WithExtraInstruction`. The escape hatch keeps its sharp edge and
  its warning label.
- **Mode is set where the agent is built.** No driver mutates a
  caller-built agent; `RunAutonomous` warns instead. In-tree spawn
  paths are updated because we own both sides there.

## Out of scope / follow-ups

- **Executor serialization of mutating tools.** The Agent SDK
  pattern: read-only tools run concurrently, mutating tools run
  sequentially, enforced by the runtime. Lands separately; when it
  does, the edit-sequencing paragraph is deleted from
  `CoreInstruction`. Tracked as the marked exit path.
- **Output-style / persona presets.** Claude Code's
  `.claude/output-styles` analog. Deferred until a consumer asks;
  layer 5 covers the need un-ergonomically in the meantime.
- **Per-quirk eval harness.** `dev/parallel-probe/` generalized into
  a re-runnable probe per quirk so quirks can be retired when
  providers improve. Worth doing; not blocking.
- **Hugo site docs** (`docs/site/content/docs/`) get the
  operator-facing pieces (config fields, flags, layer table) in the
  implementation PR, not this doc.

## Resolved questions (2026-07-27 review)

1. **Does deleting the persona line regress non-Claude providers?**
   Unanswerable at design time (`dev/parallel-probe/` needs live
   model access), so it's settled as an implementation gate rather
   than an open question: the core ships without the persona line,
   and the implementation PR must include a persona-probe run on
   Gemini (same posture and evidence standard as the parallelism
   probe) before merge. If Gemini regresses, the line ships as a
   `GeminiPersonaQuirk` with the probe result in its doc comment.
   The layer architecture is identical in both outcomes, which is
   what makes this safe to settle now.
2. **Should `ModeAutonomous` imply anything for `ask_user`?** No
   coupling. Resolved by scoping instead: the overlay's
   no-clarification line targets questions in output text (which go
   unanswered), and a deliberately installed ask channel carries its
   own exception in its tool description (see `NewAskUserTool` under
   API changes). `--ask` and mode remain independently set;
   `RefusePrompter` continues to teach the model in-band that no one
   is home.
3. **`DefaultInstruction` retirement pace.** Deprecated alias:
   `DefaultInstruction = CoreInstruction + "\n\n" + InteractiveOverlay`
   through the v2.8.x series, deleted at the next breaking window
   alongside `WithSystemInstructionPrefix`. External consumers
   compose against it by name, so it keeps compiling with close to
   today's semantics (minus persona/plan-sketch, per the disposition
   table) until then.

## Migration notes

- Consumers using `agent.New(m)` with no instruction options get
  core + quirks + interactive overlay — behaviorally close to today,
  minus the persona/plan-sketch text.
- Consumers doing `WithInstruction(DefaultInstruction + extra)`
  migrate to `WithExtraInstruction(extra)` and, if autonomous,
  `WithMode(ModeAutonomous)`. The deprecated alias (resolved
  question 3) keeps them compiling through v2.8.x.
- `cmd/core-agent` swaps `WithSystemInstructionPrefix(loaded.Instruction)`
  for `WithUserInstruction(loaded.Instruction)`; operator-visible
  effect is ordering only (memory moves from before the baseline to
  after it), which is the intended precedence flip.
- `examples/autonomous/`, `docs/autonomous.md:30,47-53`, and the
  background-subagents docs drop their hand-typed autonomous
  boilerplate in favor of `WithMode`.
