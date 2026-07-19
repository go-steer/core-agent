---
title: MCP servers
weight: 5
---


`core-agent` integrates with [Model Context Protocol](https://modelcontextprotocol.io) servers via ADK's `mcptoolset`. Declare servers in `.agents/mcp.json`; `core-agent` spawns or connects to them at startup, namespaces their tools, and routes every tool call through the [permission gate]({{< relref "/docs/reference/permissions.md" >}}).

---

## `mcp.json` schema

```json
{
  "version": 1,
  "servers": {
    "filesystem": {
      "transport": "stdio",
      "command":   "mcp-server-filesystem",
      "args":      ["--root", "/tmp"],
      "env":       { "LOG_LEVEL": "info" }
    },
    "github": {
      "transport": "http",
      "url":       "https://api.githubcopilot.com/mcp/",
      "headers":   { "Authorization": "Bearer ${env:GITHUB_TOKEN}" }
    }
  }
}
```

Top-level fields:

| Field | Type | Notes |
|---|---|---|
| `version` | int | Schema version. Currently `1`. |
| `servers` | object | Map of `name` → `ServerSpec`. The `name` becomes the tool namespace prefix. |

### `ServerSpec`

| Field | Required when | Notes |
|---|---|---|
| `transport` | always | `"stdio"` or `"http"`. |
| `command` | `transport: stdio` | Executable to spawn. |
| `args` | optional, stdio | Argv tail. |
| `env` | optional, stdio | Extra env vars; layered on top of the parent env. Values support `${env:NAME}` interpolation. |
| `url` | `transport: http` | Streamable HTTP endpoint. |
| `headers` | optional, http | Custom headers. Values support `${env:NAME}` interpolation — useful for `Authorization: Bearer ${env:TOKEN}`. |
| `auth` | optional, http | Selects an authentication strategy that manages tokens for you instead of static headers. See [Authentication](#authentication) below. |

Validation runs at config load time. A server that mixes transports (e.g. both `command` and `url`) is rejected with a clear error before the agent starts.

---

## Env-var interpolation

Both `env` values (stdio) and `headers` values (http) support `${env:NAME}` placeholders. They expand at server-start time using the parent process's env. Unset names expand to the empty string — same semantics as shell `$NAME`.

```json
{
  "servers": {
    "linear": {
      "transport": "http",
      "url":       "https://mcp.linear.app/mcp",
      "headers":   { "Authorization": "Bearer ${env:LINEAR_TOKEN}" }
    }
  }
}
```

This keeps secrets out of `mcp.json` (which you can commit) and in your local env (which you don't).

The same `${env:NAME}` syntax works in `AGENTS.md`, skill files, and skill references when the bundle ships a `.agents/env.yaml` manifest — see the [env-manifest reference]({{< relref "/docs/reference/env-manifest.md" >}}) for boot-time required-var validation, sensitive-value handling, and drift diagnostics.

---

## Authentication

Static `headers` work when you already have a token. For servers that expect tokens you'd otherwise have to mint, cache, and refresh yourself, set `auth` instead and `core-agent` does the token lifecycle for you.

### `auth.google_oauth` — Google OAuth access tokens (ADC)

Authenticates outbound MCP requests with a Google OAuth 2.0 access token sourced from [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials). Suitable for Google-hosted API endpoints that accept scoped access tokens — the [GKE remote MCP server](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/use-gke-mcp) at `https://container.googleapis.com/mcp` is the canonical first target.

```json
{
  "version": 1,
  "servers": {
    "gke": {
      "transport": "http",
      "url":       "https://container.googleapis.com/mcp",
      "auth": {
        "google_oauth": {
          "scopes": ["https://www.googleapis.com/auth/container.read-only"]
        }
      }
    }
  }
}
```

| Field | Notes |
|---|---|
| `scopes` | Required. OAuth 2.0 scopes to request on the access token. No default — each server documents its own minimum, and an implicit broad default (e.g. `cloud-platform`) would grant more privilege than necessary. |

**What happens at startup:** `core-agent` calls `google.FindDefaultCredentials(ctx, scopes...)` and pre-fetches one token so any ADC misconfiguration (no credentials, missing scope grants, unreachable metadata server) surfaces at server-init time, not on the first tool call. The returned `oauth2.TokenSource` then caches and refreshes the token transparently for every subsequent MCP HTTP request.

**Required setup** for the GKE example above:

```bash
# Local dev: one-time interactive ADC.
gcloud auth application-default login

# Caller (user or service account) needs both IAM roles. The first
# grants the right to call MCP tool endpoints at all; the second is
# the resource-viewer role the GKE MCP server's tools enforce.
PROJECT=your-gcp-project
PRINCIPAL="user:$(gcloud config get-value account)"
gcloud projects add-iam-policy-binding "$PROJECT" \
  --member="$PRINCIPAL" --role=roles/mcp.toolUser
gcloud projects add-iam-policy-binding "$PROJECT" \
  --member="$PRINCIPAL" --role=roles/container.clusterViewer
```

In production, ADC discovers credentials from `GOOGLE_APPLICATION_CREDENTIALS` (a service-account key file path) or the [GCE/GKE/Cloud Run metadata server](https://cloud.google.com/docs/authentication/application-default-credentials#attached-sa) — no code change.

### Header precedence

When both `auth` and `headers` are set on the same HTTP server, the auth layer wraps innermost. Net effect: the auth strategy's `Authorization` header **always wins** over any `Authorization` you put in `headers`. Non-conflicting static headers (e.g. `X-Custom-Trace-Id: ...`) pass through unchanged. This is intentional: the auth declaration of intent should not be silently overridable by a stray header.

### Other strategies

Audience-scoped ID-token auth (Cloud Run / IAP / custom-OIDC services) is not yet supported. The `AuthSpec` shape leaves room for a sibling `google_id_token` field — file a request when you need it.

---

## Tool namespacing

`core-agent` prefixes every tool from server `<name>` with `<sanitized_name>_`. So an MCP filesystem server's `read_file` becomes `filesystem_read_file`. This:

- Prevents collisions with consumer-provided tools that have the same base name
- Keeps function names within Gemini's `[A-Za-z0-9_]{1,64}` constraint (a `.` separator wouldn't pass)

Sanitization rule: keep `[A-Za-z0-9_]`, replace everything else with `_`. So `my-server` → `my_server_<tool>`, `file.system` → `file_system_<tool>`.

---

## Permission gating

If you've configured a [permission gate]({{< relref "/docs/reference/permissions.md" >}}), every MCP tool call goes through it under the `mcp` namespace. So an allowlist entry like:

```json
{
  "permissions": {
    "allow": ["mcp:filesystem_read_file"]
  }
}
```

…would allowlist the namespaced filesystem-server read_file specifically, without granting any other MCP tool. Pattern matching is the same as for built-in tools — see the [Permissions page]({{< relref "/docs/reference/permissions.md" >}}#pattern-grammar).

The permission detail string surfaced in prompts is `<tool_name> <json-args>` (truncated at 200 chars), so users get context about what's being asked. Skip gating entirely by configuring `permissions.mode: yolo` (the bash denylist is still applied for any `bash` tool, but MCP tools are not subject to it).

---

## Lifecycle and failure modes

- **Parallel startup** — every server is spawned/connected concurrently. Slow servers don't block the rest.
- **Failed servers don't kill the run** — a stdio server whose binary doesn't exist, or an HTTP server that returns 404, surfaces with `Status: error` and an `Err` field. The agent continues with whichever servers came up cleanly.
- **Per-server tool listing** — at startup, `core-agent` calls `Tools(ctx)` on each server's toolset to build the list of available tools. This catches non-cooperative servers early.
- **Graceful shutdown** — stdio child processes get `SIGTERM`, then `SIGKILL` after 3 seconds if they haven't exited. HTTP transports have no process to kill.

The host (your binary or the bundled `cmd/core-agent`) is responsible for surfacing per-server status to the user — see [Library API]({{< relref "/docs/library/api.md" >}}#mcp-status) for how.

---

## Elicitation

If an MCP server tries to elicit input from the user (the protocol's `elicit` request), `core-agent` needs an `ElicitorFn` to bridge that into your UI. The bundled CLI doesn't currently wire one up, so:

- **Headless mode (default)** — every elicitation request is automatically declined with a one-line notice on stderr. Calls that depend on elicitation will fail gracefully rather than hang forever.
- **Custom hosts** — pass an `ElicitorFn` to `mcp.Build()` that opens a prompt and blocks on user input. See [Library API]({{< relref "/docs/library/api.md" >}}#mcp-elicitation).

---

## Reload

`core-agent` doesn't currently watch `mcp.json` for changes — to pick up an edit, restart the process. Each `Server` exposes a `Close()` method that terminates its child process; if you build a `/reload` slash command in your host, call `Close()` on every old server before re-running `mcp.Build()`.

---

## Structural digest wrap (`--no-mcp-digest`)

Since v2.7.0-dev.4, MCP tool responses are routed through the [`pkg/digest`](https://github.com/go-steer/core-agent/tree/main/pkg/digest) structural pruner before reaching the model's context. The wrapper preserves identifier-shaped keys (`id`, `name`, `status`, `apiVersion`, `*url*`, `*_id`, …), truncates long strings past 500 chars, collapses arrays over 20 items into a head-plus-tail summary, and caps recursion at depth 8. Prose-shaped responses take a bounded passthrough (max 64 KiB) — unless the LLM subagent second-chance path is enabled (see below).

The model sees a synthetic tool response:

```json
{
  "digest": "...compressed payload...",
  "raw_bytes": 12345,
  "method": "structural_json",
  "call_id": "toolcall-abc"
}
```

The `call_id` is the escape hatch — the model can pass it to the built-in `retrieve_raw(call_id)` tool to fetch the un-digested payload when a digest looks suspicious. `retrieve_raw` is registered whenever the wrap is on AND a Store is wired (which happens automatically when `--session-db` is on).

### Configuration

- **CLI kill switch**: `--no-mcp-digest` disables the wrap layer entirely. `retrieve_raw` is not registered.
- **Per-project**: `agentic_wrap: false` (top-level in `.agents/mcp.json`) has the same effect as the CLI kill switch, scoped to that project.
- **Per-project threshold**: `agentic_wrap_threshold: 8000` (bytes). Responses below this bypass the router. Default 8000 (~2000 tokens).
- **Per-server escape hatch**: `agentic_never: true` on any `ServerSpec` opts that server out of digesting. Use for debug-sensitive or known-tiny servers where the digest hurts more than it helps.

```json
{
  "version": 1,
  "agentic_wrap": true,
  "agentic_wrap_threshold": 8000,
  "servers": {
    "gke": { "transport": "stdio", "command": "gke-mcp" },
    "debug-inspector": {
      "transport": "stdio",
      "command": "raw-mcp",
      "agentic_never": true
    }
  }
}
```

### LLM subagent second-chance (`--mcp-agentic-wrap-llm`)

Opt-in. When the structural pruner can't reduce a response below the threshold — prose-shaped payloads, malformed JSON, or JSON whose keys are all preserved by the pruner — the LLM subagent runs a small-tier model over the original payload and returns a compressed summary. Full design in [`docs/agentic-mcp-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/agentic-mcp-design.md).

- **CLI**: `--mcp-agentic-wrap-llm=true` enables the path. `--mcp-agentic-wrap-model=<id>` overrides the subagent model just for MCP (falls through to `--agentic-small-model` → provider default → parent-inherit).
- **Per-project**: `agentic_wrap_llm: true` and `agentic_wrap_model: "<id>"` in `.agents/mcp.json` mirror the CLI flags. Either source enabling turns it on.
- **Cost profile**: the subagent pays a small-tier bill (e.g. Flash: ~$0.075/M input / $0.30/M output). Break-even after one subsequent turn where the digest replaces the raw response in history resend.

```json
{
  "version": 1,
  "agentic_wrap": true,
  "agentic_wrap_llm": true,
  "agentic_wrap_model": "gemini-2.5-flash",
  "servers": {
    "gke": { "transport": "stdio", "command": "gke-mcp" }
  }
}
```

When the fallback fires the tool response `method` field is `llm_fallback` (structural cases stay `structural_json`).

### Telemetry

`GET /sessions/<id>/usage` returns a `digest_methods` block with per-method call counts + cumulative bytes saved so operators can tell which pruner path dominates:

```json
{
  "digest_methods": {
    "counts": {"structural_json": 42, "passthrough": 3, "llm_fallback": 5},
    "bytes_saved": {"structural_json": 1234567, "passthrough": 0, "llm_fallback": 89012}
  }
}
```

`GET /sessions/<id>/context` returns a `digest_savings` block with the session-cumulative view — structural vs. agentic call counts, parent-side tokens saved, subagent input/output tokens, and subagent cost. Also surfaced inline in the `/context` slash's "Digest savings" section, which computes the parent-side dollar savings from the current pricing catalog.

Design: [`docs/digest-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/digest-design.md). Tracking issue: [#128](https://github.com/go-steer/core-agent/issues/128).
