---
title: The attach protocol — why SSE, why capabilities, why /whoami
description: The choice to ship attach as SSE + JSON with capability negotiation, rather than gRPC or a custom framed protocol, turned out to matter far more than the design docs suggested.
template: doc
tableOfContents: true
sidebar:
  order: 3
---

core-agent's *attach mode* is how everything outside the daemon talks to it: the interactive TUI, third-party dashboards, CI tooling, another daemon that wants to peer. It ships as HTTP + Server-Sent Events, framed in JSON, secured with a two-layer auth model, and gated by a set of *capabilities* declared on connect.

None of those choices were obvious at the start. Every one of them has been contested at least once, either by us or by the design reviewers we ran the docs past. This post is about the three that turned out to matter most: **why SSE + JSON**, **why capability negotiation instead of version negotiation**, and **why `/whoami` earned its keep as a first-class endpoint** rather than a diagnostic afterthought.

## Why SSE + JSON, not gRPC / WebSockets / custom framing

The default advice, if you sit down to design a "let external processes talk to my daemon" protocol in 2026, is to reach for gRPC. It's fast, it's typed, the tooling is mature, most infra teams already know it. WebSockets are the runner-up for anything that needs server-push. A hand-rolled length-prefixed frame protocol over TCP is the third option people mention when they want to avoid dependencies.

core-agent's attach protocol is none of these. It is:

- **HTTP for request/response.** `GET /sessions`, `POST /interrupt`, `DELETE /sessions/{sid}`. Standard verbs, standard status codes, standard `Authorization: Bearer` — no custom framing, no length prefixes, no protobuf codegen step.
- **Server-Sent Events for streaming.** `GET /sessions/{sid}/events` returns `text/event-stream`; the client reads a line at a time. Framed as `event: <type>\ndata: <json>\n\n`. One-way (server → client), which is what we actually need — the client sends anything it wants via a separate `POST`.
- **JSON everywhere.** Every request body, every response body, every SSE frame's `data:` payload. No protobuf, no MessagePack, no CBOR.

The reasoning wasn't performance. It was that we needed these five properties, and gRPC forces you to give up several of them:

**1. `curl` is a first-class debugging tool.** Every attach endpoint is reachable from a shell in one line. When the remote TUI is misbehaving and the daemon logs are ambiguous, `curl -N -H "Authorization: Bearer $TOKEN" http://localhost:8080/sessions/abc/events` is the shortest path to "what is the daemon actually sending?" No `grpcurl`, no proto files, no `.desc` reflections. This came up more than we expected during v2.7's OTel wiring — several silent-drop bugs were diagnosed by running `curl /metrics` against the daemon and finding zero span-drop counters (which then led us to the SDK error handler that wasn't installed; [#333](https://github.com/go-steer/core-agent/pull/333)).

**2. HTTP semantics compose with the infrastructure operators already have.** TLS termination is a config change, not a Go rewrite. mTLS is one middleware. Cloud Run's IAP, an ALB with an identity gateway, Cloudflare Access — they all deliver caller identity in headers a Go net/http handler already reads. The [attach HTTP reference](/core-agent/reference/attach-http/) documents *two* orthogonal auth layers (transport bearer + per-caller identity resolution) because both are HTTP-native concepts. Rebuilding that machinery on top of gRPC's metadata layer would have been a project of its own.

**3. Proxies work.** Nginx buffers by default and will silently break SSE if you forget `X-Accel-Buffering: no`. That's a two-line fix. WebSockets through a corporate proxy is a support ticket. Custom TCP is a firewall exception request. SSE over HTTP is not.

**4. One protocol works from every language we care about.** The reference client is Go, but the TUI has been driven from shell scripts, Python, Node, and — at least once — a `fetch` call from a browser dev console. Every one of these speaks HTTP + JSON natively. gRPC clients exist in each of those languages too, but not every one of those clients tolerates the daemon shipping a new proto version, and none of them lets you paste a response body into a bug report as-is.

**5. Server-push without a bidirectional channel.** Attach mode is genuinely asymmetric: the daemon streams events, the client occasionally sends a command. WebSockets solve a problem we don't have (client-initiated streaming) and pay for it with a heavier protocol handshake and a stateful connection to reason about. SSE gets us the one direction we need, on top of a request/response layer, without inventing anything.

The cost is real. JSON over SSE is slower than protobuf over gRPC in every benchmark that measures ns-per-frame; the daemon does not tolerate arbitrary latency because SSE parsers expect line-oriented framing; and reconnection semantics have to be built by hand (the `?since=<int64>` cursor on `/events` is our answer). But *none* of those costs shows up when you're trying to debug why the remote TUI's status bar isn't updating in a demo, and *all* of them are back-loaded to a scaling regime we haven't reached yet. When we hit it, we'll add a binary transport as an option. We won't take away the JSON one.

## Why capabilities, not versions

Here is the naive design: put a version number on the wire. The daemon says `sse_version: 1.3.0`. Clients that speak v1.3 connect; clients that speak v1.2 get an error. When the daemon ships v1.4, older clients keep working at v1.3 by looking at the version and gating on it.

We shipped that. It was wrong.

The problem is that `sse_version` alone doesn't tell you *what* changed. A client shipped last month has no idea whether v1.4 added a new event type it should render, a new field on an existing event it should read, a new slash command it should offer, or something completely orthogonal it should ignore. The version number tells you "there is a difference," not "here is what the difference means for you." So clients either gate too aggressively (refuse to connect against anything not exactly v1.3) or too permissively (assume anything numerically ≥ their compile-time version is fine) — and both are wrong for different real cases.

The v1.4 spec change ([#329](https://github.com/go-steer/core-agent/issues/329), landed in [#344](https://github.com/go-steer/core-agent/pull/344)) replaced the version-as-contract model with a `capabilities` object on the handshake:

```json
{
  "capabilities": {
    "features": ["digest_savings", "peer_hub", "context_stats"],
    "slash_commands": ["compact", "done", "btw", "subagent", "replan"],
    "agent": { "kind": "llmagent", "model": "gemini-3.0-pro" },
    "caller_id": "singh@go-steer.com"
  }
}
```

Four keys, each carrying one specific class of information. Every one is *additive*: consumers tolerate absent keys (they'll get zero-valued semantics), tolerate unknown ones (they'll be ignored), and — critically — tolerate the daemon adding new ones without a version bump. Merge semantics rather than replace: a `status-update` frame that carries `capabilities` overlays the initial handshake's capabilities, so a daemon that hot-swaps a model can announce a new capability set on the next status frame without dropping the connection.

The `sse_version` field still exists — v1.4 today — because operators sometimes need to know exactly what wire format they're negotiating. But it's a *label*, not a *contract*. The contract is the capabilities.

What this bought us in practice:

- **A remote TUI compiled against SSE v1.3 works against a v1.4 daemon** because it looks up `capabilities.slash_commands` to build the palette, rather than assuming the palette from its own version. If a v1.4 daemon adds `/replan` and the v1.3 TUI doesn't know how to render its result, that's a rendering degradation, not a connection error.
- **A `core-agent` daemon rolled forward past a stable `core-agent-tui`** can announce new features (digest savings, peer hub) and the older TUI ignores them cleanly. No coordinated release.
- **Third-party consumers** (dashboards, CI drivers) can be *conservative* on what they consume without being *fragile* on what they connect to.

The failure mode the version-based contract had — pinning entire client fleets to a specific daemon version — was real. We hit it once with a v1.2 → v1.3 upgrade of the SSE spec that quietly required the client to look for `event: typed` frames rather than `event: agent`; the fallback path in [#139](https://github.com/go-steer/core-agent/pull/139) had to tolerate both forms for a release cycle before we could clean it up. The capabilities model would have made that upgrade a one-flag toggle instead of a fallback path.

## Why `/whoami` earned its keep

Attach mode's auth model is deliberately layered. The [attach HTTP reference](/core-agent/reference/attach-http/#auth-model) lays it out:

- **Transport layer** — TLS, optional mTLS, an `X-Attach-Token` bearer, optional read-only mode.
- **Per-caller layer** — a pluggable `auth.Authenticator` resolves an `auth.Caller{Identity, Labels, Admin}` from headers, mTLS client cert, or an `X-Asserted-Caller` header that a proxy has stamped.

The layers are useful precisely because different deployment shapes want different things: single-user local runs (`AnonymousAuth`), token-per-user shared daemons (`BearerTokenAuth`), or IAP/IAM-fronted daemons where the identity is *asserted* by a trusted proxy. Same daemon, three different flavors of "who are you?"

The problem is that operators very frequently *aren't sure* which flavor is active. They know they configured a token; they don't necessarily know the proxy is stripping it and asserting a different identity. They know mTLS is on; they don't necessarily know their client cert didn't verify and they've fallen back to `AnonymousAuth`. Without a way to ask, "what does the daemon think about me?", debugging an auth misconfiguration is a game of reading nginx logs and comparing to daemon logs and hoping the timestamps line up.

`/whoami` is the answer. It's a `GET`. It requires transport auth, but no per-session ACL. It returns:

```json
{
  "identity": "singh@go-steer.com",
  "admin": true,
  "source": "asserted",
  "proxy_by": "iap-service-account@..."
}
```

Four fields:

- `identity` — the resolved caller identity the daemon will use for ACL decisions.
- `admin` — whether that caller has cross-owner privileges.
- `source` — one of `bearer` / `mtls` / `iap` / `asserted` / `anonymous`. Consumers tolerate unknown values so we can add new sources without breaking anyone.
- `proxy_by` — populated only when `source="asserted"`, so operators can tell *which* proxy asserted them.

The endpoint answers three operator questions in one call: "am I authenticated at all?", "am I who I think I am?", and "who's asserting that?". Before it existed, all three took separate config-inspection or log-grepping steps.

There's a small design point worth naming: `/whoami` is not *the* source of identity. The `capabilities.caller_id` field on the SSE handshake is the display hint the TUI uses to render "logged in as X" without a second round-trip. But `/whoami` is the *canonical* source — the SSE hint is a snapshot, and can go stale if the caller's identity changes mid-session (e.g., a token rotation). Consumers that care about correctness re-fetch `/whoami`; consumers that just want to render a badge use the handshake hint. Both work.

## What we'd do differently

Two things, both about scope:

1. **Ship `capabilities` on day one.** We spent the v1.0–v1.3 cycles evolving `sse_version` semantics the hard way. Every backwards-compat pain point in that period would have been simpler if we'd started with a capabilities object and let `sse_version` be a label rather than a contract from the beginning. If you're designing a client/server protocol between components that ship on different cadences, don't wait for the second version to introduce capability negotiation.
2. **Document the `curl` recipes alongside the reference.** The [attach HTTP reference](/core-agent/reference/attach-http/) documents every endpoint's request/response shape. What it *should* have from the start is a "debugging with curl" section — the specific one-liners that answer the specific operator questions ("who am I to this daemon?", "what's the last 10 events?", "is the metrics endpoint alive?"). We wrote those recipes in Slack conversations before writing them down. Every one saved someone an hour the second time it was needed.

The through-line: **protocol design isn't about picking the fastest wire format; it's about picking one where the operator doesn't need to install anything to debug it.** SSE + JSON with capabilities and `/whoami` isn't the theoretical optimum. It's the one that stays legible when the demo breaks.
