# `kode-gopher` — Code Mode over MCP with `core-agent`

A **config-only** recipe pairing `core-agent` with [`gke-demos/kode-gopher`](https://github.com/gke-demos/kode-gopher), which implements "Code Mode" over MCP ([#654](https://github.com/go-steer/core-agent/issues/654), [#1391](https://github.com/go-steer/core-agent/issues/1391), first slice of [#200](https://github.com/go-steer/core-agent/issues/200)).

Instead of making dozens of individual REST/MCP calls and streaming raw resource lists through the context window, the agent writes a small typed Go program (`func run(ctx context.Context) (any, error)`) against pre-warmed `k8s.io/client-go` and `cloud.google.com/go/*` packages, compiles and runs it inside `kode-gopher`, and receives only the distilled JSON result.

## What ships in this example

```
.agents/
├── mcp.json        # mounts `kode-gopher serve` over stdio + per-server tools allowlist + tool_notes
├── config.json     # ask mode: pre-allows read-only doc/auth tools, keeps execute_go_code gated
└── AGENTS.md       # Code Mode instructions (lookup_package_docs → execute_go_code)
```

## Prerequisites

1. **Install `core-agent` and `kode-gopher`:**

   ```bash
   go install github.com/go-steer/core-agent/v3/cmd/core-agent@latest
   go install github.com/gke-demos/kode-gopher/cmd/kode-gopher@latest
   ```

2. **Configure model credentials:**

   ```bash
   export GEMINI_API_KEY=...
   ```

3. **Configure cluster / cloud credentials** for the `kode-gopher` sandbox (e.g., `KUBECONFIG` or Application Default Credentials via `gcloud auth application-default login`).

## Run locally (stdio transport)

`.agents/mcp.json` mounts `kode-gopher serve` over `stdio`:

```bash
cd examples/kode-gopher
core-agent
```

Try prompts that benefit from client-side filtering and cross-resource correlation in Go:

```
> Find all pods across all namespaces whose container image tag is :latest or whose restart count is > 3, grouped by owning workload
> Check gcp_auth_status, look up the client-go PodList fields, and return a JSON summary of non-Ready pods and their latest warning events
```

## In-cluster deployment (Streamable HTTP `/mcp` transport)

When `kode-gopher` runs as an in-cluster Deployment or Cloud Run service (`kode-gopher serve --http :8080`, exposing `/mcp`), switch `.agents/mcp.json` from `stdio` to `http` and configure authentication through `core-agent`'s existing HTTP MCP auth path (`auth.google_oauth` for Google OAuth / Workload Identity tokens, or `headers` with `${env:...}` interpolation for bearer tokens):

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

For in-cluster Kubernetes Service endpoints using a static or projected bearer token instead of Google OAuth, use `headers`:

```json
"headers": {
  "Authorization": "Bearer ${env:KODE_GOPHER_BEARER_TOKEN}"
}
```

## Permissions posture

| Tool (namespaced in `core-agent`) | Upstream `kode-gopher` tool | Posture in `.agents/config.json` | Why |
|---|---|---|---|
| `kode_gopher_execute_go_code` | `execute_go_code` | **Gated** (`mode: "ask"`) | Compiles and executes arbitrary Go code with the sandbox's forwarded Kubernetes and Google Cloud credentials. Always prompts in `ask` mode so a human reviews the snippet before it runs. |
| `kode_gopher_lookup_package_docs` | `lookup_package_docs` | **Pre-allowed** (`mcp:kode_gopher_lookup_package_docs*`) | Purely read-only Go doc lookup against the pre-warmed module cache. |
| `kode_gopher_gcp_auth_status` | `gcp_auth_status` | **Pre-allowed** (`mcp:kode_gopher_gcp_auth_status*`) | Purely read-only inspection of the active ADC / kubeconfig identity. |

### Why this recipe uses `tools` + `permissions.allow` instead of `readOnlyHint`

`core-agent` honours per-tool MCP `readOnlyHint` annotations ([#1098](https://github.com/go-steer/core-agent/issues/1098)), and setting `"read_only": true` on the whole server in `mcp.json` would falsely classify `execute_go_code` as read-only. Because `kode-gopher` does not publish `readOnlyHint` annotations on `lookup_package_docs` and `gcp_auth_status` today, this recipe pairs:

1. the per-server `"tools"` allowlist in `.agents/mcp.json` (`["execute_go_code", "lookup_package_docs", "gcp_auth_status"]`) so only those three verbs can register; and
2. explicit policy allows (`"mcp:kode_gopher_lookup_package_docs*"`, `"mcp:kode_gopher_gcp_auth_status*"`) in `.agents/config.json` so the two read-only tools run without prompting while `execute_go_code` stays gated.

## Why `tool_notes` is set on `execute_go_code`

`kode-gopher` requires every snippet to define `func run(ctx context.Context) (any, error)` (shorthand `func run(ctx) (any, error)`), marshals the returned value to JSON, and reports compiler errors with `phase=build`. Putting that contract in `tool_notes.execute_go_code` appends it directly to the tool declaration the model reads at the call site ([#1016](https://github.com/go-steer/core-agent/issues/1016)), preventing first-turn `func main()` mistakes.

## Verification and `kind` UAT

- **Offline CI checks:** As a config-only recipe (no `main.go`), `examples/kode-gopher` is discovered automatically by `examples/internal/recipecheck` (`TestEverySkillNamedToolIsReachable`) and verified by [`recipe_test.go`](./recipe_test.go), which tests the `stdio` and `http` MCP configs, the `tool_notes` contract, and the `permissions.Gate` behavior for all three tools without a cluster.
- **Live `kind` UAT:** [`dev/uat/kode-gopher/run.sh`](../../dev/uat/kode-gopher/run.sh) runs `core-agent` + `kode-gopher` against a `kind` cluster (and supports `--dry-run` for hermetic smoke verification).
