# Attach mode: live-tail + inject for headless agents

Design doc for the next minor (working name `v1.5.0`). Untracked
sibling to `docs/background-subagents-design.md`,
`docs/scion-harness-improvements-design.md`,
`docs/cogo-core-agent-integration.md`,
`docs/docsy-migration-notes.md`.

## Context

When core-agent is the substrate inside a Cloud Run service, a K8s
pod, or a distroless container with no shell, the operator's only
window into "what is the agent doing right now?" is the eventlog
SQL — and only if `--session-db` was enabled and the DB is reachable
from the operator's laptop. Even then it's a poll loop, not a tail.
Distroless deployments don't have `kubectl exec` as a fallback.

We already shipped the two primitives this requires:

- **Live event tail** via `eventlog.Stream.Watch(seq)`
  (`eventlog/eventlog.go:69`) — emits every event the runner appends,
  in monotonic seq order, blocking for new rows.
- **Soft interrupt** via `Agent.Inject(message)`
  (`agent/inbox.go:164`) — queues a message that the next turn's
  pre-prompt prepend picks up.

Attach mode is the small piece that exposes both over a remote
transport so an operator on a laptop can watch a production run and
nudge it back on track without restarting the pod or SSH-ing in.
This is the analog of `tmux attach` for a headless agent.

### Settled decisions (from earlier design conversations — do not relitigate)

- **Transport: HTTP + Server-Sent Events.** Not gRPC, not raw TCP.
  Single primitive serves the bundled CLI attach client *and* a
  future web UI. K8s-native (works through ingress controllers,
  through `kubectl port-forward`, through service meshes that speak
  HTTP). Stdlib-only on the server side (`net/http`).
- **Unix socket as a local-dev convenience.** Same SSE protocol
  over both transports. The `attach` client picks based on URL
  scheme — `unix://`, `http://`, `https://`.
- **Endpoints**:
  - `GET /sessions` — list active sessions
  - `GET /sessions/<id>/events` — SSE stream; supports `?since=N`
    for replay before live-tail
  - `POST /sessions/<id>/inject` — call `Agent.Inject`
- **Auth: mTLS primary, bearer token fallback.** Cert / key / CA
  files mounted from disk by the operator; no in-process cert
  generation. v1: if mTLS validates, both read and write are
  granted (no scope tokens yet). `--attach-readonly` disables
  `POST /inject` entirely regardless of auth.
- **Client UX: tmux-style.** `core-agent attach <url>` is a passive
  watch by default; pressing `:` drops into an input line that
  POSTs to `/inject`. A standalone scriptable `core-agent inject`
  form is deferred.
- **CLI subcommands**: `core-agent attach <url>`,
  `core-agent ls <url>`.
- **Agent-side flags** (already named, not yet implemented):
  ```
  --attach-listen=:7777
  --attach-tls-cert=path
  --attach-tls-key=path
  --attach-client-ca=path
  --attach-token=$ENV
  --attach-readonly
  ```

## Goals and non-goals

### Goals

- **Make running agents observable from outside the container.** An
  operator with network reach to the pod (mesh, port-forward, or
  ingress) can see the same chat-style stream a local REPL would
  show, with the same `→ tool(...)` / `← tool(...)` formatting.
- **Make running agents nudgeable from outside the container.**
  Production deployments that hit a model loop or a stuck tool
  cycle can get a message into the next turn without bouncing the
  pod.
- **Survive operator disconnect.** Closing the attach client must
  not affect the agent. Reconnecting and replaying recent history
  via `?since=N` must be lossless when the agent is running with a
  durable eventlog.
- **Stay opt-in and additive.** A binary built without any attach
  flags behaves identically to today's binary. The attach package
  is an importable library piece so consumers building their own
  daemons can wire it in directly.
- **Match the existing observability surface.** SSE event shape is
  the JSON serialization of `*session.Event` already used by the
  eventlog overlay. No new event type, no parallel format.

### Non-goals

- **Not a multi-tenant control plane.** No project / namespace
  hierarchy, no per-user RBAC. A single attach listener serves one
  binary's set of sessions.
- **Not a replacement for the audit log.** Attach is the live view;
  the eventlog is the durable record. Operators querying historical
  state still use SQL or a future eventlog HTTP read API.
- **Not a write-anything API.** The only write is `/inject`. No
  "create session", no "edit config", no "kill turn", no
  "send tool result." Adding those means more endpoints and more
  auth surface; defer until a concrete consumer asks.
- **Not a session migration mechanism.** A reconnecting client gets
  the same session's events; we don't move work between agents.
- **Not a TUI.** The attach client renders chat lines to stdout the
  way `WriteEvents` does. Bubble Tea is still consumer territory
  (see `docs/cogo-core-agent-integration.md`).
- **Not a protocol negotiation surface.** v1 wire format is SSE
  with one event type. No version handshake, no content
  negotiation. We bump a `?v=` query param if we ever break shape.

## Architecture

```
┌───────────────────────────────── agent process ─────────────────────────────────┐
│                                                                                 │
│   agent.Agent ──▶ runner.Run() ──▶ session.Service ─┬─▶ ADK events table        │
│        ▲                                            └─▶ agent_eventlog overlay  │
│        │                                                       │                │
│        │ Inject(msg)                                            │ Watch(seq)    │
│        │                                                       ▼                │
│   ┌────┴───────────────────────────────────────── attach.Server (HTTP)  ──┐     │
│   │  GET  /sessions                  → registry.List()                    │     │
│   │  GET  /sessions/<id>/events?since=N  → fan-out from eventlog.Watch    │     │
│   │  POST /sessions/<id>/inject       → registry.Lookup(id).Inject(...)   │     │
│   └────────────────────────────────────────────────────────────────────────┘    │
│        ▲             ▲                                                          │
└────────┼─────────────┼──────────────────────────────────────────────────────────┘
         │ TLS / mTLS  │ HTTP over unix socket
         │             │
   ┌─────┴─────┐   ┌───┴────────────┐
   │ operator  │   │ local dev      │
   │ laptop    │   │ (same host)    │
   └───────────┘   └────────────────┘
       │
   core-agent attach https://agent.prod.svc/  (or unix:///run/agent.sock)
       └─▶ SSE → runner.WriteEvents-shaped print
       └─▶ `:` → POST /sessions/<id>/inject
```

The server lives in a new top-level `attach/` package. It depends on
`agent` (for `*Agent.Inject` / accessors) and `eventlog` (for
`Stream.Watch` / `Stream.Since`). Nothing in `agent/`, `eventlog/`,
or `runner/` depends on `attach/` — the wiring is one-way and the
bundled CLI is the only place the two come together.

## Wire protocol

### SSE framing

One event per `event:` block. We use the standard SSE shape:

```
event: agent
id: 42
data: {"seq":42,"event":{ /* JSON-encoded *session.Event */ }}

event: heartbeat
data: {"now":"2026-05-19T14:03:11Z"}

event: end
data: {"reason":"session-completed"}
```

- `event: agent` — payload is one `Frame` (defined below); `id:` is
  the eventlog seq, so a disconnecting client knows what to pass as
  `?since=N` on reconnect. Standard SSE clients also expose this as
  the `Last-Event-ID` request header on automatic reconnect, which
  the server treats as a `?since=` shortcut.
- `event: heartbeat` — emitted every 15s when the stream is idle.
  Keeps intermediaries (ingresses, K8s networking) from closing
  the connection. No payload semantics; clients ignore it past the
  fact of "we're still connected."
- `event: end` — terminal frame, no further events on this
  connection. `reason` ∈ `{session-completed, server-shutdown,
  unauthorized, session-unknown}`. Clients exit non-zero on
  `unauthorized` / `session-unknown`, exit zero on
  `session-completed`, retry with backoff on `server-shutdown`.

### Frame shape (the JSON inside `event: agent`)

```jsonc
{
  "seq": 42,                              // monotonic from eventlog
  "event": {                              // *session.Event JSON
    "id": "a1b2c3...",                    // ADK event ID
    "invocationId": "inv-...",            // turn-scoped ID
    "timestamp": "2026-05-19T14:03:10.123Z",
    "author": "core-agent",               // or "user", tool name, "watch-prod/alert", etc.
    "branch": "",                         // "" for parent; "<parent>.<sub>" or "bg.<sub>" for subagents
    "partial": true,                      // mid-turn assistant text delta
    "content": {                          // *genai.Content
      "role": "model",
      "parts": [
        {"text": "Looking at the pod logs..."},
        {"functionCall": {"name": "bash", "args": {"command": "kubectl logs ..."}}},
        {"functionResponse": {"name": "bash", "response": {"stdout": "...", "exitCode": 0}}}
      ]
    },
    "groundingMetadata": { /* Gemini server-side tool evidence; preserved verbatim */ }
  }
}
```

The wire format is the JSON encoding of ADK's `session.Event`
verbatim — same shape the eventlog's overlay table stores per row,
same shape a future `GET /eventlog/...` read API would emit. The
attach server does not project, filter, or rename fields. Reasons:

- **One serialization, one place to maintain.** Diverging from
  the eventlog's encoding means two JSON shapes to keep in sync as
  ADK adds fields (vision parts, code-execution results, citations).
- **Lossless replay.** A client that misses an event and reconnects
  with `?since=N` must see exactly the same bytes the original
  stream would have shown.
- **No new "v1" obligations.** We're piggybacking on whatever
  forward-compat story the eventlog already has.

The minimum subset clients must handle to render chat-style output:
`author`, `partial`, `content.role`, `content.parts[].text`,
`content.parts[].functionCall.{name,args}`,
`content.parts[].functionResponse.{name,response}`. Everything else
(`branch`, `groundingMetadata`, `actions`, `longRunningToolIDs`,
`invocationId`, `id`, `timestamp`) is preserved verbatim and made
available to richer consumers (a future web UI, log shippers) but
the bundled `attach` CLI uses only the minimum subset on its first
pass.

### Request envelope for `POST /inject`

```jsonc
// Request
{
  "message": "switch to read-only mode — incident response in progress"
}

// 202 Accepted
{
  "queued": true,
  "queueDepth": 3        // current depth including the just-queued message
}

// 4xx error
{
  "error": "session-unknown",
  "message": "no active session matches id=ghi789"
}
```

Error codes: `session-unknown` (404), `readonly` (403),
`inbox-closed` (409), `bad-request` (400 — empty message, payload
too large). 5xx for unexpected.

`queueDepth` is best-effort; the inbox doesn't currently expose
depth so this is a v1 estimate (insertions since the last drain).
Treat as informational; do not script on its exact value.

## Server-side architecture

### Package layout

```
attach/
  server.go          # http.Server lifecycle, listener wiring, TLS construction
  handlers.go        # GET /sessions, GET /sessions/<id>/events, POST /sessions/<id>/inject
  sse.go             # SSE framing + heartbeat + flush plumbing
  registry.go        # SessionRegistry + Source interface
  fanout.go          # one Watch goroutine per session, N subscriber chans
  auth.go            # mTLS configuration + bearer token check
  audit.go           # structured log lines per attach request (peer identity)
  attach.go          # public API: attach.New(opts...), attach.Serve
  attach_test.go
  ...
```

`attach.New(...)` returns a `*Server` that:

- holds a `SessionRegistry` (described below) the operator's binary
  populates,
- builds an `*http.Server` with the TLS config implied by the
  options,
- exposes `Serve(ctx context.Context, ln net.Listener) error` and
  `Shutdown(ctx context.Context) error`.

Wiring from `cmd/core-agent/main.go`:

```go
if attachListen != "" {
    reg := attach.NewStaticRegistry(ag)        // single-session
    srv, err := attach.New(
        reg,
        attach.WithTLSCert(certFile, keyFile),
        attach.WithClientCA(caFile),           // optional; enables mTLS
        attach.WithBearerToken(os.Getenv("ATTACH_TOKEN")),
        attach.WithReadOnly(readOnly),
    )
    if err != nil { /* ... */ }
    ln, _ := net.Listen("tcp", attachListen)   // or unix
    go srv.Serve(ctx, ln)
    defer srv.Shutdown(shutdownCtx)
}
```

### Why a new package and not `runner/`

`runner/` is presentation glue — `WriteEvents`, the REPL, the
headless one-shot. Putting an HTTP server there would tangle three
concerns. `attach/` keeps the responsibility singular: convert
runtime state (registry + eventlog) into an HTTP surface.

### Why `attach/` doesn't import `runner/`

The SSE format is a JSON encoding, not the chat-style printer. The
attach client (under `cmd/core-agent` for the bundled binary) is
what calls `runner.WriteEvents`-style rendering after decoding
frames. Keeping `attach/` import-free of `runner/` means a consumer
embedding only the server side doesn't pay for the printer's
ANSI handling and `golang.org/x/term` dep.

### Session registry

```go
// attach/registry.go
type SessionRegistry interface {
    List(ctx context.Context) ([]SessionInfo, error)
    Lookup(ctx context.Context, id string) (Source, error)
}

type SessionInfo struct {
    ID        string            // stable handle, used in URL
    AppName   string
    UserID    string
    SessionID string             // ADK session id
    StartedAt time.Time
    Labels    map[string]string  // free-form, surfaced in GET /sessions
}

type Source interface {
    // Info returns the SessionInfo this Source publishes under.
    Info() SessionInfo
    // Watch returns an iter over events with seq > fromSeq, in seq
    // order, blocking for live ones. Equivalent shape to
    // eventlog.Stream.Watch but bound to one session.
    Watch(ctx context.Context, fromSeq int64) iter.Seq2[eventlog.Entry, error]
    // Inject delivers msg to the underlying agent's inbox. Returns
    // ErrReadOnly if the source is in read-only mode.
    Inject(ctx context.Context, msg string) error
}
```

Two reference implementations ship in `attach/`:

- `NewStaticRegistry(ag *agent.Agent)` — one agent, one session ID;
  the bundled CLI uses this. The session's `ID` is derived from
  `appName + "/" + userID + "/" + sessionID` so it's stable across
  restarts (handy for SSE reconnect).
- `NewMultiRegistry()` — a thread-safe map-backed registry library
  consumers populate with `reg.Add(source)` / `reg.Remove(id)` when
  they run many agents in one process (the Scion harness, the
  `BackgroundAgentManager`'s subagents if a consumer ever wanted
  to expose them).

The registry exists because `attach/` cannot reasonably reach
inside the runner to enumerate live agents — that knowledge lives
in whatever code constructed the agent(s). Forcing the operator's
binary to publish them keeps `attach/` agnostic about how many
agents exist and how they're identified.

### Where session IDs come from

The ID exposed in URLs is the registry's `SessionInfo.ID`, which is
deliberately a different namespace from ADK's `(appName, userID,
sessionID)` triple. The registry can choose any opaque-looking
encoding it likes (`StaticRegistry` uses base64url of the triple);
clients shouldn't parse it.

Why decouple: ADK session IDs can contain characters that would
need URL-encoding, and consumers may want to expose human-readable
names ("watch-prod-cluster") that don't map 1:1 onto session IDs.
The registry is the seam.

## Fan-out

One eventlog `Watch` per session is enough to serve N attached
clients. Multiple `Watch` goroutines per session would multiply
DB load (each polls every 200ms by default) for no gain.

```
                                        ┌──▶ subscriber chan (client A)
eventlog.Watch ──▶ broadcaster goroutine┼──▶ subscriber chan (client B)
                                        └──▶ subscriber chan (client C)
```

### Broadcaster shape

```go
// attach/fanout.go (sketch)
type broadcaster struct {
    src         Source
    mu          sync.Mutex
    subs        map[*subscriber]struct{}
    started     bool
}

type subscriber struct {
    ch       chan eventlog.Entry  // buffered; bufSize = 64
    fromSeq  int64                // requested ?since=
    slow     int32                // count of drops; >threshold → kick
}
```

First `Subscribe()` on a broadcaster spins up the Watch goroutine.
Last `Unsubscribe()` cancels it. The broadcaster delivers each
entry into each subscriber's channel with a **non-blocking send**:

- If the channel has capacity, the entry lands and the subscriber
  catches up in real time.
- If the channel is full (subscriber's HTTP write is slow), the
  entry is dropped on the floor for that subscriber, `slow` is
  incremented, and a metric is logged.
- After `maxSlowDrops` consecutive drops (default 32), the
  subscriber is forcibly closed with `end: server-shutdown` (so its
  client triggers reconnect with `Last-Event-ID`, gets the missed
  events via `?since=`, and resumes).

This is the same pressure-relief pattern the `BackgroundAgentManager`
alert channel uses (drop-oldest at the producer for alerts; here
we drop-newest at the per-subscriber buffer with a reconnect
fallback). Drop-newest is the right policy at the fan-out layer
because the durable eventlog *is* the canonical store — anything
dropped is recoverable on reconnect via `?since=`. Drop-oldest in
memory would let a slow attacher silently mask events from a
better-behaved sibling subscriber on the same broadcaster.

### Why we don't share one buffered channel across subscribers

Easier to reason about; one slow subscriber doesn't punish the rest.
The buffer-per-subscriber cost is small (64 * sizeof(*Entry) ~ 2KB)
relative to one durable Watch.

### Replay → live transition

The first `?since=N` request on a fresh subscription drains
`Stream.Since(fromSeq)` (a bounded iterator that returns when
caught up) before joining the broadcaster's live fan-out. The
broadcaster delivers entries starting from the *current* tail, so
there's a small race window where an entry could land in both the
replay and the fan-out. Each subscriber tracks the highest seq it
has emitted to its SSE writer and skips any duplicates from the
fan-out hand-off. Cheap; one int64 per subscriber.

## Replay and resume

### `?since=N` semantics

- `?since=0` (or unset) → start from current tail (live only,
  matches `tmux attach`'s "show me what's happening now" default).
- `?since=N` (N > 0) → drain `Stream.Since(N)` first, then join
  live. Caller is responsible for picking N — typically the seq of
  the last event the client acknowledged.
- `Last-Event-ID: <N>` HTTP header (SSE-standard, set automatically
  by browsers + most SSE client libs on reconnect) → treated as
  equivalent to `?since=N`. An explicit `?since=` wins if both are
  present.

### When the agent has no `--session-db`

`Stream.Since` is meaningless without a durable backend. Two
choices were considered:

1. **In-memory ring buffer in `attach/`.** A `RingBuffer` of recent
   N events (default 1024) kept by the broadcaster. Replay reads
   from the ring; anything older returns a partial-history marker.
2. **Refuse `?since=` requests when the source isn't backed by an
   eventlog.**

We picked **option 1** for live tail and **option 2 below the
ring's window**. Rationale:

- The 90% use case for `?since=` is reconnect-within-seconds, which
  a ring buffer covers comfortably with no DB dependency.
- Forcing every operator to enable `--session-db` for attach to
  work would couple two features that should be independent.
  Operators may want attach in dev (memory-only) and durable logs
  in prod (`--session-db` + remote DB).
- The ring lives inside the broadcaster, sized in events (not bytes
  or duration), and pruned FIFO. Adding seq numbers to the ring is
  cheap: we synthesize seq from a monotonic counter when the
  source has no eventlog, namespaced per-session so different
  sources can have overlapping seqs without confusion.

When a client requests `?since=N` and N predates the ring's oldest
entry (or the durable log's truncation horizon, if one is added
later), the server responds with one `event: gap` frame:

```
event: gap
data: {"from":N,"to":M,"reason":"before-buffer-window"}
```

…then continues from M (the oldest available seq). Clients
displaying chat output render the gap as a `── … missed N events
──` separator. Scripts consuming events programmatically decide
what to do (warn, fail, refetch from the audit log).

## Session discovery

```
GET /sessions

200 OK
{
  "sessions": [
    {
      "id": "Y29yZS1hZ2VudC9sb2NhbC9kZWZhdWx0",
      "appName": "core-agent",
      "userId":  "local",
      "sessionId": "default",
      "startedAt": "2026-05-19T13:55:02Z",
      "labels": {"role": "primary"}
    }
  ]
}
```

The bundled CLI has one agent → one session. `GET /sessions`
returns a single entry; `core-agent ls <url>` formats it as a
table.

Library consumers running multiple agents in one process (a
mini-orchestrator, a multi-tenant service) call
`reg.Add(SourceFor(ag, labels))` per agent. The labels map is
free-form metadata the operator's binary chooses — `role`,
`tenant`, `task-id`, whatever surfaces usefully in `ls`. No
authorization is applied at the registry layer; if the operator
authenticated they see every session the registry exposes.
Per-session ACLs are a deferred item (see Open questions).

## Inject endpoint shape

Already sketched above. Two more points worth pinning:

- **Inject is fire-and-queue, not synchronous.** 202 lands as soon
  as the inbox accepts the push (≪1ms in steady state). The
  response does *not* wait for the message to be consumed by a
  turn. There is intentionally no "wait for the model to
  acknowledge" mode — that's a structured-inject feature deferred
  to a later release (see Open questions).
- **Agent busy is not an error.** Pre-v1.3 you might have wondered
  what happens if the agent is mid-turn. With `Agent.Inject` the
  answer is "queued, drained pre-turn next time." So 202 is the
  only happy-path response code; the readonly / unknown / closed
  cases above are the only sad-path codes.

The HTTP body cap on `/inject` is 8 KiB (configurable via
`attach.WithInjectMaxBytes`). Larger payloads return 413. Rationale:
inject messages are human-typed nudges, not RAG documents; we want
to fail fast on obvious misuse.

## Security model

### TLS construction

```go
// attach/auth.go (sketch)
func buildTLSConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
    cert, err := tls.LoadX509KeyPair(certFile, keyFile)
    if err != nil { return nil, fmt.Errorf("server cert: %w", err) }
    cfg := &tls.Config{
        Certificates: []tls.Certificate{cert},
        MinVersion:   tls.VersionTLS13,        // hard floor; no negotiation
        ClientAuth:   tls.NoClientCert,
    }
    if clientCAFile != "" {
        caPEM, err := os.ReadFile(clientCAFile)
        if err != nil { return nil, fmt.Errorf("client CA: %w", err) }
        pool := x509.NewCertPool()
        if !pool.AppendCertsFromPEM(caPEM) {
            return nil, errors.New("client CA: no certs parsed")
        }
        cfg.ClientCAs = pool
        cfg.ClientAuth = tls.RequireAndVerifyClientCert
    }
    return cfg, nil
}
```

Key choices:

- **`tls.RequireAndVerifyClientCert`** — not `VerifyClientCertIfGiven`.
  If the operator supplies a CA they want enforced mTLS, period;
  a client that fails verification is rejected at the TLS handshake.
- **TLS 1.3 floor.** No reason to negotiate older versions in 2026;
  every modern client supports it; older versions are a known foot-
  gun surface.
- **No in-process cert generation.** Operators mount certs from
  cert-manager / Vault / their issuance pipeline of choice. We
  don't pretend to be a CA. Self-signed dev certs are an operator
  responsibility (`mkcert`, `openssl req`, etc.).
- **Unix sockets bypass TLS entirely.** Filesystem permissions are
  the auth boundary; if you can read the socket, you have access.
  Document the implied `chmod 0600 /var/run/agent.sock` pattern.

### Auth precedence

Per request:

1. If the listener was constructed with a client CA AND the TLS
   handshake completed with a verified client cert → authenticated,
   identity = cert subject CN + SANs.
2. Else if the request has `Authorization: Bearer <token>` AND the
   token matches `--attach-token` exactly (constant-time
   compare) → authenticated, identity = `bearer:token`.
3. Else if the listener was constructed with **neither** client CA
   nor bearer token AND is bound to a unix socket → authenticated,
   identity = `unix:<peer-uid>` (read from `SO_PEERCRED` on Linux).
4. Else → 401 Unauthorized, connection closed.

The unix-socket case (3) is the "I'm hacking on my laptop" path:
no certs, no tokens, just `core-agent --attach-listen=unix:///tmp/a.sock`.
Refusing to start with no auth on a TCP listener is intentional —
unauth'd TCP exposes the inject endpoint to anyone on the network,
which is a foot-gun we don't want to ship by default.

### Read vs. write authorization

v1 grants both read and write to any authenticated principal. The
operator-side flag `--attach-readonly` is the hard switch that
denies writes entirely (the `POST /inject` handler returns 403
regardless of who is asking). This handles the production case
"alerting can attach, nobody can inject without bouncing the
deployment to a non-readonly variant."

Fine-grained scope tokens (a token that grants read only, or
read + inject only on session X) are explicitly deferred to a
follow-up — see Open questions. The principle: ship the coarsest
useful axis (`--attach-readonly`) first, build scope-token machinery
only when a consumer's deployment actually needs it.

### Audit

Every attach event lands in two places:

- **The agent's audit log (eventlog).** Inject messages already
  show up there via the normal `[Inbox]` prepend on the next turn.
  We additionally emit a synthetic `Author="attach"` event the
  moment `/inject` is called, with `content.parts[0].text` set to
  the message body and `branch=""`. This makes the inject visible
  at request time in the audit trail, not only when the next turn
  consumes it.
- **The attach server's structured log.** Every authenticated
  request emits one log line (slog, INFO level):
  ```
  attach: request method=POST path=/sessions/abc/inject
    peer=cert:CN=alice@example.com,SAN=spiffe://prod/ops/alice
    session=core-agent/local/default queued=true
  ```
  Failed auth emits at WARN with the failure reason but never the
  presented token / cert subject (to avoid logging secrets back).

This split — audit log captures *what changed in the agent*, server
log captures *who asked* — keeps each system answering one
question. Cross-referencing happens by timestamp + session ID.

## Operator UX walkthroughs

### Laptop dev with a Unix socket

```bash
# Terminal 1
core-agent --attach-listen=unix:///tmp/agent.sock --yolo
> what's 2+2?
4
> _

# Terminal 2 (no flags — defaults to passive watch)
core-agent attach unix:///tmp/agent.sock
↪ connected to session core-agent/local/default (seq=0)
... live events stream as Terminal 1 chats ...
:                              # press `:` to enter inject mode
> kindly switch to base 16
↪ queued (seq=42)
... watch the model's response in Terminal 1 ...
```

The `attach` client uses the same `WriteEvents`-shaped renderer as
the REPL — colored partial text, cyan `→/←` tool calls, magenta
`↪` for server-side built-ins.

### K8s with `kubectl port-forward`

```yaml
# pod spec excerpt — attach over loopback inside the pod
args:
  - "--attach-listen=:7777"
  - "--attach-tls-cert=/etc/attach/tls.crt"
  - "--attach-tls-key=/etc/attach/tls.key"
  - "--attach-client-ca=/etc/attach/ca.crt"
  - "--session-db"
volumeMounts:
  - { name: attach-tls, mountPath: /etc/attach, readOnly: true }
```

```bash
# Operator's laptop
kubectl port-forward pod/agent-7c4f-x9 7777:7777
core-agent attach https://localhost:7777/ \
  --tls-cert ~/.attach/me.crt \
  --tls-key  ~/.attach/me.key \
  --tls-ca   ~/.attach/cluster-ca.crt
```

`kubectl port-forward` terminates inside the pod's net namespace,
so TLS / mTLS works end-to-end — `localhost:7777` from the laptop's
perspective is the pod's `:7777` listener.

### Production through an ingress

```
operator laptop ──TLS──▶ ingress ──TLS──▶ pod :7777
```

Two TLS hops. The outer one (operator → ingress) uses whatever the
ingress is configured for (typically a Let's Encrypt cert and a
bearer token at the ingress's auth layer, or mTLS through SPIFFE
if there's a cluster identity service). The inner one (ingress →
pod) is the attach server's own mTLS. End-to-end mTLS through the
ingress is possible if the ingress passes the client cert through
in a header — `attach/` reads `X-Forwarded-Client-Cert` (Envoy's
canonical format) when behind a trusted proxy, but this requires
explicit opt-in via `attach.WithTrustedProxy(cidr)` to avoid
spoofing.

The bundled CLI's `attach` client accepts a bearer token via
`--token=$ATTACH_TOKEN` or `--token-file=/path` for the ingress-
fronted setups that gate on tokens rather than mTLS.

## Testing strategy

Three layers, none requiring a real LLM:

### Unit — `attach/*_test.go`

Standard table-driven tests with `httptest.NewServer` /
`httptest.NewTLSServer`:

- Auth precedence (mTLS-only, token-only, both, neither on TCP →
  startup error, neither on unix → ok).
- SSE framing (heartbeat cadence, `id:` matches frame seq,
  `event: end` on shutdown).
- `?since=N` interaction with the ring buffer (replay drains
  before live, gap frame on out-of-window N).
- Fan-out (3 subscribers on one Source see identical streams; one
  slow subscriber doesn't punish the others; slow-drops escalate
  to forced disconnect after threshold).
- Inject (202 on happy path; 403 readonly; 404 unknown session;
  413 oversized body; 409 inbox-closed).

### Integration — `attach/integration_test.go`

Live `*agent.Agent` constructed against `models/mock/echo`
(`--provider=echo`), wired to a `StaticRegistry`, attached over an
ephemeral unix socket:

```go
ag, _ := agent.New(echoModel, agent.WithInstruction("..."))
reg := attach.NewStaticRegistry(ag)
srv, _ := attach.New(reg)
ln, _ := net.Listen("unix", filepath.Join(t.TempDir(), "a.sock"))
go srv.Serve(ctx, ln)

// Drive a turn on a goroutine; observe via attach.
go func() { drainEvents(ag.Run(ctx, "ping")) }()

client := attach.Dial(t, ln.Addr())
frames := client.Stream(ctx, sessionID)
require.Contains(t, drainText(frames), "ping")   // echo provider replies

require.NoError(t, client.Inject(ctx, sessionID, "now do something else"))
// Drive next turn; verify the inject lands in the prompt prepend.
```

The echo and scripted providers (`models/mock/`) plus a fixture
JSONL transcript exercise the full path — multi-turn, tool calls,
partial text — with no API key. CI runs this on every PR.

### End-to-end smoke

A `dev/smoke/07-attach.sh` script (idempotent, credential-free)
that:

1. Builds the binary.
2. Starts `core-agent --provider=scripted --script=fixture.jsonl
   --attach-listen=unix:///tmp/test.sock` in the background.
3. Runs `core-agent attach unix:///tmp/test.sock` non-interactively
   for 5 seconds, capturing stdout.
4. Asserts the captured output contains the expected scripted
   reply.
5. Runs `core-agent ls unix:///tmp/test.sock`; asserts one row.
6. POSTs an inject via `curl --unix-socket`; asserts 202.
7. Tears down.

## Open questions and deferred items

- **Cert rotation.** Today the server reads cert + key once at
  start. A SIGHUP handler that re-loads from disk would let
  cert-manager hot-rotate without a restart. Trivial to add; defer
  until a deployment hits the limitation. Mitigation in the
  meantime: the pod's existing restart cycle plus cert overlap
  windows.
- **Scope tokens for finer-grained auth.** `--attach-readonly` is
  binary. A future "this token grants read on session X but no
  inject" or "this token grants read across all sessions but
  inject only on those labelled `role=test`" requires a token
  format with scopes (JWT? Macaroon? Plain HMAC-signed JSON?). No
  consumer has asked yet; designing the format prematurely is the
  wrong direction. The seam: `Source.Inject` already takes the
  request's auth identity (via `ctx`), so scope enforcement lands
  inside the registry without changing the HTTP surface.
- **Structured inject responses with confirmation.** Today inject
  is fire-and-queue. A "wait for the model to acknowledge / refuse
  this message" mode would need a correlation ID on the inject,
  the agent to surface a structured ack tool call, and the server
  to hold the HTTP response open until the ack arrives. Useful for
  orchestrators that want exactly-once delivery semantics; not
  useful for the operator-laptop case. Defer.
- **Scriptable `core-agent inject` CLI form.** `core-agent attach`
  is interactive; a one-shot `core-agent inject <url> -m "msg"`
  would compose with shell pipelines. The handler already exists;
  the CLI surface is the only piece missing. Ship in a follow-up
  release when a real consumer asks.
- **Web UI as a sibling client.** The wire format is JSON; a small
  React / vanilla-JS page using the browser's built-in
  `EventSource` could render the same stream. Punted until the
  daemon side ships and we know what the operator's mental model
  is in practice.
- **`Stream.Watch` from a registry that exposes many sessions.**
  When `NewMultiRegistry` holds dozens of agents, we instantiate
  one broadcaster per attached session, not eagerly per registered
  session. Lazy is the right default; eagerly polling unobserved
  sessions costs DB cycles for nothing.
- **Inject delivery to autonomous handles.** `AutonomousHandle.Inject`
  already exists (`agent/autonomous_handle.go`). The registry's
  default `StaticRegistry` constructs from `*Agent`; consumers with
  an `*AutonomousHandle` will need a `SourceFromAutonomousHandle`
  helper. Trivial; ship in the same release as the rest.
- **Bandwidth caps per subscriber.** Partial text events on a long
  Claude/Gemini reply can chunk fast. We don't currently cap how
  many SSE frames per second a subscriber sees. If a real
  deployment hits this, the right answer is server-side rate
  limiting on the broadcaster, not protocol changes.
- **TLS over unix sockets.** Pointless; filesystem perms are the
  auth boundary. We deliberately skip the TLS handshake on unix
  listeners. Document, don't relitigate.

## Implementation milestones

Three shippable PRs. Each ends with green presubmits and a
demonstrable end-to-end path.

### PR 1 — `attach/` package + read-only HTTP server

**New:**

- `attach/server.go`, `attach/handlers.go`, `attach/sse.go`,
  `attach/registry.go`, `attach/fanout.go`, `attach/auth.go`,
  `attach/attach.go`
- `attach/server_test.go`, `attach/fanout_test.go`,
  `attach/auth_test.go`, `attach/integration_test.go`

**Surface:**

- `attach.New(reg, opts...) (*Server, error)` —
  `WithTLSCert/WithClientCA/WithBearerToken/WithReadOnly/WithInjectMaxBytes/WithBufferSize`.
- `attach.NewStaticRegistry(*agent.Agent)`,
  `attach.NewMultiRegistry()`.
- `attach.SourceFromAgent(*agent.Agent, ...labels)`.
- `GET /sessions` + `GET /sessions/<id>/events` + auth.
- No `POST /inject` yet.

**Scope:** No CLI changes. Library consumers can already wire
attach into their own daemons. Verified by integration test
against the echo provider.

### PR 2 — `POST /inject` + CLI `attach` and `ls` subcommands

**New:**

- `attach/handlers.go` — inject handler with all the error codes
  enumerated above.
- `cmd/core-agent/attach.go` — the `attach` and `ls` subcommands;
  argument parsing (URL → transport selection), TLS client config
  builder, SSE reader, tmux-style `:` input mode, `WriteEvents`-
  style renderer.
- `cmd/core-agent/main.go` — `--attach-listen` /
  `--attach-tls-cert` / `--attach-tls-key` / `--attach-client-ca` /
  `--attach-token` / `--attach-readonly` flags; spin up the
  attach server when `--attach-listen != ""`.
- `dev/smoke/07-attach.sh`.

**Verified:**

- Full unit + integration suite green.
- Smoke script passes (scripted provider, unix socket, full
  attach + inject round trip, no API key required).
- Manual: `kubectl port-forward` → mTLS → live tail + inject against
  a real Gemini-backed agent (documented in CHANGELOG, not gated
  in CI).

### PR 3 — Docs and a worked example

**New:**

- `docs/site/content/docs/attach.md` — operator-facing page
  covering the three walkthroughs (laptop unix, K8s
  port-forward, ingress mTLS) plus the CLI subcommand reference.
- `examples/attach-server/main.go` — minimal example showing a
  library consumer constructing a registry with two agents and
  exposing them via attach.
- `docs/site/content/docs/library-api.md` — new "Attach"
  subsection cross-linking to the operator page.
- `CHANGELOG.md`, `README.md` — feature bullet under the
  observability heading.

This PR is doc-only modulo the example, so it can land
independently and doesn't block a release tag if PR 2 needs more
review time.
