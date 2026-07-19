---
title: Agent card
weight: 9
---

`core-agent` can publish a [`/.well-known/agent-card.json`](https://agent2agent.info/docs/concepts/agentcard/) descriptor from its attach-mode listener so external discovery systems — most notably [Google Cloud Agent Registry](https://docs.cloud.google.com/agent-registry/register-agents) — can index the binary's name, description, and skills without a parallel registration channel. The endpoint is opt-in: a binary built without the card config behaves exactly like today's binary plus a card-shaped 404 at the well-known path.

The card is **discovery metadata, not a transport promise.** core-agent does not speak the A2A JSON-RPC transport — the `url` field on the card points at the attach listener, and an A2A client that POSTs JSON-RPC at it gets a 404. GCP Agent Registry, the actual consumer of this endpoint, only reads the card; it never drives the URL as A2A. If you need a non-A2A-shaped registration, GCP also supports `NO_SPEC` which doesn't require a card at all — see the Agent Registry [register-agents docs](https://docs.cloud.google.com/agent-registry/register-agents).

## Enabling the endpoint

Four layers, applied in order: `.agents/config.json`'s `agent.description` → `.agents/agent-card.json` on disk → CLI flag overrides → library API for embedders. The endpoint registers as soon as `description` is set from any layer.

`.agents/config.json`'s `agent.description` is the recommended source — set it once and it flows to **both** the card AND the ADK system prompt (via `agent.WithDescription`). The card-only layers (`.agents/agent-card.json` `description` field, `--agent-card-description` flag) override just the card, not the LLM system prompt — use them when the public-facing wording should differ from what you tell the model.

The card's `url` field is **derived per-request** from the caller's `Host` header (with `X-Forwarded-Proto` / `X-Forwarded-Host` taking precedence behind ingress) — the operator never has to know their own external address. By definition, the consumer fetching `/.well-known/agent-card.json` already knows the URL they used; the handler just echoes it back. Same convention as OIDC discovery and most other well-known endpoints. Set `external_url` only when you want to publish a canonical URL different from the fetch URL (rare — typically when serving on multiple addresses but wanting one canonical answer).

### `.agents/agent-card.json` (recommended)

Lives next to `.agents/config.json` and `.agents/mcp.json` (same `version: 1` envelope). Gets checked into the repo with the `.agents/skills/` bundles it describes:

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

A missing file is not an error — the binary just runs without the card endpoint unless flags supply `description`. A file that only sets `extra_skills` (no `description`) is rejected: the card is the public-discovery surface, not a skill-library side-channel. `external_url` is optional; set it only to override the per-request URL with a canonical alternative.

### CLI flags

Override individual fields. Useful when running under Cloud Run / Helm where mounting a file alongside the binary is awkward and per-pod values come in via env vars:

| Flag | Overrides |
|---|---|
| `--agent-card-description=<text>` | `description` — **required** to enable the endpoint |
| `--agent-card-external-url=<url>` | `external_url` — optional; overrides the per-request URL derivation with a canonical value |
| `--agent-card-name=<text>` | `name` (defaults to first registrant's AppName, else `core-agent`) |
| `--agent-card-version=<text>` | `agent_version` (defaults to the build version) |
| `--agent-card-provider-org=<text>` | `provider.organization` |
| `--agent-card-provider-url=<url>` | `provider.url` |
| `--agent-card-docs-url=<url>` | `documentation_url` |
| `--agent-card-config=<path>` | path to the JSON file (default: `.agents/agent-card.json`); `-` skips file loading entirely |

There is **no flag for curated skills.** Skill descriptions are multi-sentence and `tags`/`examples` are arrays — they live in the file (or the library API), reviewable in code review and versioned with the binary.

### Library API

Embedders building their own daemon populate `attach.Options.AgentCard` directly:

```go
srv, err := attach.NewServer(attach.Options{
    Registry: reg,
    Addr:     ":7777",
    AgentCard: attach.AgentCardConfig{
        Name:        "my-agent",
        Description: "Does the thing.",
        // ExternalURL omitted — handler derives url from each
        // request's Host / X-Forwarded-* headers.
        Provider:    attach.AgentCardProvider{Organization: "Acme", URL: "https://acme.example.com"},
        ExtraSkills: []attach.AgentCardSkill{
            {ID: "do-thing", Name: "Do the thing", Description: "Executes the thing."},
        },
    },
})
```

## Where the skills come from

Two sources, merged. Curated wins on `id` collision:

1. **Auto-derived** from every `.agents/skills/` bundle the binary loaded at startup (the same set surfaced through the in-TUI `/skills` view). Each gets `tags: ["skill"]` and lifts `name` / `description` from the SKILL.md frontmatter. Internal tools (`report_done`, MCP tools, the inbox primitives, etc.) are **never** auto-included — they're implementation detail, not skills.
2. **Curated extras** from `extra_skills` in the file (or `ExtraSkills` in the library API). Default `tags` are `["curated"]` if unset.

Newly-dropped SKILL.md bundles surface on the next card fetch without restart — the card handler re-reads the skills snapshot per request, same as `GET /sessions/<id>/skills`.

## Auth and security

The card endpoint is **always unauthenticated**, even when the rest of the attach listener requires mTLS + bearer auth — public discoverability is the point. The card's `securitySchemes` / `security` fields describe the auth required for *other* endpoints, so a discovery system can tell what credentials it would need to actually drive the agent:

| Attach `Auth` config | Emitted `securitySchemes` | Emitted `security` |
|---|---|---|
| (none) | omitted | omitted |
| `BearerToken` set | `{bearer: {type: "http", scheme: "Bearer"}}` | `[{bearer: []}]` |
| `ClientCAFile` set | `{mtls: {type: "mutualTLS"}}` | `[{mtls: []}]` |
| both set | both schemes | `[{bearer: [], mtls: []}]` (AND — both required) |

## Registering with Google Cloud Agent Registry

Once the card serves cleanly:

```bash
# Verify the card.
curl https://my-agent.example.com:7777/.well-known/agent-card.json | jq

# Register as A2A_AGENT_CARD (gets the skills indexed for keyword search).
gcloud alpha agent-registry agents create my-agent \
    --location=us-central1 \
    --agent-spec-url=https://my-agent.example.com:7777/.well-known/agent-card.json
```

The Registry fetches `/.well-known/agent-card.json` once at registration; subsequent operator searches hit the Registry's index, not the binary. If your `external_url` is publicly resolvable, you're done. If it lives behind a private VPC, see the Registry docs on [setting up Private Service Connect](https://docs.cloud.google.com/agent-registry/setup) for indexing reach.

## Validation

The card builder validates every emission against the vendored A2A v0.3.0 JSON Schema (`pkg/attach/testdata/agentcard.schema.json`) in `TestAgentCardSchemaValidation`. The vendored file gets bumped in a dedicated commit each time the A2A spec issues a minor-version bump — see `pkg/attach/testdata/AGENTCARD_SCHEMA_NOTICE.md` for the refresh policy.

## Design background

See [`docs/agent-card-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/agent-card-design.md) for the rationale and trade-offs. Future work — full A2A JSON-RPC transport (`attach/a2a.go`) — stays deferred per [`docs/attach-mode-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/attach-mode-design.md) until a concrete consumer surfaces.
