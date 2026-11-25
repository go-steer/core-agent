# Thinking level: one setting across providers

**Status:** shipped, 2026-11-25 ([#1605](https://github.com/go-steer/core-agent/issues/1605)).

## Motivation

No thinking configuration reaches the model on core-agent's turns, so every
model thinks at its provider's default. That default moved under us:

- **Claude Opus 5.5 defaults to effort `medium`,** one band below Opus 5.
- **Gemini 4 refuses any thinking budget,** so a level is the only control
  left.

core-models v0.16.0 makes `genai.ThinkingConfig.ThinkingLevel` a
provider-neutral control: it maps the level per model onto Claude's
`output_config.effort`, Gemini 4's `thinkingLevel` and OpenAI's
`reasoning.effort`, fitted to what each model accepts (measured on Vertex AI).
core-agent needs a way to set it.

## Settled decisions (do not relitigate)

- **One config field, `model.thinking_level`:** `minimal`, `low`, `medium`,
  `high`, `xhigh` or `max`, case-insensitive. Anything else is a load-time
  error (`validateThinkingLevel`), because core-models ignores a level it
  doesn't recognise and a typo would otherwise do nothing.
- **Applied by a `BeforeModelCallback` (`agent.WithThinkingLevel`), not a
  model or provider wrapper.**
  - It reaches only the agent's own turns.
  - It survives `/model` switches.
  - It leaves providers' optional interfaces (`SmallModelDefaulter`,
    `BuiltinToolsReporter`, ...) untouched, which a provider wrapper would
    hide.
- **Side calls are not affected.** Session titles, the approver, compaction,
  the summarizer and digests drive the model directly and keep their own
  settings.
- **A request that already carries a thinking level or budget is left
  alone.** The callback only fills the gap, and it runs after every
  `WithBeforeModelCallbacks` callback, so a caller's own setting stands
  whatever the option order. The last `WithThinkingLevel` wins, and
  `New` rejects an unknown level.
- **Inheritance:**
  - A declared subagent uses its own `model.thinking_level` when set,
    otherwise the parent's (`Config.SubagentThinkingLevel`), even when its
    tier resolves to a smaller model.
  - A background spawn inherits the spawning agent's level
    (`Agent.ThinkingLevel`). A spawn of a declared subagent uses that
    subagent's own level (`SubagentTemplate.ThinkingLevel`) when set.
  - Every session a multi-session daemon builds (`compose.ReproduceAgent`:
    POST /sessions, resume, A2A, scheduled) uses `model.thinking_level`.
  - Subtasks (`RunSubtask`) are side work and get none.
- **What a level does downstream** is core-models' table: on Claude and
  Gemini 4 it is fitted to the model; Gemini before 4 receives it as
  given; on Claude 4.6 and later any level turns adaptive thinking on.

## Out of scope

- A CLI flag. Config covers the use cases so far.
- Per-model levels across a `/model` switch. The level belongs to the agent,
  not the model.
- Mapping a level for providers core-models doesn't map (Chat Completions
  profiles take thinking through `extra_body`).
