# Bidirectional MCP: core-agent as an MCP server

Design doc for adding **MCP server mode** to core-agent. Untracked
sibling to `docs/background-subagents-design.md` and
`docs/scion-harness-improvements-design.md`. Targets a future minor
release; tag TBD pending the parallel attach-mode design that this
shares HTTP infrastructure with.

## Context

core-agent is an MCP **client** today. `mcp/` reads
`.agents/mcp.json`, spawns each declared server (stdio child or
Streamable HTTP), wraps each MCP toolset under namespaced names
(`<server>_<tool>`), and routes calls through the permission gate
(`mcp/lifecycle.go:106-188`). Any tool surfaced by an external MCP
server becomes a tool the agent's model can call.

We can be consumed; we cannot be invoked. A core-agent agent — even
one wired with bash, file ops, a curated MCP catalog, durable
sessions, a personality, and a budget — is invisible to other MCP
hosts. Claude Desktop, gemini-cli, Cursor, an in-house orchestrator,
or another core-agent can't reach in and ask it to do work as a
peer.

Adding **server mode** closes the loop. core-agent becomes
*bidirectional*: it speaks MCP in both directions. Any MCP-aware
host can invoke a core-agent agent the same way it invokes a
filesystem or git MCP server. That positions core-agent agents as
first-class composable units in the MCP ecosystem rather than as
private endpoints reachable only through the bundled CLI or a Go
import.

The substrate is already in place. We have an `*agent.Agent` with a
clean `Run(ctx, prompt) iter.Seq2[*session.Event, error]` shape, a
working subagent wrapper pattern (`agent/subagent.go`), a
permission gate that knows how to authorize tool calls through any
boundary, an eventlog that records every interaction, and an MCP
SDK (`github.com/modelcontextprotocol/go-sdk/mcp`) we already
depend on for the client side. Server mode is wiring + a small set
of decisions about what we expose and how.

This is a composability/ecosystem play, not a feature consumers can
immediately point at. The use cases are downstream:

1. A consumer wraps a curated research agent (system prompt +
   carefully chosen tools + budget) and exposes it to Claude Desktop
   as the single tool `ask_research_agent`. The user gets a
   purpose-built agent inside their normal Claude conversation.
2. An orchestrator (Scion, internal pipeline, custom Go binary)
   composes several heterogeneous agents — a core-agent for code,
   a Python ADK agent for data analysis, a third-party MCP server
   for ticketing — using MCP as the wire. core-agent agents become
   plug-replaceable with anything else that speaks the protocol.
3. A core-agent invokes another core-agent over MCP. Same library
   on both ends, but the call goes through the protocol so the two
   processes can live anywhere (different hosts, different
   languages later, different deployment models).

### Settled background

- The MCP spec we target is **2025-11-25**
  (https://modelcontextprotocol.io/specification/2025-11-25/). Tools
  are model-controlled, prompts are user-controlled, resources are
  application-controlled. JSON-RPC 2.0 with stateful connections.
- Two transports defined: **stdio** (newline-delimited JSON-RPC on
  stdin/stdout, no auth — credentials come from the environment)
  and **Streamable HTTP** (the modern HTTP transport; SSE-based
  notifications optional; auth via the spec's OAuth framework or
  custom strategies).
- The Go SDK (`github.com/modelcontextprotocol/go-sdk/mcp`) supports
  both transports on both sides. We already use its client surface;
  the server surface is the symmetric API.

## Goals and non-goals

### Goals

- **Expose a core-agent `*agent.Agent` as an MCP server** that any
  spec-compliant MCP host can connect to and use.
- **Support both transports.** stdio for the "MCP server invoked by
  a host as a child process" case (Claude Desktop, gemini-cli);
  Streamable HTTP for the "MCP server reachable over the network"
  case (multi-host orchestration, A2A-style composition).
- **One library API, one CLI subcommand.** Consumers should reach
  for `mcpserver.Serve(agent, opts...)` from Go and
  `core-agent mcp-serve` from the CLI without having to think about
  transport plumbing.
- **Permission gate stays authoritative.** Cross-boundary calls go
  through the same `permissions.Gate` that gates in-process tool
  calls. The "what's safe to expose over MCP" defaults are
  conservative; consumers opt into broader exposure deliberately.
- **Audit log captures everything.** A request crossing the MCP
  server boundary lands in the eventlog the same way every other
  agent interaction does — with branch attribution that makes the
  call lineage queryable.
- **Composable with the rest of core-agent.** Server mode coexists
  with client mode (an agent can be an MCP server *and* consume MCP
  servers itself), with subagents, with `RunAutonomous`, with the
  durable session backend. Three-layer compositions (host → MCP →
  core-agent → MCP → another core-agent) work cleanly.

### Non-goals

- **Replacing subagents.** `WithSubagents` and the
  `BackgroundAgentManager` stay the right answer for in-process
  composition where you want zero protocol overhead, shared
  Go-level types, and the lowest possible latency. Server mode is
  for cross-process / cross-host composition.
- **Replacing the attach-mode design.** The parallel attach-mode
  design (let an external client *attach* to a running core-agent
  session, observe events, inject prompts) is its own thing.
  Server mode and attach mode share HTTP listener / auth / TLS
  plumbing (see "Relationship to attach mode" below) but the wire
  protocol and use case differ.
- **Inventing our own protocol.** We don't extend MCP with
  non-standard methods. If a consumer needs core-agent-specific
  semantics (e.g. "subscribe to all events from a session"), they
  reach for attach mode, not for an MCP extension.
- **Full MCP server feature coverage on day one.** Resources and
  prompts are spec-defined server features (alongside tools).
  Tools is the load-bearing primitive for our use case; resources
  and prompts ship later if a consumer asks (see "Deferred").
- **Production-grade auth.** We surface the seams (TLS plumbing,
  bearer-token verification hook, OAuth-resource-metadata
  endpoint) and ship a sane minimum (bearer-token shared-secret
  config), but the full OAuth flow with token introspection and
  refresh is consumer infrastructure, not core-agent's. See "Auth".

## What gets exposed: the central decision

There are three plausible exposure modes, each with a different
mental model. **We pick mode A as the default, with mode B
available as `WithToolPalette()`, and mode C falling out naturally
since both are available on the same server.**

### Mode A: Agent-as-tool (default)

The server exposes the agent itself as a single MCP tool. The
host calls `tools/call` with method `<agent_name>` (or a
configurable name) and a single `request` string argument. The
server runs the agent for one or more turns until it produces a
final answer, and returns that answer as the tool's result.

```jsonc
// Server's tools/list response (single tool):
{
  "tools": [{
    "name": "research_agent",
    "description": "Ask the research agent a question. Returns its final answer.",
    "inputSchema": {
      "type": "object",
      "properties": {
        "request": {
          "type": "string",
          "description": "The question or task in natural language."
        }
      },
      "required": ["request"]
    }
  }]
}

// Client → server tools/call:
{ "method": "tools/call",
  "params": { "name": "research_agent", "arguments": { "request": "..." }}}

// Server → client (response):
{ "content": [{ "type": "text", "text": "<agent's final answer>" }] }
```

This mirrors `NewSubagentTool`'s shape (`agent/subagent.go:107`)
exactly: a single `request` string in, a final-text string out.
That symmetry is deliberate. The same agent can be exposed as a
subagent (in-process, via `WithSubagents`) or as an MCP server
(cross-process, via `mcpserver.Serve`) without changing the
consumer's mental model of what it is.

**Why this is the default**

- **Opaque is honest.** The host doesn't need to know whether the
  exposed agent uses bash, MCP imports, skills, a particular
  model, or recursive subagents. It asks; it gets an answer.
- **No permission surprise.** The agent owns its own permission
  gate. The host doesn't see (and can't invoke) the agent's
  bash; it only sees the agent itself. The blast radius across
  the MCP boundary is whatever the agent's gate already allows
  for its own model.
- **Stable surface.** Adding a tool to the wrapped agent doesn't
  change its MCP surface — `tools/list` still shows one tool. The
  host doesn't have to re-discover capabilities on every agent
  update.
- **Matches how MCP hosts already think.** Claude Desktop users
  install MCP servers like "filesystem" and "git" and reach for
  them via their function name. "research-agent" is the same
  shape: a named capability that takes a string and returns a
  string.

### Mode B: Tool-palette (`WithToolPalette()`)

The server re-exposes the wrapped agent's tool registry directly.
Each tool the agent has — `read_file`, `bash`, `slack_post`,
imported MCP tools, skills — becomes its own MCP tool with the
same shape it has in-process.

```jsonc
{
  "tools": [
    { "name": "read_file",   "inputSchema": { ... } },
    { "name": "write_file",  "inputSchema": { ... } },
    { "name": "bash",        "inputSchema": { ... } },
    { "name": "slack_post",  "inputSchema": { ... } },
    // ...
  ]
}
```

**Why this is opt-in**

- **Transparent but coupling-heavy.** The host sees every tool the
  agent has and chooses which to call directly. Any tool added or
  removed on the agent shifts the server's surface.
- **Permission semantics are sharper.** A host calling `bash` over
  the MCP boundary is a much bigger event than a host calling
  `ask_research_agent` and the agent internally choosing to call
  bash. The default gate posture for palette-mode tools is
  **deny** for the dangerous classes (bash, write_file, edit_file)
  unless the consumer explicitly allowlists them per-MCP-client
  (see "Permission model").
- **Loses the agent's reasoning loop.** The host's model decides
  what to do with the palette. The wrapped agent's system prompt,
  its planning, its `todo` list, its subagent topology — none of
  that runs. This is fine when the consumer specifically wants
  "expose my tool collection over MCP" but defeats the purpose
  when they wanted "expose my agent".

### Mode C: Both (`WithBothModes()`)

Tool-palette plus the agent-as-tool. The host can pick: invoke
individual tools directly, or invoke the agent and let it
orchestrate the same tools internally. Useful for orchestrators
that want flexibility but rare for end-user MCP hosts.

### Decision rationale

Mode A as default + B opt-in + C as the union covers the
spectrum without privileging any one shape:

- **Consumers building "expose my purpose-built agent to Claude
  Desktop" use the default.** Zero extra config; reuse the
  agent's name as the tool name; the host has one button to push.
- **Consumers building "expose my tool collection over MCP" call
  `mcpserver.Serve(agent, mcpserver.WithToolPalette())`.** Their
  agent might still have an instruction set, but the MCP surface
  is the tools, not the agent.
- **Consumers running orchestrators that want both** flip
  `WithBothModes()` and accept the responsibility for the union
  surface area.

The alternative we considered and rejected: making this entirely
consumer-driven via `WithExposedTools(...)` where the consumer
constructs the MCP tool list themselves. Rejected because it
forces every consumer to write the shape-translation code (we
have it already in `NewSubagentTool`'s arg schema and in the
ADK ↔ MCP toolset translation), and because the three modes
above cover the realistic landscape. Consumers with truly
custom exposure can drop down to the lower-level
`mcpserver.NewHandler(...)` API and build their own tool list.

## Transport

Both transports ship. The split mirrors the client side.

### Stdio

The bundled CLI's primary mode: `core-agent mcp-serve` reads
JSON-RPC from stdin and writes to stdout. Designed for the host
to spawn core-agent as a child process and communicate over the
inherited pipes. Same lifecycle as the stdio MCP servers we
already consume in `mcp/lifecycle.go:194-207`.

Why stdio first:

- **Claude Desktop, gemini-cli, Cursor, and most MCP-aware
  editors invoke MCP servers as child processes over stdio.**
  That's the canonical "MCP server" deployment shape. Without
  stdio support we can't be plugged into any of them.
- **No auth surface to design.** The spec explicitly says stdio
  servers should retrieve credentials from the environment (the
  same model we use for our own MCP client). The parent process
  controls the child's environment; trust is implicit.
- **Simplest to ship.** No network listener, no TLS, no auth, no
  port allocation.

### Streamable HTTP

The networked mode: `core-agent mcp-serve --http :8080` exposes
the same MCP service over HTTP at the spec-defined
`/mcp` endpoint (configurable). Uses
`mcpsdk.StreamableServerTransport` (the SDK's server-side
counterpart to the `StreamableClientTransport` we use as a
client).

Why HTTP also:

- **Cross-host orchestration.** When core-agent isn't a child
  process — when it's deployed in a container, a K8s pod, a
  Cloud Run service — HTTP is the wire.
- **Attach-mode infrastructure overlap.** The attach-mode design
  (parallel work) needs an HTTP listener with TLS and auth. Both
  features want the same plumbing. Sharing it (see "Relationship
  to attach mode") is straightforward; building two parallel HTTP
  stacks would be silly.

### Both at once?

A single invocation `core-agent mcp-serve --http :8080` can run
both transports concurrently if `--stdio` is also passed (stdio
becomes a second client; both are equivalent peers). This is
useful exactly never for the bundled CLI but cheap to support
since the SDK abstracts the transport.

Default: stdio when no `--http` flag is set; HTTP when `--http`
is set; both when both are explicit. Matches the principle of
least surprise.

## Wire protocol mapping

How does an agent run map to MCP `tools/call` semantics?

### Mode A: agent-as-tool

```
host                                  core-agent (MCP server)
 │                                     │
 │  tools/call {"name": "research-agent", "arguments": {"request": "..."}}
 ├────────────────────────────────────▶│
 │                                     │  1. Resolve session (see "Session lifecycle")
 │                                     │  2. agent.Run(ctx, request)
 │                                     │  3. Iterate the event stream:
 │                                     │     - partial text events → progress
 │                                     │       notifications (optional)
 │                                     │     - tool calls happen internally
 │                                     │     - final text accumulates
 │                                     │  4. When ctx is done or stream
 │                                     │     returns TurnComplete, build the
 │                                     │     response:
 │                                     │       { "content": [{
 │                                     │           "type": "text",
 │                                     │           "text": "<final text>"
 │                                     │       }],
 │                                     │         "isError": false }
 │                                     │
 │◀────────────────────────────────────┤  tool result
 │                                     │
```

The collection of final text mirrors `collectFinalText`
(`agent/subagent.go:268-281`) — final non-partial text parts
joined by newlines. We deliberately do **not** include the
internal tool-call traces, the partial-stream tokens, or the
subagent transcripts in the MCP result; the result is what the
*agent* concluded, not the audit trail of how it got there. The
audit trail lives in the eventlog and is accessible via attach
mode or direct DB query.

### Mode B: tool-palette

```
host                                  core-agent (MCP server)
 │                                     │
 │  tools/call {"name": "read_file", "arguments": {"path": "..."}}
 ├────────────────────────────────────▶│
 │                                     │  1. Look up tool in palette
 │                                     │  2. Run through permission gate
 │                                     │     (see Permission model — default
 │                                     │     deny for bash / write tools)
 │                                     │  3. Invoke the tool directly
 │                                     │  4. Return the tool's result content
 │                                     │
 │◀────────────────────────────────────┤  tool result
```

No agent run; the request is a direct tool invocation. The
agent's model is not consulted. The wrapped *agent* is still the
permission scope (its gate, its session) — we're just bypassing
its LLM.

### Streaming partial events

The MCP spec defines **progress notifications** (`notifications/
progress` from server to client, addressed by request ID) as the
mechanism for in-flight updates during a long-running tool call.

For mode A, we have a choice:

1. **Send no progress notifications.** Simple; the host sees only
   the final answer. Lowest cognitive load on the host side
   (most MCP hosts today don't render progress).
2. **Send progress notifications per partial text event.** The
   host sees partial text as the agent generates it (if the host
   chooses to render it). Higher fidelity; more network chatter;
   not all hosts handle it well.
3. **Send progress notifications per tool call boundary.** A
   middle ground: the host sees "thinking… now calling
   `read_file`… now calling `bash`… complete." Useful for human-
   facing hosts that want to surface activity without rendering
   the full text stream.

**Decision: option 3 by default; option 2 available via
`WithProgressMode(mcpserver.ProgressFull)`; option 1 available
via `WithProgressMode(mcpserver.ProgressOff)`.**

Reasoning: tool-call boundaries are the natural "something
happened" punctuation for an agent. They map to the kind of
progress most hosts know how to render (a busy spinner with a
brief label). Full token streaming is opt-in because it changes
the conversation about latency and bandwidth — the consumer
should choose. Off-mode is the lowest-common-denominator that
still works against the dumbest MCP host.

For mode B, no progress notifications by default — tool calls
are atomic from the host's perspective (the wrapped tool returns
when it returns; no intermediate state).

### Errors

The spec distinguishes **JSON-RPC errors** (request malformed,
method not found — `error` field in the response) from **tool
errors** (the tool ran but returned a failure — `isError: true`
in a successful response).

We use both correctly:

- Argument-validation failures (missing `request`, wrong type) →
  JSON-RPC error code `-32602` (invalid params).
- Unknown tool name in mode B → JSON-RPC error code `-32601`
  (method not found, applied to the tool name).
- Permission denial → tool result with `isError: true` and a
  human-readable explanation.
- Agent ran but returned an error (budget exhausted, model
  failure mid-stream, etc.) → tool result with `isError: true`
  and the error text.
- Context cancellation from the host (the host gave up on the
  request) → tool result `isError: true` with "canceled".

## Session lifecycle

This is the load-bearing decision for any non-trivial deployment.

### The two extreme positions

1. **Stateless: every `tools/call` is a fresh session.** The
   wrapped agent has no memory of prior calls. The host's
   conversation history (if any) has to be re-supplied as part of
   each `request` string. Simplest semantics; matches what most
   stateless RPC services do.
2. **Sticky: all `tools/call`s on one MCP connection share a
   session.** The agent accumulates conversation history. The
   second call sees what the first call did.

Stateless loses too much value — the whole point of an agent over
RPC is that it can do multi-turn reasoning. Sticky requires us to
answer "what's a session" without a clean handle from the spec.

### Decision: sticky per MCP connection by default

For stdio: one stdio connection = one session. When the host spawns
a fresh `core-agent mcp-serve` it gets a fresh session; subsequent
calls on the same stdio reuse it. When the child process exits,
the session ends.

For HTTP: the spec defines an `Mcp-Session-Id` header that the
server can issue on initialization and the client must echo on
subsequent requests. We use it. One `Mcp-Session-Id` = one
core-agent session.

This maps cleanly onto core-agent's existing `(Application,
UserID, SessionID)` triple:

| Field | Source |
|---|---|
| `Application` | `agent.AppName()` — whatever the wrapped agent reports |
| `UserID` | Server config (default: `"mcp-client"` constant; overridable via `WithUserID(fn)` callback that can read auth-derived user identity off the request) |
| `SessionID` | Derived from MCP connection identity: stdio → `"mcp-stdio-<pid>"`; HTTP → `"mcp-http-<Mcp-Session-Id>"` |

### Persistence

If the wrapped agent has an eventlog (`WithEventLog` /
`WithSessionService`), MCP-served sessions land in the same audit
log as everything else, branched by `mcp.<client-info>` (where
client-info comes from the MCP `initialize` request's
`clientInfo.name` field — Claude Desktop reports `"claude-ai"`,
gemini-cli reports `"gemini-cli"`, etc.).

This means a session served over MCP can be replayed, watched,
and resumed exactly like a session created via the bundled CLI.
`eventlog.Open(...)` → `Stream.Since(seq)` shows the MCP calls
interleaved with everything else.

If the wrapped agent has no eventlog, MCP-served sessions live in
`session.InMemoryService` and die with the connection. Same
trade-off as the rest of the substrate.

### Explicit session control: `WithSessionMode`

Consumers can override the default sticky behavior:

```go
mcpserver.Serve(ag,
    mcpserver.WithSessionMode(mcpserver.SessionPerCall),  // stateless
    // or
    mcpserver.WithSessionMode(mcpserver.SessionPerConnection), // default
    // or
    mcpserver.WithSessionMode(mcpserver.SessionShared("global")), // one shared
)
```

`SessionPerCall` is right for "expose a stateless capability to
many hosts" (e.g. a code-review agent where each call is
independent). `SessionPerConnection` is the default. `SessionShared`
is dangerous (all callers see each others' history) but useful for
a single-tenant deployment where the agent's "memory" is the point.

## Naming and discovery

How does an exposed tool name itself?

### Mode A (agent-as-tool)

Default: `<agent-name>` (taken from `agent.AgentName()`, which
comes from `agent.WithName`). Override via
`mcpserver.WithToolName("ask_research")`.

We deliberately do **not** prefix with `coreagent_` by default.
The host calls the agent the same name the agent calls itself.
If the host has a name collision in its registry, the consumer
overrides explicitly.

### Mode B (tool-palette)

Default: each tool keeps its in-process name (`read_file`,
`bash`, `slack_post`). MCP-imported tools the agent consumes
themselves use the existing client-side namespacing
(`<server>_<tool>`); we don't re-prefix when re-exporting (that
would yield `<server>_<tool>` from the outside which is fine —
the namespace already says where the tool came from).

Override via `mcpserver.WithPaletteToolPrefix("coreagent_")` if a
consumer wants every exposed palette tool prefixed for
collision-avoidance on the host side.

### Server identity

The MCP `initialize` handshake exchanges `Implementation` records.
The server reports:

- `name`: defaults to `agent.AppName()`; overridable via
  `mcpserver.WithServerName(...)`.
- `version`: defaults to `core-agent/<library-version>`;
  overridable.

This mirrors our existing client-side `SetImplementationName`
shape (`mcp/lifecycle.go:44-53`).

## Permission model

This is the highest-stakes design surface. Get it wrong and we
ship a footgun; get it too restrictive and the feature is dead on
arrival.

### Principle: the wrapped agent's gate is authoritative

The wrapped agent has a `*permissions.Gate` already. When the MCP
server runs the agent (mode A) or invokes one of the agent's
tools directly (mode B), the gate is consulted at exactly the
same call sites it's consulted today. No new permission code path
crosses the MCP boundary.

What changes is the **default policy** for what's allowed when
the caller is "an MCP client" rather than "the local human in the
REPL."

### Mode A: gate stays as configured

Mode A is opaque. The host calls one tool; the wrapped agent runs
its internal loop including any tool calls; the wrapped agent's
gate gates those internal calls exactly as before. The MCP
boundary itself doesn't gate anything new — once the host has
authority to call the exposed tool at all, the agent's internals
are the agent's business.

This means: **if you don't want the MCP host to be able to
indirectly trigger your `bash` tool, configure the agent's gate
to refuse bash.** The MCP boundary doesn't double-gate.

The one thing we add: every tool result the agent produces in
mode A is tagged in the eventlog with the calling client's
identity (`mcp.<client-info>` branch + per-call seq), so the
audit query "what did Claude Desktop trigger?" is answerable.

### Mode B: default-deny for sensitive tools

Mode B exposes the agent's tools directly. A host calling
`bash` over MCP is materially more dangerous than the agent's
own model calling bash, because the host is *not* the agent's
trusted model with the agent's system prompt and instructions —
it's some other thing.

Default-deny applies to the **dangerous classes**:

- `bash`
- `write_file`
- `edit_file`
- any MCP-imported tool from a server marked `dangerous: true` in
  `.agents/mcp.json` (new optional field; defaults to false)

Allow via explicit per-server config:

```go
mcpserver.Serve(ag,
    mcpserver.WithToolPalette(),
    mcpserver.WithPaletteAllow("read_file", "list_dir", "grep"),
    // or
    mcpserver.WithPaletteAllowAll(), // yolo: expose everything
)
```

The per-tool denylist (`permissions.Gate.IsDenied`) still applies
on top — so even a `WithPaletteAllowAll` invocation refuses
`rm -rf /` via the existing non-overridable bash denylist.

The thinking: a consumer who wants to expose bash to Claude
Desktop should have to say so in three places (`WithToolPalette`,
`WithPaletteAllow("bash")`, and *also* the gate's bash mode being
yolo/allow). Three opt-ins is enough friction to ensure the
choice is deliberate.

### Auth-driven user identity for the gate

When HTTP transport carries an auth token (see "Auth"), the
server config can derive `UserID` from the token (e.g. "alice"
vs "bob"). The gate can then apply per-user policies if the
consumer wires them up (the gate doesn't have per-user state
today; that's a separate small extension, deferred unless a
real consumer asks).

For the v1 of bidirectional MCP, all callers on one connection
share one `UserID` derived from server config (defaulting to a
constant). Per-call user-identity differentiation is a follow-up.

### Elicitation flowing the other way

The MCP spec lets a server **elicit** information from a client
(`elicitation/create` — server-initiated request for additional
info from the user). We use it client-side today
(`mcp/elicitation.go`). If the wrapped agent's tool needs user
input (e.g. an `ask_user` tool, or a permission prompt in `ask`
mode), the server can route that through MCP elicitation to the
client, which forwards to its end-user.

This is a clean win: the wrapped agent's existing `Prompter`
interface gets a new implementation `mcpserver.ElicitationPrompter`
that turns approval prompts into MCP elicitation requests. Hosts
that implement elicitation render the prompt in their own UI;
hosts that don't fall back to whatever the default `Prompter`
does (which for headless / unknown contexts is refuse-with-
notice). Same shape as the existing `StdinPrompter` and
`RefusePrompter` choice (`permissions/prompter.go`).

## Composability: the three-layer scenario

A parent core-agent (P1) invokes an MCP-exposed core-agent (S1)
that has its own subagents (P1→S1→sub-S1a) using MCP-imported
tools (sub-S1a → external MCP server X). Three layers of agent
across two protocol hops.

```
   P1 ─[MCP tools/call]→ S1 (mcpserver) ─[in-process subagent]→ sub-S1a
                                                                    │
                                                                    └─[MCP tools/call]→ X (external MCP server)
```

### Does the audit log handle it?

Each agent has its own eventlog (or shares P1's, or shares S1's,
depending on consumer wiring). Within each agent's eventlog, the
existing branch taxonomy handles the nesting:

- P1's events under `Branch=""` (P1's main turn).
- S1, when run by P1 over MCP, lives in S1's eventlog (different
  database row entirely — S1 is a different process, possibly a
  different DB). S1's events are under `Branch="mcp.P1-client"`
  (the calling client info), or whatever P1 declared in its
  `clientInfo` during the MCP handshake.
- sub-S1a (S1's subagent) is under `Branch="mcp.P1-client.sub-
  S1a"` in S1's eventlog — composed by the existing
  `composeBranch` helper (`agent/subagent.go:249-262`).
- sub-S1a's call to X is an MCP client call; X's logs (if any)
  are X's problem. From sub-S1a's perspective it's a tool call,
  audited locally as such.

The branch taxonomy extends cleanly because:

- `mcp.<client-info>` is just another branch prefix.
- `composeBranch` already handles nested branches (it's used for
  subagents-within-subagents today).
- Each agent's eventlog is self-contained; there's no expectation
  that cross-process audit queries see everything in one place.
  An orchestrator that wants the full picture queries each
  layer's eventlog separately and stitches by call ID (we
  surface the MCP call ID in the event metadata so this is
  possible).

### Does cost/usage roll up?

No, not across the MCP boundary, by design.

In-process, the deferred-from-v1 work item ("cost rollup from
subagents into the parent's usage.Tracker" — see README's
Roadmap) is a known gap. Crossing a process boundary makes the
gap unavoidable: P1's `usage.Tracker` only knows about P1's LLM
calls. S1's tokens are S1's accounting.

Mitigation: the MCP `tools/call` response includes an optional
`_meta` block (spec-reserved metadata field). We populate it with
`{"coreagent.usage": {"tokens": ..., "cost_usd": ...}}` so a
sophisticated host can read out S1's spend and roll it up on its
side. The naming follows the spec's `_meta` reservation rules
(reverse-DNS-style prefix — we use `coreagent.` since `core-
agent` isn't a valid DNS prefix segment, and our second label
isn't `mcp` or `modelcontextprotocol` so we're outside the
reserved range).

Hosts that don't read `_meta` get a working tool call with
unattributed cost. That's the current state of the world for any
MCP server today; we're not making it worse.

### Recursion safety

P1 → S1 → P1 (S1 calls P1 back as an MCP server) is a possible
loop if both processes expose each other. The MCP spec doesn't
say anything about depth; it's a problem for the implementation.

We do not add cross-process recursion detection in v1. The
in-process subagent depth cap (`subagentDepthKey{}` —
`agent/subagent.go:91-102`) doesn't cross MCP boundaries. The
safety net is the per-call budget: each agent has turn / cost /
wallclock budgets that bound how much damage a runaway recursion
can do before something kills it.

If a real cross-process recursion scenario emerges, the right
shape is probably a `Mcp-Call-Depth` custom header echoed through
the chain — defer until needed.

## Library API

The consumer-facing surface, kept narrow:

```go
package mcpserver

// Serve starts an MCP server exposing the given agent. Blocks
// until the context is canceled (HTTP) or stdin closes (stdio).
//
// Defaults: stdio transport, agent-as-tool exposure mode, session-
// per-connection, progress notifications on tool-call boundaries.
func Serve(ctx context.Context, a *agent.Agent, opts ...Option) error

// Option configures the server. The With* helpers below cover the
// common cases.
type Option func(*options)

// Transport selection.
func WithStdio() Option                       // default
func WithHTTP(addr string) Option             // ":8080" or "127.0.0.1:8080"
func WithBothTransports(addr string) Option   // stdio + HTTP
func WithTLS(certFile, keyFile string) Option // requires WithHTTP
func WithHTTPEndpoint(path string) Option     // default "/mcp"

// Exposure mode.
func WithAgentAsTool() Option                              // default
func WithToolPalette() Option
func WithBothModes() Option
func WithToolName(name string) Option                      // mode A
func WithPaletteToolPrefix(prefix string) Option           // mode B
func WithPaletteAllow(toolNames ...string) Option          // mode B
func WithPaletteAllowAll() Option                          // mode B; "yolo"

// Session lifecycle.
type SessionMode int
const (
    SessionPerConnection SessionMode = iota // default
    SessionPerCall
)
func WithSessionMode(m SessionMode) Option
func WithSessionShared(sessionID string) Option // all callers share one session

// User identity.
func WithUserID(fn func(*Request) string) Option // derive user from MCP request

// Progress.
type ProgressMode int
const (
    ProgressOff ProgressMode = iota
    ProgressOnToolCall // default
    ProgressFull       // every partial text event
)
func WithProgressMode(m ProgressMode) Option

// Server identity (for the MCP initialize handshake).
func WithServerName(name string) Option    // defaults to agent.AppName()
func WithServerVersion(v string) Option

// Auth.
func WithBearerToken(token string) Option           // require Authorization: Bearer <token>
func WithBearerTokenFile(path string) Option        // read token from disk; supports rotation
func WithAuthCallback(fn AuthFn) Option             // custom verification

// Elicitation: route the agent's Prompter through MCP elicitation
// to the client (instead of stdin / refuse-with-notice).
func WithElicitationPrompter() Option

// Low-level escape hatch for consumers who need full control.
func NewHandler(a *agent.Agent, opts ...Option) (*Handler, error)
```

The minimum-viable call is one line:

```go
mcpserver.Serve(ctx, ag) // stdio, agent-as-tool, sane defaults
```

The realistic Claude-Desktop deployment is two:

```go
mcpserver.Serve(ctx, ag,
    mcpserver.WithToolName("research"),
)
```

The networked tool-palette deployment is a handful:

```go
mcpserver.Serve(ctx, ag,
    mcpserver.WithHTTP(":8080"),
    mcpserver.WithTLS("cert.pem", "key.pem"),
    mcpserver.WithBearerTokenFile("/etc/mcp/token"),
    mcpserver.WithToolPalette(),
    mcpserver.WithPaletteAllow("read_file", "list_dir", "grep"),
)
```

### CLI subcommand

```
core-agent mcp-serve [flags]

Flags:
  --http=ADDR              Listen for HTTP (default: stdio only)
  --tls-cert=PATH          TLS certificate (requires --http)
  --tls-key=PATH           TLS private key (requires --http)
  --bearer-token-file=PATH Require Authorization: Bearer <contents>
  --tool-name=NAME         Name for the exposed agent tool (mode A; default: agent name)
  --tool-palette           Expose the agent's tools (mode B) instead of the agent
  --palette-allow=LIST     Comma-separated palette allowlist (mode B; default: read_file,list_dir,grep,glob)
  --palette-allow-all      Expose every tool (mode B; "yolo")
  --session-mode=MODE      per-connection (default) | per-call
  --progress=MODE          off | tool-call (default) | full
  --server-name=NAME       MCP server identity (default: app name)
```

The bundled CLI assembles an `*agent.Agent` from the same config
flow as `core-agent` (one-shot / REPL) — `.agents/config.json`,
provider auto-detect, AGENTS.md, MCP imports — then calls
`mcpserver.Serve` with the appropriate options.

The expected use case: a user wants Claude Desktop to be able to
ask their preconfigured core-agent for help. They add an entry to
Claude Desktop's MCP config:

```jsonc
// claude_desktop_config.json
{
  "mcpServers": {
    "research": {
      "command": "core-agent",
      "args": ["mcp-serve", "--tool-name=ask_research"],
      "env": { "GEMINI_API_KEY": "sk-..." }
    }
  }
}
```

Now Claude can call `ask_research` from inside a Claude Desktop
conversation. The core-agent uses its own provider, its own MCP
servers, its own skills, its own gate.

## Relationship to attach mode

The attach-mode design (parallel work, separate doc) lets a
client *attach* to a running core-agent and observe events / inject
prompts over HTTP. Different shape, overlapping plumbing.

### What both need

- HTTP listener with TLS support.
- Bearer-token auth (and a hook for richer auth).
- Endpoint registration on a shared listener (so both features
  can share a process).
- A way for the consumer to plug an `http.Handler` into their
  existing server (consumers who already have a web server want
  to mount us as a sub-path rather than spawning their own
  listener).

### What they don't share

- **Wire protocol.** MCP server uses JSON-RPC over a `/mcp`
  endpoint with the spec's transport semantics; attach mode uses
  whatever shape its design settles on (likely SSE or
  websockets for event streaming + a separate POST endpoint for
  injects).
- **Use case.** MCP server is "remote callers invoke this agent
  as a tool"; attach mode is "observers tail this agent's
  in-flight session." A consumer might want one and not the
  other.

### Decision: shared infrastructure package

A small new package — `httptransport/` or similar — owns:

- HTTP server lifecycle (`http.Server`, graceful shutdown,
  context plumbing).
- TLS setup (`crypto/tls.Config` from cert + key paths).
- Bearer-token middleware (one place).
- Endpoint registration (an `http.ServeMux` that both `mcpserver`
  and the attach-mode handler mount onto).

Both features depend on `httptransport/`; neither depends on the
other. A consumer who wants both runs them on the same listener:

```go
mux := httptransport.New(httptransport.WithBearerTokenFile("/etc/token"))
mcpserver.MountHTTP(mux, "/mcp", ag, opts...)
attachmode.Mount(mux, "/attach", ag, opts...)
httptransport.Serve(ctx, mux, ":8080")
```

This is the right factoring even if attach mode never ships — the
HTTP plumbing for MCP alone wants to be in a separate, narrow
package rather than embedded in `mcpserver/`.

**Coordination point**: the attach-mode designer and this design
need to agree on the `httptransport/` package shape before either
ships. The auth middleware in particular wants to be designed
once. If the schedules diverge — MCP server ships first and
attach mode comes later — the attach-mode design simply consumes
the package shape MCP server established.

## Auth

We surface the seams, ship a sane minimum, and explicitly defer
the full OAuth flow.

### Stdio: no auth

Per spec. The parent process controls the child. Trust is
inherited from the OS.

### HTTP: bearer token (shipped)

- `WithBearerToken("...")` or `WithBearerTokenFile("path")` —
  requires `Authorization: Bearer <token>` on every request.
  Constant-time comparison; rejects with `401 Unauthorized` on
  mismatch.
- `WithBearerTokenFile` re-reads the file on a SIGHUP (or a
  polling interval) so token rotation works without restart.

### HTTP: TLS (shipped)

- `WithTLS(certFile, keyFile)` — standard `http.Server.TLSConfig`
  via the std-lib. ACME / Let's Encrypt is consumer territory.
- mTLS not in v1; would land via `WithClientCAs(...)` if a
  consumer asks.

### HTTP: custom auth (shipped seam, no built-in implementation)

- `WithAuthCallback(fn AuthFn)` — consumer-supplied function that
  receives the `*http.Request` and returns either a `User` struct
  (used for session identity, downstream gate scoping) or an
  error. Consumers who want OAuth, JWT verification, IAM,
  proxy-trusted headers, etc. plug it in here.

### What's not in v1

- **MCP OAuth resource server.** The spec defines an OAuth-
  resource-server pattern (RFC 9728 + a metadata discovery
  endpoint at `/.well-known/oauth-resource`) for HTTP servers
  that want to participate in a proper OAuth flow with token
  introspection and refresh. We're not implementing it. The
  `WithAuthCallback` seam lets a consumer bolt their own OAuth
  validation on; that's the v1 story. Promote to first-class if
  a real consumer needs the spec-conformant flow.
- **Per-user gate scoping.** The gate doesn't have per-user state
  today. Adding it is its own design (does an "allow" decision
  scope to the user, the session, the gate instance?). Defer.

Net: stdio inherits OS trust; HTTP gets bearer + TLS + a custom
auth seam. Production deployments behind a reverse proxy that
handles OAuth/mTLS work fine through the seam.

## Critical files

**New:**

- `mcpserver/serve.go` — `Serve(ctx, agent, opts...)`,
  `NewHandler(...)`, option types.
- `mcpserver/handler.go` — JSON-RPC routing, `tools/list` +
  `tools/call` implementations for modes A / B.
- `mcpserver/agent_tool.go` — mode A wiring (the agent-as-single-
  tool handler; reuses the call shape from `agent/subagent.go`).
- `mcpserver/palette.go` — mode B wiring (re-export the agent's
  tool registry as individual MCP tools; default-deny on
  dangerous classes).
- `mcpserver/session.go` — session lifecycle (per-connection vs
  per-call vs shared); maps to core-agent's `(App, User, Session)`
  triple; integrates with the event log via the wrapped agent's
  `WithEventLog`.
- `mcpserver/progress.go` — progress-notification emission for
  mode A; tool-call-boundary vs full-stream modes.
- `mcpserver/elicitation.go` — `ElicitationPrompter` that adapts
  `permissions.Prompter` onto MCP elicitation requests.
- `mcpserver/stdio.go` — stdio transport wiring (calls into the
  MCP SDK's `mcpsdk.StdioServerTransport`).
- `mcpserver/http.go` — HTTP transport wiring (calls into
  `mcpsdk.StreamableServerTransport`); mounts on
  `httptransport.Mux`.
- `mcpserver/serve_test.go`, `mcpserver/handler_test.go`,
  `mcpserver/session_test.go`, `mcpserver/palette_test.go` —
  unit + integration coverage.
- `httptransport/server.go` — shared HTTP listener / TLS / bearer
  middleware (the foundation attach mode also consumes).
- `httptransport/auth.go` — bearer token + `AuthFn` callback.
- `cmd/core-agent/mcpserve.go` — `mcp-serve` subcommand wiring.
- `examples/mcp-serve/main.go` — minimal example exposing an
  agent over stdio for Claude Desktop.
- `examples/mcp-serve-http/main.go` — HTTP example with bearer
  auth.
- `docs/site/content/docs/mcp-server.md` — new user-guide page.

**Modified:**

- `cmd/core-agent/main.go` — recognize `mcp-serve` subcommand,
  dispatch to `mcpserve.go`.
- `mcp/lifecycle.go` — extract `SetImplementationName` pattern
  into a small helper used by both the client and the new server
  side. No semantics change.
- `permissions/prompter.go` — no API change; the new
  `mcpserver.ElicitationPrompter` is an external impl of
  `Prompter`.
- `agent/agent.go` — no API change; add accessors for the
  internal `tools` and `gate` if not already exported, so
  `mcpserver/palette.go` can introspect.
- `README.md` — Features bullet on bidirectional MCP; Roadmap
  entry deleted.
- `CHANGELOG.md` — new release entry.
- `docs/DESIGN.md` — new "MCP server" section under MCP integration.

## Phased delivery

Three shippable PRs. Each ends at a green state with usable
functionality; the milestone tag lands after #3.

### Phase 1 — Stdio + agent-as-tool

The MVP. Just enough to plug into Claude Desktop.

- `mcpserver/serve.go` + `handler.go` + `agent_tool.go` +
  `stdio.go` + `session.go`.
- `cmd/core-agent/mcpserve.go` with `--tool-name`, `--session-
  mode`, `--progress`.
- Tests covering: tools/list, tools/call success, tools/call
  validation errors, tools/call permission denial as tool error,
  session-per-connection across multiple calls, session-per-call
  isolation.
- `examples/mcp-serve/`.
- Docs: `docs/site/content/docs/mcp-server.md` page with the
  Claude Desktop config snippet.

Smoke test: a real Claude Desktop instance invoking a core-agent
exposed via stdio, asking it a question, getting an answer.

### Phase 2 — Tool-palette mode + elicitation

Round out the exposure modes.

- `mcpserver/palette.go` with the default-deny policy for
  dangerous classes.
- `mcpserver/elicitation.go` with the `Prompter` adapter.
- New CLI flags: `--tool-palette`, `--palette-allow`,
  `--palette-allow-all`.
- Tests covering: palette exposure, default-deny of bash /
  write_file, allowlist override, elicitation request round-trip
  with a fake client.

Smoke test: an MCP client invoking individual tools and getting
permission-prompted via elicitation when the wrapped agent's
gate is in `ask` mode.

### Phase 3 — HTTP transport + httptransport package

The networked mode.

- `httptransport/server.go` + `auth.go`.
- `mcpserver/http.go` wiring the shared listener.
- CLI flags `--http`, `--tls-cert`, `--tls-key`, `--bearer-token-
  file`.
- Tests covering: HTTP request round-trip, bearer auth (success
  + failure), TLS handshake against a self-signed cert, session-
  ID mapping via the `Mcp-Session-Id` header.
- `examples/mcp-serve-http/`.

Smoke test: a remote MCP client (could be a second core-agent
process) calling a networked server over TLS with a bearer
token.

After Phase 3 the docs site picks up the full story, the
CHANGELOG goes out, and bidirectional MCP gets a Features
bullet in the README.

## Open questions and deferred

### Open (decide before Phase 1 PR)

- **Default `UserID` constant.** `"mcp-client"` is the placeholder.
  Anything more specific is misleading without auth wiring. Stay
  with `"mcp-client"` and document it.
- **Should `ProgressOnToolCall` include the tool name in the
  progress message?** Yes — without it the progress is just "tool
  call in flight" which adds little value. Risk: leaking internal
  tool names to a host that didn't need to know them. Mitigation:
  `WithProgressMode(ProgressOpaque)` for "send progress events
  without naming the tool" — name TBD. Defer the opaque variant
  until a consumer asks.
- **What happens when the agent calls `ask_user` (the
  `tools.NewAskUserTool` Prompter) in mode A over stdio?** Stdin
  is owned by the MCP wire; we can't read from it. Default:
  `RefusePrompter` for stdio MCP servers. Document. Use
  `WithElicitationPrompter` for HTTP; for stdio elicitation does
  work over the MCP wire so this is also valid.

### Deferred (no v1 scope; promote when a real consumer hits the seam)

- **Resources and prompts (MCP server primitives beyond tools).**
  Spec defines two more server features. We ship tools only in
  v1. Resources could expose the agent's file context (its
  `read_file` outputs cached as MCP resources for hosts that
  prefer the resource model). Prompts could expose canned
  workflows. Both want a real consumer story before shipping.
- **Per-user gate scoping.** The gate has session-scoped state
  today; adding per-user state changes its shape. Defer to
  whenever per-user policy actually matters.
- **OAuth resource-server conformance.** Bearer + custom-callback
  cover the realistic deployments. Spec-conformant OAuth with
  metadata endpoint is real work and benefits a narrow set of
  hosts.
- **`Mcp-Call-Depth` recursion safety across processes.** Per-
  call budgets are the v1 safety net. Add a depth header when a
  real recursion scenario surfaces.
- **Cross-MCP-boundary cost rollup.** The `_meta.coreagent.usage`
  field surfaces the spend in the response; consumers do the
  rollup. First-class accumulation in a parent `usage.Tracker`
  would require sticking a counter on every `tools/call` and
  summing across calls — defer until a consumer with a real
  multi-agent billing story asks.
- **Reload / hot config update.** `mcp-serve` reads config at
  startup. Reload would require SIGHUP handling that re-builds
  the agent and swaps it in. Same shape as
  `mcp/lifecycle.go`'s "No reload" stance — defer.
- **MCP server as a remote subagent target.** The
  `RemoteAgentSpawner` interface (`agent/remote.go`) could grow
  an "MCP server" implementation: spawn a remote agent by
  connecting to an MCP server, expose its single tool. Trivial
  given both pieces exist; do it when a consumer needs it.
- **Custom resource access for the wrapped agent.** Right now the
  wrapped agent's context comes from its own state. If a host
  wants to push "additional context" into a call (the MCP-host-
  centric way to think about context), we'd want to map that
  onto something the agent can see. Defer until a consumer's
  flow needs it.
