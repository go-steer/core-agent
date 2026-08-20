---
title: Attach mode HTTP endpoints
---


HTTP/SSE protocol reference for the attach listener — the surface `core-agent-tui`, third-party dashboards, and CI tooling call. The daemon exposes this when launched with `--attach-listen=127.0.0.1:<port>` (or the `attach.listen` config field).

This page is the wire-level reference: paths, request/response shapes, auth requirements, status codes, and idempotency semantics. For **why** attach mode exists and how the TUI consumes it, see [Attach TUI](/reference/attach-tui/). For daemon-side listener configuration (TLS, tokens, multi-session, peer-hub), see [Configuration → attach](/reference/configuration/).

---

## Auth model

**Default bind + startup policy** (v2.8+, [#376](https://github.com/go-steer/core-agent/issues/376)): the default listen address is loopback-only (`127.0.0.1:7777`). Binding a **non-loopback** address (`:7777`, `0.0.0.0:7777`, `[::]:7777`, any non-loopback IP/hostname) without an authentication gate — bearer token, mTLS client CA, or multi-session auth with `allow_anonymous: false` — is a startup **error**: the daemon refuses to start rather than exposing transcript reads (`/events`), message injection (`/inject`), and permission approvals (`/perms/respond`) to the network. Tokenless **loopback** listeners still start, but log a loud warning that any local process can drive the agent.

Two orthogonal layers run on every request:

**Transport layer** ([`pkg/attach/auth.go`](https://github.com/go-steer/core-agent/blob/main/pkg/attach/auth.go)):

- **TLS + optional mTLS** — `attach.tls_cert` / `attach.tls_key` for server certs; `attach.client_ca` enables `RequireAndVerifyClientCert`.
- **Shared bearer token** — `--attach-token=<ENVVAR>` on the daemon side. Constant-time compare. Header precedence: `X-Attach-Token` wins over `Authorization: Bearer` even when wrong. See [Attach TUI § Behind an identity gateway](/reference/attach-tui/#behind-an-identity-gateway-cloud-run-iam-iap-cloudflare-access-) for why the two-header split exists.
- **Read-only mode** — `--attach-readonly` returns **403** for any non-`GET/HEAD/OPTIONS` request without further checks.

**Browser CSRF protection** (v2.8+, [#383](https://github.com/go-steer/core-agent/issues/383), [`pkg/attach/csrf.go`](https://github.com/go-steer/core-agent/blob/main/pkg/attach/csrf.go)) — applies to every state-changing request (any method other than `GET`/`HEAD`/`OPTIONS`), regardless of token/auth mode:

- **`Content-Type: application/json` is required** on writes — even body-less ones (`/interrupt`, `/pricing/refresh`, `DELETE`s, peer heartbeats) — otherwise **415**. This kills the CORS "simple request" vector (`text/plain` POST fires without a preflight).
- **Origin enforcement** — when an `Origin` header is present it must be a loopback origin (`localhost` / `127.0.0.0/8` / `[::1]`) or a self origin (host matching the request's `Host`), otherwise **403**. Browsers always attach `Origin` to cross-site POSTs; native clients (curl, `core-agent-tui`, SDKs) send no `Origin` and pass untouched. The literal `null` origin (sandboxed iframes, `file://` pages) is rejected.

Scripted callers: add `-H "Content-Type: application/json"` to every `curl -X POST/DELETE` against this API.

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
| `Content-Type: application/json` | request | **Required on every state-changing request** (non-`GET`/`HEAD`/`OPTIONS`), body or not — **415** otherwise. CSRF protection ([#383](https://github.com/go-steer/core-agent/issues/383)). |
| `Origin` | request | Checked on state-changing requests: non-loopback, non-self origins → **403**. Absent (native clients) passes. |
| `X-Attach-Token` | request | Transport bearer token; wins over `Authorization`. |
| `Authorization: Bearer <token>` | request | Transport bearer fallback. |
| `X-Asserted-Caller` | request | Proxy identity assertion (multi-session only). Header name overridable. |
| `X-Attach-Protocol-Version` | request | SSE protocol version the client speaks (semver). Optional; `?protocol=<semver>` is the query-param equivalent and wins when both are set. A declared **major** that differs from the server's is rejected `409`; a malformed value is `400`. Declaring nothing is accepted (back-compat). |
| `X-Attach-Protocol-Version` | response | The SSE protocol version the server speaks, echoed on every `/events` response (success or rejection). |
| `WWW-Authenticate: Bearer realm="attach"` | response | 401, transport layer. |
| `WWW-Authenticate: Bearer realm="attach-multisession"` | response | 401, per-caller layer (bad proxy assertion). |
| `X-Interrupted: nothing-in-flight` | response | `POST /interrupt` when the agent is idle. |
| `X-Hold: unsupported` | response | `POST /interrupt` with `hold` (the default) against an agent that has no `PauseController`. The turn was still cancelled; the loop was not parked. |
| `Content-Type: text/event-stream` | response | SSE endpoints (`/events`, `/perms/stream`). |
| `X-Accel-Buffering: no`, `Cache-Control: no-cache` | response | SSE headers ensuring proxies don't buffer. |

No cookies — the listener is stateless per request. Identity is re-derived from headers (and client cert, if mTLS) on every call.

## Endpoint reference

### Session lifecycle

| Method | Path | Action | Request | Response |
|---|---|---|---|---|
| `GET` | `/sessions` | `SessionList` (always OK, ACL-filtered) | — | **200** `{"sessions":[{"app":..., "user":..., "sessionID":..., "has_event_log":bool, "status":"active"\|"idle", "last_touched_at":..., "title":...}]}` — union of in-memory (`active`) + persisted-idle rows. Note the field is `sessionID`, not `session_id` — pin against the [conformance fixture](https://github.com/go-steer/core-agent/blob/main/pkg/attach/testdata/conformance/rest-sessions-list-v2.json). `last_touched_at` is RFC 3339 with arbitrary precision and zone offset (parse, don't pattern-match); the zero value `0001-01-01T00:00:00Z` means never-touched. `title` (protocol 1.6.0) is a short label derived from the session's first prompt; it is **omitted** for pre-1.6.0 daemons, for sessions whose first turn hasn't landed, and where titling is off — render the session ID when it's absent. Operators can override an inferred title — see [Renaming a session](#renaming-a-session-protocol-1100). |
| `POST` | `/sessions` | Authenticated caller | `{"viewers"?:[...], "contributors"?:[...]}` — **body optional** (absent = owner-only ACL, the pre-1.10.0 behavior) | **201** `{"app":..., "user":..., "sessionID":..., "url":...}` ([fixture](https://github.com/go-steer/core-agent/blob/main/pkg/attach/testdata/conformance/rest-create-session-v1.json)). **501** when the daemon lacks a `SessionFactory`; **401** anonymous; **409** on `ErrSessionExists`; **400** on a malformed body or an `owner` field. Caller stamped as ACL Owner — `owner` is **rejected**, not honoured, so a caller can't hand a session to someone else (see [Session ACLs](#session-acls-protocol-1100)). The body is parsed *before* the session factory runs, so a rejected one leaves no half-built session behind. Deliberately ungated during daemon shutdown: the ACL row is durable, so a session created in that window resumes normally after the restart — but it is usable only then. |
| `GET` | `/sessions/{sid}/acl` and `/sessions/{app}/{sid}/acl` | `SessionAdmin` | — | **200** `{"owner":..., "viewers":[...], "contributors":[...]}` ([fixture](https://github.com/go-steer/core-agent/blob/main/pkg/attach/testdata/conformance/rest-session-acl-v1.json)). Both lists are **always present** — `[]`, never `null`. **404** on not-found OR auth-deny (masked), so a viewer or contributor cannot read the roster of who else is on the session. |
| `PATCH` | `/sessions/{sid}/acl` and `/sessions/{app}/{sid}/acl` | `SessionAdmin` | `{"owner"?:..., "viewers"?:[...], "contributors"?:[...]}` | **200** with the same shape as `GET`, reporting the ACL as stored. An **omitted** list is left unchanged; `[]` clears it. **400** on a malformed/absent body or an `owner` that differs from the current one — `""` included (ownership is not transferable here); **404** on not-found OR auth-deny; **500** if persistence fails, in which case the in-memory ACL is rolled back. |
| `DELETE` | `/sessions/{sid}` and `/sessions/{app}/{sid}` | `SessionAdmin` | — | **204** on success. **403** on the bootstrap `"default"` session. **404** on not-found OR auth-deny (masked). **NOT idempotent** — second call returns **500** wrapping `ErrSessionNotFound`. |

### Session read (`SessionRead` — all owner/contributor/viewer OK)

Every path suffix below appears under both `/sessions/{sid}/...` and `/sessions/{app}/{sid}/...`. All GET, all 200 with zero-valued response when the underlying provider is unwired.

| Path suffix | Response |
|---|---|
| `/events` | SSE, `text/event-stream`. Query `?since=<int64>` cursor for lossless replay. **412** when the session has no eventlog. **409** when the client declares an incompatible protocol major (`?protocol=` / `X-Attach-Protocol-Version`); **400** when the declared version is malformed. Frames typed via `event: <type>` (or legacy `event: agent`). |
| `/perms/stream` | SSE, `event: prompt`. **501** without `PromptBrokerProvider`. |
| `/status` | `{"state":..., "model_name":..., "next_wake_at":..., "current_tool":...}` — never empty `state`. |
| `/usage` | `UsageInfo` — see [UsageMetadata schema](#usagemetadata-schema) below. |
| `/tools` | `{"tools":[{"name":..., "description":..., "source":..., "server":...}]}`. Empty when no provider. `source` vocabulary is `builtin \| mcp \| skill \| subagent \| other`; declarative subagents wired as parent tools report `subagent`, and `server` names the owning MCP server when `source` is `mcp` ([#767](https://github.com/go-steer/core-agent/issues/767)). MCP and skill tools reach the agent as *toolsets*, so they are folded in from the host's MCP + skill providers rather than from the agent's own tool list; a host that wires neither simply reports no rows for them. The MCP rows are the same startup snapshot `/mcp` serves, so the two endpoints cannot disagree about which server owns what. |
| `/agents` | `{"agents":[{"name":..., "description":...}]}` — **live** spawned instances ("what's running"). |
| `/subagents` | `{"subagents":[{"name":..., "description":..., "model":..., "root":..., "modes":[...], "tools":[...]}]}` — the **configured** roster the daemon loaded ("what's spawnable by reference"), distinct from `/agents`. `modes` reports how the subagent can be invoked **on this session**: `["sync","async"]` when that session's agent also carries it as a parent tool, `["async"]` (`spawn_agent` by reference only) otherwise. Predefined specs are always `["async"]`; so is every declarative subagent on a session created via `POST /sessions`, because those agents are wired with the background manager but not the synchronous subagent tools — cross-check against `/tools`, where a sync-invocable subagent appears with `source: subagent`. `tools` (protocol 1.9.0) is that subagent's own grant, sorted by name and using the same `ToolInfo` shape and `source` vocabulary as `/tools` ([#768](https://github.com/go-steer/core-agent/issues/768)) — so a specialist detail view can answer "can this one actually reach kubectl?". It lists what was **configured**, not what the runtime adds on top: the loop-control tools every spawned subagent gets regardless (`return_result`, `report_alert`, `schedule_next_turn`) are omitted. The key is **omitted** both by a pre-1.9.0 daemon and for a subagent granted no tools, so absence doesn't mean "reaches nothing" — fall back to rendering the row without a grant. Empty when no provider. |
| `/agents/{name}/events` | `{"agent":..., "parent_session_id":..., "branches":[...], "events":[{"seq":..., "event":{...}}], "next_since":..., "truncated":bool}` — one subagent's **persisted inner turns** ([#638](https://github.com/go-steer/core-agent/issues/638)). Query `?since=<int64>` + `?limit=<n>` (default 500, capped 5000; page while `truncated` is true, feeding `next_since` back as `since`). Reads history from the eventlog, not the live manager, so it works for a finished subagent and for one that ran before the last restart. `branches` echoes what was searched: the four launch spellings (`<name>`, `bg.<name>`, `sub.<name>`, `remote.<name>`), each covering its own nested descendants, **plus** the instance-suffixed labels found in the log — a subagent declared as `cluster` and spawned as `bg.cluster-1` resolves under `cluster` as well as under the roster's `cluster-1` ([#694](https://github.com/go-steer/core-agent/issues/694)). Only a `-<digits>` suffix counts as an instance counter, so a separate subagent named `cluster-probe` stays separate. Prefix matching is anchored, so ask for the **top-level** subagent name: `cluster` returns what `bg.cluster.probe` did, but querying `probe` on its own returns nothing. A name that resolves to nothing is **404** with `{"error":..., "agent":..., "branches":[...], "available":[...]}`, where `available` is every subagent name that *would* resolve in this session (distinct log branches + the live and configured rosters) — a name in either roster answers **200** with an empty list instead, as does any session where absence couldn't actually be observed — an eventlog that can't enumerate its branches, a failed branch scan, or a scan that hit its 500-label cap — so the 404 always means "looked, and it isn't here". **400** on a name that could never be a branch label (contains `.`, `/`, or whitespace); **412** when the session has no eventlog. |
| `/context` | `ContextInfo{compactions, checkpoints, chars_after_compaction, ...}`. |
| `/memory` | `{"sources":[{"scope":..., "path":..., "bytes":...}]}` — the AGENTS.md chain. |
| `/skills` | `{"skills":[{"name":..., "description":...}]}`. |
| `/mcp` | `MCPInfo{servers:[...]}` — configured servers + status. |
| `/pricing` | `PricingInfo{rate, last_refresh, ...}`. |
| `/perms` | `PermsInfo{mode, allow:[...], deny:[...], approvals:[...]}` — the live mode plus the session's approval log. Each `approvals` row is `{tool, key?, decision, at, by?}`; `by` names the principal that answered the prompt and is **omitted** when the daemon verified nobody (protocol 1.10.0, [#830](https://github.com/go-steer/core-agent/issues/830)) — see [Approval attribution](#approval-attribution-protocol-1100). |
| `/guardrails` | `GuardrailInfo{watchdog:{mode,tripped,reason}, cost_ceiling:{max_turn_usd,max_session_usd,session_cost_usd,tripped,reason,would_retrip}, halted}` — why the session is refusing turns, and whether a bare reset would re-trip ([#666](https://github.com/go-steer/core-agent/issues/666)). |

### Session write (`SessionWrite` — owner + contributor + admin)

All write endpoints cap request bodies at **8 KiB** (`operatorPostMaxBytes`).

| Method | Path suffix | Request | Response |
|---|---|---|---|
| `POST` | `/inject` | `{"message":"...", "wake"?:bool}` (empty message → **400**; `wake` defaults to **true**) | `{"injected":..., "session":..., "woke":bool}` ([fixture](https://github.com/go-steer/core-agent/blob/main/pkg/attach/testdata/conformance/rest-inject-v1.json)); **501** on `"wake": false` if the agent can't defer — see [Queuing context without a turn](#queuing-context-without-a-turn-protocol-1100); **503** + `Retry-After` during daemon shutdown (message would die with the in-memory inbox — redeliver after restart) |
| `POST` | `/wake` | `{"target"?:..., "prompt"?:...}` (both optional) | `{"woken":..., "prompt":...}`; **501** if `target` set; **503** + `Retry-After` during daemon shutdown. Emits a `wake` frame to everyone watching `/events` (protocol 1.7.0 — see [Wake notifications](#wake-notifications-protocol-170)) |
| `POST` | `/interrupt` | `{"hold"?:bool, "stop_subagents"?:bool}` — **body optional**, absent = `{"hold":true}` | `{"interrupted":bool, "paused":bool, "running_subagents":[...], "stopped_subagents":[...], "session":...}`; **412** if agent implements neither `PauseController` nor `InterruptProvider`; `X-Interrupted: nothing-in-flight` header when idle; `X-Hold: unsupported` when the agent can't park; writes audit event `Author=attach/interrupt` |
| `POST` | `/pause` | `{"reason"?:...}` — **body optional** | `{"paused":bool, "transitioned":bool, "state":"paused", "paused_since":..., "pause_reason":..., "session":...}`; **501** if no `PauseController` |
| `POST` | `/resume` | `{"mode"?:"steer"\|"continue"\|"abandon", "steer"?:...}` — **body optional** (absent = `continue`) | `{"resumed":bool, "mode":..., "state":..., "session":...}`; **400** on an unknown mode or `mode=steer` with no text; **501** if no `PauseController`; **503** + `Retry-After` during daemon shutdown |
| `POST` | `/agents/{name}/stop` | — | `{"agent":..., "stopped":true, "session":...}`; **404** when no subagent by that name is running; **501** if no `AgentStopper` |
| `POST` | `/perms/allow` / `/perms/deny` | `{"patterns":[...]}` (empty → **400**) | **204**; **501** if no controller |
| `POST` | `/perms/respond` | `{"id":..., "decision":..., "approver"?:...}` | `{"acknowledged":true, "approver"?:...}`; **404** on unknown id; **400** when `approver` disagrees with the caller the daemon verified, or when it verified nobody to check against. `approver` echoes what was recorded and is omitted when nothing was verified — see [Approval attribution](#approval-attribution-protocol-1100) |
| `POST` | `/title` | `{"title":"..."}` — the key is **required**; `""` clears | `{"session":..., "title"?:..., "persisted":bool, "detail"?:...}` ([fixture](https://github.com/go-steer/core-agent/blob/main/pkg/attach/testdata/conformance/rest-session-title-v1.json)); **400** on an omitted `title`; **501** if the agent can't set one — see [Renaming a session](#renaming-a-session-protocol-1100) |
| `POST` | `/pricing/refresh` | — | `{"updated":..., "known_models":..., "last_refresh":..., "detail":...}` |
| `POST` | `/pricing/set` | `{"model":..., "input_usd_per_mtok":..., "output_usd_per_mtok":...}` | **204** |
| `POST` | `/reload` | — | `{"memory":..., "skills":..., "mcp":..., "errors":[...]}` |
| `POST` | `/guardrails/reset` | `{"guardrail"?:"watchdog"\|"cost_ceiling"\|"all", "additional_budget_usd"?:float}` — **body optional** (absent = reset everything tripped) | `{"reset":[...], "budget_added_usd":..., "guardrails":{...}, "message":...}`; **409** when the reset would immediately re-trip (per-session spend already at the ceiling — add budget); **400** on an unknown guardrail name, a negative budget, or budget on a `watchdog`-scoped reset; **501** if no resetter |
| `POST` | `/slash/compact` | `{"focus"?:...}` | `{"summary_event_id":..., "summary_text":..., "duration_ms":..., "skipped":bool}` |
| `POST` | `/slash/done` | `{"note"?:...}` | `{"checkpoint_event_id":..., "summary_text":..., "task_note":..., "duration_ms":..., "skipped":bool}` |
| `POST` | `/slash/btw` | `{"question":...}` | `{"answer":..., "empty"?:bool, "detail"?:...}` — see [Side questions](#side-questions-slashbtw) |
| `POST` | `/slash/subagent` | `SubagentSpec{name, goal, ...}` | `{"name":..., "started_at":...}` |
| `POST` | `/slash/replan` | `{"reason"?:...}` | `{"archived_path":..., "plan_was_active":..., "message":...}` |

Any capability-missing mutation returns **501** (e.g. `/pause` or `/resume` without a `PauseController`, `/wake` with a `target` on a daemon without wake-target routing). `/interrupt` is the exception: it predates the convention and answers **412**.

Guardrail trips and resets are **durable** (v2.9.0-dev, [#643](https://github.com/go-steer/core-agent/issues/643)). A trip appends a `guardrail-trip` event (`Author=agent/guardrail-trip`) and a successful reset appends `attach-guardrail-reset` (`Author=attach/guardrail-reset`, carrying `caller`, `reset`, and `budget_added_usd`); a process that restarts against the same session folds those rows forward, so a halted session comes back halted and a cleared one comes back cleared. Like `/interrupt`'s audit row these are written by the agent from its own turn loop rather than synchronously inside the request, so tail `/events` rather than assuming the row exists the instant the reset returns. Caller attribution is stamped from the authenticated identity — a `caller` field in the request body is ignored. Restored state is always subject to the *current* process's configuration: a daemon restarted with `--watchdog=warn` does not resurrect an enforce-mode halt, and granted budget is not applied to a per-session ceiling that is no longer configured. Requires an eventlog; with no session store the endpoints behave exactly as before.

### Session ACLs (protocol 1.10.0)

A session's ACL is `owner` plus two lists — `viewers` (read) and `contributors` (read + write). The [authorization matrix](#auth-model) above has enforced all three since multi-session shipped, but until protocol 1.10.0 the lists were settable *nowhere* over HTTP: `POST /sessions` stamped the caller as `owner` and there was no route to amend anything ([#797](https://github.com/go-steer/core-agent/issues/797)). The only reachable ACL was owner-plus-admins, and a second participant got a **404** with no request that could change it.

Two ways to set them, because the two answer different questions:

| When you know | Call |
|---|---|
| At creation — the audience is a property of the work (an agent opening a session about an incident knows the on-call group) | `POST /sessions {"contributors":["oncall@example.com"]}` |
| Later — the individual isn't known until it happens (adding a specific responder mid-incident) | `PATCH /sessions/{sid}/acl {"contributors":[...]}` |

`PATCH` semantics, precisely: an **omitted** list is left alone, and `[]` clears it. That distinction is the reason this is a `PATCH` and not a `PUT` — without it, adding a contributor would silently wipe the viewers.

Both verbs require `SessionAdmin`, so in practice the **owner or an admin**. The read is gated as hard as the write on purpose: the ACL names the other people on a session, and a contributor being able to enumerate their co-responders is a disclosure the endpoint has no reason to make. It also means a contributor cannot widen the ACL — otherwise the first identity you add could add everyone else.

`owner` is accepted on both endpoints **only so it can be refused** with a `400` — including `""`, which is a transfer to nobody. Ownership is not transferable here: the persisted owner index is what makes an idle session visible to its owner, so a transfer would take the session away from the losing side with no way back. Sending the *current* owner is fine, so a client that `GET`s the document, edits it, and `PATCH`es the whole thing back doesn't have to strip the field. Dropping the field silently was the alternative, and it is the same invisible-failure shape #797 was filed about.

Identities are normalized on the way in — trimmed, empties dropped, duplicates removed, caller order preserved — and the response reports what was actually stored. Identity matching is exact, so an untrimmed pasted address would produce an ACL that reads correct and denies anyway, surfacing as a **404** that looks nothing like a typo.

Concurrent `PATCH`es are safe to interleave: the merge of your body onto the current ACL happens inside the registry lock, not in the handler, so two callers amending different lists at the same moment both land. (Doing the read first and the write second would let each one carry the other's untouched list forward from a stale snapshot, and one edit would disappear behind a `200`.)

A `PATCH` on a session with a durable ACL row writes through to it, and a failed write is a **500** with the in-memory ACL rolled back — reporting `200` for an ACL that evaporates at the next restart is worse than failing, because the caller stops retrying. A legacy unowned session (registered without an owner) is amended in memory only: "ACL row exists ⟺ session is resumable" is a load-bearing invariant, and quietly making such a session resumable is a different lifecycle than the operator configured.

A pre-1.10.0 daemon answers both paths with **404** — the same answer it gives an unauthorized caller — so feature-detect on the negotiated `protocol_version`, not by probing.

### Approval attribution (protocol 1.10.0)

An approval is a privileged act — it is the moment a human lets the agent run a command the policy would otherwise have refused. Until protocol 1.10.0 the daemon threw away who performed it: `POST /perms/respond` carried the decision into the broker and nothing else, so the approval log answered "a `bash` call was allowed at 14:02" and could not answer "by whom" ([#830](https://github.com/go-steer/core-agent/issues/830)). On a relay — a chat gateway answering for a named human, a web console behind SSO — that is the one question the log exists to answer.

Now the server attributes the decision **itself**, from the caller it verified for the request:

- `POST /perms/respond` responds `{"acknowledged":true, "approver":"oncall@example.com"}`.
- `GET /perms` history rows carry `"by":"oncall@example.com"`.

Both fields are **omitted** when the daemon verified nobody — a tokenless loopback listener, or any request whose [`GET /whoami`](#non-session-routes) source is `anonymous`. Empty is deliberate: the daemon's placeholder identity for an unauthenticated caller is a literal string, and writing a placeholder into an audit line makes an unattributed approval read exactly like an attributed one. To get attribution, front the daemon with the asserted-caller header (`X-Asserted-Caller`) or enable bearer/mTLS per-caller auth; a client cannot supply attribution the server didn't verify.

The request body accepts an optional `approver`, and it is **checked, never believed**:

| Body `approver` | Verified caller | Result |
|---|---|---|
| omitted | anything | **200** — the server attributes the decision itself |
| matches the verified caller | same identity | **200** |
| disagrees with the verified caller | some identity | **400** — the prompt stays pending |
| any value | nothing verified | **400** — there is nothing to check it against |

The field exists only so a client whose idea of the approver differs from the server's finds out. Accepting and silently ignoring it would let a relay believe it had attributed a decision it hadn't, which is the same invisible failure #830 reports; trusting it would let any caller that can reach `/perms/respond` sign someone else's name to an approval.

Attribution reaches the embedded permission gate too — `permissions.ApprovalLog` gained a `By` field, so the same identity shows up wherever the approval log is read, not only over HTTP. Embedders extend a `permissions.Prompter` to the optional `permissions.AttributingPrompter` to supply it; a host that wires a plain prompter (an interactive terminal, where the answerer is whoever is at the keyboard) records no approver, exactly as before.

### Renaming a session (protocol 1.10.0)

Sessions carry an inferred title — the agent derives one from the first prompt so the picker stops being a list of opaque IDs. Inference is right often enough to be worth doing and wrong often enough that a name the operator can see is wrong and cannot change is a worse deal than no name at all. `POST /sessions/{sid}/title` is the override ([#808](https://github.com/go-steer/core-agent/issues/808)):

```bash
curl -sS -X POST http://127.0.0.1:7777/sessions/s-4412/title \
  -H 'Content-Type: application/json' \
  -d '{"title":"payments latency incident"}'
# {"session":"s-4412","title":"payments latency incident","persisted":true}
```

The remote TUI wraps it as `/title <name>`.

**`title` is required, and `""` is a real request.** Sending `{"title":""}` clears the name and re-arms automatic titling for the next turn; **omitting** the key is a **400**. The two can't collapse into one, because the daemon does not reject unknown fields — a typo'd key would otherwise decode to the zero value and silently wipe the session's name with a 200.

**The response reports what was stored, not what you sent.** Titles are trimmed and capped (60 runes for the built-in agent), so the value the picker shows is the one in the response body, not the one in the request.

**`persisted` is not a success flag.** It reports whether the new name reached the durable session row — i.e. whether it survives eviction and restart. `false` with no `detail` means there was nowhere durable to write, which is the *normal* answer for a single-session `--attach-listen` daemon: the rename is live for the life of the process. `false` **with** a `detail` means a store was wired and the write failed, so the name will revert; the rename is still in effect, which is why this is a 200 rather than a 500.

**501** means the agent registered no title-setting capability. Unlike the 404 that masks an authorization denial, this one is safe to feature-detect on: reaching it means the caller was already authorized for the session.

Renaming needs `SessionWrite`, not `SessionAdmin`. A title is a display label, not an authorization fact — it grants nothing and reveals nothing the row didn't already carry — and the people who should be able to fix a wrong name are the people working in the session. A contributor can already `/inject`, which is a strictly larger power.

### Queuing context without a turn (protocol 1.10.0)

`POST /inject` has always done two things at once: queue the message **and** wake the agent. `"wake": false` splits them.

```bash
curl -sS -X POST http://127.0.0.1:7777/sessions/s-4412/inject \
  -H 'Content-Type: application/json' \
  -d '{"message":"second alert corroborates the first","wake":false}'
# {"injected":"second alert corroborates the first","session":"s-4412","woke":false}
```

The message is appended to the inbox, published as the usual `inbox`/queued frame, and read by the next turn — but nothing here *causes* that turn. It does not pierce a sleep and it does not un-park a paused loop.

**Who this is for: machine producers.** An alert watcher's signals arrive on their own clock, and each one used to drive its own turn. Two corroborating alerts two minutes apart meant two wakes, the second landing while the agent was still working the first. Queued, they drain together as a single `[Inbox]` block on whatever turn happens next — and because a wake-driven turn has no operator prompt of its own, that block carries [bundle-handling guidance](/embed/api/#agentinjectmessage--queue-a-message-for-the-next-turn) telling the model to treat variants as one and to acknowledge, rather than re-open, corroboration on work it already finished. Operator input should keep waking — that is what the default is for.

**There is no promptness guarantee, and that is not a hedge.** An autonomous loop reaches the message on its own sleep timer; an operator-driven session reaches it when the operator next says something; a parked session reaches it when it is resumed. If the message needs to be acted on, send it without `wake: false`.

**`woke` comes back on both paths.** Its absence means a pre-1.10.0 daemon, which always woke — so a client can tell "this daemon deferred" from "this daemon doesn't know how to."

**501** means the agent registered no deferral capability. The request is refused rather than quietly upgraded to a waking inject: a silent upgrade would hand back exactly the preemption the caller asked to avoid, behind a 200 that says nothing went wrong.

Omitting `wake`, or sending `true`, is the historical behavior in every respect — the flag is a tristate so no pre-1.10.0 client changes meaning.

### Interrupt, pause, and resume (protocol 1.5.0)

`POST /interrupt` **parks the loop by default**. Cancelling the in-flight turn alone was never enough to stop an autonomous agent: the wake loop, the scheduler, or auto-continue would drive a fresh turn seconds later, and the operator's stop read as having done nothing. With the hold, the agent enters a real `paused` state — `GET /status` reports `state: "paused"` with `paused_since` / `pause_reason` / `interrupted` — and starts no new turn until it is resumed. Send `{"hold": false}` for the pre-v1.5.0 cancel-and-carry-on behavior.

Three ways out of a park, matching Esc-then-answer in an interactive session:

| Disposition | Call | Effect |
|---|---|---|
| Steer | `POST /resume {"steer":"..."}` | Queues the instruction under interrupt framing (the model is told its last turn was killed by an operator, so it doesn't silently redo the abandoned work), opens the gate, wakes the loop. |
| Continue | `POST /resume` (empty body) | Queues a carry-on note, opens the gate, wakes the loop. |
| Abandon | `POST /resume {"mode":"abandon"}` | Opens the gate and injects nothing; the agent stays quiet until something else drives it. |

`POST /inject` also releases a hold implicitly (auto-continue's own notes excluded — a timer must not un-park a loop a human parked). So the long-standing interrupt-then-inject client pattern behaves exactly as it did before protocol 1.5.0, and a TUI whose send path is `/inject` needs no resume call to steer.

`POST /pause` is the same park **without** killing an in-flight turn — "stop after this one". A turn already running has no safe suspend point inside a model call, and reporting `paused` while tokens keep burning would be a lie, so the running turn finishes and the *next* one is what waits.

Both are idempotent: a second `/pause` returns `paused: true, transitioned: false`, and resuming an agent that isn't paused is a `200` with `resumed: false`, so two operator surfaces racing the same click don't produce a spurious failure. The first cause of a pause wins — a plain `/pause` landing on top of an operator interrupt doesn't erase the fact that work was cancelled.

Interrupting the parent **does not stop background subagents**: their runs aren't resumable, so killing them stays an explicit choice. Every `/interrupt` response lists what's still running in `running_subagents`; `{"stop_subagents": true}` stops them all, and `POST /agents/{name}/stop` stops one by name.

Clients watching `/events` see a `pause` frame on every transition (`{"state":"paused"|"resumed", "reason":..., "interrupted":bool, "mode":..., "at":...}`), emitted by the agent rather than the handler, so a park triggered in-process (an embedded TUI, a library caller) reaches remote operators identically.

The cancelled turn ends with a `turn-error` frame of `kind: "canceled"`, `retryable: false` (protocol 1.8.0 — see [`turn-error` kinds](#turn-error-kinds)). A pre-1.8.0 daemon reports the same cancel as `transient_network` / `retryable: true`, so a client that offers a retry off that flag will offer to re-run the work the operator just stopped — check `protocol_version` before wiring one.

The `/interrupt` audit event (`Author=attach/interrupt`) is written by the agent from inside its own turn loop, *after* the interrupted turn finishes unwinding — so it lands on the `/events` stream shortly after the `200` response, not synchronously before it. This avoids racing the runner's in-flight session write, which otherwise surfaced the operator's clean cancel as a spurious stale-session turn error. A consumer that needs to confirm the audit row should tail `/events` rather than assume it is present the instant `/interrupt` returns.

### Wake notifications (protocol 1.7.0)

A **wake** is the agent's wake signal firing: something out-of-band decided the loop should look at the world again. In a local session that signal is an in-process channel; over attach it is a `wake` frame on `/events` ([#802](https://github.com/go-steer/core-agent/issues/802)):

```
event: wake
data: {"at":"2026-08-19T14:32:05.117Z"}
```

**Who produces one.** In a stock daemon, two things. `POST /wake`, and — since v2.9 — **a background subagent reporting**: every alert a subagent pushes wakes the parent so the report is read on the turn that follows instead of waiting for whatever would have started one ([#780](https://github.com/go-steer/core-agent/issues/780)). A third producer is a host that calls `Agent.RequestWake` (or `autonomous.Handle.RequestWake`) for a source the runtime knows nothing about; `dev/uat/scheduled-monitor` is the worked example. Do not build a consumer that assumes a wake means an alert is sitting in the inbox — the subagent case delivers through the model's prompt as a `[Background reports]` block, not through the inbox, and a bare `POST /wake` carries no pending work at all.

Four more things are worth knowing before you build on it:

- **It is an edge, not a state.** There is no matching "unwake" and nothing to reconcile on reconnect. The payload is only `at` because the thing that did the waking mostly reports itself through its own frames — an inject as `inbox`, a subagent's work as `agent` events, the resulting turn as `status-update` / `turn-complete`. A wake adds "look now" and nothing else. The one producer with no frame of its own is a subagent's report: `pkg/agent/background` emits nothing on the wire, so for an attached operator the `wake` frame *is* the notification that a child reported.
- **`POST /inject` deliberately does not produce one**, even though it fires the same signal internally. The inject already announces itself as an `inbox` frame, and a wake on top would make every prompt an operator types raise an attention notice about their own typing. `POST /wake` carrying a `prompt` is the one call that produces both, because it is an inject and an explicit wake request at once.
- **Coalescing is not promised in either direction.** The agent's wake signal is a one-slot channel that drops a fire while one is pending, and consumers should do the same rather than count frames.
- **It is emitted by the agent, not the handler** — the same choice `pause` makes. The non-HTTP producers above never touch a handler, and putting the emit there would hide them from exactly the operator who cannot see the process.

Detect it the normal way: look for `"wake"` in the `capabilities` frame's `event_types`. A pre-1.7.0 daemon omits it and never sends the frame, so a client written against 1.7.0 degrades to no notifications rather than to an error — which is exactly what every attached operator got before this version existed, because the TUI's side of the wiring never matched the interface it claimed to implement.

:::caution[What core-tui renders today]
core-tui v0.22.0 answers a wake with a toast **and** a permanent `system` row reading *"Wake signal received — an external alert (typically a background subagent's report) is waiting in the inbox."* That copy is now right for the most common producer — a subagent's report, which since v2.9 wakes on its own — except for the last three words, since the report is delivered through the next turn's prompt rather than the inbox; and it is wrong for a bare `POST /wake`, where nothing is waiting at all. The payload carries no `reason` a consumer could branch on — and cannot, because `coretui.WakeRequester` is a `<-chan struct{}` with no payload — so the copy has to be generalised on the core-tui side. Local sessions have rendered the same row for the same reason since the capability landed; attach mode is reaching parity with it, not introducing it.
:::

### Side questions (`/slash/btw`)

A side question is answered **outside** the session's turn loop: it runs
one model call over a read-only copy of the conversation, persists
nothing to the event log, and never interrupts an in-flight turn. It is
the "ask about what's happening without touching what's happening"
channel.

The request is deliberately tool-less — no tool declarations, no
provider builtins (search, code execution), no context-cache seeding. A
side question that could call tools would take actions the operator
didn't ask for, and seeding a cache from a request that carries no
system instruction would poison every later turn in the session.

The daemon prepends a short session-status preamble (state, model, turn
count, session cost, inbox depth, running subagents) to the question, so
"what are you doing?" and "how much has this cost?" are answerable
without the model having to guess from the transcript.

**An empty answer is a `200`, not a `500`.** When the model returns no
text — a safety block, an empty candidate list, a bare finish reason —
the response is `{"empty": true, "detail": "finish_reason=SAFETY"}` with
no `answer`. `detail` carries the provider's own stated reason when
there is one (`error=<code>: <msg>` or `finish_reason=<X>`) and is
omitted when there isn't. Only genuine failures (transport, auth,
provider error) are `500`s, so a client can tell "the model declined"
apart from "the daemon is broken" — the two used to be indistinguishable.

The five `/slash/*` endpoints run unbounded model work per request, so
they sit behind the per-caller cost limiter (10/min, burst 5). Over the
limit is a **429** with `Retry-After` and
`{"error":"rate limited","retry_after_seconds":N}`. They are also
synchronous — the POST blocks for the whole model call — so a client
must give them a deadline that fits a slow model over a long history,
not its ordinary RPC timeout. The bundled clients allow 5 minutes for
`/slash/*` (and keep the shorter deadline for everything else); cancel
the request context to abandon one early.

## UsageMetadata schema

`GET /sessions/{sid}/usage` (v2.7.0-dev.3+, [#222](https://github.com/go-steer/core-agent/issues/222)). Response type `attach.UsageInfo`:

```json
{
  "overall": {
    "input_tokens": 12450,
    "input_tokens_cached": 8320,
    "input_tokens_cache_write": 4000,
    "input_tokens_uncached": 130,
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
- **`input_tokens_cached` / `input_tokens_cache_write` / `input_tokens_uncached`** — three **disjoint** subsets of `input_tokens`; they sum to it. `_cache_write` is tokens billed at a premium for *establishing* a cache entry (Anthropic's `cache_creation_input_tokens`: 1.25× the base input rate), as opposed to `_cached`, which is the discounted *read* of an existing entry. Providers that don't bill writes per token (Gemini/Vertex charge cache storage per hour instead) report `0`, and the key is omitted (v2.9+, [#263](https://github.com/go-steer/core-agent/issues/263)).
- **`per_turn`** — the v2.7-dev.3 addition. Submission-ordered list, `turn` is 1-based. `total_tokens` matches Google's `UsageMetadata.TotalTokenCount` convention.
- **`ts`** — RFC3339. Marks the model call, not the operator submission.
- **`tool_use_tokens`** — Anthropic-specific; 0 for Gemini providers.
- **`digest_methods`** — MCP pruner attribution ([Digest & MCP wrap](/concepts/mcp/#agentic-wrap)). `counts` is calls per strategy; `bytes_saved` is aggregate response-size reduction.

`omitempty` on secondary fields — a JSON consumer should treat missing keys as `0` / absent.

## Peer / hub endpoints

Registered only when `Options.PeerRegistry` is non-nil (daemon launched with `--attach-peer-hub`). Peer endpoints go through the transport layer (shared token / mTLS). When **multi-session auth** is enabled they additionally require an authenticated, non-anonymous caller and enforce owner-scoping (v2.8+, [#384](https://github.com/go-steer/core-agent/issues/384)); single-user daemons keep the transport token as the only gate.

| Method | Path | Request | Response |
|---|---|---|---|
| `POST` | `/peers` | `{"name":..., "endpoint":..., "labels"?:{...}, "heartbeat_ttl_sec"?:...}` (16 KiB cap) | **201** `{"registration_id":..., "name":..., "endpoint":..., ...}`. `endpoint` must be an **absolute http/https URL with a host** — otherwise **400** (`javascript:`, relative, host-less, `ftp:` all rejected). The registering caller is recorded as the registration's **owner**. Name-based upsert. **401** anonymous (multi-session). |
| `GET` | `/peers` | `?label=k=v` (repeatable filter) | **200** `{"peers":[{...}]}`. `registration_id` is returned **only** to the registration's owner or an admin — redacted (`omitempty`) for everyone else, closing the enumerate-then-delete vector. **401** anonymous (multi-session). |
| `POST` | `/peers/{id}/heartbeat` | — | **200** `Peer` (extended lease); **404** unknown id. |
| `DELETE` | `/peers/{id}` | — | **204** on success; **403** when the caller is neither the owner nor an admin; **204** (idempotent) on unknown id. **401** anonymous (multi-session). |

### Durable peer state

The registry is in-memory by default: a hub restart drops every registration, and each peer stays invisible until its next heartbeat fails and it re-registers — a 20–60s window in which "who's in the fleet?" answers wrong rather than slowly.

`--attach-peer-state-file` / `attach.peer_state_file` ([#595](https://github.com/go-steer/core-agent/issues/595)) snapshots the registry to a JSONL file on every register, heartbeat, deregister, and prune, and reloads it at startup. Notes that matter in a deployment:

- **Leases are honored across the restart.** An entry whose lease expired while the hub was down is dropped on load, not resurrected — a dead peer briefly reported as live is a worse answer than a live peer briefly missing.
- **The file is a capability store.** It holds registration IDs, which are what `DELETE /peers/{id}` authenticates with. Written `0600`; give the directory the same treatment, and prefer a volume that outlives the pod.
- **Ownership survives.** The owner recorded at registration is persisted alongside each peer, so the owner/admin checks on `DELETE` and on `registration_id` visibility behave the same before and after a restart. (The wire shape deliberately never exposes `owner`; the file format is separate from it for exactly this reason.)
- **It fails loudly.** A state file that exists but can't be read, or a directory that can't be written, fails startup instead of quietly running in-memory. Individual malformed *lines* are the exception: they're skipped with a warning, since those peers re-register within a heartbeat.
- Setting the flag without `--attach-peer-hub` is a startup error, not a no-op.

### Calling a peer from the model (`call_peer`)

The endpoints above make the fleet *visible*; [`tools.call_peer`](/reference/configuration/#toolscall_peer-v29) ([#595](https://github.com/go-steer/core-agent/issues/595)) makes it *reachable* from a turn. Enabling it on a hub gives the model one delegation tool whose only destination source is the registry described here — it takes a peer `name` and a `prompt`, never a URL. `enabled: true` without both attach mode and `--attach-peer-hub` fails startup, so the tool never exists in a process that couldn't resolve a destination anyway.

What a call does on the wire, against the peer's own attach server:

1. `POST /sessions` — a **fresh session per call**. Concurrent callers can't interleave prompts into one transcript, and the reply is unambiguously the answer to this request. The peer must therefore have `attach.multi_session.enabled`; without it the peer answers **501** and the tool appends that fix to the error.
2. `GET /sessions/{app}/{sid}/events` — subscribed **before** the prompt goes in. `turn-complete` is a live typed frame and is not replayed from the event log, so a stream opened after the inject can miss the turn end entirely.
3. `POST /sessions/{app}/{sid}/inject` — the prompt.
4. Read until turn end (typed `turn-complete`, ADK's `TurnComplete`, or a final non-partial model event with no tool call), then return the peer's text plus the `session_id`, so an operator can go read the delegated turn in the peer's event log.

Bounds are the caller's, not the peer's: one `timeout_seconds` deadline spanning all four steps, and one `max_response_bytes` cap after which the tool stops reading and flags `truncated`. A `turn-error` frame from the peer is surfaced with its kind and message intact rather than flattened into "the call failed".

Authentication uses the peer's transport auth — a bearer token read from `token_env` in the *hub's* environment. It is never part of the tool schema or the arguments, so it cannot leak into a transcript, and a configured-but-unset variable is an error rather than an anonymous request.

## Non-session routes

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/.well-known/agent-card.json` | **none** (bypasses transport auth) | Public agent-card discovery. Enabled when `AgentCard.Description` + `ExternalURL` are both non-empty in the daemon config. 405 on non-GET/HEAD. |
| `GET` | `/whoami` | Transport auth (no per-session ACL) | Returns `{"identity":..., "admin":bool, "source":..., "proxy_by":...}` for the current caller. `source ∈ {"bearer","mtls","iap","asserted","anonymous"}` (consumers tolerate unknowns). `proxy_by` populated only when `source="asserted"` (X-Asserted-Caller path). Companion to the SSE `capabilities.caller_id` display hint. Wire shape pinned by the [conformance fixture](https://github.com/go-steer/core-agent/blob/main/pkg/attach/testdata/conformance/rest-whoami-v1.json). |
| `GET` | `/ui/*` | Transport auth | Optional SPA passthrough — only when `Options.UI` is non-null. `/ui` (no trailing slash) → **301** → `/ui/`. |

## Streaming endpoints (summary)

Two SSE endpoints:

| Path | Content-Type | Cursor | Notes |
|---|---|---|---|
| `GET /sessions/.../events` | `text/event-stream` | `?since=<int64>` | Lossless replay via cursor. **412** when session has no eventlog. **409**/**400** on incompatible/malformed declared protocol version (`?protocol=` / `X-Attach-Protocol-Version`). Frames typed via `event: <type>` header (or legacy `event: agent`). `X-Accel-Buffering: no` + `Cache-Control: no-cache`. |
| `GET /sessions/.../perms/stream` | `text/event-stream` | none | Per-prompt frames: `event: prompt`. **501** without `PromptBrokerProvider`. |

The `since` cursor is monotonic per-session — the TUI's `/reconnect` slash sends `?since=<lastSeq>` to resume without missing events across reconnects.

### `capabilities` frame

The first frame on every `/events` stream is `event: capabilities` — the client advertises the wire contract before any state flows. The full field list lives in [the SSE spec](https://github.com/go-steer/core-tui/blob/main/docs/sse-event-stream-protocol.md#21-capabilities); the current additions are:

- **`features`** — feature-flag map derived from live runtime state. Suggested keys: `multi_session`, `perms_stream`, `cost_ceiling`, `guardrails`, `observer_mode`, `mcp`, `specialists`, `cross_daemon`, `interrupt`, `pause`. `guardrails` means `GET /guardrails` + `POST /guardrails/reset` are serviceable; `cost_ceiling` means a per-turn or per-session spend bound is **armed** (a turn can actually be refused for spend), not merely that the key is understood. Consumers treat absent keys as "off / unknown"; producers MAY add unknown keys.
- **`slash_commands`** — dynamic list of the slash names this agent's `POST /slash/<name>` will accept. Derived from capability-interface presence (`CompactSlashProvider` → `"compact"`, etc.). Clients render only what the connected agent supports.
- **`agent`** — the producing agent's own identity: `{name, version, description, model, provider, url}`. Consolidates fields previously scattered across `/.well-known/agent-card.json`, `GET /status`, and the `server` banner.
- **`caller_id`** — the resolved caller identity display hint. Canonical source: `GET /whoami`.

`status-update` also carries an optional `capabilities` field (merge semantics) for future hot updates — no producer emits it today, but consumers MUST tolerate its absence and MUST merge (not replace) when it does arrive.

### `turn-error` kinds

A failed turn ends with `event: turn-error` carrying `{kind, code?, message, retryable, hint?}`. `kind` is an open enum — the spec requires consumers to **treat an unrecognized value as `unknown`**, and no in-tree consumer switches on it exhaustively, so a producer can add one without breaking a reader — and `retryable` is the one decision the payload asks a client to make:

| `kind` | `retryable` | Raised by |
|---|---|---|
| `config_error` | false | Malformed request or provider config — a URL that won't parse, `INVALID_ARGUMENT`, `FAILED_PRECONDITION`, 400. |
| `auth_error` | false | IAM / credentials / OAuth failure (401, 403, `PERMISSION_DENIED`). |
| `model_not_found` | false | Model name / location mismatch (404, `NOT_FOUND`). |
| `rate_limited` | true | Quota or rate limit (429, `RESOURCE_EXHAUSTED`). |
| `transient_network` | true | Unreachable or timed-out upstream (502/503/504, `UNAVAILABLE`, **and a model call that hit its deadline**). |
| `cost_ceiling` | false | A configured per-turn or per-session spend bound tripped. The operator must reset it. |
| `watchdog` | false | The behavioral watchdog tripped a Critical runaway signal under `--watchdog=enforce`. The operator must reset it. |
| `canceled` | false | The turn's context was cancelled — see below (protocol 1.8.0). |
| `unknown` | false | Anything the classifier couldn't categorize. `message` still carries the upstream text. |

**`canceled` (protocol 1.8.0, [#816](https://github.com/go-steer/core-agent/issues/816)).** Every cancel is a deliberate stop: `POST /interrupt`, the TUI's Esc, a parent-context cancel at daemon shutdown, or a guardrail cutting the turn short in flight. Re-running the work is the opposite of what was asked for, so `retryable` is false and a client that wires a retry prompt off the flag must not offer one. Before 1.8.0 these arrived as `transient_network` / `retryable: true` — self-contradicting next to their own `code: "CANCELED"`, and enough to make a retry-offering client undo an operator's stop. A client built against a pre-1.8.0 daemon is required by the spec to treat the value it doesn't recognise as `unknown`; core-tui v0.22.0 (the pinned client) maps only an *empty* kind to `unknown` and otherwise prints the string it was given, gating its `↻ retryable` line on the boolean, so it renders the new value correctly with no client change.

Two consequences worth knowing. A cancel and a **timeout** are now on opposite sides of the flag: `context.DeadlineExceeded` stays `transient_network` / retryable, because nobody asked for it. And an in-flight guardrail halt produces **two** `turn-error` frames — the guardrail's own (`cost_ceiling` or `watchdog`, carrying the operator-facing reason) followed by the `canceled` for the cut turn — so read the first one for *why*; that pairing predates 1.8.0 and only the second frame's `kind` changes.

The same value rides `error.type` on the `gen_ai.agent.invocation.duration` metric where metrics are enabled, so a dashboard keyed on a `transient_network` rate stops counting deliberate stops as network failures on a daemon carrying this change.

**Guardrail-refused turns are labelled (v2.9.0-dev, [#818](https://github.com/go-steer/core-agent/issues/818)).** A turn refused at the top by an already-tripped guardrail emits no frame of its own — it points back at the `cost_ceiling` / `watchdog` frame from the trip that halted the session — but it *is* recorded on `gen_ai.agent.invocation.duration`. Before this change that record carried `error.type: unknown`: the classifier is substring-based and a guardrail reason matches none of its patterns, so `cost_ceiling` and `watchdog` were the only kinds in the table above that no classifier path could produce, and the spend-cap and runaway series went dark during exactly the incidents they exist for. Refusals now carry their own kind. Nothing on the wire changes.

### Protocol version negotiation

The `capabilities` frame carries the server's `protocol_version`, but a client can also fail fast *before* opening the stream. On the `/events` request a client MAY declare the version it speaks with the `?protocol=<semver>` query param or the `X-Attach-Protocol-Version` header (the query param wins when both are present). The server:

- echoes the version it speaks on the `X-Attach-Protocol-Version` **response** header (always, success or failure), and
- rejects a declared **major** that differs from its own with **409 Conflict**, or a malformed version with **400 Bad Request**.

Only the major is enforced — minor/patch differences within a major are compatible by the protocol's additive-field convention (older clients ignore unknown fields; older servers omit newer ones). Clients that declare nothing are accepted unchanged, so every pre-negotiation client keeps working. A future breaking (major) bump therefore fails cleanly on skewed clients instead of silently mis-rendering.

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
| **400** | Bad request — empty required field (message, patterns, ...); unknown `/resume` mode, or `mode=steer` with no text; an `owner` field on `PATCH /acl` or `POST /sessions` that isn't the current owner (`""` included); an omitted `title` on `POST /title` (send `{"title":""}` to clear). |
| **401** | Unauthenticated — missing / wrong bearer token; bad proxy assertion. |
| **403** | Forbidden — `--attach-readonly` writes; delete of the bootstrap `"default"` session; cross-origin `Origin` header on a write (CSRF protection). |
| **404** | Not found OR auth-deny (deliberately indistinguishable to avoid SID enumeration); `POST /agents/{name}/stop` for a subagent that isn't running. |
| **405** | Method not allowed — e.g. `POST /.well-known/agent-card.json`. |
| **409** | Conflict — shortcut SID ambiguous across apps; `POST /sessions` on `ErrSessionExists`; `POST /guardrails/reset` when the reset would immediately re-trip. |
| **412** | Precondition failed — session has no eventlog (SSE reader); neither `PauseController` nor `InterruptProvider` (interrupt). |
| **415** | Unsupported media type — state-changing request without `Content-Type: application/json` (CSRF protection). |
| **429** | Rate limited — the per-caller cost limiter on `/slash/*` (10/min, burst 5). Carries `Retry-After` and `{"error":"rate limited","retry_after_seconds":N}`; retryable. |
| **500** | Internal error — factory failure on `POST /sessions`; second `DELETE` of a gone session; a `PATCH /acl` whose persistence failed (the in-memory ACL is rolled back, so a retry is safe). |
| **501** | Not implemented — capability provider absent (`SessionFactory`, `InterruptProvider`, `PromptBrokerProvider`, wake `target`, etc.). |

## Idempotency

| Endpoint | Idempotent? |
|---|---|
| `DELETE /sessions/{sid}` | **No** — first call **204**, second call **500** (`ErrSessionNotFound`). Callers that retry on transient failure should treat 204 and 500 as equivalent success. |
| `DELETE /peers/{id}` | **Yes** — unknown id also **204** (owner/admin only; **403** otherwise). |
| `POST /sessions` | **No** — every call spins a fresh session. |
| `POST /peers` | Effectively **yes** — name-based upsert extends the lease of an existing peer. |
| `PATCH /sessions/{sid}/acl` | **Yes** — the listed fields are replaced, not merged, so replaying the same body lands on the same ACL. |
| `POST /perms/respond` | **No** — second respond for the same prompt → **404** (`ErrPromptNotFound`). |
| `POST /sessions/{sid}/title` | **Yes** — the title is replaced, so replaying the same body lands on the same name. |
| `POST /sessions/{sid}/inject` | **No** — every call queues another message. Redelivery after a `503` is the deliberate exception: a duplicate in the inbox beats a silently lost signal. `"wake": false` doesn't change this. |
| `POST /interrupt` | Idempotent in effect — the loop ends up cancelled and parked either way. Repeat calls while the cancelled turn is still unwinding keep reporting `interrupted: true` (the interrupt did land); once it's idle they set `X-Interrupted: nothing-in-flight`. |
| `POST /pause` / `POST /resume` | Idempotent — `transitioned` / `resumed` report whether *this* call changed anything, so a redundant press is a quiet `200`. |

## See also

- [Attach TUI](/reference/attach-tui/) — client-side behavior, permissions bridge, multi-daemon workflow.
- [`core-agent-tui` CLI reference](/reference/core-agent-tui/) — the reference client for this protocol.
- [Configuration → attach](/reference/configuration/) — daemon-side listener knobs.
- [Multi-session daemon](/concepts/multi-session/) — the per-caller ACL + admin identity model that shapes this API's authorization behavior.
