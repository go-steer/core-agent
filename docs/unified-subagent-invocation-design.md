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

### The trust boundary: `allow_adhoc`, off by default for daemons

An operator switch (working name `subagents.allow_adhoc`, default **false** on
daemons) governs whether the inline/ad-hoc form is accepted at all. When off,
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

On a **referenced** spawn the spec's `model` is the default; an override to a
*different specific* model is honored only if that model is in an operator
allowlist (open question OQ2). On an **ad-hoc** spawn the parent picks from the
same allowlist. `"small"` is always permitted (it's operator-configured by
definition).

### 2. Narrowing-only overrides on a referenced spawn

The safe override set, applied on top of a referenced spec:

| Field | Allowed | Rule |
|---|---|---|
| `goal` / task | always | the whole point of a spawn |
| `model` | if permitted | inherit / `small` / allowlisted specific |
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
poll tools become redundant and were the loop fuel in the UAT:

- **`list_agents`** — fold into the operator catalog (#627) + the pre-turn push
  digest; **remove as a model tool.**
- **`check_agent`** — a pull *does* have a legitimate niche (the parent
  explicitly wants a status before deciding). Keep at most **one** narrow status
  read, or replace it with an on-completion push and remove it too — settle in
  OQ1. Whatever remains stays classified read-only (`IsReadOnlyToolName`, #624)
  but must not be re-issuable in an auto-continue loop.
- **`stop_agent`** — stays (mutating, necessary).
- **`spawn_agent`** — stays, now the single unified surface above.

Net: the background family goes from four model tools toward **two** (`spawn_agent`
+ `stop_agent`), with introspection served by push + the operator catalog.

### 5. Discoverability (#627)

Two audiences, deliberately different:

- **The model** sees only the `spawn_agent` schema, whose `agent` field is an
  **enum of configured spec names** with their `Description`s — enough to route
  ("this is a triage → `cluster`") without a separate discovery tool. **No new
  model tool** (the "too many confusing tools" constraint).
- **The operator** gets a real catalog: a `subagent` tool-source classification,
  a `GET /subagents` attach endpoint, and a boot / `--print-config` dump listing
  each configured spec (name, description, model, mcp/skills/root scope,
  invocation policy). Operator-facing only.

## Relationship to the existing sync named-tool surface

`NewSubagentTool` registers each declarative subagent as its *own* named tool
today. Under the unified surface, sync delegation flows through
`spawn_agent { agent, wait: true }` instead. Two migration options (OQ3):

- **(a) Deprecate the per-spec named tools** — one surface, cleanest for the
  "single surface" goal; the parent addresses subagents through `spawn_agent`'s
  `agent` enum. Breaking for any config relying on the named tool.
- **(b) Keep per-spec named tools as sync sugar** — they remain as a thin alias
  for `spawn_agent { agent, wait: true }`; async is additive. Non-breaking, but
  two ways to do the sync call.

Lean: **(a)** for the stated single-surface goal, with a release note; revisit if
a consumer depends on the named-tool shape.

## Work breakdown

This doc is the keystone; the three issues execute against it:

1. **#626 (this)** — the unified `spawn_agent` surface: `agent:` reference form,
   `wait` for sync, `allow_adhoc` gate, unified model resolution, narrowing
   overrides, instance identity. Predefined specs become spawnable async;
   ad-hoc becomes policy-gated.
2. **#625** — fold the background tool family per §4 (remove `list_agents` as a
   model tool; resolve `check_agent` per OQ1; land on `spawn_agent` + `stop_agent`).
3. **#627** — the operator catalog per §5 (`subagent` tool-source, `GET /subagents`,
   boot/`--print-config` dump); model-facing discovery is the `spawn_agent` enum.

Sequencing: this design lands with (or just ahead of) the #626 code; #625 and
#627 follow as execution. Per-issue: `dev/ci/presubmits/*` green, adversarial
review gate, `-race`, no Claude attribution.

## Open questions

1. **`check_agent` fate** — keep one narrow status-pull, or go push-only for
   completion and remove it? (Loop-safety argues push-only; a parent that spawns
   async and later needs a mid-flight status argues keep-one.)
2. **Model-override allowlist** — where declared (top-level `subagents.models`?
   per-spec `allow_model_override`?), and does `"small"` bypass it (proposed:
   yes).
3. **Named-tool migration** — deprecate per-spec named tools (a) vs. keep as sync
   sugar (b).
4. **`allow_adhoc` granularity** — a single daemon-wide switch, or per-parent /
   per-depth? (Start daemon-wide; revisit if a consumer needs finer control.)
5. **`wait: true` and budgets** — a synchronous spawn blocks the parent turn; does
   the subagent's wall-clock cap need a distinct (tighter) default from the
   fire-and-continue case to avoid a parent turn hanging on a slow subagent?
