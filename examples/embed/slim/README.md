# `examples/embed/slim` — Reference Slim-Embed Consumer

This example demonstrates embedding `core-agent`'s runtime governance slice in
an existing Go binary without pulling in the interactive TUI, MCP SDK,
Prometheus, OpenTelemetry SDK/exporters, SQLite drivers, or daemon wiring.

## What it wires

- **`github.com/go-steer/core-models/llm` + `pkg/models.Adapt`** — adapts a
  `core-models` `llm.LLM` to ADK v2's `model.LLM` interface while bridging cache
  usage metadata.
- **`pkg/permissions`** — constructs a `permissions.Gate` in `ModeAllow` with an
  explicit allowlist policy (`ops:get_service_status*`).
- **`pkg/tools.GateToolset`** — wraps an `adktool.Toolset` so every tool call
  passes through `permissions.Gate` before executing.
- **`pkg/usage`** — tracks per-call token and USD spend via `usage.TurnTap`,
  `usage.Tracker`, and the built-in `core-models` price catalog (`usage.PriceFor`).
- **`pkg/agent`** — runs the multi-step ADK v2 turn loop in `ModeAutonomous`.

## Running

The example uses an offline scripted `llm.LLM` and requires no credentials or
network access:

```bash
go run ./examples/embed/slim
```

## Dependency-hygiene gate (`verify-slim-deps`)

`dev/tools/verify-slim-deps` (invoked by `dev/ci/presubmits/verify-slim-deps`)
inspects `go list -deps ./examples/embed/slim` and fails if any transitive
import matches the slim-embed denylist:

- `cmd/core-agent`, `cmd/core-agent-tui`
- `internal/attachclient`, `internal/coretuievent`, `internal/coretuiremote`,
  `internal/evals`, `internal/webui`
- `pkg/approver`, `pkg/attachadapter`, `pkg/compose`, `pkg/instruction`,
  `pkg/mcp`, `pkg/runner`, `pkg/skills`, `pkg/telemetry`
- `github.com/go-steer/core-tui/...`, `github.com/charmbracelet/...`
- `github.com/modelcontextprotocol/...`
- `github.com/prometheus/...`
- `go.opentelemetry.io/otel/sdk/...`, `go.opentelemetry.io/otel/exporters/...`
- `github.com/glebarez/sqlite`, `github.com/glebarez/go-sqlite`,
  `modernc.org/...`, `github.com/mattn/go-sqlite3`

Note: `pkg/agent` imports `pkg/attach/wire` and `pkg/eventlog` for its option and
audit-event types (`WithAttachEmitter`, `WithEventLog`), and `google.golang.org/genai`
plus `pkg/agent` import the OpenTelemetry API (`go.opentelemetry.io/otel`,
`/trace`, `/metric`). Neither pulls in SQLite drivers, Prometheus, or the
OpenTelemetry SDK unless `pkg/telemetry` or `github.com/glebarez/sqlite` is
imported by the host binary. `pkg/attach/wire` is the attach protocol's
vocabulary only ([#1415](https://github.com/go-steer/core-agent/issues/1415)):
the HTTP/SSE server in `pkg/attach` is not in this graph. Two heavy edges
remain by decision: `gorm.io/gorm` core (driverless), because
`eventlog.Handle` is part of `pkg/agent`'s contract, and
`github.com/gorilla/websocket`, which `google.golang.org/genai` imports for
the Live API. Because the denylist cannot see that kind of arrival,
the script also enforces a transitive package-count ceiling
(`SLIM_MAX_PACKAGES`, 560 against a measured 543); a PR that knowingly adds
weight raises it in the same change and says what came in.
