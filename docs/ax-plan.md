# core-agent ↔ AX integration (`axplore` branch)

## Recommendation summary

Ship a new `extras/ax-agent/` binary that mirrors the `extras/scion-agent/` pattern: a thin adapter that exposes a single core-agent instance as an AX **remote agent** by implementing `github.com/google/ax/proto.AgentService` (the native "axp" protocol — gRPC bidi stream). Each AX execution arrives as one `AgentStart` carrying the full conversation history; the adapter rebuilds a fresh genai.Contents slice from those messages, runs `agent.Run`, streams text/tool-call events back as `AgentOutputs`, then sends `AgentEnd`. Stateless per turn — no persistent session, full history on every call. **No changes to core-agent's library API are required.**

For multi-agent conversations, AX's existing model handles this: register multiple core-agent instances as separate `remote_agents` in `ax.yaml`; the AX Gemini planner converts each into a callable tool and orchestrates the conversation. We don't need to teach core-agent to talk A2A or to call other agents — AX is the broker. We document the multi-agent ax.yaml shape with a worked example.

A2A protocol is **not** the right surface for us — AX's A2A bridge is for non-AX-native agents (Python ADK, LangGraph). Since we control core-agent and want first-class behavior (streaming, tool round-trip), axp is the right choice.

## Context

The user has a sibling project `/home/user/projects/ax` (Agent eXecutor) — a distributed agent runtime with a controller, event log, and remote-agent registry. They want to:

1. Run multiple core-agent instances as remote AX agents (analogous to the existing `extras/scion-agent/` packaging for Scion's container runtime).
2. Have those agents communicate with each other within a conversation.

AX exposes two integration paths:
- **axp** (native): implement `proto.AgentService` (gRPC bidi `Connect` + `HealthCheck`). This is what `examples/remote_agent/main.go` shows.
- **a2a**: third-party agents that already speak the [A2A protocol](https://github.com/a2aproject/A2A) plug in through AX's experimental bridge.

For multi-agent conversations, AX's `internal/gemini/gemini_planner.go:516-560` converts every registered remote agent into a Gemini function declaration. The planner picks which agent to call; the picked agent runs as a sub-execution; its outputs come back to the planner as a `ToolResultContent`. Conversation history is shared across agents via the event log (`internal/controller/executor/eventlog.go`), so resuming `ax exec --conversation X --agent B` after a prior `--agent A` run sees A's prior messages.

Architectural observation: AX's "planner calls remote agents as tools" is the exact same shape as our planned `tools.NewSubagentTool` (`docs/subagents-plan.md`) — just with AX/gRPC as the transport instead of an in-process call. That's a clean fit for our project's mental model.

## Design decisions

| Decision | Choice | Why |
|---|---|---|
| Protocol | **axp** (native gRPC `AgentService`) | We control core-agent; we want first-class behavior (streaming, tool round-trip preserved through ax). A2A would force a JSON-RPC translation layer for no gain. AX's A2A bridge is for agents we *don't* control. |
| Adapter location | `extras/ax-agent/` | Mirrors `extras/scion-agent/` exactly. Optional integration; doesn't bloat the bundled `cmd/core-agent`. |
| Library changes to core-agent | **None.** Adapter wraps existing `agent.Run` | The adapter is a thin protocol shim. All the work (model resolution, tools, MCP, skills, instruction loading) reuses what `cmd/core-agent` and `extras/scion-agent` already wire. Our existing CLI codepath is the template. |
| Per-turn state | **Stateless.** Rebuild `genai.Contents` from `AgentStart.Messages` each turn; fresh `agent.Agent` per call | AX delivers full history in every `AgentStart` (`proto/ax.proto:27-31`). Maintaining a server-side session would conflict with AX's resumability model and break correctness when the controller resumes a stalled execution from the event log. Stateless is the supported shape. |
| Streaming back to AX | One `AgentOutputs` per yielded ADK event (text chunk, tool call, tool result) | AX accepts 0+ `AgentOutputs` before `AgentEnd`. Mapping each ADK event to one wire message gives the controller live streaming and matches the "Outputs" plural-shape semantic. |
| `AgentEnd` timing | Send when ADK iterator drains *or* when context is cancelled | Stream lifecycle is one turn. After `AgentEnd`, the controller closes the stream. Context cancellation must trigger a clean `AgentEnd` (or a gRPC error) so the controller doesn't hang. |
| HealthCheck shape | Simple liveness — return `{Healthy: true}` | The adapter doesn't have a deep meaning for "healthy"; we're not load-balancing. Match the example. |
| TLS / auth | Insecure listener by default; document how to wrap with TLS via `grpc.ServerOption` | AX's reference (`examples/remote_agent/main.go`) is insecure. Production deployments add TLS at the gRPC layer; that's caller policy, not our concern. |
| Configuration source | `.agents/config.json` continues to work; add `--listen` flag for the gRPC port | Keep the same `loadConfig` path as `cmd/core-agent` and `extras/scion-agent`. Only thing new is where to bind. |
| Multi-agent communication | **Don't add new code.** Document AX's existing planner-as-orchestrator pattern with a worked `ax.yaml` example | AX already handles this via the Gemini planner. Each core-agent instance registers as a separate `remote_agents` entry. The planner picks who to call; conversation history flows through the event log. No core-agent change required. |
| Testing | Reuse `models/mock` (echo + scripted) for the adapter's tests so we don't need real LLM creds in CI | Same trick we used in the mock + recording PR — plug an echo provider into core-agent and exercise the gRPC adapter end-to-end. The scripted provider lets us pin agent-side behavior. |
| `internal_only` flag on Message | Set on tool-call / tool-result messages we emit | `proto.Message` has an `internal_only` flag (presumably to hide intermediate steps from the user-facing event log render). Tool calls and results are agent-internal; only the final assistant text should be `internal_only: false`. Verify against the existing `examples/remote_agent` behavior at impl time. |

## Files

### New
- `extras/ax-agent/main.go` — entry point. Mirrors `extras/scion-agent/main.go`'s structure: flag parsing, config load, model resolve, gate setup, instruction load, MCP/skills load, gRPC server bind. Final step is `proto.RegisterAgentServiceServer(grpcServer, &server{...})` instead of running a REPL.
- `extras/ax-agent/server.go` — implements `proto.AgentServiceServer`: `Connect(stream)` and `HealthCheck`.
- `extras/ax-agent/convert.go` — bidirectional mapping between AX's `proto.Message` / `proto.Content` and genai.Content. Specifically: `axMessagesToGenai([]*proto.Message) []*genai.Content` and `genaiEventToAXOutputs(*event.Event) *proto.AgentOutputs`.
- `extras/ax-agent/main_test.go` — end-to-end test using `models/mock` echo + a stubbed in-process gRPC client. Drives an `AgentStart` through `Connect`, asserts `AgentOutputs` and `AgentEnd` come back in order with the right text.
- `extras/ax-agent/convert_test.go` — table tests for the conversion layer (text, tool call, tool result, multi-part messages, role mapping).
- `extras/ax-agent/README.md` — quickstart + multi-agent ax.yaml example. (Mirror `extras/scion-agent/README.md`.)
- `examples/ax-multi-agent/ax.yaml` — worked example of two core-agent instances + planner config that demonstrates inter-agent conversation.

### Modified
- `go.mod` — add `github.com/google/ax` as a dependency. (Likely a `replace` directive to the local `../ax` checkout in dev; module-resolved in CI.)
- `extras/scion-agent/README.md` — add a "See also" link to `extras/ax-agent/` once it lands.
- `docs/DESIGN.md` — new short section under "Optional Scion adapter" or similar, titled "Optional AX adapter," covering: why axp not a2a, why stateless-per-turn, why no core-agent library changes. Points to `extras/ax-agent/` and the multi-agent example.
- `README.md` — append AX to the deployment surfaces list (currently mentions Scion). One-line bullet.
- `docs/site/content/docs/providers.md` — no change (AX is not a model provider; it's a runtime).
- `docs/site/content/docs/library-api.md` — no change (the adapter is a binary, not a library API).
- `docs/site/content/docs/` — new `ax-adapter.md` page covering the integration shape, ax.yaml registration, multi-agent worked example.

## Implementation

### 1. `extras/ax-agent/main.go` — adapter binary

Most of the wiring is identical to `extras/scion-agent/main.go`. The differences start after the gate, instruction, MCP, and skills are set up:

```go
func run(cfgPath, modelOverride, providerOverride string, listen string, /* ... existing flags ... */) int {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    cfg, agentsDir, err := loadConfig(cfgPath, cwd)
    // ... apply flag overrides (same as scion-agent) ...

    provider, err := models.Resolve(cfg)
    m, err := provider.Model(ctx, cfg.Model.Name)
    // ... record-to wrap (same as scion-agent) ...

    gate, err := permissions.FromConfig(cfg, cwd, coreHome, nil)
    // ... instruction, MCP, skills, builtin tools (same as scion-agent) ...

    // Build agent options once; the same options are applied to every
    // per-turn agent the gRPC handler constructs.
    agentOpts := []agent.Option{
        agent.WithTools(builtinTools),
        agent.WithToolsets(allToolsets),
        agent.WithSystemInstructionPrefix(loaded.Instruction),
    }

    srv := &axServer{
        ctx:       ctx,
        model:     m,
        agentOpts: agentOpts,
        tracker:   usage.NewTracker(),
        pricing:   usage.PriceFor(cfg.Model.Name, cfg),
    }

    lis, err := net.Listen("tcp", listen)
    if err != nil { /* error */ }
    grpcServer := grpc.NewServer()
    proto.RegisterAgentServiceServer(grpcServer, srv)
    log.Printf("ax-agent listening on %s", listen)

    go func() { <-ctx.Done(); grpcServer.GracefulStop() }()
    if err := grpcServer.Serve(lis); err != nil { /* error */ }
    return runner.ExitOK
}
```

Reuse: `loadConfig`, `firstNonEmpty`, the flag set, the override application — copy from `extras/scion-agent/main.go` since the two binaries deliberately don't share helpers (per `feedback_*` memory and existing pattern in the repo).

### 2. `extras/ax-agent/server.go` — `AgentService` impl

```go
type axServer struct {
    proto.UnimplementedAgentServiceServer
    ctx       context.Context
    model     adkmodel.LLM
    agentOpts []agent.Option
    tracker   *usage.Tracker
    pricing   usage.Pricing
}

func (s *axServer) HealthCheck(_ context.Context, _ *proto.HealthCheckRequest) (*proto.HealthCheckResponse, error) {
    return &proto.HealthCheckResponse{Healthy: true, Message: "ok"}, nil
}

func (s *axServer) Connect(stream proto.AgentService_ConnectServer) error {
    // 1. Receive exactly one AgentStart.
    msg, err := stream.Recv()
    if err != nil {
        return fmt.Errorf("ax-agent: recv start: %w", err)
    }
    start := msg.GetStart()
    if start == nil {
        return fmt.Errorf("ax-agent: first message must be AgentStart, got %T", msg.GetType())
    }

    // 2. Build genai contents from the supplied history.
    contents := axMessagesToGenai(start.Messages)
    // The "current prompt" is the last user message; agent.Run takes
    // the prompt as a string. Split: extract the trailing user text
    // and feed the rest as preserved history via WithSession seeding —
    // OR just feed the entire history as the model's request and run
    // a single one-shot turn. Choose the latter for simplicity (see
    // design decision: stateless-per-turn).
    a, err := agent.New(s.model, s.agentOpts...)
    if err != nil {
        return fmt.Errorf("ax-agent: build agent: %w", err)
    }

    // 3. Stream events as AgentOutputs.
    convID := msg.ConversationId
    execID := msg.ExecId
    for event, err := range a.RunWithContents(stream.Context(), contents) {
        if err != nil {
            return fmt.Errorf("ax-agent: agent run: %w", err)
        }
        outputs := genaiEventToAXOutputs(event)
        if outputs == nil {
            continue
        }
        if err := stream.Send(&proto.AgentMessage{
            ConversationId: convID,
            ExecId:         execID,
            Type:           &proto.AgentMessage_Outputs{Outputs: outputs},
        }); err != nil {
            return fmt.Errorf("ax-agent: send outputs: %w", err)
        }
    }

    // 4. Send AgentEnd.
    return stream.Send(&proto.AgentMessage{
        ConversationId: convID,
        ExecId:         execID,
        Type:           &proto.AgentMessage_End{End: &proto.AgentEnd{}},
    })
}
```

**Caveat to verify at impl time**: `agent.Run(ctx, prompt string)` exists today; `agent.RunWithContents(ctx, []*genai.Content)` does not. Two options:

a) Add a `RunWithContents` method to `agent.Agent` that bypasses the prompt-splitting and feeds genai contents directly to ADK. ~10 lines in `agent/agent.go`. This is the cleanest path and the one library change worth making.

b) Reconstruct a string prompt from the last user message and rely on ADK's session to absorb the rest of the history. Brittle — loses tool-call history.

**Recommend (a).** Document the addition explicitly in the plan; it's a small, focused expansion of `agent.Agent`'s public API and unlocks the AX adapter cleanly. (This is the only library change in the whole plan.)

### 3. `extras/ax-agent/convert.go` — message ↔ content mapping

Two functions:

```go
// axMessagesToGenai turns AX's wire history into genai.Contents the
// ADK runner expects. Roles map: "user" → genai.RoleUser, "assistant"
// → genai.RoleModel, "model" → genai.RoleModel.
func axMessagesToGenai(msgs []*proto.Message) []*genai.Content { /* ... */ }

// genaiEventToAXOutputs converts one ADK event into an AgentOutputs
// payload, or nil when the event has no externally-meaningful content
// (e.g. partial usage updates).
func genaiEventToAXOutputs(ev *event.Event) *proto.AgentOutputs { /* ... */ }
```

The content mapping table:

| genai.Part | proto.Content variant | Notes |
|---|---|---|
| `Text` (final, `event.Partial == false`) | `TextContent{text}` | `internal_only: false` |
| `Text` (partial, `event.Partial == true`) | `TextContent{text}` | `internal_only: false`; AX accepts streaming partials |
| `FunctionCall{name, args}` | `ToolCallContent{id, FunctionCallContent{name, args}}` | `internal_only: true` |
| `FunctionResponse{id, name, response}` | `ToolResultContent{call_id, FunctionResultContent{name, response}}` | `internal_only: true` |

`args`/`response` are `map[string]any` on our side and `google.protobuf.Struct` on the wire. Use `structpb.NewStruct(m)` (already a transitive dep via genai).

### 4. Tests

**`extras/ax-agent/convert_test.go`**:
- `TestAXMessagesToGenai_TextRoundTrip` — single user message round-trips.
- `TestAXMessagesToGenai_RoleMapping` — assistant/model/user all map correctly.
- `TestAXMessagesToGenai_ToolRoundTrip` — tool_call + tool_result preserved across the boundary.
- `TestGenaiEventToAXOutputs_Text` — partial text event becomes a `TextContent` payload.
- `TestGenaiEventToAXOutputs_FunctionCall` — function call becomes `ToolCallContent` with `internal_only: true`.

**`extras/ax-agent/main_test.go`** — end-to-end without real LLM:
- `TestConnect_EchoEndToEnd` — start an in-process gRPC server with the echo provider, dial it, send `AgentStart{Messages: [{user, "ping"}]}`, drain the bidi stream, assert exactly: 1+ `AgentOutputs` containing text "ping", 1 `AgentEnd`.
- `TestConnect_RejectsNonStartFirst` — send an `AgentOutputs` first; assert the server returns an error.
- `TestHealthCheck` — `Healthy: true`.

The in-process gRPC server pattern (`grpc.NewServer()` + `bufconn.Listener`) is the canonical way to test gRPC services without binding a real port.

### 5. `examples/ax-multi-agent/ax.yaml` — worked multi-agent example

```yaml
server:
  address: ":8494"

eventlog:
  sqlite:
    filename: "eventlog/log.sqlite"

planner:
  gemini:
    model: "gemini-3-flash-preview"

registry:
  remote_agents:
    - id: "researcher"
      name: "Researcher"
      description: "A research agent that reads files and grep code. Use for any 'look up X in the codebase' question."
      address: "localhost:50051"
      protocol: "axp"
    - id: "coder"
      name: "Coder"
      description: "A code-writing agent. Use after the researcher has gathered context, to actually write or edit files."
      address: "localhost:50052"
      protocol: "axp"
```

A README in `examples/ax-multi-agent/` explains:
- Run two `core-agent ax-agent` processes on different ports
- Start `ax serve` with this config
- Run `ax exec --input "find every TODO and write a tracking doc"` — the planner sends the research task to `researcher`, sees its output, then forwards a write task to `coder`. Conversation history flows through the event log; both agents see the prior messages on subsequent turns.

### 6. The `agent.RunWithContents` addition

Only library change in the plan. Add to `agent/agent.go`:

```go
// RunWithContents drives one agent turn from a pre-built conversation
// history (genai contents) instead of a single prompt string. The
// returned iterator yields the same Event sequence as Run.
//
// Use when integrating with a runtime (e.g. AX) that supplies the
// full conversation history per turn rather than a session-managed
// prompt.
func (a *Agent) RunWithContents(ctx context.Context, contents []*genai.Content) iter.Seq2[*adkevent.Event, error] {
    // Implementation: feed contents into the inner ADK runner directly,
    // bypassing the prompt-string convenience of Run.
}
```

The implementation requires looking at ADK Go's `runner.Runner` to confirm it accepts a contents-shaped input. If it doesn't, this becomes a slightly larger lift (build an LLMRequest manually and call the LLM). To verify at impl time.

## Branching + vendoring strategy

> **Update (2026-05):** AX is now public at [`github.com/google/ax`](https://github.com/google/ax) and the module proxy resolves it. The adapter de-vendored — `internal/axproto/` was removed and the adapter now imports `github.com/google/ax/proto` directly. The "vendoring" content in this section is retained as historical context for why the snapshot existed in the first place; only Exit Condition #1 was actually triggered. The branch still lives off `main` because AX is slated for a rewrite (Exit Condition #2 has not yet fired).

---

`github.com/google/ax` is currently a private GitHub repo, which breaks vanilla `go mod download` in CI. AX is also slated for a rewrite, so anything we ship today is provisional. Combined: this work doesn't belong on `main` yet.

**Plan:** ship the AX adapter on a dedicated long-lived branch (`axplore`), with `vendor/github.com/google/ax/` checked in. Vendoring sidesteps the private-module auth problem entirely (no GOPRIVATE wiring, no PATs, no secrets), keeps CI green out of the box, and gives anyone who wants to try the integration a single `git checkout <branch>` step.

**Why a branch and not main:**
- The dep is private, the API is unstable (rewrite incoming), and there's no concrete consumer driving it onto `main` yet.
- Keeping it off `main` means our public CI / govulncheck / golangci-lint / release artifacts stay clean.
- A branch is reversible — when ax is public + stable, we either rebase the branch onto main or rewrite the integration against the new ax surface.

**Branch contents:**
- Everything in the "Files" section above.
- `vendor/` directory containing `github.com/google/ax/...` and any of its transitive deps not already in our `go.sum`.
- `go.mod` declares `github.com/google/ax v0.0.0-<commit-sha>` pinned to the vendored revision.
- `go.work` *not* needed (single-module repo); a `replace` directive isn't needed either when vendoring is committed.
- A note in `extras/ax-agent/README.md` explaining: "this branch vendors a private snapshot of ax; refresh with `dev/tools/refresh-ax-vendor` (a small script we ship in the same branch) when bumping."

**Maintenance cost (priced honestly):**
- **Re-vendor on every ax bump.** Mitigated by a one-shot script (`go mod vendor` + a curated copy from `../ax`).
- **`vendor/` bloats the diff.** Acceptable for a feature branch; reviewers know to ignore it.
- **`go mod tidy` becomes lossy.** Need to either set `GOFLAGS=-mod=vendor` in CI or accept that `tidy` may try to re-fetch from the proxy. Recommend the GOFLAGS approach in the branch's CI.

**Local dev path:** developers can either work off the vendored snapshot (no extra setup) OR add `replace github.com/google/ax => ../ax` to their local `go.mod` for quick iteration against unreleased ax changes. Don't commit the `replace`.

**Exit conditions for the branch:**
1. ax goes public → drop `vendor/`, normal `go.mod`, merge to main (or rebase).
2. ax is rewritten → rewrite the adapter against the new surface, treat the branch as throwaway prior art.
3. A consumer concretely commits to using this → revisit the merge-to-main decision; either accept the vendoring as ongoing or push for ax going public.

## Verification

```bash
cd /home/user/projects/core-agent
go test ./extras/ax-agent/...
go vet ./...
go build ./extras/ax-agent

# End-to-end smoke (no real LLM needed):
./ax-agent --provider=echo --listen=:50051 &
AX_PID=$!

# In another shell, with AX checked out alongside:
cd ../ax
cat > /tmp/ax-smoke.yaml <<EOF
server: { address: ":8494" }
eventlog: { sqlite: { filename: "/tmp/ax-eventlog.sqlite" } }
planner: { gemini: { model: "gemini-3-flash-preview" } }
registry:
  remote_agents:
    - id: "core"
      address: "localhost:50051"
      protocol: "axp"
EOF

ax serve --config /tmp/ax-smoke.yaml &
ax exec --input "ping" --agent core
# Expected: model response "ping" (echo provider returns the user's last message)

kill $AX_PID
```

For the multi-agent example, run two `ax-agent` instances on `:50051` and `:50052`, point the planner at both, and confirm the planner routes between them across a single conversation.

## Out of scope (defer until asked)

- **Calling other AX agents from inside core-agent.** The natural shape would be a `tools.NewAXAgentTool(addr string)` that wraps a remote AX agent as a callable tool from a parent core-agent. Useful for "core-agent acting as an AX-aware planner," but AX's own planner already does this. Defer until a consumer wants nested AX-call-AX from inside a core-agent loop.
- **A2A protocol support in core-agent.** Would let third-party A2A clients drive a core-agent without going through AX's bridge. Real ask, but separate scope; the AX bridge already handles the inverse.
- **Resumption / event log integration.** AX's resumption is at the controller layer; the adapter's stateless-per-turn shape means we participate naturally without writing event-log entries ourselves. If a future use case needs to write our own ExecutionEvent records (e.g., for richer tracing of internal tool calls inside a single AX turn), add that as a follow-up.
- **AX's `internal_only` semantics deep dive.** We ship sensible defaults (tool calls/results internal, final text not) and adjust based on observed behavior. Document the choice; revisit if the AX trace UI surfaces the wrong things.
- **TLS / mutual auth.** Production deployments wrap the gRPC server with `grpc.Creds(...)`; document that hook but ship insecure-by-default to match AX's reference example.
- **Cost/usage rollup into AX's event log.** Same gap as the subagent plan — `usage.Tracker` accumulates locally; AX has its own event log. Bridging the two is non-trivial and defers until someone wants billing telemetry across remote agents.

## What this plan deliberately does NOT do

- Add A2A protocol support to core-agent (axp covers our use case)
- Change core-agent's library API beyond one new method (`RunWithContents`)
- Introduce a long-running session in the adapter (stateless per turn matches AX's model)
- Modify `cmd/core-agent` or any other binary
- Change the existing `extras/scion-agent` adapter (separate concern; gets a "see also" doc link)

## Status updates

- **Lifecycle integration shipped.** The adapter registers `tools.NewLifecycleTool` (default name `set_status`) on every agent it builds. The existing `genaiEventToAXOutputs` conversion in `extras/ax-agent/convert.go` already flags FunctionCall / FunctionResponse messages with `InternalOnly: true`, so the model's status emissions arrive on AX's wire as internal-only ToolCall + ToolResult pairs — exactly the "AX UI sees the state but the conversation history stays clean" shape the autonomous-plan called for. The handler additionally logs each emit to stderr for operator tracing. Covered by `TestConnect_LifecycleToolEmitsInternalOnly` in `extras/ax-agent/main_test.go`.
