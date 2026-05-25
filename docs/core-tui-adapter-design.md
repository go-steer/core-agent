# core-tui adapter — design doc

Design doc for migrating `core-agent` off its in-tree `internal/tui/`
in favor of importing
[`github.com/go-steer/core-tui`](https://github.com/go-steer/core-tui).
This is the project-side complement to core-tui's
[`MIGRATION.md`](https://github.com/go-steer/core-tui/blob/main/MIGRATION.md)
— it picks per-host decisions, lays out the PR sequence, binds each
of the §5.1 "real semantic gaps" to a concrete resolution, and names
the files that need to change.

Pairs with
[`embedded-tui-design-v2.md`](./embedded-tui-design-v2.md), which
established core-agent's current in-process TUI. This doc is the
follow-on: instead of carrying our own copy of the TUI, we depend on
the externalized library and write a thin adapter.

## 1. Why now

The embedded TUI v2 lift from cogo landed in May 2026 and proved the
in-process TUI shape works. Since then we've grown five new TUI
features (queue panel state machine, `/btw` modal, `/subagent` flag
parser, mid-turn inbox injection, wake signal) that are non-trivial
to keep in sync between cogo's TUI tree and ours. The maintenance
cost of two copies is real:

- Every TUI feature is written twice (here + cogo) and risks
  drifting.
- A bug fix in one tree might not land in the other for weeks.
- Both projects burn engineering on the same surface.

`core-tui` is the consolidation. Once core-agent depends on it, we
delete `internal/tui/` and inherit the same TUI cogo will eventually
depend on.

## 2. Scope

In:
- New `cmd/core-agent-tui/main.go` (local in-process mode) that
  imports `core-tui` and wires every core-agent capability against
  its interface set.
- (Future PR) `cmd/core-agent-tui-attach/main.go` (attach mode)
  with the SSE consumer + reconnection loop.
- Delete `internal/tui/`.
- A focused PR sequence (see §6) so each step is reviewable.

Out:
- No changes to `internal/agent`, `internal/attachclient`,
  `internal/permissions`, `internal/pricing`, or `internal/config`
  beyond what's needed to *expose* state the adapter consumes
  through stable interfaces. The agent loop is unchanged.
- No changes to the attach API surface. Attach-mode features that
  depend on RPCs we don't have today (model swap, reload, permissions
  edit, pricing) stay "not available in attach mode" until those
  RPCs land in a separate PR.

## 3. Capability binding decisions

Each `MIGRATION.md` §3.2 row maps to one of:

- **Wrap** — adapter wraps an existing core-agent type, no new code
  on core-agent's side.
- **Expose** — core-agent grows a small new accessor or callback so
  the adapter can wrap it cleanly.
- **Defer** — capability is left unwired in the first migration;
  command degrades to "not available."

| core-tui interface | core-agent source | Binding |
|---|---|---|
| `tui.Agent.Run` | `agent.Agent.Run` | Wrap |
| `Interruptible.Interrupt` | `agent.Agent.Interrupt` | Wrap |
| `ToolLister.Tools` | `agent.Agent.Tools` | Wrap |
| `SubagentLister.Subagents` | `agent.Agent.BackgroundManager().Subagents()` | Wrap |
| `StatusReporter.Status` | `agent.Agent.AttachStatus` | Wrap |
| `ModelSwapper.{Available, SwitchModel}` | existing `rebuildAgent` callback + a new `availableModelIDs() []string` helper | Expose (small helper) |
| `Reloader.Reload` | existing `reloadFromDisk` callback | Wrap |
| `PermissionController` (5 methods) | `permissions.Gate` | Wrap |
| `PricingController.{Refresh, Set}` | `internal/pricing.{RefreshPricing, SetPricing}` | Wrap |
| `SlashProvider` for `/subagent` | new flag parser in adapter; calls `BackgroundManager().Spawn` | Adapter owns |
| `SlashProvider` for `/btw` | `agent.AskSideQuestion` | Adapter owns (with caveat — see §4.1) |
| `PermissionPrompter` (TUI-provided) | wired into `gate.SetPrompter` | Wire |
| `Elicitor` (TUI-provided) | wired into each MCP server's elicit callback | Wire |
| `UserPrompter` (TUI-provided) | (no use today; pre-wire for future tools) | Optional |

## 4. Gap resolutions

These five gaps (`MIGRATION.md` §5.1) need a project-side decision
before the adapter is written. Each is bound below.

### 4.1 `/btw` modal-rendered answer

**Today:** core-agent renders the side-question answer in a
transient modal overlay (`internal/tui/btw.go`), Glamour-rendered,
dismissable, doesn't land in chat history.

**Punt option:** Adapter implements `/btw` via `SlashProvider` and
appends the answer as a `RoleSystem` chat row. Answer lives in
history forever; no Glamour rendering on system rows (yet).

**Spec-it option:** Push a small extension to core-tui's
`SlashResult` adding an optional `ModalAnswer *SideAnswer` field;
when non-nil, the TUI renders a Glamour modal overlay with
dismiss-on-Esc semantics matching `internal/tui/btw.go` today.

**Decision: Spec it.** `/btw` is a frequent operator workflow; the
permanent-history-row punt would be a real regression. The
`ModalAnswer` extension is ~50 lines on core-tui's side and matches
the existing modal-compositor pattern (`elicit.go`, `model_picker.go`).
The adapter passes its `AskSideQuestion` result through as
`SlashResult{ModalAnswer: &SideAnswer{Question, Answer}}`; core-tui
renders the modal.

Pre-req: a small core-tui PR landing the `SlashResult.ModalAnswer`
field + the new modal renderer **before** this adapter PR.

### 4.2 Mid-turn inbox injection

**Today:** `agent.Inject(message)` feeds a message INTO the running
turn. core-agent's TUI calls `Inject` on every operator-typed-during-
streaming entry; the agent drains the inbox on turn-end and
auto-continues with the drained messages as the next prompt.

**Punt option:** Adapter ignores `Inject`; operator-typed-during-
streaming entries go to core-tui's R-CHAT-10 queue (which buffers
for the NEXT turn). Loses the auto-continue / mid-turn-context UX.

**Spec-it option:** Add an `InjectableAgent` capability with a
single `Inject(msg) error` method + an `Options.MidTurnInjectionMode`
flag (`QueueForNext` default, `InjectIntoCurrent` opt-in). When the
host enables `InjectIntoCurrent`, the queue panel's Enter handler
calls `Inject` directly (no buffering); the panel renders entries
as in-flight immediately.

**Decision: Spec it.** core-agent's operators rely on the auto-
continue / mid-turn-context flow today; punting would be a real
regression. Add the `InjectableAgent` capability + the
`Options.MidTurnInjectionMode` enum (default `QueueForNext` matches
R-CHAT-10 today; opt-in `InjectIntoCurrent` routes the queue
panel's Enter through `Inject`). cogo doesn't have inbox today so
the capability stays dormant for cogo — fine, that's what
type-assertion feature detection is for.

Pre-req: a core-tui PR landing the capability + Options field
**before** this adapter PR.

### 4.3 Queue-panel state machine

**Today:** `internal/tui/queue.go` tracks each entry through
`queued → in-flight → done / failed`, with per-entry error display
and a 2-second fade for Done entries. core-tui's queue is flat
`[]string`.

**Punt option:** Use core-tui's flat queue. Lose the per-entry
state tracking and error display.

**Spec-it option:** Promote `tui.Model.queue` from `[]string` to
`[]QueueEntry{Text, State, Err, Created}` with state glyphs
(⏳ ↻ ✓ ✗) and TTL-based culling. ~60 lines on core-tui's side.

**Decision: Spec it.** The state machine is small but high-value —
the in-flight indicator and error display are what give the queue
panel its sense of life. Bundle with the §4.1 modal answer change.

Pre-req: a small core-tui PR upgrading the queue panel **before**
this adapter PR.

### 4.4 Wake signal (`WakeRequested()`)

**Today:** `agent.WakeRequested() <-chan struct{}` exists but the
TUI doesn't surface it. Background agents can request attention but
the user sees nothing.

**Decision: Spec it.** The wake channel exists server-side; giving
it a TUI surface (transient toast banner per the Crush pattern in
[`core-tui/docs/ui-references.md`](https://github.com/go-steer/core-tui/blob/main/docs/ui-references.md#charmbraceletcrush))
finishes the feature. Add a `WakeRequester` capability with a
`WakeRequested() <-chan struct{}` method; core-tui's Init subscribes
to the channel and renders a transient toast on each signal.

Pre-req: a core-tui PR landing the `WakeRequester` capability +
toast banner.

### 4.5 `RunWithContents` (structured prompts)

**Today:** alternate `Run` taking `[]Content` instead of `string`.
Used internally for retry scenarios; never invoked from the TUI.

**Decision: Spec it.** Add `RunWithContents(ctx, contents []Content)
iter.Seq2[Event, error]` as an optional method on `tui.Agent`
(feature-detected via type assertion). Adapter uses it for retry
flows where the prompt is rebuilt from prior turn fragments.

## 5. New core-agent code

The adapter needs core-agent to expose two small helpers:

- **`agent.Agent.AvailableModels() []ModelInfo`** — returns the
  model list `rebuildAgent` would accept. Today the model list is
  implicit in `rebuildAgent`'s allow logic; the adapter needs an
  explicit query so `ModelSwapper.AvailableModels()` returns
  synchronously without falling back to a hardcoded list.
  ~10 lines (probably already exists in some internal form).
- **`pricing.Manager.Snapshot() PricingSnapshot`** — read-only
  snapshot of the live pricing catalog so the adapter can map it
  through `PricingController` cleanly. ~20 lines.

No changes to `internal/agent`'s public types, no changes to
`internal/permissions`. `internal/tui/` is deleted in the final PR
of the sequence.

## 6. PR sequence

Five focused spec PRs in core-tui first, then four PRs in core-agent.
Each is small enough to review in isolation.

**core-tui prerequisite PRs:**

1. **`SlashResult.ModalAnswer` + side-answer modal renderer.**
   Closes §4.1. Cheap, isolated; tested against the visual preview.
2. **Promote `Model.queue` to `[]QueueEntry`.** Closes §4.3.
   State machine (queued / in-flight / done / failed) + state
   glyphs + TTL-based culling.
3. **`InjectableAgent` capability + `Options.MidTurnInjectionMode`.**
   Closes §4.2. Default `QueueForNext` preserves R-CHAT-10;
   `InjectIntoCurrent` routes queue Enter through `Inject`.
4. **`WakeRequester` capability + toast banner.** Closes §4.4.
   New `R-WAKE-1` requirement; ui-references Crush toast pattern.
5. **`RunWithContents` optional method on `tui.Agent`.** Closes
   §4.5. Feature-detected via type assertion.

After PRs 1–5 land in core-tui (and a `v0.1.0` tag is cut),
core-agent's series begins:

6. **core-agent PR 1 — Expose helpers.** Add
   `agent.AvailableModels()` and `pricing.Snapshot()`. No TUI
   changes. Trivial review.
7. **core-agent PR 2 — Add `cmd/core-agent-tui/`.** New entry point
   that imports core-tui and wires every capability. Existing
   `internal/tui/` stays untouched so both code paths exist
   side-by-side and the operator can A/B them. Add a config flag
   (`tui_provider: "core-tui" | "internal"`) defaulting to
   `internal` for safety.
8. **core-agent PR 3 — Flip default.** Once PR 2 has soaked, flip
   `tui_provider` default to `core-tui`.
9. **core-agent PR 4 — Delete `internal/tui/`.** After one minor
   release with the new default green.

PRs 1–5 land in core-tui (with the version tag); 6–9 are the
core-agent series.

## 7. Smoke-test acceptance

The `cmd/core-agent-tui/` binary must demonstrate every behavior
the operator relies on today. Acceptance is per-command:

- [ ] `/help` shows the merged built-in + adapter command list
- [ ] `/clear` resets history
- [ ] `/quit` exits cleanly with transcript saved
- [ ] `/memory` renders loaded memory files
- [ ] `/stats` renders per-turn + session totals
- [ ] `/model` opens picker; `/model <id>` switches directly
- [ ] `/mcp` lists configured servers
- [ ] `/skills` lists loaded skills
- [ ] `/tools` lists tools with gate state
- [ ] `/interrupt` cancels in-flight turn (Esc shortcut also works)
- [ ] `/reload` re-reads `.agents/` and rebuilds the agent
- [ ] `/permissions` opens review picker; `/permissions list`
      prints config; `/allow <pattern>` + `/deny <pattern>` apply
      live + persist
- [ ] `/pricing refresh` and `/pricing set <model> <in> <out>`
      both return human-readable summary lines
- [ ] `/btw <question>` opens a Glamour modal with the answer;
      Esc/Enter/Space dismisses
- [ ] `/subagent <goal> --name=X --tools=Y` spawns a background
      agent visible in `/subagents` (read-only)
- [ ] Streaming: assistant text renders via Glamour live; per-turn
      footer shows model + tokens + cost + elapsed after completion
- [ ] R-CHAT-10 queue: typing during streaming queues for the next
      turn; queue auto-drains on turn-end
- [ ] `Ctrl+B` toggles `StatusHeader` / `StatusSidebar`; choice
      persists via `Options.PersistStatusLayout`
- [ ] `Shift+Tab` cycles permission-mode chip;
      `Options.PermissionMode.Set` is called per toggle
- [ ] Mouse capture default is ON; `/mouse off` toggles
- [ ] Transcript saves on clean exit to `<AgentsDir>/sessions/`

## 8. Risk register

- **R1: `SlashResult.ModalAnswer` extension diverges from existing
  modal compositor.** Mitigation: prototype the field shape against
  `internal/tui/btw.go` before the core-tui PR is opened.
- **R2: Queue-entry state machine timing.** core-agent's queue uses
  wall-clock for the 2s Done fade; core-tui's tick cadence may
  differ. Mitigation: include TTL handling tests in the core-tui PR.
- **R3: Per-model `ModelConfig.Pricing` map shape mismatch.**
  Mitigation: `PricingController.Set` writes through a callback
  the adapter owns; core-tui never sees the map shape.
- **R4: Attach-mode parity gaps.** Punted to a follow-on PR;
  flag-degrades in v1.

## 9. Out of scope (this design)

- Attach-mode adapter (`cmd/core-agent-tui-attach/`) — separate doc.
- Eventlog resume / replay UX — deferred per core-tui D20.

(`InjectableAgent`, `WakeRequester`, and `RunWithContents` were
listed as deferred in an earlier draft; the spec-everything
decision in §4 brings them in scope as core-tui prerequisite PRs.)

## 10. Open questions

- **Should we bump core-tui to a tagged release before the adapter
  lands?** Recommended: yes, after PRs 1 and 2 from §6, tag
  `v0.1.0`. Pin core-agent's `go.mod` to it; bump on schedule.
- **Should the side-answer modal share its compositor with the
  existing model picker?** Probably yes — both are simple
  centered-modal-with-Esc-dismiss. Implementation detail for the
  core-tui PR.
- **Where does `Options.MidTurnInjectionMode` live when we spec it
  later?** Probably extends `PermissionModeWiring`'s pattern: an
  `InjectionWiring` field on `Options` with a `Mode` and an
  `Inject` callback.
