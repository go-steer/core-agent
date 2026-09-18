# The first-party persona library

> **Status (2026-09-18):** SHIPPED — #656. Three builtin personas
> (`core`, `coder`, `sre`) ship in the binary and are referenced from a
> recipe's `AGENTS.md` with `@include builtin:NAME`.

Sibling to `docs/system-prompt-layering-design.md` (the layers *below*
this one — core instruction, provider quirks, mode overlays) and
`docs/instruction-loader-v2-design.md` (the `@include` mechanism this
rides on). `docs/coding-agent-instructions.md` is the prior-art survey
of other projects' prompts; this is the first-party answer to it.

## The problem this closes

Every recipe in this repo used to write its persona from scratch, and
the ones written by importing another project's system prompt behaved
worst. The failures were repetitive and none of them were about the
domain:

- An agent announced "all tasks complete, exiting session" and stopped
  answering, because the imported prose described a task worker with a
  lifecycle. core-agent agents do not have one.
- An agent read its own process environment to work out what it was
  allowed to do, instead of reading its tool list.
- An agent answered a general question ("is this normal?") with a
  three-section incident report, because its persona only knew one
  output shape.
- Agents ground through long tool sequences without saying anything,
  and the operator could not tell a deep investigation from a hang.

That is not domain knowledge. It is what a core-agent agent *is*, and
it should not have to be rediscovered once per deployment.

## Identity → equipment → conduct

A persona has three layers, and only two of them are portable.

| Layer | What it says | Portable? | Ships as |
| --- | --- | --- | --- |
| **Identity** | What kind of process you are; what honesty means here | Yes, always | `builtin:core` |
| **Conduct** | How work of a given kind is done | Yes, within a line of work | `builtin:coder`, `builtin:sre` |
| **Equipment** | Which tools, which cluster, which project, which approval mode | **No** | the recipe's own `AGENTS.md` |

Equipment is deliberately *not* in the library. It belongs next to the
config that makes it true — a persona that names a tool the deployment
never registered is the exact failure #759 and #762 fixed on the tool
side, and the fix does not work if the prose reintroduces it.

So a recipe's `AGENTS.md` gets short and specific: one line for the
conduct, then the part only that deployment knows.

```markdown
# Cluster triage bot

@include builtin:sre

## Your environment — use these exact values

- Project: `acme-prod`
- Cluster: `us-east1/primary`
- You read through `gke_get_k8s_resource`; you have no write verb.
```

## Using it

`@include builtin:NAME` works anywhere `@include` works: a project
`AGENTS.md`, a file under `.agents/AGENTS.d/`, a subagent's own content
root, and the inline `instructions` of a declarative subagent.

- **Available names:** `builtin:core`, `builtin:coder`, `builtin:sre`.
  `instruction.BuiltinNames()` is the programmatic list.
- **Composition is safe.** `builtin:coder` and `builtin:sre` each open
  with `@include builtin:core`, and the loader's visited set means core
  is emitted exactly once no matter how many paths reach it. Including
  `builtin:core` yourself first is legal and just fixes where it lands.
- **A typo is fatal.** `@include builtin:site` fails the load and names
  the alternatives, for the same reason a missing `@include` file does:
  an agent silently missing its identity section is the failure mode
  hardest to notice from outside.
- **Provenance.** Each builtin appears in `Loaded.Sources` with
  `Scope: "builtin"` and `Path: "builtin:<name>"`, so `/memory` shows
  where the text came from even though there is no file.
- **Builtins are not interpolated** and may only include other
  builtins. They are shipped text with no directory of their own;
  resolving a relative path against whichever recipe included them
  would make one sentence mean different things in different
  deployments.

## Where each rule came from

Every rule in the library traces to something that actually happened in
this project. The library is not a style guide; it is a ledger of
failures that cost a UAT run.

| Rule | Source |
| --- | --- |
| No lifecycle, no sign-off, no "exiting session" | the demo-3 native-persona rewrite; #869 |
| Answer the question you were actually asked | #857; the "format, not a costume" section of the GKE platform persona |
| Say only what's true; name the gap | the verify-first work (#325) and the GKE drill's G6, which is decided by citations |
| Your tool list is authoritative | #759 / #762 — descriptions may name only registered tools |
| Do not investigate your own configuration | demo-3: an agent reading its own process environment to discover its permissions |
| Nobody can see your tool calls | #655 — the runtime now *measures* textless runs and reports one as a fault |
| Repetition is not progress; a documented dead end is a result | #144, #649, #702, #905; the runaway-loop guardrail train #623–#627 |
| Budgets are enforced; being stopped is worse than stopping | #1049 per-turn ceiling, #145 budgets |
| A test that cannot fail is worse than no test | #1061 — `BaselineHolds` rejects a case with no answer-sourced positive check |
| Never weaken a test to make it pass | the adversarial-review gate in `AGENTS.md` |
| A fresh signal is not necessarily fresh work | #1093 — the drill takes only the incident it caused |
| Never call it applied without reading it back | the gated-apply drill (#1105 / #1109) and scenario D's witnesses |
| A refusal is an answer; do not route around a denied write | #647 approval gate; #1068 → #1090, five fixes in a row |

## What is deliberately absent

**Rules that restate a constraint the runtime already enforces** (#865).
Telling a model in prose that it may not call a tool it was never
registered is not a safety property; it is prompt weight, and worse, it
teaches the model that the prose is where the rules live. Every rule in
the library is either something no mechanism can enforce (*say only
what's true*) or something a mechanism enforces bluntly and the model is
better off understanding than colliding with (loop detection, approval
gates, budgets).

**Prose that works around a missing mechanism** (#866). If a persona
needs a paragraph to compensate for something the runtime should do,
the paragraph is a bug report, not a rule.

## Adding a persona

1. Write `pkg/instruction/personas/<name>.md`. Prompt text only — no
   frontmatter, no `${env:...}`, no relative `@include`. Open with
   `@include builtin:core` unless it *is* core.
2. Register it in `builtinPersonas` (`pkg/instruction/personas.go`) with
   a one-line summary. The map is the registry;
   `TestEveryPersonaFileIsRegistered` fails on a file nobody added.
3. Add it to the names list above — `TestPersonaLibraryIsDocumented`
   fails otherwise.
4. Justify every rule in the ledger. A rule you cannot point at an
   incident for is a rule you are guessing about, and it costs tokens on
   every turn of every session forever.

## What is not retrofitted

`examples/gke-platform-agent` and its `gated-apply` variant are graded
by the GKE drill (42 scored checks) and by scenario D. Their personas
are already core-agent-native — they are where most of the library's
prose was derived from — so converting them to `@include` is a
behaviour-neutral refactor *in principle* and a re-run of a live,
cluster-dependent drill *in practice*. They stay as they are until that
drill can be re-run. See #656.
