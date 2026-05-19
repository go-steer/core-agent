# Dynamic background subagents + remote subagents

Design doc for the v1.2.0 milestone. Untracked sibling to
`docs/cogo-core-agent-integration.md` and `docs/docsy-migration-notes.md`.

## Context

Two related capabilities for monitoring use cases (canonical example: a
single main agent watching N Kubernetes clusters in parallel, reacting
when any of them flags an issue):

1. **Dynamic in-process background subagents.** The main agent's model
   decides at runtime to spawn a long-running subagent — providing its
   system prompt, goal, tool/skill list, and budgets — without the
   developer pre-registering it. Subagent runs in a goroutine inside
   the same process; its events flow into the parent's eventlog
   branch; "alert" reports get pushed back to the parent's main loop
   and injected into the next turn.

2. **Out-of-process / remote subagents.** Same model-facing shape,
   but the actual agent runs elsewhere — gRPC call to a remote agent
   server, K8s Job, Cloud Run job, NATS-dispatched worker, etc. The
   consumer plugs in a `RemoteAgentSpawner` implementation; core-agent
   stays agnostic about transport. Mirrors the existing
   `tools.NewAskUserTool` + `Prompter` consumer-pluggability pattern.

This is the analog of cogo's `scion_message` but in-process for case 1,
and consumer-substrate-agnostic for case 2. We already have all the
substrate (`agent.RunAutonomous`, `eventlog`, `branchInjectingService`,
`Prompter` pattern); this is mostly wiring + one new tool family + a
small manager type.

### Settled design decisions

- **Tool/skill inheritance (catalog discovery):** Spawn tool's JSON
  schema is an enum of builtin tool names + open `extras: []string`
  for MCP/skills/custom. Permission gate re-validates everything
  regardless.
- **Push, not pull, for alerts.** Channel-based; main loop drains
  pre-turn and injects as user-role messages.
- **Configurable depth cap.** Reuse the existing `subagentDepthKey{}`
  context-value pattern from `agent/subagent.go:91-102`.
- **Remote spawning** added in the same release as a small additive
  surface (~150-200 LOC).
- **Permission inheritance: subagent inherits the spawner's gate
  wholesale.** Same `*permissions.Gate` instance; same allow/deny
  lists; same mode; same prompter. The whole subagent tree
  effectively shares one gate. With four targeted mitigations to
  make this safe in practice (see Permissions below). Bounded-subset
  grants with parent-as-arbiter is deferred to v1.3+.
- **Fresh `model.LLM` per spawn.** Each subagent calls
  `provider.Model(ctx, modelID)` to get its own client. Sidesteps any
  concurrent-streaming safety questions in the genai/anthropic SDKs.
  Underlying HTTP transport and auth handles are reused via SDK
  internal caches, so the per-spawn cost is small.

## Architecture

```
┌──────────────────────────────────────────────────────────────────────┐
│ Parent agent loop (REPL / Headless / RunAutonomous)                  │
│  ┌────────────────────────────────────────────────────────────────┐  │
│  │ Pre-turn: drain alert channel → prepend alerts as user content │  │
│  └─────────────────────────────────────────▲──────────────────────┘  │
└────────────────────────────────────────────┼─────────────────────────┘
                                             │ (push)
                  ┌──────────────────────────┴──────────────────────┐
                  │ agent.BackgroundAgentManager                    │
                  │  - registry: name → handle (lifecycle, channel) │
                  │  - alert channel (multiplexed from all handles) │
                  │  - depth tracking (context value, like sync)    │
                  └─────┬─────────────────────┬─────────────────────┘
                        │                     │
              ┌─────────▼──────────┐   ┌──────▼─────────────────────┐
              │ spawn_agent tool   │   │ spawn_remote_agent tool    │
              │ (in-process)       │   │ (consumer-pluggable)       │
              │  → goroutine +     │   │  → RemoteAgentSpawner.     │
              │    RunAutonomous + │   │    Spawn(spec) → Handle    │
              │    branchSvc       │   │    with Events chan        │
              └────────────────────┘   └────────────────────────────┘
```

## Permissions

The subagent inherits the spawner's `*permissions.Gate` by reference.
Session-level approvals (`allow-once`, `allow-session`,
`allow-session-tool`, `allow-always`) persist across the whole tree,
and the config-file allow/deny lists apply uniformly. Simple to reason
about. Four mitigations keep this from biting in practice.

### 1. Serialized prompter access

When multiple background subagents simultaneously hit a tool call that
needs approval in `ask` mode, they'd race for `os.Stdin` via the
inherited `StdinPrompter` and either deadlock or interleave garbage.
Fix: wrap the inherited prompter in a `serializingPrompter` that holds
a mutex across each `AskApproval` call.

```go
// permissions/serialize.go (new)
type serializingPrompter struct {
    inner Prompter
    mu    sync.Mutex
}
func (p *serializingPrompter) AskApproval(ctx context.Context, req PromptRequest) (Decision, error) {
    p.mu.Lock(); defer p.mu.Unlock()
    return p.inner.AskApproval(ctx, req)
}
func Serialize(p Prompter) Prompter { ... }
```

The `BackgroundAgentManager` wraps the parent's gate prompter with
`Serialize(...)` once at construction. Subagents waiting their turn
block on the mutex; ctx cancellation unwinds cleanly.

### 2. Source attribution in prompts

A v1.1.0 prompt today reads:

```
core-agent (permissions): bash wants to run:
  kubectl get pods
```

The human can't tell which agent is asking when subagents are in play.
Fix: add an optional `Source` field to `permissions.PromptRequest` and
surface it in `StdinPrompter`'s heading:

```
core-agent (permissions): [watch-prod-cluster] bash wants to run:
  kubectl get pods
```

The gate sets `Source` from a context value the spawn machinery
stamps on each subagent's context. Empty when the parent itself is
asking (today's behavior, no visual regression).

API touch: add `Source string` to `permissions.PromptRequest`.
Additive; no consumer breakage. Custom prompters that ignore it still
work.

### 3. Allow-state scoping by session row

`Gate.rememberSession` keys allow-state by `(toolName, key)` today.
Since subagents already use a derived session row
(`<parent>:sub:<branch>`), they should get their own scope
automatically IF the gate keys allow-state by session ID. Verify; if
not, the fix is a one-line key change in `permissions/gate.go`.

Intent: a `t` ("session-tool") decision approved while a subagent is
running scopes to that subagent's session, not the parent's. Sibling
subagents don't get the grant. Test this explicitly.

### 4. Max-concurrent-subagents cap

`BackgroundAgentManager` enforces a configurable `MaxConcurrent`
(default 8). `spawn_agent` calls that would exceed it return a clean
tool-result error the model can adapt to ("would exceed
`max_concurrent=8` background agents; stop one before spawning
another"). Defense in depth on top of per-subagent cost/turn
budgets.

### Remote subagents and permissions

`agent.NewSpawnRemoteAgentTool` does NOT extend the parent's
permission gate into the remote subagent. Once the request crosses
the process boundary, whatever the consumer's `RemoteAgentSpawner`
does is what gets done — auth/IAM/policy is the consumer's
responsibility in their substrate. The spawn tool call itself goes
through the parent's gate normally (so `spawn_remote_agent` can be
denylisted if needed).

## Phased delivery (single tag at the end: v1.2.0)

### Phase 1 — `agent.BackgroundAgentManager` + in-process spawn tool

**New: `agent/background.go`** — the core primitive. Per-parent-agent
singleton:

```go
type BackgroundAgentManager struct {
    parent       *Agent
    provider     models.Provider
    gate         *permissions.Gate
    availableTools    map[string]tool.Tool
    availableToolsets []tool.Toolset
    maxDepth     int
    defaultBudgets BackgroundBudgets

    mu      sync.Mutex
    agents  map[string]*BackgroundHandle
    alerts  chan Alert
}

type BackgroundHandle struct {
    Name        string
    Branch      string
    StartedAt   time.Time
    Status      BackgroundStatus // Running / Completed / Failed / Stopped
    Result      *RunResult
    cancel      context.CancelFunc
    done        chan struct{}
}

type Alert struct {
    From      string
    Text      string
    Timestamp time.Time
    Kind      string // "alert" | "info" | future
}

type BackgroundBudgets struct {
    MaxTurns       int
    MaxCost        float64
    MaxWallclock   time.Duration
    PerTurnTimeout time.Duration
}
```

Constructor `NewBackgroundAgentManager(parent *Agent, opts ...BackgroundManagerOption)`
returns the manager and registers the manager pointer on the parent
so other tools can find it. Options include
`WithBackgroundMaxConcurrent(n int)` (default 8).

Lifecycle: `Spawn(ctx, spec) (*BackgroundHandle, error)`,
`List() []*BackgroundHandle`, `Get(name) *BackgroundHandle`,
`Stop(name) error`, `Alerts() <-chan Alert`, `Close()`.

`Spawn` flow:

1. Validate name unique, within depth cap
   (`CurrentSubagentDepth(ctx) >= maxDepth` → error), and within
   `MaxConcurrent`.
2. Resolve `spec.Tools` against the catalog
   (`availableTools` + `availableToolsets`). Unknown names → error
   result.
3. Construct a fresh `model.LLM` via `m.provider.Model(...)`.
4. Wrap parent's `session.Service` with `branchInjectingService` under
   branch `<parent>.bg.<name>`.
5. Build the spawned `*Agent` via `agent.New(...)`.
6. Inject `report_alert` and `report_completed` tools.
7. Increment `subagentDepthKey{}` in the goroutine's ctx; stamp a
   `subagentSourceKey{}` value with the subagent's name so the gate's
   `Source` attribution works.
8. Spawn goroutine running `agent.RunAutonomous(ctx, build, spec.Goal, withBudgets...)`.
   The Agent inherits the parent's `*permissions.Gate` (same
   instance).
9. Goroutine completion writes terminal status to handle; pushes a
   synthetic "completed" Alert.

**New: `agent.NewSpawnAgentTool(mgr) tool.Tool`** — wraps Spawn as a
model-callable tool with schema:

```jsonc
{
  "name": "spawn_agent",
  "parameters": {
    "name": "string",
    "system_prompt": "string",
    "goal": "string",
    "tools": "[]string (enum: read_file, write_file, edit_file, list_dir, glob, grep, bash, todo, report_alert, report_completed)",
    "extras": "[]string (optional: MCP/skill names)",
    "max_turns": "int (optional, default 50)",
    "max_cost_usd": "number (optional, default $1.00)",
    "max_wallclock_seconds": "int (optional, default 600)"
  }
}
```

**New companion tools:** `NewListAgentsTool(mgr)`,
`NewCheckAgentTool(mgr)`, `NewStopAgentTool(mgr)` for the model to
introspect its spawned subagents.

**Reused without changes:**
- `branchInjectingService` (`agent/subagent.go:288`)
- `composeBranch` (`agent/subagent.go:246`)
- `deriveSubagentSessionID` (`agent/subagent.go:351`)
- `subagentDepthKey{}` (`agent/subagent.go:91-102`)
- `agent.RunAutonomous` + budget options (`agent/autonomous.go:49`)
- `models.Provider.Model` (`models/provider.go:41-49`)

### Phase 2 — Alert push: `report_alert` tool + pre-turn injection

**New: `NewReportAlertTool(mgr, fromName)`** — injected into every
spawned subagent. When called:

1. Writes a `session.Event` with `Author=fromName+"/alert"`,
   `Content.Parts=[{Text: text}]` (audit trail). Empty `Content.Role`
   so it doesn't poison conversation history (same trick as
   `gemini.GroundingProjection`).
2. Pushes an `Alert{From, Text, Timestamp, Kind: "alert"}` onto
   `mgr.alerts` (non-blocking; if full, drops oldest with a logged
   warning).

Similarly `NewReportCompletedTool(mgr, fromName)` for "I've finished"
— also auto-fires from the goroutine wrapper when `RunAutonomous`
returns.

**Pre-turn injection:** Modify `agent.Agent.Run` to consult the
manager before each turn:

```go
func (a *Agent) Run(ctx context.Context, prompt string) iter.Seq2[*session.Event, error] {
    if a.bgMgr != nil {
        prompt = a.bgMgr.PrependPendingAlerts(prompt)
    }
    // ... existing flow
}
```

`PrependPendingAlerts` drains all pending alerts (non-blocking) and
formats:

```
[Background reports]
- [watch-prod-cluster] pod app-7 restarted 5 times in 60s
- [watch-staging-cluster] (completed) goal achieved: no issues in 30m window

---
<original user prompt>
```

For interactive REPL, the runner also displays alerts inline as they
arrive via a goroutine listening on `Alerts()` and writing to the
info stream with the `↪` sigil under a `bg/` author prefix — reuses
the v1.1.0 magenta-↪ pattern (`runner/events.go:96-110`).

For `RunAutonomous`, alerts get drained + prepended to the
continuation prompt for each iteration.

For headless `-p PROMPT`: alerts ONLY land in the eventlog (no next
turn). Document this clearly.

### Phase 3 — Remote subagent (consumer-pluggable spawner)

**New: `agent/remote.go`** — consumer interface mirroring the
`Prompter` pattern:

```go
type RemoteAgentSpawner interface {
    Spawn(ctx context.Context, spec RemoteAgentSpec) (RemoteAgentHandle, error)
}

type RemoteAgentSpec struct {
    Name         string
    SystemPrompt string
    Goal         string
    Tools        []string
    Extras       []string
    Budgets      BackgroundBudgets
}

type RemoteAgentHandle interface {
    ID() string
    Status(ctx context.Context) (RemoteAgentStatus, error)
    Stop(ctx context.Context) error
    Events() <-chan RemoteAgentEvent
}

type RemoteAgentEvent struct {
    Kind      string // "alert", "log", "completed", "failed", "stopped"
    Text      string
    Timestamp time.Time
}
```

**New: `agent.NewSpawnRemoteAgentTool(spawner, mgr)`** — schema
identical to `spawn_agent` (the model doesn't care if it's in-process
or remote). When invoked:

1. Calls `spawner.Spawn(ctx, spec)`.
2. Wraps the returned handle.
3. Registers in `mgr.agents` so `list_agents`/`check_agent`/`stop_agent`
   work uniformly.
4. Starts a goroutine draining `handle.Events()` → `Alert` push.

Consumer wires it like ask_user:

```go
spawner := myK8sJobSpawner{kubeconfig: ...}
remoteTool, _ := agent.NewSpawnRemoteAgentTool(spawner, mgr)
agent.New(m, agent.WithTools([]tool.Tool{remoteTool, ...}))
```

`agent.RefuseRemoteAgentSpawner(reason)` for the
headless/unconfigured case (analog of `tools.RefusePrompter`).

### Phase 4 — CLI wiring + docs

`cmd/core-agent/main.go` adds manager construction after agent build,
plus a `--no-background-agents` flag (default: enabled). The remote
spawner is opt-in — consumers wire it programmatically; the bundled
CLI doesn't ship one.

The runner gains a small REPL extension to display alerts inline as
they arrive (`↪` magenta).

Docs:
- `CHANGELOG.md` — `[1.2.0]` entry.
- `README.md` — feature bullet.
- `docs/site/content/docs/library-api.md` — new "Dynamic background
  subagents" section; extend the existing "Subagents" section to
  contrast static `WithSubagents` vs dynamic `spawn_agent`.
- `docs/site/content/docs/configuration.md` — note about
  `--no-background-agents`.
- `docs/site/content/docs/permissions.md` — subagent inheritance +
  serialized prompter notes.
- `examples/background-monitor/` — minimal example showing parent
  agent spawning two K8s cluster monitors and reacting to their
  alerts.

## Critical files

**New:**
- `agent/background.go`
- `agent/background_test.go`
- `agent/background_tools.go`
- `agent/background_tools_test.go`
- `agent/remote.go`
- `agent/remote_test.go`
- `permissions/serialize.go`
- `permissions/serialize_test.go`
- `examples/background-monitor/main.go`

**Modified:**
- `agent/agent.go` — `bgMgr` field, `PrependPendingAlerts` call in
  `Run`, `WithBackgroundManager` option
- `agent/autonomous.go` — drain alerts before each continuation prompt
- `permissions/prompter.go` — add `Source string` to `PromptRequest`
- `permissions/stdin.go` — render `Source` in heading
- `permissions/gate.go` — stamp `Source` from subagent context value
- `runner/events.go` / `runner/repl.go` — inline alert display
- `cmd/core-agent/main.go` — wire manager, `--no-background-agents`,
  register spawn-related tools
- `CHANGELOG.md`, `README.md`, three site docs pages

## Verification

```bash
cd /home/user/projects/core-agent

# Unit tests, by phase
go test ./agent/... -run TestBackgroundAgentManager
go test ./agent/... -run TestReportAlert
go test ./agent/... -run TestPrependPendingAlerts
go test ./agent/... -run TestRemoteAgent

# Full
go vet ./... && go test ./...
for s in dev/ci/presubmits/*; do bash "$s"; done

# Real-LLM smoke for Phase 1+2:
source /home/user/scripts/gemini.sh && unset GEMINI_API_KEY GOOGLE_API_KEY
go build -o /tmp/core-agent ./cmd/core-agent
/tmp/core-agent --provider=vertex --yolo --session-db -p "
  Spawn two background subagents to count to 3, named 'counter-a' and
  'counter-b', each with system prompt 'count from 1 to 3 then call
  report_alert with the result then call done'. Wait for both to alert
  back, then tell me what they reported.
"
# Expect: ↪ counter-a alert + ↪ counter-b alert, then main agent
# reports both back.

# Real-LLM smoke for Phase 3 with a stub remote spawner:
go run ./examples/background-monitor
```

## Deferred (out of scope for v1.2.0)

- **Bounded permission subsets + parent-as-arbiter.** v1.2.0 inherits
  the spawner's gate wholesale. The richer model — where the spawner
  grants the subagent only a *subset* of its own permissions, and any
  out-of-subset request from the subagent gets routed to the spawner's
  *model* (not the human) for a decision via an injected synthetic
  prompt — is a v1.3+ feature. Worth doing eventually: it lets a
  parent confine a subagent to read-only operations while still being
  able to authorize specific exceptions case-by-case. Requires
  per-subagent gate construction, a cross-agent permission-request
  message type, and a "respond to permission request" tool on the
  parent side.
- **Persistence across main-agent restarts.** Background subagent
  state is process-local. If the parent process dies, in-process
  subagents die with it. Cross-restart resume would require the
  BackgroundAgentManager to persist its registry to eventlog and
  reconstruct on `ResumeAutonomous`. Defer.
- **Subagent → subagent communication.** A subagent can only
  `report_alert` to its parent (the manager owns the channel).
  Cross-tree messaging isn't supported in v1.2.0.
- **Pull mode for alerts.** Push is the only path. Channel-full
  backpressure (drop-oldest) is the safety valve if push proves
  disruptive.
- **Budget pooling.** Each subagent has its own budget; no
  cross-sibling or against-parent summation. Add a pool-level cap
  later if runaway spawns become a real cost concern.
- **Authority for remote spawning beyond the consumer's choice.**
  Whatever the consumer's `RemoteAgentSpawner` does is what gets done
  — no auth/IAM layer at the core-agent boundary. Consumers handle
  that in their spawner implementation.
- **Recursion deeper than the depth cap.** Cap is enforced; no
  "request elevation" mechanism.
