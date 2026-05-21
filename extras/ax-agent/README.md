# ax-agent

Packages [core-agent](../../) as a remote [Agent eXecutor (AX)](https://github.com/google/ax)
agent. AX is a distributed agent runtime — a controller dials remote agents
via gRPC, fans tasks out to them, and persists conversation state in an
event log. This binary is what AX dials.

> This adapter ships only on the **`axplore` branch** while AX is in
> flux (slated for a rewrite). The repo is now public at
> [`github.com/google/ax`](https://github.com/google/ax) and the
> adapter imports the upstream proto directly; merging to `main`
> waits on AX stabilizing. See [`docs/ax-plan.md`](../../docs/ax-plan.md)
> for the rationale.

## What it does

Mirrors `cmd/core-agent`'s wiring (config, permissions, model resolution,
built-in tools, MCP, skills, instruction loading) and exposes the result as
the AX `AgentService` gRPC server (`Connect` + `HealthCheck`). Each AX
execution arrives as one `AgentStart` carrying the full conversation history;
the adapter rebuilds genai contents from those messages, runs
`agent.RunWithContents`, streams text and tool-call events back as
`AgentOutputs`, then sends `AgentEnd`.

**Stateless per turn** — no persistent session between Connect calls. AX
delivers the full conversation history on every turn, so the adapter
reconstructs context from that history each time. This matches AX's
resumability model.

**`set_status` tool** — every agent gets a bundled lifecycle tool (from
`tools.NewLifecycleTool`) the model calls to signal state ("thinking",
"blocked", "ask_user", or any custom label). Calls and acks are flagged
`InternalOnly: true` on the wire, so the AX UI / event log can render
them as a status track without polluting the user-facing conversation
transcript. The handler also logs each emit to stderr (`ax-agent:
status: <state> — <detail>`) so operators can trace lifecycle traffic
without `--record-to`. Consumers tighten the state vocabulary by
constraining it in their agent's system instruction (e.g. "only emit
states from {thinking, blocked, done}").

## Quickstart

```bash
go build -o ax-agent ./extras/ax-agent

# Single agent (no AX-side multi-agent needed):
GEMINI_API_KEY=... ./ax-agent --listen=:50051

# Then in your ax.yaml:
#   registry:
#     remote_agents:
#       - id: "core"
#         address: "localhost:50051"
#         protocol: "axp"
#
# Drive it:
ax exec --server localhost:8494 --input "summarize main.go"
```

For a multi-agent example with two opposing roles (devil's advocate +
angel's advocate), see [`../../examples/ax-multi-agent/`](../../examples/ax-multi-agent/).

## Flags

```
--listen=:50051         gRPC bind address
--c=PATH                config file path (default: discover .agents/config.json)
--m=NAME                override model name
--provider=NAME         override model.provider
--no-builtin-tools      disable the whole tool suite
--disable-tools=...     comma-separated per-tool disables
--script=PATH           JSONL transcript for --provider=scripted
--script-strict         scripted: require request shape to match recorded
--record-to=PATH        record every LLM turn to a JSONL file
--session-db            persist sessions + audit log to a durable database (default off)
--session-db-path=PATH  override the database path (default: ~/.<binary>/sessions.db)
```

When `--session-db` is set, every Connect call's events land in the durable
[eventlog](../../docs/eventlog-plan.md) — useful for audit, replay across
restarts, or feeding an external consumer that wants "everything since seq N."
The Handle is opened once at startup and shared across all Connect calls so
the same DB file holds the full cross-conversation history.

The `--script`/`--record-to` flags compose with the mock providers shipped in
[`models/mock/`](../../models/mock/), so you can record a real session against
Gemini/Anthropic and replay it offline through `ax-agent --provider=scripted`.

## Multi-agent communication

AX brokers all multi-agent traffic through its Gemini planner. Each
`core-agent` you register in `ax.yaml` becomes a tool the planner can call.
The planner picks who to invoke per turn; conversation state is persisted in
the AX event log (`eventlog.sqlite`), so re-resuming `ax exec --conversation
X` against any registered agent sees the prior cross-agent history.

There is **no direct agent-to-agent wire**. The planner is the orchestrator.
That's intentional — it keeps each agent stateless and gives the controller
one place to enforce policy.

## Caveats

- **Insecure listener by default** — wrap with `grpc.Creds(...)` or run behind
  a TLS-terminating proxy for production.
- **Single AX execution per Connect stream.** AX opens a fresh stream per
  turn; the adapter handles one `AgentStart` then closes.
- **Tool execution at replay time uses the live environment.** If you record
  a real session with `--record-to` and replay against `--provider=scripted`,
  the LLM side is faithful but `bash`/`read_file` runs against whatever the
  current filesystem looks like.
- **Imports `github.com/google/ax/proto` directly.** Previously this
  adapter vendored a snapshot under `internal/axproto/` because AX was
  a private repo; that vendor was removed once AX went public and the
  module-proxy resolution started working from CI.

## See also

- [`docs/ax-plan.md`](../../docs/ax-plan.md) — full design rationale + branching strategy (the vendoring-choice section is now historical context)
- [`extras/scion-agent/`](../scion-agent/) — analogous adapter for Scion's container runtime
- [`examples/ax-multi-agent/`](../../examples/ax-multi-agent/) — devil + angel worked example
