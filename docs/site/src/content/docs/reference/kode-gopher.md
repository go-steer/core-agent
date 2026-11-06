---
title: kode-gopher (Code Mode over MCP)
description: Pairing core-agent with gke-demos/kode-gopher for typed Go Code Mode over stdio and Streamable HTTP MCP.
---

[`gke-demos/kode-gopher`](https://github.com/gke-demos/kode-gopher) implements **Code Mode** over the Model Context Protocol (MCP): instead of issuing dozens of individual tool calls and streaming raw API payloads through the model's context window, the agent writes a small typed Go snippet against pre-warmed `k8s.io/client-go` and `cloud.google.com/go/*` packages, compiles and executes it inside `kode-gopher`, and receives only the distilled JSON result.

The ready-to-run config recipe lives at [`examples/kode-gopher/`](https://github.com/go-steer/core-agent/tree/main/examples/kode-gopher) ([#1391](https://github.com/go-steer/core-agent/issues/1391), first slice of [#200](https://github.com/go-steer/core-agent/issues/200)).

---

## Local setup (`stdio` transport)

Mount `kode-gopher serve` in `.agents/mcp.json` with a per-server `tools` allowlist and a `tool_notes` entry on `execute_go_code`:

```json
{
  "version": 1,
  "servers": {
    "kode_gopher": {
      "transport": "stdio",
      "command": "kode-gopher",
      "args": ["serve"],
      "tools": [
        "execute_go_code",
        "lookup_package_docs",
        "gcp_auth_status"
      ],
      "tool_notes": {
        "execute_go_code": "Snippet contract: define `func run(ctx context.Context) (any, error)` (or `func run(ctx) (any, error)` in shorthand). The returned `any` value is marshaled to JSON in the tool response, and compile or type-check failures are returned with `phase=build` so you can fix imports or types before any cluster or cloud call executes."
      }
    }
  }
}
```

---

## In-cluster setup (Streamable HTTP `/mcp` transport)

When `kode-gopher` runs as an in-cluster service (`kode-gopher serve --http :8080`, serving `/mcp`), use `transport: "http"` and configure credentials through `core-agent`'s HTTP MCP auth surface (`auth.google_oauth` for Workload Identity / ADC tokens, or `headers` with `${env:...}` interpolation for bearer tokens):

```json
{
  "version": 1,
  "servers": {
    "kode_gopher": {
      "transport": "http",
      "url": "https://kode-gopher.internal.example.com/mcp",
      "tools": [
        "execute_go_code",
        "lookup_package_docs",
        "gcp_auth_status"
      ],
      "auth": {
        "google_oauth": {
          "scopes": [
            "https://www.googleapis.com/auth/cloud-platform"
          ]
        }
      },
      "tool_notes": {
        "execute_go_code": "Snippet contract: define `func run(ctx context.Context) (any, error)` (or `func run(ctx) (any, error)` in shorthand). The returned `any` value is marshaled to JSON in the tool response, and compile or type-check failures are returned with `phase=build` so you can fix imports or types before any cluster or cloud call executes."
      }
    }
  }
}
```

---

## Permissions posture

In `.agents/config.json`, keep `execute_go_code` gated in `ask` mode while pre-allowing the two read-only inspection tools:

```json
{
  "version": 1,
  "model": {
    "provider": "gemini",
    "name": "gemini-3.1-pro-preview-customtools"
  },
  "permissions": {
    "mode": "ask",
    "allow": [
      "mcp:kode_gopher_lookup_package_docs*",
      "mcp:kode_gopher_gcp_auth_status*"
    ]
  },
  "agent": {
    "display_name": "kode-gopher",
    "max_steps": 50
  }
}
```

- **`kode_gopher_execute_go_code` stays gated.** It compiles and runs arbitrary Go code with the sandbox's forwarded Kubernetes and Google Cloud credentials.
- **`kode_gopher_lookup_package_docs` and `kode_gopher_gcp_auth_status` are pre-allowed via `tools` + `permissions.allow`.** `core-agent` honours MCP `readOnlyHint` annotations ([#1098](https://github.com/go-steer/core-agent/issues/1098)), and setting `"read_only": true` at the server level would wrongly mark `execute_go_code` read-only. Because `kode-gopher` does not publish per-tool `readOnlyHint` annotations today, the recipe pairs the per-server `tools` allowlist in `mcp.json` with `mcp:kode_gopher_lookup_package_docs*` and `mcp:kode_gopher_gcp_auth_status*` in `permissions.allow`.

---

## Snippet contract (`tool_notes`)

`kode-gopher` expects snippets passed to `execute_go_code` to implement:

```go
func run(ctx context.Context) (any, error)
```

- The returned `any` value is serialized to JSON and handed back to the agent.
- Compilation or type-checking errors return structured diagnostics with `phase=build` before any cluster or cloud call executes.
- Attaching this contract via `tool_notes.execute_go_code` places it directly on the tool declaration the model sees at call time ([#1016](https://github.com/go-steer/core-agent/issues/1016)).

---

## See also

- [`examples/kode-gopher/`](https://github.com/go-steer/core-agent/tree/main/examples/kode-gopher) — complete config recipe and offline policy tests.
- [MCP servers](/concepts/mcp/) — `mcp.json` transport, `tools` allowlist, `tool_notes`, and `read_only` reference.
- [Permissions](/concepts/permissions/) — `mcp:<tool>*` policy pattern matching.
