# Agent card: `/.well-known/agent-card.json` for discovery

Design doc for the next minor. Untracked sibling to
[`attach-mode-design.md`](attach-mode-design.md),
[`peer-registration-design.md`](peer-registration-design.md),
[`bidirectional-mcp-design.md`](bidirectional-mcp-design.md).

## Context

Attach-mode v1 (PR #1) gives an operator a way to talk to a running
agent. Peer-registration (PR #2) gives them a way to enumerate a fleet
of attach listeners. Neither tells an *external discovery system*
("here are the agents in my org, what can each one do?") anything
about a binary it hasn't been pointed at by hand.

The canonical machine-readable answer is the
[A2A AgentCard](https://agent2agent.info/docs/concepts/agentcard/) —
a JSON document at `/.well-known/agent-card.json` describing the
agent's name, description, version, capabilities, auth, and skills.
[Google Cloud Agent Registry](https://docs.cloud.google.com/agent-registry/register-agents)
fetches this URL during registration to index the skills array for
keyword search. The card is treated as **discovery metadata**: the
Registry indexes it whether or not the URL also speaks the A2A
JSON-RPC transport. Manual registration explicitly supports two
modes:

- `A2A_AGENT_CARD` — agent serves the card, skills get indexed.
- `NO_SPEC` — plain REST endpoint, name/description/URL only.

Serving the card buys us the richer path with no commitment to
implementing the A2A transport. `attach-mode-design.md:65-77` already
established that the A2A transport itself is deferred pending a
concrete consumer; this PR addresses the discovery half of the story
in isolation.

### Settled decisions (do not relitigate)

- **One card per binary.** Not one per registered session. The card
  describes "this core-agent process and the skills it can perform,"
  matching Google Cloud Agent Registry's "one agent = one URL"
  mental model. Per-session cards (e.g.
  `/sessions/<app>/.well-known/agent-card.json`) are not in v1; if a
  consumer asks for per-app cards later, the per-session shape is
  additive on top of this PR's machinery.
- **Served from `attach.Server`, not a separate listener.** Same
  HTTP server, same `mux`, same auth posture choices. The endpoint
  is the only one in the attach package that's *always
  unauthenticated* — public discoverability is the point. (Bearer /
  mTLS auth still applies to every other attach endpoint.)
- **Opt-in.** If the operator doesn't supply a card description, the
  `/.well-known/agent-card.json` route is not registered and returns
  404. A binary built with the attach listener but no card config
  behaves exactly like today's binary plus a card-shaped 404.
- **`url` is derived from the request, not configured.** The handler
  echoes back the URL the caller used to reach us (`Host` header +
  `X-Forwarded-Proto`/`X-Forwarded-Host`). The operator doesn't have
  to know the binary's own external address — by definition, the
  consumer fetching `/.well-known/agent-card.json` already knows it.
  Same convention as OIDC discovery and most other well-known
  endpoints. An optional `external_url` override exists for the rare
  case of wanting to publish a canonical URL different from the
  fetch URL (e.g. two ingresses, one canonical).
- **Card is metadata, not a protocol promise.** The `url` points at
  the attach listener — an A2A JSON-RPC client that fetches the card
  and then POSTs to that URL will get a 404. Google Cloud Agent
  Registry, the actual consumer, only reads the card; it never drives
  the URL as A2A. `capabilities` is filled in honestly (`streaming:
  true` — SSE via attach is real; `pushNotifications: false`;
  `stateTransitionHistory: false`). No claim of A2A compliance.
- **Skills derive from two sources, merged.** (a) curated extras
  declared in `.agents/agent-card.json` (or set on
  `Options.AgentCard.ExtraSkills` by an embedder), and (b) the
  existing `.agents/skills/` bundles already loaded by
  `skills.LoadAll` and surfaced through `Registrant.AttachSkills()`
  / `agent.WithAttachSkillsProvider`. Internal tools (`report_done`,
  inbox primitives, MCP tools, etc.) are **never** auto-included —
  they'd leak implementation detail into a public search index.
- **Curated skills live in `.agents/agent-card.json`, not in CLI
  flags.** Skill descriptions are multi-sentence, tags and examples
  are arrays — none of that fits a `:`-delimited flag, and a flag
  would encourage truncated/garbled descriptions that hurt the very
  search relevance the card exists to provide. The file lives next
  to the `.agents/skills/` bundles it describes and gets checked
  into the repo with them. Discovery follows the same pattern as
  `.agents/config.json` and `.agents/mcp.json`.
- **No live reload of the card itself.** The card is built on
  request from the (already-live-reloaded) skills provider plus the
  static curated extras, so a newly-dropped SKILL.md surfaces on
  the next fetch without restart. Same shape as today's
  `GET /sessions/<id>/skills`.
- **A2A transport stays deferred.** This PR does not add JSON-RPC
  endpoints, task lifecycle plumbing, or push notifications.
  `attach-mode-design.md`'s "thin `attach/a2a.go` adapter when a
  consumer surfaces" plan is unchanged. The card and the transport
  are independent shippable units.

## Goals and non-goals

### Goals

- **Make a core-agent binary discoverable by Google Cloud Agent
  Registry** (and any other system that fetches
  `/.well-known/agent-card.json`) without changing the binary's
  protocol surface.
- **Surface `.agents/skills/` content as a public, searchable skills
  list** so operators registering a fleet get keyword-searchable
  inventory ("which of my agents has the `migrate-db` skill?").
- **Stay opt-in and additive.** No card config → no endpoint, no
  behavior change. The attach-mode v1 / peer-registration v2 surface
  is unaffected.
- **Reuse what's already wired.** `Registrant.AttachSkills()` and
  the `agent.WithAttachSkillsProvider` plumbing already exist for
  the in-TUI skills view; the card endpoint just reads the same
  data through a different projection.

### Non-goals

- **Not an A2A endpoint.** No `message/send`, no `tasks/get`, no
  JSON-RPC handler. Card-only.
- **Not per-session.** The card lives at `/.well-known/...`, not at
  `/sessions/<app>/.well-known/...`.
- **Not authenticated.** Same posture as `robots.txt` or any other
  well-known descriptor — public by spec convention.
- **Not validating the A2A spec ourselves.** We emit a JSON document
  whose required fields match the published shape; we don't ship a
  schema validator. Wire-format pinning tests substitute (see
  Acceptance).
- **Not multi-binary aggregation.** If the operator runs a peer-
  registration hub, the hub does not synthesize a combined card.
  Each registered peer's listener serves its own card; the hub's
  `GET /peers` is the enumeration surface.

## Wire format

Aligned to the **A2A v0.3.0 schema** (the latest tagged commit with a
checked-in JSON Schema; `main` removed the committed schema and now
generates it from the proto at build time — see Acceptance #6 for
vendoring policy). Example card from a `core-agent` instance running
a `migrate-db` and `triage-incident` SKILL.md bundle, with one
curated extra skill:

```json
{
  "protocolVersion": "0.3.0",
  "name":            "core-agent",
  "description":     "Production-incident response agent for the platform fleet.",
  "url":             "https://agent.prod.svc.cluster.local:7777",
  "version":         "v2.2.0",
  "provider": {
    "organization": "Platform Team",
    "url":          "https://example.internal/platform"
  },
  "documentationUrl": "https://example.internal/platform/runbooks/core-agent",
  "capabilities": {
    "streaming":              true,
    "pushNotifications":      false,
    "stateTransitionHistory": false
  },
  "securitySchemes": {
    "mtls":   { "type": "mutualTLS" },
    "bearer": { "type": "http", "scheme": "Bearer" }
  },
  "security": [
    { "mtls": [], "bearer": [] }
  ],
  "defaultInputModes":  ["text/plain"],
  "defaultOutputModes": ["text/plain"],
  "skills": [
    {
      "id":          "migrate-db",
      "name":        "migrate-db",
      "description": "Run zero-downtime schema migrations against a target environment.",
      "tags":        ["skill"]
    },
    {
      "id":          "triage-incident",
      "name":        "triage-incident",
      "description": "Pull on-call signals and produce a first-pass incident summary.",
      "tags":        ["skill"]
    },
    {
      "id":          "rollback-deploy",
      "name":        "Rollback a deploy",
      "description": "Curated: revert a Cloud Deploy release to the previous revision.",
      "tags":        ["curated", "deploy"]
    }
  ]
}
```

The v0.3.0 optional fields `preferredTransport`,
`additionalInterfaces`, `iconUrl`, `signatures`, and
`supportsAuthenticatedExtendedCard` are intentionally omitted from
v1 — they describe transport variants and signed-card features we
don't yet implement.

### Field derivation rules

| Card field | Source | Notes |
|---|---|---|
| `protocolVersion` | constant `"0.3.0"` | The schema version we conform to. Bumps when we update the vendored A2A schema fixture. |
| `name` | `Options.AgentCard.Name`, else first registrant's `AppName()`, else `"core-agent"` | One name per binary; doesn't change as sessions come and go. |
| `description` | `Options.AgentCard.Description` override, else `Registrant.Description()` via the optional `DescriptionProvider` capability, else empty. | Required by A2A spec. The bundled `core-agent` binary fans `.agents/config.json`'s `agent.description` to both ADK's `WithDescription()` AND the card config, so one source of truth lights up both surfaces. `--agent-card-description` is a card-only override for the rare case where the public-facing wording differs from the LLM-facing one. |
| `url` | per-request: `Options.AgentCard.ExternalURL` if set, else `X-Forwarded-Proto`+`X-Forwarded-Host`, else `r.TLS`-aware scheme + `r.Host`. | The caller already knows the URL they used to fetch the card; the handler echoes it back. ExternalURL is an optional override. Forwarded-headers are forgeable by direct callers, but the card is public metadata with no security implications. |
| `version` | `Options.AgentCard.Version`, else `internal/version.Version` | The ldflag-injected build version. |
| `provider` | `Options.AgentCard.Provider` | Omitted if zero. Spec requires BOTH `organization` and `url` if provider is present — the loader rejects a half-populated provider. |
| `documentationUrl` | `Options.AgentCard.DocumentationURL` | Omitted if empty. |
| `capabilities.streaming` | constant `true` | SSE via attach. |
| `capabilities.pushNotifications` | constant `false` | Would require an A2A transport. |
| `capabilities.stateTransitionHistory` | constant `false` | A2A-specific. |
| `securitySchemes` / `security` | derived from `Options.Auth` | `mtls` scheme (`{type: "mutualTLS"}`) if `ClientCAFile` set; `bearer` scheme (`{type: "http", scheme: "Bearer"}`) if `BearerToken` set. `security` emits a single AND-combination object (`{mtls: [], bearer: []}`) since the middleware enforces them together. Both omitted if neither auth is configured. |
| `defaultInputModes` | constant `["text/plain"]` | Attach-mode is text in. |
| `defaultOutputModes` | constant `["text/plain"]` | Event stream is text-shaped. |
| `skills[]` | union(curated extras, registrant skills) | Curated wins on `id` collision. Sorted by `id` ascending. `tags` is **required** by the v0.3.0 spec — auto-derived skills get `["skill"]`, curated extras default to `["curated"]` when unset. |

### Skill merge rule

For each registrant in the registry, call `AttachSkills()` and union
into a `map[id]SkillInfo`. Then overlay `Options.AgentCard.ExtraSkills`
— curated entries replace auto-derived ones on `id` collision. Emit
sorted by `id`. Each skill row maps as:

- `id`           ← `SkillInfo.Name` (the SKILL.md frontmatter `name`)
- `name`         ← `SkillInfo.Name`
- `description`  ← `SkillInfo.Description`
- `tags`         ← `["skill"]` for auto-derived, curated entry's own tags otherwise (default `["curated"]`)

If a future SKILL.md frontmatter adds `tags:` / `examples:` fields,
they propagate; today's loader (`pkg/skills/load.go`) only carries
`Name` + `Description`, so v1's auto-derived skills are minimal.

## Endpoints

| Endpoint | Method | Purpose | Auth |
|---|---|---|---|
| `/.well-known/agent-card.json` | GET | Return the binary's card | None (public by design) |

That's it. Single endpoint. Other attach endpoints are unaffected.

## Configuration

Three layers, applied in order: `.agents/agent-card.json` on disk →
CLI flag overrides for the simple scalar fields → library API for
embedders building their own daemon. Skills are file-or-library only;
the CLI deliberately doesn't try to express them.

`description` is the only field required to enable the endpoint —
`url` is derived from each incoming request (override via
`external_url` or `--agent-card-external-url` for the rare
canonical-URL case).

The bundled `core-agent` binary fans the
`.agents/config.json` field `agent.description` to **both** ADK's
`agent.WithDescription()` (used in the LLM system prompt) AND the
default for `attach.AgentCardConfig.Description`. Operators set the
agent's identity in one place; both surfaces pick it up. The card-
specific `description` field in `.agents/agent-card.json` and the
`--agent-card-description` flag override only the card, not the LLM
system prompt — use them when the public-facing wording differs from
what you tell the model.

Library embedders building their own daemon can populate either
layer directly: set `attach.AgentCardConfig.Description`
explicitly, or implement the optional `DescriptionProvider`
capability on the Registrant and the card handler falls through to
it at request time (the bundled `*agent.Agent` already does this
via `WithDescription`).

### File: `.agents/agent-card.json`

Auto-discovered next to `.agents/config.json` and `.agents/mcp.json`.
A missing file is not an error — the binary just runs without the
card endpoint unless flags or library config supply the required
fields. Same `version: 1` envelope as the other `.agents/` files:

```jsonc
{
  "version": 1,
  "name":            "core-agent",
  "description":     "Production-incident response agent for the platform fleet.",
  "agent_version":   "v2.2.0",
  "documentation_url": "https://example.internal/platform/runbooks/core-agent",
  "provider": {
    "organization": "Platform Team",
    "url":          "https://example.internal/platform"
  },
  "extra_skills": [
    {
      "id":          "rollback-deploy",
      "name":        "Rollback a deploy",
      "description": "Curated: revert a Cloud Deploy release to the previous revision.",
      "tags":        ["curated", "deploy"],
      "examples":    ["rollback the most recent payments-api release"]
    }
  ]
}
```

`agent_version` is named to avoid confusing it with the file's own
schema `version` field; everything else mirrors the wire-format field
names lower-snake-cased. `external_url` is optional — set it only to
override the per-request URL derivation with a canonical alternative.
An invalid file (malformed JSON, unknown `version`) is a startup
error — the binary refuses to come up rather than silently disabling
the endpoint. A file that only sets `extra_skills` (no `description`)
is also rejected: the file's purpose is the public-discovery surface,
not a skill-library side-channel.

### CLI flags

Override individual file fields. Provided primarily for operators who
mount the binary into an environment where checking a file alongside
the binary is awkward (Cloud Run with env-var-only config, Helm chart
values plumbed to argv, etc.):

```
--agent-card-description=<text>      override .description (required to enable endpoint)
--agent-card-external-url=<url>      override .external_url (optional; overrides the per-request URL derivation)
--agent-card-name=<text>             override .name
--agent-card-version=<text>          override .agent_version
--agent-card-provider-org=<text>     override .provider.organization
--agent-card-provider-url=<url>      override .provider.url
--agent-card-docs-url=<url>          override .documentation_url
--agent-card-config=<path>           load the JSON file from <path>
                                     instead of .agents/agent-card.json
```

No flag for skills. Operators who need curated skills set them in the
JSON file; that file is what gets reviewed in a code review and
versioned with the binary.

If neither file nor flags supply `description`, the endpoint is not
registered. No warning — same posture as not setting `--attach-listen`.

### Library API

```go
// In pkg/attach/server.go — addition to Options.
type Options struct {
    // ... existing fields ...

    // AgentCard, when its Description and ExternalURL are non-empty,
    // enables GET /.well-known/agent-card.json. Zero value disables
    // the endpoint entirely (404). Embedders building their own
    // daemon populate this directly; the bundled core-agent binary
    // fills it from .agents/agent-card.json + CLI overrides.
    AgentCard AgentCardConfig
}

// In pkg/attach/agentcard.go — new file.
type AgentCardConfig struct {
    Name             string
    Description      string             // required to enable the endpoint
    ExternalURL      string             // optional override; default URL is derived per-request from Host + X-Forwarded-*
    Version          string             // defaults to internal/version.Version
    Provider         AgentCardProvider
    DocumentationURL string
    ExtraSkills      []AgentCardSkill   // merged with AttachSkills()
}

type AgentCardProvider struct {
    Organization string
    URL          string
}

type AgentCardSkill struct {
    ID          string
    Name        string
    Description string
    Tags        []string // defaults to ["curated"] if empty
    Examples    []string
}
```

`NewServer` validates `AgentCardConfig`: a half-populated `Provider`
(only `Organization` or only `URL` set) is rejected per the A2A
spec; `ExtraSkills` entries missing `ID`/`Name`/`Description` are
rejected. Zero `AgentCardConfig` → endpoint disabled, no error.

## Implementation sketch

New files:
- `pkg/attach/agentcard.go` — types, builder, handler, wire-format
  struct tags (~250 LOC).
- `pkg/attach/agentcard_test.go` — wire-format pinning + auth-
  derivation matrix + skill-merge tests.
- `pkg/config/agentcardfile.go` — loader for `.agents/agent-card.json`,
  with the `version: 1` envelope check and the "description and
  external_url both required if any field is set" rule (~80 LOC).
- `pkg/config/agentcardfile_test.go` — file-format pinning + the
  error matrix for malformed files.

Touched files:
- `pkg/attach/server.go` — add `Options.AgentCard`, one route
  registration, validation.
- `cmd/core-agent/main.go` — load `.agents/agent-card.json`, apply
  CLI overrides, wire into `attach.Options.AgentCard`.

```
pkg/attach/
├── agentcard.go            ← new: types, builder, handler
├── agentcard_test.go       ← new: wire-format + matrix tests
├── server.go               ← +Options.AgentCard; +route registration
├── state.go                ← unchanged (existing SkillInfo type reused)
└── ...
pkg/config/
├── agentcardfile.go        ← new: .agents/agent-card.json loader
├── agentcardfile_test.go   ← new: file-format + error tests
└── ...
cmd/core-agent/
└── main.go                 ← +card flag set, +loader, +wiring
```

Skill-data path: the card handler calls `Registrant.AttachSkills()`
on each entry in the registry — the exact same accessor wired today
at `cmd/core-agent/main.go:491` for the in-TUI `/skills` view. No
new live-reload plumbing; the existing provider closure re-walks
`skills.LoadAll` on every call.

Dependencies: stdlib only. The card builder is pure Go with no new
imports beyond `encoding/json`, `net/http`, and existing
`internal/version`.

## Acceptance criteria

1. `pkg/attach/agentcard_test.go::TestCardWireFormat` pins the full
   JSON shape for a populated config + two `.agents/skills/`
   bundles. Diff-noisy if anything in the wire layout drifts.
2. `pkg/attach/agentcard_test.go::TestAuthSchemes` covers the four
   `(mtls?, bearer?)` combinations and asserts the emitted
   `authentication.schemes` array.
3. `pkg/attach/agentcard_test.go::TestSkillMerge` verifies (a)
   curated extras replace auto-derived skills on `id` collision,
   (b) result is sorted by `id`, (c) empty `tags` on a curated
   extra defaults to `["curated"]`.
4. `pkg/attach/agentcard_test.go::TestDisabledReturns404` confirms
   that a server constructed with the zero `AgentCardConfig`
   returns 404 on `/.well-known/agent-card.json`.
5. `pkg/attach/integration_test.go` extension: end-to-end fetch of
   the card through the real `http.Server`, including a registered
   agent whose `AttachSkills()` surfaces a fixture skill.
6. Validate with the official A2A spec: vendor the published JSON
   Schema export at `pkg/attach/testdata/agentcard.schema.json` and
   validate every emitted-card test fixture against it. Two distinct
   failure modes, each with its own clear signal:
   - **Our builder changes shape** → the wire-format pinning test
     (#1) diffs; the schema validation (#6) only fires if we drop a
     required field or change a type.
   - **The A2A spec changes upstream** → bump the vendored schema
     in a dedicated commit; the validation test surfaces any field
     in our card that no longer matches. The vendored file's commit
     log doubles as our "what changed in A2A and when" record.

   Refresh cadence: pull the schema on every minor-version bump of
   the published A2A spec. Vendored, not fetched at test time —
   tests stay hermetic and offline-runnable.

## Site docs and README

Per memory `feedback_update_site_docs`: user-visible changes need a
companion walk of `docs/site/content/docs/`. Add a "Discoverability
via agent card" page under `docs/site/content/docs/agent-design/`
that describes the flag set + the GCP Agent Registry registration
flow, and link it from the attach-mode page. README gets a one-liner
under the existing "Operator surface" bullet.

## Interaction with other PRs

- **Attach-mode v1 (PR #1):** depends on it — card endpoint registers
  on the same `attach.Server.mux`. Land card-PR after attach-mode v1.
- **Peer-registration (PR #2):** independent. Card-PR neither
  consumes nor produces peer entries. A hub binary serves its own
  card just like any other peer; the hub's `GET /peers` is the
  fleet-enumeration surface, distinct from per-peer discovery cards.
- **Future A2A transport (`attach/a2a.go`, deferred):** when (if) a
  concrete consumer arrives, that PR additively flips
  `capabilities.pushNotifications`/`stateTransitionHistory` to true
  if it implements those features, and replaces the card's `url`
  semantic from "the attach endpoint" to "the A2A JSON-RPC endpoint."
  Same card file, same handler, expanded honesty.

## Open questions

- **Should the card include a `documentationUrl` default** that
  points at the binary's own `/sessions` (a human-readable
  enumeration) if no explicit URL is given? Leaning no — the
  spec implies external human-readable docs, not the same machine
  endpoint. Operator can set the field if they have such a page.
- **Card content negotiation.** The spec doesn't mandate a media
  type for the response. We'll emit
  `Content-Type: application/json; charset=utf-8` and skip any
  negotiation. If a future Registry adopts a YAML or Protobuf
  variant, that's a new endpoint, not a content-type switch.
- **Caching headers.** v1 emits no `Cache-Control` / `ETag`. The
  card is cheap to rebuild and changes rarely; registries fetch it
  on registration, not on every search. Revisit if a real consumer
  asks.
