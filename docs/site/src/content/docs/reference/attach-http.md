---
title: Attach mode HTTP endpoints
---


HTTP/SSE protocol reference for the attach listener — the surface `core-agent-tui`, third-party dashboards, and CI tooling call. The daemon exposes this when launched with `--attach-listen=:<port>` (or the `attach.listen` config field).

This page is the wire-level reference: paths, request/response shapes, auth requirements, status codes, and idempotency semantics. For **why** attach mode exists and how the TUI consumes it, see [Attach TUI](/reference/attach-tui/). For daemon-side listener configuration (TLS, tokens, multi-session, peer-hub), see [Configuration → attach](/reference/configuration/).

---

## Auth model

Two orthogonal layers run on every request:

**Transport layer** ([`pkg/attach/auth.go`](https://github.com/go-steer/core-agent/blob/main/pkg/attach/auth.go)):

- **TLS + optional mTLS** — `attach.tls_cert` / `attach.tls_key` for server certs; `attach.client_ca` enables `RequireAndVerifyClientCert`.
- **Shared bearer token** — `--attach-token=<ENVVAR>` on the daemon side. Constant-time compare. Header precedence: `X-Attach-Token` wins over `Authorization: Bearer` even when wrong. See [Attach TUI § Behind an identity gateway](/reference/attach-tui/#behind-an-identity-gateway-cloud-run-iam-iap-cloudflare-access-) for why the two-header split exists.
- **Read-only mode** — `--attach-readonly` returns **403** for any non-`GET/HEAD/OPTIONS` request without further checks.

**Per-caller layer** ([`pkg/attach/caller_middleware.go`](https://github.com/go-steer/core-agent/blob/main/pkg/attach/caller_middleware.go)):

Resolves an `auth.Caller{Identity, Labels, Admin}` via a pluggable `auth.Authenticator`:

| Authenticator | Behavior |
|---|---|
| `AnonymousAuth` (default) | Every request → fixed `Caller`. Single-user mode. |
| `BearerTokenAuth` | Token → `Caller` table from `attach.multi_session.auth.table_file`. `admin_identities` set the `Admin` flag; `proxy_identities` allowlist for proxy-asserted requests. |

**Proxy-asserted caller.** When `multi_session.enabled=true` AND the transport-authenticated caller is in the `proxy_identities` allowlist, the request may carry `X-Asserted-Caller: <identity>` (header name overridable via `Options.ProxyHeader`). The effective `Caller` becomes the asserted one; the proxying identity is preserved for audit. Bad assertions → **401** with `WWW-Authenticate: Bearer realm="attach-multisession"`.

**ACL matrix** ([`pkg/auth/authorize.go`](https://github.com/go-steer/core-agent/blob/main/pkg/auth/authorize.go)):

| Action | Owner | Contributor | Viewer | Admin |
|---|---|---|---|---|
| `SessionList` | own sessions | own sessions | own sessions | all |
| `SessionRead` | ✓ | ✓ | ✓ | ✓ |
| `SessionWrite` | ✓ | ✓ | | ✓ |
| `SessionAdmin` | ✓ | | | ✓ |
| `DaemonAdmin` | | | | ✓ |

Deny returns **404** — deliberately indistinguishable from "session doesn't exist" so unauthorized callers can't enumerate SIDs. This is what "admin identity gets" that others don't: cross-owner list + read + write + delete.

## Path grammar

Every session-scoped endpoint has two shapes:

| Shape | When to use |
|---|---|
| `/sessions/{app}/{sid}/...` | Qualified — always safe, required for multi-app daemons. |
| `/sessions/{sid}/...` | Shortcut — daemon resolves `{sid}` to an unambiguous `{app}`. Returns **409 Conflict** if the SID exists in multiple apps. |

Most callers can use the shortcut. Multi-app daemons (rare — `attach.multi_app` configuration) should prefer the qualified form.

## Notable headers

| Header | Direction | Purpose |
|---|---|---|
| `X-Attach-Token` | request | Transport bearer token; wins over `Authorization`. |
| `Authorization: Bearer <token>` | request | Transport bearer fallback. |
| `X-Asserted-Caller` | request | Proxy identity assertion (multi-session only). Header name overridable. |
| `WWW-Authenticate: Bearer realm="attach"` | response | 401, transport layer. |
| `WWW-Authenticate: Bearer realm="attach-multisession"` | response | 401, per-caller layer (bad proxy assertion). |
| `X-Interrupted: nothing-in-flight` | response | `POST /interrupt` when the agent is idle. |
| `Content-Type: text/event-stream` | response | SSE endpoints (`/events`, `/perms/stream`). |
| `X-Accel-Buffering: no`, `Cache-Control: no-cache` | response | SSE headers ensuring proxies don't buffer. |

No cookies — the listener is stateless per request. Identity is re-derived from headers (and client cert, if mTLS) on every call.

## Endpoint reference

### Session lifecycle

| Method | Path | Action | Request | Response |
|---|---|---|---|---|
| `GET` | `/sessions` | `SessionList` (always OK, ACL-filtered) | — | **200** `{"sessions":[{"session_id":..., "app":..., ...}]}` — union of in-memory + persisted-idle rows. |
| `POST` | `/sessions` | Authenticated caller | — | **201** `{"app":..., "user":..., "sessionID":..., "url":...}`. **501** when the daemon lacks a `SessionFactory`; **401** anonymous; **409** on `ErrSessionExists`. Caller stamped as ACL Owner. |
| `DELETE` | `/sessions/{sid}` and `/sessions/{app}/{sid}` | `SessionAdmin` | — | **204** on success. **403** on the bootstrap `"default"` session. **404** on not-found OR auth-deny (masked). **NOT idempotent** — second call returns **500** wrapping `ErrSessionNotFound`. |

### Session read (`SessionRead` — all owner/contributor/viewer OK)

Every path suffix below appears under both `/sessions/{sid}/...` and `/sessions/{app}/{sid}/...`. All GET, all 200 with zero-valued response when the underlying provider is unwired.

| Path suffix | Response |
|---|---|
| `/events` | SSE, `text/event-stream`. Query `?since=<int64>` cursor for lossless replay. **412** when the session has no eventlog. Frames typed via `event: <type>` (or legacy `event: agent`). |
| `/perms/stream` | SSE, `event: prompt`. **501** without `PromptBrokerProvider`. |
| `/status` | `{"state":..., "model_name":..., "next_wake_at":..., "current_tool":...}` — never empty `state`. |
| `/usage` | `UsageInfo` — see [UsageMetadata schema](#usagemetadata-schema) below. |
| `/tools` | `{"tools":[{"name":..., "description":...}]}`. Empty when no provider. |
| `/agents` | `{"agents":[{"name":..., "description":...}]}`. |
| `/context` | `ContextInfo{compactions, checkpoints, chars_after_compaction, ...}`. |
| `/memory` | `{"sources":[{"scope":..., "path":..., "bytes":...}]}` — the AGENTS.md chain. |
| `/skills` | `{"skills":[{"name":..., "description":...}]}`. |
| `/mcp` | `MCPInfo{servers:[...]}` — configured servers + status. |
| `/pricing` | `PricingInfo{rate, last_refresh, ...}`. |
| `/perms` | `PermsInfo{mode, allowed:[...], denied:[...], history:[...]}`. |

### Session write (`SessionWrite` — owner + contributor + admin)

All write endpoints cap request bodies at **8 KiB** (`operatorPostMaxBytes`).

| Method | Path suffix | Request | Response |
|---|---|---|---|
| `POST` | `/inject` | `{"message":"..."}` (empty → **400**) | `{"injected":..., "session":...}` |
| `POST` | `/wake` | `{"target"?:..., "prompt"?:...}` (both optional) | `{"woken":..., "prompt":...}`; **501** if `target` set |
| `POST` | `/interrupt` | — | `{"interrupted":bool, "session":...}`; **412** if agent lacks `InterruptProvider`; `X-Interrupted: nothing-in-flight` header when idle; writes audit event `Author=attach/interrupt` |
| `POST` | `/perms/allow` / `/perms/deny` | `{"patterns":[...]}` (empty → **400**) | **204**; **501** if no controller |
| `POST` | `/perms/respond` | `{"id":..., "decision":...}` | `{"acknowledged":true}`; **404** on unknown id |
| `POST` | `/pricing/refresh` | — | `{"updated":..., "known_models":..., "last_refresh":..., "detail":...}` |
| `POST` | `/pricing/set` | `{"model":..., "input_usd_per_mtok":..., "output_usd_per_mtok":...}` | **204** |
| `POST` | `/reload` | — | `{"memory":..., "skills":..., "mcp":..., "errors":[...]}` |
| `POST` | `/slash/compact` | `{"focus"?:...}` | `{"summary_event_id":..., "summary_text":..., "duration_ms":..., "skipped":bool}` |
| `POST` | `/slash/done` | `{"note"?:...}` | `{"checkpoint_event_id":..., "summary_text":..., "task_note":..., "duration_ms":..., "skipped":bool}` |
| `POST` | `/slash/btw` | `{"question":...}` | `{"answer":...}` |
| `POST` | `/slash/subagent` | `SubagentSpec{name, goal, ...}` | `{"name":..., "started_at":...}` |
| `POST` | `/slash/replan` | `{"reason"?:...}` | `{"archived_path":..., "plan_was_active":..., "message":...}` |

Any capability-missing mutation returns **501** (e.g. `/interrupt` without an `InterruptProvider`, `/wake` with a `target` on a daemon without wake-target routing).

## UsageMetadata schema

`GET /sessions/{sid}/usage` (v2.7.0-dev.3+, [#222](https://github.com/go-steer/core-agent/issues/222)). Response type `attach.UsageInfo`:

```json
{
  "overall": {
    "input_tokens": 12450,
    "input_tokens_cached": 8320,
    "input_tokens_uncached": 4130,
    "output_tokens": 1890,
    "thoughts_tokens": 420,
    "turns": 5,
    "cost_usd": 0.0423,
    "cost_usd_uncached_reference": 0.1287
  },
  "per_model": {
    "gemini-3.1-pro": { "input_tokens": ..., "..." },
    "gemini-3.5-flash": { "input_tokens": ..., "..." }
  },
  "per_turn": [
    {
      "turn": 1,
      "ts": "2026-07-19T14:03:12Z",
      "model": "gemini-3.1-pro",
      "input_tokens": 3200,
      "input_tokens_cached": 2100,
      "input_tokens_uncached": 1100,
      "output_tokens": 420,
      "thoughts_tokens": 90,
      "tool_use_tokens": 0,
      "total_tokens": 3620,
      "cost_usd": 0.0089,
      "cost_usd_uncached_reference": 0.0270
    }
  ],
  "digest_methods": {
    "counts":      { "structural": 12, "agentic": 3, "passthrough": 8 },
    "bytes_saved": { "structural": 84120, "agentic": 15380 }
  }
}
```

Field notes:

- **`overall` / `per_model`** — cumulative totals + per-model breakdown. `_cached` / `_uncached` split lets you compute the cache-savings percentage as `1 - cost_usd / cost_usd_uncached_reference`.
- **`per_turn`** — the v2.7-dev.3 addition. Submission-ordered list, `turn` is 1-based. `total_tokens` matches Google's `UsageMetadata.TotalTokenCount` convention.
- **`ts`** — RFC3339. Marks the model call, not the operator submission.
- **`tool_use_tokens`** — Anthropic-specific; 0 for Gemini providers.
- **`digest_methods`** — MCP pruner attribution ([Digest & MCP wrap](/concepts/mcp/#agentic-wrap)). `counts` is calls per strategy; `bytes_saved` is aggregate response-size reduction.

`omitempty` on secondary fields — a JSON consumer should treat missing keys as `0` / absent.

## Peer / hub endpoints

Registered only when `Options.PeerRegistry` is non-nil (daemon launched with `--attach-peer-hub`). **Peer endpoints go through the transport layer only** — no per-caller ACL check. Rationale: peer registration is cluster-infra, not per-session state; auth is the shared token or mTLS.

| Method | Path | Request | Response |
|---|---|---|---|
| `POST` | `/peers` | `{"name":..., "endpoint":..., "labels"?:{...}, "heartbeat_ttl_sec"?:...}` (16 KiB cap) | **201** `{"registration_id":..., "name":..., "endpoint":..., "labels":..., "registered_at":..., "last_heartbeat":..., "lease_expires_at":...}`. Name-based upsert. |
| `GET` | `/peers` | `?label=k=v` (repeatable filter) | **200** `{"peers":[{...}]}`. |
| `POST` | `/peers/{id}/heartbeat` | — | **200** `Peer` (extended lease); **404** unknown id. |
| `DELETE` | `/peers/{id}` | — | **204**; **idempotent** (unknown id → 204). |

## Non-session routes

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/.well-known/agent-card.json` | **none** (bypasses transport auth) | Public agent-card discovery. Enabled when `AgentCard.Description` + `ExternalURL` are both non-empty in the daemon config. 405 on non-GET/HEAD. |
| `GET` | `/whoami` | Transport auth (no per-session ACL) | Returns `{"identity":..., "admin":bool, "source":..., "proxy_by":...}` for the current caller. `source ∈ {"bearer","mtls","iap","asserted","anonymous"}` (consumers tolerate unknowns). `proxy_by` populated only when `source="asserted"` (X-Asserted-Caller path). Companion to the SSE `capabilities.caller_id` display hint. |
| `GET` | `/ui/*` | Transport auth | Optional SPA passthrough — only when `Options.UI` is non-null. `/ui` (no trailing slash) → **301** → `/ui/`. |

## Streaming endpoints (summary)

Two SSE endpoints:

| Path | Content-Type | Cursor | Notes |
|---|---|---|---|
| `GET /sessions/.../events` | `text/event-stream` | `?since=<int64>` | Lossless replay via cursor. **412** when session has no eventlog. Frames typed via `event: <type>` header (or legacy `event: agent`). `X-Accel-Buffering: no` + `Cache-Control: no-cache`. |
| `GET /sessions/.../perms/stream` | `text/event-stream` | none | Per-prompt frames: `event: prompt`. **501** without `PromptBrokerProvider`. |

The `since` cursor is monotonic per-session — the TUI's `/reconnect` slash sends `?since=<lastSeq>` to resume without missing events across reconnects.

### `capabilities` frame

The first frame on every `/events` stream is `event: capabilities` — the client advertises the wire contract before any state flows. The full field list lives in [the SSE spec](https://github.com/go-steer/core-tui/blob/main/docs/sse-event-stream-protocol.md#21-capabilities); the current additions are:

- **`features`** — feature-flag map derived from live runtime state. Suggested keys: `multi_session`, `perms_stream`, `cost_ceiling`, `observer_mode`, `mcp`, `specialists`, `cross_daemon`, `interrupt`. Consumers treat absent keys as "off / unknown"; producers MAY add unknown keys.
- **`slash_commands`** — dynamic list of the slash names this agent's `POST /slash/<name>` will accept. Derived from capability-interface presence (`CompactSlashProvider` → `"compact"`, etc.). Clients render only what the connected agent supports.
- **`agent`** — the producing agent's own identity: `{name, version, description, model, provider, url}`. Consolidates fields previously scattered across `/.well-known/agent-card.json`, `GET /status`, and the `server` banner.
- **`caller_id`** — the resolved caller identity display hint. Canonical source: `GET /whoami`.

`status-update` also carries an optional `capabilities` field (merge semantics) for future hot updates — no producer emits it today, but consumers MUST tolerate its absence and MUST merge (not replace) when it does arrive.

### Slash-response conventions

Every `POST /sessions/.../slash/<name>` response body reserves two keys for renderer negotiation:

- **`_render`** — `"text" | "markdown" | "json" | <future>`. Advises the client which built-in renderer to use for the body. Producers MAY omit; consumers fall back to their per-slash default.
- **`_schema`** — reserved for schema-driven rendering (v0.3.0+ target). No producer emits it today.

Consumers MUST tolerate unknown values and MUST NOT crash on missing keys.

## Status code cheat sheet

| Code | Meaning here |
|---|---|
| **200** | OK — the default for GETs and most POSTs with responses. |
| **201** | Created — `POST /sessions`, `POST /peers`. |
| **204** | No content — successful DELETEs, `POST /perms/allow` etc. |
| **301** | Redirect — `/ui` → `/ui/`. |
| **400** | Bad request — empty required field (message, patterns, ...). |
| **401** | Unauthenticated — missing / wrong bearer token; bad proxy assertion. |
| **403** | Forbidden — `--attach-readonly` writes; delete of the bootstrap `"default"` session. |
| **404** | Not found OR auth-deny (deliberately indistinguishable to avoid SID enumeration). |
| **405** | Method not allowed — e.g. `POST /.well-known/agent-card.json`. |
| **409** | Conflict — shortcut SID ambiguous across apps; `POST /sessions` on `ErrSessionExists`. |
| **412** | Precondition failed — session has no eventlog (SSE reader); no `InterruptProvider` (interrupt). |
| **500** | Internal error — factory failure on `POST /sessions`; second `DELETE` of a gone session. |
| **501** | Not implemented — capability provider absent (`SessionFactory`, `InterruptProvider`, `PromptBrokerProvider`, wake `target`, etc.). |

## Idempotency

| Endpoint | Idempotent? |
|---|---|
| `DELETE /sessions/{sid}` | **No** — first call **204**, second call **500** (`ErrSessionNotFound`). Callers that retry on transient failure should treat 204 and 500 as equivalent success. |
| `DELETE /peers/{id}` | **Yes** — unknown id also **204**. |
| `POST /sessions` | **No** — every call spins a fresh session. |
| `POST /peers` | Effectively **yes** — name-based upsert extends the lease of an existing peer. |
| `POST /perms/respond` | **No** — second respond for the same prompt → **404** (`ErrPromptNotFound`). |
| `POST /interrupt` | Trivially idempotent — extra calls set `X-Interrupted: nothing-in-flight`. |

## See also

- [Attach TUI](/reference/attach-tui/) — client-side behavior, permissions bridge, multi-daemon workflow.
- [`core-agent-tui` CLI reference](/reference/core-agent-tui/) — the reference client for this protocol.
- [Configuration → attach](/reference/configuration/) — daemon-side listener knobs.
- [Multi-session daemon](/concepts/multi-session/) — the per-caller ACL + admin identity model that shapes this API's authorization behavior.
