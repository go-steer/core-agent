# Builtin subagent roster: `explore`, `plan`, `general`

**Status:** proposed, 2026-11-16. Nothing here is built. This is the design of
record for the "builtin subagent roster" capability. The sibling capability,
fan-out/join and steering, is
[`subagent-coordination-design.md`](./subagent-coordination-design.md).

## Motivation

`core-agent` has the substrate for subagents but ships no subagents. A recipe
can declare one (`subagents[]` or `.agents/subagents/<name>.md`) and reach it
through two doors: as a parent tool (`agent.WithSubagents`) and by reference
through `spawn_agent {agent: "<name>"}`. Every recipe that wants the everyday
shapes (a cheap read-only searcher, a planner, a general worker) writes them
from scratch. Peer harnesses ship these shapes as builtins; Claude Code's
`Explore`, `Plan` and `general-purpose` are the reference point.

Two consumers need this. One is concrete today and one is near.

**Self-development (#1116) is the concrete one.** The committed recipe at
`/.agents/` already hand-rolls one roster entry, `reviewer`. Getting that entry
right exposed a real defect. `--provider` overrides the parent only, so the
reviewer's pinned `model.provider` survived the flag, and the recipe could not
boot on its owner's Vertex-only machine. The fix was to declare no model block
at all
([`archive/self-development-design.md`](./archive/self-development-design.md)).
Every recipe that hand-rolls a roster entry can hit the same class of problem.

Exploration is also where the parent's money goes. In the 2026-09 spike
re-run, a frontier-model turn exploring this repository cost $2.1353 against
the recipe's then-$2.00 per-turn ceiling and was cut (#1140). The ceiling is
now $10. The cause has not changed: the parent explores at frontier rates
inside its own context window. Claude Code's answer is to hand that work to an
`Explore` subagent and keep only the conclusion.

**The daily-driver CLI is the near one.** North-star Goal 2
([`north-star.md`](./north-star.md)) is `core-agent` + `core-tui` +
`builtin:coder` as a first-party coding CLI. `builtin:coder` gives the parent
its conduct. It gives the parent no one to delegate to.

**Gemini needs it most.** #952 measured Gemini at a mean batch size of 1.00 on
open-ended search, unchanged by two rounds of prompt work. Its proposal, a
workflow-shaped navigator subagent, is an `explore` with a protocol.

## What exists today (facts this design builds on)

Verified against `0cda1ec7`.

- **One resolution, two doors.** `buildDeclaredSubagents`
  (`cmd/core-agent/subagents.go:388`) turns each `SubagentSpec` into both a
  sync `*agent.Agent` and an async `background.SubagentTemplate`. Anything that
  reaches it as a `SubagentSpec` gets both doors, the operator catalog
  (`/subagent`, `GET .../subagents`) and the model-facing roster.
- **Personas compose.** Subagent instructions go through `instruction.Expand`
  (`cmd/core-agent/subagents.go:874-879`), which resolves `@include builtin:NAME`
  without touching the filesystem (`pkg/instruction/load.go:619-629`). Only
  optional ADK placeholders (`{key?}`) are rejected
  (`pkg/config/subagents_markdown.go:253-273`). "Sent verbatim" means "no ADK
  templating", not "no directives".
- **`capability: read_only` is a boot-time check over the resolved tool
  surface** (`checkSubagentCapability`, `cmd/core-agent/subagents.go:679-709`).
  A tool passes if a `tools.policies[].mutating: false` override says so
  (`:711-724`), or if `tools.IsReadOnlyTool` does. The default read-only table
  (`pkg/tools/serialize.go`) excludes `bash` and `write_file`. MCP tools are
  listed live and pass only with `readOnlyHint`.
- **Unset `tools`, `mcp` and `skills` mean "inherit the parent's"**
  (`cmd/core-agent/subagents.go:946-977,1090-1096`). An explicit `tools` name
  the parent's registry lacks is a boot error (`:966-971`).
- **`tier`** is `small` | `mid` | `frontier`. `ResolveTierModel`
  (`pkg/config/config.go:1700-1740`) resolves it on the parent's provider. There
  is no cross-provider tier.
- **The roster reaches the model through `spawn_agent`** (`pkg/agent/background/roster.go`):
  - one `name — description` line per configured subagent in the tool
    description (`:134-148`);
  - the `agent` property's description (`:178`);
  - with ad-hoc spawns off (`--no-repl`, `cmd/core-agent/run_steps.go:1060`),
    an enum of names.

  There is no list tool, by design (`pkg/agent/background/tools.go:832-845`).
- **Delegation is gated as `spawn_agent` on both doors**
  (`pkg/agent/subagent.go:213`, `pkg/agent/background/tools.go:102`), and
  `spawn_agent` is not plan-exempt (`pkg/permissions/gate.go:323-385`).
- **Config is re-saved at runtime.** `config.Mutate` runs Load then Save
  (`pkg/config/mutate.go:50-63`) on `/model`, `/theme`, `/mouse` and
  `/permissions`, and `marshalForSave` rewrites `subagents` from the in-memory
  specs (`pkg/config/persist.go:107-117,155-162`).
- **No builtin subagents exist.** `WithPredefinedSpecs`
  (`pkg/agent/background/manager.go:756-766`) has no non-test caller. The
  closest builtins are the `agentic_*` wrapper tools (`pkg/tools/agentic`):
  read-only subtasks on the small model with 2–5 turn defaults. They are tools,
  not roster entries an operator can configure.

## Settled decisions (do not relitigate)

### D1: Opt-in by reference, never default-on

A recipe opts in per subagent:

```json
{ "subagents": [ { "extends": "builtin:explore" } ] }
```

or, in `.agents/subagents/explore.md`:

```markdown
---
extends: builtin:explore
---
```

No builtin subagent is ever present in a recipe that does not name it. There
are two reasons, and both are load-bearing:

- **The roster is prompt text.** Each configured subagent adds lines to
  `spawn_agent`'s schema and, on the sync door, a tool of its own. A default-on
  roster would change the tool surface of every existing recipe on upgrade.
  AGENTS.md treats tool descriptions as system-prompt weight that a recipe
  author can neither see nor override.
- **The trust boundary.** By-reference spawning exists so an unattended daemon
  spawns only operator-vetted specs
  ([`archive/unified-subagent-invocation-design.md`](./archive/unified-subagent-invocation-design.md)).
  A builtin the operator never named is not operator-vetted.

### D2: The authored entry is stored; the effective spec is derived

`SubagentSpec` gains an `Extends` field, and the spec holds exactly what the
operator wrote. The merged builtin + overlay spec is a **derived view**,
produced by `Config.EffectiveSubagents()` and consumed by
`buildDeclaredSubagents`, validation and the catalog. The rules:

- **Save writes what was authored.** `marshalForSave` marshals the stored entry,
  so a `/theme` never inlines the builtin's body into `config.json` and never
  drops `extends`.
- **Resolution is idempotent.** Re-applying `--agents-dir` (`ApplyAgentsDir`)
  cannot append a builtin's body twice.
- **Provenance comes from the overlay.** The derived spec carries the overlay's
  `fromMarkdown` and `baseDir`, so `expandInlineSubagentInstruction` and path
  resolution behave as they do for the authored entry.

The builtin specs are Markdown files in the `.agents/subagents/*.md` format,
embedded with `go:embed` under `pkg/config`. They are parsed by the frontmatter
parser behind `LoadSubagentMarkdownFile`
(`pkg/config/subagents_markdown.go:123`), refactored to accept bytes. An
operator can read, copy and fork a builtin with no translation.

New exported API, all in `pkg/config` (a `promised` package, so additive only
under apidiff, [`compatibility-policy.md`](./compatibility-policy.md)):

- the `SubagentSpec.Extends` field;
- `Config.EffectiveSubagents()`;
- `BuiltinSubagentNames()`.

`config import-workload` specialists use their own parser
(`pkg/config/workload.go:621`), and `extends` does not apply to them.

### D3: Overlay rules

The operator's entry is laid over the builtin field by field. A capability can
only narrow.

| Field | Rule |
|---|---|
| `name` | JSON: defaults to the builtin's name. Markdown: the file stem, as for every `.md` subagent (`subagents_markdown.go:135-139`). Set it explicitly to run two instances of one builtin. |
| `mode` | Inherited from the builtin. The Markdown loader's "mode is required" rule (`subagents_markdown.go:141-144`) is satisfied by `extends`. |
| `instructions` (or the `.md` body) | **Appended** after the builtin's body, separated by a blank line. The overlay goes through the recipe interpolator as today. The builtin's body does not; a test pins that. |
| `capability` | **Narrow only.** Empty keeps the builtin's value. `read_only` may stay `read_only`. Setting `change_executor` (or anything wider) on a `read_only` builtin is a config error. A recipe that wants `explore` with a shell writes its own spec. |
| `tools`, `mcp`, `mcp_tools`, `skills` | Replace the builtin's list. The capability check still runs on the result. Adding `bash` to `explore` fails at boot unless a `tools.policies` override declares `bash` non-mutating, which is the operator's explicit call. |
| `description`, `tier`, `model`, `max_depth`, `scheduler` | Replace when set. |
| `budgets`, `memory`, `inherit` | Replace the whole block when set. No per-field merge. |
| `output_schema` | Replaces when set. A builtin's own schema is always inline JSON, because an embedded file has no directory to resolve a path against (`subagents_markdown.go:396-398`). |
| `root`, `remote` | **Rejected** with `extends` in v1. A root brings its own `AGENTS.md` persona, so the subagent would have two persona sources and an unclear precedence. A remote subagent has no in-process persona to extend. |

`extends` accepts only `builtin:NAME` in v1. An unknown name is a config error
that lists the available builtins, matching `instruction.Builtin`.

### D4: The v1 roster is three entries

| | `explore` | `plan` | `general` |
|---|---|---|---|
| Job | Locate code and report where things are, with `file:line` evidence. It does not judge, review or recommend. | Read the code and the request and return a step-by-step implementation plan: files to touch, order, risks, open questions. | A bounded delegation of a self-contained task, with the parent's tools. |
| `mode` | `task` | `task` | `task` |
| `capability` | `read_only` | `read_only` | unset |
| Model | `tier: small` | inherits the parent's model | inherits the parent's model |
| Tools | `read_file`, `read_many_files`, `view_file_outline`, `grep`, `glob`, `list_dir`, `stat` | the `explore` set | inherited (the parent's builtins minus `spawn_agent` / `stop_agent`, per #748) |
| `mcp`, `skills` | `[]` (none) | `[]` (none) | inherited |
| Persona | `@include builtin:core` + locator conduct | `@include builtin:coder` (which includes `core`) + planning conduct | `@include builtin:coder` |

Why each choice:

- **`explore` is a locator because the spike says tier is the variable.** In the
  2026-09 four-arm spike, `claude-opus-4-5` scored 1–2/6 and `claude-opus-5`
  scored 5/6 on the same harness. A small-tier subagent asked to *judge* will
  judge badly. One asked to *find* is cheap and checkable, because its answers
  are paths and line numbers the parent can open. Claude Code draws the same
  line for `Explore`.
- **`plan` and `general` inherit the parent's model** for the same reason:
  planning and open-ended work are judgment, and a recipe that pinned a
  frontier parent did so on purpose.
- **The read-only builtins take no MCP servers or skills.** Unset `mcp` would
  inherit every parent server. The capability check would then fail on any MCP
  tool without `readOnlyHint`, so `explore` would fail to boot in most recipes.
  An operator who wants `explore` to search an MCP server names it in the
  overlay, and the check runs on that.
- **A builtin's tool list tolerates the recipe's registry.** A builtin cannot
  know whether a recipe disabled `view_file_outline` (`tools.disable`,
  `--no-builtin-tools`, a profile). For a builtin's *own* list only, a name
  missing from the registry is dropped with a boot warning, and an empty
  result is an error. An operator's overlay list keeps today's hard
  "unknown tool" error.
- **All three are `task` mode,** so they stop when done (`finish_task`) instead
  of idling, and their result returns to the parent as one value.
- **`general` gets no worktree by fiat.** Parallel writers are governed by the
  existing `safety.parallel_subagent_writes` policy (`refuse` by default;
  `worktree` provisions a detached worktree per spawn, #653). The roster does
  not override recipe safety policy.
- **`general` is described narrowly.** `spawn_agent`'s static description says
  "when a configured subagent covers the task, delegate to it instead of doing
  the work yourself" (`pkg/agent/background/tools.go:716`). A subagent with the
  parent's whole toolset covers everything. `general`'s description therefore
  names when delegating pays (a self-contained task whose working context the
  parent does not need back). P1 re-reads that static sentence with `general`
  configured, and changes it if it pushes the parent to delegate everything.
- **The persona text lives in the embedded spec body, not the persona
  library.** `builtin:explore` as a subagent and `@include builtin:explore` as a
  persona would be one string in two namespaces. Role conduct that no top-level
  agent uses belongs with the role.

The descriptions are model-facing prose and fall under AGENTS.md's three rules.
They name what the subagent returns, not its genre. They state no frequency.
They are one neutral string with no interactive and headless variants. The
embedded specs get a `ModelFacingBans` sweep.

### D5: Delegating to a `read_only` subagent is plan-exempt

Plan-first (`plan_mode: required`) blocks `spawn_agent` before `record_plan`
(`pkg/permissions/gate.go:323-385`). Read-only tools are exempt because they
cannot change anything. A delegation to a subagent whose `capability:
read_only` passed the boot check cannot change anything either, so it gets the
same exemption on both doors. Without it, `explore` and `plan` are unusable in
exactly the phase they exist for, and the self-dev recipe sets
`plan_mode: required`.

This refines, rather than reverses, plan-first's classification table
([`archive/plan-first-design.md`](./archive/plan-first-design.md)). That table
gates `spawn_agent` because "a subagent can do arbitrary work", and a
boot-verified `read_only` subagent cannot. The gate became a precondition of
the fan-out in #758, and Q3 (children inherit the parent's `planRecorded`) is
untouched. The exemption keys on the boot-verified capability, never on the
subagent's name or description. A delegation to `general`, or to any subagent without
`read_only`, stays gated. The permission *mode* is unchanged: in `ask` mode a
delegation still prompts unless `permissions.allow` lists it (e.g.
`spawn_agent:explore`).

### D6: The roster is measured on the self-dev rig before the recipe adopts it

The roster can ship without proof. The self-dev recipe (`/.agents/config.json`)
adopts `explore` only after a measured comparison on the T1 replay rig
(`dev/uat/self-dev/run.sh --tier t1 --replay`, issue #1002). That rig is pinned,
needs no GitHub, and is graded by the A1–A15 scorecard. The comparison:

- **Arms:**
  - control: the current recipe;
  - treatment: the recipe plus `extends: builtin:explore` and
    `permissions.allow: ["spawn_agent:explore"]`.

  Same model, same provider, same base. At least three runs per arm, because
  one run is an anecdote.
- **Gate:** no scorecard regression. Any A-row that passes in the control and
  fails in the explore arm blocks adoption.
- **Reported, not gated:** session cost, parent input tokens at the final turn,
  turn count and wall-clock.

`tier: small` resolves through the provider family (`pkg/taskclass`), so the
arms state which provider they ran on. Provider `vertex` maps to Gemini,
`anthropic-vertex` to Claude.

If the explore arm costs more or scores worse, that result is written up and
the recipe does not adopt it. The builtin stays available to other recipes.

## Out of scope

- **A default-on roster** in any mode (D1).
- **A builtin `review` subagent.** The repo's `reviewer` needs `bash` to confirm
  findings by running them, so it cannot be `read_only`, and its conduct is
  specific to this repository. Revisit after D6 if a generic adversarial-review
  skeleton earns its place.
- **`extends` pointing at anything but `builtin:`**, such as spec-to-spec
  inheritance or a remote catalog.
- **`extends` in `import-workload` specialists** (D2).
- **Cross-provider tiers.** `tier: small` stays on the parent's provider.
- **Unifying the two small-model resolvers.** `tier: small` uses `taskclass` or
  a provider profile, and `spawn_agent {model: "small"}` uses
  `models.ResolveSmallModel`. They can disagree today. That is a separate fix.
- **Declaring sync vs async invocation on the spec (#867).** Orthogonal. Note
  that #867's proposed `"mode": "sync"` collides with the existing `mode`
  field, so it needs a different key.
- **A CLI flag that enables builtins without a config file** (see Open
  questions).
- **Fan-out, join, steering and peer messaging.** These belong to
  [`subagent-coordination-design.md`](./subagent-coordination-design.md).
- **Changing the #748 carve-out.** `general` does not get `spawn_agent`.

## Implementation phases

- **P1: the library and `extends`.**
  - `pkg/config/builtin_subagents/{explore,plan,general}.md`, embedded.
  - The stored/derived split and Save round-trip (D2).
  - The overlay rules (D3), for both JSON and Markdown authoring.
  - The registry-tolerant builtin tool list (D4).
  - The read-only plan exemption (D5).
  - Tests:
    - every embedded file is registered and documented;
    - every builtin boots and passes its capability check against the default
      registry (in `cmd/core-agent`, where `checkSubagentCapability` lives) and
      against a registry with `view_file_outline` disabled;
    - one overlay table test per D3 row, including capability narrowing;
    - a Save round-trip leaves `extends` and the overlay byte-for-byte;
    - `ApplyAgentsDir` twice yields one body;
    - the builtin body is not interpolated;
    - plan-first lets a read-only delegation through before `record_plan` and
      blocks a `general` one;
    - a `ModelFacingBans` sweep over every builtin description.
  - The site: the configuration reference and the subagents page.
  - A CHANGELOG `#### Feature` bullet.
- **P2: the measured adoption (D6).** Run the arms, record the result as a dated
  record under `docs/`, and adopt or decline in `/.agents/config.json` with the
  evidence.
- **P3: Gemini-shaped `explore` (#952).** Fold the outline → grep → narrow-read
  protocol into `explore`'s conduct, measured on the #952 parallel probe
  (`dev/parallel-probe`). The probe decides whether the protocol helps every
  provider or only Gemini. Subagent instructions have no templating, so a
  Gemini-only protocol would need a separate builtin (`explore-protocol`, say)
  rather than a branch inside one body.

## Open questions

1. **A no-config opt-in for the CLI.** A daily-driver user with no `.agents/`
   gets no roster. Options are a `--subagents builtin:explore,…` flag, or a
   section in the default config that `core-agent config init` writes. Either
   keeps D1, since the user named the builtin. Decide after P2 shows the roster
   pays.
2. **`plan`'s output shape.** Free text, or an inline `output_schema` (steps,
   files, risks)? A schema makes the plan checkable by plan-first tooling, but
   it also constrains the plan. P1 ships free text. Revisit with a consumer.
3. **#865 interaction.** If #865 lands a generated preamble that tells the
   model its enforced constraints, including the roster, the roster lines in
   `spawn_agent`'s schema may become redundant. That is not a blocker.
