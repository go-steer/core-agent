# AX integration gap audit

Untracked sibling to `docs/attach-mode-design.md`,
`docs/bidirectional-mcp-design.md`, `docs/code-mode-design.md`. This is
a lightweight audit + punch list, not a milestone design — the goal is
to enumerate gaps in the existing `extras/ax-agent/` adapter (lives on
the `axplore` branch; see `docs/ax-plan.md`) and prioritize what closes
before we'd merge it to `main`.

## Context

AX (`github.com/google/ax`, currently a private repo) is a distributed
agent runtime: single-writer controller, durable event log, resumable
bidi gRPC streams to remote agents, and a planner that dispatches to
skills/tools/agents. The README's slogan — "AX is the layer above;
core-agent is the layer below" — is the operative constraint.
Building a parallel distributed coordinator inside core-agent is NIH
and wrong layer. `agent.RunAutonomous`, `eventlog.Stream`, and the
in-process / spawn-remote subagent seams exist because a core-agent
process must be useful when AX is *not* in front. When AX *is* in
front, core-agent should compose: AX owns conversation identity,
resumption, fan-out, audit; core-agent owns one worker's loop and its
tools.

The question is not "what should the integration do" but "how well
does the existing adapter compose, and what closes before it ships on
`main` as a first-class workload?"

## What the adapter currently handles (axplore @ `3134236`)

From `extras/ax-agent/{main,server,convert}.go` on `axplore`:

- **gRPC `AgentService`.** `Connect(stream)` + `HealthCheck`. One
  Connect == one AX execution turn. First message must be
  `AgentStart`; the adapter rebuilds `genai.Contents` from
  `start.Messages`, runs `agent.RunWithContents` (a method that exists
  only on `axplore` — see gap #1), streams ADK events back as
  `AgentOutputs`, then sends `AgentEnd`.
- **Stateless per turn.** No persistent session across Connect calls;
  AX delivers full history each turn. The agent factory builds a
  fresh `*agent.Agent` per call.
- **Content projection.** `axMessagesToGenai` /
  `genaiEventToAXOutputs` handle text, function call, function
  response. Tool calls/results get `InternalOnly: true` so the AX UI
  treats them as a side-track.
- **Lifecycle tool.** Every per-Connect agent gets
  `tools.NewLifecycleTool` (default `set_status`); handler logs to
  stderr; emit events ride the `InternalOnly` path.
- **Full tool palette.** Built-ins, MCP toolsets, skills, the
  permission gate — same wiring as `cmd/core-agent`.
- **Durable session opt-in.** `--session-db` /
  `--session-db-path` open one `eventlog.Handle` at startup and share
  it across every Connect via `agent.WithEventLog(handle)`. Default
  path `~/.<binary>/sessions.db`.
- **Vendored AX proto.** `extras/ax-agent/internal/axproto/` snapshots
  `proto/ax.proto` + `content.proto` + generated Go. Sidesteps the
  private-repo problem.
- **Multi-agent example.** `examples/ax-multi-agent/` ships a
  devil/angel demo with an `ax.yaml` registering two core-agent
  instances as separate `remote_agents`.

What it does *not* do is the substance of the rest of this doc.

## Gap-by-gap analysis

### 1. Event log projection

**Current state.** Two event logs co-exist and never meet.
Core-agent's `eventlog` (`eventlog/eventlog.go`) is an overlay on
ADK's GORM session store: monotonic `seq`, `Branch`, `Author`,
`Since(seq)` / `Watch(seq)`. AX's event log
(`ax/internal/controller/executor/eventlog.go`) is its own schema
keyed on `proto.ConversationEvent` and `proto.ExecutionEvent`.
Different types, different seq space, different durability.

With `--session-db`, every ADK event lands in core-agent's local DB —
but **AX sees only what the adapter sends as `AgentOutputs`**: text
and tool calls/results, projected through `genaiEventToAXOutputs`.
Dropped on the projection: `session.Event.Branch`, `Author`,
`Timestamp`, `Actions`, `CustomMetadata` (which is how checkpoint
events carry their typed payload — `agent/checkpoint.go:33`),
`UsageMetadata.InputTokenCount`, and the partial-vs-final distinction
on streamed text.

**Severity.** Important. Not a blocker for happy-path execution, but
AX's trace-UI view of a turn is much thinner than the local
`--session-db` view of the same turn. The two audit trails diverge.

**What "closed" looks like.** A documented projection contract:
which fields AX needs, which it tolerates losing, plus an optional
sidecar that mirrors selected metadata (token counts, checkpoint
payloads) into AX as `InternalOnly` `ToolCall` pairs under a reserved
`name` namespace — the wire already supports it (the A2A bridge does
the same trick at
`ax/internal/experimental/a2abridge/a2a_internal_state.go:25`).
Long-term: ask the AX team for a typed `metadata` field on
`ConversationEvent` so we're not abusing tool-call shape.

> `agent.RunWithContents` exists only on `axplore`
> (`agent/agent.go:343` there; absent on `main`). Promoting the
> adapter to `main` means promoting that method too — small,
> well-scoped public-API addition.

### 2. Cross-binary resume

**Current state.** Doesn't apply with the adapter as built.
Core-agent's crash-resume (`agent/resume.go`,
`agent/checkpoint.go:33-37`) keys off checkpoint events the
autonomous driver writes under `Author="<binary>/autonomous"`; the
suffix match enables cross-binary resume (`core-agent/autonomous` ↔
`scion-agent/autonomous` ↔ `ax-agent/autonomous` against the same DB).
But the adapter is stateless per turn — it does not call
`RunAutonomous` and writes no checkpoints. AX is the resumer:
`controller.tryResuming` (`ax/internal/controller/controller.go:112`)
reads `STATE_PENDING` events and replays the conversation history into
a fresh Connect call. The remote agent sees no continuity with the
prior turn beyond `AgentStart.Messages`. The adapter's local
`--session-db` accumulates disjoint per-Connect sessions, not one
resumable run.

**Severity.** Nice-to-have under the current shape; AX's resume model
covers the common case. Becomes a real issue only if a consumer wants
the worker itself to be autonomous across many turns *within* one AX
execution (e.g. a coding agent running `RunAutonomous` for ten turns
before reporting back to the planner). That pattern conflicts with
stateless-per-turn today.

**What "closed" looks like.** Either (a) document that the adapter
assumes AX-as-resumer and core-agent's own resume machinery is
inactive under AX, or (b) add a `--autonomous` mode where each
Connect drives `RunAutonomous` with a budget, checkpoints under
`ax-agent/autonomous` to the local eventlog, and survives binary
restart via the standard cross-binary suffix match. Option (b) needs
explicit design to reconcile AX's `STATE_PENDING` replay with the
per-turn checkpoint loop.

### 3. Subagent branch alignment

**Current state.** Branches stop at the gRPC boundary. Core-agent's
in-process subagents stamp `Branch="<parent>.<sub>"`
(`agent/subagent.go:200`) and `composeBranch(parent, "bg."+spec.Name)`
for background ones (`agent/background_spawn.go:76`).
`eventlog.WithBranchPrefix` lets operators query "everything under
parent.researcher" or "all bg.*". Under AX, those subagent events land
in the local `--session-db` with correct branches but reach AX only
through the parent's ADK event stream — and `genaiEventToAXOutputs`
does not consult `ev.Branch` at all. AX cannot tell a subagent
tool-call from a parent one. The three-layer composition (AX
controller → core-agent worker → in-process subagent) flattens to two
on AX's wire. AX's own `execID` chaining
(`newExecID(parent, child)`, `ax/internal/controller/executor/executor.go:36`)
is for AX-side sub-executions, not for things inside one remote
agent.

**Severity.** Important, but only for consumers who actually nest.
The axplore example (devil + angel) sidesteps it by registering each
role as its own AX `remote_agent` so AX sees them as peers. For
in-process subagents hidden from the planner ("researcher delegates
to specialist"), the flattening is invisible-state and hard to debug.

**What "closed" looks like.** Adapter convention: outputs from a
non-parent branch get an `InternalOnly` prelude `TextContent` with a
JSON marker (`{"branch":"<parent>.researcher"}`), same trick the A2A
bridge uses. Plus the recommendation: if you want AX-visible
subagents, register them as separate AX remote agents. In-process
subagents under AX are for "the planner doesn't need to know."

### 4. Skills as AX skills

**Current state.** Two skill systems, no bridge.

- Core-agent loads `.agents/skills/<name>/SKILL.md` bundles
  (Claude-compatible format) via ADK's `skilltoolset`. Each becomes a
  tool the worker's model can call.
- AX has its own first-class skill concept
  (`ax/internal/skills/skills.go`) — also reads SKILL.md, also
  supports a `scripts/` subdirectory. The AX planner dispatches to
  skills as actors. `ax.yaml`'s planner block has a `skills_dir` key.

Both implementations parse the same `agentskills.io` format. They do
not share code or registry. When core-agent runs under AX, its skills
are invisible to AX's planner — they're tools inside the worker. AX's
skills are invisible to core-agent — they live behind the controller.

**Severity.** Nice-to-have. SKILL.md is a portable on-disk format,
so the same bundle directory could be pointed at both. Real cost:
consumers can't write "this skill should be available to both worker
and planner" once.

**What "closed" looks like.** Document the two-registry reality in
the adapter README. Long-term: a shared `SkillBundle` type, or a
convention where `--skills-dir` on the adapter and `skills_dir` in
`ax.yaml` can safely point at the same directory. No core-agent code
change required today.

### 5. Lifecycle tool ↔ controller events

**Current state.** Best part of the integration. Already working.

The adapter registers `tools.NewLifecycleTool` unconditionally
(`extras/ax-agent/main.go` around the `set_status` block) and the
existing `genaiEventToAXOutputs` projection flags the call + ack
as `InternalOnly: true`. The AX trace UI sees them as a status track;
the user-facing conversation transcript stays clean. The handler also
logs each emit to stderr. This is the pattern other gaps should
emulate.

**Severity.** Closed-ish. Two minor follow-ups: (a) `AllowedStates`
is empty by default, so the system instruction is the only constraint
on vocabulary — expose `--allowed-states` if AX grows a taxonomy;
(b) AX has no built-in "blocked waiting for X" concept, but
`historyutil.WaitsForConfirmation` (`ax/internal/controller/executor/executor.go:63`)
hints at one. If AX formalizes it, the adapter should map
`state="ask_user"` to a `ConfirmationContent` instead of a generic
status emit.

### 6. Tool palette + permission gate

**Current state.** Tools live inside the worker; AX sees opaque
function calls. Core-agent's built-ins (`tools/builtins.go`) and MCP
toolsets all route through `tools.GateToolset`. AX's model (README
architecture diagram) is that **tools are MCP servers** the
controller talks to directly — there's no current spec for "the
remote agent owns its own tools." So: the AX planner cannot enumerate
the worker's catalog or route "use `bash`" to a specific worker; AX
has no policy hook on individual worker tool calls. The bash
denylist, path-scope checks, and ask/allow/yolo modes work correctly
inside the worker but are invisible to AX. AX's `ConfirmationContent`
approval is a planner-side pattern, not a worker-tool gate.

**Severity.** Important. This is the largest architectural mismatch
in the list: AX's "tools are MCP servers" and core-agent's
"tools are in-process Go" don't align without one giving ground.

**What "closed" looks like.** Three options, in increasing
ambition: (1) status quo + docs — worker tools are private, AX policy
applies at the agent-call boundary; (2) tool-catalog sidecar — the
adapter exposes a list endpoint (or MCP-shaped server) on a second
port so the planner gets introspection without gating calls (cheap;
probably the right next step); (3) per-call policy hook — adapter
calls back into AX before each invocation. Option 3 needs AX-side
protocol changes and cross-cuts the single-writer model.

### 7. Streaming and Watch semantics

**Current state.** Push model works for normal traffic; disconnect
semantics are AX's problem. The adapter pushes one
`AgentMessage_Outputs` per ADK event (`extras/ax-agent/server.go`
Connect loop) and does not use `eventlog.Stream.Watch(seq)` — it
doesn't need to, since ADK's event channel is already the source of
truth during a turn (`Watch` is for external observers like
attach-mode). AX's resumable-stream story is in flux — the proto
carries a `WARNING: There will be significant changes to this
protocol as we are solidifying resumption on the wire`. On disconnect
the adapter does nothing: `stream.Context()` cancels,
`RunWithContents` errors with `context.Canceled`, the goroutine
returns. No event survives past whatever the controller's
`outputCapturer` already wrote
(`ax/internal/controller/controller.go:233`).

**Severity.** Important, gated on AX. Until AX's resumable-stream
protocol stabilizes, the adapter cannot meaningfully participate in
mid-turn resume. The right move is to be ready to opt in when the
protocol settles.

**What "closed" looks like.** When AX defines a `seq` on
`AgentMessage` (or some equivalent token), the adapter can
checkpoint its position in the ADK iterator and re-attach. Today the
local `eventlog.Stream` already carries the seq we'd need — the
plumbing exists; only the AX-side handshake doesn't.

### 8. Cost / usage rollup

**Current state.** Both layers track independently; neither sees the
other. Core-agent's `usage.Tracker` (`usage/tracker.go:46-108`)
accumulates `InputTokens` / `OutputTokens` / `CostUSD` per agent
instance. AX has no usage concept in the proto (`grep "token\|cost"
proto/*.proto` is empty); the controller doesn't roll up. AX can't
answer "how many tokens did this conversation cost across all the
workers it touched." Core-agent's roadmap already defers subagent
cost rollup; the AX gap is the same problem one layer up.

**Severity.** Important for production billing; blocker for any
multi-worker deployment that wants per-conversation cost ceilings.

**What "closed" looks like.** Adapter emits a per-turn usage payload
as an `InternalOnly` `ToolCall` (reserved `name`, e.g.
`__usage_rollup`) at turn-end. An AX-side aggregator (built later or
in the consumer) sums them by `conversation_id`. No AX-protocol
change required today; ask the AX team whether a typed usage field on
`ConversationEvent` is on the roadmap — that would be the clean
landing spot.

## Prioritized punch list

Ranked by leverage (impact ÷ cost-to-close).

1. **Promote `agent.RunWithContents` to `main`.** Tiny, well-scoped
   library addition (~15 lines on `axplore`, see `agent/agent.go:343`
   there). Unblocks the adapter ever landing. Zero design risk.
2. **Document the projection contract and the two-event-log reality.**
   Adapter README + a section in `docs/DESIGN.md`. No code; closes
   most of gap #1 by setting expectations. Important because the
   alternative is users assuming `--session-db` and `ax serve`'s log
   are the same source of truth.
3. **Branch-prelude marker for subagent outputs (gap #3).** Small
   conversion-layer change; reuses the A2A-bridge trick; gives AX
   trace UI enough structure to render three-layer composition.
4. **Per-turn usage rollup as InternalOnly ToolCall (gap #8).** Small
   adapter change; consumer-driven aggregation. Doesn't move the
   AX-side roadmap but stops the bleed for production.
5. **Tool catalog sidecar (gap #6, option 2).** Medium adapter
   change. Useful for the planner; doesn't require AX changes.
6. **`--autonomous` mode for the adapter (gap #2).** Larger lift,
   needs explicit design about AX-replay-vs-our-checkpoint
   reconciliation. Defer until a consumer wants it.
7. **Lifecycle ↔ confirmation mapping (gap #5 follow-up).** Trivial
   when AX grows a real blocked-on-confirmation lifecycle.
8. **Resumable mid-turn streaming (gap #7).** Gated entirely on the
   AX-side protocol settling. Watch and wait.

## Constraints from AX side

Things core-agent cannot close on its own:

- **Typed metadata on `ConversationEvent`.** Without it, every piece
  of structured worker-side state (branch, author, usage, checkpoint
  payload) has to be tunneled through `InternalOnly` content
  abuse. Functional but ugly. Needs an AX-team ask.
- **Mid-turn resume protocol.** The proto file's own `WARNING` admits
  this is in flux. Until there's a stable handshake, adapter-side
  resume is speculative.
- **Per-tool-call policy hook.** A controller-side approval gate on
  individual worker tool calls cuts across AX's single-writer model.
  Would need design work on the AX side; almost certainly out of
  scope for v1 of AX.
- **Skill registry unification.** Both projects parse the same
  on-disk SKILL.md format, so the constraint here is just naming
  convention, not protocol. Could be closed by either side
  unilaterally.
- **A usage / cost type.** AX has none today. Same shape as the
  metadata ask; could ride on the same proto change.

## Recommendation

The `axplore` branch is closer to mergeable than its name suggests.
The adapter is small (~330 LOC `main.go`, ~95 `server.go`, ~180
`convert.go`, plus tests), the conversion is honest about what it
drops, the lifecycle integration is clean, and the durable-session
wiring matches the other adapters.

**Merge gates, in order:**

1. AX goes public (`github.com/google/ax` resolvable without a PAT).
   Until then the vendored `internal/axproto/` is honest but doesn't
   belong on `main` long-term.
2. AX's resumable-stream `WARNING` is removed or breaking changes
   start following a deprecation. Otherwise `main` inherits a moving
   target.
3. `agent.RunWithContents` lands on `main` with a real test.
4. Adapter README documents the projection contract (gap #1) and the
   stateless-per-turn assumption (gap #2).

**Should *not* gate the merge:** gaps 3, 5 (follow-up), 6, 7, 8.
Tractable as follow-up PRs. The current shape is enough for the
devil/angel demo and any consumer who treats AX as the orchestrator
and core-agent as the worker.

## Open questions

Things this audit can't answer without AX-team input or
post-private visibility:

- Planned typed `metadata` field on `ConversationEvent` /
  `ExecutionEvent`? If yes, gaps #1, #3, #8 consolidate.
- Will `AgentMessage` grow a `seq` field for mid-turn resume?
  `tryResuming` keys off `ConversationEvent.Seq`, which is
  controller-internal — the adapter can't address it alone.
- Tool model: stays strictly "tools are MCP servers" or grows a
  "remote agent owns its own tools" concept? Determines whether
  gap #6 option 2 is forever-correct or forever-workaround.
- Does the AX team want a skill-registry bridge, or keep the two
  separate? Drives whether gap #4 is docs-only or has a code path.
- After the slated AX rewrite, does `AgentService` survive? If not,
  the adapter is provisional regardless of the eight gaps.
