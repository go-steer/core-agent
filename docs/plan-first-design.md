# Plan-first enforcement: gate-level "plan before action"

Design doc for a substrate primitive that **enforces** the
plan-first workflow — write a plan, present it, get approval — rather
than relying on AGENTS.md prompting alone.

**Status:** proposed (2026-06-02). Awaiting approval before
implementation. v2.3 candidate.

## Motivation

Plan-first is a well-known workflow for high-trust agentic work:
research the goal, write a structured plan, get explicit operator
approval, *then* execute. Claude Code, Aider, and Cursor all
support it; Jetski CLI's "VCS-Aware Planning Mode" is the same
shape (`docs/jetski-compare.md` § 5.A).

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

The Jetski-comparison thread (this session, 2026-06-02) surfaced
this as the cleanest item we don't already do — Review Card,
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
| Spawn family (`spawn_agent`, etc.) | **yes** | a subagent can do arbitrary work |
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
$EDITOR before approval" (which Jetski's Review Card does via
`tea.ExecProcess`); operators who want that today can edit the
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

## Out of scope (deferred to v2)

- $EDITOR shell-out from the approval modal (Jetski's
  `tea.ExecProcess` pattern). The `/replan` workflow covers the
  same need adequately for v1; operators who want in-modal
  editing should weigh in.
- Per-section plan approval ("approve files 1-3, reject 4").
- Plan templates / schemas. Free-form markdown for now.
- Multi-tier plans (orchestrator plan + subagent plans).
- Plan auto-summarization for compaction. The plan stays
  out-of-context once approved, but it's worth surfacing on
  `/compact` so the post-compaction context reminds the model
  what was approved.
