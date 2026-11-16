# Subagent coordination: fan-out with join, and steering

**Status:** proposed, 2026-11-16. Nothing here is built. This is the design of
record for the "subagent coordination" capability. The sibling capability, the
builtin roster, is [`subagent-roster-design.md`](./subagent-roster-design.md).

## Motivation

`core-agent` can run subagents in parallel. What it cannot do is coordinate
them deterministically:

- **Fan-out depends on the model.** Today a parent fans out by emitting several
  `spawn_agent` calls in one response (`examples/parallel-spawn`,
  `examples/gke-parallel-triage`). Gemini does not batch: #952 measured a mean
  batch size of 1.00 on open-ended search, unchanged by prompt work. A fan-out
  that relies on parallel function calling is serial on Gemini.
- **There is no join.** A fire-and-continue child's result arrives as a
  `[Background reports]` line on some later parent turn. Nothing tells the
  parent that the fan-out it launched is complete. `gke-parallel-triage`'s
  persona says to synthesize "when the operator asks … (or after a few
  minutes)".
- **`wait: true` fan-out is serial (see below).**
- **Nothing can talk to a running child.** Once spawned, a child receives no
  further input from anyone. Its return contract says so in as many words:
  "nobody will ask you a follow-up question"
  (`pkg/agent/subagent_contract.go:45-47`).

The comparison that sharpened this is `kube-agents` as of 2026-11-16
(origin/main `966c303c0`):

- **On by default:** its A2A gateway over NATS and its `AgentProfile` CRD (one
  pod per task).
- **Steering:** a running worker can take a follow-up message, and the gateway
  holds each delegation to its chain and scope.
- **Fan-out is not shipped.** Its A2A payload spec accepts a batch width of one
  until fan-out's results and stop handling are built
  (`docs/designs/spec-a2a-payloads.md`). Its `spec-subagent-profiles.md` lists
  fan-in as a gap: "the orchestrating agent sequences its own fan-out and
  feeds results forward itself".

Deterministic fan-out with a join is a place `core-agent` can lead. Steering is
parity.

## What exists today (facts this design builds on)

Verified against `0cda1ec7`, ADK v2.6.0.

- **`wait: true` spawns in one response run one after another.**
  - ADK runs a response's function calls on concurrent goroutines
    (`adk/v2@v2.6.0/internal/llminternal/base_flow.go:1342`).
  - But `agent.New` wraps every tool in `tools.SerializeMutating` on one
    per-agent lock (`pkg/agent/agent.go:1051-1062`, #460).
  - `spawn_agent` is classified mutating (`pkg/tools/serialize.go:50-53`), and
    `serializedTool.Run` holds the lock for the whole call
    (`serialize.go:203-211`), *including* `awaitResult`.
  - So N waiting spawns run serially, each up to the sync-wait cap: 5 minutes
    by default (`cmd/core-agent/subagents.go:56`), overridable with
    `--subagent-sync-wait` or `tools.spawn_agent.sync_wait_timeout`, where `0s`
    means no cap.
  - Fire-and-continue spawns hold the lock only for the launch, so their
    children do run in parallel.
  - No test pins either behaviour for `spawn_agent`. The sync
    declarative-subagent door is serialized on purpose
    (`TestConcurrentSyncSubagentDelegationsDoNotOverlap`,
    `pkg/agent/subagent_serialize_test.go`).
- **The lock is also doing safety work today, by accident.** Under the default
  `safety.parallel_subagent_writes: refuse`, two waiting *writer* spawns in one
  response both succeed: the first holds the lock until its child finishes, so
  the second launches when no writer is running. The lock also keeps the
  parent's own same-response `bash` or `edit_file` from overlapping a waiting
  writer child. The #653 guard covers only background against background
  (`pkg/agent/background/parallelwrites.go:33-57`).
- **The concurrency cap is a hard refusal, not a queue.** `maxConcurrent: 8` and
  `defaultMaxDepth = 2` are set in `pkg/agent/background/manager.go:837,43`.
  The CLI never overrides them, and no config key exposes them (#1220). Spawn
  number nine is refused with `ErrTooManyConcurrent` (`spawn.go:310`,
  `templates.go:460`). The count covers every running handle in the tree,
  grandchildren and remote children included (`manager.go:1275-1283`).
- **Results flow up through one alert channel per manager.**
  - `pushAlert` (`manager.go:1020-1047`) feeds a 256-entry channel that drops
    the oldest entry when full, then wakes the parent.
  - `PrependPendingAlerts` (`report.go:96-117`) drains it at the start of the
    parent's next turn.
  - The manager has one parent (`AttachParent` is last-writer-wins,
    `manager.go:980-985`), one `OnAlert` slot, and a documented single drainer
    (`manager.go:1000-1007`).
  - A child's `spawn_agent` is rebound to the *same* manager
    (`templates.go:163-174`), and the child has no manager of its own
    (`spawn.go:839-879`). So **a grandchild's report goes to the root parent,
    not to the child that spawned it**.
- **No API sends into a running child.** `Manager` and `Handle` expose no
  `Inject`. The child agent is built inside `newSpawnAgentBuilder` and not
  retained (`spawn.go:878`). It runs under `autonomous.Run` inside a recover
  (`spawn.go:510-516`). The operator's `/subagent` only spawns
  (`cmd/core-agent/coretui_enabled.go:1438-1473`). Attach's `/inject` targets
  the parent only.
- **`send_message` and `list_agents` are names, not tools.** They are rows in
  `pkg/permissions/mutating.go:163,198` and `internal/effects`, carried over
  from mast's orchestration vocabulary with the #1400 port.
  - `list_agents` and `check_agent` were real tools until #625 removed them.
  - The unified-invocation design deferred "a single consolidated read (one
    call returning all instances' status, optional name filter)" until a
    consumer needed it
    ([`archive/unified-subagent-invocation-design.md`](./archive/unified-subagent-invocation-design.md),
    lines 212-216).
- **A remote (A2A) subagent uses the same door, but almost nothing crosses the
  wire.**
  - `spawn_agent {agent: X}` on a template with `remote:` takes `launchRemote`
    (`templates.go:359-366`). Caps and `wait` apply.
  - On the wire (`a2a_client.go:411-431`) the request carries the goal text,
    plus a system prompt when one is set and the skill id.
  - `core_agent.memory_scope` metadata is added only with
    `propagate_memory_scope` (`a2a_config.go:106`), and no receiver code reads
    it yet.
  - The spawn's wall-clock cap is used locally as the A2A timeout
    (`a2a_client.go:391-394`). Turn and cost caps are not sent.
  - A remote that enters `input-required` is cancelled
    (`a2a_stream.go:243-245`, `a2a_client.go:721`).
  - On the receiving side, `compose`'s A2A backend runs one agent turn per A2A
    message (`pkg/compose/a2a.go:265-310`).

## Settled decisions (do not relitigate)

### D1: The layers split in-process from distributed

`core-agent` owns coordination **inside one process**: fan-out, join,
steering, report routing. It does **not** grow a message bus, a task registry
or a cross-process scheduler. Those are the distributed-runtime layer, which is
AX on Agent Substrate in `shipmind`, and NATS in `kube-agents`.

The bridge between the layers is the remote subagent that already exists. A
fan-out entry that names a `remote:` template is dispatched over A2A exactly
like an in-process one. In `shipmind` that makes each child a sandboxed AX task
that suspends when idle, which is `kube-agents`' pod-per-task model plus
suspend and wake. Every decision below has to work unchanged when an entry is
remote, or has to say explicitly that it is in-process only.

### D2: Waiting releases the mutation lock only for children that cannot write the parent's tree

A waiting `spawn_agent` (and a waiting `spawn_agents`, D4) releases the
parent's mutation lock after launch and before it blocks, **only when the
child cannot write the parent's working tree**. That means one of two things:

- the child is not a shared-filesystem writer by the existing classification
  (`spawnWritesSharedFilesystem`, `parallelwrites.go:184`), so it holds
  only read-only tools and no MCP toolset;
- or the worktree policy gave the child its own tree.

A writer child sharing the parent's tree keeps today's behaviour: the lock is
held through the wait. That preserves both accidental protections described
above. Read-only fan-out, which is what research and triage fan-outs are, stops
being serial.

**Mechanism:** a release hook carried in the call's context. The serializer
puts a `tools.ReleaseMutationLock(ctx)` function into the context it hands
`Run`. The spawn tool calls it once the launch has succeeded and the child is
known not to write the shared tree. The serializer's deferred unlock becomes a
no-op after a release. **Rejected:** making `spawn_agent` report itself
read-only through `ReadOnlyHinter`. That would reclassify it everywhere: a
`read_only` subagent could list it, `wait_and_verify` could poll it (and
re-spawn on every poll), and the plan gate would exempt it.

The sync declarative-subagent door stays serialized. It is out of scope here.

### D3: Fan-out is one tool call, by reference only

A new tool, `spawn_agents`, takes a list of tasks and launches them as one
**batch**:

```json
{
  "tasks": [
    { "agent": "cluster", "goal": "check checkout-svc in prod" },
    { "agent": "cluster", "goal": "check cart-svc in prod" },
    { "agent": "network", "goal": "trace DNS failures from cart-svc" }
  ],
  "wait": true
}
```

- **One call, not N.** The batch does not depend on whether the model batches
  function calls, which is the Gemini problem above.
- **By reference only.** Each task names a configured subagent (`agent`), and
  may set an instance `name` and the narrowing-only overrides `spawn_agent`
  already allows. No `system_prompt`. "Fan-out needs no ad-hoc" is already the
  principle in the unified-invocation design. It also means a batch works on a
  `--no-repl` daemon, where ad-hoc spawns are off.
- **Heterogeneous tasks are allowed.** One spec × N inputs (per cluster, per
  service) and N specs × one incident (network + cache) are the same call.
- **`spawn_agent` stays.** A single delegation keeps its existing, simpler
  schema.
- **`spawn_agents` is part of the spawn-tool family.** That has five
  consequences:
  - `NewSpawnTools` registers it;
  - `rebindSpawnTools` (`templates.go:192-235`) rebinds it per session, so a
    per-session child never reaches the daemon's manager;
  - the #748 carve-out drops it from inherited tool sets;
  - it is plan-gated like `spawn_agent` (`registerPlanGated`);
  - it is classed `ClassSpawning` in `mutating.go` and `internal/effects`.

  The same applies to D6's tools.

### D4: Join semantics

- **`wait: true`** blocks the call until every task in the batch reaches a
  terminal state, or the sync-wait cap expires.
  - The result is one array in task order. Each entry has the same shape as a
    `spawn_agent` result: name, status, `output`, `final_text`,
    `stop_reason`, `error`.
  - On timeout, finished entries are returned inline, unfinished ones report
    `running`, and their results are pushed later as ordinary
    `[Background reports]` lines. That is the existing `wait` timeout contract,
    applied per entry.
  - D2 governs the lock: it is released only if every task in the batch is a
    non-writer or isolated.
- **`wait: false`** (the default) launches and returns at once with the instance
  names. Each child's terminal report is tagged with its batch and its position
  (`batch b3 · 4 of 6 done`), and the **last** one says the batch is complete.
  The parent can tell when a fan-out has finished without counting.
- **Admission is all-or-nothing; execution is not.** If admission refuses the
  batch (D5), nothing launches and the call returns the reason. Once a batch is
  admitted, one child failing does not cancel its siblings.

### D5: Admission, queueing and caps

- **Admission:**
  - The gate is consulted once per distinct `agent` named in the batch, so an
    `ask`-mode operator answers one prompt per subagent kind, not one per task.
  - The batch must fit under a configurable size ceiling (default 32).
  - The manager must have at least one free slot.
  - Any failure refuses the whole batch.
- **Queueing:** a batch runs at most `max_parallel` tasks at once. That is a
  per-call argument, defaulting to the manager's free capacity at admission
  (at least 1). The next task starts as each one finishes. A fleet-shaped
  fan-out of 30 clusters therefore works under a cap of 8.
- **A queued task re-runs every launch check when its turn comes:** the
  concurrency cap, the parallel-write guard, the pricing check
  (`spawn.go:251`). It does not re-prompt the gate; admission covered that. A
  check that fails at launch fails *that entry*, with the reason, and the batch
  continues. There is no retry.
- **Individual `spawn_agent` calls keep today's hard refusal at the cap.**
- **The caps become configuration (#1220).** `max_concurrent` and `max_depth`
  get config keys and flags, with today's 8 and 2 as defaults.
- **Budgets stay per child.** Each child keeps its own turn, cost and
  wall-clock caps. A batch does not pool budget; that deferral from the
  background-subagents design stands.

### D6: Steering is parent → child messages through the child's inbox

- **`send_message {to, text}`** queues `text` in a running child's inbox.
  - The child sees it at the start of its next turn, and a child sleeping
    between turns is woken.
  - It never pre-empts a turn in flight.
  - **Mechanism:** `newSpawnAgentBuilder` retains the child's `*agent.Agent` on
    its `Handle`, and `Manager.Send` calls `InjectAs` on it. The child keeps
    running under `autonomous.Run` inside today's recover. Switching to
    `autonomous.Start` was rejected: its goroutine has no recover, so a child
    panic would take down the daemon.
- **Who may send.** The agent that spawned the child, and the operator (a new
  attach route, `POST .../agents/{name}/inject`). Both go through
  `Manager.Send`, so the model tool and the operator cannot drift apart. The
  gate key is `send_message:<name>`. The row in `mutating.go` already classes it
  mutating.
- **A finished, stopped, remote or unknown child is an error** naming its
  status, never a silent drop. Remote steering is out of scope.
- **The child's contract follows the build.** The return contract's "nobody
  will ask you a follow-up question" stays for a child that `Manager.Send`
  cannot reach. For a child it can reach, the contract instead says follow-up
  messages may arrive between turns. That covers a spawner with `send_message`,
  or a session with the attach inject route mounted. This is a branch on what
  is true about the build, as AGENTS.md requires.
- **`list_agents` ships as the deferred consolidated read.**
  - One call returns every instance the caller spawned (name, status, batch,
    last report), with an optional name filter.
  - It is added to the read-only table in `pkg/tools/serialize.go` and to
    `wait_and_verify`'s `neverPollable`, so it cannot become the poll loop #625
    removed.
  - The consumer it was deferred for is now here: a parent that steers has to
    know what is still running.
- **Every new tool is covered by a `ModelFacingBans` sweep,** including
  conditional registration (AGENTS.md, #919).

### D7: Reports route to the nearest live spawner

A child that spawns grandchildren receives their reports, not the root.

- **Routing:** the shared manager gives each spawning agent its own
  per-lineage **view**. A view has its own alert queue, its own drainer and its
  own wake target. The single-drainer contract therefore holds per view, and
  the root's `AttachParent` and `OnAlert` are untouched. The cap and the depth
  count stay tree-wide.
- **Fallback:** if the spawner has already finished (all three roster
  builtins are `task` mode and stop when done), its view is closed, and the
  report goes to the nearest live ancestor, ultimately the root. A report is
  never dropped because its spawner ended.

A child that fans out is the natural place to join its own fan-out, and today
it can only do that with `wait: true`. This changes behaviour for nested trees,
so it ships with a `#### Bug or Regression` CHANGELOG bullet that names the old
routing. #1310 (addressing nested subagents by dotted path) is adjacent but
separate.

### D8: Limits travel across the A2A hop, tighten-only

When a batch or a spawn targets a `remote:` template, the child's turn and
cost caps travel in A2A message metadata (`core_agent.budgets`).

- **Receiver:** a receiving `core-agent` (`a2a.expose`) applies them to the
  turn that A2A task runs, as ceilings it can only tighten. The turn cap maps
  to that turn's step ceiling and the cost cap to its cost ceiling, scoped to
  the task, never to the session.
- **Non-`core-agent` remotes** ignore unknown metadata. That is the honest
  floor, and the doc says so rather than implying enforcement.
- **Wall-clock** keeps working as it does now: as the A2A timeout.

This extends the in-process narrowing rule to the distributed case. It is what
`kube-agents`' gateway scope checks do on their side. It also gives the
receiving side its first reader of `core_agent.*` metadata.

## Out of scope

- **A message bus, task registry or cross-process scheduler (D1).** If a
  `shipmind` scenario needs one, it goes to AX.
- **Sibling-to-sibling messaging.** D6 is parent → child (and operator →
  child). A child asking a *sibling* needs addressing, permission and loop
  semantics that no scenario has demanded yet. A child that needs to tell its
  parent something mid-run already has `report_alert`.
- **A child asking its parent a question and blocking on the answer.** The
  background-subagents design deferred a cross-agent permission-request
  message type. That deferral stands.
- **Dependency graphs between tasks** (task B starts when A finishes, fan-in
  cards). The model sequences batches. `kube-agents` lists the same gap, and
  mast retired `dispatch: graph`.
- **Budget pooling** across a batch or against the parent (D5).
- **Persisting the alert queue or batch records across restarts.** Both stay
  in memory.
- **Parallelizing the sync declarative-subagent door,** or releasing the lock
  for writer children (D2).
- **Steering a remote child.** The A2A client cancels `input-required` today.
  Follow-up messages on an A2A task are a later design.
- **Declaring sync vs async invocation on the spec (#867).** Orthogonal to the
  join.
- **The builtin roster.** It belongs to
  [`subagent-roster-design.md`](./subagent-roster-design.md).

## Implementation phases

- **P1: unblock what exists.**
  - **D2, as a `fix:`.** The regression test has two read-only children
    spawned with `wait: true` in one response and asserts they overlap; it
    fails on today's code. A second test pins that two writer children still
    serialize under `refuse`.
  - **#1220:** config keys for the caps (D5).
  - **Fix two `gke-parallel-triage` claims** that this doc's investigation
    found by reading. Neither was confirmed by a run, so verify each before
    changing it:
    - its persona says leaving `tools` unset inherits the parent's MCP
      toolset, but the ad-hoc path resolves only the names it is given;
    - its README states "5 turns / 60 s" defaults, while the manager's are 50
      turns / $1 / 10m.
- **P2: fan-out with join.**
  - `spawn_agents` (D3), the join semantics (D4), admission, queueing and the
    size ceiling (D5), and batch-tagged reports.
  - Migrate `gke-parallel-triage` to a by-reference batch, which also makes its
    `--no-repl` path work.
  - Add a hermetic example under `examples/`, listed in
    `examples/internal/smokeset`, plus site and CHANGELOG updates.
- **P3: steering.**
  - `Manager.Send`, `send_message` and `list_agents` (D6).
  - The attach inject route and the contract-text branch.
  - Per-lineage report routing with live-ancestor fallback (D7).
- **P4: across the hop.**
  - Budget metadata and receiver-side tighten-only enforcement (D8).
  - Write up the remote-template → AX-task mapping in `shipmind`.

## Open questions

1. **Does a batch survive the parent's turn ending under `wait: false`?** Today
   a fire-and-continue child outlives the turn that spawned it, and a batch
   should too. Its queued (not yet started) tasks, though, need an owner when
   the parent's session closes. The likely answer is that they are cancelled
   and reported as such. Settle in P2.
2. **Does the session cost ceiling preflight a batch?** N children × a
   per-child cost cap can exceed what is left of the session ceiling before
   anything launches. Refusing up front is honest, but it may refuse batches
   that would have finished cheaply. P2 needs a decision informed by how the
   existing cost preflight (`pkg/agent/cost_preflight.go`) counts subagent
   spend.
3. **`list_agents` scope in a multi-session daemon.** "Every instance the
   caller spawned" is clear for one session. Whether an admin caller can see
   across sessions is a multi-session tenancy question (#1355), not a
   coordination one.
