---
title: Configuration
weight: 4
---

# Configuration

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
  "otel": { ... }
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
| `name` | string | `gemini-3.1-pro-preview` | Model ID. **Required.** |
| `api_key` | string | `""` | Inline key for `provider: gemini`. Usually unset; read from `GOOGLE_API_KEY` / `GEMINI_API_KEY` at runtime. |
| `vertex` | object | `null` | GCP project + region. Required when `provider: vertex`. |
| `vertex.project` | string | — | GCP project ID. |
| `vertex.location` | string | — | GCP region (e.g. `us-central1`). |
| `anthropic` | object | `null` | Claude-specific settings. |
| `anthropic.api_key` | string | `""` | Inline Anthropic key. Usually read from `ANTHROPIC_API_KEY`. |
| `anthropic.vertex` | object | `null` | When `provider: anthropic-vertex`, holds project + region. |
| `anthropic.vertex.project` | string | — | GCP project ID for Vertex Anthropic. Falls back to `ANTHROPIC_VERTEX_PROJECT_ID` then `GOOGLE_CLOUD_PROJECT`. |
| `anthropic.vertex.location` | string | — | Region (e.g. `us-east5`). Falls back to `CLOUD_ML_REGION` then `GOOGLE_CLOUD_LOCATION`. |
| `pricing` | object | `null` | Per-model price override for `usage.Tracker`. |
| `pricing.input_per_mtok` | float | — | USD per 1M input tokens. |
| `pricing.output_per_mtok` | float | — | USD per 1M output tokens. |

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

Caps tool result size before it enters model context. Prevents a runaway `cat /huge.log` from blowing through your token budget. The built-in tools (`read_file`, `write_file`, `edit_file`, `list_dir`, `bash`, `todo`) honor these caps; consumer-provided tools should call `tools.Truncate(...)` to do the same.

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
      "read_file": { "max_bytes": 262144, "max_lines": 5000 },
      "list_dir":  { "max_bytes": 32768,  "max_lines": 500 }
    }
  }
}
```

core-agent ships these tools by default in the bundled CLI; library callers opt in with `tools.Build(cfg, gate, tools.Default())`. Override per-tool caps with the per-tool block above; add an entry under `per_tool` for any consumer-provided tool that should follow a non-default cap.

---

## `tools`

Controls which built-in tools are wired into the bundled CLI. Defaults to the full set; list entries here to turn specific tools off without disabling the whole suite.

| Field | Type | Default | Notes |
|---|---|---|---|
| `disable` | string[] | `[]` | Built-in tool names to turn off. Valid: `bash`, `read_file`, `write_file`, `edit_file`, `list_dir`, `todo`. Unknown names cause a startup error. |

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
