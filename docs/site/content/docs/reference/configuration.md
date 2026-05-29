---
title: Configuration
weight: 4
---


## The `.agents/` directory

`core-agent` walks up from the working directory looking for a folder named `.agents/`, analogous to how `git` looks for `.git`. The first match wins. Everything `core-agent` reads or writes lives there:

```
.agents/
├── config.json          # this file — provider, model, permissions, scope, telemetry, etc.
├── mcp.json             # MCP server declarations (see MCP page)
├── skills/              # SKILL.md bundles (see Skills page)
└── sessions/            # one-shot transcripts; auto-written, safe to .gitignore
```

You don't have to create `.agents/` — without it, `core-agent` runs with built-in defaults and skips the project-specific bits (no transcripts, no MCP, no skills). It's required only when you want to customize.

---

## `config.json` schema

Top-level shape, with all fields optional except `version` and `model.name`:

```json
{
  "version": 1,
  "model": { ... },
  "permissions": { ... },
  "path_scope": { ... },
  "agent": { ... },
  "tool_output": { ... },
  "otel": { ... },
  "url_scope": { ... },
  "attach": { ... }
}
```

`version` must be `1`. Other versions are rejected with a clear upgrade message — the schema is bumped only on breaking changes.

A minimal viable config:

```json
{
  "version": 1,
  "model": {
    "provider": "anthropic",
    "name": "claude-opus-4-7"
  }
}
```

---

## `model`

Selects the LLM backend.

| Field | Type | Default | Notes |
|---|---|---|---|
| `provider` | string | `""` (auto-detect) | One of `gemini`, `vertex`, `anthropic`, `anthropic-vertex`. Empty = auto-detect from env. |
| `name` | string | `gemini-3.1-pro-preview-customtools` | Model ID. **Required.** For Gemini, version 3.0 or later is required when using the default tool suite — see [Providers → Gemini 3.0+ required]({{< relref "providers.md#gemini-30-required-when-combining-built-ins-with-function-tools" >}}). The default uses the `-customtools` variant, which is fine-tuned to prefer developer-defined tools over raw bash; same price, same context window. Override with the un-tuned `gemini-3.1-pro-preview` if you need behavior-baseline comparisons. |
| `api_key` | string | `""` | Inline key for `provider: gemini`. Usually unset; read from `GOOGLE_API_KEY` / `GEMINI_API_KEY` at runtime. |
| `vertex` | object | `null` | GCP project + region. Required when `provider: vertex`. |
| `vertex.project` | string | — | GCP project ID. |
| `vertex.location` | string | — | GCP region (e.g. `us-central1`). |
| `anthropic` | object | `null` | Claude-specific settings. |
| `anthropic.api_key` | string | `""` | Inline Anthropic key. Usually read from `ANTHROPIC_API_KEY`. |
| `anthropic.vertex` | object | `null` | When `provider: anthropic-vertex`, holds project + region. |
| `anthropic.vertex.project` | string | — | GCP project ID for Vertex Anthropic. Falls back to `ANTHROPIC_VERTEX_PROJECT_ID` then `GOOGLE_CLOUD_PROJECT`. |
| `anthropic.vertex.location` | string | — | Region (e.g. `us-east5`). Falls back to `CLOUD_ML_REGION` then `GOOGLE_CLOUD_LOCATION`. |
| `pricing` | map | `{}` | Per-model rate overrides keyed by model name (case-insensitive). Survives `/model` switches mid-session — every model the operator routes to can carry its own rates. |
| `pricing.<model>.input_per_mtok` | float | — | USD per 1M input tokens for `<model>`. |
| `pricing.<model>.output_per_mtok` | float | — | USD per 1M output tokens for `<model>`. |

Pricing resolves through a layered chain: this `model.pricing` map → `.agents/pricing.json` (project-local) → `~/.core-agent/pricing.json` (user-global; auto-fetched + manual sections) → compiled-in fallback → longest-prefix match → "$—" (rate unknown).

Example:

```json
{
  "model": {
    "name": "gemini-3.1-pro-preview",
    "pricing": {
      "gemini-3.1-pro-preview":     {"input_per_mtok": 1.25, "output_per_mtok": 5.00},
      "claude-opus-4-7":            {"input_per_mtok": 15.0, "output_per_mtok": 75.0},
      "internal-fine-tuned-v3":     {"input_per_mtok": 0.50, "output_per_mtok": 2.00}
    }
  }
}
```

See [Providers]({{< relref "providers.md" >}}) for full details on each backend.

---

## `permissions`

Configures the permission gate that consults every tool call. See [Permissions]({{< relref "permissions.md" >}}) for the full pattern grammar.

| Field | Type | Default | Notes |
|---|---|---|---|
| `mode` | string | `ask` | One of `ask`, `allow`, `yolo`. |
| `allow` | string[] | `[]` | Allowlist patterns. Format: `<tool>:<glob>` or `<glob>`. |
| `deny` | string[] | `[]` | Denylist patterns. Always wins over allow. |

Example:

```json
{
  "permissions": {
    "mode": "ask",
    "allow": ["bash:git status", "bash:git log*", "read_file:internal/**"],
    "deny":  ["bash:sudo *"]
  }
}
```

### Interactive prompts

In `ask` mode the bundled CLI (`core-agent`) prompts on stderr whenever a tool call needs approval. The prompt looks like:

```text
core-agent (permissions): bash wants to run:
  rm -rf /tmp/foo
[y]es once · [s]ession · session-[t]ool · [a]lways · [N]o (default): 
```

Decision keys (case-insensitive, single character + enter):

| Key | Effect |
|---|---|
| `y` | Allow once. Next identical call asks again. |
| `s` | Allow this exact request for the rest of the session. |
| `t` | Allow every call to this tool for the rest of the session. |
| `a` | Allow always. Persists an entry to `.agents/config.json`'s `permissions.allow`. |
| `n` or bare enter | Deny. |

The prompter is auto-wired when stdin is a TTY. Non-TTY callers (piped stdin, CI, `nohup`) get `ErrNoPrompter`-wrapped errors that point at the bypass options below — they don't hang waiting for a non-existent user.

### `--yolo` (CLI flag)

`--yolo` forces the gate into `yolo` mode regardless of `config.permissions.mode`. Equivalent to setting `permissions.mode: "yolo"` in config; takes precedence at the call site so you don't have to edit config to unblock a one-off scripted run. Library callers achieve the same with `permissions.Options{Mode: permissions.ModeYolo}`.

### Background subagent prompts (v1.2.0+)

When background subagents are enabled (default; `--no-background-agents` disables them) and one of them triggers a permission prompt in `ask` mode, the heading is prefixed with `[<subagent-name>]` so you know which agent is asking. Concurrent prompts from different subagents are serialized through a mutex — they queue rather than race for stdin.

The subagent inherits the parent's gate wholesale: the same allow/deny lists, the same mode, the same session-level approvals. If you approve `session-tool: bash` while a subagent is asking, every subagent gets the grant for the rest of the session (sibling included). Bounded-subset grants where the parent's model arbitrates out-of-subset requests is deferred to v1.3+.

**Teaching the model to use the spawn tools.** Just registering the tools isn't always enough — most models default to doing things synchronously. Drop a short paragraph into your project's `AGENTS.md` (or pass via `agent.WithInstruction`) describing when background subagents are appropriate (monitoring, fan-out, long bounded delegations). See [Library API → Background subagents → Prompting patterns]({{< relref "library-api.md#prompting-patterns" >}}) for a ready-to-paste system instruction.

### REPL keybindings (v1.3.0+)

The bundled CLI's REPL recognizes Claude Code-style mid-turn interrupts:

| Key | Effect |
|---|---|
| **ESC** | Cancel the current turn. Conversation context is preserved; you can type a redirect. |
| **Ctrl+C** (single) | Same as ESC. Prints a hint that pressing again exits. |
| **Ctrl+C** twice within 1 s | Exit the REPL cleanly. |
| **Ctrl+D** | EOF — exit the REPL. |

Auto-enabled when stdin is a TTY. Disabled silently for piped / non-TTY use (Ctrl+C falls back to the legacy process-level exit). The REPL's startup banner reflects which mode is active. See [Library API → REPL keybindings]({{< relref "library-api.md#repl-keybindings-v130" >}}) for the underlying mechanism.

### Library callers

The `permissions.Prompter` interface is public:

```go
type Prompter interface {
    AskApproval(ctx context.Context, req PromptRequest) (Decision, error)
}
```

`permissions.StdinPrompter(in, out)` is the implementation the CLI uses; wire your own if you have a different UI (a TUI, a web prompt, a chat-based approver, etc.). Pass it via `permissions.FromConfig(cfg, projectRoot, userRoot, prompter)` when constructing the gate.

---

## `path_scope`

Extra paths file tools may touch outside the default project root + user home.

| Field | Type | Default | Notes |
|---|---|---|---|
| `allow` | string[] | `[]` | Patterns. Exact paths, directory trees ending in `/...`, or `path/filepath.Match` globs. |

Example:

```json
{
  "path_scope": {
    "allow": [
      "/etc/myapp/...",
      "/var/log/myapp.log",
      "~/scratch/*.json"
    ]
  }
}
```

---

## `agent`

Runtime tuning for the agent loop.

| Field | Type | Default | Notes |
|---|---|---|---|
| `max_steps` | int | `50` | Max tool-call cycles within a single turn before the agent gives up. |

---

## `tool_output`

Caps tool result size before it enters model context. Prevents a runaway `cat /huge.log` from blowing through your token budget. The built-in tools (`read_file`, `read_many_files`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash`, `todo`) honor these caps; consumer-provided tools should call `tools.Truncate(...)` to do the same.

| Field | Type | Default | Notes |
|---|---|---|---|
| `max_bytes` | int | `32768` | Per-tool-result byte cap. |
| `max_lines` | int | `500` | Per-tool-result line cap. |
| `per_tool` | object | see below | Per-tool overrides keyed by tool name. |

Default `per_tool` overrides (apply to the built-in tools that ship with core-agent):

```json
{
  "tool_output": {
    "per_tool": {
      "bash":      { "max_bytes": 65536,  "max_lines": 2000 },
      "read_file":       { "max_bytes": 262144, "max_lines": 5000 },
      "read_many_files": { "max_bytes": 262144, "max_lines": 5000 },
      "glob":            { "max_bytes": 32768,  "max_lines": 500 },
      "grep":            { "max_bytes": 262144, "max_lines": 5000 }
    }
  }
}
```

(`list_dir` falls back to its compile-time default of 32 KB / 500 lines when no override is set; the same for any other unlisted tool.)

core-agent ships these tools by default in the bundled CLI; library callers opt in with `tools.Build(cfg, gate, tools.Default())`. Override per-tool caps with the per-tool block above; add an entry under `per_tool` for any consumer-provided tool that should follow a non-default cap.

---

## `tools`

Controls which built-in tools are wired into the bundled CLI. Defaults to the full set; list entries here to turn specific tools off without disabling the whole suite.

| Field | Type | Default | Notes |
|---|---|---|---|
| `disable` | string[] | `[]` | Built-in tool names to turn off. Valid: `bash`, `read_file`, `read_many_files`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `todo`. Unknown names cause a startup error. |

Example — keep everything except shell access:

```json
{
  "tools": {
    "disable": ["bash"]
  }
}
```

The `--disable-tools=bash,write_file` CLI flag composes with this list by union — anything disabled in either path is off. To turn the entire suite off, use `--no-builtin-tools` (which makes `tools.disable` and `--disable-tools` moot).

---

## `mock`

Configures the `echo` and `scripted` mock providers, plus the orthogonal recording wrapper. See [Providers → Echo](#echo-mock) and [Providers → Scripted](#scripted-mock) for the full story; this section is the schema.

| Field | Type | Default | Notes |
|---|---|---|---|
| `script` | string | `""` | Path to a JSONL transcript. **Required** when `model.provider: scripted`. |
| `strict` | bool | `false` | Scripted: assert each incoming request's `Contents` JSON-equal the recorded request. Catches prompt-construction regressions. |
| `record` | string | `""` | Write a JSONL recording of every LLM turn to this path. Works with **any** provider, not just the mocks. |

Example — record a real Gemini session for later replay:

```json
{
  "model": { "provider": "gemini" },
  "mock":  { "record": "fixtures/last-session.jsonl" }
}
```

Example — replay it under tests:

```json
{
  "model": { "provider": "scripted" },
  "mock":  { "script": "fixtures/last-session.jsonl", "strict": true }
}
```

CLI flags `--script`, `--script-strict`, and `--record-to` override the corresponding fields. `--record-to` is the orthogonal one — it's safe to combine with any provider.

---

## `otel`

OpenTelemetry exporter config. Off by default — a fresh invocation makes zero outbound spans.

| Field | Type | Default | Notes |
|---|---|---|---|
| `exporter` | string | `none` | One of `none`, `console`, `otlp`. |
| `endpoint` | string | `""` | OTLP endpoint when `exporter: otlp` (or set via standard `OTEL_EXPORTER_OTLP_ENDPOINT` env). |

Console mode prints span JSON to stderr — useful for local debugging. OTLP mode honors all the standard `OTEL_*` env vars.

---

## `pricing` (top-level)

Governs the pricing-catalog refresh — distinct from `model.pricing` above (per-model rate overrides). Defaults: refresh enabled, daily cadence, LiteLLM upstream.

| Field | Type | Default | Notes |
|---|---|---|---|
| `refresh` | bool | `true` | Pull the upstream pricing JSON into `~/.core-agent/pricing.json`'s external section once per day on startup. Set to `false` for air-gapped pods or any environment where outbound network is blocked / undesirable. CLI flag `--no-pricing-refresh` always wins. |
| `source` | string | `https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json` | Upstream URL to fetch. Override for mirrors or internal pricing services. The fetched JSON must match LiteLLM's schema (per-token costs + mode field). |

The refresher uses `If-None-Match` against a stored ETag so re-fetches transfer zero bytes when upstream hasn't changed. Network failures are non-fatal: the existing cache stays in place, a one-line warning ("using N-day-old cache; network: …") goes to stderr, and the session continues.

From the in-process TUI, two slash commands give operators direct control without leaving the chat:

- `/pricing refresh` — force an out-of-cycle fetch from `pricing.source` (ignores the 24h cadence). Useful right after a provider price change. Result lands in the chat scrollback: "Refresh: updated 247 models from upstream" / "Refresh: upstream unchanged" / "Refresh failed; using N-day-old cache".
- `/pricing set <model> <input_per_mtok> <output_per_mtok>` — write a per-model rate to `~/.core-agent/pricing.json`'s `manual` section atomically + rebuild the live catalog so it takes effect immediately. Example: `/pricing set gemini-3.5-flash 0.075 0.30`. The manual section round-trips intact across the daily refresh (the auto-fetcher only rewrites `external`).

## `url_scope`

Governs which URLs the `fetch_url` built-in is allowed to reach. Same Allow/Deny grammar as [`path_scope`]({{< relref "#path_scope" >}}) but for HTTP hosts instead of filesystem paths. `Deny` always wins over `Allow`. An **empty `allow` is default-deny** — `fetch_url` is not registered as a tool at all when no allowlist is configured, so the model can't even attempt a network call without an operator-declared scope.

| Field | Type | Default | Notes |
|---|---|---|---|
| `allow` | string[] | `[]` | Host patterns. `github.com` (exact), `*.googleapis.com` (subdomain wildcard), `*` (any host), `http://localhost:*` (HTTP + any-port opt-in). HTTPS by default — prefix with `http://` to allow plain HTTP for that pattern only. |
| `deny` | string[] | `[]` | Patterns that override `allow` on overlap (same grammar). |
| `max_body_bytes` | int | `65536` | Cap on the response body returned to the model. Per-call `max_bytes` argument can lower this, never raise it. |
| `timeout_seconds` | int | `30` | HTTP timeout per call. |
| `headers` | object | `{}` | Per-host header bundles. Map of host-pattern → header-name → value template. Values pass through `os.ExpandEnv` at request time, so rotated env vars take effect on the next fetch without a restart. Most-specific pattern wins (longer wins; exact match beats wildcard). The model **never** sets headers directly — keeps credential exfiltration off the tool-argument surface. |

Worked example:

```json
{
  "url_scope": {
    "allow": [
      "api.github.com",
      "*.googleapis.com",
      "*.svc.cluster.local",
      "http://localhost:*"
    ],
    "deny": ["*.internal.evil.com"],
    "max_body_bytes":  131072,
    "timeout_seconds": 30,
    "headers": {
      "api.github.com": {
        "Authorization": "Bearer ${GITHUB_TOKEN}",
        "Accept":        "application/vnd.github+json"
      }
    }
  }
}
```

Each fetch emits a `tool/fetch_url` event into the eventlog with structured metadata (`url`, `final_url`, `status`, `content_type`, `bytes`, `truncated`), so an audit query can answer "what URLs did this agent touch, when, and what came back" without parsing tool output. Composes with the [permissions gate]({{< relref "#permissions" >}}) — write `permissions.allow: ["fetch_url:github.com/*"]` to gate per-host even within the URL allowlist.

What's **not** in `fetch_url` (by design):

- **No POST / forms / uploads** — GET only. Use a dedicated MCP server for structured POSTs where the operation can be schema-typed.
- **No JavaScript execution** — use the playwright MCP for dynamic pages.
- **No cookie persistence** — each call is stateless.
- **No model-set auth headers** — headers come from `url_scope.headers` + env expansion only. The model picks the host; the operator picks what auth ships with the request.

CLI conveniences (no config edit needed):

- `--allow-url-host="github.com,*.googleapis.com"` — appends to `url_scope.allow` for the current invocation.
- `--disable-tools=fetch_url` — turns the tool off even if an allowlist is configured.

See [`fetch-url-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/fetch-url-design.md) for the full decision record.

---

## `attach`

Default values for the attach-mode listener and the peer-registration client. Every field below is also exposed as a `--attach-*` CLI flag; the flag wins when explicitly set, otherwise the config value applies, otherwise the zero value. This section exists for K8s-style deployments where the same settings would otherwise be repeated on every invocation.

String fields are passed through `os.ExpandEnv` so per-pod values like `"https://${POD_IP}:7777"` can live in a shared ConfigMap and resolve to the right address at startup.

| Field | Type | Default | Notes |
|---|---|---|---|
| `listen` | string | `""` | Address the attach HTTP server binds to (e.g. `"0.0.0.0:7777"`). Empty → server off. Mutually exclusive with `unix_socket`. Requires `--session-db` at runtime (the broadcaster pumps from the event log). |
| `unix_socket` | string | `""` | Bind path for the Unix-socket transport (e.g. `"/var/run/core-agent.sock"`). Same SSE protocol; useful for local dev and Cloud Run sidecar shapes. |
| `tls_cert` | string | `""` | TLS server certificate (PEM path). Pair with `tls_key` to enable HTTPS. |
| `tls_key` | string | `""` | TLS server key (PEM path). |
| `client_ca` | string | `""` | CA bundle (PEM path) for client-certificate verification (mTLS). When set, clients must present a cert signed by this CA. |
| `token_env` | string | `""` | **Env var *name*** (not the secret) holding the bearer token clients must present in `Authorization: Bearer <token>`. The secret itself never lives in this file — mount it via your secret manager. |
| `readonly` | bool | `false` | Disable `POST /inject` and `POST /wake`. Read endpoints (`GET /sessions`, `GET .../events`) stay open. |
| `peer_hub` | bool | `false` | Enable peer-registration endpoints (`POST /peers`, `GET /peers`, `POST /peers/<id>/heartbeat`, `DELETE /peers/<id>`) on the listener — this agent becomes a discovery hub. |
| `register_to` | string | `""` | Hub URL this agent registers with on startup (e.g. `"https://hub.default.svc:7777"`). Empty → no registration. Heartbeats automatically until shutdown. |
| `register_endpoint` | string | `""` | Reachable URL the hub records for this agent. Required when `register_to` is set, since the agent's own `listen` value is commonly `0.0.0.0` and not directly reachable. Typically `"https://${POD_IP}:7777"`. |
| `register_name` | string | hostname | Name to register under. Defaults to `os.Hostname()` when empty. Name-based upsert: a restart re-uses the slot rather than orphaning the old entry. |

Worked example for a K8s deployment ConfigMap:

```json
{
  "version": 1,
  "model": { "provider": "vertex", "name": "gemini-3.1-pro-preview-customtools",
             "vertex": { "project": "my-proj", "location": "us-central1" } },
  "attach": {
    "listen":            "0.0.0.0:7777",
    "tls_cert":          "/etc/attach/tls.crt",
    "tls_key":           "/etc/attach/tls.key",
    "client_ca":         "/etc/attach/ca.crt",
    "token_env":         "ATTACH_TOKEN",

    "register_to":       "https://core-agent-hub.default.svc:7777",
    "register_endpoint": "https://${POD_IP}:7777",
    "register_name":     "monitor-${HOSTNAME}"
  }
}
```

See [Attach mode]({{< relref "user-guide.md" >}}) for the protocol and CLI overview, including the `--attach-token=<envvar>` flag that pairs with `token_env`.

---

## Discovery and merge

`core-agent` finds your config like this:

1. **Walk up** from the current working directory looking for a folder named `.agents/`. First match wins.
2. **Read** `<found>/config.json` if present. Missing file → use built-in defaults.
3. **Merge** the loaded JSON over `config.DefaultConfig()` — unspecified fields keep their defaults. Unknown fields are tolerated for forward compatibility.
4. **Validate** the merged result. Bad provider name, missing required field, or wrong schema version → fail fast at startup.

Override discovery with the CLI's `-c <path>` flag, which reads the file directly and treats its parent directory as the agentsDir for MCP / skills resolution.

---

## Atomic writes

`config.Save(path, cfg)` writes via temp file + `rename` so a partial write can never leave a corrupt `config.json` on disk. Use it when you build tooling that mutates config (e.g. an `init`-style command, or a `/permissions` slash command in a downstream consumer).

---

## Not in `config.json` — runtime-only flags

A handful of features are CLI-flag-only, with no `config.json` field today (consumers that want them per-project typically wrap the CLI in a script):

| Flag | Documented at |
|---|---|
| `--ask=stdin\|auto\|off` | [Library API → Prompter]({{< relref "library-api.md#prompter" >}}) |
| `--session-db`, `--session-db-path` | [Sessions and event log]({{< relref "sessions.md#cli-flags" >}}) |
| `--color=auto\|always\|never` | [Library API → Color]({{< relref "library-api.md#color" >}}) |
| `--record-to`, `--script`, `--script-strict` | [Providers → Mock providers]({{< relref "providers.md" >}}) |
| `--no-tui` | [Getting started → Multi-turn TUI]({{< relref "getting-started.md" >}}) — skip the bubble-tea TUI even on a TTY (slim build / scripts / unusual terminals) |
| `--no-compact` | [Context management → Compaction]({{< relref "context-management.md" >}}) — disable automatic compaction (`/compact` slash still works) |
| `--no-checkpoint` | [Context management → Task-boundary checkpoints]({{< relref "context-management.md" >}}) — disable `/done` slash + `mark_task_done` tool |
| `--agentic-tools` | [Context management → Agentic tool wrappers]({{< relref "context-management.md" >}}) — register the `agentic_*` tool family |
| `--agentic-small-model=ID` | [Context management → Agentic tool wrappers]({{< relref "context-management.md" >}}) — route agentic subtasks to a cheaper model |

The `CORE_AGENT_TUI=internal` environment variable picks the legacy `internal/tui` code path in place of the v2 default (core-tui). One-release escape hatch for operators who hit a regression; scheduled for removal in v2.1.
