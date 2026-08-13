# `plan-first` — config recipe for substrate-enforced plan-before-action

A **config-only** example that turns on the plan-first gating
primitive. Drop into any project, run `core-agent`, and the agent
is guaranteed to write a plan before it can touch the world.

The substrate (`pkg/permissions` v2.3+) enforces this via the
`record_plan` tool + a per-session gate flag. AGENTS.md tells the
model the workflow shape; the gate guarantees compliance.

See [`docs/plan-first-design.md`](../../docs/plan-first-design.md)
for the full design rationale.

## What ships in this example

```
.agents/
├── config.json                          # default: mode=ask + plan_mode=required
├── config.acceptedits.json.example      # variant: writes auto-allow after plan, bash still prompts
├── config.yolo.json.example             # variant: everything auto-allows after plan
├── config.advisory.json.example         # variant: plan is recorded but nothing is blocked
└── AGENTS.md                            # primes the model on the plan-first workflow
```

That's it. Substrate does the gating; AGENTS.md does the prompting.

## How it works

1. **Operator gives the agent a goal.**
2. **Agent researches.** Read tools (`read_file`, `grep`, etc.) are
   pre-allowed via `permissions.allow` — no prompts during research.
3. **Agent calls `record_plan(plan: <markdown>)`.** The plan persists
   to `.agents/plans/plan-<seq>.md` and renders in chat. The gate's
   `planRecorded` flag flips.
4. **Operator reviews the plan in chat** (or in the artifact file).
5. **Agent executes.** Mutating tools (`write_file`/`edit_file`/
   `delete_file`/`bash`/`spawn_agent`/MCP) now pass the plan-first
   pre-check and proceed per the chosen mode:
   - `ask` (default) — each call prompts
   - `acceptEdits` — file writes auto-allow, bash still prompts
   - `yolo` — everything auto-allows
6. **Operator rejects the plan via `/replan`** (optional). Latest plan
   gets renamed to `plan-<seq>-revoked.md`, the gate flag clears,
   the agent must call `record_plan` again before any further
   mutation.

## Choosing a variant

The default `config.json` (`ask` mode) gives the most operator
control — review the plan, then approve each subsequent step. Best
for: high-stakes / unfamiliar codebases, first-time use of an agent
on a project, plans you don't fully trust yet.

For a lower-friction loop, replace `.agents/config.json` with one of
the variants:

```bash
# "plan + stream writes" — file edits auto-allow after plan, shell prompts
cp .agents/config.acceptedits.json.example .agents/config.json

# "just tell me the plan, then go" — everything auto-allows after plan
cp .agents/config.yolo.json.example .agents/config.json

# "record the plan, don't wait for anyone" — audit trail, gate NOT armed
cp .agents/config.advisory.json.example .agents/config.json
```

Use the `yolo` variant when you've worked with the agent enough to
trust it end-to-end past the plan, or for batch automation where
the plan IS the human approval and what follows is mechanical.

### `required` vs `advisory`

The first three variants all set `plan_mode: "required"` and differ
only in the *permission* mode that applies after the plan lands. The
fourth changes the plan posture itself:

| `plan_mode` | `record_plan` registered | Mutating call with no plan |
|---|---|---|
| `required` | yes | **denied** until `record_plan` is called |
| `advisory` | yes | proceeds — the plan is an audit artifact, not a gate |
| `off` (default outside this recipe) | no | proceeds |

Pick `required` when a human really is going to read the plan before
the diff lands. Pick `advisory` for unattended or scheduled runs where
you want the reasoning on file but nobody is on the other end to
approve it — an armed gate with no approver just stalls the turn, and
the agent stops mid-task waiting for a signal that never arrives.
Advisory mode also tells the model so: the `record_plan` description
is mode-aware and, under `advisory`, explicitly says no call is
blocked on the plan.

## Run

```bash
cd examples/plan-first
core-agent
```

Drive it with prompts like:

```
> add a /health endpoint to the HTTP server in cmd/myservice
> fix the race in pkg/cache/store.go's Set method
> migrate the user-events table to add an idx_user_created index
```

You'll see the agent grep + read its way through the codebase, then
call `record_plan` with a structured plan. Review the plan, then
either let it proceed (it will), or hit `/replan` to force a redraft.

## Slash commands

Available in both the in-process TUI (`core-agent`) and the remote
TUI (`core-agent-tui http://...`):

| Slash | Effect |
|---|---|
| `/replan [reason]` | Revoke the current plan, force the agent to redraft. Archives `plan-<N>.md` → `plan-<N>-revoked.md`. |
| `/permissions` | Show the active mode + plan-recorded state. |
| `/allow write_file allow-session` | Once you trust the plan, lift the per-call prompts. |

## Composing with an existing project

The recipe is shaped as a standalone example. To layer it into a
project that already has its own AGENTS.md:

```bash
mkdir -p <your-project>/.agents/AGENTS.d
cp examples/plan-first/.agents/AGENTS.md \
   <your-project>/.agents/AGENTS.d/00-plan-first.md
```

Then add `permissions.plan_mode: "required"` (and the
pre-allow list for read tools) to your existing `.agents/config.json`.
The v2.3 instruction loader (`@include` + `AGENTS.d/`) loads your
existing AGENTS.md as the primary file and the plan-first guidance
as a `AGENTS.d/` overlay.

## What this example does NOT do

- **No MCP server.** Plan-first composes with any tool palette but
  this recipe doesn't ship an MCP config. If you wire one in, the
  MCP tools are gate-denied until `record_plan` (per Q1 of the
  design: gate everything by default).
- **No mid-execution checkpoints.** v1 records ONE plan that covers
  the whole task. Plan-progress tracking (TodoWrite-shape, where the
  agent marks plan items done as it goes) is a v2 design — see the
  task #9 follow-up.
- **No structured plan schema.** Plan is free-form markdown. The
  operator picks the shape via AGENTS.md prompting.
- **No `$EDITOR` shell-out for in-modal plan editing.** If you want
  to edit a plan before approval, open `.agents/plans/plan-<seq>.md`
  in another window, then `/replan` to force a redraft from
  whatever notes you typed at the agent.

## Plan artifacts on disk

```
.agents/plans/
├── plan-1.md                # first plan
├── plan-2-revoked.md        # operator /replan'd this one
└── plan-3.md                # current active plan
```

Sequence numbers monotonically increase. Revoked plans stay on
disk as `plan-<N>-revoked.md` for audit. Add `.agents/plans/` to
`.gitignore` if you don't want to check plans in (or DO check them
in — they make excellent PR descriptions).

## Compose with the rest of the substrate

- **Eventlog**: add `--session-db <path>` to persist the session.
  The `record_plan` tool call shows up in the eventlog like any
  other tool call; combined with the on-disk plan artifact, you
  get both "what was approved" and "what happened next" in
  durable form.
- **Remote attach**: run `core-agent --attach-listen=:7777 --no-repl`
  in one terminal, then `core-agent-tui http://localhost:7777` in
  another. Both surfaces share the same gate state — a `/replan` on
  either side resets both.
- **Scheduled monitoring**: `agent.RunAutonomous` + `plan_mode: "advisory"`
  + a scheduler gives "scheduled task records its plan, then carries
  it out; the operator reads the plan artifact after the fact." Use
  `"required"` here only if someone will actually be attached to
  approve — otherwise the run stalls on the gate. See
  `examples/scheduled-monitor` for the substrate pattern.
