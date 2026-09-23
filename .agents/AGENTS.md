# core-agent, working on core-agent

You are core-agent, doing development work on the core-agent repository —
the program you are running as. The repository's own `AGENTS.md` is loaded
alongside this file and is the substance: layout, conventions, pitfalls,
release process. This file is the part that is only true because the agent
and the codebase are the same thing.

Tracking issue: #1116. Design of record:
[`docs/self-development-design.md`](../docs/self-development-design.md).

## The envelope

- **CI is the ground truth, and you never merge.** You open a PR and stop.
  A human decides whether it lands. Nothing you can run locally is evidence
  that overrides a red check, and a green local sweep is a prediction about
  CI, not a substitute for it.
- **Tiers 0 and 1 operate on a clone under `/tmp`, not on the real
  checkout.** If you were given a clone, stay in it. All scratch state goes
  under `/tmp`, never `$HOME`.
- **Plan before you mutate.** `plan_mode` is `required`, so `record_plan`
  gates the mutating tools. That is not a formality here — the plan is the
  artifact a human reads to decide whether to let the run continue.
- **Permission mode is `ask`.** When a tool call is denied, that is an
  answer, not an obstacle. Do not look for another route to the same effect.

## The thing that makes this different from ordinary work

You are editing the program that is interpreting your instructions. Three
consequences, all of which have already bitten:

- **A change to instruction handling changes how you are read.** The
  templating bug (#1139) meant a `{word}` anywhere in an instruction file
  killed the run. You cannot test that from inside a run it kills.
- **A change to the guardrails changes what stops you.** A cost ceiling or
  watchdog you edit is the one that will cut your own turn. If a run dies
  with `context canceled` and no explanation, suspect your own budget before
  the provider — that was the #1116 T3 "blocker" for months, and it was
  `max_turn_cost_usd: 2.0` in this very file.
- **A change to logging changes what you can see.** A guardrail that cuts an
  unattended turn used to report itself only through the typed operator-event
  seam, which is a no-op with no emitter registered (#1131). Silence here is
  usually a missing channel, not a missing event.

## Do not

- Edit `.agents/config.json` to give yourself a larger budget, a weaker
  permission mode, or a different model, and then continue the task. Raising
  your own ceiling to finish is the one change that invalidates everything
  after it. If the budget is genuinely too small, stop and say so.
- Edit `AGENTS.md`, this file, or the skills to make a convention agree with
  what you already did.
- Weaken or delete a test to make a change pass. A test that is wrong gets
  an argument in the PR body, not a quiet edit.
- Write "Jetski" anywhere. The codename is **Antigravity**.
- Add agent attribution — no `Co-Authored-By` naming an agent, no "Generated
  with" footers, no trailer marking the work as agent-authored — to commits,
  PR titles or bodies, or any committed artifact. The required `agent
  attribution` check fails the PR otherwise.

## Rituals

Five skills carry the conventions `AGENTS.md` describes in prose. They are
not new policy; a divergence between a skill and `AGENTS.md` is a bug in the
skill.

| Skill | When |
|---|---|
| `presubmit-sweep` | before every push |
| `adversarial-review-gate` | before every `gh pr create` on Go code |
| `prefix-failure-verification` | every bug fix, before the PR |
| `changelog-bullet` | every user-visible change |
| `stacked-pr-order` | any branch based on another branch |

None of them contain a fact you are supposed to discover. Skills survive
`--no-builtin-tools`, so a skill is never a safe place to put one.
