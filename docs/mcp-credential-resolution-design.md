# Per-MCP-server credential resolution: pluggable providers + Auth Manager

Design doc for v2.4's auth-wrapper layer: a pluggable
`CredentialProvider` interface that resolves outbound credentials
per MCP server, with first-class support for caller-aware 3LO
providers (Google's Agent Identity Auth Manager, OAuth2 direct).

**Status:** proposed (2026-06-03). Awaiting approval before
implementation. Sibling design to
[`docs/multi-session-design.md`](./multi-session-design.md)
(task #12); independently valuable but composes naturally with
multi-session's Caller propagation.

## Motivation

Today `pkg/mcp/config.go:AuthSpec` ships exactly one auth
strategy: `GoogleOAuth`, which uses Application Default
Credentials to fetch an access token scoped to the configured
scopes. That covers daemon-identity 2LO well — the
`gke-parallel-triage` recipe uses it for the GKE MCP server.

It doesn't cover the cases multi-tenant deployments and broader
provider integration need:

1. **Per-caller 3LO tokens.** When Alice's session calls the
   GitHub MCP, we want Alice's GitHub OAuth token, not the
   daemon's service identity. Same for Linear, Notion,
   Salesforce, Jira, ServiceNow — any third-party API that
   identifies the user, not the calling service.
2. **Provider catalogs without per-server credential plumbing.**
   An operator deploying core-agent with three Google-API MCP
   servers (GKE, Cloud Run, Storage) shouldn't have to repeat
   the scopes / client config three times. One provider
   definition, referenced by name.
3. **Non-Google auth backends.** Auth Manager isn't the only
   credential vault; some operators will want OAuth2 direct
   (we hold client_id/secret, do the refresh-token dance),
   some will want static API keys (Cloudflare, Datadog, etc.),
   some will want mTLS client cert per MCP server.
4. **Agent Identity Auth Manager specifically.** Google's
   cloud-hosted credential vault: operators configure OAuth
   providers in the GCP Console (or via `gcloud`), then agents
   request user-bound tokens via the `iamconnectorcredentials`
   library. The Auth Manager handles storage, refresh, and
   provider-specific quirks. Direct fit for our multi-tenant
   use cases.

The blocker for all four is the same: `AuthSpec` is a closed
union of one strategy. Opening it up to a pluggable interface
with caller-aware resolution is the unlock.

## Goals

- **Pluggable provider interface.** Adding a new auth backend
  (a custom IDP, a future cloud credential service) is a single
  Go file, not a `pkg/mcp/config.go` refactor.
- **Per-caller resolution for 3LO providers.** When multi-session
  Phase 1 plumbs `Caller` through to the outbound MCP call, 3LO
  providers use it to fetch the right token. 2LO providers
  ignore caller and use the daemon's identity.
- **Shared provider definitions.** One named provider config
  reused across multiple MCP servers. Cache hits across servers
  if they request overlapping scopes.
- **Cache with refresh-before-expiry.** Per-(provider, caller,
  scopes) keyed cache; refresh ~30s before expiry to avoid
  user-visible auth latency on every call.
- **Independently valuable.** Operators in single-user
  deployments should benefit from Auth Manager + 3LO providers
  even without enabling multi-session — caller defaults to
  the daemon's configured identity.
- **Backward compatible.** Existing `auth: { google_oauth: ... }`
  configs continue to work; we honor them as a legacy shape that
  resolves to a synthetic provider entry.

## Non-goals

- **Replacing the existing `google_oauth` strategy.** It
  becomes a `CredentialProvider` implementation; the JSON
  shape stays addressable.
- **OAuth flow UX / login-to-IDP.** Auth Manager owns the login
  flow on the GCP Console side; OAuth2 direct depends on the
  operator pre-seeding refresh tokens out-of-band. We don't
  build a "log into GitHub from the TUI" experience in v2.4.
- **Managing Auth Manager provider configs.** Operators
  configure OAuth providers in the GCP Console / via `gcloud`.
  We consume the resulting provider IDs; we don't wrap the
  Auth Manager admin API.
- **Credential rotation alerting / monitoring.** Token expiry
  surfaces as MCP-call errors; alerting on systemic auth
  failures is the operator's observability concern.
- **Per-MCP-server connection pools.** Same connection, different
  injected credential per call. Deferred to v2.5.
- **Per-provider rate limiting.** Auth Manager / OAuth2 have
  their own rate limits; we don't add a second layer.

## Conceptual model

### `CredentialProvider` interface

```go
// pkg/mcp/auth (new package)

// CredentialProvider resolves credentials for one outbound MCP
// call. Implementations carry their own config; the registry
// hands them the caller identity at resolution time so 3LO
// providers can return user-delegated tokens.
//
// Resolve may be called concurrently from many goroutines;
// implementations must be safe for that. Caching is the
// registry's responsibility — providers just resolve fresh.
type CredentialProvider interface {
    // Kind returns a stable provider-type label for logs/audit.
    Kind() string

    // Resolve returns the credential to inject on an outbound
    // request. ctx carries the Caller (via pkg/auth.CallerFromContext)
    // for 3LO providers; 2LO providers ignore it.
    //
    // scopes is the scope set the MCP server requires for this
    // call. Providers may merge with their own config scopes
    // (e.g., a provider configured for ["repo", "read:org"]
    // called with scopes=["repo"] returns a token sufficient for
    // the call).
    Resolve(ctx context.Context, scopes []string) (Credential, error)
}

// Credential is what gets injected on the outbound request.
type Credential struct {
    // Inject describes how to put the credential on the request.
    Inject InjectStrategy

    // Header is the header name when Inject == InjectHeader.
    Header string

    // Value is the credential value (e.g., "Bearer ya29...").
    Value string

    // Expires is the credential's expiry timestamp. The registry
    // uses this for cache TTL. Zero means "never expires" —
    // appropriate only for long-lived static credentials.
    Expires time.Time
}

type InjectStrategy int

const (
    InjectHeader InjectStrategy = iota // Set req.Header[Header] = Value
    InjectQueryParam                   // req.URL.Query().Set(Header, Value) — discouraged
    InjectBasicAuth                    // req.SetBasicAuth(...) — rare
)
```

### Provider registry

```go
// pkg/mcp/auth

// Registry holds named providers + per-(name, caller, scopes)
// credential cache. Constructed once at daemon startup from
// config; passed to pkg/mcp.Build alongside the existing args.
type Registry struct {
    providers map[string]CredentialProvider
    cache     *credentialCache
}

func (r *Registry) Resolve(ctx context.Context, providerName string, scopes []string) (Credential, error) {
    p, ok := r.providers[providerName]
    if !ok {
        return Credential{}, fmt.Errorf("auth provider %q not configured", providerName)
    }
    caller := pkgauth.CallerFromContext(ctx)
    key := cacheKey(providerName, caller.Identity, scopes)
    if cached, ok := r.cache.get(key); ok {
        return cached, nil
    }
    cred, err := p.Resolve(ctx, scopes)
    if err != nil {
        return Credential{}, err
    }
    r.cache.put(key, cred)
    return cred, nil
}
```

### Per-(provider, caller, scopes) cache

The cache key has three parts deliberately:

- **provider**: different MCP servers using the same provider
  share the cache (Google scope expansion is incremental — a
  token with `[a, b, c]` satisfies a request for `[a]`).
- **caller**: 3LO tokens are user-bound; Alice's and Bob's
  tokens for the same provider are different entries.
- **scopes**: a token issued for `[repo]` doesn't satisfy a
  request for `[repo, admin:org]`. Cache miss → re-resolve.

Eviction:
- TTL = `min(cred.Expires - 30s, 1h)` — refresh before expiry,
  cap at 1h even for long-lived tokens.
- Active-refresh-on-near-expiry pattern: when serving a cached
  entry whose expiry is within 30s, fire a background refresh
  while returning the still-valid entry. Avoids user-visible
  auth latency.
- Per-entry size bound: cache holds at most N entries (default
  1024); LRU eviction beyond that.

### Per-caller eviction on session end

Multi-session adds session-lifecycle hooks; when a session
ends (operator closes it, daemon restart, lease expiry), we
evict cache entries keyed on that caller. Prevents stale
tokens persisting longer than the session.

## Provider implementations

Five providers ship in v2.4 (rough order of priority):

### `google_oauth` (legacy compat + 2LO)

Refactor of today's `pkg/mcp/config.go:GoogleOAuthAuth`. Uses
ADC; ignores caller; returns scoped access token. Existing
configs continue to work; the loader synthesizes a provider
entry named after the server (e.g., `_legacy_gke`) when an MCP
server has the inline `google_oauth` shape.

```go
type GoogleOAuthProvider struct {
    Scopes []string
    src    oauth2.TokenSource // cached after first call
}

func (p *GoogleOAuthProvider) Kind() string { return "google_oauth" }

func (p *GoogleOAuthProvider) Resolve(ctx context.Context, scopes []string) (Credential, error) {
    if p.src == nil {
        var err error
        p.src, err = google.DefaultTokenSource(ctx, p.Scopes...)
        if err != nil {
            return Credential{}, err
        }
    }
    tok, err := p.src.Token()
    if err != nil {
        return Credential{}, err
    }
    return Credential{
        Inject:  InjectHeader,
        Header:  "Authorization",
        Value:   "Bearer " + tok.AccessToken,
        Expires: tok.Expiry,
    }, nil
}
```

### `google_id_token` (2LO, audience-scoped)

For Cloud Run / IAP-protected services that require ID tokens
rather than access tokens. Uses ADC; takes an audience config.

```go
type GoogleIDTokenProvider struct {
    Audience string // e.g., "https://my-cloud-run-service-xxx.run.app"
}
```

### `static_api_key` (2LO, no token refresh)

For services that take a long-lived API key as a header
(Cloudflare, Datadog, OpenAI, etc.). Reads from env var or
literal.

```go
type StaticAPIKeyProvider struct {
    Header   string // default "Authorization"
    Scheme   string // default "Bearer"; empty = bare key
    EnvVar   string // read from os.Getenv at resolution time
    Literal  string // alternative to EnvVar; for non-secret keys
}
```

### `auth_manager` (3LO, Google Agent Identity Auth Manager)

Per-caller user-delegated token resolution via the
`iamconnectorcredentials` library. **The headline 3LO
provider.**

```go
type AuthManagerProvider struct {
    // ProviderID is the fully-qualified Auth Manager provider
    // resource (e.g.,
    // "projects/MY_PROJECT/locations/global/oauthProviders/github").
    // Operators configure the provider in the GCP Console
    // (OAuth client_id, secret, supported scopes) and reference
    // the resulting ID here.
    ProviderID string

    // SubjectMapping selects how to map our Caller.Identity to
    // the subject Auth Manager uses to look up stored credentials.
    // Default: "identity" (Caller.Identity verbatim).
    // Future: "label:<key>" to use Caller.Labels[<key>].
    SubjectMapping string

    // Scopes default set; merged with the per-call scope set.
    Scopes []string

    client *iamconnectorcredentials.Client // lazy-init
}

func (p *AuthManagerProvider) Kind() string { return "auth_manager" }

func (p *AuthManagerProvider) Resolve(ctx context.Context, scopes []string) (Credential, error) {
    caller := pkgauth.CallerFromContext(ctx)
    if caller.Identity == "" {
        return Credential{}, fmt.Errorf("auth_manager: caller identity required for 3LO resolution")
    }
    subject := p.resolveSubject(caller)
    finalScopes := mergeScopes(p.Scopes, scopes)

    if p.client == nil {
        var err error
        p.client, err = iamconnectorcredentials.NewClient(ctx)
        if err != nil {
            return Credential{}, fmt.Errorf("auth_manager: client init: %w", err)
        }
    }

    resp, err := p.client.GenerateAccessToken(ctx, &credentialspb.GenerateAccessTokenRequest{
        Name:    p.ProviderID,
        Subject: subject,
        Scopes:  finalScopes,
    })
    if err != nil {
        return Credential{}, fmt.Errorf("auth_manager: generate token for %s on %s: %w", subject, p.ProviderID, err)
    }

    return Credential{
        Inject:  InjectHeader,
        Header:  "Authorization",
        Value:   "Bearer " + resp.AccessToken,
        Expires: resp.ExpiresAt.AsTime(),
    }, nil
}
```

### `oauth2_direct` (3LO, self-managed)

For operators who don't use Auth Manager but want 3LO. We hold
`client_id` / `client_secret` / `auth_url` / `token_url`; the
operator pre-seeds a refresh-token store (typically a SQLite
or Postgres table indexed by caller identity); we do the
refresh-token dance per call.

```go
type OAuth2DirectProvider struct {
    ClientID     string
    ClientSecret string // env-var or secret-store reference
    TokenURL     string
    Scopes       []string

    // RefreshTokenStore looks up the refresh token for a caller.
    // Pluggable: SQLite (default), Postgres, in-memory (tests).
    RefreshTokenStore RefreshTokenStore
}

type RefreshTokenStore interface {
    Get(ctx context.Context, callerIdentity string) (refreshToken string, err error)
    Put(ctx context.Context, callerIdentity, refreshToken string) error
    Delete(ctx context.Context, callerIdentity string) error
}
```

The login-flow side (how a refresh token GETS into the store)
is out of scope — operator's responsibility (via a separate
"OAuth login helper" tool, an external onboarding flow, etc.).

## Config shape

### Shared provider definitions

A new top-level `auth_providers` block in either `.agents/mcp.json`
or a sibling `.agents/auth.json` (operator picks):

```json
{
  "auth_providers": {
    "gcp_admin": {
      "kind": "google_oauth",
      "scopes": ["https://www.googleapis.com/auth/cloud-platform"]
    },
    "gke_readonly": {
      "kind": "google_oauth",
      "scopes": ["https://www.googleapis.com/auth/container.read-only"]
    },
    "cloud_run_iap": {
      "kind": "google_id_token",
      "audience": "https://my-service-xxx.run.app"
    },
    "datadog": {
      "kind": "static_api_key",
      "header": "DD-API-KEY",
      "scheme": "",
      "env_var": "DATADOG_API_KEY"
    },
    "github_3lo": {
      "kind": "auth_manager",
      "provider_id": "projects/MY_PROJECT/locations/global/oauthProviders/github",
      "scopes": ["repo", "read:org"]
    },
    "linear_3lo": {
      "kind": "auth_manager",
      "provider_id": "projects/MY_PROJECT/locations/global/oauthProviders/linear",
      "scopes": ["read"]
    },
    "internal_oauth": {
      "kind": "oauth2_direct",
      "client_id": "agent-prod",
      "client_secret_env": "AGENT_OAUTH_SECRET",
      "token_url": "https://idp.example.com/token",
      "scopes": ["read", "write"],
      "refresh_token_store": {
        "kind": "sqlite",
        "path": "/var/lib/core-agent/refresh-tokens.db"
      }
    }
  }
}
```

### Per-server provider reference

`mcp.json` references a provider by name:

```json
{
  "version": 1,
  "servers": {
    "gke": {
      "transport": "http",
      "url": "https://container.googleapis.com/mcp/read-only",
      "auth": { "provider": "gke_readonly" }
    },
    "github": {
      "transport": "http",
      "url": "https://api.github.com/mcp",
      "auth": { "provider": "github_3lo" }
    },
    "datadog": {
      "transport": "http",
      "url": "https://api.datadoghq.com/mcp",
      "auth": { "provider": "datadog" }
    }
  }
}
```

### Backward-compat: legacy inline shape

The existing inline shape continues to work:

```json
{
  "servers": {
    "gke": {
      "transport": "http",
      "url": "https://container.googleapis.com/mcp/read-only",
      "auth": {
        "google_oauth": { "scopes": ["https://www.googleapis.com/auth/container.read-only"] }
      }
    }
  }
}
```

The loader synthesizes an anonymous provider entry for the
server and points its `auth.provider` at it. Cache key uses
the synthesized name; functional behavior unchanged.

## Composition with multi-session

The two designs (multi-session, this one) compose through one
contact point: **`pkg/auth.CallerFromContext(ctx)`**.

- Multi-session Phase 1 puts `Caller` into the request context
  at the HTTP attach layer.
- Multi-session Phase 3 propagates it through the agent loop
  into `pkg/mcp` outbound calls.
- This design's `Registry.Resolve` reads the caller from
  context and uses it (for 3LO) or ignores it (for 2LO).

**Crucially: "Caller" here means the TURN ORIGINATOR, not the
session owner.** In a shared session (Slack channel, GChat room,
team pair-programming TUI) where multiple users contribute prompts
to the same context, each turn carries the Caller of whoever
sent the message that triggered it. This works identically for
owner-only and shared sessions because the credential resolution
just reads whatever Caller is in the per-turn context — it doesn't
know or care about session ownership semantics. See the "Shared
sessions" subsection in
[`docs/multi-session-design.md`](./multi-session-design.md) for
the per-turn attribution model and the Proxy role that makes
chat-bot integrations work.

So in a shared `#incident-response` channel session:

```
Alice sends "investigate the 5xx spike"
  → turn originator = alice@
  → agent calls GitHub MCP
  → Registry.Resolve(ctx, "github_3lo", scopes)
  → reads caller from ctx → alice@
  → Auth Manager: GenerateAccessToken(subject=alice@, provider=github)
  → returns Alice's token

Bob replies "what about the deploy logs?"
  → turn originator = bob@
  → agent calls same GitHub MCP
  → Registry.Resolve(ctx, "github_3lo", scopes)
  → reads caller from ctx → bob@
  → Auth Manager: GenerateAccessToken(subject=bob@, provider=github)
  → returns Bob's token (separate cache entry, Bob's
    credentials)
```

Same session, same conversation history visible to the agent,
but the outbound MCP call resolves per-turn-originator. The
`(provider, caller, scopes)` cache key handles the per-user
isolation automatically.

Either side can land first. If credential resolution ships
before multi-session Phase 1, `Caller` defaults to "anon" /
the daemon's configured default identity; 2LO providers
(`google_oauth`, `static_api_key`) work normally; 3LO
providers (`auth_manager`, `oauth2_direct`) error with "caller
identity required" until multi-session lands. That's a
reasonable failure mode — operators turning on 3LO providers
without multi-session is almost certainly a config mistake.

## Implementation phases

### Phase A: substrate refactor + legacy compat (PR α)

- New `pkg/mcp/auth` package with `CredentialProvider`,
  `Credential`, `Registry`, cache.
- Extract today's `GoogleOAuthAuth` logic into a
  `GoogleOAuthProvider` implementing the interface.
- Loader synthesizes anonymous provider entries for existing
  inline `auth: { google_oauth: ... }` shapes. Zero-config
  migration for current consumers.
- `pkg/mcp.Build` accepts a `Registry`; outbound auth path
  resolves through it instead of the hardcoded GoogleOAuth call.
- Tests: existing GKE recipe + tests still work; new tests
  for cache hit/miss/refresh.

~600 LoC substrate + ~400 LoC tests.

### Phase B: shipped providers beyond google_oauth (PR β)

- `google_id_token` (for Cloud Run / IAP)
- `static_api_key` (for Datadog / Cloudflare / OpenAI-shape)
- Config loader extension for shared `auth_providers` block.
- Tests for each provider with mock token endpoints.

~400 LoC substrate + ~300 LoC tests.

### Phase C: Auth Manager integration (PR γ)

- `AuthManagerProvider` with `iamconnectorcredentials` client.
- Caller-aware resolution path (reads `pkgauth.CallerFromContext`).
- Subject mapping config (default `identity`; label-based
  mapping for future use cases).
- Integration test that exercises the resolve path against a
  mock / sandbox Auth Manager. (Real Auth Manager testing is
  manual UAT against a real GCP project.)
- Documentation: how to create a provider config in the GCP
  Console; how to verify a token resolves correctly.

~500 LoC substrate + ~300 LoC tests + recipe.

### Phase D: oauth2_direct + recipe + docs (PR δ)

- `OAuth2DirectProvider` + `RefreshTokenStore` interface +
  SQLite implementation.
- `docs/site/content/docs/reference/mcp-auth.md` —
  operator-facing guide covering all provider types.
- `examples/mcp-auth-manager/` recipe showing the
  Auth Manager + multi-session combination end-to-end.
- CHANGELOG v2.4 entry covering both Phase A-D of this design
  plus the multi-session pieces.

~500 LoC providers + ~300 LoC tests + ~500 LoC recipe + docs.

Total: ~3800 LoC across 4 PRs.

## Open questions

1. **Provider config location.** Top-level `auth_providers`
   block in `.agents/mcp.json`, or sibling `.agents/auth.json`
   file? Lean: support both — `mcp.json` for the simple case,
   `auth.json` for operators with many providers + many MCP
   servers wanting a cleaner separation.

2. **Subject mapping for Auth Manager.** Default: `Caller.Identity`
   verbatim. But Auth Manager's subject expectations may differ
   from our identity format (email vs OIDC sub claim vs custom).
   Options:
   - Hard-default to identity verbatim; operator must align
     their identity scheme.
   - Support label-based mapping (`subject_mapping: "label:oidc_sub"`).
   - Support template substitution (`subject_mapping: "{{.Labels.email}}"`).
   Lean: identity verbatim + label-based; defer templates.

3. **Cache invalidation on token revocation.** If user revokes
   their token at the IDP, we keep returning the cached value
   until expiry (up to 1h). Options:
   - Accept up-to-1h staleness as the operator's compromise.
   - Add a revocation-check tool (operator triggers cache
     wipe on incident response).
   - Subscribe to IDP revocation events where supported.
   Lean: accept staleness; add a `core-agent cache wipe
   --provider=<name> [--caller=<id>]` CLI for explicit eviction.

4. **Cache-on-disk vs in-memory.** In-memory keeps secrets out
   of disk; disk survives daemon restart. Lean: in-memory only.
   Restart hits a one-time fetch per (provider, caller, scopes)
   the first call after restart — acceptable. Disk caching of
   bearer tokens is genuinely risky.

5. **OAuth2 direct refresh-token-store onboarding.** How does
   a fresh refresh token GET into the store? Out of scope per
   non-goals, but worth picking a default story so operators
   aren't stuck. Options:
   - "Manual: drop into the SQLite/Postgres table with a CLI."
   - "Reference an external OAuth login helper binary the
     operator ships separately."
   - "Ship a tiny CLI tool `core-agent oauth login --provider=<name>`
     that does the device-flow dance once and writes to the store."
   Lean: ship the CLI in v2.5 (or as an extras binary); for
   v2.4 document the manual path.

6. **MCP servers that want raw caller identity, not a token.**
   Some MCP servers may want to authenticate the caller
   themselves (custom auth scheme inside the protocol). For
   them the `CredentialProvider` shape is wrong — they want
   the Caller object passed through, not a fetched token.
   Lean: orthogonal to this design — those servers consume
   the Caller from the request context directly (multi-session
   Phase 3 propagates it). They just don't configure an
   `auth.provider` in `mcp.json`.

7. **Multi-binding sub-scopes.** A provider configured for
   `[repo, read:org]` called with scopes `[repo]` returns the
   broader-scope token. But what if the per-call scope is
   broader than the provider config? E.g., provider configured
   `[read]`, MCP server requests `[read, write]`. Options:
   - Error: "provider not configured for these scopes"
   - Pass-through to the provider; let it fail at the IDP
   - Auto-merge: request `[read, write]` from the IDP.
   Lean: error — explicit config is the contract; mismatch
   indicates a config bug.

## Security considerations

- **Token-in-memory only.** Bearer tokens are high-value
  credentials. Keep them out of disk caches, structured logs,
  and stack traces. Specifically: log provider names + scopes
  + caller identity, never the token value.

- **Cache wipe on session end.** When a session ends, evict
  cache entries keyed on that caller. Prevents stale
  user-bound tokens persisting longer than the session that
  needed them.

- **Constant-time token comparison.** For `static_api_key`
  providers that operate on shared-secret schemes, use
  `subtle.ConstantTimeCompare` if we ever do server-side
  validation. (Not relevant for the outbound-injection case
  in v2.4 but worth noting for adjacent work.)

- **Provider config file mode.** Like `users.json` in the
  multi-session design, the `auth_providers` config — and any
  inline `client_secret` / `literal` values — are high-value
  credential material. Document file-mode requirements (0600,
  owner-only); recommend env-var indirection where possible
  (`env_var: SECRET_NAME` rather than inline `literal`).

- **Refresh-token-store protection (for `oauth2_direct`).**
  The store holds long-lived refresh tokens for every onboarded
  user. SQLite file mode, Postgres connection-level
  authentication, encryption-at-rest considerations all become
  the operator's responsibility — document loudly.

- **Auth Manager error leak.** Errors from the Auth Manager
  API may include the provider ID, subject, or other identity
  metadata. Make sure those don't leak across session
  boundaries (different caller's error mentions another
  caller's identity). Log at debug; user-facing errors strip
  to "auth provider unavailable" or similar.

- **3LO providers without multi-session.** Configuring an
  `auth_manager` or `oauth2_direct` provider without
  multi-session enabled is almost certainly a misconfiguration
  (every call resolves to the daemon's default identity, which
  defeats per-user attribution). Warn loudly at daemon startup.

## Out of scope (deferred to v2.5 or beyond)

- OAuth flow UX (`core-agent oauth login --provider=<name>`)
  for `oauth2_direct` onboarding.
- Per-MCP-server connection pools (currently shared connection
  with per-call credential injection).
- Per-provider rate-limiting / quotas.
- IDP revocation event subscription (push-based cache
  invalidation).
- Token-store encryption-at-rest (operator's responsibility
  today; first-class support deferred).
- Auth Manager admin-API wrapper (creating providers from
  core-agent rather than via `gcloud`).
- Reference per-caller-aware MCP server example.
- mTLS client cert per MCP server (sibling provider kind;
  defer until a concrete consumer surfaces).

## Dependencies and related work

- **Task #12 / `docs/multi-session-design.md`** — sibling design.
  Phase 1's `Caller` plumbing is the prerequisite for 3LO
  providers; Phase 3's MCP context propagation is the contact
  point. Either side can land first; 3LO providers without
  multi-session warn but still function with the daemon's
  default identity.
- **Task #4** — TUI "+ New session" picker. Not directly
  related, but a multi-session-shaped UX wants per-user
  credential-state visibility too (eventually a "which
  providers does my session have credentials for?" view).
- **`docs/kube-agents-platform-fit.md`** — platform-agent
  recipe wants Auth Manager integration so each engineer's
  GitHub / Linear / Salesforce actions attribute to them, not
  the daemon's service account. This design's `auth_manager`
  provider is what enables that.

## When this lands

- Phase A: 1 week (substrate refactor + legacy compat)
- Phase B: 1 week (extra 2LO providers + shared config block)
- Phase C: 1-2 weeks (Auth Manager integration + recipe)
- Phase D: 1 week (oauth2_direct + docs + reference recipe)

~4-5 weeks of focused work. Realistic v2.4 release if
sequenced after multi-session Phase 1 lands. Could be split
across v2.4 (Phases A + B + C) and v2.5 (Phase D) if v2.4
scope pressure builds.
