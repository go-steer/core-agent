---
title: Multi-session daemon
weight: 80
---


One `core-agent` daemon serves multiple concurrent sessions belonging to different users — each with its own identity, ACL, permission grants, instruction overlays, and audit attribution. Multi-session is **opt-in** and **strictly backward-compatible**: deployments that don't enable it see identical single-user behavior.

This page covers when to turn it on, how to configure it, and what isolation guarantees the substrate gives you. Design background: `docs/multi-session-design.md` in the repo.

---

## When to use it

The single-user "one process per user per pod" model is fine for personal interactive sessions and per-tenant pod-per-user K8s deployments. Reach for multi-session when **one or more** of these is true:

- **Multi-tenant platform-agent deployments.** A platform team wants every engineer to have their own session(s) in a shared daemon, without each one needing their own pod.
- **Shared chat-channel sessions.** A Slack/GChat/Teams bot fronts a session that channel members collectively contribute to; per-prompt attribution needs to thread through audit logs and per-caller MCP credentials.
- **Multi-session-per-user workflows.** A single user driving multiple parallel agent sessions from a chat-bot front-end (different threads, different tasks) wants those to be genuinely independent — separate context, separate permission grants, separate plan-first state.
- **Operator + agent separation in headless deployments.** An alerting system POSTs `/inject` on behalf of multiple downstream operators; today's single bearer token can't distinguish them.

If none apply, leave multi-session off — the configuration surface is small but non-zero.

---

## Enabling it

Multi-session is configured under `attach.multi_session` in `.agents/config.json`. Minimum usable shape:

```json
{
  "version": 1,
  "attach": {
    "listen": ":7777",
    "multi_session": {
      "enabled": true,
      "auth": {
        "kind": "bearer_table",
        "table_file": "/etc/core-agent/users.json"
      },
      "admin_identities": ["ops@example.com"]
    }
  }
}
```

When `enabled: true`, every request entering the attach listener resolves to a `Caller` via the configured `Authenticator`. The Caller threads through:
- per-session ACL enforcement (only the owner / viewers / contributors / admins can see or write to a session)
- the eventlog metadata sidecar (every event row carries `caller` + optional `proxy_by` so audit queries are "who did what")
- the per-caller instruction overlay path (each user can have their own `.agents/` directory layered on top of the daemon-wide instructions)
- outbound MCP tool calls (servers that inspect the caller can use it for downstream IAM / 3LO credentials)

### `users.json` — the bearer table

The static user table is the v2.4-shipped Authenticator. OIDC / JWT / mTLS / K8s ServiceAccount are designed but deferred.

```json
{
  "version": 1,
  "users": [
    { "identity": "alice@example.com", "token": "tok_alice_...", "labels": { "team": "platform" } },
    { "identity": "bob@example.com",   "token": "tok_bob_...",   "labels": { "team": "infra" } },
    { "identity": "sa:cron-runner",    "token": "tok_cron_...",  "labels": { "kind": "service" } }
  ]
}
```

**File-mode requirement:** the loader rejects `users.json` with group- or world-readable bits set. Mode `0600` or stricter (`0400`). Failing this is a startup error, not a warning — bearer tokens deserve the same posture as a private SSH key.

Generate tokens with whatever your secret manager uses; the loader has no opinion. A simple bootstrap:

```bash
for who in alice bob ops sa-cron; do
  echo "$who: $(openssl rand -hex 32)"
done
```

---

## Authorization model

Every session has an ACL with three roles. Authorization is per-action, not per-resource — the matrix is intentionally compact:

|                  | Admin | Owner | Viewers | Contributors |
|------------------|:-----:|:-----:|:-------:|:------------:|
| `SessionList`    |   ✓   |   ✓   |    ✓    |       ✓      |
| `SessionRead`    |   ✓   |   ✓   |    ✓    |       ✓      |
| `SessionWrite`   |   ✓   |   ✓   |         |       ✓      |
| `SessionAdmin`   |   ✓   |   ✓   |         |              |
| `DaemonAdmin`    |   ✓   |       |         |              |

- **Admin** identities (`admin_identities` in config) bypass every check. Use sparingly.
- **Owner** — the identity that created the session. Full access except `DaemonAdmin`.
- **Contributors** — can inject and use existing grants, can't modify the ACL. Used for shared sessions where multiple identities take turns (e.g., a Slack channel where every member can DM the agent).
- **Viewers** — read-only. Can stream events and read state; can't inject.

**Denied requests return 404, not 403.** This is intentional — hiding the existence of unauthorized sessions prevents an attacker from enumerating session IDs through differential responses. Audit logs on the server side capture the real reason.

### Anonymous and default identity

- `default_identity` (default `"anon"`) — the Caller stamped on requests that don't carry a credential. Used by single-user mode (where it's the only identity) and as the AllowAnonymous fallback when multi-session is on.
- `allow_anonymous` (default `false`) — when `true`, unauthenticated requests resolve to the DefaultCaller instead of returning 401. **Dangerous in shared environments** — every unauthenticated request becomes the same identity. Leave it off unless you're running on a trusted internal network and explicitly want that posture.

---

## Shared-session pattern (chat-bot integration)

A common ask: a Slack/GChat/Teams bot fronts ONE session that the whole channel contributes to. The bot authenticates as itself; each per-channel message asserts the human user's identity so audit logs and per-caller MCP credentials attribute to the human, not the bot.

The substrate supports this via the **proxy role** + `X-Asserted-Caller` header:

```http
POST /sessions/incident-channel/inject
Authorization: Bearer tok_slack_bot_...
X-Asserted-Caller: alice@example.com
Content-Type: application/json

{"message": "investigate the 5xx spike on checkout-svc"}
```

Configuration:

```json
{
  "attach": {
    "multi_session": {
      "enabled": true,
      "auth": { "kind": "bearer_table", "table_file": "/etc/core-agent/users.json" },
      "admin_identities": ["ops@example.com"],
      "proxy_identities": ["sa:slack-bot", "sa:gchat-bot"],
      "asserted_caller_header": "X-Asserted-Caller"
    }
  }
}
```

The `proxy_identities` allowlist is what makes this safe — a compromised bot can only assert identities **that the operator has provisioned in `users.json`**, and the audit log records BOTH identities (`caller=alice@…` + `proxy_by=sa:slack-bot`) on every event in the turn.

Rules:
- Default is no proxy capability. Identities not in `proxy_identities` get 401 if they try to use `X-Asserted-Caller`.
- The asserted identity must exist in the user table. Bots can't invent identities — only assert ones the operator provisioned.
- Asserted-caller headers from non-proxy callers are logged (forensic trail) and rejected.

---

## Per-session isolation guarantees

When multi-session is enabled, each session gets a derived sub-gate with its own:

- **Permission grants** (`sessionAllow` / `sessionAllowTools` / `sessionAllowVerbs`) — alice's `/allow write_file allow-session` doesn't grant bob's session anything.
- **Plan-first flag** (`planRecorded`) — alice's `record_plan` doesn't unblock bob's mutating tools.
- **Approval audit** (`approvals`) — per-session interactive-decision log.
- **Permission mode** — alice toggling to `yolo` via TUI chip doesn't change bob's session's mode.
- **Prompter** — each session's UI hooks (TUI broker, HTTP prompt stream) are independent.

**What's still daemon-wide** (by design — operator model is "one config, many users"):
- `permissions.allow` / `permissions.deny` patterns from config
- `/allow` / `/deny` slash commands mutate the shared policy
- `AddAlwaysAllow` decisions (DecisionAllowAlways path) mutate the shared path scope

Per-session policy and path-scope carve-outs are deferred to a future release.

---

## Per-caller instruction overlays

Each Caller can have their own `.agents/` directory layered on top of the daemon-wide instructions:

```
/var/lib/core-agent/users/         <-- attach.multi_session.users_dir
├── alice@example.com/
│   └── .agents/
│       ├── AGENTS.md              <-- alice's role-shaped overlay
│       └── AGENTS.d/
│           └── 01-incident-runbook.md
└── bob@example.com/
    └── .agents/
        └── AGENTS.md              <-- bob's overlay
```

Configuration:

```json
{
  "attach": {
    "multi_session": {
      "enabled": true,
      "users_dir": "/var/lib/core-agent/users/"
    }
  }
}
```

The overlay loader runs the same `@include` + `AGENTS.d/` semantics as the project-scope loader (see [Instruction loader](../sessions/#instruction-loader)). Missing overlay directories are silently skipped — provision overlays for the callers who need them; the rest fall back to the daemon-wide instructions.

**Path safety:** Caller identities containing `/`, `\`, or `..` are rejected at load time. Email-shaped (`alice@example.com`) and service-account-marker (`sa:slack-bot`) identities pass.

---

## Audit log

Every event written to the eventlog carries a `Metadata` sidecar with:

| Key        | Value                                                        |
|------------|--------------------------------------------------------------|
| `caller`   | Effective `Caller.Identity` (e.g. `alice@example.com`)      |
| `proxy_by` | Proxying identity, when the call went through the proxy path (e.g. `sa:slack-bot`). Omitted otherwise. |

Query directly via SQL: each `agent_eventlog` row has a `Metadata` TEXT column carrying the sidecar JSON. "Who did what in this shared channel session" becomes:

```sql
SELECT seq, author, metadata
FROM agent_eventlog
WHERE session_id = ?
ORDER BY seq;
```

In single-user deployments, the metadata column is empty for every row — no behavior change on disk or wire.

---

## Migration story

Three phases for an operator moving from single-user to multi-user:

1. **Stay single-user** — no change. `multi_session.enabled: false` (the default).
2. **Enable with a static user table** — generate tokens, populate `users.json` at mode `0600`, hand them to operators. Each operator's `core-agent-tui --attach-token=<their-token>` resolves to their identity. Sessions they create are owned by them; they can only see their own.
3. **Switch to OIDC / mTLS / K8s SA** (when shipped, v2.5+) — change `auth.kind` to the new value; tokens come from the IDP. Users / sessions unchanged.

A `core-agent users migrate` CLI is **out of scope for v2.4** — operators with existing single-user data either keep using single-user mode or accept that legacy sessions become "unowned" (admin-only-accessible) when they enable multi-session.

---

## Recipe

See `examples/multi-session-bearer/` in the repo for a minimum-viable two-user starter you can run locally in five minutes.

---

## What's not in v2.4

Designed but explicitly deferred:

- OIDC / JWT / mTLS / K8s ServiceAccount Authenticators (interfaces only — bearer-table is the v2.4 implementation).
- Per-user quotas (tokens, cost, requests).
- User-management CLI (`core-agent users add/remove`). Edit `users.json` directly.
- Cross-daemon session migration. Sessions live in the daemon process that created them.
- Per-session policy / path-scope carve-outs (the shared-substrate limitation noted above).
- IDP federation across daemons (an SSO concern, not core-agent's).
