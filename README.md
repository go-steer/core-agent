# core-agent

A reusable Go-based agent built on the [Google Agent Development Kit](https://pkg.go.dev/google.golang.org/adk).

**📚 Full documentation: [go-steer.github.io/core-agent](https://go-steer.github.io/core-agent/)**

[![CI](https://github.com/go-steer/core-agent/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/go-steer/core-agent/actions/workflows/ci.yml)
[![Docs](https://github.com/go-steer/core-agent/actions/workflows/docs.yml/badge.svg?branch=main)](https://go-steer.github.io/core-agent/)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](./LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-steer/core-agent.svg)](https://pkg.go.dev/github.com/go-steer/core-agent)

`core-agent` is the bottom layer for any project that needs a multi-turn LLM agent. It ships the wiring — model providers, MCP servers, skills loading, instruction loading, permission gating, telemetry, transcript persistence — so consuming projects can focus on their own tools and product logic.

> **Status:** early. APIs may change. The headless CLI works; the library is in active use as the foundation for downstream projects in the [go-steer](https://github.com/go-steer) org.

---

## Features

- **Multi-turn conversation** — backed by ADK's `runner.Runner` with an in-memory session service that automatically replays history across turns.
- **Multiple model providers**, picked by config or auto-detected from environment:
  - **Gemini API** via `GOOGLE_API_KEY` or `GEMINI_API_KEY` (either is accepted; `GEMINI_API_KEY` is the one Gemini's own docs and tutorials use). Gemini's built-in **Google Search** and **URL Context** tools are wired up by default; **Code Execution** is one option flip away.
  - **Vertex AI** (Gemini) via `GOOGLE_GENAI_USE_VERTEXAI=true` + ADC + `GOOGLE_CLOUD_PROJECT` — same built-in tools as Gemini API.
  - **Anthropic / Claude** via `ANTHROPIC_API_KEY` — implemented as a native ADK `model.LLM` adapter (ADK Go ships only Gemini + Apigee out of the box). Claude's server-side **Web Search** is one option flip away.
  - **Anthropic / Claude via Vertex AI** via ADC + `ANTHROPIC_VERTEX_PROJECT_ID` + `CLOUD_ML_REGION` — same adapter, GCP-authed and GCP-billed, no separate Anthropic API key required
  - **Mock providers** for credential-free testing — `--provider=echo` returns the user prompt verbatim; `--provider=scripted --script=transcript.jsonl` replays a recorded session. Pair with `--record-to=path.jsonl` against any provider to capture a transcript for later replay.
- **AGENTS.md instruction loading** — system prompt prefix is assembled from `~/.core-agent/AGENTS.md` and the project's `AGENTS.md` (with `CLAUDE.md` / `GEMINI.md` fallbacks), preserving the [agent.md](https://agent.md/) convention plus the fallback names other agent tools have adopted.
- **MCP servers** — declarative `.agents/mcp.json`; stdio and Streamable HTTP transports; tools are namespaced (`<server>_<tool>`) and pass through the permission gate.
- **Claude-compatible skills** — drop a `SKILL.md` bundle into `.agents/skills/<name>/` and the agent can invoke it on demand via ADK's `skilltoolset`.
- **Built-in tool suite** — `read_file`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash`, `todo`. Wired up by default in the bundled CLI; opt-out via `--no-builtin-tools` for the whole suite, or `--disable-tools=bash,write_file` (or `tools.disable` in config) for specific entries. All tools route through the permission gate.
- **Permission gate** — ask / allow / yolo modes, per-tool allow- and deny-list patterns, path-scope checks for file tools, and a built-in bash denylist that's non-overridable. In the bundled CLI, `ask` mode prompts the user interactively (y/s/t/a/N) when stdin is a TTY; `--yolo` bypasses the gate entirely for headless use.
- **Server-side built-in observability** — Gemini's `GoogleSearch` activity surfaces in the chat-style stdout (`↪ google_search: query: ...`, `↪ google_search: <title> — <uri>`) and as queryable `gemini/google_search`-authored rows in the eventlog when `--session-db` is enabled. Same `↪` namespace reserved for Anthropic's server-side tools when they land.
- **Telemetry** — opt-in OpenTelemetry export (console / OTLP); off by default so a fresh invocation makes zero outbound calls.
- **Headless CLI** — `core-agent -p "prompt"` for one-shot use; bare `core-agent` drops into a stdin REPL with conversation history preserved across turns.
- **Autonomous-run driver** — `agent.RunAutonomous` for unattended multi-turn workers (batch jobs, CI tasks, scheduled scripts) with budget caps (turns / tokens / cost / wallclock) and a model-driven termination signal via the bundled `tools.NewLifecycleTool`. Pair with `--ask=auto` so instructions like "ask before doing X" get a clean refusal in headless contexts instead of blocking.
- **Durable sessions + audit log** — `eventlog.Open(...)` returns a SQLite/Postgres/MySQL-backed `session.Service` plus a `Stream` with monotonic `seq` numbers, `Since(seq)` replay, and `Watch(seq)` live-tail. CLI flags `--session-db` / `--session-db-path` enable persistence; the default path `~/.<binary>/sessions.db` is derived from `os.Executable()` so adapters and forks each get their own directory.
- **Subagents** — `agent.WithSubagents([]*Agent)` registers each agent as a callable tool the parent's model can invoke by name (synchronous fan-out). For *background* subagents the parent's model decides to spawn at runtime, use `agent.NewBackgroundAgentManager` + the `spawn_agent` tool family (in-process) or `agent.NewSpawnRemoteAgentTool` with a consumer-supplied `RemoteAgentSpawner` (out-of-process: gRPC / K8s Jobs / Cloud Run). Subagent reports flow back through a pre-turn drain so the parent sees them on its next turn. Subagent events stream into the same audit log under `Branch="<parent>.<sub>"` (or `Branch="bg.<sub>"` for background subagents) for branch-scoped replay. See `examples/with-subagent/` and `examples/background-monitor/`.
- **Optional Scion adapter** — [`extras/scion-agent/`](./extras/scion-agent/) packages core-agent for [Scion](https://github.com/GoogleCloudPlatform/scion)'s container runtime: lifecycle status emission, `--input` task delivery, and a `sciontool_status` tool the model uses to declare sticky states.
- **Agent-card discovery** — opt-in `/.well-known/agent-card.json` on the attach listener describes the binary's name, description, skills, and required auth in the [A2A AgentCard](https://agent2agent.info/docs/concepts/agentcard/) shape so [Google Cloud Agent Registry](https://docs.cloud.google.com/agent-registry/register-agents) (and any other consumer of the well-known path) can index it. Configured via `.agents/agent-card.json` + `--agent-card-*` flag overrides; `.agents/skills/` bundles auto-populate the `skills` array. The endpoint stays unauthenticated by spec convention; emitting a card does not imply the binary speaks the A2A JSON-RPC transport.

---

## Releases

Tagged releases follow [SemVer](https://semver.org). See [`CHANGELOG.md`](./CHANGELOG.md) for the per-version history and the stability promise. Pre-1.0, breaking changes are possible at minor-version boundaries (`v0.X`); patches (`v0.X.Y`) are bug fixes only.

Current release: **v1.8.0** — Remote operability. Five surfaces that together turn `core-agent` from a single-process CLI into a deployable, observable, controllable runtime: **attach-mode** (HTTP + Server-Sent Events live-tail + `POST /inject` + `/wake` for headless agents, with mTLS + bearer auth), **peer registration** (hub-and-spoke fleet discovery on top of attach), **attach-config** (`.agents/config.json` defaults + `${ENV_VAR}` expansion so K8s ConfigMaps can replace 8+ CLI flags), **read-only state endpoints** (`GET /sessions/<sid>/tools|agents|status` + `permissions.Gate.Snapshot()` so an operator surface can see what's gated without a re-run), and the **`core-agent-tui`** bubble-tea binary (separate `cmd/core-agent-tui/`; the default `core-agent` binary stays bubble-tea-free so distroless K8s images don't ship the TUI deps). Plus **`fetch_url`** — HTTP GET as a structured built-in with a `url_scope` allowlist mirroring `path_scope`, the one tool we picked up from Hermes Agent's catalog because it closes a real gap (`bash curl` shell-outs lose URL + status as structured eventlog metadata) and fits our structural-defaults posture (operator-declared `url_scope.allow` with HTTPS-only by default; per-host `headers` injection from env-var references so credentials never live on tool arguments). Two REPL fixes from UAT: external `POST /inject` now actually triggers a turn (the loop selects on stdin + wake instead of stdin only), and streamed model output gets an `asst › ` chevron so the reply is visually distinct from the prompt. v1.7.0 was distroless-prep — three new built-ins (`delete_file`, `stat`, `json_query` via [`gojq`](https://github.com/itchyny/gojq)) closing the gaps an upcoming K8s deployment using `gcr.io/distroless/static` would otherwise hit, plus a new `Agent.RunWithContents` driver for runtimes that own the conversation history. v1.6.0 added scheduled monitoring — `tools.Scheduler` + `tools.SleepScheduler` / `ExitOnDeferScheduler` + `schedule_next_turn` so the model emits "wake me at T+N" intent the autonomous driver honors between turns; validated against a real GKE cluster in `dev/uat/scheduled-monitor`. v1.5.0 added remote MCP servers (Google OAuth on `.agents/mcp.json` HTTP endpoints) plus two latent fixes. v1.4.0 retargeted Gemini tool-calling (parallelism mandate, tool-description rewrites, `read_many_files`, default-model flip to `gemini-3.1-pro-preview-customtools`). v1.3.0 added interrupt machinery (`Agent.Inject`, `StartAutonomous` → `*AutonomousHandle`, ESC-mid-turn + double-Ctrl+C-exits in the REPL). v1.2.0 added dynamic background subagents (`spawn_agent`) + a `RemoteAgentSpawner` seam. v1.1.0 added interactive permissions + Gemini grounding visibility. v1.0.1 patched two `--provider=vertex` regressions. v1.0.0 was the first stable release. Public API is under SemVer; breaking changes go through `v1.X.0` minor bumps with a one-version deprecation period when feasible.

```bash
go get github.com/go-steer/core-agent@v1.8.0
```

## Documentation

Full reference docs live at **<https://go-steer.github.io/core-agent/>** — getting started, every provider with env-var details, `.agents/config.json` schema, MCP setup, skills, permissions, and library API.

The site is built with [Hugo](https://gohugo.io) using the [Docsy](https://www.docsy.dev/) theme; sources are in [`docs/site/`](./docs/site). To preview locally:

```bash
cd docs/site
npm install              # one-time: postcss + autoprefixer (Docsy CSS pipeline)
hugo server              # http://localhost:1313/core-agent/
```

Hugo Extended (≥ 0.146.0) is required — Docsy uses Hugo's SCSS pipeline.

Internal design docs live in [`docs/`](./docs) directly:

- [`DESIGN.md`](./docs/DESIGN.md) — architectural rationale, the *why* behind the package layout, the Anthropic adapter, and the deliberate non-goals
- [`acceptance-m1.md`](./docs/acceptance-m1.md), [`acceptance-m2.md`](./docs/acceptance-m2.md) — per-milestone acceptance test plans

## Install

As a CLI:

```bash
go install github.com/go-steer/core-agent/cmd/core-agent@latest
```

As a library:

```bash
go get github.com/go-steer/core-agent
```

As a container image (v2.3.1+ — multi-arch amd64 + arm64; distroless static; Sigstore signed):

```bash
docker pull ghcr.io/go-steer/core-agent:latest        # full build, in-process TUI included
docker pull ghcr.io/go-steer/core-agent-slim:latest   # headless variant, ~5MB smaller (no embedded TUI)
docker pull ghcr.io/go-steer/core-agent-tui:latest    # remote TUI client only
```

Floating tags: `:latest` (most recent semver), `:X.Y.Z` / `:X.Y` / `:X` (semver, no `v` prefix — matches Docker / Helm appVersion convention), `:main` (latest dev build), `:main-<sha>` (specific dev build). Verify signatures with `cosign verify ghcr.io/go-steer/core-agent:<tag> --certificate-identity-regexp '^https://github.com/go-steer/core-agent' --certificate-oidc-issuer https://token.actions.githubusercontent.com`.

Requires Go 1.26 or newer for source builds; container images carry their own toolchain.

---

## Quick start — CLI

```bash
# Gemini API key (accepts either GEMINI_API_KEY or GOOGLE_API_KEY)
GEMINI_API_KEY=...     core-agent -p "what's 2+2?"

# Vertex AI for Gemini (uses Application Default Credentials)
GOOGLE_GENAI_USE_VERTEXAI=true \
  GOOGLE_CLOUD_PROJECT=my-gcp-project \
  GOOGLE_CLOUD_LOCATION=us-central1 \
  core-agent -p "what's 2+2?"

# Anthropic API key
ANTHROPIC_API_KEY=...  core-agent --provider anthropic -p "what's 2+2?"

# Anthropic via Vertex AI (uses Application Default Credentials)
ANTHROPIC_VERTEX_PROJECT_ID=my-gcp-project CLOUD_ML_REGION=us-east5 \
  core-agent --provider anthropic-vertex --model claude-opus-4-7 -p "what's 2+2?"

# Multi-turn REPL (no -p)
core-agent
> hello
…
> what number did I just say?
…
> /exit
```

CLI flags:

```
-p, --prompt string     Single prompt; runs one turn and exits.
-c, --config string     Config file path (default: discover .agents/config.json).
-m, --model string      Override model name from config.
    --provider string   Override model.provider
                        (gemini|vertex|anthropic|anthropic-vertex|echo|scripted).
    --no-builtin-tools  Disable the built-in tool suite (read_file, write_file,
                        edit_file, list_dir, bash, todo).
    --disable-tools     Comma-separated list of built-in tools to disable
                        (e.g. bash,write_file). Composes by union with
                        cfg.tools.disable; ignored when --no-builtin-tools
                        is set.
    --script string     JSONL transcript for --provider=scripted.
    --script-strict     Scripted: assert each request matches the recorded one.
    --record-to string  Write a JSONL recording of all LLM turns to this path.
                        Works with any provider.
    --color string      ANSI color in streamed output: auto|always|never
                        (default: auto = on when stdout is a terminal,
                        off when piped). Tool calls render in cyan,
                        partial assistant text in green.
    --ask string        Register an ask_user tool the model can call:
                        off|stdin|auto (default: off). stdin reads from
                        os.Stdin; auto picks stdin when interactive,
                        else returns "(no user available)" so the model
                        adapts instead of blocking.
```

---

## Quick start — library

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/go-steer/core-agent/pkg/agent"
    "github.com/go-steer/core-agent/pkg/config"
    "github.com/go-steer/core-agent/pkg/models"
    _ "github.com/go-steer/core-agent/pkg/models/gemini"
)

func main() {
    cfg := config.DefaultConfig()
    cfg.Model.Provider = config.ProviderGemini

    provider, err := models.Resolve(cfg)
    if err != nil { log.Fatal(err) }

    ctx := context.Background()
    m, err := provider.Model(ctx, cfg.Model.Name)
    if err != nil { log.Fatal(err) }

    a, err := agent.New(m, agent.WithInstruction("Be concise."))
    if err != nil { log.Fatal(err) }

    for event, err := range a.Run(ctx, "What is the capital of France?") {
        if err != nil { log.Fatal(err) }
        if event.Content == nil { continue }
        for _, p := range event.Content.Parts {
            if p.Text != "" && event.Partial {
                fmt.Print(p.Text)
            }
        }
    }
    fmt.Println()
}
```

See [`examples/basic/`](./examples/basic/main.go) and [`examples/with-tools/`](./examples/with-tools/main.go) for fuller library use.

---

## Project layout

```
core-agent/
├── agent/           # ADK llmagent + runner wrapper; Option pattern
├── instruction/     # AGENTS.md / CLAUDE.md / GEMINI.md fallback loader
├── config/          # .agents/config.json schema, discovery, atomic persist
├── permissions/     # ask/allow/yolo gate + bash denylist + path scope
├── tools/           # Built-in tools (read/write/edit/list/bash/todo) +
│                    # GateToolset wrapper (bridges permissions ↔ ADK)
├── mcp/             # mcp.json schema, stdio/HTTP server lifecycle
├── skills/          # SKILL.md discovery → ADK skilltoolset
├── models/
│   ├── provider.go      # Provider interface + registry/Resolve
│   ├── gemini/          # Gemini API + Vertex AI
│   └── anthropic/       # Native model.LLM adapter for Claude
│                        # (api.anthropic.com + Vertex AI backends)
├── telemetry/       # OTEL exporter setup
├── usage/           # Per-turn token + cost tracker
├── session/         # JSON transcript persistence (.agents/sessions/)
├── eventlog/        # Durable session.Service + audit/replay event log
│                    # (SQLite/Postgres/MySQL via GORM, monotonic seq,
│                    # Since/Watch, session lock)
├── recording/       # LLM-wire recorder for offline replay through mock providers
├── runner/          # Headless (one-shot) + REPL (multi-turn) drivers
├── cmd/core-agent/  # CLI binary
├── examples/
├── extras/             # opt-in adapters that embed core-agent
│   └── scion-agent/    # runs core-agent inside Scion's container runtime
│   # (extras/ax-agent/ lives on the axplore branch — see docs/ax-plan.md)
├── dev/             # build/test/lint tooling — see dev/README.md
│   ├── tools/           # ci aggregator + per-check scripts (build, vet, lint-go, ...)
│   └── ci/presubmits/   # delegators called by .github/workflows/ci.yml
├── docs/            # internal docs (acceptance-m1.md, acceptance-m2.md, ...)
│   └── site/            # published Hugo site (Docsy theme)
└── .github/workflows/   # ci.yml, ci-docs.yml, docs.yml
```

The `Provider` interface is the extension point — register your own model backend with `models.Register("name", constructor)` and the rest of the stack picks it up.

---

## Project conventions

- **`.agents/` directory** — `core-agent` walks up from the working directory looking for `.agents/`, much like `git` looks for `.git`. It contains:
  - `config.json` — schema in [`config/config.go`](./config/config.go)
  - `mcp.json` — MCP server declarations
  - `skills/<name>/SKILL.md` — Claude-compatible skill bundles
  - `sessions/<timestamp>.json` — JSON transcript per one-shot invocation
- **`~/.<binary>/sessions.db`** — when `--session-db` is set, durable session storage + audit log lives here (binary name from `os.Executable()` so `core-agent`, `scion-agent`, and forks each get their own directory). Override with `--session-db-path`. See [`eventlog/`](./eventlog/) and the [Sessions and event log](https://go-steer.github.io/core-agent/docs/sessions/) site doc.
- **`AGENTS.md`** — project-level system instruction prefix, picked up from the discovered project root. `CLAUDE.md` and `GEMINI.md` are checked as fallbacks for repos that already have one.

---

## Roadmap

What's intentionally **not** in v1, with notes on where each lands:

- **Bubble Tea TUI** — interactive multi-turn UI with rendering, slash commands, and modal permission prompts.
- **Slash-command framework** — REPL ships with only `/exit` and `/quit`.
- **Anthropic pricing entries** in [`usage/pricing.go`](./usage/pricing.go) — Claude models currently return zero pricing; consumers can override via `cfg.Model.Pricing`.
- **Glob/grep built-in tools** — bash is the workaround today; plan in [`docs/tools-plan.md`](./docs/tools-plan.md).
- **Pause/resume mid-run** for `agent.RunAutonomous` — across-turn crash-resume shipped in M3; mid-turn pause is harder and waits for a real consumer use case.
- **Cost rollup from subagents into the parent's `usage.Tracker`** — subagent runs track usage internally; surfacing it to the parent is a follow-up.

---

## Milestones

`core-agent` follows a milestone-based development model. Each milestone lands a coherent slice of functionality, gets its build/test pass verified end-to-end, and updates this section.

### M1 — Library + CLI extraction *(shipped)*

Lifted the ADK plumbing out of [`go-steer/cogo`](https://github.com/go-steer/cogo)'s `internal/` packages into an importable library. Added a native ADK `model.LLM` adapter for Anthropic Claude — the largest piece of new code in this milestone.

Shipped:
- `agent/`, `instruction/`, `config/`, `permissions/`, `tools/`, `mcp/`, `skills/`, `telemetry/`, `usage/`, `session/`, `runner/` — all extracted from cogo and de-cogo'd
- `models/anthropic/` — new; ~500 lines covering the Provider, the streaming `model.LLM`, genai ↔ Anthropic SDK conversion (system extraction, tool round-trip, schema projection), and stop-reason mapping
- `models/gemini/` — lifted; Gemini API + Vertex AI
- `cmd/core-agent` — one-shot (`-p`) and REPL modes
- `examples/basic`, `examples/with-tools`
- `~6,000` LOC including tests; `go build ./...` and `go test ./...` clean

What v1 deliberately left behind from cogo: the Bubble Tea TUI, the slash-command machinery, and the cogo-specific branding. (The bash / read_file / write_file / edit_file / list_dir / todo built-in tool suite landed in unnumbered work later — see [`tools/`](./tools/) and the Features section above.)

### M2 — Anthropic via Vertex AI *(shipped)*

Added a second backend to the Anthropic provider so Claude can be reached through Google Vertex AI as well as `api.anthropic.com`. Users with existing GCP infrastructure no longer need a separate Anthropic API key — they get unified billing, IAM, and compliance posture by reusing their Google credentials.

The conversion code (`convert.go`, `stream.go`, `llm.go`) is provider-agnostic and stayed entirely unchanged; only client construction differs. The official SDK's `vertex` subpackage handles the URL rewriting (`/v1/messages` → `/v1/projects/{project}/locations/{region}/publishers/anthropic/models/{model}:rawPredict`) and the `anthropic_version: vertex-2023-10-16` header.

Shipped:
- `models/anthropic/vertex.go` (~95 lines) — `NewVertex(ctx, project, region)` constructor + `newVertexProvider` registry constructor
- New provider name `"anthropic-vertex"` (distinct from `"anthropic"` since auth and billing differ); `Provider` struct now carries its own `name` field so `Name()` returns the registered identity
- `ModelConfig.Anthropic.Vertex *VertexConfig` for project + region overrides; env-var fallback chain `ANTHROPIC_VERTEX_PROJECT_ID` → `GOOGLE_CLOUD_PROJECT` for project, `CLOUD_ML_REGION` → `GOOGLE_CLOUD_LOCATION` → `us-east5` for region
- ADC-based auth via `google.FindDefaultCredentials` + `vertex.WithCredentials` (we deliberately don't use `vertex.WithGoogleAuth`, which panics on missing creds)
- 5 unit tests covering input validation, env-fallback wiring, registry round-trip, and config validation
- CLI `--provider` help text updated to list `anthropic-vertex`
- [`docs/acceptance-m2.md`](./docs/acceptance-m2.md) with end-to-end gates for the new path

Out of scope (still deferred): auto-detection of `anthropic-vertex` from generic GCP env vars — too overlapping with Vertex Gemini to disambiguate safely. Users explicitly opt in via `--provider anthropic-vertex` or config. Listed under M4 candidates.

### M3 — Autonomy + durable sessions + subagents *(shipped)*

A single-themed milestone that brought core-agent from "library you can call from a REPL" to "library you can run as an unattended worker with audit logs, crash recovery, and in-process delegation." Each piece shipped behind its own opt-in option so the v1 surface stays clean.

Shipped:

- **`tools.NewLifecycleTool`** — generic state-emission primitive the model uses to signal "thinking", "blocked", "done", or any custom label. Consumer-supplied handler decides where the events go (stdout, status file, websocket, orchestrator's event log). Used internally by the autonomous driver as its termination signal; exported for direct use by orchestrator adapters.
- **`tools.NewAskUserTool`** + three built-in `Prompter`s (`StdinPrompter`, `RefusePrompter`, `StaticPrompter`) — in-turn human consultation pattern. CLI flag `--ask=stdin|auto|off`.
- **`agent.RunAutonomous`** — multi-turn driver for unattended runs (batch jobs, CI tasks, scheduled scripts). Budgets (turns / tokens / cost / wallclock / per-turn timeout), failure policy, model-driven termination via the lifecycle tool, optional permissions deadlock guard via `WithPermissionsGate`.
- **`agent.WithSessionService` + `eventlog.Open`** — durable session backend wrapping ADK's GORM-backed `database.SessionService`. Multi-driver via SQLite (pure-Go, no CGO) / Postgres / MySQL. Adds an `agent_eventlog` overlay table with monotonic `seq INTEGER PRIMARY KEY AUTOINCREMENT` for AX-style "everything since seq N" replay. `Stream.Since(seq)` for replay, `Stream.Watch(seq)` for live tail, `Handle.AcquireLock` for cross-process exclusion. CLI flags `--session-db` and `--session-db-path`.
- **`agent.ResumeAutonomous`** — crash-resume for autonomous runs. Per-turn checkpoint events land in the durable log; resume reads the latest checkpoint, re-derives totals, continues from the next turn. Cross-binary resume works via `Author="<binary>/autonomous"` suffix matching. Terminal-state short-circuit only on `Completed` so budget-exhausted runs can be resumed with a higher cap.
- **`agent.WithSubagents([]*Agent)`** + `agent.NewSubagentTool` — in-process delegation. The parent's model invokes each subagent as a tool; the subagent runs in a derived session row (same database, distinct row to satisfy ADK's optimistic-concurrency check) with `Branch="<parent>.<sub>"` for branch-scoped audit queries.
- **Mock providers** + **`recording/`** — `--provider=echo` and `--provider=scripted --script=path.jsonl` for credential-free testing; `recording.NewRecorder(m, w)` captures any real session for offline replay.
- **`runner.WriteEvents`** with `WithColor` — chat-style event streaming for library consumers; the bundled CLI uses it.
- **Two new optional adapters in `extras/`** — `extras/ax-agent/` packages core-agent as an AX (Agent eXecutor) gRPC remote agent (lives on the `axplore` branch since `github.com/google/ax` is currently private).
- **Five new published doc pages**: [Autonomous runs](https://go-steer.github.io/core-agent/docs/autonomous/), [Sessions and event log](https://go-steer.github.io/core-agent/docs/sessions/), plus expanded Library API / Permissions / Configuration / Getting Started cross-references.
- **Five new examples**: `examples/streaming/`, `examples/autonomous/`, `examples/autonomous-resume/`, `examples/with-subagent/`, plus a stable `examples/replay/`.
- Roughly **+8,000 LOC** including tests; all 7 presubmits green throughout.

Out of scope for M3 (deferred — see Roadmap above): Bubble Tea TUI, slash-command framework, glob/grep built-ins, mid-run pause/resume, subagent cost rollup, Bedrock backend, automatic provider auto-detection.

### M4 — TBD

Candidates, ordered roughly by likely value:

- **Glob/grep built-in tools** ([`docs/tools-plan.md`](./docs/tools-plan.md)) — fills a real day-to-day-ergonomics gap; bash is the workaround today.
- **Amazon Bedrock + Claude Platform on AWS** as additional Anthropic backends — direct extension of M2's pattern.
- **Slash-command framework** — extend the REPL beyond `/exit` and `/quit`.
- **Anthropic feature coverage** — extended/adaptive thinking, structured outputs, server-side tools, vision.
- **Cost rollup from subagents** into the parent's `usage.Tracker`.
- **Auto-detection for `anthropic-vertex`** — currently explicit-only; could fire on `ANTHROPIC_VERTEX_PROJECT_ID` set without `GOOGLE_API_KEY`, but the env semantics need careful design first.

Final M4 scope will be picked based on what downstream consumers ask for first.

---

## Contributing

PRs welcome. A few things to keep in mind:

- Run `dev/tools/ci` before opening a PR — it runs the same checks GitHub Actions does (vet, build, lint, mod-tidy, test, vuln scan), in fast-fail order. See [`dev/README.md`](./dev/README.md) for the full layout and how to add a check.
- Every source file carries the full Apache 2.0 header attributed to Google LLC. The `goheader` linter inside `dev/tools/lint-go` enforces this on every `.go` file. For new shell / YAML / Python files, run `dev/tools/add-license-headers` (idempotent).
- The library is meant to stay narrow. Built-in tools, CLI flags, and slash commands belong in consumer projects, not here.
- Each milestone gets an `acceptance-mN.md` plan in [`docs/`](./docs) and an entry in this README's **Milestones** section, added at the close of the milestone.

---

## License

Apache-2.0. See [LICENSE](./LICENSE).
