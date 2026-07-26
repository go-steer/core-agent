# Splitting the `pkg/agent` god package before the v2 API freezes

Design doc for decomposing `pkg/agent` — today a ~8.6k-LOC package
whose `Agent` type carries ~50 fields and 60+ exported methods across
four separable concerns — into a small stable core plus focused
sibling packages, while the v2 surface is still unfrozen and the
break is cheap.

**Status:** in progress (2026-07-26). Human-led / API-shape; design-doc
first per `docs/cleanup-execution-plan.md` (Wave 3). Phase 1 (seam)
landed in #440. Phases 2 and 3 (`autonomous` + `background`) landed
together — see **Phasing** for why they could not be separated and the
**Implementation notes** for how the design changed under contact.

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

So the *field-access* seam is narrow — the split is unblocked by
exposing a small, deliberate accessor set, not by surgery across dozens
of fields. The core turn loop itself (`agent.go`, `compactor.go`,
`checkpointer.go`, `subtask.go`, `cost_ceiling.go`, `context_stats.go`)
stays put: it is cohesive and is the surface everyone legitimately
depends on.

**Correction (as-built):** the field table above is accurate but it
undersold the *type*-level coupling for the background manager, which is
what actually made phase 3 harder than "operates on
`BackgroundAgentManager`, no `Agent` internals" implies:

- `agent.go` held a `*BackgroundAgentManager` **field** and called its
  methods directly (`AttachAgents`, `AttachSpawnSubagent`). Moving the
  manager into a sub-package would make `agent` import `background`,
  while `background` must import `agent` (its spawned subagents *are*
  `*agent.Agent`) — a cycle no accessor fixes.
- The background spawner runs each subagent through the autonomous
  driver, so `background` also imports `autonomous`. Once `autonomous`
  is its own package, phase 3 cannot compile until phase 2 exists — the
  two moves are not independently landable.
- Core's `subagent.go`/`subtask.go` share unexported session-derivation
  plumbing (`composeBranch`, `deriveSubagentSessionID`,
  `branchInjectingService`, the depth context key) with the background
  spawner. Splitting the packages orphans that shared code.

The resolution for all three is in **Implementation notes**.

## Target layout

```
pkg/agent/                 core: Agent, Run, options, context mgmt,
                           cost ceiling, watchdog, event hooks, inbox,
                           wake, and the SubagentManager seam interface
  subagent_manager.go      SubagentManager interface (the manager seam)
pkg/agent/internal/subsession/
                           shared subagent session-derivation plumbing
                           (branch compose, session-id derive, depth
                           context, branch-injecting session.Service)
pkg/agent/autonomous/      Driver over an *agent.Agent: RunAutonomous,
                           resume, autonomous Handle, checkpoint loop
pkg/agent/background/      background.Manager, spawn tools, remote
                           spawner seam
pkg/attachadapter/         Adapter bridging *agent.Agent ⇄ pkg/attach
                           (the 22 Attach* methods, as an adapter type)
                           — phase 4, not yet landed
```

Sub-packages of `pkg/agent` (not top-level siblings) for the driver and
manager: they are agent-scoped and the import reads well
(`autonomous.RunAutonomous`, `background.Manager`). The attach adapter
goes top-level next to `pkg/attach` because it depends on both
`pkg/agent` and `pkg/attach` and belongs to neither — a sub-package
would invert the natural dependency direction.

`inbox.go`/`wake.go` **stayed in core** (see Implementation notes): they
are wired into the core `Run`/`Inject` path, not just the manager, so
relocating them is a separate change and out of scope for this split.

### The seam

Rather than export the raw fields, `pkg/agent` gains a narrow,
documented accessor set consumed only by the sibling packages:

```go
// The read-only accessor seam the driver/manager build on (in agent.go).
func (a *Agent) Inner() adkagent.Agent               // for autonomous
func (a *Agent) ModelName() string
func (a *Agent) EventLog() *eventlog.Handle           // for checkpoint
func (a *Agent) Emit(eventType string, payload any)   // attach SSE
func (a *Agent) Streaming() adkagent.StreamingMode    // for spawn
func (a *Agent) Tracker() *usage.Tracker              // for spawn/usage roll-up
```

**As-built, the open question resolved toward "export the read-only
accessors, keep the turn-loop entrypoint private."** `RunOneTurn` was
*not* promoted: the autonomous driver kept its own unexported
`runOneTurn`, which simply moved into the `autonomous` package with the
rest of the loop — so no turn-loop entrypoint is frozen onto the public
surface and no `internal/agentcore` type was needed. The accessors are
all read-only and nil-safe; `Streaming()`/`Tracker()` were added beyond
the phase-1 set once the background spawner (which builds child agents
mirroring the parent's streaming mode and rolls child usage into the
parent tracker) turned out to need them.

Because the background manager could not simply be referenced by
concrete type from the core (that is the import cycle above), the core
also grows a **manager seam interface** rather than only accessors:

```go
// subagent_manager.go — the core's view of "something that spawns subagents".
type SubagentManager interface {
    AttachParent(*Agent)
    PrependPendingAlerts(prompt string) string
    ListSubagents() []attach.AgentInfo
    SpawnSubagent(ctx context.Context, spec attach.SubagentSpec) (attach.SubagentSpawnResponse, error)
}
```

`agent.WithBackgroundManager` and `agent.BackgroundManager()` traffic in
this interface; `*background.Manager` implements it. Callers needing the
manager's richer surface recover the concrete type with
`background.ManagerOf(a)`.

Inbox/wake did **not** move (contrary to the original plan): they are
part of the core `Run`/`Inject` path, so `a.inbox`/`a.wake` stay `Agent`
fields. Relocating them is deferred as a separate change.

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

## Phasing (as landed)

1. **Seam** — add the read-only accessors (`Inner`, `ModelName`,
   `EventLog`, `Emit`, later `Streaming`, `Tracker`). No behavior
   change; no consumer break. **Landed in #440.** (Inbox/wake relocation
   was dropped from this phase — see Implementation notes.)
2. **`pkg/agent/autonomous`** — move the driver behind the seam.
   Consumer break: `agent.RunAutonomous(...)` → `autonomous.RunAutonomous(...)`
   (the symbols keep their names; only the import path changes).
3. **`pkg/agent/background`** — move the manager + spawn tools + remote,
   behind the new `SubagentManager` seam, with the `Background*`→`*`
   rename. Consumer break confined to background-spawn callers.

   **Phases 2 and 3 were combined into one PR.** They are not
   independently landable: `background` imports `autonomous` (subagents
   run through the driver) and imports `agent` (subagents are
   `*agent.Agent`), while the core→manager reference had to flip to the
   `SubagentManager` interface in the same change to break the cycle. A
   phase-2-only tree wouldn't compile phase 3, and a phase-3-only tree
   needs phase 2 to exist. One breaking PR, one migration for consumers.
4. **`pkg/attachadapter`** — the clean break above. Largest consumer
   surface change; last so it rebases once over the settled tree. **Not
   yet landed.**

Each PR: regression tests, `dev/ci/presubmits/*`, one CHANGELOG
`[Unreleased]` bullet (Breaking changes for 2–4), and the migration
note. Site reference docs (`docs/site/...`) updated in the same PR for
any user-visible constructor change.

## Implementation notes (deviations from the proposal)

Recorded so the next reader trusts the code over the plan:

- **Phases 2+3 shipped together** for the import-cycle reason above.
- **`SubagentManager` interface** (`subagent_manager.go`) is the core's
  seam onto the manager, replacing the concrete `*BackgroundAgentManager`
  field. This is the load-bearing piece the original "background barely
  touches `Agent`" framing missed: the coupling was a *type* reference in
  `agent.go`, not private-field reach.
- **`pkg/agent/internal/subsession`** houses the subagent
  session-derivation plumbing that core (`subagent.go`, `subtask.go`)
  and `background` both need: `ComposeBranch`, `DeriveSessionID`,
  `CurrentDepth`/`WithDepth`, and `BranchInjectingService`. A Go
  `internal/` package under `pkg/agent/` is importable by `pkg/agent`
  and its sub-packages but nothing else, so this shares code without
  widening the public surface. `subagentInvocationID` stayed in core
  (it feeds `DeriveSessionID` but is core's concern).
- **`Background*` prefix dropped** on the manager API now that the
  package name carries the qualifier (`background.Manager`, not
  `background.BackgroundAgentManager`). Full table in the CHANGELOG.
- **`Streaming()`/`Tracker()`** were added to the seam beyond phase 1's
  set, needed by the spawner.
- **Inbox/wake stayed in core.** The proposal had them moving with
  `background`; in practice they sit on the core `Run`/`Inject` path, so
  moving them is a separable change and was left for later.
- **`RunOneTurn` was not promoted;** the driver's `runOneTurn` moved into
  `autonomous` unexported. No `internal/agentcore` type was introduced.

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
- **Import cycles.** `attachadapter` → {`agent`, `attach`} is acyclic.
  The `agent`/`autonomous`/`background` triad required care: the naïve
  move creates `agent → background → agent`. Resolved by the
  `SubagentManager` interface (core references the manager only through
  it) and the `internal/subsession` shared package. Final graph:
  `agent → subsession`; `autonomous → agent`; `background → {agent,
  autonomous, subsession}`. Acyclic, verified by `go build ./...`.
- **Hidden private-field reach.** The audit above is the current state;
  the phase-1 PR adds a lint/build gate so a later field access outside
  the seam fails CI rather than silently re-coupling.
