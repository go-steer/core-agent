# Unified subagent invocation

**Status:** DESIGN (draft, iterated during implementation). Target: **v2.9**.
Tracking issue: [#626](https://github.com/go-steer/core-agent/issues/626).
Companion issues: [#625](https://github.com/go-steer/core-agent/issues/625)
(fold the background-subagent tool family) and
[#627](https://github.com/go-steer/core-agent/issues/627) (operator-visible
subagent catalog) — both framed here as execution against this model, not
independent designs. Builds on `docs/declarative-subagents-design.md` (the
predefined roster) and `docs/background-subagents-design.md` (the runtime spawn
substrate + push-based reports). Part of the Hermes-replacement epic
[#589](https://github.com/go-steer/core-agent/issues/589).

## Motivation

A parent agent can already get a subagent three ways
(`docs/declarative-subagents-design.md` §"three ways"): `WithSubagents` (Go),
`spawn_agent` (runtime, ad-hoc persona), and `subagents[]` (config, predefined).
But the model-facing surface is **split into two disjoint worlds**:

- **Predefined subagents** (`config.SubagentSpec`, `pkg/config/config.go:556`)
  are wired via `NewSubagentTool` as **named, synchronous** delegation tools.
  The parent calls `cluster(task=...)`, blocks, gets the result inline. It can
  route by reading each tool's `Description`, and pass a plan as the task text.
  There is **no way to run one of these asynchronously.**
- **Background subagents** (`spawn_agent`, `pkg/agent/background/tools.go:30`)
  run **asynchronously** and push results back as `[Background reports]` lines on
  the parent's next turn — but the persona is **ad-hoc**: the parent supplies
  `system_prompt`/`tools`/`goal` inline at spawn time, and there is **no model
  selection at all** (every background subagent runs on the manager's single
  configured model, `spawn.go:138`). There is no way to spawn a *predefined*
  spec this way.

So today: predefined ⇒ sync-only; async ⇒ ad-hoc-only. The kube-platform-agent
UAT ([#622](https://github.com/go-steer/core-agent/issues/622)) wants the natural
middle: *the platform agent decides an incoming event is a triage, comes up with
a quick plan, and delegates it to the predefined `cluster` subagent — sometimes
inline (wait for the answer), sometimes fire-and-continue (handle other events in
parallel).* Neither world offers that.

Two secondary problems surfaced alongside it:

- The `list_agents`/`check_agent` **poll** tools were the fixation source in the
  runaway-loop UAT (fixed defensively in
  [#624](https://github.com/go-steer/core-agent/issues/624)); async results are
  already delivered by *push*, so the poll tools are largely redundant fuel.
- There is no operator-facing way to see which subagents a running daemon is
  configured with.

## Goals

- **One model-facing surface** for invoking a subagent, sync or async — the
  fewest tools that still express both modes (the "too many confusing tools"
  constraint).
- **Reference a predefined spec** by name, or (where policy allows) spawn an
  **ad-hoc** one — as two ends of the *same* call, not two tools.
- **Unified model resolution** on every path: inherit the parent, a `small`
  tier shorthand, or a specific model.
- **Narrowing-only overrides** on a referenced spawn — the parent may restrict
  blast radius, never widen it.
- **Push-based results** as the norm; retire the poll tools (#625).
- **Operator-visible catalog** of configured subagents (#627).
- Reuse the shipped substrate (`NewSubagentTool`, the background `Manager`,
  branch isolation, depth cap, `[Background reports]` push) — wiring, not a new
  runtime.

## Non-goals (v2.9)

- Remote/peer subagents — W6 ([#595](https://github.com/go-steer/core-agent/issues/595))
  `call_peer`; this doc is in-process (the existing `RemoteAgentSpawner`
  substrate is untouched and inherits the same surface).
- A structured "plan" artifact type — a plan is passed as the goal/task **text**;
  the parent composes it. No new schema.
- Widening the permission model — subagents still inherit the spawner's gate
  wholesale (`docs/background-subagents-design.md` §Permissions). Narrowing
  overrides operate *within* that gate.
- Recursive declarative nesting beyond the existing `MaxDepth` cap.

## Conceptual model — one surface, one trust boundary

The key reframe: **predefined vs. ad-hoc is not two mechanisms — it is two ends
of one spawn call, separated only by a trust boundary.**

```
spawn_agent { agent: "cluster", goal: "<plan>" }              // reference: pre-wired, trusted
spawn_agent { agent: "cluster", goal, model: "small" }        // reference + operator-allowed override
spawn_agent { name, system_prompt, tools, goal, model }       // ad-hoc: parent invents everything
```

- **Predefined** = an operator-curated **allowlist**. The spec pre-wires the
  risky surface — which MCP servers, which skills, which content `root`, which
  tool grants, which model. The parent supplies only a goal/plan (± a whitelisted
  override). This is the trust boundary a locked-down daemon needs: the platform
  agent can delegate to `cluster`/`triage`, but cannot invent a subagent wired to
  arbitrary MCP servers or dangerous tools.
- **Ad-hoc** = parent-authored, bounded only by the spawner's gate. Useful for
  interactive/dev exploration where a human is driving; a **liability** for an
  unattended daemon.

Everything else (sync vs. async, model, budgets) is a **parameter of the spawn**,
not a separate concept.

### Sync vs. async is a flag, not a tool

| | Call | Result delivery |
|---|---|---|
| **async** (default) | `spawn_agent { agent, goal }` | pushed as `[Background reports]` on a later parent turn |
| **sync** | `spawn_agent { agent, goal, wait: true }` | returned inline as the tool result |

`wait: true` blocks the parent turn on the subagent's completion and returns its
final text as the tool result — i.e. the current `NewSubagentTool` behavior,
reachable through the unified surface. Omitting it is fire-and-continue.

Because a synchronous spawn **holds the parent turn open**, it carries its own,
**tighter** default wall-clock cap (operator-tunable, distinct from the
fire-and-continue default) so a slow subagent can't hang the parent
indefinitely; on timeout the tool returns a **partial/timeout result** rather
than blocking. As with all budgets, the override is tighten-only (D5).

### The trust boundary: `allow_adhoc`, off by default for daemons

An operator switch (working name `subagents.allow_adhoc`, default **false** on
daemons, a single daemon-wide flag — D4) governs whether the inline/ad-hoc form
is accepted at all. When off,
`spawn_agent` **requires** an `agent:` reference to a configured spec; an inline
persona is rejected at the tool boundary. When on (the interactive/dev default),
both forms work.

**Fan-out needs no ad-hoc.** The one plausible daemon use for ad-hoc — spinning
up a team to work in parallel — is served by referencing one predefined spec N
times with different goals:

```
spawn_agent { agent: "worker", goal: "triage node pool A" }
spawn_agent { agent: "worker", goal: "triage node pool B" }
```

A curated, broad-persona `worker` spec covers fan-out with the blast radius still
pinned by the operator. Anything a fan-out instance would need beyond the spec is
a *new predefined spec*, not an ad-hoc escape hatch.

## Design details

### 1. Model resolution (both paths)

Replace the two half-answers (predefined has a full `ModelConfig` but no tier
shorthand; ad-hoc has nothing) with **one rule**:

| `model` value | Resolves to |
|---|---|
| omitted | inherit the parent's model |
| `"small"` | the configured small-tier model (ties into the existing `--small-tier-parent` notion) |
| a specific model | a named model / full `ModelConfig` |

On a **referenced** spawn the spec's `model` is the default. The *only*
overridable values are `inherit` (omit) and `"small"` — a per-spawn cost knob
(e.g. fan out this instance on the cheap tier). Overriding to a **specific**
model is deliberately **not** allowed: it requires its own predefined spec. This
removes the need for a model-override allowlist entirely and closes the
model-escalation path (D2). On an **ad-hoc** spawn the parent likewise picks only
`inherit` / `"small"`, or a specific model *if* `allow_adhoc` is on (ad-hoc is
already parent-authored, so a specific model there is no additional escalation).

### 2. Narrowing-only overrides on a referenced spawn

The safe override set, applied on top of a referenced spec:

| Field | Allowed | Rule |
|---|---|---|
| `goal` / task | always | the whole point of a spawn |
| `model` | inherit / `small` only | a *specific* model requires its own spec (D2) |
| `tools` | **subset only** | may *drop* tools the spec granted; may **never add** one it didn't |
| budgets (`max_turns`, `max_cost_usd`, `max_wallclock_seconds`) | tighten only | may lower a cap, never raise it |

Anything that would *widen* blast radius (new MCP server, new skill, a tool the
spec didn't grant, a higher budget) requires a new predefined spec. This keeps
"same `worker`, but this instance is read-only" expressible without opening an
escalation path.

### 3. Instance identity

A spec is a **template**; a spawn is an **instance**. Referencing one spec N
times must not require the model to invent a unique `name` each time (as ad-hoc
does today). The runtime auto-derives an instance id (`cluster-1`, `cluster-2`,
…) from the spec name; the parent *may* pass an explicit `name` to address a
specific instance later (for `wait`-less spawns it wants to reference). Instance
ids are what appear in `[Background reports]` and the catalog.

### 4. Result delivery: push, prune the pollers (#625)

Async results already arrive by push — `report_alert`/completion →
`[Background reports]` prepended to the parent's next turn
(`pkg/agent/background/report.go`). This is the model we keep and lean on. The
poll tools become redundant and were the loop fuel in the UAT, so **both are
removed as model tools** (D1):

- **`list_agents`** — removed; its content is served by the pre-turn push digest
  (for the model) and the operator catalog #627 (for humans).
- **`check_agent`** — removed. Push already covers fan-out coordination: the
  parent receives a `[Background reports]` line as *each* instance finishes, so
  "spawn N, synthesize when the last completes" works turn-by-turn with no
  polling. The only thing a status *pull* adds is the ability to **block** on
  progress — which is exactly what `wait: true` expresses. Removing it kills the
  loop fuel outright.
- **`stop_agent`** — stays (mutating, necessary).
- **`spawn_agent`** — stays, now the single unified surface above.

Net: the background family drops from **four model tools to two** (`spawn_agent`
+ `stop_agent`); introspection is push + operator catalog.

**Escape hatch:** if a real consumer needs *synchronous mid-turn* status of
already-running async spawns (without blocking on any one of them), add a single
consolidated read (one call returning all instances' status, optional name
filter) — not the two per-name pollers. It would be classified read-only
(`IsReadOnlyToolName`, #624) and must be non-re-issuable under auto-continue.
Deferred until a consumer demands it.

### 5. Discoverability (#627)

Two audiences, deliberately different:

- **The model** sees only the `spawn_agent` schema, whose `agent` field is an
  **enum of configured spec names** with their `Description`s — enough to route
  ("this is a triage → `cluster`") without a separate discovery tool. **No new
  model tool** (the "too many confusing tools" constraint).
  *Shipped in #640:* the roster is rendered at `Declaration()` time by a
  `rosterTool` wrapper around `spawn_agent`, not at construction — the tools are
  built before `SetSubagentTemplates` registers the declarative subagents, so a
  construction-time snapshot would miss exactly the entries that matter. Names
  and descriptions go into the tool description; the `agent` enum is applied
  only when ad-hoc spawning is **off**, since an ad-hoc spawn legitimately
  leaves `agent` empty. Both halves read the same `Manager.Catalog` the
  operator surfaces use.
- **The operator** gets a real catalog on three surfaces, all in this repo:
  - a `GET /subagents` attach endpoint (`pkg/attach`, cloning the `GET /peers`
    handler pattern) — the **contract** every client reads;
  - a **`/subagents` TUI slash command** wired through the existing
    `coretui.SlashProvider` adapter (`coreAgentAdapter.SlashCommands`/`InvokeSlash`
    in `cmd/core-agent/coretui_enabled.go`, alongside `/context`, `/usage`), with
    the remote TUI adapter (`internal/coretuiremote`) proxying the endpoint;
  - a boot / `--print-config` dump for the no-companion case.

  Each lists every configured spec (name, description, model, mcp/skills/root
  scope, invocation policy) plus a `subagent` tool-source classification.
  Operator-facing only. The **bare REPL** (`pkg/runner/repl.go`) stays out — it is
  deliberately `/exit`-only.

**Naming note (touches #626):** the TUI already registers a singular
**`/subagent`** command (aliases `/sub`) for *spawning* — currently **stubbed**
(`coretui_enabled.go:895`, "flag parser … isn't wired"). #626 should finish
wiring that spawn command onto the unified surface; #627 adds the plural
**`/subagents`** for *listing*. Keep the singular=spawn / plural=list split
explicit so the two don't collide.

## Relationship to the existing sync named-tool surface

`NewSubagentTool` registers each declarative subagent as its *own* named tool
today. Under the unified surface, sync delegation flows through
`spawn_agent { agent, wait: true }` instead.

**Decision (D3): single surface, with a one-release compatibility shim.**
`spawn_agent` becomes the one model-facing surface; sync is `wait: true`. The
per-spec named tools are **kept as deprecated aliases through v2.9** (a thin
forward to `spawn_agent { agent, wait: true }`, documented as legacy), so no
existing config — notably the live kube-platform-agent recipe, which delegates to
a named `cluster` tool — breaks on upgrade. In the same #626 train our own recipe
migrates to `spawn_agent`; removal of the named-tool aliases targets **v2.10**
with a release note. This reaches the single-surface goal without a hard break.

## Work breakdown

This doc is the keystone; the three issues execute against it:

1. **#626 (this)** — the unified `spawn_agent` surface: `agent:` reference form,
   `wait` for sync, `allow_adhoc` gate, unified model resolution, narrowing
   overrides, instance identity. Predefined specs become spawnable async;
   ad-hoc becomes policy-gated. Also finish wiring the stubbed singular
   `/subagent` TUI spawn command (`coretui_enabled.go:895`) onto this surface.
2. **#625** — fold the background tool family per §4 / D1 (remove both
   `list_agents` and `check_agent` as model tools; land on `spawn_agent` +
   `stop_agent`).
3. **#627** — the operator catalog per §5 (`subagent` tool-source, `GET /subagents`,
   the plural `/subagents` TUI slash command, boot/`--print-config` dump);
   model-facing discovery is the `spawn_agent` enum, no new model tool.

Sequencing: this design lands with (or just ahead of) the #626 code; #625 and
#627 follow as execution. Per-issue: `dev/ci/presubmits/*` green, adversarial
review gate, `-race`, no Claude attribution.

## Resolved decisions

- **D1 — Result delivery is push-only; both pollers removed.** `list_agents` and
  `check_agent` are dropped as model tools (§4). Push covers fan-out
  coordination; blocking is `wait: true`. Background family → `spawn_agent` +
  `stop_agent`. A consolidated read is a deferred escape hatch, not shipped.
- **D2 — No model-override allowlist.** Only `inherit`/`"small"` are overridable
  at spawn (§1); a specific model requires its own predefined spec. This deletes
  the allowlist and the model-escalation path.
- **D3 — Single surface with a one-release compat shim.** `spawn_agent` is the
  one surface (`wait: true` = sync); per-spec named tools remain deprecated
  aliases through v2.9, removed in v2.10; our recipe migrates in-train (§"named
  tool surface").
- **D4 — `allow_adhoc` is a single daemon-wide switch,** off by default for
  daemons, on for interactive/dev (§"trust boundary"). Per-parent/per-depth
  granularity is revisited only if a consumer needs it.
- **D5 — Sync spawns get a distinct tighter wall-clock default** (operator-tunable)
  and return a partial/timeout result rather than hanging the parent turn
  (§"sync vs async"); all budget overrides are tighten-only.

## Implementation status (#626)

Landed in the #626 train, against this design:

- **Async-by-reference for *declarative* subagents (option B).** The declarative
  builder (`cmd/core-agent/subagents.go`) now emits, alongside each
  `agent.WithSubagents` entry, a `background.SubagentTemplate` — a pre-resolved
  persona + model *factory* + toolsets (MCP + skills), including a rooted
  subagent's own content root. These are registered on the manager
  (`Manager.SetSubagentTemplates`, broken out of construction to resolve the
  builder-runs-after-manager ordering) so the *same* subagent the parent calls
  synchronously via its named tool is also spawnable async by reference through
  `spawn_agent {agent: "<name>"}`. MCP/skill toolsets are process-long-lived,
  stateless handles shared across concurrent instances; each instance still gets
  its own session, branch, and freshly-built LLM (via the factory). Both the sync
  named tool (existing) and the async reference coexist — the D3 one-release
  compat shim, no removal this increment.
- **`wait: true` (synchronous spawn).** `spawn_agent {wait: true}` blocks the
  turn on the subagent's completion and returns its completion report inline
  (`Manager.awaitResult`), capped by a distinct, tighter sync wall-clock
  (`WithSyncWaitTimeout`, default `5m` in `cmd/core-agent`, D5). On timeout or
  parent-context cancellation the subagent keeps running and its result is pushed
  on a later turn.
- **Unified model resolution + narrowing-only overrides** on both the catalog
  (`resolvePredefinedSpec`) and template (`SpawnTemplate`) reference paths:
  goal replaceable; model inherit/`small` only (a specific model needs its own
  spec, D2); budgets tighten-only. Template tool-narrowing is rejected with a
  clear error (a rooted grant spans built-ins + MCP + skills — configure a
  dedicated subagent instead).
- **`allow_adhoc` gate** wired as `!noREPL` — off for the attach-only daemon
  (`--no-repl`), on for interactive REPL and one-shot `-p` (preserves pre-#626
  ad-hoc behavior). A dedicated config key is deferred (below).
- **`/subagent` TUI spawn command** finished: singular `/subagent <name> <goal>`
  spawns a *configured* subagent by name (fire-and-continue, non-blocking in
  core-tui's synchronous Update loop) via `Manager.SpawnRef`; with no args it
  lists configured names (`Manager.ReferenceNames`). Plural `/subagents` stays
  the live-instance list (#627).

## Still open (settle in implementation)

- A dedicated **`subagents.allow_adhoc` config key** (D4). Currently the switch is
  derived (`!noREPL`); a first-class config key + `--print-config` surfacing is
  deferred until the config surface for `subagents[]` is revisited alongside
  `docs/declarative-subagents-design.md`.
- The **sync wall-clock default** is `5m` (an initial pick); revisit against real
  subagent latencies once the recipe exercises `wait: true` under GKE UAT.
- The **instance-id format** (`<name>-<n>`) is implemented; if a consumer needs
  stable/addressable ids across restarts, reconcile then.
- **Double-delivery on `wait: true`** — *resolved (#646).* `awaitResult` now takes a
  sync claim on the handle *before* it blocks, and the completion goroutine
  consumes that claim in place of pushing its alert. Exactly one of the two wins:
  the goroutine suppresses the redundant `[Background reports]` line, or a waiter
  that times out / is canceled releases the claim so the alert takes the normal
  async path. One window stays open by construction: a subagent that reaches a
  terminal state *before* the waiter claims has already passed the suppression
  check, so `claimSync` refuses and that (already-queued) alert is still delivered.
  That is the safe direction — a duplicate, never a dropped result.
