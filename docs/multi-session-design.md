# Multi-session core-agent: per-user auth + cross-session isolation

Design doc for v2.4's headline feature: one `core-agent` daemon
process safely serves multiple concurrent sessions belonging to
different users.

**Status:** proposed (2026-06-03). Awaiting approval before
implementation. v2.4 candidate.

## Motivation

Today the typical core-agent deployment is **one process per
session per user**:

- Local interactive: `core-agent` in a terminal — one session,
  one user.
- Headless daemon: `core-agent --attach-listen` — supports
  multiple sessions in principle (the `SessionRegistry` indexes by
  `appName/userID/sessionID`), but in practice every existing
  consumer wires a single `agent.Agent` and treats the daemon as
  single-user. `--attach-token` is one bearer for the whole
  daemon — anyone with the token can attach to any session.
- Per-pod: most K8s deployments run one core-agent per pod, one
  pod per user (the kube-agents Platform Agent pattern).

This works for the early "one agent per pod" pattern but doesn't
scale to:

1. **Multi-tenant platform-agent deployments.** A platform team
   running `core-agent` for their org wants every engineer to have
   their own session(s) in a shared daemon, without each one
   needing their own pod.
2. **Multi-session-per-user workflows.** A single user driving
   multiple parallel agent sessions from a chat-bot front-end
   (different threads, different tasks) wants those to be
   genuinely independent — separate context, separate permissions,
   separate plan-first state.
3. **Shared infrastructure with audit.** When the daemon runs
   long-lived in a managed environment, the operator needs to know
   "user X did action Y in session Z" — caller identity has to
   thread through the audit log.
4. **Operator + agent separation in headless deployments.** An
   alerting system can `--attach-listen` POST messages on behalf
   of multiple downstream operators; today there's no way to
   distinguish them.

The blocker for all four is the same: per-user identity in the
attach API + per-session isolation in the substrate.

## Goals

- **One daemon, many sessions, many users.** A single
  `core-agent` process holds N independent agent sessions,
  authenticates the caller per request, and serves only the
  sessions the caller is authorized for.
- **No cross-session context bleed.** Conversation history is
  already isolated (sessionID-keyed eventlog). Extend to:
  permission grants, plan-first flag, instruction overlays,
  in-flight tool calls. User A's `/allow write_file
  allow-session` must NOT carry over to user B's session.
- **Pluggable authentication.** Start with the smallest useful
  mechanism (bearer-token-per-user via a static table); design
  the interface so OIDC / JWT / mTLS / K8s ServiceAccount can
  layer in later without refactoring the call sites.
- **Pluggable authorization.** Session-owner-only access by
  default; design the ACL surface so "owner grants viewer/
  contributor to user X" and "operator-defined admin role with
  see-everything access" can layer in later.
- **Backward compatible.** Today's single-user single-token
  shape must continue to work unchanged when multi-session is
  not enabled. Opt-in via config.
- **Caller identity in the audit log.** Every gate decision,
  every tool call, every plan-first state change carries the
  identity of the caller who triggered it.

## Non-goals (v2.4)

- **User management UI.** No `core-agent users add` CLI or REST
  endpoint for managing the user table. Operators edit the auth
  config file directly or pipe in from external IDP (OIDC).
- **Quotas / rate-limiting per user.** Per-user budget
  enforcement (tokens, cost, requests) is interesting but
  separable; defer to v2.5.
- **Cross-daemon session migration.** A session lives in the
  daemon process where it was created; we don't try to relocate
  it. (Crash-resume via `--session-db` works the same as today.)
- **Inter-session message routing.** A user can't say "send this
  message to user B's session from inside mine." Sessions stay
  isolated end-to-end.
- **Federated identity across daemons.** Each daemon owns its
  own user identity scope. Federating across daemons (single
  sign-on across a fleet) is an SSO concern, not core-agent's.
- **OIDC / mTLS / K8s ServiceAccount in v1 implementation.**
  Interface designed for them; static bearer table is what
  ships first.

## Conceptual model

### Identity primitive: `Caller`

Every request entering the daemon carries (or fails to carry) a
`Caller` — an opaque identity that subsequent code uses for
authorization and audit.

```go
// pkg/auth (new package)

type Caller struct {
    Identity string            // stable opaque ID; "alice@example.com",
                               // "sa:platform-agent",
                               // "anon" if unauthenticated and anon is allowed
    Labels   map[string]string // free-form metadata from the auth source
                               // (e.g., "groups": "platform-team",
                               // "issuer": "https://oidc.example.com")
    Admin    bool              // see-everything role; set per config
}

// Authenticator extracts a Caller from an HTTP request, or returns
// ErrUnauthenticated if no valid credential is present.
type Authenticator interface {
    Authenticate(r *http.Request) (Caller, error)
}
```

Implementations (shipped in v2.4):
- `BearerTokenAuth(table map[string]Caller)` — token → Caller lookup.
- `AnonymousAuth()` — single fixed Caller; current single-user mode
  uses this implicitly.

Implementations (designed but not shipped in v2.4):
- `OIDCAuth(issuerURL, audience, mapClaims)` — JWT validation,
  claim-to-identity mapping.
- `MTLSAuth(certPolicy)` — cert SAN / subject DN → Caller.
- `K8sSAAuth(audience, namespace)` — projected ServiceAccount
  token validation via TokenReview.

### Ownership primitive: `Session.Owner`

Every session gains an `Owner Caller.Identity` field. Set at
creation; immutable for the session's lifetime. Sessions without
an owner (legacy / single-user mode) are accessible to any
authenticated caller (or anyone if no auth is configured).

### ACL primitive: `Session.ACL`

```go
type SessionACL struct {
    Owner       string   // Caller.Identity of creator; full access
    Viewers     []string // can stream events; can't inject or grant
    Contributors []string // can inject + use existing grants;
                         // can't modify ACL or grant new perms
}
```

v1 ships with only `Owner` populated; the `Viewers` /
`Contributors` fields are reserved (schema honored, behavior
deferred to v2.5).

### Authorization rules

Resolved by `pkg/auth.Authorize(caller, action, session)`:

| Action | Allowed if |
|---|---|
| `SessionList` | Always (results filtered per-caller; see below) |
| `SessionRead` (events stream, status, memory, perms, ...) | `caller.Admin` OR `caller == session.Owner` OR `caller ∈ session.ACL.Viewers ∪ Contributors` |
| `SessionWrite` (inject, slash commands, ...) | `caller.Admin` OR `caller == session.Owner` OR `caller ∈ session.ACL.Contributors` |
| `SessionAdmin` (modify ACL, delete session) | `caller.Admin` OR `caller == session.Owner` |
| `DaemonAdmin` (peer registry, global metrics, ...) | `caller.Admin` |

`SessionList` is a special case: the endpoint accepts the call
from any authed Caller but only returns sessions the Caller is
authorized to `SessionRead`. (Hiding the existence of unauthorized
sessions prevents leaking activity patterns.)

## Per-substrate isolation rules

The hard work isn't the auth model — it's making the substrate
isolate per-session state that today is daemon-wide.

### Permissions (`pkg/permissions.Gate`)

**Today:** one `Gate` per daemon. Session-allow / session-tool /
session-verb grants live on the Gate; `MarkPlanRecorded` flips a
single bool on the Gate.

**Problem:** user A grants `allow-session write_file`; user B's
session inherits the grant via the shared Gate. Same for
plan-first state.

**Fix:** introduce **per-session sub-gates**. The daemon owns a
"template gate" carrying the config-level mode + policy + scope.
Each session gets a derived sub-gate that:
- shares the template's mode / policy / scope (read-only reference)
- has its own session-allow / session-tool / session-verb maps
- has its own `planRecorded` flag
- has its own approvals audit log
- delegates the prompter to the per-session HTTP/stdin path

```go
// pkg/permissions

func (template *Gate) DeriveForSession(sessionID string, prompter Prompter) *Gate {
    // returns a new Gate that shares template's read-only config
    // (mode/policy/scope) but has its own per-session mutable state
    // (sessionAllow, planRecorded, approvals, prompter)
}
```

`agent.New` calls `template.DeriveForSession(sid, ...)` per
session. The shared template is constructed once at daemon start
from `permissions.FromConfig`.

Backward compat: when no derivation happens (single-session
mode), the template Gate IS the session gate. No behavior change.

### Instructions (`pkg/instruction.Loaded`)

**Today:** one `Loaded` per daemon, computed at startup by walking
`projectRoot/.agents/` and `userRoot/.agents/`. Same content
prepended to every session's system prompt.

**Problem:** in multi-tenant mode, user A and user B may want
different `AGENTS.md` content. A platform-agent serving a fleet
of internal users might give each user a different role-shaped
instruction overlay.

**Fix (v1 of multi-session):** keep daemon-wide instructions
as the baseline (same as today); add a **per-session instruction
overlay** that's loaded from the caller's user-scoped directory
(`<usersHome>/<caller.Identity>/.agents/`) and merged via the v2
loader's `AGENTS.d/` semantics.

```go
// pkg/instruction (extended)

func LoadForSession(projectRoot, userRoot string, caller Caller) (Loaded, error) {
    // Loads:
    //  1. projectRoot/.agents/ (shared, all sessions)
    //  2. userRoot/.agents/    (current ~/.agents/, all sessions)
    //  3. <multiSessionUsersDir>/<caller.Identity>/.agents/  (NEW; per-caller overlay)
}
```

The per-caller directory is configured via
`config.attach.multi_session.users_dir` (e.g.
`/var/lib/core-agent/users/`). Operator drops a `.agents/`
directory there per user; the v2 loader handles merging.

If `multi_session.users_dir` is unset, behavior is unchanged
(daemon-wide instructions only).

### MCP servers (`pkg/mcp`)

**Today:** MCP servers are constructed once at daemon start
from `.agents/mcp.json`. The connection is shared across
sessions. Tool calls flow through with no per-session
attribution. Outbound auth is daemon-identity 2LO only (current
`AuthSpec.GoogleOAuth` uses ADC of the daemon's service identity).

**Problem (two layers):**

1. **Identity propagation.** If an MCP server enforces caller
   identity (e.g., GKE MCP authorizing on the caller's IAM,
   not the daemon's), every user's actions are attributed to
   the same identity downstream. Audit trail outside the daemon
   is broken.
2. **Credential resolution.** Even if identity propagates,
   today there's no way to fetch a per-caller credential
   (Alice's GitHub OAuth token vs Bob's) and inject it on the
   outbound MCP call. The current `google_oauth` strategy is
   2LO only.

**Fix (Phase 3 of multi-session):** ship the **identity
propagation** half only. The agent loop sets the `Caller` on
the tool-call context; `pkg/mcp` propagates it to the outbound
call.

```go
// pkg/mcp (extended)

// When the agent loop calls an MCP tool, the gate's CallContext
// now carries the Caller identity via pkg/auth.CallerFromContext.
// MCP servers that want per-call identity inspect it; the pkg/mcp
// layer just propagates.
```

This is **opt-in for MCP server authors** — existing servers
that don't inspect it work unchanged.

**Credential resolution is a sibling design**, not part of
Phase 3. See
[`docs/mcp-credential-resolution-design.md`](./mcp-credential-resolution-design.md)
(task #13) for the pluggable `CredentialProvider` interface, 3LO
provider implementations (Auth Manager via
`iamconnectorcredentials`, OAuth2 direct), shared provider
config blocks, and per-caller credential caching. That design
depends on this one's Caller propagation but is otherwise
independent; the two pieces can land sequentially or in parallel.

A more ambitious "per-caller MCP connection pool" (separate
gRPC/stdio session per caller) is deferred to v2.5 — most MCP
servers don't need it.

### Tools (`pkg/tools`)

**Today:** tools are constructed once per daemon via
`tools.Build(cfg, gate, agentsDir, builtin)`. The tool registry
is shared across sessions.

**Problem:** less of a problem than the others — tools themselves
are stateless wrappers; what they DO is gated by the per-session
sub-gate (after the fix above).

**Fix:** none required at the substrate level. Per-session
sub-gates handle the isolation. The `record_plan` tool's
handler closes over the session's gate at construction time —
since each session gets its own gate (above), each session's
`record_plan` flips its own flag.

What DOES change: tool registration becomes per-session in the
multi-session deployment. `tools.Build` is called once per
session (with that session's sub-gate) rather than once per
daemon. Small refactor in the construction path; no API change.

### Background subagents (`pkg/agent.BackgroundAgentManager`)

**Today:** subagents inherit the parent's `*Gate` (pointer-shared).

**Implication for multi-session:** subagents inherit the
parent SESSION's sub-gate, not the daemon template. Subagent
spawned by user A's session is gated by user A's session-gate;
its grants don't leak to user B. Correct behavior; no fix
needed once sub-gates land.

### Eventlog + session.Service

**Today:** already isolated by sessionID. No change needed.

### `pkg/attach` server + endpoints

**Today:** every `/sessions/{sid}/...` endpoint resolves a
session from the SessionRegistry and serves it. No per-caller
filtering.

**Fix:** wrap every session-scoped handler in an authorization
check:

```go
// pkg/attach (extended)

func (h *handlers) resolveAuthorizedSession(w, r, requiredAction) (*Entry, Caller, bool) {
    caller, err := h.authenticator.Authenticate(r)
    if err != nil {
        http.Error(w, err.Error(), http.StatusUnauthorized)
        return nil, Caller{}, false
    }
    entry, ok := h.Registry.Get(/* parse sid from req */)
    if !ok {
        http.Error(w, "session not found", http.StatusNotFound)
        return nil, caller, false
    }
    if !auth.Authorize(caller, requiredAction, entry.ACL) {
        // Return 404 not 403 to avoid leaking session existence
        http.Error(w, "session not found", http.StatusNotFound)
        return nil, caller, false
    }
    return entry, caller, true
}
```

`/sessions` list endpoint becomes:

```go
func (h *handlers) sessionsList(w, r) {
    caller, _ := h.authenticator.Authenticate(r)
    out := h.Registry.ListAuthorized(caller)
    writeJSON(w, http.StatusOK, out)
}
```

Caller identity is added to every prompt request, every audit-log
entry, every approval record on the session's sub-gate.

## Config surface

```json
{
  "version": 1,
  "attach": {
    "multi_session": {
      "enabled": true,
      "users_dir": "/var/lib/core-agent/users/",
      "auth": {
        "kind": "bearer_table",
        "table_file": "/etc/core-agent/users.json"
      },
      "admin_identities": ["ops@example.com"],
      "allow_anonymous": false
    }
  }
}
```

`users.json` shape (the static table for bearer auth):

```json
{
  "version": 1,
  "users": [
    { "identity": "alice@example.com", "token": "tok_abc...", "labels": { "team": "platform" } },
    { "identity": "bob@example.com",   "token": "tok_def...", "labels": { "team": "infra" } },
    { "identity": "sa:cron-runner",    "token": "tok_xyz...", "labels": { "kind": "service" } }
  ]
}
```

When `multi_session.enabled: false` (default), the daemon
behaves as today: single `Auth.BearerToken`, no caller
identity threading, no per-session ACL, no per-caller
instruction overlays. Drop-in compatible.

## Migration story

Three phases for an operator moving from single-user to
multi-user:

1. **Stay single-user** — no change. `multi_session.enabled: false`
   (the default).
2. **Enable multi-session with a static user table** — generate
   tokens, populate `users.json`, hand them to operators. Each
   operator's `core-agent-tui --attach-token=<their-token>`
   resolves to their identity. Sessions they create are owned by
   them; they can only see their own.
3. **Switch to OIDC / mTLS / K8s SA** (when shipped, v2.5+) —
   change `auth.kind` to `oidc` / `mtls` / `k8s_sa`; tokens come
   from the IDP. Users / sessions unchanged.

A `core-agent users migrate` CLI is **out of scope for v2.4** —
operators with existing single-user data either keep using
single-user mode or accept that legacy sessions become
"unowned" (admin-only-accessible) when they enable multi-session.

## Implementation phases

### Phase 1: foundations (PR α) — auth + caller plumbing

- New `pkg/auth` package: `Caller`, `Authenticator` interface,
  `BearerTokenAuth`, `AnonymousAuth`, `Authorize`.
- Wire `Authenticator` into `pkg/attach.ServerOptions` (default
  `AnonymousAuth` for backward compat).
- Pass `Caller` through every session-scoped handler via
  `resolveAuthorizedSession`.
- Config: `attach.multi_session.auth.kind: bearer_table` + a
  loader for `users.json`.
- Tests: single-user mode unchanged; multi-user mode rejects
  cross-session access.

~600 LoC + ~500 LoC tests.

### Phase 2: per-session sub-gates (PR β)

- `Gate.DeriveForSession(sid, prompter) *Gate` — copy-on-write of
  the mutable state (`sessionAllow`, `sessionAllowTools`,
  `sessionAllowVerbs`, `planRecorded`, `approvals`); share the
  read-only state (mode, policy, scope, requirePlanArtifact).
- Refactor `agent.New` to accept a derived gate per session.
- Wire `cmd/core-agent` to use the derivation when
  `multi_session.enabled: true`.
- Tests: user A's `allow-session` grant does not appear in user
  B's session; user A's `record_plan` doesn't unblock user B's
  mutating tools.

~400 LoC + ~400 LoC tests.

### Phase 3: per-caller instruction overlays + MCP caller context (PR γ)

- `LoadForSession(projectRoot, userRoot, caller)` extension to
  `pkg/instruction`.
- Caller context propagation through `pkg/mcp` tool calls
  (identity-only; credential resolution is a sibling design —
  see [`docs/mcp-credential-resolution-design.md`](./mcp-credential-resolution-design.md)).
- Per-session tool registry construction (call `tools.Build`
  per session).
- Tests: caller-specific overlay content lands only in that
  caller's session prompt; MCP servers can read caller identity
  from context.

~400 LoC + ~300 LoC tests.

### Phase 4: docs + examples + Hugo site update (PR δ)

- `docs/site/content/docs/reference/multi-session.md` —
  operator-facing guide.
- `examples/multi-session-bearer/` — recipe showing the
  static-table auth + per-user instruction overlays.
- CHANGELOG v2.4.0 entry.

~600 LoC of docs + recipe.

Total: ~3200 LoC across 4 PRs.

## Open questions

1. **Session creation API.** Today a session is created
   implicitly when an agent first runs. In multi-user mode, who
   creates a session for a caller, and how? Options:
   - Implicit on first `--attach-listen` POST that doesn't name
     an existing sessionID (server assigns a new ID, owner=caller).
   - Explicit `POST /sessions` endpoint that returns the new
     sessionID + URL.

   Lean: ship explicit `POST /sessions` for clarity; allow
   implicit creation as a fallback for backward compat.

2. **Identity for default single-user mode.** When
   `multi_session.enabled: false`, what's the Caller's identity
   field? Options:
   - `"anon"` (everyone's the same identity; gate audit
     log shows "anon")
   - The OS username of the daemon process
   - A configured default identity

   Lean: `"anon"` by default; configurable via
   `attach.default_identity`.

3. **Session deletion semantics.** When a session is deleted,
   what happens to its eventlog? Options:
   - Hard-delete (remove from session DB)
   - Soft-delete (mark deleted; retain for audit)
   - Per-policy (config flag)

   Lean: soft-delete by default with a sweep tool for hard-delete.
   Cross-session audit trail integrity matters more than disk
   space.

4. **Default ACL on session creation.** Owner-only is the safe
   default. Should there be a config option to broaden to
   "owner's team" (using `caller.Labels["team"]`) by default?

   Lean: owner-only; team-default is a v2.5+ enhancement when
   we have a fuller authz story.

5. **Audit log shape.** Today gate decisions log under
   `Gate.approvals[]`. For multi-session we want caller
   identity threaded through. New `GateDecision` struct with
   `Caller Identity` field?

   Lean: yes, extend the existing struct (additive change).

6. **MCP-server-as-caller-aware.** Do we ship a reference
   pattern for "MCP server that uses the caller's identity to
   make downstream calls" (e.g., a GKE MCP that uses workload
   identity per caller)? Or document the context-propagation
   path and leave the rest to the MCP server author?

   Lean: document the path; defer the reference pattern to a
   v2.5 example when a concrete consumer surfaces.

7. **Rate limiting / quotas.** Out of scope for v2.4, but worth
   noting: per-user quotas (max sessions, max tokens, max cost)
   would land on top of the Caller plumbing this design lays
   down. Caller identity is the natural key.

## Security considerations

- **Token storage.** The `users.json` file becomes a high-value
  credential file. Document file-mode requirements (0600,
  owner-only). Consider supporting tokens stored externally
  (Vault / Secret Manager) in a later phase.
- **Token rotation.** Bearer tokens have no built-in rotation;
  rotate by replacing entries in `users.json` and signaling the
  daemon to reload (`/reload` already exists).
- **Anonymous escape.** `allow_anonymous: true` is dangerous in
  shared environments — every unauthenticated request becomes the
  same Caller. Document the risk; default is `false`.
- **Information leakage via 404.** `Authorize` returns
  "session not found" rather than "forbidden" so an attacker
  can't enumerate session IDs by trying to access them. Same
  pattern other multi-tenant systems use.
- **Cross-session timing attacks.** Constant-time token
  comparison in `BearerTokenAuth` (use `subtle.ConstantTimeCompare`).
- **Audit-log retention.** Caller identity in audit logs may
  itself be sensitive (employee identifiers, customer
  identifiers); document retention/scrubbing policy
  considerations.

## Out of scope (deferred to v2.5 or beyond)

- OIDC / JWT / mTLS / K8s ServiceAccount authentication
  implementations (interfaces only in v2.4).
- Per-user quotas (tokens, cost, request rate).
- Session sharing (Viewers / Contributors ACL beyond Owner).
- Cross-daemon session migration.
- User-management CLI (`core-agent users add/remove`).
- IDP federation across daemons (SSO).
- Reference per-caller-aware MCP server example.
- Cross-session message routing.
- TUI affordances for multi-session UX (task #4 already filed —
  "+ New session" picker row).

## Dependencies and related work

- **Task #4** — "+ New session" picker row in core-agent-tui.
  TUI-side companion to this substrate design. Lands after
  Phase 1 (Caller plumbing) so the TUI knows the caller's
  identity for session listing.
- **Task #11** — Scion team-coordination investigation. Scion
  has its own concepts of agent identity / template / lifecycle;
  multi-session core-agent's auth model should not contradict
  Scion's. Investigation result may inform whether Scion-managed
  deployments use core-agent's per-user auth or defer to Scion's.
- **Task #8** — `/btw` token attribution. Once Caller is
  available in tool-call context, `AskSideQuestion`'s usage
  attribution can include the caller.
- **`docs/kube-agents-platform-fit.md`** — kube-agents Platform
  Agent is a natural multi-session consumer (platform team's
  engineers as users, fleet-management sessions as their work).
  Multi-session design here is what makes that recipe viable
  for a real shared deployment.
- **Task #13 / `docs/mcp-credential-resolution-design.md`** —
  sibling design for the per-MCP-server credential resolution
  layer (CredentialProvider interface, Auth Manager integration,
  per-caller credential caching). Composes with this design's
  Caller propagation; covers what to actually DO with the
  caller identity once it reaches the outbound MCP call.

## When this lands

Phase 1 + Phase 2 are the substantive substrate work. Phase 3 +
Phase 4 are extensions + docs. Realistic v2.4 timeline:

- Phase 1: 1-2 weeks (auth + Caller plumbing; lots of test surface)
- Phase 2: 1 week (sub-gate derivation; careful threading)
- Phase 3: 1 week (instruction overlays + MCP context)
- Phase 4: 1 week (docs + recipe + Hugo site)

~4-5 weeks of focused work for a clean v2.4 release. Could be
spread over a longer period if v2.4 also picks up other items
from the backlog (plan-progress tracking, etc.).
