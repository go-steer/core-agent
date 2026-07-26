# Splitting the `pkg/agent` god package before the v2 API freezes

Design doc for decomposing `pkg/agent` — today a ~8.6k-LOC package
whose `Agent` type carries ~50 fields and 60+ exported methods across
four separable concerns — into a small stable core plus focused
sibling packages, while the v2 surface is still unfrozen and the
break is cheap.

**Status:** proposed (2026-07-26). Human-led / API-shape; design-doc
first per `docs/cleanup-execution-plan.md` (Wave 3).

**Tracking issue:** [#388](https://github.com/go-steer/core-agent/issues/388)

**Sequencing:** this lands **before** the `pkg/compose` extraction
([#386](https://github.com/go-steer/core-agent/issues/386)) so that
compose is written against the settled agent surface rather than
chasing it. Signed off with the maintainer 2026-07-26.

## Why now

`cmd/core-agent` is meant to be *a* reference composition, not *the*
runtime. Every method and option on `Agent` is a compat commitment the
moment a v2 consumer (cogo, scion, ax) pins the module. `Agent` today
mixes four concerns that have no reason to share a type:

1. **Core turn loop** — `Run`, the ADK wiring, gate, tracker, session
   plumbing. This is the substrate everyone depends on.
2. **Autonomous driver** — `RunAutonomous`/resume/handle/checkpoint
   (~1.8k LOC). Touches only the core's public surface plus one
   unexported hook (`runOneTurn`).
3. **Background subagent manager** — `background*.go`, `remote.go`,
   `inbox.go`, `wake.go` (~1.9k LOC). Already its own object graph
   (`BackgroundAgentManager`); barely reaches into `Agent` at all.
4. **Attach adapter** — 22 `Attach*` methods + 9 `WithAttach*`
   options + ~11 fields welded onto `Agent` (~900 LOC). Pure
   presentation/IPC glue for `pkg/attach`; nothing in the core turn
   loop needs it.

Freezing concerns 2–4 onto the core type is the mistake this doc
prevents.

## Key finding: the seam is narrow

The blocker named in #388 is private-field access from the driver /
manager files. An audit of exactly which unexported `Agent` internals
each mover-candidate file reaches shows the coupling is far shallower
than the file sizes suggest:

| File | Private `Agent` internals touched |
|---|---|
| `autonomous.go` | `inner`, `modelName` |
| `resume.go` | *(none — public surface only)* |
| `autonomous_handle.go` | *(none on `Agent`; `a.kind` is on the handle type)* |
| `checkpoint.go` | `eventLog` |
| `background.go`, `background_spawn.go`, `background_tools.go`, `background_report.go`, `remote.go` | *(none — operate on `BackgroundAgentManager`)* |
| `inbox.go` | `inbox`, `emit`, `drainInboxFull` |
| `wake.go` | `wake` |

So the entire split is unblocked by exposing a **small, deliberate
seam** — not by surgery across dozens of fields. The core turn loop
itself (`agent.go`, `compactor.go`, `checkpointer.go`, `subtask.go`,
`cost_ceiling.go`, `context_stats.go`) stays put: it is cohesive and
is the surface everyone legitimately depends on.

## Target layout

```
pkg/agent/                 core: Agent, Run, options, context mgmt,
                           cost ceiling, watchdog, event hooks
  core.go                  exported seam (see below)
pkg/agent/autonomous/      Driver over an *agent.Agent: RunAutonomous,
                           resume, AutonomousHandle, checkpoint loop
pkg/agent/background/      BackgroundAgentManager, remote spawner seam,
                           inbox, wake
pkg/attachadapter/         Adapter bridging *agent.Agent ⇄ pkg/attach
                           (the 22 Attach* methods, as an adapter type)
```

Sub-packages of `pkg/agent` (not top-level siblings) for the driver and
manager: they are agent-scoped and the import reads well
(`autonomous.Driver`, `background.Manager`). The attach adapter goes
top-level next to `pkg/attach` because it depends on both `pkg/agent`
and `pkg/attach` and belongs to neither — a sub-package would invert the
natural dependency direction.

### The seam

Rather than export the raw fields, `pkg/agent` gains a narrow,
documented accessor set consumed only by the sibling packages:

```go
// core.go — the seam the driver/manager/adapter build on.
func (a *Agent) Inner() adkagent.Agent        // for autonomous
func (a *Agent) ModelName() string
func (a *Agent) EventLog() *eventlog.Handle    // for checkpoint
func (a *Agent) Emit(eventType string, payload any)  // already exists internally as emit()
func (a *Agent) RunOneTurn(...) (...)          // promoted from unexported
```

Inbox/wake move *with* the background package (they are the manager's
own state, not the core's), so `a.inbox`/`a.wake` stop being `Agent`
fields entirely — removing three of the private-access rows above
outright rather than exposing them.

Open question for review: whether `Inner()`/`RunOneTurn()` should be
exported on `Agent` or live behind an `internal/agentcore` shared type
that only the three sibling packages import. Exporting is simpler and
these are genuinely useful to advanced consumers; `internal` keeps the
core's public surface minimal. **Recommendation: export `Inner()`,
`ModelName()`, `EventLog()` (all read-only, already-safe accessors);
keep `RunOneTurn` behind `internal/agentcore`** so the turn-loop
entrypoint isn't a frozen public commitment.

## The attach adapter (clean break)

Decision (signed off 2026-07-26): **move the attach surface off `Agent`
now, no deprecating shims.** Pre-freeze is exactly when this is cheap;
shims would re-freeze the surface we're trying to shed.

Today:

```go
a := agent.New(..., agent.WithAttachMemoryProvider(f), ...)
srv, _ := attach.NewServer(a, ...)   // a satisfies attach's iface via Attach* methods
```

After:

```go
a := agent.New(...)
ad := attachadapter.New(a,
    attachadapter.WithMemoryProvider(f),
    attachadapter.WithSkillsProvider(g),
    ...)                              // the 9 WithAttach* options move here
srv, _ := attach.NewServer(ad, ...)  // ad satisfies attach's iface
```

The 22 `Attach*` methods become methods on `*attachadapter.Adapter`;
the `attach*Fn` fields, `attachRegistrar`, `attachPromptBroker`, and
`emitMu`/`attachEmit` move into the adapter. `pkg/attach` keeps
depending on an *interface* (it already does), so only the constructor
wiring changes for consumers — a mechanical, greppable migration
documented in the CHANGELOG under **Breaking changes**.

This alone removes 22 methods + 9 options + ~11 fields from the frozen
`Agent` surface.

## Phasing (stacked PRs, each independently green)

1. **Seam + inbox/wake relocation** — add the accessors; move
   `inbox.go`/`wake.go` state into what will become the background
   package (kept in-package first to isolate the diff). No behavior
   change; no consumer break.
2. **`pkg/agent/autonomous`** — move the driver behind the seam.
   Consumer break: `a.RunAutonomous(...)` → `autonomous.New(a).Run(...)`.
3. **`pkg/agent/background`** — move the manager + inbox/wake + remote.
   Consumer break confined to background-spawn callers.
4. **`pkg/attachadapter`** — the clean break above. Largest consumer
   surface change; last so it rebases once over the settled tree.

Each PR: regression tests, `dev/ci/presubmits/*`, one CHANGELOG
`[Unreleased]` bullet (Breaking changes for 2–4), and the migration
note. Site reference docs (`docs/site/...`) updated in the same PR for
any user-visible constructor change.

## Smaller warts folded in (from #388)

- `eventlog.Handle` exposes both `DB` and unexported `db` — collapse to
  one accessor while we're touching the seam.
- `WithEventLog` vs `WithSessionService` are order-sensitive — make
  option application order-independent or document + guard it.
- `checkpoint.go` vs `checkpointer.go` naming collision — rename the
  driver-side file when it moves to `autonomous/`.

## Non-goals

- No behavior change to the turn loop, autonomous driver, or attach
  protocol — this is a pure decomposition.
- Context management (compactor/checkpointer/subtask) stays in
  `pkg/agent`; it is cohesive and correctly core.
- Not touching `pkg/compose` here — that is #386, which builds on the
  surface this doc settles.

## Risks

- **Consumer churn.** cogo/scion/ax must update constructor wiring.
  Mitigated by doing it pre-freeze, keeping breaks mechanical
  (interface-satisfaction unchanged), and documenting each in the
  CHANGELOG with before/after snippets.
- **Import cycles.** `attachadapter` → {`agent`, `attach`} is acyclic;
  `autonomous`/`background` → `agent` is acyclic. Verified against the
  current import graph; the seam introduces no back-edge.
- **Hidden private-field reach.** The audit above is the current state;
  the phase-1 PR adds a lint/build gate so a later field access outside
  the seam fails CI rather than silently re-coupling.
