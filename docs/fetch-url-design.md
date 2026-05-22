# `fetch_url`: HTTP GET as a first-class built-in + URL-scope allowlist

Design doc for the only tool we're picking up from Hermes's
~80-file `tools/` catalog. Untracked sibling to
[`shared-memory-design.md`](shared-memory-design.md),
[`attach-mode-design.md`](attach-mode-design.md),
[`peer-registration-design.md`](peer-registration-design.md),
[`scheduled-monitoring-design.md`](scheduled-monitoring-design.md),
[`bidirectional-mcp-design.md`](bidirectional-mcp-design.md),
[`code-mode-design.md`](code-mode-design.md). Surfaced 2026-05-22
while auditing Hermes's tool catalog for genuine gaps.

## Context

Agents constantly need to read URLs — GitHub issues, OpenAPI
specs, doc pages, MCP-discovered endpoints, release-notes pages,
status APIs that aren't full MCP servers. Today the model has
three paths and all three are bad:

1. **`bash curl`** — works, but the URL is a string inside a
   shell command rather than a structured tool argument; output
   is truncated through `bash`'s per-tool cap before any HTTP
   metadata is preserved; the URL itself never lands in the
   eventlog as a queryable field (only as bash command text);
   the permission gate can only see "the model is running curl,"
   not "the model is fetching `github.com/X`."
2. **A downstream MCP server** (e.g. fetch-mcp). Works for
   operators who set one up, but it's awkward for a baseline
   capability that 80% of agent runs need. Adds another process
   to deploy.
3. **`web_tools`-style provider extensions** (Anthropic web
   search, Gemini grounded search). Bundled with the model, not
   under our gate, output shape varies by provider.

Hermes ships three files for this category (`web_tools.py`,
`url_safety.py`, `website_policy.py`); we can collapse the same
capability into **one tool + one config knob + one CLI flag**.

The structural-defaults framing: today operators wanting to lock
HTTP egress at the agent level have to either (a) wrap the
process in OpenShell / a sidecar proxy or (b) trust convention
prompts. A first-class URL allowlist enforced at the tool
boundary gives them a third option that *doesn't* require an
outer sandbox. Same shape as `PathScopeConfig` for files —
familiar mental model, mirror surface.

### Settled decisions (do not relitigate)

- **One tool: `fetch_url`.** Not `web_get` / `web_search` /
  `download_file` / etc. One verb, HTTP GET, returns body +
  metadata. POSTs / forms / scrapers belong in dedicated MCP
  servers; we don't go there.
- **One config field: `url_scope`** on `config.Config`, mirroring
  the existing `path_scope`. Same Allow + Deny grammar; same
  Allow-vs-Deny precedence rules (Deny wins on overlap).
- **Default policy: deny everything.** A binary with no
  `url_scope.allow` and no `--allow-url-host` flag refuses every
  fetch. Matches `permissions.mode: "ask"` posture for tools —
  fail closed, not open.
- **Pattern grammar: host-only globs.** `github.com`,
  `*.googleapis.com`, `*.svc.cluster.local`. Path-level
  patterns (`github.com/orgname/*`) are out of scope for v1 —
  cleaner mental model, easier to audit, easier to write
  correctly. Revisit if a consumer asks.
- **Body cap: 64 KiB default**, configurable via tool argument
  (model-set, capped by `ToolOutputConfig`) and via
  `url_scope.max_body_bytes`. Matches the existing per-tool
  truncation pattern.
- **Redirect policy: follow up to 5, same-scope enforced
  per-hop.** Each redirect target is re-checked against the
  allowlist. A redirect from an allowed host to a denied host
  fails with a clear error (`url_scope: redirect target
  example.com not in allowlist`).
- **HTTPS only by default.** Plain HTTP requires an opt-in
  pattern with `http://` prefix in the allowlist
  (`http://localhost:*`, `http://*.svc.cluster.local`). The
  default `github.com` pattern is HTTPS-only.
- **No cookie persistence, no session state.** Each `fetch_url`
  call is independent; no cookie jar carries between calls. If
  you need stateful HTTP, that's a custom MCP server, not us.
- **Auth headers from env only.** A model-facing tool argument
  for `Authorization: Bearer ...` is a *credential exfiltration
  vector by design* — the model can ask itself for any header
  value. Instead: operator pre-declares per-host header
  templates in `url_scope.headers`, sourced via `${ENV_VAR}`
  expansion. Same pattern as `--attach-token=ENVVAR` in
  attach-config. The model picks the host; the headers are
  filled in by the operator-controlled config.
- **Every fetch emits an eventlog entry** with `Author="tool/fetch_url"`,
  `CustomMetadata={url, final_url, status, content_type,
  bytes_returned, truncated}`. This is the audit-derived
  property — paired with the shared-memory work, every URL the
  agent ever fetched is queryable via `recall_memory` after the
  fact.
- **Gate-checked like every other tool.** `permissions.Gate.Check`
  sees `tool=fetch_url, args={url, max_bytes}`. Operators can
  `permissions.allow: ["fetch_url:github.com/*"]` patterns the
  same way they allow bash subcommands today.

## The `fetch_url` tool

### Signature

```jsonc
// model-facing schema (genai function declaration)
{
  "name": "fetch_url",
  "description": "Fetch a URL via HTTP GET. Returns body, content type, status, and final URL after redirects. URLs must be in the configured url_scope.allow list. HTTPS only unless the operator explicitly allowed http:// patterns.",
  "parameters": {
    "type": "object",
    "required": ["url"],
    "properties": {
      "url":        { "type": "string", "description": "Fully-qualified URL to fetch (e.g. https://api.github.com/repos/X/issues/1)." },
      "max_bytes":  { "type": "integer", "description": "Body size cap. Default 65536. Hard cap by config." }
    }
  }
}
```

### Return shape

```jsonc
{
  "url":           "https://api.github.com/repos/foo/bar/issues/1",
  "final_url":     "https://api.github.com/repos/foo/bar/issues/1", // after redirects
  "status":        200,
  "content_type":  "application/json; charset=utf-8",
  "bytes":         12483,
  "truncated":     false,
  "body":          "..."   // text; binary content returns truncated=true + body=""
}
```

### Errors

Surfaced as tool-result errors the model can adapt to (not panics):

| Condition | Error |
|---|---|
| URL not in allowlist | `url_scope: host github.com not in allowlist (configured: foo.com, bar.com)` |
| Redirect target denied | `url_scope: redirect target evil.com not in allowlist` |
| HTTP (no https://) without explicit opt-in | `url_scope: http:// requires explicit http://* pattern in allowlist` |
| Body exceeds cap | `body truncated at 65536 bytes (content was 184321 bytes); pass max_bytes=N or narrow the query` |
| Non-text content + no `application/json|text/*` | returned with `truncated=true, body=""` rather than dumping bytes |
| Network failure / timeout | `fetch_url: connection to <host>: <err>` |
| Non-2xx response | returned normally with `status=4xx/5xx, body=<error body>`; not an error — model decides |

### Timeouts + retries

- 30 s default per-fetch timeout, configurable via
  `url_scope.timeout_seconds`.
- **No automatic retries.** A 5xx is the model's problem to
  retry intentionally — silent retry hides intermittent failures
  from the audit trail.

## `URLScopeConfig`

```go
// package config

// URLScopeConfig governs which URLs the fetch_url built-in is allowed
// to reach. Same Allow/Deny grammar + precedence as PathScopeConfig:
// Deny wins on overlap; empty Allow == default-deny-everything.
// Host-only globs; path-level patterns are out of scope for v1.
type URLScopeConfig struct {
    Allow          []string                  `json:"allow,omitempty"`
    Deny           []string                  `json:"deny,omitempty"`
    MaxBodyBytes   int                       `json:"max_body_bytes,omitempty"`   // default 65536
    TimeoutSeconds int                       `json:"timeout_seconds,omitempty"`  // default 30
    Headers        map[string]map[string]string `json:"headers,omitempty"`        // host-pattern → header-name → ${ENV_VAR}
}
```

Worked example:

```jsonc
"url_scope": {
  "allow": [
    "api.github.com",
    "*.googleapis.com",
    "*.svc.cluster.local",
    "http://localhost:*"
  ],
  "deny": [
    "*.internal.evil.com"
  ],
  "max_body_bytes":   131072,
  "timeout_seconds":  30,
  "headers": {
    "api.github.com": {
      "Authorization": "Bearer ${GITHUB_TOKEN}",
      "Accept":        "application/vnd.github+json"
    }
  }
}
```

Headers are matched by **most-specific host pattern wins**
(`api.github.com` beats `*.github.com` beats `*`). Values pass
through `os.ExpandEnv` at request time, not config-load time, so
rotated env vars take effect on the next fetch without a
restart.

## CLI surface

```
--allow-url-host=<pattern>    # appends to url_scope.allow; repeatable
--no-fetch                    # disable the fetch_url tool entirely
--url-scope-headers=<path>    # load headers from a separate file
                              # (so secrets-bearing headers stay out of
                              #  the main config that may be committed)
```

`--allow-url-host` is the convenience flag for one-off invocations
that just need to read one specific host without editing the
config file. Composes additively with `url_scope.allow`.

## Composition with existing primitives

| Primitive | Interaction |
|---|---|
| **`permissions.Gate`** | Every `fetch_url` call goes through it; operators can write `permissions.allow: ["fetch_url:github.com/*"]` patterns that gate per-host (Gate sees the URL as a structured arg, not a stringly-typed bash command). |
| **`config.PathScopeConfig`** | URL scope is the network analog; same Allow/Deny grammar so operators have one mental model. |
| **`tools.ToolOutputConfig`** | Per-tool truncation cap applies on top of `url_scope.max_body_bytes`; the smaller wins. |
| **`eventlog`** | Every fetch emits a `tool/fetch_url` event with structured URL + status + bytes metadata. Audit trail captures what URLs were touched, when, what came back. |
| **`memory.FromEventlog`** (per `shared-memory-design.md`) | Because fetches land in the eventlog, the `recall_memory` tool can answer "what URLs did I fetch about topic X" — fetched URLs are part of the same audit-derived recall substrate. |
| **`attach-mode`** | The live-tail SSE stream surfaces `fetch_url` events the same as any other tool call — operator can watch fetches in real time. |

## Out of scope (v1)

- **POSTs / forms / multipart uploads.** `fetch_url` is GET-only.
  Operators wanting structured POSTs build a dedicated MCP
  server (where the operation can be schema-typed properly).
- **JavaScript execution / dynamic content.** Use the playwright
  MCP server (`@modelcontextprotocol/playwright`); same
  capability, dedicated maintenance.
- **Cookie persistence between calls.** Each fetch is
  independent; no cookie jar.
- **Auth-header injection by the model.** Headers come from
  operator-declared `url_scope.headers` + env expansion, never
  from a tool argument.
- **Path-level URL patterns** (`github.com/orgname/*`). Host-only
  globs in v1 — simpler mental model, fewer footguns. Revisit
  on consumer ask.
- **Streaming responses.** Body returned in one chunk after
  download completes (with cap). Streaming would change the
  return shape and the audit-event shape; defer until needed.
- **HTTP/2 server push, WebSockets, SSE consumption.** Not in
  the GET-a-URL bucket; out of scope.
- **Caching.** No HTTP cache; each fetch hits the wire. The
  shared-memory `recall_memory` tool is the structured-cache
  story — agents that want "did I already fetch this" ask the
  memory, not a cache.

## Implementation sketch

About **180 LoC + ~150 LoC tests** on top of the existing
`tools/` package. One PR.

- `tools/fetch.go` — `fetchURLFunc` + `fetchURLSchema` + URL
  validation against `URLScopeConfig` + header injection +
  redirect handling + body truncation. (~120 LoC.)
- `tools/fetch_test.go` — round-trip against `httptest.NewServer`:
  happy path, allowlist denial, redirect-to-denied-host, body
  truncation, header injection from env, http-vs-https policy,
  timeout. (~150 LoC.)
- `config/config.go` — add `URLScope URLScopeConfig` field +
  `URLScopeConfig` type + defaults. (~30 LoC.)
- `config/url_scope_test.go` — round-trip, Allow/Deny precedence,
  empty-allow-is-deny-all. (~60 LoC.)
- `tools/builtins.go` — register `FetchURL` field; tool joins
  the `tools.Default()` set behind a `URLScope`-non-empty check
  (no scope → no tool, no surprises). (~20 LoC.)
- `cmd/core-agent/main.go` — wire `--allow-url-host`,
  `--no-fetch`, `--url-scope-headers` flags. (~30 LoC.)
- `docs/site/content/docs/configuration.md` — new `## url_scope`
  section with field table + worked example. CHANGELOG entry
  under `[Unreleased]`.

### Follow-on: `osv_query` (~40 LoC)

Once `fetch_url` exists, an `osv_query` built-in is essentially a
schema'd wrapper around `POST api.osv.dev/v1/query` — security-aware
dev agents (autonomous PR reviewer, dependency-audit loops)
love this. Implement as a separate ~40-line tool that internally
calls `fetch_url` under the hood, sharing the gate + audit + cap
machinery. Skip if no consumer asks; capture as the worked example
of "what `fetch_url` unlocks."

## Open questions

1. **Tool argument for per-call headers?** Today's design: no
   model-set headers, only operator-declared via config. Should
   we allow a model-set `headers` arg restricted to a
   non-credential allowlist (`Accept`, `Accept-Language`,
   `User-Agent`)? Lean: yes, with a hardcoded allowlist of
   non-sensitive header names — small surface, real value
   (content negotiation matters). Decide before PR freezes the
   schema.
2. **Per-host rate limits.** `url_scope.rate_limit_per_host` to
   cap fetches per minute? Useful for runaway loops; not strictly
   needed for v1. Defer.
3. **Caching layer.** Out of scope above, but worth revisiting
   once `recall_memory` is shipped — the natural shape is
   "fetched URL → eventlog entry → memory recall," which *is* a
   structured cache but requires the model to think about it.
   If a consumer asks for transparent caching, design it then.
4. **HTTPS cert verification.** Default: full verification.
   Should `url_scope.insecure_hosts: ["*.internal.example.com"]`
   exist for self-signed internal services? Lean: yes, narrow
   opt-in, denied by default. Capture as v1.1.
5. **Compose with `bidirectional-mcp`.** When core-agent acts as
   an MCP server (per `bidirectional-mcp-design.md`), does
   `fetch_url` get re-exported to upstream MCP clients?
   Probably yes — it's a generally useful tool. Mention it in
   the bidi-MCP doc.

## Why this is the only tool we're picking up

Hermes ships ~80 tool files; we audited the full list. Most fall
into three buckets: (a) **already covered** by our existing
suite (file ops, bash, todo, mcp, skills, code-execution,
delegate-to-subagent); (b) **better as `extras/` or downstream
MCP** (browser automation, image/video/voice generation,
platform integrations like Discord/Slack/HomeAssistant); (c)
**Nous-specific internals** (Tirith, debug helpers, schema
sanitizers).

`fetch_url` is the **only** tool that closes a real gap *and*
fits our structural-defaults posture *and* unlocks a downstream
capability story (URL allowlist as agent-level egress control,
audit-derived recall of HTTP requests via shared-memory). One
clean addition — not a parity grab.
