# Plan-first enforcement: gate-level "plan before action"

Design doc for a substrate primitive that **enforces** the
plan-first workflow — write a plan, present it, get approval — rather
than relying on AGENTS.md prompting alone.

**Status:** SHIPPED (header updated 2026-08-13 — the doc below is the
original proposal). Gate enforcement landed in v2.3 behind
`require_plan_artifact`; the advisory/soft variant
([#215](https://github.com/go-steer/core-agent/issues/215)) landed in
v2.9 and replaced that bool with the three-valued
`permissions.plan_mode`. See
[Implementation notes](#implementation-notes-2026-08-13-215) at the
bottom for what the shipped shape actually is.

## Motivation

Plan-first is a well-known workflow for high-trust agentic work:
research the goal, write a structured plan, get explicit operator
approval, *then* execute. Claude Code, Aider, and Cursor all
support it; Antigravity has the same shape.

Today an operator can approximate plan-first with core-agent in
two ways, both unsatisfying:

1. **AGENTS.md prompting only.** Tell the model in instructions
   to write a plan and wait. Model-honored, not enforced. A model
   that mis-reads or skips the instruction can call `write_file`
   immediately. Frontier models (Gemini 3.1 Pro, Claude Opus 4.7)
   mostly comply; smaller / faster models drift more often.
2. **`permissions.mode: plan`.** Hard-blocks every tool including
   reads. The agent can't even research before planning.

Neither gives the actual property an operator wants: **"I am
guaranteed to see a plan before any file gets written or any
command gets run."**

An Antigravity comparison thread (2026-06-02) surfaced this as
the cleanest item we don't already do — batched review UX,
multi-scope grants, ModeAcceptEdits, and ModePlan all exist;
true plan-gating doesn't.

## Goals

- **Enforce.** Writes and shell exec are gate-denied until a plan
  artifact exists. The model cannot bypass via instruction
  drift.
- **Allow research.** Read tools (`read_file`, `read_many_files`,
  `grep`, `glob`, `list_dir`, `stat`, `fetch_url`, `json_query`,
  `todo`) work normally during the plan phase. Plan-first
  without research is just guessing.
- **Surface the plan.** The plan content is visible to the
  operator in the TUI / attached client without an extra step.
  Operators reject by typing "no, revise" — agent rewrites the
  plan, no special tooling needed.
- **Persist the plan.** Plan is a real on-disk artifact so it
  survives session restart, can be diffed across revisions, and
  is auditable after the fact.
- **Opt-in.** Existing modes (`ask`, `allow`, `yolo`, `plan`,
  `acceptEdits`) unchanged. Plan-first is a separate config knob
  that composes with `ask`.

## Non-goals (v1)

- **Multi-plan workflows** (sub-plans per subagent, dependent plans).
  One plan per session for v1.
- **Structured plan schema.** Plans are free-form markdown in v1.
  No required sections, no validation. (Operators can enforce
  shape via AGENTS.md prompting on top.)
- **Replanning checkpoints mid-execution.** Once approved, the
  plan is the contract for the rest of the session. If the
  operator wants to replan, they revoke approval (mechanism TBD;
  see Open Questions) and the model is re-gated.
- **Per-subagent plans.** Spawned subagents inherit the parent's
  approval status — if the parent has an approved plan, subagents
  execute without re-gating. This matches today's `Gate`
  inheritance model.
- **Multi-step approval (e.g. "approve sections 1-3, hold 4").**
  All-or-nothing for v1.

## Proposed design

Two pieces: a new built-in tool, and a gate-level pre-check that
consults a per-session flag.

### Piece 1: `record_plan` built-in tool

```go
// Tool: record_plan
//
// Args:
//   plan: string (markdown — the plan to display + persist)
//
// Behavior:
//   1. Write plan markdown to:
//        <agentsDir>/plans/<session-id>-<seq>.md
//      where <seq> is a monotonically increasing counter (so
//      revisions don't overwrite earlier drafts).
//   2. Set the per-session `planRecorded` flag on the gate.
//   3. Return the path of the written artifact + sequence number
//      so the model knows the plan was accepted.
//
// Gate behavior:
//   - record_plan is ALWAYS allowed regardless of mode or
//     planRecorded state. It's the one tool the agent can use
//     to escape plan-required gating.
//   - The write goes through normal path_scope (the plans dir is
//     under .agents/, which is in-scope by default).
```

The plan ends up in two places: (1) the chat scrollback, because
the tool's args render in the TUI, and (2) on disk under
`.agents/plans/`. Operators see it without any extra UI.

### Piece 2: gate-level `RequirePlanArtifact` pre-check

```go
type Options struct {
    // ... existing fields ...

    // RequirePlanArtifact, when true, denies write_file /
    // edit_file / delete_file / bash tool calls until the model
    // has called `record_plan` at least once this session.
    // Read tools (read_file, grep, glob, list_dir, stat,
    // fetch_url, json_query, todo) are NOT gated by this flag —
    // research stays unblocked.
    //
    // Plan-gated denials carry a clear message instructing the
    // model to call record_plan first.
    RequirePlanArtifact bool
}
```

The gate keeps a per-session boolean `planRecorded` (default
`false`). `record_plan` flips it to `true`. The pre-check runs
before the existing mode-based logic — even in `yolo` mode, if
`RequirePlanArtifact` is set and no plan has been recorded,
write/exec tools deny.

### Tool classification

| Tool | Plan-gated? | Why |
|---|---|---|
| `read_file`, `read_many_files`, `stat`, `list_dir`, `glob`, `grep` | no | research |
| `json_query`, `fetch_url`, `todo` | no | research / state |
| `write_file`, `edit_file`, `delete_file` | **yes** | mutation |
| `bash` | **yes** | mutation / exec |
| `record_plan` | no (always allowed) | the escape valve |
| `wait_and_verify` (v2.9) | no | read-only by construction — it refuses to poll a tool the runtime can't classify read-only, and each poll re-enters the polled tool's own gate check, so an MCP poll is plan-gated exactly as a direct MCP call is |
| `spawn_agent`, `spawn_remote_agent` | **yes** | a subagent can do arbitrary work |
| `stop_agent` | no | cancels; a denial leaves running what the model was trying to stop (#758) |
| MCP tools | TBD — see Open Questions | depends on operator's MCP server posture |

### Config surface

```json
{
  "version": 1,
  "permissions": {
    "mode": "ask",
    "require_plan_artifact": true
  }
}
```

Composes with every existing mode:
- `ask` + `require_plan_artifact`: agent researches freely,
  records plan (visible in chat), operator reviews plan in chat
  or `.agents/plans/`, model is unblocked once plan is recorded
  and then mutation calls prompt-per-call as normal.
- `acceptEdits` + `require_plan_artifact`: same, then writes
  auto-allow after plan is recorded.
- `yolo` + `require_plan_artifact`: same, then everything
  auto-allows after plan is recorded. ("trust me with the plan, then
  trust me with execution")

### Revocation

For v1, the operator revokes plan approval via a new slash
command `/replan`. This:
1. Clears the `planRecorded` flag on the gate.
2. Renames `<sid>-<seq>.md` to `<sid>-<seq>-revoked.md` (audit
   trail).
3. Drops a system note into the next turn: "Operator requested a
   replan. Your previous plan was rejected. Research further if
   needed and call `record_plan` again."

This avoids needing TUI primitives for "edit the plan in
$EDITOR before approval" (an in-modal editor shell-out, which
some plan-first agents support); operators who want that today
can edit the
plan file in another window, then `/replan` to force a redraft
in conversation context.

## Operator experience

### Initial prompt → plan → approval → execution

```
operator> implement the X feature in pkg/foo per the spec in docs/foo-spec.md

agent> [reads spec, greps for existing patterns, lists relevant files —
        all via pre-allowed read tools]

agent> [calls record_plan with markdown plan]
       Plan recorded at .agents/plans/abc123-1.md
       
       ## Goal
       Implement X in pkg/foo per docs/foo-spec.md.
       
       ## Files to change
       - pkg/foo/x.go (new): X implementation
       - pkg/foo/x_test.go (new): unit tests
       - pkg/foo/foo.go: wire X into the existing dispatcher
       
       ## Approach
       - ...
       
       Awaiting approval.

operator> go

agent> [calls write_file pkg/foo/x.go → ask-mode prompts as normal,
        but the gate no longer plan-denies]
```

### Plan rejection

```
agent> [calls record_plan ...]
       Plan recorded.
       [plan content]

operator> /replan — split this into two PRs, do the test scaffolding first

agent> [next turn includes the system note "Operator requested a
        replan..."]
        [reads more, refines]
        [calls record_plan again with revised plan]
```

### Bypass attempt

```
agent> [calls write_file ...]
       Error: write_file denied: plan-first mode requires
       record_plan to be called before any file mutation.
       Call record_plan(plan: <your-markdown-plan>) first.

agent> [recovers, calls record_plan, then write_file]
```

## Alternatives considered

### B. Loosen `ModePlan` + slash flip

- Loosen `ModePlan` to "block write/exec only, allow reads"
- Recipe sets `mode: plan` for research
- Operator runs `/plan-approve` slash to flip to `mode: ask`
- Plan lives in chat scrollback only — no artifact

Rejected:
- Plan is fragile (chat-only — lost on session restart, can't
  be diffed across revisions, no audit trail)
- Semantic change to `ModePlan` risks existing users who chose
  it precisely for "block everything" (the chip is documented
  as "read-and-think — shouldn't touch the world", which today
  literally means no reads either — operators relying on
  paranoid no-IO might be surprised)
- No clean revocation mechanism short of "switch back to plan
  mode" which then re-blocks reads

### C. Skill-based (no substrate change)

- Ship a `/plan` skill that the model invokes
- Skill writes the plan, model agrees not to do anything else

Rejected — same problem as AGENTS.md prompting: no enforcement.
Skill activation is also LLM-judged, not gate-checked.

### D. Wrap the gate in a `PlanGate` decorator

- New `permissions.PlanGate` wraps existing `Gate`, intercepts
  write/exec calls, denies until a plan exists
- No flag on the base `Gate` struct

Rejected — splits gate state across two objects (existing
session-allow maps on `Gate`, plan flag on `PlanGate`),
complicates the subagent gate-inheritance path (which copies
`*Gate`), and forces callers to pick which type they want to
pass around. A single bool on `Options` is cleaner.

## Open questions

1. **MCP tools — gate or not?** MCP tool calls go through the
   same `Gate.PromptForTool` path, so by default they'd be
   plan-gated by tool name (any MCP tool would deny pre-plan).
   That may be wrong for read-only MCP servers (e.g.
   `gke.list_clusters`, `linear.get_issue`). Options:
   - Default: gate everything (safer, matches "no actions before
     plan")
   - Add MCP tool-name allowlist in `RequirePlanArtifact` config
     so operators can exempt read-only MCP tools per-server
   - Per-tool annotation in MCP schema (out of scope for v1)

   Lean: gate everything by default; if it bites, add an
   exemption list in v2.

2. **What counts as "calling record_plan"?** A plan with empty
   body? A two-character "ok" plan? Options:
   - Accept any non-empty string (simplest; operator controls
     quality via AGENTS.md prompting + `/replan`)
   - Require minimum length / structure (footgun risk; plan
     quality is a judgment call, not a length check)

   Lean: accept any non-empty. AGENTS.md guides quality;
   `/replan` corrects bad plans.

3. **Spawn family — plan-gated, or inherit parent approval?**
   Today subagents inherit the parent's `*Gate`. With
   plan-required:
   - Option A: Spawned subagents inherit `planRecorded=true`
     from the parent. Subagents execute freely if the parent
     was approved. Simpler; matches today's gate-inheritance
     story.
   - Option B: Spawned subagents start with `planRecorded=false`
     and must record their own plan. Safer but breaks the
     "parent already planned the fan-out" workflow (e.g.
     `gke-parallel-triage` would require 4 subagent plans).

   Lean: Option A. The parent's plan covers the fan-out.

4. **Where to store the plan artifact?** Two options:
   - `.agents/plans/<sid>-<seq>.md` (.agents-relative, alongside
     `sessions/`)
   - `<session-db>/plans/<sid>-<seq>.md` (lives with the
     session DB when `--session-db` is set, falls back to
     `.agents/plans/` otherwise)

   Lean: `.agents/plans/` always. Plans are project-scoped
   artifacts that ideally get checked in (or `.gitignore`d
   uniformly), independent of where the session DB lives.

5. **Plan visibility in the in-process TUI.** `record_plan`'s
   args render in chat as a tool-call card. For a long plan
   (~3KB markdown) that may be visually noisy. Options:
   - Render in a collapsed card with "expand to view"
   - Render inline (today's tool-call rendering)
   - Render as a distinct "Plan" panel above the next agent
     turn

   Lean: today's inline rendering for v1. UX polish later.

## Migration

Plan-first is opt-in via `require_plan_artifact: true`. No
behavior change for existing configs.

For new operators, a `examples/plan-first/` recipe ships
alongside the implementation:

```
examples/plan-first/
├── README.md
├── .agents/
│   ├── config.json    # mode: ask, require_plan_artifact: true,
│   │                  #   read tools pre-allowed
│   └── AGENTS.md      # primes the model on the workflow
```

The recipe composes with the v2 instruction loader: drop
`examples/plan-first/.agents/AGENTS.md` as
`<your-project>/.agents/AGENTS.d/00-plan-first.md` and your
existing project's AGENTS.md keeps its other guidance.

## Implementation sketch

| Component | File(s) | LoC est. |
|---|---|---|
| `record_plan` tool registration | `pkg/tools/builtins.go` | +30 |
| `record_plan` tool handler | `pkg/tools/record_plan.go` (new) | +120 |
| Gate flag + pre-check | `pkg/permissions/gate.go` | +60 |
| Config field + validation | `pkg/config/config.go` | +20 |
| `/replan` slash handler | `pkg/attach/handlers_slash.go` + in-process equivalent | +80 |
| Tests | `pkg/permissions/plan_test.go`, `pkg/tools/record_plan_test.go` | +200 |
| `examples/plan-first/` recipe | new directory | +150 |
| Docs (Hugo + CHANGELOG) | `docs/site/content/docs/reference/configuration.md`, `CHANGELOG.md` | +80 |
| **Total** | | **~740** |

Single PR if we keep scope tight (the substrate change is small
and self-contained); two PRs (substrate + recipe) if reviewers
want them separated.

## Resolved during review (2026-06-02)

- **Q1 — MCP tools:** gate everything by default; add a server-
  level allowlist later if it bites. Operators opted into plan-
  first because they want no actions before plan, and an MCP
  tool the operator configured is "an action."
- **Q2 — plan validation:** any non-empty string after trim. Plan
  quality is a judgment call; the operator catches bad plans in
  chat and `/replan`s. Structure enforcement is a footgun.
- **Q3 — subagent inheritance:** spawned subagents inherit the
  parent's `planRecorded` flag. The parent's plan covers the
  fan-out (matches `gke-parallel-triage` orchestration shape).
  Per-subagent re-planning was rejected as adding indirection
  without an operator benefit.

  *Implemented in #758, three releases after it was written.* Until
  then the flag was inherited but the spawn itself was ungated, which
  made the enforcement the exact inverse of the intent: a child whose
  tools were MCP-namespaced had to record its own plan before its
  first call (`mcp` is not plan-exempt), while creating the child was
  free. The 2026-08-19 GKE traces show both halves in one stack —
  `record_plan` (parent, voluntary) → `spawn_agent` (ungated) →
  `record_plan` (child, forced). `spawn_agent` and
  `spawn_remote_agent` now call `gate.CheckGeneric`, so the parent's
  plan is a precondition of the fan-out rather than a hope about it.
  `stop_agent` is excluded — see the classification table.
- **Q4 — storage path:** `.agents/plans/<sid>-<seq>.md` always.
  Project-scoped artifacts go in `.agents/`, regardless of
  whether `--session-db` is set.
- **Q5 — TUI rendering:** today's inline tool-call card for v1.
  If long plans become annoying we add a collapsed card with
  `c`-to-expand in v2.

## Composition note (worth surfacing in the recipe)

The existing modes compose with `require_plan_artifact` to give
three useful flavors out of the box, picked by the base mode:

| Composition | Behavior after plan recorded |
|---|---|
| `ask + require_plan_artifact` | writes prompt per call ("approve each step") |
| `acceptEdits + require_plan_artifact` | writes auto-allow, bash still prompts |
| `yolo + require_plan_artifact` | everything auto-allows ("just tell me the plan") |

The third row is the "we just want to know the plan" case — no new
mode needed. `yolo`'s "no prompts" semantics still hold *after*
the plan; the only deny is the one-time gate before the plan
exists.

The recipe should ship three `config.json` variants (one per row)
so operators pick by uncommenting.

## Out of scope (deferred to v2 / follow-up tasks)

- **Plan-progress tracking.** Claude Code's TodoWrite/TodoStatus
  pattern: the plan is a checklist, the agent marks items as it
  executes. Today's `todo` tool overlaps but is mis-scoped
  (process-wide, ephemeral, single list); a plan-coupled
  progress tracker wants plan-instance-scoped, persistent,
  one-per-plan. Filed as a separate v2 design task — likely
  combines a sibling tool (`plan_progress`) with a scope
  adjustment to the `todo` store so the same primitive does
  double duty (ad-hoc + plan execution). The TUI rendering
  surface, compaction interaction, and operator-edit semantics
  all need design work, not just code.
- $EDITOR shell-out from the approval modal (let the operator
  tweak the proposed edit in their own editor before approving).
  The `/replan` workflow covers the
  same need adequately for v1; operators who want in-modal
  editing should weigh in.
- Per-section plan approval ("approve files 1-3, reject 4").
- Plan templates / schemas. Free-form markdown for now.
- Multi-tier plans (orchestrator plan + subagent plans).
- Plan auto-summarization for compaction. The plan stays
  out-of-context once approved, but it's worth surfacing on
  `/compact` so the post-compaction context reminds the model
  what was approved.

## Implementation notes (2026-08-13, #215)

The advisory variant this doc left unshipped landed in v2.9. It
replaced the boolean with a three-valued mode, because the bool
conflated two things the advisory case needs apart:

| `permissions.plan_mode` | `record_plan` registered | gate armed |
|---|---|---|
| `off` (default) | no | no |
| `advisory` | yes | no |
| `required` | yes | yes |

- **One reader, not two synced fields.** `PermissionsConfig` grew
  `PlanMode string` alongside the now-deprecated
  `RequirePlanArtifact bool`, but nothing reads either field
  directly. `ResolvedPlanMode()` folds them (mode wins; the bool
  means `required`), and the two predicates every consumer actually
  calls are `PlanToolRegistered()` (registration) and
  `PlanGateArmed()` (enforcement). Keeping two fields *in sync* is
  the exact drift this milestone is about, so there is deliberately
  no sync — `cmd/core-agent` writes `PlanMode` and zeroes the bool
  once resolution is done.
- **`Validate` rejects the one genuinely ambiguous pair.**
  `plan_mode: "off"` next to `require_plan_artifact: true` is a
  half-finished migration where either reading leaves an operator
  wrong about whether the gate is armed, so it is an error rather
  than a silent winner. `advisory`/`required` alongside the old bool
  is legal — mode just outranks it.
- **The tool description is mode-aware.** Under `required`,
  `record_plan`'s description tells the model its mutating calls
  will be denied until the plan is on file. Under `advisory` it says
  the opposite, explicitly, so the model records the plan and
  carries it out in the same turn. Telling a model a gate exists
  when none does makes it stall for an approval nobody is coming to
  give — the inverse of, and the same bug class as, claiming a
  safety property the runtime doesn't enforce. `/replan`'s response
  text is switched on the same state for the same reason.
- **CLI.** `--plan-mode=off|advisory|required` supersedes
  `--plan-first`, which is retained as a deprecated alias meaning
  `required` (and `--plan-first=false` meaning `off`). Precedence,
  highest first: `--plan-mode`, `--plan-first`,
  `permissions.plan_mode`, `permissions.require_plan_artifact`, the
  task profile's plan-first default. The resolved mode and the
  source that won are printed at startup.
- **Advisory does not weaken `required`.** The gate reads
  `PlanGateArmed()`, so the only mode that can ever set
  `permissions.Gate.RequirePlanArtifact` is `required`; the artifact
  is written identically in both modes.

## Implementation notes (2026-08-14, #747)

The 2026-08-14 GKE UAT ran a recipe with `plan_mode: required`,
every mutating built-in disabled, and one read-only MCP server. A
parent and its declarative subagent each recorded a plan in one
incident. Three things the plan surface said were not what
happened, and all three were the same shape — reporting a design
instead of the run.

- **The result names the gate it actually armed.** `record_plan`
  answered "Mutating tools are now unblocked for this session"
  unconditionally: a category the recipe had emptied, while saying
  nothing about the `gke` MCP surface the plan really unblocked
  (`mcp` is deliberately not plan-exempt). It was also mode-blind,
  telling advisory sessions about an unblock that cannot happen —
  the result-path half of the bug the #215 description split fixed
  one surface earlier.

  The fix mirrors `SetNativeSearchTools` / `ActiveSearchBinaries`
  (#158): `tools.Build`, `GateToolset` and `peer.New` each call
  `gate.RegisterPlanGatedTools(...)` with the names they registered,
  the gate drops the plan-exempt ones, and the message renders from
  mode plus that set. `PlanGatedTools()` returns a `known` bool, so
  a host that wires tools by hand gets prose that declines to
  enumerate rather than a confident empty list — "unknown" must not
  collapse into "none".

  Note the set reflects what the gate is actually asked about. When
  this was written `spawn_agent` was described as plan-gated in this
  doc and in `--plan-mode`'s help but routed through no gate, so it
  did not appear — the message reporting the runtime rather than the
  intention is what made the gap legible, and it was tracked and
  closed as #758. The spawn tools register themselves with the gate
  when they are built, so they now appear in the message on any build
  that wires a background manager.

- **Every artifact says who wrote it.** Plans now open with a YAML
  frontmatter block carrying `plan`, `agent` and `session`. Without
  it a plans directory is anonymous markdown — the UAT's `plan-1.md`
  and `plan-2.md` had nothing on disk distinguishing parent from
  subagent, and multi-session makes it worse (the gate flag is
  per-session, `<agentsDir>/plans/` is process-global, so concurrent
  tenants interleave into one sequence). Filenames are unchanged:
  the sequence is load-bearing for `nextPlanSeq` / `LatestActivePlan`.
  No timestamp — the file's mtime carries it, and a clock would make
  every artifact test non-deterministic.

- **`/replan` archives the operator's plan, not the newest one.**
  `RevokeLatestPlan` took max-sequence, which with a subagent in
  play is the subagent's: an operator rejecting the parent's
  delegation was filing the specialist's investigation notes and
  leaving the plan they meant to reject active. `RevokePlanBy(gate,
  agentsDir, PlanOwner{Agent, Session})` scopes the revocation;
  `RevokeLatestPlan` is kept as the zero-owner spelling with its
  historical semantics intact.

  When the owner has no active plan the gate flag still clears —
  /replan's contract is "the next mutating call needs a fresh plan",
  and that has to hold whether or not there was an artifact to file.
  A plans directory with no attribution at all falls back to
  newest-wins so an upgrade mid-incident doesn't read as "you have
  no plan"; a directory with a *mix* gets the strict answer, because
  once some artifact can say who wrote it, silently revoking one
  that can't is the guess this change exists to stop making.

  Scoping is on agent *and* session, which costs one case: a daemon
  restart mints a new session, so a `/replan` afterwards will not
  archive the plan the previous session recorded. Session is kept
  anyway, because background subagents run under their own session
  IDs in the same process and the plans directory is shared — it is
  the field that separates them from the parent, not just tenants
  from each other. The declining message reports the artifact's own
  frontmatter (`describePlanOwner`) rather than asserting "another
  agent", so the restart case reads as what it is and the operator
  can see whose plan is sitting there.

## Implementation notes (2026-08-20, #693)

Open question 1 ("MCP tools — gate or not?") is resolved, and the
answer is the third option the question dismissed as out of scope for
v1, arrived at from the config side rather than the protocol side.

The lean held for two releases: gate everything. It bit in exactly the
way anticipated. Under `plan_mode: required`, a recipe whose only
surface is a read-only MCP server can't call `list_clusters` to learn
what it would plan *about* — the research a plan is made of is denied
until the plan exists. The gate isn't being conservative here so much
as blind: `gatedTool.Run` checks with the namespace (`"mcp"`), never
the underlying tool name, so it cannot tell `get_pod` from
`delete_pod` even in principle. A per-tool allowlist in the gate's
config, the second option, would have papered over that by asking the
operator to re-state per tool what they already know per endpoint.

What shipped instead: `read_only: true` on the `ServerSpec` in
`mcp.json`. Every tool from that server carries the read-only dispatch
class (`tools.ReadOnlyHinter`), `pkg/tools`' gate wrapper classifies
each call with `IsReadOnlyTool` and routes read-only ones to the new
`Gate.CheckReadOnlyToolCall`, and `planFirstDenial` exempts them. The
classification is one thing declared once and consumed in three places
— plan-first, the mutation serializer, and `wait_and_verify`'s
poll refusal — rather than a plan-first-shaped exemption list.

Two properties worth keeping if this is ever revisited:

- **The exemption is per *call*, not per name.** No entry is added to
  `planExemptTools`; a name table cannot express this, because the
  name is `"mcp"` for every tool from every server. That is also why
  `CheckReadOnlyToolCall` is a sibling of `CheckToolCall` rather than
  a new field on the gate: the fact travels with the call.

- **It relaxes plan-first and nothing else.** Policy allow/deny,
  permission mode and prompting are untouched — a read-only MCP tool
  in ask mode still asks. `read_only` had to be allowed to move
  plan-first (a safety property) because it is an *operator*
  assertion about an endpoint they chose, carrying the same authority
  as the config that turned plan-first on; it is not a claim the
  server made about itself, and nothing verifies it. Letting it also
  move allow/deny would have made it an allowlist bypass, which is a
  different and much larger grant.

The table row above still describes `wait_and_verify` correctly: an
MCP poll re-enters the polled tool's own gate check, so it is
plan-gated exactly as a direct call is — which now means exempt on a
`read_only` server and gated everywhere else.

## Implementation notes (2026-09-03, #906)

The sketch above says `record_plan` writes to a path whose sequence
is "a monotonically increasing counter (so revisions don't overwrite
earlier drafts)". Read as *per call* rather than *per plan*, which is
how it was implemented, that is a tool a runaway can use to write
unbounded files into the plans directory — and one did. On a live GKE
run (`2.9.0-dev.4`) an agent in completion-reporting mode called
`record_plan` eight times in one turn, seven of them consecutively,
minting `plan-5` through `plan-11`, each with a reworded body and each
answered "Plan recorded" plus a re-announcement of the unblock list
that had already been announced.

The counter is now allocated per plan. A repeat by the same author —
`(agent, session)`, the pair the frontmatter already records — within
the same turn overwrites the artifact it wrote rather than filing a
sibling; an identical body writes nothing at all; a genuinely new plan
in a later turn still takes the next sequence number, so the
cross-turn audit trail is unchanged. `/replan` stays the
revoke-and-redraft path, and because the guard verifies the remembered
artifact is still on disk, a redraft after revocation gets a fresh
sequence instead of resurrecting the archived one.

Three notes for anyone revisiting this:

- **The behaviour is the fix; the wording is not.** #857 answered the
  same loop shape on `mark_task_done` with an honest "this did
  nothing" status and the model re-called thirteen times. The message
  changes here (`recorded` / `updated` / `unchanged`, and the unblock
  list only on an actual gate transition) are worth having, but the
  load-bearing part is that call two does not produce a file. The
  `outcome` field exists so a detector can key on the no-op without
  parsing prose (#907) — though the detector does not read `outcome`
  itself. #918 found that gap: #907's `no-op-streak` reads exactly one
  reserved response key, `no_op`, and this result did not set it, so
  the two halves shipped a month apart and never met. The `unchanged`
  branch now sets `no_op`; `updated` deliberately does not, because
  overwriting the artifact is work and a revising agent must not
  accumulate a streak toward a Critical halt. Keeping the two fields
  separate is the point rather than an accident: `outcome` is this
  tool's own vocabulary and may be reworded, `no_op` is the runtime's
  and may not.

- **The turn boundary comes from ADK's invocation ID.** `pkg/agent`'s
  checkpointer keys its in-turn repeat flag off an `Agent` field that
  a post-turn hook clears; `pkg/tools` has neither an `Agent` nor a
  turn hook, but `tool.Context.InvocationID()` is the same signal
  already threaded through every tool call in a turn, and state keyed
  by it expires on its own instead of needing a reset callback a
  library caller could forget to wire.

- **State is per author, not per turn.** Keying the map by turn would
  lose the ability to notice that the *current* artifact already holds
  this exact plan; keying it by author and storing the invocation
  inside the entry gets both, and keeps a parent and its declarative
  subagent (the #747 case) on separate plans when they interleave
  inside one turn. The map is bounded at 64 authors, least-recently-used
  evicted first; an evicted author degrades to the old behaviour — one
  needless plan file, never a wrong one. LRU rather than insertion
  order is load-bearing, not a detail: the synchronous subagent door
  derives a session ID per delegation, so a fan-out parent produces a
  stream of single-use keys, and under insertion order the primary
  session — inserted once and thereafter only re-read — would be the
  first key that churn evicted, switching the guard off for exactly the
  session it exists to protect.

Two limits worth stating plainly, since both are things the guard
deliberately does not do:

- **The unit is one delegated run, not one incident.** Because a
  synchronous subagent's session ID is derived per delegation,
  delegating twice to the same subagent yields two plan artifacts. That
  is the right reading — two pieces of work — but it means the
  directory still grows with delegation count, and only a loop *inside*
  a run is collapsed.

- **The overwritten in-turn draft is not archived.** Plan artifacts are
  the current plan, not a version history; a draft the model revised
  seconds later is not an operator decision worth preserving, and the
  audit trail that does matter — a plan the operator rejected — is what
  `/replan`'s `-revoked.md` rename keeps. Both `record_plan` and
  `/replan` now mutate the plans directory under one process-wide lock,
  which closes the window where a revoke landing between the guard's
  existence check and its write could have put a live plan back at a
  path the operator had just retired.
