---
title: MCP servers
---


`core-agent` integrates with [Model Context Protocol](https://modelcontextprotocol.io) servers via ADK's `mcptoolset`. Declare servers in `.agents/mcp.json` and/or `~/.agents/mcp.json`; `core-agent` spawns or connects to them at startup, namespaces their tools, and routes every tool call through the [permission gate](/concepts/permissions/).

### Where `mcp.json` is read from

At startup the loader reads `mcp.json` from two paths and merges the results:

| Precedence | Path | Scope |
|---|---|---|
| 1 (highest) | `<agentsDir>/mcp.json` (typically `<project>/.agents/mcp.json`) | Project — checked in to the repo |
| 2 | `~/.agents/mcp.json` | Portable user-scope, applies to every project |

Server entries merge by name: on collision, the project entry wins. Non-server fields (`agentic_wrap`, `agentic_wrap_threshold`, `agentic_wrap_llm`, `agentic_wrap_model`) take the first explicitly-set value walking the precedence list. Missing files at either path are silently treated as "no servers here."

Use `~/.agents/mcp.json` for servers that should follow you into every project (e.g. Linear, Slack, GitHub); use `.agents/mcp.json` for servers specific to one repo. Nothing prevents both — a project-local entry with the same name as a user-scope entry will win, letting you override a global server per project.

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
| `tools` | optional | Allowlist of tool names, as the **server** spells them. When set, only these are registered; the rest of the catalog is dropped before the model ever hears of it. Absent means the whole catalog. See [Mounting part of a server](#mounting-part-of-a-server) below. |
| `read_only` | optional | Declares that nothing this server exposes can mutate state. Covers the tools the server did not annotate itself. See [Read-only tools and servers](#read-only-tools-and-servers) below. Default `false`. |
| `agentic_never` | optional | Opts this server out of the [digest wrap](#structural-digest-wrap---no-mcp-digest). Default `false`. |
| `tool_notes` | optional | Map of tool name → text appended to that tool's description, in front of the model at the call site. See [Tool notes](#tool-notes) below. |

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

The same `${env:NAME}` syntax works in `AGENTS.md`, skill files, and skill references when the bundle ships a `.agents/env.yaml` manifest — see [Environment variables (env.yaml)](/concepts/env-manifest/) for boot-time required-var validation, sensitive-value handling, and drift diagnostics.

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

## Mounting part of a server

Mounting a server is otherwise all-or-nothing: every tool it advertises becomes a tool the model can see and call. `tools` narrows that to a list you write.

```json
{
  "version": 1,
  "servers": {
    "gke": {
      "transport": "http",
      "url":   "https://container.googleapis.com/mcp",
      "tools": ["get_k8s_resource", "list_k8s_events", "get_k8s_logs", "patch_k8s_resource"]
    }
  }
}
```

That endpoint serves 23 tools. The agent above sees four.

Names are the ones the **server** publishes — no `gke_` prefix, same as `tool_notes`, because the prefix is this server's own key in the enclosing object. Filtering happens before the namespace wrap, so an unlisted tool is never registered, never declared to the model, never gated, and never appears in `/tools` or the startup summary. Absent (or `[]`) means the whole catalog, so nothing changes for a config that doesn't set it.

The reason to reach for this is that **a tool in the catalog is a promise.** If you want the full GKE endpoint for one mutating verb under an approval gate, the alternative is advertising `delete_k8s_resource` and the cluster-lifecycle verbs alongside it and relying on `deny` patterns to refuse them — which spends a turn discovering a boundary the catalog should have described, and splits the decision across two files (`mcp.json` mounts the server, `config.json` denies the tools) either of which can be edited without the other noticing.

Two things it is not:

- **Not a security boundary.** It is a client-side filter on a catalog; the server decides what a call is allowed to do. Scope the credential — IAM roles, RBAC, a read-only endpoint — and use `tools` so the model isn't told a different story from the one the server will enforce.
- **Not a permission decision.** The gate, `deny` patterns and `ask` mode all still apply to the tools that survive the list. This is about what gets registered, not what gets allowed.

An entry that names no tool the server exposes is reported at startup — `core-agent: mcp: <server>: tools allowlist names 1 tool(s) this server does not expose: …`, with the exposed names listed. The failure mode is otherwise silence: the tool is simply missing, and nothing distinguishes a typo from a server that never had it. Note that a `tool_notes` key for a tool the allowlist excluded will warn too, since the note now describes nothing.

---

## Read-only tools and servers

The runtime sorts every tool into one of two dispatch classes, read-only or mutating, and three behaviours hang off the answer. For MCP tools it comes from two places.

### What the server says

The MCP protocol has a per-tool `readOnlyHint` annotation — "if true, the tool does not modify its environment" — and `core-agent` reads it. A server that annotates its tools needs no configuration: the 14 read tools on `container.googleapis.com/mcp` classify read-only and the mutating ones (`apply_k8s_manifest`, `patch_k8s_resource`, `delete_k8s_resource`, the cluster-lifecycle verbs) classify mutating, per tool, with nothing in `mcp.json` saying so.

A tool that ships an annotation block counts as having answered even if the block omits `readOnlyHint`, because the spec's default for a published block is `false`. A tool with no annotations at all has said nothing, and falls through to the server declaration below.

### What the operator says

Plenty of servers annotate nothing. Several providers also publish a read-only endpoint alongside the full one — the GKE MCP server's `container.googleapis.com/mcp/read-only`, say — which `core-agent` can't tell apart by looking. `read_only: true` is how you say what you already know:

```json
{
  "version": 1,
  "servers": {
    "gke": {
      "transport": "http",
      "url":       "https://container.googleapis.com/mcp/read-only",
      "read_only": true
    }
  }
}
```

Every *unannotated* tool from that server then classifies read-only, which changes three things:

- **`wait_and_verify` can poll it.** The waiter refuses to poll anything classified mutating, because polling repeats the call up to `max_attempts` times. Before `read_only`, the only way to poll an MCP tool was to name each one in `tools.wait_and_verify.poll_allow`; a whole read-only server no longer needs that list.
- **Its calls run concurrently.** Mutating tools serialize on a per-agent lock so two edits can't interleave. Reads don't need it, so a batch of `get`/`list` calls in one turn now overlaps.
- **Plan-first mode stops treating a `list` as a mutation.** Under `permissions.require_plan_artifact`, mutating calls are denied until `record_plan` runs. The gate sees only the namespace (`mcp`) and never the underlying tool name, so before this every MCP call was gated — including the research the model needs to *write* the plan. Read-only calls are now exempt, the same way built-in `grep` and `read_file` always have been.

What it does **not** change: allow/deny patterns, permission mode, and prompting all behave identically. A read-only MCP tool in `ask` mode still asks, and a `deny` pattern still denies it. `read_only` is a dispatch-class declaration, not an allowlist.

Two guardrails worth knowing:

- **Per-tool beats per-server.** Where a server annotates a tool's `readOnlyHint`, that answer wins for that tool, in both directions — a server-level declaration can't launder a tool that says it mutates, and it doesn't need to repeat one that says it reads. `read_only: true` on a fully annotated endpoint is therefore inert, not a second opinion.
- **It is an operator assertion, not a server claim.** Nothing verifies it; you are vouching for an endpoint you chose. That is exactly why it carries enough authority to relax plan-first — it comes from the same config that turned plan-first on. Point it at a read/write URL and you have disabled a safety property by hand.

---

## Tool notes

A tool's description is written by whoever wrote the server, for nobody in particular. The thing an agent most needs to know at the call site is often a property of *this* deployment — which of an enum's values are useful here, which argument is expensive against your cluster, what the field you actually want is called. `tool_notes` lets the operator append that to the description the model reads.

```json
{
  "version": 1,
  "servers": {
    "gke": {
      "transport": "http",
      "url":       "https://container.googleapis.com/mcp/read-only",
      "read_only": true,
      "tool_notes": {
        "get_k8s_resource": "outputFormat selects FIDELITY, not rendering. TABLE, WIDE, NAME and CUSTOM_COLUMNS return columns only and carry NOTHING under spec. Only YAML and JSON return the whole object."
      }
    }
  }
}
```

The keys are the names the **server** exposes, with no namespace prefix — `get_k8s_resource`, not `gke_get_k8s_resource`. The prefix is the server's own key in this file, so repeating it in every entry would be noise that goes stale the moment you rename the server.

The note is **appended** to the server's description, separated by a blank line, never substituted for it: the server's own text is the only account of what the tool does, and a note is by definition something it left out. It reaches both the declaration the model is sent and the description `/tools` and `/mcp` print, so what you read in a listing is what the model read.

A key that names no tool the server exposes is reported at startup — `core-agent: mcp: <server>: tool_notes names 1 tool(s) this server does not expose: …`, with the exposed names listed so you can spot the typo. Without that, a misspelled key would fail by doing nothing, which is the exact failure mode a note is usually written to prevent.

Two things it deliberately cannot do:

- **It cannot change what a call is allowed to do.** Unlike `read_only`, the text reaches the declaration and nothing else — no dispatch class, no permission decision.
- **It is not a substitute for a persona or a skill.** Use it for facts that only matter while choosing arguments for one specific tool. Anything broader belongs in `AGENTS.md`, where it isn't repeated in the tool list on every single request.

Like `read_only`, this is an operator assertion rather than something the server said about itself, and it carries the same authority: it is written in the same file, by the same person, as the decision to mount the server at all.

---

## Permission gating

If you've configured a [permission gate](/concepts/permissions/), every MCP tool call goes through it under the `mcp` namespace. So an allowlist entry like:

```json
{
  "permissions": {
    "allow": ["mcp:filesystem_read_file"]
  }
}
```

…would allowlist the namespaced filesystem-server read_file specifically, without granting any other MCP tool. Pattern matching is the same as for built-in tools — see the [Permissions page](/concepts/permissions/#pattern-grammar).

The permission detail string surfaced in prompts is `<tool_name> <json-args>` (truncated at 200 chars), so users get context about what's being asked. Skip gating entirely by configuring `permissions.mode: yolo` (the bash denylist is still applied for any `bash` tool, but MCP tools are not subject to it).

---

## Lifecycle and failure modes

- **Parallel startup** — every server is spawned/connected concurrently. Slow servers don't block the rest.
- **Failed servers don't kill the run** — a stdio server whose binary doesn't exist, or an HTTP server that returns 404, surfaces with `Status: error` and an `Err` field. The agent continues with whichever servers came up cleanly.
- **Per-server tool listing** — at startup, `core-agent` calls `Tools(ctx)` on each server's toolset to build the list of available tools. This catches non-cooperative servers early.
- **Graceful shutdown** — stdio child processes get `SIGTERM`, then `SIGKILL` after 3 seconds if they haven't exited. HTTP transports have no process to kill.

The host (your binary or the bundled `cmd/core-agent`) is responsible for surfacing per-server status to the user — see [Library API](/embed/api/#mcp-status) for how.

---

## Elicitation

If an MCP server tries to elicit input from the user (the protocol's `elicit` request), `core-agent` needs an `ElicitorFn` to bridge that into your UI. The bundled CLI doesn't currently wire one up, so:

- **Headless mode (default)** — every elicitation request is automatically declined with a one-line notice on stderr. Calls that depend on elicitation will fail gracefully rather than hang forever.
- **Custom hosts** — pass an `ElicitorFn` to `mcp.Build()` that opens a prompt and blocks on user input. See [Library API](/embed/api/#mcp-elicitation).

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

Since v2.9 the same wrap also covers four built-ins — `read_many_files`, `grep`, `glob`, `list_dir` — so a 54 KB filesystem walk and a 54 KB MCP response cost the same. See [Tools → Digested survey tools](/concepts/tools/#digested-survey-tools-v29) for which built-ins are in the set and why `read_file` and `bash` are not.

### Configuration

- **CLI kill switch**: `--no-mcp-digest` disables the wrap layer entirely — both halves, MCP and built-in. `retrieve_raw` is not registered.
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
- **Cost profile**: the subagent pays a small-tier bill (e.g. `gemini-3.5-flash-lite`: ~$0.30/M input, $0.03/M cached, $2.50/M output). Break-even after one subsequent turn where the digest replaces the raw response in history resend.
- **Attribution**: the subagent's spend is billed to the session whose tool call triggered the digest — it appears in that session's `/stats` turns and counts against its `--cost-ceiling`. In multi-session mode each session carries its own digests; before v2.9 they all landed on the primary session's ledger.

```json
{
  "version": 1,
  "agentic_wrap": true,
  "agentic_wrap_llm": true,
  "agentic_wrap_model": "gemini-3.5-flash-lite",
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
