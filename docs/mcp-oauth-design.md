# MCP Streamable HTTP transport + OAuth 2.0 client authentication

Design doc for the v2.7 addition to `pkg/mcp`: full support for MCP-spec-compliant Streamable HTTP transport and OAuth 2.0 client authentication (RFC 8414 Authorization Server Metadata + RFC 9728 Protected Resource Metadata + RFC 7636 PKCE), enabling core-agent to consume first-party MCP servers that follow the standard auth shape (Slack, likely Notion / GitHub / Linear as they ship first-party MCPs).

**Status:** approved (design merged 2026-07-10). Implementation begins after PR #191 (this doc) lands, following the guardrails in §"Forward compatibility with credential-resolution (v2.8+)" so v2.7's daemon-scope OAuth doesn't box out the per-caller extension planned for v2.8+. Tracking issue: [#190](https://github.com/go-steer/core-agent/issues/190).

## Motivation

`pkg/mcp` today supports:
- Transports: `stdio` (subprocess with JSON-RPC over stdin/stdout) and `http` (plain HTTP with per-request POSTs).
- HTTP auth: `google_oauth` only (ADC / Application Default Credentials, the pattern Google-hosted MCP servers like `container.googleapis.com/mcp` expect).

The MCP spec [2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#streamable-http) standardized two things that our client doesn't do:

- **Streamable HTTP transport.** Client posts JSON-RPC over HTTP; server can push responses AND server-initiated notifications (via a lightweight SSE-adjacent framing) on the same connection. Supersedes the older stdio-only + full-SSE-transport combo. Every modern MCP server ships this.
- **OAuth 2.0 authorization** with RFC 8414 (Authorization Server Metadata) + RFC 9728 (Protected Resource Metadata) for discovery, and PKCE (RFC 7636) for the auth-code flow. Standardizes "how does an MCP client authenticate to an OAuth-protected MCP server."

Without both, we can't consume Slack's official MCP (`mcp.slack.com/mcp` — Streamable HTTP, RFC 8414-discoverable, user-token OAuth 2.0) or any similar server. And Slack won't be the last — Notion, GitHub, Linear, and others are actively building first-party MCPs on the same shape.

The blocker is small: the underlying MCP SDK we already depend on (`github.com/modelcontextprotocol/go-sdk`) implements every one of these primitives. `pkg/mcp` just doesn't wire them up.

## Goals

- **Consume MCP-spec-compliant Streamable HTTP MCP servers** with zero non-standard hacks.
- **Full OAuth 2.0 support with autodiscovery** — operators point at a server URL, we discover the authorization server via RFC 9728, discover its endpoints via RFC 8414, complete PKCE-protected auth-code flow.
- **Headless-daemon-friendly runtime.** Refresh tokens obtained once (interactively) via a bootstrap CLI; the daemon then auto-refreshes access tokens on demand without human input. Fits the K8s deployment model where the daemon is a pod, not a person.
- **Backward compatible.** Existing `stdio` and `http` (with `google_oauth` or unauth) configs keep working unchanged. This is additive.
- **Config-surface-first.** Operators express intent via `mcp.json`; no code changes required to add a new OAuth-authenticated MCP server.
- **Reuse SDK primitives, don't reinvent.** Every piece we need is in the SDK — `pkg/mcp`'s job is to translate config → SDK construction.

## Non-goals (v2.7)

- **Custom OAuth 2.0 authorization server implementation.** We're a client, not a server. `pkg/attach`'s multi-session bearer-table auth is separate substrate; nothing here touches it.
- **Interactive OAuth flow AT DAEMON RUNTIME.** The daemon is headless. Any flow requiring a browser happens ONCE at bootstrap; the daemon uses refresh tokens.
- **Legacy OAuth 1.0a.** Only OAuth 2.0.
- **JWT bearer tokens (RFC 7523) or SAML assertion flows.** Not a common shape in the MCP ecosystem today. Add later if a specific MCP server needs one.
- **Federated identity (SAML/SSO) inside the OAuth flow.** The authorization server handles that; we don't care.
- **Auto-registering MCP servers with Slack-marketplace-style app approval.** Operators pre-register their MCP app the normal way; we consume the resulting client_id / client_secret.
- **Client-credentials grant for headless machine-to-machine flows.** The MCP servers we're targeting (Slack, etc.) use user-token OAuth. Client-credentials is a smaller-scope future addition.

## Conceptual model

### Transport: Streamable HTTP

`mcp.StreamableClientTransport` from the SDK (`mcp/streamable_client.go`). Client-side:

1. `Connect(ctx)` → HTTP POST to the endpoint with an `initialize` request. Server responds with `MCP-Session-ID` header — the session identifier used on subsequent requests.
2. Regular MCP messages: HTTP POST with JSON-RPC body. Response can be a synchronous JSON-RPC response OR an SSE-style stream if the server needs to push notifications during the request.
3. Standalone SSE stream: separately, the client opens a persistent GET request to receive server-initiated notifications outside any specific request. This is the "streamable" part.
4. `Close()` → DELETE request to terminate the session cleanly.

The SDK gives us this transport with a single field of configuration: `HTTPClient *http.Client`. Point that HTTP client at an OAuth-wrapped `oauth2.NewClient(ctx, tokenSource)` and every request carries the current access token automatically. Refresh happens under the hood via `golang.org/x/oauth2`.

### Discovery: RFC 9728 → RFC 8414

MCP spec 2025-11-25 defines two-step discovery:

1. **Protected Resource Metadata** (RFC 9728, `oauthex.ResourceMeta`). The MCP server publishes `/.well-known/oauth-protected-resource` naming its authorization server(s), the scopes it accepts, and the resource identifier the client should include in token requests.
2. **Authorization Server Metadata** (RFC 8414, `oauthex.AuthServerMeta`). The authorization server publishes `/.well-known/oauth-authorization-server` naming its `authorization_endpoint`, `token_endpoint`, supported `code_challenge_methods_supported`, `token_endpoint_auth_methods_supported`, `scopes_supported`, and more.

For Slack:
- `https://mcp.slack.com/.well-known/oauth-protected-resource` → names `https://slack.com` as the auth server (roughly).
- `https://slack.com/.well-known/oauth-authorization-server` → names `https://slack.com/oauth/v2_user/authorize` + `https://slack.com/api/oauth.v2.user.access` as the endpoints.

Full autodiscovery reduces the config to: server URL + client_id + client_secret. That's the target.

For servers that don't publish discovery documents (some early / simple servers), the operator supplies `authorization_endpoint` + `token_endpoint` explicitly in config.

### Auth handler: `AuthorizationCodeHandler` + `oauth2.TokenSource`

The SDK's `auth.AuthorizationCodeHandler` (`auth/authorization_code.go`) implements the RFC 6749 authorization-code grant with PKCE. Two entry points relevant to us:

- **`StartAuthorizationCodeFlow(ctx, opts)`** — initiates the interactive flow. Generates PKCE verifier/challenge, opens the browser to `authorization_endpoint?code_challenge=...`, waits for callback with `code`, exchanges `code` at `token_endpoint`, returns access + refresh tokens. Used by our bootstrap CLI.
- **`LoadRefreshToken(ctx, refreshToken)`** — hydrates the handler with a pre-existing refresh token; subsequent `TokenSource(ctx)` calls return a source that auto-refreshes access tokens as needed. Used by the daemon at runtime.

Both funnel to the same `TokenSource(ctx) (oauth2.TokenSource, error)` method. We wrap that in `oauth2.NewClient(ctx, tokenSource)` and pass the resulting `*http.Client` to `StreamableClientTransport{HTTPClient: ...}`.

### Refresh-token storage: env vars pointing to Secrets

Refresh tokens are long-lived credentials — treat them like any other secret. The config carries a `refresh_token_env` field naming an environment variable; the daemon reads the token from that env var at startup. In K8s, the env var is populated from a `Secret` (`valueFrom.secretKeyRef.name`, standard pattern).

Rotation: operator regenerates the refresh token via the bootstrap CLI, updates the Secret, `kubectl rollout restart deployment core-agent`. No hot-swap; a rolling restart is acceptable for a config-change operation.

## Detailed design

### Config surface

New transport option `"streamable_http"` and new auth block `"oauth2_direct"`:

```jsonc
{
  "version": 1,
  "servers": {
    "slack": {
      "transport": "streamable_http",
      "url": "https://mcp.slack.com/mcp",
      "auth": {
        "oauth2_direct": {
          // Required — the OAuth 2.0 client identity registered with
          // the authorization server (e.g., created via Slack app
          // registration).
          "client_id": "1234567890.abcdefgh",
          "client_secret_env": "SLACK_APP_CLIENT_SECRET",

          // Required for runtime — env var carrying the refresh token
          // produced by `core-agent mcp oauth-bootstrap`. Absent env
          // var → startup error (so misconfig fails fast, not at first
          // MCP call).
          "refresh_token_env": "SLACK_MCP_REFRESH_TOKEN",

          // Scopes the client requests. Autodiscovery via RFC 8414
          // can validate these against `scopes_supported`; if
          // discovery is disabled the operator MUST provide them.
          "scopes": ["chat:write", "channels:history", "users:read"],

          // Optional — override RFC 9728 / RFC 8414 discovery.
          // When empty, `pkg/mcp` fetches:
          //   {url}/.well-known/oauth-protected-resource
          //     → names the authorization server
          //   {auth-server}/.well-known/oauth-authorization-server
          //     → names the endpoints
          //
          // Some servers don't publish discovery documents; provide
          // these explicitly then.
          "authorization_endpoint": "",
          "token_endpoint": "",
          "discovery_url": ""
        }
      }
    }
  }
}
```

Existing configs (`stdio` + `http` with `google_oauth` or unauth) keep working unchanged. Parser validation:

- `oauth2_direct` is valid only with `streamable_http` transport.
- Cannot combine `oauth2_direct` with `google_oauth` on the same server.
- `client_id`, `client_secret_env`, `refresh_token_env`, `scopes` required.
- If any of `authorization_endpoint` / `token_endpoint` are set, both must be set (partial-override rejected).

### Runtime wiring (in `pkg/mcp/lifecycle.go`)

Per-server bring-up when config selects Streamable HTTP + OAuth:

```
1. Load refresh_token from env; error if missing.
2. Load client_secret from env; error if missing.
3. Resolve endpoints:
   a. If authorization_endpoint + token_endpoint set → use as-is.
   b. Else discovery:
      - GET {url}/.well-known/oauth-protected-resource (RFC 9728)
      - GET {auth_server}/.well-known/oauth-authorization-server (RFC 8414)
      - Extract authorization_endpoint + token_endpoint.
      - Validate scopes against scopes_supported (WARN log if any
        requested scope isn't advertised — Slack in particular
        may accept unlisted scopes).
4. Construct auth.AuthorizationCodeHandler{
     ClientID:      cfg.ClientID,
     ClientSecret:  clientSecret,
     Scopes:        cfg.Scopes,
     PKCEMethod:    "S256",  // default; disable via config knob only
                             // if a discovered server explicitly
                             // requires "plain"
     AuthEndpoint:  authEndpoint,
     TokenEndpoint: tokenEndpoint,
   }
5. handler.LoadRefreshToken(ctx, refreshToken) → hydrates the source.
6. tokenSource, _ := handler.TokenSource(ctx)
7. httpClient := oauth2.NewClient(ctx, tokenSource)
8. transport := &mcp.StreamableClientTransport{
     Endpoint:   cfg.URL,
     HTTPClient: httpClient,
   }
9. Standard mcp.Client{Transport: transport}.Connect(ctx).
```

Errors at step 3 or 5 fail startup — misconfigured OAuth is a fatal config error, not a runtime degradation. Once connected, `oauth2.TokenSource` automatically refreshes access tokens on 401 responses using the refresh token.

### Bootstrap CLI: `core-agent mcp oauth-bootstrap`

Interactive one-time flow for operators to produce the refresh token that goes in a Secret. Shape:

```
Usage: core-agent mcp oauth-bootstrap [flags]

Runs the OAuth 2.0 authorization-code flow for a Streamable HTTP MCP
server configured in mcp.json. Opens a browser to complete the flow;
prints the refresh token to stdout for the operator to stash in a
Kubernetes Secret (or equivalent secret manager).

Flags:
  --mcp-config PATH    Path to mcp.json. Required.
  --server NAME        Name of the server entry to bootstrap. Required.
  --client-secret      Client secret. Read from --client-secret-env if
                       unset; prompted interactively as a last resort.
  --client-secret-env NAME
                       Env var carrying the client secret. Default:
                       the server entry's client_secret_env.
  --callback-addr      Local address to bind the OAuth callback
                       listener on. Default: 127.0.0.1:8765.
  --scopes             Comma-separated scopes override. Default: the
                       server entry's scopes.
  --output-format      "text" (default) prints "REFRESH_TOKEN=..."
                       to stdout. "json" prints {"refresh_token":"...",
                       "expires_in":..., "scopes":[...]}.
```

The flow:

1. Parse `mcp.json`, extract the server entry.
2. Resolve endpoints (discovery or config-supplied).
3. Bind an ephemeral HTTP listener on `--callback-addr` for the OAuth callback.
4. Construct `AuthorizationCodeHandler`, call `StartAuthorizationCodeFlow` with a redirect URI pointing at the local listener.
5. Print the authorization URL to stderr AND attempt to open it in the operator's browser (via `xdg-open` / `open` / `start` per OS). If browser-open fails, operator copy-pastes the URL manually.
6. Callback arrives on the local listener with `?code=...`. Handler exchanges the code for tokens.
7. Print refresh token to stdout in the requested format.
8. Exit.

Operator then:

```bash
kubectl -n agent-triage create secret generic mcp-oauth-tokens \
    --from-literal=SLACK_MCP_REFRESH_TOKEN="<refresh-token-from-stdout>" \
    --from-literal=SLACK_APP_CLIENT_SECRET="<client-secret>"
kubectl -n agent-triage rollout restart deployment core-agent
```

### HTTP client construction

`oauth2.NewClient(ctx, tokenSource)` wraps a base HTTP transport (defaults to `http.DefaultTransport`) with a `Transport` that adds `Authorization: Bearer <token>` to every request AND handles 401-driven token refresh via the source.

We pass `ctx` — the daemon's lifetime context — so token-source cache invalidation happens at shutdown. No goroutine leaks.

### Discovery caching

RFC 8414 metadata is stable across the daemon's lifetime (auth servers republish only on planned events). Cache the fetched metadata in the server-registration struct; refetch only on connection failure (in case an operator did rotate endpoints).

Caching TTL: infinity by default (metadata is treated as immutable). Add a config knob if operators ask, but the default assumption is fine.

## Per-substrate impact

### `pkg/mcp/config.go`

- Add `TransportStreamableHTTP = "streamable_http"` constant.
- Extend `MCPAuth` with `OAuth2 *OAuth2Auth` field.
- New `OAuth2Auth` struct matching the config surface above.
- Parser: validate mutual exclusion (`oauth2_direct` vs `google_oauth`), transport pairing, required fields.

### `pkg/mcp/lifecycle.go`

- New branch in the server bring-up switch for `TransportStreamableHTTP`.
- Wire the SDK's `AuthorizationCodeHandler` per the runtime flow above.
- Startup errors surface with the usual daemon `ExitConfigError`.

### `cmd/core-agent`

- New subcommand `core-agent mcp oauth-bootstrap` per the CLI shape above. Reuses `pkg/mcp` config parsing + auth handler construction; the interactive flow is CLI-side.

### `pkg/mcp` tests

- `httptest.NewServer` mocks for discovery endpoints, token endpoint. Verify the SDK's handler is constructed correctly, tokens flow into the HTTP client, requests carry the bearer token.
- End-to-end test with a mock Streamable HTTP MCP server (SDK provides `mcptest` helpers).

### Docs

- Update `docs/site/content/docs/reference/mcp.md` — add "OAuth 2.0 MCP servers" section covering config surface, bootstrap CLI, refresh-token storage, security guidance.

### `go.mod`

- SDK bump: `github.com/modelcontextprotocol/go-sdk` from `v1.4.1` → latest (`v1.6.1` at time of writing). No breaking API changes in the pieces we touch; low-risk bump.
- `golang.org/x/oauth2` moves from indirect to direct — already a transitive dep, just declared explicitly.

## Config surface — full example

Operator's `mcp.json` with three servers demonstrating all supported combinations:

```jsonc
{
  "version": 1,
  "servers": {
    // Existing pattern: local stdio subprocess. Unchanged.
    "fs": {
      "transport": "stdio",
      "command": "mcp-fs",
      "args": ["--root", "/tmp"]
    },
    // Existing pattern: HTTP with Google ADC. Unchanged.
    "gke": {
      "transport": "http",
      "url": "https://container.googleapis.com/mcp",
      "auth": {
        "google_oauth": {
          "scopes": ["https://www.googleapis.com/auth/cloud-platform"]
        }
      }
    },
    // NEW: Streamable HTTP with OAuth 2.0.
    "slack": {
      "transport": "streamable_http",
      "url": "https://mcp.slack.com/mcp",
      "auth": {
        "oauth2_direct": {
          "client_id": "1234567890.abcdefgh",
          "client_secret_env": "SLACK_APP_CLIENT_SECRET",
          "refresh_token_env": "SLACK_MCP_REFRESH_TOKEN",
          "scopes": ["chat:write", "channels:history"]
        }
      }
    }
  }
}
```

## Migration story

Net-new feature. No migration.

- **Existing deployments**: no config change → no behavior change.
- **New OAuth-authenticated MCP servers**: operators run the bootstrap CLI once, stash the refresh token in a Secret, add the server entry to `mcp.json`, restart the daemon.
- **Rotating a refresh token**: bootstrap CLI again, update the Secret, rolling restart.

## Implementation phases

### Phase 1 — Config parsing + streamable transport wiring (PR ε.1 of #190)

- `pkg/mcp/config.go` — new transport constant, `OAuth2Auth` struct, parser + validator.
- `pkg/mcp/lifecycle.go` — new branch consuming the SDK's `AuthorizationCodeHandler.LoadRefreshToken` path + `StreamableClientTransport`.
- SDK bump to latest.
- Tests: config-parse matrix (valid combinations, mutual exclusion, missing fields), lifecycle test with `httptest` mock server exercising discovery + token exchange + first RPC.

Estimate: ~400 LoC prod + ~350 LoC tests. ~3 days.

### Phase 2 — Bootstrap CLI subcommand (PR ε.2)

- `cmd/core-agent/mcp_oauth_bootstrap.go` — new subcommand.
- Interactive OAuth flow: local callback listener, browser-open helper, `StartAuthorizationCodeFlow` invocation, token print.
- Tests: mock authorization server via `httptest`, verify the callback flow completes end-to-end.

Estimate: ~250 LoC prod + ~200 LoC tests. ~2 days.

### Phase 3 — Docs + Hugo reference page + CHANGELOG (PR ε.3)

- Update `docs/site/content/docs/reference/mcp.md` — "OAuth 2.0 MCP servers" section.
- New operator recipe: `examples/mcp-oauth-slack/` demonstrating end-to-end Slack MCP setup + bootstrap + Secret creation + daemon config.
- CHANGELOG v2.7.0 entry.
- Design doc status flip to "shipped in v2.7".

Estimate: ~200 LoC docs + ~150 LoC recipe. ~2 days.

**Total**: ~1,550 LoC across 3 PRs, ~1 week of focused work.

## Open questions

### 1. Refresh token storage in the config — env var vs file mount

Options for how the daemon reads the refresh token:

- **Env var** (current design) — `refresh_token_env: NAME`. K8s Secret mounted via `valueFrom.secretKeyRef`. Standard pattern; simple.
- **File mount** — `refresh_token_file: /etc/core-agent/tokens/slack.txt`. Rotation is easier (update the Secret, no pod restart if using file-mounted Secrets with `subPath` false).
- **Both** — support one, deprecate the other later.

**Recommendation**: env var only for v2.7. File-mount is a v2.8 enhancement if operators ask for hot rotation. K8s rolling restarts are cheap; the ergonomic gain is marginal.

### 2. Do we support Dynamic Client Registration (RFC 7591)

The SDK ships `oauthex.Register` (DCR). Some MCP servers support DCR (client registers itself dynamically, no manual app-registration step). Slack explicitly does NOT support DCR.

- **Skip DCR in v2.7.** Every server we're targeting (Slack, near-term) requires pre-registration. Add DCR support later if a specific server requires it.
- **Ship DCR now.** Extra config surface (`dcr: { enabled: true, ... }`), extra bootstrap-CLI path, but future-proof.

**Recommendation**: skip in v2.7. Simpler PR. Add when the first DCR-supporting MCP server we care about ships.

### 3. Browser-open in the bootstrap CLI — required or optional

Attempting `xdg-open` / `open` / `start` is convenient but flaky (no browser installed, headless SSH session, container-based dev environments).

- **Print URL + attempt browser** (current design). Operator sees the URL either way.
- **Print URL only.** No auto-open. Simpler, one less code path to test.

**Recommendation**: print URL only. Operators expect this pattern from `gcloud auth login` and other tools that offer a `--no-launch-browser` mode. Simpler test surface. Add `--open-browser` opt-in flag later if operators ask.

### 4. Multi-user tokens on a shared daemon

Slack's OAuth is user-token — the refresh token represents ONE Slack user's authorization. In a multi-session core-agent deployment (v2.4+), multiple operators share one daemon. Who's the Slack user?

- **One shared refresh token** (current design). The daemon acts as a single Slack identity (e.g., an `sre-oncall-bot` user). All operators' incidents post from that identity. Audit trail is at the core-agent side, not the Slack side.
- **Per-caller refresh tokens** — each operator's session uses that operator's Slack authorization. Complex: requires threading `Caller.Identity` through the MCP call, refresh-token lookup keyed by identity, per-caller token secrets.

**Recommendation**: shared token for v2.7. The "each incident posts as its assigned operator" scenario is real but the complexity isn't warranted before a specific use case demands it. Document the limitation.

### 5. Client secret storage — env var, file, or in-memory

Slack (and most OAuth authorization servers) require a client secret. Options mirror OQ #1.

**Recommendation**: env var for v2.7. Same rationale as refresh token.

### 6. Discovery cache TTL

Auth server metadata is meant to be stable. But operators occasionally rotate endpoints.

- **Cache forever** (current design). Refetch only on connection failure.
- **Cache for N (e.g., 24h)** — refresh proactively.
- **Configurable per-server** — `discovery_cache_ttl: "24h"`.

**Recommendation**: cache forever, refresh on connection failure. Simplest; matches how well-behaved OAuth clients treat this.

### 7. What happens on refresh-token expiry / revocation

Refresh tokens can be revoked server-side (user revoked the app; Slack workspace admin unpublished the app; token expired at Slack's max lifetime).

- **Fail loudly** (current design). Next MCP request returns 401 → `oauth2_direct` package returns error → MCP client returns error → tool call fails. Operator sees a clear "MCP call failed: token refresh failed" in the daemon log.
- **Silent degradation** — mark the server as "disconnected", allow the daemon to keep serving other MCP servers.
- **Proactive re-bootstrap** — the daemon exits with a specific code that a restart controller could trap to trigger a fresh bootstrap. Complex; not native to K8s.

**Recommendation**: fail loudly for v2.7. Operator gets a stack-trace-style error with the underlying cause; they rerun the bootstrap CLI and update the Secret.

### 8. Do we bump the SDK now or defer

Current pin: `github.com/modelcontextprotocol/go-sdk@v1.4.1`. Latest: `v1.6.1`.

- **Bump as part of ε.1** (current design). Small blast radius; we're touching MCP code anyway.
- **Defer** — separate PR to bump; ε.1 works against v1.4.1 if the primitives are there.

**Recommendation**: bump as part of ε.1. The primitives ARE in v1.4.1 (verified) but the newer version has bug fixes we'd want anyway.

## Forward compatibility with credential-resolution (v2.8+)

v2.7's OAuth support is deliberately daemon-scope: one refresh token per MCP server, one identity for all callers, no `Caller`-awareness. That's the right MVP shape. But `docs/mcp-credential-resolution-design.md` (a deferred v2.4 design, re-scoped to v2.8+) will eventually add per-caller 3LO credentials, shared named providers, and Google Auth Manager integration — and implementers of this doc need to leave that door open.

Every implementation decision in ε.1 should preserve the following eight properties so v2.8+ can extend without a rewrite. These are architectural guardrails, not new scope.

### 1. Wrap HTTP client construction behind an internal `httpClientForServer` interface

Don't call `oauth2.NewClient(ctx, tokenSource)` directly from `pkg/mcp/lifecycle.go`. Introduce a small unexported `httpClientForServer(cfg ServerSpec) (*http.Client, error)` — today it returns the SDK-wrapped client with one token source; tomorrow it returns a client whose `Transport` does per-caller lookup at `RoundTrip` time. The lifecycle wiring + SDK transport construction stay unchanged across the v2.8+ swap.

### 2. Preserve `auth.CallerFromContext` all the way to `RoundTrip`

v2.4's α.2 work already threads `Caller` via `context.Value` through outbound MCP requests. Verify the OAuth wrapper doesn't strip context values. Add a test asserting `auth.CallerFromContext(ctx).Identity` is available at the transport layer for OAuth-authenticated servers. Cheap test; blocks a subtle regression that would silently break per-caller resolution when it arrives.

### 3. Keyed cache with `caller` as an ignored dimension today

If we cache anything (even the single daemon-wide token source), key on an opaque struct: `{providerName, callerIdentity, scopes}`. Today `callerIdentity == ""` (or the daemon's default identity). Tomorrow it varies per request. Zero-cost extensibility.

### 4. Config shape admits a named-provider variant

Ship `auth.oauth2_direct` as inline per-server (this doc's design). But reserve `auth.provider: "<name>"` for v2.8+ — the field is recognized by the loader today with a clear error: `"named providers are not supported in v2.7; use inline auth.oauth2_direct"`. When the credential-resolution work lands, the loader flips to accept both shapes. No config migration for operators upgrading; new deployments can adopt named providers from day one.

### 5. Bootstrap CLI records the identity it authenticated as

`core-agent mcp oauth-bootstrap`'s output includes not just the refresh token but the identity that consented (extractable from the OAuth token response's `id_token` or a follow-up userinfo call). In v2.7 this metadata is documentation. In v2.8+, the bootstrap CLI keys the Secret entry by the caller identity (`SLACK_MCP_REFRESH_TOKEN_alice_at_example_com`) — that convention needs the identity captured now.

### 6. Provider kind named `oauth2_direct` (matches credential-resolution taxonomy)

Renamed in this design from `oauth2`. Aligns with the `oauth2_direct` provider from `docs/mcp-credential-resolution-design.md` §"Provider implementations." In v2.7, `oauth2_direct` only supports the daemon-scope case (single `refresh_token_env`). In v2.8+, additional config fields (`caller_scoped: true`, `caller_token_lookup`) unlock per-caller mode — same provider kind, extension via new fields. Config-shape stability across releases; no rename lift.

Prevents the awkward "we have `oauth2` and `oauth2_direct` — what's the difference?" question when the credential-resolution work lands.

### 7. Per-server state scoping in `pkg/mcp` — no package-level maps

Concrete implementation rule: no package-level maps of token sources, no package-level HTTP client cache, no daemon-global OAuth handler. Every server gets its own instances, keyed by server name inside the MCP subsystem's own struct. This is table stakes for coexistence — a bug where Server A's OAuth token leaks to Server B is exactly the kind of state coupling that makes per-caller support miserable to retrofit. Cheap on day one; painful to unwind.

Add a test asserting: a config with three servers, three different providers (`google_oauth`, `oauth2_direct`, and a stub `static_api_key`), sends the right credential to each. Fails immediately on cross-server state coupling.

### 8. ctx-Caller propagation preserved through the provider boundary even when the provider doesn't consume it

The instinct on v2.7 might be "OAuth doesn't use Caller — I don't need to worry about it." Wrong instinct: preserve `ctx` propagation through the entire request path so per-caller providers (arriving in v2.8+) find `Caller` there. The provider-boundary abstraction should always pass `ctx` through; whether the provider *uses* it is per-Kind.

### Mixed-provider coexistence — the target for v2.7's tests

ε.1's tests must include a config demonstrating both credential modes coexisting side-by-side, because we're going to want this working end-to-end for real deployments (GKE MCP uses daemon KSA; Slack MCP uses the OAuth-bootstrapped token):

```jsonc
{
  "version": 1,
  "servers": {
    // Daemon-identity credentials — every session uses the daemon's KSA.
    "gke": {
      "transport": "http",
      "url": "https://container.googleapis.com/mcp",
      "auth": { "google_oauth": { "scopes": ["https://www.googleapis.com/auth/cloud-platform"] } }
    },
    // OAuth (v2.7: daemon-wide refresh token; v2.8+: per-caller when
    // caller_scoped is set on the provider). Same server config today;
    // one added field tomorrow.
    "slack": {
      "transport": "streamable_http",
      "url": "https://mcp.slack.com/mcp",
      "auth": {
        "oauth2_direct": {
          "client_id": "1234567890.abcdefgh",
          "client_secret_env": "SLACK_APP_CLIENT_SECRET",
          "refresh_token_env": "SLACK_MCP_REFRESH_TOKEN",
          "scopes": ["chat:write", "channels:history"]
        }
      }
    }
  }
}
```

GKE calls always route through the daemon's `google_oauth` provider (ignores `Caller`). Slack calls in v2.7 all route through the daemon-wide `oauth2_direct` provider (same identity for every caller). In v2.8+, the Slack entry gets one additional field enabling per-caller resolution; GKE stays as-is because it's fundamentally daemon-identity-shaped.

Test invariant: a request that reaches `pkg/mcp` with `Caller.Identity = "alice@example.com"` and targets the GKE server routes to `google_oauth`'s daemon-KSA token; the same caller targeting the Slack server routes to `oauth2_direct`'s bootstrapped token. Different callers hitting the same GKE server get the same daemon token. Zero cross-server or cross-provider bleed.

## Security considerations

- **Refresh tokens are long-lived credentials.** Store in Kubernetes Secrets (or your org's secret manager); never commit. Rotate on any suspected compromise.
- **Client secrets are also credentials.** Same posture as refresh tokens. Slack's client secret in particular gates the entire OAuth flow.
- **PKCE is on by default.** Even though we're a confidential client (with a client_secret), PKCE adds defense-in-depth against authorization-code interception. Only disable if a specific server explicitly rejects PKCE (unlikely — most support both).
- **The bootstrap CLI's local callback listener.** Bound to `127.0.0.1:<port>` by default. NOT publicly reachable. Operators running the bootstrap from a jump host / dev container should port-forward to their laptop — the SDK's OAuth handler enforces the callback URL registered with the authorization server, so listening on `0.0.0.0` would only work if the operator registered `0.0.0.0:<port>` (which they shouldn't).
- **Discovery URL validation.** RFC 8414 metadata URLs are validated by the SDK (`oauthex.validateAuthServerMetaURLs`) to prevent XSS-via-metadata. Our config-supplied override endpoints get the same treatment.
- **Access token scope.** Requested scopes are the operator's choice; principle of least privilege — request only what the triage skill needs. Slack: `chat:write` alone (post-only) if you don't need to read channels.
- **Audit.** Every MCP tool call the agent makes with an OAuth-authenticated server is captured in the eventlog with the tool name, args, and result. Multi-session mode also stamps the caller identity, so "who invoked what" is queryable.

## Out of scope (deferred to v2.8+)

- **Client-credentials grant (RFC 6749 §4.4)** for machine-to-machine flows without a user in the loop.
- **JWT bearer grant (RFC 7523)** for signed-JWT-based authentication (used by some enterprise MCP servers).
- **Per-caller OAuth tokens** in multi-session deployments (OQ #4). Deferred to v2.8+ via the credential-resolution design (`docs/mcp-credential-resolution-design.md`); v2.7's implementation MUST follow §"Forward compatibility with credential-resolution (v2.8+)" so the extension is additive, not a rewrite.
- **Dynamic Client Registration (RFC 7591)** for MCP servers that support it (OQ #2).
- **Hot rotation of refresh tokens** without daemon restart (OQ #1).
- **Browser-open helper in the bootstrap CLI** (OQ #3 — opt-in if operators ask).
- **A `/mcp/reconnect` slash command** to trigger a specific MCP server reconnect after fixing a token — currently requires full daemon restart.

## Dependencies and related work

- **[#186](https://github.com/go-steer/core-agent/issues/186) v2.6 k8s-event agent** — ε.3 (escalation MCP integration) is deferred pending this work. Once MCP-OAuth ships, the triage recipe can add Slack's official MCP as the canonical escalation path.
- **[MCP spec 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#streamable-http)** — the transport we're implementing.
- **[github.com/modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk)** — the SDK we consume. Every primitive we need is already in `auth/`, `oauthex/`, `mcp/streamable_client.go`.
- **[Slack MCP server docs](https://docs.slack.dev/ai/slack-mcp-server)** — the first canonical OAuth-Streamable MCP we'll consume.
- **RFC 6749 (OAuth 2.0)**, **RFC 7636 (PKCE)**, **RFC 8414 (Server Metadata)**, **RFC 9728 (Protected Resource Metadata)** — the standards this design implements client-side.

## When this lands

- Phase 1 (config + transport + auth wiring): ~3 days
- Phase 2 (bootstrap CLI): ~2 days
- Phase 3 (docs + recipe + CHANGELOG): ~2 days

~1 week of focused work across 3 PRs. Smaller than the v2.6 k8s-event agent stack; unlocks broad MCP ecosystem access as a side effect of enabling one specific integration (Slack).
