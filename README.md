# core-agent

A reusable Go base agent built on the [Google Agent Development Kit](https://pkg.go.dev/google.golang.org/adk).

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
- **Built-in tool suite** — `read_file`, `write_file`, `edit_file`, `list_dir`, `bash`, `todo`. Wired up by default in the bundled CLI; opt-out via `--no-builtin-tools` for the whole suite, or `--disable-tools=bash,write_file` (or `tools.disable` in config) for specific entries. All tools route through the permission gate.
- **Permission gate** — ask / allow / yolo modes, per-tool allow- and deny-list patterns, path-scope checks for file tools, and a built-in bash denylist that's non-overridable.
- **Telemetry** — opt-in OpenTelemetry export (console / OTLP); off by default so a fresh invocation makes zero outbound calls.
- **Headless CLI** — `core-agent -p "prompt"` for one-shot use; bare `core-agent` drops into a stdin REPL with conversation history preserved across turns.
- **Optional Scion adapter** — [`extras/scion-agent/`](./extras/scion-agent/) packages core-agent for [Scion](https://github.com/GoogleCloudPlatform/scion)'s container runtime: lifecycle status emission, `--input` task delivery, and a `sciontool_status` tool the model uses to declare sticky states.

---

## Documentation

Full reference docs live at **<https://go-steer.github.io/core-agent/>** — getting started, every provider with env-var details, `.agents/config.json` schema, MCP setup, skills, permissions, and library API.

The site is built with [Hugo](https://gohugo.io) using the [Hextra](https://github.com/imfing/hextra) theme; sources are in [`docs/site/`](./docs/site). To preview locally:

```bash
cd docs/site
hugo server   # http://localhost:1313/core-agent/
```

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

Requires Go 1.26 or newer.

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
```

---

## Quick start — library

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/go-steer/core-agent/agent"
    "github.com/go-steer/core-agent/config"
    "github.com/go-steer/core-agent/models"
    _ "github.com/go-steer/core-agent/models/gemini"
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
├── session/         # Transcript persistence (.agents/sessions/)
├── runner/          # Headless (one-shot) + REPL (multi-turn) drivers
├── cmd/core-agent/  # CLI binary
├── examples/
├── extras/             # opt-in adapters that embed core-agent
│   └── scion-agent/    # runs core-agent inside Scion's container runtime
├── dev/             # build/test/lint tooling — see dev/README.md
│   ├── tools/           # ci aggregator + per-check scripts (build, vet, lint-go, ...)
│   └── ci/presubmits/   # delegators called by .github/workflows/ci.yml
├── docs/            # internal docs (acceptance-m1.md, acceptance-m2.md, ...)
│   └── site/            # published Hugo site (Hextra theme)
└── .github/workflows/   # ci.yml, ci-docs.yml, docs.yml
```

The `Provider` interface is the extension point — register your own model backend with `models.Register("name", constructor)` and the rest of the stack picks it up.

---

## Project conventions

- **`.agents/` directory** — `core-agent` walks up from the working directory looking for `.agents/`, much like `git` looks for `.git`. It contains:
  - `config.json` — schema in [`config/config.go`](./config/config.go)
  - `mcp.json` — MCP server declarations
  - `skills/<name>/SKILL.md` — Claude-compatible skill bundles
  - `sessions/<timestamp>.json` — transcript per one-shot invocation
- **`AGENTS.md`** — project-level system instruction prefix, picked up from the discovered project root. `CLAUDE.md` and `GEMINI.md` are checked as fallbacks for repos that already have one.

---

## Roadmap

What's intentionally **not** in v1, with notes on where each lands:

- **Subagents** — a `WithSubagents([]*Agent)` option that registers each subagent as a synthetic tool. Marker is in [`agent/agent.go`](./agent/agent.go) where the option will plug in.
- **Bubble Tea TUI** — interactive multi-turn UI with rendering, slash commands, and modal permission prompts.
- **File-backed session service** — today the ADK in-memory store is used, so REPL history dies with the process.
- **Slash-command framework** — REPL ships with only `/exit` and `/quit`.
- **Anthropic pricing entries** in [`usage/pricing.go`](./usage/pricing.go) — Claude models currently return zero pricing; consumers can override via `cfg.Model.Pricing`.

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

What v1 deliberately leaves behind from cogo: the Bubble Tea TUI, the bash/read_file/grep/write_file built-in tools (consumers add their own), the slash-command machinery, and the cogo-specific branding.

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

Out of scope (deferred to M3): auto-detection of `anthropic-vertex` from generic GCP env vars — too overlapping with Vertex Gemini to disambiguate safely. Users explicitly opt in via `--provider anthropic-vertex` or config.

### M3 — TBD

Candidates, ordered roughly by likely value to downstream consumers:

- **Subagents** — `WithSubagents([]*Agent)` option that registers each subagent as a synthetic tool. Marker is in [`agent/agent.go`](./agent/agent.go).
- **Amazon Bedrock + Claude Platform on AWS** as additional Anthropic backends — direct extension of M2's pattern (same conversion code, different client construction via the SDK's `bedrock/` and `aws/` subpackages).
- **File-backed session service** — REPL conversation history that survives process restart.
- **Slash-command framework** — extend the REPL beyond `/exit` and `/quit` (e.g. `/model`, `/permissions`, `/memory`).
- **Anthropic feature coverage** — extended/adaptive thinking, structured outputs, server-side tools (web_search, code_execution), vision.
- **Auto-detection for `anthropic-vertex`** — currently explicit-only; could fire on `ANTHROPIC_VERTEX_PROJECT_ID` set without `GOOGLE_API_KEY`, but the env semantics need careful design first.

Final M3 scope will be picked based on what downstream consumers ask for first.

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
